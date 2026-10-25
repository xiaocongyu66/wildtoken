//! Parses expressions such as `100/m`, `1000/h` and `50/10s`.

use std::time::Duration;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RateLimit {
    pub requests: i64,
    pub window: Duration,
}

/// Returns `None` for an empty expression, which means "no limit".
pub fn parse_rate_limit(expression: &str) -> Result<Option<RateLimit>, String> {
    if expression.is_empty() {
        return Ok(None);
    }

    let invalid = || {
        format!(
            "invalid rate limit expression: {expression} (expected format: 100/m, 1000/h, 50/10s)"
        )
    };

    let (count_text, rest) = expression.split_once('/').ok_or_else(invalid)?;
    if count_text.is_empty() || !count_text.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err(invalid());
    }

    let mut chars = rest.chars();
    let unit = chars.next_back().ok_or_else(invalid)?;
    if !matches!(unit, 's' | 'm' | 'h' | 'd') {
        return Err(invalid());
    }
    let multiplier_text: String = chars.collect();
    if !multiplier_text.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err(invalid());
    }

    let requests: i64 = count_text
        .parse()
        .map_err(|_| format!("invalid request count in rate limit: {count_text}"))?;
    if requests <= 0 {
        return Err(format!("invalid request count in rate limit: {count_text}"));
    }

    let multiplier: i64 = if multiplier_text.is_empty() {
        1
    } else {
        let parsed: i64 = multiplier_text
            .parse()
            .map_err(|_| format!("invalid time multiplier in rate limit: {multiplier_text}"))?;
        if parsed <= 0 {
            return Err(format!(
                "invalid time multiplier in rate limit: {multiplier_text}"
            ));
        }
        parsed
    };

    let base_millis: i64 = match unit {
        's' => 1_000,
        'm' => 60_000,
        'h' => 3_600_000,
        'd' => 86_400_000,
        _ => return Err(invalid()),
    };
    let Some(total_millis) = multiplier.checked_mul(base_millis) else {
        return Err(format!("rate limit window is too long: {expression}"));
    };

    Ok(Some(RateLimit {
        requests,
        window: Duration::from_millis(total_millis as u64),
    }))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_the_documented_forms() {
        assert_eq!(parse_rate_limit("").unwrap(), None);
        assert_eq!(
            parse_rate_limit("100/s").unwrap().unwrap(),
            RateLimit {
                requests: 100,
                window: Duration::from_secs(1)
            }
        );
        assert_eq!(
            parse_rate_limit("1000/h").unwrap().unwrap(),
            RateLimit {
                requests: 1000,
                window: Duration::from_secs(3600)
            }
        );
        assert_eq!(
            parse_rate_limit("50/10s").unwrap().unwrap(),
            RateLimit {
                requests: 50,
                window: Duration::from_secs(10)
            }
        );
    }

    #[test]
    fn rejects_malformed_expressions() {
        for expression in ["100", "100/", "/m", "abc/m", "100/10x", "100  /m"] {
            assert!(parse_rate_limit(expression).is_err(), "{expression}");
        }
        assert!(parse_rate_limit("0/m").is_err());
        assert!(parse_rate_limit("100/0s").is_err());
        assert!(parse_rate_limit("9223372036854775808/m").is_err());
    }

    #[test]
    fn rejects_windows_that_overflow() {
        let error = parse_rate_limit("1/9223372036854775807d").unwrap_err();
        assert!(error.contains("too long"));
    }
}
