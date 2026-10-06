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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// A synchronous create's broker create lands after the delete engine's
// broker delete for the same run already returned (the broker had no entry
// yet, so it answered 404) and the delete finished. The row is gone, so
// nothing in the hub tracks the container that create started: the create
// deletes that run again (ptone/scion#3055).
func TestCreateDispatch_BrokerCreateLandsAfterDelete_NoOrphan(t *testing.T) {
	srv, s, project, client, broker := newRunBrokerServer(t)
	pub := recordCreatedEvents(t, srv)

	var sent *RemoteCreateAgentRequest
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		sent = req
		require.NotEmpty(t, req.RunID, "the create names its run")
		// The engine's broker delete reaches the broker first.
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+req.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		// Then the create lands and starts the container.
		broker.land(req.RunID)
		return syncRunningAnswer(req), nil, nil
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "orphan-sync", "projectId": project.ID, "task": "do it",
	})
	require.NotNil(t, sent, "dispatch ran: %d %s", rec.Code, rec.Body.String())
	require.True(t, agentGone(t, s, sent.ID), "the delete removed the row")

	assert.Zero(t, broker.live(),
		"a container started for a deleted agent is left running with nothing tracking it (broker deletes: %v)", broker.deletes)
	assert.Equal(t, 2, broker.deleteCount(sent.RunID), "the engine's delete, then the create's compensating delete, both for this run")
	assert.Contains(t, rec.Body.String(), "agent was deleted while it was starting; its container was removed",
		"the create response says what happened")
	assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
	assert.Equal(t, 1, pub.count("deleted"), "the compensating delete publishes nothing: %v", pub.kinds())
}

// The agent is deleted while its create is in flight, and a new agent is
// created under the same name (its own run) whose container starts first.
// The stale create's compensating delete removes only the old run's
// container; the successor's survives.
func TestCreateDispatch_CompensationSparesSameNameSuccessor(t *testing.T) {
	srv, s, project, client, broker := newRunBrokerServer(t)

	const name = "orphan-successor"
	var stale, successor *RemoteCreateAgentRequest
	var successorRec *httptest.ResponseRecorder
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		if stale != nil {
			// The successor's create: it lands at once.
			successor = req
			broker.land(req.RunID)
			return syncRunningAnswer(req), nil, nil
		}
		stale = req
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+req.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		successorRec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name": name, "projectId": project.ID, "task": "do it",
		})
		broker.land(req.RunID)
		return syncRunningAnswer(req), nil, nil
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": name, "projectId": project.ID, "task": "do it",
	})
	require.NotNil(t, stale, "dispatch ran: %d %s", rec.Code, rec.Body.String())
	require.NotNil(t, successor, "successor dispatched")
	require.Equal(t, http.StatusCreated, successorRec.Code, successorRec.Body.String())
	require.Equal(t, stale.Slug, successor.Slug, "same name")
	require.NotEqual(t, stale.RunID, successor.RunID, "the successor has its own run")

	assert.False(t, broker.has(stale.RunID), "the stale run's container is removed")
	assert.True(t, broker.has(successor.RunID), "the successor's container survives")
	assert.Zero(t, broker.deleteCount(successor.RunID), "no delete ever names the successor's run")
	live := mustGetAgent(t, s, successor.ID)
	assert.Empty(t, live.DeletionState, "the successor is untouched")
}

// Control: with no delete, the create sends no compensating delete.
func TestCreateDispatch_NoDelete_NoCompensation(t *testing.T) {
	srv, s, project, client, broker := newRunBrokerServer(t)
	var sent *RemoteCreateAgentRequest
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		sent = req
		broker.land(req.RunID)
		return syncRunningAnswer(req), nil, nil
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "orphan-control", "projectId": project.ID, "task": "do it",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.True(t, broker.has(sent.RunID), "the container runs")
	assert.Empty(t, broker.deletes, "no broker delete is sent")
	assert.NotContains(t, rec.Body.String(), "deleted while it was starting")
	assert.Equal(t, string(state.PhaseRunning), mustGetAgent(t, s, sent.ID).Phase)
}
