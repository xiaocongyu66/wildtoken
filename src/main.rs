//! WildToken — Rust rewrite on axum + Tokio.
//!
//! Milestone 0 serves the health endpoint only; the rest of the surface is
//! ported milestone by milestone.

use std::net::SocketAddr;
use std::process::ExitCode;

use axum::routing::get;
use axum::{Json, Router};
use serde_json::{json, Value};
use tokio::net::TcpListener;
use tracing_subscriber::EnvFilter;

const DEFAULT_HOST: &str = "127.0.0.1";
const DEFAULT_PORT: u16 = 3100;

fn app() -> Router {
    Router::new().route("/health", get(health))
}

async fn health() -> Json<Value> {
    Json(json!({"status": "ok", "service": "WildToken"}))
}

async fn bind_address() -> Result<SocketAddr, String> {
    let host = std::env::var("APP__SERVER__HOST").unwrap_or_else(|_| DEFAULT_HOST.to_string());
    let port = match std::env::var("APP__SERVER__PORT") {
        Ok(value) => value
            .parse::<u16>()
            .map_err(|_| format!("invalid APP__SERVER__PORT: {value}"))?,
        Err(_) => DEFAULT_PORT,
    };

    let mut addresses = tokio::net::lookup_host((host.as_str(), port))
        .await
        .map_err(|error| format!("could not resolve {host}:{port}: {error}"))?;
    addresses
        .next()
        .ok_or_else(|| format!("APP__SERVER__HOST did not resolve: {host}"))
}

async fn shutdown_signal() {
    let ctrl_c = async {
        let _ = tokio::signal::ctrl_c().await;
    };

    #[cfg(unix)]
    let terminate = async {
        use tokio::signal::unix::{signal, SignalKind};
        if let Ok(mut stream) = signal(SignalKind::terminate()) {
            stream.recv().await;
        }
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {},
        _ = terminate => {},
    }
}

#[tokio::main]
async fn main() -> ExitCode {
    let filter = std::env::var("WILDTOKEN_LOG").unwrap_or_else(|_| "info".to_string());
    tracing_subscriber::fmt()
        .with_env_filter(EnvFilter::new(filter))
        .init();

    let address = match bind_address().await {
        Ok(address) => address,
        Err(error) => {
            tracing::error!(%error, "could not resolve the listen address");
            return ExitCode::FAILURE;
        }
    };

    let listener = match TcpListener::bind(address).await {
        Ok(listener) => listener,
        Err(error) => {
            tracing::error!(%error, %address, "could not bind the listen address");
            return ExitCode::FAILURE;
        }
    };

    let local = listener.local_addr().unwrap_or(address);
    tracing::info!(%local, "WildToken starting");

    match axum::serve(listener, app())
        .with_graceful_shutdown(shutdown_signal())
        .await
    {
        Ok(()) => {
            tracing::info!("WildToken stopped");
            ExitCode::SUCCESS
        }
        Err(error) => {
            tracing::error!(%error, "server failed");
            ExitCode::FAILURE
        }
    }
}
