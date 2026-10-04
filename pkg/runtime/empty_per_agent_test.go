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

// TestSelectWorkspaceBackend_EmptyPerAgent pins that empty-per-agent takes
// the NFS backend when NFS workspace storage is configured (design #2703
// P3: the caller mounts the agent's own directory on it), and the local
// backend for every other configuration, including the other shared-volume
// backends.
func TestSelectWorkspaceBackend_EmptyPerAgent(t *testing.T) {
	for _, tc := range []struct {
		cfg  *config.V1WorkspaceStorageConfig
		want string
	}{
		{nil, "local"},
		{&config.V1WorkspaceStorageConfig{}, "local"},
		{&config.V1WorkspaceStorageConfig{Backend: "local"}, "local"},
		{&config.V1WorkspaceStorageConfig{Backend: "nfs", NFS: &config.V1NFSConfig{MountRoot: "/mnt/ws"}}, "nfs"},
		{&config.V1WorkspaceStorageConfig{Backend: "cloudrun-volume"}, "local"},
		{&config.V1WorkspaceStorageConfig{Backend: "gke-shared-volume"}, "local"},
	} {
		if got := SelectWorkspaceBackend(tc.cfg, store.SharingModeEmptyPerAgent).Name(); got != tc.want {
			t.Errorf("SelectWorkspaceBackend(%+v, empty-per-agent) = %q, want %q", tc.cfg, got, tc.want)
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
