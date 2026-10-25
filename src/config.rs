//! Startup configuration: TOML files, `APP__*` variables and legacy `.env` names.

use std::fs;
use std::path::Path;

use serde::Deserialize;

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(default)]
pub struct Settings {
    pub server: ServerSettings,
    pub database: DatabaseSettings,
    pub logging: LoggingSettings,
    pub upstream: UpstreamSettings,
    pub admin: AdminSettings,
    pub themes: ThemeSettings,
    pub images: ImageSettings,
}

#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(default)]
pub struct ServerSettings {
    pub host: String,
    pub port: u16,
}

impl Default for ServerSettings {
    fn default() -> Self {
        Self {
            host: "127.0.0.1".to_string(),
            port: 3100,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(default)]
pub struct DatabaseSettings {
    pub url: String,
    pub max_connections: i64,
    pub sqlite_cache_size_kib: i64,
    pub sqlite_statement_cache_capacity: i64,
    pub sqlite_mmap_size_bytes: i64,
    pub idle_timeout_seconds: i64,
}

impl Default for DatabaseSettings {
    fn default() -> Self {
        Self {
            url: "sqlite:wildtoken.db?mode=rwc".to_string(),
            max_connections: 3,
            sqlite_cache_size_kib: 2048,
            sqlite_statement_cache_capacity: 32,
            sqlite_mmap_size_bytes: 0,
            idle_timeout_seconds: 60,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(default)]
pub struct LoggingSettings {
    pub log_queue_capacity: i64,
}

impl Default for LoggingSettings {
    fn default() -> Self {
        Self {
            log_queue_capacity: 512,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(default)]
pub struct UpstreamSettings {
    pub default_timeout_seconds: f64,
}

impl Default for UpstreamSettings {
    fn default() -> Self {
        Self {
            default_timeout_seconds: 300.0,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(default)]
pub struct AdminSettings {
    pub token: String,
    /// Header carrying the real client address behind a reverse proxy.
    pub client_ip_header: String,
}

impl Default for AdminSettings {
    fn default() -> Self {
        Self {
            token: "change-me".to_string(),
            client_ip_header: String::new(),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(default)]
pub struct ThemeSettings {
    pub dir: String,
}

impl Default for ThemeSettings {
    fn default() -> Self {
        Self {
            dir: "themes".to_string(),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(default)]
pub struct ImageSettings {
    pub dir: String,
}

impl Default for ImageSettings {
    fn default() -> Self {
        Self {
            dir: "images".to_string(),
        }
    }
}

/// Reads `default.toml`, `<RUN_ENV>.toml`, `APP__*` variables and legacy names.
pub fn load() -> Result<Settings, String> {
    load_from(Path::new("."))
}

pub fn load_from(root: &Path) -> Result<Settings, String> {
    load_dotenv(&root.join(".env"));

    let run_env = std::env::var("RUN_ENV")
        .ok()
        .filter(|value| !value.is_empty())
        .unwrap_or_else(|| "development".to_string());

    let mut merged = toml::Value::Table(toml::Table::new());
    for name in ["default", run_env.as_str()] {
        let path = root.join("config").join(format!("{name}.toml"));
        let text = match fs::read_to_string(&path) {
            Ok(text) => text,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) => return Err(format!("read {}: {error}", path.display())),
        };
        let value: toml::Value =
            toml::from_str(&text).map_err(|error| format!("parse {}: {error}", path.display()))?;
        merge_value(&mut merged, value);
    }

    let mut settings =
        Settings::deserialize(merged).map_err(|error| format!("invalid configuration: {error}"))?;

    apply_env_overrides(&mut settings)?;

    if let Some(token) = non_empty_env("ADMIN_TOKEN") {
        settings.admin.token = token;
    }
    if let Some(url) = non_empty_env("DATABASE_URL") {
        settings.database.url = url;
    }
    if let Some(dir) = non_empty_env("WILDTOKEN_THEME_DIR") {
        settings.themes.dir = dir;
    }
    if let Some(dir) = non_empty_env("WILDTOKEN_IMAGE_DIR") {
        settings.images.dir = dir;
    }

    Ok(settings)
}

fn non_empty_env(name: &str) -> Option<String> {
    std::env::var(name).ok().filter(|value| !value.is_empty())
}

const ENV_OVERRIDES: &[&str] = &[
    "APP__SERVER__HOST",
    "APP__SERVER__PORT",
    "APP__DATABASE__URL",
    "APP__DATABASE__MAX_CONNECTIONS",
    "APP__DATABASE__SQLITE_CACHE_SIZE_KIB",
    "APP__DATABASE__SQLITE_STATEMENT_CACHE_CAPACITY",
    "APP__DATABASE__SQLITE_MMAP_SIZE_BYTES",
    "APP__DATABASE__IDLE_TIMEOUT_SECONDS",
    "APP__LOGGING__LOG_QUEUE_CAPACITY",
    "APP__UPSTREAM__DEFAULT_TIMEOUT_SECONDS",
    "APP__ADMIN__TOKEN",
    "APP__ADMIN__CLIENT_IP_HEADER",
    "APP__THEMES__DIR",
    "APP__IMAGES__DIR",
];

fn apply_env_overrides(settings: &mut Settings) -> Result<(), String> {
    for key in ENV_OVERRIDES.iter().copied() {
        let Ok(value) = std::env::var(key) else {
            continue;
        };
        apply_env_override(settings, key, &value)?;
    }
    Ok(())
}

fn apply_env_override(settings: &mut Settings, key: &str, value: &str) -> Result<(), String> {
    let invalid = |error: Box<dyn std::error::Error>| format!("invalid {key}: {error}");
    match key {
        "APP__SERVER__HOST" => settings.server.host = value.to_string(),
        "APP__SERVER__PORT" => {
            settings.server.port = value.parse().map_err(|error| invalid(Box::new(error)))?
        }
        "APP__DATABASE__URL" => settings.database.url = value.to_string(),
        "APP__DATABASE__MAX_CONNECTIONS" => {
            settings.database.max_connections =
                value.parse().map_err(|error| invalid(Box::new(error)))?
        }
        "APP__DATABASE__SQLITE_CACHE_SIZE_KIB" => {
            settings.database.sqlite_cache_size_kib =
                value.parse().map_err(|error| invalid(Box::new(error)))?
        }
        "APP__DATABASE__SQLITE_STATEMENT_CACHE_CAPACITY" => {
            settings.database.sqlite_statement_cache_capacity =
                value.parse().map_err(|error| invalid(Box::new(error)))?
        }
        "APP__DATABASE__SQLITE_MMAP_SIZE_BYTES" => {
            settings.database.sqlite_mmap_size_bytes =
                value.parse().map_err(|error| invalid(Box::new(error)))?
        }
        "APP__DATABASE__IDLE_TIMEOUT_SECONDS" => {
            settings.database.idle_timeout_seconds =
                value.parse().map_err(|error| invalid(Box::new(error)))?
        }
        "APP__LOGGING__LOG_QUEUE_CAPACITY" => {
            settings.logging.log_queue_capacity =
                value.parse().map_err(|error| invalid(Box::new(error)))?
        }
        "APP__UPSTREAM__DEFAULT_TIMEOUT_SECONDS" => {
            settings.upstream.default_timeout_seconds =
                value.parse().map_err(|error| invalid(Box::new(error)))?
        }
        "APP__ADMIN__TOKEN" => settings.admin.token = value.to_string(),
        "APP__ADMIN__CLIENT_IP_HEADER" => settings.admin.client_ip_header = value.to_string(),
        "APP__THEMES__DIR" => settings.themes.dir = value.to_string(),
        "APP__IMAGES__DIR" => settings.images.dir = value.to_string(),
        _ => {}
    }
    Ok(())
}

fn merge_value(base: &mut toml::Value, overlay: toml::Value) {
    match (base, overlay) {
        (toml::Value::Table(base_table), toml::Value::Table(overlay_table)) => {
            for (key, value) in overlay_table {
                match base_table.get_mut(&key) {
                    Some(existing) if existing.is_table() && value.is_table() => {
                        merge_value(existing, value)
                    }
                    _ => {
                        base_table.insert(key, value);
                    }
                }
            }
        }
        (base, overlay) => *base = overlay,
    }
}

/// Fills variables from a `.env` file without overwriting the real environment.
pub fn load_dotenv(path: &Path) {
    let Ok(text) = fs::read_to_string(path) else {
        return;
    };

    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let line = line.strip_prefix("export ").unwrap_or(line);
        let Some((key, value)) = line.split_once('=') else {
            continue;
        };
        let key = key.trim();
        let mut value = value.trim();
        if value.len() >= 2 {
            let first = value.as_bytes()[0];
            let last = value.as_bytes()[value.len() - 1];
            if (first == b'"' && last == b'"') || (first == b'\'' && last == b'\'') {
                value = &value[1..value.len() - 1];
            }
        }
        if std::env::var_os(key).is_none() {
            std::env::set_var(key, value);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    static ENV_LOCK: Mutex<()> = Mutex::new(());

    fn clear_env() {
        for key in ENV_OVERRIDES.iter().copied() {
            std::env::remove_var(key);
        }
        for key in [
            "RUN_ENV",
            "ADMIN_TOKEN",
            "DATABASE_URL",
            "WILDTOKEN_THEME_DIR",
            "WILDTOKEN_IMAGE_DIR",
        ] {
            std::env::remove_var(key);
        }
    }

    #[test]
    fn defaults_match_the_shipped_configuration() {
        let settings = Settings::default();
        assert_eq!(settings.server.host, "127.0.0.1");
        assert_eq!(settings.server.port, 3100);
        assert_eq!(settings.database.url, "sqlite:wildtoken.db?mode=rwc");
        assert_eq!(settings.database.max_connections, 3);
        assert_eq!(settings.database.sqlite_cache_size_kib, 2048);
        assert_eq!(settings.database.sqlite_statement_cache_capacity, 32);
        assert_eq!(settings.database.idle_timeout_seconds, 60);
        assert_eq!(settings.logging.log_queue_capacity, 512);
        assert_eq!(settings.upstream.default_timeout_seconds, 300.0);
        assert_eq!(settings.admin.token, "change-me");
        assert_eq!(settings.themes.dir, "themes");
        assert_eq!(settings.images.dir, "images");
    }

    #[test]
    fn toml_files_merge_and_env_overrides_win() {
        let _guard = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        clear_env();

        let dir = tempfile::tempdir().unwrap();
        fs::create_dir_all(dir.path().join("config")).unwrap();
        fs::write(
            dir.path().join("config/default.toml"),
            "[server]\nport = 4100\nhost = \"0.0.0.0\"\n[logging]\nlog_queue_capacity = 32\n",
        )
        .unwrap();
        std::env::set_var("RUN_ENV", "test");
        fs::write(
            dir.path().join("config/test.toml"),
            "[server]\nport = 4200\n",
        )
        .unwrap();
        std::env::set_var("APP__SERVER__HOST", "127.0.0.2");

        let settings = load_from(dir.path()).unwrap();
        assert_eq!(settings.server.port, 4200);
        assert_eq!(settings.server.host, "127.0.0.2");
        assert_eq!(settings.logging.log_queue_capacity, 32);
        assert_eq!(settings.database.max_connections, 3);

        clear_env();
    }

    #[test]
    fn dotenv_does_not_override_the_real_environment() {
        let _guard = ENV_LOCK.lock().unwrap_or_else(|error| error.into_inner());
        std::env::remove_var("WILDTOKEN_TEST_DOTENV");
        std::env::set_var("WILDTOKEN_TEST_DOTENV", "real");

        let dir = tempfile::tempdir().unwrap();
        fs::write(
            dir.path().join(".env"),
            "export WILDTOKEN_TEST_DOTENV='from-file'\nANOTHER_DOTENV=value\n",
        )
        .unwrap();
        load_dotenv(&dir.path().join(".env"));

        assert_eq!(std::env::var("WILDTOKEN_TEST_DOTENV").unwrap(), "real");
        assert_eq!(std::env::var("ANOTHER_DOTENV").unwrap(), "value");

        std::env::remove_var("WILDTOKEN_TEST_DOTENV");
        std::env::remove_var("ANOTHER_DOTENV");
    }

    #[test]
    fn invalid_numbers_are_rejected_with_the_variable_name() {
        let mut settings = Settings::default();
        let error = apply_env_override(&mut settings, "APP__SERVER__PORT", "nope").unwrap_err();
        assert!(error.contains("APP__SERVER__PORT"));
    }
}
