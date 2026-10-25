//! Domain failures mapped onto the `{"error": ...}` body every client expects.

use axum::Json;
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use serde_json::json;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    NotFound,
    BadRequest,
    Conflict,
    Upstream,
    Internal,
    Database,
    Json,
    GatewayTimeout,
}

#[derive(Debug, Clone)]
pub struct ApiError {
    pub kind: Kind,
    pub message: String,
}

impl ApiError {
    pub fn new(kind: Kind, message: impl Into<String>) -> Self {
        Self {
            kind,
            message: message.into(),
        }
    }

    pub fn not_found(message: impl Into<String>) -> Self {
        Self::new(Kind::NotFound, message)
    }

    pub fn bad_request(message: impl Into<String>) -> Self {
        Self::new(Kind::BadRequest, message)
    }

    pub fn conflict(message: impl Into<String>) -> Self {
        Self::new(Kind::Conflict, message)
    }

    pub fn upstream(message: impl Into<String>) -> Self {
        Self::new(Kind::Upstream, message)
    }

    pub fn internal(message: impl Into<String>) -> Self {
        Self::new(Kind::Internal, message)
    }

    pub fn database(error: impl std::fmt::Display) -> Self {
        Self::new(Kind::Database, error.to_string())
    }

    pub fn json(error: impl std::fmt::Display) -> Self {
        Self::new(Kind::Json, error.to_string())
    }

    pub fn gateway_timeout(message: impl Into<String>) -> Self {
        Self::new(Kind::GatewayTimeout, message)
    }

    /// The client-visible status and message. Failures whose detail must not
    /// reach the caller are logged here, matching the Go implementation.
    pub fn status_and_message(&self) -> (StatusCode, String) {
        match self.kind {
            Kind::NotFound => (StatusCode::NOT_FOUND, self.message.clone()),
            Kind::BadRequest => (StatusCode::BAD_REQUEST, self.message.clone()),
            Kind::Conflict => (StatusCode::CONFLICT, self.message.clone()),
            Kind::Upstream => (StatusCode::BAD_GATEWAY, self.message.clone()),
            Kind::GatewayTimeout => (StatusCode::GATEWAY_TIMEOUT, self.message.clone()),
            Kind::Database => {
                tracing::error!(error = %self.message, "database error");
                (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    "internal database error".to_string(),
                )
            }
            Kind::Json => {
                tracing::error!(error = %self.message, "JSON error");
                (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    "internal JSON error".to_string(),
                )
            }
            Kind::Internal => {
                tracing::error!(detail = %self.message, "internal error");
                (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    "internal server error".to_string(),
                )
            }
        }
    }

    pub fn json_body(&self) -> serde_json::Value {
        let (_, message) = self.status_and_message();
        json!({ "error": message })
    }
}

impl std::fmt::Display for ApiError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(formatter, "{}", self.message)
    }
}

impl std::error::Error for ApiError {}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        let (status, message) = self.status_and_message();
        (status, Json(json!({ "error": message }))).into_response()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn internal_details_stay_out_of_the_body() {
        let error = ApiError::database("no such table: secrets");
        let (status, message) = error.status_and_message();
        assert_eq!(status, StatusCode::INTERNAL_SERVER_ERROR);
        assert_eq!(message, "internal database error");
        assert_eq!(error.json_body()["error"], "internal database error");
    }

    #[test]
    fn client_errors_keep_their_message() {
        let error = ApiError::bad_request("name is required");
        assert_eq!(error.json_body()["error"], "name is required");
    }
}
