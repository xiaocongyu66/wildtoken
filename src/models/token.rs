//! Downstream token shapes, validation and the stored timestamp shape.

use serde::{Deserialize, Serialize};
use time::format_description::well_known::Rfc3339;
use time::macros::format_description;
use time::{OffsetDateTime, PrimitiveDateTime, UtcOffset};

use crate::models::quota::{parse_quota_expression, QuotaState};
use crate::ratelimit::parse_rate_limit;

pub const APITOKEN_NAME_MAX_CHARS: usize = 80;
pub const APITOKEN_DESCRIPTION_MAX_CHARS: usize = 200;
pub const APITOKEN_MIN_BYTES: usize = 1;
pub const APITOKEN_MAX_BYTES: usize = 256;
pub const APITOKEN_ALLOWED_MODELS_MAX: usize = 500;
pub const APITOKEN_ALLOWED_MODEL_MAX_CHARS: usize = 200;

/// The shape SQLite's `datetime('now')` produces. Fixed width and zero padded,
/// so lexical order is chronological order.
pub const TIMESTAMP_FORMAT: &str = "2006-01-02 15:04:05";

const EXPIRY_FORMAT_ERROR: &str =
    "token expiry must be an RFC 3339 timestamp or 'YYYY-MM-DD HH:MM:SS' in UTC";

/// Renders a timestamp in the stored UTC shape.
pub fn format_timestamp(value: OffsetDateTime) -> String {
    let value = value.to_offset(UtcOffset::UTC);
    format!(
        "{:04}-{:02}-{:02} {:02}:{:02}:{:02}",
        value.year(),
        value.month() as u8,
        value.day(),
        value.hour(),
        value.minute(),
        value.second()
    )
}

pub fn utc_now_timestamp() -> String {
    format_timestamp(OffsetDateTime::now_utc())
}

/// Converts a caller-supplied expiry into the stored UTC shape.
///
/// `None` and a blank value both mean "never expires".
pub fn normalize_expires_at(raw: Option<&str>) -> Result<Option<String>, String> {
    let Some(raw) = raw else {
        return Ok(None);
    };
    let value = raw.trim();
    if value.is_empty() {
        return Ok(None);
    }
    if let Ok(parsed) = OffsetDateTime::parse(value, &Rfc3339) {
        return Ok(Some(format_timestamp(parsed)));
    }
    let fixed = format_description!("[year]-[month]-[day] [hour]:[minute]:[second]");
    if let Ok(parsed) = PrimitiveDateTime::parse(value, fixed) {
        return Ok(Some(format_timestamp(parsed.assume_utc())));
    }
    Err(EXPIRY_FORMAT_ERROR.to_string())
}

/// Validates and trims a rate-limit expression. Blank means no limit.
pub fn normalize_rate_limit(raw: Option<&str>) -> Result<Option<String>, String> {
    let Some(raw) = raw else {
        return Ok(None);
    };
    let value = raw.trim();
    if value.is_empty() {
        return Ok(None);
    }
    parse_rate_limit(value)
        .map_err(|_| "rate limit must look like 100/m, 1000/h or 50/10s".to_string())?;
    Ok(Some(value.to_string()))
}

/// Trims, drops blanks and case-insensitive duplicates.
pub fn normalize_allowed_models(raw: &[String]) -> Result<Vec<String>, String> {
    let mut seen = std::collections::HashSet::new();
    let mut output = Vec::new();

    for entry in raw {
        let trimmed = entry.trim();
        if trimmed.is_empty() {
            continue;
        }
        if trimmed.chars().count() > APITOKEN_ALLOWED_MODEL_MAX_CHARS {
            return Err("allowed model names must be at most 200 characters".to_string());
        }
        if trimmed.chars().any(|character| character.is_control()) {
            return Err("allowed model names must not contain control characters".to_string());
        }
        let without_suffix = trimmed.strip_suffix('*').unwrap_or(trimmed);
        if without_suffix.contains('*') || trimmed == "*" {
            return Err(
                "\"*\" is only allowed at the end of a model name, after a prefix".to_string(),
            );
        }

        let key = trimmed.to_lowercase();
        if seen.insert(key) {
            output.push(trimmed.to_string());
        }
    }

    if output.len() > APITOKEN_ALLOWED_MODELS_MAX {
        return Err("a token may list at most 500 allowed models".to_string());
    }
    Ok(output)
}

/// Case-insensitive allowlist matching; an empty list allows everything.
pub fn model_allowed(allowed: &[String], model: &str) -> bool {
    if allowed.is_empty() {
        return true;
    }
    let target = model.trim().to_lowercase();
    for entry in allowed {
        let pattern = entry.to_lowercase();
        if let Some(prefix) = pattern.strip_suffix('*') {
            if target.starts_with(prefix) {
                return true;
            }
            continue;
        }
        if target == pattern {
            return true;
        }
    }
    false
}

#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct APITokenRow {
    pub id: i64,
    pub name: String,
    pub description: String,
    pub token_preview: String,
    /// The stored plaintext, empty for rows written before it was kept.
    pub token: String,
    pub enabled: i64,
    pub expires_at: Option<String>,
    pub created_at: String,
    pub updated_at: String,
    pub group_id: i64,
    pub used_tokens: i64,
    pub limit_tokens: Option<i64>,
    pub rate_limit: Option<String>,
    /// JSON array string, `"[]"` when unrestricted.
    pub allowed_models: String,
}

#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct APITokenIn {
    pub name: String,
    pub description: String,
    /// `None` means "generate one".
    pub token: Option<String>,
    pub enabled: bool,
    pub expires_at: Option<String>,
    pub group_id: Option<i64>,
    pub limit_expression: String,
    pub rate_limit: Option<String>,
    pub allowed_models: Vec<String>,
}

impl Default for APITokenIn {
    fn default() -> Self {
        Self {
            name: String::new(),
            description: String::new(),
            token: None,
            // The console's create form leaves `enabled` out, meaning enabled.
            enabled: true,
            expires_at: None,
            group_id: None,
            limit_expression: String::new(),
            rate_limit: None,
            allowed_models: Vec::new(),
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct APITokenUpdateIn {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub expires_at: Option<String>,
    #[serde(default)]
    pub group_id: Option<i64>,
    /// Absent, null or blank all keep the current credential.
    #[serde(default)]
    pub token: Option<String>,
    #[serde(default)]
    pub limit_expression: String,
    #[serde(default)]
    pub rate_limit: Option<String>,
    #[serde(default)]
    pub allowed_models: Vec<String>,
    #[serde(default)]
    pub enabled: Option<bool>,
}

/// Carries the full token value so the console can hand it back to its owner.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct APITokenOut {
    pub id: i64,
    pub name: String,
    pub description: String,
    pub token: String,
    pub token_preview: String,
    pub enabled: bool,
    pub expires_at: Option<String>,
    pub created_at: String,
    pub updated_at: String,
    pub group_id: i64,
    pub group_name: String,
    pub quota: QuotaState,
    pub rate_limit: Option<String>,
    pub allowed_models: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct APITokenCreatedOut {
    pub id: i64,
    pub name: String,
    pub description: String,
    pub token: String,
    pub token_preview: String,
    pub enabled: bool,
    pub expires_at: Option<String>,
    pub created_at: String,
    pub updated_at: String,
    pub group_id: i64,
    pub group_name: String,
    pub quota: QuotaState,
    pub rate_limit: Option<String>,
    pub allowed_models: Vec<String>,
}

impl From<&APITokenOut> for APITokenCreatedOut {
    fn from(value: &APITokenOut) -> Self {
        Self {
            id: value.id,
            name: value.name.clone(),
            description: value.description.clone(),
            token: value.token.clone(),
            token_preview: value.token_preview.clone(),
            enabled: value.enabled,
            expires_at: value.expires_at.clone(),
            created_at: value.created_at.clone(),
            updated_at: value.updated_at.clone(),
            group_id: value.group_id,
            group_name: value.group_name.clone(),
            quota: value.quota.clone(),
            rate_limit: value.rate_limit.clone(),
            allowed_models: value.allowed_models.clone(),
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct TokenEnabledIn {
    pub enabled: Option<bool>,
}

impl TokenEnabledIn {
    pub fn value(&self) -> Result<bool, String> {
        self.enabled
            .ok_or_else(|| "enabled is required".to_string())
    }
}

/// The fields a replacing edit must carry as values.
pub const TOKEN_UPDATE_REQUIRED: &[&str] = &[
    "name",
    "description",
    "expires_at",
    "group_id",
    "limit_expression",
    "rate_limit",
    "allowed_models",
];

/// Names the required fields an edit left out or sent as null where null means
/// nothing.
pub fn missing_token_fields(
    body: &serde_json::Map<String, serde_json::Value>,
) -> Vec<&'static str> {
    let mut missing = Vec::new();
    for name in TOKEN_UPDATE_REQUIRED {
        let nullable = *name == "expires_at" || *name == "rate_limit";
        match body.get(*name) {
            None => missing.push(*name),
            Some(serde_json::Value::Null) if !nullable => missing.push(*name),
            _ => {}
        }
    }
    missing
}

impl APITokenUpdateIn {
    /// The replacement value, or an empty string when the edit keeps the
    /// current credential.
    pub fn requested_token(&self) -> &str {
        self.token.as_deref().unwrap_or("")
    }
}

impl APITokenIn {
    pub fn validate(&mut self) -> Result<(), String> {
        validate_token_metadata(&mut self.name, &mut self.description)?;
        normalize_expires_at(self.expires_at.as_deref())?;
        parse_quota_expression(&self.limit_expression)?;
        normalize_rate_limit(self.rate_limit.as_deref())?;
        normalize_allowed_models(&self.allowed_models)?;
        if let Some(token) = &self.token {
            validate_token_value(token)?;
        }
        Ok(())
    }

    pub fn normalized_expires_at(&self) -> Result<Option<String>, String> {
        normalize_expires_at(self.expires_at.as_deref())
    }

    pub fn parsed_limit(&self) -> Result<Option<i64>, String> {
        parse_quota_expression(&self.limit_expression)
    }

    pub fn normalized_rate_limit(&self) -> Result<Option<String>, String> {
        normalize_rate_limit(self.rate_limit.as_deref())
    }
}

impl APITokenUpdateIn {
    pub fn validate(&mut self) -> Result<(), String> {
        validate_token_metadata(&mut self.name, &mut self.description)?;
        normalize_expires_at(self.expires_at.as_deref())?;
        parse_quota_expression(&self.limit_expression)?;
        normalize_rate_limit(self.rate_limit.as_deref())?;
        normalize_allowed_models(&self.allowed_models)?;
        if !self.requested_token().is_empty() {
            validate_token_value(self.requested_token())?;
        }
        Ok(())
    }

    pub fn normalized_expires_at(&self) -> Result<Option<String>, String> {
        normalize_expires_at(self.expires_at.as_deref())
    }

    pub fn parsed_limit(&self) -> Result<Option<i64>, String> {
        parse_quota_expression(&self.limit_expression)
    }

    pub fn normalized_rate_limit(&self) -> Result<Option<String>, String> {
        normalize_rate_limit(self.rate_limit.as_deref())
    }
}

/// Judges the stored name and description, trimming them in place.
pub fn validate_token_metadata(name: &mut String, description: &mut String) -> Result<(), String> {
    let trimmed_name = name.trim();
    if trimmed_name.is_empty() || trimmed_name.chars().count() > APITOKEN_NAME_MAX_CHARS {
        return Err("token name must be between 1 and 80 characters".to_string());
    }
    if trimmed_name.chars().any(|character| character.is_control()) {
        return Err("token name must not contain control characters".to_string());
    }

    let trimmed_description = description.trim();
    if trimmed_description.chars().count() > APITOKEN_DESCRIPTION_MAX_CHARS {
        return Err("token description must be at most 200 characters".to_string());
    }
    if trimmed_description
        .chars()
        .any(|character| character.is_control())
    {
        return Err("token description must not contain control characters".to_string());
    }

    *name = trimmed_name.to_string();
    *description = trimmed_description.to_string();
    Ok(())
}

pub fn is_ascii_graphic(value: &str) -> bool {
    value.bytes().all(|byte| byte > 0x20 && byte < 0x7f)
}

/// Judges an operator-supplied credential.
pub fn validate_token_value(token: &str) -> Result<(), String> {
    if token.len() < APITOKEN_MIN_BYTES || token.len() > APITOKEN_MAX_BYTES {
        return Err("custom token must be between 1 and 256 bytes".to_string());
    }
    if !is_ascii_graphic(token) {
        return Err(
            "custom token must contain only printable ASCII characters without spaces".to_string(),
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn expiry_accepts_rfc3339_and_the_stored_shape() {
        assert_eq!(normalize_expires_at(None).unwrap(), None);
        assert_eq!(normalize_expires_at(Some("  ")).unwrap(), None);
        assert_eq!(
            normalize_expires_at(Some("2030-01-02T03:04:05Z")).unwrap(),
            Some("2030-01-02 03:04:05".to_string())
        );
        assert_eq!(
            normalize_expires_at(Some("2030-01-02 03:04:05")).unwrap(),
            Some("2030-01-02 03:04:05".to_string())
        );
        assert!(normalize_expires_at(Some("tomorrow")).is_err());
    }

    #[test]
    fn timestamps_are_zero_padded() {
        let parsed = OffsetDateTime::parse("2024-03-04T05:06:07Z", &Rfc3339).unwrap();
        assert_eq!(format_timestamp(parsed), "2024-03-04 05:06:07");
    }

    #[test]
    fn rate_limit_is_trimmed_and_validated() {
        assert_eq!(normalize_rate_limit(None).unwrap(), None);
        assert_eq!(normalize_rate_limit(Some(" ")).unwrap(), None);
        assert_eq!(
            normalize_rate_limit(Some(" 100/m ")).unwrap(),
            Some("100/m".to_string())
        );
        assert!(normalize_rate_limit(Some("100")).is_err());
    }

    #[test]
    fn allowed_models_are_deduplicated_case_insensitively() {
        let models = normalize_allowed_models(&[
            " gpt-4o ".to_string(),
            "GPT-4O".to_string(),
            "claude-*".to_string(),
            "".to_string(),
        ])
        .unwrap();
        assert_eq!(models, vec!["gpt-4o", "claude-*"]);
    }

    #[test]
    fn a_star_is_only_allowed_as_a_suffix() {
        assert!(normalize_allowed_models(&["*".to_string()]).is_err());
        assert!(normalize_allowed_models(&["gpt*4".to_string()]).is_err());
        assert!(normalize_allowed_models(&["gpt-*".to_string()]).is_ok());
    }

    #[test]
    fn model_matching_is_case_insensitive() {
        let allowed = vec!["gpt-4o".to_string(), "claude-*".to_string()];
        assert!(model_allowed(&allowed, "GPT-4o"));
        assert!(model_allowed(&allowed, "claude-sonnet-5"));
        assert!(!model_allowed(&allowed, "o3"));
        assert!(model_allowed(&[], "anything"));
    }

    #[test]
    fn token_metadata_is_trimmed_and_bounded() {
        let mut name = "  ci  ".to_string();
        let mut description = "  smoke  ".to_string();
        validate_token_metadata(&mut name, &mut description).unwrap();
        assert_eq!(name, "ci");
        assert_eq!(description, "smoke");

        let mut too_long = "x".repeat(81);
        assert!(validate_token_metadata(&mut too_long, &mut String::new()).is_err());
    }

    #[test]
    fn custom_token_must_be_printable_ascii() {
        assert!(validate_token_value("sk-abc").is_ok());
        assert!(validate_token_value("").is_err());
        assert!(validate_token_value("has space").is_err());
        assert!(validate_token_value("tab\there").is_err());
        assert!(validate_token_value(&"x".repeat(257)).is_err());
    }

    #[test]
    fn missing_fields_reports_null_where_null_means_nothing() {
        let body = serde_json::json!({
            "name": "a",
            "description": "b",
            "expires_at": null,
            "rate_limit": null,
            "group_id": null,
        });
        let map = body.as_object().unwrap();
        let missing = missing_token_fields(map);
        assert!(!missing.contains(&"expires_at"));
        assert!(!missing.contains(&"rate_limit"));
        assert!(missing.contains(&"group_id"));
        assert!(missing.contains(&"allowed_models"));
    }
}
