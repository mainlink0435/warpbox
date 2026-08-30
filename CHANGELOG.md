# Changelog

All notable changes to Warpbox will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.8.0] - 2026-08-14

### Added
- Circuit-breaker quarantine escalation — a persistently-failing item's stale window now doubles each retry cycle up to `cache.circuit_breaker_max_stale_minutes` (default 60), so a permanently-broken torrent/usenet item stops being hammered
- "Failed links" section on the landing page listing items that can't obtain a CDN link: amber **Failing (N)** while accumulating, red **Quarantined** once tripped (with remove-and-re-add guidance)
- "API Health" section on the landing page (Healthy / Degraded / Insufficient data badge, trailing-window successes/failures/rate, last success/failure with relative time) plus a `requestdl_success_ratio` sparkline; the window is configurable via `stats.api_health_window_seconds` (default 300)
- CDN 416 Range Not Satisfiable handling — the stored file size is corrected from the CDN's reported size and 416 is returned instead of 502
- Sync status row on the landing page (`🔄 Syncing…` / `Idle`)
- Uptime and "last sync" durations now include days (e.g. `16d16h32m55s`)
- Non-200 TorBox API responses now surface the error code (e.g. `DATABASE_ERROR`, `BAD_TOKEN`) in logs

### Fixed
- API health now counts **every** TorBox call (metadata sync, download-link requests, and CDN data-plane attempts) at the raw HTTP level — including retried-and-recovered failures that previously counted as a single success — so a multi-day provider outage shows up in the "API Health" section (failures, degraded badge) instead of reading "Healthy / Failures: 0"
- `circuit_breaker_window_seconds` default raised 60 → 600 so persistent low-rate failures (throttled by the negative cache) trip the breaker, not just burst storms
- A transient TorBox-wide outage no longer falsely labels healthy items as dead — the "remove and re-add" guidance only appears when the API is otherwise healthy
- Items removed or re-added at TorBox clear quarantine automatically on the next metadata sync (breaker entries pruned for item ids no longer in the library)
- Hang/poll entry log demoted from WARN to Info
- TorBox account details on the landing page now show real data: the plan name is derived from the tier (Free/Essential/Pro/Standard), the premium-expiry date and subscription state (⭐) are no longer dropped by a JSON-tag mismatch, and dates are human-readable. The account table is trimmed to Plan, Email, Premium expires, and Account created (the previously-shown download/egress/ratio/referral rows came from fields the API doesn't return)
- Landing-page chart sparkline axes are now pinned: count metrics (success/failed/429s/db locks/neg cache/breaker) start at 0, and the API-health ratio is capped at 0–100%

### Changed
- The "Failed links" table title now stays fixed while its body scrolls, and the logs page width is standardised to match the landing page (1200px)

## [v0.7.6] - 2026-08-04

### Fixed
- Metadata sync no longer deletes previously-synced torrent and usenet files when a fetch fails — a failed fetch is never treated as "these items are gone", so a transient TorBox API error can no longer silently wipe the library from the store
- A slow or transiently-failing page during sync is retried in place instead of aborting the whole list and restarting from the beginning
- A manual resync can no longer run concurrently with a periodic sync (in-flight guard), which previously could double-prune the store
- The landing page log-level selector now shows the currently active level after a page refresh

### Added
- Configurable TorBox API request timeout: `torbox.request_timeout_seconds` (default 90s)
- Configurable manual-resync overall cap: `sync.sync_timeout_seconds` (default 0 = no cap)
- Configurable CDN proxy timeout (`cache.cdn_proxy_timeout_seconds`, default 30s) and CDN 429 backoff (`cache.cdn_url_429_backoff_seconds`, default 30s)

## [v0.7.5] - 2026-07-23

### Fixed
- Library `on_items_added` / `on_items_removed` hook commands now run relative to the config file's directory (e.g. `/data/` in Docker) instead of the process working directory
- Config is mounted as a named Docker volume instead of a host bind mount

## [v0.7.4] - 2026-07-20

### Fixed
- Docker images: canonical (`vX.Y.Z`) and `latest` tags now use a multi-arch manifest (linux/amd64 + linux/arm64) instead of pointing only to amd64

## [v0.7.3] - 2026-07-17

### Added
- Per–virtual-path `min_file_size` and `max_file_size` config options to filter files by byte range (e.g. `1.5GB` for movies, `300MB` for TV). Bounds apply after name/regex filters and before `largest_file_only`. Empty = no limit. (thanks @Allifreyr)

## [v0.7.2] - 2026-07-16

### Fixed
- Percent-encoding in WebDAV hrefs for filenames containing a literal `%` (e.g. `30% Iron Chef`) — rclone no longer fails with `invalid URL escape` thanks to per-segment percent-encoded D:href values (thanks @Allifreyr)
- CDN connection semaphore now acquired before the upstream request — `max_cdn_connections` correctly limits concurrent TorBox CDN opens (thanks @Allifreyr)
- Hang/poll mode now retries on transient CDN data errors (429/5xx/disguised text body) instead of streaming error pages as file content into rclone's VFS cache (thanks @Allifreyr)

## [v0.7.0] - 2026-07-09

### Added
- CDN URL fallback to alternative TorBox items when the primary item's file cannot be fetched — improves resilience when duplicate downloads exist
- Unique path count on landing page (shown as "N total / M unique")
- `CountDistinctPaths()` store method for deduplicated file counts
- `GetFileAlternatives()` store method for querying duplicate entries

### Changed
- **Database schema v2:** File uniqueness is now enforced by `(source, item_id, file_id)` instead of `path`. Duplicate virtual paths from different TorBox items are preserved as separate rows. Existing databases are automatically recreated on first startup — the cache will repopulate on the next sync cycle. **This is a one-way upgrade; to downgrade, delete `warpbox.db` and re-sync.**
- `UpsertFile` conflict target changed from `path` to `(source, item_id, file_id)` — CDN URL cache fields are preserved on conflict
- `GetFileByPath` returns the highest-internal-ID record when duplicates exist (deterministic tiebreaker)
- `ListDir` deduplicates by path (one row per unique path)
- Landing page shows both total file rows and distinct virtual paths
- `dbinspect` diagnostic tool updated for new schema checks

## [v0.6.0] - 2026-06-26

### Added
- Mylist pagination — all torrents/usenet items sync regardless of account size. TorBox caps each response at ~10,000 items; warpbox pages through with offset until exhaustion. (thanks @Fredddi43, closes #1)
- Configurable `sync.list_page_size` — controls the per-request page window when paginating mylist API calls (default 5000, range 1–10000), shown on landing page
- Exponential backoff in CDN hang/poll mode on repeated 429 rate-limit errors (15s → 30s → 60s → 2min → 5min max), preventing per-item requestdl death spirals
- Item count on landing page — distinct torrents/usenet items alongside total files

### Fixed
- CDN text error body is no longer streamed and cached as file data. TorBox's CDN sometimes returns HTTP 200/206 with "Too many requests" or HTML body instead of 429; the GET handler now checks Content-Type before streaming. (thanks @Fredddi43, closes #3)

### Changed
- Removed `sync.limit` — pagination now fetches all items without a ceiling. The old cap was a workaround from before the pagination engine existed.

## [v0.5.4] - 2026-06-23

### Added
- Configurable sync retry: `sync.retry_attempts` (default 3) and `sync.retry_backoff` (default 1s) control exponential backoff for transient API errors during metadata sync

### Fixed
- TorBox API transient errors (502, timeouts, HTML error pages) during metadata sync now trigger retry with exponential backoff instead of failing immediately
- TorBox API returning HTML error pages with HTTP 200 now logs the body at WARN (200-char preview) and DEBUG (full body) instead of a cryptic `invalid character '<'` error
- CDN 403/404 failures after URL repair exhaustion are cached in the negative cache, preventing Plex retry storms from burning TorBox API calls
- Pre-existing data race in `Status()`/`syncOnce()` on `lastError`/`lastSuccess` — now protected by mutex

## [v0.5.3] - 2026-06-19

### Fixed
- Set `largest_file_only: false` for `tv` virtual path — season packs now show all episode files instead of just one, refs #172
- Remove `:ro` from docker-compose config volume mount so `GenerateTemplate` can create `config.yml` on first run, refs #172

## [v0.5.2] - 2026-06-16

### Fixed
- Always inject `__all__` synthetic directory at /webdav/, /http/, /infuse/ root even when no virtual paths are configured
- Silently ignore user-configured virtual path named `__all__` instead of returning a validation error

## [v0.5.1] - 2026-06-16

### Added
- Graceful HTTP server shutdown with 30s timeout, refs #162

### Changed
- Wire stats.interval_seconds, log dropped errors, update stale docs, refs #163 #166 #168
- Address audit findings and expand test coverage, refs #153 #156 #158 #157
- Docker tag to :latest and add source-build section, refs #152
- Humanise README and contributing guide, refs #84 #106

### Fixed
- Log discarded time.Parse errors in stats queries, refs #160
- Pass caller context in ringBufferHandler instead of context.Background(), refs #159
- Change prune gate to check API success not count>0, refs #155
- Log discarded ListItemDirs errors in sync change detection, refs #160
- Remove invalid directory_regex and duplicate entries in config.yml.example, refs #162
- Correct ListenAddr default comment from :8080 to :1412, refs #152 #154

## [v0.5.0] - 2026-06-16

### Added
- Virtual library paths with directory/file regex filtering and change hooks, refs #32 #33
- Chi router for structured HTTP routing with middleware support, refs #43
- Chi-driven OpenAPI spec generation via route introspection, refs #53
- Optional HTTP Basic Authentication for web management UI, refs #79
- Sync worker restart action via landing page, refs #95
- Pre-release codebase audit script, refs #96
- Report disclaimer and use deepseek-pro model for audits, refs #96
- Code comment quality check in audit prompt, refs #145
- HTTP browser folder sizes and column sorting (name, size, modified), refs #146
- `/healthz` endpoint for container health checks, refs #111
- Audit self-reports now emit individual issue findings with run metadata, refs #147

### Changed
- Consolidate health/metrics into single DB-backed source of truth — remove redundant 5-minute memory stats log ticker (`cache.memory_stats_interval_minutes` removed), closes #98, closes #99
- Replace `directory_regex` with `directory_include` / `directory_exclude` for path filtering
- Replace `sync.Cond` with channel-based throttle queue to prevent goroutine leak, refs #142
- Use `url.JoinPath` instead of raw string concatenation for URL construction, refs #113
- Use `defer` for CDN connection release in non-hang streaming path, refs #112
- Migrate all documentation to standard conventions with `docs/tech-spec.md` skeleton, refs #96
- Move internal AI instructions and Git Authorship rules into docs/

### Fixed
- HTTP browser hrefs missing virtual path mount prefix in breadcrumbs and links
- Virtual paths now correctly nested under `/webdav/` as subdirectories
- Remove DEBUG-level per-row UpsertFile logging that flooded logs during sync
- Record `gc_cycles` as per-interval delta instead of cumulative gauge in stats charts
- Replace `torrent_id` with `item_id` in dbinspect queries, refs #141
- Gate `/debug/pprof/` behind `enable_pprof` config flag, wire SyncLimit, fix stale comment, refs #107, refs #108, refs #140
- Batch prune deletes and retry SetCDNURL to prevent SQLite lock contention, refs #100
- Remove live API credentials from repo — switch to `.template` files, refs #143
- Fix pre-release audit documentation issues across multiple tickets, refs #109 #110 #138 #139

[Unreleased]: /compare/v0.8.0...HEAD
[v0.8.0]: /compare/v0.7.6...v0.8.0
[v0.7.6]: /compare/v0.7.5...v0.7.6
[v0.7.5]: /compare/v0.7.4...v0.7.5
[v0.7.4]: /compare/v0.7.3...v0.7.4
[v0.7.3]: /compare/v0.7.2...v0.7.3
[v0.7.2]: /compare/v0.7.1...v0.7.2
[v0.7.1]: /compare/v0.7.0...v0.7.1
[v0.7.0]: /compare/v0.6.0...v0.7.0
[v0.6.0]: /compare/v0.5.4...v0.6.0

[v0.5.4]: /compare/v0.5.3...v0.5.4

[v0.5.3]: /compare/v0.5.2...v0.5.3

[v0.5.2]: /compare/v0.5.1...v0.5.2

[v0.5.1]: /compare/v0.5.0...v0.5.1

