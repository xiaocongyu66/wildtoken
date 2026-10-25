//! The model-test prompt template table.

use rusqlite::{params, Connection};

use crate::db::Db;
use crate::error::ApiError;
use crate::models::settings::{ModelTestPromptTemplate, ModelTestPromptTemplateIn};

const COLUMNS: &str = "id, name, prompt, created_at, updated_at";

fn read(row: &rusqlite::Row<'_>) -> rusqlite::Result<ModelTestPromptTemplate> {
    Ok(ModelTestPromptTemplate {
        id: row.get(0)?,
        name: row.get(1)?,
        prompt: row.get(2)?,
        created_at: row.get(3)?,
        updated_at: row.get(4)?,
    })
}

pub async fn list_templates(db: &Db) -> Result<Vec<ModelTestPromptTemplate>, ApiError> {
    db.with_conn(|connection| {
        let query = format!("SELECT {COLUMNS} FROM model_test_prompt_templates ORDER BY id ASC");
        let mut statement = connection.prepare(&query).map_err(ApiError::database)?;
        let rows = statement.query_map([], read).map_err(ApiError::database)?;
        let mut templates = Vec::new();
        for row in rows {
            templates.push(row.map_err(ApiError::database)?);
        }
        Ok(templates)
    })
    .await
}

fn get_conn(connection: &Connection, id: i64) -> Result<ModelTestPromptTemplate, ApiError> {
    let query = format!("SELECT {COLUMNS} FROM model_test_prompt_templates WHERE id = ?1");
    connection
        .query_row(&query, params![id], read)
        .map_err(ApiError::database)
}

pub async fn create_template(
    db: &Db,
    input: ModelTestPromptTemplateIn,
) -> Result<ModelTestPromptTemplate, ApiError> {
    db.transaction(move |transaction| {
        transaction
            .execute(
                "INSERT INTO model_test_prompt_templates (name, prompt, created_at, updated_at)
                 VALUES (?1, ?2, datetime('now'), datetime('now'))",
                params![input.name.trim(), input.prompt],
            )
            .map_err(ApiError::database)?;
        get_conn(transaction, transaction.last_insert_rowid())
    })
    .await
}

pub async fn update_template(
    db: &Db,
    id: i64,
    input: ModelTestPromptTemplateIn,
) -> Result<Option<ModelTestPromptTemplate>, ApiError> {
    db.transaction(move |transaction| {
        let affected = transaction
            .execute(
                "UPDATE model_test_prompt_templates
                 SET name = ?1, prompt = ?2, updated_at = datetime('now')
                 WHERE id = ?3",
                params![input.name.trim(), input.prompt, id],
            )
            .map_err(ApiError::database)?;
        if affected == 0 {
            return Ok(None);
        }
        get_conn(transaction, id).map(Some)
    })
    .await
}

pub async fn delete_template(db: &Db, id: i64) -> Result<bool, ApiError> {
    db.with_conn(move |connection| {
        let affected = connection
            .execute(
                "DELETE FROM model_test_prompt_templates WHERE id = ?1",
                params![id],
            )
            .map_err(ApiError::database)?;
        Ok(affected == 1)
    })
    .await
}
