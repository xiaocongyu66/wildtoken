//! How a downstream path is read, shared by authentication and forwarding.

/// The part of a downstream URL that names the endpoint: `chat/completions`.
///
/// The `/v1` prefix is matched as a whole segment, so `/v1beta/...` keeps its
/// first segment.
pub fn proxy_path(url_path: &str) -> String {
    let trimmed = url_path.trim_matches('/');
    if trimmed == "v1" {
        return String::new();
    }
    let rest = trimmed.strip_prefix("v1/").unwrap_or(trimmed);
    rest.trim_matches('/').to_string()
}

/// Whether a proxy path addresses the Anthropic Messages API, including its
/// sub-resources.
pub fn is_anthropic_messages(proxy_path: &str) -> bool {
    proxy_path == "messages" || proxy_path.starts_with("messages/")
}

/// Whether the gateway relays a request: inference endpoints only.
pub fn proxy_endpoint_allowed(method: &str, proxy_path: &str) -> bool {
    if proxy_path == "responses" || proxy_path.starts_with("responses/") {
        return true;
    }
    if method == "GET" && proxy_path.starts_with("models/") {
        return true;
    }
    if method != "POST" {
        return false;
    }
    matches!(
        proxy_path,
        "chat/completions"
            | "completions"
            | "embeddings"
            | "moderations"
            | "messages"
            | "messages/count_tokens"
            | "images/generations"
            | "images/edits"
            | "images/variations"
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn proxy_path_strips_the_version_segment() {
        assert_eq!(proxy_path("/v1/chat/completions"), "chat/completions");
        assert_eq!(proxy_path("/v1//messages"), "messages");
        assert_eq!(proxy_path("v1"), "");
        assert_eq!(proxy_path("/v1/"), "");
        assert_eq!(proxy_path("/v1beta/models"), "v1beta/models");
        assert_eq!(proxy_path("images/generations"), "images/generations");
    }

    #[test]
    fn anthropic_messages_includes_sub_resources() {
        assert!(is_anthropic_messages("messages"));
        assert!(is_anthropic_messages("messages/count_tokens"));
        assert!(!is_anthropic_messages("message"));
        assert!(!is_anthropic_messages("chat/completions"));
    }

    #[test]
    fn endpoint_allowlist_matches_the_original() {
        for path in [
            "chat/completions",
            "completions",
            "embeddings",
            "moderations",
            "messages",
            "messages/count_tokens",
            "images/generations",
            "images/edits",
            "images/variations",
        ] {
            assert!(proxy_endpoint_allowed("POST", path), "{path}");
        }
        assert!(proxy_endpoint_allowed("POST", "responses"));
        assert!(proxy_endpoint_allowed("GET", "responses/abc"));
        assert!(proxy_endpoint_allowed("GET", "models/gpt-4o"));
        assert!(!proxy_endpoint_allowed("GET", "chat/completions"));
        for path in [
            "files",
            "batches",
            "fine_tuning/jobs",
            "audio/speech",
            "models",
        ] {
            assert!(!proxy_endpoint_allowed("POST", path), "{path}");
        }
    }
}
