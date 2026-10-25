//! Downstream API-token storage.

use rusqlite::types::Value;
use rusqlite::{params, params_from_iter, Connection};

use crate::db::token::{generate_api_token, token_digest, token_preview};
use crate::db::{is_unique_violation, Db};
use crate::error::ApiError;
use crate::models::group::DEFAULT_GROUP_ID;
use crate::models::quota::QuotaState;
use crate::models::token::{
    normalize_allowed_models, APITokenCreatedOut, APITokenIn, APITokenOut, APITokenUpdateIn,
};
use crate::models::token::utc_now_timestamp;

const TOKEN_COLUMNS: &str = "t.id, t.name, t.description, t.token_preview, \
    COALESCE(t.token_plain, ''), t.enabled, t.expires_at, t.created_at, t.updated_at, \
    COALESCE(t.group_id, 1), \
    COALESCE((SELECT g.name FROM groups g WHERE g.id = t.group_id), 'default'), \
    COALESCE(t.used_tokens, 0), t.limit_tokens, t.rate_limit, \
    COALESCE(t.allowed_models, '[]')";
const TOKEN_FROM: &str = " FROM api_tokens AS t";
const DETACH_LOG_BATCH: i64 = 5000;

struct RawToken {
    id: i64,
    name: String,
    description: String,
    token_preview: String,
    token: String,
    enabled: i64,
    expires_at: Option<String>,
    created_at: String,
    updated_at: String,
    group_id: i64,
    group_name: String,
    used_tokens: i64,
    limit_tokens: Option<i64>,
    rate_limit: Option<String>,
    allowed_models: String,
}

fn read_token(row: &rusqlite::Row<'_>) -> rusqlite::Result<RawToken> {
    Ok(RawToken {
        id: row.get(0)?,
        name: row.get(1)?,
        description: row.get(2)?,
        token_preview: row.get(3)?,
        token: row.get(4)?,
        enabled: row.get(5)?,
        expires_at: row.get(6)?,
        created_at: row.get(7)?,
        updated_at: row.get(8)?,
        group_id: row.get(9)?,
        group_name: row.get(10)?,
        used_tokens: row.get(11)?,
        limit_tokens: row.get(12)?,
        rate_limit: row.get(13)?,
        allowed_models: row.get(14)?,
    })
}

fn decode_allowed_models(raw: &str) -> Vec<String> {
    serde_json::from_str(raw).unwrap_or_default()
}

fn into_out(raw: RawToken) -> APITokenOut {
    let limit_tokens = raw.limit_tokens.filter(|value| *value > 0);
    let rate_limit = raw.rate_limit.filter(|value| !value.is_empty());
    APITokenOut {
        id: raw.id,
        name: raw.name,
        description: raw.description,
        token: raw.token,
        token_preview: raw.token_preview,
        enabled: raw.enabled == 1,
        expires_at: raw.expires_at,
        created_at: raw.created_at,
        updated_at: raw.updated_at,
        group_id: raw.group_id,
        group_name: raw.group_name,
        quota: QuotaState::new(raw.used_tokens, limit_tokens),
        rate_limit,
        allowed_models: decode_allowed_models(&raw.allowed_models),
    }
}

pub async fn list_tokens(db: &Db) -> Result<Vec<APITokenOut>, ApiError> {
    db.with_conn(|connection| {
        let query = format!("SELECT {TOKEN_COLUMNS}{TOKEN_FROM} ORDER BY t.id ASC");
        let mut statement = connection.prepare(&query).map_err(ApiError::database)?;
        let rows = statement
            .query_map([], read_token)
            .map_err(ApiError::database)?;
        let mut tokens = Vec::new();
        for row in rows {
            tokens.push(into_out(row.map_err(ApiError::database)?));
        }
        Ok(tokens)
    })
    .await
}

fn get_conn(connection: &Connection, id: i64) -> Result<Option<APITokenOut>, ApiError> {
    let query = format!("SELECT {TOKEN_COLUMNS}{TOKEN_FROM} WHERE t.id = ?1");
    match connection.query_row(&query, params![id], read_token) {
        Ok(raw) => Ok(Some(into_out(raw))),
        Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
        Err(error) => Err(ApiError::database(error)),
    }
}

pub async fn get_token(db: &Db, id: i64) -> Result<Option<APITokenOut>, ApiError> {
    db.with_conn(move |connection| get_conn(connection, id))
        .await
}

fn reject_past_expiry(expires_at: Option<&str>) -> Result<(), ApiError> {
    let Some(value) = expires_at else {
        return Ok(());
    };
    let now = utc_now_timestamp();
    if value <= now.as_str() {
        return Err(ApiError::bad_request(
            "token expiry must be in the future; disable or delete the token to revoke it now",
        ));
    }
    Ok(())
}

fn resolve_token_group(connection: &Connection, requested: Option<i64>) -> Result<i64, ApiError> {
    let group_id = requested
        .filter(|value| *value > 0)
        .unwrap_or(DEFAULT_GROUP_ID);
    let count: i64 = connection
        .query_row(
            "SELECT COUNT(*) FROM groups WHERE id = ?1",
            params![group_id],
            |row| row.get(0),
        )
        .map_err(ApiError::database)?;
    if count != 1 {
        return Err(ApiError::bad_request("group does not exist"));
    }
    Ok(group_id)
}

fn encode_allowed_models(allowed: &[String]) -> Result<String, ApiError> {
    serde_json::to_string(allowed)
        .map_err(|_| ApiError::internal("could not encode allowed models"))
}

pub async fn create_token(db: &Db, mut input: APITokenIn) -> Result<APITokenCreatedOut, ApiError> {
    input.validate().map_err(ApiError::bad_request)?;
    let expires_at = input
        .normalized_expires_at()
        .map_err(ApiError::bad_request)?;
    reject_past_expiry(expires_at.as_deref())?;

    let token_value = input.token.clone().unwrap_or_else(generate_api_token);
    let digest = token_digest(&token_value);
    let preview = token_preview(&token_value);
    let limit_tokens = input.parsed_limit().map_err(ApiError::bad_request)?;
    let rate_limit = input
        .normalized_rate_limit()
        .map_err(ApiError::bad_request)?;
    let allowed = normalize_allowed_models(&input.allowed_models).map_err(ApiError::bad_request)?;
    let allowed_models = encode_allowed_models(&allowed)?;

    db.transaction(move |transaction| {
        let group_id = resolve_token_group(transaction, input.group_id)?;
        transaction
            .execute(
                "INSERT INTO api_tokens
                    (name, description, token, token_hash, token_preview, token_plain,
                     enabled, expires_at, group_id, limit_tokens, rate_limit, allowed_models,
                     created_at, updated_at)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12,
                         datetime('now'), datetime('now'))",
                params![
                    input.name.trim(),
                    input.description.trim(),
                    digest,
                    digest,
                    preview,
                    token_value,
                    input.enabled as i64,
                    expires_at,
                    group_id,
                    limit_tokens,
                    rate_limit,
                    allowed_models,
                ],
            )
            .map_err(ApiError::database)?;
        let id = transaction.last_insert_rowid();
        let created = get_conn(transaction, id)?
            .ok_or_else(|| ApiError::internal("token was not persisted"))?;
        Ok(APITokenCreatedOut::from(&created))
    })
    .await
}

fn planned_token_replacement(
    connection: &Connection,
    id: i64,
    existing: &APITokenOut,
    input: &APITokenUpdateIn,
) -> Result<String, ApiError> {
    let requested = input.requested_token();
    if requested.is_empty() || requested == existing.token {
        return Ok(String::new());
    }
    let taken: i64 = connection
        .query_row(
            "SELECT COUNT(*) FROM api_tokens WHERE token_hash = ?1 AND id <> ?2",
            params![token_digest(requested), id],
            |row| row.get(0),
        )
        .map_err(ApiError::database)?;
    if taken != 0 {
        return Err(ApiError::bad_request(
            "token value is already used by another token",
        ));
    }
    Ok(requested.to_string())
}

fn opt_text(value: Option<&String>) -> Value {
    value
        .map(|value| Value::Text(value.clone()))
        .unwrap_or(Value::Null)
}

fn opt_int(value: Option<i64>) -> Value {
    value.map(Value::Integer).unwrap_or(Value::Null)
}

pub async fn update_token(
    db: &Db,
    id: i64,
    mut input: APITokenUpdateIn,
) -> Result<APITokenOut, ApiError> {
    input.validate().map_err(ApiError::bad_request)?;
    let expires_at = input
        .normalized_expires_at()
        .map_err(ApiError::bad_request)?;

    db.transaction(move |transaction| {
        let existing =
            get_conn(transaction, id)?.ok_or_else(|| ApiError::not_found("token not found"))?;

        if expires_at != existing.expires_at {
            reject_past_expiry(expires_at.as_deref())?;
        }

        let group_id = resolve_token_group(transaction, input.group_id)?;
        let limit_tokens = input.parsed_limit().map_err(ApiError::bad_request)?;
        let replacement = planned_token_replacement(transaction, id, &existing, &input)?;
        let rate_limit = input
            .normalized_rate_limit()
            .map_err(ApiError::bad_request)?;
        let allowed =
            normalize_allowed_models(&input.allowed_models).map_err(ApiError::bad_request)?;
        let allowed_models = encode_allowed_models(&allowed)?;

        let mut query = String::from(
            "UPDATE api_tokens SET name = ?, description = ?, expires_at = ?,
             group_id = ?, limit_tokens = ?, rate_limit = ?, allowed_models = ?,
             updated_at = datetime('now')",
        );
        let mut values: Vec<Value> = vec![
            Value::Text(input.name.trim().to_string()),
            Value::Text(input.description.trim().to_string()),
            opt_text(expires_at.as_ref()),
            Value::Integer(group_id),
            opt_int(limit_tokens),
            opt_text(rate_limit.as_ref()),
            Value::Text(allowed_models),
        ];
        if let Some(enabled) = input.enabled {
            query.push_str(", enabled = ?");
            values.push(Value::Integer(enabled as i64));
        }
        if !replacement.is_empty() {
            let digest = token_digest(&replacement);
            query.push_str(", token = ?, token_hash = ?, token_preview = ?, token_plain = ?");
            values.push(Value::Text(digest.clone()));
            values.push(Value::Text(digest));
            values.push(Value::Text(token_preview(&replacement)));
            values.push(Value::Text(replacement));
        }
        query.push_str(" WHERE id = ?");
        values.push(Value::Integer(id));

        transaction
            .execute(&query, params_from_iter(values))
            .map_err(|error| {
                let api = ApiError::database(error);
                if is_unique_violation(&api) {
                    ApiError::bad_request("token name or value already exists")
                } else {
                    api
                }
            })?;

        get_conn(transaction, id)?.ok_or_else(|| ApiError::not_found("token not found"))
    })
    .await
}

pub async fn set_token_enabled(db: &Db, id: i64, enabled: bool) -> Result<APITokenOut, ApiError> {
    db.with_conn(move |connection| {
        connection
            .execute(
                "UPDATE api_tokens SET enabled = ?1, updated_at = datetime('now') WHERE id = ?2",
                params![enabled as i64, id],
            )
            .map_err(ApiError::database)?;
        get_conn(connection, id)?.ok_or_else(|| ApiError::not_found("token not found"))
    })
    .await
}

pub async fn reset_token_usage(db: &Db, id: i64) -> Result<APITokenOut, ApiError> {
    db.with_conn(move |connection| {
        connection
            .execute(
                "UPDATE api_tokens SET used_tokens = 0, updated_at = datetime('now') WHERE id = ?1",
                params![id],
            )
            .map_err(ApiError::database)?;
        get_conn(connection, id)?.ok_or_else(|| ApiError::not_found("token not found"))
    })
    .await
}

pub async fn delete_token(db: &Db, id: i64) -> Result<bool, ApiError> {
    db.with_conn(move |connection| {
        loop {
            let affected = connection
                .execute(
                    "UPDATE request_logs SET downstream_token_id = NULL
                     WHERE id IN (SELECT id FROM request_logs
                                  WHERE downstream_token_id = ?1 LIMIT ?2)",
                    params![id, DETACH_LOG_BATCH],
                )
                .map_err(ApiError::database)?;
            if affected < DETACH_LOG_BATCH as usize {
                break;
            }
        }
        let affected = connection
            .execute("DELETE FROM api_tokens WHERE id = ?1", params![id])
            .map_err(ApiError::database)?;
        Ok(affected > 0)
    })
    .await
}
