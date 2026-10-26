package db

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/liguangsheng/wildtoken/internal/models"
)

func itoa(value int) string { return strconv.Itoa(value) }

func nowUnix() int64 { return time.Now().UTC().Unix() }

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: true}
}

func nullStringInvalid() sql.NullString { return sql.NullString{} }

func logIDs(entries []models.RequestLogOut) []int64 {
	ids := make([]int64, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	return ids
}

func TestListLogsPaginatesByCursorAndAppliesFilters(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	// request_logs references upstreams, so the channel has to exist first.
	if _, err := database.Exec(`INSERT INTO upstreams (id, name, base_url)
        VALUES (1, 'primary', 'https://example.test')`); err != nil {
		t.Fatalf("insert upstream: %v", err)
	}

	for i := 1; i <= 5; i++ {
		status := 200
		if i == 3 {
			status = 500
		}
		// Only one row carries a token name, so a search for it has to narrow
		// rather than match everything the way the shared model name does.
		tokenName := "customer-a"
		if i != 2 {
			tokenName = "customer-b"
		}
		_, err := database.Exec(`INSERT INTO request_logs
            (id, created_at, method, path, client_type, upstream_id, upstream_name,
             downstream_token_name, model, stream, status_code, total_tokens)
            VALUES (?, datetime('now', ?), 'POST', '/v1/responses', 'codex',
                    1, 'primary', ?, 'gpt-test', 0, ?, 10)`,
			i, "-"+itoa(5-i)+" minutes", tokenName, status)
		if err != nil {
			t.Fatalf("insert log %d: %v", i, err)
		}
	}

	page, err := ListLogs(ctx, database, 2, 0, nil, LogFilter{})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(page) != 2 || page[0].ID != 5 || page[1].ID != 4 {
		t.Fatalf("first page = %v, want the two newest rows", logIDs(page))
	}

	cursor := &LogCursor{CreatedAt: page[1].CreatedAt, ID: page[1].ID}
	next, err := ListLogs(ctx, database, 2, 0, cursor, LogFilter{})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(next) != 2 || next[0].ID != 3 || next[1].ID != 2 {
		t.Fatalf("second page = %v, want rows 3 and 2", logIDs(next))
	}

	status := "5xx"
	failures, err := ListLogs(ctx, database, 10, 0, nil, LogFilter{Status: &status})
	if err != nil {
		t.Fatalf("status filter: %v", err)
	}
	if len(failures) != 1 || failures[0].ID != 3 {
		t.Errorf("status filter = %v, want only the 5xx row", logIDs(failures))
	}

	search := "gpt-test"
	matches, err := ListLogs(ctx, database, 10, 0, nil, LogFilter{Search: &search})
	if err != nil {
		t.Fatalf("search filter: %v", err)
	}
	if len(matches) != 5 {
		t.Errorf("search matched %d rows, want all 5", len(matches))
	}

	tokenSearch := "customer-a"
	byToken, err := ListLogs(ctx, database, 10, 0, nil, LogFilter{Search: &tokenSearch})
	if err != nil {
		t.Fatalf("token name search: %v", err)
	}
	if len(byToken) != 1 || byToken[0].ID != 2 {
		t.Errorf("token name search = %v, want only row 2", logIDs(byToken))
	}

	// A search for a literal wildcard must not match every row.
	wildcard := "%"
	none, err := ListLogs(ctx, database, 10, 0, nil, LogFilter{Search: &wildcard})
	if err != nil {
		t.Fatalf("wildcard search: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("a literal %% matched %d rows, want 0", len(none))
	}
}

func TestGetLogDetailResolvesOverridesAgainstCanonicalSnapshots(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	if _, err := database.Exec(`INSERT INTO request_logs
        (id, method, path, client_type, stream) VALUES (1, 'POST', '/v1/responses', 'codex', 0)`); err != nil {
		t.Fatalf("insert log: %v", err)
	}
	// The upstream request matched the downstream one; the downstream response
	// was explicitly absent. Stored the way releases before chunked storage
	// kept snapshots, and migrated.
	seedLegacyPayloads(t, database, `INSERT INTO request_log_payloads
        (request_log_id, request_snapshot, upstream_request_override, upstream_request_is_override,
         response_snapshot, downstream_response_override, downstream_response_is_override)
        VALUES (1, '{"body":{"text":"req"}}', NULL, 0, '{"body":{"text":"res"}}', NULL, 1)`)

	detail, ok, err := GetLogDetail(ctx, database, 1)
	if err != nil || !ok {
		t.Fatalf("get detail: %v (ok=%v)", err, ok)
	}
	if string(detail.DownstreamRequest) != `{"body":{"text":"req"}}` {
		t.Errorf("downstream request = %s", detail.DownstreamRequest)
	}
	// A cleared override flag means the peer was identical, so the canonical
	// snapshot is what the console sees.
	if string(detail.UpstreamRequest) != `{"body":{"text":"req"}}` {
		t.Errorf("upstream request = %s, want the canonical snapshot", detail.UpstreamRequest)
	}
	if string(detail.UpstreamResponse) != `{"body":{"text":"res"}}` {
		t.Errorf("upstream response = %s", detail.UpstreamResponse)
	}
	// A set override flag with a null value means the peer was absent.
	if detail.DownstreamResponse != nil {
		t.Errorf("downstream response = %s, want nil", detail.DownstreamResponse)
	}

	if _, ok, err := GetLogDetail(ctx, database, 404); err != nil || ok {
		t.Errorf("missing log: ok=%v err=%v", ok, err)
	}
}

func TestGetLogSnapshotResolvesEachFieldLikeTheDetail(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	for _, stmt := range []string{
		`INSERT INTO request_logs (id, method, path, client_type, stream)
            VALUES (1, 'POST', '/v1/responses', 'codex', 0)`,
		`INSERT INTO request_logs (id, method, path, client_type, stream)
            VALUES (2, 'POST', '/v1/responses', 'codex', 0)`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seedLegacyPayloads(t, database, `INSERT INTO request_log_payloads
            (request_log_id, request_snapshot, upstream_request_override, upstream_request_is_override,
             response_snapshot, downstream_response_override, downstream_response_is_override)
            VALUES (1, '{"body":{"text":"req"}}', '{"body":{"text":"up"}}', 1,
                    '{"body":{"text":"res"}}', NULL, 1)`)

	want := map[LogSnapshotField]string{
		LogSnapshotDownstreamRequest:  `{"body":{"text":"req"}}`,
		LogSnapshotUpstreamRequest:    `{"body":{"text":"up"}}`,
		LogSnapshotUpstreamResponse:   `{"body":{"text":"res"}}`,
		LogSnapshotDownstreamResponse: "",
	}
	for field, expected := range want {
		snapshot, ok, err := GetLogSnapshot(ctx, database, 1, field)
		if err != nil || !ok {
			t.Fatalf("%s: err=%v ok=%v", field, err, ok)
		}
		if string(snapshot) != expected {
			t.Errorf("%s = %q, want %q", field, snapshot, expected)
		}
	}

	// A log without a payload row is found, with nothing to show.
	snapshot, ok, err := GetLogSnapshot(ctx, database, 2, LogSnapshotDownstreamRequest)
	if err != nil || !ok || snapshot != nil {
		t.Errorf("payload-less log: snapshot=%q ok=%v err=%v", snapshot, ok, err)
	}

	if _, ok, err := GetLogSnapshot(ctx, database, 404, LogSnapshotDownstreamRequest); err != nil || ok {
		t.Errorf("missing log: ok=%v err=%v", ok, err)
	}
	if _, ok := ParseLogSnapshotField("headers"); ok {
		t.Error("unknown field parsed")
	}
}

func TestTopLogStatsRanksModelsAndChannels(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	for id, name := range map[int]string{1: "primary", 2: "secondary"} {
		if _, err := database.Exec(`INSERT INTO upstreams (id, name, base_url)
            VALUES (?, ?, 'https://example.test')`, id, name); err != nil {
			t.Fatalf("insert upstream %d: %v", id, err)
		}
	}

	rows := []struct {
		id            int
		upstreamID    int
		upstreamName  string
		upstreamModel string
		totalTokens   int
	}{
		{1, 1, "primary", "gpt-test", 100},
		{2, 1, "primary", "gpt-test", 200},
		{3, 2, "secondary", "claude-test", 50},
		{4, 2, "secondary", "gpt-test", 10},
	}
	for _, row := range rows {
		_, err := database.Exec(`INSERT INTO request_logs
            (id, method, path, client_type, upstream_id, upstream_name, upstream_model,
             stream, status_code, total_tokens)
            VALUES (?, 'POST', '/v1/responses', 'codex', ?, ?, ?, 0, 200, ?)`,
			row.id, row.upstreamID, row.upstreamName, row.upstreamModel, row.totalTokens)
		if err != nil {
			t.Fatalf("insert log %d: %v", row.id, err)
		}
	}

	stats, err := TopLogStats(ctx, database, LogTopWindowSevenDays, 10)
	if err != nil {
		t.Fatalf("top stats: %v", err)
	}
	if stats.Window != "7d" {
		t.Errorf("window = %q, want 7d", stats.Window)
	}
	if len(stats.Models) != 2 || stats.Models[0].Name != "gpt-test" || stats.Models[0].Count != 3 {
		t.Errorf("model ranking = %+v, want gpt-test first with 3", stats.Models)
	}
	if len(stats.Channels) != 2 || stats.Channels[0].Name != "primary" || stats.Channels[0].Count != 2 {
		t.Errorf("channel ranking = %+v, want primary first with 2", stats.Channels)
	}
	// Channel rankings expose the numeric group key so the console can link.
	if stats.Channels[0].ID == nil || *stats.Channels[0].ID != 1 {
		t.Errorf("channel id = %v, want 1", stats.Channels[0].ID)
	}
	if stats.Models[0].ID != nil {
		t.Errorf("model ranking exposed an id: %v", stats.Models[0].ID)
	}
	if len(stats.ModelTokens) == 0 || stats.ModelTokens[0].Count != 310 {
		t.Errorf("model token ranking = %+v, want gpt-test with 310", stats.ModelTokens)
	}
	if len(stats.ChannelTokens) == 0 || stats.ChannelTokens[0].Count != 300 {
		t.Errorf("channel token ranking = %+v, want primary with 300", stats.ChannelTokens)
	}

	// The limit is clamped into 1..=20.
	clamped, err := TopLogStats(ctx, database, LogTopWindowSevenDays, 0)
	if err != nil {
		t.Fatalf("clamped stats: %v", err)
	}
	if len(clamped.Models) != 1 {
		t.Errorf("a zero limit returned %d models, want it clamped to 1", len(clamped.Models))
	}
}

func TestParseLogTopWindowAcceptsDashboardRanges(t *testing.T) {
	testCases := []struct {
		value string
		want  string
	}{
		{value: "all", want: "all"},
		{value: "default", want: "30d"},
		{value: "custom", want: "custom"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.value, func(t *testing.T) {
			window, ok := ParseLogTopWindow(testCase.value)
			if !ok {
				t.Fatalf("ParseLogTopWindow(%q) was rejected", testCase.value)
			}
			if got := window.QueryValue(); got != testCase.want {
				t.Errorf("QueryValue() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestTopLogStatsAllTimeIncludesOldRows(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	if _, err := database.Exec(`INSERT INTO upstreams (id, name, base_url)
        VALUES (1, 'primary', 'https://example.test')`); err != nil {
		t.Fatalf("insert upstream: %v", err)
	}
	for id, age := range []string{"-90 days", "-1 day"} {
		if _, err := database.Exec(`INSERT INTO request_logs
            (id, created_at, method, path, client_type, upstream_id, upstream_name,
             upstream_model, stream, status_code, total_tokens)
            VALUES (?, datetime('now', ?), 'POST', '/v1/responses', 'codex',
                    1, 'primary', 'gpt-test', 0, 200, 10)`, id+1, age); err != nil {
			t.Fatalf("insert log %d: %v", id+1, err)
		}
	}

	window, ok := ParseLogTopWindow("all")
	if !ok {
		t.Fatal("all-time window was rejected")
	}
	stats, err := TopLogStats(ctx, database, window, 10)
	if err != nil {
		t.Fatalf("top stats: %v", err)
	}
	if stats.Window != "all" {
		t.Errorf("window = %q, want all", stats.Window)
	}
	if len(stats.Models) != 1 || stats.Models[0].Count != 2 {
		t.Errorf("model ranking = %+v, want both old and recent rows", stats.Models)
	}
}

func TestCustomLogRangesUseAnInclusiveEndDate(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()
	if _, err := database.Exec(`INSERT INTO upstreams (id, name, base_url)
        VALUES (1, 'primary', 'https://example.test')`); err != nil {
		t.Fatalf("insert upstream: %v", err)
	}
	for id, createdAt := range []string{
		"2026-08-01 00:00:00",
		"2026-08-03 23:59:59",
		"2026-08-04 00:00:00",
	} {
		if _, err := database.Exec(`INSERT INTO request_logs
            (id, created_at, method, path, client_type, upstream_id, upstream_name,
             upstream_model, stream, status_code, total_tokens)
            VALUES (?, ?, 'POST', '/v1/responses', 'codex', 1, 'primary',
                    'gpt-test', 0, 200, 10)`, id+1, createdAt); err != nil {
			t.Fatalf("insert log %d: %v", id+1, err)
		}
	}

	usage, err := QueryTokenUsage(ctx, database, LogTopWindowCustom,
		"2026-08-01 00:00:00", "2026-08-04 00:00:00")
	if err != nil {
		t.Fatalf("custom token usage: %v", err)
	}
	if usage.TotalTokens != 20 || usage.RequestCount != 2 || usage.AllRequestCount != 2 {
		t.Errorf("custom token usage = %+v, want the first two rows", usage)
	}

	stats, err := TopLogStatsCustom(ctx, database,
		"2026-08-01 00:00:00", "2026-08-04 00:00:00", 10)
	if err != nil {
		t.Fatalf("custom top stats: %v", err)
	}
	if stats.Window != "custom" || len(stats.Models) != 1 || stats.Models[0].Count != 2 {
		t.Errorf("custom top stats = %+v, want two rows", stats)
	}
}

func TestDeleteOldLogsRemovesPayloadsWithTheirLogs(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	storeTestLog(t, database, "-40 days", textSnapshot(`{}`, "x"))
	storeTestLog(t, database, "-1 days", textSnapshot(`{}`, "x"))

	if err := DeleteOldLogs(ctx, database, 30); err != nil {
		t.Fatalf("delete: %v", err)
	}

	logCount := countRows(t, database, "SELECT COUNT(*) FROM request_logs")
	snapshotCount := countRows(t, database, "SELECT COUNT(*) FROM request_log_snapshots")
	if logCount != 1 || snapshotCount != 1 {
		t.Errorf("after retention: %d logs, %d snapshot rows, want 1 and 1", logCount, snapshotCount)
	}
}

func TestRefreshGroupsLogsIntoUsageWindows(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	if _, err := database.Exec(`INSERT INTO request_logs
       (id, created_at, method, path, client_type, stream, prompt_tokens,
        prompt_cached_tokens, total_tokens) VALUES
       (1, datetime('now'), 'POST', '/v1/responses', 'codex', 0, 80, 20, 100),
       (2, datetime('now'), 'POST', '/v1/responses', 'codex', 0, NULL, NULL, NULL),
       (3, datetime('now', '-2 days'), 'POST', '/v1/responses', 'codex', 0, 120, 30, 200),
       (4, datetime('now', '-8 days'), 'POST', '/v1/responses', 'codex', 0, NULL, NULL, NULL),
       (5, datetime('now', '-31 days'), 'POST', '/v1/responses', 'codex', 0, 200, 50, 400)`); err != nil {
		t.Fatalf("insert logs: %v", err)
	}

	cache, err := LoadLogStatsCache(ctx, database)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	snapshot := cache.Snapshot()

	if snapshot.TotalLogCount != 5 {
		t.Errorf("total = %d, want 5", snapshot.TotalLogCount)
	}
	if snapshot.LogCount24h != 2 {
		t.Errorf("24h = %d, want 2", snapshot.LogCount24h)
	}
	// The 8-day-old row is outside seven days but inside thirty, and the
	// 31-day-old row falls out of the window entirely.
	sevenDays := snapshot.TokenUsage.SevenDays
	if sevenDays.TotalTokens != 300 || sevenDays.PromptTokens != 200 ||
		sevenDays.PromptCachedTokens != 50 || sevenDays.RequestCount != 2 ||
		sevenDays.AllRequestCount != 3 {
		t.Errorf("seven-day window = %+v", sevenDays)
	}
	thirtyDays := snapshot.TokenUsage.ThirtyDays
	if thirtyDays.TotalTokens != 300 || thirtyDays.AllRequestCount != 4 {
		t.Errorf("thirty-day window = %+v", thirtyDays)
	}
	allTime := snapshot.TokenUsage.AllTime
	if allTime.TotalTokens != 700 || allTime.PromptTokens != 400 ||
		allTime.PromptCachedTokens != 100 || allTime.RequestCount != 3 ||
		allTime.AllRequestCount != 5 {
		t.Errorf("all-time window = %+v", allTime)
	}
}

func TestRecordPersistedEntriesUpdatesCacheWithoutRefresh(t *testing.T) {
	cache := NewLogStatsCache()
	now := nowUnix()
	total, prompt, cached := int64(12), int64(9), int64(3)

	cache.RecordPersistedEntries([]PersistedLogStats{
		{ID: 1, CreatedAtUnixSeconds: now, TotalTokens: &total,
			PromptTokens: &prompt, PromptCachedTokens: &cached},
		{ID: 2, CreatedAtUnixSeconds: now},
	})

	snapshot := cache.Snapshot()
	if snapshot.TotalLogCount != 2 || snapshot.LogCount24h != 2 {
		t.Errorf("counts = %d total / %d in 24h, want 2/2",
			snapshot.TotalLogCount, snapshot.LogCount24h)
	}
	window := snapshot.TokenUsage.ThirtyDays
	if window.TotalTokens != 12 || window.PromptTokens != 9 ||
		window.PromptCachedTokens != 3 || window.RequestCount != 1 ||
		window.AllRequestCount != 2 {
		t.Errorf("window = %+v", window)
	}

	// A replayed entry must not be counted twice.
	cache.RecordPersistedEntries([]PersistedLogStats{{ID: 1, CreatedAtUnixSeconds: now,
		TotalTokens: &total, PromptTokens: &prompt, PromptCachedTokens: &cached}})
	if replayed := cache.Snapshot(); replayed.TotalLogCount != 2 {
		t.Errorf("total after replay = %d, want 2", replayed.TotalLogCount)
	}
}

func TestRecentOneMinuteRateCountsOnlySuccessfulRequests(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	if _, err := database.Exec(`INSERT INTO request_logs
       (id, created_at, method, path, client_type, stream, status_code, total_tokens) VALUES
       (1, datetime('now'), 'POST', '/v1/r', 'codex', 0, 200, 10),
       (2, datetime('now', '-30 seconds'), 'POST', '/v1/r', 'codex', 0, 204, 20),
       (3, datetime('now', '-5 seconds'), 'POST', '/v1/r', 'codex', 0, 500, 99),
       (4, datetime('now', '-10 seconds'), 'POST', '/v1/r', 'codex', 0, 404, NULL),
       (5, datetime('now', '-15 seconds'), 'POST', '/v1/r', 'codex', 0, NULL, 7),
       (6, datetime('now', '-90 seconds'), 'POST', '/v1/r', 'codex', 0, 200, 30)`); err != nil {
		t.Fatalf("insert logs: %v", err)
	}

	count, err := RecentOneMinuteLogCount(ctx, database)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want only the two 2xx rows inside the window", count)
	}

	rate, err := RecentOneMinuteLogRate(ctx, database)
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	if rate.RequestCount != 2 || rate.TotalTokens != 30 {
		t.Errorf("rate = %+v, want 2 requests and 30 tokens", rate)
	}
}

func TestRefreshPreservesPendingEntriesNewerThanTheWatermark(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	if _, err := database.Exec(`INSERT INTO request_logs
        (id, created_at, method, path, client_type, stream, total_tokens)
        VALUES (1, datetime('now'), 'POST', '/v1/r', 'codex', 0, 10)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	cache := NewLogStatsCache()
	total, prompt, cached := int64(5), int64(4), int64(1)
	cache.RecordPersistedEntries([]PersistedLogStats{{
		ID: 2, CreatedAtUnixSeconds: nowUnix(),
		TotalTokens: &total, PromptTokens: &prompt, PromptCachedTokens: &cached,
	}})
	if err := cache.RefreshFromDB(ctx, database); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Row 2 committed after the refresh read the table, so its counts are carried
	// over rather than lost.
	snapshot := cache.Snapshot()
	if snapshot.TotalLogCount != 2 {
		t.Errorf("total = %d, want 2", snapshot.TotalLogCount)
	}
	window := snapshot.TokenUsage.ThirtyDays
	if window.TotalTokens != 15 || window.PromptTokens != 4 ||
		window.PromptCachedTokens != 1 || window.RequestCount != 2 {
		t.Errorf("window = %+v, want the pending entry merged in", window)
	}
}

func TestPendingEntriesAreBoundedWhenRefreshesStopHappening(t *testing.T) {
	cache := NewLogStatsCache()
	now := nowUnix()

	// Every committed row is held until the next successful rebuild carries it
	// across. Nothing trimmed the map, so a database that stopped answering
	// turned steady traffic into unbounded memory.
	surplus := 1000
	entries := make([]PersistedLogStats, 0, maxPendingEntries+surplus)
	for id := range int64(maxPendingEntries + surplus) {
		entries = append(entries, PersistedLogStats{ID: id + 1, CreatedAtUnixSeconds: now})
	}
	cache.RecordPersistedEntries(entries)

	// Snapshot prunes, which is where the cap applies.
	snapshot := cache.Snapshot()

	cache.mu.Lock()
	held := len(cache.state.pendingEntries)
	cache.mu.Unlock()

	if held > maxPendingEntries {
		t.Errorf("held %d pending entries, want at most %d", held, maxPendingEntries)
	}
	// Trimming leaves headroom rather than stopping at the cap, so the next
	// batch does not put the map straight back over it and pay for another
	// full sort of every key.
	if held > pendingLowWater {
		t.Errorf("trimmed to %d, want the low-water mark %d so the sort amortizes",
			held, pendingLowWater)
	}
	// The counts the dropped entries contributed stay in the buckets; only the
	// ability to replay them across a rebuild is given up.
	if snapshot.TotalLogCount != int64(maxPendingEntries+surplus) {
		t.Errorf("total = %d, want every entry counted once", snapshot.TotalLogCount)
	}
}

func TestPruneDoesNotSortWhileTheMapIsWithinItsCap(t *testing.T) {
	cache := NewLogStatsCache()
	now := nowUnix()

	// The ordinary case is a map holding one refresh interval of rows. Prune
	// runs on every batch and every console poll, so it must not touch the
	// entries at all until the cap is actually reached.
	entries := make([]PersistedLogStats, 0, 128)
	for id := range int64(128) {
		entries = append(entries, PersistedLogStats{ID: id + 1, CreatedAtUnixSeconds: now})
	}
	cache.RecordPersistedEntries(entries)
	cache.Snapshot()

	cache.mu.Lock()
	held := len(cache.state.pendingEntries)
	cache.mu.Unlock()

	if held != 128 {
		t.Errorf("held %d entries, want all 128 kept below the cap", held)
	}
}

func TestRetentionDeletesEverythingExpiredAcrossBatches(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	// More rows than one batch, so the loop has to come back for the rest.
	// Deleting the window in one statement is what this replaces: the amount
	// waiting is not bounded by anything the gateway controls.
	total := int(logDeleteBatchSize)*2 + 37
	for i := range total {
		if _, err := database.Exec(`INSERT INTO request_logs
           (created_at, method, path, client_type, stream)
           VALUES (datetime('now', '-40 days'), 'POST', '/v1/responses', 'codex', 0)`); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// One row inside the window, which must survive.
	if _, err := database.Exec(`INSERT INTO request_logs
       (created_at, method, path, client_type, stream)
       VALUES (datetime('now'), 'POST', '/v1/responses', 'codex', 0)`); err != nil {
		t.Fatalf("insert recent: %v", err)
	}

	if err := DeleteOldLogs(ctx, database, 30); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var remaining int64
	if err := database.QueryRow("SELECT COUNT(*) FROM request_logs").Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Errorf("%d rows left, want only the one inside the window", remaining)
	}
}

func TestRetentionTakesThePayloadRowsWithIt(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	storeTestLog(t, database, "-40 days", textSnapshot(`{}`, "x"), nil, textSnapshot(`{}`, "y"))

	if err := DeleteOldLogs(ctx, database, 30); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The snapshot and body rows go with their parent through ON DELETE
	// CASCADE, which is why the delete does not name them separately.
	for _, table := range []string{"request_log_snapshots", "request_log_bodies"} {
		if rows := countRows(t, database, "SELECT COUNT(*) FROM "+table); rows != 0 {
			t.Errorf("%d %s rows survived their log row", rows, table)
		}
	}
}

func TestALogListingGivesUpRatherThanHoldingItsConnection(t *testing.T) {
	database := memoryDB(t)

	// None of the filters can use an index, so a filter matching nothing reads
	// the whole table while holding one of the few pooled connections. An
	// already-cancelled context stands in for the deadline expiring: what
	// matters is that the query reports it instead of running to completion.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	search := "needle"
	_, err := ListLogs(ctx, database, 50, 0, nil, LogFilter{Search: &search})
	if err == nil {
		t.Fatal("a cancelled listing returned as though it had finished")
	}
}

func TestLogSearchIsBoundedBeforeItReachesTheQuery(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	if _, err := database.Exec(`INSERT INTO request_logs
       (created_at, method, path, client_type, stream, upstream_name)
       VALUES (datetime('now'), 'POST', '/v1/responses', 'codex', 0, 'primary')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// A bounded term still finds what it names.
	search := "primary"
	entries, err := ListLogs(ctx, database, 50, 0, nil, LogFilter{Search: &search})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("found %d rows, want the one matching the search", len(entries))
	}

	// And the wildcards inside it stay literal.
	wildcard := "%"
	entries, err = ListLogs(ctx, database, 50, 0, nil, LogFilter{Search: &wildcard})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a literal %% matched %d rows, want none", len(entries))
	}
}

// Deleting a channel or token detaches its logs across batches. The logs stay,
// and those of other rows keep their references.
func TestDeletingARowDetachesAllItsLogs(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()

	if _, err := database.Exec(`INSERT INTO upstreams (id, name, base_url)
        VALUES (1, 'gone', 'https://x'), (2, 'kept', 'https://x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO api_tokens (id, name, token, token_hash, token_preview)
        VALUES (1, 'gone', 't1', 'h1', 'p1'), (2, 'kept', 't2', 'h2', 'p2')`); err != nil {
		t.Fatal(err)
	}

	// Two full batches and a remainder for row 1, three logs for row 2.
	const detachable = 2*detachLogBatch + 1
	if _, err := database.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
        INSERT INTO request_logs (method, path, client_type, stream, upstream_id, downstream_token_id)
        SELECT 'POST', 'r', 'codex', 0, IIF(i <= 3, 2, 1), IIF(i <= 3, 2, 1) FROM n`,
		detachable+3); err != nil {
		t.Fatal(err)
	}

	for _, remove := range []func(context.Context, *sql.DB, int64) (bool, error){DeleteUpstream, DeleteToken} {
		if deleted, err := remove(ctx, database, 1); err != nil || !deleted {
			t.Fatalf("deleted=%v err=%v", deleted, err)
		}
	}

	count := func(query string) int64 {
		t.Helper()
		var n int64
		if err := database.QueryRow(query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM request_logs
        WHERE upstream_id IS NULL AND downstream_token_id IS NULL`); n != detachable {
		t.Errorf("%d logs detached, want %d", n, detachable)
	}
	if n := count(`SELECT COUNT(*) FROM request_logs
        WHERE upstream_id = 2 AND downstream_token_id = 2`); n != 3 {
		t.Errorf("%d logs of the kept rows still reference them, want 3", n)
	}
	if n := count(`SELECT (SELECT COUNT(*) FROM upstreams WHERE id = 1)
        + (SELECT COUNT(*) FROM api_tokens WHERE id = 1)`); n != 0 {
		t.Errorf("%d deleted rows remain", n)
	}
}

// Retention stopped by shutdown reports the cancellation rather than success,
// so the pass stops instead of running on into queries that then fail.
func TestRetentionReportsItsCancellation(t *testing.T) {
	database := memoryDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := DeleteOldLogs(ctx, database, 30); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// A delete completes when its caller has gone: stopping between the detach and
// the delete left the row standing with part of its history cut away.
func TestADeleteOutlivesItsCaller(t *testing.T) {
	database := memoryDB(t)
	if _, err := database.Exec(`INSERT INTO upstreams (id, name, base_url) VALUES (1, 'c', 'https://x')`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if deleted, err := DeleteUpstream(ctx, database, 1); err != nil || !deleted {
		t.Errorf("deleted=%v err=%v, want the delete finished", deleted, err)
	}
}
