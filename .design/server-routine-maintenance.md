# Server Routine Maintenance: Admin Operations Panel

**Created:** 2026-03-31
**Status:** Proposed
**Related:** `pkg/hub/admin_mode.go`, `pkg/hub/admin_settings.go`, `web/src/components/pages/admin-scheduler.ts`, `.design/secret-id-hub-refactor.md`

---

## 1. Overview

Scion Hub operators currently perform maintenance operations through CLI commands (`scion hub secret migrate`), shell scripts (`gce-start-hub.sh`, `build-images.sh`, `pull-containers.sh`), and manual SSH sessions. There is no centralized visibility into which maintenance tasks have been run, when, or whether they succeeded.

This design adds a **Maintenance Operations** page to the Admin section of the web UI. It provides:

1. **Migration Checklist** — One-time data migrations (e.g., secret naming scheme migration) tracked with creation date, completion status, and completion timestamp.
2. **Routine Operations** — On-demand infrastructure actions (pull images, rebuild server from git) that operators can trigger and monitor from the UI.
3. **Operation History** — An audit log of completed operations with timestamps, initiator, and outcome.

### Goals

- Give operators a single pane of glass for hub maintenance tasks.
- Track which one-time migrations have been applied to this hub instance.
- Eliminate SSH-based manual operations for common tasks (image pulls, server rebuilds).
- Provide audit trail for who ran what and when.

### Non-Goals

- Replacing the CLI for complex or scripted workflows (batch operations, CI/CD pipelines).
- Automated scheduling of maintenance operations (use the existing Scheduler for that).
- Multi-hub orchestration — this is per-hub instance visibility.
- Database schema migrations — those are handled automatically by the `Migrate()` function at startup and do not need manual intervention.

---

## 2. Current State

### Maintenance Operations Today

| Operation | How It's Done Today | Visibility |
|-----------|-------------------|------------|
| Secret naming migration | `scion hub secret migrate --project=... [--hub-id=...]` | CLI output only, no persistent record |
| Pull container images | `pull-containers.sh` via SSH, or `docker pull` manually | No record |
| Rebuild server from git | `gce-start-hub.sh --fast` via SSH (git pull → build → restart) | systemd journal only |
| Build container images | `build-images.sh --registry=... --push` via SSH | No record |
| Database reset | `gce-start-hub.sh --reset-db` via SSH | Destructive, no audit trail |

### Existing Admin UI

The Admin section currently has five pages:

| Page | Path | Purpose |
|------|------|---------|
| Hub Settings | `/settings` | Environment variables, secrets, templates |
| Server Config | `/admin/server-config` | settings.yaml editor with live reload |
| Scheduler | `/admin/scheduler` | Read-only view of recurring/scheduled tasks |
| Users | `/admin/users` | User management (promote, suspend, delete) |
| Groups | `/admin/groups` | Group and membership management |

The new Maintenance Operations page will be added alongside these.

---

## 3. Design

### 3.1 Data Model: Maintenance Operations

A new `maintenance_operations` table tracks both one-time migrations and on-demand operations.

```sql
CREATE TABLE IF NOT EXISTS maintenance_operations (
    id          TEXT PRIMARY KEY,
    key         TEXT NOT NULL UNIQUE,       -- machine-readable identifier (e.g., "secret-hub-id-migration")
    title       TEXT NOT NULL,              -- human-readable name
    description TEXT NOT NULL DEFAULT '',   -- what this operation does
    category    TEXT NOT NULL,              -- "migration" or "operation"
    status      TEXT NOT NULL DEFAULT 'pending',  -- pending, running, completed, failed
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at  TIMESTAMP,
    completed_at TIMESTAMP,
    started_by  TEXT,                       -- user ID who initiated
    result      TEXT,                       -- JSON: outcome details, error message, stats
    metadata    TEXT NOT NULL DEFAULT '{}', -- JSON: operation-specific config/parameters
    UNIQUE(key)
);
CREATE INDEX idx_maintenance_ops_category ON maintenance_operations(category);
CREATE INDEX idx_maintenance_ops_status ON maintenance_operations(status);
```

**Category semantics:**
- `migration` — One-time tasks that transition the system from state A to state B. Once completed, they are marked done and cannot be re-run from the UI (CLI `--force` flag remains for exceptional cases). These are seeded into the table at startup or via schema migration.
- `operation` — Repeatable infrastructure tasks. Each execution creates a new history entry (see `maintenance_operation_runs` below).

**Status transitions:**

```
migration:  pending → running → completed
                    ↘ failed → running (retry)

operation:  (stateless — status lives on individual runs)
```

### 3.2 Data Model: Operation Runs (History)

Repeatable operations track each execution as a run:

```sql
CREATE TABLE IF NOT EXISTS maintenance_operation_runs (
    id            TEXT PRIMARY KEY,
    operation_key TEXT NOT NULL,             -- FK to maintenance_operations.key
    status        TEXT NOT NULL DEFAULT 'running',  -- running, completed, failed
    started_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at  TIMESTAMP,
    started_by    TEXT,                       -- user ID
    result        TEXT,                       -- JSON: outcome details
    log           TEXT NOT NULL DEFAULT '',   -- captured stdout/stderr
    FOREIGN KEY (operation_key) REFERENCES maintenance_operations(key)
);
CREATE INDEX idx_maintenance_runs_key ON maintenance_operation_runs(operation_key);
CREATE INDEX idx_maintenance_runs_started ON maintenance_operation_runs(started_at DESC);
```

### 3.3 Seeded Migrations

The following migrations are seeded into `maintenance_operations` when the table is first created (via the DB schema migration that adds the table). Additional migrations are added via future schema migrations as new one-time tasks are introduced.

| Key | Title | Description |
|-----|-------|-------------|
| `secret-hub-id-migration` | Secret Hub ID Namespace Migration | Migrates hub-scoped secrets from the legacy fixed "hub" scope ID to the per-instance hub ID. Required when upgrading a hub that was created before the hub ID namespacing feature. Only needed for GCP Secret Manager backend. |

### 3.4 Seeded Operations

Routine operations are also seeded at table creation. These are the repeatable actions available from the UI.

| Key | Title | Description |
|-----|-------|-------------|
| `pull-images` | Pull Container Images | Pulls the latest container images for all configured harnesses from the image registry. |
| `rebuild-server` | Rebuild Server from Git | Pulls latest code from the repository, rebuilds the server binary and web assets, then restarts the hub service. Equivalent to the fast-deploy mode of `gce-start-hub.sh`. |
| `rebuild-web` | Rebuild Web Frontend | Rebuilds only the web frontend assets from source without restarting the server binary. Changes take effect on the next page load. |

### 3.5 API Endpoints

All endpoints require admin role authentication.

#### List Operations

```
GET /api/v1/admin/maintenance/operations
```

Response:
```json
{
  "migrations": [
    {
      "id": "...",
      "key": "secret-hub-id-migration",
      "title": "Secret Hub ID Namespace Migration",
      "description": "...",
      "category": "migration",
      "status": "pending",
      "createdAt": "2026-03-30T...",
      "completedAt": null,
      "startedBy": null,
      "result": null
    }
  ],
  "operations": [
    {
      "id": "...",
      "key": "pull-images",
      "title": "Pull Container Images",
      "description": "...",
      "category": "operation",
      "lastRun": {
        "id": "...",
        "status": "completed",
        "startedAt": "2026-03-30T...",
        "completedAt": "2026-03-30T...",
        "startedBy": "user-id",
        "result": "{\"pulled\": 4}"
      }
    }
  ]
}
```

#### Execute Operation

```
POST /api/v1/admin/maintenance/operations/{key}/run
```

Request body (optional, operation-specific parameters):
```json
{
  "params": {
    "registry": "ghcr.io/myorg",
    "tag": "latest"
  }
}
```

Response:
```json
{
  "runId": "...",
  "status": "running"
}
```

#### Execute Migration

```
POST /api/v1/admin/maintenance/migrations/{key}/run
```

Request body (optional, migration-specific parameters):
```json
{
  "params": {
    "dryRun": true
  }
}
```

Response:
```json
{
  "status": "running"
}
```

Returns `409 Conflict` if the migration is already completed (use CLI `--force` for re-runs).

#### Get Run Status

```
GET /api/v1/admin/maintenance/operations/{key}/runs/{runId}
```

Response:
```json
{
  "id": "...",
  "operationKey": "pull-images",
  "status": "completed",
  "startedAt": "...",
  "completedAt": "...",
  "startedBy": "user-id",
  "result": "{\"pulled\": 4}",
  "log": "Pulling scion-claude:latest...\nPulling scion-gemini:latest...\n..."
}
```

#### List Run History

```
GET /api/v1/admin/maintenance/operations/{key}/runs?limit=20
```

Response:
```json
{
  "runs": [
    { "id": "...", "status": "completed", "startedAt": "...", "completedAt": "...", "startedBy": "...", "result": "..." }
  ]
}
```

### 3.6 Operation Executors

Each operation has a server-side executor that implements the `MaintenanceExecutor` interface:

```go
// MaintenanceExecutor defines the interface for a runnable maintenance operation.
type MaintenanceExecutor interface {
    // Run executes the operation. The context is cancelled if the server shuts down.
    // The logger captures output that is streamed to the run's log field.
    // Params contains operation-specific configuration from the API request.
    Run(ctx context.Context, logger io.Writer, params map[string]string) error
}
```

#### Pull Images Executor

Shells out to the container runtime to pull images. Reads the image registry and harness list from the server configuration (same values used by the runtime broker when launching agents).

```go
type PullImagesExecutor struct {
    runtimeType string   // "docker", "podman", "container"
    registry    string   // from config or params override
    tag         string   // from params, default "latest"
    harnesses   []string // configured harnesses
}
```

Steps:
1. Detect container runtime (same logic as `pull-containers.sh`).
2. For each configured harness image, run `{runtime} image pull {registry}/scion-{harness}:{tag}`.
3. Capture stdout/stderr to the run log.
4. Report pulled count in result JSON.

#### Rebuild Server Executor

Performs the equivalent of the fast-deploy flow from `gce-start-hub.sh`:

```go
type RebuildServerExecutor struct {
    repoPath   string // path to the scion source checkout
    binaryDest string // install path (e.g., /usr/local/bin/scion)
    serviceName string // systemd service name
}
```

Steps:
1. `git pull` in the repo directory.
2. `make web` to rebuild frontend assets.
3. `go build -o {repoPath}/scion.rebuild ./cmd/scion` to rebuild the binary into a staging path inside the repo directory (where the service user has write access).
4. `sudo install -m 755 {repoPath}/scion.rebuild {binaryDest}` to install the binary to the final destination.
5. `systemctl restart {serviceName}` to restart.
6. Health check loop (poll `/healthz` endpoint).

**Safety:** The rebuild executor only runs on Linux (where systemd is available). Returns an error on other platforms with a message directing the operator to restart manually.

**Privileges:** Two privilege grants are required, both installed by the deployment script (`gce-start-hub.sh`):
- **Binary installation:** A sudoers rule at `/etc/sudoers.d/scion-install-binary` allows the `scion` user to run the specific `install` command for placing the rebuilt binary into `/usr/local/bin/`. The rule is narrowly scoped to the exact source and destination paths.
- **Service restart:** A polkit rule at `/etc/polkit-1/rules.d/50-scion-hub-restart.rules` grants the `scion` user permission to manage the `scion-hub.service` unit without interactive authentication.

**Why staging + sudo install:** The service user cannot write directly to `/usr/local/bin/`, and writing directly to the running binary would fail with `ETXTBSY` on Linux. Building to a staging path in the repo directory (owned by the service user) avoids both problems. The `install` command atomically creates a new file at the destination rather than overwriting in-place.

**Graceful shutdown:** The hub process handles the restart by building the new binary, installing it, then signaling itself via systemd. The new process picks up the existing database and configuration.

#### Rebuild Web Executor

A lighter variant that only rebuilds frontend assets:

```go
type RebuildWebExecutor struct {
    repoPath string
}
```

Steps:
1. `git pull` in the repo directory.
2. `make web` to rebuild frontend assets.
3. No restart needed — static assets are served from disk.

#### Secret Migration Executor

Wraps the existing `runSecretMigrate` logic from `cmd/hub_secret_migrate.go`:

```go
type SecretMigrationExecutor struct {
    store       store.Store
    gcpConfig   secret.GCPBackendConfig
    hubID       string
}
```

Steps:
1. Create GCP backend with the hub's resolved hub ID.
2. List all secrets from the database.
3. For each non-migrated secret, write to GCP SM and update the DB ref.
4. Report migrated/skipped counts in result JSON.

Supports `dryRun` parameter to preview without changes.

### 3.7 Web UI Page

**Route:** `/admin/maintenance`
**Component:** `scion-page-admin-maintenance`
**Nav entry:** Added to the Admin section in `nav.ts` between "Scheduler" and "Users":

```typescript
{ path: '/admin/maintenance', label: 'Maintenance', icon: 'wrench-adjustable' }
```

**Layout:**

The page has two main sections:

#### Migrations Section

A card-based list of one-time migrations. Each card shows:

```
┌─────────────────────────────────────────────────────────┐
│ ○ Secret Hub ID Namespace Migration          [Pending]  │
│                                                         │
│ Migrates hub-scoped secrets from the legacy fixed       │
│ "hub" scope ID to the per-instance hub ID.              │
│                                                         │
│ Created: 2026-03-30                                     │
│                                               [Run ▶]   │
└─────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────┐
│ ✓ Example Completed Migration               [Completed] │
│                                                         │
│ Description of what this migration did.                 │
│                                                         │
│ Created: 2026-02-15  |  Completed: 2026-03-01           │
│ By: admin@example.com                                   │
└─────────────────────────────────────────────────────────┘
```

- Pending migrations have a **Run** button.
- Running migrations show a spinner and disable the button.
- Completed migrations show a checkmark with completion date.
- Failed migrations show an error icon, the error message, and a **Retry** button.

#### Operations Section

A card-based list of routine operations with a **Run** button and expandable history:

```
┌─────────────────────────────────────────────────────────┐
│ Pull Container Images                         [Run ▶]   │
│                                                         │
│ Pulls the latest container images for all configured    │
│ harnesses from the image registry.                      │
│                                                         │
│ Last run: 2 hours ago (completed) by admin@example.com  │
│                                                         │
│ ▸ Run History (3 runs)                                  │
└─────────────────────────────────────────────────────────┘
```

Expanding "Run History" shows a table of recent runs:

```
│  Started           │ Duration │ Status    │ By                │
│  Mar 30, 14:32     │ 45s      │ Completed │ admin@example.com │
│  Mar 28, 09:15     │ 52s      │ Completed │ admin@example.com │
│  Mar 25, 11:00     │ --       │ Failed    │ admin@example.com │
```

Clicking a run row opens a detail view with the captured log output.

#### Parameter Dialogs

Operations that accept parameters (e.g., `pull-images` with registry/tag overrides) show a dialog before execution:

```
┌──────────────────────────────────────┐
│  Pull Container Images               │
│                                      │
│  Registry: [ghcr.io/myorg      ]    │
│  Tag:      [latest             ]    │
│                                      │
│            [Cancel]  [Run]           │
└──────────────────────────────────────┘
```

Default values are populated from the server configuration.

### 3.8 Execution Model

Operations run asynchronously on the server. The API returns immediately with a `runId`, and the client polls for status. This avoids HTTP timeout issues for long-running operations like image pulls.

```go
func (s *Server) executeOperation(ctx context.Context, key string, executor MaintenanceExecutor, params map[string]string, userID string) (string, error) {
    runID := api.NewUUID()
    // Create run record with status "running"
    // Launch goroutine to execute
    go func() {
        var buf bytes.Buffer
        err := executor.Run(ctx, &buf, params)
        // Update run record with status, log, result
    }()
    return runID, nil
}
```

For migrations, the operation record itself is updated (no separate run table entry needed since migrations run at most once).

---

## 4. Affected Files

### New Files

| File | Purpose |
|------|---------|
| `pkg/hub/admin_maintenance.go` | API handlers for maintenance operations endpoints |
| `pkg/hub/maintenance_executors.go` | Executor implementations for each operation |
| `web/src/components/pages/admin-maintenance.ts` | Web UI page component |

### Modified Files

| File | Change |
|------|--------|
| `pkg/store/models.go` | Add `MaintenanceOperation` and `MaintenanceOperationRun` model structs |
| `pkg/store/store.go` | Add `MaintenanceStore` interface methods |
| `pkg/store/sqlite/sqlite.go` | Add V41 migration for new tables + seed data |
| `pkg/hub/server.go` | Register new API endpoints, wire up executors |
| `web/src/components/shared/nav.ts` | Add "Maintenance" nav item to admin section |
| `web/src/client/main.ts` | Add route for `/admin/maintenance` |
| `web/scripts/copy-shoelace-icons.mjs` | Add `wrench-adjustable` to `USED_ICONS` |

---

## 5. Store Interface

```go
// MaintenanceStore defines storage operations for maintenance tasks.
type MaintenanceStore interface {
    // ListMaintenanceOperations returns all registered operations and migrations.
    ListMaintenanceOperations(ctx context.Context) ([]MaintenanceOperation, error)

    // GetMaintenanceOperation returns a single operation by key.
    GetMaintenanceOperation(ctx context.Context, key string) (*MaintenanceOperation, error)

    // UpdateMaintenanceOperation updates an operation's status and result fields.
    UpdateMaintenanceOperation(ctx context.Context, op *MaintenanceOperation) error

    // CreateMaintenanceRun inserts a new run record.
    CreateMaintenanceRun(ctx context.Context, run *MaintenanceOperationRun) error

    // UpdateMaintenanceRun updates a run's status, result, and log.
    UpdateMaintenanceRun(ctx context.Context, run *MaintenanceOperationRun) error

    // GetMaintenanceRun returns a single run by ID.
    GetMaintenanceRun(ctx context.Context, id string) (*MaintenanceOperationRun, error)

    // ListMaintenanceRuns returns runs for a given operation key, ordered by started_at DESC.
    ListMaintenanceRuns(ctx context.Context, operationKey string, limit int) ([]MaintenanceOperationRun, error)
}
```

---

## 6. Security Considerations

- All endpoints require admin role. Non-admin users receive 403.
- Operation executors run server-side with the hub process's permissions. They do NOT accept arbitrary shell commands — only predefined executors are available.
- The `rebuild-server` executor is inherently destructive (restarts the process). The UI should show a confirmation dialog with a warning.
- Parameters are validated against allowlists (e.g., registry must match configured registries, tag must be alphanumeric).
- Captured logs may contain sensitive information (image registry URLs, build output). They are stored in the database and only visible to admins.

---

## 7. Rollout

### Phase 1: Migration Tracking -- COMPLETE

- Add database table and seed the secret migration entry.
- Add list/get API endpoints (read-only).
- Add the web UI page with migration checklist (display only, no execution).
- Add nav entry.

### Phase 2: Migration Execution -- COMPLETE

- Add the `POST .../run` endpoint for migrations.
- Implement the `SecretMigrationExecutor`.
- Wire up the Run button in the UI with parameter dialog (dry-run checkbox).
- Add `MaintenanceExecutor` interface for future operation executors.
- Add status polling for running migrations.

### Phase 3: Routine Operations -- COMPLETE

- Add the operation runs table and run endpoints.
- Implement `PullImagesExecutor` and `RebuildServerExecutor`.
- Add the Operations section to the UI with run history.
- Add the `RebuildWebExecutor`.

---

## 8. Future Work (Out of Scope)

- **Build container images from the UI** — More complex than pulling, requires buildx setup. Keep as CLI-only for now.
- **Database backup/restore** — Important but needs careful design around SQLite file locking and data integrity.
- **Log streaming** — Real-time log output via SSE/WebSocket during operation execution. Phase 1-3 uses polling.
- **Operation scheduling** — Allow scheduling operations via cron (e.g., nightly image pulls). Could integrate with the existing Scheduler system.
- **Multi-hub operation broadcast** — Triggering operations across multiple hub instances from a central dashboard.
- **Custom operation plugins** — Allow operators to register custom maintenance scripts via configuration.
