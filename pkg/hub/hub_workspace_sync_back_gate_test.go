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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubWorkspaceNeedsSyncBack is the gating both hub workspace sync-backs
// share (ptone/scion#4387): hub-native and shared-workspace projects only,
// and no embedded or colocated broker.
func TestHubWorkspaceNeedsSyncBack(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := func(slug, gitRemote, mode string) *store.Project {
		p := &store.Project{ID: tid("gate-" + slug), Slug: tid("gate-" + slug), Name: slug, GitRemote: gitRemote}
		if mode != "" {
			p.Labels = map[string]string{store.LabelWorkspaceMode: mode}
		}
		require.NoError(t, st.CreateProject(ctx, p))
		return p
	}
	native := project("native", "", "")
	shared := project("shared", "github.com/example/gate-shared", store.WorkspaceModeShared)
	worktree := project("worktree", "github.com/example/gate-worktree", store.WorkspaceModeWorktreePerAgent)
	emptyPerAgent := project("empty", "", string(store.SharingModeEmptyPerAgent))

	broker := func(name, localPath string) string {
		b := &store.RuntimeBroker{ID: tid("gate-broker-" + name), Name: name, Slug: name, Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
		require.NoError(t, st.CreateRuntimeBroker(ctx, b))
		for _, p := range []*store.Project{native, shared} {
			require.NoError(t, st.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: p.ID, BrokerID: b.ID, BrokerName: name, LocalPath: localPath, Status: store.BrokerStatusOnline,
			}))
		}
		return b.ID
	}
	remote := broker("gate-remote", "")
	colocated := broker("gate-colocated", "/home/user/.scion")
	embedded := broker("gate-embedded", "")
	srv.SetEmbeddedBrokerID(embedded)

	tests := []struct {
		name        string
		projectID   string
		brokerID    string
		wantOK      bool
		wantErr     bool
		wantProject *store.Project
	}{
		{name: "no project", projectID: "", brokerID: remote},
		{name: "missing project", projectID: tid("gate-missing"), brokerID: remote, wantErr: true},
		{name: "git worktree project", projectID: worktree.ID, brokerID: remote, wantProject: worktree},
		{name: "empty-per-agent project", projectID: emptyPerAgent.ID, brokerID: remote, wantProject: emptyPerAgent},
		{name: "hub-native, remote broker", projectID: native.ID, brokerID: remote, wantOK: true, wantProject: native},
		{name: "shared workspace, remote broker", projectID: shared.ID, brokerID: remote, wantOK: true, wantProject: shared},
		{name: "hub-native, broker without provider", projectID: native.ID, brokerID: tid("gate-unknown"), wantOK: true, wantProject: native},
		{name: "hub-native, no broker", projectID: native.ID, wantOK: true, wantProject: native},
		{name: "hub-native, colocated broker", projectID: native.ID, brokerID: colocated, wantProject: native},
		{name: "shared workspace, colocated broker", projectID: shared.ID, brokerID: colocated, wantProject: shared},
		{name: "hub-native, embedded broker", projectID: native.ID, brokerID: embedded, wantProject: native},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &store.Agent{ID: "gate-agent", ProjectID: tt.projectID, RuntimeBrokerID: tt.brokerID}
			got, ok, err := srv.hubWorkspaceNeedsSyncBack(ctx, agent)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantProject == nil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tt.wantProject.ID, got.ID)
			}
		})
	}
}
