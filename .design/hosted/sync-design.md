# Hosted Workspace Sync Design

> **Note (local-only mode removed):** this document describes the CLI-only mode with no Hub ("solo" or "local" mode) as current. That mode was removed; every agent now runs through a Hub (the workstation server on a single machine). See `docs-site/src/content/docs/reference/local-mode-removal.md`.

**Created:** 2026-02-03
**Updated:** 2026-02-11
**Status:** Approved - Ready for Implementation
**Author:** Architecture Team

---

## Table of Contents

1. [Overview](#1-overview)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [Design Options Considered](#3-design-options-considered)
4. [Recommended Approach](#4-recommended-approach)
5. [Architecture](#5-architecture)
6. [Storage Layout](#6-storage-layout)
7. [API Specification](#7-api-specification)
8. [CLI Interface](#8-cli-interface)
9. [Runtime Broker Integration](#9-runtime-broker-integration)
10. [Code Reuse and Factoring](#10-code-reuse-and-factoring)
11. [Incremental Sync](#11-incremental-sync)
12. [Security Considerations](#12-security-considerations)
13. [Workspace Bootstrap at Agent Creation](#13-workspace-bootstrap-at-agent-creation)
14. [Design Decisions](#14-design-decisions)
15. [Implementation Plan](#15-implementation-plan)
16. [References](#16-references)

---

## 1. Overview

### 1.1 Problem Statement

The hosted architecture milestone requires workspace synchronization between remote agents and the local CLI. Currently:

- Agents run on Runtime Brokers that may be behind NAT or on different machines
- The CLI needs to retrieve workspace changes made by agents
- The CLI needs to push local changes to running agents
- The existing local sync (`cmd/sync.go`) uses tar-based or mutagen sync, which doesn't work across the Hub

See [hosted-e2e.md](../walkthroughs/hosted-e2e.md) Section 2.3 - workspace sync is the **only remaining blocker** for the end-to-end milestone.

### 1.2 Current State

**What Exists:**
- Local sync via `scion sync to/from <agent>` using tar or mutagen
- Template storage using GCS with signed URLs (see [hosted-templates.md](hosted-templates.md))
- WebSocket control channel for Hub → Runtime Broker communication (see [runtimebroker-websocket.md](runtimebroker-websocket.md))
- Storage abstraction layer (`pkg/storage/storage.go`)
- File collection utilities (`pkg/hubclient/manifest.go`)

**What's Missing:**
- Hub endpoints for workspace sync
- Runtime Broker workspace upload/download handlers
- CLI hosted sync mode
- Workspace storage path conventions

### 1.3 Key Design Decision

**Reuse the template signed-URL pattern for workspace sync.**

The template system already implements:
- Signed URL generation for direct CLI ↔ GCS transfer
- File manifest with content hashes
- Hub as metadata coordinator (not data path)
- Incremental transfer via hash comparison

This pattern provides excellent performance for large workspaces while keeping the Hub out of the data path.

---

## 2. Goals and Non-Goals

### 2.1 Goals

| Goal | Description |
|------|-------------|
| **Functional parity** | `scion sync to/from` works identically in solo and hosted modes |
| **Incremental sync** | Only transfer changed files (via content hashing) |
| **Large workspace support** | Handle multi-GB workspaces efficiently |
| **NAT traversal** | Work with Runtime Brokers behind NAT/firewalls |
| **Code reuse** | Leverage template storage infrastructure |
| **Bidirectional** | Support both push (to agent) and pull (from agent) |

### 2.2 Non-Goals

| Non-Goal | Rationale |
|----------|-----------|
| Real-time sync | On-demand sync is sufficient for milestone |
| Conflict resolution | Accept "last write wins" semantics initially |
| Partial file sync | Full file granularity, not block-level |
| Automatic sync | Explicit command only (no background daemon) |
| Mutagen integration | Defer hosted mutagen to future work |

---

## 3. Design Options Considered

### 3.1 Option A: Tar via Hub Relay (Staged)

```
CLI → HTTP → Hub (temp file) → Control Channel → Runtime Broker
                ↓
         Hub stores tar temporarily
                ↓
CLI ← HTTP ← Hub
```

**Pros:**
- Simple implementation
- No cloud storage credentials on CLI

**Cons:**
- Full transfer every time (no incremental)
- Hub becomes bandwidth bottleneck
- Temporary storage on Hub (disk pressure)
- Higher latency (two hops)

### 3.2 Option B: HTTP Streaming through Hub

```
CLI → HTTP → Hub → Control Channel → Runtime Broker
       ←────── tar stream (pass-through) ──────
```

**Pros:**
- No temp files on Hub
- Works with NAT

**Cons:**
- Full transfer every time
- Hub still in data path
- HTTP timeout concerns for large workspaces

### 3.3 Option C: GCS Direct Sync (rclone)

```
Runtime Broker → rclone sync → GCS bucket
CLI → rclone sync ← GCS bucket
```

**Pros:**
- Incremental sync
- No Hub bandwidth

**Cons:**
- CLI needs GCS credentials
- Complex auth setup for end users
- Egress costs

### 3.4 Option D: Signed URL Pattern (Template-Style) ✓ Recommended

```
                    Hub
                     │ (metadata only)
    ┌────────────────┴────────────────┐
    │                                  │
    ▼                                  ▼
   CLI ──────── signed URLs ──────── GCS ←───── Runtime Broker
         (direct upload/download)
```

**Pros:**
- Incremental sync via manifest hashes
- No Hub bandwidth for data transfer
- ~60% code reuse from templates
- CLI needs no GCS credentials (uses signed URLs)
- Consistent with template architecture

**Cons:**
- Medium implementation effort
- Requires Runtime Broker to sync to GCS first

**This is the recommended approach** - see Section 4 for details.

### 3.5 Comparison Matrix

| Factor | Tar via Hub | HTTP Stream | GCS Direct | Signed URLs |
|--------|-------------|-------------|------------|-------------|
| Incremental | ❌ | ❌ | ✅ | ✅ |
| CLI auth complexity | Low | Low | High | Low |
| Hub bandwidth | High | High | None | None |
| Implementation effort | Low | Medium | Medium | Medium |
| Large workspace perf | Poor | Poor | Good | Good |
| Code reuse | Low | Low | Medium | High |

---

## 4. Recommended Approach

### 4.1 Core Concept

Apply the same pattern used for templates (see [hosted-templates.md](hosted-templates.md) Section 2):

1. **Hub as Coordinator:** Hub generates signed URLs and stores metadata, but never touches file content
2. **Direct Storage Access:** CLI and Runtime Broker both access GCS directly via signed URLs
3. **Manifest-Based Sync:** File manifest with content hashes enables incremental sync
4. **Existing Infrastructure:** Reuse `pkg/storage`, `pkg/hubclient/manifest.go`, signed URL generation

### 4.2 Sync FROM Agent (Download)

```
┌─────────┐         ┌─────────┐         ┌─────────────┐         ┌─────────┐
│   CLI   │         │   Hub   │         │ Runtime Broker│         │   GCS   │
└────┬────┘         └────┬────┘         └──────┬──────┘         └────┬────┘
     │                   │                     │                      │
     │ POST /agents/{id}/workspace/sync-from   │                      │
     ├──────────────────>│                     │                      │
     │                   │                     │                      │
     │                   │ Tunnel: POST /workspace/upload             │
     │                   ├────────────────────>│                      │
     │                   │                     │                      │
     │                   │                     │ Upload workspace     │
     │                   │                     ├─────────────────────>│
     │                   │                     │                      │
     │                   │ Response: manifest  │                      │
     │                   │<────────────────────┤                      │
     │                   │                     │                      │
     │ Response: {manifest, downloadUrls[]}    │                      │
     │<──────────────────┤                     │                      │
     │                   │                     │                      │
     │ GET files via signed URLs               │                      │
     ├────────────────────────────────────────────────────────────────>│
     │                   │                     │                      │
     │ File content      │                     │                      │
     │<────────────────────────────────────────────────────────────────┤
     │                   │                     │                      │
```

### 4.3 Sync TO Agent (Upload)

```
┌─────────┐         ┌─────────┐         ┌─────────────┐         ┌─────────┐
│   CLI   │         │   Hub   │         │ Runtime Broker│         │   GCS   │
└────┬────┘         └────┬────┘         └──────┬──────┘         └────┬────┘
     │                   │                     │                      │
     │ POST /agents/{id}/workspace/sync-to     │                      │
     │ {files: [{path, size, hash}, ...]}      │                      │
     ├──────────────────>│                     │                      │
     │                   │                     │                      │
     │ Response: {uploadUrls[], existingFiles[]}                      │
     │<──────────────────┤                     │                      │
     │                   │                     │                      │
     │ PUT files via signed URLs (skip existing)                      │
     ├────────────────────────────────────────────────────────────────>│
     │                   │                     │                      │
     │ POST /agents/{id}/workspace/sync-to/finalize                   │
     │ {manifest}        │                     │                      │
     ├──────────────────>│                     │                      │
     │                   │                     │                      │
     │                   │ Tunnel: POST /workspace/apply              │
     │                   ├────────────────────>│                      │
     │                   │                     │                      │
     │                   │                     │ Download from GCS    │
     │                   │                     │<─────────────────────┤
     │                   │                     │                      │
     │                   │                     │ Apply to workspace   │
     │                   │                     │                      │
     │                   │ Response: OK        │                      │
     │                   │<────────────────────┤                      │
     │                   │                     │                      │
     │ Response: OK      │                     │                      │
     │<──────────────────┤                     │                      │
```

---

## 5. Architecture

### 5.1 Component Responsibilities

| Component | Responsibility |
|-----------|----------------|
| **CLI** | Collect local files, upload/download via signed URLs, apply to local workspace |
| **Hub** | Generate signed URLs, coordinate sync requests, tunnel commands to Runtime Broker |
| **Runtime Broker** | Upload workspace to GCS, download from GCS and apply to container |
| **GCS** | Store workspace snapshots, serve signed URL requests |

### 5.2 High-Level Data Flow

```
                         ┌────────────────────────────────────────┐
                         │          Hub Storage Bucket            │
                         │  gs://scion-hub-{env}/                 │
                         │    ├── templates/...                   │
                         │    └── workspaces/                     │
                         │        └── {groveId}/{agentId}/        │
                         │            ├── manifest.json           │
                         │            └── files/...               │
                         └────────────────┬───────────────────────┘
                                          │
              ┌───────────────────────────┼───────────────────────┐
              │                           │                       │
              ▼                           │                       ▼
       ┌─────────────┐                    │              ┌─────────────────┐
       │   Scion     │                    │              │  Runtime Broker   │
       │   CLI       │                    │              │                 │
       │             │◄───────────────────┘              │  ┌───────────┐  │
       │  Workspace  │    Signed URLs                    │  │ Container │  │
       │  Sync       │                                   │  │ Workspace │  │
       └─────────────┘                                   │  └───────────┘  │
              │                                          │                 │
              │  1. Request sync                         │                 │
              └──────────────────────────────────────────┤                 │
                                                         │  2. Upload to   │
                                                         │     GCS         │
                                                         └─────────────────┘
```

### 5.3 Integration with Control Channel

For Runtime Brokers behind NAT, the Hub uses the WebSocket control channel to tunnel HTTP requests (see [runtimebroker-websocket.md](runtimebroker-websocket.md) Section 3.3).

The sync commands are tunneled as standard HTTP requests:
- `POST /api/v1/workspace/upload` - Trigger workspace upload to GCS
- `POST /api/v1/workspace/apply` - Apply GCS workspace to container

---

## 6. Storage Layout

### 6.1 Bucket Structure

Workspaces share the Hub storage bucket with templates, under a `/workspaces` prefix:

```
gs://scion-hub-{env}/
├── templates/                          # Existing (see hosted-templates.md)
│   ├── global/{templateName}/
│   └── groves/{groveId}/{templateName}/
│
└── workspaces/                         # New
    └── {groveId}/
        └── {agentId}/
            ├── manifest.json           # File list with hashes
            ├── metadata.json           # Sync metadata
            └── files/                  # Actual workspace files
                ├── src/
                │   └── main.go
                ├── README.md
                └── ...
```

### 6.2 Storage Path Functions

Add to `pkg/storage/storage.go`:

```go
// WorkspaceStoragePath returns the storage path for an agent's workspace.
func WorkspaceStoragePath(groveID, agentID string) string {
    return "workspaces/" + groveID + "/" + agentID
}

// WorkspaceStorageURI returns the full storage URI for an agent's workspace.
func WorkspaceStorageURI(bucket, groveID, agentID string) string {
    path := WorkspaceStoragePath(groveID, agentID)
    return "gs://" + bucket + "/" + path + "/"
}
```

### 6.3 Manifest Format

The workspace manifest mirrors the template manifest format:

```json
{
  "version": "1.0",
  "agentId": "agent-abc123",
  "groveId": "grove-xyz",
  "syncedAt": "2026-02-03T10:30:00Z",
  "syncedFrom": "runtime-broker",
  "contentHash": "sha256:abc123...",
  "files": [
    {
      "path": "src/main.go",
      "size": 2048,
      "hash": "sha256:def456...",
      "mode": "0644"
    },
    {
      "path": "README.md",
      "size": 512,
      "hash": "sha256:789abc...",
      "mode": "0644"
    }
  ]
}
```

### 6.4 Metadata File

Optional metadata for tracking sync history:

```json
{
  "lastSyncFrom": {
    "timestamp": "2026-02-03T10:30:00Z",
    "source": "runtime-broker",
    "contentHash": "sha256:abc123..."
  },
  "lastSyncTo": {
    "timestamp": "2026-02-03T09:00:00Z",
    "source": "cli",
    "contentHash": "sha256:xyz789..."
  }
}
```

---

## 7. API Specification

### 7.1 Hub Endpoints

#### 7.1.1 Initiate Sync FROM Agent

Triggers Runtime Broker to upload workspace to GCS, returns signed download URLs.

```
POST /api/v1/agents/{agentId}/workspace/sync-from
```

**Request Body:** (optional)
```json
{
  "excludePatterns": [".git/**", "node_modules/**"]
}
```

**Response:** `200 OK`
```json
{
  "manifest": {
    "version": "1.0",
    "contentHash": "sha256:abc123...",
    "files": [
      {"path": "src/main.go", "size": 2048, "hash": "sha256:def456..."}
    ]
  },
  "downloadUrls": [
    {
      "path": "src/main.go",
      "url": "https://storage.googleapis.com/...",
      "size": 2048,
      "hash": "sha256:def456..."
    }
  ],
  "expires": "2026-02-03T10:45:00Z"
}
```

**Errors:**
- `404 Not Found` - Agent not found
- `409 Conflict` - Agent not running
- `504 Gateway Timeout` - Runtime Broker unreachable

#### 7.1.2 Initiate Sync TO Agent

Returns signed upload URLs for workspace files.

```
POST /api/v1/agents/{agentId}/workspace/sync-to
```

**Request Body:**
```json
{
  "files": [
    {"path": "src/main.go", "size": 2048, "hash": "sha256:def456..."},
    {"path": "README.md", "size": 512, "hash": "sha256:789abc..."}
  ]
}
```

**Response:** `200 OK`
```json
{
  "uploadUrls": [
    {
      "path": "src/main.go",
      "url": "https://storage.googleapis.com/...",
      "method": "PUT",
      "headers": {"Content-Type": "application/octet-stream"}
    }
  ],
  "existingFiles": ["README.md"],
  "expires": "2026-02-03T10:45:00Z"
}
```

**Notes:**
- `existingFiles` lists files whose hashes match storage - skip upload
- Enables incremental sync

#### 7.1.3 Finalize Sync TO Agent

After uploading files, finalize applies them to the agent workspace.

```
POST /api/v1/agents/{agentId}/workspace/sync-to/finalize
```

**Request Body:**
```json
{
  "manifest": {
    "version": "1.0",
    "files": [
      {"path": "src/main.go", "size": 2048, "hash": "sha256:def456...", "mode": "0644"}
    ]
  }
}
```

**Response:** `200 OK`
```json
{
  "applied": true,
  "contentHash": "sha256:abc123...",
  "filesApplied": 2,
  "bytesTransferred": 2560
}
```

#### 7.1.4 Get Workspace Status

Returns current workspace sync status.

```
GET /api/v1/agents/{agentId}/workspace
```

**Response:** `200 OK`
```json
{
  "agentId": "agent-abc123",
  "groveId": "grove-xyz",
  "storageUri": "gs://scion-hub-dev/workspaces/grove-xyz/agent-abc123/",
  "lastSync": {
    "direction": "from",
    "timestamp": "2026-02-03T10:30:00Z",
    "contentHash": "sha256:abc123...",
    "fileCount": 15,
    "totalSize": 102400
  }
}
```

### 7.2 Runtime Broker Endpoints

These endpoints are called by the Hub via the control channel tunnel.

#### 7.2.1 Upload Workspace to GCS

```
POST /api/v1/workspace/upload
```

**Request Body:**
```json
{
  "agentId": "agent-abc123",
  "storagePath": "workspaces/grove-xyz/agent-abc123",
  "excludePatterns": [".git/**", "node_modules/**"]
}
```

**Response:** `200 OK`
```json
{
  "manifest": {
    "version": "1.0",
    "contentHash": "sha256:abc123...",
    "files": [...]
  },
  "uploadedFiles": 15,
  "uploadedBytes": 102400
}
```

#### 7.2.2 Apply Workspace from GCS

```
POST /api/v1/workspace/apply
```

**Request Body:**
```json
{
  "agentId": "agent-abc123",
  "storagePath": "workspaces/grove-xyz/agent-abc123",
  "manifest": {
    "version": "1.0",
    "files": [...]
  }
}
```

**Response:** `200 OK`
```json
{
  "applied": true,
  "filesApplied": 15,
  "bytesTransferred": 102400
}
```

---

## 8. CLI Interface

### 8.1 Command Syntax

The existing `scion sync` command is extended to support hosted mode:

```bash
# Sync workspace FROM remote agent to local
scion sync from <agent-name>

# Sync workspace TO remote agent from local
scion sync to <agent-name>

# Options
scion sync from <agent-name> [--exclude <pattern>]... [--dry-run]
scion sync to <agent-name> [--exclude <pattern>]... [--dry-run]
```

### 8.2 Mode Detection

The sync command detects hosted mode via the same mechanism as other commands:

```go
// In cmd/sync.go
hubCtx, err := CheckHubAvailability(grovePath)
if hubCtx != nil {
    // Hosted mode: use Hub API
    return syncViaHub(hubCtx, agentName, direction)
}
// Solo mode: use existing local sync
return rt.Sync(ctx, agentName, direction)
```

### 8.3 Example Usage

```bash
# Start an agent on remote Runtime Broker
scion start my-agent --type claude "Fix the login bug"

# Agent makes changes to workspace...

# Sync changes back to local machine
scion sync from my-agent

# Make local edits...

# Push local changes to running agent
scion sync to my-agent

# Verify sync status
scion sync status my-agent
```

### 8.4 Output Format

```
$ scion sync from my-agent
Using Hub: https://hub.example.com
Requesting workspace sync from agent 'my-agent'...
Uploading workspace to storage... done
Downloading 15 files (102.4 KB)...
  src/main.go (2.0 KB) ✓
  src/lib.go (1.5 KB) ✓
  ...
Sync complete: 15 files, 102.4 KB transferred

$ scion sync to my-agent --dry-run
Using Hub: https://hub.example.com
Scanning local workspace...
Would upload 3 changed files (5.2 KB):
  src/main.go (modified)
  src/new_file.go (new)
  tests/test_main.go (modified)
Would skip 12 unchanged files
```

---

## 9. Runtime Broker Integration

### 9.1 Workspace Upload Handler

The Runtime Broker implements workspace upload using the existing rclone integration:

```go
// pkg/runtimebroker/workspace_handlers.go

func (s *Server) handleWorkspaceUpload(w http.ResponseWriter, r *http.Request) {
    var req WorkspaceUploadRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    // Get container workspace path
    workspacePath, err := s.getAgentWorkspacePath(req.AgentID)
    if err != nil {
        http.Error(w, err.Error(), http.StatusNotFound)
        return
    }

    // Build manifest from container workspace
    manifest, err := buildWorkspaceManifest(workspacePath, req.ExcludePatterns)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    // Sync to GCS using rclone (reuse existing pkg/gcp/storage.go)
    bucket := s.config.StorageBucket
    if err := gcp.SyncToGCS(r.Context(), workspacePath, bucket, req.StoragePath+"/files"); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    // Upload manifest
    if err := uploadManifest(r.Context(), bucket, req.StoragePath, manifest); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    json.NewEncoder(w).Encode(WorkspaceUploadResponse{
        Manifest: manifest,
    })
}
```

### 9.2 Workspace Apply Handler

```go
func (s *Server) handleWorkspaceApply(w http.ResponseWriter, r *http.Request) {
    var req WorkspaceApplyRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    // Get container workspace path
    workspacePath, err := s.getAgentWorkspacePath(req.AgentID)
    if err != nil {
        http.Error(w, err.Error(), http.StatusNotFound)
        return
    }

    // Sync from GCS to container workspace
    bucket := s.config.StorageBucket
    if err := gcp.SyncFromGCS(r.Context(), bucket, req.StoragePath+"/files", workspacePath); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    // Optionally: fix file permissions based on manifest modes
    if err := applyFilePermissions(workspacePath, req.Manifest.Files); err != nil {
        // Log but don't fail
    }

    json.NewEncoder(w).Encode(WorkspaceApplyResponse{
        Applied: true,
    })
}
```

### 9.3 Container Workspace Access

The Runtime Broker must access the agent container's workspace directory. For Docker:

```go
func (s *Server) getAgentWorkspacePath(agentID string) (string, error) {
    // Option 1: Volume mount inspection
    container, err := s.docker.ContainerInspect(ctx, agentID)
    if err != nil {
        return "", err
    }
    for _, mount := range container.Mounts {
        if mount.Destination == "/workspace" {
            return mount.Source, nil
        }
    }

    // Option 2: Known worktree location
    return filepath.Join(s.config.WorktreeBase, agentID), nil
}
```

---

## 10. Code Reuse and Factoring

### 10.1 Existing Components to Reuse

| Component | Location | Reuse For |
|-----------|----------|-----------|
| `CollectFiles()` | `pkg/hubclient/manifest.go` | Scan workspace directory |
| `ManifestBuilder` | `pkg/hubclient/manifest.go` | Build workspace manifest |
| `ComputeContentHash()` | `pkg/hubclient/manifest.go` | Hash workspace content |
| `UploadFile()` | `pkg/hubclient/templates.go` | Upload to signed URLs |
| `DownloadFile()` | `pkg/hubclient/templates.go` | Download from signed URLs |
| `GenerateSignedURL()` | `pkg/storage/storage.go` | Create workspace URLs |
| `SyncToGCS()` | `pkg/gcp/storage.go` | Runtime Broker upload |
| `SyncFromGCS()` | `pkg/gcp/storage.go` | Runtime Broker download |

### 10.2 Shared Interface Extraction

**Decision:** Factor common file transfer elements into a new `pkg/transfer` package.

This consolidates duplicate code between templates and workspaces, providing a unified foundation for any future file transfer needs (logs, artifacts, etc.).

Extract common file transfer types to the shared package:

```go
// pkg/transfer/types.go
package transfer

// FileInfo describes a file for transfer.
type FileInfo struct {
    Path     string `json:"path"`
    FullPath string `json:"-"` // Local only
    Size     int64  `json:"size"`
    Hash     string `json:"hash"`
    Mode     string `json:"mode,omitempty"`
}

// Manifest describes a collection of files.
type Manifest struct {
    Version     string     `json:"version"`
    ContentHash string     `json:"contentHash,omitempty"`
    Files       []FileInfo `json:"files"`
}

// UploadURLInfo contains a signed URL for upload.
type UploadURLInfo struct {
    Path    string            `json:"path"`
    URL     string            `json:"url"`
    Method  string            `json:"method"`
    Headers map[string]string `json:"headers,omitempty"`
}

// DownloadURLInfo contains a signed URL for download.
type DownloadURLInfo struct {
    Path string `json:"path"`
    URL  string `json:"url"`
    Size int64  `json:"size"`
    Hash string `json:"hash,omitempty"`
}
```

### 10.3 Unified Transfer Client

```go
// pkg/transfer/client.go
package transfer

// Client handles file transfers using signed URLs.
type Client struct {
    httpClient *http.Client
}

// UploadFiles uploads files to their respective signed URLs.
func (c *Client) UploadFiles(ctx context.Context, files []FileInfo, urls []UploadURLInfo) error

// DownloadFiles downloads files from signed URLs to a destination directory.
func (c *Client) DownloadFiles(ctx context.Context, urls []DownloadURLInfo, destDir string) error

// CollectFiles scans a directory and returns file info.
func CollectFiles(basePath string, excludePatterns []string) ([]FileInfo, error)

// BuildManifest creates a manifest from collected files.
func BuildManifest(files []FileInfo) *Manifest
```

### 10.4 Hub Client Extension

Add workspace methods to the existing hubclient:

```go
// pkg/hubclient/workspace.go

type WorkspaceService interface {
    // SyncFrom initiates download of workspace from agent.
    SyncFrom(ctx context.Context, agentID string, opts *SyncOptions) (*SyncFromResponse, error)

    // SyncTo initiates upload of workspace to agent.
    SyncTo(ctx context.Context, agentID string, files []transfer.FileInfo) (*SyncToResponse, error)

    // FinalizeSyncTo completes the sync-to operation.
    FinalizeSyncTo(ctx context.Context, agentID string, manifest *transfer.Manifest) error

    // GetStatus returns current workspace sync status.
    GetStatus(ctx context.Context, agentID string) (*WorkspaceStatus, error)
}
```

### 10.5 Migration Path for Templates

The template code will be updated to use `pkg/transfer` during Phase 0. This is a refactoring with no API changes:

| Current Location | New Location | Notes |
|------------------|--------------|-------|
| `hubclient.FileInfo` | `transfer.FileInfo` | Add `FullPath` field |
| `hubclient.TemplateFile` | `transfer.FileInfo` | Merge with FileInfo |
| `hubclient.TemplateManifest` | `transfer.Manifest` | Generalize for workspaces |
| `hubclient.CollectFiles()` | `transfer.CollectFiles()` | Move function |
| `hubclient.ManifestBuilder` | `transfer.ManifestBuilder` | Move struct |
| `hubclient.ComputeContentHash()` | `transfer.ComputeContentHash()` | Move function |
| `hubclient.UploadFile()` | `transfer.Client.UploadFile()` | Extract to client |
| `hubclient.DownloadFile()` | `transfer.Client.DownloadFile()` | Extract to client |

**Backward Compatibility:**

The `pkg/hubclient/templates.go` will continue to expose existing types and methods, delegating to `pkg/transfer` internally. This ensures:
- No breaking changes to existing template CLI commands
- No changes required in `cmd/templates.go`
- Gradual migration path

```go
// pkg/hubclient/templates.go (after refactoring)

// TemplateFile is an alias for transfer.FileInfo for backward compatibility.
type TemplateFile = transfer.FileInfo

// TemplateManifest wraps transfer.Manifest with template-specific fields.
type TemplateManifest struct {
    transfer.Manifest
    Harness string `json:"harness,omitempty"`
}

// CollectFiles delegates to transfer.CollectFiles.
func CollectFiles(basePath string, ignorePatterns []string) ([]FileInfo, error) {
    return transfer.CollectFiles(basePath, ignorePatterns)
}
```

---

## 11. Incremental Sync

### 11.1 Hash-Based Deduplication

Files are identified by their SHA-256 content hash. This enables:

1. **Skip unchanged files:** Compare local hashes to remote manifest
2. **Resume interrupted syncs:** Only upload/download missing files
3. **Bandwidth optimization:** Large workspaces with few changes transfer quickly

### 11.2 Sync FROM (Incremental Download)

```go
func syncFromIncremental(ctx context.Context, client *hubclient.Client, agentID, destDir string) error {
    // 1. Get remote manifest
    resp, err := client.Workspace().SyncFrom(ctx, agentID, nil)
    if err != nil {
        return err
    }

    // 2. Build local manifest
    localFiles, err := transfer.CollectFiles(destDir, defaultExcludes)
    if err != nil {
        return err
    }
    localHashes := make(map[string]string)
    for _, f := range localFiles {
        localHashes[f.Path] = f.Hash
    }

    // 3. Identify files to download
    var toDownload []DownloadURLInfo
    for _, url := range resp.DownloadURLs {
        if localHash, exists := localHashes[url.Path]; !exists || localHash != url.Hash {
            toDownload = append(toDownload, url)
        }
    }

    // 4. Download only changed files
    return transferClient.DownloadFiles(ctx, toDownload, destDir)
}
```

### 11.3 Sync TO (Incremental Upload)

The Hub returns `existingFiles` in the `sync-to` response - files whose hashes already match storage:

```go
func syncToIncremental(ctx context.Context, client *hubclient.Client, agentID, srcDir string) error {
    // 1. Collect local files
    files, err := transfer.CollectFiles(srcDir, defaultExcludes)
    if err != nil {
        return err
    }

    // 2. Request upload URLs (Hub checks existing hashes)
    resp, err := client.Workspace().SyncTo(ctx, agentID, files)
    if err != nil {
        return err
    }

    // 3. Skip files that already exist with matching hash
    existingSet := make(map[string]bool)
    for _, path := range resp.ExistingFiles {
        existingSet[path] = true
    }

    // 4. Upload only new/changed files
    for _, url := range resp.UploadURLs {
        if existingSet[url.Path] {
            continue
        }
        // Find file and upload
        for _, f := range files {
            if f.Path == url.Path {
                if err := uploadFile(ctx, url, f.FullPath); err != nil {
                    return err
                }
                break
            }
        }
    }

    // 5. Finalize
    manifest := transfer.BuildManifest(files)
    return client.Workspace().FinalizeSyncTo(ctx, agentID, manifest)
}
```

---

## 12. Security Considerations

### 12.1 Signed URL Security

- **Time-limited:** URLs expire after 15 minutes (configurable)
- **Method-restricted:** Upload URLs are PUT-only, download URLs are GET-only
- **Path-scoped:** Each URL is valid only for its specific object
- **No credential exposure:** CLI never sees GCS service account credentials

### 12.2 Authorization

- User must have access to the agent (verified by Hub)
- Runtime Broker must be registered for the grove
- HMAC authentication for Hub ↔ Runtime Broker communication

### 12.3 Content Validation

- File sizes are verified against manifest
- Content hashes are verified on download
- Optional: scan for sensitive files (.env, credentials) with warnings

### 12.4 Exclude Patterns

Default exclude patterns prevent accidental sync of:
- `.git/**` - Git internals
- `node_modules/**` - Package caches
- `*.env` - Environment files
- `.scion/**` - Scion metadata

---

## 13. Workspace Bootstrap at Agent Creation

### 13.1 Problem Statement (The Bootstrap Gap)

Sections 4-9 of this document describe **on-demand** workspace synchronization between a running agent and the CLI. However, there is a critical gap: when a CLI user on Machine A creates an agent that executes on a Runtime Broker on Machine B, there is no mechanism to provision the agent's **initial** workspace with the correct content.

#### 13.1.1 Current Behavior

Today, the agent creation flow through Hub → Broker works as follows:

1. **CLI** sends `CreateAgentRequest` to Hub with `Name`, `GroveID`, `Template`, `Branch`. The `--workspace` flag is parsed but never sent in the Hub path — it is only used in local mode.
2. **Hub** looks up the `GroveProvider.LocalPath` for the target broker — a filesystem path registered when the broker was linked to the grove. Hub sends this as `GrovePath` in the dispatch to the broker.
3. **Runtime Broker** receives `GrovePath` and passes it to `manager.Start()`, which calls `ProvisionAgent()`. This function resolves a `projectDir` from `GrovePath`, creates a git worktree from whatever repository exists **on the broker's local filesystem** at that path, and bind-mounts the resulting directory into the container.

#### 13.1.2 Consequences

- The agent's workspace reflects the **broker's local git state** (different commits, different branch, different working tree), not the CLI user's.
- The `Branch` field from the create request is not propagated through to the broker's provisioning logic.
- If the broker does not have the repository cloned at the registered `LocalPath`, provisioning fails entirely.
- There is no path for the CLI user's workspace content to reach the remote agent before it starts.

#### 13.1.3 Relationship to On-Demand Sync

The existing on-demand sync (`scion sync to <agent>`) cannot fill this gap because:

- `handleWorkspaceSyncToFinalize()` requires `agent.Status == "running"` — but the agent needs workspace content to start meaningfully.
- Even if the status check were relaxed, the broker's provisioning has already created the worktree from its local state before the agent starts, so the sync would be a corrective overwrite rather than a clean bootstrap.

This creates a chicken-and-egg problem: the agent needs workspace content to do useful work, but the sync mechanism requires the agent to already be running.

### 13.2 Design Principle: Git vs Non-Git

Workspace bootstrap strategy depends on whether the grove is backed by a git repository. The two cases have fundamentally different characteristics and should use different transfer mechanisms.

| Scenario | Strategy | Rationale |
|----------|----------|-----------|
| **Git-backed grove** | Git clone/fetch on broker from remote | Git is purpose-built for distributing repository state efficiently. Avoids uploading potentially large repos through GCS. The remote is the single source of truth. |
| **Non-git workspace** | GCS signed URL sync | No git remote to pull from. Reuse the existing transfer infrastructure from on-demand sync (Sections 4-9). |

> **Implementation note:** The non-git GCS bootstrap (Section 13.4) is targeted for near-term implementation. The git-backed bootstrap (Section 13.3) is deferred until a comprehensive remote git workflow design is produced, as git credential management, remote access patterns, and branch lifecycle across distributed brokers require a broader design scope. The git-specific notes below are retained to inform that future design.

### 13.3 Git-Backed Workspace Bootstrap

> **Status:** Deferred — pending a dedicated remote git workflow design document. The design below captures the intended approach and will be refined when that work is undertaken.

When the grove is a git repository, the broker should obtain workspace content by cloning or fetching from the git remote — not by relying on a pre-existing local checkout.

#### 13.3.1 CLI-Side Validation

Before dispatching agent creation, the CLI must validate that the current workspace state is **reproducible from the git remote**. If it is not, the remote agent cannot be given equivalent content, and creation must fail or warn.

**Check 1: All commits must be pushed.**

The CLI compares the local branch HEAD with the remote tracking branch. If there are unpushed commits, creation fails:

```
Error: Cannot create remote agent: branch 'feature-xyz' has 3 unpushed
commits that would not be available to the remote agent.

Push your changes first:
  git push origin feature-xyz
```

**Check 2: No uncommitted changes (warning).**

If the working tree has uncommitted modifications, the CLI warns but proceeds. The remote agent will get the last committed state:

```
Warning: Working tree has uncommitted changes that will not be present
on the remote agent's workspace. Commit and push, or use
'scion sync to <agent>' after creation to transfer local state.
```

**Check 3: Remote URL is resolvable.**

The CLI reads the remote URL from `git remote get-url origin` (or the configured remote) and includes it in the create request. The broker must be able to reach this URL. The CLI does not validate reachability on the broker's behalf — that surfaces as a broker-side error during provisioning.

#### 13.3.2 Data Flow

```
CLI                          Hub                    Runtime Broker
 │                            │                           │
 │ 1. Validate local git:     │                           │
 │    - HEAD pushed?          │                           │
 │    - dirty working tree?   │                           │
 │                            │                           │
 │ 2. CreateAgentRequest      │                           │
 │    + gitRemoteURL          │                           │
 │    + gitRef (branch/SHA)   │                           │
 ├───────────────────────────>│                           │
 │                            │                           │
 │                            │ 3. Dispatch to broker     │
 │                            │    + gitRemoteURL         │
 │                            │    + gitRef               │
 │                            ├──────────────────────────>│
 │                            │                           │
 │                            │    4. Clone or fetch repo │
 │                            │       into managed path   │
 │                            │       Create worktree at  │
 │                            │       gitRef              │
 │                            │                           │
 │                            │ 5. Container started      │
 │                            │<──────────────────────────┤
 │                            │                           │
 │ 6. Response: created       │                           │
 │<───────────────────────────│                           │
```

#### 13.3.3 Request Changes

The `CreateAgentRequest` (CLI → Hub) and `RemoteCreateAgentRequest` (Hub → Broker) gain workspace bootstrap fields:

```go
// In hubclient (CLI → Hub)
type CreateAgentRequest struct {
    // ... existing fields ...

    // Workspace bootstrap (git mode)
    GitRemoteURL string `json:"gitRemoteUrl,omitempty"` // e.g. "git@github.com:org/repo.git"
    GitRef       string `json:"gitRef,omitempty"`       // Branch name or commit SHA
}

// In hub (Hub → Broker dispatch)
type RemoteCreateAgentRequest struct {
    // ... existing fields ...

    GitRemoteURL string `json:"gitRemoteUrl,omitempty"`
    GitRef       string `json:"gitRef,omitempty"`
}
```

The Hub passes these fields through from the CLI request to the broker dispatch. It also stores them on the agent record for observability (e.g., `scion status` can show what ref an agent was created from).

#### 13.3.4 Broker Provisioning Changes

When the broker receives a create request with `GitRemoteURL` set, workspace provisioning changes from the current `GrovePath`-based logic to a git-remote-based flow:

1. **Check for existing clone.** The broker maintains a managed directory for repository clones (e.g., `<broker-data>/repos/<grove-id>/`). If a clone of the same remote URL already exists, run `git fetch origin` to update it.

2. **Fresh clone if needed.** If no existing clone, run `git clone --bare <gitRemoteURL>` (or `--mirror`) into the managed directory. A bare clone is sufficient since the broker only needs to create worktrees from it.

3. **Create worktree at ref.** Create a git worktree at the specified `GitRef` under the agent's workspace directory, the same as local provisioning does today but from the managed clone rather than from `GrovePath`.

4. **Fallback.** If `GitRemoteURL` is not set, fall back to current behavior (use `GrovePath` from `GroveProvider.LocalPath`). This preserves backward compatibility for brokers co-located with the repo.

```go
// Pseudocode for broker workspace resolution
func resolveWorkspace(req CreateAgentRequest) (string, error) {
    if req.GitRemoteURL != "" {
        repoDir := filepath.Join(brokerDataDir, "repos", req.GroveID)
        if exists(repoDir) {
            gitFetch(repoDir)
        } else {
            gitCloneBare(req.GitRemoteURL, repoDir)
        }
        ref := req.GitRef
        if ref == "" {
            ref = slugify(req.Name)
        }
        worktreePath := filepath.Join(agentsDir, req.Name, "workspace")
        createWorktree(repoDir, worktreePath, ref)
        return worktreePath, nil
    }

    // Fallback: existing GrovePath behavior
    return provisionFromGrovePath(req.GrovePath, req.Name)
}
```

#### 13.3.5 Git Credential Access on Broker

> **Note:** This section is provisional and subject to revision as part of the future remote git workflow design.

The broker needs git credentials to clone private repositories. This is a broker-level configuration concern, not a per-request concern:

- **SSH:** The broker's service account or user has SSH keys with access to the relevant repositories. Standard `~/.ssh/config` or deploy keys.
- **HTTPS with credential helper:** The broker is configured with a git credential helper (e.g., `gcloud` credential helper for Cloud Source Repositories, GitHub App token helper).
- **Shared credentials via Grove registration:** When a broker is registered as a grove provider, the administrator ensures the broker has appropriate access to the grove's repository.

This is explicitly **not** a credential-forwarding mechanism from the CLI. The broker is a trusted compute node that has been granted access to the repositories it serves.

#### 13.3.6 Branch Handling

The `GitRef` field supports multiple forms:

| Value | Behavior |
|-------|----------|
| Branch name (e.g., `main`, `feature-xyz`) | Create worktree tracking that branch |
| Commit SHA | Create worktree in detached HEAD at that commit |
| Empty / omitted | Create a new branch named after the agent slug (current behavior) |

When the CLI sends a branch name, it should send the same value from `git rev-parse --abbrev-ref HEAD` (the current branch) so the agent works on the same branch the user is on.

### 13.4 Non-Git Workspace Bootstrap

For workspaces that are not backed by a git repository, the existing GCS signed URL mechanism (Sections 4-9) is used, but integrated into the creation flow as a pre-start step rather than a separate post-creation command.

#### 13.4.1 Data Flow

```
CLI                          Hub                    GCS              Runtime Broker
 │                            │                      │                     │
 │ 1. Collect workspace files │                      │                     │
 │    (transfer.CollectFiles) │                      │                     │
 │                            │                      │                     │
 │ 2. CreateAgent request     │                      │                     │
 │    + file manifest         │                      │                     │
 ├───────────────────────────>│                      │                     │
 │                            │                      │                     │
 │ 3. Hub creates agent       │                      │                     │
 │    (status: "provisioning")│                      │                     │
 │    Returns upload URLs     │                      │                     │
 │<───────────────────────────│                      │                     │
 │                            │                      │                     │
 │ 4. Upload files via        │                      │                     │
 │    signed URLs             │                      │                     │
 ├──────────────────────────────────────────────────>│                     │
 │                            │                      │                     │
 │ 5. POST finalize-and-start │                      │                     │
 ├───────────────────────────>│                      │                     │
 │                            │                      │                     │
 │                            │ 6. Dispatch to broker│                     │
 │                            │    + storagePath     │                     │
 │                            ├──────────────────────────────────────────>│
 │                            │                      │                     │
 │                            │                      │  7. Download files  │
 │                            │                      │<────────────────────┤
 │                            │                      │                     │
 │                            │                      │  8. Populate        │
 │                            │                      │     workspace dir,  │
 │                            │                      │     start container │
 │                            │                      │                     │
 │                            │ 9. Agent running     │                     │
 │                            │<─────────────────────────────────────────│
 │                            │                      │                     │
 │ 10. Response: running      │                      │                     │
 │<───────────────────────────│                      │                     │
```

#### 13.4.2 Key Differences from On-Demand Sync

| Aspect | On-demand sync-to (existing) | Bootstrap sync-to (new) |
|--------|------------------------------|-------------------------|
| Agent status required | `running` | `provisioning` (new status) |
| Triggered by | Explicit `scion sync to` | Automatic during `scion start` / `scion create` |
| Workspace pre-exists | Yes (already mounted in container) | No (broker creates empty dir, populates from GCS) |
| Finalize action | Tunnel apply to running container | Trigger broker to populate dir + start container |

#### 13.4.3 Changes Required

- **New agent status `provisioning`:** Agents in this status have been created in the Hub database but have not yet been dispatched to a broker. This allows the CLI to upload workspace content before the agent starts.
- **Hub `handleWorkspaceSyncToFinalize` relaxation:** Accept agents in `provisioning` status (not just `running`) so the CLI can finalize the upload and trigger the start.
- **`RemoteCreateAgentRequest` gains `WorkspaceStoragePath`:** Signals to the broker that it should download workspace content from GCS rather than using its local filesystem.
- **Broker provisioning:** When `WorkspaceStoragePath` is set, the broker creates an empty workspace directory, downloads files from GCS into it, then starts the container with that directory mounted.

### 13.5 CLI Decision Flow

The CLI determines which bootstrap strategy to use automatically based on the grove type:

```
scion start my-agent "Fix the bug"
    │
    ├── Is Hub mode?
    │       │
    │      No ──> Local mode (unchanged, current behavior)
    │       │
    │      Yes
    │       │
    │       ├── Is grove a git repo?
    │       │       │
    │       │      Yes ── GIT BOOTSTRAP PATH ──┐
    │       │       │                           │
    │       │       ├── git remote get-url      │
    │       │       │   origin → remoteURL      │
    │       │       │                           │
    │       │       ├── HEAD pushed to remote?  │
    │       │       │       │                   │
    │       │       │      No ──> ERROR:        │
    │       │       │       │   "Push changes   │
    │       │       │       │    first"          │
    │       │       │       │                   │
    │       │       │      Yes                  │
    │       │       │       │                   │
    │       │       ├── Uncommitted changes?    │
    │       │       │       │                   │
    │       │       │      Yes ──> WARNING       │
    │       │       │       │   (proceed anyway) │
    │       │       │       │                   │
    │       │       │      No                   │
    │       │       │       │                   │
    │       │       └── Send CreateAgent with   │
    │       │           gitRemoteURL + gitRef   │
    │       │                                   │
    │       │                                   │
    │       └──No ── GCS BOOTSTRAP PATH ──┐     │
    │               │                     │     │
    │               ├── Collect files      │     │
    │               │   (transfer.Collect) │     │
    │               │                     │     │
    │               ├── Send CreateAgent   │     │
    │               │   with manifest      │     │
    │               │                     │     │
    │               ├── Upload to GCS via  │     │
    │               │   signed URLs        │     │
    │               │                     │     │
    │               └── Finalize + start   │     │
    │                                     │     │
    └─────────────────────────────────────┘─────┘
```

### 13.6 Impact on Existing Components

| Component | Change |
|-----------|--------|
| `cmd/create.go` / `cmd/common.go` | Add git validation logic; detect git vs non-git; populate new request fields |
| `pkg/hubclient/types.go` | Add `GitRemoteURL`, `GitRef` to `CreateAgentRequest` |
| `pkg/hub/handlers.go` | Pass new fields through to agent record and dispatch |
| `pkg/hub/httpdispatcher.go` | Include `GitRemoteURL`, `GitRef`, `WorkspaceStoragePath` in `RemoteCreateAgentRequest` |
| `pkg/hub/workspace_handlers.go` | Relax status check in `handleWorkspaceSyncToFinalize` to accept `provisioning` |
| `pkg/runtimebroker/handlers.go` | Branch on `GitRemoteURL` vs `WorkspaceStoragePath` vs `GrovePath` for workspace resolution |
| `pkg/agent/provision.go` | Factor out workspace resolution so broker can call git-remote path |
| `pkg/store/models.go` | Store `GitRemoteURL`, `GitRef` on agent record |

### 13.7 Resolved Questions

| # | Question | Decision | Rationale |
|---|----------|----------|-----------|
| Q1 | **Should the CLI block until the agent is fully provisioned?** | **Yes, poll until running.** | The CLI should poll agent status until the agent transitions to `running`. The broker should update agent status with provisioning detail (e.g., `provisioning:cloning`, `provisioning:syncing`) so the CLI can display meaningful progress. For GCS bootstrap, the CLI already blocks during upload; for git bootstrap (deferred), polling handles variable clone times. |
| Q2 | **Should the broker cache bare clones across agents in the same grove?** | **Deferred** | Deferred to the future remote git workflow design. The question of shared clones, locking, and lifecycle management is closely tied to how brokers manage git state at scale. Carried forward as an open question for that design. |
| Q3 | **How should `scion create` (without start) interact with bootstrap?** | **Option (a): `create` does not bootstrap.** | Workspace provisioning happens at `start` time, not `create` time. This matches the current `create` vs `start` distinction and supports late binding of configuration. See also: config and late-binding patterns (to be documented separately). |
| Q4 | **Should submodules be supported in the git bootstrap path?** | **No, defer.** | Submodules are not supported in the initial implementation. This can be revisited if a concrete use case requires it. |

---

## 14. Design Decisions

### 14.1 Resolved Questions

All open questions have been resolved with the following decisions:

| # | Question | Decision | Rationale |
|---|----------|----------|-----------|
| Q1 | **Automatic upload on agent stop?** | **Explicit only** | Keeps behavior predictable, matches milestone requirement. Users can always run `scion sync from` before stopping. |
| Q2 | **Conflict handling for sync-to?** | **Last-write-wins** | Simple for MVP. Agent can always re-generate changes. More sophisticated conflict handling deferred. |
| Q3 | **Signed URL expiry duration?** | **15 minutes** | Matches template system. Sufficient for large transfers. Consistent across codebase. |
| Q4 | **Large file handling?** | **No limit** | GCS handles large files natively. rclone supports resumable uploads. No artificial constraints needed. |
| Q5 | **Shared package factoring?** | **Yes - `pkg/transfer`** | Consolidates duplicate code between templates and workspaces. ~60% code reuse. Foundation for future transfer needs. |

### 14.2 Deferred Decisions

| Question | Deferral Reason |
|----------|-----------------|
| Background periodic sync | Not needed for milestone; adds complexity |
| Mutagen integration for hosted mode | rclone is sufficient; mutagen adds operational overhead |
| Conflict resolution UI | Last-write-wins is acceptable for CLI-driven workflow |
| Sync history/versioning | GCS versioning available; not needed for milestone |

---

## 15. Implementation Plan

### Phase 0: Shared Transfer Package ✅

**Goal:** Extract common file transfer code into `pkg/transfer` for reuse across templates and workspaces.

- [x] Create `pkg/transfer/types.go`:
  - [x] `FileInfo` struct (consolidate from `hubclient.FileInfo` and `hubclient.TemplateFile`)
  - [x] `Manifest` struct (consolidate from `hubclient.TemplateManifest`)
  - [x] `UploadURLInfo` struct
  - [x] `DownloadURLInfo` struct
- [x] Create `pkg/transfer/collect.go`:
  - [x] Move `CollectFiles()` from `pkg/hubclient/manifest.go`
  - [x] Move `ManifestBuilder` from `pkg/hubclient/manifest.go`
  - [x] Move `ComputeContentHash()` from `pkg/hubclient/manifest.go`
  - [x] Add configurable exclude patterns
- [x] Create `pkg/transfer/client.go`:
  - [x] `UploadFiles()` - upload to signed URLs (extract from `hubclient.UploadFile`)
  - [x] `DownloadFiles()` - download from signed URLs (extract from `hubclient.DownloadFile`)
  - [x] Progress reporting callback
- [x] Update `pkg/hubclient/templates.go`:
  - [x] Import and use `pkg/transfer` types
  - [x] Delegate to `transfer.Client` for file operations
  - [x] Maintain backward-compatible API
- [x] Update `pkg/hub/template_handlers.go`:
  - [x] Uses `store.TemplateFile` (structurally equivalent to `transfer.FileInfo`)
  - [x] Keep existing handler logic
- [x] Add unit tests for `pkg/transfer`

### Phase 1: Storage Path & Hub Endpoints (Day 2) ✅

**Goal:** Add workspace storage conventions and Hub API endpoints.

- [x] Add to `pkg/storage/storage.go`:
  - [x] `WorkspaceStoragePath(groveID, agentID string) string`
  - [x] `WorkspaceStorageURI(bucket, groveID, agentID string) string`
- [x] Create `pkg/hub/workspace_handlers.go`:
  - [x] `handleWorkspaceSyncFrom()` - `POST /api/v1/agents/{id}/workspace/sync-from`
  - [x] `handleWorkspaceSyncTo()` - `POST /api/v1/agents/{id}/workspace/sync-to`
  - [x] `handleWorkspaceSyncToFinalize()` - `POST /api/v1/agents/{id}/workspace/sync-to/finalize`
  - [x] `handleWorkspaceStatus()` - `GET /api/v1/agents/{id}/workspace`
- [x] Add workspace routes to Hub router in `pkg/hub/server.go`
- [x] Add request/response types using `transfer.FileInfo`

### Phase 2: Runtime Broker Handlers (Day 3) ✅

**Goal:** Implement workspace upload/apply on Runtime Broker.

- [x] Create `pkg/runtimebroker/workspace_handlers.go`:
  - [x] `handleWorkspaceUpload()` - `POST /api/v1/workspace/upload`
  - [x] `handleWorkspaceApply()` - `POST /api/v1/workspace/apply`
- [x] Add `getAgentWorkspacePath()` for container workspace resolution
- [x] Integrate with existing `pkg/gcp/storage.go` (SyncToGCS/SyncFromGCS)
- [x] Add workspace routes to Runtime Broker router
- [x] Use `transfer.CollectFiles()` for manifest building
- [x] Add `GetWorkspacePath()` method to Runtime interface (Docker, K8s, Apple Container)
- [x] Add `StorageBucket` and `WorktreeBase` config options to ServerConfig
- [x] Add unit tests for workspace handlers

### Phase 3: Hub Client & CLI (Day 4) ✅

**Goal:** Add workspace client and update CLI sync command.

- [x] Create `pkg/hubclient/workspace.go`:
  - [x] `WorkspaceService` interface
  - [x] `SyncFrom()` method
  - [x] `SyncTo()` method
  - [x] `FinalizeSyncTo()` method
  - [x] `GetStatus()` method
- [x] Update `cmd/sync.go`:
  - [x] Add hosted mode detection via `CheckHubAvailability()`
  - [x] Implement `syncViaHub()` for hosted mode
  - [x] Implement `syncFromViaHub()` with incremental download
  - [x] Implement `syncToViaHub()` with incremental upload
  - [x] Add `--dry-run` flag support
  - [x] Add progress output
- [x] Use `transfer.Client` for file operations

### Phase 4: Testing & Polish (Day 5) ✅

**Goal:** Validate end-to-end and update documentation.

- [x] Integration tests:
  - [x] `pkg/transfer` unit tests
  - [x] Hub workspace endpoint tests (mock storage)
  - [x] Runtime Broker workspace handler tests
  - [x] CLI sync command tests (mock hubclient)
- [x] End-to-end test with real Hub/Runtime Broker
- [x] CLI output formatting and progress display
- [x] Error handling and edge cases:
  - [x] Agent not running
  - [x] Storage unavailable
  - [x] Partial upload/download recovery
- [x] Update `hosted-e2e walkthrough`:
  - [x] Mark Scenario 4 complete
  - [x] Update success criteria (7/7)
- [x] Update test setup commands in walkthrough

### Phase 5: Non-Git Workspace Bootstrap (Section 13.4)

**Goal:** Implement GCS-based workspace bootstrap for non-git workspaces at agent creation time.

- [ ] Add `provisioning` agent status to `pkg/store/models.go` and Hub status transitions
- [ ] Update `pkg/hubclient/types.go`:
  - [ ] Add `WorkspaceFiles` (file manifest) to `CreateAgentRequest`
- [ ] Update `pkg/hub/handlers.go`:
  - [ ] Accept optional file manifest in create request
  - [ ] When manifest present, create agent in `provisioning` status and return signed upload URLs
- [ ] Update `pkg/hub/workspace_handlers.go`:
  - [ ] Relax `handleWorkspaceSyncToFinalize` to accept `provisioning` status
  - [ ] On finalize for `provisioning` agent, trigger dispatch to broker with `WorkspaceStoragePath`
- [ ] Update `pkg/hub/httpdispatcher.go`:
  - [ ] Add `WorkspaceStoragePath` field to `RemoteCreateAgentRequest`
  - [ ] Pass through on dispatch
- [ ] Update `pkg/runtimebroker/handlers.go`:
  - [ ] When `WorkspaceStoragePath` is set, download workspace from GCS before starting container
  - [ ] Create empty workspace directory, populate from GCS, then start
- [ ] Update `cmd/create.go` / `cmd/common.go`:
  - [ ] Detect non-git grove in Hub mode
  - [ ] Collect workspace files, include manifest in create request
  - [ ] Upload files to GCS via signed URLs
  - [ ] Call finalize-and-start
  - [ ] Poll agent status until `running`
- [ ] Add CLI progress output for workspace upload during creation
- [ ] Add tests for bootstrap flow (Hub handler, broker handler, CLI)

### Phase 6: Git Workspace Bootstrap (Section 13.3) — Deferred

**Goal:** Implement git-based workspace bootstrap for git-backed groves. Deferred pending a dedicated remote git workflow design.

- [ ] Design: Produce remote git workflow design document covering credential management, remote access patterns, and branch lifecycle
- [ ] CLI git validation (unpushed commits check, dirty tree warning)
- [ ] Add `GitRemoteURL`, `GitRef` fields to `CreateAgentRequest` and `RemoteCreateAgentRequest`
- [ ] Hub passthrough of git fields to broker dispatch and agent record
- [ ] Broker git clone/fetch workspace provisioning (bare clone cache, worktree creation)
- [ ] Broker git credential configuration
- [ ] Integration tests for git bootstrap path

---

## 16. References

### Design Documents

| Document | Relevance |
|----------|-----------|
| [hosted-e2e.md](../walkthroughs/hosted-e2e.md) | Milestone requirements (Scenario 4) |
| [hosted-templates.md](hosted-templates.md) | Template upload/download pattern (Sections 2, 5) |
| [hosted-architecture.md](hosted-architecture.md) | Overall architecture context |
| [runtimebroker-websocket.md](runtimebroker-websocket.md) | Control channel for NAT traversal (Section 3) |
| [hub-api.md](hub-api.md) | Hub API conventions |
| [runtime-broker-api.md](runtime-broker-api.md) | Runtime Broker API conventions |

### Source Files

| File | Relevance |
|------|-----------|
| `pkg/transfer/` | **New** - Shared file transfer package (Phase 0) |
| `pkg/storage/storage.go` | Storage interface, add `WorkspaceStoragePath()` |
| `pkg/hubclient/manifest.go` | File collection utilities → migrate to `pkg/transfer` |
| `pkg/hubclient/templates.go` | Template client → refactor to use `pkg/transfer` |
| `pkg/hubclient/workspace.go` | **New** - Workspace client (Phase 3) |
| `pkg/hub/workspace_handlers.go` | **New** - Hub workspace endpoints (Phase 1) |
| `pkg/runtimebroker/workspace_handlers.go` | **New** - Runtime Broker handlers (Phase 2) |
| `pkg/gcp/storage.go` | rclone GCS integration (reuse existing) |
| `cmd/sync.go` | Current sync command → extend for hosted mode |

---

## Appendix A: Package Structure After Implementation

```
pkg/
├── transfer/                    # NEW - Shared file transfer
│   ├── types.go                 # FileInfo, Manifest, URL types
│   ├── collect.go               # CollectFiles, ManifestBuilder
│   ├── client.go                # UploadFiles, DownloadFiles
│   └── hash.go                  # ComputeContentHash
│
├── hubclient/
│   ├── templates.go             # REFACTORED - uses transfer
│   ├── manifest.go              # DEPRECATED - delegates to transfer
│   └── workspace.go             # NEW - WorkspaceService
│
├── hub/
│   ├── template_handlers.go     # EXISTING - uses transfer types
│   └── workspace_handlers.go    # NEW - workspace sync endpoints
│
├── runtimebroker/
│   └── workspace_handlers.go    # NEW - upload/apply handlers
│
└── storage/
    └── storage.go               # EXTENDED - WorkspaceStoragePath
```
