# Gitea Auth Matrix

This document maps every identity, credential, and auth context used across the Warpbox ecosystem — CI/CD, MCP, git commits, and the Gitea runner fleet.

## Identities

| User | UID | Purpose | Created |
|------|-----|---------|---------|
| **ben** | 1 | Human owner — commits, releases owned by ben | 2026-03-19 |
| **cline** | 3 | AI assistant — issue management, board operations, wiki edits | 2026-06-09 |
| **ci-bot** | 4 | Automated CI/CD — build runs, releases published, docker images pushed | 2026-06-09 |

## Credential Inventory

### 1. `cline` Token (REST API + `tea` CLI)

| Attribute | Value |
|-----------|-------|
| **Token** | `cc42173960d87142ba0ca3863d1744b6585dc059` |
| **Password** | `itAfGdmnV53yvWz` |
| **Stored in** | 3 places |
| **→ MCP config** | `C:\Users\user\AppData\Roaming\Code\User\globalStorage\saoudrizwan.claude-dev\settings\cline_mcp_settings.json` — `gitea-unified` section |
| **→ tea CLI** | `C:\Users\user\.config\tea\config.yml` |
| **→ git remote** | *(not stored — git uses credentials from tea/VS Code)* |
| **Purpose** | AI assistant: issues, boards, wiki, code reads via MCP |
| **Scopes** | Full repo access to `ben/warpbox` |

### 2. `cline` Actions Token

| Attribute | Value |
|-----------|-------|
| **Token** | `58b0ccdbefdc56de17a67aefe1d84037d0eae0aa` |
| **Stored in** | MCP config (`GITEA_ACTIONS_TOKEN`) |
| **Purpose** | AI assistant: reading Actions workflow runs/jobs/logs |
| **Scopes** | Actions read access |

### 3. `ci-bot` Token (Actions Secret)

| Attribute | Value |
|-----------|-------|
| **Token** | *(opaque — only the Gitea server knows the value)* |
| **Stored in** | Gitea Actions Secret at `ben/warpbox` → Settings → Actions → Secrets → `GITEATOKEN` |
| **Purpose** | CI runner: creating releases, uploading binaries, logging into container registry |
| **Identity used** | `ci-bot` (docker login `-u ci-bot`) |

### 4. `ben` Token (Legacy — **retired**)

The original Actions secret was `ben`'s token. This caused all releases and docker pushes (v0.0.7+) to be attributed to `ben`. Replaced by `ci-bot`'s token.

## Auth Flow Diagram

```
┌─────────────────────────────────────────────────────────────┐
│                      VS Code (Cline)                        │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  MCP Servers                                        │   │
│  │  ┌──────────────────┐  ┌─────────────────────────┐  │   │
│  │  │ gitea-unified    │  │ docker-REDACTED/dockcross │  │   │
│  │  │ cline token      │  │ DOCKER_HOST env vars     │  │   │
│  │  │ cline password   │  └─────────────────────────┘  │   │
│  │  └──────┬───────────┘                                │   │
│  └─────────┼────────────────────────────────────────────┘   │
└────────────┼────────────────────────────────────────────────┘
             │
             ▼
┌──────────────────────────────┐
│    Gitea (nas.lan.ourhouse)  │
│                              │
│  ┌────────────────────────┐  │
│  │ ben   (id:1, owner)    │  │  ← Human commits
│  ├────────────────────────┤  │
│  │ cline (id:3)           │  │  ← MCP REST + web session auth
│  ├────────────────────────┤  │
│  │ ci-bot (id:4)          │  │  ← Release + docker push via Actions
│  └────────────────────────┘  │
└──────────────┬───────────────┘
               │
               ▼
┌──────────────────────────────────────┐
│  Dockcross VM (5 Gitea Runners)     │
│                                      │
│  gitea_runner_1  ─── dockcross VM   │
│  gitea_runner_2  ─── dockcross VM   │
│  gitea_runner_3  ─── dockcross VM   │
│  gitea_runner_4  ─── dockcross VM   │
│  gitea_runner_5  ─── dockcross VM   │
│                                      │
│  All register with a shared         │
│  Gitea runner token from Gitea      │
│  admin panel (not user token)       │
└──────────────────────────────────────┘
```

## Key Facts

1. **MCP server (`gitea-unified`)** talks to Gitea as **cline**. It uses:
   - `GITEA_TOKEN` → REST API calls (issues, repos, releases read)
   - `GITEA_USERNAME` + `GITEA_PASSWORD` → web session auth (board operations)
   - `GITEA_ACTIONS_TOKEN` → Actions run/job/log queries

2. **CI runners** execute workflows as **ci-bot** (via `secrets.GITEATOKEN`). They:
   - Create/update releases → attributed to `ci-bot`
   - Upload binary assets → attributed to `ci-bot`
   - `docker login -u ci-bot` → pushes attributed to `ci-bot`

3. **Git commits** are authored by whoever's local `git config user.name`/`user.email` is set. On the Windows dev machine, that's `ben`. On the CI runner, git is not configured for user identity — the Gitea runner preserves the committer from the push.

## Token Rotation Procedure

To rotate any token:
1. **Token in MCP config:** Open the Cline MCP settings JSON, replace the value, restart Cline.
2. **Token in Actions secret:** Gitea web UI → `ben/warpbox` → Settings → Actions → Secrets → edit `GITEATOKEN`.
3. **`ci-bot` password/token:** Log into Gitea as `ci-bot` → Settings → Applications → regenerate.
4. **`cline` password/token:** Log into Gitea as `cline` → Settings → Applications → regenerate. Then update MCP config + `tea` config.

## Historical Releases — Author Audit

| Release | Author | Was Correct? |
|---------|--------|-------------|
| v0.0.1  | **ben** | ✅ (pre-ci-bot era) |
| v0.0.3  | **ci-bot** | ✅ |
| v0.0.4  | **ci-bot** | ✅ |
| v0.0.5  | **ci-bot** | ✅ |
| v0.0.7+ | **ben** | ❌ (ci-bot token was replaced) |
| v0.1.0+ | **ben** | ❌ |