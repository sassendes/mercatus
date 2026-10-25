mod index;

use axum::{
    extract::{DefaultBodyLimit, Request, State},
    http::{header, StatusCode},
    middleware::{self, Next},
    response::{IntoResponse, Response},
    routing::{get, post},
    Json, Router,
};
use index::{Document, SearchIndex};
use serde::{Deserialize, Serialize};
use std::sync::{Arc, RwLock};

#[derive(Clone)]
struct AppState {
    idx: Arc<RwLock<SearchIndex>>,
    token: Option<Arc<String>>,
}

#[derive(Deserialize)]
struct IndexDoc {
    id: i64,
    title: String,
    #[serde(default)]
    body: String,
}

#[derive(Deserialize)]
struct ReplaceRequest {
    documents: Vec<IndexDoc>,
}

#[derive(Deserialize)]
struct DeleteRequest {
    id: i64,
}

#[derive(Deserialize)]
struct SearchQuery {
    q: String,
    #[serde(default = "default_limit")]
    limit: usize,
}

fn default_limit() -> usize {
    20
}

#[derive(Serialize)]
struct SearchHit {
    id: i64,
    title: String,
    score: f64,
}

#[derive(Serialize)]
struct SearchResponse {
    total_indexed: usize,
    hits: Vec<SearchHit>,
}

fn log(level: &str, msg: &str, fields: serde_json::Value) {
    let mut line = serde_json::json!({ "level": level, "msg": msg, "service": "search-engine" });
    if let (Some(obj), Some(extra)) = (line.as_object_mut(), fields.as_object()) {
        for (k, v) in extra {
            obj.insert(k.clone(), v.clone());
        }
    }
    println!("{}", line);
}

fn to_doc(d: IndexDoc) -> Document {
    Document { id: d.id, title: d.title.chars().take(500).collect(), body: d.body.chars().take(20_000).collect() }
}

async fn health() -> impl IntoResponse {
    Json(serde_json::json!({ "status": "ok" }))
}

async fn ready(State(st): State<AppState>) -> impl IntoResponse {
    let n = st.idx.read().unwrap_or_else(|e| e.into_inner()).len();
    Json(serde_json::json!({ "status": "ready", "documents": n }))
}

async fn index_doc(State(st): State<AppState>, Json(d): Json<IndexDoc>) -> impl IntoResponse {
    let mut idx = st.idx.write().unwrap_or_else(|e| e.into_inner());
    idx.add(to_doc(d));
    Json(serde_json::json!({ "indexed": true, "total": idx.len() }))
}

async fn replace(State(st): State<AppState>, Json(req): Json<ReplaceRequest>) -> impl IntoResponse {
    let mut fresh = SearchIndex::new();
    for d in req.documents {
        fresh.add(to_doc(d));
    }
    let total = fresh.len();
    *st.idx.write().unwrap_or_else(|e| e.into_inner()) = fresh;
    Json(serde_json::json!({ "replaced": true, "total": total }))
}

async fn delete_doc(State(st): State<AppState>, Json(req): Json<DeleteRequest>) -> impl IntoResponse {
    let removed = st.idx.write().unwrap_or_else(|e| e.into_inner()).remove(req.id);
    Json(serde_json::json!({ "removed": removed }))
}

async fn search(State(st): State<AppState>, Json(q): Json<SearchQuery>) -> impl IntoResponse {
    let idx = st.idx.read().unwrap_or_else(|e| e.into_inner());
    let query: String = q.q.chars().take(200).collect();
    let hits = idx
        .search(&query, q.limit.clamp(1, 1000))
        .into_iter()
        .map(|(doc, score)| SearchHit { id: doc.id, title: doc.title, score })
        .collect();
    Json(SearchResponse { total_indexed: idx.len(), hits })
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
    let state = AppState { idx: Arc::new(RwLock::new(SearchIndex::new())), token };

    let protected = Router::new()
        .route("/index", post(index_doc))
        .route("/index/replace", post(replace))
        .route("/index/delete", post(delete_doc))
        .route("/search", post(search))
        .route_layer(middleware::from_fn_with_state(state.clone(), require_token));

    let app = Router::new()
        .route("/health", get(health))
        .route("/healthz", get(health))
        .route("/readyz", get(ready))
        .merge(protected)
        .layer(DefaultBodyLimit::max(32 * 1024 * 1024))
        .with_state(state);

    let port = std::env::var("PORT").unwrap_or_else(|_| "8092".to_string());
    let addr = format!("0.0.0.0:{}", port);
    let listener = match tokio::net::TcpListener::bind(&addr).await {
        Ok(l) => l,
        Err(e) => {
            log("error", "bind failed", serde_json::json!({ "addr": addr, "error": e.to_string() }));
            std::process::exit(1);
        }
    };
    log("info", "search-engine listening", serde_json::json!({ "addr": addr }));
    if let Err(e) = axum::serve(listener, app).with_graceful_shutdown(shutdown_signal()).await {
        log("error", "server error", serde_json::json!({ "error": e.to_string() }));
        std::process::exit(1);
    }
}
