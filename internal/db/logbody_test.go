package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liguangsheng/wildtoken/internal/logbody"
)

// legacyCreateRequestLogPayloads is the table snapshots were stored in whole,
// as JSON, before bodies were stored by chunk. Tests build it to migrate.
const legacyCreateRequestLogPayloads = `
CREATE TABLE IF NOT EXISTS request_log_payloads (
    request_log_id INTEGER PRIMARY KEY
        REFERENCES request_logs(id) ON DELETE CASCADE,
    request_snapshot TEXT,
    upstream_request_override TEXT,
    upstream_request_is_override INTEGER NOT NULL DEFAULT 0
        CHECK (upstream_request_is_override IN (0, 1)),
    response_snapshot TEXT,
    downstream_response_override TEXT,
    downstream_response_is_override INTEGER NOT NULL DEFAULT 0
        CHECK (downstream_response_is_override IN (0, 1)),
    bodies_cleared INTEGER NOT NULL DEFAULT 0
        CHECK (bodies_cleared IN (0, 1))
);`

// textSnapshot captures a whole body under the given metadata.
func textSnapshot(meta, body string) *logbody.Snapshot {
	return &logbody.Snapshot{
		Meta: []byte(meta),
		Body: logbody.CaptureBody([]byte(body), len(body)+1, len(body)),
	}
}

// storeTestLog writes a log row and its snapshots the way the log writer does:
// cut and packed first, stored in one write transaction.
func storeTestLog(t *testing.T, database *sql.DB, age string, snapshots ...*logbody.Snapshot) int64 {
	t.Helper()
	ctx := context.Background()

	var slots [logbody.SlotCount]*logbody.Snapshot
	copy(slots[:], snapshots)
	batch := logbody.NewBatch()
	prepared := batch.Prepare(slots)
	if err := PackNewChunks(ctx, database, batch.Chunks()); err != nil {
		t.Fatalf("pack: %v", err)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO request_logs (created_at, method, path, client_type, stream)
        VALUES (datetime('now', ?), 'POST', '/v1/messages', 'claude', 0)`, age)
	if err != nil {
		t.Fatalf("insert log: %v", err)
	}
	logID, _ := result.LastInsertId()
	store := NewPayloadStore(tx)
	defer store.Close()
	if err := store.Store(ctx, logID, prepared, false); err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return logID
}

// seedLegacyPayloads builds the table snapshots were kept in before bodies
// were stored by chunk, fills it, and migrates it as an upgraded start would.
func seedLegacyPayloads(t *testing.T, database *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range append([]string{legacyCreateRequestLogPayloads}, statements...) {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := migrateLegacyLogPayloads(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

func countRows(t *testing.T, database *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := database.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return count
}

func readSnapshot(t *testing.T, database *sql.DB, logID int64, field LogSnapshotField) string {
	t.Helper()
	snapshot, ok, err := GetLogSnapshot(context.Background(), database, logID, field)
	if err != nil || !ok {
		t.Fatalf("read %s of log %d: ok=%v err=%v", field, logID, ok, err)
	}
	return string(snapshot)
}

// randomText is words drawn at random, seeded, so the chunker has content to
// cut on the way it does in a real conversation.
func randomText(random *rand.Rand, n int) string {
	words := strings.Fields("the a model tool call result file line error test build user " +
		"assistant read write change function value return list map key")
	var text strings.Builder
	for text.Len() < n {
		text.WriteString(words[random.IntN(len(words))])
		text.WriteByte(' ')
	}
	return text.String()
}

func TestStoredSnapshotsReadBackAsCaptured(t *testing.T) {
	database := memoryDB(t)

	downstream := &logbody.Snapshot{Meta: []byte(`{"headers":{"a":"b"},"method":"POST","url":"https://x"}`),
		Body: logbody.CaptureBody([]byte(`{"model":"m","messages":["中文 <tag> & more"]}`), 20, 49)}
	upstream := &logbody.Snapshot{Meta: downstream.Meta,
		Body: logbody.CaptureBody([]byte{0xff, 0, 1, 2}, 100, 4)}
	response := &logbody.Snapshot{Meta: []byte(`{"headers":{},"status":200,"status_code":200}`),
		Body: logbody.CaptureBody([]byte("secret"), 0, 6)}
	id := storeTestLog(t, database, "0 seconds", downstream, upstream, response)

	detail, ok, err := GetLogDetail(context.Background(), database, id)
	if err != nil || !ok {
		t.Fatalf("detail: ok=%v err=%v", ok, err)
	}
	for name, pair := range map[string][2]json.RawMessage{
		"downstream request":  {detail.DownstreamRequest, downstream.JSON()},
		"upstream request":    {detail.UpstreamRequest, upstream.JSON()},
		"upstream response":   {detail.UpstreamResponse, response.JSON()},
		"downstream response": {detail.DownstreamResponse, nil},
	} {
		if string(pair[0]) != string(pair[1]) {
			t.Errorf("%s = %s, want %s", name, pair[0], pair[1])
		}
	}
	if _, ok, err := GetLogDetail(context.Background(), database, 404); err != nil || ok {
		t.Errorf("missing log: ok=%v err=%v", ok, err)
	}
}

// The header set a client sends on every request is stored once, and two turns
// of one conversation share every chunk but the tail.
func TestRowsShareMetadataAndChunks(t *testing.T) {
	database := memoryDB(t)
	random := rand.New(rand.NewPCG(3, 4))
	meta := `{"headers":{"user-agent":"claude-cli"},"method":"POST","url":"https://x"}`

	first := randomText(random, 200<<10)
	second := first + randomText(random, 4<<10)
	storeTestLog(t, database, "0 seconds", textSnapshot(meta, first))
	before := countRows(t, database, "SELECT COUNT(*) FROM log_chunks")
	id := storeTestLog(t, database, "0 seconds", textSnapshot(meta, second))

	if metas := countRows(t, database, "SELECT COUNT(*) FROM log_metas"); metas != 1 {
		t.Errorf("stored %d metadata values, want the shared one once", metas)
	}
	if added := countRows(t, database, "SELECT COUNT(*) FROM log_chunks") - before; added > 3 {
		t.Errorf("the second turn added %d chunks, want only its tail", added)
	}
	var body struct {
		Body struct{ Text string } `json:"body"`
	}
	if err := json.Unmarshal([]byte(readSnapshot(t, database, id, LogSnapshotDownstreamRequest)), &body); err != nil ||
		body.Body.Text != second {
		t.Errorf("the second turn did not read back whole (%v)", err)
	}
}

func TestCollectingChunksKeepsWhatAStoredBodyLists(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()
	random := rand.New(rand.NewPCG(5, 6))
	shared := randomText(random, 40<<10)

	var ids []int64
	for i := range 3 {
		ids = append(ids, storeTestLog(t, database, "0 seconds",
			textSnapshot(`{}`, shared+randomText(random, 40<<10)+fmt.Sprint(i))))
	}
	if cleared, err := ClearOldLogBodies(ctx, database, 1, nil); err != nil || cleared != 2 {
		t.Fatalf("cleared %d (%v), want 2", cleared, err)
	}
	collected, err := CollectLogChunks(ctx, database)
	if err != nil || collected == 0 {
		t.Fatalf("collected %d chunks (%v), want the cleared rows' own", collected, err)
	}

	// What the kept row lists, shared prefix included, is all still there.
	var listed int64
	marks, _, last, err := markLogChunks(ctx, database)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	for id := int64(1); id <= last; id++ {
		if marks.has(id) {
			listed++
		}
	}
	if stored := countRows(t, database, "SELECT COUNT(*) FROM log_chunks"); stored != listed {
		t.Errorf("%d chunks stored, %d listed: collection left garbage or took a live chunk", stored, listed)
	}
	if !strings.Contains(readSnapshot(t, database, ids[2], LogSnapshotDownstreamRequest), `"text":"`) {
		t.Error("the kept row lost its body")
	}
	if got := readSnapshot(t, database, ids[0], LogSnapshotDownstreamRequest); got != `{"body":{"cleared":true}}` {
		t.Errorf("a cleared row reads %s", got)
	}
}

// A chunk nothing listed when the collector looked, which a row written before
// its sweep lists again, must survive: the sweep catches up on new records.
func TestCollectionSparesAChunkListedDuringItsSweep(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()
	body := randomText(rand.New(rand.NewPCG(7, 8)), 30<<10)

	storeTestLog(t, database, "0 seconds", textSnapshot(`{}`, body))
	storeTestLog(t, database, "0 seconds", textSnapshot(`{}`, "a different body"))
	if _, err := ClearOldLogBodies(ctx, database, 1, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}

	var reused int64
	afterChunkMark = func() {
		reused = storeTestLog(t, database, "0 seconds", textSnapshot(`{}`, body))
	}
	defer func() { afterChunkMark = nil }()
	if _, err := CollectLogChunks(ctx, database); err != nil {
		t.Fatalf("collect: %v", err)
	}

	var got struct {
		Body struct{ Text string } `json:"body"`
	}
	if err := json.Unmarshal([]byte(readSnapshot(t, database, reused, LogSnapshotDownstreamRequest)), &got); err != nil ||
		got.Body.Text != body {
		t.Fatalf("the row written during the sweep lost its body (%v)", err)
	}
}

func TestRetentionTakesSnapshotsAndTheirMetadata(t *testing.T) {
	database := memoryDB(t)
	ctx := context.Background()
	storeTestLog(t, database, "-40 days", textSnapshot(`{"method":"old"}`, "x"))
	storeTestLog(t, database, "-1 days", textSnapshot(`{"method":"new"}`, "y"))

	if err := DeleteOldLogs(ctx, database, 30); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rows := countRows(t, database, "SELECT COUNT(*) FROM request_log_snapshots"); rows != 1 {
		t.Errorf("%d snapshot rows, want only the kept log's", rows)
	}
	if rows := countRows(t, database, "SELECT COUNT(*) FROM request_log_bodies"); rows != 1 {
		t.Errorf("%d body records, want only the kept log's", rows)
	}
	if deleted, err := CollectLogMetas(ctx, database); err != nil || deleted != 1 {
		t.Errorf("collected %d metadata values (%v), want the expired log's", deleted, err)
	}
}

func TestMigrationKeepsWhatTheConsoleRead(t *testing.T) {
	database := memoryDB(t)
	for id := 1; id <= 6; id++ {
		if _, err := database.Exec(`INSERT INTO request_logs (id, method, path, client_type, stream)
            VALUES (?, 'POST', '/v1/messages', 'claude', 0)`, id); err != nil {
			t.Fatalf("insert log: %v", err)
		}
	}

	request := `{"body":{"byte_length":5,"text":"hello"},"headers":{"a":"b"},"method":"POST","url":"https://x"}`
	rewritten := `{"body":{"base64":"/wE=","byte_length":4,"encoding":"base64","truncated":true},"headers":{"a":"b"},"method":"POST","url":"https://x"}`
	response := `{"body":{"byte_length":3,"cleared":true},"headers":{},"status":200,"status_code":200}`
	cleared := `{"body":{"cleared":true},"headers":{"a":"b"},"method":"POST","url":"https://x"}`
	odd := `{"body":"a string body from an old release","method":"GET"}`
	seedLegacyPayloads(t, database,
		// Identical upstream request, explicitly absent downstream response.
		`INSERT INTO request_log_payloads VALUES (1, '`+request+`', NULL, 0, '`+response+`', NULL, 1, 0)`,
		// A rewritten upstream request, the same response on both sides.
		`INSERT INTO request_log_payloads VALUES (2, '`+request+`', '`+rewritten+`', 1, '`+response+`', NULL, 0, 0)`,
		// Bodies already cleared by the keep count.
		`INSERT INTO request_log_payloads VALUES (3, '`+cleared+`', NULL, 0, NULL, NULL, 0, 1)`,
		// A shape this release does not write, and a snapshot that never parsed.
		`INSERT INTO request_log_payloads VALUES (4, '`+odd+`', NULL, 0, '{not json', NULL, 0, 0)`,
		// An orphan, which the new foreign key would refuse. Only a copy made
		// with foreign keys off can hold one.
		`PRAGMA foreign_keys = OFF`,
		`INSERT INTO request_log_payloads VALUES (99, '`+request+`', NULL, 0, NULL, NULL, 0, 0)`,
		`PRAGMA foreign_keys = ON`,
	)

	want := map[int64][4]string{
		1: {request, request, response, ""},
		2: {request, rewritten, response, response},
		3: {cleared, cleared, "", ""},
		4: {odd, odd, "", ""},
		5: {"", "", "", ""},
	}
	fields := []LogSnapshotField{LogSnapshotDownstreamRequest, LogSnapshotUpstreamRequest,
		LogSnapshotUpstreamResponse, LogSnapshotDownstreamResponse}
	for id, expected := range want {
		for i, field := range fields {
			got := readSnapshot(t, database, id, field)
			if expected[i] == "" {
				if got != "" {
					t.Errorf("log %d %s = %s, want none", id, field, got)
				}
				continue
			}
			var left, right any
			if json.Unmarshal([]byte(got), &left) != nil || json.Unmarshal([]byte(expected[i]), &right) != nil ||
				!reflect.DeepEqual(left, right) {
				t.Errorf("log %d %s = %s, want %s", id, field, got, expected[i])
			}
		}
	}

	if tables := countRows(t, database,
		"SELECT COUNT(*) FROM sqlite_master WHERE name = 'request_log_payloads'"); tables != 0 {
		t.Error("the old table was not dropped")
	}
	// Starting again finds nothing to migrate.
	if err := Init(context.Background(), database); err != nil {
		t.Fatalf("second init: %v", err)
	}
}

func TestAnInterruptedMigrationResumes(t *testing.T) {
	database := memoryDB(t)
	statements := []string{legacyCreateRequestLogPayloads}
	for id := 1; id <= 5; id++ {
		statements = append(statements,
			fmt.Sprintf(`INSERT INTO request_logs (id, method, path, client_type, stream)
                VALUES (%d, 'POST', '/v1/messages', 'claude', 0)`, id),
			fmt.Sprintf(`INSERT INTO request_log_payloads (request_log_id, request_snapshot)
                VALUES (%d, '{"body":{"byte_length":1,"text":"%d"},"method":"POST"}')`, id, id))
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	migrationBatchRows = 2
	ctx, cancel := context.WithCancel(context.Background())
	afterMigrationBatch = cancel
	defer func() { migrationBatchRows, afterMigrationBatch = 500, nil }()
	if err := migrateLegacyLogPayloads(ctx, database); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if left := countRows(t, database, "SELECT COUNT(*) FROM request_log_payloads"); left != 3 {
		t.Fatalf("%d rows left after one batch, want 3", left)
	}

	afterMigrationBatch = nil
	if err := migrateLegacyLogPayloads(context.Background(), database); err != nil {
		t.Fatalf("resume: %v", err)
	}
	for id := int64(1); id <= 5; id++ {
		if got := readSnapshot(t, database, id, LogSnapshotDownstreamRequest); !strings.Contains(got, fmt.Sprintf(`"text":"%d"`, id)) {
			t.Errorf("log %d = %s", id, got)
		}
	}
}

// A database created before the limits were widened keeps every setting it
// held, in whatever order its columns were added.
func TestWideningTheSettingsLimitsKeepsEveryValue(t *testing.T) {
	database, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()

	// The table as an early release created it, before most columns existed.
	if _, err := database.Exec(`CREATE TABLE runtime_settings (
        id INTEGER PRIMARY KEY CHECK (id = 1),
        log_body_keep_count INTEGER NOT NULL CHECK (log_body_keep_count BETWEEN 1 AND 10000),
        log_retention_days INTEGER NOT NULL CHECK (log_retention_days BETWEEN 1 AND 3650),
        log_body_max_bytes INTEGER NOT NULL CHECK (log_body_max_bytes BETWEEN 0 AND 1048576),
        revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
        updated_at TEXT NOT NULL DEFAULT (datetime('now')));
        INSERT INTO runtime_settings VALUES (1, 500, 14, 1000000, 7, '2026-09-24 01:57:46');`); err != nil {
		t.Fatalf("create the old table: %v", err)
	}
	if err := Init(context.Background(), database); err != nil {
		t.Fatalf("init: %v", err)
	}

	var keep, days, maxBytes, revision int64
	var updated string
	var multiplier float64
	if err := database.QueryRow(`SELECT log_body_keep_count, log_retention_days, log_body_max_bytes,
            revision, updated_at, dashboard_multiplier FROM runtime_settings WHERE id = 1`).
		Scan(&keep, &days, &maxBytes, &revision, &updated, &multiplier); err != nil {
		t.Fatalf("read: %v", err)
	}
	if keep != 500 || days != 14 || maxBytes != 1000000 || revision != 7 ||
		updated != "2026-09-24 01:57:46" || multiplier != 1 {
		t.Errorf("settings = %d %d %d %d %s %v, want the stored values kept",
			keep, days, maxBytes, revision, updated, multiplier)
	}

	if _, err := database.Exec(`UPDATE runtime_settings
        SET log_body_keep_count = 100000, log_body_max_bytes = 134217728 WHERE id = 1`); err != nil {
		t.Errorf("the widened limits were refused: %v", err)
	}
	if _, err := database.Exec(`UPDATE runtime_settings SET log_body_keep_count = 100001 WHERE id = 1`); err == nil {
		t.Error("a keep count past the new limit was accepted")
	}

	var before, after string
	database.QueryRow("SELECT sql FROM sqlite_master WHERE name = 'runtime_settings'").Scan(&before)
	if err := Init(context.Background(), database); err != nil {
		t.Fatalf("second init: %v", err)
	}
	database.QueryRow("SELECT sql FROM sqlite_master WHERE name = 'runtime_settings'").Scan(&after)
	if before != after {
		t.Error("a second start rebuilt the table again")
	}
}

// Writes, clearing with collection, and reads run at once on a file database
// with real locking. A read sees a body whole or cleared, never part of one.
func TestConcurrentWritesCleanupAndReadsNeverShowAPartialBody(t *testing.T) {
	database := openContendedDB(t, SQLiteTxLock)
	ctx := context.Background()
	random := rand.New(rand.NewPCG(9, 10))

	var mu sync.Mutex
	stored := map[int64]string{}
	conversation := randomText(random, 20<<10)

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := ClearOldLogBodies(ctx, database, 3, nil); err != nil {
				t.Errorf("clear: %v", err)
				return
			}
			if _, err := CollectLogChunks(ctx, database); err != nil {
				t.Errorf("collect: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		reader := rand.New(rand.NewPCG(11, 12))
		for {
			select {
			case <-done:
				return
			default:
			}
			mu.Lock()
			var id int64
			var want string
			if len(stored) > 0 {
				id = int64(reader.IntN(len(stored)) + 1)
				want = stored[id]
			}
			mu.Unlock()
			if id == 0 {
				continue
			}
			snapshot, _, err := GetLogSnapshot(ctx, database, id, LogSnapshotDownstreamRequest)
			if err != nil {
				t.Errorf("read %d: %v", id, err)
				return
			}
			var got struct {
				Body struct {
					Text    *string
					Cleared bool
				} `json:"body"`
			}
			if err := json.Unmarshal(snapshot, &got); err != nil {
				t.Errorf("read %d: %v", id, err)
				return
			}
			if got.Body.Text != nil && *got.Body.Text != want {
				t.Errorf("log %d read back a different body", id)
				return
			}
			if got.Body.Cleared {
				// Reading as cleared is only right for a row that was cleared;
				// otherwise a chunk went missing under the read.
				var flagged int64
				if err := database.QueryRow(`SELECT bodies_cleared FROM request_log_snapshots
                    WHERE request_log_id = ?`, id).Scan(&flagged); err != nil || flagged != 1 {
					t.Errorf("log %d read as cleared while it still has its bodies (%v)", id, err)
					return
				}
			}
		}
	}()

	for turn := range 60 {
		conversation += randomText(random, 2<<10)
		id := storeTestLog(t, database, "0 seconds", textSnapshot(`{}`, conversation))
		mu.Lock()
		stored[id] = conversation
		mu.Unlock()
		if turn%10 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	close(done)
	wg.Wait()
}
