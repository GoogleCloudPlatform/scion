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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the heartbeat adoption of a stranded run (ptone/scion#3661).

const (
	logRunAdopted      = "agent_run_adopted_from_heartbeat"
	logRunUnrecognised = "agent_run_label_unrecognised"
	logRunEmpty        = "agent_run_label_empty"
)

// runAdoptionFixture is a reconcile fixture with a captured lifecycle log
// and a settable clock for the adoption.
type runAdoptionFixture struct {
	*reconcileFixture
	logs   *captureHandler
	offset time.Duration
}

func newRunAdoptionFixture(t *testing.T) *runAdoptionFixture {
	t.Helper()
	f := &runAdoptionFixture{reconcileFixture: newReconcileFixture(t), logs: &captureHandler{}}
	f.srv.agentLifecycleLog = slog.New(f.logs)
	f.srv.runAdoption.nowFor = func() time.Time { return time.Now().Add(f.offset) }
	return f
}

// afterQuiet moves the adoption clock past the quiet period plus one
// heartbeat interval.
func (f *runAdoptionFixture) afterQuiet() {
	f.offset = runAdoptionQuietPeriod() + 30*time.Second
}

// mint signs a token for run and records it with the run-ID write, as
// the dispatcher's beginRun does, and returns the token and credential.
func (f *runAdoptionFixture) mint(a *store.Agent, run string) (string, *store.AgentCredential) {
	f.t.Helper()
	tok, cred, err := f.srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID: a.ID, ProjectID: a.ProjectID, Scopes: ScopesForRole(AgentRoleFull),
	}, run)
	require.NoError(f.t, err)
	_, err = f.s.SetAgentRunID(context.Background(), a.ID, run, cred)
	require.NoError(f.t, err)
	return tok, cred
}

// setRun records run without a credential (a dispatch whose token was not
// recorded, or an execution whose broker call was never made).
func (f *runAdoptionFixture) setRun(a *store.Agent, run string) {
	f.t.Helper()
	_, err := f.s.SetAgentRunID(context.Background(), a.ID, run, nil)
	require.NoError(f.t, err)
}

// beat sends a complete heartbeat from brokerID, through srv, reporting
// slug with label and, when given, entries on other targets.
func (f *runAdoptionFixture) beatVia(srv *Server, brokerID, slug, label string, others ...brokerAgentRuntimeEntry) {
	f.t.Helper()
	inv := completeInventory("docker", "k8s")
	inv.RunLabels = true
	hb := brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: inv,
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{{
			Slug: slug, Phase: "running", Activity: "working", RuntimeTarget: "docker", RunID: label,
			OtherEntries: others,
		}}}},
	}
	f.sendVia(srv, brokerID, hb)
}

func (f *runAdoptionFixture) beat(brokerID, slug, label string, others ...brokerAgentRuntimeEntry) {
	f.t.Helper()
	f.beatVia(f.srv, brokerID, slug, label, others...)
}

func (f *runAdoptionFixture) sendVia(srv *Server, brokerID string, hb brokerHeartbeatRequest) {
	f.t.Helper()
	rec := doRequest(f.t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+brokerID+"/heartbeat", hb)
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
}

// addBroker adds a second online broker providing the fixture's project.
func (f *runAdoptionFixture) addBroker(name string) string {
	f.t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID: tid(name), Name: name, Slug: name, Status: store.BrokerStatusOnline,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(f.t, f.s.CreateRuntimeBroker(ctx, b))
	require.NoError(f.t, f.s.UpdateRuntimeBrokerHeartbeat(ctx, b.ID, store.BrokerStatusOnline))
	require.NoError(f.t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.projectID, BrokerID: b.ID, BrokerName: b.Name, Status: b.Status,
	}))
	return b.ID
}

// strand reproduces the overlap edge on a: execution 0 lands with run L
// and its token; two overlapping executions mint R1 and R2 and are both
// confirmed not acted on. Execution 1's revert (R1 -> L) misses because
// the row already holds R2; execution 2's revert (R2 -> R1) applies. The
// row ends on R1, which no container holds, while the container keeps L.
func (f *runAdoptionFixture) strand(a *store.Agent) (token string, cred *store.AgentCredential, l, r string) {
	f.t.Helper()
	ctx := context.Background()
	l, r1, r2 := "run-l-"+a.Slug, "run-r1-"+a.Slug, "run-r2-"+a.Slug
	token, cred = f.mint(a, l)
	f.mint(a, r1)
	f.mint(a, r2)
	ok, err := f.s.RevertAgentRunID(ctx, a.ID, r1, l)
	require.NoError(f.t, err)
	require.False(f.t, ok, "execution 1's revert misses")
	ok, err = f.s.RevertAgentRunID(ctx, a.ID, r2, r1)
	require.NoError(f.t, err)
	require.True(f.t, ok, "execution 2's revert applies")
	require.Equal(f.t, r1, f.get(a.ID).RunID)
	return token, cred, l, r1
}

// runScopeOutcome is the run-scope check's outcome for token.
func (f *runAdoptionFixture) runScopeOutcome(token string, cred *store.AgentCredential) string {
	f.t.Helper()
	ctx := context.Background()
	claims, err := f.srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(f.t, err)
	row, err := f.s.GetAgentCredentialByJTIHash(ctx, cred.TokenJTIHash)
	require.NoError(f.t, err)
	c := testRunScopeChecker(agentRunScopeObserve, time.Time{}, f.s)
	return c.outcomeName(ctx, claims, agentTokenCredentialState{evaluated: true, cred: row}, "")
}

// TestRunAdoption_StrandedRowAdopted: the overlap edge leaves the row on a
// run no container holds, so the token issued at dispatch no longer
// matches. Within the quiet period the heartbeat changes nothing; one
// heartbeat interval past it, the row is adopted to the container's run,
// logged once, run_id_updated_at moves, and the token issued at dispatch
// matches again.
func TestRunAdoption_StrandedRowAdopted(t *testing.T) {
	f := newRunAdoptionFixture(t)
	a := f.addAgent("stranded", "running", "working")
	token, cred, l, r := f.strand(a)
	assert.Equal(t, runScopeOutcomeSuperseded, f.runScopeOutcome(token, cred))
	before := f.get(a.ID).RunIDUpdatedAt
	require.NotNil(t, before)

	f.beat(f.brokerID, a.Slug, l)
	assert.Equal(t, r, f.get(a.ID).RunID, "within the quiet period nothing is adopted")
	assert.Empty(t, f.logs.records(logRunAdopted))

	f.afterQuiet()
	f.beat(f.brokerID, a.Slug, l)
	got := f.get(a.ID)
	assert.Equal(t, l, got.RunID)
	assert.Empty(t, got.PreviousRunIDs)
	require.NotNil(t, got.RunIDUpdatedAt)
	assert.True(t, got.RunIDUpdatedAt.After(*before), "run_id_updated_at moves with the adoption")
	recs := f.logs.records(logRunAdopted)
	require.Len(t, recs, 1)
	assert.Equal(t, a.ID, recs[0].attrs["agent_id"])
	assert.Equal(t, r, recs[0].attrs["previous_run_id"])
	assert.Equal(t, l, recs[0].attrs["run_id"])
	assert.Equal(t, runScopeOutcomeBound, f.runScopeOutcome(token, cred))

	// The row is current now: further heartbeats change nothing.
	updated := *got.RunIDUpdatedAt
	f.beat(f.brokerID, a.Slug, l)
	assert.Len(t, f.logs.records(logRunAdopted), 1)
	assert.True(t, f.get(a.ID).RunIDUpdatedAt.Equal(updated))
}

// TestRunAdoption_NotAdopted covers each case in which the heartbeat
// leaves a mismatched row alone.
func TestRunAdoption_NotAdopted(t *testing.T) {
	t.Run("before the quiet period", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		a := f.addAgent("early", "running", "working")
		_, _, l, r := f.strand(a)
		f.offset = runAdoptionQuietPeriod() - 30*time.Second
		f.beat(f.brokerID, a.Slug, l)
		assert.Equal(t, r, f.get(a.ID).RunID)
		assert.Empty(t, f.logs.records(logRunAdopted))
	})

	t.Run("another broker reports R", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		other := f.addBroker("ra-other")
		a := f.addAgent("held", "running", "working")
		_, _, l, r := f.strand(a)
		f.afterQuiet()
		f.beat(other, a.Slug, r)
		f.beat(f.brokerID, a.Slug, l)
		assert.Equal(t, r, f.get(a.ID).RunID)
		assert.Empty(t, f.logs.records(logRunAdopted))
	})

	t.Run("L has no credential row", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		a := f.addAgent("nocred", "running", "working")
		f.setRun(a, "run-l-uncredentialed") // the container's run, token not recorded
		f.setRun(a, "run-r")
		f.afterQuiet()
		f.beat(f.brokerID, a.Slug, "run-l-uncredentialed")
		f.beat(f.brokerID, a.Slug, "run-l-uncredentialed")
		assert.Equal(t, "run-r", f.get(a.ID).RunID, "fail closed")
		assert.Empty(t, f.logs.records(logRunAdopted))
		recs := f.logs.records(logRunUnrecognised)
		require.Len(t, recs, 1, "logged once while the label persists")
		assert.Equal(t, "run-l-uncredentialed", recs[0].attrs["reported_run_id"])
	})

	t.Run("unrecognised label", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		a := f.addAgent("unknownlabel", "running", "working")
		f.mint(a, "run-r")
		f.afterQuiet()
		f.beat(f.brokerID, a.Slug, "run-not-minted")
		assert.Equal(t, "run-r", f.get(a.ID).RunID)
		assert.Len(t, f.logs.records(logRunUnrecognised), 1)
		assert.Empty(t, f.logs.records(logRunAdopted))
	})

	t.Run("credential row of another agent", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		a := f.addAgent("borrower", "running", "working")
		b := f.addAgent("donor", "running", "working")
		f.mint(b, "run-of-b")
		f.mint(a, "run-r")
		f.afterQuiet()
		f.beat(f.brokerID, a.Slug, "run-of-b")
		assert.Equal(t, "run-r", f.get(a.ID).RunID)
		assert.Len(t, f.logs.records(logRunUnrecognised), 1)
	})

	t.Run("empty label is logged only", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		a := f.addAgent("unlabelled", "running", "working")
		f.mint(a, "run-r")
		f.afterQuiet()
		f.beat(f.brokerID, a.Slug, "")
		f.beat(f.brokerID, a.Slug, "")
		assert.Equal(t, "run-r", f.get(a.ID).RunID)
		assert.Len(t, f.logs.records(logRunEmpty), 1)
		assert.Empty(t, f.logs.records(logRunAdopted))
	})

	t.Run("container on a broker other than B", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		other := f.addBroker("ra-elsewhere")
		a := f.addAgent("moved", "running", "working")
		_, _, l, r := f.strand(a)
		f.afterQuiet()
		f.beat(other, a.Slug, l)
		f.beat(other, a.Slug, l)
		assert.Equal(t, r, f.get(a.ID).RunID)
		assert.Empty(t, f.logs.records(logRunAdopted))
	})

	t.Run("an entry on another target of B reports R", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		a := f.addAgent("auxheld", "running", "working")
		_, _, l, r := f.strand(a)
		f.afterQuiet()
		f.beat(f.brokerID, a.Slug, l, brokerAgentRuntimeEntry{RuntimeTarget: "k8s", RunID: r})
		assert.Equal(t, r, f.get(a.ID).RunID)
		assert.Empty(t, f.logs.records(logRunAdopted))
	})

	t.Run("B reports two other runs", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		a := f.addAgent("tworuns", "running", "working")
		_, _, l, r := f.strand(a)
		f.afterQuiet()
		// Both runs were issued for the agent (strand mints R2 too).
		f.beat(f.brokerID, a.Slug, l, brokerAgentRuntimeEntry{RuntimeTarget: "k8s", RunID: "run-r2-" + a.Slug})
		assert.Equal(t, r, f.get(a.ID).RunID)
		assert.Empty(t, f.logs.records(logRunAdopted))
	})

	t.Run("B's report is incomplete", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		a := f.addAgent("incomplete", "running", "working")
		_, _, l, r := f.strand(a)
		f.afterQuiet()
		inv := completeInventory("docker")
		inv.RunLabels = true
		inv.Targets = append(inv.Targets, brokerInventoryTarget{ID: "k8s", Complete: false})
		f.sendVia(f.srv, f.brokerID, brokerHeartbeatRequest{
			Status: store.BrokerStatusOnline, Inventory: inv,
			Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{{
				Slug: a.Slug, Phase: "running", Activity: "working", RuntimeTarget: "docker", RunID: l,
			}}}},
		})
		assert.Equal(t, r, f.get(a.ID).RunID)
		assert.Empty(t, f.logs.records(logRunAdopted))
	})

	t.Run("B does not say it reports every entry", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		a := f.addAgent("noflag", "running", "working")
		_, _, l, r := f.strand(a)
		f.afterQuiet()
		f.sendVia(f.srv, f.brokerID, brokerHeartbeatRequest{
			Status: store.BrokerStatusOnline, Inventory: completeInventory(),
			Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{{
				Slug: a.Slug, Phase: "running", Activity: "working", RuntimeTarget: "docker", RunID: l,
			}}}},
		})
		assert.Equal(t, r, f.get(a.ID).RunID)
		assert.Empty(t, f.logs.records(logRunAdopted))
	})

	t.Run("row without run_id_updated_at", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		a := f.addAgent("noupdatedat", "running", "working")
		_, _, l, r := f.strand(a)
		snapshot := f.get(a.ID)
		snapshot.RunIDUpdatedAt = nil // as for a row last written before the column
		f.afterQuiet()
		f.srv.classifyReportedRuns(context.Background(), f.brokerID,
			&ownRunEntries{agent: snapshot, labels: []string{l}}, true,
			newHeartbeatReport(), f.srv.runAdoption.now(), runAdoptionQuietPeriod())
		assert.Equal(t, r, f.get(a.ID).RunID)
	})

	t.Run("delete holds the row", func(t *testing.T) {
		f := newRunAdoptionFixture(t)
		a := f.addAgent("deleting", "running", "working")
		_, _, l, r := f.strand(a)
		st, lease := store.DeletionStateDeleting, time.Now().Add(time.Hour)
		_, err := f.s.UpdateAgentDeletion(context.Background(), a.ID, store.DeletionPredicate{},
			store.DeletionFields{State: &st, LeaseAt: &lease})
		require.NoError(t, err)
		f.afterQuiet()
		f.beat(f.brokerID, a.Slug, l)
		assert.Equal(t, r, f.get(a.ID).RunID)
		assert.Empty(t, f.logs.records(logRunAdopted))
	})
}

// TestRunAdoption_ConcurrentChangeLosesCleanly: a run-ID write between
// the heartbeat's read of the row and its swap makes the swap match
// nothing; the newer run is kept, nothing is written and nothing is
// logged as adopted.
func TestRunAdoption_ConcurrentChangeLosesCleanly(t *testing.T) {
	f := newRunAdoptionFixture(t)
	a := f.addAgent("racing", "running", "working")
	_, _, l, r := f.strand(a)
	snapshot := f.get(a.ID) // the heartbeat's read
	require.Equal(t, r, snapshot.RunID)

	f.setRun(a, "run-newer") // a dispatch lands in between
	updated := *f.get(a.ID).RunIDUpdatedAt

	f.afterQuiet()
	f.srv.classifyReportedRuns(context.Background(), f.brokerID,
		&ownRunEntries{agent: snapshot, labels: []string{l}}, true,
		newHeartbeatReport(), f.srv.runAdoption.now(), runAdoptionQuietPeriod())

	got := f.get(a.ID)
	assert.Equal(t, "run-newer", got.RunID)
	assert.True(t, got.RunIDUpdatedAt.Equal(updated), "no write")
	assert.Empty(t, f.logs.records(logRunAdopted))
}

// TestRunAdoption_OldBrokerChangesNothing: a heartbeat from a broker that
// sends no labels changes no run and logs nothing above debug.
func TestRunAdoption_OldBrokerChangesNothing(t *testing.T) {
	f := newRunAdoptionFixture(t)
	a := f.addAgent("oldbroker", "running", "working")
	_, _, _, r := f.strand(a)
	before := *f.get(a.ID).RunIDUpdatedAt
	f.afterQuiet()
	f.heartbeat(completeInventory(), a.Slug) // no runId field at all
	got := f.get(a.ID)
	assert.Equal(t, r, got.RunID)
	assert.True(t, got.RunIDUpdatedAt.Equal(before))
	assert.Empty(t, f.logs.records(logRunAdopted))
	assert.Empty(t, f.logs.records(logRunUnrecognised))
}

// TestRunAdoption_TwoReplicas: two hub replicas share one store. A second
// broker reporting R, through either replica, stops the adoption on B,
// and so does an entry of B on another target labelled R. Once neither has
// been reported for the quiet period, whichever replica handles B's next
// heartbeat adopts L.
func TestRunAdoption_TwoReplicas(t *testing.T) {
	f := newRunAdoptionFixture(t)
	other := f.addBroker("ra-replica-other")
	srv2, err := newTestHubServer(t, testServerConfig(), f.s)
	require.NoError(t, err)
	srv2.SetHubID("test-hub-id-2")
	waitUserScopedDataSweep(t, srv2)
	srv2.agentLifecycleLog = slog.New(f.logs)
	srv2.runAdoption.nowFor = f.srv.runAdoption.nowFor

	a := f.addAgent("replicas", "running", "working")
	_, _, l, r := f.strand(a)
	f.afterQuiet()
	start := f.offset

	// Replica 1 handles the other broker's report of R; replica 2 handles
	// B's heartbeat and must not adopt.
	f.beatVia(f.srv, other, a.Slug, r)
	f.beatVia(srv2, f.brokerID, a.Slug, l)
	assert.Equal(t, r, f.get(a.ID).RunID, "a report of R on another broker, through the other replica, stops adoption")

	// The other broker's R is gone, but B now holds R on another target.
	f.offset = start + 30*time.Second
	f.beatVia(f.srv, f.brokerID, a.Slug, l, brokerAgentRuntimeEntry{RuntimeTarget: "k8s", RunID: r})
	assert.Equal(t, r, f.get(a.ID).RunID, "R on another target of B stops adoption")

	// Within Q of the last off-broker report of R: still no adoption.
	f.offset = start + runAdoptionQuietPeriod() - 30*time.Second
	f.beatVia(srv2, f.brokerID, a.Slug, l)
	assert.Equal(t, r, f.get(a.ID).RunID, "R reported elsewhere within Q stops adoption")
	assert.Empty(t, f.logs.records(logRunAdopted))

	// Both gone for Q: B's next heartbeat, on either replica, adopts L.
	f.offset = start + runAdoptionQuietPeriod() + 30*time.Second
	f.beatVia(srv2, f.brokerID, a.Slug, l)
	assert.Equal(t, l, f.get(a.ID).RunID)
	assert.Len(t, f.logs.records(logRunAdopted), 1)
}

// TestRunAdoption_OffBrokerReportsRecordedAndPruned: an entry of an agent
// assigned to another broker is recorded only when it carries the agent's
// current run; B's own entries are not recorded; entries over the per-agent
// cap are skipped and the agent's current run is recorded instead; rows
// older than the quiet period are pruned by a later heartbeat.
func TestRunAdoption_OffBrokerReportsRecordedAndPruned(t *testing.T) {
	ctx := context.Background()
	f := newRunAdoptionFixture(t)
	other := f.addBroker("ra-record-other")
	a := f.addAgent("recorded", "running", "working")
	f.mint(a, "run-r")
	reported := func(agentID, run string, since time.Time) bool {
		t.Helper()
		got, err := f.s.AgentRunReportedSince(ctx, agentID, run, since)
		require.NoError(t, err)
		return got
	}

	f.beat(other, a.Slug, "run-off", brokerAgentRuntimeEntry{RuntimeTarget: "k8s", RunID: "run-r"})
	f.beat(f.brokerID, a.Slug, "run-own")
	since := f.srv.runAdoption.now().Add(-time.Minute)
	assert.True(t, reported(a.ID, "run-r", since), "an off-broker entry carrying the current run is recorded")
	assert.False(t, reported(a.ID, "run-off", since), "an off-broker entry carrying another run writes nothing")
	assert.False(t, reported(a.ID, "run-own", since), "B's own entries are not recorded")

	// Over the cap: none of the handled entries carries the current run,
	// but the current run is recorded as reported, and the skip is logged
	// once for the heartbeat.
	b := f.addAgent("capped", "running", "working")
	f.mint(b, "run-b")
	var others []brokerAgentRuntimeEntry
	for i := 0; i < maxOffBrokerEntriesPerAgent+2; i++ {
		others = append(others, brokerAgentRuntimeEntry{RuntimeTarget: "k8s", RunID: "run-dup"})
	}
	f.beat(other, b.Slug, "run-dup", others...)
	assert.True(t, reported(b.ID, "run-b", since), "over the cap, the current run is recorded")
	assert.False(t, reported(b.ID, "run-dup", since))
	assert.Len(t, f.logs.records("heartbeat: run adoption: entries over the per-agent cap skipped"), 1)

	f.offset = 2*runAdoptionQuietPeriod() + time.Minute
	f.beat(f.brokerID, a.Slug, "run-own")
	assert.False(t, reported(a.ID, "run-r", time.Time{}), "rows older than Q are pruned")
	assert.False(t, reported(b.ID, "run-b", time.Time{}), "rows older than Q are pruned")
}
