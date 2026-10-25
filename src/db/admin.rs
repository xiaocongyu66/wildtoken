//! The singleton admin credential row.

use rusqlite::{params, Connection};

use crate::db::Db;
use crate::error::ApiError;
use crate::models::settings::AdminCredential;

fn load_conn(connection: &Connection) -> Result<Option<AdminCredential>, ApiError> {
    match connection.query_row(
        "SELECT credential_hash, credential_version FROM admin_credential WHERE id = 1",
        [],
        |row| {
            Ok(AdminCredential {
                credential_hash: row.get(0)?,
                credential_version: row.get(1)?,
            })
        },
    ) {
        Ok(credential) => Ok(Some(credential)),
        Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
        Err(error) => Err(ApiError::database(error)),
    }
}

pub async fn load_admin_credential(db: &Db) -> Result<Option<AdminCredential>, ApiError> {
    db.with_conn(|connection| load_conn(connection)).await
}

/// Inserts the bootstrap credential only when this database has never had one.
pub async fn bootstrap_admin_credential(
    db: &Db,
    bootstrap_hash: String,
) -> Result<AdminCredential, ApiError> {
    db.transaction(move |transaction| {
        if let Some(existing) = load_conn(transaction)? {
            return Ok(existing);
        }
        transaction
            .execute(
                "INSERT INTO admin_credential (id, credential_hash, credential_version)
                 VALUES (1, ?1, 1) ON CONFLICT(id) DO NOTHING",
                params![bootstrap_hash],
            )
            .map_err(ApiError::database)?;
        load_conn(transaction)?
            .ok_or_else(|| ApiError::internal("admin credential was not persisted"))
    })
    .await
}

/// Atomically replaces the credential if it is still at `expected_version`.
pub async fn rotate_admin_credential(
    db: &Db,
    replacement_hash: String,
    expected_version: i64,
) -> Result<Option<AdminCredential>, ApiError> {
    db.with_conn(move |connection| {
        match connection.query_row(
            "UPDATE admin_credential
             SET credential_hash = ?1, credential_version = credential_version + 1,
                 rotated_at = datetime('now')
             WHERE id = 1 AND credential_version = ?2
             RETURNING credential_hash, credential_version",
            params![replacement_hash, expected_version],
            |row| {
                Ok(AdminCredential {
                    credential_hash: row.get(0)?,
                    credential_version: row.get(1)?,
                })
            },
        ) {
            Ok(credential) => Ok(Some(credential)),
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
            Err(error) => Err(ApiError::database(error)),
        }
    })
    .await
}
