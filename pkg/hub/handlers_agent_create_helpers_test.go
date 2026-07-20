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

//go:build !no_sqlite

package hub

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPopulateAgentConfig_RelativeWorkspacePreserved(t *testing.T) {
	// Set up a temp HOME so hubManagedProjectPath can resolve.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	slug := "test-project"
	globalDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(globalDir, "projects", slug), 0755))

	srv, _ := testServer(t)

	project := &store.Project{
		ID:   tid("project-rel-ws"),
		Name: "Test Project",
		Slug: slug,
		// No GitRemote — hub-managed project
	}

	t.Run("relative workspace preserved", func(t *testing.T) {
		agent := &store.Agent{
			ID: tid("agent-rel"),
			AppliedConfig: &store.AgentAppliedConfig{
				Workspace: "packages/web",
			},
		}

		srv.populateAgentConfig(context.Background(), agent, project, nil)

		assert.Equal(t, "packages/web", agent.AppliedConfig.Workspace,
			"relative workspace should not be overwritten by hub-managed path")
	})

	t.Run("absolute workspace preserved", func(t *testing.T) {
		agent := &store.Agent{
			ID: tid("agent-abs"),
			AppliedConfig: &store.AgentAppliedConfig{
				Workspace: "/absolute/path",
			},
		}

		srv.populateAgentConfig(context.Background(), agent, project, nil)

		assert.Equal(t, "/absolute/path", agent.AppliedConfig.Workspace,
			"absolute workspace should be preserved as user override")
	})

	t.Run("empty workspace overwritten", func(t *testing.T) {
		agent := &store.Agent{
			ID: tid("agent-empty"),
			AppliedConfig: &store.AgentAppliedConfig{
				Workspace: "",
			},
		}

		srv.populateAgentConfig(context.Background(), agent, project, nil)

		expectedPath, err := hubManagedProjectPath(slug)
		require.NoError(t, err)
		assert.Equal(t, expectedPath, agent.AppliedConfig.Workspace,
			"empty workspace should be overwritten with hub-managed path")
	})
}
