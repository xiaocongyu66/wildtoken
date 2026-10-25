//! Shared application state and startup wiring.

use std::net::IpAddr;
use std::sync::{Arc, RwLock};

use time::OffsetDateTime;

use crate::auth::credential::{hash_admin_token, verify_admin_token, Credentials};
use crate::auth::throttle::Throttle;
use crate::config::Settings;
use crate::db::{self, Db};
use crate::metrics::Runtime;
use crate::models::settings::{validate_admin_token, AdminCredential, RuntimeSettings};

/// Holds the operator-editable runtime policy.
pub struct SettingsStore {
    current: RwLock<RuntimeSettings>,
}

impl SettingsStore {
    pub fn new(initial: RuntimeSettings) -> Self {
        Self {
            current: RwLock::new(initial),
        }
    }

    pub fn get(&self) -> RuntimeSettings {
        self.current
            .read()
            .unwrap_or_else(|error| error.into_inner())
            .clone()
    }

    pub fn set(&self, settings: RuntimeSettings) {
        let mut current = self
            .current
            .write()
            .unwrap_or_else(|error| error.into_inner());
        *current = settings;
    }
}

/// The shared application state every handler receives.
pub struct AppState {
    pub db: Db,
    pub settings: Settings,
    pub runtime: SettingsStore,
    pub credentials: Credentials,
    pub started_at: OffsetDateTime,
    pub metrics: Runtime,
}

impl AppState {
    /// The fallback timeout for channels that leave theirs unset.
    pub fn effective_upstream_timeout_seconds(&self) -> f64 {
        let runtime = self.runtime.get();
        if runtime.default_upstream_timeout_seconds > 0 {
            runtime.default_upstream_timeout_seconds as f64
        } else {
            self.settings.upstream.default_timeout_seconds
        }
    }
}

/// Opens the database, creates the schema and resolves the admin credential.
pub async fn build(settings: Settings) -> Result<AppState, String> {
    let db = Db::open(&settings.database).map_err(|error| error.message)?;
    db.with_conn(db::schema::init)
        .await
        .map_err(|error| error.message)?;

    let credential = load_or_bootstrap_admin_credential(&db, &settings).await?;
    let runtime_settings = load_runtime_settings(&db).await;

    let credentials = Credentials::new(credential, Arc::new(Throttle::new()));

    Ok(AppState {
        db,
        settings,
        runtime: SettingsStore::new(runtime_settings),
        credentials,
        started_at: OffsetDateTime::now_utc(),
        metrics: Runtime::new(),
    })
}

async fn load_runtime_settings(db: &Db) -> RuntimeSettings {
    match db::settings::load_runtime_settings(db).await {
        Ok(Some(mut settings)) => {
            if let Err(error) = settings.validate() {
                tracing::warn!(%error, "runtime_settings contains invalid values; using startup defaults");
                return RuntimeSettings::default();
            }
            settings.database_override = true;
            settings
        }
        Ok(None) => {
            tracing::warn!("runtime_settings row is missing; using startup defaults");
            RuntimeSettings::default()
        }
        Err(error) => {
            tracing::warn!(%error, "could not load runtime_settings; using startup defaults");
            RuntimeSettings::default()
        }
    }
}

async fn load_or_bootstrap_admin_credential(
    db: &Db,
    settings: &Settings,
) -> Result<AdminCredential, String> {
    let loopback_only = is_loopback_bind_host(&settings.server.host);

    if let Some(credential) = db::admin::load_admin_credential(db)
        .await
        .map_err(|error| error.message)?
    {
        let uses_known_default = verify_admin_token(&credential.credential_hash, "change-me");
        if uses_known_default && !loopback_only {
            return Err(
                "refusing non-loopback startup with the legacy change-me admin credential; \
                 start on localhost and rotate it first"
                    .to_string(),
            );
        }
        if uses_known_default {
            tracing::warn!(
                "the stored admin credential is still change-me; \
                 rotate it before exposing WildToken beyond localhost"
            );
        }
        return Ok(credential);
    }

    let token = settings.admin.token.trim();
    if token.eq_ignore_ascii_case("change-me") {
        if !loopback_only {
            return Err(
                "a new database listening beyond localhost requires an explicit ADMIN_TOKEN"
                    .to_string(),
            );
        }
        tracing::warn!(
            "using the local-only bootstrap admin token change-me; \
             rotate it from the admin console"
        );
    } else {
        validate_admin_token(token)?;
    }

    let hash = hash_admin_token(token).map_err(|error| error.message)?;
    db::admin::bootstrap_admin_credential(db, hash)
        .await
        .map_err(|error| error.message)
}

pub fn is_loopback_bind_host(host: &str) -> bool {
    let trimmed = host.trim();
    let trimmed = trimmed
        .strip_prefix('[')
        .and_then(|rest| rest.strip_suffix(']'))
        .unwrap_or(trimmed);
    if trimmed.eq_ignore_ascii_case("localhost") {
        return true;
    }
    trimmed
        .parse::<IpAddr>()
        .map(|address| address.is_loopback())
        .unwrap_or(false)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn loopback_hosts_are_recognised() {
        for host in ["127.0.0.1", "::1", "[::1]", "localhost", " 127.0.0.1 "] {
            assert!(is_loopback_bind_host(host), "{host}");
        }
        for host in ["0.0.0.0", "::", "192.168.1.2", "example.com", ""] {
            assert!(!is_loopback_bind_host(host), "{host}");
        }
    }
}
