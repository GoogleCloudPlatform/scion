# Hub Agent Soft Delete

**Status:** Proposed
**Updated:** 2026-02-22

## 1. Overview

This document specifies the design for soft-deleting agents in the Scion Hub. Currently, `DELETE /api/v1/agents/{id}` performs an immediate hard delete—removing the agent record from the database and dispatching container/filesystem cleanup to the runtime broker. There is no recovery path.

Soft delete introduces a grace period between the delete request and permanent purging. During this grace period, agents are marked as `deleted` but remain in the database, allowing recovery from accidental deletions and forensic investigation of agent history.

### 1.1 Goals

- **Recoverable deletion**: Deleted agents enter a `deleted` state with a configurable retention period before permanent purge.
- **Zero-disruption default**: The default retention is `0` (immediate purge), preserving current behavior for users who don't opt in.
- **Human-friendly configuration**: Retention duration is specified as a human-readable string (e.g., `72h`, `168h` for 7 days).
- **Invisible by default**: Soft-deleted agents are excluded from standard list views unless explicitly requested.
- **Automatic purging**: The Hub server runs a periodic background loop to purge expired soft-deleted agents.
- **Undelete support**: Agents in the `deleted` state can be restored before the retention period expires.

### 1.2 Non-Goals (This Iteration)

- Per-grove or per-agent retention overrides (retention is a single hub-wide setting).
- Retention of runtime artifacts (containers, worktrees). These are cleaned up at delete time as today; only the Hub database record is retained.
- Archival to external storage (e.g., exporting deleted agent records to GCS).
- UI/web frontend changes (CLI and API only in this iteration).

---

## 2. Configuration

### 2.1 Hub Server Setting

Soft-delete retention is configured in the Hub server configuration under a new field on `HubServerConfig`:

```go
// In pkg/config/hub_config.go

type HubServerConfig struct {
    // ... existing fields ...

    // SoftDeleteRetention is the duration to retain deleted agent records
    // before permanent purge. Specified as a Go duration string (e.g., "72h",
    // "168h", "720h"). A value of "0" or "" means immediate deletion (no
    // soft delete). Default: "0".
    SoftDeleteRetention time.Duration `json:"softDeleteRetention" yaml:"softDeleteRetention" koanf:"softDeleteRetention"`

    // SoftDeleteRetainFiles controls whether agent workspace files are
    // preserved on the broker during soft-delete. When true, deleteFiles
    // defaults to false for soft-deleted agents (files are kept for restore).
    // When false (default), deleteFiles=true remains the default and files
    // are removed immediately even during soft-delete.
    SoftDeleteRetainFiles bool `json:"softDeleteRetainFiles" yaml:"softDeleteRetainFiles" koanf:"softDeleteRetainFiles"`
}
```

### 2.2 V1 Settings Format

In the versioned settings file (`settings.yaml`), this maps to:

```yaml
server:
  hub:
    soft_delete_retention: "168h"  # 7 days
    soft_delete_retain_files: false  # default: remove files immediately
```

The `V1ServerHubConfig` struct gains:

```go
// In pkg/config/settings_v1.go

type V1ServerHubConfig struct {
    // ... existing fields ...

    // SoftDeleteRetention is the retention period for soft-deleted agents.
    SoftDeleteRetention string `json:"soft_delete_retention,omitempty" yaml:"soft_delete_retention,omitempty" koanf:"soft_delete_retention"`

    // SoftDeleteRetainFiles controls whether workspace files are preserved during soft-delete.
    SoftDeleteRetainFiles *bool `json:"soft_delete_retain_files,omitempty" yaml:"soft_delete_retain_files,omitempty" koanf:"soft_delete_retain_files"`
}
```

### 2.3 Environment Variable Override

The retention can also be set via environment variable:

```
SCION_SERVER_HUB_SOFT_DELETE_RETENTION=168h
SCION_SERVER_HUB_SOFT_DELETE_RETAIN_FILES=true
```

### 2.4 ServerConfig Plumbing

The parsed duration is passed through to `hub.ServerConfig` so the Hub server has access at runtime:

```go
// In pkg/hub/server.go

type ServerConfig struct {
    // ... existing fields ...

    // SoftDeleteRetention is the retention period for soft-deleted agents.
    // Zero means immediate hard delete (default behavior).
    SoftDeleteRetention time.Duration

    // SoftDeleteRetainFiles controls whether workspace files are preserved
    // on the broker during soft-delete. Default: false.
    SoftDeleteRetainFiles bool
}
```

---

## 3. Agent Status: `deleted`

### 3.1 New Status Constants

Two new agent status constants are added to the existing set:

```go
// In pkg/store/models.go

const (
    // ... existing statuses ...
    AgentStatusDeleted  = "deleted"
    AgentStatusRestored = "restored"
)
```

- `deleted`: The agent has been soft-deleted and is awaiting purge.
- `restored`: The agent was restored from `deleted` state. No container or runtime artifacts exist—the agent must be re-provisioned via `scion start` before it can run again. This is distinct from `stopped`, which implies a container previously existed and was halted.

### 3.2 New Timestamp Field

The `Agent` model gains a `DeletedAt` field to track when the agent was soft-deleted:

```go
// In pkg/store/models.go

type Agent struct {
    // ... existing fields ...

    // DeletedAt is the timestamp when the agent was soft-deleted.
    // Zero value means the agent has not been deleted.
    DeletedAt time.Time `json:"deletedAt,omitempty"`
}
```

This field is also added to `api.AgentInfo`:

```go
// In pkg/api/types.go

type AgentInfo struct {
    // ... existing fields ...

    // DeletedAt is the timestamp when the agent was soft-deleted (zero if not deleted).
    DeletedAt time.Time `json:"deletedAt,omitempty"`
}
```

The `ToAPI()` conversion in `store/models.go` maps `DeletedAt` accordingly.

### 3.3 Database Schema

The agents table requires:
- A new `deleted_at` column (`TIMESTAMP`, nullable, default `NULL`).
- The existing `status` column already supports arbitrary string values, so `"deleted"` requires no schema change.

For SQLite (the current store implementation), a migration adds:

```sql
ALTER TABLE agents ADD COLUMN deleted_at TIMESTAMP;
CREATE INDEX idx_agents_deleted_at ON agents(deleted_at) WHERE deleted_at IS NOT NULL;
```

---

## 4. Deletion Flow

### 4.1 Delete Handler Changes

The `deleteAgent` handler in `pkg/hub/handlers.go` changes behavior based on the configured retention:

```
DELETE /api/v1/agents/{id}?deleteFiles=true&removeBranch=true
```

**When retention is 0 (default):** Behavior is unchanged—immediate hard delete with runtime dispatch.

**When retention > 0:**

1. Verify broker availability (unchanged).
2. Dispatch container/filesystem cleanup to the runtime broker. The `deleteFiles` behavior depends on the `SoftDeleteRetainFiles` hub setting:
   - `SoftDeleteRetainFiles: false` (default): `deleteFiles` uses the caller-supplied value (default `true`). Files are removed immediately as today.
   - `SoftDeleteRetainFiles: true`: `deleteFiles` is forced to `false` for soft-deletes, preserving workspace files on the broker for a more complete restore. The caller-supplied `deleteFiles` value is ignored.
3. Instead of `store.DeleteAgent(id)`, call `store.UpdateAgent()` to set:
   - `Status` → `"deleted"`
   - `DeletedAt` → `time.Now()`
4. Publish `AgentDeleted` event (unchanged—the agent is effectively gone from the user's perspective).
5. Return `204 No Content` (unchanged).

```go
func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, id string) {
    // ... existing broker check and dispatch logic ...

    if s.config.SoftDeleteRetention > 0 {
        // Soft delete: mark as deleted, retain record
        agent.Status = store.AgentStatusDeleted
        agent.DeletedAt = time.Now()
        if err := s.store.UpdateAgent(ctx, agent); err != nil {
            writeErrorFromErr(w, err, "")
            return
        }
    } else {
        // Hard delete: remove record immediately (current behavior)
        if err := s.store.DeleteAgent(ctx, id); err != nil {
            writeErrorFromErr(w, err, "")
            return
        }
    }

    s.events.PublishAgentDeleted(ctx, agent.ID, agent.GroveID)
    w.WriteHeader(http.StatusNoContent)
}
```

### 4.2 Force Delete

A `force=true` query parameter bypasses soft delete and performs immediate hard deletion regardless of the retention setting:

```
DELETE /api/v1/agents/{id}?force=true
```

This is useful for operators who want to immediately purge a specific agent.

### 4.3 Broker-Side Agent Info Marking

When the Hub dispatches a soft-delete to the runtime broker, the broker must update the local `agent-info.json` to reflect the deleted state before performing container cleanup. This ensures that the broker's local filesystem is consistent with the Hub even if the broker later loses connectivity.

**Why this matters:** If the broker retained an `agent-info.json` with a non-deleted status, local CLI listing (`scion ls` in local/no-hub mode) would show a stale agent that no longer exists on the Hub. The mark is also what lets the broker tell retained files apart from a live agent's when the purge later asks it to remove them (Section 4.4).

**Broker delete handler changes** (`pkg/runtimebroker/handlers.go`):

When the Hub dispatches a delete with soft-delete context (indicated by a `softDelete=true` query parameter and a `deletedAt` timestamp), the broker's delete handler updates `agent-info.json` before proceeding with container cleanup:

```
DELETE /api/v1/agents/{id}?deleteFiles=true&removeBranch=true&softDelete=true&deletedAt=2026-02-22T10:00:00Z
```

```go
func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, id string) {
    // ... existing logic ...

    // If this is a soft-delete, mark agent-info.json before cleanup
    softDelete := query.Get("softDelete") == "true"
    if softDelete {
        deletedAtStr := query.Get("deletedAt")
        if deletedAtStr != "" {
            if deletedAt, err := time.Parse(time.RFC3339, deletedAtStr); err == nil {
                _ = agent.UpdateAgentConfig(id, grovePath, "deleted", "", "")
                // Also write deletedAt to agent-info.json
                _ = agent.UpdateAgentDeletedAt(id, grovePath, deletedAt)
            }
        }
    }

    // Proceed with container cleanup as normal...
}
```

When `deleteFiles=true`, the entire agent directory is removed, so the `agent-info.json` update is moot. The marking only has a lasting effect when `deleteFiles=false` (agent files are preserved for potential restore).

**Hub dispatch changes** (`pkg/hub/httpdispatcher.go`):

`DispatchAgentDelete` passes the soft-delete context when retention is active:

```go
func (d *HTTPAgentDispatcher) DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
    // ... existing logic, with softDelete and deletedAt appended as query parameters ...
}
```

### 4.4 Purge, Retained Files and Disconnected Brokers

A soft delete with `soft_delete_retain_files` leaves the agent's files on the broker. When the retention window passes, the Hub purge loop (Section 7) asks the broker to remove them before it removes the agent record (ptone/scion#4744):

```
DELETE /api/v1/agents/{id}?deleteFiles=true&removeBranch=false&projectId=<project>&runId=<run>&purgeRetained=true
```

- **Which agents:** a soft-deleted row does not record whether its files were kept, so every expired soft-deleted agent with a broker gets this dispatch. For an agent whose files were deleted, the broker finds nothing and answers 404.
- **What the broker removes:** only the files of an agent whose `agent-info.json` carries the soft-delete mark, with no runtime entry and no start, restart or create of the agent in flight, and no symlinked agent directory. Otherwise it answers 409 `retained_files_refused` and removes nothing. While the removal runs, the broker refuses a start, restart or create of that agent with a retryable 409 `retained_purge_in_progress`. The request must carry `projectId` (400 otherwise).
- **Restore safety:** before dispatching, the Hub claims the row with the delete marker (`deletion_state` deleting under a 5-minute lease), so a restore or start is refused while the broker removes the files. A restore that landed first makes the claim match nothing, and the record stays. If the Hub stops mid-dispatch, the lease lapses: a restore is allowed again (and clears the lapsed marker), and the next purge run re-claims the row.
- **Outcomes:**
  - The record is purged when the broker removed the files or had none (204/404), another run holds the agent's name there, the broker refused (`retained_files_refused`, logged with its reason), or the request was rejected as invalid (400).
  - The record is kept, with the claim released, when the broker is offline or unreachable, or the dispatch fails or times out. The next run retries.
  - A broker that no longer exists: the record is purged, and a warning says any retained files were left on it.
  - A broker that stays registered but unreachable: the record is kept until 30 days past the retention window (`retainedPurgeGiveUp`). It is then purged, and a warning says the retained files were left on that broker.

A broker that was offline at soft-delete time never soft-deleted the agent: the Hub's `checkBrokerAvailability` rejected the delete (HTTP 503), and the agent stays in its previous state.

If a broker reconnects after a purge and attempts to sync agents, agents that were purged from the Hub will not be found. The broker should handle "not found" responses gracefully during sync.

---

## 5. Listing and Filtering

### 5.1 AgentFilter Changes

The `AgentFilter` struct gains a field to control inclusion of deleted agents:

```go
// In pkg/store/store.go

type AgentFilter struct {
    GroveID         string
    RuntimeBrokerID string
    Status          string
    OwnerID         string
    IncludeDeleted  bool   // When false (default), exclude agents with status "deleted"
}
```

### 5.2 Store Implementation

The `ListAgents` query adds a default exclusion:

```sql
-- When IncludeDeleted is false (default):
WHERE status != 'deleted'
-- Combined with any other status filter
```

When `Status` is explicitly set to `"deleted"`, only deleted agents are returned (overrides `IncludeDeleted`).

When `IncludeDeleted` is `true`, all agents are returned regardless of deleted status.

### 5.3 API Query Parameter

The list agents endpoint accepts a new query parameter:

```
GET /api/v1/agents?includeDeleted=true
```

Handler change in `listAgents`:

```go
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
    query := r.URL.Query()

    filter := store.AgentFilter{
        GroveID:         query.Get("groveId"),
        RuntimeBrokerID: query.Get("runtimeBrokerId"),
        Status:          query.Get("status"),
        IncludeDeleted:  query.Get("includeDeleted") == "true",
    }
    // ...
}
```

### 5.4 CLI List Command

The `scion list` command gains a `--deleted` flag that sets `includeDeleted=true` (or `status=deleted` to show only deleted agents). By default, deleted agents are hidden.

### 5.5 Local-Mode Deleted Agent Warning

When the CLI lists agents in local mode (Hub unavailable or disabled), it reads `agent-info.json` from the filesystem. If an agent's `agent-info.json` has `status: "deleted"` and a `deletedAt` timestamp, the CLI should:

1. Display the agent with a `deleted` status indicator.
2. Print a warning: `"Agent '<name>' was deleted on the Hub (<deletedAt>) and may have been purged."`

This addresses the case where a broker retains agent files (`deleteFiles=false`) but the Hub has since purged the database record. The warning informs the user that the agent's Hub record may no longer exist and the local files are remnants.

```go
// In pkg/agent/list.go, when reading agent-info.json:
if info.Status == "deleted" && !info.DeletedAt.IsZero() {
    agents[i].Warnings = append(agents[i].Warnings,
        fmt.Sprintf("Agent was deleted on the Hub (%s) and may have been purged",
            info.DeletedAt.Format("2006-01-02")))
}
```

---

## 6. Undelete (Restore)

### 6.1 API Endpoint

A new action restores a soft-deleted agent:

```
POST /api/v1/agents/{id}/restore
```

This action:
1. Verifies the agent exists and is in `deleted` status.
2. Sets `Status` to `restored`. This status is distinct from `stopped` because no container or runtime artifacts exist—the agent must be re-provisioned via `scion start` before it can run again.
3. Clears `DeletedAt` to zero value.
4. Publishes an `AgentCreated` event to notify subscribers.
5. Returns `200 OK` with the restored agent record.

```go
func (s *Server) restoreAgent(w http.ResponseWriter, r *http.Request, id string) {
    ctx := r.Context()

    agent, err := s.store.GetAgent(ctx, id)
    if err != nil {
        writeErrorFromErr(w, err, "")
        return
    }

    if agent.Status != store.AgentStatusDeleted {
        BadRequest(w, "Agent is not in deleted state")
        return
    }

    agent.Status = store.AgentStatusRestored
    agent.DeletedAt = time.Time{}
    if err := s.store.UpdateAgent(ctx, agent); err != nil {
        writeErrorFromErr(w, err, "")
        return
    }

    s.events.PublishAgentCreated(ctx, agent)
    writeJSON(w, http.StatusOK, agent)
}
```

### 6.2 CLI Command

```
scion restore <agent-name-or-id>
```

Calls the restore endpoint. Requires the agent to be in `deleted` status.

---

## 7. Automatic Purge Loop

### 7.1 Background Goroutine

When `SoftDeleteRetention > 0`, the Hub server starts a background goroutine during `Start()` that periodically purges expired soft-deleted agents:

```go
func (s *Server) startPurgeLoop(ctx context.Context) {
    if s.config.SoftDeleteRetention <= 0 {
        return
    }

    ticker := time.NewTicker(1 * time.Hour)
    go func() {
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                s.purgeExpiredAgents(ctx)
            }
        }
    }()
}
```

### 7.2 Purge Logic

The purge first settles each expired agent's retained files on its broker (Section 4.4, `pkg/hub/retained_files_purge.go`): it pages through `ListSoftDeletedAgentsBefore(cutoff)`, dispatches the `purgeRetained` delete under a claim, and purges the settled records with `PurgeDeletedAgentsByID(ids, cutoff)`, which keeps `PurgeDeletedAgents`' transaction, restore re-check and cascade. The sketch below is the original, broker-free form.

```go
func (s *Server) purgeExpiredAgents(ctx context.Context) {
    cutoff := time.Now().Add(-s.config.SoftDeleteRetention)

    purged, err := s.store.PurgeDeletedAgents(ctx, cutoff)
    if err != nil {
        slog.Error("Failed to purge deleted agents", "error", err)
        return
    }

    if purged > 0 {
        slog.Info("Purged expired soft-deleted agents", "count", purged, "cutoff", cutoff)
    }
}
```

### 7.3 New Store Method

```go
// In pkg/store/store.go

type AgentStore interface {
    // ... existing methods ...

    // PurgeDeletedAgents permanently removes all agents with status "deleted"
    // whose DeletedAt is before the given cutoff time.
    // Returns the number of agents purged.
    PurgeDeletedAgents(ctx context.Context, cutoff time.Time) (int, error)
}
```

SQL implementation:

```sql
DELETE FROM agents WHERE status = 'deleted' AND deleted_at < ?
```

### 7.4 Lifecycle Integration

- `startPurgeLoop` is called from `Server.Start()`.
- The goroutine exits when the server's context is cancelled during `Shutdown()`.
- The purge loop logs each cycle's results at INFO level.

---

## 8. GetAgent Behavior

`GetAgent` and `GetAgentBySlug` continue to return agents in `deleted` status. Callers that need to exclude deleted agents should check the status field. This ensures:

- The restore endpoint can find deleted agents.
- The purge loop can query for expired agents.
- The delete handler's idempotency is preserved (deleting an already-deleted agent is a no-op 204).

---

## 9. Impact on Agent Counts

The `AgentCount` computed field on `Grove` (populated during listing) should exclude `deleted` agents by default. The store query for grove agent counts should filter on `status != 'deleted'`.

---

## 10. Summary of Changes

| Component | File(s) | Change |
|-----------|---------|--------|
| Agent model | `pkg/store/models.go` | Add `AgentStatusDeleted` and `AgentStatusRestored` constants, `DeletedAt` field |
| API types | `pkg/api/types.go` | Add `DeletedAt` field to `AgentInfo` |
| Store interface | `pkg/store/store.go` | Add `IncludeDeleted` to `AgentFilter`, add `PurgeDeletedAgents` method |
| Store implementation | `pkg/store/sqlite.go` (or equivalent) | Implement filter exclusion, purge query, migration |
| Hub config | `pkg/config/hub_config.go` | Add `SoftDeleteRetention` and `SoftDeleteRetainFiles` to `HubServerConfig` |
| V1 settings | `pkg/config/settings_v1.go` | Add `SoftDeleteRetention` and `SoftDeleteRetainFiles` to `V1ServerHubConfig`, conversion logic |
| Hub server config | `pkg/hub/server.go` | Add `SoftDeleteRetention` and `SoftDeleteRetainFiles` to `ServerConfig` |
| Delete handler | `pkg/hub/handlers.go` | Conditional soft vs hard delete, `force` parameter |
| List handler | `pkg/hub/handlers.go` | Pass `includeDeleted` query param to filter |
| Restore handler | `pkg/hub/handlers.go` | New `restore` action on agent |
| Purge loop | `pkg/hub/server.go` | Background goroutine for periodic purge |
| Hub dispatcher | `pkg/hub/httpdispatcher.go` | Pass `softDelete` and `deletedAt` params to broker delete |
| Broker delete handler | `pkg/runtimebroker/handlers.go` | Mark `agent-info.json` with deleted status before cleanup |
| Agent info update | `pkg/agent/provision.go` | New `UpdateAgentDeletedAt` helper to write `deletedAt` to agent-info |
| Local agent listing | `pkg/agent/list.go` | Emit warning for agents with `status: deleted` in agent-info |
| CLI delete | `cmd/delete.go` | No change needed (backend handles soft delete transparently) |
| CLI list | `cmd/list.go` | Add `--deleted` flag |
| CLI restore | `cmd/restore.go` | New command to restore soft-deleted agents |
| Tests | Various `_test.go` | Test soft delete, restore, purge, filter exclusion, force delete, broker-side marking |

---

## 11. Design Decisions

1. **Restore sets `restored` status, not `stopped`**: Restored agents enter a new `restored` state rather than `stopped`. This makes it explicit that no container or runtime artifacts exist and the agent must be re-provisioned via `scion start`. Restore does not trigger automatic re-provisioning.

2. **`deleteFiles=true` remains the default**: Files are removed immediately during soft-delete by default. Operators who want file retention for a more complete restore can enable `SoftDeleteRetainFiles` in hub settings, which forces `deleteFiles=false` for soft-deleted agents.

3. **Broker sync on reconnect** (future enhancement): When a broker reconnects to the Hub after being offline, a reconciliation step could clean up stale local agent state by checking which agents have been purged on the Hub. This is out of scope for this iteration but noted as a future improvement.