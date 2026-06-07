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

// gkeSharedVolumeBackend resolves and realizes workspace mounts using a
// GKE-provided shared volume (e.g. a Filestore CSI-backed PVC). The PVC
// is managed by GKE — the backend emits MountDescriptor with Type
// "gke-shared-volume" containing the volume name, PVC name, and sub-path.
type gkeSharedVolumeBackend struct {
	cfg *config.V1GKESharedVolumeConfig
}

// NewGKESharedVolumeBackend returns a WorkspaceBackend for GKE-managed shared volumes.
func NewGKESharedVolumeBackend(cfg *config.V1GKESharedVolumeConfig) WorkspaceBackend {
	return &gkeSharedVolumeBackend{cfg: cfg}
}

func (b *gkeSharedVolumeBackend) Name() string { return "gke-shared-volume" }

// Resolve computes workspace paths within the GKE shared volume.
// ServerRelativePath holds the sub-path for the project workspace within
// the PVC. There is no HostPath — the volume is PVC-managed.
func (b *gkeSharedVolumeBackend) Resolve(in ResolveInput) (ResolvedWorkspace, error) {
	if in.ProjectID == "" {
		return ResolvedWorkspace{}, fmt.Errorf("gkeSharedVolumeBackend.Resolve: ProjectID is required")
	}
	if b.cfg == nil {
		return ResolvedWorkspace{}, fmt.Errorf("gkeSharedVolumeBackend.Resolve: GKESharedVolume config is nil")
	}
	if b.cfg.VolumeName == "" {
		return ResolvedWorkspace{}, fmt.Errorf("gkeSharedVolumeBackend.Resolve: volume_name is required")
	}

	subPathRoot := b.cfg.SubPathRoot
	if subPathRoot == "" {
		subPathRoot = "projects"
	}

	workspaceRelPath := filepath.Join(subPathRoot, in.ProjectID, "workspace")

	res := ResolvedWorkspace{
		ServerRelativePath: workspaceRelPath,
		Backend:            "gke-shared-volume",
		SharedDirs:         make(map[string]ResolvedSharedDir, len(in.SharedDirNames)),
	}

	for _, name := range in.SharedDirNames {
		sdRelPath := filepath.Join(subPathRoot, in.ProjectID, "shared-dirs", name)
		res.SharedDirs[name] = ResolvedSharedDir{
			ServerRelativePath: sdRelPath,
		}
	}

	return res, nil
}

// Realize emits a gke-shared-volume MountDescriptor with the volume name,
// PVC claim name, and sub-path. K8s/GKE wires the actual mount.
func (b *gkeSharedVolumeBackend) Realize(in RealizeInput) (MountDescriptor, error) {
	target := in.ContainerWorkspace
	if target == "" {
		target = "/workspace"
	}

	if b.cfg == nil || b.cfg.VolumeName == "" {
		return MountDescriptor{}, fmt.Errorf("gkeSharedVolumeBackend.Realize: volume_name is required")
	}

	return MountDescriptor{
		Type:        "gke-shared-volume",
		Target:      target,
		VolumeName:  b.cfg.VolumeName,
		PVClaimName: b.cfg.PVClaimName,
		SubPath:     in.Resolved.ServerRelativePath,
	}, nil
}
