# Grove-Level Workspace Sync

**Created:** 2026-03-29
**Updated:** 2026-03-31
**Status:** Draft — Decisions Captured
**Related:** `hosted/sync-design.md`, `hosted/hub-groves.md`, `hosted/git-groves.md`, `hosted/multi-hub-broker.md`

---

## 1. Problem Statement

The current `scion sync` command is agent-centric: it transfers files to or from a specific agent's workspace via `scion sync [to|from] <agent-name>`. This framing misses the broader need — **groves themselves need to be synchronized between brokers**.

Consider these scenarios the current design doesn't address well:

1. **Multi-broker grove**: A linked grove exists on Broker A and Broker B. A developer makes changes on Broker A and wants them reflected on Broker B before dispatching agents there.
2. **Local-to-hub bootstrap**: A developer has a local project directory and wants to push the full grove workspace to the hub for remote agents — not tied to any specific agent.
3. **Hub-managed grove download**: A hub-managed grove's workspace was populated by agents. The developer wants to pull the current state to their local machine.
4. **Non-git groves**: Hub-managed groves have no git remote. There's no git-based mechanism to propagate changes — file-level sync is the only option.

### 1.1 Key Insight

Agent workspace sync is a *special case* of grove workspace sync. An agent's workspace is a derivative of the grove's workspace (a worktree branch, a clone, or a bind-mount). Synchronizing at the grove level is the fundamental operation; agent-level sync should compose on top of it.

---

## 2. Goals and Non-Goals

### 2.1 Goals

| Goal | Description |
|------|-------------|
| **Grove-level sync** | `scion sync` operates on the current grove, not a specific agent |
| **Bidirectional by default** | Bare `scion sync` performs per-file bidirectional merge (newer wins) |
| **File-level sync as primary** | Working tree files are the unit of sync — `.git/` excluded |
| **Hub as canonical state** | Each broker syncs against the hub independently; hub is the single source of truth |
| **WebDAV relay** | Hub exposes a WebDAV endpoint; rclone targets it for efficient streaming sync |
| **Multi-broker** | Enable syncing a grove's workspace from one broker to another (mediated by hub) |
| **Implicit grove** | When hub is enabled, `scion sync` should "just work" with the resolved grove |

### 2.2 Non-Goals

| Non-Goal | Rationale |
|----------|-----------|
| Real-time continuous sync | On-demand is sufficient; continuous sync (mutagen/syncthing) is future work |
| Conflict resolution beyond mtime | Last-write-wins by mtime for true conflicts (modified on both sides) |
| Cross-hub grove sync | Out of scope; multi-hub broker design handles per-hub grove isolation |
| Replacing agent-level sync | Existing `scion sync to/from <agent>` remains for targeting a running agent's container |
| Solo mode grove sync | Grove-level sync requires a hub; solo mode uses agent-level sync only |

---

## 3. Proposed UX

### 3.1 Command Structure

```bash
# Bidirectional sync of the current grove (inferred from cwd or -g flag)
scion sync                     # Bidirectional per-file merge (newer wins)

# One-directional overrides
scion sync push                # Force push local → hub
scion sync pull                # Force pull hub → local

# Explicit grove targeting (uses existing -g flag)
scion sync -g /path/to/grove
scion sync push -g /path/to/grove

# Existing agent-level sync preserved (different terminology: to/from)
scion sync to <agent-name>     # Push to a specific running agent
scion sync from <agent-name>   # Pull from a specific running agent

# Options
scion sync --dry-run
scion sync --exclude "*.log" --exclude "tmp/**"
scion sync push --force         # Overwrite without hash comparison
```

### 3.2 Default Behavior (Bidirectional)

When a user runs bare `scion sync` in a hub-enabled grove:

1. Resolve the current grove (same resolution as other commands: `-g` flag → project `.scion/` → global).
2. Collect local file manifest (paths, sizes, mtimes, hashes).
3. Fetch remote manifest from hub.
4. Compare per-file:
   - File exists only locally → upload to hub
   - File exists only on hub → download to local
   - File exists on both, hashes match → skip
   - File exists on both, hashes differ → newer mtime wins
   - File modified on both sides since last sync → mtime wins (true conflict, last-write-wins)
5. Execute transfers in both directions.

### 3.3 Terminology

| Scope | Direction Words | Example |
|-------|----------------|---------|
| Grove-level | `push` / `pull` | `scion sync push` |
| Agent-level | `to` / `from` | `scion sync to my-agent` |

The different terminology disambiguates scope. Bare `scion sync` (no subcommand, no agent name) is always grove-level bidirectional.

### 3.4 Grove Resolution

The user must be inside a grove directory (containing `.scion/`) or specify one via `-g`. This is consistent with all other grove-scoped commands.

---

## 4. Recommended Approach: Hub WebDAV Relay + rclone

### 4.1 Overview

The hub exposes a **WebDAV endpoint** scoped per grove. The CLI uses **rclone's WebDAV backend** (already a project dependency) to sync the local grove workspace against this endpoint. The hub translates WebDAV operations into file read/write operations on the grove's canonical workspace.

```
┌─────────────┐         WebDAV          ┌──────────────┐
│  CLI / rclone│◄──────────────────────▶│   Hub        │
│  (Broker A) │    PROPFIND, GET, PUT   │  WebDAV      │
└─────────────┘                         │  Endpoint    │
                                        │              │
                                        │  ┌────────┐  │
┌─────────────┐         WebDAV          │  │ Grove  │  │
│  rclone     │◄──────────────────────▶│  │Workspace│  │
│  (Broker B) │                         │  │ Files  │  │
└─────────────┘                         │  └────────┘  │
                                        └──────────────┘
```

### 4.2 Why WebDAV

| Factor | WebDAV | GCS Signed URLs (current) | Direct SFTP |
|--------|--------|--------------------------|-------------|
| NAT traversal | Yes (HTTPS) | Yes (HTTPS) | No |
| Cloud dependency | None | Requires GCS | None |
| Auth | Hub auth (existing) | Signed URL generation | SSH key management |
| rclone support | Native backend | Native backend | Native backend |
| Hub role | Protocol adapter + relay | Metadata only | Not involved |
| Bidirectional | Natural (read + write) | Requires separate flows | Natural |
| Browser access | Not needed (REST API serves web UI) | N/A | N/A |

WebDAV eliminates the GCS dependency for sync while remaining NAT-safe. The hub already has authentication and grove-scoped authorization — the WebDAV endpoint reuses this.

### 4.3 Hub WebDAV Endpoint Design

**Mount point:** `/api/v1/groves/{id}/dav/`

The hub serves a WebDAV interface over each grove's workspace directory. This is scoped and authenticated identically to the existing grove APIs.

**Supported WebDAV methods:**

| Method | Purpose | Maps to |
|--------|---------|---------|
| `PROPFIND` | List files, get metadata (size, mtime) | `os.Stat` / `os.ReadDir` on grove workspace |
| `GET` | Download file | Read from grove workspace |
| `PUT` | Upload file | Write to grove workspace |
| `DELETE` | Remove file | Delete from grove workspace |
| `MKCOL` | Create directory | `os.MkdirAll` |
| `MOVE` | Rename/move file | `os.Rename` |

**Implementation:** Use `golang.org/x/net/webdav` which provides a ready-made WebDAV server handler. The hub mounts it with a custom `FileSystem` implementation that:
1. Scopes all paths to the grove's workspace directory
2. Enforces grove-level authorization (existing middleware)
3. Excludes `.git/`, `.scion/`, `node_modules/` etc. from listings
4. For linked groves on remote brokers: relays WebDAV operations through the control channel to the broker that owns the workspace

**Authentication:** Same as all hub API endpoints — bearer token or HMAC broker auth. No additional auth mechanism needed.

### 4.4 rclone Integration (CLI Side)

The CLI uses rclone's WebDAV backend to sync against the hub's WebDAV endpoint:

```go
import (
    "github.com/rclone/rclone/fs"
    "github.com/rclone/rclone/fs/sync"
    _ "github.com/rclone/rclone/backend/webdav"
    _ "github.com/rclone/rclone/backend/local"
)

func groveSync(ctx context.Context, localPath, hubURL, authToken string) error {
    // On-the-fly WebDAV backend — no config file needed
    remote := fmt.Sprintf(":webdav,url=%s,bearer_token=%s:", hubURL, authToken)

    srcFs, _ := fs.NewFs(ctx, localPath)
    dstFs, _ := fs.NewFs(ctx, remote)

    // rclone bisync handles bidirectional merge
    return bisync.Bisync(ctx, dstFs, srcFs, bisyncOpts)
}
```

**rclone handles:**
- Delta detection (size + mtime comparison via PROPFIND)
- Incremental transfer (only changed files)
- Progress reporting (native, passed through to CLI output)
- Retries and resume on failure
- Exclude patterns

### 4.5 Broker-to-Broker Sync (via Hub)

When two brokers both serve the same grove, they each sync independently against the hub's WebDAV endpoint. The hub is the canonical state:

```
Broker A ←──bisync──▶ Hub (canonical) ◀──bisync──→ Broker B
```

Divergence is resolved naturally:
- Each broker syncs with the hub using bidirectional per-file merge
- Files modified on only one broker propagate through the hub to the other
- True conflicts (modified on both brokers since last sync) resolved by mtime (last-write-wins)

### 4.6 Hub-Hosted vs Broker-Hosted Workspaces

For **hub-managed groves**, the workspace files live on the hub's filesystem. WebDAV serves them directly — zero relay needed.

For **linked groves**, the workspace lives on a broker. The hub must relay WebDAV operations to the broker that owns the workspace. Two sub-approaches:

| Approach | How | Trade-off |
|----------|-----|-----------|
| **Store-and-forward** | Broker periodically pushes state to hub storage; hub serves from its copy | Hub has its own copy (storage cost), but WebDAV is always fast |
| **Live relay** | Hub proxies WebDAV operations to broker via control channel in real-time | No hub storage, but latency depends on broker availability |

**Recommendation:** Store-and-forward for MVP. The hub maintains a cached copy of each linked grove's workspace. Brokers push updates to the hub after modifications. This keeps the WebDAV endpoint simple and fast, and works even when the source broker is offline.

### 4.7 File Exclusions

The following patterns are excluded from sync (consistent with existing `transfer.DefaultExcludePatterns`):

- `.git/**` — git internals (history transferred separately if needed)
- `.scion/**` — grove metadata (managed locally)
- `node_modules/**` — dependency artifacts
- `*.env` — secrets

### 4.8 Optional: Git History Sync

For git-backed groves, committed history can optionally be transferred using `git bundle`:

```bash
scion sync --include-history
```

- Bundles the **default branch only** (not agent branches)
- Hub stores only the **latest** bundle (no history of bundles)
- First sync: full bundle. Subsequent: incremental from last-synced-commit.
- This is a supplementary feature — the primary sync is always file-level.

---

## 5. Relationship to Existing Interfaces

### 5.1 Existing Grove Workspace File API (`grove_workspace_handlers.go`)

The REST API for grove workspace files (upload/download/delete) remains unchanged. It serves the **web UI** file browser. The WebDAV endpoint serves **CLI sync** via rclone. Both are backed by the same underlying grove workspace storage.

```
Web UI ──REST API──▶ ┌──────────────────┐
                     │  Grove Workspace  │
CLI  ───WebDAV────▶  │  Storage Layer    │
                     └──────────────────┘
```

### 5.2 Agent-Scoped Sync (`scion sync to/from <agent>`)

The existing agent-level sync continues to work as-is. It operates on a running agent's container workspace, using GCS signed URLs with manifest-diff. Grove-level sync is a separate, orthogonal operation.

For agents with **bind-mounted** workspaces (colocated broker), grove sync automatically updates what agents see — the workspace is the same filesystem. No notification or restart needed.

### 5.3 Agent Startup / git clone

Existing agent startup mechanisms (git clone for git-anchored groves, bind-mount for linked groves) are unchanged. Grove sync is an orthogonal operation that keeps the hub's canonical state up to date.

---

## 6. Architecture

### 6.1 Component Roles

```
┌─────────────┐                          ┌─────────────────┐
│   CLI       │        WebDAV            │  Runtime        │
│  (or Broker)│◄────────────────────────▶│  Broker B       │
│             │    via Hub relay          │                 │
└──────┬──────┘                          └────────┬────────┘
       │                                          │
       │  rclone bisync                           │  rclone bisync
       │                                          │
       ▼                                          ▼
┌─────────────────────────────────────────────────────────┐
│                        Hub                              │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ WebDAV       │  │ REST API     │  │ Sync         │  │
│  │ Endpoint     │  │ (web UI)     │  │ Metadata     │  │
│  │ /dav/        │  │ /workspace/  │  │ (last sync)  │  │
│  └──────┬───────┘  └──────┬───────┘  └──────────────┘  │
│         │                 │                             │
│         ▼                 ▼                             │
│  ┌──────────────────────────────┐                      │
│  │  Grove Workspace Storage    │                      │
│  │  (filesystem or relayed)    │                      │
│  └──────────────────────────────┘                      │
└─────────────────────────────────────────────────────────┘
```

### 6.2 API Surface

```
# WebDAV endpoint (new)
/api/v1/groves/{id}/dav/*        # WebDAV methods (PROPFIND, GET, PUT, DELETE, MKCOL, MOVE)

# Sync metadata (new)
GET  /api/v1/groves/{id}/sync/status     # Last sync time, file count, per-broker state

# Existing (unchanged)
POST /api/v1/groves/{id}/workspace/...   # REST file API for web UI
POST /api/v1/agents/{id}/workspace/...   # Agent-scoped sync (GCS signed URLs)
```

### 6.3 Sync Metadata Storage

The hub tracks sync state per grove:

```sql
CREATE TABLE grove_sync_state (
    grove_id        TEXT NOT NULL,
    broker_id       TEXT,              -- NULL for hub-managed groves
    last_sync_time  TIMESTAMP,
    last_commit_sha TEXT,              -- for git history sync
    file_count      INTEGER,
    total_bytes     INTEGER,
    PRIMARY KEY (grove_id, broker_id)
);
```

---

## 7. Sync Flows

### 7.1 Bidirectional Sync (default: `scion sync`)

```
1. CLI: Resolve grove from cwd or -g flag (must contain .scion/)
2. CLI: rclone bisync local grove workspace ↔ hub WebDAV endpoint
   - rclone PROPFIND on hub: get remote file list + mtimes
   - rclone compares with local files
   - Files only on one side → propagate
   - Files differ → newer mtime wins
   - Transfers in both directions simultaneously
3. CLI: rclone reports progress (native progress output)
4. Hub: Updates sync metadata (last_sync_time, file_count)
```

### 7.2 Push (`scion sync push`)

```
1. CLI: Resolve grove
2. CLI: rclone sync local → hub WebDAV (one-directional, local is source of truth)
3. Hub: Updates sync metadata
```

### 7.3 Pull (`scion sync pull`)

```
1. CLI: Resolve grove
2. CLI: rclone sync hub WebDAV → local (one-directional, hub is source of truth)
3. CLI: Updates local state
```

### 7.4 Multi-Broker Sync

```
1. Broker A: scion sync          # bisync Broker A ↔ Hub
2. Broker B: scion sync          # bisync Broker B ↔ Hub
   - Hub canonical state now includes changes from both brokers
   - Each broker gets the other's changes on next sync
```

### 7.5 Git History Sync (optional: `scion sync --include-history`)

```
1. CLI: git bundle create with default branch only
2. CLI: Upload bundle to hub (via WebDAV PUT or dedicated endpoint)
3. Hub: Stores latest bundle (replaces any previous)
4. Dest: Downloads bundle, git fetch from it
```

---

## 8. Decisions Log

| # | Question | Decision | Rationale |
|---|----------|----------|-----------|
| Q1 | `scion sync` without arguments | **Bidirectional per-file merge** (newer mtime wins) | Most intuitive default; `push`/`pull` available as one-directional overrides |
| Q2 | Uncommitted changes in git groves | **Sync working tree files directly** (`.git/` excluded) | Git and non-git groves use the same file-level sync path; git history is a separate optional layer |
| Q3 | Local workspace path resolution | **Must be in a grove directory** (has `.scion/`), or specify via `-g` | Consistent with all other grove-scoped commands |
| Q4 | Auto-propagate to running agents | **Automatic for bind-mounted workspaces** (same filesystem); restart needed for copied workspaces | No notification mechanism needed — bind-mount means it's the same files |
| Q5 | Git bundle scope | **Default branch only** | Agent branches are workspace-local; only the shared default branch needs cross-broker transfer |
| Q6 | Broker-to-broker sync | **Always mediated by hub via WebDAV** | Hub exposes WebDAV endpoint; rclone targets it. No direct broker connectivity required, NAT-safe |
| Q7 | Multi-broker divergence | **Hub is canonical state; each broker syncs independently against hub** | Two independent bidirectional syncs, mtime-based conflict resolution |
| Q8 | Git bundle history | **Latest only** | Simpler storage; brokers that are far behind get a full bundle |
| Q9 | Replace git clone at startup | **No** — keep existing provisioning mechanisms | Some git-groves use workspace-based git (bind-mount), others clone. Sync is orthogonal |
| Q10 | rclone vs `pkg/transfer` | **rclone library directly** with WebDAV backend | rclone handles delta detection, streaming, progress, retries. `pkg/transfer` remains for agent-level GCS sync |
| Q11 | Grove workspace API relationship | **WebDAV for CLI sync, REST API for web UI**, both backed by same storage | Web UI needs REST; CLI sync needs WebDAV. Different protocols, same data |
| Q12 | Terminology | **`push`/`pull` for grove-level, `to`/`from` for agent-level** | Different words disambiguate scope |
| Q13 | Solo mode support | **No** — grove-level sync requires a hub | Solo mode has no second endpoint to sync with |
| Q14 | Progress reporting | **Pass through rclone's native progress** | Already well-formatted, zero implementation effort |

---

## 9. Implementation Plan

### Phase 1: Hub WebDAV Endpoint ✅ Complete

- ✅ Implement WebDAV handler using `golang.org/x/net/webdav`
- ✅ Mount at `/api/v1/groves/{id}/dav/` with grove-scoped authorization
- ✅ Serve hub-managed grove workspaces directly from filesystem
- ✅ File exclusion filter (`.git/`, `.scion/`, `node_modules/`, `*.env`)
- ✅ Add `grove_sync_state` table for tracking sync metadata
- ✅ Add `GET /api/v1/groves/{id}/sync/status` endpoint

### Phase 2: CLI Grove Sync Command ✅ Complete

- ✅ Add `scion sync` (bare), `scion sync push`, `scion sync pull` subcommands
- ✅ Integrate rclone WebDAV backend for hub communication
- ✅ Wire up rclone bisync for bidirectional default
- ✅ Wire up rclone sync for one-directional push/pull
- ✅ Pass through rclone progress output
- ✅ Support `--dry-run`, `--exclude`, `--force` flags
- ✅ Grove resolution via `-g` flag or cwd

### Phase 3: Linked Grove Relay ✅ Complete

- ✅ For linked groves where workspace lives on a broker: hub maintains a cached copy
- ✅ Broker pushes workspace updates to hub after agent modifications
- ✅ Hub WebDAV serves from its cached copy
- ✅ Implement cache invalidation / refresh triggers

### Phase 4: Git History Sync (Optional)

- `scion sync --include-history` flag
- `git bundle create` / `git fetch` wrapper (default branch only)
- Hub stores latest bundle per grove
- Upload/download via WebDAV PUT/GET on a well-known path

---

## 10. References

- [Hosted Workspace Sync Design](hosted/sync-design.md) — existing agent-scoped sync (GCS signed URLs)
- [Hub-Managed Groves](hosted/hub-groves.md) — non-git grove workspaces
- [Git-Anchored Groves](hosted/git-groves.md) — git-backed grove creation and cloning
- [Multi-Hub Broker](hosted/multi-hub-broker.md) — broker identity and grove-to-hub mapping
- [Git Workspace Hybrid](git-workspace-hybrid.md) — hybrid git/file workspace approaches
- [`golang.org/x/net/webdav`](https://pkg.go.dev/golang.org/x/net/webdav) — Go WebDAV server library
- `pkg/transfer/` — existing file collection, hashing, and transfer infrastructure (retained for agent-level sync)
- `pkg/gcp/storage.go` — rclone library integration (already a project dependency)
- `pkg/hub/grove_workspace_handlers.go` — existing REST file API for web UI
- `cmd/sync.go` — current sync command implementation
