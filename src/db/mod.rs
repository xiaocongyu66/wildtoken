//! SQLite access with an explicit connection pool and `BEGIN IMMEDIATE`
//! transactions.

pub mod schema;
pub mod token;

use std::path::Path;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use rusqlite::{Connection, OpenFlags, Transaction, TransactionBehavior};
use tokio::sync::Semaphore;

use crate::config::DatabaseSettings;
use crate::error::ApiError;

pub struct Db {
    inner: Arc<Inner>,
}

struct Inner {
    path: String,
    cache_size_kib: i64,
    mmap_size_bytes: i64,
    idle: Mutex<Vec<IdleConnection>>,
    permits: Semaphore,
    idle_timeout: Duration,
}

struct IdleConnection {
    connection: Connection,
    since: Instant,
}

impl Db {
    pub fn open(settings: &DatabaseSettings) -> Result<Self, ApiError> {
        let path = database_path(&settings.url).map_err(ApiError::internal)?;
        restrict_database_files(&path);

        let idle_timeout = if settings.idle_timeout_seconds > 0 {
            Duration::from_secs(settings.idle_timeout_seconds as u64)
        } else {
            Duration::MAX
        };

        let db = Self {
            inner: Arc::new(Inner {
                path,
                cache_size_kib: settings.sqlite_cache_size_kib,
                mmap_size_bytes: settings.sqlite_mmap_size_bytes,
                idle: Mutex::new(Vec::new()),
                permits: Semaphore::new(settings.max_connections.max(1) as usize),
                idle_timeout,
            }),
        };

        // Fail at startup rather than on the first request.
        db.open_connection().map_err(ApiError::database)?;
        Ok(db)
    }

    pub fn path(&self) -> &str {
        &self.inner.path
    }

    /// Runs a closure on a pooled connection without a transaction.
    pub async fn with_conn<T, F>(&self, work: F) -> Result<T, ApiError>
    where
        T: Send + 'static,
        F: FnOnce(&mut Connection) -> Result<T, ApiError> + Send + 'static,
    {
        let _permit = self
            .inner
            .permits
            .acquire()
            .await
            .map_err(|_| ApiError::internal("database pool is closed"))?;

        let mut connection = self.take_connection().map_err(ApiError::database)?;

        let joined = tokio::task::spawn_blocking(move || {
            let result = work(&mut connection);
            (connection, result)
        })
        .await;

        match joined {
            Ok((connection, result)) => {
                self.put_connection(connection);
                result
            }
            Err(error) => Err(ApiError::internal(format!("database task failed: {error}"))),
        }
    }

    /// Runs a closure inside `BEGIN IMMEDIATE`.
    pub async fn transaction<T, F>(&self, work: F) -> Result<T, ApiError>
    where
        T: Send + 'static,
        F: FnOnce(&Transaction<'_>) -> Result<T, ApiError> + Send + 'static,
    {
        self.with_conn(move |connection| {
            let transaction = connection
                .transaction_with_behavior(TransactionBehavior::Immediate)
                .map_err(ApiError::database)?;
            let value = work(&transaction)?;
            transaction.commit().map_err(ApiError::database)?;
            Ok(value)
        })
        .await
    }

    /// Drops idle connections, for shutdown.
    pub fn close(&self) {
        let mut idle = self
            .inner
            .idle
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        idle.clear();
    }

    fn take_connection(&self) -> rusqlite::Result<Connection> {
        let mut idle = self
            .inner
            .idle
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        while let Some(entry) = idle.pop() {
            if entry.since.elapsed() <= self.inner.idle_timeout {
                return Ok(entry.connection);
            }
        }
        drop(idle);
        self.open_connection()
    }

    fn put_connection(&self, connection: Connection) {
        let mut idle = self
            .inner
            .idle
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        idle.push(IdleConnection {
            connection,
            since: Instant::now(),
        });
    }

    fn open_connection(&self) -> rusqlite::Result<Connection> {
        let connection = Connection::open_with_flags(&self.inner.path, OpenFlags::default())?;
        connection.busy_timeout(Duration::from_secs(5))?;
        let cache_size = self.inner.cache_size_kib.max(256);
        let mmap_size = self.inner.mmap_size_bytes.max(0);
        connection.execute_batch(&format!(
            "PRAGMA foreign_keys=ON;\n\
             PRAGMA auto_vacuum=2;\n\
             PRAGMA cache_size=-{cache_size};\n\
             PRAGMA mmap_size={mmap_size};"
        ))?;
        Ok(connection)
    }
}

/// Extracts the file path from `sqlite:` / `sqlite://` URLs.
pub fn database_path(url: &str) -> Result<String, String> {
    let raw = url
        .strip_prefix("sqlite://")
        .or_else(|| url.strip_prefix("sqlite:"))
        .unwrap_or(url);
    let path = raw.split('?').next().unwrap_or("");
    if path.is_empty() {
        return Err("database url is empty".to_string());
    }
    Ok(path.to_string())
}

/// Keeps the database readable by its owner alone.
pub fn restrict_database_files(path: &str) {
    if path.is_empty() || path.starts_with(":memory:") || path.starts_with("file:") {
        return;
    }

    #[cfg(unix)]
    {
        use std::fs::OpenOptions;
        use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};

        if let Ok(file) = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .open(path)
        {
            drop(file);
        }

        for name in [
            path.to_string(),
            format!("{path}-wal"),
            format!("{path}-shm"),
            format!("{path}-journal"),
        ] {
            if let Ok(metadata) = std::fs::metadata(&name) {
                let mut permissions = metadata.permissions();
                if permissions.mode() & 0o077 != 0 {
                    permissions.set_mode(0o600);
                    let _ = std::fs::set_permissions(&name, permissions);
                }
            }
        }
    }

    #[cfg(not(unix))]
    {
        let _ = path;
    }
}

/// Opens a standalone connection for tests and one-off work.
pub fn open_standalone(path: &Path) -> rusqlite::Result<Connection> {
    let connection = Connection::open(path)?;
    connection.busy_timeout(Duration::from_secs(5))?;
    connection.execute_batch(
        "PRAGMA foreign_keys=ON;\n\
         PRAGMA auto_vacuum=2;\n\
         PRAGMA cache_size=-2048;",
    )?;
    Ok(connection)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn database_urls_strip_the_scheme_and_query() {
        assert_eq!(
            database_path("sqlite:wildtoken.db?mode=rwc").unwrap(),
            "wildtoken.db"
        );
        assert_eq!(
            database_path("sqlite:///var/lib/wildtoken.db").unwrap(),
            "/var/lib/wildtoken.db"
        );
        assert_eq!(
            database_path("wildtoken.db?mode=ro").unwrap(),
            "wildtoken.db"
        );
        assert!(database_path("sqlite:").is_err());
        assert!(database_path("").is_err());
    }

    #[tokio::test]
    async fn transactions_are_immediate_and_roll_back_on_error() {
        let dir = tempfile::tempdir().unwrap();
        let settings = DatabaseSettings {
            url: format!("sqlite:{}?mode=rwc", dir.path().join("db.sqlite").display()),
            ..DatabaseSettings::default()
        };
        let db = Db::open(&settings).unwrap();

        db.with_conn(|connection| {
            schema::init(connection)?;
            Ok(())
        })
        .await
        .unwrap();

        let rolled_back: Result<(), ApiError> = db
            .transaction(|transaction| {
                transaction
                    .execute(
                        "INSERT INTO groups (name, description) VALUES ('x', '')",
                        [],
                    )
                    .map_err(ApiError::database)?;
                Err(ApiError::bad_request("roll back"))
            })
            .await;
        rolled_back.unwrap_err();

        let count: i64 = db
            .with_conn(|connection| {
                connection
                    .query_row("SELECT COUNT(*) FROM groups WHERE name = 'x'", [], |row| {
                        row.get(0)
                    })
                    .map_err(ApiError::database)
            })
            .await
            .unwrap();
        assert_eq!(count, 0);
    }
}
