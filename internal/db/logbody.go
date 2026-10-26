package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"strings"

	"github.com/liguangsheng/wildtoken/internal/apperr"
	"github.com/liguangsheng/wildtoken/internal/logbody"
)

const (
	// lookupBatchSize bounds the ids or hashes one IN list carries, well under
	// SQLite's limit on bound parameters.
	lookupBatchSize = 500
	// chunkSweepBatchSize is how many unlisted chunks one collector transaction
	// deletes, so the write lock is held briefly.
	chunkSweepBatchSize = 1000
)

// placeholders returns "?, ?, ?" for n parameters.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// PackNewChunks compresses the chunks of a batch that are not stored yet.
//
// It runs before the write transaction, so the transaction only inserts bytes
// that are ready rather than compressing while holding the write lock. The
// lookup is only a hint: a chunk collected after it is compressed and stored
// again inside the transaction, and one stored meanwhile is simply found.
func PackNewChunks(ctx context.Context, q Queryer, chunks []*logbody.Chunk) error {
	for start := 0; start < len(chunks); start += lookupBatchSize {
		group := chunks[start:min(start+lookupBatchSize, len(chunks))]
		args := make([]any, len(group))
		for i, c := range group {
			args[i] = c.Hash[:]
		}
		rows, err := q.QueryContext(ctx,
			"SELECT hash FROM log_chunks WHERE hash IN ("+placeholders(len(group))+")", args...)
		if err != nil {
			return apperr.Database(err)
		}
		stored := map[logbody.Hash]bool{}
		for rows.Next() {
			var hash []byte
			if err := rows.Scan(&hash); err != nil {
				rows.Close()
				return apperr.Database(err)
			}
			stored[logbody.Hash(hash)] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return apperr.Database(err)
		}
		for _, c := range group {
			if !stored[c.Hash] {
				c.Pack()
			}
		}
	}
	return nil
}

// PayloadStore writes log snapshots inside one write transaction. It remembers
// the ids it resolved, so the rows of a batch look each metadata value and
// chunk up once.
type PayloadStore struct {
	tx          *sql.Tx
	metas       map[logbody.Hash]int64
	chunks      map[logbody.Hash]int64
	insertChunk *sql.Stmt
}

func NewPayloadStore(tx *sql.Tx) *PayloadStore {
	return &PayloadStore{tx: tx, metas: map[logbody.Hash]int64{}, chunks: map[logbody.Hash]int64{}}
}

// Close releases the statement the store prepared.
func (s *PayloadStore) Close() {
	if s.insertChunk != nil {
		s.insertChunk.Close()
	}
}

// Store writes one log row's snapshots. cleared keeps only their metadata, as
// for a row whose bodies the keep-count policy has already dropped.
func (s *PayloadStore) Store(ctx context.Context, logID int64, p *logbody.Prepared, cleared bool) error {
	var metaIDs [logbody.SlotCount]any
	present := false
	for i := range p {
		if p[i].Snapshot == nil {
			continue
		}
		id, err := s.metaID(ctx, p[i].MetaHash, p[i].Snapshot.Meta)
		if err != nil {
			return err
		}
		metaIDs[i] = id
		present = true
	}
	if !present {
		return nil
	}

	if !cleared {
		if err := s.resolveChunks(ctx, p); err != nil {
			return err
		}
		record := p.Record(func(c *logbody.Chunk) int64 { return s.chunks[c.Hash] })
		if !record.Empty() {
			if _, err := s.tx.ExecContext(ctx,
				"INSERT INTO request_log_bodies (request_log_id, record) VALUES (?, ?)",
				logID, logbody.AppendRecord(nil, &record)); err != nil {
				return apperr.Database(err)
			}
		}
	}

	_, err := s.tx.ExecContext(ctx, `INSERT INTO request_log_snapshots
        (request_log_id, downstream_request_meta, upstream_request_meta,
         upstream_response_meta, downstream_response_meta, bodies_cleared)
    VALUES (?, ?, ?, ?, ?, ?)`,
		logID, metaIDs[0], metaIDs[1], metaIDs[2], metaIDs[3], boolToInt64(cleared))
	if err != nil {
		return apperr.Database(err)
	}
	return nil
}

// metaID finds or stores one metadata value.
func (s *PayloadStore) metaID(ctx context.Context, hash logbody.Hash, meta []byte) (int64, error) {
	if id, ok := s.metas[hash]; ok {
		return id, nil
	}
	var id int64
	err := s.tx.QueryRowContext(ctx, "SELECT id FROM log_metas WHERE hash = ?", hash[:]).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		codec, packed := logbody.Pack(meta)
		err = s.tx.QueryRowContext(ctx,
			"INSERT INTO log_metas (hash, size, codec, data) VALUES (?, ?, ?, ?) RETURNING id",
			hash[:], len(meta), codec, packed).Scan(&id)
	}
	if err != nil {
		return 0, apperr.Database(err)
	}
	s.metas[hash] = id
	return id, nil
}

// resolveChunks finds or stores every chunk of a row. Inside the transaction
// the lookup is authoritative: a chunk the pre-transaction lookup saw may have
// been collected since, and is stored again here.
func (s *PayloadStore) resolveChunks(ctx context.Context, p *logbody.Prepared) error {
	var missing []*logbody.Chunk
	queued := map[logbody.Hash]bool{}
	for i := range p {
		for _, c := range p[i].Chunks {
			if _, ok := s.chunks[c.Hash]; ok || queued[c.Hash] {
				continue
			}
			queued[c.Hash] = true
			missing = append(missing, c)
		}
	}

	for start := 0; start < len(missing); start += lookupBatchSize {
		group := missing[start:min(start+lookupBatchSize, len(missing))]
		args := make([]any, len(group))
		for i, c := range group {
			args[i] = c.Hash[:]
		}
		rows, err := s.tx.QueryContext(ctx,
			"SELECT id, hash FROM log_chunks WHERE hash IN ("+placeholders(len(group))+")", args...)
		if err != nil {
			return apperr.Database(err)
		}
		for rows.Next() {
			var id int64
			var hash []byte
			if err := rows.Scan(&id, &hash); err != nil {
				rows.Close()
				return apperr.Database(err)
			}
			s.chunks[logbody.Hash(hash)] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return apperr.Database(err)
		}
	}

	for _, c := range missing {
		if _, ok := s.chunks[c.Hash]; ok {
			continue
		}
		if s.insertChunk == nil {
			statement, err := s.tx.PrepareContext(ctx,
				"INSERT INTO log_chunks (hash, size, codec, data) VALUES (?, ?, ?, ?) RETURNING id")
			if err != nil {
				return apperr.Database(err)
			}
			s.insertChunk = statement
		}
		// Packed already unless the lookup before the transaction found it.
		c.Pack()
		var id int64
		if err := s.insertChunk.QueryRowContext(ctx, c.Hash[:], len(c.Data), c.Codec, c.Packed).
			Scan(&id); err != nil {
			return apperr.Database(err)
		}
		s.chunks[c.Hash] = id
	}
	return nil
}

// storedBlob is a metadata value or a chunk as read back, not yet decoded.
type storedBlob struct {
	hash  []byte
	size  int
	codec logbody.Codec
	data  []byte
}

// decode inflates a stored value and checks it is the one that was stored.
// Raw DEFLATE carries no checksum, so the content hash is what catches damage.
func (b storedBlob) decode() ([]byte, error) {
	data, err := logbody.Unpack(b.codec, b.data, b.size)
	if err != nil {
		return nil, err
	}
	if sum := logbody.Sum(data); string(sum[:]) != string(b.hash) {
		return nil, logbody.ErrCorrupt
	}
	return data, nil
}

// loadLogSnapshots reads and renders the given snapshots of one log row. found
// is false when the log does not exist; a snapshot the row never had is nil.
//
// Everything is read in one snapshot of the database, so a body cleared and
// collected meanwhile is seen either whole or not at all. The decoding happens
// after that snapshot ends, so a large body does not hold a connection.
func loadLogSnapshots(ctx context.Context, database *sql.DB, logID int64,
	slots ...int) (out [logbody.SlotCount]json.RawMessage, found bool, err error) {
	tx, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, false, apperr.Database(err)
	}
	defer tx.Rollback()

	var metaIDs [logbody.SlotCount]sql.NullInt64
	var cleared sql.NullInt64
	var encoded []byte
	err = tx.QueryRowContext(ctx, `SELECT s.downstream_request_meta, s.upstream_request_meta,
              s.upstream_response_meta, s.downstream_response_meta,
              s.bodies_cleared, b.record
       FROM request_logs AS l
       LEFT JOIN request_log_snapshots AS s ON s.request_log_id = l.id
       LEFT JOIN request_log_bodies AS b ON b.request_log_id = l.id
       WHERE l.id = ?`, logID).
		Scan(&metaIDs[0], &metaIDs[1], &metaIDs[2], &metaIDs[3], &cleared, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, apperr.Database(err)
	}
	bodiesCleared := cleared.Int64 != 0

	var record logbody.Record
	if encoded != nil && !bodiesCleared {
		if record, err = logbody.DecodeRecord(encoded); err != nil {
			// The bodies are lost, but their metadata still reads.
			slog.Warn("a stored log body record does not decode", "log_id", logID, "error", err)
			bodiesCleared = true
		}
	}

	var wantMetas, wantChunks []int64
	for _, slot := range slots {
		if !metaIDs[slot].Valid {
			continue
		}
		wantMetas = append(wantMetas, metaIDs[slot].Int64)
		if !bodiesCleared {
			wantChunks = append(wantChunks, record[slot].Chunks...)
		}
	}
	metas, err := readBlobs(ctx, tx, "log_metas", wantMetas)
	if err != nil {
		return out, false, err
	}
	chunks, err := readBlobs(ctx, tx, "log_chunks", wantChunks)
	if err != nil {
		return out, false, err
	}
	tx.Rollback()

	for _, slot := range slots {
		if !metaIDs[slot].Valid {
			continue
		}
		stored, ok := metas[metaIDs[slot].Int64]
		if !ok {
			slog.Warn("a log snapshot names metadata that is not stored", "log_id", logID, "slot", slot)
			continue
		}
		meta, err := stored.decode()
		if err != nil {
			slog.Warn("stored log metadata is damaged", "log_id", logID, "slot", slot, "error", err)
			continue
		}
		if bodiesCleared {
			out[slot] = logbody.RenderCleared(meta)
			continue
		}
		body, err := assembleBody(&record[slot], chunks)
		if err != nil {
			slog.Warn("a stored log body is incomplete", "log_id", logID, "slot", slot, "error", err)
			out[slot] = logbody.RenderCleared(meta)
			continue
		}
		out[slot] = logbody.Render(meta, body)
	}
	return out, true, nil
}

// readBlobs reads stored values by id from log_metas or log_chunks.
func readBlobs(ctx context.Context, q Queryer, table string, ids []int64) (map[int64]storedBlob, error) {
	blobs := make(map[int64]storedBlob, len(ids))
	unique := make([]any, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	for start := 0; start < len(unique); start += lookupBatchSize {
		group := unique[start:min(start+lookupBatchSize, len(unique))]
		rows, err := q.QueryContext(ctx, fmt.Sprintf(
			"SELECT id, hash, size, codec, data FROM %s WHERE id IN (%s)", table, placeholders(len(group))),
			group...)
		if err != nil {
			return nil, apperr.Database(err)
		}
		for rows.Next() {
			var id int64
			var blob storedBlob
			if err := rows.Scan(&id, &blob.hash, &blob.size, &blob.codec, &blob.data); err != nil {
				rows.Close()
				return nil, apperr.Database(err)
			}
			blobs[id] = blob
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, apperr.Database(err)
		}
	}
	return blobs, nil
}

// assembleBody rebuilds a slot's body from its chunks, or nil for a slot
// without one. A missing or damaged chunk is an error rather than a gap: a body
// with a hole in it would read as the real one.
func assembleBody(slot *logbody.SlotBody, chunks map[int64]storedBlob) (*logbody.Body, error) {
	switch slot.Kind {
	case logbody.KindNone:
		return nil, nil
	case logbody.KindOmitted:
		return &logbody.Body{Kind: logbody.KindOmitted, Length: slot.Length}, nil
	}

	data := make([]byte, 0, slot.Captured)
	for i, id := range slot.Chunks {
		stored, ok := chunks[id]
		if !ok {
			return nil, fmt.Errorf("chunk %d of %d is missing", i+1, len(slot.Chunks))
		}
		part, err := stored.decode()
		if err != nil {
			return nil, fmt.Errorf("chunk %d of %d: %w", i+1, len(slot.Chunks), err)
		}
		data = append(data, part...)
	}
	if int64(len(data)) != slot.Captured {
		return nil, fmt.Errorf("chunks hold %d bytes, the record says %d", len(data), slot.Captured)
	}
	return &logbody.Body{Kind: slot.Kind, Data: data, Length: slot.Length, Truncated: slot.Truncated}, nil
}

// chunkMarks records which chunk ids a stored record lists.
type chunkMarks []uint64

func newChunkMarks(highest int64) chunkMarks { return make(chunkMarks, highest/64+1) }

func (m chunkMarks) set(id int64)      { m[id/64] |= 1 << (uint64(id) % 64) }
func (m chunkMarks) has(id int64) bool { return m[id/64]&(1<<(uint64(id)%64)) != 0 }

// count reports how many ids are marked, for the collector's log line.
func (m chunkMarks) count() (n int) {
	for _, word := range m {
		n += bits.OnesCount64(word)
	}
	return n
}

// markRecord marks the chunks a record lists. Ids above highest were stored
// after the collector looked and are never candidates, so they need no mark.
func (m chunkMarks) markRecord(encoded []byte, highest int64) error {
	record, err := logbody.DecodeRecord(encoded)
	if err != nil {
		return err
	}
	for i := range record {
		for _, id := range record[i].Chunks {
			if id <= highest {
				m.set(id)
			}
		}
	}
	return nil
}

// afterChunkMark runs between the collector's mark and its sweep. Tests use it
// to commit a write in that window.
var afterChunkMark func()

// CollectLogChunks deletes the chunks no stored body lists any more and
// reports how many went.
//
// It marks every chunk the stored records list, in one read snapshot, and then
// deletes the unmarked ones in short write transactions. Each of those first
// marks what records written since the snapshot list. That is enough because
// write transactions never overlap, a record is only ever inserted or deleted,
// never changed, and log ids only grow, so every record written since the
// snapshot has an id above the highest the snapshot saw. Chunks stored since
// then have ids above the highest it saw and are left alone. A chunk is
// therefore deleted only when no record lists it at that moment.
func CollectLogChunks(ctx context.Context, database *sql.DB) (int64, error) {
	marks, lastLog, lastChunk, err := markLogChunks(ctx, database)
	if err != nil || lastChunk == 0 {
		return 0, err
	}
	if afterChunkMark != nil {
		afterChunkMark()
	}

	var deleted int64
	for cursor := int64(0); cursor < lastChunk; {
		n, next, seen, err := sweepLogChunks(ctx, database, marks, lastLog, cursor, lastChunk)
		deleted += n
		if err != nil {
			return deleted, err
		}
		cursor, lastLog = next, seen
	}
	return deleted, nil
}

// markLogChunks reads every stored record in one snapshot. It also returns the
// highest log and chunk ids handed out by then; sqlite_sequence holds those,
// and a row inserted later always gets a higher one.
func markLogChunks(ctx context.Context, database *sql.DB) (chunkMarks, int64, int64, error) {
	tx, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, 0, apperr.Database(err)
	}
	defer tx.Rollback()

	var lastLog, lastChunk int64
	if err := tx.QueryRowContext(ctx, `SELECT
            COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'request_logs'), 0),
            COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'log_chunks'), 0)`).
		Scan(&lastLog, &lastChunk); err != nil {
		return nil, 0, 0, apperr.Database(err)
	}
	marks := newChunkMarks(lastChunk)

	rows, err := tx.QueryContext(ctx, "SELECT request_log_id, record FROM request_log_bodies")
	if err != nil {
		return nil, 0, 0, apperr.Database(err)
	}
	defer rows.Close()
	for rows.Next() {
		var logID int64
		var encoded []byte
		if err := rows.Scan(&logID, &encoded); err != nil {
			return nil, 0, 0, apperr.Database(err)
		}
		// A record that does not decode lists chunks nobody can name, so
		// nothing may be collected until it is cleared.
		if err := marks.markRecord(encoded, lastChunk); err != nil {
			return nil, 0, 0, fmt.Errorf("log %d body record: %w", logID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, apperr.Database(err)
	}
	return marks, lastLog, lastChunk, nil
}

// sweepLogChunks catches up on records written since lastLog and deletes up to
// chunkSweepBatchSize unmarked chunks above cursor, in one write transaction.
// It returns how many it deleted, where the next sweep starts, and the highest
// log id it has caught up to.
func sweepLogChunks(ctx context.Context, database *sql.DB, marks chunkMarks,
	lastLog, cursor, lastChunk int64) (int64, int64, int64, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, cursor, lastLog, apperr.Database(err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		"SELECT request_log_id, record FROM request_log_bodies WHERE request_log_id > ? ORDER BY request_log_id",
		lastLog)
	if err != nil {
		return 0, cursor, lastLog, apperr.Database(err)
	}
	seen := lastLog
	for rows.Next() {
		var logID int64
		var encoded []byte
		if err := rows.Scan(&logID, &encoded); err != nil {
			rows.Close()
			return 0, cursor, lastLog, apperr.Database(err)
		}
		if err := marks.markRecord(encoded, lastChunk); err != nil {
			rows.Close()
			return 0, cursor, lastLog, fmt.Errorf("log %d body record: %w", logID, err)
		}
		seen = logID
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, cursor, lastLog, apperr.Database(err)
	}

	// Walk the chunk ids in order, collecting unmarked ones until a batch is
	// full or the walk reaches the last id the mark saw.
	rows, err = tx.QueryContext(ctx,
		"SELECT id FROM log_chunks WHERE id > ? AND id <= ? ORDER BY id LIMIT ?",
		cursor, lastChunk, 8*chunkSweepBatchSize)
	if err != nil {
		return 0, cursor, lastLog, apperr.Database(err)
	}
	next := lastChunk
	var doomed []any
	scanned := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, cursor, lastLog, apperr.Database(err)
		}
		scanned++
		if !marks.has(id) {
			doomed = append(doomed, id)
		}
		if len(doomed) == chunkSweepBatchSize || scanned == 8*chunkSweepBatchSize {
			next = id
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, cursor, lastLog, apperr.Database(err)
	}

	for start := 0; start < len(doomed); start += lookupBatchSize {
		group := doomed[start:min(start+lookupBatchSize, len(doomed))]
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM log_chunks WHERE id IN ("+placeholders(len(group))+")", group...); err != nil {
			return 0, cursor, lastLog, apperr.Database(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, cursor, lastLog, apperr.Database(err)
	}
	return int64(len(doomed)), next, seen, nil
}

// CollectLogMetas deletes metadata no snapshot refers to any more and reports
// how many went. Snapshot rows go only with their logs, so this follows
// retention. One statement, so it runs under one write lock and cannot race a
// writer that is about to refer to a value it found.
func CollectLogMetas(ctx context.Context, database *sql.DB) (int64, error) {
	result, err := database.ExecContext(ctx, `DELETE FROM log_metas WHERE id NOT IN (
           SELECT downstream_request_meta FROM request_log_snapshots WHERE downstream_request_meta IS NOT NULL
           UNION ALL
           SELECT upstream_request_meta FROM request_log_snapshots WHERE upstream_request_meta IS NOT NULL
           UNION ALL
           SELECT upstream_response_meta FROM request_log_snapshots WHERE upstream_response_meta IS NOT NULL
           UNION ALL
           SELECT downstream_response_meta FROM request_log_snapshots WHERE downstream_response_meta IS NOT NULL)`)
	if err != nil {
		return 0, apperr.Database(err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, apperr.Database(err)
	}
	return deleted, nil
}
