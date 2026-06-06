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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// ValidateNotExportRoot ensures that hostPath is a proper subdirectory of
// hostBase, never equal to it. This is the critical isolation guard from
// design §9.4: bind-mount only projects/<pid>/..., NEVER the export root
// <MountRoot>/<shareID> itself.
//
// Returns nil if hostBase is empty (local backend — no guard needed).
func ValidateNotExportRoot(hostPath, hostBase string) error {
	if hostBase == "" {
		return nil // local backend — no export root to guard against
	}

	cleanPath := filepath.Clean(hostPath)
	cleanBase := filepath.Clean(hostBase)

	if cleanPath == cleanBase {
		return fmt.Errorf("isolation violation: bind path %q equals export root %q — "+
			"must bind a project subtree, never the export root", hostPath, hostBase)
	}

	if !strings.HasPrefix(cleanPath, cleanBase+"/") {
		return fmt.Errorf("isolation violation: bind path %q is not under export root %q",
			hostPath, hostBase)
	}

	return nil
}

// NFSSharedDirsToVolumeMounts converts shared directory declarations into
// VolumeMount entries using NFS-resolved paths from a ResolvedWorkspace.
// This is the NFS counterpart of config.SharedDirsToVolumeMounts — the
// container-side targets are unchanged (/scion-volumes/<name> or in-workspace),
// but the host-side source paths come from the NFS backend's Resolve output
// instead of the local filesystem helpers.
//
// The containerWorkspace parameter specifies the container-side workspace path
// (e.g., /workspace). The resolved workspace must have been produced by
// nfsBackend.Resolve with the shared dir names included in ResolveInput.
func NFSSharedDirsToVolumeMounts(resolved ResolvedWorkspace, dirs []api.SharedDir, containerWorkspace string) ([]api.VolumeMount, error) {
	if len(dirs) == 0 {
		return nil, nil
	}

	if containerWorkspace == "" {
		containerWorkspace = "/workspace"
	}

	var mounts []api.VolumeMount
	for _, d := range dirs {
		sd, ok := resolved.SharedDirs[d.Name]
		if !ok {
			return nil, fmt.Errorf("shared dir %q not found in NFS resolution", d.Name)
		}

		// Isolation guard: shared dir paths must be under the host base, not the root.
		if err := ValidateNotExportRoot(sd.HostPath, resolved.HostBase); err != nil {
			return nil, fmt.Errorf("shared dir %q: %w", d.Name, err)
		}

		target := fmt.Sprintf("/scion-volumes/%s", d.Name)
		if d.InWorkspace {
			target = fmt.Sprintf("%s/.scion-volumes/%s", containerWorkspace, d.Name)
		}

		mounts = append(mounts, api.VolumeMount{
			Source:   sd.HostPath,
			Target:   target,
			ReadOnly: d.ReadOnly,
		})
	}

	return mounts, nil
}
