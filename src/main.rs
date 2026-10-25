//! WildToken — Rust rewrite on axum + Tokio.

use std::net::SocketAddr;
use std::process::ExitCode;
use std::sync::Arc;

use tokio::net::TcpListener;
use tracing_subscriber::EnvFilter;

use wildtoken::{config, routes, state};

async fn bind_address(host: &str, port: u16) -> Result<SocketAddr, String> {
    let mut addresses = tokio::net::lookup_host((host, port))
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

    let settings = match config::load() {
        Ok(settings) => settings,
        Err(error) => {
            tracing::error!(%error, "could not load configuration");
            return ExitCode::FAILURE;
        }
    };

    let state = match state::build(settings.clone()).await {
        Ok(state) => Arc::new(state),
        Err(error) => {
            tracing::error!(%error, "could not start WildToken");
            return ExitCode::FAILURE;
        }
    };

    let address = match bind_address(&settings.server.host, settings.server.port).await {
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

    let app = routes::router(state);
    match axum::serve(
        listener,
        app.into_make_service_with_connect_info::<SocketAddr>(),
    )
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
