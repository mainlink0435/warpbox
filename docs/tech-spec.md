# Technical Specification

This document is the authoritative technical specification for Warpbox.
Every claim here MUST match the current source code — any deviation is a bug.
If you change the code, update this spec.

## 1. Server Startup & Lifecycle

**Key files:** `cmd/warpbox/main.go`, `internal/server/server.go`

Signal handling (SIGINT/SIGTERM → context cancellation), config loading,
throttle queue init, sync worker init, HTTP server start, cleanup loop.

<!-- TODO: document startup sequence, signal handling, graceful shutdown -->

## 2. HTTP Request Pipeline

**Key files:** `internal/server/server.go`, `internal/server/propfind.go`,
`internal/server/get.go`, `internal/server/landing.go`

WebDAV routing via standard `http.ServeMux`. Routes: PROPFIND (directory
listing), GET (byte-range streaming with CDN fallback), HEAD, OPTIONS,
and the web UI (landing page, actions, logs, HTTP browser).

<!-- TODO: document route table, middleware chain, request flow -->

## 3. CDN Streaming & Fallback Chain

**Key files:** `internal/server/get.go`

Flow: try CDN URL → retry if stale → check negative cache → check circuit
breaker → hang/poll for "slow disk" mode. Byte-range support for video
seeking. CDN connection semaphore to limit concurrent downloads.

<!-- TODO: document retry policy, negative cache TTL, circuit breaker thresholds,
     CDN semaphore sizing -->

## 4. TorBox API Client

**Key files:** `internal/torbox/client.go`, `internal/throttle/queue.go`

Hand-written client wrapping `net/http`. Endpoints: ListTorrents,
ListUsenet, GetDownloadURL, GetUsenetDownloadURL, GetUserInfo. All calls
routed through the throttle queue. Generic `listGeneric[T]` response
parser with `apiResponse[T]` generic types.

<!-- TODO: document error handling, rate limiting, retry logic -->

## 5. Metadata Sync

**Key files:** `internal/metadata/sync.go`, `internal/metadata/store.go`

Periodic TorBox API → SQLite sync loop. Sync tags for tracking batches,
pruning stale records, upserting file metadata. Source disambiguation
(torrent vs usenet).

<!-- TODO: document sync interval, tag lifecycle, prune strategy -->

## 6. SQLite Store

**Key files:** `internal/metadata/store.go`, `internal/metadata/stats.go`

WAL-mode SQLite. Tables: `files` (file metadata), `meta` (key-value for
sync tags), CDN URL cache with TTL expiry. Time-series stats recording
(memory, sync duration, GC cycles, API responses).

<!-- TODO: document schema, indexes, migration strategy, CDN cache expiry -->

## 7. Stats & Observability

**Key files:** `internal/metadata/stats.go`, `internal/server/landing.go`,
`internal/server/logs.go`

Time-series stats table with periodic recording in the cleanup loop.
Chart.js frontend on the landing page. In-memory ring buffer for recent
log lines at `/logs/`. Structured logging via `log/slog` with runtime
level switching.

<!-- TODO: document stats schema, retention policy, chart data flow -->

## 8. Configuration

**Key files:** `internal/config/config.go`, `config.yml.example`

YAML config with 7 sections: API, cache, throttle, sync, stats, server,
logging. Validation on load. Defaults generated for all settings. Runtime
log level update via `/actions/loglevel`.

<!-- TODO: document config sections, validation rules, default values -->

## 9. Throttle Queue

**Key files:** `internal/throttle/queue.go`

Token-bucket rate limiter. Blocking enqueue (never fails fast). API call
counting (total, successful, failed). 429 response detection.

<!-- TODO: document rate calculation, queue depth, stress characteristics -->
