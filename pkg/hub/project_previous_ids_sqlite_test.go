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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDispatch_PreviousProjectIDsOnCreateStartRestart: for a shared-workspace
// git project the create request, the start extras and the restart extras
// all carry the legacy ID of the project's remote; for a project in another
// workspace mode none of them does.
func TestDispatch_PreviousProjectIDsOnCreateStartRestart(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   []string
	}{
		{"shared-workspace project", map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared}, []string{prevIDsLegacyID}},
		{"worktree-per-agent project", map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memStore := createTestStore(t)
			require.NoError(t, memStore.CreateProject(ctx, &store.Project{
				ID: prevIDsCurrent, Name: "widgets", Slug: "widgets",
				GitRemote: prevIDsRemote, Labels: tc.labels,
			}))
			require.NoError(t, memStore.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: tid("host-prev-ids"), Name: "test-host", Slug: "test-host",
				Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
			}))
			require.NoError(t, memStore.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: prevIDsCurrent, BrokerID: tid("host-prev-ids"), BrokerName: "test-host",
				LocalPath: "/home/user/src/widgets", Status: store.BrokerStatusOnline,
			}))
			client := &mockRuntimeBrokerClient{}
			d := NewHTTPAgentDispatcherWithClient(memStore, client, false, slog.Default())
			ag := &store.Agent{
				ID: "agent-prev-ids", Name: "prev-ids-agent", Slug: "prev-ids-agent",
				ProjectID: prevIDsCurrent, OwnerID: "owner-uuid", RuntimeBrokerID: tid("host-prev-ids"),
				AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
			}

			req, err := d.buildCreateRequest(ctx, ag, "test")
			require.NoError(t, err)
			assert.Equal(t, tc.want, req.PreviousProjectIDs, "create")

			require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
			assert.Equal(t, tc.want, client.lastStartExtras.PreviousProjectIDs, "start")

			require.NoError(t, d.DispatchAgentRestart(ctx, ag))
			assert.Equal(t, tc.want, client.lastRestartExtras.PreviousProjectIDs, "restart")
		})
	}
}
