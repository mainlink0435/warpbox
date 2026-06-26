# Troubleshooting

This page covers common problems, what they mean, and how to fix them.

## Container exits immediately

**What you see:** `docker compose up` starts warpbox, then it exits with an error in the logs.

| Log message | Cause | Fix |
|-------------|-------|-----|
| `torbox.api_key is required` | Config exists but `api_key` is `"changeme"` or missing | Edit `config.yml` and set your real TorBox API key |
| `failed to load config` | YAML syntax error or file unreadable | Validate your `config.yml` — check indentation, quotes, and that the file is mounted to `/data/config.yml` in the container |
| `creating database directory` | No write permission in the data directory | Ensure the mounted volume at `/data/` is writable by the container user |
| `opening metadata store` | SQLite database is corrupt or unwritable | Delete the old `warpbox.db` and restart — it is recreated automatically |
| `server error` / `bind: address already in use` | Port 1412 is already taken | Change `server.listen_addr` to a different port, or stop the other process using 1412 |

## rclone mount fails

**What you see:** `rclone mount` exits immediately, or the mount point is empty/unreachable.

| Cause | Fix |
|-------|-----|
| `/dev/fuse` not available in container | Add `devices: - /dev/fuse:/dev/fuse:rwm` to the rclone service |
| Missing `SYS_ADMIN` capability | Add `cap_add: - SYS_ADMIN` to the rclone service |
| AppArmor blocking FUSE | Add `security_opt: - apparmor:unconfined` to the rclone service |
| PUID/PGID mismatch | Check the user ID on the host: `id <username>`. Set `PUID`/`PGID` to match. If Plex runs as UID 1001, use `PUID=1001, PGID=1001`. |
| warpbox not reachable | Ensure warpbox started first. On bare metal, verify `curl http://localhost:1412/webdav/` responds. |
| Mount directory doesn't exist | Create it: `mkdir -p /mnt/warpbox` |

## Rate limit errors (429) still appearing

**What you see:** `429 Too Many Requests` in logs, or TorBox emails about rate limiting.

| Cause | Fix |
|-------|-----|
| API key is wrong or expired | Verify in TorBox dashboard. Regenerate if needed. |
| `throttle.requests_per_minute` too high | Default is 250 (below TorBox's 300 limit). If you raised it, lower it back. |
| Multiple warpbox instances sharing one API key | Each instance has its own throttle. One key, one instance. |
| A single torrent causing repeated failures | Check the landing page for circuit breaker status — a stuck torrent can burn rate budget on retries. |

## Files not showing up in my media server

**What you see:** Mount looks empty, or recent TorBox additions don't appear.

1. **Check sync health.** Open the landing page (`http://localhost:1412/`). **Last Sync** should show seconds or minutes ago. If it says `never`, the sync hasn't completed yet. **API Bad** in red means TorBox is unreachable.
2. **Wait for first sync.** It fires immediately at startup. Depending on library size, it may take 30–90 seconds. Refresh the landing page.
3. **Wait for rclone poll interval.** New files appear after `--poll-interval` (default 5 minutes). Force a refresh: `rclone rc vfs/refresh recursive=true`.
4. **Check your mount path.** The media server library path should be the mount point (e.g. `/mnt/warpbox/`). With virtual paths enabled, use the filtered subdirectory (e.g. `/mnt/warpbox/movies/`).

## Playback stutters or buffers

| Cause | Fix |
|-------|-----|
| `max_cdn_connections` too low | Default is 4. If you have multiple simultaneous streams, increase it (max 64). |
| `--transfers` in rclone too high | Keep `--transfers` at or below `max_cdn_connections` minus 1 so there's headroom for seek requests. |
| Chunk size too small | `--vfs-read-chunk-size 32M` is the default. Lower values mean more round-trips per second. |
| Network bandwidth to TorBox CDN | Geographic distance to CDN servers is outside warpbox's control. |
| CDN URL expiring mid-playback | Auto-repair handles this. Check logs for `stale CDN URL detected`. If frequent, reduce `cdn_url_ttl_minutes`. |

## "CDN unavailable" in logs repeatedly

**What you see:** `entering hang/poll mode` frequently, or playback takes a long time to start.

| Cause | Fix |
|-------|-----|
| Circuit breaker tripped on a torrent | A torrent with repeated failures is quarantined (default 5 minutes). The breaker auto-resets. This is normal — it stops one bad torrent from burning the rate budget. |
| TorBox CDN regional outage | Outside warpbox's control. Hang/poll is designed for this — it holds the connection open and retries with exponential backoff (15s → 30s → 60s → 2min → 5min max on repeated 429s). |
| `cdn_url_ttl_minutes` set too high | Stale URLs fail on first use, triggering repair. Default 120 minutes is safe. Reduce if you see frequent stale URL warnings. |

## Web UI not accessible

**What you see:** Browser can't reach `http://localhost:1412/`, or you get a 401 login prompt.

| Cause | Fix |
|-------|-----|
| Auth enabled and credentials forgotten | Default username is `admin`. Reset the password in `config.yml` or disable auth temporarily with `auth.enabled: false`. |
| Firewall blocking port 1412 | Check your firewall rules. The port must be reachable from your browser machine. |
| Wrong host or port | Default is `:1412` on all interfaces. If you changed `server.listen_addr`, use that port. |
| Browser hitting WebDAV path | `/webdav/` returns directory XML, not HTML. The landing page is at `/` (root path). |

## Data disappears after restart

| Cause | Fix |
|-------|-----|
| Config not volume-mounted | Without `./config.yml:/data/config.yml` in your compose file, a new empty config is generated each start. |
| Database not on persistent storage | Mount the whole `/data` directory as a volume: `./warpbox-data:/data`. |
| rclone cache not a named volume | Use a named volume (as shown in the README `docker-compose.yml`) so cache survives restarts. |
