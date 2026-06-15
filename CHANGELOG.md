# Changelog

All notable changes to Warpbox will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.4.0] - 2026-06-13

### Added
- Config validation — every tunable field now has strict range checking with clear error messages instead of silent clamping, refs #97
- HTTP streaming endpoint at `/http/` for direct file serving through Warpbox, closes #25
- Display TorBox account details (plan, email, storage used) on the landing page, refs #93

### Changed
- Remove RAM byte-range cache — all data passes through the CDN proxy without an intermediate memory buffer

### Fixed
- Record per-interval deltas for counter metrics in stats charts (previously showed cumulative values), refs #89
- Highlight active log level button on landing page and prevent re-clicking the already-selected level, refs #90
- Lower CDN streaming copy error logs from ERROR to DEBUG to reduce noise from connection races

## [v0.3.1] - 2026-06-12

### Fixed
- Separate Docker push token from release token — use ben's token for registry push, ci-bot token for release creation

## [v0.3.0] - 2026-06-12

### Added
- Time-series stats with sparkline charts on landing page via /stats.json endpoint
- Toast notification on log level change
- Changelog system with Keep a Changelog format

### Changed
- Replace SVG sparklines with Chart.js line charts via /stats.json endpoint

### Fixed
- Route CDN proxy 429/5xx into hang/poll mode to avoid rclone error counting
- Add debug.FreeOSMemory() to periodic cleanup loop to reclaim unused Go arenas
- Add toast notifications to resync and clear cache actions
- Exclude gitea_web.go from compilation with //go:build ignore
- Use ci-bot identity for docker login in publish job

## [v0.2.3] - 2026-06-11

### Added
- Config options for memory management (negative cache max, circuit breaker max, cleanup interval, memory stats interval)
- Route CDN URL fetches by source type (torrent vs usenet)
- Replace extea pwsh-based board ops with direct Python web session (BoardSession)

### Changed
- Reduce CPU and memory — log level default to info, pre-sized buffers, skip tiny chunks, CDN connection semaphore, runtime log level toggle
- Stream CDN response instead of io.ReadAll, reducing peak memory and GC pressure

### Fixed
- Memory usage investigation and mitigation
- Cache key collision from broken rune-based hashing

## [v0.2.2] - 2026-06-11

### Added
- Self-upgrading entrypoint and dev-deploy script for fast iteration
- Slow-disk CDN hang to prevent Plex file trashing

### Fixed
- Replace path-based prune with sync_tag batch approach
- Landing page centering and PROPFIND directory timestamps
- Cross-reference board operations in source-control.md and ARCHITECTURE.md
- Use docker exec to hot-swap binary, works with existing image lacking entrypoint
- Use single quotes for sh -c to prevent apk help error
- Remove --no-cache flag for apk v3 compatibility on golang:1.26-alpine
- Reduce default cdn_url_retry_attempts to 1

## [v0.2.1] - 2026-06-11

### Fixed
- Hide API key in non-200 response logs, increase 429 backoff to 30s

## [v0.2.0] - 2026-06-11

### Added
- Make retry/backoff/negative-cache/circuit-breaker thresholds configurable via config.yml

### Changed
- Generate config template from config.yml.example

### Fixed
- Add exponential backoff, negative cache, and circuit breaker for CDN URL fetches

## [v0.1.6] - 2026-06-11

### Added
- CDN URL auto-repair on stale download links

## [v0.1.5] - 2026-06-11

### Fixed
- Prune stale records after metadata sync

## [v0.1.4] - 2026-06-11

### Fixed
- Sanitize path segments and fix single-file torrent paths

## [v0.1.3] - 2026-06-11

### Fixed
- Use TorBox CreatedAt for PROPFIND LastModified timestamps
- Landing page centring, rclone docker-compose service, log message consistency

## [v0.1.2] - 2026-06-10

### Added
- Sync Usenet downloads alongside torrents

### Changed
- Derive virtual paths from s3_path instead of constructing manually

### Fixed
- Flatten hash-named torrent files directly to root
- Strip emoji prefix, use short_name, skip empty file lists
- Hardcode TorBox API base URL, show API health on landing page, fix rclone flags in README
- Tests and .gitignore for base_url removal, closes CI build failure

## [v0.1.1] - 2026-06-10

### Fixed
- PROPFIND returns deeply nested entries as flat children

## [v0.0.9] - 2026-06-10

### Added
- Wire API call stats into throttle section and landing page
- Action buttons (resync metadata, clear RAM cache) to landing page
- /infuse/ WebDAV endpoint, /logs/ viewer with ring buffer
- /http/ HTML directory browser with breadcrumbs
- Auto-generate config.yml when missing
- Runtime stats, config display, and file count to landing page

### Changed
- Replace PNG logos with SVG (warpbox.svg), clean up old PNGs
- Add canonical semver Docker tag alongside arch-specific tags
- Switch to GPL v3 license

### Fixed
- PROPFIND returns deeply nested entries as flat children
- Add missing banner.txt for CI build
- Don't exit after auto-generating config

## [v0.0.8] - 2026-06-10

### Added
- Show warpbox ASCII banner on startup

### Fixed
- Bootstrap structured logger before config load
- Return 207 Multi-Status XML for GET on WebDAV directory paths

### Changed
- Add carriage return and version line after banner

## [v0.0.5] - 2026-06-10

### Fixed
- Revert docker login to -u ben

## [v0.0.4] - 2026-06-10

### Fixed
- Docker login as ci-bot not ben

## [Unreleased]

### Changed
- Consolidate all health/metrics collection into a single DB-backed source of truth — remove redundant 5-minute memory stats log ticker (`cache.memory_stats_interval_minutes` config key removed), closes #98
- Drop misleading "total allocated" (cumulative odometer) from landing page and chart — add `gc_cycles`, `heap_objects`, and `db_lock_errors` charts instead, closes #99

[Unreleased]: https://REDACTED/ben/warpbox/compare/v0.4.0...HEAD
[v0.4.0]: https://REDACTED/ben/warpbox/compare/v0.3.1...v0.4.0
[v0.3.1]: https://REDACTED/ben/warpbox/compare/v0.3.0...v0.3.1
[v0.3.0]: https://REDACTED/ben/warpbox/compare/v0.2.3...v0.3.0
[v0.2.3]: https://REDACTED/ben/warpbox/compare/v0.2.2...v0.2.3
[v0.2.2]: https://REDACTED/ben/warpbox/compare/v0.2.1...v0.2.2
[v0.2.1]: https://REDACTED/ben/warpbox/compare/v0.2.0...v0.2.1
[v0.2.0]: https://REDACTED/ben/warpbox/compare/v0.1.6...v0.2.0
[v0.0.9]: https://REDACTED/ben/warpbox/compare/v0.0.8...v0.0.9
[v0.0.8]: https://REDACTED/ben/warpbox/compare/v0.0.4...v0.0.8
[v0.0.5]: https://REDACTED/ben/warpbox/compare/v0.0.4...v0.0.5
[v0.0.4]: https://REDACTED/ben/warpbox/compare/v0.0.2...v0.0.4