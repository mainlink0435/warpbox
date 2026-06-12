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

Mount Warpbox as a local filesystem using rclone's WebDAV backend. Below are the recommended flags for optimal performance and stability — each one explained in plain English so you know why it matters.

### Quick start (Docker Compose)

If you're using Docker Compose, here's a complete setup. You can copy these services into your `docker-compose.yml` and adjust the values (especially `PUID`, `PGID`, and `--vfs-cache-max-size`) to match your system.

```yaml
services:
  warpbox:
    image: REDACTED/ben/warpbox:0.2.2
    container_name: warpbox
    ports:
      - "1412:1412"
    volumes:
      - ./config.yml:/data/config.yml:ro
    restart: unless-stopped

  rclone:
    image: rclone/rclone:latest
    container_name: warpbox-rclone
    restart: unless-stopped
    environment:
      - PUID=1000                        # Change to your user's UID
      - PGID=1000                        # Change to your user's GID
      - RCLONE_CONFIG_WARPBOX_TYPE=webdav
      - RCLONE_CONFIG_WARPBOX_URL=http://warpbox:1412/webdav/
      - RCLONE_CONFIG_WARPBOX_VENDOR=other
    volumes:
      - /mnt/warpbox:/data:rshared       # The FUSE mount (shared with Plex)
      - rclone_cache:/cache              # Persists the VFS disk cache across restarts
    cap_add:
      - SYS_ADMIN
    security_opt:
      - apparmor:unconfined
    devices:
      - /dev/fuse:/dev/fuse:rwm
    depends_on:
      - warpbox
    command: >
      mount warpbox: /data
      --uid ${PUID}
      --gid ${PGID}
      --cache-dir /cache
      --vfs-cache-mode full
      --vfs-cache-max-age 24h
      --vfs-cache-max-size 100G
      --vfs-cache-min-free-space 20G
      --vfs-read-chunk-size 32M
      --vfs-read-chunk-size-limit 256M
      --vfs-read-ahead 256M
      --buffer-size 128M
      --transfers 2
      --checkers 8
      --timeout 300s
      --contimeout 30s
      --low-level-retries 3
      --dir-cache-time 10m
      --attr-timeout 24h
      --poll-interval 5m
      --no-checksum
      --no-modtime
      --allow-other
      --allow-non-empty
      --vfs-fast-fingerprint
      --ignore-case
      --log-level NOTICE

volumes:
  rclone_cache:
```

### Bare mount command (Linux/macOS without Docker)

```bash
rclone mount warpbox: /mnt/warpbox \
  --webdav-url http://localhost:1412/webdav/ \
  --webdav-vendor other \
  --cache-dir /tmp/rclone-cache \
  --vfs-cache-mode full \
  --vfs-cache-max-age 24h \
  --vfs-cache-max-size 100G \
  --vfs-cache-min-free-space 20G \
  --vfs-read-chunk-size 32M \
  --vfs-read-chunk-size-limit 256M \
  --vfs-read-ahead 256M \
  --buffer-size 128M \
  --transfers 2 \
  --checkers 8 \
  --timeout 300s \
  --contimeout 30s \
  --low-level-retries 3 \
  --dir-cache-time 10m \
  --attr-timeout 24h \
  --poll-interval 5m \
  --no-checksum \
  --no-modtime \
  --allow-other \
  --allow-non-empty \
  --vfs-fast-fingerprint \
  --ignore-case \
  --daemon
```

### Flag-by-flag explanation

Each setting below has a purpose. Read through them once, then adjust the few that depend on your hardware (`PUID`/`PGID`, `--vfs-cache-max-size`, `--cache-dir`).

#### File caching & persistence

| Flag | Recommended | Why |
|------|-------------|-----|
| `--vfs-cache-mode` | `full` | **Required.** Saves every downloaded chunk to disk. Without this, seeking or scrubbing in a video forces a full re-download from TorBox. With `full`, the file stays on your drive after the first watch. |
| `--vfs-cache-max-age` | `24h` | How long downloaded chunks survive on disk. If this is too short (e.g. 30 seconds), chunks are deleted faster than you can use them and you re-download everything every time. 24 hours means last night's episode is still cached this morning. |
| `--vfs-cache-max-size` | `100G` | Hard disk limit for the cache. Once the cache fills up, rclone removes the oldest files to make room. Set this to whatever free space you can spare — more means less re-downloading. |
| `--vfs-cache-min-free-space` | `20G` | Safety valve. If your disk drops below 20 GB free, rclone stops caching to avoid filling the drive completely. |
| `--cache-dir` | `/cache` | Where cached files are stored on disk. In Docker, this should be a named volume (so the cache survives container restarts). On bare metal, point it somewhere with plenty of free space. |

#### Chunk download tuning

| Flag | Recommended | Why |
|------|-------------|-----|
| `--vfs-read-chunk-size` | `32M` | How much data rclone fetches in a single request to warpbox. Each request = one call to the TorBox CDN. 32 MB is big enough to be efficient (not many round-trips) but small enough to arrive quickly. |
| `--vfs-read-chunk-size-limit` | `256M` | When rclone sees you're playing a file sequentially (e.g. watching a movie), it gradually doubles the chunk size up to this limit. Fewer chunk requests = less CDN overhead as playback continues. |
| `--vfs-read-ahead` | `256M` | How far ahead rclone pre-fetches data beyond your current position. Imagine you're at minute 5 of a video — rclone quietly downloads up to minute 10 in the background. That makes seeking instant. 256 MB covers about 2 minutes of 1080p video or 15 seconds of a 4K remux. |
| `--buffer-size` | `128M` | Memory buffer per open file. Data passes through this RAM buffer on its way to disk. 128 MB can hold two chunks at once (one being written to disk, one being downloaded), which is plenty. |

#### Concurrency & timeouts

| Flag | Recommended | Why |
|------|-------------|-----|
| `--transfers` | `2` | How many files rclone downloads at the same time. Keep this low — warpbox only opens 4 CDN connections total, and each file stream uses at least one slot. |
| `--checkers` | `8` | How many files rclone scans in parallel during library refreshes. This only reads metadata (name, size, type) — it does NOT download anything. Warpbox serves metadata from a local SQLite database, so more checkers = faster scans with zero API cost. |
| `--timeout` | `300s` | How long rclone waits for warpbox to respond before giving up. When the TorBox CDN has a temporary hiccup, warpbox holds the connection open and retries every 15 seconds. 5 minutes gives warpbox 20 attempts to recover. |
| `--contimeout` | `30s` | How long rclone waits to establish the initial network connection. 30 seconds is more than enough on a local network. |
| `--low-level-retries` | `3` | How many times rclone retries a failed request before calling it an error. Catches brief network blips. |

#### Caching & metadata

| Flag | Recommended | Why |
|------|-------------|-----|
| `--dir-cache-time` | `10m` | How long rclone remembers directory listings in memory before asking warpbox again. Reduces repeated queries during library scans. |
| `--attr-timeout` | `24h` | How long rclone caches file metadata (size, type, timestamps). Since warpbox syncs with TorBox every 5 minutes, new files appear quickly. This prevents rclone from re-checking files that haven't changed. |
| `--poll-interval` | `5m` | How often rclone checks for new or removed files. Sets the maximum delay before new TorBox content shows up in your mount. |

#### Safety & compatibility

| Flag | Recommended | Why |
|------|-------------|-----|
| `--no-checksum` | (include) | Stops rclone from reading every file to compute a checksum. Without this, a library scan would download a chunk of every single file — hundreds of unnecessary CDN requests. |
| `--no-modtime` | (include) | Prevents rclone from trying to set file modification times. Not needed for streaming, and avoids pointless write attempts to a read-only mount. |
| `--allow-other` | (include) | Lets other users and containers (like Plex or Jellyfin) access the mounted files. Required for multi-service setups. |
| `--allow-non-empty` | (include) | Allows mounting on a directory that might already contain files. Prevents rclone from refusing to mount. |
| `--vfs-fast-fingerprint` | (include) | Identifies files by size + modification time instead of hashing their contents. Fast and accurate — warpbox provides correct metadata, so hashing is unnecessary. |
| `--ignore-case` | (include) | Makes file name lookups case-insensitive. Prevents "file not found" errors when torrent names use unexpected capitalisation. |
| `--log-level` | `NOTICE` | Hides routine log messages but shows warnings and errors. Keeps logs readable. Set to `DEBUG` only when troubleshooting. |

### Docker permissions (PUID / PGID)

The `PUID` and `PGID` environment variables control which user owns the cached files on disk. Set them to match the user that runs your media server (Plex, Jellyfin, etc.):

- If Plex runs as `plex:plex`, find its IDs: `id plex` → e.g. `uid=1001, gid=1001`, then set `PUID=1001, PGID=1001`.
- If all your containers run as a shared user (common setup), use that user's ID.
- If you're not sure, start with `PUID=1000, PGID=1000` (typical for the first user on most Linux systems).

Mismatched permissions mean cached files are owned by the wrong user, and Plex may not be able to read them — which defeats the purpose of caching.

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