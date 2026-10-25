//! Token generation, digest and preview helpers.

use std::fmt::Write as _;

use base64::Engine;
use rand::RngCore;
use sha2::{Digest, Sha256};

const TOKEN_PREVIEW_CHARS: usize = 8;
const API_TOKEN_PREFIX: &str = "sk-";
const API_TOKEN_RANDOM_CHARS: &[u8] =
    b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
const API_TOKEN_RANDOM_LEN: usize = 32;
/// The largest multiple of the alphabet size that fits in a byte. Bytes at or
/// above it are discarded rather than folded, which would skew the alphabet.
const API_TOKEN_RANDOM_CUTOFF: u8 = (256 - (256 % API_TOKEN_RANDOM_CHARS.len())) as u8;

/// Mints a fresh downstream token (`sk-` plus 32 random alphanumerics).
pub fn generate_api_token() -> String {
    let mut random = Vec::with_capacity(API_TOKEN_RANDOM_LEN);
    let mut buffer = [0u8; API_TOKEN_RANDOM_LEN];
    let mut rng = rand::thread_rng();

    while random.len() < API_TOKEN_RANDOM_LEN {
        rng.fill_bytes(&mut buffer);
        for byte in buffer {
            if byte >= API_TOKEN_RANDOM_CUTOFF {
                continue;
            }
            random.push(API_TOKEN_RANDOM_CHARS[byte as usize % API_TOKEN_RANDOM_CHARS.len()]);
            if random.len() == API_TOKEN_RANDOM_LEN {
                break;
            }
        }
    }

    let mut token = String::with_capacity(API_TOKEN_PREFIX.len() + API_TOKEN_RANDOM_LEN);
    token.push_str(API_TOKEN_PREFIX);
    token.push_str(std::str::from_utf8(&random).expect("the alphabet is ASCII"));
    token
}

/// The form authentication matches against: the lowercase SHA-256 hex digest.
pub fn token_digest(token: &str) -> String {
    let digest = Sha256::digest(token.as_bytes());
    let mut encoded = String::with_capacity(digest.len() * 2);
    for byte in digest.iter() {
        let _ = write!(encoded, "{byte:02x}");
    }
    encoded
}

/// Renders the console hint, never revealing a short token in full.
pub fn token_preview(token: &str) -> String {
    let runes: Vec<char> = token.chars().collect();
    let mut visible = runes.len() / 2;
    if runes.len() > TOKEN_PREVIEW_CHARS {
        visible = TOKEN_PREVIEW_CHARS;
    }
    let quarter = runes.len() / 4;
    if visible > quarter {
        visible = quarter;
    }
    let mut preview: String = runes[..visible].iter().collect();
    preview.push('…');
    preview
}

/// A collision-free prefix for the legacy `token` column during migration.
pub fn unique_marker_prefix() -> String {
    let mut bytes = [0u8; 16];
    rand::thread_rng().fill_bytes(&mut bytes);
    let encoded = base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(bytes);
    format!("__wildtoken_token_migration_{encoded}__")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generated_tokens_have_the_documented_shape() {
        let token = generate_api_token();
        assert_eq!(token.len(), 3 + 32);
        assert!(token.starts_with("sk-"));
        assert!(token[3..]
            .bytes()
            .all(|byte| API_TOKEN_RANDOM_CHARS.contains(&byte)));
    }

    #[test]
    fn digests_are_lowercase_hex() {
        assert_eq!(
            token_digest("sk-test"),
            "f3abf2a6cc4f00987743db5f544ba345b4899ae31f326d8ee9c4816de153c9e0"
        );
        assert_eq!(
            token_digest(""),
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
        assert!(token_digest("A") != token_digest("a"));
    }

    #[test]
    fn previews_never_reveal_a_short_token() {
        assert_eq!(
            token_preview("sk-abcdefghijklmnopqrstuvwxyz012345"),
            "sk-abcde…"
        );
        assert_eq!(token_preview("12345678"), "12…");
        assert_eq!(token_preview("abcd"), "a…");
    }
}
