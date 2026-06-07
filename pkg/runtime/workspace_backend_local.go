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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// localBackend wraps today's node-local workspace behavior. Resolve delegates
// to existing path helpers; Provision and Realize mirror the current local flow.
// This is the default backend — selecting it produces zero behavior change.
type localBackend struct{}

// NewLocalBackend returns a WorkspaceBackend backed by node-local storage.
func NewLocalBackend() WorkspaceBackend {
	return &localBackend{}
}

func (b *localBackend) Name() string { return "local" }

// Resolve computes workspace and shared-dir host paths using the existing
// local path resolution logic. The ProjectDir field on ResolveInput must be
// set (typically from the broker's hub-native project path resolution).
func (b *localBackend) Resolve(in ResolveInput) (ResolvedWorkspace, error) {
	if in.ProjectDir == "" {
		return ResolvedWorkspace{}, fmt.Errorf("localBackend.Resolve: ProjectDir is required")
	}

	res := ResolvedWorkspace{
		HostPath:   in.ProjectDir,
		Backend:    "local",
		SharedDirs: make(map[string]ResolvedSharedDir, len(in.SharedDirNames)),
	}

	// Resolve shared dirs using the existing config helpers.
	for _, name := range in.SharedDirNames {
		hostPath, err := config.GetSharedDirPath(in.ProjectDir, name)
		if err != nil {
			return ResolvedWorkspace{}, fmt.Errorf("localBackend.Resolve: shared dir %q: %w", name, err)
		}
		res.SharedDirs[name] = ResolvedSharedDir{
			HostPath: hostPath,
		}
	}

	return res, nil
}

// Realize returns a local bind-mount descriptor pointing at the resolved
// host path. This mirrors today's Docker `-v HOST:/workspace` behavior.
func (b *localBackend) Realize(in RealizeInput) (MountDescriptor, error) {
	target := in.ContainerWorkspace
	if target == "" {
		target = "/workspace"
	}

	return MountDescriptor{
		Type:     "local",
		HostPath: in.Resolved.HostPath,
		Target:   target,
	}, nil
}
