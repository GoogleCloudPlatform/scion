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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var layoutPrecheckHub = projectfs.Layout{
	Backend: projectfs.BackendNFS, HostBase: "/mnt/nfs/a", SubPathRoot: "projects",
	Share: projectfs.Share{ID: "a", Server: "10.0.0.2", Export: "/share/a"},
}

func layoutPrecheckDescriptor(server string) *api.BrokerWorkspaceStorage {
	return &api.BrokerWorkspaceStorage{Backend: "nfs", Homes: "local", NFS: &api.BrokerNFSWorkspaceStorage{
		Server: server, Export: "/share/a", SubPathRoot: "projects", Healthy: true}}
}

// layoutPrecheckDispatcher is a dispatcher whose hub Layout is layout, with
// a broker reporting desc and a hub-native project in workspace mode
// modeLabel ("" is shared-plain) holding one agent.
func layoutPrecheckDispatcher(t *testing.T, layout projectfs.Layout, desc *api.BrokerWorkspaceStorage, modeLabel string) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("layout-broker"), Name: "layout-broker", Slug: "layout-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline, WorkspaceStorage: desc,
	}))
	project := &store.Project{ID: tid("layout-project"), Name: "layout", Slug: "layout"}
	if modeLabel != "" {
		project.Labels = map[string]string{store.LabelWorkspaceMode: modeLabel}
	}
	if modeLabel == "clone-per-agent" {
		// clone-per-agent needs a git project.
		project.GitRemote = "github.com/test/layout"
	}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("layout-agent"), Name: "layout-agent", Slug: "layout-agent",
		ProjectID: project.ID, RuntimeBrokerID: tid("layout-broker"),
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	m := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, m, false, slog.Default())
	d.SetStorageLayout(func() (projectfs.Layout, error) { return layout, nil })
	return d, m, agent
}

// TestDispatch_StorageLayoutPrecheck pins the hub/broker Layout identity
// check (ptone/scion#5823): on a shared hub Layout every dispatch path
// refuses a shared-mode agent on a broker whose reported layout disagrees,
// with a 409 carrying the diff and no broker call; a matching broker, a
// per-agent workspace mode and a local hub dispatch.
func TestDispatch_StorageLayoutPrecheck(t *testing.T) {
	ctx := context.Background()
	for _, op := range precheckOps(ctx) {
		t.Run(op.name+" refuses a mismatched broker", func(t *testing.T) {
			d, m, a := layoutPrecheckDispatcher(t, layoutPrecheckHub, layoutPrecheckDescriptor("10.0.0.9"), "")
			err := op.run(d, a)
			var refusal *RuntimeTargetRefusal
			require.True(t, errors.As(err, &refusal), "refused with a typed refusal: %v", err)
			assert.Equal(t, http.StatusConflict, refusal.Status)
			details, ok := refusal.Details[storageLayoutMismatchDetail].(map[string]interface{})
			require.True(t, ok, "structured details: %#v", refusal.Details)
			assert.Equal(t, []string{"nfs server"}, details["fields"])
			assert.Equal(t, []string{"nfs server: differs"}, details["issues"])
			assert.Contains(t, refusal.Message, "server.project_storage")
			// The refusal names the field, never the values, in the
			// message and in the structured details.
			for _, v := range []string{"10.0.0.2", "10.0.0.9", "/share/a", "projects"} {
				assert.NotContains(t, refusal.Message, v)
				assert.NotContains(t, fmt.Sprintf("%v", refusal.Details), v)
			}
			assert.False(t, op.called(m), "no broker call")
		})
		t.Run(op.name+" refuses a broker without a descriptor", func(t *testing.T) {
			d, m, a := layoutPrecheckDispatcher(t, layoutPrecheckHub, nil, "worktree-per-agent")
			var refusal *RuntimeTargetRefusal
			require.True(t, errors.As(op.run(d, a), &refusal))
			assert.False(t, op.called(m), "no broker call")
		})
	}
	for name, tc := range map[string]struct {
		layout     projectfs.Layout
		desc       *api.BrokerWorkspaceStorage
		mode       string
		wantCalled bool
	}{
		"matching broker":         {layoutPrecheckHub, layoutPrecheckDescriptor("10.0.0.2"), "", true},
		"clone-per-agent project": {layoutPrecheckHub, layoutPrecheckDescriptor("10.0.0.9"), "clone-per-agent", false},
		"empty-per-agent project": {layoutPrecheckHub, layoutPrecheckDescriptor("10.0.0.9"), "empty-per-agent", false},
		"local hub":               {projectfs.Layout{Backend: projectfs.BackendLocal}, nil, "", true},
	} {
		t.Run(name+" is not refused for its layout", func(t *testing.T) {
			d, m, a := layoutPrecheckDispatcher(t, tc.layout, tc.desc, tc.mode)
			err := d.DispatchAgentStart(ctx, a, "task", false)
			var refusal *RuntimeTargetRefusal
			assert.False(t, errors.As(err, &refusal) && refusal.Details[storageLayoutMismatchDetail] != nil, "no layout refusal: %v", err)
			if tc.wantCalled {
				assert.True(t, m.startCalled, "broker called")
			}
		})
	}
	t.Run("unusable hub layout fails closed", func(t *testing.T) {
		d, m, a := layoutPrecheckDispatcher(t, layoutPrecheckHub, layoutPrecheckDescriptor("10.0.0.2"), "")
		d.SetStorageLayout(func() (projectfs.Layout, error) { return projectfs.Layout{}, errors.New("bad block") })
		var refusal *RuntimeTargetRefusal
		require.True(t, errors.As(d.DispatchAgentStart(ctx, a, "task", false), &refusal))
		assert.NotContains(t, refusal.Message, "bad block", "the configuration detail stays in the hub log")
		assert.False(t, m.startCalled)
	})
}

// TestHealthSummary_BrokerLayoutMismatch: the health payload carries the
// per-broker layout diff and counts it as a problem.
func TestHealthSummary_BrokerLayoutMismatch(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig("/mnt/nfs"))
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("hs-layout-bad"), Name: "bad", Slug: "hs-layout-bad", Endpoint: "http://localhost:9801",
		Status: store.BrokerStatusOnline, WorkspaceStorage: &api.BrokerWorkspaceStorage{Backend: "local", Homes: "local"},
	}))
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("hs-layout-ok"), Name: "ok", Slug: "hs-layout-ok", Endpoint: "http://localhost:9802",
		Status: store.BrokerStatusOnline, WorkspaceStorage: &api.BrokerWorkspaceStorage{Backend: "nfs", Homes: "local",
			NFS: &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/scion", SubPathRoot: "projects", Healthy: true}},
	}))
	list, _, err := srv.healthSummaryBrokers(ctx, nil, "")
	require.NoError(t, err)
	rows := map[string]HealthSummaryBroker{}
	for _, r := range list.Items {
		rows[r.ID] = r
	}
	require.NotNil(t, rows[tid("hs-layout-bad")].WorkspaceStorage)
	assert.Equal(t, []string{`backend: differs`}, rows[tid("hs-layout-bad")].WorkspaceStorage.LayoutMismatch,
		"a caller that is not a hub admin sees the differing field only")
	assert.True(t, healthSummaryBrokerHasProblem(rows[tid("hs-layout-bad")]))
	require.NotNil(t, rows[tid("hs-layout-ok")].WorkspaceStorage)
	assert.Empty(t, rows[tid("hs-layout-ok")].WorkspaceStorage.LayoutMismatch)
	assert.False(t, healthSummaryBrokerHasProblem(rows[tid("hs-layout-ok")]))

	// A hub admin sees the values.
	adminCtx := storageAdminCtx(t, s, "hs-layout-admin")
	list, _, err = srv.healthSummaryBrokers(adminCtx, nil, "")
	require.NoError(t, err)
	for _, r := range list.Items {
		if r.ID == tid("hs-layout-bad") {
			assert.Equal(t, []string{`backend: hub "nfs", broker "local"`}, r.WorkspaceStorage.LayoutMismatch)
		}
	}
}
