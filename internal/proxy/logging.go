package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liguangsheng/wildtoken/internal/apperr"
	"github.com/liguangsheng/wildtoken/internal/db"
	"github.com/liguangsheng/wildtoken/internal/logbody"
	"github.com/liguangsheng/wildtoken/internal/models"
	"github.com/liguangsheng/wildtoken/internal/quota"
)

const (
	slowDBOperationThreshold    = time.Second
	logWriteMaxAttempts         = 5
	logWriteRetryBaseDelay      = 50 * time.Millisecond
	logWriteBatchSize           = 20
	logWriteBatchInterval       = 50 * time.Millisecond
	logWriteTimeout             = 15 * time.Second
	carriedUsageRetryInterval   = 5 * time.Second
	logEventChannelCapacity     = 1024
	cleanupStartupDelay         = 120 * time.Second
	logBodyCleanupInterval      = 60 * time.Second
	logRetentionCleanupInterval = 3600 * time.Second
	incrementalVacuumMaxPages   = 4096

	// logWriteBatchBytes caps the captured body bytes one batch carries, so a
	// few large bodies do not hold the write lock for long.
	logWriteBatchBytes = 64 << 20
	// logQueueBodyBudget caps the captured body bytes waiting in the queue. A
	// queued snapshot keeps its request's buffer alive and the queue holds
	// hundreds of entries, so past the budget a body keeps only its length.
	logQueueBodyBudget = 256 << 20
	// chunkCollectionInterval spaces out chunk collection, which reads every
	// stored body record.
	chunkCollectionInterval = 10 * time.Minute
)

// LogEntry is a structured record of one proxied request.
type LogEntry struct {
	Method              string
	Path                string
	DownstreamTokenID   *int64
	DownstreamTokenName *string
	// ClientIP is the caller's address, resolved from the forwarded headers
	// with the socket peer as fallback. nil when it could not be determined.
	ClientIP                  *string
	ClientType                *string
	UpstreamID                *int64
	UpstreamName              *string
	Model                     *string
	RequestModel              *string
	UpstreamModel             *string
	ReasoningEffort           *string
	UpstreamReasoningEffort   *string
	ResponseReasoningEffort   *string
	Stream                    bool
	StatusCode                *int32
	PromptTokens              *int32
	CompletionTokens          *int32
	TotalTokens               *int32
	PromptCachedTokens        *int32
	CacheCreationTokens       *int32
	CompletionReasoningTokens *int32
	FirstTokenMs              *int32
	DurationMs                *int32
	Error                     *string
	// The captured snapshots. One snapshot may fill several slots, and several
	// entries: a retried request logs the same request on every attempt.
	DownstreamRequest  *logbody.Snapshot
	UpstreamRequest    *logbody.Snapshot
	UpstreamResponse   *logbody.Snapshot
	DownstreamResponse *logbody.Snapshot
}

// snapshots lists the entry's snapshots in body record order.
func (e *LogEntry) snapshots() [logbody.SlotCount]*logbody.Snapshot {
	return [logbody.SlotCount]*logbody.Snapshot{
		e.DownstreamRequest, e.UpstreamRequest, e.UpstreamResponse, e.DownstreamResponse,
	}
}

// bodyBytes is how many captured body bytes the entry keeps in memory. A
// buffer two snapshots share — a request nothing rewrote — counts once.
func (e *LogEntry) bodyBytes() int {
	total := 0
	var seen [logbody.SlotCount]*byte
	for i, snapshot := range e.snapshots() {
		if snapshot.BodyBytes() == 0 {
			continue
		}
		first := &snapshot.Body.Data[0]
		shared := false
		for _, other := range seen[:i] {
			shared = shared || other == first
		}
		seen[i] = first
		if !shared {
			total += snapshot.BodyBytes()
		}
	}
	return total
}

// withoutBodies returns a copy of the entry whose bodies keep only their
// lengths. Slots that shared a snapshot still share its replacement; the
// originals are left alone, since other entries may hold them.
func (e LogEntry) withoutBodies() LogEntry {
	replaced := map[*logbody.Snapshot]*logbody.Snapshot{}
	strip := func(snapshot *logbody.Snapshot) *logbody.Snapshot {
		if snapshot == nil {
			return nil
		}
		if replacement, ok := replaced[snapshot]; ok {
			return replacement
		}
		replaced[snapshot] = snapshot.WithoutBody()
		return replaced[snapshot]
	}
	e.DownstreamRequest = strip(e.DownstreamRequest)
	e.UpstreamRequest = strip(e.UpstreamRequest)
	e.UpstreamResponse = strip(e.UpstreamResponse)
	e.DownstreamResponse = strip(e.DownstreamResponse)
	return e
}

// LogStreamEvent reports that a committed request-log row became available to
// the admin console.
//
// The database remains the source of truth for pagination and details. The
// event only carries the lightweight list row, without request/response bodies.
type LogStreamEvent struct {
	Log       models.RequestLogOut `json:"log"`
	RecentRPM *int64               `json:"recent_rpm,omitempty"`
	RecentTPM *int64               `json:"recent_tpm,omitempty"`
}

// LogMetricsRecorder is the metrics subset the writer reports to.
type LogMetricsRecorder interface {
	RecordLogEnqueue()
	RecordLogDequeue(count uint64)
	RecordLogWritten(count uint64)
	RecordLogDrop()
	RecordLogWriteFailureCount(count uint64)
	RecordSlowDBOperation()
	BeginCleanup()
	RecordCleanupBatch(rowsCleared uint64)
	FinishCleanup(success bool, duration time.Duration)
}

type persistedLogRecord struct {
	stats db.PersistedLogStats
	event LogStreamEvent
}

// LogWriter batches request logs onto a single background writer, so proxy
// requests never wait on SQLite's write lock.
type LogWriter struct {
	entries chan LogEntry
	metrics LogMetricsRecorder
	events  *eventBroker
	quotas  *quota.Tracker
	done    chan struct{}
	// closeMu orders closing the queue against scheduling onto it. Sending on a
	// closed channel panics and a select's default case does not prevent that,
	// so the two are made exclusive rather than left to race: a stream still
	// running when the server stopped waiting for it schedules its log after
	// shutdown has begun.
	closeMu   sync.RWMutex
	closed    bool
	closeOnce sync.Once

	// queuedBodyBytes counts the captured body bytes entries in the queue hold,
	// against logQueueBodyBudget.
	queuedBodyBytes atomic.Int64

	// carried is usage whose row was dropped or failed to commit, per token.
	// It is still owed to the stored total, so a later batch applies it on its
	// own; until then it stays held against the quota.
	carriedMu sync.Mutex
	carried   map[int64]int64
}

// NewLogWriter starts the background writer. It stops when Close is called,
// which is also what drains the queue: ctx bounds each write rather than
// abandoning the rows still waiting to be made.
func NewLogWriter(ctx context.Context, database *sql.DB, metrics LogMetricsRecorder,
	logStats *db.LogStatsCache, queueCapacity int, quotas *quota.Tracker) *LogWriter {
	writer := &LogWriter{
		entries: make(chan LogEntry, max(queueCapacity, 1)),
		metrics: metrics,
		events:  newEventBroker(),
		quotas:  quotas,
		done:    make(chan struct{}),
	}
	go writer.run(ctx, database, logStats)
	return writer
}

// Schedule queues a log entry, dropping it when the queue is full or closed.
//
// A dropped log is preferable to blocking a proxied request on the writer.
func (w *LogWriter) Schedule(entry LogEntry) {
	w.metrics.RecordLogEnqueue()

	// Held across the send so the queue cannot close underneath it.
	w.closeMu.RLock()
	defer w.closeMu.RUnlock()

	if w.closed {
		w.metrics.RecordLogDequeue(1)
		w.metrics.RecordLogDrop()
		slog.Warn("request log queue closed; dropping request log")
		w.carryDropped(entry)
		return
	}

	// The usage is held against the token's quota until the row commits,
	// because until then the stored total still understates it. The hold comes
	// before the send: taken after it, a writer that committed at once settled
	// a hold not yet there, and the hold then taken stayed for good, refusing
	// the token early until a restart.
	tokenID, used, metered := quotaUsage(entry)
	if metered {
		w.quotas.Meter(tokenID, used)
	}

	// A burst of large requests must not pile their buffers up in memory while
	// they wait for the writer. Past the budget the body keeps only its length;
	// the row and the usage it carries are still written.
	size := int64(entry.bodyBytes())
	if size > 0 && w.queuedBodyBytes.Add(size) > logQueueBodyBudget {
		w.queuedBodyBytes.Add(-size)
		entry, size = entry.withoutBodies(), 0
		slog.Warn("request log queue holds too many body bytes; logging only the body lengths")
	}

	select {
	case w.entries <- entry:
	default:
		w.queuedBodyBytes.Add(-size)
		w.metrics.RecordLogDequeue(1)
		w.metrics.RecordLogDrop()
		slog.Warn("request log queue full; dropping request log")
		// Held already; a later batch applies it and settles the hold.
		if metered {
			w.carry(tokenID, used)
		}
	}
}

// carryDropped keeps the usage of an entry that will not be written.
//
// Dropping it with its row let a token under load spend without the quota ever
// seeing it: the busier the gateway, the more usage went uncounted.
func (w *LogWriter) carryDropped(entry LogEntry) {
	tokenID, used, ok := quotaUsage(entry)
	if !ok {
		return
	}
	w.quotas.Meter(tokenID, used)
	w.carry(tokenID, used)
}

func (w *LogWriter) carry(tokenID, used int64) {
	w.carriedMu.Lock()
	defer w.carriedMu.Unlock()
	if w.carried == nil {
		w.carried = map[int64]int64{}
	}
	w.carried[tokenID] += used
}

// takeCarried hands everything owed to one batch.
func (w *LogWriter) takeCarried() map[int64]int64 {
	w.carriedMu.Lock()
	defer w.carriedMu.Unlock()
	carried := w.carried
	w.carried = nil
	return carried
}

// quotaUsage reports the token and amount a log entry contributes to a quota.
//
// It is the single definition of what counts, so the hold taken at enqueue and
// the increment applied at commit can never disagree.
func quotaUsage(entry LogEntry) (tokenID int64, used int64, ok bool) {
	if entry.DownstreamTokenID == nil || entry.TotalTokens == nil || *entry.TotalTokens <= 0 {
		return 0, 0, false
	}
	return *entry.DownstreamTokenID, int64(*entry.TotalTokens), true
}

// Subscribe returns a feed of committed rows. Close releases it.
func (w *LogWriter) Subscribe() *LogSubscription {
	return w.events.subscribe()
}

// Close stops accepting entries and waits for the queued batch to drain.
func (w *LogWriter) Close() { w.CloseWithin(0) }

// CloseWithin stops accepting entries and waits up to limit for the queue to
// drain, reporting whether it finished. A limit of zero waits indefinitely.
//
// Shutdown needs the bound. Each batch may spend up to logWriteTimeout on a
// database that has stopped answering, and a full queue is enough batches for
// that to add up to minutes — long past the point where an operator expects the
// process to be gone. Giving up loses the rows still queued, which is the same
// outcome as being killed, and only after having tried.
func (w *LogWriter) CloseWithin(limit time.Duration) bool {
	w.closeOnce.Do(func() {
		w.closeMu.Lock()
		w.closed = true
		w.closeMu.Unlock()
		close(w.entries)
	})

	if limit <= 0 {
		<-w.done
		return true
	}

	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-w.done:
		return true
	case <-timer.C:
		slog.Error("request log queue did not drain before shutdown gave up",
			"waited", limit)
		return false
	}
}

func (w *LogWriter) run(ctx context.Context, database *sql.DB, logStats *db.LogStatsCache) {
	defer close(w.done)

	// Carried usage normally rides with the next batch; the tick applies it
	// when no batch is coming.
	carriedRetry := time.NewTicker(carriedUsageRetryInterval)
	defer carriedRetry.Stop()

	batch := make([]LogEntry, 0, logWriteBatchSize)
	for {
		var entry LogEntry
		var ok bool
		select {
		case entry, ok = <-w.entries:
		case <-carriedRetry.C:
			w.flush(ctx, database, logStats, nil)
			continue
		}
		if !ok {
			w.flush(ctx, database, logStats, nil)
			return
		}
		batch = append(batch, entry)
		batchBytes := entry.bodyBytes()

		// Collect further entries until the batch fills or the window closes, so
		// bursts commit in one transaction.
		flushDeadline := time.NewTimer(logWriteBatchInterval)
		channelClosed := false
	collect:
		for len(batch) < logWriteBatchSize && batchBytes < logWriteBatchBytes {
			select {
			case <-flushDeadline.C:
				break collect
			case next, stillOpen := <-w.entries:
				if !stillOpen {
					channelClosed = true
					break collect
				}
				batch = append(batch, next)
				batchBytes += next.bodyBytes()
			}
		}
		flushDeadline.Stop()

		entries := batch
		batch = make([]LogEntry, 0, logWriteBatchSize)
		w.flush(ctx, database, logStats, entries)

		if channelClosed {
			return
		}
	}
}

func (w *LogWriter) flush(ctx context.Context, database *sql.DB, logStats *db.LogStatsCache,
	entries []LogEntry) {
	entryCount := uint64(len(entries))
	w.metrics.RecordLogDequeue(entryCount)
	// Out of the queue, these bodies no longer count against its budget.
	for i := range entries {
		w.queuedBodyBytes.Add(-int64(entries[i].bodyBytes()))
	}

	carried := w.takeCarried()
	if len(entries) == 0 && len(carried) == 0 {
		return
	}

	// Shutdown cancels the jobs context, but the batch it interrupts still has
	// to reach the database: these rows carry the quota increments, so a write
	// abandoned mid-shutdown hands that usage back for free. The deadline keeps
	// a stuck write from holding shutdown open instead.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), logWriteTimeout)
	defer cancel()

	startedAt := time.Now()
	prepared := prepareLogBatch(writeCtx, database, entries)
	records, err := insertLogBatchWithRetry(writeCtx, database, entries, prepared, carried)
	if err != nil {
		w.metrics.RecordLogWriteFailureCount(entryCount)
		slog.Error("failed to persist request logs", "error", err, "entry_count", entryCount)

		// The rows are lost; the usage they carry is not. It stays held and
		// goes to a later batch, which applies it without them.
		for _, entry := range entries {
			if tokenID, used, ok := quotaUsage(entry); ok {
				w.carry(tokenID, used)
			}
		}
		for tokenID, used := range carried {
			w.carry(tokenID, used)
		}
	} else {
		// Committed, so the stored total carries it and the hold can go.
		for _, entry := range entries {
			if tokenID, used, ok := quotaUsage(entry); ok {
				w.quotas.Settle(tokenID, used)
			}
		}
		for tokenID, used := range carried {
			w.quotas.Settle(tokenID, used)
		}
		w.metrics.RecordLogWritten(entryCount)
		w.publish(writeCtx, database, logStats, records)
	}
	if time.Since(startedAt) >= slowDBOperationThreshold {
		w.metrics.RecordSlowDBOperation()
	}
}

// publish folds committed rows into the stats cache and notifies subscribers.
//
// insertLogBatchWithRetry only returns after its SQLite transaction commits.
// Never publish from Schedule, or a dropped or rolled-back row could appear in
// the live console.
func (w *LogWriter) publish(ctx context.Context, database *sql.DB, logStats *db.LogStatsCache,
	records []persistedLogRecord) {
	if len(records) == 0 {
		return
	}

	stats := make([]db.PersistedLogStats, 0, len(records))
	for _, record := range records {
		stats = append(stats, record.stats)
	}
	logStats.RecordPersistedEntries(stats)

	var recentRate *db.RecentLogRate
	if rate, err := db.RecentOneMinuteLogRate(ctx, database); err == nil {
		recentRate = &rate
	} else {
		slog.Warn("failed to calculate recent log rate for live log event", "error", err)
	}

	for _, record := range records {
		event := record.event
		if recentRate != nil {
			requestCount := recentRate.RequestCount
			totalTokens := recentRate.TotalTokens
			event.RecentRPM = &requestCount
			event.RecentTPM = &totalTokens
		}
		w.events.publish(event)
	}
}

// prepareLogBatch cuts and hashes a batch's bodies and compresses the chunks
// not stored yet. It runs before the write transaction, so only inserts happen
// while the write lock is held; retries reuse its work.
func prepareLogBatch(ctx context.Context, database *sql.DB, entries []LogEntry) []*logbody.Prepared {
	chunks := logbody.NewBatch()
	prepared := make([]*logbody.Prepared, len(entries))
	for i := range entries {
		prepared[i] = chunks.Prepare(entries[i].snapshots())
	}
	// The lookup is only a hint; the transaction compresses whatever it finds
	// missing, so a failure here costs time, not rows.
	if err := db.PackNewChunks(ctx, database, chunks.Chunks()); err != nil {
		slog.Warn("looking up stored log chunks failed", "error", err)
	}
	return prepared
}

func insertLogBatchWithRetry(ctx context.Context, database *sql.DB, entries []LogEntry,
	prepared []*logbody.Prepared, carried map[int64]int64) ([]persistedLogRecord, error) {
	if len(entries) == 0 && len(carried) == 0 {
		return nil, nil
	}

	for attempt := 0; ; attempt++ {
		records, err := insertLogBatch(ctx, database, entries, prepared, carried)
		if err == nil {
			return records, nil
		}
		if !isDatabaseLocked(err) || attempt+1 >= logWriteMaxAttempts {
			return nil, err
		}
		delay := logWriteRetryBaseDelay << min(attempt, 4)
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(delay):
		}
	}
}

// isDatabaseLocked reports a contended write that is worth retrying.
func isDatabaseLocked(err error) bool {
	var appErr *apperr.AppError
	if errors.As(err, &appErr) {
		err = appErr.Unwrap()
	}
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "database is locked") ||
		strings.Contains(err.Error(), "SQLITE_BUSY")
}

// insertLogBatch writes a batch's rows, their snapshots and the quota usage
// they carry in one transaction. prepared holds each entry's snapshots, cut by
// prepareLogBatch.
func insertLogBatch(ctx context.Context, database *sql.DB, entries []LogEntry,
	prepared []*logbody.Prepared, carried map[int64]int64) ([]persistedLogRecord, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, apperr.Database(err)
	}
	defer tx.Rollback()
	payloads := db.NewPayloadStore(tx)
	defer payloads.Close()

	// Usage owed by rows that were dropped or failed earlier.
	for tokenID, used := range carried {
		if _, err := tx.ExecContext(ctx,
			"UPDATE api_tokens SET used_tokens = used_tokens + ? WHERE id = ?",
			used, tokenID); err != nil {
			return nil, apperr.Database(err)
		}
	}

	records := make([]persistedLogRecord, 0, len(entries))
	for i, entry := range entries {
		now := time.Now().UTC()
		createdAt := now.Format(models.TimestampFormat)
		streamInt := int32(0)
		if entry.Stream {
			streamInt = 1
		}
		clientType := "unknown"
		if entry.ClientType != nil {
			clientType = *entry.ClientType
		}

		// The token and channel ids were resolved when the request began, and
		// either row may have been deleted since. A dangling id failed the
		// foreign key and rolled back the whole batch, other tokens' rows and
		// quota increments with it, so a vanished parent is stored as NULL:
		// what ON DELETE SET NULL would have left had the row been older.
		result, err := tx.ExecContext(ctx, `INSERT INTO request_logs
        (method, path, downstream_token_id, downstream_token_name, client_ip,
         client_type,
         upstream_id, upstream_name, model, request_model, upstream_model,
         reasoning_effort, upstream_reasoning_effort, response_reasoning_effort,
         stream, status_code,
         prompt_tokens, completion_tokens, total_tokens,
         prompt_cached_tokens, cache_creation_tokens, completion_reasoning_tokens,
         duration_ms, first_token_ms, error, created_at)
    VALUES (?, ?, (SELECT id FROM api_tokens WHERE id = ?), ?, ?, ?,
            (SELECT id FROM upstreams WHERE id = ?),
            ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			entry.Method, entry.Path, entry.DownstreamTokenID, entry.DownstreamTokenName,
			entry.ClientIP,
			clientType, entry.UpstreamID, entry.UpstreamName, entry.Model,
			entry.RequestModel, entry.UpstreamModel, entry.ReasoningEffort,
			entry.UpstreamReasoningEffort,
			entry.ResponseReasoningEffort, streamInt, entry.StatusCode,
			entry.PromptTokens, entry.CompletionTokens, entry.TotalTokens,
			entry.PromptCachedTokens, entry.CacheCreationTokens,
			entry.CompletionReasoningTokens, entry.DurationMs, entry.FirstTokenMs,
			entry.Error, createdAt)
		if err != nil {
			return nil, apperr.Database(err)
		}
		logID, err := result.LastInsertId()
		if err != nil {
			return nil, apperr.Database(err)
		}

		if err := payloads.Store(ctx, logID, prepared[i], false); err != nil {
			return nil, err
		}

		// The quota counter advances with the log row, so a committed request is
		// always accounted for exactly once. A request that carried no usage, or
		// came from a deleted token, contributes nothing.
		//
		// Through quotaUsage, so the increment applied here and the hold taken
		// at enqueue cannot drift apart. They were two copies of one condition,
		// which is only correct for as long as nobody edits one of them.
		if tokenID, used, ok := quotaUsage(entry); ok {
			_, err := tx.ExecContext(ctx,
				"UPDATE api_tokens SET used_tokens = used_tokens + ? WHERE id = ?",
				used, tokenID)
			if err != nil {
				return nil, apperr.Database(err)
			}
		}

		records = append(records, persistedLogRecord{
			stats: db.PersistedLogStats{
				ID:                   logID,
				CreatedAtUnixSeconds: now.Unix(),
				TotalTokens:          int32PtrToInt64Ptr(entry.TotalTokens),
				PromptTokens:         int32PtrToInt64Ptr(entry.PromptTokens),
				PromptCachedTokens:   int32PtrToInt64Ptr(entry.PromptCachedTokens),
			},
			event: LogStreamEvent{
				Log: models.RequestLogOut{
					ID:                        logID,
					CreatedAt:                 createdAt,
					Method:                    entry.Method,
					Path:                      entry.Path,
					DownstreamTokenID:         entry.DownstreamTokenID,
					DownstreamTokenName:       entry.DownstreamTokenName,
					ClientIP:                  entry.ClientIP,
					ClientType:                clientType,
					UpstreamID:                entry.UpstreamID,
					UpstreamName:              entry.UpstreamName,
					Model:                     entry.Model,
					RequestModel:              entry.RequestModel,
					UpstreamModel:             entry.UpstreamModel,
					ReasoningEffort:           entry.ReasoningEffort,
					UpstreamReasoningEffort:   entry.UpstreamReasoningEffort,
					ResponseReasoningEffort:   entry.ResponseReasoningEffort,
					Stream:                    streamInt,
					StatusCode:                entry.StatusCode,
					PromptTokens:              entry.PromptTokens,
					CompletionTokens:          entry.CompletionTokens,
					TotalTokens:               entry.TotalTokens,
					PromptCachedTokens:        entry.PromptCachedTokens,
					CacheCreationTokens:       entry.CacheCreationTokens,
					CompletionReasoningTokens: entry.CompletionReasoningTokens,
					DurationMs:                entry.DurationMs,
					FirstTokenMs:              entry.FirstTokenMs,
					Error:                     entry.Error,
				},
			},
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, apperr.Database(err)
	}
	return records, nil
}

func int32PtrToInt64Ptr(value *int32) *int64 {
	if value == nil {
		return nil
	}
	converted := int64(*value)
	return &converted
}

// eventBroker fans committed log rows out to the console's SSE subscribers.
type eventBroker struct {
	mu          sync.Mutex
	subscribers map[int64]*LogSubscription
	nextID      int64
}

// LogSubscription is one console's feed of committed rows.
type LogSubscription struct {
	broker *eventBroker
	id     int64
	events chan LogStreamEvent
	// missed is set when publish drops an event this subscriber had no room for.
	missed atomic.Bool
}

func newEventBroker() *eventBroker {
	return &eventBroker{subscribers: map[int64]*LogSubscription{}}
}

func (b *eventBroker) subscribe() *LogSubscription {
	b.mu.Lock()
	defer b.mu.Unlock()

	subscription := &LogSubscription{
		broker: b,
		id:     b.nextID,
		events: make(chan LogStreamEvent, logEventChannelCapacity),
	}
	b.nextID++
	b.subscribers[subscription.id] = subscription
	return subscription
}

// Events delivers committed rows until Close.
func (s *LogSubscription) Events() <-chan LogStreamEvent { return s.events }

// TakeMissed reports whether rows were dropped since it last said so. A dropped
// row is never sent late; the console reloads from the database instead.
func (s *LogSubscription) TakeMissed() bool { return s.missed.Swap(false) }

// Close stops delivery and closes the Events channel. A repeat is a no-op.
func (s *LogSubscription) Close() {
	s.broker.mu.Lock()
	defer s.broker.mu.Unlock()
	if _, ok := s.broker.subscribers[s.id]; ok {
		delete(s.broker.subscribers, s.id)
		close(s.events)
	}
}

// publish delivers without blocking. A subscriber that cannot keep up loses the
// event rather than stalling the writer, and is marked to reload: the database
// stays authoritative.
func (b *eventBroker) publish(event LogStreamEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, subscription := range b.subscribers {
		select {
		case subscription.events <- event:
		default:
			subscription.missed.Store(true)
		}
	}
}

// ── Snapshots ────────────────────────────────────────────────────────────────

// SnapshotRequest captures a request: its method, url and redacted headers as
// metadata, and up to bodyMaxBytes of its body.
func SnapshotRequest(method, url string, headers map[string]string, body []byte,
	bodyMaxBytes int) *logbody.Snapshot {
	snapshot := &logbody.Snapshot{Meta: mustMarshalSnapshot(map[string]any{
		"method":  method,
		"url":     url,
		"headers": redactHeaders(headers),
	})}
	if body != nil {
		snapshot.Body = logbody.CaptureBody(body, bodyMaxBytes, len(body))
	}
	return snapshot
}

// SnapshotResponse captures a response: its status and redacted headers as
// metadata, and up to bodyMaxBytes of its body.
func SnapshotResponse(status int, headers map[string]string, body []byte,
	bodyMaxBytes int) *logbody.Snapshot {
	length := -1
	if body != nil {
		length = len(body)
	}
	return SnapshotResponseWithBodyLength(status, headers, body, length, bodyMaxBytes)
}

// SnapshotResponseWithBodyLength captures a response from a bounded body
// prefix while retaining the original response length. A negative
// bodyByteLength means the prefix is the whole body.
func SnapshotResponseWithBodyLength(status int, headers map[string]string, body []byte,
	bodyByteLength, bodyMaxBytes int) *logbody.Snapshot {
	snapshot := &logbody.Snapshot{Meta: mustMarshalSnapshot(map[string]any{
		"status_code": status,
		"status":      status,
		"headers":     redactHeaders(headers),
	})}
	if body != nil {
		length := bodyByteLength
		if length < 0 {
			length = len(body)
		}
		snapshot.Body = logbody.CaptureBody(body, bodyMaxBytes, length)
	}
	return snapshot
}

// mustMarshalSnapshot serializes snapshot metadata built from types that
// always marshal. A failure here would mean a programming error, so it degrades
// to an empty object rather than failing the request being logged.
func mustMarshalSnapshot(snapshot map[string]any) json.RawMessage {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		slog.Error("could not serialize a log snapshot", "error", err)
		return json.RawMessage(`{}`)
	}
	return encoded
}

func redactHeaders(headers map[string]string) map[string]string {
	redacted := make(map[string]string, len(headers))
	for name, value := range headers {
		if IsSensitiveHeaderName(name) {
			redacted[name] = "***REDACTED***"
			continue
		}
		redacted[name] = value
	}
	return redacted
}

// ── Background cleanup ──────────────────────────────────────────────────────

// RunCleanupLoop keeps full bodies near the configured row limit, collects the
// chunks dropped bodies leave behind every ten minutes, and performs the
// heavier retention and statistics work hourly.
//
// The runtime policy is read through a getter rather than a shared lock, so the
// loop always sees the latest console edit without owning the settings.
func RunCleanupLoop(ctx context.Context, database *sql.DB, settings func() models.RuntimeSettings,
	metrics LogMetricsRecorder, logStats *db.LogStatsCache) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(cleanupStartupDelay):
	}

	ticker := time.NewTicker(logBodyCleanupInterval)
	defer ticker.Stop()
	nextRetentionCleanup := time.Now()
	nextChunkCollection := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := time.Now()
		var steps CleanupSteps
		if !now.Before(nextRetentionCleanup) {
			steps.Retention = true
			nextRetentionCleanup = now.Add(logRetentionCleanupInterval)
		}
		if !now.Before(nextChunkCollection) {
			steps.Chunks = true
			nextChunkCollection = now.Add(chunkCollectionInterval)
		}

		current := settings()
		RunCleanupPass(ctx, database, &current, metrics, logStats, steps)
	}
}

// CleanupSteps says which of the heavier steps a cleanup pass takes, besides
// clearing the bodies past the keep count, which every pass does.
type CleanupSteps struct {
	// Retention deletes logs past the retention window, with the snapshot
	// metadata only they referred to.
	Retention bool
	// Chunks deletes the body chunks no stored log lists any more.
	Chunks bool
}

// RunCleanupPass performs one cleanup cycle.
func RunCleanupPass(ctx context.Context, database *sql.DB, settings *models.RuntimeSettings,
	metrics LogMetricsRecorder, logStats *db.LogStatsCache, steps CleanupSteps) {
	cleanupStartedAt := time.Now()
	cleanupSucceeded := true
	metrics.BeginCleanup()

	// A pass interrupted by shutdown is neither a success nor a fault: it
	// reports what stopped it and the next start picks the work up. Counting it
	// as an error would make every restart look like a cleanup failure, and
	// counting it as success would claim work that was not done.
	if _, err := db.ClearOldLogBodies(ctx, database, settings.LogBodyKeepCount, metrics); err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("log body cleanup stopped for shutdown")
			return
		}
		cleanupSucceeded = false
		slog.Error("clearing old log bodies failed", "error", err)
	}

	if steps.Retention {
		deleteStartedAt := time.Now()
		if err := db.DeleteOldLogs(ctx, database, settings.LogRetentionDays); err != nil {
			if errors.Is(err, context.Canceled) {
				slog.Info("log retention stopped for shutdown")
				return
			}
			cleanupSucceeded = false
			slog.Error("deleting old logs failed", "error", err)
		}
		// Snapshot metadata is shared between logs, so it goes only once the
		// last log referring to it has.
		if _, err := db.CollectLogMetas(ctx, database); err != nil {
			if errors.Is(err, context.Canceled) {
				slog.Info("log metadata collection stopped for shutdown")
				return
			}
			cleanupSucceeded = false
			slog.Error("collecting log metadata failed", "error", err)
		}
		if time.Since(deleteStartedAt) >= slowDBOperationThreshold {
			metrics.RecordSlowDBOperation()
		}

		statsRefreshStartedAt := time.Now()
		if err := logStats.RefreshFromDB(ctx, database); err != nil {
			cleanupSucceeded = false
			slog.Warn("request log statistics refresh after cleanup failed", "error", err)
		}
		if time.Since(statsRefreshStartedAt) >= slowDBOperationThreshold {
			metrics.RecordSlowDBOperation()
		}
	}

	// After clearing and retention, so it sees every body those just dropped.
	if steps.Chunks {
		collectStartedAt := time.Now()
		collected, err := db.CollectLogChunks(ctx, database)
		switch {
		case errors.Is(err, context.Canceled):
			slog.Info("log chunk collection stopped for shutdown")
			return
		case err != nil:
			cleanupSucceeded = false
			slog.Error("collecting log body chunks failed", "error", err)
		case collected > 0:
			slog.Debug("collected log body chunks no log lists", "chunks", collected)
		}
		if time.Since(collectStartedAt) >= slowDBOperationThreshold {
			metrics.RecordSlowDBOperation()
		}
	}

	reclaimStartedAt := time.Now()
	pagesReclaimed, err := db.ReclaimFreePages(ctx, database, incrementalVacuumMaxPages)
	switch {
	case err != nil:
		cleanupSucceeded = false
		slog.Error("incremental SQLite vacuum failed", "error", err)
	case pagesReclaimed > 0:
		slog.Debug("reclaimed SQLite pages after log cleanup", "pages_reclaimed", pagesReclaimed)
	}
	if time.Since(reclaimStartedAt) >= slowDBOperationThreshold {
		metrics.RecordSlowDBOperation()
	}

	metrics.FinishCleanup(cleanupSucceeded, time.Since(cleanupStartedAt))
}
