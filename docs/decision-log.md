# Decision Log

This page documents all significant architectural and technical decisions made during the development of Warpbox, along with their context, rationale, and outcomes.

## D-001: Reject torbox-sdk-go

- **Date:** 2026-06-07
- **Context:** Need to integrate with TorBox API to list cached torrents and get CDN download URLs.
- **Decision:** Do not use the official `github.com/TorBox-App/torbox-sdk-go`.
- **Rationale:**
  - The `go.mod` declares `module torbox-sdk-go` (no GitHub path), causing Go toolchain rejection.
  - 10,000+ lines of auto-generated code for features we don't need (Usenet, RSS, Web Downloads, Integrations).
  - Uses `*float64` for IDs and sizes, requiring constant casting.
  - Wraps `net/http` in a custom REST client that can't be easily routed through our throttle queue.
- **Alternatives considered:** Hand-written client informed by OpenAPI spec; `oapi-codegen` generation.
- **Outcome:** SDK is unimportable until TorBox fixes the `go.mod` module path.

## D-002: Reject oapi-codegen for TorBox OpenAPI 3.1 spec

- **Date:** 2026-06-07
- **Context:** TorBox hosts an OpenAPI 3.1 spec at `https://api.torbox.app/openapi.json`. `oapi-codegen v2.7.1` was tested to generate a Go client automatically.
- **Decision:** Do not use `oapi-codegen` for this spec.
- **Rationale:**
  - TorBox spec uses OpenAPI 3.1 `anyOf: [null, <type>]` patterns extensively. `oapi-codegen` doesn't support 3.1 and throws errors on `anyOf: [null]`.
  - A Python downgrade script was tested (3.1 to 3.0) but generated code had duplicate symbol errors.
  - Manual fix-up would be fragile on spec updates.
- **Outcome:** Hand-written client is the correct approach for this API.

## D-003: Hand-written TorBox API client

- **Date:** 2026-06-07
- **Context:** Need to call `GET /v1/api/torrents/mylist` and `GET /v1/api/torrents/requestdl`.
- **Decision:** Write a thin, focused client in `internal/torbox/client.go` (~200 lines).
- **Rationale:**
  - We only need 2 of the ~50 available endpoints.
  - The official OpenAPI spec provides exact request/response shapes to model our types on.
  - Full control over error handling, HTTP client configuration, and context propagation.
  - No generated code bloat or dependency on fragile codegen tools.
  - Easy to test with a mock `http.RoundTripper`.
- **Key design:**
  - `do()` helper reads full response body, closes it, returns `[]byte` — no double-close bug.
  - `apiResponse[T]` generic wrapper matches TorBox's `{data, success, detail, error}` envelope.
  - `Torrent` and `TorrentFile` structs use `int64` for sizes/IDs (not `*float64` as in the SDK).

## D-004: Token auth asymmetry

- **Date:** 2026-06-07
- **Context:** TorBox API uses different auth mechanisms for different endpoints.
- **Decision:** Use Bearer header for `/mylist` and query parameter `token` for `/requestdl`.
- **Rationale:**
  - Discovered from the official OpenAPI spec: `/mylist` defines `security: [OAuth2PasswordBearer]`; `/requestdl` defines `token` as a required query parameter (no security scheme).
  - The SDK's `RequestDownloadLinkRequestParams` confirms the token is a query param.

## D-005: CGO dependency via mattn/go-sqlite3

- **Date:** 2026-06-07
- **Context:** SQLite WAL mode is required for persistent metadata storage.
- **Decision:** Use `github.com/mattn/go-sqlite3` (cgo-based).
- **Rationale:**
  - `mattn/go-sqlite3` is the de facto standard Go SQLite driver, uses CGO + SQLite amalgamation.
  - Pure-Go alternatives (modernc.org/sqlite) exist but lack WAL mode support guarantees.
  - MinGW-w64 GCC is available on the dev machine.
- **Trade-off:** Cross-compilation for non-Windows targets requires a C cross-compiler or a different driver.

## D-008: Exponential backoff + negative cache + circuit breaker for CDN URL fetches

- **Date:** 2026-06-11
- **Context:** Plex's ~2s retry loop on files with expired TorBox CDN URLs caused a death spiral: 500 errors → more API calls → TorBox abuse protection returns 429 → all calls fail.
- **Decision:** Implement three mitigation layers:
  1. **Exponential backoff + retry (1s, 2s, 4s)** for 5xx and 429 errors. 429s get a 30s backoff. Max 1 retry.
  2. **Negative cache** (30s TTL) mapping `(torrent_id, file_id)` → error. Subsequent Plex retries return the cached error without hitting the API.
  3. **Circuit breaker** per torrent: 5 failures in a 60s sliding window marks the torrent "stale" for 5 minutes.
- **Rationale:** The negative cache breaks Plex's retry loop at the application level. The circuit breaker prevents a single expired torrent from consuming all rate budget.
- **Thresholds:** retries=1, backoff=[1s], 429 backoff=30s, negative-cache TTL=30s, circuit-breaker=[5 failures, 60s window, 5min stale]
- **Issue:** #59

## D-013: "Slow disk" hang instead of error when CDN is unavailable

- **Date:** 2026-06-11
- **Context:** When TorBox CDN returns 500, warpbox returns 502 → rclone counts as error → after 10 errors rclone permanently kills the file → Plex trashes it.
- **Decision:** When CDN URL fetch fails, send `200 OK`/`206 Partial Content` headers immediately and hold the body stream open while polling for the CDN URL every 15 seconds.
- **Rationale:** Rclone only increments `errorCount` on actual errors. A slow-but-successful read resets the counter. Plex already buffers and shows a spinner for slow-starting streams.
- **Alternatives considered:** Changing 502 to 503, increasing rclone's `maxErrorCount`, removing negative cache, returning fake data.
- **Outcome:** Implementation in `internal/server/get.go` `handleGet`.
- **Issue:** #64

## D-018: Batch SQLite prune deletes + retry SetCDNURL to prevent "database is locked"

- **Date:** 2026-06-14
- **Context:** Production logs showed intermittent "database is locked" errors on `SetCDNURL` after CDN URL fetch. Two occurrences observed in a 24-hour window.
- **Decision:**
  1. Batch `PruneBySyncTag` into 250-row LIMIT subqueries instead of one bulk DELETE.
  2. Add retry loop in `SetCDNURL` (3 attempts, 100/200/400ms exponential backoff).
  3. Add duration timing (`slog.Debug`) to all write methods for observability.
  4. Add `db_lock_errors` atomic counter surfaced in periodic memory stats log.
- **Rationale:** `PruneBySyncTag` held the SQLite writer lock for the entire bulk DELETE. In WAL mode, `_busy_timeout=5000` gave concurrent `SetCDNURL` calls a 5-second wait, but large deletes could exceed that. Batching releases the lock between batches. Retry adds belt-and-suspenders. Diagnostics let us track remaining lock errors without grepping logs.
- **Implementation:** `internal/metadata/store.go` and `cmd/warpbox/main.go` — no schema changes, no new config keys.
- **Issue:** #100
- **Outcome:** All 19 metadata tests pass. Committed as 2442ec4.

## D-015: CDN proxy 429/5xx → hang/poll mode (extends D-013)

- **Date:** 2026-06-11
- **Context:** D-013 only covered CDN URL *fetch* failures. CDN data servers themselves also return 429 on concurrent chunk downloads targeting the same torrent.
- **Decision:** Route CDN data proxy 429/5xx responses into `handleGetCDNHang` instead of returning 502. Invalidate the cached CDN URL first so the hang loop polls for a fresh URL.
- **Rationale:** Same "slow spinning disk" pattern as D-013. Invalidating the cached URL gives the CDN time to drain connections.
- **Implementation:** `internal/server/get.go` lines 192-215.
- **Issue:** #64

## D-016: CDN connection semaphore + reduced default concurrency 8→4

- **Date:** 2026-06-11
- **Context:** TorBox CDN rate-limits per-torrent concurrent chunk downloads. Eight concurrent 32MB chunk downloads triggered CDN 429s.
- **Decision:** Implement a channel-based CDN connection semaphore and lower the default `MaxCDNConnections` from 8 to 4.
- **Rationale:** The semaphore guarantees we never exceed N concurrent CDN data connections. Configurable via `cache.max_cdn_connections` (valid range 1–64).
- **Alternatives considered:** Client-side rate limiting via token bucket — rejected because CDN throttles per-connection, not per-request.
- **Implementation:** `internal/server/server.go` — `cdnSem` field, `AcquireCDNConn()`/`ReleaseCDNConn()` methods.
- **Issue:** #64

## D-017: `debug.FreeOSMemory()` + pprof endpoint (INVALIDATED)

- **Date:** 2026-06-12 (invalidated 2026-06-15)
- **Context:** After 12 hours of runtime, Go's `runtime.MemStats.Sys` reported 1,684MB. This was misinterpreted as resident memory.
- **Decision (original):** Add `runtime/debug.FreeOSMemory()` to the periodic cleanup loop.
- **Invalidation Rationale:** `MemStats.Sys` is a **cumulative counter** of all bytes ever requested from the OS — it only grows and never decreases, even when Go returns pages. It does not represent actual RSS/working set. Real container memory usage is 47MB RSS, confirming `FreeOSMemory()` is unnecessary. The `pprof` endpoints at `/debug/pprof/` were already added (not related to this decision).
- **Key evidence:** Actual Docker stats show 47.2MB RSS, 0% CPU idle. `sys_mb` stats metric has been a flat 46MB for the entire 120-minute window. `alloc_mb` averages 3.8MB (live heap). The "20GB" value once observed across a 12GB host physically disproved the interpretation — a 12GB machine cannot allocate 20GB of RSS.
- **Implementation:** Never actually committed — `FreeOSMemory()` was never added to `startCleanupLoop()`. The code was correct without it.
- **Issue:** #64, #105
