// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import (
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// WorkspaceBackend abstracts workspace storage so callers can resolve and
// realize workspace paths without knowing whether storage is node-local or
// NFS-backed. The two methods map to the design's questions (§3):
//
//   - Resolve: given project/agent/mode, what is the storage location?
//   - Realize: emit the runtime mount descriptor for Docker/K8s/Cloud Run.
//
// Provisioning (clone, worktree, mkdir) is handled by the standalone
// provisionShared function (Tier 1) — it is vendor-agnostic and operates
// solely on ProvisionInput fields, not on any backend configuration.
//
// Implementations: localBackend (today's node-local behavior, default) and
// nfsBackend (shared network storage).
type WorkspaceBackend interface {
	// Resolve computes workspace and shared-dir paths deterministically from
	// project/agent IDs and the sharing mode. No DB lookup, no I/O — any
	// replica calling Resolve with the same input produces identical paths.
	//
	// For nfsBackend, ResolvedWorkspace.ServerRelativePath holds the layout
	// under the NFS export (e.g. "projects/<pid>/workspace") and HostBase
	// holds the host mount prefix (<MountRoot>/<shareID>).
	//
	// For localBackend, ResolvedWorkspace.HostPath holds the absolute local
	// host path (today's behavior) and ServerRelativePath is empty.
	Resolve(in ResolveInput) (ResolvedWorkspace, error)

	// Realize emits the runtime mount descriptor (bind mount source, NFS
	// volume, etc.) that the container runtime should use to expose the
	// workspace. The returned MountDescriptor is expressive enough for
	// Docker bind mounts today and K8s/Cloud Run volumes later.
	//
	// N1-1 scope: localBackend returns today's local bind mount; nfsBackend
	// returns a stub — full wiring lands in N1-3.
	Realize(in RealizeInput) (MountDescriptor, error)

	// Name returns a human-readable identifier for this backend ("local" or "nfs").
	Name() string
}

// ResolveInput contains everything needed to deterministically compute
// workspace and shared-dir paths. All fields are stable IDs — no filesystem
// state is consulted.
type ResolveInput struct {
	// ProjectID is the project's stable UUID.
	ProjectID string

	// AgentID is the agent's stable UUID (used for worktree-per-agent paths).
	AgentID string

	// ProjectSlug is the project's slug (used by localBackend for path resolution).
	ProjectSlug string

	// Mode is the canonical workspace sharing mode that governs layout.
	Mode store.WorkspaceSharingMode

	// SharedDirNames lists declared shared-dir names to resolve paths for.
	SharedDirNames []string

	// ProjectDir is the existing host-side project directory (used by
	// localBackend to delegate to existing path-resolution helpers).
	// Empty for nfsBackend (paths are derived from IDs, not host state).
	ProjectDir string
}

// ResolvedWorkspace holds the deterministic path resolution result.
type ResolvedWorkspace struct {
	// HostPath is the absolute host-side path for the workspace.
	// For localBackend this is the existing project path (e.g.
	// ~/.scion.projects/<slug>/). For nfsBackend this is
	// <MountRoot>/<shareID>/<ServerRelativePath>.
	HostPath string

	// ServerRelativePath is the path relative to the NFS export root.
	// Empty for localBackend. For nfsBackend, e.g. "projects/<pid>/workspace".
	ServerRelativePath string

	// HostBase is the host mount prefix for NFS-backed workspaces
	// (<MountRoot>/<shareID>). Empty for localBackend.
	HostBase string

	// SharedDirs maps shared-dir name → resolved path info.
	SharedDirs map[string]ResolvedSharedDir

	// Backend identifies which backend produced this resolution ("local" or "nfs").
	Backend string
}

// ResolvedSharedDir holds path resolution for a single shared directory.
type ResolvedSharedDir struct {
	// HostPath is the absolute host path for this shared dir.
	HostPath string

	// ServerRelativePath is the NFS export-relative path (empty for local).
	ServerRelativePath string
}

// ProvisionInput holds parameters for workspace provisioning.
type ProvisionInput struct {
	// Resolved is the output of a prior Resolve call.
	Resolved ResolvedWorkspace

	// ProjectID is the project's stable UUID.
	ProjectID string

	// AgentID is the agent's stable UUID.
	AgentID string

	// AgentName is a human-readable agent name (used for worktree branch names).
	AgentName string

	// Mode is the workspace sharing mode.
	Mode store.WorkspaceSharingMode

	// GitClone holds git-clone config when the project is git-backed; nil otherwise.
	GitClone *api.GitCloneConfig

	// Locker provides the per-project advisory lock for the NFS first-access
	// provisioning guard (design §7, risk RN1). On Postgres-backed deployments
	// this uses pg_try_advisory_lock(classid, objid) for cross-node mutual
	// exclusion; on SQLite it's a no-op (single-writer serializes already).
	//
	// May be nil — nfsBackend.Provision degrades to sentinel-only guarding
	// (correct for single-node but NOT safe for multi-node).
	Locker store.AdvisoryLocker

	// NFSUID and NFSGID are the stable NFS ownership values (default 1000:1000).
	// Used for one-time chown of newly provisioned workspace directories.
	NFSUID int
	NFSGID int
}

// RealizeInput holds parameters for emitting a runtime mount descriptor.
type RealizeInput struct {
	// Resolved is the output of a prior Resolve call.
	Resolved ResolvedWorkspace

	// ContainerWorkspace is the container-side mount target (e.g. "/workspace").
	ContainerWorkspace string
}

// MountDescriptor describes how the container runtime should mount the
// workspace. It is intentionally expressive enough to cover Docker bind
// mounts (HostPath → Target), K8s PVC+subPath, and Cloud Run NFS volumes.
type MountDescriptor struct {
	// Type is "local" for a host bind mount or "nfs" for a direct NFS mount.
	Type string

	// HostPath is the source for a Docker bind mount (populated for local).
	HostPath string

	// Target is the container-side mount path (e.g. "/workspace").
	Target string

	// NFSServer is the NFS server address (populated for nfs type).
	NFSServer string

	// NFSExportPath is the server-side export path (populated for nfs type).
	NFSExportPath string

	// SubPath is the sub-path within the volume (K8s PVC subPath).
	SubPath string

	// PVClaimName is the K8s PVC name for NFS-backed mounts.
	PVClaimName string
}

// SelectWorkspaceBackend returns the appropriate WorkspaceBackend based on
// configuration and workspace sharing mode. The selection rules (design §3.1):
//
//   - nfsBackend when cfg.Backend == "nfs" AND mode is SharedPlain or WorktreePerAgent.
//   - localBackend otherwise — including ClonePerAgent even when Backend=nfs
//     (the deliberate node-local escape hatch).
//   - Backend empty or "local" always yields localBackend.
func SelectWorkspaceBackend(cfg *config.V1WorkspaceStorageConfig, mode store.WorkspaceSharingMode) WorkspaceBackend {
	if cfg != nil && cfg.Backend == "nfs" {
		switch mode {
		case store.SharingModeSharedPlain, store.SharingModeWorktreePerAgent:
			return NewNFSBackend(cfg.NFS)
		}
		// ClonePerAgent (and any unknown mode) → local, even when Backend=nfs.
	}
	// Backend empty, "local", or nil config → local.
	return NewLocalBackend()
}
