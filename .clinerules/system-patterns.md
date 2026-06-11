# System Patterns: Warpbox

## 1. Core Architecture

Warpbox operates as an intercepting WebDAV proxy, designed to be consumed by rclone's WebDAV backend. It acts as a shield between aggressive local media servers (accessed via rclone) and strict cloud APIs (TorBox). The primary pattern is **decoupling filesystem speed from network speed**.

```
Plex/Jellyfin → rclone (FUSE mount) → WebDAV → Warpbox → TorBox API
```

## 2. Configuration Management

* All application settings must be driven by a declarative `config.yml` file.
* The structure should logically separate upstream cloud credentials, local WebDAV server settings, caching rules, and rate-limiting parameters.
* The exact schema is flexible but must support graceful degradation if optional parameters are omitted.
* **No hardcoded behaviour:** Any tunable logic (thresholds, strategy selection, durations, cache lifetimes, eviction policies) MUST be surfaced as a configuration option with a sensible default. The config file is the single source of truth for all runtime behaviour.
* **Example file completeness:** Every configuration key must appear in `config.yml.example` with an explanatory comment. Comments must document: the purpose of the setting, its default value, whether it is required or optional, and all valid options/ranges where applicable.

## 3. State & Caching Patterns

* **Persistent State (Metadata):** Use SQLite running in WAL (Write-Ahead Logging) mode to store the virtual directory structure, file metadata, and cache pointers. This allows zero-API directory browsing.
* **Ephemeral State (Data):** Use Just-In-Time (JIT) RAM buffering for video chunk look-aheads. File headers and media chunks should be held in memory temporarily to serve rapid sequential byte-range requests, then evaporated based on a configurable TTL.

## 4. Logging

* Exclusively use Go's native structured logging package (`log/slog`).
* The logging implementation must support toggling between human-readable text output (for local development/debugging) and structured JSON output (for production/containerised environments).

## 5. Network & Rate Limiting

* **Never fail fast with HTTP 429s to the media server.** \* Implement blocking queues and internal throttling to manage massive concurrent read requests. The proxy must absorb burst traffic from Plex and drip-feed it to the TorBox API strictly below the 300 requests/minute limit.

## 6. Decision Tracking

* Always consult decision-log.md before implementing complex logic to avoid repeating failed experiments. Whenever a significant architectural decision is made, a workaround is implemented, or an approach fails, you must immediately document the context, decision, and rationale in decision-log.md.

## 7. Feature & Issue Tracking

* All feature requests, bugs, enhancements, and documentation tasks are tracked
  as Gitea Issues in the `ben/warpbox` repository.
* The **"Warpbox Kanban"** Gitea Project board defines the workflow:
  📥 Backlog → 🧠 Research/Spikes → 📋 Ready to Dev → 🚧 In Progress → 👀 Review/QA → ✅ Done
* WIP limits: maximum 2 issues in "In Progress" and 2 in "Review/QA" at any time.
  Finish (or move back) existing work before pulling new items.
* Every issue MUST carry at least one type label: `bug`, `enhancement`,
  `testing`, `research`, `architecture`, `docs`, `infra`, or `refactor`.
* The AI assistant uses the `gitea-unified` MCP server to create, read, update,
  and search issues, and to close completed issues.
* Implementation commits reference issues (e.g., `fix: handle CDN expiry, refs #28`).
  The issue stays open until the fix is verified in deployment.
* Before starting any non-trivial work, consult the issue tracker — not the
  chat history — for context and priorities.
* When an issue is completed, close it via `issue_write` (method: `update`,
  state: `closed`), then move it to ✅ Done on the board (see §8 for board
  operations).

## 8. Project Board Operations (Kanban)

* **Gitea has no REST API for project boards.** API tokens return 404 on all
  board endpoints. Do NOT attempt to implement board operations via HTTP calls
  to the Gitea web UI — this has been attempted and failed repeatedly.
* The `extea.exe` CLI tool (`C:\Users\user\Documents\Cline\MCP\extea\extea.exe`)
  can manage board operations via web session auth, but **it requires an
  interactive TTY**. Subprocess calls from the AI assistant hang indefinitely.
  Board operations are therefore limited to the Gitea web UI.
* Tea login config: `C:\Users\user\.config\tea\config.yml` (login: `cline`)
* For reference, the extea commands (run manually in a real terminal) are:
  ```
  extea projects create --title "Name" --template kanban -r ben/warpbox -l cline
  extea projects list -r ben/warpbox -l cline -o json
  extea projects view 1 -r ben/warpbox -l cline -o json
  extea columns create --project 1 --title "Backlog" -r ben/warpbox -l cline
  extea projects assign 5 --issue 1 -r ben/warpbox -l cline
  extea projects move 5 --column 3 --issue 1 -r ben/warpbox -l cline
  ```
* Issue tracking relies solely on labels (`status:*` or type labels), milestone
  assignment, and the Gitea Issue tracker. Column position on the Kanban board
  is managed manually via the web UI.

## 9. Wiki Usage

* Living specifications, testing strategies, architecture decision records
  (beyond the decision log), and onboarding guides belong in the Gitea Wiki.
* Issues are for actionable, completable units of work. If content will be
  continually updated over time (e.g., a test plan), it belongs in the Wiki.
* When an issue's scope expands into a living document, create a Wiki page
  and link it from the issue.
