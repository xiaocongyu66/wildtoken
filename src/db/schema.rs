//! SQLite schema, migrations and seeds, kept verbatim so an existing
//! `wildtoken.db` opens unchanged.

use std::collections::HashSet;

use rusqlite::params;
use rusqlite::{Connection, TransactionBehavior};

use crate::db::token::{token_digest, token_preview, unique_marker_prefix};
use crate::error::ApiError;

const CREATE_UPSTREAMS: &str = r#"
CREATE TABLE IF NOT EXISTS upstreams (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL UNIQUE,
    base_url        TEXT NOT NULL,
    api_key         TEXT,
    model_names     TEXT NOT NULL DEFAULT '[]',
    model_prefixes  TEXT NOT NULL DEFAULT '[]',
    model_mappings  TEXT NOT NULL DEFAULT '{}',
    priority        INTEGER NOT NULL DEFAULT 100,
    weight          INTEGER NOT NULL DEFAULT 100 CHECK (weight BETWEEN 0 AND 10000),
    auto_weight_enabled INTEGER NOT NULL DEFAULT 1 CHECK (auto_weight_enabled IN (0, 1)),
    enabled         INTEGER NOT NULL DEFAULT 1,
    archived             INTEGER NOT NULL DEFAULT 0 CHECK (archived IN (0, 1)),
    archived_prev_enabled INTEGER,
    extra_headers   TEXT NOT NULL DEFAULT '{}',
    timeout_seconds REAL NOT NULL DEFAULT 300.0,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);"#;

const CREATE_MODEL_TEST_PROMPT_TEMPLATES: &str = r#"
CREATE TABLE IF NOT EXISTS model_test_prompt_templates (
    id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, prompt TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')), updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);"#;

const SEED_MODEL_TEST_PROMPT_TEMPLATES: &str = r#"INSERT INTO model_test_prompt_templates (name, prompt) VALUES
    ('模型能力概览', '请用中文说明你当前使用的模型名称、两项主要能力，以及用户提交复杂任务时应提供的关键信息。使用自然段，不要使用表格、工具或外部引用。总回复不超过 120 个汉字。'),
    ('代码审查', '请审查以下需求的实现风险：一个 HTTP API 需要支持鉴权、超时、错误处理和请求日志。用三条简短建议说明优先级和原因，不要编造未提供的事实。'),
    ('问题排查', '请给出排查 API 请求失败的步骤。按网络、认证、请求格式、上游响应四个方面排序，每项一句，并说明最先应收集的证据。'),
    ('结构化摘要', '请用三条要点总结：如何把一项复杂工程任务拆分为可验证的步骤。每条不超过 30 个汉字，不要使用表格。'),
    ('中文问答', '请用中文解释为什么客户端超时不一定代表上游服务故障。给出一个简短例子，并说明日志中应重点查看哪些字段。'),
    ('工单信息抽取', '阅读下列工单摘要：用户在周一上午提交订单，支付成功后页面仍显示待付款，客服已确认未重复扣款。请提取时间、现象、已知事实和下一步核查项，按四行中文输出；不得补充未提供的原因。'),
    ('用户向改写', '将下面的产品说明改写为面向普通用户的两段中文：系统会在请求失败后按固定间隔重试三次，并记录每次状态码。要求保留事实、避免技术缩写、每段不超过45个汉字，末尾给出一个简短标题。'),
    ('指标计算', '根据以下数据计算并解释结果：某服务一分钟收到240个请求，其中12个返回5xx，18个因客户端取消，其余成功。请给出成功率和服务端错误率，保留一位小数，并用两句话说明计算口径。'),
    ('发布风险评估', '请判断以下发布方案是否存在风险：先停掉旧版本全部实例，再启动新版本，健康检查失败时由人工回滚。列出三个最重要的风险及对应改进，每条包含“风险：”和“改进：”，不要假设额外基础设施。'),
    ('JSON结构化输出', '请把零散信息整理成 JSON，字段固定为 title、priority、owner、next_step：任务是修复登录超时；优先级高；负责人小林；先收集失败请求的时间范围和状态码。只输出合法 JSON，不要使用 Markdown 代码块。'
    ON CONFLICT(name) DO NOTHING"#;

const CREATE_ADMIN_CREDENTIAL: &str = r#"
CREATE TABLE IF NOT EXISTS admin_credential (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    credential_hash TEXT NOT NULL,
    credential_version INTEGER NOT NULL CHECK (credential_version >= 1),
    rotated_at TEXT NOT NULL DEFAULT (datetime('now'))
);"#;

const CREATE_REQUEST_LOGS: &str = r#"
CREATE TABLE IF NOT EXISTS request_logs (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    method              TEXT NOT NULL,
    path                TEXT NOT NULL,
    downstream_token_id INTEGER REFERENCES api_tokens(id) ON DELETE SET NULL,
    downstream_token_name TEXT,
    client_ip           TEXT,
    client_type         TEXT NOT NULL DEFAULT 'unknown',
    upstream_id         INTEGER REFERENCES upstreams(id) ON DELETE SET NULL,
    upstream_name       TEXT,
    model               TEXT,
    request_model       TEXT,
    upstream_model      TEXT,
    reasoning_effort    TEXT,
    response_reasoning_effort TEXT,
    stream              INTEGER NOT NULL DEFAULT 0,
    status_code         INTEGER,
    prompt_tokens       INTEGER,
    completion_tokens   INTEGER,
    total_tokens        INTEGER,
    prompt_cached_tokens INTEGER,
    cache_creation_tokens INTEGER,
    completion_reasoning_tokens INTEGER,
    duration_ms         INTEGER,
    first_token_ms      INTEGER,
    error               TEXT
);"#;

const CREATE_REQUEST_LOG_PAYLOADS: &str = r#"
CREATE TABLE IF NOT EXISTS request_log_payloads (
    request_log_id INTEGER PRIMARY KEY
        REFERENCES request_logs(id) ON DELETE CASCADE,
    request_snapshot TEXT,
    upstream_request_override TEXT,
    upstream_request_is_override INTEGER NOT NULL DEFAULT 0
        CHECK (upstream_request_is_override IN (0, 1)),
    response_snapshot TEXT,
    downstream_response_override TEXT,
    downstream_response_is_override INTEGER NOT NULL DEFAULT 0
        CHECK (downstream_response_is_override IN (0, 1)),
    bodies_cleared INTEGER NOT NULL DEFAULT 0
        CHECK (bodies_cleared IN (0, 1))
);"#;

const CREATE_API_TOKENS: &str = r#"
CREATE TABLE IF NOT EXISTS api_tokens (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    token       TEXT NOT NULL UNIQUE,
    token_hash  TEXT NOT NULL,
    token_preview TEXT NOT NULL,
    token_plain TEXT,
    enabled     INTEGER NOT NULL DEFAULT 1,
    expires_at  TEXT,
    used_tokens  INTEGER NOT NULL DEFAULT 0,
    limit_tokens INTEGER,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);"#;

const CREATE_GROUPS: &str = r#"
CREATE TABLE IF NOT EXISTS groups (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);"#;

/// Either unique column can already be taken, so the conflict target is
/// deliberately unnamed.
const SEED_DEFAULT_GROUP: &str = r#"INSERT OR IGNORE INTO groups (id, name, description)
    VALUES (1, 'default', '默认分组')"#;

const CREATE_UPSTREAM_GROUPS: &str = r#"
CREATE TABLE IF NOT EXISTS upstream_groups (
    upstream_id INTEGER NOT NULL REFERENCES upstreams(id) ON DELETE CASCADE,
    group_id    INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    PRIMARY KEY (upstream_id, group_id)
);"#;

const CREATE_UPSTREAM_GROUPS_INDEX: &str = r#"CREATE INDEX IF NOT EXISTS idx_upstream_groups_group
    ON upstream_groups(group_id, upstream_id);"#;

const BACKFILL_UPSTREAM_GROUPS: &str = r#"INSERT INTO upstream_groups (upstream_id, group_id)
    SELECT id, 1 FROM upstreams
    WHERE id NOT IN (SELECT upstream_id FROM upstream_groups)"#;

const BACKFILL_TOKEN_GROUPS: &str = "UPDATE api_tokens SET group_id = 1 WHERE group_id IS NULL";

const CREATE_RUNTIME_SETTINGS: &str = r#"
CREATE TABLE IF NOT EXISTS runtime_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    log_body_keep_count INTEGER NOT NULL CHECK (log_body_keep_count BETWEEN 1 AND 10000),
    log_retention_days INTEGER NOT NULL CHECK (log_retention_days BETWEEN 1 AND 3650),
    log_body_max_bytes INTEGER NOT NULL CHECK (log_body_max_bytes BETWEEN 0 AND 1048576),
    max_retries INTEGER NOT NULL DEFAULT 1 CHECK (max_retries BETWEEN 0 AND 5),
    same_upstream_retry_interval_ms INTEGER NOT NULL DEFAULT 1000 CHECK (same_upstream_retry_interval_ms BETWEEN 0 AND 60000),
    auto_weight_failure_penalty INTEGER NOT NULL DEFAULT 20 CHECK (auto_weight_failure_penalty BETWEEN 0 AND 100),
    auto_weight_success_increment INTEGER NOT NULL DEFAULT 5 CHECK (auto_weight_success_increment BETWEEN 0 AND 100),
    auto_weight_recovery_increment INTEGER NOT NULL DEFAULT 10 CHECK (auto_weight_recovery_increment BETWEEN 0 AND 100),
    auto_weight_recovery_interval_seconds INTEGER NOT NULL DEFAULT 60 CHECK (auto_weight_recovery_interval_seconds BETWEEN 1 AND 3600),
    proxy_enabled INTEGER NOT NULL DEFAULT 0 CHECK (proxy_enabled IN (0, 1)),
    proxy_url TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);"#;

const SEED_RUNTIME_SETTINGS: &str = r#"INSERT INTO runtime_settings (id, log_body_keep_count, log_retention_days, log_body_max_bytes, revision) VALUES (1, 100, 30, 200000, 1) ON CONFLICT(id) DO NOTHING"#;

const SCHEMA_STATEMENTS: &[&str] = &[
    CREATE_UPSTREAMS,
    CREATE_MODEL_TEST_PROMPT_TEMPLATES,
    CREATE_ADMIN_CREDENTIAL,
    "CREATE INDEX IF NOT EXISTS idx_upstreams_enabled_priority ON upstreams(enabled, priority, id);",
    CREATE_REQUEST_LOGS,
    CREATE_REQUEST_LOG_PAYLOADS,
    "CREATE INDEX IF NOT EXISTS idx_request_logs_created_at ON request_logs(created_at);",
    "CREATE INDEX IF NOT EXISTS idx_request_logs_upstream_created_at ON request_logs(upstream_id, created_at);",
    "DROP INDEX IF EXISTS idx_request_logs_created_at_id_desc;",
    "DROP INDEX IF EXISTS idx_request_logs_upstream_created_at_id_desc;",
    "CREATE INDEX IF NOT EXISTS idx_request_logs_downstream_token_created_at ON request_logs(downstream_token_id, created_at);",
    "CREATE INDEX IF NOT EXISTS idx_request_log_payloads_bodies_cleared ON request_log_payloads(bodies_cleared, request_log_id);",
    CREATE_API_TOKENS,
    CREATE_GROUPS,
    SEED_DEFAULT_GROUP,
    CREATE_UPSTREAM_GROUPS,
    CREATE_UPSTREAM_GROUPS_INDEX,
];

const UPSTREAM_COLUMNS: &[(&str, &str)] = &[
    (
        "weight",
        "INTEGER NOT NULL DEFAULT 100 CHECK (weight BETWEEN 0 AND 10000)",
    ),
    (
        "auto_weight_enabled",
        "INTEGER NOT NULL DEFAULT 1 CHECK (auto_weight_enabled IN (0, 1))",
    ),
    ("rate_limit", "TEXT"),
    ("effort_mappings", "TEXT NOT NULL DEFAULT '{}'"),
    (
        "archived",
        "INTEGER NOT NULL DEFAULT 0 CHECK (archived IN (0, 1))",
    ),
    ("archived_prev_enabled", "INTEGER"),
];

const REQUEST_LOG_COLUMNS: &[(&str, &str)] = &[
    ("request_model", "TEXT"),
    ("upstream_model", "TEXT"),
    ("upstream_reasoning_effort", "TEXT"),
    ("prompt_cached_tokens", "INTEGER"),
    ("cache_creation_tokens", "INTEGER"),
    ("completion_reasoning_tokens", "INTEGER"),
    ("client_ip", "TEXT"),
];

const API_TOKEN_COLUMNS: &[(&str, &str)] = &[
    ("used_tokens", "INTEGER NOT NULL DEFAULT 0"),
    ("limit_tokens", "INTEGER"),
    ("rate_limit", "TEXT"),
    // JSON array of permitted model names; empty means any.
    ("allowed_models", "TEXT NOT NULL DEFAULT '[]'"),
];

const RUNTIME_COLUMNS: &[(&str, &str)] = &[
    (
        "max_retries",
        "INTEGER NOT NULL DEFAULT 1 CHECK (max_retries BETWEEN 0 AND 5)",
    ),
    (
        "same_upstream_retry_interval_ms",
        "INTEGER NOT NULL DEFAULT 1000 CHECK (same_upstream_retry_interval_ms BETWEEN 0 AND 60000)",
    ),
    (
        "auto_weight_failure_penalty",
        "INTEGER NOT NULL DEFAULT 20 CHECK (auto_weight_failure_penalty BETWEEN 0 AND 100)",
    ),
    (
        "auto_weight_success_increment",
        "INTEGER NOT NULL DEFAULT 5 CHECK (auto_weight_success_increment BETWEEN 0 AND 100)",
    ),
    (
        "auto_weight_recovery_increment",
        "INTEGER NOT NULL DEFAULT 10 CHECK (auto_weight_recovery_increment BETWEEN 0 AND 100)",
    ),
    (
        "auto_weight_recovery_interval_seconds",
        "INTEGER NOT NULL DEFAULT 60 CHECK (auto_weight_recovery_interval_seconds BETWEEN 1 AND 3600)",
    ),
    (
        "proxy_enabled",
        "INTEGER NOT NULL DEFAULT 0 CHECK (proxy_enabled IN (0, 1))",
    ),
    ("proxy_url", "TEXT NOT NULL DEFAULT ''"),
    (
        "default_upstream_timeout_seconds",
        "INTEGER NOT NULL DEFAULT 0 CHECK (default_upstream_timeout_seconds BETWEEN 0 AND 3600)",
    ),
    (
        "image_storage_max_mb",
        "INTEGER NOT NULL DEFAULT 10240 CHECK (image_storage_max_mb BETWEEN 0 AND 1048576)",
    ),
    (
        "dashboard_multiplier",
        "REAL NOT NULL DEFAULT 1 CHECK (dashboard_multiplier > 0 AND dashboard_multiplier <= 1000)",
    ),
];

/// Creates the current schema, seeds defaults and enables the SQLite settings
/// WildToken depends on.
pub fn init(connection: &mut Connection) -> Result<(), ApiError> {
    let _mode: String = connection
        .query_row("PRAGMA journal_mode=WAL", [], |row| row.get(0))
        .map_err(ApiError::database)?;
    connection
        .execute_batch("PRAGMA foreign_keys=ON;")
        .map_err(ApiError::database)?;

    for statement in SCHEMA_STATEMENTS {
        connection
            .execute_batch(statement)
            .map_err(ApiError::database)?;
    }

    seed_prompt_templates_once(connection)?;

    for (column, definition) in UPSTREAM_COLUMNS {
        ensure_column(connection, "upstreams", column, definition)?;
    }
    for (column, definition) in REQUEST_LOG_COLUMNS {
        ensure_column(connection, "request_logs", column, definition)?;
    }
    ensure_column(connection, "api_tokens", "expires_at", "TEXT")?;
    ensure_column(connection, "api_tokens", "group_id", "INTEGER")?;
    for (column, definition) in API_TOKEN_COLUMNS {
        ensure_column(connection, "api_tokens", column, definition)?;
    }

    migrate_legacy_token_storage(connection)?;

    for statement in [BACKFILL_UPSTREAM_GROUPS, BACKFILL_TOKEN_GROUPS] {
        connection
            .execute_batch(statement)
            .map_err(ApiError::database)?;
    }

    connection
        .execute_batch(CREATE_RUNTIME_SETTINGS)
        .map_err(ApiError::database)?;
    for (column, definition) in RUNTIME_COLUMNS {
        ensure_column(connection, "runtime_settings", column, definition)?;
    }
    connection
        .execute_batch(SEED_RUNTIME_SETTINGS)
        .map_err(ApiError::database)?;

    check_auto_vacuum(connection);
    Ok(())
}

fn ensure_column(
    connection: &Connection,
    table: &str,
    column: &str,
    definition: &str,
) -> Result<(), ApiError> {
    let exists: i64 = connection
        .query_row(
            "SELECT COUNT(*) FROM pragma_table_info(?1) WHERE name = ?2",
            params![table, column],
            |row| row.get(0),
        )
        .map_err(ApiError::database)?;
    if exists == 0 {
        connection
            .execute_batch(&format!(
                "ALTER TABLE {table} ADD COLUMN {column} {definition}"
            ))
            .map_err(ApiError::database)?;
    }
    Ok(())
}

fn table_columns(connection: &Connection, table: &str) -> Result<HashSet<String>, ApiError> {
    let mut statement = connection
        .prepare("SELECT name FROM pragma_table_info(?1) ORDER BY cid")
        .map_err(ApiError::database)?;
    let rows = statement
        .query_map([table], |row| row.get::<_, String>(0))
        .map_err(ApiError::database)?;
    let mut columns = HashSet::new();
    for row in rows {
        columns.insert(row.map_err(ApiError::database)?);
    }
    Ok(columns)
}

fn seed_prompt_templates_once(connection: &Connection) -> Result<(), ApiError> {
    let used: i64 = connection
        .query_row(
            "SELECT (SELECT COUNT(*) FROM model_test_prompt_templates) +
                    (SELECT COUNT(*) FROM sqlite_sequence WHERE name = 'model_test_prompt_templates')",
            [],
            |row| row.get(0),
        )
        .map_err(ApiError::database)?;
    if used == 0 {
        connection
            .execute_batch(SEED_MODEL_TEST_PROMPT_TEMPLATES)
            .map_err(ApiError::database)?;
    }
    Ok(())
}

fn count_missing_token_hashes(connection: &Connection) -> Result<i64, ApiError> {
    connection
        .query_row(
            "SELECT COUNT(*) FROM api_tokens WHERE token_hash IS NULL",
            [],
            |row| row.get(0),
        )
        .map_err(ApiError::database)
}

fn hash_legacy_plaintext_tokens(transaction: &rusqlite::Transaction<'_>) -> Result<(), ApiError> {
    let legacy: Vec<(i64, String)> = {
        let mut statement = transaction
            .prepare("SELECT id, token FROM api_tokens WHERE token_hash IS NULL ORDER BY id")
            .map_err(ApiError::database)?;
        let rows = statement
            .query_map([], |row| Ok((row.get(0)?, row.get(1)?)))
            .map_err(ApiError::database)?;
        let mut collected = Vec::new();
        for row in rows {
            collected.push(row.map_err(ApiError::database)?);
        }
        collected
    };

    let prefix = unique_marker_prefix();
    for (id, plaintext) in legacy {
        transaction
            .execute(
                "UPDATE api_tokens SET token = ?, token_hash = ?, token_preview = ? WHERE id = ?",
                params![
                    format!("{prefix}{id}"),
                    token_digest(&plaintext),
                    token_preview(&plaintext),
                    id
                ],
            )
            .map_err(ApiError::database)?;
    }
    Ok(())
}

/// Brings a token table up to the current column set in one transaction.
fn migrate_legacy_token_storage(connection: &mut Connection) -> Result<(), ApiError> {
    let transaction = connection
        .transaction_with_behavior(TransactionBehavior::Immediate)
        .map_err(ApiError::database)?;

    let columns = table_columns(&transaction, "api_tokens")?;
    if !columns.contains("token") {
        return Err(ApiError::internal(
            "api_tokens is missing its compatibility token column",
        ));
    }
    if !columns.contains("token_hash") {
        transaction
            .execute_batch("ALTER TABLE api_tokens ADD COLUMN token_hash TEXT")
            .map_err(ApiError::database)?;
    }
    if !columns.contains("token_preview") {
        transaction
            .execute_batch(
                "ALTER TABLE api_tokens ADD COLUMN token_preview TEXT NOT NULL DEFAULT ''",
            )
            .map_err(ApiError::database)?;
    }
    if !columns.contains("token_plain") {
        transaction
            .execute_batch("ALTER TABLE api_tokens ADD COLUMN token_plain TEXT")
            .map_err(ApiError::database)?;
    }

    let mut missing = count_missing_token_hashes(&transaction)?;
    if missing != 0 {
        hash_legacy_plaintext_tokens(&transaction)?;
        missing = count_missing_token_hashes(&transaction)?;
    }
    if missing != 0 {
        return Err(ApiError::internal(
            "api_tokens contains rows without a token digest",
        ));
    }

    transaction
        .execute_batch("UPDATE api_tokens SET token = token_hash WHERE token <> token_hash;")
        .map_err(ApiError::database)?;
    transaction
        .execute_batch(
            "CREATE UNIQUE INDEX IF NOT EXISTS idx_api_tokens_token_hash ON api_tokens(token_hash);",
        )
        .map_err(ApiError::database)?;

    transaction.commit().map_err(ApiError::database)
}

fn check_auto_vacuum(connection: &Connection) {
    let mode: i64 = match connection.query_row("PRAGMA auto_vacuum", [], |row| row.get(0)) {
        Ok(mode) => mode,
        Err(_) => return,
    };
    if mode != 2 {
        tracing::warn!(
            sqlite_auto_vacuum = mode,
            "SQLite incremental auto-vacuum is not active; run a maintenance VACUUM once"
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn open_fresh() -> (tempfile::TempDir, Connection) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("wildtoken.db");
        let mut connection = Connection::open(&path).unwrap();
        connection.execute_batch("PRAGMA foreign_keys=ON;").unwrap();
        init(&mut connection).unwrap();
        (dir, connection)
    }

    fn table_names(connection: &Connection) -> Vec<String> {
        let mut statement = connection
            .prepare("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
            .unwrap();
        statement
            .query_map([], |row| row.get(0))
            .unwrap()
            .map(|row| row.unwrap())
            .collect()
    }

    #[test]
    fn creates_every_table() {
        let (_dir, connection) = open_fresh();
        let tables = table_names(&connection);
        for expected in [
            "admin_credential",
            "api_tokens",
            "groups",
            "model_test_prompt_templates",
            "request_log_payloads",
            "request_logs",
            "runtime_settings",
            "upstream_groups",
            "upstreams",
        ] {
            assert!(
                tables.iter().any(|name| name.as_str() == expected),
                "{expected}"
            );
        }
    }

    #[test]
    fn seeds_defaults() {
        let (_dir, connection) = open_fresh();

        let (id, name, description): (i64, String, String) = connection
            .query_row(
                "SELECT id, name, description FROM groups WHERE id = 1",
                [],
                |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
            )
            .unwrap();
        assert_eq!(
            (id, name.as_str(), description.as_str()),
            (1, "default", "默认分组")
        );

        let templates: i64 = connection
            .query_row(
                "SELECT COUNT(*) FROM model_test_prompt_templates",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(templates, 10);

        let (keep, days, bytes, revision): (i64, i64, i64, i64) = connection
            .query_row(
                "SELECT log_body_keep_count, log_retention_days, log_body_max_bytes, revision
                 FROM runtime_settings WHERE id = 1",
                [],
                |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
            )
            .unwrap();
        assert_eq!((keep, days, bytes, revision), (100, 30, 200_000, 1));
    }

    #[test]
    fn init_is_idempotent() {
        let (_dir, mut connection) = open_fresh();
        init(&mut connection).unwrap();
        init(&mut connection).unwrap();
        let templates: i64 = connection
            .query_row(
                "SELECT COUNT(*) FROM model_test_prompt_templates",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(templates, 10);
    }

    #[test]
    fn api_tokens_has_every_current_column() {
        let (_dir, connection) = open_fresh();
        let columns = table_columns(&connection, "api_tokens").unwrap();
        for expected in [
            "token",
            "token_hash",
            "token_preview",
            "token_plain",
            "expires_at",
            "group_id",
            "used_tokens",
            "limit_tokens",
            "rate_limit",
            "allowed_models",
        ] {
            assert!(columns.contains(expected), "{expected}");
        }
    }

    #[test]
    fn legacy_plaintext_tokens_are_hashed() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("legacy.db");
        let mut connection = Connection::open(&path).unwrap();
        connection
            .execute_batch(
                r#"
                CREATE TABLE api_tokens (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    name TEXT NOT NULL UNIQUE,
                    description TEXT NOT NULL DEFAULT '',
                    token TEXT NOT NULL UNIQUE,
                    enabled INTEGER NOT NULL DEFAULT 1,
                    created_at TEXT NOT NULL DEFAULT (datetime('now')),
                    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
                );
                INSERT INTO api_tokens (name, token) VALUES ('legacy', 'plain-secret');
                "#,
            )
            .unwrap();

        init(&mut connection).unwrap();

        let (token, token_hash, token_preview_value, token_plain): (
            String,
            String,
            String,
            String,
        ) = connection
            .query_row(
                "SELECT token, token_hash, token_preview, token_plain FROM api_tokens WHERE id = 1",
                [],
                |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
            )
            .unwrap();

        assert_eq!(token, token_hash);
        assert_eq!(token_hash, token_digest("plain-secret"));
        assert!(!token_preview_value.is_empty());
        assert_eq!(token_plain, "plain-secret");

        let index: i64 = connection
            .query_row(
                "SELECT COUNT(*) FROM sqlite_master
                 WHERE type = 'index' AND name = 'idx_api_tokens_token_hash'",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(index, 1);
    }
}
