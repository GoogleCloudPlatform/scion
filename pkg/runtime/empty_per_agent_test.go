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
	"context"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestCheckWorkspaceBackendMode pins that empty-per-agent fails closed on
// NFS workspace storage (design #2703 P2; NFS support is P3) and that no
// other backend/mode combination is affected.
func TestCheckWorkspaceBackendMode(t *testing.T) {
	cfgs := map[string]*config.V1WorkspaceStorageConfig{
		"nil":               nil,
		"empty":             {},
		"local":             {Backend: "local"},
		"nfs":               {Backend: "nfs", NFS: &config.V1NFSConfig{MountRoot: "/mnt/ws"}},
		"cloudrun-volume":   {Backend: "cloudrun-volume"},
		"gke-shared-volume": {Backend: "gke-shared-volume"},
	}
	modes := []store.WorkspaceSharingMode{
		store.SharingModeSharedPlain,
		store.SharingModeWorktreePerAgent,
		store.SharingModeClonePerAgent,
		store.SharingModeEmptyPerAgent,
	}
	for name, cfg := range cfgs {
		for _, mode := range modes {
			err := CheckWorkspaceBackendMode(cfg, mode)
			wantErr := name == "nfs" && mode == store.SharingModeEmptyPerAgent
			if wantErr {
				if !errors.Is(err, ErrEmptyPerAgentNFSUnsupported) {
					t.Errorf("CheckWorkspaceBackendMode(%s, %s) = %v, want ErrEmptyPerAgentNFSUnsupported", name, mode, err)
				}
			} else if err != nil {
				t.Errorf("CheckWorkspaceBackendMode(%s, %s) = %v, want nil", name, mode, err)
			}
		}
	}
}

// TestSelectWorkspaceBackend_EmptyPerAgentIsLocal pins that empty-per-agent
// never selects a shared-volume backend: it has no shared project workspace.
func TestSelectWorkspaceBackend_EmptyPerAgentIsLocal(t *testing.T) {
	for _, cfg := range []*config.V1WorkspaceStorageConfig{
		nil,
		{Backend: "local"},
		{Backend: "nfs", NFS: &config.V1NFSConfig{MountRoot: "/mnt/ws"}},
		{Backend: "cloudrun-volume"},
		{Backend: "gke-shared-volume"},
	} {
		if got := SelectWorkspaceBackend(cfg, store.SharingModeEmptyPerAgent).Name(); got != "local" {
			t.Errorf("SelectWorkspaceBackend(%+v, empty-per-agent) = %q, want local", cfg, got)
		}
	}
}

// TestCloudRunRuntime_RejectsEmptyPerAgent pins that Cloud Run, which always
// mounts the project's shared workspace, refuses the mode before any API
// call (a nil config/client would otherwise fail differently).
func TestCloudRunRuntime_RejectsEmptyPerAgent(t *testing.T) {
	r := &CloudRunRuntime{}
	_, err := r.Run(context.Background(), RunConfig{
		Env:    []string{"SCION_AGENT_ID=a1", "SCION_WORKSPACE_MODE=empty-per-agent"},
		Labels: map[string]string{"agent_id": "a1"},
	})
	if !errors.Is(err, errEmptyPerAgentCloudRun) {
		t.Fatalf("Run(empty-per-agent) error = %v, want errEmptyPerAgentCloudRun", err)
	}
	for _, env := range [][]string{
		nil,
		{"SCION_WORKSPACE_MODE=shared-plain"},
		{"SCION_WORKSPACE_MODE=worktree-per-agent"},
		{"OTHER=empty-per-agent"},
	} {
		if err := rejectEmptyPerAgentOnCloudRun(RunConfig{Env: env}); err != nil {
			t.Errorf("rejectEmptyPerAgentOnCloudRun(%v) = %v, want nil", env, err)
		}
	}
}
