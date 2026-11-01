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

// WP1 slice 2 hub-shard tests (design section 8, pkg/hub list): the
// experiment gate and its runtime toggle, the sync create through the real
// HTTPAgentDispatcher (E(iv) from settleLandedRun's recorded answer), the
// restart paths, the create rollback, label PATCH and the forced project
// delete, each drained to a MemorySink.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// lcSetExperiment turns hub.lifecycle_events on or off on srv by replacing
// its registry (every other experiment keeps its registered default).
func lcSetExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.LifecycleEvents {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.LifecycleEvents))
}

// lcBrokerClient shapes the mock broker's synchronous create answer: the
// phase it reports, and whether it names the run the Hub minted.
type lcBrokerClient struct {
	*mockRuntimeBrokerClient
	phase   string
	echoRun bool
	// runID, when set, is the run the answer names instead (a broker that
	// kept another running entry; the Hub adopts it by compare-and-swap).
	runID string
}

func (c *lcBrokerClient) shape(resp *RemoteAgentResponse, req *RemoteCreateAgentRequest) {
	if resp == nil || resp.Agent == nil || req == nil {
		return
	}
	if c.phase != "" {
		resp.Agent.Phase = c.phase
	}
	if c.echoRun {
		resp.Agent.RunID = req.RunID
	}
	if c.runID != "" {
		resp.Agent.RunID = c.runID
	}
}

func (c *lcBrokerClient) CreateAgent(ctx context.Context, brokerID, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	resp, err := c.mockRuntimeBrokerClient.CreateAgent(ctx, brokerID, endpoint, req)
	c.shape(resp, req)
	return resp, err
}

func (c *lcBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	resp, envs, err := c.mockRuntimeBrokerClient.CreateAgentWithGather(ctx, brokerID, endpoint, req)
	c.shape(resp, req)
	return resp, envs, err
}

// lcDispatchServer is a create-capable server with the real
// HTTPAgentDispatcher over a shaped mock broker, a pinned hub ID, a
// MemorySink, and the observer installed through the experiment.
func lcDispatchServer(t *testing.T, phase string, echoRun bool) (*Server, lifecycleEventStore, string, *telemetryemit.MemorySink, *lcBrokerClient) {
	t.Helper()
	srv, s, project, mock := setupThinkingLevelDispatch(t)
	client := &lcBrokerClient{mockRuntimeBrokerClient: mock, phase: phase, echoRun: echoRun}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	ls, ok := s.(lifecycleEventStore)
	require.True(t, ok)
	srv.config.HubIDPinned = true
	srv.config.SoftDeleteRetention = 0
	sink := telemetryemit.NewMemorySink()
	srv.SetLifecycleEventsSink(sink, telemetryemit.NewMemoryCounters())
	lcSetExperiment(t, srv, true)
	srv.startLifecycleEvents(context.Background())
	t.Cleanup(func() { srv.stopLifecycleEvents(context.Background()) })
	require.True(t, ls.AgentWriteObserverInstalled())
	return srv, ls, project.ID, sink, client
}

// lcEvents returns the agent's events from the outbox and the sink, in
// revision order, without disturbing a running drainer's leases for long.
func lcEvents(t *testing.T, ls lifecycleEventStore, sink *telemetryemit.MemorySink, agentID string) []*tc.LifecycleEvent {
	t.Helper()
	lifecycleDrain(t, ls, sink, func() time.Time { return time.Now().Add(time.Hour) })
	byRev := map[int64]*tc.LifecycleEvent{}
	for _, e := range lifecycleDecode(t, sink.Delivered()) {
		if e.AgentID == agentID {
			byRev[e.TransitionRevision] = e
		}
	}
	out := make([]*tc.LifecycleEvent, 0, len(byRev))
	for rev := int64(1); len(out) < len(byRev); rev++ {
		if e, ok := byRev[rev]; ok {
			out = append(out, e)
		}
		if rev > 1000 {
			break
		}
	}
	return out
}

// lcSeqFull renders transition@rev(kind,cause,reason).
func lcSeqFull(evs []*tc.LifecycleEvent) string {
	parts := make([]string, 0, len(evs))
	for _, e := range evs {
		s := fmt.Sprintf("%s@%d", e.Transition, e.TransitionRevision)
		var attrs []string
		for _, a := range []string{e.StopKind, e.Cause, e.EndReason, e.DeleteMode} {
			if a != "" {
				attrs = append(attrs, a)
			}
		}
		if len(attrs) > 0 {
			s += "(" + strings.Join(attrs, ",") + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// assertDense checks revisions 1..n with no gap and no resynced.
func assertDense(t *testing.T, evs []*tc.LifecycleEvent) {
	t.Helper()
	for i, e := range evs {
		assert.Equal(t, int64(i+1), e.TransitionRevision, "revisions must be dense: %s", lcSeqFull(evs))
		assert.NotEqual(t, tc.TransitionResynced, e.Transition, "no resync in an observed flow: %s", lcSeqFull(evs))
	}
}

// TestLifecycleEvents_ExperimentToggle: the observer follows
// hub.lifecycle_events on every settings apply, with no restart; the
// state and sink kind are reported (design section 22 items 4, 13).
func TestLifecycleEvents_ExperimentToggle(t *testing.T) {
	srv, ls, projectID, sink, counters := lifecycleTestServer(t, true)
	wireKinds := []string{telemetryemit.SinkKindCloudLogging, telemetryemit.SinkKindStdout, telemetryemit.SinkKindNone}
	// Setting the sink publishes its kind: a StdoutSink is stdout=1 and the
	// other two 0.
	srv.SetLifecycleEventsSink(telemetryemit.NewStdoutSink(io.Discard), counters)
	for _, k := range wireKinds {
		want := int64(0)
		if k == telemetryemit.SinkKindStdout {
			want = 1
		}
		assert.Equal(t, want, counters.Get(telemetryemit.MetricSinkKind, k), "sink kind %s for a StdoutSink: %s", k, counters)
	}
	// Back to the test's MemorySink, which is none of the three wire kinds:
	// the gauge is published again, with all three written as 0.
	srv.SetLifecycleEventsSink(sink, counters)
	fields := strings.Fields(counters.String())
	for _, k := range wireKinds {
		assert.Contains(t, fields, telemetryemit.MetricSinkKind+"{"+k+"}=0", "a MemorySink publishes sink kind %s as 0", k)
	}
	srv.startLifecycleEvents(context.Background())
	t.Cleanup(func() { srv.stopLifecycleEvents(context.Background()) })
	assert.False(t, ls.AgentWriteObserverInstalled(), "off by default")
	assert.Equal(t, int64(1), counters.Get(telemetryemit.MetricObserverState, telemetryemit.ObserverStateDisabledExperimentOff))

	lcSetExperiment(t, srv, true)
	ApplySnapshot(srv, Layer1Snapshot{})
	require.True(t, ls.AgentWriteObserverInstalled(), "a settings apply installs the observer")
	assert.Equal(t, int64(1), counters.Get(telemetryemit.MetricObserverState, telemetryemit.ObserverStateEnabled))
	srv.lifecycleEvents.mu.Lock()
	running := srv.lifecycleEvents.drainer != nil && !srv.lifecycleEvents.backlogOnly
	srv.lifecycleEvents.mu.Unlock()
	assert.True(t, running, "an installed observer has a running drainer")
	id := lifecycleCreateAgent(t, srv, projectID, "lc-toggle-on")
	// Wait until the running drainer has delivered every row of the create,
	// so nothing is still in flight when the count is taken below.
	require.Eventually(t, func() bool {
		st, err := ls.LifecycleOutboxStats(context.Background())
		return err == nil && st.Pending == 0 && len(sink.Delivered()) > 0
	}, 30*time.Second, 20*time.Millisecond)

	lcSetExperiment(t, srv, false)
	ApplySnapshot(srv, Layer1Snapshot{})
	assert.False(t, ls.AgentWriteObserverInstalled(), "turning it off removes the observer")
	assert.Equal(t, int64(1), counters.Get(telemetryemit.MetricObserverState, telemetryemit.ObserverStateDisabledExperimentOff))
	before := len(sink.Delivered())
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+id, map[string]interface{}{"labels": map[string]string{"x": "y"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	lifecycleDrain(t, ls, sink, time.Now)
	assert.Equal(t, before, len(sink.Delivered()), "no rows while off")
}

// TestLifecycleEvents_ExperimentOnWithoutStableHubID: the experiment on a
// hostname-derived hub ID still refuses (design 3.3).
func TestLifecycleEvents_ExperimentOnWithoutStableHubID(t *testing.T) {
	srv, ls, _, _, counters := lifecycleTestServer(t, false)
	lcSetExperiment(t, srv, true)
	srv.startLifecycleEvents(context.Background())
	t.Cleanup(func() { srv.stopLifecycleEvents(context.Background()) })
	assert.False(t, ls.AgentWriteObserverInstalled())
	assert.Equal(t, int64(1), counters.Get(telemetryemit.MetricObserverState, telemetryemit.ObserverStateDisabledUnpinnedHubID))
}

// TestLifecycleEvents_SyncCreate: a sync create whose broker answers
// running and names the minted run gives created, [configured], started
// at consecutive revisions; started carries the run and the creator comes
// from the create's edge (design section 8, r6 B1, r7 N1).
func TestLifecycleEvents_SyncCreate(t *testing.T) {
	srv, ls, projectID, sink, client := lcDispatchServer(t, "running", true)
	id := lifecycleCreateAgent(t, srv, projectID, "lc-sync")
	require.True(t, client.createCalled)
	row, err := srv.store.GetAgent(context.Background(), id)
	require.NoError(t, err)
	evs := lcEvents(t, ls, sink, id)
	assert.Regexp(t, regexp.MustCompile(`^created@1( configured@2)? started@[23]$`), lcSeqFull(evs))
	assertDense(t, evs)
	started := evs[len(evs)-1]
	assert.Empty(t, started.Cause, "a run-named running answer is correlated")
	assert.Equal(t, row.RunID, started.LaunchID)
	require.NotNil(t, evs[0].Metadata)
	assert.NotEmpty(t, evs[0].Metadata.CreatorPrincipalType)
}

// TestLifecycleEvents_SyncCreateLegacyBroker: an answer that names no run
// gives started(cause=run_uncorrelated) (r7 N2).
func TestLifecycleEvents_SyncCreateLegacyBroker(t *testing.T) {
	srv, ls, projectID, sink, _ := lcDispatchServer(t, "running", false)
	id := lifecycleCreateAgent(t, srv, projectID, "lc-legacy")
	evs := lcEvents(t, ls, sink, id)
	assert.Regexp(t, regexp.MustCompile(`^created@1( configured@2)? started@[23]\(run_uncorrelated\)$`), lcSeqFull(evs))
	assertDense(t, evs)
}

// TestLifecycleEvents_SyncCreateStartingAnswer: a run-named answer that is
// not running gives no started; started waits for E.
func TestLifecycleEvents_SyncCreateStartingAnswer(t *testing.T) {
	srv, ls, projectID, sink, _ := lcDispatchServer(t, "starting", true)
	id := lifecycleCreateAgent(t, srv, projectID, "lc-starting")
	evs := lcEvents(t, ls, sink, id)
	for _, e := range evs {
		assert.NotEqual(t, tc.TransitionStarted, e.Transition, "no started from a starting answer: %s", lcSeqFull(evs))
	}
	assertDense(t, evs)
}

// TestLifecycleEvents_SyncCreateLegacyStartingAnswer: on a plain sync
// create, a legacy broker answering starting with no run gives no started.
// The plain create keeps the broker's phase; the F2 case proper (a
// Hub-forced running on env submit after a legacy starting answer) is
// pinned at store and unit level (TestLifecycleStore2_SyncDispatchResult,
// TestNewDispatchResult), review r2 O1.
func TestLifecycleEvents_SyncCreateLegacyStartingAnswer(t *testing.T) {
	srv, ls, projectID, sink, _ := lcDispatchServer(t, "starting", false)
	id := lifecycleCreateAgent(t, srv, projectID, "lc-legacy-starting")
	evs := lcEvents(t, ls, sink, id)
	for _, e := range evs {
		assert.NotEqual(t, tc.TransitionStarted, e.Transition, "no started: %s", lcSeqFull(evs))
	}
	assertDense(t, evs)
}

// TestLifecycleEvents_EnvSubmitForcedRunning is F2 (tele-wp1-arch 22:06)
// and the r7 N1 env-submit fixture on the real finalize-env path: the
// broker answers provisioning, the Hub forces running, and that is no
// start, whether or not the answer named the run (no E(iv), no legacy
// running observation). A run-named running answer is a correlated start.
func TestLifecycleEvents_EnvSubmitForcedRunning(t *testing.T) {
	for _, c := range []struct {
		name        string
		phase       string
		echoRun     bool
		wantStarted bool
	}{
		{"legacy provisioning answer, Hub-forced running", "provisioning", false, false},
		{"run-named provisioning answer, Hub-forced running", "provisioning", true, false},
		{"run-named running answer", "running", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, ls, projectID, sink, _ := lcDispatchServer(t, c.phase, c.echoRun)
			ctx := context.Background()
			project, err := srv.store.GetProject(ctx, projectID)
			require.NoError(t, err)
			agent := &store.Agent{
				ID: tid("agent-lc-env-" + c.phase), Name: "lc-env", Slug: "lc-env",
				ProjectID: projectID, RuntimeBrokerID: project.DefaultRuntimeBrokerID,
				Phase:         string(state.PhaseProvisioning),
				AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "task"},
			}
			require.NoError(t, srv.store.CreateAgent(ctx, agent))
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/env",
				SubmitEnvRequest{Env: map[string]string{"SOME_KEY": "v"}})
			require.Less(t, rec.Code, 300, rec.Body.String())
			row, err := srv.store.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			require.Equal(t, string(state.PhaseRunning), row.Phase, "the Hub forces running after env submit")
			evs := lcEvents(t, ls, sink, agent.ID)
			assertDense(t, evs)
			started := 0
			for _, e := range evs {
				if e.Transition == tc.TransitionStarted {
					started++
					assert.Empty(t, e.Cause, "only a correlated start is expected")
					assert.Equal(t, row.RunID, e.LaunchID)
				}
			}
			if c.wantStarted {
				assert.Equal(t, 1, started, lcSeqFull(evs))
			} else {
				assert.Zero(t, started, "a Hub-forced running is not a start: %s", lcSeqFull(evs))
			}
		})
	}
}

// TestLifecycleEvents_StopRestartHalt: the HTTP restart of a running agent
// closes R1 at the stop leg (restart_stop_leg, before R2 is minted) and
// starts R2; a stop closes R2; a restart from stopped starts R3 with no
// closure (design section 8, r4 B1, token-analyst-2 05:49).
func TestLifecycleEvents_StopRestartHalt(t *testing.T) {
	srv, ls, projectID, sink, _ := lcDispatchServer(t, "running", true)
	id := lifecycleCreateAgent(t, srv, projectID, "lc-restart")
	r1, err := srv.store.GetAgent(context.Background(), id)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+id+"/restart", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	r2, err := srv.store.GetAgent(context.Background(), id)
	require.NoError(t, err)
	require.NotEqual(t, r1.RunID, r2.RunID)

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+id+"/stop", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+id+"/restart", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())

	evs := lcEvents(t, ls, sink, id)
	seq := lcSeqFull(evs)
	assertDense(t, evs)
	tail := regexp.MustCompile(`started@\d+ stopped@\d+\(stopped,restart_stop_leg\) started@\d+ stopped@\d+\(stopped\) started@\d+$`)
	require.Regexp(t, tail, seq)
	var closure, start2 *tc.LifecycleEvent
	for i, e := range evs {
		if e.Cause == tc.CauseRestartStopLeg {
			closure, start2 = e, evs[i+1]
		}
	}
	require.NotNil(t, closure)
	assert.Equal(t, r1.RunID, closure.LaunchID, "the closure names R1")
	require.NotNil(t, closure.SegmentMetadataRevision)
	assert.Equal(t, r2.RunID, start2.LaunchID, "the next start names R2")
}

// TestLifecycleEvents_CreateRollback: a dispatch failure rolls the create
// back: launch_failed(create_rolled_back) then deleted(create_rolled_back)
// at consecutive revisions, after created.
func TestLifecycleEvents_CreateRollback(t *testing.T) {
	srv, ls, projectID, sink, client := lcDispatchServer(t, "running", true)
	client.returnErr = errors.New("injected broker failure")
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "lc-rollback", ProjectID: projectID, Task: "x"})
	require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	lifecycleDrain(t, ls, sink, func() time.Time { return time.Now().Add(time.Hour) })
	// The running drainer and this drain can both deliver a row (the
	// shifted clock expires its lease): keep one copy per event_id, in
	// revision order. A redelivery must be byte-identical (design 5.4).
	seen := map[string][]byte{}
	var evs []*tc.LifecycleEvent
	for _, p := range sink.Delivered() {
		e := lifecycleDecode(t, [][]byte{p})[0]
		if first, ok := seen[e.EventID]; ok {
			assert.Equal(t, string(first), string(p), "a redelivered event_id %s must be byte-identical", e.EventID)
			continue
		}
		seen[e.EventID] = p
		evs = append(evs, e)
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].TransitionRevision < evs[j].TransitionRevision })
	require.NotEmpty(t, evs)
	seq := lcSeqFull(evs)
	assert.Regexp(t, regexp.MustCompile(`^created@1 .*launch_failed@(\d+)\(create_rolled_back\) deleted@\d+\(create_rolled_back\)$`), seq)
	assertDense(t, evs)
	last, prev := evs[len(evs)-1], evs[len(evs)-2]
	assert.Equal(t, prev.TransitionRevision+1, last.TransitionRevision)
	assert.NotEmpty(t, prev.ErrorCode, "the failed stage is the error code")
}

// TestLifecycleEvents_PatchLabels: adding, editing and removing labels
// gives metadata_changed each time with an exact label snapshot; a
// non-ASCII label is refused with 400 and records nothing.
func TestLifecycleEvents_PatchLabels(t *testing.T) {
	srv, ls, projectID, sink, _ := lcDispatchServer(t, "running", true)
	id := lifecycleCreateAgent(t, srv, projectID, "lc-labels")
	for _, labels := range []map[string]string{
		{"stage": "implement", "x/y.z-1": "a<b>&c"},
		{"stage": "review", "x/y.z-1": "a<b>&c"},
		{"stage": "review"},
	} {
		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+id, map[string]interface{}{"labels": labels})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+id, map[string]interface{}{"labels": map[string]string{"stage": "révision"}})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	evs := lcEvents(t, ls, sink, id)
	assertDense(t, evs)
	var changes []*tc.LifecycleEvent
	for _, e := range evs {
		if e.Transition == tc.TransitionMetadataChanged {
			changes = append(changes, e)
		}
	}
	require.Len(t, changes, 3, lcSeqFull(evs))
	assert.Equal(t, tc.Labels{{Key: "stage", Value: "implement"}, {Key: "x/y.z-1", Value: "a<b>&c"}}, changes[0].Metadata.Labels)
	assert.Equal(t, tc.Labels{{Key: "stage", Value: "review"}}, changes[2].Metadata.Labels)
	assert.Less(t, changes[0].MetadataRevision, changes[2].MetadataRevision)
}

// TestLifecycleEvents_ForcedProjectDelete is section 22 item 9 (moved from
// slice 1): a forced project delete through ProjectDeletionService emits
// deleted(project_deleted) for the live agent and leaves no cursor.
func TestLifecycleEvents_ForcedProjectDelete(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&deleteDispatcher{})
	ls, ok := s.(lifecycleEventStore)
	require.True(t, ok)
	srv.config.HubIDPinned = true
	sink := telemetryemit.NewMemorySink()
	srv.SetLifecycleEventsSink(sink, telemetryemit.NewMemoryCounters())
	_, err := srv.installLifecycleObserver()
	require.NoError(t, err)
	project, _, agent := setupOnlineBrokerAgent(t, s, "lc-proj")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID+"?force=true", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	lifecycleDrain(t, ls, sink, time.Now)
	evs := lifecycleDecode(t, sink.Delivered())
	assert.Equal(t, "created@1 deleted@2(project_deleted)", lcSeqFull(evs))
	for _, e := range evs {
		assert.Equal(t, agent.ID, e.AgentID)
	}
}
