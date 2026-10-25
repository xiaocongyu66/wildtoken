//! Public endpoints: health, console assets and theme packs.

use std::fs;
use std::path::Path;

use axum::extract::State;
use axum::http::header::{CACHE_CONTROL, CONTENT_TYPE};
use axum::http::{HeaderValue, StatusCode};
use axum::response::{IntoResponse, Redirect, Response};
use axum::Json;
use serde_json::json;

use crate::error::ApiError;
use crate::routes::SharedState;
use crate::themes;

const NO_STORE: &str = "no-store, no-cache, must-revalidate, max-age=0";

pub async fn health(State(state): State<SharedState>) -> Result<Response, ApiError> {
    state
        .db
        .with_conn(|connection| {
            connection
                .query_row("SELECT 1", [], |row| row.get::<_, i64>(0))
                .map(|_| ())
                .map_err(ApiError::database)
        })
        .await?;
    Ok(Json(json!({"status": "ok", "service": "WildToken"})).into_response())
}

pub fn root() -> Response {
    Redirect::to("/console").into_response()
}

pub fn console() -> Result<Response, ApiError> {
    let index = Path::new("web").join("dist").join("index.html");
    match fs::read(&index) {
        Ok(html) => {
            let mut response = html.into_response();
            response.headers_mut().insert(
                CONTENT_TYPE,
                HeaderValue::from_static("text/html; charset=utf-8"),
            );
            response
                .headers_mut()
                .insert(CACHE_CONTROL, HeaderValue::from_static(NO_STORE));
            Ok(response)
        }
        Err(_) => Ok((
            StatusCode::NOT_FOUND,
            "Console build not found; run npm run build in web/",
        )
            .into_response()),
    }
}

pub fn theme_packs(State(state): State<SharedState>) -> Result<Response, ApiError> {
    let packs = themes::list_packs(Path::new(&state.settings.themes.dir))
        .map_err(|error| ApiError::internal(format!("failed to read theme packs: {error}")))?;
    Ok(Json(packs).into_response())
}
