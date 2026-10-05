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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createAgent × delete interleaving (ptone/scion#2972): a create whose
// dispatch returns after a DELETE claimed (or finished with) the row must
// not publish agent.created, on the synchronous dispatch path and on the
// asynchronous launch path (ptone/scion#2153).

// createdRecordingPublisher is deleteRecordingPublisher plus agent.created.
type createdRecordingPublisher struct {
	*deleteRecordingPublisher
}

func (p *createdRecordingPublisher) PublishAgentCreated(ctx context.Context, a *store.Agent) {
	p.mu.Lock()
	p.events = append(p.events, recordedAgentEvent{
		kind: "created", phase: a.Phase, activity: a.Activity,
		deletion: store.ComputeAgentDeletion(a, time.Now()),
	})
	p.mu.Unlock()
	p.deleteRecordingPublisher.PublishAgentCreated(ctx, a)
}

func recordCreatedEvents(t *testing.T, srv *Server) *createdRecordingPublisher {
	t.Helper()
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	pub := &createdRecordingPublisher{newDeleteRecordingPublisher(bus)}
	srv.events = pub
	return pub
}

// kinds lists the recorded event kinds in order.
func (p *createdRecordingPublisher) kinds() []string {
	var out []string
	for _, e := range p.snapshot() {
		out = append(out, e.kind)
	}
	return out
}

// createRaceDispatcher runs hook inside DispatchAgentCreateWithGather, as a
// DELETE that lands while the broker create is in flight would.
type createRaceDispatcher struct {
	engineStubDispatcher
	hook func(a *store.Agent)
}

func (d *createRaceDispatcher) DispatchAgentCreateWithGather(ctx context.Context, a *store.Agent) (*CreateDispatchResult, error) {
	if d.hook != nil {
		d.hook(a)
	}
	return d.engineStubDispatcher.DispatchAgentCreateWithGather(ctx, a)
}

// raceAsyncClient is asyncLaunchClient whose broker delete can block.
type raceAsyncClient struct {
	*asyncLaunchClient
	mu       sync.Mutex
	deleteFn func(ctx context.Context) error
}

func (c *raceAsyncClient) DeleteAgent(ctx context.Context, _, _, _, _ string, _, _, _ bool, _ time.Time) error {
	c.mu.Lock()
	fn := c.deleteFn
	c.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx)
}

func (c *raceAsyncClient) setDeleteFn(fn func(ctx context.Context) error) {
	c.mu.Lock()
	c.deleteFn = fn
	c.mu.Unlock()
}

// newRaceAsyncCreateServer is newAsyncCreateServer (flag on) with a
// raceAsyncClient, so a test can interleave a DELETE with the accepted
// launch.
func newRaceAsyncCreateServer(t *testing.T) (*Server, store.Store, *store.Project, *raceAsyncClient) {
	t.Helper()
	ctx := context.Background()
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-create"))
	require.NoError(t, err)
	broker.Endpoint = "http://localhost:9800"
	broker.Capabilities = &store.BrokerCapabilities{AsyncLaunch: true}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	client := &raceAsyncClient{asyncLaunchClient: &asyncLaunchClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings {
		return AsyncLaunchSettings{Enabled: true, Timeout: 5 * time.Minute, KeepaliveSeconds: 15}
	})
	srv.SetDispatcher(d)
	return srv, s, project, client
}

// deleteMode is how far the racing DELETE gets before the create's dispatch
// returns.
type deleteMode string

const (
	// deleteClaimed: the DELETE has claimed the row and is blocked in its
	// broker dispatch; it completes after the create answers.
	deleteClaimed deleteMode = "claimed"
	// deleteDone: the DELETE has finished (row gone or soft-deleted).
	deleteDone deleteMode = "done"
)

var createDeleteRaceCases = []struct {
	name      string
	mode      deleteMode
	retention time.Duration
}{
	{"hard/claimed", deleteClaimed, 0},
	{"hard/done", deleteDone, 0},
	{"soft/claimed", deleteClaimed, time.Hour},
	{"soft/done", deleteDone, time.Hour},
}

// assertDeleteLanded checks the row is hard-deleted, or soft-deleted when
// retention is on.
func assertDeleteLanded(t *testing.T, s store.Store, agentID string, retention time.Duration) {
	t.Helper()
	if retention == 0 {
		assert.True(t, agentGone(t, s, agentID), "hard delete removes the row")
		return
	}
	got := mustGetAgent(t, s, agentID)
	assert.False(t, got.DeletedAt.IsZero(), "soft delete keeps a tombstoned row")
}

// assertAsyncDeleteLanded checks the row is hard-deleted: a create whose
// launch is still in flight is an incomplete create, which the delete
// hard-deletes even with retention on (agent_delete_engine.go claim, design
// ptone/scion#2483 acceptance (bb)).
func assertAsyncDeleteLanded(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	assert.True(t, agentGone(t, s, agentID), "an in-flight create is hard-deleted")
}

// Sync dispatch: the DELETE lands while DispatchAgentCreateWithGather runs.
// No created is published, exactly one deleted is, and the delete completes.
func TestCreateAgentPublish_SyncDispatchRacesDelete(t *testing.T) {
	for i, tc := range createDeleteRaceCases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &createRaceDispatcher{}
			srv, s, project := setupCreateAgentServer(t, disp)
			srv.config.SoftDeleteRetention = tc.retention
			pub := recordCreatedEvents(t, srv)

			var agentID string
			var delCh <-chan deleteResult
			release := make(chan struct{})
			disp.hook = func(a *store.Agent) {
				agentID = a.ID
				switch tc.mode {
				case deleteClaimed:
					entered := make(chan struct{})
					disp.setFn(blockingDelete(entered, release, nil))
					delCh = deleteAsync(t, srv, "/api/v1/agents/"+a.ID, nil)
					waitClosed(t, entered, 5*time.Second, "broker delete dispatch")
					assert.Equal(t, store.DeletionStateDeleting, mustGetAgent(t, s, a.ID).DeletionState)
				case deleteDone:
					rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+a.ID, nil)
					require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
				}
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "race-sync-" + string(rune('a'+i)), ProjectID: project.ID, Task: "do it",
			})
			require.NotEmpty(t, agentID, "dispatch ran: %s", rec.Body.String())
			assert.Zero(t, pub.count("created"), "no created after the delete claimed: %v", pub.kinds())

			if tc.mode == deleteClaimed {
				close(release)
				r := waitDelete(t, delCh, 10*time.Second)
				require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
			}
			assert.Zero(t, pub.count("created"), "no created at all: %v", pub.kinds())
			assert.Equal(t, 1, pub.count("deleted"), "exactly one deleted: %v", pub.kinds())
			assertDeleteLanded(t, s, agentID, tc.retention)
		})
	}
}

// Async launch: the broker accepts the launch, and the DELETE lands before
// createAgent publishes. No created is published; a late launch report for
// the deleted agent publishes no status either.
func TestCreateAgentPublish_AsyncLaunchRacesDelete(t *testing.T) {
	for i, tc := range createDeleteRaceCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project, client := newRaceAsyncCreateServer(t)
			srv.config.SoftDeleteRetention = tc.retention
			pub := recordCreatedEvents(t, srv)

			var sent *RemoteCreateAgentRequest
			var delCh <-chan deleteResult
			release := make(chan struct{})
			client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				sent = req
				require.True(t, req.AsyncLaunch, "the create is dispatched for async launch")
				switch tc.mode {
				case deleteClaimed:
					entered := make(chan struct{})
					var once sync.Once
					client.setDeleteFn(func(ctx context.Context) error {
						once.Do(func() { close(entered) })
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					})
					delCh = deleteAsync(t, srv, "/api/v1/agents/"+req.ID, nil)
					waitClosed(t, entered, 5*time.Second, "broker delete dispatch")
					assert.Equal(t, store.DeletionStateDeleting, mustGetAgent(t, s, req.ID).DeletionState)
				case deleteDone:
					rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+req.ID, nil)
					require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
				}
				return acceptedAnswer(req, req.LaunchID), nil, nil
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
				"name": "race-async-" + string(rune('a'+i)), "projectId": project.ID, "task": "do it", "acceptAsyncLaunch": true,
			})
			require.NotNil(t, sent, "dispatch ran: %s", rec.Body.String())
			assert.Zero(t, pub.count("created"), "no created after the delete claimed: %v", pub.kinds())

			if tc.mode == deleteClaimed {
				close(release)
				r := waitDelete(t, delCh, 10*time.Second)
				require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
			}
			assert.Zero(t, pub.count("created"), "no created at all: %v", pub.kinds())
			require.Equal(t, 1, pub.count("deleted"), "exactly one deleted: %v", pub.kinds())
			assertAsyncDeleteLanded(t, s, sent.ID)

			// A launch report arriving after the delete is refused and
			// publishes nothing.
			before := len(pub.snapshot())
			lr := postLaunchReport(t, srv, tid("broker-create"), sent.ID, tid("broker-create"), AgentLaunchReport{
				LaunchID: sent.LaunchID, InstanceID: "broker-instance-1", Seq: 1, State: store.LaunchReportStateSucceeded,
				Phase: string(state.PhaseRunning),
			})
			assert.NotEqual(t, http.StatusOK, lr.Code, lr.Body.String())
			assert.Equal(t, before, len(pub.snapshot()), "late launch report publishes nothing: %v", pub.kinds())
		})
	}
}

// Control: a create with no racing delete publishes exactly one created, on
// both paths.
func TestCreateAgentPublish_NoDeletePublishesOnce(t *testing.T) {
	t.Run("sync", func(t *testing.T) {
		srv, _, project := setupCreateAgentServer(t, &createRaceDispatcher{})
		pub := recordCreatedEvents(t, srv)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name: "ctl-sync", ProjectID: project.ID, Task: "do it",
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"created"}, pub.kinds())
	})
	t.Run("async", func(t *testing.T) {
		srv, _, project, _ := newRaceAsyncCreateServer(t)
		pub := recordCreatedEvents(t, srv)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name": "ctl-async", "projectId": project.ID, "task": "do it", "acceptAsyncLaunch": true,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent.Launch, "the launch was accepted asynchronously")
		evs := pub.snapshot()
		require.Len(t, evs, 1, "%v", pub.kinds())
		assert.Equal(t, "created", evs[0].kind)
		assert.Equal(t, string(state.PhaseProvisioning), evs[0].phase)
	})
}

// The helper's predicate, per deletion state: only a gone, soft-deleted or
// claimed row suppresses the publish; a failed or lapsed delete does not.
func TestPublishAgentCreatedIfLive_DeletionStates(t *testing.T) {
	cases := []struct {
		name    string
		seed    *deleteSeed
		soft    bool
		gone    bool
		publish bool
	}{
		{name: "no marker", publish: true},
		{name: "failed", seed: &deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError}, publish: true},
		{name: "deleting lease expired", seed: &deleteSeed{state: store.DeletionStateDeleting, leaseIn: -time.Minute}, publish: true},
		{name: "deleting live", seed: &deleteSeed{state: store.DeletionStateDeleting, leaseIn: time.Minute}},
		{name: "finalizing live", seed: &deleteSeed{state: store.DeletionStateFinalizing, leaseIn: time.Minute}},
		{name: "finalizing lease expired", seed: &deleteSeed{state: store.DeletionStateFinalizing, leaseIn: -time.Minute}},
		{name: "soft-deleted", soft: true},
		{name: "gone", gone: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			pub := recordCreatedEvents(t, srv)
			ctx := context.Background()
			agent := setupBrokerAgentInPhase(t, s, "cpl-"+string(rune('a'+i)), state.PhaseProvisioning)
			if tc.seed != nil {
				seedAgentDeletion(t, s, agent.ID, *tc.seed)
			}
			if tc.soft {
				row := mustGetAgent(t, s, agent.ID)
				row.DeletedAt = time.Now()
				require.NoError(t, s.UpdateAgent(ctx, row))
				require.False(t, mustGetAgent(t, s, agent.ID).DeletedAt.IsZero())
			}
			if tc.gone {
				require.NoError(t, s.DeleteAgent(ctx, agent.ID))
			}

			srv.publishAgentCreatedIfLive(ctx, agent)
			if tc.publish {
				assert.Equal(t, []string{"created"}, pub.kinds())
			} else {
				assert.Empty(t, pub.kinds())
			}
		})
	}
}
