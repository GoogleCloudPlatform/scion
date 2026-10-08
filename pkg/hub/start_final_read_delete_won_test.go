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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A start that loses to a delete claimed just before its final read
// answers 409 delete_in_progress, as the lifecycle start does after its
// final write (ptone/scion#3546), on the two start paths that did not:
// the managed-runtime start and restart (ptone/scion#3705) and the start of
// an existing agent through POST /agents (ptone/scion#3711). The check is
// deleteWonOnRead's rule (design ptone/scion#2483 §2.1): a live deleting
// claim, a finalizing row (even with its lease expired), or a row that is
// soft- or hard-deleted is delete-won; a failed delete, or a deleting row
// whose lease lapsed, is a live agent and still answers 200.

// finalReadDeletes is the deletion-state table: how a delete holds the row
// at the final read, and whether the start must answer 409.
var finalReadDeletes = []struct {
	name      string
	apply     func(t *testing.T, s store.Store, id string)
	deleteWon bool
}{
	{"none", func(*testing.T, store.Store, string) {}, false},
	{"deleting-live", func(t *testing.T, s store.Store, id string) {
		claimForTest(t, s, id, store.DeletionStateDeleting, time.Minute)
	}, true},
	{"finalizing-live", func(t *testing.T, s store.Store, id string) {
		claimForTest(t, s, id, store.DeletionStateFinalizing, time.Minute)
	}, true},
	{"finalizing-expired", func(t *testing.T, s store.Store, id string) {
		claimForTest(t, s, id, store.DeletionStateFinalizing, -time.Minute)
	}, true},
	{"hard-deleted", func(t *testing.T, s store.Store, id string) {
		require.NoError(t, s.DeleteAgent(context.Background(), id))
	}, true},
	{"soft-deleted", func(t *testing.T, s store.Store, id string) {
		a, err := s.GetAgent(context.Background(), id)
		require.NoError(t, err)
		a.DeletedAt = time.Now()
		require.NoError(t, s.UpdateAgent(context.Background(), a))
	}, true},
	{"delete-failed", func(t *testing.T, s store.Store, id string) {
		claimForTest(t, s, id, store.DeletionStateFailed, time.Minute)
	}, false},
	{"deleting-lease-lapsed", func(t *testing.T, s store.Store, id string) {
		claimForTest(t, s, id, store.DeletionStateDeleting, -time.Minute)
	}, false},
}

// finalReadDeleteStore is set as srv.store (a dispatcher keeps the raw
// store). It applies the delete once, on the raw store, at the start of
// the first GetAgent made from inside readFrom (and, when set, also from
// inside within), before that read reaches the store: the delete lands
// just before the handler's final read.
type finalReadDeleteStore struct {
	store.Store
	readFrom string
	within   string
	apply    func()
	pub      *deleteRecordingPublisher
	applied  atomic.Bool
	// eventsAtApply is how many events pub had recorded when the delete
	// was applied.
	eventsAtApply atomic.Int64
}

func (p *finalReadDeleteStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if calledFrom(p.readFrom) && (p.within == "" || calledFrom(p.within)) && p.applied.CompareAndSwap(false, true) {
		p.eventsAtApply.Store(int64(len(p.pub.snapshot())))
		p.apply()
	}
	return p.Store.GetAgent(ctx, id)
}

// statusEventsAfterApply returns the status events published after the
// delete was applied.
func (p *finalReadDeleteStore) statusEventsAfterApply() []recordedAgentEvent {
	var out []recordedAgentEvent
	for _, e := range p.pub.snapshot()[p.eventsAtApply.Load():] {
		if e.kind == "status" {
			out = append(out, e)
		}
	}
	return out
}

// recordAgentEvents sets a recording publisher as srv.events.
func recordAgentEvents(t *testing.T, srv *Server) *deleteRecordingPublisher {
	t.Helper()
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	pub := newDeleteRecordingPublisher(bus)
	srv.events = pub
	return pub
}

// requireDeleteWonAnswer asserts the 409 delete_in_progress answer of a
// start that lost to a delete: the code, details.agentId, the message, and
// no agent body.
func requireDeleteWonAnswer(t *testing.T, rec *httptest.ResponseRecorder, agentID string) {
	t.Helper()
	requireIntentDeleteInProgress(t, rec, agentID)
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, deletedWhileStartingMessage, body.Error.Message)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "id", "no agent body")
	assert.NotContains(t, raw, "agent", "no agent body")
	assert.NotContains(t, raw, "phase", "no agent body")
}

// newManagedStartServer returns a server and a stopped managed-runtime
// agent (no runtime broker: the managed path acts in the hub).
func newManagedStartServer(t *testing.T) (*Server, store.Store, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	srv, s := testServer(t)
	project := &store.Project{
		ID:   tid("managed-final-project-" + t.Name()),
		Name: "managed-final-project",
		Slug: "managed-final-project-" + tidSlugSafe(t.Name()),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID:            tid("managed-final-agent-" + t.Name()),
		Name:          "managed-final-agent",
		Slug:          "managed-final-agent",
		ProjectID:     project.ID,
		OwnerID:       tid("managed-final-user"),
		Runtime:       ManagedRuntimePrefix + "stub",
		Phase:         string(state.PhaseStopped),
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	prev := managedBackendInst
	managedBackendInst = stubManagedAgentBackend{}
	t.Cleanup(func() { managedBackendInst = prev })
	return srv, s, agent
}

// ptone/scion#3705: a managed-runtime start or restart whose row a delete
// claims just before the final read (settleLifecycleWrite's reload) answers
// 409 delete_in_progress with no agent body and publishes nothing; a live
// row answers 200 with the agent, as before.
func TestManagedStart_DeleteWonAtFinalRead(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		for _, del := range finalReadDeletes {
			t.Run(action+"/"+del.name, func(t *testing.T) {
				srv, s, agent := newManagedStartServer(t)
				p := &finalReadDeleteStore{
					Store:    s,
					readFrom: ".(*Server).reloadGuardedColumns",
					within:   ".(*Server).handleManagedAgentLifecycle",
					apply:    func() { del.apply(t, s, agent.ID) },
					pub:      recordAgentEvents(t, srv),
				}
				srv.store = p

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
				require.True(t, p.applied.Load(), "the final read ran, and the delete was applied just before it: %s", rec.Body.String())

				if !del.deleteWon {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					var resp store.Agent
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
					assert.Equal(t, agent.ID, resp.ID)
					assert.Equal(t, string(state.PhaseRunning), resp.Phase)
					assert.Len(t, p.statusEventsAfterApply(), 1, "a live start publishes its status once")
					return
				}
				requireDeleteWonAnswer(t, rec, agent.ID)
				assert.Empty(t, p.statusEventsAfterApply(), "no status publish after a delete won")
			})
		}
	}
}

// deleteBeforeStartWriteStore hard-deletes the row just before the managed
// path's final status write (the running write of a start or restart).
type deleteBeforeStartWriteStore struct {
	store.Store
	pub           *deleteRecordingPublisher
	applied       atomic.Bool
	eventsAtApply atomic.Int64
}

func (p *deleteBeforeStartWriteStore) UpdateAgentStatus(ctx context.Context, id string, u store.AgentStatusUpdate) error {
	if u.Phase == string(state.PhaseRunning) && p.applied.CompareAndSwap(false, true) {
		p.eventsAtApply.Store(int64(len(p.pub.snapshot())))
		if err := p.DeleteAgent(ctx, id); err != nil {
			return err
		}
	}
	return p.Store.UpdateAgentStatus(ctx, id, u)
}

// ptone/scion#3705: a managed-runtime start or restart whose row is
// hard-deleted before its final write answers 409 delete_in_progress, not
// 404.
func TestManagedStart_HardDeleteBeforeFinalWrite_Answers409(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		t.Run(action, func(t *testing.T) {
			srv, s, agent := newManagedStartServer(t)
			p := &deleteBeforeStartWriteStore{Store: s, pub: recordAgentEvents(t, srv)}
			srv.store = p

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.True(t, p.applied.Load(), "the row was deleted before the final write")
			requireDeleteWonAnswer(t, rec, agent.ID)
			for _, e := range p.pub.snapshot()[p.eventsAtApply.Load():] {
				assert.NotEqual(t, "status", e.kind, "no status publish after a delete won")
			}
		})
	}
}

// ptone/scion#3705: a managed-runtime start or restart whose final reload
// fails with an error that is not "row gone" cannot tell whether a delete
// won: it answers 200 from the requested phase, as the lifecycle start does
// (TestLifecycle_SettleReloadError_Answers200).
func TestManagedStart_SettleReloadError_Answers200(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		t.Run(action, func(t *testing.T) {
			srv, s, agent := newManagedStartServer(t)
			p := &failReloadStore{Store: s}
			srv.store = p

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.True(t, p.failedReload.Load(), "the settle reload ran and failed")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp store.Agent
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, agent.ID, resp.ID)
			assert.Equal(t, string(state.PhaseRunning), resp.Phase)
		})
	}
}

// ptone/scion#3705: a managed-runtime start whose final write fails with an
// error that is not a delete answers a server error, not delete_in_progress.
func TestManagedStart_FinalWriteError_NotDeleteWon(t *testing.T) {
	srv, s, agent := newManagedStartServer(t)
	p := &failRunningWriteStore{Store: s}
	srv.store = p

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+api.AgentActionStart, nil)
	require.True(t, p.failed.Load(), "the final write ran and failed")
	require.GreaterOrEqual(t, rec.Code, http.StatusInternalServerError, rec.Body.String())
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotEqual(t, ErrCodeDeleteInProgress, body.Error.Code)
}

// failRunningWriteStore fails the running status write with a database
// error that is not a delete.
type failRunningWriteStore struct {
	store.Store
	failed atomic.Bool
}

func (p *failRunningWriteStore) UpdateAgentStatus(ctx context.Context, id string, u store.AgentStatusUpdate) error {
	if u.Phase == string(state.PhaseRunning) {
		p.failed.Store(true)
		return errors.New("db unavailable")
	}
	return p.Store.UpdateAgentStatus(ctx, id, u)
}

// ptone/scion#3711: each branch of POST /agents that starts an existing
// agent (handleExistingAgent) answers 409 delete_in_progress with no agent
// body, publishes nothing and subscribes nothing, when a delete claims the
// row after the post-start step, just before the final read
// (existingAgentDeleteWonBeforeAnswer); a live row answers 200 with the
// agent, as before.
func TestCreateExisting_DeleteWonAtFinalRead(t *testing.T) {
	branches := []struct {
		name  string
		phase state.Phase
		body  map[string]interface{}
	}{
		{"resume-suspended", state.PhaseSuspended, nil},
		{"resume-stopped", state.PhaseStopped, map[string]interface{}{"resume": true}},
		{"start-created", state.PhaseCreated, nil},
	}
	for _, br := range branches {
		for _, del := range finalReadDeletes {
			t.Run(br.name+"/"+del.name, func(t *testing.T) {
				f := handleExistingAgentAuthzSetup(t)
				agent := f.agent(t, "cx-final", string(br.phase))
				client := &landingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, reportRunID: true}
				f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default()))
				p := &finalReadDeleteStore{
					Store:    f.store,
					readFrom: ".(*Server).existingAgentDeleteWonBeforeAnswer",
					apply:    func() { del.apply(t, f.store, agent.ID) },
					pub:      recordAgentEvents(t, f.srv),
				}
				f.srv.store = p

				req := map[string]interface{}{"name": agent.Slug, "projectId": f.project.ID, "notify": true}
				for k, v := range br.body {
					req[k] = v
				}
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", req)
				require.NotEmpty(t, client.lastStartExtras.RunID, "the start reached the broker: %s", rec.Body.String())
				assert.Empty(t, client.deleteRuns, "the start landed while the agent was live: no compensating delete")
				require.True(t, p.applied.Load(), "the final read ran, and the delete was applied just before it: %s", rec.Body.String())
				subs, err := f.store.GetNotificationSubscriptions(context.Background(), agent.ID)
				require.NoError(t, err)

				if !del.deleteWon {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					var resp CreateAgentResponse
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
					require.NotNil(t, resp.Agent)
					assert.Equal(t, agent.ID, resp.Agent.ID)
					assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase)
					assert.NotEmpty(t, subs, "notify subscribes on a successful start")
					return
				}
				requireDeleteWonAnswer(t, rec, agent.ID)
				assert.Empty(t, p.statusEventsAfterApply(), "no status publish after a delete won")
				assert.Empty(t, subs, "no notify subscription after a delete won")
			})
		}
	}
}

// claimAfterPostStartWriteStore claims the row for a delete right after the
// post-start step's agent update of handleExistingAgent landed (after that
// step's own re-read), independently of where the handler reads the row
// next.
type claimAfterPostStartWriteStore struct {
	store.Store
	apply   func()
	applied atomic.Bool
}

func (p *claimAfterPostStartWriteStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	err := p.Store.UpdateAgent(ctx, a)
	if err == nil && a.Phase == string(state.PhaseRunning) && calledFrom(".(*Server).handleExistingAgent") &&
		p.applied.CompareAndSwap(false, true) {
		p.apply()
	}
	return err
}

// ptone/scion#3711: a delete that claims the row right after the post-start
// write, while the start's claim is still held, is caught by the final read
// before the answer: 409 delete_in_progress, no agent body, no subscription.
func TestCreateExisting_DeleteClaimAfterPostStartWrite_Answers409(t *testing.T) {
	branches := []struct {
		name  string
		phase state.Phase
		body  map[string]interface{}
	}{
		{"resume-suspended", state.PhaseSuspended, nil},
		{"resume-stopped", state.PhaseStopped, map[string]interface{}{"resume": true}},
		{"start-created", state.PhaseCreated, nil},
	}
	for _, br := range branches {
		t.Run(br.name, func(t *testing.T) {
			f := handleExistingAgentAuthzSetup(t)
			agent := f.agent(t, "cx-poststart", string(br.phase))
			client := &landingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, reportRunID: true}
			f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default()))
			p := &claimAfterPostStartWriteStore{Store: f.store, apply: func() {
				claimForTest(t, f.store, agent.ID, store.DeletionStateDeleting, time.Minute)
			}}
			f.srv.store = p

			req := map[string]interface{}{"name": agent.Slug, "projectId": f.project.ID, "notify": true}
			for k, v := range br.body {
				req[k] = v
			}
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", req)
			require.NotEmpty(t, client.lastStartExtras.RunID, "the start reached the broker: %s", rec.Body.String())
			require.True(t, p.applied.Load(), "the delete claimed the row after the post-start write")
			requireDeleteWonAnswer(t, rec, agent.ID)
			subs, err := f.store.GetNotificationSubscriptions(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Empty(t, subs, "no notify subscription after a delete won")
		})
	}
}
