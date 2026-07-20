# quotabeetle

[![CI](https://github.com/AvaGanesh/quotabeetle/actions/workflows/ci.yml/badge.svg)](https://github.com/AvaGanesh/quotabeetle/actions/workflows/ci.yml)
[![Go Reference](https://img.shields.io/github/go-mod/go-version/AvaGanesh/quotabeetle)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/docker-compose%20up-2496ED?logo=docker&logoColor=white)](docker-compose.yml)

A token-bucket API rate limiter / quota service backed by [TigerBeetle](https://tigerbeetle.com) as the ledger. Instead of counters in Redis, quota is money: each API key is an account, each request is a two-phase transfer, and TigerBeetle's single-writer-per-account consensus is what guarantees no double-spend under concurrency — no locking required on our side.

- **Language/runtime agnostic** — it's a standalone HTTP service; call it from anything that can make an HTTP request.
- **Atomic under concurrency** — TigerBeetle serializes transfers per account, so no locks or Lua scripts are needed to avoid double-spend.
- **Two-phase by design** — reserve capacity up front, commit or void once you know the outcome, instead of guessing the cost before you've done the work.
- **Real token bucket** — a background refill loop replenishes each key at its own rate, capped at its own burst capacity.
- **Runs in one command** — `docker compose up -d` gets you the service, TigerBeetle, and formatting/healthchecks wired together.
- **Documented** — interactive API docs at `/docs` (OpenAPI 3, see [`internal/httpapi/openapi.yaml`](internal/httpapi/openapi.yaml)).

## How it works

- **Each API key is a TigerBeetle account**, flagged `DebitsMustNotExceedCredits`. Its account ID is `sha256(apiKey)[:16]`, so no separate key→ID mapping table is needed. Its bucket `capacity` and `refill_per_interval` are stored on the account itself (`UserData64`/`UserData32`) at creation time and are fixed thereafter.
- **A key's bucket starts full**: `POST /v1/keys` grants it `capacity` tokens immediately.
- **Each request reserves capacity with a pending (two-phase) transfer** debiting the key's account and crediting a system "sink" account. TigerBeetle atomically rejects the reserve (`quota_exceeded`) if it would push debits past the account's granted credits — this check and the debit happen as one atomic operation on TigerBeetle's side, and TigerBeetle processes transfers against a given account strictly one at a time, so this holds even under heavy concurrent load with no double-spend.
- The reservation is **not** final. The caller does the downstream work, then:
  - **commits** it (`POST /v1/quota/reservations/{id}/commit`) to permanently spend the tokens, or
  - **voids** it (`POST /v1/quota/reservations/{id}/void`) to release them back if the downstream work failed.
- Reservations also carry a **timeout** — if never settled, TigerBeetle auto-expires and releases them, so a crashed caller can't leak quota forever.
- **A background refill loop** (`Ledger.Refill`, driven by a ticker in `cmd/ratelimiter`) tops up every key's bucket by its `refill_per_interval` on each tick, capped at `capacity`. This is what turns a static balance into a real token bucket: a steady replenishment rate with bursts allowed up to capacity. Set `refill_per_interval: 0` for a plain static quota with no automatic replenishment.

Note on TigerBeetle accounting: `CreditsPosted` is a monotonically increasing total-ever-granted counter, not "tokens currently in the bucket" — it's already at `capacity` right after the initial full fill and never drops as the bucket is spent. The quantity that must stay ≤ `capacity` is the *available* balance (`CreditsPosted - DebitsPosted - DebitsPending`), so `Refill` measures headroom against that, not against `CreditsPosted` directly.

## Project layout

```
cmd/ratelimiter/       HTTP server entrypoint (main.go) + the background refill ticker
internal/ratelimit/    Ledger: wraps the TigerBeetle client with domain operations
                        (Bootstrap, CreateKey, Refill, Reserve, Commit, Void, GetBalance),
                        plus a reusable net/http Middleware for Go services
                        that want to embed quota checks directly.
internal/httpapi/      The standalone HTTP service — what other languages/services call.
                        openapi.yaml is the OpenAPI 3 spec, embedded into the
                        binary and served live at /openapi.yaml and /docs.
scripts/                dev-tigerbeetle.sh: installs and runs a single-replica dev cluster.
test/                   Integration tests proving no double-spend and correct refill behavior.
```

## Quick start

Requires Go 1.23+.

```bash
# 1. Install & start a single-replica TigerBeetle cluster (installs into ./.tigerbeetle)
scripts/dev-tigerbeetle.sh &

# 2. Run the service (connects to 127.0.0.1:3000 by default)
go run ./cmd/ratelimiter
```

Then open [`localhost:8080/docs`](http://localhost:8080/docs) for interactive API docs, or try it from the command line:

```bash
# Provision a key: bucket holds up to 5 tokens, refilling by 1 every tick (REFILL_INTERVAL, default 1s)
curl -X POST localhost:8080/v1/keys \
  -H 'content-type: application/json' \
  -d '{"api_key":"demo","capacity":5,"refill_per_interval":1}'

# Reserve 1 token (repeat 6x — the 6th is denied until a refill tick tops the bucket back up)
curl -X POST localhost:8080/v1/quota/reserve \
  -H 'content-type: application/json' \
  -d '{"api_key":"demo","cost":1}'
# -> {"allowed":true,"reservation_id":"...","timeout_seconds":60}

# Settle it once you know whether the downstream work succeeded
curl -X POST localhost:8080/v1/quota/reservations/<reservation_id>/commit   # spend it
curl -X POST localhost:8080/v1/quota/reservations/<reservation_id>/void    # give it back

curl localhost:8080/v1/keys/demo/balance
# -> {"api_key":"demo","capacity":5,"refill_per_interval":1,"tokens":...,"used":...,"pending":...,"available":...}
```

To wipe local cluster state and start fresh: `scripts/dev-tigerbeetle.sh --reset`.

### Or with Docker

```bash
docker compose up -d
```

This builds the service image, formats a single-replica TigerBeetle data file into a named volume (`tigerbeetle-format`, a one-shot init step that's a no-op on subsequent runs), starts TigerBeetle (`tigerbeetle`, healthchecked with `nc`), then starts the service (`ratelimiter`) once TigerBeetle is healthy. The API is on `localhost:8080`, TigerBeetle on `localhost:3000`.

Notes:
- The `Dockerfile` needs a C toolchain to build (`tigerbeetle-go` uses cgo to link TigerBeetle's prebuilt native client library), hence the `golang:1.23-bookworm` build stage rather than an Alpine one.
- TigerBeetle's client requires a literal IP, not a hostname — it rejects Compose service names like `tigerbeetle:3000` outright. `cmd/ratelimiter` resolves `TB_ADDRESS` via DNS before connecting, so both `TB_ADDRESS=tigerbeetle:3000` (Docker) and `TB_ADDRESS=127.0.0.1:3000` (local) work.
- `docker compose down -v` removes the TigerBeetle data volume; plain `down` preserves it across restarts.

## HTTP API

Full interactive docs (Swagger UI) are served by the running app at **`/docs`** — e.g. `http://localhost:8080/docs` — backed by the OpenAPI 3 spec at `/openapi.yaml`. The same spec lives in the repo at [`internal/httpapi/openapi.yaml`](internal/httpapi/openapi.yaml) if you want to import it into Postman/Insomnia or generate a client without running the service.

| Method | Path | Body | Description |
|---|---|---|---|
| `POST` | `/v1/keys` | `{"api_key", "capacity", "refill_per_interval"}` | Provision a key's bucket (starts full); a no-op if the key already exists with the same config |
| `GET` | `/v1/keys/{apiKey}/balance` | – | `{capacity, refill_per_interval, tokens, used, pending, available}` |
| `POST` | `/v1/quota/reserve` | `{"api_key", "cost"}` | Atomically check-and-decrement; `cost` defaults to 1 |
| `POST` | `/v1/quota/reservations/{id}/commit` | `{"amount"?}` | Finalize a reservation (optionally for less than reserved) |
| `POST` | `/v1/quota/reservations/{id}/void` | – | Release a reservation, restoring its tokens |
| `GET` | `/healthz` | – | Liveness check |
| `GET` | `/docs` / `/openapi.yaml` | – | Swagger UI / raw OpenAPI 3 spec |

Status codes: `reserve` returns `429` when the bucket is empty and `404` for an unknown key; `POST /v1/keys` returns `409` if the key already exists with a *different* `capacity`/`refill_per_interval` (both are immutable after creation); `commit`/`void` return `404` if the reservation doesn't exist, `409` if it already expired or was already settled.

**No built-in authentication** — `api_key` is a quota identifier, not a credential. This is meant to run on a trusted internal network (e.g. only reachable by other backend services); put a gateway or network policy in front of it for anything more exposed.

### Using it as a Go middleware

Services written in Go can skip the HTTP hop and embed `ratelimit.Middleware` directly in their own handler chain — it reserves before calling the wrapped handler and automatically commits on a non-5xx response or voids on a 5xx:

```go
ledger := ratelimit.New(tbClient, 30*time.Second)
handler := ratelimit.Middleware(ledger, 1 /* cost */, "X-API-Key")(myHandler)
```

## Configuration

Set via environment variables on the server:

| Variable | Default | Description |
|---|---|---|
| `TB_ADDRESS` | `127.0.0.1:3000` | TigerBeetle replica address |
| `TB_CLUSTER_ID` | `0` | TigerBeetle cluster ID |
| `PORT` | `8080` | HTTP listen port |
| `RESERVATION_TIMEOUT` | `60s` | How long a reservation may stay pending before auto-expiring |
| `REFILL_INTERVAL` | `1s` | How often the background loop tops up every key's bucket |

The dev script uses `TB_PORT` (default `3000`) and `TB_CLUSTER_ID` (default `0`) for the cluster it starts.

## Testing

```bash
# Unit tests (no TigerBeetle required)
go test ./...

# Integration tests: prove no double-spend and correct refill behavior against a live cluster
scripts/dev-tigerbeetle.sh &
go test -tags=integration ./test/...
```

The integration suite:
- `TestConcurrentReserve_NoDoubleSpend` — fires 300 concurrent reservations against a 100-token bucket and asserts exactly 100 succeed.
- `TestConcurrentReserveCommitVoid_ReclaimsCapacity` — concurrently reserves, commits half, voids half, then fires another concurrent wave to prove exactly the voided capacity (and no more) is reclaimable.
- `TestRefillReplenishesCapacity` — drains a bucket, calls `Refill`, and confirms the refilled tokens are spendable and that a subsequent refill on a partially-drained bucket caps at `capacity` rather than overshooting it.
