package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/liguangsheng/wildtoken/internal/db"
	"github.com/liguangsheng/wildtoken/internal/logbody"
	"github.com/liguangsheng/wildtoken/internal/metrics"
	"github.com/liguangsheng/wildtoken/internal/models"
	"github.com/liguangsheng/wildtoken/internal/quota"
)

func decodeSnapshotMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return decoded
}

func TestRequestAndResponseSnapshotsRedactMixedCaseSensitiveHeaders(t *testing.T) {
	headers := map[string]string{
		"aUtHoRiZaTiOn":       "Bearer secret",
		"sEt-CoOkIe":          "session=secret",
		"Api-Key":             "api-secret",
		"X-aCcEsS-tOkEn":      "access-secret",
		"X-Custom-Secret":     "custom-secret",
		"PrOxY-aUtHoRiZaTiOn": "proxy-secret",
		"X-Request-Id":        "request-123",
	}

	for _, snapshot := range []*logbody.Snapshot{
		SnapshotRequest("GET", "https://example.test", headers, nil, 1024),
		SnapshotResponse(200, headers, nil, 1024),
	} {
		decoded := decodeSnapshotMap(t, snapshot.JSON())
		snapshotHeaders, ok := decoded["headers"].(map[string]any)
		if !ok {
			t.Fatalf("headers missing from snapshot: %s", snapshot.JSON())
		}
		for _, name := range []string{"aUtHoRiZaTiOn", "sEt-CoOkIe", "Api-Key",
			"X-aCcEsS-tOkEn", "X-Custom-Secret", "PrOxY-aUtHoRiZaTiOn"} {
			if snapshotHeaders[name] != "***REDACTED***" {
				t.Errorf("header %s = %v, want it redacted", name, snapshotHeaders[name])
			}
		}
		if snapshotHeaders["X-Request-Id"] != "request-123" {
			t.Errorf("a non-sensitive header was redacted: %v", snapshotHeaders["X-Request-Id"])
		}
	}
}

func TestBoundedResponseSnapshotRetainsTheOriginalByteLength(t *testing.T) {
	snapshot := SnapshotResponseWithBodyLength(200, map[string]string{},
		[]byte("aé"), 100, 2)
	decoded := decodeSnapshotMap(t, snapshot.JSON())
	body, ok := decoded["body"].(map[string]any)
	if !ok {
		t.Fatalf("body missing from snapshot: %s", snapshot.JSON())
	}
	if body["text"] != "a" {
		t.Errorf("text = %v, want the prefix cut at a rune boundary", body["text"])
	}
	if body["byte_length"] != float64(100) {
		t.Errorf("byte_length = %v, want the original 100", body["byte_length"])
	}
	if body["truncated"] != true {
		t.Error("truncated was not reported")
	}
}

// textSnapshot captures a whole body under the given metadata.
func textSnapshot(meta string, body string) *logbody.Snapshot {
	return &logbody.Snapshot{
		Meta: []byte(meta),
		Body: logbody.CaptureBody([]byte(body), len(body)+1, len(body)),
	}
}

// insertLogs writes entries the way the writer does: cut outside the
// transaction, stored inside it.
func insertLogs(ctx context.Context, database *sql.DB, entries []LogEntry,
	carried map[int64]int64) ([]persistedLogRecord, error) {
	return insertLogBatch(ctx, database, entries, prepareLogBatch(ctx, database, entries), carried)
}

func countRows(t *testing.T, database *sql.DB, query string) int64 {
	t.Helper()
	var count int64
	if err := database.QueryRow(query).Scan(&count); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return count
}

func loggingTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.Init(context.Background(), database); err != nil {
		t.Fatalf("init: %v", err)
	}
	return database
}

func TestLogInsertWritesMetadataAndDeduplicatedPayloadAtomically(t *testing.T) {
	database := loggingTestDB(t)
	request := textSnapshot(`{"method":"POST"}`, "request")
	response := textSnapshot(`{"status":200}`, "response")
	promptCached, cacheCreation, reasoning := int32(12), int32(34), int32(56)

	records, err := insertLogs(context.Background(), database, []LogEntry{{
		Method:                    "POST",
		Path:                      "/v1/responses",
		DownstreamRequest:         request,
		UpstreamRequest:           request,
		UpstreamResponse:          response,
		PromptCachedTokens:        &promptCached,
		CacheCreationTokens:       &cacheCreation,
		CompletionReasoningTokens: &reasoning,
	}}, nil)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("inserted %d records, want 1", len(records))
	}

	detail, ok, err := db.GetLogDetail(context.Background(), database, 1)
	if err != nil || !ok {
		t.Fatalf("detail: ok=%v err=%v", ok, err)
	}
	want := `{"body":{"byte_length":7,"text":"request"},"method":"POST"}`
	if string(detail.DownstreamRequest) != want || string(detail.UpstreamRequest) != want {
		t.Errorf("requests = %s / %s, want %s", detail.DownstreamRequest, detail.UpstreamRequest, want)
	}
	if string(detail.UpstreamResponse) != `{"body":{"byte_length":8,"text":"response"},"status":200}` {
		t.Errorf("upstream response = %s", detail.UpstreamResponse)
	}
	// An absent snapshot reads back as absent.
	if detail.DownstreamResponse != nil {
		t.Errorf("downstream response = %s, want nil", detail.DownstreamResponse)
	}

	// The identical request is stored once: one metadata value, one chunk.
	var downstreamMeta, upstreamMeta int64
	if err := database.QueryRow(`SELECT downstream_request_meta, upstream_request_meta
           FROM request_log_snapshots WHERE request_log_id = 1`).
		Scan(&downstreamMeta, &upstreamMeta); err != nil {
		t.Fatalf("read snapshot row: %v", err)
	}
	if downstreamMeta != upstreamMeta {
		t.Errorf("metadata ids %d and %d, want the identical request stored once", downstreamMeta, upstreamMeta)
	}
	if chunks := countRows(t, database, "SELECT COUNT(*) FROM log_chunks"); chunks != 2 {
		t.Errorf("stored %d chunks, want one per distinct body", chunks)
	}

	var storedCached, storedCreation, storedReasoning sql.NullInt64
	err = database.QueryRow(`SELECT prompt_cached_tokens, cache_creation_tokens,
              completion_reasoning_tokens FROM request_logs WHERE id = 1`).
		Scan(&storedCached, &storedCreation, &storedReasoning)
	if err != nil {
		t.Fatalf("read token details: %v", err)
	}
	if storedCached.Int64 != 12 || storedCreation.Int64 != 34 || storedReasoning.Int64 != 56 {
		t.Errorf("token details = %d/%d/%d, want 12/34/56",
			storedCached.Int64, storedCreation.Int64, storedReasoning.Int64)
	}
}

// A coding agent resends its whole conversation every turn. Stored by chunk,
// each turn costs its new tail rather than the whole conversation again.
func TestAGrowingConversationStoresOnlyItsNewTurn(t *testing.T) {
	database := loggingTestDB(t)
	ctx := context.Background()

	// Words drawn at random, seeded: periodic text would give the chunker no
	// content to cut on, which real conversations never do.
	words := strings.Fields("the a model tool call result file line error test build user " +
		"assistant read write change function value return list map key")
	random := rand.New(rand.NewPCG(1, 2))
	conversation := ""
	for turn := range 20 {
		var text strings.Builder
		for text.Len() < 24<<10 {
			text.WriteString(words[random.IntN(len(words))])
			text.WriteByte(' ')
		}
		conversation += fmt.Sprintf("turn %d: %s\n", turn, text.String())
		request := textSnapshot(`{"method":"POST"}`, conversation)
		if _, err := insertLogs(ctx, database, []LogEntry{{
			Method: "POST", Path: "/v1/messages",
			DownstreamRequest: request, UpstreamRequest: request,
		}}, nil); err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
	}

	var storedBytes int64
	if err := database.QueryRow("SELECT COALESCE(SUM(size), 0) FROM log_chunks").Scan(&storedBytes); err != nil {
		t.Fatalf("sum chunks: %v", err)
	}
	// 20 turns of about 25 KB each were sent as 20 growing bodies, 5 MB in
	// all. Every chunk is stored once: the conversation plus the tail cut
	// differently on each turn.
	if storedBytes > 2*int64(len(conversation)) {
		t.Errorf("stored %d bytes of chunks for a %d byte conversation", storedBytes, len(conversation))
	}

	last, ok, err := db.GetLogSnapshot(ctx, database, 20, db.LogSnapshotDownstreamRequest)
	if err != nil || !ok {
		t.Fatalf("read last turn: ok=%v err=%v", ok, err)
	}
	body := decodeSnapshotMap(t, last)["body"].(map[string]any)
	if body["text"] != conversation {
		t.Error("the last turn did not read back whole")
	}
}

// A body past the queue's budget keeps only its length; the row and the
// usage it carries are written all the same.
func TestQueuedBodiesPastTheBudgetKeepOnlyTheirLength(t *testing.T) {
	database := loggingTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := NewLogWriter(ctx, database, metrics.New(), db.NewLogStatsCache(), 16, quota.NewTracker())

	writer.queuedBodyBytes.Store(logQueueBodyBudget)
	request := textSnapshot(`{"method":"POST"}`, "a body that does not fit")
	writer.Schedule(LogEntry{Method: "POST", Path: "/v1/messages",
		DownstreamRequest: request, UpstreamRequest: request})
	writer.queuedBodyBytes.Store(0)
	writer.Close()

	snapshot, ok, err := db.GetLogSnapshot(context.Background(), database, 1, db.LogSnapshotUpstreamRequest)
	if err != nil || !ok {
		t.Fatalf("read: ok=%v err=%v", ok, err)
	}
	body := decodeSnapshotMap(t, snapshot)["body"].(map[string]any)
	if body["cleared"] != true || body["byte_length"] != float64(24) {
		t.Errorf("body = %v, want only its length", body)
	}
	if request.Body.Kind != logbody.KindText {
		t.Error("the shared snapshot was changed in place")
	}
}

func TestLogWriterBatchesQueuedEntriesAndUpdatesMetrics(t *testing.T) {
	database := loggingTestDB(t)
	runtimeMetrics := metrics.New()
	logStats := db.NewLogStatsCache()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := NewLogWriter(ctx, database, runtimeMetrics, logStats, 16, quota.NewTracker())
	subscription := writer.Subscribe()
	defer subscription.Close()
	events := subscription.Events()

	status := int32(200)
	for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
		writer.Schedule(LogEntry{Method: "POST", Path: path, StatusCode: &status,
			DownstreamRequest: textSnapshot(`{"method":"POST"}`, path)})
	}
	writer.Close()

	var count int64
	if err := database.QueryRow("SELECT COUNT(*) FROM request_logs").Scan(&count); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if count != 2 {
		t.Fatalf("wrote %d logs, want 2", count)
	}

	if snapshots := countRows(t, database, "SELECT COUNT(*) FROM request_log_snapshots"); snapshots != 2 {
		t.Errorf("wrote %d snapshot rows, want 2", snapshots)
	}

	snapshot := runtimeMetrics.Snapshot()
	if snapshot.LogQueueDepth != 0 {
		t.Errorf("queue depth = %d, want 0", snapshot.LogQueueDepth)
	}
	if snapshot.LogWrittenTotal != 2 {
		t.Errorf("written total = %d, want 2", snapshot.LogWrittenTotal)
	}
	// Both entries land in one batch, because the second arrives inside the
	// collection window.
	if snapshot.LogWriteBatchesTotal != 1 {
		t.Errorf("batches = %d, want 1", snapshot.LogWriteBatchesTotal)
	}
	if snapshot.LogDroppedTotal != 0 || snapshot.LogWriteFailuresTotal != 0 {
		t.Errorf("dropped=%d failures=%d, want none",
			snapshot.LogDroppedTotal, snapshot.LogWriteFailuresTotal)
	}

	statsSnapshot := logStats.Snapshot()
	if statsSnapshot.TotalLogCount != 2 || statsSnapshot.LogCount24h != 2 {
		t.Errorf("stats = %d total / %d in 24h, want 2/2",
			statsSnapshot.TotalLogCount, statsSnapshot.LogCount24h)
	}

	received := make([]LogStreamEvent, 0, 2)
	for range 2 {
		select {
		case event := <-events:
			received = append(received, event)
		case <-time.After(time.Second):
			t.Fatalf("only received %d events", len(received))
		}
	}
	if received[0].Log.ID != 1 || received[1].Log.ID != 2 {
		t.Errorf("event ids = %d, %d, want 1, 2", received[0].Log.ID, received[1].Log.ID)
	}
	if received[0].Log.Method != "POST" || received[0].Log.Path != "/v1/responses" {
		t.Errorf("first event = %+v, want the first scheduled entry", received[0].Log)
	}
	if received[0].RecentRPM == nil || *received[0].RecentRPM != 2 {
		t.Errorf("recent rpm = %v, want 2", received[0].RecentRPM)
	}
	if received[0].RecentTPM == nil || *received[0].RecentTPM != 0 {
		t.Errorf("recent tpm = %v, want 0", received[0].RecentTPM)
	}
}

func TestLogWriterDropsEntriesRatherThanBlockingWhenTheQueueIsFull(t *testing.T) {
	database := loggingTestDB(t)
	runtimeMetrics := metrics.New()

	// A zero capacity is raised to one, so exactly one entry can be queued while
	// the writer is busy.
	ctx, cancel := context.WithCancel(context.Background())
	writer := NewLogWriter(ctx, database, runtimeMetrics, db.NewLogStatsCache(), 1, quota.NewTracker())

	// Stop the writer before scheduling, so nothing drains the queue.
	cancel()
	for range 64 {
		writer.Schedule(LogEntry{Method: "POST", Path: "/v1/responses"})
	}

	if dropped := runtimeMetrics.Snapshot().LogDroppedTotal; dropped == 0 {
		t.Error("a full queue did not drop any entry")
	}
}

func TestCleanupPassClearsBodiesBeyondTheKeepCount(t *testing.T) {
	database := loggingTestDB(t)
	ctx := context.Background()

	// Each body is its own chunk, so clearing a row frees exactly its chunk.
	for i := range 5 {
		if _, err := insertLogs(ctx, database, []LogEntry{{
			Method:            "POST",
			Path:              "/v1/responses",
			DownstreamRequest: textSnapshot(`{"method":"POST"}`, fmt.Sprintf("request %d", i)),
		}}, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	settings := models.DefaultRuntimeSettings()
	settings.LogBodyKeepCount = 2
	runtimeMetrics := metrics.New()
	RunCleanupPass(ctx, database, &settings, runtimeMetrics, db.NewLogStatsCache(), CleanupSteps{Chunks: true})

	if cleared := countRows(t, database,
		"SELECT COUNT(*) FROM request_log_snapshots WHERE bodies_cleared = 1"); cleared != 3 {
		t.Errorf("cleared %d rows, want the 3 beyond the keep count", cleared)
	}
	// Clearing deletes the body records, and collection the chunks only they
	// listed: that is what gives the space back.
	if bodies := countRows(t, database, "SELECT COUNT(*) FROM request_log_bodies"); bodies != 2 {
		t.Errorf("%d body records remain, want 2", bodies)
	}
	if chunks := countRows(t, database, "SELECT COUNT(*) FROM log_chunks"); chunks != 2 {
		t.Errorf("%d chunks remain, want the 2 the kept rows list", chunks)
	}

	// The newest rows keep their bodies.
	retained, _, err := db.GetLogSnapshot(ctx, database, 5, db.LogSnapshotDownstreamRequest)
	if err != nil || string(retained) != `{"body":{"byte_length":9,"text":"request 4"},"method":"POST"}` {
		t.Errorf("the newest payload = %s (%v)", retained, err)
	}
	// A cleared row keeps its metadata but loses the body.
	cleared, _, err := db.GetLogSnapshot(ctx, database, 1, db.LogSnapshotDownstreamRequest)
	if err != nil || string(cleared) != `{"body":{"cleared":true},"method":"POST"}` {
		t.Errorf("cleared snapshot = %s (%v), want its body replaced", cleared, err)
	}

	if snapshot := runtimeMetrics.Snapshot(); snapshot.CleanupRunsTotal != 1 ||
		snapshot.CleanupLastRowsCleared != 3 || snapshot.CleanupActive {
		t.Errorf("cleanup metrics = %+v, want one finished run clearing 3 rows", snapshot)
	}
}

func TestCommittedLogsAdvanceTheTokenQuotaCounter(t *testing.T) {
	database := loggingTestDB(t)
	ctx := context.Background()

	created, err := db.CreateToken(ctx, database, &models.APITokenIn{
		Name: "quota-client", Enabled: true, LimitExpression: "1M",
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	total := int32(1500)
	entry := LogEntry{
		Method: "POST", Path: "/v1/responses",
		DownstreamTokenID: &created.ID,
		TotalTokens:       &total,
	}
	if _, err := insertLogs(ctx, database, []LogEntry{entry, entry}, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The counter advances in the same transaction as the log row, so two
	// committed requests are counted exactly once each.
	reloaded, _, err := db.GetToken(ctx, database, created.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Quota.UsedTokens != 3000 {
		t.Errorf("used = %d, want 3000", reloaded.Quota.UsedTokens)
	}
}

func TestARequestWithoutUsageLeavesTheQuotaUntouched(t *testing.T) {
	database := loggingTestDB(t)
	ctx := context.Background()

	created, err := db.CreateToken(ctx, database, &models.APITokenIn{
		Name: "quota-client", Enabled: true, LimitExpression: "1M",
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	// A failed request reports no usage, so it must not consume budget.
	message := "upstream unreachable"
	status := int32(502)
	if _, err := insertLogs(ctx, database, []LogEntry{{
		Method: "POST", Path: "/v1/responses",
		DownstreamTokenID: &created.ID,
		StatusCode:        &status,
		Error:             &message,
	}}, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	reloaded, _, err := db.GetToken(ctx, database, created.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Quota.UsedTokens != 0 {
		t.Errorf("used = %d, want 0 for a request with no usage", reloaded.Quota.UsedTokens)
	}
}

// The token and channel ids are resolved when a request begins, and either may
// be deleted before its row is written. A dangling id failed the foreign key and
// rolled back every row batched with it, other tokens' usage included.
func TestARowForADeletedTokenOrChannelDoesNotSinkItsBatch(t *testing.T) {
	database := loggingTestDB(t)
	ctx := context.Background()

	gone, err := db.CreateToken(ctx, database, &models.APITokenIn{Name: "gone", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	kept, err := db.CreateToken(ctx, database, &models.APITokenIn{Name: "kept", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("DELETE FROM api_tokens WHERE id = ?", gone.ID); err != nil {
		t.Fatal(err)
	}

	missingChannel := int64(999)
	hundred, fifty := int32(100), int32(50)
	if _, err := insertLogs(ctx, database, []LogEntry{
		{Method: "POST", Path: "responses", DownstreamTokenID: &gone.ID, TotalTokens: &hundred},
		{Method: "POST", Path: "responses", DownstreamTokenID: &kept.ID,
			UpstreamID: &missingChannel, TotalTokens: &fifty},
	}, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var rows, danglingIDs int64
	if err := database.QueryRow("SELECT COUNT(*) FROM request_logs").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM request_logs
        WHERE downstream_token_id = ? OR upstream_id = ?`, gone.ID, missingChannel).
		Scan(&danglingIDs); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || danglingIDs != 0 {
		t.Errorf("rows=%d dangling=%d, want both rows with the vanished ids cleared", rows, danglingIDs)
	}
	reloaded, _, err := db.GetToken(ctx, database, kept.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Quota.UsedTokens != 50 {
		t.Errorf("the surviving token used %d, want 50", reloaded.Quota.UsedTokens)
	}
}

// idleLogWriter is a writer whose batches the test drives itself.
func idleLogWriter(quotas *quota.Tracker) *LogWriter {
	return &LogWriter{
		entries: make(chan LogEntry, 1),
		metrics: metrics.New(),
		events:  newEventBroker(),
		quotas:  quotas,
		done:    make(chan struct{}),
	}
}

// A full queue drops the row, but the usage is still owed: dropping it too let a
// token under load spend without the quota seeing it.
func TestADroppedEntryStillReachesTheQuota(t *testing.T) {
	database := loggingTestDB(t)
	ctx := context.Background()
	created, err := db.CreateToken(ctx, database, &models.APITokenIn{Name: "busy", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	quotas := quota.NewTracker()
	writer := idleLogWriter(quotas)
	used := int32(700)
	entry := LogEntry{Method: "POST", Path: "responses", DownstreamTokenID: &created.ID, TotalTokens: &used}
	writer.Schedule(entry)
	writer.Schedule(entry) // the queue holds one; this one is dropped

	if held := quotas.Outstanding(created.ID); held != 1400 {
		t.Fatalf("held %d, want both requests weighed until committed", held)
	}

	writer.flush(ctx, database, db.NewLogStatsCache(), []LogEntry{<-writer.entries})

	reloaded, _, err := db.GetToken(ctx, database, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Quota.UsedTokens != 1400 {
		t.Errorf("used = %d, want the dropped request counted too", reloaded.Quota.UsedTokens)
	}
	if held := quotas.Outstanding(created.ID); held != 0 {
		t.Errorf("still held %d after commit", held)
	}
}

// A batch that fails to commit loses its rows, not its usage: the next write
// applies it, even one with no rows of its own.
func TestUsageOfAFailedBatchIsAppliedLater(t *testing.T) {
	database := loggingTestDB(t)
	ctx := context.Background()
	created, err := db.CreateToken(ctx, database, &models.APITokenIn{Name: "unlucky", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	broken, err := sql.Open("sqlite", "file:"+t.Name()+"-broken?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	broken.Close()

	quotas := quota.NewTracker()
	writer := idleLogWriter(quotas)
	used := int32(300)
	entry := LogEntry{Method: "POST", Path: "responses", DownstreamTokenID: &created.ID, TotalTokens: &used}
	quotas.Meter(created.ID, 300)
	writer.flush(ctx, broken, db.NewLogStatsCache(), []LogEntry{entry})

	if held := quotas.Outstanding(created.ID); held != 300 {
		t.Fatalf("held %d after the failed write, want the usage still held", held)
	}

	writer.flush(ctx, database, db.NewLogStatsCache(), nil)
	reloaded, _, err := db.GetToken(ctx, database, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Quota.UsedTokens != 300 {
		t.Errorf("used = %d, want the failed batch's usage applied", reloaded.Quota.UsedTokens)
	}
	if held := quotas.Outstanding(created.ID); held != 0 {
		t.Errorf("still held %d after it was applied", held)
	}
}

func TestSchedulingAfterCloseDropsTheEntryRatherThanPanicking(t *testing.T) {
	database := loggingTestDB(t)
	runtimeMetrics := metrics.New()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := NewLogWriter(ctx, database, runtimeMetrics, db.NewLogStatsCache(), 8, quota.NewTracker())
	writer.Close()

	// A stream still running when the server stopped waiting for it schedules
	// its log after shutdown has closed the queue. Sending on a closed channel
	// panics, and the select's default case does not prevent that.
	writer.Schedule(LogEntry{Method: "POST", Path: "/v1/responses"})

	if dropped := runtimeMetrics.Snapshot().LogDroppedTotal; dropped != 1 {
		t.Errorf("dropped = %d, want the entry to be dropped once", dropped)
	}
}

func TestSchedulingConcurrentlyWithCloseNeverPanics(t *testing.T) {
	database := loggingTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := NewLogWriter(ctx, database, metrics.New(), db.NewLogStatsCache(), 8, quota.NewTracker())

	// Close races the schedulers rather than following them, which is the order
	// a shutdown that timed out on its in-flight requests produces.
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 32 {
				writer.Schedule(LogEntry{Method: "POST", Path: "/v1/responses"})
			}
		})
	}
	wg.Go(writer.Close)
	wg.Wait()
}

// A subscriber with no room loses the event and is marked, once, to reload.
func TestAFullSubscriberIsToldItMissedRows(t *testing.T) {
	broker := newEventBroker()
	subscription := broker.subscribe()
	defer subscription.Close()

	for range logEventChannelCapacity {
		broker.publish(LogStreamEvent{})
	}
	if subscription.TakeMissed() {
		t.Fatal("marked before anything was dropped")
	}

	broker.publish(LogStreamEvent{})
	if !subscription.TakeMissed() {
		t.Fatal("a dropped event left no mark")
	}
	if subscription.TakeMissed() {
		t.Error("the mark survived being taken")
	}
}
