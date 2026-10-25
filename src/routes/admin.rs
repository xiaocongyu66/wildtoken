//! The admin API.

use axum::body::Bytes;
use axum::extract::{Path as AxumPath, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::{Extension, Json};
use time::format_description::well_known::Rfc3339;
use time::OffsetDateTime;

use crate::auth::credential::hash_admin_token;
use crate::db;
use crate::error::ApiError;
use crate::metrics::Snapshot;
use crate::models::group::{GroupIn, DEFAULT_GROUP_ID, DEFAULT_GROUP_NAME};
use crate::models::settings::{
    AdminTokenRotateIn, ModelTestPromptTemplateIn, RuntimeCleanupMetricsOut,
    RuntimeLogSettingsSummary, RuntimeMetricsOut, RuntimeSettingsIn, RuntimeSettingsOut,
    SystemInfoOut,
};
use crate::models::token::{APITokenIn, TokenEnabledIn};
use crate::routes::{decode_body, decode_token_update, parse_id, AdminAuth, SharedState};

pub async fn get_settings(State(state): State<SharedState>) -> Result<Response, ApiError> {
    let settings = state.runtime.get();
    Ok(Json(RuntimeSettingsOut::from_settings(&settings)).into_response())
}

pub async fn update_settings(
    State(state): State<SharedState>,
    body: Bytes,
) -> Result<Response, ApiError> {
    let input: RuntimeSettingsIn = decode_body(&body)?;
    input.validate().map_err(ApiError::bad_request)?;

    let updated = db::settings::update_runtime_settings(&state.db, &input).await?;
    if updated.revision > state.runtime.get().revision {
        state.runtime.set(updated.clone());
    }
    Ok(Json(RuntimeSettingsOut::from_settings(&updated)).into_response())
}

pub async fn list_prompt_templates(State(state): State<SharedState>) -> Result<Response, ApiError> {
    let templates = db::prompts::list_templates(&state.db).await?;
    Ok(Json(templates).into_response())
}

pub async fn create_prompt_template(
    State(state): State<SharedState>,
    body: Bytes,
) -> Result<Response, ApiError> {
    let input: ModelTestPromptTemplateIn = decode_body(&body)?;
    input.validate().map_err(ApiError::bad_request)?;

    match db::prompts::create_template(&state.db, input).await {
        Ok(template) => Ok((StatusCode::CREATED, Json(template)).into_response()),
        Err(error) if db::is_unique_violation(&error) => {
            Err(ApiError::bad_request("prompt template name already exists"))
        }
        Err(error) => Err(error),
    }
}

pub async fn update_prompt_template(
    State(state): State<SharedState>,
    AxumPath(id): AxumPath<String>,
    body: Bytes,
) -> Result<Response, ApiError> {
    let id = parse_id(&id)?;
    let input: ModelTestPromptTemplateIn = decode_body(&body)?;
    input.validate().map_err(ApiError::bad_request)?;

    match db::prompts::update_template(&state.db, id, input).await {
        Ok(Some(template)) => Ok(Json(template).into_response()),
        Ok(None) => Err(ApiError::not_found("model test prompt template not found")),
        Err(error) if db::is_unique_violation(&error) => {
            Err(ApiError::bad_request("prompt template name already exists"))
        }
        Err(error) => Err(error),
    }
}

pub async fn delete_prompt_template(
    State(state): State<SharedState>,
    AxumPath(id): AxumPath<String>,
) -> Result<Response, ApiError> {
    let id = parse_id(&id)?;
    if db::prompts::delete_template(&state.db, id).await? {
        Ok(StatusCode::NO_CONTENT.into_response())
    } else {
        Err(ApiError::not_found("model test prompt template not found"))
    }
}

pub async fn rotate_admin_token(
    State(state): State<SharedState>,
    Extension(auth): Extension<AdminAuth>,
    body: Bytes,
) -> Result<Response, ApiError> {
    let input: AdminTokenRotateIn = decode_body(&body)?;
    if !input.confirm {
        return Err(ApiError::bad_request("explicit confirmation is required"));
    }
    let token = input.validated_token().map_err(ApiError::bad_request)?;
    let hash = hash_admin_token(&token)
        .map_err(|_| ApiError::internal("could not hash admin credential"))?;

    let credential = db::admin::rotate_admin_credential(&state.db, hash, auth.credential_version)
        .await?
        .ok_or_else(|| ApiError::conflict("admin credential version conflict"))?;

    state.credentials.publish(credential);
    Ok(StatusCode::NO_CONTENT.into_response())
}

pub async fn system_info(State(state): State<SharedState>) -> Result<Response, ApiError> {
    let database_ok = db::stats::database_reachable(&state.db).await.is_ok();
    let database_allocated_bytes = if database_ok {
        db::stats::database_allocated_bytes(&state.db).await.ok()
    } else {
        None
    };
    let total_log_count = db::stats::total_log_count(&state.db).await.unwrap_or(0);
    let log_count_24h = db::stats::log_count_24h(&state.db).await.unwrap_or(0);
    let recent_one_minute_log_count = db::stats::recent_one_minute_log_count(&state.db)
        .await
        .unwrap_or(0);
    let (enabled_upstream_count, total_upstream_count) = db::stats::upstream_counts(&state.db)
        .await
        .unwrap_or((0, 0));

    let settings = state.runtime.get();
    let now = OffsetDateTime::now_utc();
    let uptime_seconds = (now - state.started_at).whole_seconds().max(0) as u64;

    let info = SystemInfoOut {
        service: "WildToken".to_string(),
        version: env!("CARGO_PKG_VERSION").to_string(),
        default_upstream_timeout_seconds: state.settings.upstream.default_timeout_seconds,
        uptime_seconds,
        current_server_time: now.format(&Rfc3339).unwrap_or_default(),
        database_ok,
        database_allocated_bytes,
        total_log_count,
        log_count_24h,
        enabled_upstream_count,
        total_upstream_count,
        recent_one_minute_log_count,
        runtime_log_settings: RuntimeLogSettingsSummary {
            log_body_keep_count: settings.log_body_keep_count,
            log_retention_days: settings.log_retention_days,
            log_body_max_bytes: settings.log_body_max_bytes,
            revision: settings.revision,
        },
        runtime_metrics: runtime_metrics_out(&state.metrics.snapshot()),
    };
    Ok(Json(info).into_response())
}

pub async fn runtime_metrics(State(state): State<SharedState>) -> Result<Response, ApiError> {
    Ok(Json(runtime_metrics_out(&state.metrics.snapshot())).into_response())
}

fn runtime_metrics_out(snapshot: &Snapshot) -> RuntimeMetricsOut {
    RuntimeMetricsOut {
        active_sse_streams: snapshot.active_sse_streams,
        sse_completed_total: snapshot.sse_completed_total,
        sse_client_disconnects_total: snapshot.sse_client_disconnects_total,
        sse_recent_disconnects_10m: snapshot.sse_recent_disconnects_10m,
        sse_upstream_errors_total: snapshot.sse_upstream_errors_total,
        log_queue_depth: snapshot.log_queue_depth,
        log_written_total: snapshot.log_written_total,
        log_write_batches_total: snapshot.log_write_batches_total,
        log_dropped_total: snapshot.log_dropped_total,
        log_write_failures_total: snapshot.log_write_failures_total,
        slow_db_operations_total: snapshot.slow_db_operations_total,
        cleanup: RuntimeCleanupMetricsOut {
            active: snapshot.cleanup.active,
            runs_total: snapshot.cleanup.runs_total,
            errors_total: snapshot.cleanup.errors_total,
            rows_cleared_total: snapshot.cleanup.rows_cleared_total,
            batches_total: snapshot.cleanup.batches_total,
            current_rows_cleared: snapshot.cleanup.current_rows_cleared,
            current_batches: snapshot.cleanup.current_batches,
            last_started_unix_seconds: snapshot.cleanup.last_started_unix_seconds,
            last_finished_unix_seconds: snapshot.cleanup.last_finished_unix_seconds,
            last_duration_ms: snapshot.cleanup.last_duration_ms,
            last_rows_cleared: snapshot.cleanup.last_rows_cleared,
        },
    }
}

pub async fn list_tokens(State(state): State<SharedState>) -> Result<Response, ApiError> {
    let tokens = db::tokens::list_tokens(&state.db).await?;
    Ok(Json(tokens).into_response())
}

pub async fn get_token(
    State(state): State<SharedState>,
    AxumPath(id): AxumPath<String>,
) -> Result<Response, ApiError> {
    let id = parse_id(&id)?;
    db::tokens::get_token(&state.db, id)
        .await?
        .map(|token| Json(token).into_response())
        .ok_or_else(|| ApiError::not_found("token not found"))
}

pub async fn create_token(
    State(state): State<SharedState>,
    body: Bytes,
) -> Result<Response, ApiError> {
    let input: APITokenIn = decode_body(&body)?;
    match db::tokens::create_token(&state.db, input).await {
        Ok(created) => Ok((StatusCode::CREATED, Json(created)).into_response()),
        Err(error) if db::is_unique_violation(&error) => {
            Err(ApiError::bad_request("token name or value already exists"))
        }
        Err(error) => Err(error),
    }
}

pub async fn update_token(
    State(state): State<SharedState>,
    AxumPath(id): AxumPath<String>,
    body: Bytes,
) -> Result<Response, ApiError> {
    let id = parse_id(&id)?;
    let input = decode_token_update(&body)?;
    let updated = db::tokens::update_token(&state.db, id, input).await?;
    Ok(Json(updated).into_response())
}

pub async fn set_token_enabled(
    State(state): State<SharedState>,
    AxumPath(id): AxumPath<String>,
    body: Bytes,
) -> Result<Response, ApiError> {
    let id = parse_id(&id)?;
    let input: TokenEnabledIn = decode_body(&body)?;
    let enabled = input.value().map_err(ApiError::bad_request)?;
    let updated = db::tokens::set_token_enabled(&state.db, id, enabled).await?;
    Ok(Json(updated).into_response())
}

pub async fn reset_token_usage(
    State(state): State<SharedState>,
    AxumPath(id): AxumPath<String>,
) -> Result<Response, ApiError> {
    let id = parse_id(&id)?;
    let updated = db::tokens::reset_token_usage(&state.db, id).await?;
    Ok(Json(updated).into_response())
}

pub async fn delete_token(
    State(state): State<SharedState>,
    AxumPath(id): AxumPath<String>,
) -> Result<Response, ApiError> {
    let id = parse_id(&id)?;
    if db::tokens::delete_token(&state.db, id).await? {
        Ok(StatusCode::NO_CONTENT.into_response())
    } else {
        Err(ApiError::not_found("token not found"))
    }
}

pub async fn list_groups(State(state): State<SharedState>) -> Result<Response, ApiError> {
    let groups = db::groups::list_groups(&state.db).await?;
    Ok(Json(groups).into_response())
}

pub async fn get_group(
    State(state): State<SharedState>,
    AxumPath(id): AxumPath<String>,
) -> Result<Response, ApiError> {
    let id = parse_id(&id)?;
    db::groups::get_group(&state.db, id)
        .await?
        .map(|group| Json(group).into_response())
        .ok_or_else(|| ApiError::not_found("group not found"))
}

pub async fn create_group(
    State(state): State<SharedState>,
    body: Bytes,
) -> Result<Response, ApiError> {
    let mut input: GroupIn = decode_body(&body)?;
    input.validate().map_err(ApiError::bad_request)?;
    match db::groups::create_group(&state.db, &input).await {
        Ok(group) => Ok((StatusCode::CREATED, Json(group)).into_response()),
        Err(error) if db::is_unique_violation(&error) => {
            Err(ApiError::bad_request("group name already exists"))
        }
        Err(error) => Err(error),
    }
}

pub async fn update_group(
    State(state): State<SharedState>,
    AxumPath(id): AxumPath<String>,
    body: Bytes,
) -> Result<Response, ApiError> {
    let id = parse_id(&id)?;
    let mut input: GroupIn = decode_body(&body)?;
    input.validate().map_err(ApiError::bad_request)?;
    if id == DEFAULT_GROUP_ID && input.name != DEFAULT_GROUP_NAME {
        return Err(ApiError::bad_request("the default group cannot be renamed"));
    }

    match db::groups::update_group(&state.db, id, &input).await {
        Ok(Some(group)) => Ok(Json(group).into_response()),
        Ok(None) => Err(ApiError::not_found("group not found")),
        Err(error) if db::is_unique_violation(&error) => {
            Err(ApiError::bad_request("group name already exists"))
        }
        Err(error) => Err(error),
    }
}

pub async fn delete_group(
    State(state): State<SharedState>,
    AxumPath(id): AxumPath<String>,
) -> Result<Response, ApiError> {
    let id = parse_id(&id)?;
    if db::groups::delete_group(&state.db, id).await? {
        Ok(StatusCode::NO_CONTENT.into_response())
    } else {
        Err(ApiError::not_found("group not found"))
    }
}
