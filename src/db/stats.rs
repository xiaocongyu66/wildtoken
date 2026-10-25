//! Counts and sizes the admin system panel reports.

use rusqlite::params;

use crate::db::Db;
use crate::error::ApiError;

pub async fn total_log_count(db: &Db) -> Result<i64, ApiError> {
    db.with_conn(|connection| {
        connection
            .query_row("SELECT COUNT(*) FROM request_logs", [], |row| row.get(0))
            .map_err(ApiError::database)
    })
    .await
}

pub async fn log_count_24h(db: &Db) -> Result<i64, ApiError> {
    db.with_conn(|connection| {
        connection
            .query_row(
                "SELECT COUNT(*) FROM request_logs
                 WHERE created_at >= datetime('now', '-1 day')",
                [],
                |row| row.get(0),
            )
            .map_err(ApiError::database)
    })
    .await
}

pub async fn recent_one_minute_log_count(db: &Db) -> Result<i64, ApiError> {
    db.with_conn(|connection| {
        connection
            .query_row(
                "SELECT COUNT(*) FROM request_logs
                 WHERE created_at >= datetime('now', '-60 seconds')
                   AND status_code >= 200 AND status_code < 300",
                [],
                |row| row.get(0),
            )
            .map_err(ApiError::database)
    })
    .await
}

/// Returns `(enabled, total)` channel counts.
pub async fn upstream_counts(db: &Db) -> Result<(i64, i64), ApiError> {
    db.with_conn(|connection| {
        connection
            .query_row(
                "SELECT COALESCE(SUM(CASE WHEN enabled = 1 THEN 1 ELSE 0 END), 0), COUNT(*)
                 FROM upstreams",
                [],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .map_err(ApiError::database)
    })
    .await
}

pub async fn database_allocated_bytes(db: &Db) -> Result<i64, ApiError> {
    db.with_conn(|connection| {
        connection
            .query_row(
                "SELECT (SELECT page_count FROM pragma_page_count()) *
                        (SELECT page_size FROM pragma_page_size())",
                [],
                |row| row.get(0),
            )
            .map_err(ApiError::database)
    })
    .await
}

pub async fn database_reachable(db: &Db) -> Result<(), ApiError> {
    db.with_conn(|connection| {
        connection
            .query_row("SELECT 1", [], |row| row.get::<_, i64>(0))
            .map(|_| ())
            .map_err(ApiError::database)
    })
    .await
}

pub async fn token_exists(db: &Db, id: i64) -> Result<bool, ApiError> {
    db.with_conn(move |connection| {
        let count: i64 = connection
            .query_row(
                "SELECT COUNT(*) FROM api_tokens WHERE id = ?1",
                params![id],
                |row| row.get(0),
            )
            .map_err(ApiError::database)?;
        Ok(count == 1)
    })
    .await
}
