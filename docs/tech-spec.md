# Technical Specification

This document is the authoritative technical specification for Warpbox.
Every claim here MUST match the current source code — any deviation is a bug.
If you change the code, update this spec.

## 1. Server Startup & Lifecycle

**Key files:** `cmd/warpbox/main.go`, `internal/server/server.go`

### Startup Sequence

1. **Flag parsing.** `cmd/warpbox/main.go` uses `flag.Parse()` with three flags:
   - `-config <path>` — path to YAML config file (default: `"config.yml"`)
   - `-db <path>` — path to SQLite database file (default: `"warpbox.db"`)
   - `-version` — print the build version and exit

2. **Banner & version check.** If `-version` is set, print `Version` (injected at build time via `-ldflags -X main.Version=vX.Y.Z`, defaults to `"dev"` for local builds) and `os.Exit(0)`.

3. **Bootstrap logger.** `slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))` — ensures early errors (before config is loaded) are structured but kept at ERROR level only.

4. **Config template generation.** `config.GenerateTemplate(*configPath)`:
   - Checks if the config file already exists (`os.Stat`)
   - If not, reads `config.yml.example` from the same directory and writes it to the config path
   - Returns `true` if a new file was created — main.go logs a warning telling the user to edit the file

5. **Config load.** `config.Load(*configPath)`:
   - `os.ReadFile(path)` → `yaml.Unmarshal(data, &Config{})` → `setDefaults(cfg)` → `validate(cfg)`
   - See Section 8 for defaults and validation rules

6. **Logging setup.** The second phase of logger initialization:
   - `config.ParseLevel(cfg.Logging.Level)` — converts `"debug"|"info"|"warn"|"error"` to `slog.Level`
   - Creates an `slog.LevelVar` for atomic runtime level switching
   - Creates either `slog.NewTextHandler(os.Stderr, opts)` or `slog.NewJSONHandler(os.Stderr, opts)` depending on `cfg.Logging.Format`
   - Wraps the handler in a `ringBufferHandler` (the in-memory ring buffer for `/logs/`)
   - Stores the buffer globally via `server.SetLogBuffer(bufHandler)`
   - `slog.SetDefault(logger)` — replaces the bootstrap logger
   - If level is not `"info"`, logs a warning about production performance

7. **Banner print.** Embedded `banner.txt` (via `//go:embed banner.txt`) printed to stdout, version string, description.

8. **Database directory.** `os.MkdirAll(filepath.Dir(*dbPath), 0755)` — ensures the directory for the SQLite database exists.

9. **Database open.** `metadata.Open(*dbPath)`:
   - DSN: `dbPath?_journal_mode=WAL&_busy_timeout=5000&_cache_size=-8192`
   - WAL mode for concurrent read performance
   - 5-second busy timeout for writer contention
   - 8 MB page cache
   - Auto-runs schema migrations (`CREATE TABLE IF NOT EXISTS`)
   - `defer metadataStore.Close()` — closed on process exit

10. **Throttle queue.** `throttle.NewQueue(cfg.Throttle.RequestsPerMinute)`:
    - Computes inter-request spacing: `rate = time.Minute / RPM`
    - Creates buffered channel (capacity 1024)
    - `queue.Start(ctx)` — launches the `processLoop` goroutine

11. **TorBox API client.** `torbox.NewClient(cfg.TorBox.APIKey)`:
    - Hardcoded base URL: `"https://api.torbox.app"`
    - HTTP client with 30-second timeout
    - Wires `HTTP429Callback`: `func() { throttleQueue.Record429() }` — called when the TorBox API returns HTTP 429

12. **Signal context.** `ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM); defer stop()` — the root context is cancelled when SIGINT or SIGTERM is received. All components derive their contexts from this root.

13. **Sync worker.** `metadata.NewSyncWorker(store, client, queue, interval, limit)`:
    - Stores references to the metadata store, TorBox client, throttle queue, interval, and limit
    - Wires library hooks: `syncWorker.OnItemsAdded` and `syncWorker.OnItemsRemoved` are set to call `runItemsHook(libCfg.OnItemsAdded, libCfg.HookTimeoutSec, items)` when configured
    - `go syncWorker.Start(ctx)` — runs the periodic sync loop in a background goroutine

14. **Actions registration.** `server.SetActions(map[string]server.ActionFunc{...})`:
    - `"resync"` → `syncWorker.SyncNow()` — triggers an immediate out-of-cycle sync
    - `"restart-sync"` → `syncWorker.Restart()` — stops and restarts the sync loop

15. **Server config assembly.** A `server.Config` struct is populated from all config sections:
    - **Server:** `ListenAddr`, `Version`, `EnablePprof`, `ConfigPath`
    - **Cache:** `CDNTtlMinutes`, `CDNURLAutoRepair`, `CDNURLRepairRetries`, `CDNURLRetryBackoff`, `CDNURLRetryCount`, `NegativeCacheTTLSeconds`, `CircuitBreakerFailures`, `CircuitBreakerWindowSec`, `CircuitBreakerStaleMin`, `NegativeCacheMaxEntries`, `CircuitBreakerMaxEntries`, `CleanupIntervalSeconds`, `MaxCDNConnections`
    - **Throttle:** `RequestsPerMinute` (for landing page display)
    - **Logging:** `LogFormat`, `LogLevel`, `LevelVar` (shared pointer for runtime switching)
    - **Sync:** `SyncIntervalMinute`, `SyncLimit` (for landing page display)
    - **Stats:** `StatsIntervalSeconds`, `StatsRetentionHours`, `StatsChartMinutes`
    - **Auth:** `AuthEnabled`, `AuthUsername`, `AuthPassword`
    - **Library:** `VirtualPaths`

16. **Server creation.** `server.New(cfg, store, torBox, queue)`:
    - Creates `chi.NewRouter()`
    - Pre-fills CDN connection semaphore with `maxConns` tokens
    - Builds virtual path filters from config
    - Registers all HTTP routes (see Section 2)
    - Starts the cleanup goroutine (see Section 7)

17. **Sync status callback.** `srv.SetSyncStatus(syncWorker.Status)` — wires the sync worker's `Status()` method so the landing page can show sync state.

18. **HTTP server start.** `go srv.Start(ctx)` → `http.ListenAndServe(s.cfg.ListenAddr, s.mux)` inside a goroutine, errors sent to a `chan error`.

19. **User info fetch.** A background goroutine calls `torBoxClient.GetUserInfo(ctx)` with a 10-second timeout. On success, stores the result via `srv.SetTorBoxUserInfo(ui)` — displayed on the landing page.

20. **Blocking wait.** `select { case <-ctx.Done(): "shutting down" | case err := <-serverErr: fatal error }` — the process blocks here until either:
    - A signal is received (SIGINT/SIGTERM) via the signal context
    - The HTTP server fails to start or crashes

### Graceful Shutdown

- On signal, `srv.Shutdown(ctx)` is called with a 30-second timeout context, which invokes `http.Server.Shutdown()` — draining active connections and stopping the listener.
- The `defer metadataStore.Close()` runs, flushing WAL.
- The sync worker's loop exits when its derived context is cancelled (via context propagation from the signal context).
- The throttle queue's `processLoop` goroutine exits when its context is cancelled.

### Library Hook Execution (`runItemsHook`)

- `exec.CommandContext(timeoutCtx, cmd, args..., items...)` — runs a shell command with item directory names as positional arguments.
- Command string is split by whitespace (`strings.Fields`); first token is the executable, rest are prepended to the item list.
- Timeout from `library.hook_timeout_seconds` (default 30s). Timeout produces a warning log, not a fatal error.
- `CombinedOutput()` captures stdout+stderr for error logging.


## 2. HTTP Request Pipeline

**Key files:** `internal/server/server.go`, `internal/server/propfind.go`, `internal/server/get.go`, `internal/server/landing.go`, `internal/server/auth.go`, `internal/server/actions.go`, `internal/server/healthz.go`, `internal/server/logs.go`, `internal/server/http_browser.go`

### Router

Uses `github.com/go-chi/chi/v5` (not the standard library `http.ServeMux`). Chi was chosen for its lightweight middleware chaining, route grouping, and method-based routing. Because PROPFIND is not a standard HTTP method, `chi.RegisterMethod("PROPFIND")` is called at startup to register it.

### Middleware Chain

The `versionHeader` middleware is applied to **all routes** via `s.mux.Use(s.versionHeader)`. It sets the `Server` header to `"warpbox/<version>"` on every response.

The `requireAuth` middleware is applied **per-route** using chi's `s.mux.With(requireAuth)` pattern. It wraps the handler with HTTP Basic Authentication when `auth.enabled` is true, otherwise passes through without checking.

### Route Table

| Method(s) | Path | Auth | Handler | Description |
|-----------|------|------|---------|-------------|
| GET, HEAD, OPTIONS, PROPFIND | `/webdav` + `/webdav/*` | No | `handleWebDAV` | WebDAV method dispatcher |
| GET, HEAD, OPTIONS, PROPFIND | `/infuse` + `/infuse/*` | No | `handleWebDAV` (path rewrite) | Infuse-compatible WebDAV (rewrites `/infuse` → `/webdav`) |
| GET | `/http` + `/http/*` | Yes | `handleHTTP` | HTML directory browser + CDN file streaming |
| GET | `/logs` + `/logs/` | Yes | `handleLogs` | HTML log viewer (last 200 lines) |
| GET | `/healthz` | No | `handleHealthz` | JSON health check (DB ping, 3s timeout) |
| GET | `/stats.json` | Yes | `handleStatsJSON` | Time-series metrics JSON |
| POST | `/actions/resync` | Yes | `handleResync` | Trigger immediate TorBox metadata sync |
| POST | `/actions/restart-sync` | Yes | `handleRestartSync` | Stop and restart the sync worker loop |
| POST | `/actions/loglevel` | Yes | `handleLogLevel` | Change log level at runtime + persist to config |
| GET | `/debug/pprof` + `/debug/pprof/*` | Yes | stdlib `http.DefaultServeMux` | pprof endpoints (only registered when `enable_pprof: true`) |
| GET | `/` | Yes | `handleLanding` | Branded HTML status page with stats, charts, buttons |
| GET | `/warpbox.svg` | No | `handleLogo` | Embedded SVG logo (from `embed` FS) |
| GET | `/favicon.ico` | No | `handleLogo` | Same SVG as favicon |
| GET | `/openapi.json` | Yes | `openapi.Handler()` | Auto-generated OpenAPI 3.0 spec from annotated routes |
| GET | `/chart.umd.min.js` | No | `handleChartJS` | Embedded Chart.js library (served from compile-time `//go:embed`) |

### WebDAV Dispatch (`handleWebDAV`)

Because chi uses `Handle` (not per-method routing) for the WebDAV paths, all methods arrive at `handleWebDAV`, which performs internal dispatch:

1. **Infuse path rewrite:** If `r.URL.Path` starts with `/infuse`, it's replaced with the WebDAV root (`/webdav`).

2. **Virtual path detection:** The first path segment after `/webdav/` is extracted via `virtualPathName(path)`:
   - `"__all__"` → unfiltered view. The mount root is set to `/webdav/__all__` in the request context.
   - A registered virtual path name (e.g. `"movies"`) → applies the associated `library.Filter`. The filter and mount root are set in the request context via `context.WithValue`.
   - No segment + virtual paths configured → root level, PROPFIND/GET will show synthetic directory entries (`__all__/` + virtual path names).
   - No segment + no virtual paths → normal unfiltered listing.

3. **Context values:**
   - `filterKey` → `*library.Filter` — used by PROPFIND and GET to filter file listings
   - `mountRootKey` → `string` — the full WebDAV mount path (e.g. `/webdav/movies`) for path prefix stripping

4. **Method dispatch:**
   - `GET` → `handleGet(w, r)`
   - `HEAD` → `handleHead(w, r)`
   - `OPTIONS` → `handleOptions(w, r)` — sets `DAV: 1`, `Allow: OPTIONS, GET, HEAD, PROPFIND`
   - `PROPFIND` → `handlePropfind(w, r)`
   - Default → `405 Method Not Allowed`

### Virtual Path Filtering

The `library.Filter` types (built in `server.go:buildFilters()` from `library.virtual_paths` config) provide regex-based includes/excludes on directory and file names, plus a `largest_file_only` option. Each filter is mounted at a named virtual directory (e.g. `/webdav/movies/`). Filters are compiled from config at server startup and stored in `s.virtualFilters` (slice) and `s.virtualPathMap` (map for O(1) lookup). The `__all__` mount bypasses all filtering.

### OpenAPI Spec

An OpenAPI 3.0 specification is auto-generated from the route tree using `openapi.NewBuilder()` → `BuildFromRouter(s.mux)` (walks all routes using chi's `Walk`) → served as JSON at `/openapi.json`. Routes are annotated inline with `openapi.Annotated()` providing summary, description, tags, request body schemas, and response schemas. The spec is built AFTER all routes are registered so that `Walk` enumerates the complete tree.

### Server Start

`http.ListenAndServe(s.cfg.ListenAddr, s.mux)` — no TLS (designed to run behind a reverse proxy). Errors from `ListenAndServe` are sent to a channel consumed by `main()`.

### Health Check (`/healthz`)

Calls `s.store.Ping(ctx)` with a 3-second context timeout. Returns `{"status": "ok"}` (200) on success or `{"status": "error", "detail": "database unreachable"}` (503) on failure. No auth — designed for Docker healthchecks and load balancer probes.

### Actions (`/actions/*`)

Post-only endpoints wired from `main.go` via `server.SetActions()`:
- **resync** → calls `syncWorker.SyncNow()` (out-of-cycle sync in a 60s background context)
- **restart-sync** → calls `syncWorker.Restart()` (cancels current loop, starts fresh)
- **loglevel** → reads `level` form value, validates via `config.ParseLevel()`, atomically sets `slog.LevelVar`, and persists to config file via `config.UpdateLogLevel()` (temp file + rename for atomicity)

All actions execute asynchronously in a goroutine and return `200 OK` immediately.

### Landing Page (`/`)

See Section 7. Renders a Go `html/template` with runtime stats, configuration values, TorBox account info, sync status, and Chart.js sparklines.


## 3. CDN Streaming & Fallback Chain

**Key files:** `internal/server/get.go`

### GET Handler Flow (`handleGet`)

1. **Virtual path resolution.** Extract `library.Filter` from context (if any). Determine the mount root via `rootForRequest(r)`. Strip root from URL path to get the virtual path.

2. **Empty path** → `serveDirListing(w, reqPath, "1", libFilter, root)` — serves a WebDAV Multi-Status XML directory listing (same as PROPFIND).

3. **File lookup.** `s.store.GetFileByPath(virtualPath)`:
   - Found → proceed to streaming
   - `nil` + `ListDir` returns results → virtual directory → `serveDirListing`
   - `nil` + no children → `404 Not Found`

4. **Redirect optimization.** If the request has **no Range header** and a **cached CDN URL exists** (`s.store.GetCDNURL(file.ID)` returns a non-expired URL), issue a `302 Found` redirect directly to the CDN URL. This avoids consuming a CDN connection slot for full-file downloads and preserves existing behaviour where rclone fetches metadata efficiently. The `/http/` browser does NOT use this optimization — it always proxies through `streamFileContent` to set proper Content-Type headers for browser inline playback.

5. **Streaming.** `s.streamFileContent(w, r, file)` — the main CDN proxy pipeline.

### CDN URL Resolution (`fetchCDNURL`)

Examines three layers before making an API call:

1. **Negative cache check.**
   - Key format: `"<source>:<itemID>:<fileID>"` — e.g. `"torrent:123:456"` or `"usenet:789:012"`
   - If an entry exists and `time.Now().Before(entry.expiresAt)`, return the cached error immediately — the API call is skipped entirely.
   - If expired, delete the entry and proceed.

2. **Circuit breaker check.** `isTorrentStale(itemID)`:
   - Looks up the `torrentFailureTracker` for this item ID.
   - If `time.Now().Before(tracker.staleUntil)`, return error — the API call is skipped. A warning is logged.
   - If the stale period has expired, delete the tracker (auto-reset) and proceed.

3. **API call with retry.** `getCDNURLWithRetry(source, itemID, fileID)` — see below.

4. **On failure:** Write the error to the negative cache under the file's key with `NegativeCacheTTLSeconds` TTL (default 30s).

### Retry / Backoff (`getCDNURLWithRetry`)

- **Max attempts** = `cdn_url_retry_attempts + 1` (default 2 — 1 initial + 1 retry).
- Each attempt enqueues a `throttle.Request` that calls `GetDownloadURL()` (torrent) or `GetUsenetDownloadURL()` (usenet) depending on source.
- **Retryable errors:** HTTP 429 or 5xx (detected by string matching `"unexpected status 429"` or `"unexpected status 5"`).
- **Backoff:** Exponential `baseBackoff * 2^attempt` where `baseBackoff = cdn_url_retry_backoff` seconds (default 1s).
- **429 special backoff:** Fixed 30-second sleep (regardless of attempt number) — once TorBox rate-limits, aggressive retries would make it worse.
- **Non-retryable or exhausted:** Call `recordTorrentFailure(itemID)` (see circuit breaker below), return error.

### Circuit Breaker (`recordTorrentFailure`)

Per-item (torrent or usenet) failure tracker stored in `s.torrentFailures`:

- **Data:** `torrentFailureTracker{failures []time.Time, staleUntil time.Time}`
- **Sliding window:** Failures older than `CircuitBreakerWindowSec` (default 60s) are pruned before counting.
- **Threshold:** `CircuitBreakerFailures` failures within the window (default 5).
- **Trip:** When threshold is reached, `staleUntil = now + CircuitBreakerStaleMin` (default 5 minutes). A warning is logged with full context.
- **Auto-reset:** After the stale period expires, the next `isTorrentStale()` call cleans up the tracker.
- **Map limits:** `circuit_breaker_max_entries` (default 2000). Swept every cleanup interval — excess entries are evicted by oldest `staleUntil`.

### CDN Proxy (`streamFileContent`)

1. **Get/refresh CDN URL.** If no cached URL, fetch via throttle queue. Cache with `CDNTtlMinutes` TTL (default 120) if set.

2. **Range parsing.** `parseRange(rangeHeader, file.Size)`:
   - Format: `"bytes=start-end"` or suffix `"bytes=-N"` (last N bytes)
   - Single range only (rclone uses single ranges)
   - Returns `httpRange{Start, End, Length}` or `416 Range Not Satisfiable`
   - No Range header → full file range `{0, file.Size-1, file.Size}`

3. **Proxy request.** `http.Client{Timeout: 30s}` → `GET cdnURL` with `Range: bytes=start-end`.

4. **Response handling:**
   - **403/404 + `cdn_url_auto_repair=true` + retries remaining** (`cdn_url_repair_retries`, default 2):
     - Re-fetch CDN URL from TorBox via `fetchCDNURL()`
     - Update cached URL with new TTL
     - Retry the proxy request with the fresh URL
   - **429 or 5xx (transient CDN error):**
     - Invalidate cached CDN URL (set to "" with past expiry)
     - Enter hang/poll mode (see below)
   - **Other non-2xx:** `502 Bad Gateway` to client
   - **403/404 exhausted (no retries remaining):** File key is added to the negative cache with `NegativeCacheTTLSeconds` TTL. Subsequent requests for this file skip the API and enter hang/poll mode directly, preventing retry storms.
   - **200/206:** Proceed to streaming

5. **CDN connection semaphore.** Before proxy streaming, `AcquireCDNConn()` blocks until a slot is available. `ReleaseCDNConn()` returns the slot when done. Capacity: `max_cdn_connections` (default 4). Channel-based with pre-filled tokens.

6. **Streaming.** Set response headers:
   - `Content-Type`: from file's MIME type, or `"application/octet-stream"`
   - `Content-Length`: range length
   - `Accept-Ranges: bytes`
   - For ranged requests: `Content-Range: bytes start-end/size` + `206 Partial Content`
   - For full file: `200 OK`
   - `io.Copy(w, proxyResp.Body)` — streams CDN → client
   - Copy errors (context cancelled, broken pipe, connection reset) are logged at DEBUG only — these are normal client-side disconnects from Plex seeking or buffering

7. **Retry exhaustion.** If all CDN repair retries are exhausted without success, returns `502 Bad Gateway` with "CDN proxy error after retries".

### Hang/Poll Mode (`handleGetCDNHang`)

Entered when the CDN URL cannot be obtained (API failure, circuit breaker trip) or the CDN returns transient errors (429/5xx). The goal is to avoid returning an error to rclone, which counts errors toward `maxErrorCount=10`:

1. **Immediate success headers.** Send `200 OK` or `206 Partial Content` with full response headers (Content-Type, Content-Length, Accept-Ranges, Content-Range) BEFORE any data is available. This makes rclone think the connection succeeded (it will wait for data).

2. **Poll loop.** `time.NewTicker(cdnPollInterval)` — polls `fetchCDNURL()` every 15 seconds:
   - URL recovered → cache it, proxy data from CDN, exit
   - Still unavailable → `select { case <-r.Context().Done(): cleanup and return | case <-ticker.C: continue }`
   - Client disconnect → clean exit (context cancelled), logged at DEBUG

3. **Data proxy after recovery.** Acquire CDN connection slot, proxy GET with content range, `io.Copy` to client.

### Byte-Range Parsing (`parseRange`)

- Supports single-range format only: `"bytes=start-end"`
- Supports suffix ranges: `"bytes=-N"` (last N bytes)
- Validates bounds: `start > end || start < 0 || end >= fileSize` → error
- Returns `httpRange{Start, End, Length}` or error for `416 Range Not Satisfiable`

### Negative Cache Management

- **Key format:** `"torrent:itemID:fileID"` or `"usenet:itemID:fileID"`
- **Entry:** `negativeCacheEntry{err error, expiresAt time.Time}`
- **TTL:** `negative_cache_ttl_seconds` (default 30s, range 1–300)
- **Sweep:** Every `cleanup_interval_seconds` (default 60s), expired entries are removed. If `len > negative_cache_max_entries`, the oldest entries (by expiry time) are evicted first via selection sort.
- **Max entries:** `negative_cache_max_entries` (default 5000, range 100–50000)


## 4. TorBox API Client

**Key files:** `internal/torbox/client.go`, `internal/throttle/queue.go`

### Client

`NewClient(apiKey)` → base URL hardcoded `"https://api.torbox.app"`, HTTP client with 30-second timeout. Test hooks available: `SetBaseURL()` and `SetHTTPClient()` — exported but documented as test-only (not for production use).

### API Response Envelope

Every TorBox API response follows the shape:

```go
type apiResponse[T any] struct {
    Data    T        `json:"data"`
    Success *bool    `json:"success,omitempty"`
    Detail  *string  `json:"detail,omitempty"`
    Error   *string  `json:"error,omitempty"`
}
```

The generic type parameter allows type-safe decoding. All endpoints parse into `apiResponse[T]` after reading the full response body.

### Endpoints

| Method | Path | Auth Mechanism | Client Method | Purpose |
|--------|------|----------------|---------------|---------|
| GET | `/v1/api/torrents/mylist` | `Authorization: Bearer <key>` | `ListTorrents()` | List all torrents with file trees |
| GET | `/v1/api/usenet/mylist` | `Authorization: Bearer <key>` | `ListUsenet()` | List all Usenet downloads |
| GET | `/v1/api/torrents/requestdl` | Query param `?token=<key>` | `GetDownloadURL()` | Get CDN download URL for a torrent file |
| GET | `/v1/api/usenet/requestdl` | Query param `?token=<key>` | `GetUsenetDownloadURL()` | Get CDN download URL for a Usenet file |
| GET | `/v1/api/user/me` | `Authorization: Bearer <key>` | `GetUserInfo()` | Get authenticated user's account details |

### IsRetryable

`IsRetryable(err error) bool` is an exported function that checks whether a TorBox
API error is likely transient and worth retrying with exponential backoff. It returns
`true` for:

- HTTP 429 (rate limit)
- HTTP 5xx (server errors)
- `context deadline exceeded` and `Client.Timeout`
- Non-JSON responses (`invalid character '<'`) — Cloudflare error pages returned with HTTP 200
- Network errors: `connection refused`, `no such host`, `i/o timeout`, `EOF`

Returns `false` for `nil` errors, 4xx (except 429), and application-level API errors.
Used by both the CDN URL fetch pipeline (`getCDNURLWithRetry`) and the metadata sync
worker to decide whether to retry a failed API call.

### Auth Token Asymmetry

- **`/mylist` and `/user/me`:** API key sent as `Authorization: Bearer <key>` HTTP header.
- **`/requestdl`:** API key sent as `?token=<key>` query parameter (TorBox's documented design). The `redirect` query parameter (`true`/`false`) is always set to `false` — Warpbox proxies the response rather than following the redirect.

### List Endpoints

Both `ListTorrents` and `ListUsenet` use `listGeneric[T]`:

```go
func (c *Client) listGeneric(ctx, endpoint, label, params) ([]Torrent, error)
```

- Builds URL with `bypass_cache`, `offset`, `limit` query parameters.
- Sends Bearer token via Authorization header.
- Returns Torrent struct (Usenet API returns the same JSON shape).
- Params: `ListFilesParams{BypassCache bool, Offset int, Limit int}`

### Download URL Endpoints

`GetDownloadURL(ctx, torrentID, fileID, redirect bool)`:
- Builds URL with query params: `token=<key>`, `torrent_id=<id>`, `file_id=<id>`, `redirect=<bool>`.
- Returns `apiResponse[string]` where Data is the CDN URL.

`GetUsenetDownloadURL(ctx, usenetID, fileID, redirect bool)` — same pattern with `usenet_id` instead of `torrent_id`.

### Types

**`Torrent`** (25 fields including): `ID int64`, `AuthID string`, `Name string`, `Hash string`, `Size int64`, `DownloadState string`, `DownloadPresent bool`, `Files []TorrentFile`, `CreatedAt string`, `UpdatedAt string`, `ExpiresAt string`, `DownloadFinished bool`, etc.

**`TorrentFile`** (7 fields): `ID int64`, `Name string`, `Size int64`, `MimeType string`, `S3Path string`, `ShortName string`, `MD5 *string`.

**`UserInfo`** (15 fields): `ID int64`, `AuthID string`, `Email string`, `Plan int`, `PlanName string`, `Premium bool`, `PremiumExpires *string`, `CreatedAt string`, `UpdatedAt string`, `ReferralCode string`, `Registered bool`, `PremiumDownloadLimit int64`, `TotalDownloaded int64`, `TotalEgressed int64`, `OverallRatio float64`.

### Internal `do()` Helper

The core request executor:

1. `c.httpClient.Do(req)` — network error returns `"torbox: request failed"`
2. `io.ReadAll(resp.Body)` — body always closed via `defer resp.Body.Close()` before returning
3. **429 detection:** If `resp.StatusCode == 429`, calls `c.HTTP429Callback()` (wired in main.go to `throttleQueue.Record429()`) — BEFORE returning the error
4. **Non-200:** Logs a warning with status, URL path (not full URL — avoids leaking API key in query params), and truncated body (512 bytes max). Returns `fmt.Errorf("torbox: unexpected status %d", code)`
5. **200:** Returns body bytes

### Error Handling

- Network errors → `"torbox: request failed: <error>"`
- Non-200 status → `"torbox: unexpected status <code>"` (body logged at WARN, truncated to 512 bytes)
- API-level errors → `"torbox <endpoint> API error: <detail>"` (from `env.Error` field)
- **Non-JSON responses with HTTP 200:** When `json.Unmarshal` fails and the response body starts with `<` (indicating an HTML error page, e.g. Cloudflare), a WARN log is emitted with `"expected JSON, got non-JSON response"` and a 200-byte body preview. The full truncated body is logged at DEBUG.
- URL query parameters (containing the API token) are NOT included in log messages — only `req.URL.Path` is logged


## 5. Metadata Sync

**Key files:** `internal/metadata/sync.go`, `internal/metadata/store.go`

### Sync Worker Lifecycle

`SyncWorker` manages the periodic TorBox → SQLite synchronisation loop:

- **`NewSyncWorker(store, client, queue, interval, limit, retryAttempts, retryBackoff)`** — stores references. `retryAttempts` (default 3) controls how many times each API call is retried on transient failures. `retryBackoff` (default 1s) is the base exponential backoff duration. Does not start.
- **`Start(ctx)`** — stores `ctx` as `parentCtx`, creates a derived `cancelCtx`, calls `runLoop(ctx)`, closes `loopDone` channel on exit.
- **`Stop()`** — calls the cancel function on the current loop, waits up to 90 seconds for `loopDone` to close. Safe to call multiple times or before `Start`.
- **`Restart()`** — calls `Stop()`, creates a new derived context from `parentCtx`, launches `runLoop` in a new goroutine.
- **`SyncNow()`** — creates a fresh background context with 60-second timeout, calls `syncOnce(ctx)` synchronously.
- **`Status()`** — returns `SyncStatus{LastSuccess time.Time, LastError string}`.

### Sync Loop (`runLoop`)

1. Immediate first sync: `syncOnce(ctx)` runs on start before the first tick.
2. `time.NewTicker(interval)` — fires every `sync.interval_minutes` (default 5).
3. Each tick calls `syncOnce(ctx)`.
4. Context cancellation → clean exit.

### Sync Cycle (`syncOnce`)

**1. Snapshot for change detection.** If `OnItemsAdded` or `OnItemsRemoved` hooks are configured, `store.ListItemDirs()` is called before the sync to capture the current item set.

**2. Parallel fetch with retry.** Two API calls are enqueued via the throttle queue simultaneously. Each call uses exponential backoff retry on transient errors:
   - `ListTorrents(ctx, ...)` — retries up to `sync.retry_attempts` times with backoff `sync.retry_backoff * 1s, * 2s, * 4s, ...`
   - `ListUsenet(ctx, ...)` — same retry pattern
   - Only transient errors (defined by `torbox.IsRetryable()`) trigger a retry — non-retryable errors (401, 404, API-level errors) bail immediately
   - Retry is gated by the caller's context timeout (60s for `SyncNow()`), not a fixed wall-clock limit
   - Results collected on channels. Errors are logged but do not abort — usenet may succeed if torrents fail.

**3. Sync tag reservation.** `store.GetNextSyncTag()` atomically increments a counter in the `meta` table:
   - `INSERT OR IGNORE INTO meta VALUES ('sync_tag', '0')` — ensures the row exists.
   - `UPDATE meta SET value = CAST(value AS INTEGER) + 1 RETURNING CAST(value AS INTEGER)` — atomically increments and returns the new tag. This tag identifies all records from this sync cycle.

**4. Torrent flattening.** Iterate torrents returned by the API:
   - Skip if `DownloadState != "cached"` AND `!DownloadPresent` (only shows cached/available items).
   - Skip torrents with zero files (log at DEBUG).
   - For each file in the torrent: `buildFileRecord(torrentID, file, syncTag, SourceTorrent, createdAt)` → `store.UpsertFile(rec)`.

**5. Usenet flattening.** Same iterative pattern with `SourceUsenet`.

**6. Prune.** `store.PruneBySyncTag(syncTag)`:
   - Deletes all records where `sync_tag != currentTag` OR `sync_tag = 0` (legacy/unsynced).
   - Batched in 250-row chunks (`DELETE ... WHERE id IN (SELECT id FROM files WHERE ... LIMIT 250)`) to avoid holding the SQLite writer lock for too long.
   - Always runs on partial success too — avoids accumulating orphaned entries.

**7. Change detection.** After pruning, `ListItemDirs()` is called again to get the new item set. The old and new sets are compared by `(ItemID, Source)` key:
   - Items in new but not old → `OnItemsAdded(dirNames)`
   - Items in old but not new → `OnItemsRemoved(dirNames)`
   - Hooks run synchronously within the sync cycle.

**8. GC tracking.** Records GC cycles that occurred during sync (DEBUG level only).

**9. Status update.** Sets `lastError` (nil or most significant error) and `lastSuccess` (current time if no error).

### File Record Building (`buildFileRecord`)

The `s3_path` from TorBox has the format `"<hash>/<path>"`. Path derivation rules:

- **Multi-file torrent** (s3_path has two or more slashes after the hash, e.g. `"abc123/Movies/Title.mkv"`): virtual path = `"Movies/Title.mkv"` — each segment is sanitized.
- **Single-file torrent** (s3_path has exactly one more segment after the hash, e.g. `"abc123/Title.mkv"`): virtual path = `"Title.mkv"` — placed directly at root level without a wrapper directory.
- **No slash in s3_path fallback:** virtual path = `sanitize(ShortName)`.

### Path Sanitization (`sanitizePathSegment`)

Characters `\ / : * ? " < > | &` are replaced with `_`. All other characters (including valid Unicode, spaces, dots, hyphens) are preserved. The ampersand `&` is sanitized because it can cause filesystem path issues on some systems and is stripped by the official TorBox WebDAV.

### Change Hooks

Config keys `library.on_items_added` and `library.on_items_removed` specify shell commands. When the sync worker detects items have appeared or disappeared, it calls `runItemsHook(command, timeoutSec, items)` in `main.go`, which:
- Splits command by whitespace: first token = executable, rest = static args
- Appends item directory names as trailing positional arguments
- `exec.CommandContext(timeoutCtx, cmd, args..., items...)`
- Timeout from `hook_timeout_seconds` (default 30, range 1–3600)
- Exceeding timeout → warning log, not fatal
- Stdout+stderr captured via `CombinedOutput()` for error diagnostics


## 6. SQLite Store

**Key files:** `internal/metadata/store.go`, `internal/metadata/stats.go`

### Database Connection

```go
sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_cache_size=-8192")
```

- **WAL mode:** High concurrent read performance without blocking writers. Writers can proceed while readers read the old snapshot.
- **Busy timeout:** 5000ms (5 seconds) — SQLite will retry locked statements for up to 5 seconds before returning an error.
- **Cache size:** -8192 pages → 8 MB page cache (negative means kilobytes).

### Schema (v0 — applied via `CREATE IF NOT EXISTS`)

```sql
CREATE TABLE IF NOT EXISTS files (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id         INTEGER NOT NULL DEFAULT 0,
    file_id         INTEGER NOT NULL DEFAULT 0,
    source          INTEGER NOT NULL DEFAULT 0,  -- 0=torrent, 1=usenet
    name            TEXT    NOT NULL,
    path            TEXT    NOT NULL UNIQUE,       -- virtual path, derived from s3_path
    size            INTEGER NOT NULL DEFAULT 0,
    mime_type       TEXT    NOT NULL DEFAULT '',
    cdn_url         TEXT    NOT NULL DEFAULT '',
    cdn_url_expires TEXT    NOT NULL DEFAULT '',  -- RFC 3339 UTC timestamp
    created_at      TEXT    NOT NULL DEFAULT '',
    sync_tag        INTEGER NOT NULL DEFAULT 0,   -- sync batch ID; 0 = legacy
    updated         TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_files_path ON files(path);
CREATE INDEX IF NOT EXISTS idx_files_source_file_id ON files(source, file_id);
CREATE INDEX IF NOT EXISTS idx_files_sync_tag ON files(sync_tag);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS stats (
    timestamp TEXT NOT NULL DEFAULT (datetime('now')),
    metric    TEXT NOT NULL,
    value     REAL NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_stats_metric_time ON stats(metric, timestamp);
```

### Migration Strategy

`CREATE TABLE IF NOT EXISTS` — migrations are additive only. There are no versioned migrations, downgrade paths, or schema version tracking. Schema changes must be backward-compatible (add tables, add optional columns). The `meta` table stores key-value pairs for operational state (currently only `sync_tag` counter).

### Store API

| Method | Primary SQL | Notes |
|--------|-------------|-------|
| `UpsertFile(f)` | `INSERT ... ON CONFLICT(path) DO UPDATE SET item_id=excluded.item_id, ... all fields, sync_tag=excluded.sync_tag, updated=datetime('now')` | ON CONFLICT uses the `path` UNIQUE constraint. Updates all fields including CDN URL cache. |
| `ListDir(prefix)` | `SELECT id, item_id, file_id, source, name, path, size, mime_type, created_at FROM files WHERE path LIKE prefix||'%' ORDER BY name` | Returns all files under a virtual directory prefix. |
| `GetFileByPath(path)` | `SELECT ... FROM files WHERE path = ?` | Returns `*FileRecord` or `nil` on `sql.ErrNoRows`. |
| `GetFileByFileID(source, fileID)` | `SELECT ... FROM files WHERE source = ? AND file_id = ? LIMIT 1` | For CDN URL lookups when path-based lookup is not available. |
| `SetCDNURL(internalID, url, expiresAt)` | `UPDATE files SET cdn_url=?, cdn_url_expires=?, updated=datetime('now') WHERE id=?` | Retries up to 3 times with exponential backoff (100ms, 200ms, 400ms) if the database is locked. Increments `dbLockErrors` counter on lock failures. |
| `GetCDNURL(internalID)` | `SELECT cdn_url, cdn_url_expires FROM files WHERE id = ?` | Returns `""` if: row not found, url is empty, or expiry time is in the past (RFC 3339 UTC comparison). |
| `ListItemDirs()` | `SELECT DISTINCT item_id, source, CASE WHEN instr(path,'/')>0 THEN substr(...) ELSE path END FROM files` | Returns all distinct top-level directories for change detection. |
| `CountFiles()` | `SELECT COUNT(*) FROM files` | Landing page display. |
| `GetItemIDByFileID(source, fileID)` | `SELECT item_id FROM files WHERE source=? AND file_id=? LIMIT 1` | Maps file → parent item for CDN URL generation. |
| `GetNextSyncTag()` | `INSERT OR IGNORE INTO meta(key,value) VALUES('sync_tag','0'); UPDATE meta SET value=CAST(value AS INTEGER)+1 RETURNING CAST(value AS INTEGER)` | Atomic counter increment using SQLite's `RETURNING` clause (requires SQLite 3.35+). |
| `PruneBySyncTag(tag)` | `DELETE FROM files WHERE id IN (SELECT id FROM files WHERE sync_tag != ? OR sync_tag = 0 LIMIT 250)` | Batched 250 rows at a time. Returns total rows deleted across all batches. Refuses to run with tag <= 0. |
| `RecordStats(metrics)` | Batch `INSERT INTO stats (timestamp, metric, value) VALUES (?, ?, ?)` in a transaction | See Section 7. |
| `PruneStats(retention)` | `DELETE FROM stats WHERE timestamp < ?` | See Section 7. |

### CDN URL Caching

- CDN URLs are stored in the `files` table alongside file metadata (columns `cdn_url`, `cdn_url_expires`).
- Expiry is stored as an RFC 3339 UTC timestamp string.
- `GetCDNURL` checks `time.Now().UTC().After(expiryTime)` — if expired, returns `""`.
- Setting `cdn_url = ""` with a past expiry is the mechanism for invalidating a cached URL on transient errors.
- TTL is controlled by `cache.cdn_url_ttl_minutes` (default 120, range 1–1440). A value of 0 disables caching.

### Lock Error Handling

`isLockedError(err)` detects `"database is locked"`, `"SQLITE_BUSY"`, or `"SQLITE_LOCKED"` in the error string. The function is used in:

- `SetCDNURL` — retries 3 times with 100/200/400ms backoff
- `UpsertFile` — increments `dbLockErrors` counter
- `PruneBySyncTag` — increments counter, returns error

The `dbLockErrors` counter (atomic.Int64) is exposed on the landing page and recorded in stats as a per-interval delta.


## 7. Stats & Observability

**Key files:** `internal/metadata/stats.go`, `internal/server/server.go` (recordStats), `internal/server/landing.go`, `internal/server/logs.go`

### Stats Table Schema

```sql
CREATE TABLE IF NOT EXISTS stats (
    timestamp TEXT NOT NULL DEFAULT (datetime('now')),
    metric    TEXT NOT NULL,
    value     REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_stats_metric_time ON stats(metric, timestamp);
```

- Timestamp format: `"2006-01-02 15:04:05"` (SQLite datetime, always UTC).
- Metric names are strings. Values are 64-bit floats.
- The composite index on `(metric, timestamp)` supports efficient per-metric time-range queries.

### Recording (`recordStats`)

Runs on an independent ticker in the cleanup goroutine (`startCleanupLoop`), driven by `stats.interval_seconds` (default 60s) — separate from the cache sweep interval. The cleanup loop lifecycle:

1. `time.NewTicker(cleanupInterval)` — cache sweeps every `cleanup_interval_seconds` (default 60s)
2. `time.NewTicker(statsInterval)` — stats recording every `stats.interval_seconds` (default 60s)
3. Each cleanup tick: `sweepNegativeCache()` → `sweepCircuitBreaker()`
4. Each stats tick: `recordStats()` → `PruneStats(retention)` (if configured)
5. Stopped by closing `cleanupStopCh` (used by tests)

**Delta computation.** Counter metrics (success, fail, 429, lock errors, GC cycles) are recorded as per-interval deltas, not cumulative totals. The previous value is subtracted from the current value to get the rate:

```go
dSuccess := throttleStats.SuccessfulCalls - s.prevSuccessfulCalls
s.prevSuccessfulCalls = throttleStats.SuccessfulCalls
```

This means charts show call rate per interval, not monotonically increasing totals.

**Metrics recorded per interval:**

| Metric | Type | Source | Description |
|--------|------|--------|-------------|
| `api_calls_success` | Delta (count) | `throttle.Queue.Stats().SuccessfulCalls` | Successful TorBox API calls in the last interval |
| `api_calls_failed` | Delta (count) | `throttle.Queue.Stats().FailedCalls` | Failed API calls in the last interval |
| `api_calls_429` | Delta (count) | `throttle.Queue.Stats().HTTP429Calls` | 429 rate-limit responses in the last interval |
| `db_lock_errors` | Delta (count) | `s.store.DBLockErrors()` | SQLite lock errors in the last interval |
| `gc_cycles` | Delta (count) | `runtime.ReadMemStats(&mem).NumGC` | Go GC cycles in the last interval |
| `sys_mb` | Gauge | `mem.Sys / 1024 / 1024` | Total OS memory allocated by the Go runtime (MB) |
| `alloc_mb` | Gauge | `mem.Alloc / 1024 / 1024` | Heap memory in use (MB) |
| `heap_objects` | Gauge | `mem.HeapObjects` | Number of live heap objects |
| `negative_cache_entries` | Gauge | `s.NegativeCacheSize()` | Current entries in the negative cache map |
| `circuit_breaker_entries` | Gauge | `s.CircuitBreakerSize()` | Current entries in the circuit breaker map |

### Stats Recording (SQL)

`RecordStats(metrics map[string]float64)`:
1. Begin transaction
2. Prepare `INSERT INTO stats (timestamp, metric, value) VALUES (?, ?, ?)` statement
3. For each metric: `stmt.Exec(now, metric, value)` where `now` is UTC `"2006-01-02 15:04:05"`
4. Commit transaction

### Pruning

`PruneStats(retention time.Duration)`: `DELETE FROM stats WHERE timestamp < cutoff` where cutoff is `time.Now().UTC().Add(-retention)`. Runs every stats interval when `cfg.StatsRetentionHours > 0`. Returns number of rows deleted. Default retention: 24 hours.

### Stats Query Endpoints

- **`QueryStats(metric, since)`** — single metric, ordered by timestamp ascending.
- **`QueryAllStatsSince(since)`** — all metrics since `since`, ordered by `metric, timestamp`. Returns `map[string][]StatsRecord`.
- **`GetMetricValuesSince(metric, since)`** — convenience wrapper returning `[]float64` for sparkline charts.
- **`GetAllMetricValuesSince(since)`** — returns `map[string][]float64`.

### JSON Stats Endpoint (`/stats.json`)

- **Auth required** (see Section 9).
- Accepts optional `?minutes=N` query parameter (default from `stats.chart_minutes`, default 60).
- Calls `QueryAllStatsSince(now - minutes)` and groups by metric.
- Returns `map[string][]{t: "RFC3339 UTC", v: number}`:
```json
{
  "api_calls_success": [{"t": "2026-01-15T10:30:00Z", "v": 5}, ...],
  "sys_mb": [{"t": "2026-01-15T10:30:00Z", "v": 42}, ...],
  ...
}
```

### Chart.js Frontend

The landing page HTML template (`internal/server/landing.html`, embedded via `//go:embed`) includes Chart.js (also embedded at compile time via `//go:embed chart.umd.min.js` and served locally at `/chart.umd.min.js`). It fetches `/stats.json` on page load and renders sparkline charts for each metric using `<canvas>` elements. Charts show the last N minutes of data (configurable via `stats.chart_minutes`).

### Log Ring Buffer

**`ringBufferHandler`** implements `slog.Handler` and wraps the real handler:

- **Capacity:** 1000 lines (`ringBufferCapacity`)
- **Storage:** Fixed-size array `[1000]string` with circular index (`pos`) and `full` flag.
- **Handle:** Calls `formatRecord(&r)` to produce a single-line string → stores at `buf[pos]` → advances `pos` → delegates to inner handler.
- **Lines(n):** Returns last `n` lines in reverse chronological order (newest first). Handles wraparound correctly by iterating backward from `pos-1`.
- **Format:** `"2006-01-02T15:04:05.000Z07:00 LEVEL message key=value ..."` — produced by `formatRecord(r)` which iterates `r.Attrs()`.

**Log viewer (`/logs/`):**
- Auth required.
- HTML page with monospace log output, dark theme.
- Returns last 200 lines via `globalLogBuffer.Lines(200)`.
- Content HTML-escaped for safe browser rendering.

**Log level control:** `slog.LevelVar` shared between handler options and server config. The `/actions/loglevel` endpoint calls `LevelVar.Set(parsedLevel)` — atomic, immediate effect across all loggers. Also persisted to config file.

### Cleanup Loop

The cleanup goroutine is started in `server.New()` and runs until `cleanupStopCh` is closed:

```go
func (s *Server) startCleanupLoop() {
    interval := time.Duration(s.cfg.CleanupIntervalSeconds) * time.Second
    go func() {
        ticker := time.NewTicker(interval)
        for {
            select {
            case <-ticker.C:
                s.sweepNegativeCache()
                s.sweepCircuitBreaker()
                s.recordStats()
                // Prune stats if retention is configured
                if s.statsRetention > 0 {
                    s.store.PruneStats(s.statsRetention)
                }
            case <-s.cleanupStopCh:
                return
            }
        }
    }()
}
```


## 8. Configuration

**Key files:** `internal/config/config.go`, `config.yml.example`

### Config File Format

YAML. Parsed with `gopkg.in/yaml.v3` (preserves comments on round-trip). The config file is loaded from the path specified by the `-config` flag. If the file doesn't exist at startup, `config.yml.example` (located in the same directory) is copied to the config path with a warning message.

### Configuration Sections (9)

#### 1. `torbox`
| Key | Type | Default | Validated | Description |
|-----|------|---------|-----------|-------------|
| `api_key` | string | **Required** | non-empty | TorBox API key for authentication |

#### 2. `server`
| Key | Type | Default | Validation | Description |
|-----|------|---------|------------|-------------|
| `listen_addr` | string | `":1412"` | — | WebDAV server listen address |
| `enable_pprof` | bool | `false` | — | Enable `/debug/pprof/` endpoints |

#### 3. `cache`
| Key | Type | Default | Validation | Description |
|-----|------|---------|------------|-------------|
| `cdn_url_ttl_minutes` | int | `120` | 1–1440 | CDN URL cache TTL |
| `cdn_url_auto_repair` | bool (pointer) | `true` | — | Auto-repair stale CDN URLs on 403/404 |
| `cdn_url_repair_retries` | int (pointer) | `2` | 0–10 | Max CDN proxy retries per request |
| `cdn_url_retry_backoff` | int (pointer) | `1` | 1–60 | CDN URL fetch retry backoff base (seconds) |
| `cdn_url_retry_attempts` | int (pointer) | `1` | 0–10 | Max CDN URL fetch retry attempts |
| `negative_cache_ttl_seconds` | int (pointer) | `30` | 1–300 | CDN error cache TTL |
| `circuit_breaker_failures` | int (pointer) | `5` | 1–100 | Failures before trip |
| `circuit_breaker_window_seconds` | int (pointer) | `60` | 1–3600 | Sliding window for counting failures |
| `circuit_breaker_stale_minutes` | int (pointer) | `5` | 1–60 | Stale duration after trip |
| `negative_cache_max_entries` | int (pointer) | `5000` | 100–50000 | Max negative cache entries |
| `circuit_breaker_max_entries` | int (pointer) | `2000` | 50–20000 | Max circuit breaker entries |
| `cleanup_interval_seconds` | int (pointer) | `60` | 10–3600 | Cache sweep interval |
| `max_cdn_connections` | int (pointer) | `4` | 1–64 | Max concurrent CDN proxy connections |

#### 4. `throttle`
| Key | Type | Default | Validation | Description |
|-----|------|---------|------------|-------------|
| `requests_per_minute` | int | `250` | 10–1000 | TorBox API rate limit |

#### 5. `logging`
| Key | Type | Default | Validation | Description |
|-----|------|---------|------------|-------------|
| `format` | string | `"text"` | `"text"` or `"json"` | Log output format |
| `level` | string | `"info"` | `"debug"`, `"info"`, `"warn"`, `"error"` | Log level |

#### 6. `sync`
| Key | Type | Default | Validation | Description |
|-----|------|---------|------------|-------------|
| `interval_minutes` | int | `5` | 1–1440 | Metadata sync interval |
| `limit` | int | `5000` | 1–100000 | Max files per sync cycle |
| `list_page_size` | int | `5000` | 1–10000 | Per-request page window when paginating mylist API calls |
| `retry_attempts` | int (pointer) | `3` | 0–10 | Max sync API retry attempts on transient errors (0 = no retry) |
| `retry_backoff` | int (pointer) | `1` | 1–60 | Sync retry exponential backoff base (seconds) |

#### 7. `stats`
| Key | Type | Default | Validation | Description |
|-----|------|---------|------------|-------------|
| `interval_seconds` | int | `60` | 10–3600 | Stats snapshot interval |
| `retention_hours` | int | `24` | 1–720 | Stats retention period |
| `chart_minutes` | int | `60` | 1–1440 | Chart time window on landing page |

#### 8. `auth`
| Key | Type | Default | Validation | Description |
|-----|------|---------|------------|-------------|
| `enabled` | bool | `false` | — | Enable HTTP Basic Auth for web UI |
| `username` | string | `"admin"` | — | Auth username |
| `password` | string | `""` | required when enabled | Auth password (plaintext) |

#### 9. `library`
| Key | Type | Default | Validation | Description |
|-----|------|---------|------------|-------------|
| `virtual_paths` | array | `[]` | unique names, no `/`, no `__all__` | Virtual path filters |
| `on_items_added` | string | `""` | — | Shell command for new items |
| `on_items_removed` | string | `""` | — | Shell command for removed items |
| `hook_timeout_seconds` | int | `30` | 1–3600 | Hook execution timeout |

Each virtual path entry:
| Key | Type | Validation |
|-----|------|------------|
| `name` | string | Required, no `/`, unique (the reserved name `__all__` is silently accepted but filtered out at runtime) |
| `directory_include` | string | Must compile as regex |
| `directory_exclude` | string | Must compile as regex |
| `file_regex` | string | Must compile as regex |
| `largest_file_only` | bool | — |

### Pointer-to-Int Pattern

Several `cache` config keys use `*int` or `*bool` with nil → default semantics:

```go
type CacheConfig struct {
    NegativeCacheTTLSeconds *int  `yaml:"negative_cache_ttl_seconds"`
}
```

This allows the config file to omit the key entirely (nil → default) while explicitly setting `0` would be treated as "use default" (via `setDefaults`). This was done because YAML's zero-value semantics make it impossible to distinguish "not set" from `0` for plain `int` fields.

### Load Pipeline (`Load(path)`)

1. `os.ReadFile(path)` → error if file doesn't exist or unreadable
2. `yaml.Unmarshal(data, &Config{})` — populates struct, leaving missing keys at zero/nil
3. `setDefaults(cfg)` — fills in all zero-valued fields and nil pointers with defaults
4. `validate(cfg)` — runs validation (see below)
5. Returns `*Config` or error

### Validation Rules (`validate()`)

- **Required:** `torbox.api_key` must be non-empty
- **Enums:** `logging.format` → text|json; `logging.level` → debug|info|warn|error
- **Range checks:** All numeric fields have min/max bounds
- **Auth:** `auth.password` required when `auth.enabled` is true
- **Library:** Virtual path names must be unique, non-empty, no `/`, not `__all__`; regex fields must compile; hook_timeout_seconds in range 1–3600

### Runtime Log Level Update (`UpdateLogLevel`)

- Reads config file into `yaml.Node` tree to preserve comments, formatting, and structure on round-trip
- Validates the new level via `ParseLevel()`
- Updates `cfg.Logging.Level`
- Marshals back to YAML
- Writes to `path.tmp` temp file
- `os.Rename(tmpPath, path)` — atomic replacement on POSIX; on Windows, it's best-effort (deletes original first)
- Removes temp file on failure
- Called from `/actions/loglevel` handler

### Default Config Generation (`GenerateTemplate`)

- Checks if `os.Stat(path)` returns nil (file exists) → return `false, nil`
- Reads `config.yml.example` from same directory
- Writes content to `path` with 0644 permissions
- Returns `true, nil` — caller logs warning telling user to edit the file


## 9. Web UI Authentication

**Key files:** `internal/config/config.go`, `internal/server/auth.go`, `internal/server/server.go`

### Configuration

- `auth.enabled` (bool, default `false`): toggle HTTP Basic Authentication for the web management UI.
- `auth.username` (string, default `"admin"`): the expected username.
- `auth.password` (string, required when enabled): the expected password. Stored as plaintext in `config.yml` (consistent with the TorBox API key pattern — credentials are managed by the same mechanism as the upstream service).

### Middleware Design (`requireAuth`)

The `requireAuth` middleware is implemented as a method on `*Server`:

```go
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc
```

When `auth.enabled = false` (default), it returns the `next` handler unchanged — zero overhead, no branching on each request.

When `auth.enabled = true`, the returned handler:
1. Calls `r.BasicAuth()` to extract the `user` and `pass` from the `Authorization` header
2. Compares both values using `subtle.ConstantTimeCompare()` — timing-safe comparison to prevent timing attacks
3. On mismatch or missing auth: sets `WWW-Authenticate: Basic realm="warpbox"` header, returns `401 Unauthorized` with body `"Unauthorized"`
4. On match: calls `next(w, r)`

### Middleware Integration with chi

Because chi's `With()` expects a `func(http.Handler) http.Handler` middleware signature, the `requireAuth` method is adapted:

```go
requireAuth := func(next http.Handler) http.Handler {
    return s.requireAuth(next.ServeHTTP)
}
```

This adapter is applied to protected routes via `s.mux.With(requireAuth).Get(...)`.

### Protected Routes (auth required)

| Route | Purpose |
|-------|---------|
| `/` | Landing page with runtime stats and management buttons |
| `/logs/` | Log viewer |
| `/actions/*` | Management API (resync, restart-sync, loglevel) |
| `/stats.json` | Time-series metrics endpoint |
| `/http/*` | HTML file browser + CDN streaming |
| `/debug/pprof/*` | pprof profiling (only when enabled) |
| `/openapi.json` | OpenAPI specification |

### Excluded Routes (no auth)

| Route | Reason |
|-------|--------|
| `/webdav/*` | Consumed by rclone/Plex — not a web UI endpoint |
| `/infuse/*` | Consumed by Infuse on Apple devices |
| `/healthz` | Docker healthcheck / load balancer probe |
| `/warpbox.svg` | Static asset (favicon / branding) |
| `/favicon.ico` | Browser favicon |


## 10. Throttle Queue

**Key files:** `internal/throttle/queue.go`

### Design

Token-bucket rate limiter implemented as a blocking request queue. The design philosophy is: **never fail fast**. Burst traffic from Plex (scanning an entire library) is queued and trickled to the TorBox API at a safe rate. If the queue is full, `Enqueue` blocks until space is available — the HTTP handler waits, not the caller.

### Rate Calculation

```go
rate = time.Minute / time.Duration(requestsPerMinute)
```

- Default: 250 RPM → `rate = 240ms` (minimum 240ms between calls)
- Free TorBox plan limit: 300 RPM → rate must be set ≤ 300
- Config validation: 10–1000 RPM

### Queue Structure

```go
type Queue struct {
    mu         sync.Mutex
    items      chan Request       // buffered channel, capacity 1024
    rate       time.Duration      // inter-request minimum spacing
    lastCall   time.Time
    totalCalls int64
    callWindow []time.Time
    successfulCalls int64
    failedCalls     int64
    http429Calls    int64
}
```

The buffered channel (`queueBufferSize = 1024`) absorbs short bursts. If all 1024 slots are occupied, the next `Enqueue()` blocks the producer.

### Enqueue

```go
func (q *Queue) Enqueue(r Request) {
    q.items <- r
}
```

`Enqueue` sends to the channel. It blocks when the buffer is full. The `Request` carries:
- `Label` (string) — descriptive name for log messages
- `Execute` (`func(ctx context.Context) error`) — the actual API call

### Process Loop (`processLoop`)

Runs in a background goroutine started by `Start(ctx)`:

1. **Receive:** `select { case <-ctx.Done(): return | case r := <-q.items: }` — blocks on empty queue, exits on shutdown
2. **Throttle:** `elapsed := time.Since(q.lastCall)` — if less than `rate` has elapsed, `time.After(rate - elapsed)` waits (or context cancellation)
3. **Execute:** `err := r.Execute(ctx)` — runs the API call
4. **Stats update** (under lock):
   - `lastCall = time.Now()`
   - `totalCalls++`
   - On error: `failedCalls++`; logged at DEBUG only (caller owns ERROR-level logging)
   - On success: `successfulCalls++`
   - Append `lastCall` to `callWindow`
   - Trim `callWindow` to entries within the last 60 seconds (sliding window)

### Stats

```go
type Stats struct {
    TotalCalls        int64   // lifetime total
    SuccessfulCalls   int64   // lifetime successful
    FailedCalls       int64   // lifetime failed
    HTTP429Calls      int64   // lifetime 429s (counted by Record429())
    CallsLastMinute   int     // count in sliding 60s window
    RequestsPerMinute int     // configured rate (derived from rate)
}
```

`Stats()` recomputes `CallsLastMinute` on each call by scanning `callWindow` for timestamps within the last 60 seconds. This is O(n) on the window size (typically small — max 250 entries at 240ms spacing).

### 429 Detection

- TorBox client's `do()` calls `HTTP429Callback()` when `resp.StatusCode == 429`
- Wired in `main.go`: `torBoxClient.HTTP429Callback = func() { throttleQueue.Record429() }`
- `Record429()` simply increments `http429Calls` under lock.

### Stress Characteristics

- **Maximum sustained rate:** 250 RPM default, up to 1000 RPM configured.
- **Burst absorption:** 1024-item buffer allows absorbing spikes. At 240ms spacing, a full buffer represents ~4 minutes of queued work.
- **Blocking behaviour:** When buffer is full, `handleGet` (which calls `Enqueue`) blocks. Plex/jellyfin sees a slow response but not an error. This is intentional — Plex counts errors (`maxErrorCount=10`) far more severely than slow responses.
- **Concurrency:** Single goroutine processes all requests sequentially. The throttle itself is not a bottleneck (each request involves a TorBox API round-trip in the 100–500ms range).
- **Backpressure:** The `processLoop` is the only consumer. Downstream components (sync worker, GET handler) all route through `Enqueue`. There is no priority queue or request differentiation.
