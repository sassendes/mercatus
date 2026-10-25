mod scoring;

use axum::{
    extract::{DefaultBodyLimit, Request, State},
    http::{header, StatusCode},
    middleware::{self, Next},
    response::{IntoResponse, Response},
    routing::{get, post},
    Json, Router,
};
use scoring::{Decision, FraudEngine, TxContext};
use serde::{Deserialize, Serialize};
use std::sync::{Arc, Mutex};
use std::time::Duration;

#[derive(Clone)]
struct AppState {
    engine: Arc<Mutex<FraudEngine>>,
    token: Option<Arc<String>>,
}

#[derive(Deserialize)]
struct ScoreRequest {
    buyer_id: i64,
    amount_cents: i64,
    #[serde(default)]
    new_account: bool,
    #[serde(default)]
    billing_country: String,
    #[serde(default)]
    ip_country: String,
}

#[derive(Serialize)]
struct ScoreResponse {
    score: u32,
    decision: String,
    reasons: Vec<String>,
}

#[derive(Deserialize)]
struct BlockRequest {
    buyer_id: i64,
}

fn log(level: &str, msg: &str, fields: serde_json::Value) {
    let mut line = serde_json::json!({ "level": level, "msg": msg, "service": "fraud-engine" });
    if let (Some(obj), Some(extra)) = (line.as_object_mut(), fields.as_object()) {
        for (k, v) in extra {
            obj.insert(k.clone(), v.clone());
        }
    }
    println!("{}", line);
}

async fn health() -> impl IntoResponse {
    Json(serde_json::json!({ "status": "ok" }))
}

async fn score(State(st): State<AppState>, Json(req): Json<ScoreRequest>) -> Response {
    if req.buyer_id <= 0 || req.amount_cents < 0 {
        return (StatusCode::BAD_REQUEST, Json(serde_json::json!({ "error": "invalid request" }))).into_response();
    }
    let mut engine = st.engine.lock().unwrap_or_else(|e| e.into_inner());
    let tx = TxContext {
        buyer_id: req.buyer_id,
        amount_cents: req.amount_cents,
        new_account: req.new_account,
        billing_country: req.billing_country.chars().take(2).collect::<String>().to_uppercase(),
        ip_country: req.ip_country.chars().take(2).collect::<String>().to_uppercase(),
    };
    let r = engine.score(&tx);
    let decision = match r.decision {
        Decision::Approve => "approve",
        Decision::Review => "review",
        Decision::Decline => "decline",
    };
    if decision != "approve" {
        log("warn", "transaction flagged", serde_json::json!({ "buyer_id": req.buyer_id, "score": r.score, "decision": decision }));
    }
    Json(ScoreResponse { score: r.score, decision: decision.to_string(), reasons: r.reasons }).into_response()
}

async fn block(State(st): State<AppState>, Json(req): Json<BlockRequest>) -> impl IntoResponse {
    st.engine.lock().unwrap_or_else(|e| e.into_inner()).block(req.buyer_id);
    log("info", "buyer blocked", serde_json::json!({ "buyer_id": req.buyer_id }));
    Json(serde_json::json!({ "blocked": req.buyer_id }))
}

async fn unblock(State(st): State<AppState>, Json(req): Json<BlockRequest>) -> impl IntoResponse {
    st.engine.lock().unwrap_or_else(|e| e.into_inner()).unblock(req.buyer_id);
    Json(serde_json::json!({ "unblocked": req.buyer_id }))
}

async fn require_token(State(st): State<AppState>, req: Request, next: Next) -> Response {
    if let Some(token) = &st.token {
        let ok = req
            .headers()
            .get(header::AUTHORIZATION)
            .and_then(|v| v.to_str().ok())
            .and_then(|v| v.strip_prefix("Bearer "))
            .map(|v| constant_eq(v.as_bytes(), token.as_bytes()))
            .unwrap_or(false);
        if !ok {
            return (StatusCode::UNAUTHORIZED, Json(serde_json::json!({ "error": "unauthorized" }))).into_response();
        }
    }
    next.run(req).await
}

fn constant_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

async fn shutdown_signal() {
    let ctrl_c = async {
        let _ = tokio::signal::ctrl_c().await;
    };
    #[cfg(unix)]
    let term = async {
        if let Ok(mut s) = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            s.recv().await;
        }
    };
    #[cfg(not(unix))]
    let term = std::future::pending::<()>();
    tokio::select! {
        _ = ctrl_c => {},
        _ = term => {},
    }
    log("info", "shutdown signal received", serde_json::json!({}));
}

#[tokio::main]
async fn main() {
    let token = std::env::var("INTERNAL_TOKEN").ok().filter(|t| !t.is_empty()).map(Arc::new);
    let allow_open = std::env::var("ALLOW_NO_TOKEN").map(|v| v == "true").unwrap_or(false);
    if token.is_none() && !allow_open {
        log("error", "INTERNAL_TOKEN is required, set ALLOW_NO_TOKEN=true to run without auth", serde_json::json!({}));
        std::process::exit(1);
    }
    let state = AppState { engine: Arc::new(Mutex::new(FraudEngine::new())), token };

    let pruner = state.engine.clone();
    tokio::spawn(async move {
        let mut tick = tokio::time::interval(Duration::from_secs(60));
        loop {
            tick.tick().await;
            pruner.lock().unwrap_or_else(|e| e.into_inner()).prune();
        }
    });

    let protected = Router::new()
        .route("/score", post(score))
        .route("/block", post(block))
        .route("/unblock", post(unblock))
        .route_layer(middleware::from_fn_with_state(state.clone(), require_token));

    let app = Router::new()
        .route("/health", get(health))
        .route("/healthz", get(health))
        .route("/readyz", get(health))
        .merge(protected)
        .layer(DefaultBodyLimit::max(64 * 1024))
        .with_state(state);

    let port = std::env::var("PORT").unwrap_or_else(|_| "8093".to_string());
    let addr = format!("0.0.0.0:{}", port);
    let listener = match tokio::net::TcpListener::bind(&addr).await {
        Ok(l) => l,
        Err(e) => {
            log("error", "bind failed", serde_json::json!({ "addr": addr, "error": e.to_string() }));
            std::process::exit(1);
        }
    };
    log("info", "fraud-engine listening", serde_json::json!({ "addr": addr }));
    if let Err(e) = axum::serve(listener, app).with_graceful_shutdown(shutdown_signal()).await {
        log("error", "server error", serde_json::json!({ "error": e.to_string() }));
        std::process::exit(1);
    }
}
