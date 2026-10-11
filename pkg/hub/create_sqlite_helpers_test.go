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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// activeReservationResources returns the resource IDs of the active
// reservations for limitName in the given scope.
func activeReservationResources(t *testing.T, s store.Store, limitName, scopeType, scopeID string) []string {
	t.Helper()
	ctx := context.Background()
	def, err := s.GetLimitDefinitionByName(ctx, limitName)
	require.NoError(t, err)
	res, err := s.ListActiveReservations(ctx, def.ID, scopeType, scopeID)
	require.NoError(t, err)
	ids := make([]string, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.ResourceID)
	}
	return ids
}

// recordingManagedBackend is a managed-agent backend whose CreateInteraction
// runs hook (with the create's row ID) and succeeds, and which records the
// interactions it is asked to cancel. Every interaction reads in progress.
type recordingManagedBackend struct {
	failingManagedAgentBackend
	s         store.Store
	projectID string
	hook      func(agentID string)

	mu        sync.Mutex
	cancelled []string
	cancelErr error
	getErr    error
	// noState makes GetInteraction answer no state and no error.
	noState bool
	// cancelCtxErr records the ctx error each cancel saw.
	cancelCtxErr []error
}

// useManagedBackend installs backend for the test. Tests that call it must
// not run in parallel: the backend is package-level.
func useManagedBackend(t *testing.T, backend managedagent.ManagedAgentBackend) {
	t.Helper()
	managedBackendMu.Lock()
	prev := managedBackendInst
	managedBackendInst = backend
	managedBackendMu.Unlock()
	t.Cleanup(func() {
		managedBackendMu.Lock()
		managedBackendInst = prev
		managedBackendMu.Unlock()
	})
}

// requireDeletedDuringCreate checks rec is the 409 delete_in_progress answer
// for agentID, with no agent body, and returns its details.warnings.
func requireDeletedDuringCreate(t *testing.T, rec *httptest.ResponseRecorder, agentID string) []string {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "agent", "the 409 carries no agent body")
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeDeleteInProgress, body.Error.Code)
	assert.Equal(t, deletedDuringCreateMessage, body.Error.Message)
	assert.Equal(t, agentID, body.Error.Details["agentId"])
	var warnings []string
	if ws, ok := body.Error.Details["warnings"].([]interface{}); ok {
		for _, w := range ws {
			warnings = append(warnings, w.(string))
		}
	}
	return warnings
}

// newRaceSyncCreateServer is newRaceAsyncCreateServer with async launch off,
// so createAgent takes the synchronous broker create path through the real
// HTTPAgentDispatcher.
func newRaceSyncCreateServer(t *testing.T) (*Server, store.Store, *store.Project, *raceAsyncClient) {
	t.Helper()
	srv, s, project, client := newRaceAsyncCreateServer(t)
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return AsyncLaunchSettings{} })
	srv.SetDispatcher(d)
	return srv, s, project, client
}

// syncRunningAnswer is the broker's answer to a synchronous create that
// started the container.
func syncRunningAnswer(req *RemoteCreateAgentRequest) *RemoteAgentResponse {
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID: "container-" + req.Slug, Slug: req.Slug, Name: req.Name,
			Phase: string(state.PhaseRunning), ContainerStatus: "Up 1 second",
			RunID: req.RunID,
		},
		Created: true,
	}
}

// runBroker is a broker that tracks the runtime entries it holds by run ID:
// a create adds its run's entry when it lands, and a delete removes the
// entry for the run it names (a delete for a run with no entry is the
// broker's 404, which the hub treats as success). A delete naming no run
// matches by name, so it removes every entry.
type runBroker struct {
	*raceAsyncClient
	mu      sync.Mutex
	entries map[string]bool
	deletes []string
}

// newRunBrokerServer is newRaceSyncCreateServer over a runBroker.
func newRunBrokerServer(t *testing.T) (*Server, store.Store, *store.Project, *raceAsyncClient, *runBroker) {
	t.Helper()
	srv, s, project, client := newRaceSyncCreateServer(t)
	broker := &runBroker{raceAsyncClient: client, entries: map[string]bool{}}
	d := NewHTTPAgentDispatcherWithClient(s, broker, false, srv.agentLifecycleLog)
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return AsyncLaunchSettings{} })
	srv.SetDispatcher(d)
	return srv, s, project, client, broker
}

func (b *recordingManagedBackend) CreateInteraction(context.Context, managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	if b.hook != nil {
		result, err := b.s.ListAgents(context.Background(), store.AgentFilter{ProjectID: b.projectID}, store.ListOptions{})
		if err == nil && len(result.Items) == 1 {
			b.hook(result.Items[0].ID)
		}
	}
	return &managedagent.InteractionHandle{InteractionID: "interaction-1"}, nil
}

func (b *recordingManagedBackend) GetInteraction(_ context.Context, id string) (*managedagent.InteractionState, error) {
	if b.getErr != nil {
		return nil, b.getErr
	}
	if b.noState {
		return nil, nil
	}
	return &managedagent.InteractionState{InteractionID: id, Status: managedagent.StatusInProgress}, nil
}

func (b *recordingManagedBackend) CancelInteraction(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelled = append(b.cancelled, id)
	b.cancelCtxErr = append(b.cancelCtxErr, ctx.Err())
	return b.cancelErr
}

func (b *recordingManagedBackend) cancels() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.cancelled...)
}

func (b *runBroker) DeleteAgent(ctx context.Context, brokerID, endpoint, agentID, projectID string, opts DeleteAgentOptions) error {
	b.mu.Lock()
	b.deletes = append(b.deletes, opts.RunID)
	if opts.RunID == "" {
		// A delete naming no run resolves by name: every entry goes.
		b.entries = map[string]bool{}
	}
	delete(b.entries, opts.RunID)
	b.mu.Unlock()
	return b.raceAsyncClient.DeleteAgent(ctx, brokerID, endpoint, agentID, projectID, opts)
}

func (b *runBroker) land(runID string) {
	b.mu.Lock()
	b.entries[runID] = true
	b.mu.Unlock()
}

func (b *runBroker) live() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

func (b *runBroker) has(runID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.entries[runID]
}

func (b *runBroker) deleteCount(runID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, r := range b.deletes {
		if r == runID {
			n++
		}
	}
	return n
}
