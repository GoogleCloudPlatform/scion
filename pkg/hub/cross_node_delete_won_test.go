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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A deferred (cross-node) start, restart or create runs its broker dispatch
// on the owner node. When a delete wins after that dispatch landed, the
// owner's compensating delete (compensateLandedRun) reports its outcome as a
// dispatch warning; the completed broker_dispatch row now carries those
// warnings back, so the requester's 409 delete_in_progress has them in
// details.warnings (ptone/scion#3456). The row also tells the requester the
// delete won, so a start or restart stops waiting for a running phase that
// will not come.

const landedRunRemoveFailedWarning = "agent was deleted while it was starting; removing its container failed: broker unreachable"

// landingOwnerDispatcher is the owner node's dispatcher: each start,
// restart, create and finalize succeeds after running onLand with the
// owner's dispatch context, as the real dispatcher runs settleLandedRun.
type landingOwnerDispatcher struct {
	ownerErrDispatcher
	onLand func(ctx context.Context, a *store.Agent)
	bus    *ChannelEventPublisher
}

// DispatchAgentStop publishes the stopped status a broker report would, so
// a restart's deferred stop leg ends at once.
func (d *landingOwnerDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	_ = d.ownerErrDispatcher.DispatchAgentStop(ctx, a)
	stopped := *a
	stopped.Phase = string(state.PhaseStopped)
	d.bus.PublishAgentStatus(context.Background(), &stopped)
	return nil
}

func (d *landingOwnerDispatcher) land(ctx context.Context, a *store.Agent) {
	if d.onLand != nil {
		d.onLand(ctx, a)
	}
}

func (d *landingOwnerDispatcher) DispatchAgentStart(ctx context.Context, a *store.Agent, task string, resume bool) error {
	_ = d.ownerErrDispatcher.DispatchAgentStart(ctx, a, task, resume)
	d.land(ctx, a)
	return nil
}

func (d *landingOwnerDispatcher) DispatchAgentRestart(ctx context.Context, a *store.Agent) error {
	_ = d.ownerErrDispatcher.DispatchAgentRestart(ctx, a)
	d.land(ctx, a)
	return nil
}

func (d *landingOwnerDispatcher) DispatchAgentCreateWithGather(ctx context.Context, a *store.Agent) (*CreateDispatchResult, error) {
	_, _ = d.ownerErrDispatcher.DispatchAgentCreateWithGather(ctx, a)
	d.land(ctx, a)
	return nil, nil
}

func (d *landingOwnerDispatcher) DispatchFinalizeEnv(ctx context.Context, a *store.Agent, env map[string]string) (*CreateDispatchResult, error) {
	_, _ = d.ownerErrDispatcher.DispatchFinalizeEnv(ctx, a, env)
	d.land(ctx, a)
	return nil, nil
}

// newCrossNodeLandingServer makes srv the requesting node of a two-node
// hub over s (as crossNodeHandlerServerWithOwner does), with a
// landingOwnerDispatcher on the owner node.
func newCrossNodeLandingServer(t *testing.T, srv *Server, s store.Store) (*landingOwnerDispatcher, *ChannelEventPublisher) {
	t.Helper()
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	srv.events = bus

	owner := &Server{
		store:             s,
		instanceID:        "hub-owner-" + uuid.NewString()[:8],
		agentLifecycleLog: slog.Default(),
		events:            bus,
	}
	ownerDisp := &landingOwnerDispatcher{bus: bus}
	owner.SetDispatcher(ownerDisp)
	owner.execDispatch = owner.executeDispatch
	owner.deliverMsg = owner.deliverMessage

	requester := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{returnErr: ErrLifecycleDeferred}, false, slog.Default())
	requester.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
	requester.SetCrossNodeDeps(bus, ownerSignalBus{owner: owner})
	srv.SetDispatcher(requester)
	return ownerDisp, bus
}

// deleteWonOnOwner returns an onLand hook that applies del and, when the
// delete won, reports warning as the owner's compensateLandedRun would. A
// live agent gets the running status a broker report would publish.
func deleteWonOnOwner(t *testing.T, s store.Store, bus *ChannelEventPublisher, apply func(*testing.T, store.Store, string), won bool, warning string) func(context.Context, *store.Agent) {
	return func(ctx context.Context, a *store.Agent) {
		apply(t, s, a.ID)
		if won {
			addDispatchWarnings(ctx, warning)
			return
		}
		running := *a
		running.Phase = string(state.PhaseRunning)
		bus.PublishAgentStatus(context.Background(), &running)
	}
}

func TestCrossNodeLifecycle_DeleteWonAfterLanding_CarriesOwnerWarnings(t *testing.T) {
	// A requester that misses the owner's DeleteWon waits out the rolling
	// window and answers 502: keep it short so that fails fast.
	setLifecycleTimings(t, 3*time.Second, 50*time.Millisecond, 0)
	actions := []struct {
		action string
		phase  state.Phase
	}{
		{api.AgentActionStart, state.PhaseStopped},
		{api.AgentActionRestart, state.PhaseRunning},
	}
	warnings := []string{landedRunRemovedWarning, landedRunRemoveFailedWarning}
	for _, act := range actions {
		for i, del := range landingDeletes {
			t.Run(act.action+"/"+del.name, func(t *testing.T) {
				srv, s := testServer(t)
				ownerDisp, bus := newCrossNodeLandingServer(t, srv, s)
				agent := setupBrokerAgentInPhase(t, s, "xnl-"+act.action+"-"+del.name, act.phase)
				warning := warnings[i%len(warnings)]
				ownerDisp.onLand = deleteWonOnOwner(t, s, bus, del.apply, del.compensate, warning)

				began := time.Now()
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+act.action, nil)
				if !del.compensate {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					return
				}
				require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
				code, details := errorBody(t, rec)
				assert.Equal(t, ErrCodeDeleteInProgress, code)
				assert.Equal(t, agent.ID, details["agentId"])
				assert.Equal(t, []interface{}{warning}, details["warnings"],
					"the owner's compensation outcome reaches the requester's 409")
				assert.Less(t, time.Since(began), 2500*time.Millisecond,
					"the done row ends the wait; no rolling-window timeout")
			})
		}
	}
}

// A deferred create (createAgent dispatched to the owner node) that loses
// to a delete answers 409 with the owner's warnings.
func TestCrossNodeCreate_DeleteWonAfterLanding_CarriesOwnerWarnings(t *testing.T) {
	for _, del := range landingDeletes {
		t.Run(del.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
			ownerDisp, bus := newCrossNodeLandingServer(t, srv, s)
			ownerDisp.onLand = deleteWonOnOwner(t, s, bus, del.apply, del.compensate, landedRunRemovedWarning)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
				"name": "xnode-create-" + del.name, "projectId": project.ID, "task": "do it",
			})
			if !del.compensate {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				var resp CreateAgentResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				require.NotNil(t, resp.Agent)
				assert.NotContains(t, resp.Warnings, landedRunRemovedWarning)
				return
			}
			var body ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
			agentID, _ := body.Error.Details["agentId"].(string)
			require.NotEmpty(t, agentID, rec.Body.String())
			warnings := requireDeletedDuringCreate(t, rec, agentID)
			assert.Equal(t, []string{landedRunRemovedWarning}, warnings,
				"the owner's compensation outcome reaches the requester's 409")
		})
	}
}

// A deferred env submit (finalize_env on the owner node) carries the
// owner's warnings into the 409 the same way.
func TestCrossNodeSubmitEnv_DeleteWonAfterLanding_CarriesOwnerWarnings(t *testing.T) {
	srv, s := testServer(t)
	ownerDisp, bus := newCrossNodeLandingServer(t, srv, s)
	agent := setupBrokerAgentInPhase(t, s, "xnl-env", state.PhaseProvisioning)
	ownerDisp.onLand = deleteWonOnOwner(t, s, bus, func(t *testing.T, s store.Store, id string) {
		claimForTest(t, s, id, store.DeletionStateDeleting, time.Minute)
	}, true, landedRunRemovedWarning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+agent.ProjectID+"/agents/"+agent.Slug+"/env",
		map[string]interface{}{"env": map[string]string{"A_KEY": "v"}})
	warnings := requireDeletedDuringCreate(t, rec, agent.ID)
	assert.Equal(t, []string{landedRunRemovedWarning}, warnings)
}

// The owner's result for a completed start or restart: empty when there is
// nothing to carry (as before), else the warnings and DeleteWon.
func TestLifecycleDispatchResult_RoundTrip(t *testing.T) {
	assert.Empty(t, marshalLifecycleResult(LifecycleDispatchResult{}))
	in := LifecycleDispatchResult{Warnings: []string{"w1"}, DeleteWon: true}
	assert.Equal(t, in, decodeLifecycleResult(marshalLifecycleResult(in)))
	assert.Equal(t, LifecycleDispatchResult{}, decodeLifecycleResult(""))
	assert.Equal(t, LifecycleDispatchResult{}, decodeLifecycleResult(stopSupersededResult), "other ops' results decode to nothing")
	assert.Equal(t, LifecycleDispatchResult{}, decodeLifecycleResult("not json"))
}

// The wait for a start's success phase ends with nil once the row is done
// and reports DeleteWon; a done row without it keeps waiting (see
// TestCrossNodeStart_DoneWithNonTerminalRowKeepsWaiting for in_progress).
func TestWaitForLifecycleOutcome_DoneDeleteWonEndsWait(t *testing.T) {
	setLifecycleTimings(t, 2*time.Second, 50*time.Millisecond, 0)
	f := newCrossNodeFixture(t, nil, false)
	go func() {
		row, ok := f.claimPending(t)
		if !ok {
			return
		}
		res := marshalLifecycleResult(LifecycleDispatchResult{DeleteWon: true, Warnings: []string{landedRunRemovedWarning}})
		if !assert.NoError(t, f.store.CompleteBrokerDispatch(context.Background(), row.ID, res)) {
			return
		}
		f.events.PublishDispatchDone(context.Background(), row.ID)
	}()
	ctx, warns := withDispatchWarnings(crossNodeCtx(t))
	began := time.Now()
	require.NoError(t, f.requester.DispatchAgentStart(ctx, f.agent, "", false))
	assert.Less(t, time.Since(began), 1500*time.Millisecond)
	assert.Equal(t, []string{landedRunRemovedWarning}, warns.Warnings(), "the owner's warnings are added to the requester's collector")
}
