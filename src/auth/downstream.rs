//! Downstream API-token authentication and its two vendor error shapes.

use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::Json;
use rusqlite::params;
use serde_json::{json, Value};

use crate::db::token::token_digest;
use crate::db::Db;
use crate::error::ApiError;
use crate::models::group::DEFAULT_GROUP_ID;

/// Identifies the API token a proxied request presented.
#[derive(Debug, Clone, PartialEq)]
pub struct DownstreamCredential {
    pub token_id: i64,
    pub token_name: String,
    pub group_id: i64,
    pub used_tokens: i64,
    pub limit_tokens: Option<i64>,
    /// The stored rate expression (`100/m`), `None` when unlimited.
    pub rate_limit: Option<String>,
    /// Empty when the token may request any model.
    pub allowed_models: Vec<String>,
}

/// Reads a bearer token, falling back to `x-api-key` for Anthropic requests.
pub fn extract_downstream_token(headers: &HeaderMap, anthropic: bool) -> Option<String> {
    if let Some(value) = headers
        .get("authorization")
        .and_then(|value| value.to_str().ok())
    {
        if let Some((scheme, credentials)) = value.split_once(' ') {
            if scheme.eq_ignore_ascii_case("bearer") {
                let token = credentials.trim();
                if !token.is_empty() {
                    return Some(token.to_string());
                }
            }
        }
    }
    if !anthropic {
        return None;
    }
    headers
        .get("x-api-key")
        .and_then(|value| value.to_str().ok())
        .map(str::trim)
        .filter(|token| !token.is_empty())
        .map(str::to_string)
}

/// Labels the caller from its originator and user-agent headers.
pub fn detect_client_type(headers: &HeaderMap, anthropic: bool) -> &'static str {
    let originator = header_lower(headers, "originator");
    let user_agent = header_lower(headers, "user-agent");

    if originator.contains("codex desktop") {
        return "codex-desktop";
    }
    if originator.contains("codex-tui") {
        return "codex-tui";
    }
    if user_agent.contains("codex desktop") {
        return "codex-desktop";
    }
    if user_agent.contains("codex-tui") {
        return "codex-tui";
    }
    if user_agent.contains("opencode") {
        return "opencode";
    }
    if is_pi_user_agent(&user_agent) {
        return "pi";
    }
    if originator.contains("codex") || user_agent.contains("codex") {
        return "codex";
    }
    if anthropic || user_agent.contains("claude") || headers.contains_key("anthropic-version") {
        return "claude";
    }
    "unknown"
}

fn header_lower(headers: &HeaderMap, name: &str) -> String {
    headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .unwrap_or("")
        .to_lowercase()
}

/// Matched on a prefix rather than a substring: "pi" is two letters that sit
/// inside copilot, rapidapi, and other agent names.
fn is_pi_user_agent(user_agent: &str) -> bool {
    user_agent == "pi"
        || user_agent.starts_with("pi ")
        || user_agent.starts_with("pi/")
        || user_agent.starts_with("pi-coding-agent")
}

/// Resolves a token to its row. An expired token is indistinguishable from a
/// disabled or nonexistent one.
pub async fn lookup_enabled_downstream_token(
    db: &Db,
    token: &str,
) -> Result<Option<DownstreamCredential>, ApiError> {
    if token.is_empty() {
        return Ok(None);
    }

    let digest = token_digest(token);
    db.with_conn(move |connection| {
        let mut statement = connection
            .prepare(
                "SELECT id, name, group_id, COALESCE(used_tokens, 0), limit_tokens, rate_limit,
                        COALESCE(allowed_models, '[]')
                 FROM api_tokens
                 WHERE token_hash = ?1 AND enabled = 1
                   AND (expires_at IS NULL OR expires_at > datetime('now'))",
            )
            .map_err(ApiError::database)?;

        let row = statement.query_row(params![digest], |row| {
            Ok((
                row.get::<_, i64>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, Option<i64>>(2)?,
                row.get::<_, i64>(3)?,
                row.get::<_, Option<i64>>(4)?,
                row.get::<_, Option<String>>(5)?,
                row.get::<_, String>(6)?,
            ))
        });

        match row {
            Ok((
                token_id,
                token_name,
                group_id,
                used_tokens,
                limit_tokens,
                rate_limit,
                allowed,
            )) => {
                let allowed_models: Vec<String> =
                    serde_json::from_str(&allowed).unwrap_or_default();
                Ok(Some(DownstreamCredential {
                    token_id,
                    token_name,
                    group_id: group_id
                        .filter(|value| *value > 0)
                        .unwrap_or(DEFAULT_GROUP_ID),
                    used_tokens,
                    limit_tokens: limit_tokens.filter(|value| *value > 0),
                    rate_limit: rate_limit.filter(|value| !value.is_empty()),
                    allowed_models,
                }))
            }
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
            Err(error) => Err(ApiError::database(error)),
        }
    })
    .await
}

const QUOTA_EXHAUSTED_CODE: &str = "API_KEY_QUOTA_EXHAUSTED";
const QUOTA_EXHAUSTED_MESSAGE: &str = "API key 额度已用完";
const RATE_LIMITED_CODE: &str = "API_KEY_RATE_LIMITED";
const RATE_LIMITED_MESSAGE: &str = "API key 请求频率超限";

pub fn rejection_body(anthropic: bool, message: &str) -> Value {
    if anthropic {
        json!({
            "type": "error",
            "error": {"type": "authentication_error", "message": message},
        })
    } else {
        json!({
            "error": {
                "message": message,
                "type": "invalid_api_key",
                "code": "invalid_api_key",
            },
        })
    }
}

pub fn quota_rejection_body(anthropic: bool, detail: &str) -> Value {
    let mut body = json!({
        "code": QUOTA_EXHAUSTED_CODE,
        "message": QUOTA_EXHAUSTED_MESSAGE,
    });
    if anthropic {
        body["type"] = json!("error");
        body["error"] = json!({"type": "rate_limit_error", "message": detail});
    } else {
        body["error"] = json!({
            "message": detail,
            "type": "insufficient_quota",
            "code": "insufficient_quota",
        });
    }
    body
}

pub fn rate_limit_rejection_body(anthropic: bool, rate_limit: &str) -> Value {
    let detail = format!("请求频率超过限制（{rate_limit}），请稍后重试");
    let mut body = json!({
        "code": RATE_LIMITED_CODE,
        "message": RATE_LIMITED_MESSAGE,
    });
    if anthropic {
        body["type"] = json!("error");
        body["error"] = json!({"type": "rate_limit_error", "message": detail});
    } else {
        body["error"] = json!({
            "message": detail,
            "type": "rate_limit_exceeded",
            "code": "rate_limit_exceeded",
        });
    }
    body
}

pub fn rejection_response(anthropic: bool, status: StatusCode, message: &str) -> Response {
    (status, Json(rejection_body(anthropic, message))).into_response()
}

pub fn quota_rejection_response(anthropic: bool, detail: &str) -> Response {
    (
        StatusCode::TOO_MANY_REQUESTS,
        Json(quota_rejection_body(anthropic, detail)),
    )
        .into_response()
}

pub fn rate_limit_rejection_response(anthropic: bool, rate_limit: &str) -> Response {
    (
        StatusCode::TOO_MANY_REQUESTS,
        Json(rate_limit_rejection_body(anthropic, rate_limit)),
    )
        .into_response()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::DatabaseSettings;
    use axum::http::HeaderValue;

    fn headers(pairs: &[(&'static str, &str)]) -> HeaderMap {
        let mut headers = HeaderMap::new();
        for (name, value) in pairs {
            headers.insert(*name, HeaderValue::from_str(value).unwrap());
        }
        headers
    }

    #[test]
    fn bearer_tokens_win_and_x_api_key_is_anthropic_only() {
        let openai = headers(&[("authorization", "Bearer sk-one")]);
        assert_eq!(
            extract_downstream_token(&openai, false),
            Some("sk-one".to_string())
        );

        let anthropic = headers(&[("x-api-key", "sk-two")]);
        assert_eq!(
            extract_downstream_token(&anthropic, true),
            Some("sk-two".to_string())
        );
        assert_eq!(extract_downstream_token(&anthropic, false), None);

        let both = headers(&[("authorization", "bearer sk-one"), ("x-api-key", "sk-two")]);
        assert_eq!(
            extract_downstream_token(&both, true),
            Some("sk-one".to_string())
        );

        assert_eq!(extract_downstream_token(&headers(&[]), true), None);
        assert_eq!(
            extract_downstream_token(&headers(&[("authorization", "Basic abc")]), true),
            None
        );
    }

    #[test]
    fn client_types_follow_the_original_order() {
        let cases = [
            (&[("originator", "codex desktop")][..], "codex-desktop"),
            (&[("originator", "codex-tui")][..], "codex-tui"),
            (&[("user-agent", "Codex Desktop/1")][..], "codex-desktop"),
            (&[("user-agent", "codex-tui/1")][..], "codex-tui"),
            (&[("user-agent", "opencode/1")][..], "opencode"),
            (&[("user-agent", "pi (linux 6.1.0; x64)")][..], "pi"),
            (&[("user-agent", "copilot")][..], "unknown"),
            (&[("user-agent", "codex/1")][..], "codex"),
            (&[("user-agent", "claude-cli/1")][..], "claude"),
            (&[("anthropic-version", "2023-06-01")][..], "claude"),
            (&[][..], "unknown"),
        ];
        for (pairs, expected) in cases {
            let map = headers(pairs);
            assert_eq!(detect_client_type(&map, false), expected, "{pairs:?}");
        }
        assert_eq!(detect_client_type(&headers(&[]), true), "claude");
    }

    #[test]
    fn pi_is_recognised_by_prefix_not_substring() {
        assert!(is_pi_user_agent("pi"));
        assert!(is_pi_user_agent("pi 0.9"));
        assert!(is_pi_user_agent("pi/1.0"));
        assert!(!is_pi_user_agent("copilot"));
        assert!(!is_pi_user_agent("rapidapi"));
    }

    #[test]
    fn rejection_bodies_match_both_vendor_shapes() {
        let openai = rejection_body(false, "Incorrect API key provided");
        assert_eq!(openai["error"]["type"], "invalid_api_key");
        assert_eq!(openai["error"]["code"], "invalid_api_key");
        assert_eq!(openai["error"]["message"], "Incorrect API key provided");

        let anthropic = rejection_body(true, "Incorrect API key provided");
        assert_eq!(anthropic["type"], "error");
        assert_eq!(anthropic["error"]["type"], "authentication_error");

        let quota = quota_rejection_body(false, "token quota exhausted: 1 of 1 tokens used");
        assert_eq!(quota["code"], "API_KEY_QUOTA_EXHAUSTED");
        assert_eq!(quota["message"], "API key 额度已用完");
        assert_eq!(quota["error"]["type"], "insufficient_quota");
        assert_eq!(
            quota["error"]["message"],
            "token quota exhausted: 1 of 1 tokens used"
        );

        let rate = rate_limit_rejection_body(true, "100/m");
        assert_eq!(rate["code"], "API_KEY_RATE_LIMITED");
        assert_eq!(rate["error"]["type"], "rate_limit_error");
        assert_eq!(
            rate["error"]["message"],
            "请求频率超过限制（100/m），请稍后重试"
        );
    }

    #[tokio::test]
    async fn lookup_finds_only_enabled_unexpired_tokens() {
        let dir = tempfile::tempdir().unwrap();
        let settings = DatabaseSettings {
            url: format!("sqlite:{}?mode=rwc", dir.path().join("db.sqlite").display()),
            ..DatabaseSettings::default()
        };
        let db = Db::open(&settings).unwrap();
        db.with_conn(|connection| {
            crate::db::schema::init(connection)?;
            Ok(())
        })
        .await
        .unwrap();

        let digest = token_digest("sk-live");
        db.with_conn(move |connection| {
            connection
                .execute(
                    "INSERT INTO api_tokens
                        (name, token, token_hash, token_preview, token_plain, enabled, expires_at,
                         used_tokens, limit_tokens, rate_limit, allowed_models)
                     VALUES
                        ('live', ?1, ?1, 'sk-liv…', 'sk-live', 1, NULL,
                         42, 100, '100/m', '[\"gpt-4o\"]')",
                    params![digest],
                )
                .map_err(ApiError::database)?;
            connection
                .execute(
                    "INSERT INTO api_tokens
                        (name, token, token_hash, token_preview, token_plain, enabled, expires_at)
                     VALUES ('expired', 'x', ?1, 'x…', 'x', 1, '2000-01-01 00:00:00')",
                    params![token_digest("sk-expired")],
                )
                .map_err(ApiError::database)?;
            connection
                .execute(
                    "INSERT INTO api_tokens
                        (name, token, token_hash, token_preview, token_plain, enabled)
                     VALUES ('disabled', 'y', ?1, 'y…', 'y', 0)",
                    params![token_digest("sk-disabled")],
                )
                .map_err(ApiError::database)?;
            Ok(())
        })
        .await
        .unwrap();

        let found = lookup_enabled_downstream_token(&db, "sk-live")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(found.token_name, "live");
        assert_eq!(found.group_id, DEFAULT_GROUP_ID);
        assert_eq!(found.used_tokens, 42);
        assert_eq!(found.limit_tokens, Some(100));
        assert_eq!(found.rate_limit.as_deref(), Some("100/m"));
        assert_eq!(found.allowed_models, vec!["gpt-4o"]);

        assert_eq!(
            lookup_enabled_downstream_token(&db, "sk-expired")
                .await
                .unwrap(),
            None
        );
        assert_eq!(
            lookup_enabled_downstream_token(&db, "sk-disabled")
                .await
                .unwrap(),
            None
        );
        assert_eq!(
            lookup_enabled_downstream_token(&db, "sk-missing")
                .await
                .unwrap(),
            None
        );
    }
}
