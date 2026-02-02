# Hosted Architecture End-to-End Milestone Walkthrough

**Created:** 2026-02-02
**Status:** Planning Document
**Goal:** Enable end-to-end manual integration testing of the hosted architecture

---

## 1. Target Milestone Scenarios

The following end-to-end user scenarios define the milestone:

1. **Authenticate the CLI** with the Hub
2. **Use a locally defined template** to start an agent (exercising remote template infrastructure - push to cloud storage and register with Hub)
3. **Attach to the agent** and interact with it over tmux
4. **Synchronize the workspace** back to the local machine
5. **Stop the agent**
6. **Remove the agent**

These scenarios should work with Hub server and Runtime Host running on different machines (or emulated via separate processes on the same machine).

---

## 2. Current Implementation Status

### 2.1 What's Fully Implemented

| Component | Status | Key Files |
|-----------|--------|-----------|
| **CLI Authentication** | ✅ Complete | `cmd/hub_auth.go` |
| - OAuth browser-based login | ✅ | `scion hub auth login` |
| - Dev auth fallback | ✅ | |
| - Credential storage | ✅ | `pkg/credentials/` |
| **Template Management** | ✅ Complete | `cmd/templates.go` |
| - `scion template sync` (create/update in Hub) | ✅ | |
| - `scion template push` (upload files to GCS) | ✅ | |
| - `scion template pull` (download from Hub) | ✅ | |
| - GCS storage via rclone | ✅ | `pkg/gcp/storage.go` |
| - Signed URL generation | ✅ | `pkg/hub/template_handlers.go` |
| **Hub Registration** | ✅ Complete | `cmd/hub.go` |
| - `scion hub register` | ✅ | |
| - `scion hub deregister` | ✅ | |
| - `scion hub status` | ✅ | |
| - HMAC-based host authentication | ✅ | `pkg/hub/hostauth.go`, `pkg/runtimehost/hostauth.go` |
| - Bidirectional HMAC (Hub→Host signing) | ✅ | `pkg/hub/hostclient.go` |
| - Secret rotation endpoint | ✅ | `POST /api/v1/hosts/{id}/rotate-secret` |
| - Nonce cache (replay prevention) | ✅ | Enabled by default |
| **Agent Lifecycle (Hub Mode)** | ✅ Complete | `cmd/create.go`, `cmd/start.go`, `cmd/stop.go`, `cmd/delete.go` |
| - Create via Hub | ✅ | |
| - Start via Hub | ✅ | |
| - Stop via Hub | ✅ | |
| - Delete via Hub | ✅ | |
| **HTTP Dispatcher** | ✅ Complete | `pkg/hub/httpdispatcher.go` |
| - Dispatch to remote Runtime Hosts via HTTP | ✅ | |
| - Authenticated dispatch (HMAC-signed) | ✅ | `pkg/hub/hostclient.go` |
| **Runtime Host API** | ✅ Complete | `pkg/runtimehost/` |
| - Agent lifecycle endpoints | ✅ | |
| - Template cache/hydration | ✅ | `pkg/templatecache/` |
| - Heartbeat to Hub | ✅ | `pkg/runtimehost/heartbeat.go` |
| - Strict auth mode (configurable) | ✅ | `HostAuthStrictMode` config |
| **Observability** | ✅ Complete | |
| - Audit logging | ✅ | `pkg/hub/audit.go` |
| - Host auth metrics | ✅ | `pkg/hub/metrics.go`, `/metrics` endpoint |

### 2.2 What's NOT Implemented (Blocking Scenarios)

| Component | Status | Impact | Key Files |
|-----------|--------|--------|-----------|
| **PTY/Attach via Hub** | ❌ Not Implemented | Blocks Scenario 3 | `cmd/attach.go:33` |
| - WebSocket PTY relay | ❌ | | `runtimehost-websocket.md` |
| - PTY stream multiplexing | ❌ | | |
| **Workspace Sync (Hosted)** | ❌ Not Implemented | Blocks Scenario 4 | `cmd/sync.go` |
| - Sync workspace files to/from remote host | ❌ | | |
| - rclone integration for workspace | ❌ | | |
| **WebSocket Control Channel** | ⚠️ Not Implemented | Works around via HTTP | |
| - Hub-initiated commands | ❌ | | |
| - NAT/firewall traversal | ❌ | | |

### 2.3 Current Behavior of Blocking Features

**Attach (`scion attach`):**
```go
// cmd/attach.go:33
if hubCtx != nil {
    return fmt.Errorf("attach is not yet supported when using Hub integration\n\nTo attach locally, use: scion --no-hub attach %s", agentName)
}
```

**Workspace Sync:**
- `cmd/sync.go` exists for local sync (tar-based or mutagen)
- No hosted mode implementation
- The rclone library is already imported and used for templates (`pkg/gcp/storage.go`)

---

## 3. Scenario-by-Scenario Analysis

### Scenario 1: Authenticate the CLI ✅

**Status:** Fully implemented

**Commands:**
```bash
# Set Hub endpoint
export SCION_HUB_ENDPOINT=http://hub.example.com:9000

# Authenticate via browser OAuth
scion hub auth login

# Verify authentication
scion hub status
```

**What Happens:**
1. CLI opens browser for OAuth flow
2. User authenticates with OAuth provider
3. Access token stored in `~/.config/scion/credentials/<endpoint-hash>/credentials.json`
4. Subsequent commands use stored token

**No Implementation Work Required.**

---

### Scenario 2: Use Local Template to Start Agent ⚠️

**Status:** Partially implemented - **requires configuration and testing**

**Commands:**
```bash
# Push local template to Hub (uploads to GCS, registers in Hub)
scion template sync custom-claude \
  --from .scion/templates/claude \
  --scope grove \
  --harness claude

# Start agent using the template
scion start my-agent --type custom-claude "Fix the login bug"
```

**What Works:**
- Template sync/push/pull CLI commands
- GCS storage via rclone
- Signed URL generation
- Template resolution in agent creation
- HTTP dispatch to Runtime Host
- Template hydration on Runtime Host

**Configuration Required:**

1. **GCS Bucket Setup:**
   - Create bucket: `gs://scion-hub-<env>/`
   - Configure in Hub settings

2. **Service Account Credentials:**
   - Service account with `storage.objects.create`, `storage.objects.get`
   - `iam.serviceAccounts.signBlob` for signed URLs
   - For dev: `gcloud auth application-default login --impersonate-service-account=<sa>`

3. **Hub Storage Configuration:**
   ```yaml
   hub:
     storage:
       provider: "gcs"
       bucket: "scion-hub-dev"
   ```

4. **Runtime Host Template Cache:**
   ```yaml
   runtimeHost:
     templateCache:
       path: "~/.scion/cache/templates"
       maxSize: "100MB"
   ```

**Gap: Runtime Host Endpoint Discovery**

When Hub dispatches to Runtime Host, it needs the host endpoint URL. Currently:
- Host registers with Hub and provides endpoint URL
- Hub stores endpoint in database
- Dispatcher looks up endpoint for dispatch

**Open Question:** How does the Runtime Host determine its externally-reachable endpoint URL when behind NAT?

Options:
1. User explicitly provides endpoint during registration
2. Runtime Host has public IP/hostname configured
3. Fall back to WebSocket control channel (not implemented)

---

### Scenario 3: Attach and Interact via tmux ❌

**Status:** Not implemented - **requires significant work**

**Current Behavior:**
```bash
scion attach my-agent
# Error: attach is not yet supported when using Hub integration
```

**What's Needed:**

**Option A: WebSocket PTY Relay (Recommended)**

Implement the design in `runtimehost-websocket.md`:

1. **Hub Side:**
   - WebSocket endpoint: `WS /api/v1/agents/{id}/pty`
   - Stream mapper to route browser/CLI WebSocket to host stream
   - Proxy WebSocket data between CLI and Runtime Host

2. **Runtime Host Side:**
   - WebSocket endpoint: `WS /api/v1/agents/{id}/attach` or via control channel
   - PTY attachment to tmux session
   - Bidirectional stream handling

3. **CLI Side:**
   - Establish WebSocket connection to Hub
   - Forward stdin/stdout to WebSocket
   - Handle terminal resize events

**Implementation Files:**
- `pkg/hub/pty_handlers.go` (new)
- `pkg/runtimehost/pty_handlers.go` (new)
- `cmd/attach.go` (modify to use WebSocket when Hub enabled)

**Estimated Effort:** 2-3 days

**Option B: Direct SSH to Runtime Host (Simpler, Limited)**

If Runtime Host is directly reachable:
1. Hub returns Runtime Host SSH endpoint
2. CLI SSHs directly to host
3. Attach locally on host

**Limitation:** Doesn't work for NAT-ed hosts

---

### Scenario 4: Synchronize Workspace to Local Machine ❌

**Status:** Not implemented - **requires new implementation**

**What's Needed:**

The rclone library is already imported and used for templates. Leverage it for workspace sync.

**Proposed Design:**

1. **Upload workspace to GCS from Runtime Host:**
   ```go
   // On Runtime Host after agent stops or on-demand
   gcp.SyncToGCS(ctx, agentWorkspacePath, bucket, "workspaces/{groveId}/{agentId}/")
   ```

2. **Download workspace from GCS to CLI:**
   ```go
   // CLI command
   gcp.SyncFromGCS(ctx, bucket, "workspaces/{groveId}/{agentId}/", localPath)
   ```

**CLI Command:**
```bash
# Sync workspace from remote agent to local
scion sync from my-agent

# Sync local changes to remote agent
scion sync to my-agent
```

**Implementation:**

1. **Add Hub endpoint for sync metadata:**
   ```
   GET /api/v1/agents/{id}/workspace
   Response: { syncUri: "gs://bucket/workspaces/...", lastSync: "..." }
   ```

2. **Runtime Host triggers upload:**
   - On agent stop
   - On explicit sync request from Hub
   - Periodic sync for long-running agents

3. **CLI downloads:**
   - Uses rclone to sync from GCS URI to local path
   - Or receives tar archive over HTTP (simpler, less efficient)

**Open Questions:**

1. **Sync trigger:** When should workspace sync happen?
   - On-demand only (explicit command)
   - On agent stop (automatic)
   - Periodic (background)

2. **Conflict handling:** What if local and remote both have changes?
   - Always prefer remote (current agent state)
   - Merge with conflict markers
   - Error and require explicit resolution

3. **Large workspaces:** How to handle multi-GB workspaces?
   - Incremental sync (rclone handles this)
   - Exclude patterns (.git, node_modules, etc.)

**Estimated Effort:** 1-2 days (basic implementation using rclone)

---

### Scenario 5: Stop the Agent ✅

**Status:** Fully implemented

**Commands:**
```bash
scion stop my-agent
```

**What Happens:**
1. CLI calls Hub API: `POST /api/v1/agents/{id}/stop`
2. Hub dispatches to Runtime Host via HTTP
3. Runtime Host stops the agent container

**No Implementation Work Required.**

---

### Scenario 6: Remove the Agent ✅

**Status:** Fully implemented

**Commands:**
```bash
scion delete my-agent
# Or
scion stop my-agent --rm
```

**What Happens:**
1. CLI calls Hub API: `DELETE /api/v1/agents/{id}`
2. Hub dispatches to Runtime Host via HTTP
3. Runtime Host stops container, removes files, optionally removes git branch
4. Hub removes agent record from database

**No Implementation Work Required.**

---

## 4. Implementation Priority

### Phase 1: Configuration & Testing (Day 1)
**Goal:** Verify existing functionality works end-to-end

1. Set up GCS bucket with proper permissions
2. Configure Hub and Runtime Host settings
3. Test template push/pull workflow
4. Test agent create/start/stop/delete workflow
5. Document any issues discovered

### Phase 2: Workspace Sync (Days 2-3)
**Goal:** Enable syncing workspace files

1. Add `workspace` prefix to Hub storage
2. Implement sync trigger on Runtime Host (on-demand initially)
3. Add Hub endpoint for workspace sync metadata
4. Update `scion sync` command for hosted mode
5. Test with rclone

### Phase 3: PTY Attach (Days 4-6)
**Goal:** Enable interactive agent sessions

1. Implement WebSocket PTY endpoint on Hub
2. Implement PTY attachment on Runtime Host
3. Update CLI attach command to use WebSocket
4. Handle terminal resize, disconnect, reconnect
5. Test interactive sessions

### Phase 4: Polish & Documentation (Day 7)
**Goal:** Complete milestone

1. Error handling and edge cases
2. User-facing documentation
3. Integration test script
4. Update status.md

---

## 5. Open Questions for Decision

### Q1: Workspace Sync Direction

When syncing workspaces, what's the primary direction?

**Options:**
- **A. Download-only:** Workspace is authoritative on remote, sync pulls to local
- **B. Bidirectional:** Changes can be made locally and pushed to remote
- **C. On-demand both:** Explicit `sync to` and `sync from` commands (current local behavior)

**Recommendation:** Option C - explicit commands, matching current local behavior

### Q2: Sync Storage Location

Where should workspace snapshots be stored?

**Options:**
- **A. Same bucket as templates:** `gs://scion-hub-{env}/workspaces/{groveId}/{agentId}/`
- **B. Separate bucket per grove:** `gs://{groveId}-workspaces/`
- **C. User-configurable:** Allow different storage backends

**Recommendation:** Option A - simpler, one bucket to manage

### Q3: Attach Authentication

How should CLI authenticate WebSocket connections for PTY?

**Options:**
- **A. Query parameter token:** `ws://hub/agents/{id}/pty?token=<bearer>`
- **B. Ticket-based:** Request short-lived ticket first, use in WebSocket
- **C. Cookie-based:** If Hub shares session with web frontend

**Recommendation:** Option B - more secure, aligns with design doc

### Q4: Runtime Host Endpoint Registration

How does Runtime Host specify its externally-reachable endpoint?

**Options:**
- **A. Explicit flag:** `scion server start --endpoint http://myhost:9800`
- **B. Auto-detect:** Determine from network interfaces
- **C. Registration response:** Hub tells host its observed IP

**Recommendation:** Option A - explicit is more reliable, especially for dev

---

## 6. Test Setup

### Local Emulation (Single Machine)

Run Hub and Runtime Host as separate processes:

```bash
# Terminal 1: Start Hub
scion server start --enable-hub --hub-port 9000

# Terminal 2: Start Runtime Host (different port)
scion server start --enable-runtime-host --host-port 9800 \
  --hub-endpoint http://localhost:9000 \
  --endpoint http://localhost:9800

# Terminal 3: CLI operations
export SCION_HUB_ENDPOINT=http://localhost:9000
scion hub auth login
scion hub register
scion template sync my-template --from .scion/templates/claude --harness claude
scion start my-agent --type my-template "Hello world"
scion attach my-agent  # Will fail until implemented
scion sync from my-agent  # Will fail until implemented
scion stop my-agent
scion delete my-agent
```

### Distributed Setup

Same commands but with actual different machines:
- Hub: `hub.example.com:9000`
- Runtime Host: `host.example.com:9800`
- CLI: Developer laptop

---

## 7. Success Criteria

The milestone is complete when:

1. ✅ CLI can authenticate with Hub
2. ✅ Local template can be pushed to Hub (GCS)
3. ✅ Agent can be started on remote Runtime Host using pushed template
4. ⬜ CLI can attach to remote agent and interact via tmux
5. ⬜ Workspace can be synced from remote agent to local machine
6. ✅ Agent can be stopped via CLI
7. ✅ Agent can be removed via CLI

All scenarios work with Hub and Runtime Host running as separate processes.

---

## 8. Related Documentation

| Document | Relevance |
|----------|-----------|
| [status.md](status.md) | Overall implementation status |
| [hosted-architecture.md](hosted-architecture.md) | System design |
| [hosted-templates.md](hosted-templates.md) | Template management design |
| [runtimehost-websocket.md](runtimehost-websocket.md) | WebSocket/PTY design |
| [hub-api.md](hub-api.md) | Hub API specification |
| [runtime-host-api.md](runtime-host-api.md) | Runtime Host API specification |
| [hub-api-testing-walkthrough.md](hub-api-testing-walkthrough.md) | Hub API testing guide |
| [runtime-host-testing-walkthrough.md](runtime-host-testing-walkthrough.md) | Runtime Host testing guide |
