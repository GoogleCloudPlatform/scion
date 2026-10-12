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

//go:build !no_sqlite && (!hubshard || hubshard_1)

package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An applied succeeded launch report settles the run the launch started
// (ptone/scion#3176): the previous runs are cleared, keyed on the run the
// broker reports, as a synchronous dispatch that lands does.

type launchSettleFixture struct {
	srv      *Server
	store    store.Store
	agent    *store.Agent
	brokerID string
	launchID string
	// hooks is the wrapper installed as srv.store over the raw store; once
	// armed, it runs afterReport right after the store applies a launch
	// report, before the handler settles the run.
	hooks *launchSettleStore
	fault *storeFaultSwitch
}

// launchSettleStore runs afterReport (when armed) right after a launch
// report is applied, as another hub write landing between the report and
// the settle would.
type launchSettleStore struct {
	store.Store
	fault       *storeFaultSwitch
	afterReport func(ctx context.Context, agentID string)
}

func (w *launchSettleStore) ApplyLaunchReport(ctx context.Context, agentID, brokerID string, r store.LaunchReport) (store.LaunchReportAnswer, store.Agent, error) {
	answer, updated, err := w.Store.ApplyLaunchReport(ctx, agentID, brokerID, r)
	if err == nil && w.fault.Active() && w.afterReport != nil {
		w.afterReport(ctx, agentID)
	}
	return answer, updated, err
}

// afterReport arms hook to run right after each applied report.
func (f *launchSettleFixture) afterReport(hook func(ctx context.Context, agentID string)) {
	f.hooks.afterReport = hook
	f.fault.Arm()
}

// newLaunchSettleFixture is an agent with an active create launch whose row
// records run-1 as current and run-0 as a previous run.
func newLaunchSettleFixture(t *testing.T, name string) *launchSettleFixture {
	t.Helper()
	srv, s := testServer(t)
	hooks, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *launchSettleStore {
		return &launchSettleStore{Store: inner, fault: f}
	})
	ctx := context.Background()
	brokerID := tid("broker-" + name)
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "broker-" + name, Slug: "broker-" + name,
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	project := &store.Project{ID: tid("project-" + name), Slug: name, Name: name, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("agent-" + name), Slug: name, Name: name, ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: brokerID, StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	for _, r := range []string{"run-0", "run-1"} {
		_, err := s.SetAgentRunID(ctx, agent.ID, r, nil)
		require.NoError(t, err)
	}
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	return &launchSettleFixture{srv: srv, store: s, agent: agent, brokerID: brokerID, launchID: launchID, hooks: hooks, fault: fault}
}

func (f *launchSettleFixture) report(t *testing.T, reportState, runID string) {
	t.Helper()
	f.reportResult(t, reportState, runID)
}

// reportResult sends the report and returns its 200 result. The agent info
// (with runID) is attached whatever the state, so the settle's state gate,
// not a missing run ID, is what a failed report is tested against.
func (f *launchSettleFixture) reportResult(t *testing.T, reportState, runID string) string {
	t.Helper()
	rep := AgentLaunchReport{LaunchID: f.launchID, InstanceID: "i1", State: reportState,
		Agent: &RemoteAgentInfo{ID: f.agent.Slug, Slug: f.agent.Slug, RunID: runID}}
	if reportState == store.LaunchReportStateFailed {
		rep.ErrorCode = "boom"
	}
	rec := postLaunchReport(t, f.srv, f.brokerID, f.agent.ID, f.brokerID, rep)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body agentLaunchReportAppliedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Result
}

func (f *launchSettleFixture) previousRuns(t *testing.T) []string {
	t.Helper()
	return mustGetAgent(t, f.store, f.agent.ID).PreviousRunIDs
}

// A succeeded report naming the current run clears the previous runs, so a
// later delete names only the current run.
func TestLaunchReportSettle_SucceededCurrentRun_Clears(t *testing.T) {
	f := newLaunchSettleFixture(t, "lrsettle-ok")
	require.Equal(t, []string{"run-0"}, f.previousRuns(t))
	f.report(t, store.LaunchReportStateSucceeded, "run-1")
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, "run-1", got.RunID)
	assert.Empty(t, got.PreviousRunIDs, "the launched run settled")

	client := &landingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	d := NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default())
	require.NoError(t, d.DispatchAgentDelete(context.Background(), got, false, false, false, time.Time{}))
	assert.Equal(t, []string{"run-1"}, client.deleteRuns, "a later delete names only the current run")
}

// An older broker reports no run ID: nothing is cleared.
func TestLaunchReportSettle_NoRunID_Keeps(t *testing.T) {
	f := newLaunchSettleFixture(t, "lrsettle-norun")
	f.report(t, store.LaunchReportStateSucceeded, "")
	assert.Equal(t, []string{"run-0"}, f.previousRuns(t))
}

// No run ID never clears, even on a row whose run ID is itself empty (set
// up through the store: a revert to an empty run keeps the list), where an
// unkeyed same-value swap would match.
func TestLaunchReportSettle_NoRunID_EmptyRowRun_Keeps(t *testing.T) {
	f := newLaunchSettleFixture(t, "lrsettle-norun-empty")
	swapped, err := f.store.RevertAgentRunID(context.Background(), f.agent.ID, "run-1", "")
	require.NoError(t, err)
	require.True(t, swapped)
	f.report(t, store.LaunchReportStateSucceeded, "")
	got := mustGetAgent(t, f.store, f.agent.ID)
	require.Equal(t, "", got.RunID)
	assert.Equal(t, []string{"run-0"}, got.PreviousRunIDs)
}

// heldRunClient is a broker whose runtime holds one entry of the agent,
// labelled with run held, and refuses a delete naming another run as the
// real broker does (a run-mismatch, ptone/scion#3080). A delete naming the
// held run removes the entry.
type heldRunClient struct {
	*mockRuntimeBrokerClient
	held       string
	deleteRuns []string
}

func (c *heldRunClient) DeleteAgent(_ context.Context, _, _, _, _ string, opts DeleteAgentOptions) error {
	c.deleteRuns = append(c.deleteRuns, opts.RunID)
	if c.held == "" {
		return nil
	}
	if opts.RunID != c.held {
		return &DeleteRunMismatchError{RequestedRunID: opts.RunID, CurrentRunID: c.held, Err: ErrDeleteRunMismatch}
	}
	c.held = ""
	return nil
}

// An async create that keeps a running entry (ptone/scion#2965): the
// broker's Start found run-0's entry running and returned it, so the
// succeeded report names run-0, not run-1 the hub recorded for the launch.
// The hub adopts run-0 and clears the previous runs, and a later delete
// removes the kept entry.
func TestLaunchReportSettle_KeptRunningEntry_AdoptedAndDeleted(t *testing.T) {
	f := newLaunchSettleFixture(t, "lrsettle-kept")
	f.report(t, store.LaunchReportStateSucceeded, "run-0")
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, "run-0", got.RunID, "the kept entry's run is adopted")
	assert.Empty(t, got.PreviousRunIDs, "the run settled")

	client := &heldRunClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, held: "run-0"}
	d := NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default())
	require.NoError(t, d.DispatchAgentDelete(context.Background(), got, false, false, false, time.Time{}),
		"the delete names the run the broker holds")
	assert.Equal(t, []string{"run-0"}, client.deleteRuns)
	assert.Empty(t, client.held, "the kept running entry is removed")
}

// The adoption is keyed on the run the row recorded when the report
// applied: a run recorded after that is not overwritten.
func TestLaunchReportSettle_LaterRun_Keeps(t *testing.T) {
	f := newLaunchSettleFixture(t, "lrsettle-later")
	f.afterReport(func(ctx context.Context, agentID string) {
		_, err := f.store.SetAgentRunID(ctx, agentID, "run-2", nil)
		require.NoError(t, err)
	})
	f.report(t, store.LaunchReportStateSucceeded, "run-0")
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, "run-2", got.RunID, "the later run is kept")
	assert.Equal(t, []string{"run-0", "run-1"}, got.PreviousRunIDs)
}

// A delete that claims the row after the report applied, before the settle,
// keeps its run: the swap is guarded in the store, not only on the report's
// snapshot of the row.
func TestLaunchReportSettle_DeleteClaimedAfterReport_Keeps(t *testing.T) {
	f := newLaunchSettleFixture(t, "lrsettle-claimed-after")
	f.afterReport(func(_ context.Context, agentID string) {
		claimForTest(t, f.store, agentID, store.DeletionStateDeleting, time.Minute)
	})
	require.Equal(t, store.LaunchReportResultApplied, f.reportResult(t, store.LaunchReportStateSucceeded, "run-0"))
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, "run-1", got.RunID, "the claimed row's run is unchanged")
	assert.Equal(t, []string{"run-0"}, got.PreviousRunIDs)
}

// A delete that holds the row keeps its run: the report neither adopts the
// kept entry's run nor clears the previous runs.
func TestLaunchReportSettle_DeleteHoldsRow_Keeps(t *testing.T) {
	f := newLaunchSettleFixture(t, "lrsettle-claimed")
	claimForTest(t, f.store, f.agent.ID, store.DeletionStateDeleting, time.Minute)
	require.Equal(t, store.LaunchReportResultApplied, f.reportResult(t, store.LaunchReportStateSucceeded, "run-0"))
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, "run-1", got.RunID, "the claimed row's run is unchanged")
	assert.Equal(t, []string{"run-0"}, got.PreviousRunIDs)
}

// A failed launch settles nothing, even when its report names the current
// run.
func TestLaunchReportSettle_Failed_Keeps(t *testing.T) {
	f := newLaunchSettleFixture(t, "lrsettle-fail")
	f.report(t, store.LaunchReportStateFailed, "run-1")
	assert.Equal(t, []string{"run-0"}, f.previousRuns(t))
}

// A repeated succeeded report is not applied again and settles nothing: a
// list recorded after the first report is kept. The store answers a repeat
// of a terminal report for an ended launch "completed" (ApplyLaunchReport's
// ended branch), never "duplicate", so this also pins that a completed
// answer is deliberately not settled.
func TestLaunchReportSettle_DuplicateSucceeded_Keeps(t *testing.T) {
	f := newLaunchSettleFixture(t, "lrsettle-dup")
	require.Equal(t, store.LaunchReportResultApplied, f.reportResult(t, store.LaunchReportStateSucceeded, "run-1"))
	require.Empty(t, f.previousRuns(t))

	ctx := context.Background()
	_, err := f.store.SetAgentRunID(ctx, f.agent.ID, "run-2", nil)
	require.NoError(t, err)
	swapped, err := f.store.RevertAgentRunID(ctx, f.agent.ID, "run-2", "run-1")
	require.NoError(t, err)
	require.True(t, swapped)
	require.Equal(t, []string{"run-1"}, f.previousRuns(t))

	assert.Equal(t, store.LaunchReportResultCompleted, f.reportResult(t, store.LaunchReportStateSucceeded, "run-1"))
	assert.Equal(t, []string{"run-1"}, f.previousRuns(t), "a repeated report does not settle")
}

// A report for a superseded launch is rejected (409 stale_launch) and
// settles nothing, even when it names the current run.
func TestLaunchReportSettle_StaleLaunch_Keeps(t *testing.T) {
	f := newLaunchSettleFixture(t, "lrsettle-stale")
	_, err := f.store.BeginLaunch(context.Background(), f.agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	rec := postLaunchReport(t, f.srv, f.brokerID, f.agent.ID, f.brokerID, AgentLaunchReport{
		LaunchID: f.launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
		Agent: &RemoteAgentInfo{ID: f.agent.Slug, Slug: f.agent.Slug, RunID: "run-1"},
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"run-0"}, f.previousRuns(t))
}

// The broker's report type carries runId only when set, and the hub's
// decoder reads it.
func TestLaunchReportInfo_RunIDWire(t *testing.T) {
	b, err := json.Marshal(hubclient.AgentLaunchReportInfo{Slug: "a"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "runId", "omitted when empty")

	b, err = json.Marshal(hubclient.AgentLaunchReportInfo{Slug: "a", RunID: "run-1"})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"runId":"run-1"`)
	var got RemoteAgentInfo
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "run-1", got.RunID)
}
