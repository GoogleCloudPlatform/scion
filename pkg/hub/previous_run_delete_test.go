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
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A delete claimed after a start's beginRun names the new run. When that
// start never lands, the agent's previous runtime entry still carries the
// previous run, so the delete must name that run too (ptone/scion#3097).

// startRunBroker is a runBroker whose start and restart run onStart. A
// start that lands replaces the agent's entry (the broker's Start removes
// the previous entry by name), see landStart.
type startRunBroker struct {
	*runBroker
	onStart func(runID string) (*RemoteAgentResponse, error)
}

func (b *startRunBroker) StartAgent(_ context.Context, _, _, _, _, _, _, _, _, _, _ string, _ map[string]string, _ []ResolvedSecret, _ *api.ScionConfig, _ []api.SharedDir, _, _ bool, extras StartExtras) (*RemoteAgentResponse, error) {
	return b.onStart(extras.RunID)
}

func (b *startRunBroker) RestartAgent(_ context.Context, _, _, _, _ string, _ map[string]string, extras StartExtras) (*RemoteAgentResponse, error) {
	return b.onStart(extras.RunID)
}

// landStart replaces the agent's entry with one for runID, as a start
// that lands does, and returns the broker's answer.
func (b *startRunBroker) landStart(slug, runID string) *RemoteAgentResponse {
	b.mu.Lock()
	b.entries = map[string]bool{runID: true}
	b.mu.Unlock()
	return &RemoteAgentResponse{Agent: &RemoteAgentInfo{
		ID: "container-" + slug, Slug: slug, Name: slug,
		Phase: string(state.PhaseRunning), ContainerStatus: "Up 1 second", RunID: runID,
	}}
}

func (b *startRunBroker) deleted() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.deletes...)
}

type prevRunFixture struct {
	srv    *Server
	store  store.Store
	broker *startRunBroker
	agent  *store.Agent
	// firstRun is the run the agent's create landed with.
	firstRun string
}

// newPrevRunFixture creates an agent through the hub whose create lands
// with its own run, then leaves the agent in phase.
func newPrevRunFixture(t *testing.T, name string, phase state.Phase) *prevRunFixture {
	t.Helper()
	srv, s, project, client := newRaceSyncCreateServer(t)
	b := &startRunBroker{runBroker: &runBroker{raceAsyncClient: client, entries: map[string]bool{}}}
	d := NewHTTPAgentDispatcherWithClient(s, b, false, srv.agentLifecycleLog)
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return AsyncLaunchSettings{} })
	srv.SetDispatcher(d)

	var sent *RemoteCreateAgentRequest
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		sent = req
		b.land(req.RunID)
		return syncRunningAnswer(req), nil, nil
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": name, "projectId": project.ID, "task": "do it",
	})
	require.NotNil(t, sent, "create dispatched: %d %s", rec.Code, rec.Body.String())
	require.Less(t, rec.Code, 300, rec.Body.String())

	a := mustGetAgent(t, s, sent.ID)
	require.Equal(t, sent.RunID, a.RunID, "the create's run is recorded")
	require.Empty(t, a.PreviousRunIDs, "a first create has no previous run")
	a.Phase = string(phase)
	require.NoError(t, s.UpdateAgent(context.Background(), a))
	return &prevRunFixture{srv: srv, store: s, broker: b, agent: a, firstRun: sent.RunID}
}

// prevRunOps are the dispatches that replace an existing runtime entry.
var prevRunOps = []struct {
	name  string
	phase state.Phase
	path  string
}{
	{"start", state.PhaseStopped, "/start"},
	{"restart", state.PhaseRunning, "/restart"},
}

// prevRunFailures are ways the start can fail without landing.
var prevRunFailures = []struct {
	name string
	err  func(t *testing.T, runID string) error
}{
	// A transport failure after the request was sent: the row keeps the
	// minted run.
	{"unconfirmed", func(*testing.T, string) error { return errors.New("connection reset by peer") }},
	// A failure from inside the broker's Start with no current run
	// reported (an older broker, or a failed re-list): the row keeps the
	// minted run.
	{"start-attempted", func(t *testing.T, runID string) error {
		return brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttempted(runID))
	}},
	// The request was never sent: the start reverts the row to the
	// previous run, but only after the delete took its snapshot.
	{"not-sent", func(*testing.T, string) error { return errStartRequestNotSent }},
}

// Reproduction (ptone/scion#3097): the delete is claimed while the start
// is at the broker, after beginRun recorded the new run, and the start
// never lands. The delete must remove the previous run's entry, which is
// the only one there is.
func TestPreviousRunDelete_StartNeverLands_DeletesPreviousRun(t *testing.T) {
	for _, op := range prevRunOps {
		for _, fail := range prevRunFailures {
			t.Run(op.name+"/"+fail.name, func(t *testing.T) {
				f := newPrevRunFixture(t, "prev-"+op.name+"-"+fail.name, op.phase)
				var minted string
				f.broker.onStart = func(runID string) (*RemoteAgentResponse, error) {
					minted = runID
					atStart := mustGetAgent(t, f.store, f.agent.ID)
					require.Equal(t, runID, atStart.RunID, "beginRun recorded the new run")
					require.Equal(t, []string{f.firstRun}, atStart.PreviousRunIDs, "and kept the previous one")
					rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
					require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
					return nil, fail.err(t, runID)
				}

				rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+op.path, nil)
				require.NotEmpty(t, minted, "the start reached the broker: %d %s", rec.Code, rec.Body.String())
				require.True(t, agentGone(t, f.store, f.agent.ID), "the delete removed the row")

				assert.False(t, f.broker.has(f.firstRun),
					"the previous run's entry is left behind with nothing tracking it (broker deletes: %v)", f.broker.deleted())
				assert.Zero(t, f.broker.live())
				assert.Equal(t, []string{minted, f.firstRun}, f.broker.deleted(),
					"the current run, then the previous run, each run-scoped")
			})
		}
	}
}

// Control: the start lands normally, then a delete. The landing settles the
// run, so the delete names exactly the current run, once.
func TestPreviousRunDelete_StartLands_DeletesOnlyCurrentRun(t *testing.T) {
	for _, op := range prevRunOps {
		t.Run(op.name, func(t *testing.T) {
			f := newPrevRunFixture(t, "prevland-"+op.name, op.phase)
			var minted string
			f.broker.onStart = func(runID string) (*RemoteAgentResponse, error) {
				minted = runID
				return f.broker.landStart(f.agent.Slug, runID), nil
			}
			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+op.path, nil)
			require.Less(t, rec.Code, 300, rec.Body.String())
			got := mustGetAgent(t, f.store, f.agent.ID)
			require.Equal(t, minted, got.RunID)
			assert.Empty(t, got.PreviousRunIDs, "a landed start settles the run")

			rec = doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			assert.Equal(t, []string{minted}, f.broker.deleted(), "exactly one delete, for the current run")
			assert.Zero(t, f.broker.live())
		})
	}
}

// Control: no previous run (the first create). The delete is unchanged:
// one delete, for the create's run.
func TestPreviousRunDelete_NoPreviousRun_Unchanged(t *testing.T) {
	f := newPrevRunFixture(t, "prevnone", state.PhaseRunning)
	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, []string{f.firstRun}, f.broker.deleted())
	assert.Zero(t, f.broker.live())
}

// A same-name successor's entry, labelled with its own run, survives the
// previous-run delete: that delete is scoped to the previous run, never by
// name.
func TestPreviousRunDelete_SparesSameNameSuccessor(t *testing.T) {
	f := newPrevRunFixture(t, "prevsucc", state.PhaseStopped)
	const successorRun = "successor-run"
	f.broker.onStart = func(runID string) (*RemoteAgentResponse, error) {
		rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		return nil, errors.New("connection reset by peer")
	}
	f.broker.land(successorRun)

	doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
	require.True(t, agentGone(t, f.store, f.agent.ID))
	assert.False(t, f.broker.has(f.firstRun), "the previous run is deleted")
	assert.True(t, f.broker.has(successorRun), "the successor's entry survives (broker deletes: %v)", f.broker.deleted())
	assert.NotContains(t, f.broker.deleted(), "", "no delete by name")
}

// Semantics (2): a start that was never sent reverts the row to the
// previous run. A revert keeps the list (here just the restored run, which
// a delete skips), so a later delete names the old run only.
func TestPreviousRunDelete_RevertedStart_LaterDeleteNamesOldRun(t *testing.T) {
	f := newPrevRunFixture(t, "prevrevert", state.PhaseStopped)
	f.broker.onStart = func(string) (*RemoteAgentResponse, error) { return nil, errStartRequestNotSent }
	doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
	got := mustGetAgent(t, f.store, f.agent.ID)
	require.Equal(t, f.firstRun, got.RunID, "the start reverted the run")
	assert.Equal(t, []string{f.firstRun}, got.PreviousRunIDs, "a revert keeps the list; it holds only the restored run")

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, []string{f.firstRun}, f.broker.deleted())
	assert.Zero(t, f.broker.live())
}

// A failed start whose broker reports the run its runtime now holds
// settles the row to that run: a later delete names it only.
func TestPreviousRunDelete_BrokerReportedRun_Settles(t *testing.T) {
	f := newPrevRunFixture(t, "prevreported", state.PhaseStopped)
	f.broker.onStart = func(runID string) (*RemoteAgentResponse, error) {
		return nil, brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttemptedAt(runID, f.firstRun))
	}
	doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
	got := mustGetAgent(t, f.store, f.agent.ID)
	require.Equal(t, f.firstRun, got.RunID)
	assert.Empty(t, got.PreviousRunIDs)

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, []string{f.firstRun}, f.broker.deleted())
}

// Semantics (3): two starts that each fail in doubt. Neither run settled,
// so both are kept as previous runs, and the delete names all three; the
// oldest entry, the only live one here, is removed.
func TestPreviousRunDelete_TwoUnsettledStarts_KeepsOldestRun(t *testing.T) {
	f := newPrevRunFixture(t, "prevtwo", state.PhaseStopped)
	var runs []string
	f.broker.onStart = func(runID string) (*RemoteAgentResponse, error) {
		runs = append(runs, runID)
		return nil, brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttempted(runID))
	}
	for i := 0; i < 2; i++ {
		doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
		a := mustGetAgent(t, f.store, f.agent.ID)
		if a.Phase != string(state.PhaseStopped) {
			a.Phase = string(state.PhaseStopped)
			require.NoError(t, f.store.UpdateAgent(context.Background(), a))
		}
	}
	require.Len(t, runs, 2)
	got := mustGetAgent(t, f.store, f.agent.ID)
	require.Equal(t, runs[1], got.RunID)
	assert.Equal(t, []string{f.firstRun, runs[0]}, got.PreviousRunIDs, "oldest first")

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, []string{runs[1], runs[0], f.firstRun}, f.broker.deleted(), "current first, then newest to oldest")
	assert.False(t, f.broker.has(f.firstRun))
}

// A previous run's delete that fails (not a 404) fails the delete through
// the engine's ordinary handling: the row stays, with the failed banner,
// and a retry deletes both runs again (the current one is then a 404).
func TestPreviousRunDelete_PreviousRunDeleteFails_FailsDelete(t *testing.T) {
	f := newPrevRunFixture(t, "prevfail", state.PhaseStopped)
	var calls int
	f.broker.setDeleteFn(func(context.Context) error {
		calls++
		if calls == 2 {
			return &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: "boom"}
		}
		return nil
	})
	var minted string
	f.broker.onStart = func(runID string) (*RemoteAgentResponse, error) {
		minted = runID
		rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
		require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
		code, _ := errorBody(t, rec)
		assert.NotEmpty(t, code)
		return nil, errors.New("connection reset by peer")
	}
	doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
	require.False(t, agentGone(t, f.store, f.agent.ID), "the row stays")
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
	assert.Equal(t, store.DeletionCodeRuntimeError, got.DeletionCode)
	assert.Equal(t, []string{minted, f.firstRun}, f.broker.deleted())
	// runBroker drops an entry before its delete hook fails; the broker's
	// failure left the previous run's entry in place.
	f.broker.land(f.firstRun)

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, f.broker.has(f.firstRun))
	assert.Equal(t, []string{minted, f.firstRun, minted, f.firstRun}, f.broker.deleted())
}

// A delete handed to the owning node carries the previous runs from the
// requesting node's snapshot, and the owner's dispatch names them.
func TestPreviousRunDelete_DeferredCarriesPreviousRuns(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 100 * time.Millisecond })
	df := newDeferredDeleteFixture(t, "prevdefer", nil)
	ctx := context.Background()
	_, err := df.store.SetAgentRunID(ctx, df.agent.ID, "run-1")
	require.NoError(t, err)
	_, err = df.store.SetAgentRunID(ctx, df.agent.ID, "run-2")
	require.NoError(t, err)

	requireInDoubt(t, df, df.del(t, ""))
	intents := df.pendingDeleteIntents(t)
	require.Len(t, intents, 1)
	args, err := UnmarshalDeleteArgs(intents[0].Args)
	require.NoError(t, err)
	assert.Equal(t, []string{"run-1"}, args.PreviousRunIDs, "the intent carries the snapshot's previous runs")

	// The owner executes such an intent: the current run, then the
	// previous runs the intent names.
	f := newPrevRunFixture(t, "prevowner", state.PhaseRunning)
	raw, err := MarshalDispatchArgs(&DeleteDispatchArgs{PreviousRunIDs: []string{"run-x"}})
	require.NoError(t, err)
	_, err = f.srv.execDispatchDelete(ctx, store.BrokerDispatch{AgentID: f.agent.ID, Args: raw})
	require.NoError(t, err)
	assert.Equal(t, []string{f.firstRun, "run-x"}, f.broker.deleted())
}

// stopAgent puts the row back in phase stopped, as a failed start may not.
func (f *prevRunFixture) stopAgent(t *testing.T) {
	t.Helper()
	a := mustGetAgent(t, f.store, f.agent.ID)
	if a.Phase != string(state.PhaseStopped) {
		a.Phase = string(state.PhaseStopped)
		require.NoError(t, f.store.UpdateAgent(context.Background(), a))
	}
}

// Review B1 regression: a start that fails in doubt, then a start the
// broker never acts on (never sent, or handed to another node) that is
// reverted. The revert restores the in-doubt run but must not forget the
// first run, whose entry is the live one: the delete still removes it.
func TestPreviousRunDelete_InDoubtThenRevertedStart_KeepsEarlierRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"not-sent", errStartRequestNotSent},
		// No cross-node deps are wired, so the hand-off fails right after
		// the revert.
		{"deferred", ErrLifecycleDeferred},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrevRunFixture(t, "prevb1-"+tc.name, state.PhaseStopped)
			var runs []string
			f.broker.onStart = func(runID string) (*RemoteAgentResponse, error) {
				runs = append(runs, runID)
				if len(runs) == 1 {
					return nil, errors.New("connection reset by peer")
				}
				return nil, tc.err
			}
			doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
			f.stopAgent(t)
			got := mustGetAgent(t, f.store, f.agent.ID)
			require.Equal(t, runs[0], got.RunID, "the in-doubt start keeps its run")
			require.Equal(t, []string{f.firstRun}, got.PreviousRunIDs)

			doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
			f.stopAgent(t)
			require.Len(t, runs, 2)
			got = mustGetAgent(t, f.store, f.agent.ID)
			require.Equal(t, runs[0], got.RunID, "the second start reverted")
			assert.Equal(t, []string{f.firstRun, runs[0]}, got.PreviousRunIDs, "the revert kept the first run")

			rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			assert.False(t, f.broker.has(f.firstRun), "the first run's entry is not orphaned (broker deletes: %v)", f.broker.deleted())
			assert.Equal(t, []string{runs[0], f.firstRun}, f.broker.deleted(), "the current run, then the first run; the restored run is not repeated")
		})
	}
}

// Review N3: a failed start whose broker reports the minted run as the one
// its runtime holds (the entry was created, then the start failed) settles
// that run: the previous runs are cleared, and a later delete names only
// the minted run.
func TestPreviousRunDelete_FailedStartReportsMintedRun_Settles(t *testing.T) {
	f := newPrevRunFixture(t, "prevreportedminted", state.PhaseStopped)
	var minted string
	f.broker.onStart = func(runID string) (*RemoteAgentResponse, error) {
		minted = runID
		f.broker.landStart(f.agent.Slug, runID)
		return nil, brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttemptedAt(runID, runID))
	}
	doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
	got := mustGetAgent(t, f.store, f.agent.ID)
	require.Equal(t, minted, got.RunID)
	assert.Empty(t, got.PreviousRunIDs, "the reported run settles the row")

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, []string{minted}, f.broker.deleted())
	assert.Zero(t, f.broker.live())
}

// deferOnSecondDelete answers the first broker delete itself and defers
// every later one to the owning node.
type deferOnSecondDelete struct {
	*mockRuntimeBrokerClient
	mu      sync.Mutex
	deletes []string
}

func (c *deferOnSecondDelete) DeleteAgent(_ context.Context, _, _, _, _ string, opts DeleteAgentOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes = append(c.deletes, opts.RunID)
	if len(c.deletes) == 1 {
		return nil
	}
	return ErrLifecycleDeferred
}

// Review N1: the current run's delete succeeds and the first previous
// run's delete is handed to the owning node. The intent names the runs not
// yet deleted, oldest first, including the one that was deferred, and the
// outcome is in doubt as for any deferred delete still outstanding.
func TestPreviousRunDelete_MidLoopDeferral_HandsOffRemainingRuns(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 100 * time.Millisecond })
	df := newDeferredDeleteFixture(t, "prevmidloop", nil)
	client := &deferOnSecondDelete{mockRuntimeBrokerClient: df.client}
	d := NewHTTPAgentDispatcherWithClient(df.store, client, false, slog.Default())
	d.SetCrossNodeDeps(df.bus, NoopCommandBus{})
	df.srv.SetDispatcher(d)
	ctx := context.Background()
	for _, r := range []string{"run-1", "run-2", "run-3"} {
		_, err := df.store.SetAgentRunID(ctx, df.agent.ID, r)
		require.NoError(t, err)
	}

	requireInDoubt(t, df, df.del(t, ""))
	assert.Equal(t, []string{"run-3", "run-2"}, client.deletes, "the current run, then the newest previous run, which was deferred")
	intents := df.pendingDeleteIntents(t)
	require.Len(t, intents, 1)
	args, err := UnmarshalDeleteArgs(intents[0].Args)
	require.NoError(t, err)
	assert.Equal(t, []string{"run-1", "run-2"}, args.PreviousRunIDs, "the runs not yet deleted, oldest first")
}

// Review N2: a compensating delete names only the run that landed, even
// when the dispatch's struct still lists previous runs (the settle swap
// missed the hard-deleted row, so it did not clear them).
func TestPreviousRunDelete_CompensationNamesOnlyLandedRun(t *testing.T) {
	f, c := newLandingFixture(t, "prevcomp")
	f.agent.PreviousRunIDs = []string{"older-run"}
	c.onLand = func() { require.NoError(t, f.store.DeleteAgent(context.Background(), f.agent.ID)) }
	require.NoError(t, f.dispatcher.DispatchAgentStart(context.Background(), f.agent, "", false))
	require.NotEmpty(t, f.agent.PreviousRunIDs, "the struct still lists previous runs")
	assert.Equal(t, []string{c.lastStartExtras.RunID}, c.deleteRuns, "only the landed run")
}

// Review nit 4: deletePreviousRuns names every listed run other than the
// current one, newest first, and nothing for a row with no run ID (its
// delete resolved by name, which covers every run).
func TestDeletePreviousRuns_Direct(t *testing.T) {
	f, c := newLandingFixture(t, "prevdirect")
	a := *f.agent
	a.RunID = "cur"
	a.PreviousRunIDs = []string{"a", "cur", "", "b"}
	require.NoError(t, f.dispatcher.deletePreviousRuns(context.Background(), &a, "http://broker", DeleteAgentOptions{DeleteFiles: true}))
	assert.Equal(t, []string{"b", "a"}, c.deleteRuns)

	c.deleteRuns = nil
	a.RunID = ""
	require.NoError(t, f.dispatcher.deletePreviousRuns(context.Background(), &a, "http://broker", DeleteAgentOptions{}))
	assert.Empty(t, c.deleteRuns)
}
