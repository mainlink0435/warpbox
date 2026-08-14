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

## D-015b: CDN hang/poll 429 exponential backoff (extends D-015)

- **Date:** 2026-06-26
- **Context:** `handleGetCDNHang` polled at a fixed 15-second interval. When
  TorBox rate-limited `requestdl` per-item, the fixed polling created a death
  spiral — calling every 15s kept the per-item limit from ever resetting.
- **Decision:** Add exponential backoff inside the hang/poll loop. On each
  429 response from `fetchCDNURL()`, double the poll interval
  (15s → 30s → 60s → 2min → 5min max). Non-429 failures keep the current
  interval. Uses `time.After(interval)` instead of a fixed `time.Ticker`.
- **Rationale:** Items stuck in a 429 loop back off to 5-minute intervals,
  reducing API calls per item from ~2/min to ~0.4/min. Items that succeed on
  the first poll attempt are unaffected (interval stays at 15s).
- **Implementation:** `internal/server/get.go` — `handleGetCDNHang` poll loop.
- **Issue:** (none — discovered during deployment log review)

## D-015c: CDN hang/poll data retry after URL recovery (extends D-015/b)

- **Date:** 2026-07-16
- **Context:** D-015 routed CDN data 429/5xx into the hang/poll loop. However,
  after CDN URL recovery, the data proxy was a one-shot operation — any
  transient error from the CDN data server (429, 5xx, disguised text body)
  would stream the error page as file content into rclone's VFS cache,
  permanently corrupting the cached copy. Plex's thumbnail probes and
  hover-play on multi-file torrents could trigger sub-second 429 → URL
  recovery → 429 thrash loops.
- **Decision:** Extend `handleGetCDNHang` so that after obtaining a CDN URL,
  the data proxy itself is also inside the retry loop. Transient data errors
  (429, 5xx, or 200/206 with `text/*`/`html`/`json` Content-Type) invalidate
  the cached URL, apply exponential backoff (binary‑doubled to 5‑minute max),
  and loop back to re‑fetch a fresh URL. Only a valid binary 200/206 response
  is streamed to the client.
- **Rationale:** Matches the original D-013 "slow spinning disk" intent even
  when the bottleneck is CDN data rather than requestdl rate limits. The
  disguised-content-type detection (`isCDNDisguisedErrorBody`) prevents error
  pages from being cached as file data.
- **Implementation:** `internal/server/get.go` — `handleGetCDNHang`, `isCDNDisguisedErrorBody`.
- **Issue:** (context.txt Widow's Bay / explorer thumbnail)

## D-016: CDN connection semaphore + reduced default concurrency 8→4

- **Date:** 2026-06-11
- **Updated:** 2026-07-16 — semaphore acquired *before* `client.Do` (not after),
  so `max_cdn_connections` limits concurrent upstream CDN opens, not just
  concurrent streams. Each error path now explicitly releases the slot before
  returning. Implementation: `internal/server/get.go` — `streamFileContent`,
  `handleGetCDNHang`.
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

## D-019: Optional HTTP Basic Authentication for web UI

- **Date:** 2026-06-15
- **Context:** The web management UI (landing page, logs, actions, stats, HTTP browser) was accessible to anyone on the network without authentication. Users hosting warpbox on shared or semi-public networks needed a way to restrict management access.
- **Decision:** Add an optional `auth` config section with `enabled`, `username`, and `password` keys. When enabled, HTTP Basic Authentication is enforced on browser-facing routes. WebDAV and Infuse routes are deliberately excluded because they are consumed by rclone/Plex (which do not support interactive auth prompts). The `/healthz` endpoint is also excluded for Docker healthchecks.
- **Rationale:** Basic Auth is simple, built into Go's `net/http`, and requires no external dependencies. Plaintext password storage matches the existing TorBox API key pattern — the config file is already protected by filesystem permissions. Constant-time comparison (`subtle.ConstantTimeCompare`) prevents timing attacks on the middleware.
- **Alternatives considered:** bcrypt-hashed passwords (rejected — overkill for a LAN-side UI); Bearer token (rejected — adds header management complexity); protecting WebDAV routes (rejected — would break rclone/Plex integrations).
- **Implementation:** `internal/config/config.go` (AuthConfig struct, defaults, validation), `internal/server/auth.go` (requireAuth middleware), `internal/server/server.go` (route wrapping), `internal/server/landing.go`/`landing.html` (auth status indicator), `cmd/warpbox/main.go` (wiring).
- **Issue:** #79

## D-020: Replace worktree-based AI workflow with clone-based workflow

- **Date:** 2026-06-16
- **Context:** The AI instructions (maintained on the `internal` branch) instructed opencode to use git worktrees (`git worktree add`) for isolated issue development. In practice, two problems emerged:
  1. `opencode --dir` does not exist as a TUI flag — the agent could not launch itself into a worktree directory.
  2. Git worktrees share the same `.git` and `.opencode/` directory as the main repo, so all opencode sessions are global across all worktrees. Four parallel worktrees on four issues produce session collisions.
- **Decision:** Use full clones (`git clone . ../warpbox-issue-NN`) instead of worktrees.
- **Rationale:** Each clone has its own `.opencode/` directory and independent session pool. The user still must manually `cd` into the clone and start a new opencode session (the agent cannot do this), but sessions for different issues do not interfere.
- **Alternatives considered:** Fixing worktrees by symlinking `.opencode/` per worktree (brittle, no community precedent); staying with worktrees and accepting session collision (confusing); documenting `cd` as the only manual step (accepted).
- **Outcome:** Internal instructions updated from "Worktree Lifecycle" to "Clone Lifecycle".

## D-021: Extend retry, IsRetryable(), and negative cache for sync and CDN proxy

- **Date:** 2026-06-20
- **Context:** Production logs showed three failure modes during TorBox API outages:
  1. Sync failures on 502/timeout/HTML responses with zero retry — stale metadata for 5+ minutes between sync intervals.
  2. Cloudflare error pages returned with HTTP 200 produced cryptic JSON parse errors with no body visibility in logs.
  3. CDN 403/404 after repair exhaustion triggered infinite Plex retry storms (120+ API calls for one file in ~45 seconds).
- **Decision:**
  1. Add exponential backoff retry to metadata sync (`ListTorrents`/`ListUsenet`) with configurable `sync.retry_attempts` (default 3) and `sync.retry_backoff` (default 1s).
  2. Extract unified `IsRetryable()` function used by both CDN fetch and sync retry — single source of truth for what constitutes a transient error.
  3. Add HTML body logging on JSON unmarshal failure — WARN with 200-char preview, DEBUG with full truncated body.
  4. Cache CDN 403/404 failures in the negative cache after all repair attempts are exhausted, so subsequent Plex retries skip the API entirely.
- **Rationale:** Extends the existing D-008 retry/backoff/negative-cache pattern to (a) the sync path and (b) the CDN proxy path. The unified `IsRetryable()` prevents retry-logic drift between the two call sites.
- **Config:** `sync.retry_backoff` (1–60, default 1), `sync.retry_attempts` (0–10, default 3). Existing `negative_cache_ttl_seconds` and `cdn_url_retry_*` keys are reused.
- **Implementation:** `internal/torbox/client.go` (IsRetryable, HTML logging), `internal/metadata/sync.go` (retry), `internal/server/get.go` (CDN 404 negative cache).

## D-022: Store duplicates by (source, item_id, file_id) instead of overwriting on path collision

- **Date:** 2026-07-09
- **Context:** A user added the same usenet download twice by scripting error. TorBox returned two separate items with different hashes but identical directory structures. The previous UNIQUE(path) constraint caused the second item to silently overwrite the first, losing the alternative item_id/file_id needed for CDN URL fallback. If the overwriting item was later deleted from TorBox, streaming would fail until the next sync.
- **Decision:** Remove UNIQUE(path) and enforce uniqueness on (source, item_id, file_id) — the natural TorBox key. Duplicate virtual paths (same path from different items) are preserved as separate rows. Deduplication happens at the query/display layer:
  - `GetFileByPath` returns the highest-internal-id row (last upserted).
  - `ListDir` deduplicates by path using MAX(id) subquery.
  - `GetFileAlternatives` returns non-primary rows for CDN fallback.
- **CDN fallback:** When the primary item's `requestdl` fails, `tryCDNFallback` queries alternatives and tries each. The fallback is read-only — no DB writes. On next sync, deleted items are pruned naturally by `PruneBySyncTag`.
- **Schema upgrade:** v1 databases are detected by inspecting the `CREATE TABLE` text for `path TEXT NOT NULL UNIQUE`. The database file is deleted and recreated automatically. This is a one-way upgrade — downgrading requires deleting `warpbox.db` and re-syncing.
- **Rationale:** The database is a cache derived from the TorBox API — it self-heals on the next sync cycle. A destructive schema upgrade is simpler and less risky than a table copy/rename migration. Since the DB holds no unique data (everything comes from the API), auto-recreate is safe.
- **Config impact:** None. The landing page now shows both total and unique file counts.
- **Implementation:** `internal/metadata/store.go` (schema, queries, migration), `internal/server/get.go` (CDN fallback), `internal/server/landing.go` (dual counts), `internal/tools/dbinspect/main.go` (updated checks).

## D-023: Percent-encode WebDAV D:href and HTTP browser links

- **Date:** 2026-07-16
- **Context:** Filenames containing a literal `%` (e.g. "30% Iron Chef") produced
  invalid URL references in PROPFIND `D:href` responses. Rclone parsed these as
  malformed percent-escape sequences (`"% o"` is not a valid hex pair) and
  failed with `invalid URL escape`, making those files completely inaccessible
  through the rclone mount.
- **Decision:** Add `encodeDAVHref()` which splits each path on `/`,
  percent-encodes every segment via `url.PathEscape`, then joins with `/`.
  Trailing slashes are preserved. Stored SQLite paths and `DisplayName` values
  remain unencoded — only the wire-format `D:href` and HTTP browser `href`
  attributes are encoded.
- **Rationale:** Segment-level `url.PathEscape` is the correct encoding for
  URL path segments per RFC 3986. `url.QueryEscape` would encode `/`, and
  raw string concatenation would allow invalid URL characters through. The
  unencoded `seen` map ensures deduplication works on logical paths, not
  their encoded form.
- **Implementation:** `internal/server/propfind.go` (`encodeDAVHref`, `appendResponse`), `internal/server/http_browser.go` (10 call sites).
- **Issue:** #5

## D-024: Per-virtual-path `min_file_size` / `max_file_size`

- **Date:** 2026-07-17
- **Context:** Rclone's `--min-size`/`--max-size` flags apply to the entire
  mount. With multiple virtual paths (movies, tv, anime) on a single mount,
  there was no way to apply different size policies per view. Clients that
  bypass rclone entirely (Infuse WebDAV, HTTP browser) had no size filtering
  at all.
- **Decision:** Add optional `min_file_size` / `max_file_size` string fields
  to each `virtual_paths` entry. Values are human-readable with binary units
  (e.g. `300MB`, `1.5GB`, `10GB`). Parsed by `ParseFileSize()` into `int64`
  bytes at config load and validated (min ≤ max). Applied in `Filter.Apply()`
  after name/regex filters and before `largest_file_only`. Zero means no bound
  (unlimited).
- **Rationale:** Human-readable strings avoid unit errors while keeping config
  files readable. Binary (1024) multipliers match media tooling conventions.
  Applying size bounds after name filtering stays consistent with the existing
  filter pipeline ordering.
- **Alternatives considered:** Raw bytes (error-prone); decimal units (would
  misalign with file sizes reported by other tools).
- **Implementation:** `internal/library/size.go` (`ParseFileSize`), `internal/library/filter.go` (`MinSize`, `MaxSize`, `MatchSize`), `internal/config/config.go` (fields + validation), `internal/server/server.go` (`buildFilters` wiring).

## D-025: Harden metadata sync against transient TorBox usenet list degradation

- **Date:** 2026-08-04
- **Context:** The usenet portion of the library stopped syncing — every cycle
  logged `metadata sync: usenet failed error=torbox: reading response: stream
  error: stream ID N; INTERNAL_ERROR; received from peer` after ~15s. Torrents
  (941 items) always worked; only usenet failed. A live diagnosis against
  `GET /v1/api/usenet/mylist` showed the endpoint was erratic: pages ≤ ~1500
  items returned complete JSON (taking 30–47s, occasionally much faster), while
  `limit` ≥ 2000 consistently truncated/failed around 15s (HTTP/2
  `INTERNAL_ERROR`, partial JSON). This is the signature of an overloaded,
  degraded server rather than a permanent limit: the OpenAPI spec (default
  `limit=1000`, no documented maximum) describes the usenet and torrents
  endpoints identically, and a day later the same `limit=5000` request returned
  all 4,380 usenet items in one valid response. In short, the endpoint was
  **transiently degraded**, and warpbox's settings amplified the impact.
- **Decisions:**
  1. **Keep `sync.list_page_size` at its 5000 default.** The truncation was a
     transient degradation, not a permanent cap, so the default stays 5000
     (fewer API calls). If usenet sync fails with `INTERNAL_ERROR` again during
     a degradation window, lower the page size (e.g. 1000) as a workaround until
     it recovers — documented in `config.yml.example`.
  2. **Prune per source — never on a failed fetch.** `PruneBySyncTag` now takes
     a `FileSource`; `syncOnce` prunes a source only when that source's fetch
     succeeded. A failed fetch is not a definitive answer ("the items are gone")
     — it is a transport/API failure, so that source's previously-synced rows
     are kept. Previously the gate `torRes.err == nil || usenetRes.err == nil`
     ran the prune when *either* source succeeded, silently wiping the failed
     source's rows from the store (and firing spurious `OnItemsRemoved` hooks).
  3. **Retry at page granularity.** Retry logic moved from wrapping the whole
     `ListTorrents`/`ListUsenet` call into the pagination loop itself, so a
     transient failure on one page retries that same `offset` in place instead
     of aborting the entire list and restarting from offset 0.
  4. **In-flight sync guard.** A periodic tick or manual resync that arrives
     while a sync is running is skipped with a log line (`atomic.Bool` in
     `syncOnce`). Periodic syncs were already serialized by the ticker loop,
     but a manual resync (HTTP handler goroutine) could race a periodic sync
     and double-prune; the guard closes that window for all paths.
  5. **Propagate the sync context to HTTP requests.** The throttle queue passes
     its *own* context to `Execute`, so a manual resync timeout or shutdown
     cancellation never reached in-flight requests. The sync closures now
     capture the sync's context.
  6. **Make the timeout values configurable.** `torbox.request_timeout_seconds`
     (default 90, replacing a hardcoded 30 that was too tight for slow usenet
     pages), `sync.sync_timeout_seconds` (default 0 = no cap, replacing a
     hardcoded 60 that couldn't finish a large paginated usenet sync),
     `cache.cdn_proxy_timeout_seconds` (default 30), and
     `cache.cdn_url_429_backoff_seconds` (default 30) — the last two replacing
     hardcoded 30s values in the CDN proxy path.
  7. **Leave `IsRetryable` unchanged.** Retrying API *delay* errors (the
     HTTP/2 stream abort class) was deliberately not added: retrying those
     could stretch a sync substantially, and `IsRetryable` is intended for 429
     and transient status errors. Parked for later consideration.
- **Rationale:** The durable value of this change is not the page size — it is
  that a degraded usenet endpoint can no longer wipe previously-synced rows
  (per-source prune), force wasted full-list restarts (page-level retry), or
  race a manual resync (in-flight guard), and that the timeouts are
  configurable. When TorBox's usenet endpoint misbehaves, warpbox now fails
  gracefully — keeping the last-known-good data and retrying in place — instead
  of destroying the usenet library.
- **Config:** `sync.list_page_size` (default 5000), `torbox.request_timeout_seconds`, `sync.sync_timeout_seconds`, `cache.cdn_proxy_timeout_seconds`, `cache.cdn_url_429_backoff_seconds`.
- **Implementation:** `internal/config/config.go`, `internal/torbox/client.go` (page retry, `SetTimeout`), `internal/metadata/sync.go` (guard, per-source prune, ctx propagation, `SyncNow`), `internal/metadata/store.go` (`PruneBySyncTag(tag, source)`), `internal/server/get.go` + `internal/server/server.go` (CDN timeouts), `cmd/warpbox/main.go`, plus tests and docs.

## D-026: Quarantine escalation + global health gate + failed-links UI

- **Date:** 2026-08-10
- **Context:** An 8-hour log window showed a single orphaned TorBox torrent (item
  70658216, `server=0` — a cache entry stranded on a dead node) making
  `GET /v1/api/torrents/requestdl` return HTTP 500 `DATABASE_ERROR` persistently,
  while every other torrent and usenet file succeeded. Warpbox's circuit breaker
  (D-008) tripped, auto-reset, and re-hammered the item every ~6 minutes for
  hours (~1800 wasted requestdl calls). The item itself is unfixable from warpbox
  — it needs to be removed and re-added at TorBox — but warpbox's reaction was
  not ideal: it kept retrying indefinitely, logged no clear remediation, and
  exposed nothing in the UI.
- **Decisions:**
  1. **Surface API error codes.** `torbox` non-200 responses now include the
     envelope's `error` field (e.g. `torbox: unexpected status 500
     (DATABASE_ERROR)`, `(BAD_TOKEN)`), so callers can classify failures.
  2. **Quarantine escalation (half-open).** Item-scoped failures escalate the
     stale window (`5m → 10m → 20m → …`) capped by the new
     `circuit_breaker_max_stale_minutes` (default 60), so a permanently-broken
     item settles to ~1 probe/hour instead of hammering. After each window, one
     probe is allowed; success clears the tracker, failure re-escalates.
  3. **Global health gate.** A rolling window of requestdl outcomes flags a
     transient TorBox-wide outage (`globalDegraded()`). During it, item failures
     are treated as transient and never escalated or labelled "dead", so a
     global flap cannot falsely quarantine healthy items.
  4. **Classification.** `auth` (`BAD_TOKEN`) failures never quarantine (they
     are account-level and surfaced by the sync error); `transient`
     (429/network/HTML) failures trip the base window only.
  5. **Sync-driven pruning.** After each successful sync,
     `PruneBreakerForMissingItems` drops breaker/negative-cache entries for item
     ids no longer in the store, so a remove-and-re-add heals within one sync
     interval. Quarantine stays keyed by item id only — never hash/path — so a
     re-added torrent (new id) never inherits the old item's dead state.
  6. **Failed-links UI.** The landing page gains a "Quarantined items (failed
     links)" table with per-error-code guidance, and the stale-skip log was
     demoted from WARN to Debug to stop the log flood.
- **Rationale:** The 8-hour storm was the failure mode of a *permanently* broken
  item — a case the D-008 breaker was never designed for (it was built for
  recoverable expired torrents). Escalation + the global-health gate distinguish
  "this item is dead" from "the API is having a moment", and the UI gives the
  user an actionable list instead of a log wall.
- **Config:** `cache.circuit_breaker_max_stale_minutes` (default 60, 5–1440).
- **Implementation:** `internal/torbox/client.go`, `internal/server/get.go`,
  `internal/server/server.go`, `internal/metadata/sync.go` (`OnSyncDone`),
  `internal/metadata/store.go` (`GetFileLabelByItemID`), `internal/server/landing.go`/`landing.html`, `internal/config/config.go`, `cmd/warpbox/main.go`, plus tests and docs.

## D-027: CDN 416 Range Not Satisfiable → size-mismatch correction

- **Date:** 2026-08-10
- **Context:** Some files (e.g. obfuscated releases) returned HTTP 416 from the
  CDN because the CDN's actual cached file is smaller than warpbox's recorded
  size. Warpbox returned 502 (which rclone counts toward `maxErrorCount=10`,
  risking the file being trashed from the mount) and never invalidated the
  cached URL, so the player hammered the same dead URL for hours. The CDN's 416
  response carries the true size in `Content-Range: bytes */<size>`.
- **Decisions:** On a CDN 416, parse the true size; if it differs from the
  stored size, update the DB size, invalidate the cached CDN URL, negative-cache
  the file, and return 416 to the client (not 502). Never route 416 into
  hang/poll and never quarantine on it — it is a size/content mismatch, often
  transient while a torrent finishes caching, and the size-correction +
  fresh-URL retry handles recovery.
- **Rationale:** A 416 is neither a stale-URL error (403/404) nor a transient
  CDN error (429/5xx) — it is a definite size mismatch. Correcting the size
  makes partially-cached files playable up to their true length and shows stub
  files at their real (tiny) size, while returning 416 instead of 502 avoids
  burning rclone's error budget.
- **Implementation:** `internal/server/get.go` (`streamFileContent` +
  `handleGetCDNHang`), `internal/metadata/store.go` (`SetFileSize`), plus tests.

## D-028: Quarantine persistence window + API Health dashboard

- **Date:** 2026-08-10
- **Context:** Dev-deploy verification of D-026 showed a gap: a persistently-broken
  item at low request rate (the negative cache throttles requestdl to ~1 call per
  30s per file) never tripped the breaker, because the failure-counting window
  (60s) could never accumulate 5 failures. The item showed in "Quarantined items"
  with `Failures=1` and no trip/retry times, and kept hitting the API. Separately,
  there was no quick way to tell "the TorBox API is flaky right now" from the UI.
- **Decisions:**
  1. **Persist, not just burst.** Bump `circuit_breaker_window_seconds` default
     60 → 600 (10 min), so sustained low-rate failures trip the breaker too.
     Bursts still trip instantly.
  2. **Flap-safe UI.** At record time, downgrade a `database_error` to
     `transient` while `globalDegraded()` — the row shows "retrying
     automatically", never the false "remove and re-add".
  3. **Only real quarantines listed.** `QuarantinedItems` filters to trackers
     that have tripped, so a half-populated row (Failures=1, dashes) never
     appears.
  4. **API Health.** Surface the requestdl health tracker as a landing-page
     "API Health" section (badge Healthy/Degraded/Insufficient data + window
     successes/failures/rate + last success/failure) and as a
     `requestdl_success_ratio` (0–100) gauge/sparkline so "is the API flaky?"
     is answerable at a glance. Demoted the hang/poll entry log WARN → Info.
  5. **Sync visibility.** Expose `SyncStatus.InProgress` as a landing-page "Sync
     Status" row (`🔄 Syncing…` / `Idle`) so a slow first sync (large paginated
     mylist through the throttle queue) is identifiable rather than reading as
     "last sync: never". The `starting`/`complete` sync logs stay at Debug to
     avoid spamming the log on a short interval.
  6. **Configurable API-health window.** The rolling window behind the API-health
     indicator (and the flap-detection gate) is now `stats.api_health_window_seconds`
     (default 300, 60–3600) instead of a fixed 5-minute constant.
  7. **Failing items always visible.** The landing "Failed links" list shows
     every tracked item: amber **Failing (N)** for untripped items and red
     **Quarantined** for tripped ones, so a persistently-failing item is visible
     immediately rather than only after the breaker trips.
- **Rationale:** Quarantine should engage on persistence, not just retry storms;
  and the health gate that prevents false "dead" labels is only trustworthy if
  its state is visible.
- **Config:** `circuit_breaker_window_seconds` default changed 60 → 600.
- **Implementation:** `internal/config/config.go`, `internal/server/get.go`,
  `internal/server/server.go` (`APIHealth`, `requestdlHealth`,
  `QuarantinedItems` filter), `internal/server/landing.go`/`landing.html`,
  plus tests and docs.

## D-029: Correct TorBox user-info field mapping + trim account display

- **Date:** 2026-08-10
- **Context:** The landing page "TorBox Account" section showed misleading data:
  a blank plan name (`(tier 2)` with no name), `Premium download limit 0`,
  `Total egressed 0`, `Overall ratio 0.00`, `Premium expires —`, and a blank
  referral code — even on a Pro account. The `UserInfo` struct in
  `internal/torbox/client.go` used JSON tags that did not match the real
  TorBox `/user/me` response.
- **Decision:** Map the struct to the API's actual field names (verified
  against the official TorBox JS SDK, `TorBox-App/torbox-sdk-js`):
  - `is_subscribed` (not `premium`/`registered`) → drives the ⭐ Premium badge.
  - `premium_expires_at` (not `premium_expires`) → the real expiry date.
  - Remove fields the API does not return: `plan_name`, `premium`,
    `registered`, `premium_download_limit`, `total_egressed`, `overall_ratio`,
    `referral_code` (`user_referral`), `total_downloaded`.
  - The plan name is not in the response; derive it from the tier integer
    (`0=Free, 1=Essential, 2=Pro, 3=Standard`) via `planName()`.
  - Trim the landing-page account table to exactly: Plan, Email, Premium
    expires, Account created — dropping the fake 0/blank rows — and format
    dates as human-readable (`formatTBDate`, e.g. "12 May 2026").
- **Rationale:** The old tags silently discarded real data (expiry date,
  referral) and surfaced fields that never exist, making the section read as
  "free/empty" for everyone. Mapping to the real schema restores accuracy with
  no API change.
- **Implementation:** `internal/torbox/client.go` (`UserInfo`),
  `internal/server/landing.go` (`planName`, `formatTBDate`, `LandingData`),
  `internal/server/landing.html`, plus tests and docs.
