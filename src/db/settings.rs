//! The singleton `runtime_settings` row.

use rusqlite::{params, Connection, Row};

use crate::db::Db;
use crate::error::ApiError;
use crate::models::settings::{RuntimeSettings, RuntimeSettingsIn};

const RUNTIME_SETTINGS_COLUMNS: &str = "log_body_keep_count, log_retention_days, \
    log_body_max_bytes, max_retries, same_upstream_retry_interval_ms, \
    auto_weight_failure_penalty, auto_weight_success_increment, \
    auto_weight_recovery_increment, auto_weight_recovery_interval_seconds, \
    proxy_enabled, proxy_url, default_upstream_timeout_seconds, \
    image_storage_max_mb, dashboard_multiplier, revision, updated_at";

fn read_settings(row: &Row<'_>) -> rusqlite::Result<RuntimeSettings> {
    Ok(RuntimeSettings {
        log_body_keep_count: row.get(0)?,
        log_retention_days: row.get(1)?,
        log_body_max_bytes: row.get(2)?,
        max_retries: row.get(3)?,
        same_upstream_retry_interval_ms: row.get(4)?,
        auto_weight_failure_penalty: row.get(5)?,
        auto_weight_success_increment: row.get(6)?,
        auto_weight_recovery_increment: row.get(7)?,
        auto_weight_recovery_interval_seconds: row.get(8)?,
        proxy_enabled: row.get(9)?,
        proxy_url: row.get(10)?,
        default_upstream_timeout_seconds: row.get(11)?,
        image_storage_max_mb: row.get(12)?,
        dashboard_multiplier: row.get(13)?,
        revision: row.get(14)?,
        updated_at: row.get(15)?,
        database_override: false,
    })
}

fn load_conn(connection: &Connection) -> Result<Option<RuntimeSettings>, ApiError> {
    let query = format!("SELECT {RUNTIME_SETTINGS_COLUMNS} FROM runtime_settings WHERE id = 1");
    match connection.query_row(&query, [], read_settings) {
        Ok(settings) => Ok(Some(settings)),
        Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
        Err(error) => Err(ApiError::database(error)),
    }
}

/// Returns `None` when the singleton row is missing.
pub async fn load_runtime_settings(db: &Db) -> Result<Option<RuntimeSettings>, ApiError> {
    db.with_conn(|connection| load_conn(connection)).await
}

/// Applies a revision compare-and-swap.
pub async fn update_runtime_settings(
    db: &Db,
    input: &RuntimeSettingsIn,
) -> Result<RuntimeSettings, ApiError> {
    db.transaction(|transaction| {
        let query = format!(
            "UPDATE runtime_settings
             SET log_body_keep_count = ?, log_retention_days = ?, log_body_max_bytes = ?,
                 max_retries = ?, same_upstream_retry_interval_ms = ?,
                 auto_weight_failure_penalty = ?, auto_weight_success_increment = ?,
                 auto_weight_recovery_increment = ?, auto_weight_recovery_interval_seconds = ?,
                 proxy_enabled = ?, proxy_url = ?,
                 default_upstream_timeout_seconds = ?,
                 image_storage_max_mb = ?,
                 dashboard_multiplier = ?,
                 revision = revision + 1, updated_at = datetime('now')
             WHERE id = 1 AND revision = ?"
        );
        let affected = transaction
            .execute(
                &query,
                params![
                    input.log_body_keep_count,
                    input.log_retention_days,
                    input.log_body_max_bytes,
                    input.max_retries,
                    input.same_upstream_retry_interval_ms,
                    input.auto_weight_failure_penalty,
                    input.auto_weight_success_increment,
                    input.auto_weight_recovery_increment,
                    input.auto_weight_recovery_interval_seconds,
                    input.proxy_enabled,
                    input.proxy_url.trim(),
                    input.default_upstream_timeout_seconds,
                    input.image_storage_max_mb,
                    input.dashboard_multiplier,
                    input.revision,
                ],
            )
            .map_err(ApiError::database)?;
        if affected != 1 {
            return Err(ApiError::conflict("runtime settings revision conflict"));
        }

        let read_query =
            format!("SELECT {RUNTIME_SETTINGS_COLUMNS} FROM runtime_settings WHERE id = 1");
        let mut updated = transaction
            .query_row(&read_query, [], read_settings)
            .map_err(ApiError::database)?;
        updated.database_override = true;
        Ok(updated)
    })
    .await
}
