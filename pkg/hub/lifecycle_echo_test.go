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

// WP1 slice 3 hub-shard tests (design/wp1-slice3-addendum-v12.md section
// 7, pkg/hub list): the broker effective-configuration echo on the sync
// dispatch path and the async launch-report path, the stale-echo counter
// and its classification, and the conversion's string validation.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// lcEcho is the effective configuration a slice-3 fake broker echoes.
type lcEcho struct {
	Template, TemplateHash, Harness, Model, HarnessConfigRevision, BrokerBuild string
}

var lcDefaultEcho = lcEcho{
	TemplateHash: "sha256:echo", Harness: "claude", Model: "model-echo-1",
	HarnessConfigRevision: "rev-echo", BrokerBuild: "v9.9.9",
}

// lcEchoClient is lcBrokerClient whose create, start and restart answers
// also carry an effective-configuration echo (nil: an older broker).
type lcEchoClient struct {
	*lcBrokerClient
	mu   sync.Mutex
	echo *lcEcho
}

func (c *lcEchoClient) applyEcho(resp *RemoteAgentResponse, runID string) {
	if resp == nil || resp.Agent == nil {
		return
	}
	if c.phase != "" {
		resp.Agent.Phase = c.phase
	}
	if c.echoRun && runID != "" {
		resp.Agent.RunID = runID
	}
	c.mu.Lock()
	e := c.echo
	c.mu.Unlock()
	if e == nil {
		return
	}
	if e.Template != "" {
		resp.Agent.Template = e.Template
	}
	resp.Agent.TemplateHash, resp.Agent.Harness, resp.Agent.Model = e.TemplateHash, e.Harness, e.Model
	resp.Agent.HarnessConfigRevision, resp.Agent.BrokerBuild = e.HarnessConfigRevision, e.BrokerBuild
}

func (c *lcEchoClient) setEcho(e *lcEcho) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.echo = e
}

func (c *lcEchoClient) CreateAgent(ctx context.Context, brokerID, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	resp, err := c.lcBrokerClient.CreateAgent(ctx, brokerID, endpoint, req)
	if req != nil {
		c.applyEcho(resp, req.RunID)
	}
	return resp, err
}

func (c *lcEchoClient) CreateAgentWithGather(ctx context.Context, brokerID, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	resp, envs, err := c.lcBrokerClient.CreateAgentWithGather(ctx, brokerID, endpoint, req)
	if req != nil {
		c.applyEcho(resp, req.RunID)
	}
	return resp, envs, err
}

func (c *lcEchoClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool, extras StartExtras) (*RemoteAgentResponse, error) {
	resp, err := c.mockRuntimeBrokerClient.StartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs, sharedWorkspace, resume, extras)
	c.applyEcho(resp, extras.RunID)
	return resp, err
}

func (c *lcEchoClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string, extras StartExtras) (*RemoteAgentResponse, error) {
	resp, err := c.mockRuntimeBrokerClient.RestartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, resolvedEnv, extras)
	c.applyEcho(resp, extras.RunID)
	return resp, err
}

// lcEchoServer is lcDispatchServer over a broker that echoes e (nil: an
// older broker), returning the lifecycle counters too.
func lcEchoServer(t *testing.T, e *lcEcho) (*Server, lifecycleEventStore, string, *telemetryemit.MemorySink, *telemetryemit.MemoryCounters, *lcEchoClient) {
	t.Helper()
	srv, s, project, mock := setupThinkingLevelDispatch(t)
	client := &lcEchoClient{lcBrokerClient: &lcBrokerClient{mockRuntimeBrokerClient: mock, phase: "running", echoRun: true}, echo: e}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	ls, ok := s.(lifecycleEventStore)
	require.True(t, ok)
	srv.config.HubIDPinned = true
	srv.config.SoftDeleteRetention = 0
	sink := telemetryemit.NewMemorySink()
	counters := telemetryemit.NewMemoryCounters()
	srv.SetLifecycleEventsSink(sink, counters)
	lcSetExperiment(t, srv, true)
	srv.startLifecycleEvents(context.Background())
	t.Cleanup(func() { srv.stopLifecycleEvents(context.Background()) })
	require.True(t, ls.AgentWriteObserverInstalled())
	return srv, ls, project.ID, sink, counters, client
}

// effModel is an event's effective_model ("-" when omitted, "nometa" for
// a non-snapshot event).
func effModel(e *tc.LifecycleEvent) string {
	if e.Metadata == nil {
		return "nometa"
	}
	if e.Metadata.EffectiveModel == "" && e.Metadata.EffectiveTemplateHash == "" &&
		e.Metadata.EffectiveHarness == "" && e.Metadata.EffectiveBrokerBuild == "" &&
		e.Metadata.EffectiveTemplate == "" && e.Metadata.HarnessConfigRevision == "" {
		return "-"
	}
	return e.Metadata.EffectiveModel
}

// configuredWithEcho returns the configured events that carry effective_*.
func configuredWithEcho(evs []*tc.LifecycleEvent) []*tc.LifecycleEvent {
	var out []*tc.LifecycleEvent
	for _, e := range evs {
		if e.Transition == tc.TransitionConfigured && effModel(e) != "-" {
			out = append(out, e)
		}
	}
	return out
}

// countOf counts events of transition.
func countOf(evs []*tc.LifecycleEvent, transition string) int {
	n := 0
	for _, e := range evs {
		if e.Transition == transition {
			n++
		}
	}
	return n
}

func staleEchoes(c *telemetryemit.MemoryCounters, path, reason string) int64 {
	return c.Get(telemetryemit.MetricStaleEcho, telemetryemit.StaleEchoKey(path, reason))
}

// assertNoStaleEcho checks that no stale echo was counted at all.
func assertNoStaleEcho(t *testing.T, c *telemetryemit.MemoryCounters) {
	t.Helper()
	assert.NotContains(t, c.String(), telemetryemit.MetricStaleEcho, "no stale echo may be counted")
}

// TestLifecycleEvents_SyncEcho: a sync create whose answer carries the
// echo gives configured with effective_* and launch_id = the run, before
// started (created, configured, [configured,] started); the stored echo is
// fenced on the dispatch target broker.
func TestLifecycleEvents_SyncEcho(t *testing.T) {
	e := lcDefaultEcho
	srv, ls, projectID, sink, counters, _ := lcEchoServer(t, &e)
	id := lifecycleCreateAgent(t, srv, projectID, "lc-echo")
	row := mustGetAgent(t, srv.store, id)
	evs := lcEvents(t, ls, sink, id)
	assertDense(t, evs)
	assert.Regexp(t, `^created@1 configured@2( configured@3)? started@[34]$`, lcSeqFull(evs))
	echoed := configuredWithEcho(evs)
	require.NotEmpty(t, echoed, lcSeqFull(evs))
	first := echoed[0]
	assert.Equal(t, int64(2), first.TransitionRevision, "the echo's configured precedes the dispatch result")
	assert.Equal(t, row.RunID, first.LaunchID)
	assert.Equal(t, "model-echo-1", first.Metadata.EffectiveModel)
	assert.Equal(t, "sha256:echo", first.Metadata.EffectiveTemplateHash)
	assert.Equal(t, "claude", first.Metadata.EffectiveHarness)
	assert.Equal(t, "rev-echo", first.Metadata.HarnessConfigRevision)
	assert.Equal(t, "v9.9.9", first.Metadata.EffectiveBrokerBuild)
	require.NotNil(t, row.EffectiveConfig)
	assert.Equal(t, row.RunID, row.EffectiveConfig.RunID)
	assert.Equal(t, row.RuntimeBrokerID, row.EffectiveConfig.BrokerID, "BrokerID is the dispatch target")
	assert.Equal(t, store.EffectiveConfigSourceSync, row.EffectiveConfig.Source)
	assertNoStaleEcho(t, counters)
}

// TestLifecycleEvents_OldBrokerNoEffective: an answer without
// effective-only fields records nothing, emits no effective_* and counts
// nothing.
func TestLifecycleEvents_OldBrokerNoEffective(t *testing.T) {
	srv, ls, projectID, sink, counters, _ := lcEchoServer(t, nil)
	id := lifecycleCreateAgent(t, srv, projectID, "lc-old-broker")
	evs := lcEvents(t, ls, sink, id)
	assert.Empty(t, configuredWithEcho(evs), lcSeqFull(evs))
	for _, e := range evs {
		assert.Contains(t, []string{"-", "nometa"}, effModel(e), "%s@%d", e.Transition, e.TransitionRevision)
	}
	assert.Nil(t, mustGetAgent(t, srv.store, id).EffectiveConfig)
	assertNoStaleEcho(t, counters)
}

// TestLifecycleEvents_RequestedVsResolvedTemplate (section 4a): the broker
// resolves another template than requested; requested_template stays the
// request on every event while effective_template and the row's template
// carry the broker's.
func TestLifecycleEvents_RequestedVsResolvedTemplate(t *testing.T) {
	e := lcDefaultEcho
	e.Template = "resolved-b"
	srv, ls, projectID, sink, _, _ := lcEchoServer(t, &e)
	id := lifecycleCreateAgent(t, srv, projectID, "lc-tmpl")
	evs := lcEvents(t, ls, sink, id)
	require.NotNil(t, evs[0].Metadata)
	requested := evs[0].Metadata.RequestedTemplate
	assert.NotEqual(t, "resolved-b", requested)
	for _, ev := range evs {
		if ev.Metadata != nil {
			assert.Equal(t, requested, ev.Metadata.RequestedTemplate, "%s@%d", ev.Transition, ev.TransitionRevision)
		}
	}
	echoed := configuredWithEcho(evs)
	require.NotEmpty(t, echoed)
	assert.Equal(t, "resolved-b", echoed[0].Metadata.EffectiveTemplate)
	assert.Equal(t, "resolved-b", mustGetAgent(t, srv.store, id).Template, "the operational template is the broker's")
}

// TestLifecycleEvents_SyncRestartEcho and start (N6): the echo of a
// restart answer and of a start answer is configured for the new run, even
// with values identical to the previous run's.
func TestLifecycleEvents_SyncRestartEcho(t *testing.T) {
	e := lcDefaultEcho
	srv, ls, projectID, sink, counters, _ := lcEchoServer(t, &e)
	id := lifecycleCreateAgent(t, srv, projectID, "lc-echo-restart")
	r1 := mustGetAgent(t, srv.store, id).RunID

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+id+"/restart", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	r2 := mustGetAgent(t, srv.store, id).RunID
	require.NotEqual(t, r1, r2)

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+id+"/stop", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+id+"/start", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	r3 := mustGetAgent(t, srv.store, id).RunID
	require.NotEqual(t, r2, r3)

	evs := lcEvents(t, ls, sink, id)
	assertDense(t, evs)
	var runs []string
	for _, c := range configuredWithEcho(evs) {
		runs = append(runs, c.LaunchID)
	}
	assert.Equal(t, []string{r1, r2, r3}, runs, "one echo configured per run: %s", lcSeqFull(evs))
	// Each run's configured precedes its started.
	for _, run := range []string{r1, r2, r3} {
		var configuredAt, startedAt int64
		for _, ev := range evs {
			if ev.LaunchID != run {
				continue
			}
			if ev.Transition == tc.TransitionConfigured && effModel(ev) != "-" && configuredAt == 0 {
				configuredAt = ev.TransitionRevision
			}
			if ev.Transition == tc.TransitionStarted && startedAt == 0 {
				startedAt = ev.TransitionRevision
			}
		}
		assert.Positive(t, configuredAt, "run %s has an echo configured", run)
		assert.Less(t, configuredAt, startedAt, "run %s: configured before started: %s", run, lcSeqFull(evs))
	}
	assertNoStaleEcho(t, counters)
}

// TestLifecycleEvents_GatherEcho (N6): the env-gather two-pass records the
// echo of the answer of the start that ran.
func TestLifecycleEvents_GatherEcho(t *testing.T) {
	e := lcDefaultEcho
	srv, ls, projectID, sink, counters, _ := lcEchoServer(t, &e)
	ctx := context.Background()
	project, err := srv.store.GetProject(ctx, projectID)
	require.NoError(t, err)
	agent := &store.Agent{
		ID: tid("agent-lc-gather-echo"), Name: "lc-gather-echo", Slug: "lc-gather-echo",
		ProjectID: projectID, RuntimeBrokerID: project.DefaultRuntimeBrokerID,
		Phase:         string(state.PhaseProvisioning),
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "task"},
	}
	require.NoError(t, srv.store.CreateAgent(ctx, agent))
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/env",
		SubmitEnvRequest{Env: map[string]string{"SOME_KEY": "v"}})
	require.Less(t, rec.Code, 300, rec.Body.String())
	row := mustGetAgent(t, srv.store, agent.ID)
	evs := lcEvents(t, ls, sink, agent.ID)
	echoed := configuredWithEcho(evs)
	require.Len(t, echoed, 1, lcSeqFull(evs))
	assert.Equal(t, row.RunID, echoed[0].LaunchID)
	require.NotNil(t, row.EffectiveConfig)
	assert.Equal(t, row.RunID, row.EffectiveConfig.RunID)
	assertNoStaleEcho(t, counters)
}

// lcStaleFixture is a lifecycle-observed server with one running agent on
// broker b-1 at run R1, for calling the sync seam directly.
func lcStaleFixture(t *testing.T) (*Server, lifecycleEventStore, *telemetryemit.MemorySink, *telemetryemit.MemoryCounters, *store.Agent) {
	t.Helper()
	srv, ls, projectID, sink, counters := lifecycleTestServer(t, true)
	lcSetExperiment(t, srv, true)
	srv.startLifecycleEvents(context.Background())
	t.Cleanup(func() { srv.stopLifecycleEvents(context.Background()) })
	ctx := context.Background()
	brokerID := tid("broker-lc-stale")
	require.NoError(t, srv.store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "lc-stale", Slug: "lc-stale", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	agent := &store.Agent{
		ID: tid("agent-lc-stale"), Name: "lc-stale", Slug: "lc-stale", ProjectID: projectID,
		RuntimeBrokerID: brokerID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, srv.store.CreateAgent(ctx, agent))
	_, err := srv.store.SetAgentRunID(ctx, agent.ID, "R1", nil)
	require.NoError(t, err)
	return srv, ls, sink, counters, mustGetAgent(t, srv.store, agent.ID)
}

// echoAnswer is a slice-3 broker answer naming run.
func echoAnswer(run string) *RemoteAgentResponse {
	return &RemoteAgentResponse{Agent: &RemoteAgentInfo{
		RunID: run, Phase: string(state.PhaseRunning), TemplateHash: "sha256:echo", Harness: "claude",
		Model: "model-echo-1", BrokerBuild: "v9.9.9",
	}}
}

// TestLifecycleEvents_SyncStaleEchoes: the stale sync echoes write nothing,
// emit no configured, and are counted by reason (store reasons as
// returned; an answer naming another run as run_mismatch without a store
// call); an answer without effective-only fields counts nothing.
func TestLifecycleEvents_SyncStaleEchoes(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		setup  func(t *testing.T, srv *Server, a *store.Agent) (*store.Agent, *RemoteAgentResponse)
		reason string // "" counts nothing
	}{
		{"StaleEcho: the answer names an older run (the row moved on)", func(t *testing.T, srv *Server, a *store.Agent) (*store.Agent, *RemoteAgentResponse) {
			return a, echoAnswer("R0")
		}, store.StaleEchoRunMismatch},
		{"a newer run recorded during the call", func(t *testing.T, srv *Server, a *store.Agent) (*store.Agent, *RemoteAgentResponse) {
			_, err := srv.store.SetAgentRunID(ctx, a.ID, "R2", nil)
			require.NoError(t, err)
			return a, echoAnswer("R1") // the dispatched copy still holds R1
		}, store.StaleEchoRunMismatch},
		{"StaleBrokerEcho: the row was reassigned to another broker", func(t *testing.T, srv *Server, a *store.Agent) (*store.Agent, *RemoteAgentResponse) {
			row := mustGetAgent(t, srv.store, a.ID)
			row.RuntimeBrokerID = tid("broker-other")
			require.NoError(t, srv.store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: row.RuntimeBrokerID, Name: "other", Slug: "other", Endpoint: "http://localhost:9801", Status: store.BrokerStatusOnline,
			}))
			require.NoError(t, srv.store.UpdateAgent(ctx, row))
			return a, echoAnswer("R1")
		}, store.StaleEchoBrokerMismatch},
		{"DeleteInFlightNoEcho: a soft delete landed during the call", func(t *testing.T, srv *Server, a *store.Agent) (*store.Agent, *RemoteAgentResponse) {
			now := time.Now()
			_, err := srv.store.FinalizeAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFinalizeSoft, store.DeletionFields{DeletedAt: &now}, nil)
			require.NoError(t, err)
			return a, echoAnswer("R1")
		}, store.StaleEchoDeletedOrHeld},
		{"the row is gone", func(t *testing.T, srv *Server, a *store.Agent) (*store.Agent, *RemoteAgentResponse) {
			require.NoError(t, srv.store.DeleteAgent(ctx, a.ID))
			return a, echoAnswer("R1")
		}, store.StaleEchoGone},
		{"an older broker's answer with no effective field", func(t *testing.T, srv *Server, a *store.Agent) (*store.Agent, *RemoteAgentResponse) {
			_, err := srv.store.SetAgentRunID(ctx, a.ID, "R2", nil)
			require.NoError(t, err)
			return a, &RemoteAgentResponse{Agent: &RemoteAgentInfo{RunID: "R1", Phase: "running", Template: "t"}}
		}, ""},
		{"a legacy answer naming no run", func(t *testing.T, srv *Server, a *store.Agent) (*store.Agent, *RemoteAgentResponse) {
			return a, echoAnswer("")
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, ls, sink, counters, a := lcStaleFixture(t)
			dispatched, resp := c.setup(t, srv, a)
			syncDispatchEcho(ctx, srv.store, counters, slog.Default(), dispatched, resp)
			if c.reason == "" {
				assertNoStaleEcho(t, counters)
			} else {
				assert.Equal(t, int64(1), staleEchoes(counters, telemetryemit.StaleEchoPathSync, c.reason), counters.String())
			}
			if got, err := srv.store.GetAgent(ctx, a.ID); err == nil {
				assert.Nil(t, got.EffectiveConfig, "a stale echo writes nothing")
			}
			evs := lcEvents(t, ls, sink, a.ID)
			assert.Empty(t, configuredWithEcho(evs), lcSeqFull(evs))
			if n := len(evs); n > 0 && evs[n-1].Transition == tc.TransitionDeleted {
				assert.NotEqual(t, tc.TransitionConfigured, evs[n-1].Transition, "no configured after deleted")
			}
		})
	}
}

// echoFaultStore wraps the hub's store: it forwards the effective-config
// writer, can fail it, and can fail or refuse the launch settle.
type echoFaultStore struct {
	store.Store
	mu        sync.Mutex
	failEcho  int    // effective writes still to fail
	adoptMode string // "", "error" or "refuse": the applied settle (AdoptAgentRunID)
	// settleMode is adoptMode for the completed settle (SettleLaunchedRun,
	// WP1 slice 3b). beforeSettle, when set, runs right before
	// SettleLaunchedRun delegates, as a hub write landing between the
	// handler's check and the swap would.
	settleMode   string
	beforeSettle func(ctx context.Context, agentID string)
}

var errInjectedEcho = errors.New("injected effective-config failure")

func (w *echoFaultStore) SetAgentEffectiveConfig(ctx context.Context, agentID, runID string, eff store.AgentEffectiveConfig) (store.EffectiveConfigResult, error) {
	w.mu.Lock()
	fail := w.failEcho > 0
	if fail {
		w.failEcho--
	}
	w.mu.Unlock()
	if fail {
		return store.EffectiveConfigResult{}, errInjectedEcho
	}
	return w.Store.(effectiveConfigWriter).SetAgentEffectiveConfig(ctx, agentID, runID, eff)
}

func (w *echoFaultStore) AdoptAgentRunID(ctx context.Context, agentID, expectedRunID, newRunID string, unchangedSince time.Time) (bool, error) {
	w.mu.Lock()
	mode := w.adoptMode
	w.mu.Unlock()
	switch mode {
	case "error":
		return false, errInjectedEcho
	case "refuse":
		return false, nil
	}
	return w.Store.AdoptAgentRunID(ctx, agentID, expectedRunID, newRunID, unchangedSince)
}

func (w *echoFaultStore) SettleLaunchedRun(ctx context.Context, agentID string, f store.LaunchSettleFence) (bool, error) {
	w.mu.Lock()
	mode, hook := w.settleMode, w.beforeSettle
	w.mu.Unlock()
	switch mode {
	case "error":
		return false, errInjectedEcho
	case "refuse":
		return false, nil
	}
	if hook != nil {
		hook(ctx, agentID)
	}
	return w.Store.SettleLaunchedRun(ctx, agentID, f)
}

// echoReportFixture is an agent with an active create launch bound to its
// minted run "run-m", on a lifecycle-observed server whose store is
// wrapped in echoFaultStore; logs captures the server log at DEBUG.
type echoReportFixture struct {
	srv      *Server
	raw      store.Store
	ls       lifecycleEventStore
	sink     *telemetryemit.MemorySink
	counters *telemetryemit.MemoryCounters
	wrap     *echoFaultStore
	logs     *syncBuffer
	agent    *store.Agent
	brokerID string
	launchID string
}

func newEchoReportFixture(t *testing.T, name string) *echoReportFixture {
	t.Helper()
	srv, ls, projectID, sink, counters := lifecycleTestServer(t, true)
	lcSetExperiment(t, srv, true)
	srv.startLifecycleEvents(context.Background())
	t.Cleanup(func() { srv.stopLifecycleEvents(context.Background()) })
	ctx := context.Background()
	raw := srv.store
	brokerID := tid("broker-" + name)
	require.NoError(t, raw.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: name, Slug: name, Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	agent := &store.Agent{
		ID: tid("agent-" + name), Name: name, Slug: name, ProjectID: projectID,
		RuntimeBrokerID: brokerID, Phase: string(state.PhaseCreated),
	}
	require.NoError(t, raw.CreateAgent(ctx, agent))
	_, err := raw.SetAgentRunID(ctx, agent.ID, "run-m", nil)
	require.NoError(t, err)
	launchID, err := raw.BeginLaunchWithOptions(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute, store.BeginLaunchOptions{RunID: "run-m"})
	require.NoError(t, err)
	require.Equal(t, "run-m", mustGetAgent(t, raw, agent.ID).LaunchRunID, "the launch binds its minted run")
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	wrap, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *echoFaultStore {
		return &echoFaultStore{Store: inner}
	})
	return &echoReportFixture{srv: srv, raw: raw, ls: ls, sink: sink, counters: counters, wrap: wrap, logs: logs,
		agent: agent, brokerID: brokerID, launchID: launchID}
}

// report sends the succeeded report naming run (with the echo unless
// noEcho) and returns its 200 result.
func (f *echoReportFixture) report(t *testing.T, run string, noEcho bool) string {
	t.Helper()
	info := &RemoteAgentInfo{ID: f.agent.Slug, Slug: f.agent.Slug, RunID: run, Phase: string(state.PhaseRunning)}
	if !noEcho {
		info.TemplateHash, info.Harness, info.Model, info.BrokerBuild = "sha256:echo", "claude", "model-echo-1", "v9.9.9"
		info.HarnessConfigRevision = "rev-echo"
	}
	rec := postLaunchReport(t, f.srv, f.brokerID, f.agent.ID, f.brokerID, AgentLaunchReport{
		LaunchID: f.launchID, InstanceID: "i1", Seq: 1, State: store.LaunchReportStateSucceeded, Agent: info,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body agentLaunchReportAppliedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Result
}

func (f *echoReportFixture) events(t *testing.T) []*tc.LifecycleEvent {
	t.Helper()
	return lcEvents(t, f.ls, f.sink, f.agent.ID)
}

func (f *echoReportFixture) unsettledLines() int {
	return strings.Count(f.logs.String(), "lifecycle_echo_unsettled_adoption")
}

// assertNoEffective checks that no event carries effective_*.
func assertNoEffective(t *testing.T, evs []*tc.LifecycleEvent) {
	t.Helper()
	for _, e := range evs {
		assert.Contains(t, []string{"-", "nometa"}, effModel(e), "%s@%d carries effective_*", e.Transition, e.TransitionRevision)
	}
}

// TestLifecycleEvents_AsyncEcho: the succeeded report gives started (the
// report write), then configured (the effective write) with launch_id =
// the run; the stored echo names the launch and the reporting broker. A
// resent report gives no new configured (EchoReportRetry).
func TestLifecycleEvents_AsyncEcho(t *testing.T) {
	f := newEchoReportFixture(t, "lc-async-echo")
	require.Equal(t, store.LaunchReportResultApplied, f.report(t, "run-m", false))
	evs := f.events(t)
	assertDense(t, evs)
	require.Regexp(t, `started@\d+ configured@\d+$`, lcSeqFull(evs))
	c := evs[len(evs)-1]
	assert.Equal(t, "run-m", c.LaunchID)
	assert.Equal(t, "model-echo-1", effModel(c))
	row := mustGetAgent(t, f.raw, f.agent.ID)
	require.NotNil(t, row.EffectiveConfig)
	assert.Equal(t, f.launchID, row.EffectiveConfig.LaunchID)
	assert.Equal(t, f.brokerID, row.EffectiveConfig.BrokerID)
	assert.Equal(t, store.EffectiveConfigSourceLaunchReport, row.EffectiveConfig.Source)

	f.report(t, "run-m", false) // EchoReportRetry
	again := f.events(t)
	assert.Equal(t, 1, countOf(again, tc.TransitionConfigured), lcSeqFull(again))
	assert.Equal(t, 1, countOf(again, tc.TransitionStarted), lcSeqFull(again))
	assertNoStaleEcho(t, f.counters)
}

// TestLifecycleEvents_AsyncEchoOldBroker: a report without effective-only
// fields records and counts nothing.
func TestLifecycleEvents_AsyncEchoOldBroker(t *testing.T) {
	f := newEchoReportFixture(t, "lc-async-old")
	f.report(t, "run-m", true)
	assertNoEffective(t, f.events(t))
	assert.Nil(t, mustGetAgent(t, f.raw, f.agent.ID).EffectiveConfig)
	assertNoStaleEcho(t, f.counters)
}

// TestLifecycleEvents_AsyncEchoAdoptedRun (B1): the report names the
// broker's run run-b; the settle adopts it, the echo is recorded for
// run-b, configured carries launch_id run-b, nothing is stale.
func TestLifecycleEvents_AsyncEchoAdoptedRun(t *testing.T) {
	f := newEchoReportFixture(t, "lc-async-adopt")
	require.Equal(t, store.LaunchReportResultApplied, f.report(t, "run-b", false))
	row := mustGetAgent(t, f.raw, f.agent.ID)
	require.Equal(t, "run-b", row.RunID, "the settle adopted the broker's run")
	require.NotNil(t, row.EffectiveConfig)
	assert.Equal(t, "run-b", row.EffectiveConfig.RunID)
	echoed := configuredWithEcho(f.events(t))
	require.Len(t, echoed, 1)
	assert.Equal(t, "run-b", echoed[0].LaunchID)
	assertNoStaleEcho(t, f.counters)
	assert.Zero(t, f.unsettledLines())
}

// TestLifecycleEvents_AsyncEchoReplayRecovers (token-analyst-2 02:34): the
// effective write fails after the report commits; the identical replay is
// answered completed with no new started, and the handler writes the
// missing echo: exactly one started and one configured. A second replay
// writes nothing.
func TestLifecycleEvents_AsyncEchoReplayRecovers(t *testing.T) {
	f := newEchoReportFixture(t, "lc-async-recover")
	f.wrap.failEcho = 1
	require.Equal(t, store.LaunchReportResultApplied, f.report(t, "run-m", false))
	evs := f.events(t)
	require.Equal(t, 1, countOf(evs, tc.TransitionStarted), lcSeqFull(evs))
	require.Empty(t, configuredWithEcho(evs), "the effective write failed")

	assert.Equal(t, store.LaunchReportResultCompleted, f.report(t, "run-m", false))
	evs = f.events(t)
	assert.Equal(t, 1, countOf(evs, tc.TransitionStarted), lcSeqFull(evs))
	assert.Len(t, configuredWithEcho(evs), 1, "the replay recovers the echo: %s", lcSeqFull(evs))

	f.report(t, "run-m", false)
	assert.Len(t, f.events(t), len(evs), "a second replay writes nothing")
	assertNoStaleEcho(t, f.counters)
}

// TestLifecycleEvents_AsyncEchoReplayAdoptionSkipped (paired negative
// fixture, review r4 R2): the report names run-b and the settle fails, so
// the row keeps its launch's own run: stale{launch_report,
// unsettled_adoption} with the INFO line, on the first report and again on
// the identical replay, whose completed settle (WP1 slice 3b) fails too.
// No configured, no effective_* anywhere. The "refuse" mode is the same
// with the adoption refused instead of failing.
// TestLifecycleEvents_AsyncEchoReplayAdoptionRecovers is the replay whose
// settle succeeds.
func TestLifecycleEvents_AsyncEchoReplayAdoptionSkipped(t *testing.T) {
	for _, mode := range []string{"error", "refuse"} {
		t.Run(mode, func(t *testing.T) {
			f := newEchoReportFixture(t, "lc-async-skip-"+mode)
			f.wrap.adoptMode = mode
			f.wrap.settleMode = mode
			require.Equal(t, store.LaunchReportResultApplied, f.report(t, "run-b", false))
			assert.Equal(t, int64(1), staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, store.StaleEchoUnsettledAdoption), f.counters.String())
			assert.Equal(t, 1, f.unsettledLines())

			assert.Equal(t, store.LaunchReportResultCompleted, f.report(t, "run-b", false))
			assert.Equal(t, int64(2), staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, store.StaleEchoUnsettledAdoption), f.counters.String())
			assert.Equal(t, 2, f.unsettledLines())

			evs := f.events(t)
			assert.Zero(t, countOf(evs, tc.TransitionConfigured), lcSeqFull(evs))
			assertNoEffective(t, evs)
			assert.Nil(t, mustGetAgent(t, f.raw, f.agent.ID).EffectiveConfig)
			assert.NotContains(t, f.logs.String(), "model-echo-1", "the INFO line carries no echo values")
		})
	}
}

// TestLifecycleEvents_AsyncEchoReplayAdoptionRecovers (WP1 slice 3b, cause
// 3): the applied settle fails or is refused, so the first report's echo
// is unsettled_adoption. The broker's identical replay is answered
// completed; the row still holds the launch's own run, so the completed
// settle adopts run-b and the echo applies: one configured for run-b with
// the adoption provenance, and no second stale echo.
func TestLifecycleEvents_AsyncEchoReplayAdoptionRecovers(t *testing.T) {
	for _, mode := range []string{"error", "refuse"} {
		t.Run(mode, func(t *testing.T) {
			f := newEchoReportFixture(t, "lc-async-recover-adopt-"+mode)
			f.wrap.adoptMode = mode
			require.Equal(t, store.LaunchReportResultApplied, f.report(t, "run-b", false))
			require.Equal(t, int64(1), staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, store.StaleEchoUnsettledAdoption), f.counters.String())

			assert.Equal(t, store.LaunchReportResultCompleted, f.report(t, "run-b", false))
			row := mustGetAgent(t, f.raw, f.agent.ID)
			assert.Equal(t, "run-b", row.RunID, "the completed replay settles")
			require.NotNil(t, row.EffectiveConfig)
			assert.Equal(t, "run-b", row.EffectiveConfig.RunID)
			assert.Equal(t, int64(1), staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, store.StaleEchoUnsettledAdoption), f.counters.String())
			assert.Equal(t, 1, f.unsettledLines())
			echoed := configuredWithEcho(f.events(t))
			require.Len(t, echoed, 1)
			assert.Equal(t, "run-b", echoed[0].LaunchID)
			assert.Equal(t, "run-m", echoed[0].AdoptedFromLaunchID)
			assert.Equal(t, store.AdoptionSourceCAS, echoed[0].AdoptionSource)
		})
	}
}

// runningObserved makes the launch RunningObserved: a status report of
// running for the minted run before the succeeded report.
func (f *echoReportFixture) runningObserved(t *testing.T) {
	t.Helper()
	require.NoError(t, f.raw.UpdateAgentStatus(context.Background(), f.agent.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseRunning), ReportSource: store.StatusReportSourceRuntime, ReportRunID: "run-m",
	}))
	row := mustGetAgent(t, f.raw, f.agent.ID)
	require.Equal(t, store.LaunchStateEnded, row.LaunchState, "fixture: the running write ends the launch")
	require.Equal(t, store.LaunchEndReasonRunningObserved, row.LaunchEndReason)
}

// TestLifecycleEvents_AsyncRunningObservedAdoptedRunAttributed (WP1 slice
// 3b design v9 section 7; formerly AsyncEchoRunningObservedUnsettled,
// review r2 R1b, which asserted slice 3's unsettled behaviour): the agent
// is seen running before the succeeded report, which names run-b. The
// answer is completed (changed); the row still holds the launch's run, so
// the completed settle adopts run-b, the echo is recorded for run-b with
// the reporting broker, and configured carries launch_id run-b,
// effective_* and the adoption provenance (run-m, cas). Nothing is stale.
func TestLifecycleEvents_AsyncRunningObservedAdoptedRunAttributed(t *testing.T) {
	f := newEchoReportFixture(t, "lc-async-ro")
	f.runningObserved(t)
	assert.Equal(t, store.LaunchReportResultCompleted, f.report(t, "run-b", false))
	row := mustGetAgent(t, f.raw, f.agent.ID)
	assert.Equal(t, "run-b", row.RunID, "slice 3b settles the completed answer")
	require.NotNil(t, row.EffectiveConfig)
	assert.Equal(t, "run-b", row.EffectiveConfig.RunID)
	assert.Equal(t, f.brokerID, row.EffectiveConfig.BrokerID)
	assertNoStaleEcho(t, f.counters)
	assert.Zero(t, f.unsettledLines())
	evs := f.events(t)
	echoed := configuredWithEcho(evs)
	require.Len(t, echoed, 1, lcSeqFull(evs))
	assert.Equal(t, "run-b", echoed[0].LaunchID)
	assert.Equal(t, "model-echo-1", effModel(echoed[0]))
	assert.Equal(t, "run-m", echoed[0].AdoptedFromLaunchID)
	assert.Equal(t, store.AdoptionSourceCAS, echoed[0].AdoptionSource)
}

// TestLifecycleEvents_RunningObservedAfterSyncRestart (review r4 R1): the
// launch is RunningObserved, a sync restart mints run-3, then the late
// succeeded report arrives: stale{run_mismatch} (the row's run-3 is not
// the launch's run-m), no unsettled_adoption line, no configured.
func TestLifecycleEvents_RunningObservedAfterSyncRestart(t *testing.T) {
	f := newEchoReportFixture(t, "lc-async-ro-restart")
	f.runningObserved(t)
	_, err := f.raw.SetAgentRunID(context.Background(), f.agent.ID, "run-3", nil)
	require.NoError(t, err)
	f.report(t, "run-b", false)
	assert.Equal(t, int64(1), staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, store.StaleEchoRunMismatch), f.counters.String())
	assert.Zero(t, staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, store.StaleEchoUnsettledAdoption))
	assert.Zero(t, f.unsettledLines())
	assert.Zero(t, countOf(f.events(t), tc.TransitionConfigured))
}

// TestLifecycleEvents_ReplayAfterSyncRestart (review r3 R1d): the async
// create succeeds and is settled, a sync restart mints run-3, then the
// identical succeeded report is replayed: no new configured,
// stale{launch_report, run_mismatch}, no unsettled_adoption line.
func TestLifecycleEvents_ReplayAfterSyncRestart(t *testing.T) {
	f := newEchoReportFixture(t, "lc-async-replay-restart")
	require.Equal(t, store.LaunchReportResultApplied, f.report(t, "run-m", false))
	_, err := f.raw.SetAgentRunID(context.Background(), f.agent.ID, "run-3", nil)
	require.NoError(t, err)
	f.report(t, "run-m", false)
	assert.Equal(t, int64(1), staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, store.StaleEchoRunMismatch), f.counters.String())
	assert.Zero(t, f.unsettledLines())
	assert.Equal(t, 1, countOf(f.events(t), tc.TransitionConfigured))
}

// TestClassifyStaleEcho is the classifier table (review r5 O1, tele-em
// N1/N2): run_mismatch is refined to unsettled_adoption only when the row
// bound its launch's run, the row's launch is the report's, and the row
// still holds that run. Each negative row fails exactly one conjunct; the
// other store reasons are never refined.
func TestClassifyStaleEcho(t *testing.T) {
	const report = "L"
	unsettled := store.EffectiveConfigResult{StaleReason: store.StaleEchoRunMismatch, RowRunID: "Rm", RowLaunchID: "L", RowLaunchRunID: "Rm"}
	conjuncts := func(r store.EffectiveConfigResult) []bool {
		return []bool{r.RowLaunchRunID != "", r.RowLaunchID == report, r.RowRunID == r.RowLaunchRunID}
	}
	for _, c := range []struct {
		name string
		res  store.EffectiveConfigResult
		want string
	}{
		{"(e) bound, same launch, row still on its run", unsettled, store.StaleEchoUnsettledAdoption},
		{"(a) launch_run_id empty, row run empty, same launch (N1)",
			store.EffectiveConfigResult{StaleReason: store.StaleEchoRunMismatch, RowRunID: "", RowLaunchID: "L", RowLaunchRunID: ""}, store.StaleEchoRunMismatch},
		{"(b) launch_run_id stale from an earlier launch",
			store.EffectiveConfigResult{StaleReason: store.StaleEchoRunMismatch, RowRunID: "R2", RowLaunchID: "L", RowLaunchRunID: "R0"}, store.StaleEchoRunMismatch},
		{"(c) the row's launch is another launch",
			store.EffectiveConfigResult{StaleReason: store.StaleEchoRunMismatch, RowRunID: "Rm", RowLaunchID: "L2", RowLaunchRunID: "Rm"}, store.StaleEchoRunMismatch},
		{"(d) a newer mint: the row's run is not launch_run_id",
			store.EffectiveConfigResult{StaleReason: store.StaleEchoRunMismatch, RowRunID: "R3", RowLaunchID: "L", RowLaunchRunID: "Rm"}, store.StaleEchoRunMismatch},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, classifyStaleEcho(c.res, report))
			if c.want == store.StaleEchoRunMismatch {
				failed := 0
				for _, ok := range conjuncts(c.res) {
					if !ok {
						failed++
					}
				}
				assert.Equal(t, 1, failed, "a negative row must fail exactly one conjunct (N2)")
			}
		})
	}
	for _, reason := range []string{store.StaleEchoGone, store.StaleEchoDeletedOrHeld, store.StaleEchoBrokerMismatch} {
		r := unsettled
		r.StaleReason = reason
		assert.Equal(t, reason, classifyStaleEcho(r, report), "%s is never refined", reason)
	}
}

// TestBrokerEffectiveConfigOf: the one conversion validates every broker
// string (at most 256 bytes, valid UTF-8, printable), omits an invalid one
// rather than truncating it, takes BrokerID from the caller, and reports
// whether an effective-only field survived.
func TestBrokerEffectiveConfigOf(t *testing.T) {
	long := strings.Repeat("m", maxEchoStringBytes+1)
	info := &RemoteAgentInfo{
		RunID: "R1", Template: "t", TemplateHash: "sha256:x", Harness: "claude\x07", Model: long,
		HarnessConfigRevision: "rev\xff", BrokerBuild: `v1 "quoted" \ ok`, Image: "img:1", Profile: "p", Runtime: "docker",
		HarnessConfig: "hc", HarnessConfigSource: "template",
	}
	eff, ok := brokerEffectiveConfigOf(info, "b-target", nil)
	require.True(t, ok)
	assert.Equal(t, "b-target", eff.BrokerID)
	assert.Equal(t, "R1", eff.RunID)
	assert.Equal(t, "sha256:x", eff.TemplateHash)
	assert.Empty(t, eff.Harness, "a control character is omitted")
	assert.Empty(t, eff.Model, "an over-long value is omitted, not truncated")
	assert.Empty(t, eff.HarnessConfigRevision, "invalid UTF-8 is omitted")
	assert.Equal(t, `v1 "quoted" \ ok`, eff.BrokerBuild, "printable punctuation is kept")
	assert.Equal(t, "img:1", eff.Image)

	_, ok = brokerEffectiveConfigOf(&RemoteAgentInfo{RunID: "R1", Template: "t", Model: long}, "b", nil)
	assert.False(t, ok, "only invalid effective-only fields: an older broker's answer")
	_, ok = brokerEffectiveConfigOf(nil, "b", nil)
	assert.False(t, ok)
	assert.True(t, validEchoString(strings.Repeat("é", maxEchoStringBytes/2)))
	assert.False(t, validEchoString("tab\there"))
}

// TestDispatchAgentCreate_AsyncBindsLaunchRun: the real async dispatch
// binds the launch to the run beginRun minted (req.RunID).
func TestDispatchAgentCreate_AsyncBindsLaunchRun(t *testing.T) {
	f := newAsyncLaunchFixture(t, &store.BrokerCapabilities{AsyncLaunch: true})
	agent := f.agent(t, "binds-run", string(state.PhaseCreated), true)
	_, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)
	row := f.row(t, agent.ID)
	require.NotEmpty(t, row.RunID)
	assert.Equal(t, row.RunID, row.LaunchRunID)
	require.Len(t, f.client.sends, 1)
	assert.Equal(t, f.client.sends[0].RunID, row.LaunchRunID)
}

// TestAsyncStartDispatch_BindsLaunchRun: an async start or restart launch
// binds the run beginRun minted for the dispatch (addendum section 1: the
// start-launch caller passes the minted run), so the launch-report
// classifier can tell an unsettled adoption of a start launch.
func TestAsyncStartDispatch_BindsLaunchRun(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		kind, prior string
	}{
		{store.LaunchKindStart, string(state.PhaseStopped)},
		{store.LaunchKindRestart, string(state.PhaseRunning)},
	} {
		t.Run(c.kind, func(t *testing.T) {
			f := newAsyncDispatchFixture(t, "asd-bind-"+c.kind, string(state.PhaseStopped), true, nil, asyncOn)
			res, err := f.d.DispatchAgentStartWithOptions(ctx, f.agent, "", false, StartDispatchOptions{Async: true, Kind: c.kind, PriorPhase: c.prior})
			require.NoError(t, err)
			require.NotNil(t, res.AcceptedLaunch())
			row := getAgent(t, f.s, f.agent.ID)
			require.NotEmpty(t, row.RunID)
			assert.Equal(t, c.kind, row.LaunchKind)
			assert.Equal(t, row.RunID, row.LaunchRunID, "the start launch binds its minted run")
			// The bound run is the run this dispatch sent to the broker, as
			// for a create (TestDispatchAgentCreate_AsyncBindsLaunchRun).
			sent := f.client.sent()
			require.Len(t, sent, 1)
			assert.Equal(t, sent[0].RunID, row.LaunchRunID, "the start launch binds the run sent to the broker")
		})
	}
}
