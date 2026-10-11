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
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// siteIntentDispatcher records, at each dispatch, the run intent the store
// holds for the agent, so a test can tell that the intent was written before
// the dispatch rather than after it. A deleted row records "".
type siteIntentDispatcher struct {
	createAgentDispatcher
	s         store.Store
	createErr error
	startErr  error

	mu   sync.Mutex
	seen map[string][]store.RunIntent
}

func newSiteIntentDispatcher(s store.Store) *siteIntentDispatcher {
	return &siteIntentDispatcher{s: s, seen: map[string][]store.RunIntent{}}
}

func (d *siteIntentDispatcher) record(op, agentID string) {
	var intent store.RunIntent
	if a, err := d.s.GetAgent(context.Background(), agentID); err == nil {
		intent = a.RunIntent
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen[op] = append(d.seen[op], intent)
}

func (d *siteIntentDispatcher) intents(op string) []store.RunIntent {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]store.RunIntent(nil), d.seen[op]...)
}

func (d *siteIntentDispatcher) DispatchAgentCreate(_ context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.record("create", agent.ID)
	if d.createErr != nil {
		return nil, d.createErr
	}
	agent.Phase = string(state.PhaseRunning)
	return nil, nil
}

func (d *siteIntentDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	return d.DispatchAgentCreate(ctx, agent)
}

func (d *siteIntentDispatcher) DispatchAgentProvision(_ context.Context, agent *store.Agent) error {
	d.record("provision", agent.ID)
	agent.Phase = string(state.PhaseCreated)
	return nil
}

func (d *siteIntentDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.record("start", agent.ID)
	if d.startErr != nil {
		return d.startErr
	}
	agent.Phase = string(state.PhaseRunning)
	return nil
}

func (d *siteIntentDispatcher) DispatchAgentDelete(_ context.Context, agent *store.Agent, _, _, _ bool, _ time.Time) error {
	d.record("delete", agent.ID)
	return nil
}

func createSiteAgent(t *testing.T, s store.Store, project *store.Project, name string, phase state.Phase, intent store.RunIntent) *store.Agent {
	t.Helper()
	ctx := context.Background()
	agent := &store.Agent{
		ID:              tid("agent-site-" + name),
		Slug:            name,
		Name:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: project.DefaultRuntimeBrokerID,
		Phase:           string(phase),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	if intent != "" {
		_, err := s.SetRunIntent(ctx, agent.ID, intent)
		require.NoError(t, err)
	}
	return agent
}

func TestRunIntentSites_CreateRecordsRunningBeforeDispatch(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.s = s

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "site-create", ProjectID: project.ID, Task: "work",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []store.RunIntent{store.RunIntentRunning}, disp.intents("create"))
}

func TestRunIntentSites_ProvisionOnlyCreateRecordsStopped(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.s = s

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "site-provision", ProjectID: project.ID, ProvisionOnly: true,
	})
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.Equal(t, []store.RunIntent{store.RunIntentStopped}, disp.intents("provision"))
}

// A create whose dispatch fails records stopped before the cleanup removes
// what the broker provisioned.
func TestRunIntentSites_FailedCreateCleanupRecordsStopped(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	disp.createErr = errors.New("broker refused")
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.s = s

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "site-create-fail", ProjectID: project.ID, Task: "work",
	})
	require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	assert.Equal(t, []store.RunIntent{store.RunIntentRunning}, disp.intents("create"))
	assert.Equal(t, []store.RunIntent{store.RunIntentStopped}, disp.intents("delete"),
		"the cleanup records stopped before it removes the runtime")
}

// Each start an existing-agent create performs records running before its
// dispatch; the env-gather recreate records stopped before its delete.
func TestRunIntentSites_CreateExistingAgent(t *testing.T) {
	cases := []struct {
		name   string
		phase  state.Phase
		req    CreateAgentRequest
		op     string
		intent store.RunIntent
		prior  store.RunIntent
	}{
		{name: "suspended-resume", phase: state.PhaseSuspended, op: "start", intent: store.RunIntentRunning, prior: store.RunIntentStopped},
		{name: "stopped-resume", phase: state.PhaseStopped, req: CreateAgentRequest{Resume: true}, op: "start", intent: store.RunIntentRunning, prior: store.RunIntentStopped},
		{name: "env-gather-recreate", phase: state.PhaseProvisioning, req: CreateAgentRequest{GatherEnv: true}, op: "delete", intent: store.RunIntentStopped, prior: store.RunIntentRunning},
		{name: "provisioning-start", phase: state.PhaseProvisioning, op: "start", intent: store.RunIntentRunning, prior: store.RunIntentStopped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newSiteIntentDispatcher(nil)
			srv, s, project := setupCreateAgentServer(t, disp)
			disp.s = s
			name := "site-existing-" + tc.name
			createSiteAgent(t, s, project, name, tc.phase, tc.prior)

			req := tc.req
			req.Name, req.ProjectID, req.Task = name, project.ID, "work"
			doRequest(t, srv, http.MethodPost, "/api/v1/agents", req)
			got := disp.intents(tc.op)
			require.NotEmpty(t, got, "expected a %s dispatch", tc.op)
			assert.Equal(t, tc.intent, got[0])
		})
	}
}

func TestRunIntentSites_WakeRecordsRunningBeforeDispatch(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	disp.startErr = errors.New("broker refused")
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.s = s
	agent := createSiteAgent(t, s, project, "site-wake", state.PhaseSuspended, store.RunIntentStopped)

	_, dmErr := srv.wakeAgentForDM(context.Background(), agent)
	require.NotNil(t, dmErr)
	assert.Equal(t, []store.RunIntent{store.RunIntentRunning}, disp.intents("start"))
}

func TestRunIntentSites_SyncFinalizeRecordsRunningBeforeDispatch(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.s = s
	srv.SetStorage(newMockStorage("test-bucket"))
	agent := createSiteAgent(t, s, project, "site-sync", state.PhaseProvisioning, store.RunIntentStopped)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-to/finalize",
		map[string]any{"manifest": map[string]any{"version": "1.0", "files": []any{}}})
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.Equal(t, []store.RunIntent{store.RunIntentRunning}, disp.intents("create"))
}

func TestRunIntentSites_ScheduledCreateRecordsRunningBeforeDispatch(t *testing.T) {
	f := bypassAgentsSetup(t)
	disp := newSiteIntentDispatcher(f.store)
	f.srv.SetDispatcher(disp)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "site-scheduled"))
	assert.Equal(t, []store.RunIntent{store.RunIntentRunning}, disp.intents("create"))
	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "site-scheduled")
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent)
}

func TestRunIntentSites_ProjectDeleteRecordsStoppedBeforeDispatch(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.s = s
	agent := createSiteAgent(t, s, project, "site-project-delete", state.PhaseRunning, store.RunIntentRunning)
	gone := store.Agent{ID: tid("agent-site-gone"), RuntimeBrokerID: project.DefaultRuntimeBrokerID}

	srv.dispatchAgentDeletions(context.Background(), []store.Agent{*agent, gone})
	assert.Equal(t, []store.RunIntent{store.RunIntentStopped, ""}, disp.intents("delete"),
		"a remaining row records stopped; a row already gone is still dispatched")
}

func TestRunIntentSites_ManagedLifecycleRecordsIntent(t *testing.T) {
	managedBackendMu.Lock()
	prevBackend := managedBackendInst
	managedBackendInst = stubManagedAgentBackend{}
	managedBackendMu.Unlock()
	t.Cleanup(func() {
		managedBackendMu.Lock()
		managedBackendInst = prevBackend
		managedBackendMu.Unlock()
	})

	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "site-managed", state.PhaseStopped)
	agent.Runtime = ManagedRuntimePrefix + "stub"
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	path := "/api/v1/agents/" + agent.ID

	rec := doRequest(t, srv, http.MethodPost, path+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	started := requireRunIntent(t, s, agent.ID, store.RunIntentRunning)

	rec = doRequest(t, srv, http.MethodPost, path+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	stopped := requireRunIntent(t, s, agent.ID, store.RunIntentStopped)
	assert.True(t, stopped.RunIntentAt.After(*started.RunIntentAt))

	rec = doRequest(t, srv, http.MethodPost, path+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireRunIntent(t, s, agent.ID, store.RunIntentRunning)
}
