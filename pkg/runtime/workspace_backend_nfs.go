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

// Realize emits a Docker bind-mount descriptor from the NFS host path to the
// container workspace. The host path points at the project subtree under the
// NFS mount (<MountRoot>/<shareID>/<SubPathRoot>/<projectID>/workspace), NOT
// the export root — this is the critical isolation guarantee (design §9.4).
//
// For K8s the SubPath and PVClaimName fields are populated for PVC+subPath
// wiring; for Docker, HostPath is the bind-mount source.
func (b *nfsBackend) Realize(in RealizeInput) (MountDescriptor, error) {
	target := in.ContainerWorkspace
	if target == "" {
		target = "/workspace"
	}

	// Isolation guard: never bind the host base (export root mount) directly.
	// The resolved HostPath must be a subdirectory of HostBase, not equal to it.
	if err := ValidateNotExportRoot(in.Resolved.HostPath, in.Resolved.HostBase); err != nil {
		return MountDescriptor{}, err
	}

	desc := MountDescriptor{
		Type:     "nfs",
		HostPath: in.Resolved.HostPath,
		Target:   target,
		SubPath:  in.Resolved.ServerRelativePath,
	}

	// Populate K8s PVC info from the first share if available.
	if b.cfg != nil && len(b.cfg.Shares) > 0 && b.cfg.Shares[0].PVName != "" {
		desc.PVClaimName = b.cfg.Shares[0].PVName
	}

	return desc, nil
}
