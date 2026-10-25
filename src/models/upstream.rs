//! Upstream channel shapes shared by the admin API, proxy and routing.

use serde::{Deserialize, Serialize};

use crate::models::token::normalize_rate_limit;

pub const UPSTREAM_NAME_MAX_CHARS: usize = 80;
pub const UPSTREAM_MAX_TIMEOUT_SECONDS: f64 = 3600.0;
pub const EFFORT_MAPPING_MAX_ENTRIES: usize = 32;
pub const EFFORT_VALUE_MAX_CHARS: usize = 32;

/// Mirrors a row of the `upstreams` table.
#[derive(Debug, Clone, PartialEq)]
pub struct UpstreamRow {
    pub id: i64,
    pub name: String,
    pub base_url: String,
    pub api_key: Option<String>,
    pub model_names: String,
    pub model_prefixes: String,
    pub model_mappings: String,
    pub effort_mappings: String,
    pub priority: i64,
    pub weight: i64,
    pub auto_weight_enabled: i64,
    pub enabled: i64,
    pub archived: i64,
    pub archived_prev_enabled: Option<i64>,
    pub extra_headers: String,
    pub timeout_seconds: f64,
    pub rate_limit: Option<String>,
    pub created_at: String,
    pub updated_at: String,
}

/// The create payload; absent collections and priorities get defaults.
#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(default)]
pub struct UpstreamIn {
    pub name: String,
    pub base_url: String,
    pub api_key: Option<String>,
    pub model_names: Vec<String>,
    pub model_prefixes: Vec<String>,
    pub model_mappings: std::collections::BTreeMap<String, String>,
    pub effort_mappings: std::collections::BTreeMap<String, String>,
    pub priority: i64,
    pub weight: i64,
    pub auto_weight_enabled: bool,
    pub enabled: bool,
    pub extra_headers: std::collections::BTreeMap<String, String>,
    pub timeout_seconds: Option<f64>,
    pub rate_limit: Option<String>,
    pub group_ids: Vec<i64>,
    pub archived: Option<bool>,
}

impl Default for UpstreamIn {
    fn default() -> Self {
        Self {
            name: String::new(),
            base_url: String::new(),
            api_key: None,
            model_names: Vec::new(),
            model_prefixes: Vec::new(),
            model_mappings: std::collections::BTreeMap::new(),
            effort_mappings: std::collections::BTreeMap::new(),
            priority: 100,
            weight: 100,
            auto_weight_enabled: true,
            enabled: true,
            extra_headers: std::collections::BTreeMap::new(),
            timeout_seconds: None,
            rate_limit: None,
            group_ids: Vec::new(),
            archived: None,
        }
    }
}

/// The replace payload carries every stored field plus `clear_api_key`.
#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(default)]
pub struct UpstreamUpdate {
    #[serde(flatten)]
    pub input: UpstreamIn,
    pub clear_api_key: bool,
}

/// Required fields a replacing update must carry as values.
pub const UPSTREAM_UPDATE_REQUIRED: &[&str] = &[
    "name",
    "base_url",
    "model_names",
    "model_prefixes",
    "model_mappings",
    "effort_mappings",
    "priority",
    "weight",
    "auto_weight_enabled",
    "enabled",
    "extra_headers",
    "rate_limit",
    "group_ids",
];

pub fn missing_upstream_fields(
    body: &serde_json::Map<String, serde_json::Value>,
) -> Vec<&'static str> {
    let mut missing = Vec::new();
    for name in UPSTREAM_UPDATE_REQUIRED {
        let nullable = *name == "rate_limit";
        match body.get(*name) {
            None => missing.push(*name),
            Some(serde_json::Value::Null) if !nullable => missing.push(*name),
            _ => {}
        }
    }
    missing
}

impl UpstreamIn {
    pub fn validate(&mut self) -> Result<(), String> {
        let name = self.name.trim();
        if name.is_empty() || name.chars().count() > UPSTREAM_NAME_MAX_CHARS {
            return Err("channel name must be between 1 and 80 characters".to_string());
        }
        if name.chars().any(|character| character.is_control()) {
            return Err("channel name must not contain control characters".to_string());
        }
        self.name = name.to_string();

        if self.weight < 0 || self.weight > 10000 {
            return Err("weight must be between 0 and 10000".to_string());
        }
        if let Some(seconds) = self.timeout_seconds {
            if seconds < 0.0 || seconds > UPSTREAM_MAX_TIMEOUT_SECONDS {
                return Err("timeout_seconds must be between 0 and 3600".to_string());
            }
        }

        self.base_url = validate_base_url(&self.base_url)?;
        normalize_rate_limit(self.rate_limit.as_deref())
            .map_err(|_| "rate limit must look like 100/m, 1000/h or 50/10s".to_string())?;
        self.effort_mappings = normalize_effort_mappings(&self.effort_mappings)?;
        Ok(())
    }

    pub fn normalized_rate_limit(&self) -> Result<Option<String>, String> {
        normalize_rate_limit(self.rate_limit.as_deref())
    }

    /// Names the groups this channel should serve, descending to the default.
    pub fn requested_group_ids(&self) -> Vec<i64> {
        if self.group_ids.is_empty() {
            vec![crate::models::group::DEFAULT_GROUP_ID]
        } else {
            self.group_ids.clone()
        }
    }
}

/// Trims and lower-cases effort keys while keeping the values as written.
fn normalize_effort_mappings(
    entries: &std::collections::BTreeMap<String, String>,
) -> Result<std::collections::BTreeMap<String, String>, String> {
    if entries.is_empty() {
        return Ok(std::collections::BTreeMap::new());
    }
    if entries.len() > EFFORT_MAPPING_MAX_ENTRIES {
        return Err("effort_mappings must have at most 32 entries".to_string());
    }

    let mut normalized = std::collections::BTreeMap::new();
    for (from, to) in entries {
        let key = from.trim().to_lowercase();
        let value = to.trim();
        if key.is_empty() || value.is_empty() {
            continue;
        }
        if key.chars().count() > EFFORT_VALUE_MAX_CHARS
            || value.chars().count() > EFFORT_VALUE_MAX_CHARS
        {
            return Err("effort_mappings values must be at most 32 characters".to_string());
        }
        if key.chars().any(|character| character.is_control())
            || value.chars().any(|character| character.is_control())
        {
            return Err("effort_mappings must not contain control characters".to_string());
        }
        if let Some(existing) = normalized.get(&key) {
            if existing != value {
                return Err(format!("effort_mappings has conflicting entries for {key}"));
            }
        }
        normalized.insert(key, value.to_string());
    }
    Ok(normalized)
}

/// Checks a channel's base URL is one the proxy can build a request from.
pub fn validate_base_url(raw: &str) -> Result<String, String> {
    let value = raw.trim();
    if value.is_empty() {
        return Err("base_url is required".to_string());
    }
    if value.len() > 2048 {
        return Err("base_url must be at most 2048 bytes".to_string());
    }

    let parsed =
        url::Url::parse(value).map_err(|error| format!("base_url is not a valid URL: {error}"))?;
    if parsed.scheme() != "http" && parsed.scheme() != "https" {
        return Err("base_url must start with http:// or https://".to_string());
    }
    if parsed.host_str().is_none() {
        return Err(
            "base_url must include a host, for example https://api.example.com/v1".to_string(),
        );
    }
    if parsed.query().is_some() {
        return Err("base_url must not carry a query string".to_string());
    }
    if parsed.fragment().is_some() {
        return Err("base_url must not carry a fragment".to_string());
    }
    Ok(value.to_string())
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct UpstreamEnabledIn {
    pub enabled: Option<bool>,
}

impl UpstreamEnabledIn {
    pub fn value(&self) -> Result<bool, String> {
        self.enabled
            .ok_or_else(|| "enabled is required".to_string())
    }
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct UpstreamArchivedIn {
    pub archived: Option<bool>,
}

impl UpstreamArchivedIn {
    pub fn value(&self) -> Result<bool, String> {
        self.archived
            .ok_or_else(|| "archived is required".to_string())
    }
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct UpstreamPriorityIn {
    pub priority: Option<i64>,
}

impl UpstreamPriorityIn {
    pub fn value(&self) -> Result<i64, String> {
        self.priority
            .ok_or_else(|| "priority is required".to_string())
    }
}

/// The list representation; the API key is never included.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct UpstreamOut {
    pub id: i64,
    pub name: String,
    pub base_url: String,
    pub api_key_set: bool,
    pub model_names: Vec<String>,
    pub model_prefixes: Vec<String>,
    pub model_mappings: std::collections::BTreeMap<String, String>,
    pub effort_mappings: std::collections::BTreeMap<String, String>,
    pub priority: i64,
    pub weight: i64,
    pub auto_weight_enabled: bool,
    pub enabled: bool,
    pub archived: bool,
    pub extra_headers: std::collections::BTreeMap<String, String>,
    pub timeout_seconds: f64,
    pub rate_limit: Option<String>,
    pub created_at: String,
    pub updated_at: String,
    pub runtime_health_score: i64,
    pub effective_weight: f64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub health_recovery_remaining_seconds: Option<i64>,
    pub group_ids: Vec<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub enabled_before_archive: Option<bool>,
}

/// The single-item representation, carrying the stored key for the edit form.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct UpstreamDetailOut {
    pub id: i64,
    pub name: String,
    pub base_url: String,
    pub api_key: Option<String>,
    pub api_key_set: bool,
    pub model_names: Vec<String>,
    pub model_prefixes: Vec<String>,
    pub model_mappings: std::collections::BTreeMap<String, String>,
    pub effort_mappings: std::collections::BTreeMap<String, String>,
    pub priority: i64,
    pub weight: i64,
    pub auto_weight_enabled: bool,
    pub enabled: bool,
    pub extra_headers: std::collections::BTreeMap<String, String>,
    pub timeout_seconds: f64,
    pub rate_limit: Option<String>,
    pub created_at: String,
    pub updated_at: String,
    pub runtime_health_score: i64,
    pub effective_weight: f64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub health_recovery_remaining_seconds: Option<i64>,
    pub group_ids: Vec<i64>,
    pub archived: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub enabled_before_archive: Option<bool>,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
pub struct ExportUpstreamsRequest {
    #[serde(default)]
    pub ids: Vec<i64>,
    #[serde(default)]
    pub include_api_keys: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct ChannelExportItem {
    pub name: String,
    pub base_url: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub api_key: Option<String>,
    pub model_names: Vec<String>,
    pub model_prefixes: Vec<String>,
    pub model_mappings: std::collections::BTreeMap<String, String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub effort_mappings: Option<std::collections::BTreeMap<String, String>>,
    pub priority: i64,
    pub weight: i64,
    pub auto_weight_enabled: bool,
    pub enabled: bool,
    pub extra_headers: std::collections::BTreeMap<String, String>,
    pub timeout_seconds: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rate_limit: Option<String>,
    pub group_ids: Vec<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub group_names: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub archived: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub enabled_before_archive: Option<bool>,
}

#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct ExportUpstreamsResponse {
    pub kind: String,
    pub version: i64,
    pub exported_at: String,
    pub channels: Vec<ChannelExportItem>,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(default)]
pub struct ImportUpstreamsRequest {
    pub kind: String,
    pub version: i64,
    pub channels: Vec<ChannelExportItem>,
    /// `"skip"` or `"overwrite"`.
    pub mode: String,
}

#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct ImportResultItem {
    pub name: String,
    pub action: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct ImportUpstreamsResponse {
    pub created: i64,
    pub updated: i64,
    pub skipped: i64,
    pub failed: i64,
    pub items: Vec<ImportResultItem>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base_urls_are_validated_and_trimmed() {
        assert_eq!(
            validate_base_url("  https://api.example.com/v1  ").unwrap(),
            "https://api.example.com/v1"
        );
        assert_eq!(
            validate_base_url("http://127.0.0.1:4000").unwrap(),
            "http://127.0.0.1:4000"
        );
        for bad in [
            "",
            "ftp://example.com",
            "not a url",
            "https://",
            "https://example.com?x=1",
            "https://example.com#frag",
        ] {
            assert!(validate_base_url(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn defaults_match_created_channels() {
        let input = UpstreamIn::default();
        assert_eq!(input.priority, 100);
        assert_eq!(input.weight, 100);
        assert!(input.auto_weight_enabled);
        assert!(input.enabled);
        assert_eq!(input.model_names, vec![] as Vec<String>);
    }

    #[test]
    fn effort_mappings_are_lower_cased_and_deduped_case_insensitively() {
        let map = std::collections::BTreeMap::from([
            ("MAX".to_string(), " xhigh ".to_string()),
            ("max".to_string(), "xhigh".to_string()),
            ("  ".to_string(), "x".to_string()),
        ]);
        let normalized = normalize_effort_mappings(&map).unwrap();
        assert_eq!(normalized.get("max").map(String::as_str), Some("xhigh"));
    }

    #[test]
    fn conflicting_effort_mappings_are_refused() {
        let map = std::collections::BTreeMap::from([
            ("max".to_string(), "xhigh".to_string()),
            ("MAX".to_string(), "high".to_string()),
        ]);
        assert!(normalize_effort_mappings(&map).is_err());
    }

    #[test]
    fn missing_update_fields_are_reported() {
        let body = serde_json::json!({"name": "a", "rate_limit": null, "group_ids": null});
        let missing = missing_upstream_fields(body.as_object().unwrap());
        assert!(!missing.contains(&"name"));
        assert!(!missing.contains(&"rate_limit"));
        assert!(missing.contains(&"group_ids"));
        assert!(missing.contains(&"base_url"));
    }
}
