//! Discovers external CSS-only theme packs on disk.

use std::fs;
use std::path::Path;

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct Pack {
    pub id: String,
    pub label: String,
    pub swatch: [String; 2],
    pub css: String,
    pub version: Option<String>,
    pub description: Option<String>,
    pub source: String,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
struct Manifest {
    id: String,
    label: String,
    name: String,
    css: String,
    swatch: Vec<String>,
    version: Option<String>,
    description: Option<String>,
}

/// Reads every valid theme pack under root. A missing root yields no packs.
pub fn list_packs(root: &Path) -> Result<Vec<Pack>, String> {
    let entries = match fs::read_dir(root) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(error) => return Err(error.to_string()),
    };

    let mut packs = Vec::new();
    for entry in entries {
        let entry = match entry {
            Ok(entry) => entry,
            Err(error) => {
                tracing::warn!(%error, "could not read a theme entry");
                continue;
            }
        };
        let is_dir = entry.file_type().map(|kind| kind.is_dir()).unwrap_or(false);
        if !is_dir {
            continue;
        }
        let dir_name = entry.file_name().to_string_lossy().to_string();
        match read_manifest(root, &dir_name) {
            Ok(Some(pack)) => packs.push(pack),
            Ok(None) => {}
            Err(error) => {
                tracing::warn!(
                    path = %root.join(&dir_name).join("theme.json").display(),
                    %error,
                    "skipping invalid theme pack"
                );
            }
        }
    }

    packs.sort_by(|left, right| {
        left.label
            .cmp(&right.label)
            .then_with(|| left.id.cmp(&right.id))
    });
    Ok(packs)
}

fn read_manifest(root: &Path, dir_name: &str) -> Result<Option<Pack>, String> {
    let manifest_path = root.join(dir_name).join("theme.json");
    let text = match fs::read_to_string(&manifest_path) {
        Ok(text) => text,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(error.to_string()),
    };

    let manifest: Manifest = serde_json::from_str(&text).map_err(|error| error.to_string())?;

    let id = manifest.id.trim().to_string();
    if id != dir_name {
        return Err("manifest id must match its directory name".to_string());
    }
    if !is_valid_theme_id(&id) {
        return Err("theme id must use lowercase letters, numbers, and hyphens".to_string());
    }

    let css_path = normalize_css_path(&manifest.css)
        .ok_or_else(|| "css must be a relative .css path".to_string())?;
    let css_file = root.join(&id).join(&css_path);
    let info = fs::metadata(&css_file).map_err(|error| error.to_string())?;
    if !info.is_file() {
        return Err("css path is not a file".to_string());
    }

    let label_source = if manifest.label.trim().is_empty() {
        &manifest.name
    } else {
        &manifest.label
    };

    Ok(Some(Pack {
        id: id.clone(),
        label: clean_text(label_source, &id, 64),
        swatch: normalize_swatch(&manifest.swatch),
        css: format!("/theme-packs/{id}/{css_path}"),
        version: optional_text(manifest.version.as_deref(), 32),
        description: optional_text(manifest.description.as_deref(), 160),
        source: "external".to_string(),
    }))
}

/// Trims a manifest string, drops control characters and bounds its length.
fn clean_text(value: &str, fallback: &str, max_chars: usize) -> String {
    let mut cleaned = String::new();
    for character in value.trim().chars() {
        if character.is_control() {
            continue;
        }
        if cleaned.chars().count() >= max_chars {
            break;
        }
        cleaned.push(character);
    }
    if cleaned.is_empty() {
        return fallback.to_string();
    }
    cleaned
}

fn optional_text(value: Option<&str>, max_chars: usize) -> Option<String> {
    let value = value?;
    let cleaned = clean_text(value, "", max_chars);
    if cleaned.is_empty() {
        None
    } else {
        Some(cleaned)
    }
}

fn normalize_swatch(values: &[String]) -> [String; 2] {
    let mut swatch = ["#f8fafc".to_string(), "#f97316".to_string()];
    for (index, slot) in swatch.iter_mut().enumerate() {
        if let Some(value) = values.get(index) {
            if is_hex_color(value) {
                *slot = value.clone();
            }
        }
    }
    swatch
}

fn is_hex_color(value: &str) -> bool {
    let Some(hex) = value.trim().strip_prefix('#') else {
        return false;
    };
    if !matches!(hex.len(), 3 | 4 | 6 | 8) {
        return false;
    }
    hex.chars().all(|character| character.is_ascii_hexdigit())
}

fn is_valid_theme_id(value: &str) -> bool {
    if value.is_empty() || value.len() > 48 {
        return false;
    }
    for (index, character) in value.chars().enumerate() {
        if index == 0 {
            if !character.is_ascii_lowercase() {
                return false;
            }
            continue;
        }
        if !(character.is_ascii_lowercase() || character.is_ascii_digit() || character == '-') {
            return false;
        }
    }
    true
}

/// Accepts only a relative `.css` path built from plain segments.
fn normalize_css_path(value: &str) -> Option<String> {
    let value = value.trim();
    if value.is_empty() || !value.ends_with(".css") {
        return None;
    }
    if value.starts_with('/') || value.contains('\\') {
        return None;
    }

    let mut cleaned = Vec::new();
    for part in value.split('/') {
        if part.is_empty() || part == "." || part == ".." {
            return None;
        }
        if !part.chars().all(|character| {
            character.is_ascii_alphanumeric()
                || character == '.'
                || character == '_'
                || character == '-'
        }) {
            return None;
        }
        cleaned.push(part);
    }
    if cleaned.is_empty() {
        return None;
    }
    Some(cleaned.join("/"))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn write_pack(root: &Path, id: &str, manifest: &str, css: &str) {
        let dir = root.join(id);
        fs::create_dir_all(&dir).unwrap();
        fs::write(dir.join("theme.json"), manifest).unwrap();
        fs::write(dir.join("theme.css"), css).unwrap();
    }

    #[test]
    fn lists_valid_packs_and_skips_invalid_ones() {
        let dir = tempfile::tempdir().unwrap();
        write_pack(
            dir.path(),
            "zeta",
            r##"{"id":"zeta","label":"Zeta","css":"theme.css","swatch":["#000000","#ffffff"]}"##,
            ":root{}",
        );
        write_pack(
            dir.path(),
            "alpha",
            r#"{"id":"alpha","name":"Alpha Name","css":"theme.css"}"#,
            ":root{}",
        );
        write_pack(
            dir.path(),
            "broken",
            r#"{"id":"other","label":"Broken","css":"theme.css"}"#,
            ":root{}",
        );

        let packs = list_packs(dir.path()).unwrap();
        assert_eq!(packs.len(), 2);
        assert_eq!(packs[0].id, "alpha");
        assert_eq!(packs[0].label, "Alpha Name");
        assert_eq!(packs[0].swatch, ["#f8fafc", "#f97316"]);
        assert_eq!(packs[0].css, "/theme-packs/alpha/theme.css");
        assert_eq!(packs[1].id, "zeta");
        assert_eq!(packs[1].swatch, ["#000000", "#ffffff"]);
        assert_eq!(packs[0].source, "external");
    }

    #[test]
    fn a_missing_root_is_not_an_error() {
        let dir = tempfile::tempdir().unwrap();
        assert!(list_packs(&dir.path().join("missing")).unwrap().is_empty());
    }

    #[test]
    fn css_paths_cannot_escape_the_pack() {
        assert_eq!(
            normalize_css_path("theme.css"),
            Some("theme.css".to_string())
        );
        assert_eq!(
            normalize_css_path("css/theme.css"),
            Some("css/theme.css".to_string())
        );
        for value in [
            "../theme.css",
            "/theme.css",
            "css/../theme.css",
            "theme.txt",
            "a b.css",
        ] {
            assert_eq!(normalize_css_path(value), None, "{value}");
        }
    }

    #[test]
    fn theme_ids_follow_the_documented_shape() {
        assert!(is_valid_theme_id("ark"));
        assert!(is_valid_theme_id("sakura-mist"));
        assert!(is_valid_theme_id("a1-b"));
        assert!(!is_valid_theme_id(""));
        assert!(!is_valid_theme_id("Ark"));
        assert!(!is_valid_theme_id("1ark"));
        assert!(!is_valid_theme_id("ark_1"));
        assert!(!is_valid_theme_id(&"a".repeat(49)));
    }

    #[test]
    fn labels_drop_control_characters_and_fall_back() {
        assert_eq!(clean_text("  Name\u{0007}", "fallback", 10), "Name");
        assert_eq!(clean_text("   ", "fallback", 10), "fallback");
        assert_eq!(clean_text("abcdef", "fallback", 3), "abc");
    }
}
