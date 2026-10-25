//! Groups scope which channels a downstream token may reach.

use rusqlite::{params, Connection};

use crate::db::Db;
use crate::error::ApiError;
use crate::models::group::{GroupIn, DEFAULT_GROUP_ID};

fn read_group(row: &rusqlite::Row<'_>) -> rusqlite::Result<(i64, String, String, String, String)> {
    Ok((
        row.get(0)?,
        row.get(1)?,
        row.get(2)?,
        row.get(3)?,
        row.get(4)?,
    ))
}

fn group_from(parts: (i64, String, String, String, String)) -> crate::models::group::Group {
    let (id, name, description, created_at, updated_at) = parts;
    crate::models::group::Group {
        id,
        name,
        description,
        created_at,
        updated_at,
        upstream_count: 0,
        token_count: 0,
        is_default: id == DEFAULT_GROUP_ID,
    }
}

pub async fn list_groups(db: &Db) -> Result<Vec<crate::models::group::Group>, ApiError> {
    db.with_conn(|connection| {
        let mut statement = connection
            .prepare(
                "SELECT g.id, g.name, g.description, g.created_at, g.updated_at,
                        (SELECT COUNT(*) FROM upstream_groups ug WHERE ug.group_id = g.id),
                        (SELECT COUNT(*) FROM api_tokens t WHERE t.group_id = g.id)
                 FROM groups AS g ORDER BY g.id ASC",
            )
            .map_err(ApiError::database)?;
        let rows = statement
            .query_map([], |row| {
                Ok(crate::models::group::Group {
                    id: row.get(0)?,
                    name: row.get(1)?,
                    description: row.get(2)?,
                    created_at: row.get(3)?,
                    updated_at: row.get(4)?,
                    upstream_count: row.get(5)?,
                    token_count: row.get(6)?,
                    is_default: row.get::<_, i64>(0)? == DEFAULT_GROUP_ID,
                })
            })
            .map_err(ApiError::database)?;

        let mut groups = Vec::new();
        for row in rows {
            groups.push(row.map_err(ApiError::database)?);
        }
        Ok(groups)
    })
    .await
}

fn get_conn(
    connection: &Connection,
    id: i64,
) -> Result<Option<crate::models::group::Group>, ApiError> {
    match connection.query_row(
        "SELECT id, name, description, created_at, updated_at FROM groups WHERE id = ?1",
        params![id],
        read_group,
    ) {
        Ok(parts) => Ok(Some(group_from(parts))),
        Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
        Err(error) => Err(ApiError::database(error)),
    }
}

pub async fn get_group(db: &Db, id: i64) -> Result<Option<crate::models::group::Group>, ApiError> {
    db.with_conn(|connection| get_conn(connection, id)).await
}

pub async fn create_group(
    db: &Db,
    input: &GroupIn,
) -> Result<crate::models::group::Group, ApiError> {
    db.transaction(|transaction| {
        transaction
            .execute(
                "INSERT INTO groups (name, description, created_at, updated_at)
                 VALUES (?1, ?2, datetime('now'), datetime('now'))",
                params![input.name.trim(), input.description.trim()],
            )
            .map_err(ApiError::database)?;
        let id = transaction.last_insert_rowid();
        get_conn(transaction, id)?.ok_or_else(|| ApiError::internal("group was not persisted"))
    })
    .await
}

pub async fn update_group(
    db: &Db,
    id: i64,
    input: &GroupIn,
) -> Result<Option<crate::models::group::Group>, ApiError> {
    db.transaction(|transaction| {
        let affected = transaction
            .execute(
                "UPDATE groups SET name = ?1, description = ?2, updated_at = datetime('now')
                 WHERE id = ?3",
                params![input.name.trim(), input.description.trim(), id],
            )
            .map_err(ApiError::database)?;
        if affected == 0 {
            return Ok(None);
        }
        get_conn(transaction, id)
    })
    .await
}

/// Removes a group, moving its tokens and last-home channels to the default.
pub async fn delete_group(db: &Db, id: i64) -> Result<bool, ApiError> {
    if id == DEFAULT_GROUP_ID {
        return Err(ApiError::bad_request("the default group cannot be deleted"));
    }

    db.transaction(|transaction| {
        transaction
            .execute(
                "UPDATE api_tokens SET group_id = ?1, updated_at = datetime('now')
                 WHERE group_id = ?2",
                params![DEFAULT_GROUP_ID, id],
            )
            .map_err(ApiError::database)?;

        transaction
            .execute(
                "INSERT OR IGNORE INTO upstream_groups (upstream_id, group_id)
                 SELECT upstream_id, ?1 FROM upstream_groups
                 WHERE group_id = ?2 AND upstream_id NOT IN (
                     SELECT upstream_id FROM upstream_groups WHERE group_id <> ?2
                 )",
                params![DEFAULT_GROUP_ID, id],
            )
            .map_err(ApiError::database)?;

        let affected = transaction
            .execute("DELETE FROM groups WHERE id = ?1", params![id])
            .map_err(ApiError::database)?;
        Ok(affected > 0)
    })
    .await
}
