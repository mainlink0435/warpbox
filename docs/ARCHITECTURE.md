# System Patterns: Warpbox

## Core Architecture

Warpbox operates as an intercepting WebDAV proxy, designed to be consumed by rclone's WebDAV backend. It acts as a shield between aggressive local media servers (accessed via rclone) and strict cloud APIs (TorBox). The primary pattern is decoupling filesystem speed from network speed.

```
Plex/Jellyfin → rclone (FUSE mount) → WebDAV → Warpbox → TorBox API
```

## Configuration Management

- All application settings are driven by a declarative `config.yml` file.
- The structure logically separates upstream cloud credentials, local WebDAV server settings, caching rules, and rate-limiting parameters.
- The exact schema is flexible but must support graceful degradation if optional parameters are omitted.
- No hardcoded behaviour: any tunable logic (thresholds, strategy selection, durations, cache lifetimes, eviction policies) must be surfaced as a configuration option with a sensible default.
- Every configuration key must appear in `config.yml.example` with an explanatory comment documenting purpose, default value, required/optional status, and valid options/ranges.

## State & Caching Patterns

- **Persistent State (Metadata):** SQLite in WAL (Write-Ahead Logging) mode for the virtual directory structure, file metadata, and cache pointers. Enables zero-API directory browsing.
  - **Uniqueness model:** File records are keyed by `(source, item_id, file_id)` — the natural TorBox identifier. Two different TorBox items sharing the same virtual path produce separate rows. The display layer deduplicates by path (`GetFileByPath` returns highest-ID record, `ListDir` collapses duplicates).
  - **CDN fallback:** When the primary item's CDN URL fetch fails, alternative items (same path, different `item_id`) are tried before falling back to hang/poll mode.
  - **Schema upgrades:** The database is a cache derived from the TorBox API. v1→v2 upgrades recreate the database file automatically (one-way; downgrading requires deleting `warpbox.db` and re-syncing).
- **Ephemeral State (Data):** CDN proxy passthrough. No intermediate memory buffering — all data streams directly through the CDN proxy from TorBox's origin servers to the client. The proxy handles concurrent connections via a semaphore-controlled pool (configurable via `max_cdn_connections`).

## Additional Endpoints

Beyond the core WebDAV proxy, warpbox serves several management and observability endpoints:

| Endpoint | Description |
|----------|-------------|
| `/webdav/` + `/webdav/*` | Primary WebDAV mount point (PROPFIND, GET, HEAD, OPTIONS). No auth. |
| `/infuse/` + `/infuse/*` | Infuse-compatible alias — rewrites `/infuse` → `/webdav` internally. No auth. |
| `/http/` + `/http/*` | HTML directory listing with sortable columns, plus CDN file streaming. Auth required. |
| `/logs/` | Ring-buffer log viewer (last 1000 lines). Auth required. |
| `/healthz` | JSON health check — pings SQLite with 3s timeout. No auth. |
| `/stats.json` | Time-series metrics API (API calls, memory, cache sizes). Auth required. |
| `/openapi.json` | Auto-generated OpenAPI 3.0 spec from annotated routes. Auth required. |
| `/actions/*` | Management actions (resync, restart-sync, loglevel). POST only, CSRF-protected. Auth required. |
| `/` | Landing page with stats, charts, and status. Auth required. |
| `/chart.umd.min.js`, `/warpbox.svg`, `/favicon.ico` | Static assets (embedded at compile time). No auth. |

## CDN Resilience

The CDN proxy pipeline includes several defensive layers beyond the semaphore-controlled connection pool:

- **Negative cache:** Failed CDN URL fetches are cached in-memory (configurable TTL, default 30s). Subsequent Plex retries skip the API entirely, preventing retry storms.
- **Circuit breaker:** Per-torrent failure tracking with a sliding window (default 5 failures in 60s). A tripped breaker shorts all requests to that torrent for a cooldown period (default 5 minutes), preventing cascading failures from burning the rate budget.
- **Disguised error detection (`isCDNDisguisedErrorBody`):** TorBox's CDN sometimes returns HTTP 200 with a text/html/json body instead of a proper 429 when rate-limiting. The proxy detects this by Content-Type and treats it as a transient error — the error page is never streamed to the client.
- **CDN URL caching:** CDN download URLs are cached in SQLite (configurable TTL, default 120 minutes). Repeated range requests to the same file reuse the cached URL without hitting the API.
- **Statistics recording:** Configurable interval stats (API call rates, memory usage, cache sizes) are written to SQLite and surfaced as sparkline charts on the landing page.

## Library Change Hooks

When the sync worker detects items have been added or removed, it can invoke shell commands (`library.on_items_added`, `library.on_items_removed`). Commands receive item directory names as positional arguments, with a configurable timeout (default 30s). Useful for refreshing media server libraries after sync.

## CSRF Protection

All `/actions/*` POST endpoints require an `X-CSRF-Token` header (generated per-session, stored in-memory). This prevents external websites from triggering management actions if the user is browsing while authenticated.

## Logging

- Exclusively uses Go's native structured logging package (`log/slog`).
- Supports toggling between human-readable text output (local development) and structured JSON output (production/containerised environments).

## Network & Rate Limiting

- Blocking queues and internal throttling manage massive concurrent read requests. The proxy absorbs burst traffic from Plex and drip-feeds it to the TorBox API strictly below the 300 requests/minute limit.

## Documentation Structure

- Living specifications, testing strategies, architecture decision records (beyond the decision log), and onboarding guides belong in the `docs/` folder.
- Issues are for actionable, completable units of work. If content will be continually updated over time (e.g., a test plan), it belongs in `docs/`.
- When an issue's scope expands into a living document, create a Markdown file under `docs/` and link it from the issue.
