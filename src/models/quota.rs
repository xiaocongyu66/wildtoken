//! Token quota expressions such as `100M` and `1B`.

use serde::Serialize;

/// SQLite holds an int64; a larger limit is indistinguishable from unlimited.
pub const QUOTA_MAX_TOKENS: i64 = 1 << 62;

/// Returns `None` for an empty expression, which means "no limit".
pub fn parse_quota_expression(raw: &str) -> Result<Option<i64>, String> {
    let value = raw.trim();
    if value.is_empty() {
        return Ok(None);
    }

    let upper = value.to_uppercase();
    let mut body = upper.as_str();
    let mut unit = "";
    if let Some(last) = upper.chars().last().filter(|last| !last.is_ascii_digit()) {
        unit = &upper[upper.len() - last.len_utf8()..];
        body = &upper[..upper.len() - last.len_utf8()];
    }

    let multiplier: i64 = match unit {
        "" => 1,
        "K" => 1_000,
        "M" => 1_000_000,
        "B" => 1_000_000_000,
        "T" => 1_000_000_000_000,
        _ => return Err("limit unit must be one of K, M, B, T".to_string()),
    };
    if body.is_empty() {
        return Err("limit must start with a number, for example 100M".to_string());
    }
    if !is_plain_decimal(body) {
        return Err("limit must be a number optionally followed by K, M, B or T".to_string());
    }

    let amount: f64 = body
        .parse()
        .map_err(|_| "limit must be a number optionally followed by K, M, B or T".to_string())?;
    if amount < 0.0 {
        return Err("limit must not be negative".to_string());
    }

    let scaled = amount * multiplier as f64;
    if scaled > QUOTA_MAX_TOKENS as f64 {
        return Err("limit is too large".to_string());
    }
    let limit = scaled.round() as i64;
    if limit <= 0 {
        return Err("limit must be at least 1 token".to_string());
    }
    Ok(Some(limit))
}

fn is_plain_decimal(value: &str) -> bool {
    let mut seen_point = false;
    let mut seen_digit = false;
    for character in value.chars() {
        match character {
            '0'..='9' => seen_digit = true,
            '.' => {
                if seen_point {
                    return false;
                }
                seen_point = true;
            }
            _ => return false,
        }
    }
    seen_digit
}

/// Renders a stored limit back into the shortest exact expression.
pub fn format_quota(limit: Option<i64>) -> String {
    let Some(value) = limit else {
        return String::new();
    };

    for (suffix, multiplier) in [
        ("T", 1_000_000_000_000_i64),
        ("B", 1_000_000_000),
        ("M", 1_000_000),
        ("K", 1_000),
    ] {
        if value < multiplier {
            continue;
        }
        let value = value as i128;
        let multiplier = multiplier as i128;
        if value % multiplier == 0 {
            return format!("{}{suffix}", value / multiplier);
        }
        let tenths = value * 10 / multiplier;
        if tenths * multiplier == value * 10 {
            return format_tenths(tenths, suffix);
        }
        break;
    }
    value.to_string()
}

fn format_tenths(tenths: i128, suffix: &str) -> String {
    if tenths % 10 == 0 {
        format!("{}{suffix}", tenths / 10)
    } else {
        format!("{}.{}{suffix}", tenths / 10, tenths % 10)
    }
}

#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct QuotaState {
    pub used_tokens: i64,
    pub limit_tokens: Option<i64>,
    pub limit_expression: String,
    pub remaining_tokens: Option<i64>,
    pub exhausted: bool,
}

impl QuotaState {
    pub fn new(used_tokens: i64, limit_tokens: Option<i64>) -> Self {
        let mut state = Self {
            used_tokens,
            limit_tokens,
            limit_expression: format_quota(limit_tokens),
            remaining_tokens: None,
            exhausted: false,
        };
        let Some(limit) = limit_tokens else {
            return state;
        };
        let remaining = (limit - used_tokens).max(0);
        state.remaining_tokens = Some(remaining);
        state.exhausted = used_tokens >= limit;
        state
    }
}

pub fn quota_exceeded_message(used_tokens: i64, limit_tokens: i64) -> String {
    format!(
        "token quota exhausted: {} of {} tokens used",
        format_quota(Some(used_tokens)),
        format_quota(Some(limit_tokens))
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_documented_expressions() {
        assert_eq!(parse_quota_expression("").unwrap(), None);
        assert_eq!(parse_quota_expression("  ").unwrap(), None);
        assert_eq!(parse_quota_expression("100M").unwrap(), Some(100_000_000));
        assert_eq!(parse_quota_expression("100m").unwrap(), Some(100_000_000));
        assert_eq!(parse_quota_expression("1B").unwrap(), Some(1_000_000_000));
        assert_eq!(parse_quota_expression("1000K").unwrap(), Some(1_000_000));
        assert_eq!(parse_quota_expression("250000").unwrap(), Some(250_000));
        assert_eq!(parse_quota_expression("1.5M").unwrap(), Some(1_500_000));
        assert_eq!(parse_quota_expression("0.5").unwrap(), Some(1));
    }

    #[test]
    fn rejects_bad_expressions() {
        for expression in ["1e9", "+5", "100 M", "100X", "abc", "-5", "0", "1..5"] {
            assert!(
                parse_quota_expression(expression).is_err(),
                "{expression} was accepted"
            );
        }
        assert!(parse_quota_expression("999999999999999999999T").is_err());
    }

    #[test]
    fn formats_the_shortest_exact_expression() {
        assert_eq!(format_quota(None), "");
        assert_eq!(format_quota(Some(100_000_000)), "100M");
        assert_eq!(format_quota(Some(8_200_000)), "8.2M");
        assert_eq!(format_quota(Some(1_500_000)), "1.5M");
        assert_eq!(format_quota(Some(250_000)), "250K");
        assert_eq!(format_quota(Some(999)), "999");
    }

    #[test]
    fn quota_state_floors_remaining_at_zero() {
        let state = QuotaState::new(120, Some(100));
        assert_eq!(state.remaining_tokens, Some(0));
        assert!(state.exhausted);
        assert_eq!(state.limit_expression, "100");

        let unlimited = QuotaState::new(120, None);
        assert_eq!(unlimited.remaining_tokens, None);
        assert!(!unlimited.exhausted);
    }
}
