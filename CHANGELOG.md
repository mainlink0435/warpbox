# Changelog

All notable changes to Warpbox will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: /compare/v0.5.1...HEAD

[v0.5.1]: /compare/v0.5.0...v0.5.1

