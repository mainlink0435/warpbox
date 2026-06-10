![Warpbox](warpbox.png)

# Warpbox

Warpbox is a high-performance, lightweight WebDAV proxy written in Go, designed to be consumed by rclone's WebDAV backend. It mounts a cloud-hosted torrent cache (TorBox) as a native, stable local filesystem via rclone's FUSE layer. Its primary function is to act as a defensive interceptor, protecting strict upstream API limits from the aggressive scanning behaviours of media servers like Plex and Jellyfin.

```
Plex/Jellyfin → rclone (FUSE mount) → WebDAV → Warpbox → TorBox API
```

## The Problem: The API Collision

Standard mounting tools (like running rclone directly against an HTTP endpoint) act as literal translators between the operating system and the cloud provider. This creates catastrophic failures when paired with TorBox:

1. **The `ffprobe` Avalanche:** Media servers scan files by requesting specific byte ranges (the start and end of a file) to extract metadata, codecs, and sonic data. rclone faithfully translates every byte-range request into a WebDAV call to Warpbox. Every range request forces Warpbox to ask the TorBox API for a secure CDN token. A single library scan can generate hundreds of API requests in seconds, instantly breaching TorBox's strict **300 requests/minute** limit and triggering account lockouts.
2. **The Retention Trap:** TorBox relies on a 30-day inactivity timer to clear cloud storage. If a media server constantly probes every file for metadata, TorBox registers this as active "access" and resets the timer. TorBox actively monitors for this abuse and will ban accounts attempting to artificially retain data without active human playback.

## The Solution: The Warpbox Architecture

Warpbox solves this by decoupling the filesystem speed from the network speed. It lies to the media server (via rclone) to protect the upstream API.

### 1. SQLite State Mapping (Zero-API Browsing)

Warpbox periodically synchronises the TorBox directory structure into a local SQLite database (running in WAL mode for high concurrency). When rclone requests directory listings or file timestamps, Warpbox serves this data instantly from SQLite. **Cost: 0 API calls.**

### 2. Just-In-Time (JIT) RAM Buffering

Warpbox distinguishes between metadata scans and human playback based on byte-range requests. When a media server (via rclone) requests the first 500 KB of a file:

* Warpbox requests a secure CDN link and downloads a larger chunk (e.g., 16 MB) directly into the server's RAM.
* It serves the 500 KB to rclone instantly.
* When rclone subsequently asks for the next few megabytes, Warpbox serves it directly from the RAM buffer.
* Unused chunks evaporate from RAM after a configurable TTL.

### 3. The Blocking Throttle

Warpbox never fails fast. If a user imports files and rclone propagates 200 simultaneous reads, Warpbox intercepts all 200 requests, places them in a blocking queue, and trickles the API calls to TorBox at a safe, configured rate (e.g., 4 requests per second). rclone simply perceives a slow mechanical hard drive; Plex does not crash, and the TorBox API remains secure.

### 4. Smart Playback Handoff

When Warpbox detects a request for a byte range deep within the file (indicating active human playback rather than a header scan), it establishes a continuous stream from the TorBox CDN, or issues an HTTP 302 redirect to offload bandwidth entirely, depending on the configuration.

## Technical Specifications

* **Language:** Go (Golang)
* **Configuration:** Exclusively managed via a declarative `config.yml`
* **Dependencies:** Minimal footprint; relies primarily on standard Go libraries (`net/http`, `log/slog`).

## Recommended rclone Configuration

Mount Warpbox as a local filesystem using rclone's WebDAV backend. Below are the recommended flags for optimal performance and stability.

### Basic mount command

```
rclone mount warpbox: /mnt/warpbox \
  --webdav-url http://localhost:1412/webdav/ \
  --webdav-vendor other \
  --daemon
```

For a quick one-liner (Windows or single-line shell):
```
rclone mount warpbox: Z: --webdav-url http://localhost:1412/webdav/ --webdav-vendor other --buffer-size 16M --vfs-cache-mode full --vfs-read-ahead 0 --vfs-cache-max-age 720h --transfers 2 --checkers 8 --no-checksum --timeout 60s --contimeout 30s --low-level-retries 3
```

### Recommended flags

| Flag | Recommended Value | Why |
|---|---|---|
| `--buffer-size` | `16M` | Matches Warpbox's default chunk size. One rclone buffer = one Warpbox chunk. Smaller values cause more CDN round-trips; larger causes double-buffering waste. |
| `--vfs-cache-mode` | `full` | **Required for disk caching.** Warpbox only does JIT read-ahead into RAM (512 MB, 30-second TTL). It does NOT persist files to disk. Without `full` mode, you will re-download the entire file every time you scrub or seek back. rclone's VFS disk cache is the *only* persistent cache. |
| `--vfs-read-ahead` | `0` | Warpbox already fetches 16 MB ahead per chunk. rclone pre-fetching would leapfrog the buffer and trigger unnecessary CDN URL requests. |
| `--vfs-cache-max-age` | `720h` (30 days) | Warpbox's 30-second RAM TTL and rclone's VFS cache age are unrelated. Set this to match TorBox's retention window (30 days) so cached files persist locally. |
| `--transfers` | `2` | Conservative concurrency. Files download once in `full` mode, so 2 concurrent transfers won't overwhelm the throttle. |
| `--checkers` | `8` | PROPFIND queries hit SQLite directly (zero API calls). More checkers mean faster library scans with no downside. |
| `--no-checksum` | (use flag) | Prevents rclone from reading every file to compute checksums, which would trigger CDN URL requests. |
| `--timeout` | `60s` | Covers throttle queue delays under load. |
| `--contimeout` | `30s` | Connection timeout for the initial WebDAV handshake. |
| `--low-level-retries` | `3` | CDN proxying may transiently fail; rclone should retry before reporting an error. |

### Full example (Linux/macOS with daemon)

```
rclone mount warpbox: /mnt/warpbox \
  --webdav-url http://localhost:1412/webdav/ \
  --webdav-vendor other \
  --buffer-size 16M \
  --vfs-cache-mode full \
  --vfs-read-ahead 0 \
  --vfs-cache-max-age 720h \
  --transfers 2 \
  --checkers 8 \
  --no-checksum \
  --timeout 60s \
  --contimeout 30s \
  --low-level-retries 3 \
  --daemon
```

### Deprecated flags (removed)

The following flags were present in earlier versions of this README but have been removed because they were incorrect or counterproductive:

| Flag | Why it was removed |
|---|---|
| `--max-stats-groups 0` | Setting this to 0 breaks rclone. Stats groups are internal memory accounting, not network calls. The claim that it "reduces background PROPFIND calls" was factually wrong. |
| `--vfs-read-wait 100ms` | Only applies when the VFS cache has stale entries. With `full` mode and a long `--vfs-cache-max-age`, the cache is never stale. Dead flag. |
| `--vfs-cache-mode writes` | Warpbox is read-only; no writes ever occur. `writes` mode provides zero disk caching. Only `full` mode gives you persistent file caching for seeking and scrubbing. |

## TorBox Terms of Service Compliance

Warpbox is designed to operate within TorBox's Terms of Service, available at [github.com/TorBox-App/hosted-terms_of_service](https://github.com/TorBox-App/hosted-terms_of_service).

- **Rate limiting:** Warpbox's throttle queue enforces a configurable rate limit (default 250 requests/minute) to stay below TorBox's 300 RPM limit. It never bypasses or circumvents API rate limits.
- **Private access:** All CDN URLs and content access tokens stay on your local machine. Warpbox does not share, cache publicly, or distribute any private access links.
- **No account sharing:** Warpbox uses your own TorBox API key, configured locally. It does not bundle, resell, or distribute API keys.
- **Fair usage:** Warpbox's architecture (SQLite metadata cache, JIT RAM buffering) minimises API calls rather than generating unnecessary requests.

You are responsible for ensuring your use of warpbox complies with TorBox's current TOS. The TOS may change — refer to the canonical source linked above.

## Release Process

1. Tag the current commit with a semantic version:
   ```
   git tag v0.1.0
   git push origin v0.1.0
   ```
2. The CI pipeline (`.gitea/workflows/build.yml`) automatically builds binaries for all platforms (Linux amd64/arm64, Windows amd64) and pushes Docker images to the Gitea container registry.
3. Update `docker-compose.yml` to point to the new version tag.

See `.clinerules/source-control.md` for versioning conventions.

## AI Usage

This project is developed with heavy AI assistance. See [AI_DISCLOSURE.md](AI_DISCLOSURE.md) for details.

## Status

Active Development. Core WebDAV handlers (PROPFIND, GET with byte-range), CDN URL caching, metadata sync, and rate-limited throttle are implemented.
