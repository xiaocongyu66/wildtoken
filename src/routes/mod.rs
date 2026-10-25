//! HTTP surface: public endpoints and the admin API.

pub mod admin;
pub mod public;

use std::net::SocketAddr;
use std::sync::Arc;

use axum::extract::{ConnectInfo, DefaultBodyLimit, Request};
use axum::http::{HeaderName, StatusCode};
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::routing::{delete, get, patch, post, put};
use axum::{Json, Router};
use serde::de::DeserializeOwned;
use serde_json::json;

use crate::auth::client_ip::resolve_client;
use crate::error::ApiError;
use crate::models::token::{missing_token_fields, APITokenUpdateIn};
use crate::state::AppState;

pub type SharedState = Arc<AppState>;

/// What the admin middleware attached to an authorized request.
#[derive(Debug, Clone, Copy)]
pub struct AdminAuth {
    pub credential_version: i64,
}

pub fn router(state: SharedState) -> Router {
    let body_limited_admin = Router::new()
        .route(
            "/settings",
            get(admin::get_settings).put(admin::update_settings),
        )
        .route(
            "/settings/",
            get(admin::get_settings).put(admin::update_settings),
        )
        .route(
            "/settings/model-test-prompts",
            get(admin::list_prompt_templates).post(admin::create_prompt_template),
        )
        .route(
            "/settings/model-test-prompts/{id}",
            patch(admin::update_prompt_template).delete(admin::delete_prompt_template),
        )
        .route(
            "/settings/admin-token/rotate",
            post(admin::rotate_admin_token),
        )
        .route("/system", get(admin::system_info))
        .route("/system/metrics", get(admin::runtime_metrics))
        .route("/tokens", get(admin::list_tokens).post(admin::create_token))
        .route(
            "/tokens/",
            get(admin::list_tokens).post(admin::create_token),
        )
        .route(
            "/tokens/{id}",
            get(admin::get_token)
                .put(admin::update_token)
                .delete(admin::delete_token),
        )
        .route("/tokens/{id}/enabled", patch(admin::set_token_enabled))
        .route("/tokens/{id}/usage/reset", post(admin::reset_token_usage))
        .route("/groups", get(admin::list_groups).post(admin::create_group))
        .route(
            "/groups/",
            get(admin::list_groups).post(admin::create_group),
        )
        .route(
            "/groups/{id}",
            get(admin::get_group)
                .put(admin::update_group)
                .delete(admin::delete_group),
        )
        .layer(DefaultBodyLimit::max(4 * 1024 * 1024))
        .route_layer(middleware::from_fn_with_state(state.clone(), require_admin));

    Router::new()
        .route("/health", get(public::health))
        .route("/", get(public::root))
        .route("/console", get(public::console))
        .route("/api/themes", get(public::theme_packs))
        .nest("/api/admin", body_limited_admin)
        .layer(DefaultBodyLimit::disable())
        .with_state(state)
}

async fn require_admin(
    axum::extract::State(state): axum::extract::State<SharedState>,
    mut request: Request,
    next: Next,
) -> Response {
    let token = request
        .headers()
        .get("x-admin-token")
        .and_then(|value| value.to_str().ok())
        .map(str::trim)
        .filter(|token| !token.is_empty())
        .map(str::to_string);
    let Some(token) = token else {
        return unauthorized();
    };

    let peer = request
        .extensions()
        .get::<ConnectInfo<SocketAddr>>()
        .map(|info| info.0.ip());
    let header_name = state.settings.admin.client_ip_header.trim();
    let forwarded = if header_name.is_empty() {
        None
    } else {
        HeaderName::from_bytes(header_name.as_bytes())
            .ok()
            .and_then(|name| request.headers().get(name))
            .and_then(|value| value.to_str().ok())
    };
    let client = resolve_client(peer, !header_name.is_empty(), forwarded);

    match state.credentials.authenticate(&token, client).await {
        Some(credential_version) => {
            request
                .extensions_mut()
                .insert(AdminAuth { credential_version });
            next.run(request).await
        }
        None => unauthorized(),
    }
}

fn unauthorized() -> Response {
    (
        StatusCode::UNAUTHORIZED,
        Json(json!({"error": "unauthorized"})),
    )
        .into_response()
}

pub(crate) fn decode_body<T: DeserializeOwned>(body: &[u8]) -> Result<T, ApiError> {
    serde_json::from_slice(body)
        .map_err(|error| ApiError::bad_request(format!("invalid request body: {error}")))
}

pub(crate) fn decode_token_update(body: &[u8]) -> Result<APITokenUpdateIn, ApiError> {
    let value: serde_json::Value = serde_json::from_slice(body)
        .map_err(|error| ApiError::bad_request(format!("invalid request body: {error}")))?;
    let object = value
        .as_object()
        .ok_or_else(|| ApiError::bad_request("invalid request body: expected a JSON object"))?;
    let missing = missing_token_fields(object);
    if !missing.is_empty() {
        return Err(ApiError::bad_request(format!(
            "missing fields: {}",
            missing.join(", ")
        )));
    }
    serde_json::from_value(value)
        .map_err(|error| ApiError::bad_request(format!("invalid request body: {error}")))
}

pub(crate) fn parse_id(value: &str) -> Result<i64, ApiError> {
    value
        .parse()
        .map_err(|_| ApiError::bad_request("id must be an integer"))
}
