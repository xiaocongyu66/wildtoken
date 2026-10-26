package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/liguangsheng/wildtoken/internal/apperr"
	"github.com/liguangsheng/wildtoken/internal/logbody"
	"github.com/liguangsheng/wildtoken/internal/models"
)

const (
	// logBodyCleanupBatchSize is how many rows one batch clears.
	//
	// Clearing deletes a range of body records and flips a flag; no JSON is
	// parsed. A batch has to outpace a gateway that keeps writing: at eight rows
	// per 25ms an earlier version cleared about 320 a second, and a busier
	// gateway then grew the stored bodies without the keep-count policy ever
	// catching up.
	logBodyCleanupBatchSize  int64 = 1000
	logBodyCleanupBatchPause       = 25 * time.Millisecond
	// logDeleteBatchSize is how many expired log rows one delete removes.
	//
	// Larger than the body batch because the work per row is a delete rather
	// than a JSON rewrite, and retention has a whole window to get through.
	logDeleteBatchSize  int64 = 500
	logDeleteBatchPause       = 25 * time.Millisecond
	// actualModelExpression prefers the model actually sent to the upstream.
	// `model` is retained as a compatibility fallback for logs written before
	// `upstream_model` was added.
	actualModelExpression = "COALESCE(NULLIF(TRIM(upstream_model), ''), NULLIF(TRIM(model), ''))"
)

// logListColumns is the projection backing RequestLogOut.
const logListColumns = `id, created_at, method, path,
                downstream_token_id, downstream_token_name,
                client_ip, client_type,
                upstream_id, upstream_name, model, request_model, upstream_model,
                reasoning_effort, upstream_reasoning_effort, response_reasoning_effort,
                stream, status_code,
                prompt_tokens, completion_tokens, total_tokens,
                prompt_cached_tokens, cache_creation_tokens, completion_reasoning_tokens,
                duration_ms, first_token_ms,
                error`

// LogTopWindow is a ranking window accepted by the top-stats endpoint.
type LogTopWindow int

const (
	LogTopWindowToday LogTopWindow = iota
	LogTopWindowOneDay
	LogTopWindowThreeDays
	LogTopWindowSevenDays
	LogTopWindowThirtyDays
	LogTopWindowAll
	LogTopWindowCustom
)

// ParseLogTopWindow maps a query value onto a window.
func ParseLogTopWindow(value string) (LogTopWindow, bool) {
	switch value {
	case "today":
		return LogTopWindowToday, true
	case "1d":
		return LogTopWindowOneDay, true
	case "3d":
		return LogTopWindowThreeDays, true
	case "7d":
		return LogTopWindowSevenDays, true
	case "30d":
		return LogTopWindowThirtyDays, true
	case "all":
		return LogTopWindowAll, true
	case "default":
		return LogTopWindowThirtyDays, true
	case "custom":
		return LogTopWindowCustom, true
	default:
		return 0, false
	}
}

func (w LogTopWindow) QueryValue() string {
	switch w {
	case LogTopWindowToday:
		return "today"
	case LogTopWindowOneDay:
		return "1d"
	case LogTopWindowThreeDays:
		return "3d"
	case LogTopWindowSevenDays:
		return "7d"
	case LogTopWindowThirtyDays:
		return "30d"
	case LogTopWindowAll:
		return "all"
	case LogTopWindowCustom:
		return "custom"
	default:
		return ""
	}
}

func (w LogTopWindow) cutoffExpression() string {
	switch w {
	case LogTopWindowToday:
		return "datetime('now', 'localtime', 'start of day', 'utc')"
	case LogTopWindowOneDay:
		return "datetime('now', '-1 day')"
	case LogTopWindowThreeDays:
		return "datetime('now', '-3 days')"
	case LogTopWindowSevenDays:
		return "datetime('now', '-7 days')"
	case LogTopWindowThirtyDays:
		return "datetime('now', '-30 days')"
	default:
		return ""
	}
}

func appendLogTimePredicate(query *strings.Builder, args []any, window LogTopWindow,
	startAt, endAt string) ([]any, error) {
	switch window {
	case LogTopWindowAll:
		query.WriteString("1 = 1")
	case LogTopWindowCustom:
		if startAt == "" || endAt == "" {
			return nil, apperr.BadRequest("custom window requires start_date and end_date")
		}
		query.WriteString("created_at >= ? AND created_at < ?")
		args = append(args, startAt, endAt)
	case LogTopWindowToday:
		// A caller that knows the operator's zone passes the start of their day;
		// without one, "today" is the server's.
		if startAt == "" {
			fmt.Fprintf(query, "created_at >= %s", window.cutoffExpression())
		} else {
			query.WriteString("created_at >= ?")
			args = append(args, startAt)
		}
	default:
		cutoff := window.cutoffExpression()
		if cutoff == "" {
			return nil, apperr.BadRequest("invalid log statistics window")
		}
		fmt.Fprintf(query, "created_at >= %s", cutoff)
	}
	return args, nil
}

// detachLogBatch bounds one pass of detachLogs.
const detachLogBatch = 5000

// detachLogs clears one row's references from request_logs in short
// transactions, ahead of deleting that row.
//
// ON DELETE SET NULL does the same inside the DELETE, as one statement holding
// the write lock throughout: 0.9s per 300k logs on a desktop. Past the
// five-second busy timeout an edit made meanwhile fails as "database is
// locked"; past the log writer's fifteen-second deadline a batch of logs is
// lost. The DELETE afterwards only has what arrived in between left to clear.
//
// column is one of this package's own column names, never caller input.
func detachLogs(ctx context.Context, database *sql.DB, column string, id int64) error {
	query := fmt.Sprintf(`UPDATE request_logs SET %[1]s = NULL
        WHERE id IN (SELECT id FROM request_logs WHERE %[1]s = ? LIMIT ?)`, column)
	for {
		result, err := database.ExecContext(ctx, query, id, detachLogBatch)
		if err != nil {
			return apperr.Database(err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return apperr.Database(err)
		}
		if affected < detachLogBatch {
			return nil
		}
	}
}

// LogFilter narrows a log listing. A nil field means the filter is not applied.
type LogFilter struct {
	UpstreamID *int64
	Search     *string
	Status     *string
	ClientType *string
}

// appendFilters adds the WHERE fragments and their bind values.
func (f LogFilter) appendFilters(query *strings.Builder, args []any) []any {
	if f.UpstreamID != nil {
		query.WriteString(" AND upstream_id = ?")
		args = append(args, *f.UpstreamID)
	}
	if f.Search != nil {
		// The wildcards are escaped so an operator searching for a literal '%'
		// or '_' does not match every row.
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(*f.Search)
		pattern := "%" + escaped + "%"
		query.WriteString(` AND (LOWER(model) LIKE LOWER(?) ESCAPE '\'` +
			` OR LOWER(request_model) LIKE LOWER(?) ESCAPE '\'` +
			` OR LOWER(upstream_model) LIKE LOWER(?) ESCAPE '\'` +
			` OR LOWER(upstream_name) LIKE LOWER(?) ESCAPE '\'` +
			` OR LOWER(downstream_token_name) LIKE LOWER(?) ESCAPE '\'` +
			` OR LOWER(error) LIKE LOWER(?) ESCAPE '\'` +
			` OR CAST(id AS TEXT) LIKE ? ESCAPE '\'` +
			` OR CAST(status_code AS TEXT) LIKE ? ESCAPE '\')`)
		for range 8 {
			args = append(args, pattern)
		}
	}
	if f.Status != nil {
		switch *f.Status {
		case "2xx":
			query.WriteString(" AND status_code BETWEEN 200 AND 299")
		case "4xx":
			query.WriteString(" AND status_code BETWEEN 400 AND 499")
		case "5xx":
			query.WriteString(" AND status_code BETWEEN 500 AND 599")
		case "none":
			query.WriteString(" AND status_code IS NULL")
		}
	}
	if f.ClientType != nil {
		query.WriteString(" AND client_type = ?")
		args = append(args, *f.ClientType)
	}
	return args
}

// LogCursor is the keyset position a page continues from.
type LogCursor struct {
	CreatedAt string
	ID        int64
}

func scanLogListRow(row interface{ Scan(...any) error }) (models.RequestLogOut, error) {
	var entry models.RequestLogOut
	var downstreamTokenID, upstreamID sql.NullInt64
	var downstreamTokenName, upstreamName, model, requestModel, upstreamModel sql.NullString
	var reasoningEffort, upstreamReasoningEffort, responseReasoningEffort, logError sql.NullString
	var statusCode, promptTokens, completionTokens, totalTokens sql.NullInt64
	var promptCachedTokens, cacheCreationTokens, completionReasoningTokens sql.NullInt64
	var durationMs, firstTokenMs sql.NullInt64
	var clientIP sql.NullString

	err := row.Scan(&entry.ID, &entry.CreatedAt, &entry.Method, &entry.Path,
		&downstreamTokenID, &downstreamTokenName, &clientIP, &entry.ClientType,
		&upstreamID, &upstreamName, &model, &requestModel, &upstreamModel,
		&reasoningEffort, &upstreamReasoningEffort, &responseReasoningEffort,
		&entry.Stream, &statusCode,
		&promptTokens, &completionTokens, &totalTokens,
		&promptCachedTokens, &cacheCreationTokens, &completionReasoningTokens,
		&durationMs, &firstTokenMs, &logError)
	if err != nil {
		return entry, err
	}

	entry.DownstreamTokenID = nullInt64Ptr(downstreamTokenID)
	entry.UpstreamID = nullInt64Ptr(upstreamID)
	entry.DownstreamTokenName = nullStringPtr(downstreamTokenName)
	entry.ClientIP = nullStringPtr(clientIP)
	entry.UpstreamName = nullStringPtr(upstreamName)
	entry.Model = nullStringPtr(model)
	entry.RequestModel = nullStringPtr(requestModel)
	entry.UpstreamModel = nullStringPtr(upstreamModel)
	entry.ReasoningEffort = nullStringPtr(reasoningEffort)
	entry.UpstreamReasoningEffort = nullStringPtr(upstreamReasoningEffort)
	entry.ResponseReasoningEffort = nullStringPtr(responseReasoningEffort)
	entry.Error = nullStringPtr(logError)
	entry.StatusCode = nullInt32Ptr(statusCode)
	entry.PromptTokens = nullInt32Ptr(promptTokens)
	entry.CompletionTokens = nullInt32Ptr(completionTokens)
	entry.TotalTokens = nullInt32Ptr(totalTokens)
	entry.PromptCachedTokens = nullInt32Ptr(promptCachedTokens)
	entry.CacheCreationTokens = nullInt32Ptr(cacheCreationTokens)
	entry.CompletionReasoningTokens = nullInt32Ptr(completionReasoningTokens)
	entry.DurationMs = nullInt32Ptr(durationMs)
	entry.FirstTokenMs = nullInt32Ptr(firstTokenMs)
	return entry, nil
}

// LogQueryTimeout bounds how long one log listing may run.
//
// None of the filters can use an index: client_type and status_code have none,
// and the search is a leading-wildcard LIKE, which nothing can. LIMIT bounds the
// rows returned, not the rows examined, so a filter matching little or nothing
// reads the whole table — holding one of the few pooled connections while the
// proxy path waits for one. The deadline turns that into an error the operator
// sees rather than a stall the gateway absorbs.
const LogQueryTimeout = 10 * time.Second

// ListLogs returns one page, newest first. A cursor takes precedence over the
// offset, so a page boundary stays stable while new rows arrive.
func ListLogs(ctx context.Context, database *sql.DB, limit, offset int32,
	cursor *LogCursor, filter LogFilter) ([]models.RequestLogOut, error) {
	ctx, cancel := context.WithTimeout(ctx, LogQueryTimeout)
	defer cancel()

	var query strings.Builder
	query.WriteString("SELECT " + logListColumns + " FROM request_logs WHERE 1 = 1")

	args := filter.appendFilters(&query, nil)
	if cursor != nil {
		query.WriteString(" AND (created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	query.WriteString(" ORDER BY created_at DESC, id DESC LIMIT ?")
	args = append(args, limit)
	if cursor == nil {
		query.WriteString(" OFFSET ?")
		args = append(args, offset)
	}

	rows, err := database.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, apperr.Database(err)
	}
	defer rows.Close()

	entries := []models.RequestLogOut{}
	for rows.Next() {
		entry, err := scanLogListRow(rows)
		if err != nil {
			return nil, apperr.Database(err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Database(err)
	}
	return entries, nil
}

// GetLogDetail returns one log with its captured payloads, or ok=false.
func GetLogDetail(ctx context.Context, database *sql.DB, logID int64) (models.RequestLogDetailOut, bool, error) {
	row := database.QueryRowContext(ctx, `SELECT l.id, l.created_at, l.method, l.path,
              l.downstream_token_id, l.downstream_token_name,
              l.client_ip, l.client_type,
              l.upstream_id, l.upstream_name, l.model,
              l.request_model, l.upstream_model,
              l.reasoning_effort, l.upstream_reasoning_effort,
              l.response_reasoning_effort,
              l.stream, l.status_code,
              l.prompt_tokens, l.completion_tokens, l.total_tokens,
              l.prompt_cached_tokens, l.cache_creation_tokens, l.completion_reasoning_tokens,
              l.duration_ms, l.first_token_ms,
              l.error
       FROM request_logs AS l
       WHERE l.id = ?`, logID)

	var detail models.RequestLogDetailOut
	var downstreamTokenID, upstreamID sql.NullInt64
	var downstreamTokenName, upstreamName, model, requestModel, upstreamModel sql.NullString
	var reasoningEffort, upstreamReasoningEffort, responseReasoningEffort, logError sql.NullString
	var statusCode, promptTokens, completionTokens, totalTokens sql.NullInt64
	var promptCachedTokens, cacheCreationTokens, completionReasoningTokens sql.NullInt64
	var durationMs, firstTokenMs sql.NullInt64
	var clientIP sql.NullString

	err := row.Scan(&detail.ID, &detail.CreatedAt, &detail.Method, &detail.Path,
		&downstreamTokenID, &downstreamTokenName, &clientIP, &detail.ClientType,
		&upstreamID, &upstreamName, &model, &requestModel, &upstreamModel,
		&reasoningEffort, &upstreamReasoningEffort, &responseReasoningEffort,
		&detail.Stream, &statusCode,
		&promptTokens, &completionTokens, &totalTokens,
		&promptCachedTokens, &cacheCreationTokens, &completionReasoningTokens,
		&durationMs, &firstTokenMs, &logError)
	if errors.Is(err, sql.ErrNoRows) {
		return detail, false, nil
	}
	if err != nil {
		return detail, false, apperr.Database(err)
	}

	detail.DownstreamTokenID = nullInt64Ptr(downstreamTokenID)
	detail.UpstreamID = nullInt64Ptr(upstreamID)
	detail.DownstreamTokenName = nullStringPtr(downstreamTokenName)
	detail.ClientIP = nullStringPtr(clientIP)
	detail.UpstreamName = nullStringPtr(upstreamName)
	detail.Model = nullStringPtr(model)
	detail.RequestModel = nullStringPtr(requestModel)
	detail.UpstreamModel = nullStringPtr(upstreamModel)
	detail.ReasoningEffort = nullStringPtr(reasoningEffort)
	detail.UpstreamReasoningEffort = nullStringPtr(upstreamReasoningEffort)
	detail.ResponseReasoningEffort = nullStringPtr(responseReasoningEffort)
	detail.Error = nullStringPtr(logError)
	detail.StatusCode = nullInt32Ptr(statusCode)
	detail.PromptTokens = nullInt32Ptr(promptTokens)
	detail.CompletionTokens = nullInt32Ptr(completionTokens)
	detail.TotalTokens = nullInt32Ptr(totalTokens)
	detail.PromptCachedTokens = nullInt32Ptr(promptCachedTokens)
	detail.CacheCreationTokens = nullInt32Ptr(cacheCreationTokens)
	detail.CompletionReasoningTokens = nullInt32Ptr(completionReasoningTokens)
	detail.DurationMs = nullInt32Ptr(durationMs)
	detail.FirstTokenMs = nullInt32Ptr(firstTokenMs)

	snapshots, found, err := loadLogSnapshots(ctx, database, logID,
		logbody.SlotDownstreamRequest, logbody.SlotUpstreamRequest,
		logbody.SlotUpstreamResponse, logbody.SlotDownstreamResponse)
	if err != nil || !found {
		// Not found here means the log was deleted between the two reads.
		return detail, false, err
	}
	detail.DownstreamRequest = snapshots[logbody.SlotDownstreamRequest]
	detail.UpstreamRequest = snapshots[logbody.SlotUpstreamRequest]
	detail.UpstreamResponse = snapshots[logbody.SlotUpstreamResponse]
	detail.DownstreamResponse = snapshots[logbody.SlotDownstreamResponse]
	return detail, true, nil
}

// LogSnapshotField names one of the four captured payloads.
type LogSnapshotField string

const (
	LogSnapshotDownstreamRequest  LogSnapshotField = "downstream_request"
	LogSnapshotUpstreamRequest    LogSnapshotField = "upstream_request"
	LogSnapshotUpstreamResponse   LogSnapshotField = "upstream_response"
	LogSnapshotDownstreamResponse LogSnapshotField = "downstream_response"
)

// ParseLogSnapshotField maps a URL segment onto a field, or ok=false.
func ParseLogSnapshotField(name string) (LogSnapshotField, bool) {
	switch field := LogSnapshotField(name); field {
	case LogSnapshotDownstreamRequest, LogSnapshotUpstreamRequest,
		LogSnapshotUpstreamResponse, LogSnapshotDownstreamResponse:
		return field, true
	}
	return "", false
}

// snapshotSlots places each field in a stored body record.
var snapshotSlots = map[LogSnapshotField]int{
	LogSnapshotDownstreamRequest:  logbody.SlotDownstreamRequest,
	LogSnapshotUpstreamRequest:    logbody.SlotUpstreamRequest,
	LogSnapshotUpstreamResponse:   logbody.SlotUpstreamResponse,
	LogSnapshotDownstreamResponse: logbody.SlotDownstreamResponse,
}

// GetLogSnapshot returns one captured payload, or ok=false when the log is
// missing. A log without that payload is found with a nil snapshot.
//
// The console opens a log with its metadata already in hand and fetches each
// payload as the reader asks for it, so this reads only the chunks that
// payload needs instead of all four bodies.
func GetLogSnapshot(ctx context.Context, database *sql.DB, logID int64, field LogSnapshotField) (json.RawMessage, bool, error) {
	slot, ok := snapshotSlots[field]
	if !ok {
		return nil, false, apperr.BadRequest("unknown snapshot field")
	}
	snapshots, found, err := loadLogSnapshots(ctx, database, logID, slot)
	if err != nil || !found {
		return nil, found, err
	}
	return snapshots[slot], true, nil
}

// topLogCountSpec describes one ranking query.
type topLogCountSpec struct {
	nameExpression   string
	groupExpression  string
	sourceFilter     string
	metricExpression string
	metricFilter     string
	// exposeGroupID surfaces the numeric group key as `id` (channel rankings).
	exposeGroupID bool
	// withLatency attaches per-group average duration and error rate to the
	// ranking rows (request-count rankings of models/channels).
	withLatency bool
}

// channelNameExpression prefers a non-empty upstream_name snapshot, else "#<id>".
const channelNameExpression = `CASE
              WHEN upstream_name IS NOT NULL AND TRIM(upstream_name) <> '' THEN TRIM(upstream_name)
              ELSE '#' || upstream_id
           END`

const modelSourceFilter = "COALESCE(NULLIF(TRIM(upstream_model), ''), NULLIF(TRIM(model), '')) IS NOT NULL"

const tokenMetricFilter = "total_tokens IS NOT NULL AND total_tokens > 0"

// TopLogStats ranks models and channels by request count and token usage.
func TopLogStats(ctx context.Context, database *sql.DB, window LogTopWindow, limit int64) (models.RequestLogTopStatsOut, error) {
	return topLogStats(ctx, database, window, "", "", limit)
}

// TopLogStatsRange ranks logs in a window the caller resolved: a custom
// interval, or "today" from the operator's own midnight.
func TopLogStatsRange(ctx context.Context, database *sql.DB, window LogTopWindow,
	startAt, endAt string, limit int64) (models.RequestLogTopStatsOut, error) {
	return topLogStats(ctx, database, window, startAt, endAt, limit)
}

// TopLogStatsCustom ranks logs in the half-open UTC interval [startAt, endAt).
func TopLogStatsCustom(ctx context.Context, database *sql.DB, startAt, endAt string,
	limit int64) (models.RequestLogTopStatsOut, error) {
	return topLogStats(ctx, database, LogTopWindowCustom, startAt, endAt, limit)
}

func topLogStats(ctx context.Context, database *sql.DB, window LogTopWindow,
	startAt, endAt string, limit int64) (models.RequestLogTopStatsOut, error) {
	limit = min(max(limit, 1), 20)

	// Channels aggregate by upstream_id so renamed or same-name channels stay
	// distinct.
	specs := [4]topLogCountSpec{
		{
			nameExpression:   actualModelExpression,
			groupExpression:  actualModelExpression,
			sourceFilter:     modelSourceFilter,
			metricExpression: "1",
			withLatency:      true,
		},
		{
			nameExpression:   channelNameExpression,
			groupExpression:  "upstream_id",
			sourceFilter:     "upstream_id IS NOT NULL",
			metricExpression: "1",
			exposeGroupID:    true,
		},
		{
			nameExpression:   actualModelExpression,
			groupExpression:  actualModelExpression,
			sourceFilter:     modelSourceFilter,
			metricExpression: "COALESCE(total_tokens, 0)",
			metricFilter:     tokenMetricFilter,
		},
		{
			nameExpression:   channelNameExpression,
			groupExpression:  "upstream_id",
			sourceFilter:     "upstream_id IS NOT NULL",
			metricExpression: "COALESCE(total_tokens, 0)",
			metricFilter:     tokenMetricFilter,
			exposeGroupID:    true,
		},
	}

	var rankings [4][]models.RequestLogTopItemOut
	for i, spec := range specs {
		items, err := topLogCounts(ctx, database, window, startAt, endAt, spec, limit)
		if err != nil {
			return models.RequestLogTopStatsOut{}, err
		}
		rankings[i] = items
	}

	return models.RequestLogTopStatsOut{
		Window:        window.QueryValue(),
		Models:        rankings[0],
		Channels:      rankings[1],
		ModelTokens:   rankings[2],
		ChannelTokens: rankings[3],
	}, nil
}

func topLogCounts(ctx context.Context, database *sql.DB, window LogTopWindow,
	startAt, endAt string, spec topLogCountSpec, limit int64) ([]models.RequestLogTopItemOut, error) {
	// Rows group by groupExpression (e.g. upstream_id) but surface a display
	// name. When several names share one group key, MAX(name) picks a stable
	// non-null label.
	idSelect := "CAST(NULL AS INTEGER) AS id"
	if spec.exposeGroupID {
		idSelect = "CAST(group_key AS INTEGER) AS id"
	}
	// Latency context rides along as extra per-row columns so one pass feeds
	// both the ranking metric and the duration/error aggregates.
	rowExtras := ""
	outerExtras := ""
	if spec.withLatency {
		rowExtras = `, CASE WHEN duration_ms IS NOT NULL AND duration_ms >= 0 THEN duration_ms END AS dur,
			CASE WHEN status_code IS NULL OR status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END AS is_err`
		outerExtras = `, AVG(dur) AS avg_dur,
			CAST(SUM(is_err) AS REAL) / COUNT(*) AS error_rate,
			SUM(CASE WHEN dur IS NOT NULL THEN 1 ELSE 0 END) AS dur_count`
	}

	var query strings.Builder
	fmt.Fprintf(&query, "SELECT MAX(name) AS name, SUM(value) AS count, %s%s FROM (SELECT %s AS group_key, %s AS name, %s AS value%s FROM request_logs WHERE ",
		idSelect, outerExtras, spec.groupExpression, spec.nameExpression, spec.metricExpression, rowExtras)
	args, err := appendLogTimePredicate(&query, nil, window, startAt, endAt)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(&query, " AND (%s)", spec.sourceFilter)
	if spec.metricFilter != "" {
		fmt.Fprintf(&query, " AND (%s)", spec.metricFilter)
	}
	query.WriteString(`) WHERE group_key IS NOT NULL AND name IS NOT NULL AND name <> '' ` +
		`GROUP BY group_key HAVING count > 0 ` +
		`ORDER BY count DESC, name COLLATE NOCASE ASC LIMIT ?`)
	args = append(args, limit)

	rows, err := database.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, apperr.Database(err)
	}
	defer rows.Close()

	items := []models.RequestLogTopItemOut{}
	for rows.Next() {
		var item models.RequestLogTopItemOut
		var id sql.NullInt64
		var avgDur, errorRate sql.NullFloat64
		var durCount sql.NullInt64
		var scanDest []any
		if spec.withLatency {
			scanDest = []any{&item.Name, &item.Count, &id, &avgDur, &errorRate, &durCount}
		} else {
			scanDest = []any{&item.Name, &item.Count, &id}
		}
		if err := rows.Scan(scanDest...); err != nil {
			return nil, apperr.Database(err)
		}
		if spec.exposeGroupID && id.Valid {
			value := id.Int64
			item.ID = &value
		}
		if durCount.Valid && durCount.Int64 > 0 && avgDur.Valid {
			avg := avgDur.Float64
			item.AvgDurationMs = &avg
		}
		if errorRate.Valid {
			rate := errorRate.Float64
			item.ErrorRate = &rate
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Database(err)
	}
	return items, nil
}

// CleanupMetricsRecorder is the metrics subset log cleanup reports to.
type CleanupMetricsRecorder interface {
	RecordSlowDBOperation()
	RecordCleanupBatch(rowsCleared uint64)
}

// ClearOldLogBodies drops stored bodies beyond the newest keepCount rows,
// working in bounded batches so the write lock is never held for long.
func ClearOldLogBodies(ctx context.Context, database *sql.DB, keepCount int64,
	recorder CleanupMetricsRecorder) (uint64, error) {
	var totalAffected uint64
	for {
		batchStartedAt := time.Now()
		affected, err := clearOldLogBodiesBatch(ctx, database, keepCount, logBodyCleanupBatchSize)
		if err != nil {
			return totalAffected, err
		}
		if recorder != nil {
			if time.Since(batchStartedAt) >= slowDBOperationThreshold {
				recorder.RecordSlowDBOperation()
			}
			recorder.RecordCleanupBatch(affected)
		}
		totalAffected += affected

		if affected == 0 || affected < uint64(logBodyCleanupBatchSize) {
			return totalAffected, nil
		}

		select {
		case <-ctx.Done():
			// Shutdown, not completion. Reported as such so the cleanup pass
			// does not record a successful run it did not finish.
			return totalAffected, ctx.Err()
		case <-time.After(logBodyCleanupBatchPause):
		}
	}
}

// clearOldLogBodiesBatch drops the bodies of up to batchSize of the oldest rows
// outside the newest keepCount that still have them.
//
// The window is counted on the partial index of rows with bodies, by id: the
// keepCount-th newest of them is the threshold, and everything below it goes.
// Dropping a row's bodies deletes its body record, which frees the space once
// CollectLogChunks takes the chunks only it listed; the metadata stays.
func clearOldLogBodiesBatch(ctx context.Context, database *sql.DB, keepCount, batchSize int64) (uint64, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, apperr.Database(err)
	}
	defer tx.Rollback()

	var threshold int64
	err = tx.QueryRowContext(ctx, `SELECT request_log_id FROM request_log_snapshots
       WHERE bodies_cleared = 0
       ORDER BY request_log_id DESC
       LIMIT 1 OFFSET ?`, keepCount-1).Scan(&threshold)
	if errors.Is(err, sql.ErrNoRows) {
		// Fewer rows than the window holds.
		return 0, nil
	}
	if err != nil {
		return 0, apperr.Database(err)
	}

	var first, last, count int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(request_log_id), 0),
              COALESCE(MAX(request_log_id), 0), COUNT(*)
       FROM (SELECT request_log_id FROM request_log_snapshots
             WHERE bodies_cleared = 0 AND request_log_id < ?
             ORDER BY request_log_id
             LIMIT ?)`, threshold, batchSize).Scan(&first, &last, &count)
	if err != nil {
		return 0, apperr.Database(err)
	}
	if count == 0 {
		return 0, nil
	}

	// Every row in [first, last] is below the threshold, so a range is exact.
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM request_log_bodies WHERE request_log_id BETWEEN ? AND ?", first, last); err != nil {
		return 0, apperr.Database(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE request_log_snapshots SET bodies_cleared = 1
       WHERE bodies_cleared = 0 AND request_log_id BETWEEN ? AND ?`, first, last); err != nil {
		return 0, apperr.Database(err)
	}
	if err := tx.Commit(); err != nil {
		return 0, apperr.Database(err)
	}
	return uint64(count), nil
}

// ReclaimFreePages returns free SQLite pages to the filesystem when incremental
// auto-vacuum is enabled.
//
// Existing databases need one full VACUUM after switching from NONE to
// INCREMENTAL; until then this safely acts as a no-op.
func ReclaimFreePages(ctx context.Context, database *sql.DB, maxPages uint32) (uint64, error) {
	if maxPages == 0 {
		return 0, nil
	}

	var autoVacuum int64
	if err := database.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&autoVacuum); err != nil {
		return 0, apperr.Database(err)
	}
	if autoVacuum != 2 {
		return 0, nil
	}

	var before int64
	if err := database.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&before); err != nil {
		return 0, apperr.Database(err)
	}
	if before == 0 {
		return 0, nil
	}

	if _, err := database.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", maxPages)); err != nil {
		return 0, apperr.Database(err)
	}

	var after int64
	if err := database.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&after); err != nil {
		return 0, apperr.Database(err)
	}
	if after > before {
		return 0, nil
	}
	return uint64(before - after), nil
}

// DeleteOldLogs drops logs past the retention window in bounded batches.
//
// Deleting the whole window in one statement held the write lock for as long as
// that took, and the amount waiting is not bounded by anything the gateway
// controls: enabling retention for the first time, or shortening it from ten
// years to a week, presents every row at once. Meanwhile every proxied request's
// log write — and the quota increment it carries — queues behind it. Batching
// puts an upper bound on how long any one transaction can block them, at the
// cost of taking several to finish, which nothing depends on.
//
// The payload rows go with their parent through ON DELETE CASCADE, so only the
// parents are named here.
func DeleteOldLogs(ctx context.Context, database *sql.DB, retentionDays int64) error {
	for {
		affected, err := deleteOldLogsBatch(ctx, database, retentionDays, logDeleteBatchSize)
		if err != nil {
			return err
		}
		if affected < logDeleteBatchSize {
			return nil
		}

		// Yield the write lock between batches so a proxied request's log does
		// not wait out the whole cleanup. A cancellation is reported: taken as
		// done, the caller went on to queries the cancellation failed, and a
		// shutdown was logged as a cleanup error.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(logDeleteBatchPause):
		}
	}
}

// deleteOldLogsBatch removes up to batchSize expired rows and reports how many
// it deleted.
func deleteOldLogsBatch(ctx context.Context, database *sql.DB,
	retentionDays, batchSize int64) (int64, error) {
	result, err := database.ExecContext(ctx, `DELETE FROM request_logs
       WHERE id IN (
           SELECT id FROM request_logs
           WHERE created_at < datetime('now', '-' || ? || ' days')
           LIMIT ?
       )`, retentionDays, batchSize)
	if err != nil {
		return 0, apperr.Database(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, apperr.Database(err)
	}
	return affected, nil
}

func nullStringPtr(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func nullInt64Ptr(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func nullInt32Ptr(value sql.NullInt64) *int32 {
	if !value.Valid {
		return nil
	}
	converted := int32(value.Int64)
	return &converted
}
