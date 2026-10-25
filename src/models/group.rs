//! Groups scope which channels a downstream token may reach.

use serde::{Deserialize, Serialize};

pub const GROUP_NAME_MAX_CHARS: usize = 60;
pub const GROUP_DESCRIPTION_MAX_CHARS: usize = 200;

/// The seeded group every channel and token falls back to.
pub const DEFAULT_GROUP_ID: i64 = 1;
pub const DEFAULT_GROUP_NAME: &str = "default";

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Group {
    pub id: i64,
    pub name: String,
    pub description: String,
    pub created_at: String,
    pub updated_at: String,
    pub upstream_count: i64,
    pub token_count: i64,
    pub is_default: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct GroupIn {
    pub name: String,
    pub description: String,
}

impl GroupIn {
    /// Judges the values that will be stored and normalizes them in place.
    pub fn validate(&mut self) -> Result<(), String> {
        let name = self.name.trim();
        if name.is_empty() || name.chars().count() > GROUP_NAME_MAX_CHARS {
            return Err("group name must be between 1 and 60 characters".to_string());
        }
        if name.chars().any(|character| character.is_control()) {
            return Err("group name must not contain control characters".to_string());
        }

        let description = self.description.trim();
        if description.chars().count() > GROUP_DESCRIPTION_MAX_CHARS {
            return Err("group description must be at most 200 characters".to_string());
        }
        if description.chars().any(|character| character.is_control()) {
            return Err("group description must not contain control characters".to_string());
        }

        self.name = name.to_string();
        self.description = description.to_string();
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn trims_and_validates() {
        let mut input = GroupIn {
            name: "  default  ".to_string(),
            description: "  ".to_string(),
        };
        input.validate().unwrap();
        assert_eq!(input.name, "default");
        assert_eq!(input.description, "");
    }

    #[test]
    fn control_characters_are_refused() {
        let mut input = GroupIn {
            name: "bad\nname".to_string(),
            description: String::new(),
        };
        assert!(input.validate().is_err());
    }
}
