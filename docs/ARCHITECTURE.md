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

## Logging

- Exclusively uses Go's native structured logging package (`log/slog`).
- Supports toggling between human-readable text output (local development) and structured JSON output (production/containerised environments).

## Network & Rate Limiting

- Blocking queues and internal throttling manage massive concurrent read requests. The proxy absorbs burst traffic from Plex and drip-feeds it to the TorBox API strictly below the 300 requests/minute limit.

## Documentation Structure

- Living specifications, testing strategies, architecture decision records (beyond the decision log), and onboarding guides belong in the `docs/` folder.
- Issues are for actionable, completable units of work. If content will be continually updated over time (e.g., a test plan), it belongs in `docs/`.
- When an issue's scope expands into a living document, create a Markdown file under `docs/` and link it from the issue.
