# Warpbox Testing Suite

## Overview

Two PowerShell test scripts validate the WebDAV pipeline against a live TorBox API key.

## `integration test` — Basic WebDAV Contract

Quick smoke-test (~10s). Run this first.

**Phases:** Server startup, OPTIONS/PROPFIND, HEAD, GET byte-range, CDN caching, concurrent requests.

```
.\integration test -ConfigPath config.yml -ServerPort 8080 -WebRoot /webdav
```

## `intensive test` — Data Integrity & Edge Cases

Deeper checks on byte accuracy, property completeness, headers, error modes.
Expects server already running from `integration test`.

```
.\intensive test
```

## Running Together

```powershell
go build -o warpbox-test.exe ./cmd/warpbox
.\integration test -ConfigPath config.yml
.\intensive test
```

## Test Gaps

| Gap | Priority |
|---|---|
| Throttle backpressure (200+ concurrent) | High |
| API 401 expiry handling | High |
| CDN URL expiry/repair | Medium |
| SQLite metadata sync freshness | Medium |
| Go unit tests (throttle, cache, config, torbox) | High |

## Strategy

1. **Every commit:** `integration test` must pass
2. **Every tag:** Both scripts must pass
3. **Go unit tests:** `_test.go` files for internal packages (no live key needed)