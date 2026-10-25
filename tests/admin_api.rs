//! Contract tests for the admin API added in milestone 3.

use std::sync::Arc;

use axum::body::Body;
use axum::http::{header, Request, StatusCode};
use axum::Router;
use http_body_util::BodyExt;
use serde_json::{json, Value};
use tower::ServiceExt;

use wildtoken::config::{DatabaseSettings, Settings};
use wildtoken::routes;
use wildtoken::state::{self, AppState};

const ADMIN_TOKEN: &str = "change-me";

struct Harness {
    app: Router,
    state: Arc<AppState>,
    _dir: tempfile::TempDir,
}

async fn harness() -> Harness {
    let dir = tempfile::tempdir().unwrap();
    let settings = Settings {
        server: wildtoken::config::ServerSettings {
            host: "127.0.0.1".to_string(),
            port: 0,
        },
        database: DatabaseSettings {
            url: format!("sqlite:{}?mode=rwc", dir.path().join("db.sqlite").display()),
            ..DatabaseSettings::default()
        },
        ..Settings::default()
    };
    let state = Arc::new(state::build(settings).await.unwrap());
    Harness {
        app: routes::router(state.clone()),
        state,
        _dir: dir,
    }
}

struct Reply {
    status: StatusCode,
    body: Vec<u8>,
}

impl Reply {
    fn json(&self) -> Value {
        serde_json::from_slice(&self.body).unwrap_or(Value::Null)
    }

    fn text(&self) -> String {
        String::from_utf8_lossy(&self.body).into_owned()
    }
}

async fn send(
    app: &Router,
    method: &str,
    uri: &str,
    token: Option<&str>,
    body: Option<Value>,
) -> Reply {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(token) = token {
        builder = builder.header("x-admin-token", token);
    }
    let body = match body {
        Some(value) => {
            builder = builder.header(header::CONTENT_TYPE, "application/json");
            Body::from(value.to_string())
        }
        None => Body::empty(),
    };
    let response = app
        .clone()
        .oneshot(builder.body(body).unwrap())
        .await
        .unwrap();
    let status = response.status();
    let body = response
        .into_body()
        .collect()
        .await
        .unwrap()
        .to_bytes()
        .to_vec();
    Reply { status, body }
}

fn admin_settings_payload(revision: i64) -> Value {
    json!({
        "log_body_keep_count": 100,
        "log_retention_days": 30,
        "log_body_max_bytes": 200000,
        "max_retries": 1,
        "same_upstream_retry_interval_ms": 1000,
        "auto_weight_failure_penalty": 20,
        "auto_weight_success_increment": 5,
        "auto_weight_recovery_increment": 10,
        "auto_weight_recovery_interval_seconds": 60,
        "proxy_enabled": false,
        "proxy_url": "",
        "default_upstream_timeout_seconds": 0,
        "image_storage_max_mb": 10240,
        "dashboard_multiplier": 1,
        "revision": revision,
    })
}

#[tokio::test]
async fn health_and_the_admin_gate() {
    let harness = harness().await;

    let health = send(&harness.app, "GET", "/health", None, None).await;
    assert_eq!(health.status, StatusCode::OK);
    assert_eq!(health.json()["status"], "ok");
    assert_eq!(health.json()["service"], "WildToken");

    let unauthorized = send(&harness.app, "GET", "/api/admin/tokens", None, None).await;
    assert_eq!(unauthorized.status, StatusCode::UNAUTHORIZED);
    assert_eq!(unauthorized.json()["error"], "unauthorized");

    let wrong = send(
        &harness.app,
        "GET",
        "/api/admin/tokens",
        Some("wrong"),
        None,
    )
    .await;
    assert_eq!(wrong.status, StatusCode::UNAUTHORIZED);

    let authorized = send(
        &harness.app,
        "GET",
        "/api/admin/tokens",
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(authorized.status, StatusCode::OK);
    assert_eq!(authorized.json(), json!([]));
}

#[tokio::test]
async fn tokens_are_created_read_updated_and_deleted() {
    let harness = harness().await;

    let created = send(
        &harness.app,
        "POST",
        "/api/admin/tokens",
        Some(ADMIN_TOKEN),
        Some(json!({"name": "ci", "description": "smoke", "enabled": true})),
    )
    .await;
    assert_eq!(created.status, StatusCode::CREATED);
    let created = created.json();
    let id = created["id"].as_i64().unwrap();
    let token = created["token"].as_str().unwrap().to_string();
    assert!(token.starts_with("sk-"));
    assert_eq!(created["enabled"], true);
    assert_eq!(created["group_id"], 1);
    assert_eq!(created["group_name"], "default");
    assert_eq!(created["quota"]["limit_tokens"], Value::Null);

    let detail = send(
        &harness.app,
        "GET",
        &format!("/api/admin/tokens/{id}"),
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(detail.status, StatusCode::OK);
    assert_eq!(detail.json()["token"], token);

    let missing = send(
        &harness.app,
        "PUT",
        &format!("/api/admin/tokens/{id}"),
        Some(ADMIN_TOKEN),
        Some(json!({"name": "ci"})),
    )
    .await;
    assert_eq!(missing.status, StatusCode::BAD_REQUEST);
    assert!(missing.text().contains("missing fields"));

    let updated = send(
        &harness.app,
        "PUT",
        &format!("/api/admin/tokens/{id}"),
        Some(ADMIN_TOKEN),
        Some(json!({
            "name": "ci-renamed",
            "description": "",
            "expires_at": null,
            "group_id": 1,
            "limit_expression": "1K",
            "rate_limit": "100/m",
            "allowed_models": ["gpt-4o", "claude-*"],
            "enabled": true,
        })),
    )
    .await;
    assert_eq!(updated.status, StatusCode::OK);
    let updated = updated.json();
    assert_eq!(updated["name"], "ci-renamed");
    assert_eq!(updated["quota"]["limit_tokens"], 1000);
    assert_eq!(updated["rate_limit"], "100/m");
    assert_eq!(updated["allowed_models"], json!(["gpt-4o", "claude-*"]));

    let reset = send(
        &harness.app,
        "POST",
        &format!("/api/admin/tokens/{id}/usage/reset"),
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(reset.status, StatusCode::OK);
    assert_eq!(reset.json()["quota"]["used_tokens"], 0);

    let disabled = send(
        &harness.app,
        "PATCH",
        &format!("/api/admin/tokens/{id}/enabled"),
        Some(ADMIN_TOKEN),
        Some(json!({"enabled": false})),
    )
    .await;
    assert_eq!(disabled.status, StatusCode::OK);
    assert_eq!(disabled.json()["enabled"], false);

    let deleted = send(
        &harness.app,
        "DELETE",
        &format!("/api/admin/tokens/{id}"),
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(deleted.status, StatusCode::NO_CONTENT);
    assert!(deleted.body.is_empty());

    let gone = send(
        &harness.app,
        "GET",
        &format!("/api/admin/tokens/{id}"),
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(gone.status, StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn runtime_settings_use_a_revision_compare_and_swap() {
    let harness = harness().await;

    let initial = send(
        &harness.app,
        "GET",
        "/api/admin/settings",
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(initial.status, StatusCode::OK);
    assert_eq!(initial.json()["revision"], 1);
    assert_eq!(initial.json()["database_override"], true);

    let updated = send(
        &harness.app,
        "PUT",
        "/api/admin/settings",
        Some(ADMIN_TOKEN),
        Some(admin_settings_payload(1)),
    )
    .await;
    assert_eq!(updated.status, StatusCode::OK);
    assert_eq!(updated.json()["revision"], 2);
    assert_eq!(harness.state.runtime.get().revision, 2);

    let stale = send(
        &harness.app,
        "PUT",
        "/api/admin/settings",
        Some(ADMIN_TOKEN),
        Some(admin_settings_payload(1)),
    )
    .await;
    assert_eq!(stale.status, StatusCode::CONFLICT);

    let unknown = send(
        &harness.app,
        "PUT",
        "/api/admin/settings",
        Some(ADMIN_TOKEN),
        Some(json!({"revision": 2, "unexpected": true})),
    )
    .await;
    assert_eq!(unknown.status, StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn groups_keep_the_default_one_protected() {
    let harness = harness().await;

    let groups = send(
        &harness.app,
        "GET",
        "/api/admin/groups",
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(groups.status, StatusCode::OK);
    assert_eq!(groups.json()[0]["name"], "default");
    assert_eq!(groups.json()[0]["is_default"], true);

    let created = send(
        &harness.app,
        "POST",
        "/api/admin/groups",
        Some(ADMIN_TOKEN),
        Some(json!({"name": "vip", "description": "paid"})),
    )
    .await;
    assert_eq!(created.status, StatusCode::CREATED);
    let id = created.json()["id"].as_i64().unwrap();

    let duplicate = send(
        &harness.app,
        "POST",
        "/api/admin/groups",
        Some(ADMIN_TOKEN),
        Some(json!({"name": "vip", "description": ""})),
    )
    .await;
    assert_eq!(duplicate.status, StatusCode::BAD_REQUEST);
    assert_eq!(duplicate.json()["error"], "group name already exists");

    let rename_default = send(
        &harness.app,
        "PUT",
        "/api/admin/groups/1",
        Some(ADMIN_TOKEN),
        Some(json!({"name": "other", "description": ""})),
    )
    .await;
    assert_eq!(rename_default.status, StatusCode::BAD_REQUEST);

    let delete_default = send(
        &harness.app,
        "DELETE",
        "/api/admin/groups/1",
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(delete_default.status, StatusCode::BAD_REQUEST);

    let deleted = send(
        &harness.app,
        "DELETE",
        &format!("/api/admin/groups/{id}"),
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(deleted.status, StatusCode::NO_CONTENT);
}

#[tokio::test]
async fn prompt_templates_system_info_and_rotation() {
    let harness = harness().await;

    let templates = send(
        &harness.app,
        "GET",
        "/api/admin/settings/model-test-prompts",
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(templates.status, StatusCode::OK);
    assert_eq!(templates.json().as_array().unwrap().len(), 10);

    let created = send(
        &harness.app,
        "POST",
        "/api/admin/settings/model-test-prompts",
        Some(ADMIN_TOKEN),
        Some(json!({"name": "custom", "prompt": "say hi"})),
    )
    .await;
    assert_eq!(created.status, StatusCode::CREATED);
    let id = created.json()["id"].as_i64().unwrap();

    let updated = send(
        &harness.app,
        "PATCH",
        &format!("/api/admin/settings/model-test-prompts/{id}"),
        Some(ADMIN_TOKEN),
        Some(json!({"name": "custom-2", "prompt": "say hello"})),
    )
    .await;
    assert_eq!(updated.status, StatusCode::OK);
    assert_eq!(updated.json()["name"], "custom-2");

    let deleted = send(
        &harness.app,
        "DELETE",
        &format!("/api/admin/settings/model-test-prompts/{id}"),
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(deleted.status, StatusCode::NO_CONTENT);

    let system = send(
        &harness.app,
        "GET",
        "/api/admin/system",
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(system.status, StatusCode::OK);
    let system = system.json();
    assert_eq!(system["service"], "WildToken");
    assert_eq!(system["version"], env!("CARGO_PKG_VERSION"));
    assert_eq!(system["database_ok"], true);
    assert_eq!(system["runtime_log_settings"]["revision"], 1);

    let metrics = send(
        &harness.app,
        "GET",
        "/api/admin/system/metrics",
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(metrics.status, StatusCode::OK);
    assert_eq!(metrics.json()["active_sse_streams"], 0);

    let rotated = send(
        &harness.app,
        "POST",
        "/api/admin/settings/admin-token/rotate",
        Some(ADMIN_TOKEN),
        Some(json!({"confirm": true, "token": "new-admin-token"})),
    )
    .await;
    assert_eq!(rotated.status, StatusCode::NO_CONTENT);

    let old = send(
        &harness.app,
        "GET",
        "/api/admin/tokens",
        Some(ADMIN_TOKEN),
        None,
    )
    .await;
    assert_eq!(old.status, StatusCode::UNAUTHORIZED);

    let new = send(
        &harness.app,
        "GET",
        "/api/admin/tokens",
        Some("new-admin-token"),
        None,
    )
    .await;
    assert_eq!(new.status, StatusCode::OK);
}

#[tokio::test]
async fn the_console_and_theme_endpoints_answer() {
    let harness = harness().await;

    let root = send(&harness.app, "GET", "/", None, None).await;
    assert_eq!(root.status, StatusCode::SEE_OTHER);

    let console = send(&harness.app, "GET", "/console", None, None).await;
    // web/dist is built in CI, not committed locally.
    assert!(
        console.status == StatusCode::OK || console.status == StatusCode::NOT_FOUND,
        "unexpected console status: {}",
        console.status
    );

    let themes = send(&harness.app, "GET", "/api/themes", None, None).await;
    assert_eq!(themes.status, StatusCode::OK);
    assert!(themes.json().is_array());
}
