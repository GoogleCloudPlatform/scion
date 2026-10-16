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

// Design wave 3 phase D3b (ptone/scion#4669), store-backed: the async
// start branch of the dispatcher, of startAgentCore and of its callers, the
// cross-node start, and the D3a follow-ups.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- dispatcher level -------------------------------------------------------

// asyncEchoClient answers starts the way a broker would: "accept" echoes
// an asynchronous launch, "mismatch" echoes another launch ID, "refuse"
// answers 409, anything else answers synchronously. It records each send's
// extras and the agent ID query its context carries.
type asyncEchoClient struct {
	*mockRuntimeBrokerClient
	mode     string
	mu       sync.Mutex
	extras   []StartExtras
	agentIDQ []string
}

func (c *asyncEchoClient) StartAgent(ctx context.Context, _, _, agentID, _, _, _, _, _, _, _ string, _ map[string]string, _ []ResolvedSecret, _ *api.ScionConfig, _ []api.SharedDir, _, _ bool, extras StartExtras) (*RemoteAgentResponse, error) {
	c.mu.Lock()
	c.extras = append(c.extras, extras)
	c.agentIDQ = append(c.agentIDQ, dispatchAgentIDQuery(ctx, ""))
	c.mu.Unlock()
	switch {
	case c.mode == "accept" && extras.AsyncLaunch:
		return &RemoteAgentResponse{LaunchPending: true, LaunchID: extras.LaunchID, LaunchInstanceID: "inst-1"}, nil
	case c.mode == "mismatch" && extras.AsyncLaunch:
		return &RemoteAgentResponse{LaunchPending: true, LaunchID: "someone-else", LaunchInstanceID: "inst-1"}, nil
	case c.mode == "refuse":
		return nil, &brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"code":"conflict","message":"refused"}}`}
	}
	return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: agentID, Name: agentID, Phase: "running"}}, nil
}

func (c *asyncEchoClient) sent() []StartExtras {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]StartExtras(nil), c.extras...)
}

// asyncDispatchFixture is a store with one project, broker and agent, and
// an HTTPAgentDispatcher over asyncEchoClient with async launch on.
type asyncDispatchFixture struct {
	s      store.Store
	d      *HTTPAgentDispatcher
	client *asyncEchoClient
	gen    *fakeMintingTokenGenerator
	agent  *store.Agent // the caller's copy: Phase is the prior phase
}

// newAsyncDispatchFixture makes the agent in prior; with toStarting the row
// then reads starting, as beginStartDispatch leaves it.
func newAsyncDispatchFixture(t *testing.T, name, prior string, toStarting bool, caps *store.BrokerCapabilities, settings AsyncLaunchSettings) *asyncDispatchFixture {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: tid("p-" + name), Name: name, Slug: name}))
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("b-" + name), Name: "b-" + name, Slug: "b-" + name, Endpoint: "http://localhost:9800",
		Status: store.BrokerStatusOnline, Capabilities: caps,
	}))
	a := &store.Agent{
		ID: tid("a-" + name), Name: name, Slug: name, ProjectID: tid("p-" + name), RuntimeBrokerID: tid("b-" + name),
		Phase: prior, AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	require.NoError(t, s.CreateAgent(ctx, a))
	if toStarting {
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}))
	}
	client := &asyncEchoClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, mode: "accept"}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: s}
	d.SetTokenGenerator(gen)
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return settings })
	return &asyncDispatchFixture{s: s, d: d, client: client, gen: gen, agent: a}
}

var asyncOn = AsyncLaunchSettings{Enabled: true, Timeout: 5 * time.Minute, KeepaliveSeconds: 30}

// TestAsyncStartDispatch_AcceptedBeginsStartLaunch pins design D-d: an
// eligible start begins a start launch recording the prior phase and the
// revoke decision, sends the launch fields and the agent ID, and returns the
// accepted launch; the credential it issued is kept.
func TestAsyncStartDispatch_AcceptedBeginsStartLaunch(t *testing.T) {
	ctx := context.Background()
	f := newAsyncDispatchFixture(t, "asd-accept", string(state.PhaseStopped), true, nil, asyncOn)

	res, err := f.d.DispatchAgentStartWithOptions(ctx, f.agent, "", false, StartDispatchOptions{Async: true, Kind: store.LaunchKindStart, PriorPhase: string(state.PhaseStopped)})
	require.NoError(t, err)
	require.NotNil(t, res.AcceptedLaunch())

	row := getAgent(t, f.s, f.agent.ID)
	assert.Equal(t, row.LaunchID, res.Launch.ID)
	assert.Equal(t, "inst-1", res.Launch.Owner)
	assert.Equal(t, store.LaunchStateActive, row.LaunchState)
	assert.Equal(t, store.LaunchKindStart, row.LaunchKind)
	assert.Equal(t, "inst-1", row.LaunchOwner)
	assert.Equal(t, string(state.PhaseStopped), row.LaunchPriorPhase)
	assert.True(t, row.LaunchRevokeOnFail, "a start from a confirmed non-running phase that issued a credential revokes on failure")
	assert.Equal(t, string(state.PhaseStarting), row.Phase, "the launch's reports move the phase on")

	sent := f.client.sent()
	require.Len(t, sent, 1)
	assert.True(t, sent[0].AsyncLaunch)
	assert.Equal(t, row.LaunchID, sent[0].LaunchID)
	assert.Greater(t, sent[0].LaunchTimeoutSeconds, 290)
	assert.LessOrEqual(t, sent[0].LaunchTimeoutSeconds, 300)
	assert.Equal(t, 30, sent[0].LaunchKeepaliveSeconds)
	assert.Equal(t, api.AgentIDQueryParam+"="+url.QueryEscape(f.agent.ID), f.client.agentIDQ[0])

	require.Len(t, f.gen.jtis, 1)
	assert.Nil(t, getTestAgentCredential(t, f.s, f.gen.lastJTI()).RevokedAt, "an accepted start keeps its credential")
}

// TestAsyncStartDispatch_RestartLaunchNeverRevokes pins design L6 at the
// dispatcher: a restart's start leg begins a restart launch (moving a
// stopped row to starting) and never records a revoke, even where the
// synchronous rule would revoke.
func TestAsyncStartDispatch_RestartLaunchNeverRevokes(t *testing.T) {
	ctx := context.Background()
	f := newAsyncDispatchFixture(t, "asd-restart", string(state.PhaseStopped), false, nil, asyncOn)

	res, err := f.d.DispatchAgentStartWithOptions(ctx, f.agent, "", false, StartDispatchOptions{Async: true, Kind: store.LaunchKindRestart, PriorPhase: string(state.PhaseRunning)})
	require.NoError(t, err)
	require.NotNil(t, res.AcceptedLaunch())
	row := getAgent(t, f.s, f.agent.ID)
	assert.Equal(t, store.LaunchKindRestart, row.LaunchKind)
	assert.Equal(t, string(state.PhaseStarting), row.Phase)
	assert.Equal(t, string(state.PhaseRunning), row.LaunchPriorPhase)
	assert.False(t, row.LaunchRevokeOnFail, "a restart launch never revokes")
	require.Len(t, f.gen.jtis, 1, "the start issued a credential (so only the kind keeps the revoke off)")
}

// TestAsyncStartDispatch_NotLaunchedOutcomes pins design D-d's other
// outcomes: an old broker's synchronous answer ends the launch not_launched
// and is a synchronous success; a refusal ends it not_launched and is
// returned; another launch's echo ends it and fails the start.
func TestAsyncStartDispatch_NotLaunchedOutcomes(t *testing.T) {
	for _, c := range []struct {
		mode    string
		wantErr error
		anyErr  bool
	}{
		{mode: "sync"},
		{mode: "refuse", anyErr: true},
		{mode: "mismatch", wantErr: errLaunchEchoMismatch, anyErr: true},
	} {
		t.Run(c.mode, func(t *testing.T) {
			f := newAsyncDispatchFixture(t, "asd-"+c.mode, string(state.PhaseStopped), true, nil, asyncOn)
			f.client.mode = c.mode
			res, err := f.d.DispatchAgentStartWithOptions(context.Background(), f.agent, "", false, StartDispatchOptions{Async: true, Kind: store.LaunchKindStart, PriorPhase: string(state.PhaseStopped)})
			assert.Nil(t, res.AcceptedLaunch(), "not accepted")
			if c.anyErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if c.wantErr != nil {
				assert.ErrorIs(t, err, c.wantErr)
			}
			sent := f.client.sent()
			require.Len(t, sent, 1)
			assert.True(t, sent[0].AsyncLaunch, "the launch fields were sent")
			row := getAgent(t, f.s, f.agent.ID)
			assert.Equal(t, store.LaunchStateEnded, row.LaunchState)
			assert.Equal(t, store.LaunchEndReasonNotLaunched, row.LaunchEndReason)
		})
	}
}

// TestAsyncStartDispatch_Ineligible pins the dispatcher's own rules: no
// asynchronous launch is begun or asked for without opts.Async, with
// hub.async_agent_launch off, with a launch timeout the broker would answer
// synchronously (20 s or less), for a broker whose capabilities lack
// AsyncStart, or when the row is not in a phase a start launch may begin
// on (sent synchronously, D-b). The agent ID is sent on every start.
func TestAsyncStartDispatch_Ineligible(t *testing.T) {
	async := StartDispatchOptions{Async: true, Kind: store.LaunchKindStart, PriorPhase: string(state.PhaseStopped)}
	cases := []struct {
		name       string
		opts       StartDispatchOptions
		settings   AsyncLaunchSettings
		caps       *store.BrokerCapabilities
		toStarting bool
	}{
		{"no async option", StartDispatchOptions{}, asyncOn, nil, true},
		{"launch experiment off", async, AsyncLaunchSettings{Enabled: false, Timeout: 5 * time.Minute}, nil, true},
		{"timeout 20s", async, AsyncLaunchSettings{Enabled: true, Timeout: 20 * time.Second}, nil, true},
		{"broker lacks AsyncStart", async, asyncOn, &store.BrokerCapabilities{AsyncLaunch: true}, true},
		{"row not starting", async, asyncOn, nil, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAsyncDispatchFixture(t, fmt.Sprintf("asd-inel-%d", i), string(state.PhaseStopped), c.toStarting, c.caps, c.settings)
			res, err := f.d.DispatchAgentStartWithOptions(context.Background(), f.agent, "", false, c.opts)
			require.NoError(t, err)
			assert.Nil(t, res, "a synchronous start returns no result")
			sent := f.client.sent()
			require.Len(t, sent, 1)
			assert.False(t, sent[0].AsyncLaunch)
			assert.Empty(t, sent[0].LaunchID)
			assert.Zero(t, sent[0].LaunchTimeoutSeconds)
			assert.Equal(t, api.AgentIDQueryParam+"="+url.QueryEscape(f.agent.ID), f.client.agentIDQ[0], "the agent ID is sent on a synchronous start too")
			assert.Empty(t, getAgent(t, f.s, f.agent.ID).LaunchID, "no launch begun")
		})
	}
}

// TestDispatchAgentStart_AttachesAgentID pins that the plain start entry
// point sends the agent ID (R1: on every start, sync or async).
func TestDispatchAgentStart_AttachesAgentID(t *testing.T) {
	f := newAsyncDispatchFixture(t, "asd-plain", string(state.PhaseStopped), true, nil, asyncOn)
	require.NoError(t, f.d.DispatchAgentStart(context.Background(), f.agent, "", false))
	require.Len(t, f.client.agentIDQ, 1)
	assert.Equal(t, api.AgentIDQueryParam+"="+url.QueryEscape(f.agent.ID), f.client.agentIDQ[0])
	assert.False(t, f.client.sent()[0].AsyncLaunch)
}

// TestDispatchLaunching_CreateParity pins that the shared launch core left
// create unchanged: the request carries the begun launch's fields when the
// transport is called, and each broker answer resolves as before.
func TestDispatchLaunching_CreateParity(t *testing.T) {
	type sendResult struct {
		resp    *RemoteAgentResponse
		envReqs *RemoteEnvRequirementsResponse
		err     error
	}
	cases := []struct {
		name        string
		answer      func(launchID string) sendResult
		wantLaunch  bool
		wantOwner   string
		wantErr     error
		wantEnd     string // "" while active
		wantEnvReqs bool
	}{
		{"accepted", func(id string) sendResult {
			return sendResult{resp: &RemoteAgentResponse{LaunchPending: true, LaunchID: id, LaunchInstanceID: "own"}}
		}, true, "own", nil, "", false},
		{"sync answer", func(string) sendResult { return sendResult{resp: &RemoteAgentResponse{Created: true}} }, false, "", nil, store.LaunchEndReasonNotLaunched, false},
		{"env requirements", func(string) sendResult {
			return sendResult{envReqs: &RemoteEnvRequirementsResponse{}}
		}, false, "", nil, store.LaunchEndReasonNotLaunched, true},
		{"refused", func(string) sendResult {
			return sendResult{err: errParityRefused}
		}, false, "", errParityRefused, store.LaunchEndReasonNotLaunched, false},
		{"echo mismatch", func(string) sendResult {
			return sendResult{resp: &RemoteAgentResponse{LaunchPending: true, LaunchID: "other"}}
		}, false, "", errLaunchEchoMismatch, store.LaunchEndReasonNotLaunched, false},
		{"deferred", func(string) sendResult { return sendResult{err: ErrLifecycleDeferred} }, false, "", ErrLifecycleDeferred, "", false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			f := newAsyncDispatchFixture(t, fmt.Sprintf("asd-create-%d", i), string(state.PhaseCreated), false, nil, asyncOn)
			agent := getAgent(t, f.s, f.agent.ID)
			agent.LaunchAsyncOptIn = true
			req := &RemoteCreateAgentRequest{LaunchID: "stale", AsyncLaunch: true}
			var seen RemoteCreateAgentRequest
			calls := 0
			_, envReqs, launch, err := f.d.dispatchLaunching(ctx, agent, "http://localhost:9800", req, false,
				func(_ context.Context, r *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
					calls++
					seen = *r
					a := c.answer(r.LaunchID)
					return a.resp, a.envReqs, a.err
				})
			require.Equal(t, 1, calls)
			row := getAgent(t, f.s, agent.ID)
			assert.True(t, seen.AsyncLaunch)
			assert.Equal(t, row.LaunchID, seen.LaunchID, "the transport sees the begun launch")
			assert.Greater(t, seen.LaunchTimeoutSeconds, 290)
			assert.Equal(t, 30, seen.LaunchKeepaliveSeconds)
			assert.Equal(t, store.LaunchKindCreate, row.LaunchKind)
			if c.wantErr != nil {
				assert.ErrorIs(t, err, c.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, c.wantEnvReqs, envReqs != nil)
			if c.wantLaunch {
				require.NotNil(t, launch)
				assert.Equal(t, row.LaunchID, launch.ID)
				assert.Equal(t, c.wantOwner, launch.Owner)
				assert.Equal(t, c.wantOwner, row.LaunchOwner)
			} else {
				assert.Nil(t, launch)
			}
			if c.wantEnd == "" {
				assert.Equal(t, store.LaunchStateActive, row.LaunchState)
			} else {
				assert.Equal(t, store.LaunchStateEnded, row.LaunchState)
				assert.Equal(t, c.wantEnd, row.LaunchEndReason)
			}
		})
	}
}

// errParityRefused is the broker refusal TestDispatchLaunching_CreateParity
// sends.
var errParityRefused = &brokerStatusError{StatusCode: 400, Body: "no"}

// ---- hub level --------------------------------------------------------------

// asyncHubDispatcher is a dispatcher that takes start options. An Async
// start is accepted as a broker would accept it (the launch begun and
// marked accepted) when accept is set, and otherwise answered
// synchronously, as by an old broker.
type asyncHubDispatcher struct {
	*claimTestDispatcher
	s      store.Store
	accept bool
	// onAccept, when set, runs after a start is accepted (for example a
	// delete that wins while the start is in flight).
	onAccept func(ctx context.Context, a *store.Agent)
	mu       sync.Mutex
	opts     []StartDispatchOptions
	resumes  []bool
	plain    int
}

func (d *asyncHubDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, resume bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.plain++
	d.resumes = append(d.resumes, resume)
	return nil
}

func (d *asyncHubDispatcher) DispatchAgentStartWithOptions(ctx context.Context, a *store.Agent, _ string, resume bool, opts StartDispatchOptions) (*StartDispatchResult, error) {
	d.mu.Lock()
	d.opts = append(d.opts, opts)
	d.resumes = append(d.resumes, resume)
	d.mu.Unlock()
	if !opts.Async || !d.accept {
		return nil, nil
	}
	id, err := d.s.BeginStartLaunch(ctx, a.ID, opts.launchKind(), 5*time.Minute, store.BeginLaunchOptions{PriorPhase: opts.PriorPhase})
	if err != nil {
		return nil, err
	}
	if _, err := d.s.MarkLaunchAccepted(ctx, a.ID, id, "inst-1"); err != nil {
		return nil, err
	}
	if d.onAccept != nil {
		d.onAccept(ctx, a)
	}
	return &StartDispatchResult{Launch: &LaunchAccepted{ID: id, Owner: "inst-1"}}, nil
}

func (d *asyncHubDispatcher) calls() ([]StartDispatchOptions, int, []bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]StartDispatchOptions(nil), d.opts...), d.plain, append([]bool(nil), d.resumes...)
}

// setAsyncStartExperiment sets hub.async_agent_start's default on srv's
// registry.
func setAsyncStartExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.AsyncAgentStart {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.AsyncAgentStart))
}

// seedAsyncStartGate sets the all-live-hubs answer as a fresh refresh would.
func seedAsyncStartGate(srv *Server, value bool) {
	g := &srv.asyncStartGate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.value = value
	g.refreshedAt = asyncStartGateNow()
	g.logged = true
}

// newAsyncStartFixture is newClaimFixture (start claims on) with an
// asyncHubDispatcher, the experiment on, the hub-node check passing and
// the fixture broker as the project's default.
func newAsyncStartFixture(t *testing.T) (*reconcileFixture, *asyncHubDispatcher) {
	t.Helper()
	f, cd, _ := newClaimFixture(t)
	d := &asyncHubDispatcher{claimTestDispatcher: cd, s: f.s, accept: true}
	f.srv.SetDispatcher(d)
	setAsyncStartExperiment(t, f.srv, true)
	seedAsyncStartGate(f.srv, true)
	ctx := context.Background()
	p, err := f.s.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	p.DefaultRuntimeBrokerID = f.brokerID
	require.NoError(t, f.s.UpdateProject(ctx, p))
	return f, d
}

// lifecycleWithBody posts a lifecycle action with a JSON body.
func lifecycleWithBody(t *testing.T, f *reconcileFixture, id, action string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+id+"/"+action, body)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// launchOf returns the "launch" object of a response body.
func launchOf(body map[string]interface{}) map[string]interface{} {
	if a, ok := body["agent"].(map[string]interface{}); ok {
		body = a
	}
	l, _ := body["launch"].(map[string]interface{})
	return l
}

// requireHandedOff checks the row an accepted start leaves: phase starting,
// an active launch of kind with prior phase prior, and the start claim
// still held and linked to that launch.
func requireHandedOff(t *testing.T, s store.Store, id, kind, prior string) *store.Agent {
	t.Helper()
	row := getAgent(t, s, id)
	assert.Equal(t, string(state.PhaseStarting), row.Phase, "no started status is written")
	assert.Equal(t, store.LaunchStateActive, row.LaunchState)
	assert.Equal(t, kind, row.LaunchKind)
	assert.Equal(t, prior, row.LaunchPriorPhase)
	assert.NotEmpty(t, row.StartClaimID, "the claim is handed to the launch, not released")
	assert.Equal(t, row.LaunchID, row.StartClaimLaunchID, "the claim is linked to the launch")
	return row
}

// TestAsyncStart_LifecycleStartAccepted pins design D-d/D-AC2: an opted-in
// lifecycle start of a stopped agent is answered 200 with the agent in
// starting and its active start launch; the claim is handed to the launch
// and no started status is written.
func TestAsyncStart_LifecycleStartAccepted(t *testing.T) {
	f, d := newAsyncStartFixture(t)
	a := f.addAgent("as-lc", string(state.PhaseStopped), "")

	code, body := lifecycleWithBody(t, f, a.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, string(state.PhaseStarting), body["phase"])
	l := launchOf(body)
	require.NotNil(t, l, "the answer carries the launch")
	assert.Equal(t, store.LaunchKindStart, l["kind"])
	assert.Equal(t, true, l["active"])

	opts, plain, _ := d.calls()
	require.Len(t, opts, 1)
	assert.Equal(t, 0, plain)
	assert.Equal(t, StartDispatchOptions{Async: true, Kind: store.LaunchKindStart, PriorPhase: string(state.PhaseStopped)}, opts[0])
	requireHandedOff(t, f.s, a.ID, store.LaunchKindStart, string(state.PhaseStopped))
}

// TestAsyncStart_SynchronousWhenIneligible pins design D-AC5/D-AC9 at the
// hub: with the experiment off, without the opt-in, while a live hub node
// lacks async_start_v1, or for a start of a running agent, the start is
// sent synchronously and completes as before. An old broker (an Async
// start answered synchronously) is a synchronous success too.
func TestAsyncStart_SynchronousWhenIneligible(t *testing.T) {
	optIn := map[string]bool{"acceptAsyncLaunch": true}
	cases := []struct {
		name      string
		phase     state.Phase
		body      interface{}
		setup     func(f *reconcileFixture, d *asyncHubDispatcher)
		wantAsync bool // an Async option reached the dispatcher (answered synchronously)
	}{
		{"experiment off", state.PhaseStopped, optIn, func(f *reconcileFixture, _ *asyncHubDispatcher) { setAsyncStartExperiment(t, f.srv, false) }, false},
		{"no opt-in", state.PhaseStopped, nil, nil, false},
		{"hub node lacks async_start_v1", state.PhaseStopped, optIn, func(f *reconcileFixture, _ *asyncHubDispatcher) { seedAsyncStartGate(f.srv, false) }, false},
		{"running agent", state.PhaseRunning, optIn, nil, false},
		{"old broker", state.PhaseStopped, optIn, func(_ *reconcileFixture, d *asyncHubDispatcher) { d.accept = false }, true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, d := newAsyncStartFixture(t)
			if c.setup != nil {
				c.setup(f, d)
			}
			a := f.addAgent(fmt.Sprintf("as-sync-%d", i), string(c.phase), "")
			code, body := lifecycleWithBody(t, f, a.ID, "start", c.body)
			require.Equal(t, http.StatusOK, code, body)
			opts, plain, _ := d.calls()
			if c.wantAsync {
				require.Len(t, opts, 1)
				assert.True(t, opts[0].Async)
			} else {
				assert.Empty(t, opts, "no asynchronous start asked for")
				assert.Equal(t, 1, plain)
			}
			row := getAgent(t, f.s, a.ID)
			assert.Equal(t, string(state.PhaseRunning), row.Phase, "the synchronous start wrote running")
			assert.Empty(t, row.LaunchID, "no launch")
			assert.Empty(t, row.StartClaimID, "the claim is released")
		})
	}
}

// TestAsyncStart_LifecycleRestartAccepted pins design D-AC3: an opted-in
// restart runs its stop leg synchronously, then its start leg is accepted
// as a restart launch; the answer is 200 starting with the launch, and no
// stopped or running status is written.
func TestAsyncStart_LifecycleRestartAccepted(t *testing.T) {
	f, d := newAsyncStartFixture(t)
	a := f.addAgent("as-restart", string(state.PhaseRunning), "")

	code, body := lifecycleWithBody(t, f, a.ID, "restart", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, int32(1), d.stops.Load(), "the stop leg ran")
	l := launchOf(body)
	require.NotNil(t, l)
	assert.Equal(t, store.LaunchKindRestart, l["kind"])

	opts, _, _ := d.calls()
	require.Len(t, opts, 1)
	assert.Equal(t, store.LaunchKindRestart, opts[0].Kind)
	requireHandedOff(t, f.s, a.ID, store.LaunchKindRestart, string(state.PhaseRunning))
}

// TestAsyncStart_CreateEndpointAccepted pins design D-a's opt-in on the
// create endpoint (handleExistingAgent): the released CLIs' acceptAsyncLaunch
// makes the suspended branch (resume), the resume-in-place branch and the
// provisioned branch (for an error row that remembers created, R3) answer
// 200 with the launching agent. The first start of a created agent (a
// counted phase, so the dispatch writes no starting) stays synchronous
// (TestAsyncStart_CreateEndpointCreatedStaysSync).
func TestAsyncStart_CreateEndpointAccepted(t *testing.T) {
	cases := []struct {
		name       string
		phase      state.Phase
		memory     bool
		resume     bool
		wantResume bool
	}{
		{"suspended (Branch 1)", state.PhaseSuspended, false, false, true},
		{"stopped resume in place (Branch 2)", state.PhaseStopped, false, true, false},
		{"created memory (Branch 4)", state.PhaseCreated, true, false, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, d := newAsyncStartFixture(t)
			a := f.addAgent(fmt.Sprintf("as-create-%d", i), string(c.phase), "")
			if c.memory {
				a = seedResumeMemory(t, f.s, a, string(c.phase))
			}
			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: a.Slug, ProjectID: f.projectID, Resume: c.resume, AcceptAsyncLaunch: true,
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			l := launchOf(body)
			require.NotNil(t, l, "the answer carries the launch")
			assert.Equal(t, true, l["active"])
			opts, plain, resumes := d.calls()
			require.Len(t, opts, 1)
			assert.Equal(t, 0, plain)
			assert.Equal(t, []bool{c.wantResume}, resumes)
			requireHandedOff(t, f.s, a.ID, store.LaunchKindStart, string(c.phase))
		})
	}
}

// TestAsyncStart_DeleteWonDuringAcceptedStart pins review r1 O3: a delete
// that claims or removes the row while an asynchronous start is being
// accepted answers 409 delete_in_progress, as a synchronous start does, on
// lifecycle start, lifecycle restart and the create endpoint; with no
// delete the answer is the launching agent.
func TestAsyncStart_DeleteWonDuringAcceptedStart(t *testing.T) {
	type entry struct {
		name  string
		phase state.Phase
		send  func(t *testing.T, f *reconcileFixture, a *store.Agent) (int, string)
	}
	entries := []entry{
		{"lifecycle start", state.PhaseStopped, func(t *testing.T, f *reconcileFixture, a *store.Agent) (int, string) {
			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", map[string]bool{"acceptAsyncLaunch": true})
			return rec.Code, rec.Body.String()
		}},
		{"lifecycle restart", state.PhaseRunning, func(t *testing.T, f *reconcileFixture, a *store.Agent) (int, string) {
			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", map[string]bool{"acceptAsyncLaunch": true})
			return rec.Code, rec.Body.String()
		}},
		{"create endpoint resume", state.PhaseSuspended, func(t *testing.T, f *reconcileFixture, a *store.Agent) (int, string) {
			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: a.Slug, ProjectID: f.projectID, AcceptAsyncLaunch: true})
			return rec.Code, rec.Body.String()
		}},
	}
	deletes := []struct {
		name  string
		apply func(t *testing.T, s store.Store, id string)
		won   bool
	}{
		{"none", func(*testing.T, store.Store, string) {}, false},
		{"delete-claimed", func(t *testing.T, s store.Store, id string) {
			claimForTest(t, s, id, store.DeletionStateDeleting, time.Minute)
		}, true},
		{"hard-deleted", func(t *testing.T, s store.Store, id string) {
			require.NoError(t, s.DeleteAgent(context.Background(), id))
		}, true},
	}
	for i, e := range entries {
		for j, del := range deletes {
			t.Run(e.name+"/"+del.name, func(t *testing.T) {
				f, d := newAsyncStartFixture(t)
				a := f.addAgent(fmt.Sprintf("as-dw-%d-%d", i, j), string(e.phase), "")
				d.onAccept = func(_ context.Context, a *store.Agent) { del.apply(t, f.s, a.ID) }
				code, body := e.send(t, f, a)
				opts, _, _ := d.calls()
				require.Len(t, opts, 1, "the start was accepted asynchronously")
				if !del.won {
					require.Equal(t, http.StatusOK, code, body)
					assert.Contains(t, body, `"launch"`)
					return
				}
				require.Equal(t, http.StatusConflict, code, body)
				assert.Contains(t, body, ErrCodeDeleteInProgress)
			})
		}
	}
}

// TestAsyncStart_CreateEndpointCreatedStaysSync pins design D-b's phase
// rule on the create endpoint: the first start of a created agent (a
// counted phase) is synchronous even when opted in.
func TestAsyncStart_CreateEndpointCreatedStaysSync(t *testing.T) {
	f, d := newAsyncStartFixture(t)
	a := f.addAgent("as-create-first", string(state.PhaseCreated), "")
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: a.Slug, ProjectID: f.projectID, AcceptAsyncLaunch: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	opts, plain, _ := d.calls()
	assert.Empty(t, opts)
	assert.Equal(t, 1, plain)
	row := getAgent(t, f.s, a.ID)
	assert.Equal(t, string(state.PhaseRunning), row.Phase)
	assert.Empty(t, row.LaunchID)
}

// ---- cross-node -------------------------------------------------------------

// asyncOwnerDispatcher is the owner node's dispatcher: it takes start
// options and accepts an Async start as asyncHubDispatcher does.
type asyncOwnerDispatcher struct {
	landingOwnerDispatcher
	s    store.Store
	mu   sync.Mutex
	opts []StartDispatchOptions
}

func (d *asyncOwnerDispatcher) DispatchAgentStartWithOptions(ctx context.Context, a *store.Agent, task string, resume bool, opts StartDispatchOptions) (*StartDispatchResult, error) {
	d.mu.Lock()
	d.opts = append(d.opts, opts)
	d.mu.Unlock()
	if !opts.Async {
		return nil, d.DispatchAgentStart(ctx, a, task, resume)
	}
	id, err := d.s.BeginStartLaunch(ctx, a.ID, opts.launchKind(), 5*time.Minute, store.BeginLaunchOptions{PriorPhase: opts.PriorPhase})
	if err != nil {
		return nil, err
	}
	if _, err := d.s.MarkLaunchAccepted(ctx, a.ID, id, "owner-inst"); err != nil {
		return nil, err
	}
	return &StartDispatchResult{Launch: &LaunchAccepted{ID: id, Owner: "owner-inst"}}, nil
}

// TestAsyncStart_CrossNode pins design D-d (H3) and D-AC7: an opted-in start
// whose broker another node owns is sent to the owner with Async, Kind and
// PriorPhase; the owner's accepted launch, carried in the done row, ends the
// requester's wait, and the requester answers with the launching agent and
// hands its claim to the launch. An owner without start options runs the
// start synchronously and the requester waits for the phase as before.
func TestAsyncStart_CrossNode(t *testing.T) {
	setLifecycleTimings(t, 3*time.Second, 50*time.Millisecond, 0)
	for _, ownerAsync := range []bool{true, false} {
		t.Run(fmt.Sprintf("owner async %v", ownerAsync), func(t *testing.T) {
			srv, s := testServer(t)
			ownerDisp, bus := newCrossNodeLandingServer(t, srv, s)
			requester := srv.GetDispatcher().(*HTTPAgentDispatcher)
			requester.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return asyncOn })
			setAsyncStartExperiment(t, srv, true)
			seedAsyncStartGate(srv, true)

			agent := setupBrokerAgentInPhase(t, s, fmt.Sprintf("xn-async-%v", ownerAsync), state.PhaseStopped)
			var od *asyncOwnerDispatcher
			if ownerAsync {
				od = &asyncOwnerDispatcher{landingOwnerDispatcher: landingOwnerDispatcher{bus: bus}, s: s}
				ownerSrv := ownerOf(t, requester)
				ownerSrv.SetDispatcher(od)
			} else {
				ownerDisp.onLand = func(_ context.Context, a *store.Agent) {
					running := *a
					running.Phase = string(state.PhaseRunning)
					bus.PublishAgentStatus(context.Background(), &running)
				}
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", map[string]bool{"acceptAsyncLaunch": true})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			if !ownerAsync {
				assert.Nil(t, launchOf(body))
				row := getAgent(t, s, agent.ID)
				assert.Equal(t, string(state.PhaseRunning), row.Phase, "the synchronous owner's start completes as before")
				// The requester began a launch, then its client deferred
				// (no route check here): it ended that launch before
				// handing off, so no launch is left for the reaper.
				require.NotEmpty(t, row.LaunchID)
				assert.Equal(t, store.LaunchStateEnded, row.LaunchState)
				assert.Equal(t, store.LaunchEndReasonNotLaunched, row.LaunchEndReason)
				return
			}
			od.mu.Lock()
			require.Len(t, od.opts, 1)
			assert.Equal(t, StartDispatchOptions{Async: true, Kind: store.LaunchKindStart, PriorPhase: string(state.PhaseStopped)}, od.opts[0])
			od.mu.Unlock()
			l := launchOf(body)
			require.NotNil(t, l, "the owner's launch reaches the requester's answer")
			row := getAgent(t, s, agent.ID)
			assert.Equal(t, row.LaunchID, l["id"])
			assert.Equal(t, store.LaunchStateActive, row.LaunchState)
			assert.Equal(t, string(state.PhaseStarting), row.Phase, "the requester writes no started status")
			assert.Equal(t, row.LaunchID, row.StartClaimLaunchID, "the requester's claim is linked to the owner's launch")
		})
	}
}

// ownerOf returns the owner Server behind the requester's cross-node bus
// (newCrossNodeLandingServer).
func ownerOf(t *testing.T, d *HTTPAgentDispatcher) *Server {
	t.Helper()
	bus, ok := d.commandBus.(ownerSignalBus)
	require.True(t, ok, "the requester signals an owner node")
	return bus.owner
}

// ---- D3a follow-ups ---------------------------------------------------------

// postAgentStatusAsAgentRun posts a status report with an agent token of
// runID.
func postAgentStatusAsAgentRun(t *testing.T, srv *Server, agent *store.Agent, runID, body string) map[string]interface{} {
	t.Helper()
	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)
	token, _, err := tokenSvc.SignAgentToken(AgentTokenGrant{AgentID: agent.ID, ProjectID: agent.ProjectID, Scopes: []AgentTokenScope{ScopeAgentStatusUpdate}}, runID)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/status", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Scion-Agent-Token", token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// TestAsyncStartGuard0f_NoopReason pins the D3a follow-up: while a start
// launch is active, a phase-only report whose token names no run, or a run
// other than the row's (recorded with SetAgentRunID), is answered
// {"applied":false,"reason":"start_launch_other_run"} and changes nothing; a
// report of the row's run applies.
func TestAsyncStartGuard0f_NoopReason(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("g0fr-project"), Slug: "g0fr-project", Name: "G0fr", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	a := &store.Agent{
		ID: tid("g0fr"), Slug: "g0fr", Name: "g0fr", ProjectID: project.ID,
		Phase: string(state.PhaseStopped), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, a))
	_, err := s.SetAgentRunID(ctx, a.ID, "run-new", nil)
	require.NoError(t, err)
	require.Equal(t, "run-new", getAgent(t, s, a.ID).RunID)
	beginTestStartLaunch(t, s, a, store.LaunchKindStart, "stopped", false)
	const report = `{"phase":"stopped","activity":"completed","message":"Agent shutting down"}`

	for _, run := range []string{"", "run-old"} {
		out := postAgentStatusAsAgentRun(t, srv, a, run, report)
		assert.Equal(t, false, out["applied"], "run %q", run)
		assert.Equal(t, statusUpdateReasonStartLaunchOtherRun, out["reason"], "run %q", run)
		assert.Equal(t, string(state.PhaseStarting), getAgent(t, s, a.ID).Phase)
	}
	out := postAgentStatusAsAgentRun(t, srv, a, "run-new", `{"phase":"running","activity":"working"}`)
	assert.Equal(t, true, out["applied"])
	assert.Equal(t, string(state.PhaseRunning), getAgent(t, s, a.ID).Phase, "the launched run's own report applies")
}

// TestAsyncStartExecDispatchStart_PlainDispatcherError pins the D3a
// follow-up: an owner whose dispatcher takes no start options returns its
// synchronous start's error.
func TestAsyncStartExecDispatchStart_PlainDispatcherError(t *testing.T) {
	f, _, a := newClaimFixture(t)
	args, err := MarshalDispatchArgs(&StartDispatchArgs{Async: true})
	require.NoError(t, err)
	row := store.BrokerDispatch{ID: "d-plain-err", BrokerID: f.brokerID, AgentID: a.ID, ProjectID: f.projectID, Op: "start", Args: args}
	pd := &plainStartDispatcher{lastErr: fmt.Errorf("x: %w", errStartRequestNotSent)}
	f.srv.SetDispatcher(pd)
	res, err := f.srv.execDispatchStart(context.Background(), row)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dispatch start:")
	assert.ErrorIs(t, err, errStartRequestNotSent)
	assert.Empty(t, res)
	assert.Equal(t, 1, pd.starts)
}

// TestAsyncStartExecDispatchStart_PassesKindAndPriorPhase pins that the
// owner hands the intent's Kind and PriorPhase to the dispatcher.
func TestAsyncStartExecDispatchStart_PassesKindAndPriorPhase(t *testing.T) {
	f, _, a := newClaimFixture(t)
	args, err := MarshalDispatchArgs(&StartDispatchArgs{Async: true, Kind: store.LaunchKindRestart, PriorPhase: "running"})
	require.NoError(t, err)
	row := store.BrokerDispatch{ID: "d-kind", BrokerID: f.brokerID, AgentID: a.ID, ProjectID: f.projectID, Op: "start", Args: args}
	od := &optionsProbeDispatcher{}
	f.srv.SetDispatcher(od)
	_, err = f.srv.execDispatchStart(context.Background(), row)
	require.NoError(t, err)
	assert.Equal(t, StartDispatchOptions{Async: true, Kind: store.LaunchKindRestart, PriorPhase: "running"}, od.lastOpts)
}

// ---- D2 wire contract -------------------------------------------------------

// d2Broker is an HTTP broker answering a start as the D2 broker does
// (agreed with as-d2-dev): it takes the asynchronous branch only when the
// agentId query parameter is present and the body carries asyncLaunch, a
// launchId and launchTimeoutSeconds above 20, and then answers 202
// {launchPending, launchId (echo), launchInstanceId} with no agent;
// otherwise it answers the synchronous 202 {agent, created:false}.
type d2Broker struct {
	mu     sync.Mutex
	asyncs int
	syncs  int
	query  url.Values
	body   map[string]interface{}
}

func (b *d2Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.query, b.body = r.URL.Query(), body
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	timeout, _ := body["launchTimeoutSeconds"].(float64)
	launchID, _ := body["launchId"].(string)
	if r.URL.Query().Get(api.AgentIDQueryParam) != "" && body["asyncLaunch"] == true && launchID != "" && timeout > 20 {
		b.asyncs++
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"launchPending": true, "launchId": launchID, "launchInstanceId": "d2-inst"})
		return
	}
	b.syncs++
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"created": false, "agent": map[string]interface{}{"id": "x", "name": "x", "phase": "running"}})
}

// TestAsyncStartD2Contract pins the hub side of the start contract agreed
// with D2 (the broker's async startAgent) over the real HTTP transport: the
// URL carries agentId; an async start's body carries asyncLaunch, launchId,
// launchTimeoutSeconds (> 20) and launchKeepaliveSeconds; the broker's 202
// launchPending echo is an accepted launch owned by launchInstanceId; its
// synchronous 202 (no launchPending) ends the launch not_launched and is a
// synchronous success; and the broker's capability names asyncStart (JSON)
// and "asyncStart" (registration list) are read as AsyncStart.
func TestAsyncStartD2Contract(t *testing.T) {
	ctx := context.Background()
	for _, async := range []bool{true, false} {
		t.Run(fmt.Sprintf("async %v", async), func(t *testing.T) {
			b := &d2Broker{}
			srv := httptest.NewServer(b)
			defer srv.Close()
			name := fmt.Sprintf("d2c-%v", async)
			s := createTestStore(t)
			require.NoError(t, s.CreateProject(ctx, &store.Project{ID: tid("p-" + name), Name: name, Slug: name}))
			require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: tid("b-" + name), Name: "b-" + name, Slug: "b-" + name, Endpoint: srv.URL, Status: store.BrokerStatusOnline,
				Capabilities: &store.BrokerCapabilities{AsyncLaunch: true, AsyncStart: true},
			}))
			a := &store.Agent{ID: tid("a-" + name), Name: name, Slug: name, ProjectID: tid("p-" + name), RuntimeBrokerID: tid("b-" + name),
				Phase: string(state.PhaseSuspended), AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"}}
			require.NoError(t, s.CreateAgent(ctx, a))
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}))
			d := NewHTTPAgentDispatcherWithClient(s, NewHTTPRuntimeBrokerClient(), false, slog.Default())
			d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return asyncOn })

			res, err := d.DispatchAgentStartWithOptions(ctx, a, "", true, StartDispatchOptions{Async: async, Kind: store.LaunchKindStart, PriorPhase: string(state.PhaseSuspended)})
			require.NoError(t, err)
			b.mu.Lock()
			defer b.mu.Unlock()
			assert.Equal(t, a.ID, b.query.Get(api.AgentIDQueryParam), "agentId on every start")
			assert.Equal(t, true, b.body["resume"])
			row := getAgent(t, s, a.ID)
			if !async {
				assert.Equal(t, 1, b.syncs)
				assert.Nil(t, b.body["asyncLaunch"], "no launch fields on a synchronous start")
				assert.Nil(t, b.body["launchId"])
				assert.Nil(t, res)
				assert.Empty(t, row.LaunchID)
				return
			}
			require.Equal(t, 1, b.asyncs, "the broker took its async branch")
			assert.Equal(t, row.LaunchID, b.body["launchId"])
			assert.Greater(t, b.body["launchTimeoutSeconds"].(float64), float64(20))
			assert.Equal(t, float64(30), b.body["launchKeepaliveSeconds"])
			require.NotNil(t, res.AcceptedLaunch())
			assert.Equal(t, "d2-inst", res.Launch.Owner)
			assert.Equal(t, "d2-inst", row.LaunchOwner)
			assert.Equal(t, store.LaunchStateActive, row.LaunchState)
		})
	}

	t.Run("sync 202 answers an async request", func(t *testing.T) {
		// A runtime that cannot launch asynchronously (D2: substrate,
		// cloudrun) answers the plain 202 even to an async request.
		f := newAsyncDispatchFixture(t, "d2c-syncanswer", string(state.PhaseStopped), true, nil, asyncOn)
		f.client.mode = "sync"
		res, err := f.d.DispatchAgentStartWithOptions(ctx, f.agent, "", false, StartDispatchOptions{Async: true, Kind: store.LaunchKindStart, PriorPhase: string(state.PhaseStopped)})
		require.NoError(t, err)
		assert.Nil(t, res.AcceptedLaunch())
		assert.Equal(t, store.LaunchEndReasonNotLaunched, getAgent(t, f.s, f.agent.ID).LaunchEndReason)
	})

	t.Run("capability names", func(t *testing.T) {
		assert.True(t, capabilitiesFromStrings([]string{"asyncStart"}).AsyncStart, "registration name asyncStart")
		var caps store.BrokerCapabilities
		require.NoError(t, json.Unmarshal([]byte(`{"asyncStart":true}`), &caps))
		assert.True(t, caps.AsyncStart, "heartbeat/info JSON field asyncStart")
	})
}
