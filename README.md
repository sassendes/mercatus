# Mercatus

A multi-seller software marketplace (Amazon style) shipped as **one container image**. Sellers list apps, tools and license keys and deliver them to buyers themselves after payment. Out of the box the store only has one product: a $1 seller guide.

- `mercatus` is a single static Go binary. It runs every Go service in one process, serves the React storefront, and also carries the `migrate` and `seed` commands.
- `search-engine` (BM25 ranking) and `fraud-engine` (risk scoring) are static Rust binaries in the same image. Run them as separate containers from that image.

The only things it needs are a Postgres database and a few environment variables.

## Run it

```bash
docker build -t mercatus .

docker run --rm -e DATABASE_URL=... mercatus migrate
docker run --rm -e DATABASE_URL=... -e SEED_PASSWORD=demo-pass-123 mercatus seed
docker run --rm -p 8080:8080 -e DATABASE_URL=... -e JWT_SECRET=$(openssl rand -base64 48) mercatus serve
```

Open http://localhost:8080. With `docker compose up --build` you get Postgres, migrations, seed data, the app and both engines in one go.

Seeded accounts (password is `SEED_PASSWORD`, or a generated one printed in the seed log):

| Email | Role |
|---|---|
| admin@example.com | admin |
| buyer@example.com | buyer |
| store@example.com | seller (the official store that sells the guide) |

Seeding is idempotent. Running it again never duplicates rows or resets passwords.

## Kubernetes

`deploy/kubernetes` is a kustomize base: the app deployment runs `migrate` as an init container (safe with many replicas, it takes a Postgres advisory lock), plus the two engines, a seed Job, PDB, HPA and a NetworkPolicy that only lets the app talk to the engines.

```bash
cp deploy/kubernetes/secret.example.yaml secret.yaml   # fill it in, keep it out of git
kubectl apply -f secret.yaml
kubectl apply -k deploy/kubernetes
```

Point an Ingress at the `mercatus` service on port 80. Scrape metrics on port 9090.

## Commands

| Command | What it does |
|---|---|
| `mercatus serve [--only=auth,catalog,...]` | HTTP server. `--only` mounts a subset of modules so the same image can be split into separate deployments later. Refuses to start if migrations are pending. |
| `mercatus migrate` | Applies embedded `schema/*.sql` in order, tracked in `schema_migrations` with checksums. |
| `mercatus seed` | Inserts the admin, a demo buyer, the official store, software categories and the $1 seller guide. |
| `mercatus healthcheck` | Probes `/healthz` on the local server, for Docker healthchecks. |

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `DATABASE_URL` | required | Postgres DSN. Add `binary_parameters=yes` when connecting through PgBouncer in transaction mode, otherwise parameterized queries fail at random with `unnamed prepared statement does not exist` |
| `JWT_SECRET` | required | at least 32 characters |
| `PORT` | `8080` | |
| `METRICS_ADDR` | `:9090` | Prometheus `/metrics`, `off` to disable |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | `text` for local dev |
| `SEARCH_URL` | empty | e.g. `http://mercatus-search:8092`. Without it, search falls back to Postgres |
| `FRAUD_URL` | empty | e.g. `http://mercatus-fraud:8093`. Without it, checkout skips fraud scoring |
| `FRAUD_FAIL_OPEN` | `false` | approve orders when the fraud engine is unreachable instead of declining them |
| `INTERNAL_TOKEN` | empty | shared bearer token between the app and the engines. Required (16+ chars) when `SEARCH_URL` or `FRAUD_URL` is set. The engines refuse to start without it unless `ALLOW_NO_TOKEN=true` |
| `COUNTRY_HEADER` | empty | trusted ingress header carrying the client country (e.g. `CF-IPCountry`), passed to fraud scoring |
| `TRUST_PROXY_HEADERS` | `false` | set `true` behind an ingress so rate limits use the real client IP |
| `RATE_LIMIT_RPS` / `RATE_LIMIT_BURST` | `20` / `60` | per client IP, per pod |
| `CORS_ORIGINS` | empty | comma list, only needed if the API is called from another origin |
| `DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS` / `DB_CONN_MAX_LIFETIME` | `20` / `10` / `30m` | |
| `SHUTDOWN_DELAY` / `SHUTDOWN_TIMEOUT` | `5s` / `20s` | readiness flips to 503 first, then in-flight requests drain |
| `BACKGROUND_WORKERS` | `true` | reservation expiry, saga recovery, token pruning, search reindex |
| `S3_ENDPOINT`, `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_REGION`, `S3_PUBLIC_URL`, `S3_PATH_STYLE` | empty | enables real presigned uploads (works with Garage, MinIO, AWS) |
| `SEED_PASSWORD` | generated | password for seeded accounts |

## Layout

```
cmd/mercatus     the binary: wiring, server lifecycle, background jobs
platform/           config, database + migrations, logging, metrics (infrastructure only, no domain logic)
schema/             SQL migrations, embedded into the binary
seed/               idempotent demo data
web/                React + Tailwind storefront, embedded into the binary at build time
<service>/          auth catalog seller cart notification reviews inventory order payment media admin pipeline gateway
search-engine/      Rust BM25 index
fraud-engine/       Rust risk scoring
deploy/kubernetes/  kustomize base
```

Each service keeps its own `model / repository / service / handler / middleware` and never imports another service. Cross-service calls (checkout touching cart, orders, inventory, payments) are wired as plain functions in `cmd/mercatus/app.go`.

## Money and checkout

- Money is `int64` cents, commission is basis points (1000 = 10%).
- Balances are derived from the double-entry ledger. Every capture and refund writes rows that sum to zero; admins can check at `/api/payments/ledger/integrity`.
- Checkout is a saga: price the cart server side, create the order, fraud check, reserve stock atomically, charge, then commit stock and mark the order paid. Any failure refunds and releases in reverse order. Interrupted sagas are recovered by a background job. Checkout and payments are idempotent by key.

## Tests

```bash
make run-db                 # throwaway Postgres in Docker
make test                   # go test -race (each test gets its own schema) + cargo test
```

What the tests prove: no oversell with 20 concurrent reservations for 1 unit, multi-line reservations are all or nothing, 8 concurrent charges with one key create one payment, ledger nets to zero after charges and refunds, a declined card releases stock and cancels the order, seeding twice changes nothing, buyers cannot self-promote to admin, `alg=none` tokens are rejected, sellers cannot touch other stores' stock, and refresh token reuse kills the session.

## Notes

- Payments use a built-in test provider. Swap `TestStripeProvider` for a real Stripe client in `payment/service`.
- The search index and fraud velocity counters live in memory. Run one replica of each engine; the app rebuilds the search index on start and every `SEARCH_REINDEX_INTERVAL`.
- Rate limiting is per pod. Put a global limit at the ingress for hard guarantees.
