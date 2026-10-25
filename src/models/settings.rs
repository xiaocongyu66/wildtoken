//! Operator-editable runtime policy stored in SQLite.

use serde::{Deserialize, Serialize};

pub const DEFAULT_LOG_BODY_KEEP_COUNT: i64 = 100;
pub const DEFAULT_LOG_RETENTION_DAYS: i64 = 30;
pub const DEFAULT_LOG_BODY_MAX_BYTES: i64 = 200_000;
pub const DEFAULT_MAX_RETRIES: i64 = 1;
pub const DEFAULT_SAME_UPSTREAM_RETRY_INTERVAL_MS: i64 = 1000;
pub const DEFAULT_AUTO_WEIGHT_FAILURE_PENALTY: i64 = 20;
pub const DEFAULT_AUTO_WEIGHT_SUCCESS_INCREMENT: i64 = 5;
pub const DEFAULT_AUTO_WEIGHT_RECOVERY_INCREMENT: i64 = 10;
pub const DEFAULT_AUTO_WEIGHT_RECOVERY_INTERVAL_SECONDS: i64 = 60;
pub const DEFAULT_IMAGE_STORAGE_MAX_MB: i64 = 10240;
pub const MAX_IMAGE_STORAGE_MAX_MB: i64 = 1 << 20;
pub const MAX_DASHBOARD_MULTIPLIER: f64 = 1000.0;

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct RuntimeSettings {
    pub log_body_keep_count: i64,
    pub log_retention_days: i64,
    pub log_body_max_bytes: i64,
    pub max_retries: i64,
    pub same_upstream_retry_interval_ms: i64,
    pub auto_weight_failure_penalty: i64,
    pub auto_weight_success_increment: i64,
    pub auto_weight_recovery_increment: i64,
    pub auto_weight_recovery_interval_seconds: i64,
    pub proxy_enabled: bool,
    pub proxy_url: String,
    pub default_upstream_timeout_seconds: i64,
    pub image_storage_max_mb: i64,
    pub dashboard_multiplier: f64,
    pub revision: i64,
    pub updated_at: String,
    /// Whether these values came from SQLite rather than startup defaults.
    #[serde(skip)]
    pub database_override: bool,
}

impl Default for RuntimeSettings {
    fn default() -> Self {
        Self {
            log_body_keep_count: DEFAULT_LOG_BODY_KEEP_COUNT,
            log_retention_days: DEFAULT_LOG_RETENTION_DAYS,
            log_body_max_bytes: DEFAULT_LOG_BODY_MAX_BYTES,
            max_retries: DEFAULT_MAX_RETRIES,
            same_upstream_retry_interval_ms: DEFAULT_SAME_UPSTREAM_RETRY_INTERVAL_MS,
            auto_weight_failure_penalty: DEFAULT_AUTO_WEIGHT_FAILURE_PENALTY,
            auto_weight_success_increment: DEFAULT_AUTO_WEIGHT_SUCCESS_INCREMENT,
            auto_weight_recovery_increment: DEFAULT_AUTO_WEIGHT_RECOVERY_INCREMENT,
            auto_weight_recovery_interval_seconds: DEFAULT_AUTO_WEIGHT_RECOVERY_INTERVAL_SECONDS,
            proxy_enabled: false,
            proxy_url: String::new(),
            default_upstream_timeout_seconds: 0,
            image_storage_max_mb: DEFAULT_IMAGE_STORAGE_MAX_MB,
            dashboard_multiplier: 1.0,
            revision: 0,
            updated_at: String::new(),
            database_override: false,
        }
    }
}

impl RuntimeSettings {
    pub fn validate(&self) -> Result<(), String> {
        let checks: [(i64, i64, i64, &str); 11] = [
            (
                self.log_body_keep_count,
                1,
                10_000,
                "log_body_keep_count must be between 1 and 10000",
            ),
            (
                self.log_retention_days,
                1,
                3_650,
                "log_retention_days must be between 1 and 3650",
            ),
            (
                self.log_body_max_bytes,
                0,
                1_048_576,
                "log_body_max_bytes must be between 0 and 1048576",
            ),
            (
                self.max_retries,
                0,
                5,
                "max_retries must be between 0 and 5",
            ),
            (
                self.same_upstream_retry_interval_ms,
                0,
                60_000,
                "same_upstream_retry_interval_ms must be between 0 and 60000",
            ),
            (
                self.auto_weight_failure_penalty,
                0,
                100,
                "auto_weight_failure_penalty must be between 0 and 100",
            ),
            (
                self.auto_weight_success_increment,
                0,
                100,
                "auto_weight_success_increment must be between 0 and 100",
            ),
            (
                self.auto_weight_recovery_increment,
                0,
                100,
                "auto_weight_recovery_increment must be between 0 and 100",
            ),
            (
                self.auto_weight_recovery_interval_seconds,
                1,
                3_600,
                "auto_weight_recovery_interval_seconds must be between 1 and 3600",
            ),
            (
                self.default_upstream_timeout_seconds,
                0,
                3_600,
                "default_upstream_timeout_seconds must be between 0 and 3600",
            ),
            (
                self.image_storage_max_mb,
                0,
                MAX_IMAGE_STORAGE_MAX_MB,
                "image_storage_max_mb must be between 0 and 1048576",
            ),
        ];

        for (value, minimum, maximum, message) in checks {
            if value < minimum || value > maximum {
                return Err(message.to_string());
            }
        }

        // NaN fails both comparisons, so test the accepted range.
        if !(self.dashboard_multiplier > 0.0
            && self.dashboard_multiplier <= MAX_DASHBOARD_MULTIPLIER)
        {
            return Err("dashboard_multiplier must be greater than 0 and at most 1000".to_string());
        }

        validate_proxy_url(&self.proxy_url, self.proxy_enabled)
    }
}

/// Accepts an empty value only when the proxy is disabled.
pub fn validate_proxy_url(value: &str, enabled: bool) -> Result<(), String> {
    let trimmed = value.trim();
    if trimmed.is_empty() {
        if enabled {
            return Err("proxy_url is required when proxy_enabled is true".to_string());
        }
        return Ok(());
    }
    if trimmed.len() > 500 {
        return Err("proxy_url must be at most 500 bytes".to_string());
    }
    let parsed =
        url::Url::parse(trimmed).map_err(|_| "proxy_url must be a valid URL".to_string())?;
    match parsed.scheme() {
        "http" | "https" | "socks5" | "socks5h" => {}
        _ => return Err("proxy_url scheme must be http, https, socks5, or socks5h".to_string()),
    }
    if parsed.host_str().is_none() {
        return Err("proxy_url must include a host".to_string());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_are_the_safe_policy() {
        let settings = RuntimeSettings::default();
        assert_eq!(settings.log_body_keep_count, 100);
        assert_eq!(settings.log_retention_days, 30);
        assert_eq!(settings.log_body_max_bytes, 200_000);
        assert_eq!(settings.max_retries, 1);
        assert_eq!(settings.same_upstream_retry_interval_ms, 1000);
        assert_eq!(settings.image_storage_max_mb, 10240);
        assert_eq!(settings.dashboard_multiplier, 1.0);
        settings.validate().unwrap();
    }

    #[test]
    fn bounds_are_enforced() {
        let settings = RuntimeSettings {
            max_retries: 6,
            ..RuntimeSettings::default()
        };
        assert!(settings.validate().is_err());
        let settings = RuntimeSettings {
            dashboard_multiplier: f64::NAN,
            ..RuntimeSettings::default()
        };
        assert!(settings.validate().is_err());
    }

    #[test]
    fn proxy_url_rules_match_the_original() {
        assert!(validate_proxy_url("", false).is_ok());
        assert!(validate_proxy_url("", true).is_err());
        assert!(validate_proxy_url("http://127.0.0.1:7890", true).is_ok());
        assert!(validate_proxy_url("socks5h://proxy:1080", true).is_ok());
        assert!(validate_proxy_url("ftp://proxy", true).is_err());
        assert!(validate_proxy_url("http://", true).is_err());
    }
}
