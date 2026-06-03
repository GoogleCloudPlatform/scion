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
	"fmt"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// nfsBackend resolves workspace and shared-dir paths onto an NFS-backed
// filesystem. Resolution is deterministic from project/agent IDs — no DB
// lookup, no I/O — so any replica computes the same path.
//
// Layout under the NFS mount (design §3):
//
//	<MountRoot>/<shareID>/<SubPathRoot>/<projectID>/workspace
//	<MountRoot>/<shareID>/<SubPathRoot>/<projectID>/shared-dirs/<name>
//
// Provision and Realize are stubs in N1-1; full implementations land in
// N1-4 (provisioning) and N1-3 (mount wiring).
type nfsBackend struct {
	cfg *config.V1NFSConfig
}

// NewNFSBackend returns a WorkspaceBackend backed by NFS shared storage.
// The cfg must be non-nil and should have defaults applied (ApplyNFSDefaults).
func NewNFSBackend(cfg *config.V1NFSConfig) WorkspaceBackend {
	return &nfsBackend{cfg: cfg}
}

func (b *nfsBackend) Name() string { return "nfs" }

// Resolve computes workspace and shared-dir paths on the NFS filesystem.
// The result includes both the server-relative path (for K8s subPath /
// Cloud Run server path) and the full host path (for Docker bind mounts).
//
// The first configured share is used by default. ProjectID is required.
//
// Paths are deterministic: same (ProjectID, ShareID, SubPathRoot) → same path.
// No I/O, no DB lookup.
func (b *nfsBackend) Resolve(in ResolveInput) (ResolvedWorkspace, error) {
	if in.ProjectID == "" {
		return ResolvedWorkspace{}, fmt.Errorf("nfsBackend.Resolve: ProjectID is required")
	}
	if b.cfg == nil {
		return ResolvedWorkspace{}, fmt.Errorf("nfsBackend.Resolve: NFS config is nil")
	}
	if len(b.cfg.Shares) == 0 {
		return ResolvedWorkspace{}, fmt.Errorf("nfsBackend.Resolve: no NFS shares configured")
	}

	share := b.cfg.Shares[0]
	subPathRoot := b.cfg.SubPathRoot
	if subPathRoot == "" {
		subPathRoot = "projects"
	}

	// Server-relative workspace path: <SubPathRoot>/<projectID>/workspace
	workspaceRelPath := filepath.Join(subPathRoot, in.ProjectID, "workspace")

	// Host base: <MountRoot>/<shareID>
	hostBase := filepath.Join(b.cfg.MountRoot, share.ID)

	// Full host path: <MountRoot>/<shareID>/<SubPathRoot>/<projectID>/workspace
	hostPath := filepath.Join(hostBase, workspaceRelPath)

	res := ResolvedWorkspace{
		HostPath:           hostPath,
		ServerRelativePath: workspaceRelPath,
		HostBase:           hostBase,
		Backend:            "nfs",
		SharedDirs:         make(map[string]ResolvedSharedDir, len(in.SharedDirNames)),
	}

	// Resolve shared dirs on NFS: <SubPathRoot>/<projectID>/shared-dirs/<name>
	for _, name := range in.SharedDirNames {
		sdRelPath := filepath.Join(subPathRoot, in.ProjectID, "shared-dirs", name)
		res.SharedDirs[name] = ResolvedSharedDir{
			HostPath:           filepath.Join(hostBase, sdRelPath),
			ServerRelativePath: sdRelPath,
		}
	}

	return res, nil
}

// Provision is a stub in N1-1. Full NFS provisioning (git clone onto NFS
// under a Postgres advisory lock) lands in N1-4.
func (b *nfsBackend) Provision(in ProvisionInput) error {
	// N1-4 will implement: acquire advisory lock → git clone/worktree → release.
	return nil
}

// Realize is a stub in N1-1. Full NFS mount wiring (Docker bind-mount from
// the NFS host path, K8s PVC+subPath, Cloud Run NFS volume) lands in N1-3.
func (b *nfsBackend) Realize(in RealizeInput) (MountDescriptor, error) {
	target := in.ContainerWorkspace
	if target == "" {
		target = "/workspace"
	}

	// Return a descriptor with enough information for N1-3 to wire up.
	// For now the HostPath is populated so callers can see what would be
	// mounted, but the actual mount wiring is deferred.
	return MountDescriptor{
		Type:     "nfs",
		HostPath: in.Resolved.HostPath,
		Target:   target,
		SubPath:  in.Resolved.ServerRelativePath,
	}, nil
}
