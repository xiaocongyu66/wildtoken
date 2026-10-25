//! Argon2id admin credentials, their verified-token cache and generation.

use std::sync::atomic::{AtomicI64, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, RwLock};

use argon2::password_hash::rand_core::OsRng;
use argon2::password_hash::{PasswordHash, PasswordHasher, PasswordVerifier, SaltString};
use argon2::{Algorithm, Argon2, Params, Version};
use hmac::{Hmac, Mac};
use rand::RngCore;
use sha2::Sha256;
use subtle::ConstantTimeEq;

use crate::auth::throttle::{Client, Throttle, VerificationQueue};
use crate::error::ApiError;
use crate::models::settings::AdminCredential;

/// Argon2id parameters, matching the Rust `argon2` crate defaults the service
/// previously used. A stored hash records its own parameters, so these only
/// govern new hashes.
const ARGON_MEMORY_KIB: u32 = 19_456;
const ARGON_TIME: u32 = 2;
const ARGON_THREADS: u32 = 1;
const ARGON_KEY_LEN: usize = 32;
const MIN_MEMORY_KIB: u32 = 8;
const MAX_MEMORY_KIB: u32 = 1 << 21;
const MAX_THREADS: u32 = u8::MAX as u32;

type HmacSha256 = Hmac<Sha256>;

/// Derives a PHC-encoded Argon2id hash. The plaintext is never persisted.
pub fn hash_admin_token(token: &str) -> Result<String, ApiError> {
    let params = Params::new(
        ARGON_MEMORY_KIB,
        ARGON_TIME,
        ARGON_THREADS,
        Some(ARGON_KEY_LEN),
    )
    .map_err(|_| ApiError::internal("could not configure the admin credential hash"))?;
    let argon2 = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
    let salt = SaltString::generate(&mut OsRng);
    argon2
        .hash_password(token.as_bytes(), &salt)
        .map(|hash| hash.to_string())
        .map_err(|_| ApiError::internal("could not hash admin credential"))
}

/// Checks a token against a stored credential snapshot.
pub fn verify_admin_token(encoded: &str, token: &str) -> bool {
    let Ok(parsed) = PasswordHash::new(encoded) else {
        return false;
    };
    if parsed.algorithm.as_str() != "argon2id" {
        return false;
    }
    if parsed.version != Some(0x13) {
        return false;
    }
    let Some(salt) = parsed.salt.as_ref() else {
        return false;
    };
    if salt.as_str().is_empty() {
        return false;
    }
    if parsed.hash.is_none() {
        return false;
    }

    let Some(memory) = parsed.params.get_decimal("m") else {
        return false;
    };
    let Some(time) = parsed.params.get_decimal("t") else {
        return false;
    };
    let Some(threads) = parsed.params.get_decimal("p") else {
        return false;
    };
    if time < 1
        || threads < 1
        || threads > MAX_THREADS
        || memory < MIN_MEMORY_KIB
        || memory > MAX_MEMORY_KIB
    {
        return false;
    }

    // The verifier takes its parameters from the parsed hash, which is why the
    // bounds above must be checked first: they are what keeps a corrupt row
    // from asking for an allocation the machine cannot make.
    Argon2::default()
        .verify_password(token.as_bytes(), &parsed)
        .is_ok()
}

#[derive(Debug, Clone, Copy)]
struct CachedToken {
    credential_version: i64,
    fingerprint: [u8; 32],
}

struct AuthCache {
    key: [u8; 32],
    /// Guards a comparison only; the Argon2id verification happens outside it.
    verified: Mutex<Option<CachedToken>>,
    queue: VerificationQueue,
    argon2_verifications: AtomicU64,
}

impl AuthCache {
    fn new() -> Self {
        let mut key = [0u8; 32];
        rand::thread_rng().fill_bytes(&mut key);
        Self {
            key,
            verified: Mutex::new(None),
            queue: VerificationQueue::new(),
            argon2_verifications: AtomicU64::new(0),
        }
    }

    fn fingerprint(&self, token: &str) -> [u8; 32] {
        let mut mac =
            HmacSha256::new_from_slice(&self.key).expect("HMAC accepts keys of any length");
        mac.update(token.as_bytes());
        let mut fingerprint = [0u8; 32];
        fingerprint.copy_from_slice(&mac.finalize().into_bytes());
        fingerprint
    }

    fn hit(&self, fingerprint: [u8; 32], credential_version: i64) -> bool {
        let verified = self
            .verified
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        match verified.as_ref() {
            Some(cached) if cached.credential_version == credential_version => {
                bool::from(cached.fingerprint.ct_eq(&fingerprint))
            }
            _ => false,
        }
    }

    fn store(&self, fingerprint: [u8; 32], credential_version: i64) {
        let mut verified = self
            .verified
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        *verified = Some(CachedToken {
            credential_version,
            fingerprint,
        });
    }

    fn clear(&self) {
        let mut verified = self
            .verified
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        *verified = None;
    }
}

/// Authenticates admin tokens against the current credential.
pub struct Credentials {
    current: RwLock<AdminCredential>,
    version: AtomicI64,
    cache: AuthCache,
    throttle: Arc<Throttle>,
}

impl Credentials {
    pub fn new(credential: AdminCredential, throttle: Arc<Throttle>) -> Self {
        let version = AtomicI64::new(credential.credential_version);
        Self {
            current: RwLock::new(credential),
            version,
            cache: AuthCache::new(),
            throttle,
        }
    }

    pub fn snapshot(&self) -> AdminCredential {
        self.current
            .read()
            .unwrap_or_else(|error| error.into_inner())
            .clone()
    }

    pub fn version(&self) -> i64 {
        self.version.load(Ordering::SeqCst)
    }

    /// Reports the credential generation a token proved itself against.
    ///
    /// A token already verified is answered without touching the verification
    /// slot, so wrong-token traffic cannot queue ahead of a signed-in operator.
    pub async fn authenticate(&self, token: &str, client: Client) -> Option<i64> {
        let fingerprint = self.cache.fingerprint(token);

        let mut credential_version = self.version.load(Ordering::SeqCst);
        if self.cache.hit(fingerprint, credential_version) {
            return Some(credential_version);
        }

        if !self.throttle.admit(client) {
            return None;
        }
        let _permit = self.cache.queue.enter().await?;

        // Whoever held the slot may have just verified this same token.
        credential_version = self.version.load(Ordering::SeqCst);
        if self.cache.hit(fingerprint, credential_version) {
            return Some(credential_version);
        }

        let credential = self.snapshot();
        if credential.credential_version != credential_version {
            return None;
        }

        self.cache
            .argon2_verifications
            .fetch_add(1, Ordering::SeqCst);
        let encoded = credential.credential_hash.clone();
        let candidate = token.to_string();
        let verified =
            tokio::task::spawn_blocking(move || verify_admin_token(&encoded, &candidate))
                .await
                .unwrap_or(false);

        if !verified {
            self.throttle.record_failure(client);
            return None;
        }
        // A rotation that landed mid-verification is not the caller's failure.
        if self.version.load(Ordering::SeqCst) != credential_version {
            return None;
        }

        self.cache.store(fingerprint, credential_version);
        self.throttle.record_success(client);
        Some(credential_version)
    }

    /// Installs a credential that has already committed to SQLite.
    pub fn publish(&self, credential: AdminCredential) {
        loop {
            let current = self.version.load(Ordering::SeqCst);
            if credential.credential_version <= current {
                break;
            }
            if self
                .version
                .compare_exchange(
                    current,
                    credential.credential_version,
                    Ordering::SeqCst,
                    Ordering::SeqCst,
                )
                .is_ok()
            {
                break;
            }
        }

        {
            let mut current = self
                .current
                .write()
                .unwrap_or_else(|error| error.into_inner());
            if credential.credential_version > current.credential_version {
                *current = credential;
            }
        }

        self.cache.clear();
    }

    #[cfg(test)]
    pub fn argon2_verifications(&self) -> u64 {
        self.cache.argon2_verifications.load(Ordering::SeqCst)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::IpAddr;

    fn loopback() -> Client {
        Client::from_addr("127.0.0.1".parse::<IpAddr>().unwrap())
    }

    #[test]
    fn hashes_use_the_original_parameters() {
        let hash = hash_admin_token("admin-token").unwrap();
        assert!(
            hash.starts_with("$argon2id$v=19$m=19456,t=2,p=1$"),
            "{hash}"
        );
        assert!(verify_admin_token(&hash, "admin-token"));
        assert!(!verify_admin_token(&hash, "admin-token "));
    }

    #[test]
    fn malformed_and_out_of_bounds_hashes_are_refused() {
        assert!(!verify_admin_token("", "x"));
        assert!(!verify_admin_token("not-a-phc-string", "x"));

        let hash = hash_admin_token("x").unwrap();
        for broken in [
            hash.replacen("t=2", "t=0", 1),
            hash.replacen("p=1", "p=0", 1),
            hash.replacen("m=19456", "m=1", 1),
            hash.replacen("m=19456", "m=4294967295", 1),
            hash.replacen("v=19", "v=16", 1),
            hash.replacen("$argon2id$", "$argon2i$", 1),
        ] {
            assert!(!verify_admin_token(&broken, "x"), "{broken}");
        }
    }

    #[tokio::test]
    async fn the_cache_absorbs_repeat_verifications() {
        let hash = hash_admin_token("admin-token").unwrap();
        let credentials = Credentials::new(
            AdminCredential {
                credential_hash: hash,
                credential_version: 1,
            },
            Arc::new(Throttle::new()),
        );

        assert_eq!(
            credentials.authenticate("admin-token", loopback()).await,
            Some(1)
        );
        assert_eq!(
            credentials.authenticate("admin-token", loopback()).await,
            Some(1)
        );
        assert_eq!(credentials.argon2_verifications(), 1);
        assert_eq!(credentials.authenticate("wrong", loopback()).await, None);
    }

    #[tokio::test]
    async fn publishing_a_rotation_invalidates_the_old_token() {
        let credentials = Credentials::new(
            AdminCredential {
                credential_hash: hash_admin_token("old-token").unwrap(),
                credential_version: 1,
            },
            Arc::new(Throttle::new()),
        );
        assert_eq!(
            credentials.authenticate("old-token", loopback()).await,
            Some(1)
        );

        credentials.publish(AdminCredential {
            credential_hash: hash_admin_token("new-token").unwrap(),
            credential_version: 2,
        });

        assert_eq!(
            credentials.authenticate("old-token", loopback()).await,
            None
        );
        assert_eq!(
            credentials.authenticate("new-token", loopback()).await,
            Some(2)
        );
        assert_eq!(credentials.version(), 2);
    }
}
