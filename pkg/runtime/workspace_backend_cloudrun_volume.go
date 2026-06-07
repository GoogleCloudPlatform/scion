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

// cloudrunVolumeBackend resolves and realizes workspace mounts using a
// Cloud Run managed volume. Cloud Run volumes are declared in the service
// spec and mounted by the platform — no host path or NFS server is needed.
// The backend emits MountDescriptor with Type "cloudrun-volume".
type cloudrunVolumeBackend struct {
	cfg *config.V1CloudRunVolumeConfig
}

// NewCloudRunVolumeBackend returns a WorkspaceBackend for Cloud Run managed volumes.
func NewCloudRunVolumeBackend(cfg *config.V1CloudRunVolumeConfig) WorkspaceBackend {
	return &cloudrunVolumeBackend{cfg: cfg}
}

func (b *cloudrunVolumeBackend) Name() string { return "cloudrun-volume" }

// Resolve computes workspace paths within the Cloud Run volume.
// There is no host path — the volume is platform-managed. ServerRelativePath
// holds the sub-path within the volume for the project workspace.
func (b *cloudrunVolumeBackend) Resolve(in ResolveInput) (ResolvedWorkspace, error) {
	if in.ProjectID == "" {
		return ResolvedWorkspace{}, fmt.Errorf("cloudrunVolumeBackend.Resolve: ProjectID is required")
	}
	if b.cfg == nil {
		return ResolvedWorkspace{}, fmt.Errorf("cloudrunVolumeBackend.Resolve: CloudRunVolume config is nil")
	}
	if b.cfg.VolumeName == "" {
		return ResolvedWorkspace{}, fmt.Errorf("cloudrunVolumeBackend.Resolve: volume_name is required")
	}

	subPathRoot := b.cfg.SubPathRoot
	if subPathRoot == "" {
		subPathRoot = "projects"
	}

	workspaceRelPath := filepath.Join(subPathRoot, in.ProjectID, "workspace")

	res := ResolvedWorkspace{
		ServerRelativePath: workspaceRelPath,
		Backend:            "cloudrun-volume",
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

// Realize emits a cloudrun-volume MountDescriptor with the volume name and
// sub-path. Cloud Run wires the actual mount — the runtime just declares it.
func (b *cloudrunVolumeBackend) Realize(in RealizeInput) (MountDescriptor, error) {
	target := in.ContainerWorkspace
	if target == "" {
		target = "/workspace"
	}

	if b.cfg == nil || b.cfg.VolumeName == "" {
		return MountDescriptor{}, fmt.Errorf("cloudrunVolumeBackend.Realize: volume_name is required")
	}

	return MountDescriptor{
		Type:       "cloudrun-volume",
		Target:     target,
		VolumeName: b.cfg.VolumeName,
		SubPath:    in.Resolved.ServerRelativePath,
	}, nil
}
