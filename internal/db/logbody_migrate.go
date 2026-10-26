package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/liguangsheng/wildtoken/internal/apperr"
	"github.com/liguangsheng/wildtoken/internal/logbody"
)

// Bounds on one migration batch: the stored snapshot bytes it holds in memory
// and its rows. Variables so a test can make batches small.
var (
	migrationBatchBytes int64 = 32 << 20
	migrationBatchRows        = 500
)

// afterMigrationBatch runs after each migrated batch commits. Tests use it to
// cut a migration short.
var afterMigrationBatch func()

// migrateLegacyLogPayloads moves snapshots stored whole as JSON, the way logs
// were kept before bodies were stored by chunk, into the chunk store, and then
// drops the table they were in.
//
// It runs at startup, before anything writes logs. Each batch is stored and
// removed from the old table in one transaction, so a start that is cut short
// resumes where it stopped.
func migrateLegacyLogPayloads(ctx context.Context, database *sql.DB) error {
	var exists int64
	if err := database.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'request_log_payloads'").
		Scan(&exists); err != nil {
		return fmt.Errorf("look for stored log snapshots: %w", err)
	}
	if exists == 0 {
		return nil
	}

	// The old foreign key kept orphans out, but a copy restored by hand may
	// hold some, and the new tables' keys would refuse them.
	if _, err := database.ExecContext(ctx,
		"DELETE FROM request_log_payloads WHERE request_log_id NOT IN (SELECT id FROM request_logs)"); err != nil {
		return fmt.Errorf("drop orphaned log snapshots: %w", err)
	}

	var total int64
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_log_payloads").
		Scan(&total); err != nil {
		return fmt.Errorf("count stored log snapshots: %w", err)
	}
	if total > 0 {
		slog.Info("moving stored log snapshots to chunked storage", "rows", total)
	}

	var migrated, verbatim int64
	reported := time.Now()
	for {
		first, last, err := nextMigrationBatch(ctx, database)
		if err != nil {
			return err
		}
		if first == 0 {
			break
		}
		rows, kept, err := migrateLogPayloadBatch(ctx, database, first, last)
		if err != nil {
			return fmt.Errorf("move log snapshots %d to %d: %w", first, last, err)
		}
		migrated += rows
		verbatim += kept

		if afterMigrationBatch != nil {
			afterMigrationBatch()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Since(reported) >= 2*time.Second {
			slog.Info("moving stored log snapshots", "done", migrated, "rows", total)
			reported = time.Now()
		}
	}

	if _, err := database.ExecContext(ctx, "DROP TABLE request_log_payloads"); err != nil {
		return fmt.Errorf("drop the old log snapshot table: %w", err)
	}
	if total > 0 {
		slog.Info("moved stored log snapshots to chunked storage",
			"rows", migrated, "kept_verbatim", verbatim)
	}
	return nil
}

// nextMigrationBatch picks the oldest rows left, as many as fit the bounds and
// always at least one, as an id range. first is 0 when none are left.
func nextMigrationBatch(ctx context.Context, database *sql.DB) (first, last int64, err error) {
	rows, err := database.QueryContext(ctx, `SELECT request_log_id,
              COALESCE(length(CAST(request_snapshot AS BLOB)), 0)
              + COALESCE(length(CAST(upstream_request_override AS BLOB)), 0)
              + COALESCE(length(CAST(response_snapshot AS BLOB)), 0)
              + COALESCE(length(CAST(downstream_response_override AS BLOB)), 0)
       FROM request_log_payloads
       ORDER BY request_log_id
       LIMIT ?`, migrationBatchRows)
	if err != nil {
		return 0, 0, apperr.Database(err)
	}
	defer rows.Close()

	var held int64
	for rows.Next() {
		var id, size int64
		if err := rows.Scan(&id, &size); err != nil {
			return 0, 0, apperr.Database(err)
		}
		if first != 0 && held+size > migrationBatchBytes {
			break
		}
		if first == 0 {
			first = id
		}
		last, held = id, held+size
	}
	if err := rows.Err(); err != nil {
		return 0, 0, apperr.Database(err)
	}
	return first, last, nil
}

// legacyLogRow is one row of the old table, split for the chunk store.
type legacyLogRow struct {
	id        int64
	snapshots [logbody.SlotCount]*logbody.Snapshot
	cleared   bool
}

// migrateLogPayloadBatch moves the rows in [first, last] and reports how many
// it moved and how many snapshots it had to keep verbatim.
func migrateLogPayloadBatch(ctx context.Context, database *sql.DB, first, last int64) (int64, int64, error) {
	rows, err := database.QueryContext(ctx, `SELECT request_log_id, request_snapshot,
              upstream_request_override, upstream_request_is_override,
              response_snapshot, downstream_response_override, downstream_response_is_override,
              bodies_cleared
       FROM request_log_payloads
       WHERE request_log_id BETWEEN ? AND ?
       ORDER BY request_log_id`, first, last)
	if err != nil {
		return 0, 0, apperr.Database(err)
	}

	var batch []legacyLogRow
	var verbatim int64
	for rows.Next() {
		var row legacyLogRow
		var request, upstreamOverride, response, downstreamOverride sql.NullString
		var upstreamIsOverride, downstreamIsOverride, cleared int64
		if err := rows.Scan(&row.id, &request, &upstreamOverride, &upstreamIsOverride,
			&response, &downstreamOverride, &downstreamIsOverride, &cleared); err != nil {
			rows.Close()
			return 0, 0, apperr.Database(err)
		}
		row.cleared = cleared != 0

		// The four snapshots, resolved as the console read them: a cleared
		// override flag means the peer was the canonical snapshot itself.
		upstreamRequest := request
		if upstreamIsOverride != 0 {
			upstreamRequest = upstreamOverride
		}
		downstreamResponse := response
		if downstreamIsOverride != 0 {
			downstreamResponse = downstreamOverride
		}
		raws := [logbody.SlotCount]json.RawMessage{
			decodeSnapshot(request), decodeSnapshot(upstreamRequest),
			decodeSnapshot(response), decodeSnapshot(downstreamResponse),
		}
		for i, raw := range raws {
			if raw == nil {
				continue
			}
			if i%2 == 1 && bytes.Equal(raws[i-1], raw) {
				row.snapshots[i] = row.snapshots[i-1]
				continue
			}
			snapshot, kept := logbody.FromLegacy(raw, row.cleared)
			if kept {
				verbatim++
			}
			row.snapshots[i] = snapshot
		}
		batch = append(batch, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, apperr.Database(err)
	}

	chunks := logbody.NewBatch()
	prepared := make([]*logbody.Prepared, len(batch))
	for i := range batch {
		prepared[i] = chunks.Prepare(batch[i].snapshots)
	}
	if err := PackNewChunks(ctx, database, chunks.Chunks()); err != nil {
		return 0, 0, err
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, apperr.Database(err)
	}
	defer tx.Rollback()
	store := NewPayloadStore(tx)
	defer store.Close()
	for i := range batch {
		if err := store.Store(ctx, batch[i].id, prepared[i], batch[i].cleared); err != nil {
			return 0, 0, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM request_log_payloads WHERE request_log_id BETWEEN ? AND ?", first, last); err != nil {
		return 0, 0, apperr.Database(err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, apperr.Database(err)
	}
	return int64(len(batch)), verbatim, nil
}

// decodeSnapshot reads an old snapshot the way the console did: absent, empty
// or unparseable reads as no snapshot at all.
func decodeSnapshot(stored sql.NullString) json.RawMessage {
	if !stored.Valid || stored.String == "" {
		return nil
	}
	if !json.Valid([]byte(stored.String)) {
		return nil
	}
	return json.RawMessage(stored.String)
}
