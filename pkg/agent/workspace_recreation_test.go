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

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartNFSWorkspace_PopulatedWorkspaceUntouchedOnStart is the
// regression guard for start and restart on a persistent (NFS-backed)
// Kubernetes workspace: a start carries the git clone settings so a
// workspace the runtime did not keep can be cloned again, but a workspace
// that survived on the export must be left exactly as it is. Both cases are
// covered: the broker lost the agent's own state (the first start
// provisions the agent again) and the broker kept it (the second start).
func TestStartNFSWorkspace_PopulatedWorkspaceUntouchedOnStart(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	wsPath := filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "workspace")
	require.NoError(t, os.MkdirAll(filepath.Join(wsPath, ".git"), 0o755))
	unpushed := filepath.Join(wsPath, "unpushed.go")
	const content = "package main // not pushed\n"
	require.NoError(t, os.WriteFile(unpushed, []byte(content), 0o644))

	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))

	var runs []runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			runs = append(runs, rc)
			return "mock-id", nil
		},
	})
	opts := api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		// FreshProvision is left false: buildStartContext sets it only for
		// a create, never for a start or restart.
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-2157",
			"SCION_PROJECT_ID": testNFSWorkspaceProjectID,
		},
		GitClone: &api.GitCloneConfig{URL: "https://example.com/repo.git", Branch: "main"},
	}

	for i, state := range []string{"broker lost the agent state", "broker kept the agent state"} {
		_, err := mgr.Start(context.Background(), opts)
		require.NoError(t, err, state)
		require.Len(t, runs, i+1, state)
		assert.Equal(t, "projects/"+testNFSWorkspaceProjectID+"/workspace", runs[i].NFSSubPath, state)

		got, err := os.ReadFile(unpushed)
		require.NoError(t, err, "%s: the populated workspace must survive the start", state)
		assert.Equal(t, content, string(got), state)
		assert.DirExists(t, filepath.Join(wsPath, ".git"), state)
	}
}
