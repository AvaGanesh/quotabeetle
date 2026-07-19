# api-ratelimiter

An API rate limiter / quota service backed by [TigerBeetle](https://tigerbeetle.com) as the ledger. Instead of counters in Redis, quota is money: each API key is an account, each request is a two-phase transfer, and TigerBeetle's single-writer-per-account consensus is what guarantees no double-spend under concurrency — no locking required on our side.

## How it works

- **Each API key is a TigerBeetle account**, flagged `DebitsMustNotExceedCredits`. Its account ID is `sha256(apiKey)[:16]`, so no separate key→ID mapping table is needed.
- **Quota grants** (`POST /v1/keys`) are transfers from a system "issuer" account into the key's account, crediting it.
- **Each request reserves capacity with a pending (two-phase) transfer** debiting the key's account and crediting a system "sink" account. TigerBeetle atomically rejects the reserve (`quota_exceeded`) if it would push debits past the account's granted credits — this check and the debit happen as one atomic operation on TigerBeetle's side, and TigerBeetle processes transfers against a given account strictly one at a time, so this holds even under heavy concurrent load with no double-spend.
- The reservation is **not** final. The caller does the downstream work, then:
  - **commits** it (`POST /v1/quota/reservations/{id}/commit`) to permanently spend the quota, or
  - **voids** it (`POST /v1/quota/reservations/{id}/void`) to release it back if the downstream work failed.
- Reservations also carry a **timeout** — if never settled, TigerBeetle auto-expires and releases them, so a crashed caller can't leak quota forever.

## Project layout

```
cmd/ratelimiter/       HTTP server entrypoint (main.go)
internal/ratelimit/    Ledger: wraps the TigerBeetle client with domain operations
                        (Bootstrap, Grant, Reserve, Commit, Void, GetBalance),
                        plus a reusable net/http Middleware for Go services
                        that want to embed quota checks directly.
internal/httpapi/      The standalone HTTP service — what other languages/services call.
scripts/                dev-tigerbeetle.sh: installs and runs a single-replica dev cluster.
test/                   Integration tests proving no double-spend under concurrency.
```

## Quick start

Requires Go 1.23+.

```bash
# 1. Install & start a single-replica TigerBeetle cluster (installs into ./.tigerbeetle)
scripts/dev-tigerbeetle.sh &

# 2. Run the service (connects to 127.0.0.1:3000 by default)
go run ./cmd/ratelimiter
```

Try it:

```bash
# Provision a key with 5 units of quota
curl -X POST localhost:8080/v1/keys \
  -H 'content-type: application/json' \
  -d '{"api_key":"demo","quota":5}'

# Reserve 1 unit (repeat 6x — the 6th is denied)
curl -X POST localhost:8080/v1/quota/reserve \
  -H 'content-type: application/json' \
  -d '{"api_key":"demo","cost":1}'
# -> {"allowed":true,"reservation_id":"...","timeout_seconds":60}

# Settle it once you know whether the downstream work succeeded
curl -X POST localhost:8080/v1/quota/reservations/<reservation_id>/commit   # spend it
curl -X POST localhost:8080/v1/quota/reservations/<reservation_id>/void    # give it back

curl localhost:8080/v1/keys/demo/balance
# -> {"api_key":"demo","granted":5,"used":...,"pending":...,"available":...}
```

To wipe local cluster state and start fresh: `scripts/dev-tigerbeetle.sh --reset`.

## HTTP API

| Method | Path | Body | Description |
|---|---|---|---|
| `POST` | `/v1/keys` | `{"api_key", "quota"}` | Provision a key, or top up its quota (additive) |
| `GET` | `/v1/keys/{apiKey}/balance` | – | `{granted, used, pending, available}` |
| `POST` | `/v1/quota/reserve` | `{"api_key", "cost"}` | Atomically check-and-decrement; `cost` defaults to 1 |
| `POST` | `/v1/quota/reservations/{id}/commit` | `{"amount"?}` | Finalize a reservation (optionally for less than reserved) |
| `POST` | `/v1/quota/reservations/{id}/void` | – | Release a reservation, restoring its quota |
| `GET` | `/healthz` | – | Liveness check |

Status codes: `reserve` returns `429` when quota is exhausted and `404` for an unknown key; `commit`/`void` return `404` if the reservation doesn't exist, `409` if it already expired or was already settled.

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

The dev script uses `TB_PORT` (default `3000`) and `TB_CLUSTER_ID` (default `0`) for the cluster it starts.

## Testing

```bash
# Unit tests (no TigerBeetle required)
go test ./...

# Integration tests: prove no double-spend under concurrency against a live cluster
scripts/dev-tigerbeetle.sh &
go test -tags=integration ./test/...
```

The integration suite:
- `TestConcurrentReserve_NoDoubleSpend` — fires 300 concurrent reservations against a 100-unit quota and asserts exactly 100 succeed.
- `TestConcurrentReserveCommitVoid_ReclaimsCapacity` — concurrently reserves, commits half, voids half, then fires another concurrent wave to prove exactly the voided capacity (and no more) is reclaimable.
