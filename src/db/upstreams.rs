//! Upstream channel storage and group membership.

use rusqlite::types::Value;
use rusqlite::{params, params_from_iter, Connection};
use serde::Serialize;

use crate::db::{is_unique_violation, Db};
use crate::error::ApiError;
pub use crate::models::upstream::UpstreamRow;

const COLUMNS: &str = "id, name, base_url, api_key, model_names, model_prefixes, \
    model_mappings, effort_mappings, priority, weight, auto_weight_enabled, enabled, \
    archived, archived_prev_enabled, extra_headers, timeout_seconds, rate_limit, \
    created_at, updated_at";

fn read_row(row: &rusqlite::Row<'_>) -> rusqlite::Result<UpstreamRow> {
    Ok(UpstreamRow {
        id: row.get(0)?,
        name: row.get(1)?,
        base_url: row.get(2)?,
        api_key: row.get(3)?,
        model_names: row.get(4)?,
        model_prefixes: row.get(5)?,
        model_mappings: row.get(6)?,
        effort_mappings: row.get(7)?,
        priority: row.get(8)?,
        weight: row.get(9)?,
        auto_weight_enabled: row.get(10)?,
        enabled: row.get(11)?,
        archived: row.get(12)?,
        archived_prev_enabled: row.get(13)?,
        extra_headers: row.get(14)?,
        timeout_seconds: row.get(15)?,
        rate_limit: row.get(16)?,
        created_at: row.get(17)?,
        updated_at: row.get(18)?,
    })
}

fn encode_json<T: Serialize>(value: &T) -> String {
    serde_json::to_string(value).unwrap_or_else(|_| "[]".to_string())
}

pub fn decode_vec(raw: &str) -> Vec<String> {
    serde_json::from_str(raw).unwrap_or_default()
}

pub fn decode_map(raw: &str) -> std::collections::BTreeMap<String, String> {
    serde_json::from_str(raw).unwrap_or_default()
}

pub async fn list_upstreams(db: &Db) -> Result<Vec<UpstreamRow>, ApiError> {
    db.with_conn(|connection| {
        let query = format!("SELECT {COLUMNS} FROM upstreams ORDER BY id ASC");
        let mut statement = connection.prepare(&query).map_err(ApiError::database)?;
        let rows = statement
            .query_map([], read_row)
            .map_err(ApiError::database)?;
        let mut upstreams = Vec::new();
        for row in rows {
            upstreams.push(row.map_err(ApiError::database)?);
        }
        Ok(upstreams)
    })
    .await
}

fn get_conn(connection: &Connection, id: i64) -> Result<Option<UpstreamRow>, ApiError> {
    let query = format!("SELECT {COLUMNS} FROM upstreams WHERE id = ?1");
    match connection.query_row(&query, params![id], read_row) {
        Ok(row) => Ok(Some(row)),
        Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
        Err(error) => Err(ApiError::database(error)),
    }
}

pub async fn get_upstream(db: &Db, id: i64) -> Result<Option<UpstreamRow>, ApiError> {
    db.with_conn(move |connection| get_conn(connection, id))
        .await
}

fn list_group_ids_conn(connection: &Connection, id: i64) -> Result<Vec<i64>, ApiError> {
    let mut statement = connection
        .prepare("SELECT group_id FROM upstream_groups WHERE upstream_id = ?1 ORDER BY group_id")
        .map_err(ApiError::database)?;
    let rows = statement
        .query_map(params![id], |row| row.get(0))
        .map_err(ApiError::database)?;
    let mut ids = Vec::new();
    for row in rows {
        ids.push(row.map_err(ApiError::database)?);
    }
    Ok(ids)
}

pub async fn list_group_ids(db: &Db, id: i64) -> Result<Vec<i64>, ApiError> {
    db.with_conn(move |connection| list_group_ids_conn(connection, id))
        .await
}

fn set_group_membership(
    connection: &Connection,
    id: i64,
    group_ids: &[i64],
) -> Result<(), ApiError> {
    connection
        .execute(
            "DELETE FROM upstream_groups WHERE upstream_id = ?1",
            params![id],
        )
        .map_err(ApiError::database)?;
    let mut statement = connection
        .prepare("INSERT OR IGNORE INTO upstream_groups (upstream_id, group_id) VALUES (?1, ?2)")
        .map_err(ApiError::database)?;
    for group_id in group_ids {
        statement
            .execute(params![id, group_id])
            .map_err(ApiError::database)?;
    }
    Ok(())
}

pub async fn create_upstream(
    db: &Db,
    input: crate::models::upstream::UpstreamIn,
) -> Result<UpstreamRow, ApiError> {
    let names = encode_json(&input.model_names);
    let prefixes = encode_json(&input.model_prefixes);
    let mappings = encode_json(&input.model_mappings);
    let effort = encode_json(&input.effort_mappings);
    let headers = encode_json(&input.extra_headers);
    let archived = input.archived.unwrap_or(false);
    let requested_enabled = input.enabled;
    let (enabled, archived_prev_enabled) = if archived {
        (0, Some(requested_enabled as i64))
    } else {
        (requested_enabled as i64, None)
    };
    let timeout = input.timeout_seconds.unwrap_or(300.0);
    let rate_limit = input.rate_limit.clone();
    let group_ids = input.requested_group_ids();

    db.transaction(move |transaction| {
        transaction
            .execute(
                "INSERT INTO upstreams
                    (name, base_url, api_key, model_names, model_prefixes, model_mappings,
                     effort_mappings, priority, weight, auto_weight_enabled, enabled,
                     archived, archived_prev_enabled, extra_headers, timeout_seconds,
                     rate_limit, created_at, updated_at)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14,
                         ?15, ?16, datetime('now'), datetime('now'))",
                params![
                    input.name,
                    input.base_url,
                    input.api_key,
                    names,
                    prefixes,
                    mappings,
                    effort,
                    input.priority,
                    input.weight,
                    input.auto_weight_enabled as i64,
                    enabled,
                    archived as i64,
                    archived_prev_enabled,
                    headers,
                    timeout,
                    rate_limit,
                ],
            )
            .map_err(ApiError::database)?;
        let id = transaction.last_insert_rowid();
        set_group_membership(transaction, id, &group_ids)?;
        get_conn(transaction, id)?.ok_or_else(|| ApiError::internal("upstream was not persisted"))
    })
    .await
}

fn opt_text(value: Option<&String>) -> Value {
    value
        .map(|value| Value::Text(value.clone()))
        .unwrap_or(Value::Null)
}

fn opt_f64(value: Option<f64>) -> Value {
    value.map(Value::Real).unwrap_or(Value::Null)
}

pub async fn update_upstream(
    db: &Db,
    id: i64,
    input: crate::models::upstream::UpstreamUpdate,
) -> Result<UpstreamRow, ApiError> {
    let names = encode_json(&input.input.model_names);
    let prefixes = encode_json(&input.input.model_prefixes);
    let mappings = encode_json(&input.input.model_mappings);
    let effort = encode_json(&input.input.effort_mappings);
    let headers = encode_json(&input.input.extra_headers);
    let group_ids = input.input.requested_group_ids();

    db.transaction(move |transaction| {
        let existing =
            get_conn(transaction, id)?.ok_or_else(|| ApiError::not_found("upstream not found"))?;

        let mut query = String::from(
            "UPDATE upstreams
             SET name = ?, base_url = ?, model_names = ?, model_prefixes = ?,
                 model_mappings = ?, effort_mappings = ?, priority = ?, weight = ?,
                 auto_weight_enabled = ?, extra_headers = ?, updated_at = datetime('now')",
        );
        let mut values: Vec<Value> = vec![
            Value::Text(input.input.name.clone()),
            Value::Text(input.input.base_url.clone()),
            Value::Text(names),
            Value::Text(prefixes),
            Value::Text(mappings),
            Value::Text(effort),
            Value::Integer(input.input.priority),
            Value::Integer(input.input.weight),
            Value::Integer(input.input.auto_weight_enabled as i64),
            Value::Text(headers),
        ];
        // The key is replaced only when a new one or a clear is requested.
        let api_key: Option<String> = match &input.input.api_key {
            Some(api_key) if !api_key.is_empty() => Some(api_key.clone()),
            _ => {
                if input.clear_api_key {
                    None
                } else {
                    existing.api_key.clone()
                }
            }
        };
        query.push_str(", api_key = ?");
        values.push(opt_text(api_key.as_ref()));

        let timeout = input
            .input
            .timeout_seconds
            .unwrap_or(existing.timeout_seconds);
        query.push_str(", timeout_seconds = ?");
        values.push(Value::Real(timeout));

        let rate_limit = input
            .input
            .normalized_rate_limit()
            .map_err(ApiError::bad_request)?;
        query.push_str(", rate_limit = ?");
        values.push(opt_text(rate_limit.as_ref()));

        // Archive state is applied here, not by the dedicated endpoints.
        match input.input.archived {
            Some(archived) => {
                let (enabled, archived_prev_enabled) = if archived {
                    (0, Some(input.input.enabled as i64))
                } else {
                    let restored = existing
                        .archived_prev_enabled
                        .unwrap_or(input.input.enabled as i64);
                    (restored, None)
                };
                query.push_str(", enabled = ?, archived = ?, archived_prev_enabled = ?");
                values.push(Value::Integer(enabled));
                values.push(Value::Integer(archived as i64));
                values.push(
                    archived_prev_enabled
                        .map(Value::Integer)
                        .unwrap_or(Value::Null),
                );
            }
            None => {
                query.push_str(", enabled = ?");
                values.push(Value::Integer(input.input.enabled as i64));
            }
        }

        query.push_str(" WHERE id = ?");
        values.push(Value::Integer(id));

        transaction
            .execute(&query, params_from_iter(values))
            .map_err(|error| {
                let api = ApiError::database(error);
                if is_unique_violation(&api) {
                    ApiError::bad_request("channel name already exists")
                } else {
                    api
                }
            })?;

        set_group_membership(transaction, id, &group_ids)?;
        get_conn(transaction, id)?.ok_or_else(|| ApiError::not_found("upstream not found"))
    })
    .await
}

pub async fn set_enabled(db: &Db, id: i64, enabled: bool) -> Result<Option<UpstreamRow>, ApiError> {
    db.with_conn(move |connection| {
        connection
            .execute(
                "UPDATE upstreams SET enabled = ?1, updated_at = datetime('now') WHERE id = ?2",
                params![enabled as i64, id],
            )
            .map_err(ApiError::database)?;
        get_conn(connection, id)
    })
    .await
}

pub async fn set_archived(
    db: &Db,
    id: i64,
    archived: bool,
) -> Result<Option<UpstreamRow>, ApiError> {
    db.with_conn(move |connection| {
        let existing = get_conn(connection, id)?;
        let Some(existing) = existing else {
            return Ok(None);
        };
        let (enabled, archived_prev_enabled) = if archived {
            (0, Some(existing.enabled))
        } else {
            (existing.archived_prev_enabled.unwrap_or(0), None)
        };
        connection
            .execute(
                "UPDATE upstreams
                 SET enabled = ?1, archived = ?2, archived_prev_enabled = ?3,
                     updated_at = datetime('now')
                 WHERE id = ?4",
                params![enabled, archived as i64, archived_prev_enabled, id,],
            )
            .map_err(ApiError::database)?;
        get_conn(connection, id)
    })
    .await
}

pub async fn set_priority(
    db: &Db,
    id: i64,
    priority: i64,
) -> Result<Option<UpstreamRow>, ApiError> {
    db.with_conn(move |connection| {
        connection
            .execute(
                "UPDATE upstreams SET priority = ?1, updated_at = datetime('now') WHERE id = ?2",
                params![priority, id],
            )
            .map_err(ApiError::database)?;
        get_conn(connection, id)
    })
    .await
}

pub async fn delete_upstream(db: &Db, id: i64) -> Result<bool, ApiError> {
    db.with_conn(move |connection| {
        loop {
            let affected = connection
                .execute(
                    "UPDATE request_logs SET upstream_id = NULL
                     WHERE id IN (SELECT id FROM request_logs WHERE upstream_id = ?1 LIMIT ?2)",
                    params![id, 5000_i64],
                )
                .map_err(ApiError::database)?;
            if affected < 5000usize {
                break;
            }
        }
        connection
            .execute(
                "DELETE FROM upstream_groups WHERE upstream_id = ?1",
                params![id],
            )
            .map_err(ApiError::database)?;
        let affected = connection
            .execute("DELETE FROM upstreams WHERE id = ?1", params![id])
            .map_err(ApiError::database)?;
        Ok(affected > 0)
    })
    .await
}
