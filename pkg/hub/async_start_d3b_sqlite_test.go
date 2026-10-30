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
// cross-node start, the Guard 0f no-op reason and the plain-dispatcher
// start error.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// beginStartLaunchFailStore fails BeginStartLaunch with err.
type beginStartLaunchFailStore struct {
	store.Store
	err error
}

func (s beginStartLaunchFailStore) BeginStartLaunch(context.Context, string, string, time.Duration, store.BeginLaunchOptions) (string, error) {
	return "", s.err
}

// deleteHoldBeginStore places a real delete hold on the row inside
// BeginStartLaunch, after recording the run the dispatch minted, and then
// refuses the launch as the store does for a row out of phase
// (store.ErrInvalidPhase); beginStartLaunch then builds the real delete-held
// refusal.
type deleteHoldBeginStore struct {
	store.Store
	t      *testing.T
	minted string
}

func (s *deleteHoldBeginStore) BeginStartLaunch(ctx context.Context, agentID, _ string, _ time.Duration, _ store.BeginLaunchOptions) (string, error) {
	a, err := s.GetAgent(ctx, agentID)
	require.NoError(s.t, err)
	s.minted = a.RunID
	claimForTest(s.t, s.Store, agentID, store.DeletionStateDeleting, time.Minute)
	return "", store.ErrInvalidPhase
}

// TestAsyncStartDispatch_BeginStartLaunchFailureDidNotHappen pins the
// rule of errStartRunNotBegun (as for a run not begun): when
// BeginStartLaunch fails after the run was begun, nothing was sent, so the
// error is errStartRunNotBegun: the claim releases (startReleased) and the
// start counts as not happened (startDidNotHappen), for a start and for a
// restart's start leg. A delete-held row's error (ErrLaunchInvalidPhase
// wrapping store.ErrDeleteInProgress) is released for the claim but never
// counts as not happened, with or without the wrap: the delete engine owns
// the row's intent.
func TestAsyncStartDispatch_BeginStartLaunchFailureDidNotHappen(t *testing.T) {
	for _, kind := range []string{store.LaunchKindStart, store.LaunchKindRestart} {
		t.Run(kind, func(t *testing.T) {
			f := newAsyncDispatchFixture(t, "asd-blfail-"+kind, string(state.PhaseStopped), true, nil, asyncOn)
			d := NewHTTPAgentDispatcherWithClient(beginStartLaunchFailStore{Store: f.s, err: errors.New("db down")}, f.client, false, slog.Default())
			d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return asyncOn })
			res, err := d.DispatchAgentStartWithOptions(context.Background(), f.agent, "", false, StartDispatchOptions{Async: true, Kind: kind, PriorPhase: string(state.PhaseStopped)})
			require.Error(t, err)
			assert.Nil(t, res)
			assert.ErrorIs(t, err, errStartRunNotBegun, "a pre-send store failure is a run not begun")
			assert.Equal(t, startReleased, startOutcomeOf(err), "the claim releases a start that never left the hub")
			assert.True(t, startDidNotHappen(err), "the start did not happen")
			assert.Empty(t, f.client.sent(), "nothing was sent")
		})
	}
	// A delete-held refusal still reverts the minted run, which never
	// launched, so the row does not keep a run the runtime never held.
	t.Run("delete held reverts the minted run", func(t *testing.T) {
		f := newAsyncDispatchFixture(t, "asd-blfail-held", string(state.PhaseStopped), true, nil, asyncOn)
		before := getAgent(t, f.s, f.agent.ID).RunID
		hs := &deleteHoldBeginStore{Store: f.s, t: t}
		d := NewHTTPAgentDispatcherWithClient(hs, f.client, false, slog.Default())
		d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return asyncOn })
		_, err := d.DispatchAgentStartWithOptions(context.Background(), f.agent, "", false, StartDispatchOptions{Async: true, Kind: store.LaunchKindStart, PriorPhase: string(state.PhaseStopped)})
		require.ErrorIs(t, err, ErrLaunchInvalidPhase)
		require.ErrorIs(t, err, store.ErrDeleteInProgress, "the real delete-held refusal")
		require.NotEmpty(t, hs.minted, "the dispatch minted a run")
		require.NotEqual(t, before, hs.minted)
		assert.True(t, getAgent(t, f.s, f.agent.ID).DeletionHoldsRow(time.Now()), "the row is delete-held")
		after := getAgent(t, f.s, f.agent.ID).RunID
		assert.Equal(t, before, after, "the row returns to the previous run under a delete hold")
	})
	bare := fmt.Errorf("%w: %w", ErrLaunchInvalidPhase, store.ErrDeleteInProgress)
	held := fmt.Errorf("%w: %w", errStartRunNotBegun, bare)
	assert.False(t, startDidNotHappen(bare), "a delete-held refusal is not a start that did not happen")
	assert.False(t, startDidNotHappen(held), "nor with the wrap")
	assert.Equal(t, startReleased, startOutcomeOf(bare))
	assert.Equal(t, startReleased, startOutcomeOf(held), "the claim is still released")
}

// deleteHeldStartDispatcher claims a delete on the row while the start is
// in flight, then refuses the start as a launch begin does for a
// delete-held row (ErrLaunchInvalidPhase wrapping store.ErrDeleteInProgress).
type deleteHeldStartDispatcher struct {
	*claimTestDispatcher
	t *testing.T
	s store.Store
}

func (d *deleteHeldStartDispatcher) DispatchAgentStart(_ context.Context, a *store.Agent, _ string, _ bool) error {
	claimForTest(d.t, d.s, a.ID, store.DeletionStateDeleting, time.Minute)
	return fmt.Errorf("%w: %w: %w", ErrLaunchInvalidPhase, store.ErrInvalidPhase, store.ErrDeleteInProgress)
}

// TestStartDidNotHappen_DeleteHeldKeepsIntent pins the delete-hold rule: a
// start refused because a delete holds the row (ErrLaunchInvalidPhase
// wrapping store.ErrDeleteInProgress) leaves the run intent to the delete
// engine. A provisioned agent's start runs no provisioned settle (intent
// unchanged, no message), and a queued-stop agent's start runs no
// ptone/scion#5070 revert (intent unchanged, stop_queued untouched).
func TestStartDidNotHappen_DeleteHeldKeepsIntent(t *testing.T) {
	cases := []struct {
		name   string
		phase  state.Phase
		mutate func(a *store.Agent)
	}{
		{"provisioned", state.PhaseCreated, nil},
		{"queued stop", state.PhaseStopped, func(a *store.Agent) {
			a.ContainerStatus = containerStatusStopQueued
			a.Message = offlineStopMessage
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, cd, _ := newClaimFixture(t)
			f.srv.SetDispatcher(&deleteHeldStartDispatcher{claimTestDispatcher: cd, t: t, s: f.s})
			var muts []func(a *store.Agent)
			if c.mutate != nil {
				muts = append(muts, c.mutate)
			}
			a := f.addAgent(fmt.Sprintf("dh-%d", i), string(c.phase), "", muts...)
			before := getAgent(t, f.s, a.ID)
			err := f.srv.startAgentCore(context.Background(), before, StartOpts{Kind: store.StartClaimUser})
			require.Error(t, err)
			assert.False(t, startDidNotHappen(err))
			got := getAgent(t, f.s, a.ID)
			assert.Equal(t, store.RunIntentRunning, got.RunIntent, "the delete engine owns the intent: no revert to stopped")
			assert.Equal(t, before.Message, got.Message, "no settle message")
			if c.mutate != nil {
				assert.Equal(t, containerStatusStopQueued, got.ContainerStatus, "stop_queued is left to the delete")
			}
		})
	}
}

// TestAsyncStart_BeginStartLaunchFailureSettlesAtStartAgentCore pins the
// same rule at the start path: an async start whose BeginStartLaunch
// fails after the run was begun (nothing was sent) is a start that did not
// happen, so startAgentCore settles it as it settles any refused start: a
// provisioned agent (a created memory row) goes back to rest (intent
// stopped, the failure message), and a queued-stop agent gets its stopped
// intent back (ptone/scion#5070). The run the dispatch minted is reverted.
func TestAsyncStart_BeginStartLaunchFailureSettlesAtStartAgentCore(t *testing.T) {
	cases := []struct {
		name    string
		mk      func(t *testing.T, f *reconcileFixture) *store.Agent
		wantMsg func(t *testing.T, msg string)
	}{
		{"provisioned (created memory)", func(t *testing.T, f *reconcileFixture) *store.Agent {
			return newD9Agent(t, f, "bsl-prov", d9Prior{"created memory", state.PhaseCreated, state.PhaseCreated})
		}, func(t *testing.T, msg string) {
			assert.True(t, strings.HasPrefix(msg, "Start failed: "), "message: %q", msg)
			assert.True(t, strings.HasSuffix(msg, provisionedRestingNote), "message: %q", msg)
		}},
		{"queued stop", func(t *testing.T, f *reconcileFixture) *store.Agent {
			return f.addAgent("bsl-queued", string(state.PhaseStopped), "", func(a *store.Agent) {
				a.ContainerStatus = containerStatusStopQueued
				a.Message = offlineStopMessage
			})
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newAsyncStartFixture(t)
			ctx := context.Background()
			b, err := f.s.GetRuntimeBroker(ctx, f.brokerID)
			require.NoError(t, err)
			b.Endpoint = "http://localhost:9800"
			require.NoError(t, f.s.UpdateRuntimeBroker(ctx, b))
			client := &asyncEchoClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, mode: "accept"}
			d := NewHTTPAgentDispatcherWithClient(beginStartLaunchFailStore{Store: f.s, err: errors.New("db down")}, client, false, slog.Default())
			d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return asyncOn })
			f.srv.SetDispatcher(d)
			a := c.mk(t, f)
			before := getAgent(t, f.s, a.ID)

			err = f.srv.startAgentCore(ctx, before, StartOpts{Kind: store.StartClaimUser, AcceptAsyncLaunch: true})
			require.Error(t, err)
			require.ErrorIs(t, err, errStartRunNotBegun)
			require.ErrorContains(t, err, "db down")
			assert.Empty(t, client.sent(), "nothing was sent")
			got := getAgent(t, f.s, a.ID)
			assert.Equal(t, store.RunIntentStopped, got.RunIntent, "the refused start is settled back to the stopped intent")
			assert.Equal(t, before.RunID, got.RunID, "the minted run is reverted")
			assert.Empty(t, got.StartClaimID, "the claim is released")
			if c.wantMsg != nil {
				c.wantMsg(t, got.Message)
			} else {
				// The queued-stop revert (ptone/scion#5070) writes the
				// refused-start message and keeps the stop queued.
				assert.Equal(t, refusedStartMessage(err), got.Message, "the refused-start message")
				assert.True(t, strings.HasPrefix(got.Message, "Start failed: "), "message: %q", got.Message)
				assert.True(t, strings.HasSuffix(got.Message, "The agent stays stopped."), "message: %q", got.Message)
				assert.Equal(t, containerStatusStopQueued, got.ContainerStatus, "the stop stays queued")
			}
		})
	}
}

// TestDispatchAgentStart_AttachesAgentID pins that the plain start entry
// point sends the agent ID (on every start, sync or async).
func TestDispatchAgentStart_AttachesAgentID(t *testing.T) {
	f := newAsyncDispatchFixture(t, "asd-plain", string(state.PhaseStopped), true, nil, asyncOn)
	require.NoError(t, f.d.DispatchAgentStart(context.Background(), f.agent, "", false))
	require.Len(t, f.client.agentIDQ, 1)
	assert.Equal(t, api.AgentIDQueryParam+"="+url.QueryEscape(f.agent.ID), f.client.agentIDQ[0], "the plain start sends the agent ID")
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
	// failSync, when set, is what a synchronous start returns.
	failSync error
	// launchTimeout is the accepted launch's budget (default 5 minutes).
	launchTimeout time.Duration
	mu            sync.Mutex
	opts          []StartDispatchOptions
	resumes       []bool
	plain         int
}

func (d *asyncHubDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, resume bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.plain++
	d.resumes = append(d.resumes, resume)
	return d.failSync
}

func (d *asyncHubDispatcher) DispatchAgentStartWithOptions(ctx context.Context, a *store.Agent, _ string, resume bool, opts StartDispatchOptions) (*StartDispatchResult, error) {
	d.mu.Lock()
	d.opts = append(d.opts, opts)
	d.resumes = append(d.resumes, resume)
	d.mu.Unlock()
	if !opts.Async || !d.accept {
		return nil, nil
	}
	timeout := d.launchTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	id, err := d.s.BeginStartLaunch(ctx, a.ID, opts.launchKind(), timeout, store.BeginLaunchOptions{PriorPhase: opts.PriorPhase})
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
	assert.Equal(t, string(state.PhaseStarting), body["phase"], "the accepted start answers phase starting")
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
	require.Equal(t, http.StatusOK, code, "the accepted restart answers 200: %v", body)
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
// provisioned branch (for an error row that remembers created, design R3) answer
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

// TestAsyncStart_DeleteWonDuringAcceptedStart pins that a delete
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

// statusRecorder records the agent status events published, with the
// phase and launch state each carried.
type statusRecorder struct {
	EventPublisher
	mu     sync.Mutex
	events []store.Agent
}

func (r *statusRecorder) PublishAgentStatus(ctx context.Context, a *store.Agent) {
	r.mu.Lock()
	r.events = append(r.events, *a)
	r.mu.Unlock()
	r.EventPublisher.PublishAgentStatus(ctx, a)
}

func (r *statusRecorder) forAgent(id string) []store.Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []store.Agent
	for _, a := range r.events {
		if a.ID == id {
			out = append(out, a)
		}
	}
	return out
}

// recordStatusEvents installs a statusRecorder on srv.
func recordStatusEvents(t *testing.T, srv *Server) *statusRecorder {
	t.Helper()
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	r := &statusRecorder{EventPublisher: bus}
	srv.events = r
	return r
}

// TestAsyncStart_AcceptedPublishesLaunchingAgent pins that an
// accepted start publishes the re-read row (phase starting, its active
// launch) at once, so watchers do not wait for the launch's first report;
// that a row a delete has claimed meanwhile publishes
// nothing.
func TestAsyncStart_AcceptedPublishesLaunchingAgent(t *testing.T) {
	f, d := newAsyncStartFixture(t)
	rec := recordStatusEvents(t, f.srv)
	a := f.addAgent("as-pub", string(state.PhaseStopped), "")
	code, body := lifecycleWithBody(t, f, a.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	events := rec.forAgent(a.ID)
	require.NotEmpty(t, events, "the acceptance publishes the agent")
	last := events[len(events)-1]
	assert.Equal(t, string(state.PhaseStarting), last.Phase, "the published row is the launching one")
	assert.Equal(t, store.LaunchStateActive, last.LaunchState)

	held := f.addAgent("as-pub-held", string(state.PhaseStopped), "")
	d.onAccept = func(_ context.Context, a *store.Agent) {
		claimForTest(t, f.s, a.ID, store.DeletionStateDeleting, time.Minute)
	}
	code, body = lifecycleWithBody(t, f, held.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusConflict, code, body)
	assert.Empty(t, rec.forAgent(held.ID), "a delete-held row publishes nothing on acceptance")
}

// TestAsyncStart_HandoffSetsContainerStarting pins ptone/scion#5266: a
// start that supersedes a queued stop and is handed off to an asynchronous
// launch clears the queued-stop notice and marks the container starting
// (not running, and not stop_queued).
func TestAsyncStart_HandoffSetsContainerStarting(t *testing.T) {
	f, _ := newAsyncStartFixture(t)
	a := f.addAgent("as-queued", string(state.PhaseStopped), "", func(a *store.Agent) {
		a.ContainerStatus = containerStatusStopQueued
		a.Message = offlineStopMessage
	})
	code, body := lifecycleWithBody(t, f, a.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	row := requireHandedOff(t, f.s, a.ID, store.LaunchKindStart, string(state.PhaseStopped))
	assert.Empty(t, row.Message, "the queued-stop notice is cleared")
	assert.Equal(t, containerStatusStarting, row.ContainerStatus, "the handed-off container is marked starting")
}

// TestAsyncStart_FailedHandoffReleasesSlot pins ptone/scion#5266: after a
// start that superseded a queued stop is handed off and its launch fails,
// the agent no longer holds broker capacity and its slot is released, also
// after the reservation sweep. While the launch is live it holds the slot
// through its phase (starting).
func TestAsyncStart_FailedHandoffReleasesSlot(t *testing.T) {
	f, _ := newAsyncStartFixture(t)
	setBrokerAgentCeiling(t, f.s, 5)
	a := f.addAgent("as-queued-fail", string(state.PhaseStopped), "", func(a *store.Agent) {
		a.ContainerStatus = containerStatusStopQueued
		a.Message = offlineStopMessage
	})
	code, body := lifecycleWithBody(t, f, a.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	row := requireHandedOff(t, f.s, a.ID, store.LaunchKindStart, string(state.PhaseStopped))
	assert.True(t, agentHoldsBrokerCapacity(row), "a live launch holds capacity through its phase")
	assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the slot is held while the launch is live")

	rec := postLaunchReport(t, f.srv, f.brokerID, a.ID, f.brokerID, AgentLaunchReport{
		LaunchID: row.LaunchID, InstanceID: "inst-1", State: "failed", ErrorCode: "boom",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), store.LaunchReportResultApplied)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "the failed start returns to its prior phase")
	assert.NotEqual(t, containerStatusStopQueued, got.ContainerStatus)
	assert.False(t, agentHoldsBrokerCapacity(got), "a failed hand-off no longer holds broker capacity")
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the failed launch released the slot")

	f.srv.ReconcileStaleBrokerQuotaReservations(context.Background())
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the sweep does not restore it")
}

// startEnd is the part of an agent row D-T9 compares between a failed
// synchronous start and a failed asynchronous start.
type startEnd struct {
	phase     string
	intent    store.RunIntent
	message   string
	reserved  bool
	resumeMem string
}

func readStartEnd(t *testing.T, f *reconcileFixture, id string) startEnd {
	t.Helper()
	a := getAgent(t, f.s, id)
	return startEnd{
		phase: a.Phase, intent: a.RunIntent, message: a.Message,
		reserved:  hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, id),
		resumeMem: a.LaunchResumePhase,
	}
}

// d9Prior is one D-T9 prior: the agent's phase, and the phase a failed
// start remembers (resume memory) when the row starts as an error row
// that remembers it ("" for a plain row).
type d9Prior struct {
	name   string
	phase  state.Phase
	memory state.Phase
}

var d9Priors = []d9Prior{
	{"stopped", state.PhaseStopped, ""},
	{"error", state.PhaseError, ""},
	{"suspended", state.PhaseSuspended, ""},
	{"halted", state.PhaseHalted, ""},
	{"created memory (retry)", state.PhaseCreated, state.PhaseCreated},
	{"provisioning memory (retry)", state.PhaseProvisioning, state.PhaseProvisioning},
	{"suspended memory (retry)", state.PhaseSuspended, state.PhaseSuspended},
	{"halted memory (retry)", state.PhaseHalted, state.PhaseHalted},
}

// newD9Agent makes the prior's row: a plain row in its phase, or an error
// row remembering the memory phase (seedResumeMemory).
func newD9Agent(t *testing.T, f *reconcileFixture, slug string, p d9Prior) *store.Agent {
	t.Helper()
	if p.memory == "" {
		return f.addAgent(slug, string(p.phase), "")
	}
	return seedResumeMemory(t, f.s, f.addAgent(slug, string(p.memory), ""), string(p.memory))
}

// TestAsyncStart_D9SyncAsyncParity checks sync/async parity, including a
// retry from a resume-memory row: for each prior phase, a failed
// synchronous start and a failed asynchronous start (an applied failed
// report) leave the agent with
//   - the phase D-c assigns: the synchronous rollback restores the stored
//     phase; the asynchronous end restores stopped or error, and ends error
//     with resume memory for a suspended, halted, created or provisioning
//     prior (D-c failure phase, the memory carried forward on a retry);
//   - the same run intent: stopped for a provisioned prior (created or
//     provisioning, directly or remembered), which also gets the same
//     message shape, "Start failed: <the failure>. <provisionedRestingNote>";
//     running otherwise, since a failed start is still a start asked for;
//   - the same reservation state: released (every prior here is uncounted).
func TestAsyncStart_D9SyncAsyncParity(t *testing.T) {
	refused := fmt.Errorf("broker refused: %w", errStartRequestNotSent)
	for i, p := range d9Priors {
		t.Run(p.name, func(t *testing.T) {
			f, d := newAsyncStartFixture(t)
			setBrokerAgentCeiling(t, f.s, 10)
			d.failSync = refused

			syncA := newD9Agent(t, f, fmt.Sprintf("d9-sync-%d", i), p)
			code, body := lifecycleWithBody(t, f, syncA.ID, "start", nil)
			require.NotEqual(t, http.StatusOK, code, "the synchronous start failed: %v", body)
			syncEnd := readStartEnd(t, f, syncA.ID)

			asyncA := newD9Agent(t, f, fmt.Sprintf("d9-async-%d", i), p)
			code, body = lifecycleWithBody(t, f, asyncA.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
			require.Equal(t, http.StatusOK, code, body)
			row := getAgent(t, f.s, asyncA.ID)
			require.Equal(t, store.LaunchStateActive, row.LaunchState)
			rec := postLaunchReport(t, f.srv, f.brokerID, asyncA.ID, f.brokerID, AgentLaunchReport{
				LaunchID: row.LaunchID, InstanceID: "inst-1", State: "failed", ErrorCode: "boom", Step: "pull", Message: "image pull failed",
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), store.LaunchReportResultApplied)
			asyncEnd := readStartEnd(t, f, asyncA.ID)

			// Phase, per D-c.
			wantSync := string(state.PhaseError)
			if p.memory == "" {
				wantSync = string(p.phase)
			}
			assert.Equal(t, wantSync, syncEnd.phase, "the synchronous rollback restores the stored phase")
			wantAsync, wantMem := string(state.PhaseError), string(p.phase)
			if p.phase == state.PhaseStopped || (p.phase == state.PhaseError && p.memory == "") {
				wantAsync, wantMem = string(p.phase), ""
			}
			assert.Equal(t, wantAsync, asyncEnd.phase, "the asynchronous end phase follows D-c")
			assert.Equal(t, wantMem, asyncEnd.resumeMem, "the asynchronous end's resume memory follows D-c")

			// Run intent and message shape.
			provisioned := p.phase == state.PhaseCreated || p.phase == state.PhaseProvisioning
			assert.Equal(t, syncEnd.intent, asyncEnd.intent, "sync and async leave the same run intent")
			if provisioned {
				assert.Equal(t, store.RunIntentStopped, asyncEnd.intent, "a failed async start of a provisioned agent returns it to rest")
				for _, e := range []startEnd{syncEnd, asyncEnd} {
					assert.True(t, strings.HasPrefix(e.message, "Start failed: "), "message shape: %q", e.message)
					assert.True(t, strings.HasSuffix(e.message, provisionedRestingNote), "message shape: %q", e.message)
				}
				assert.Contains(t, asyncEnd.message, "pull: image pull failed", "the async message names the failure")
			} else {
				assert.Equal(t, store.RunIntentRunning, asyncEnd.intent)
			}

			// Reservation.
			assert.Equal(t, syncEnd.reserved, asyncEnd.reserved, "sync and async leave the same reservation state")
			assert.False(t, asyncEnd.reserved, "the failed launch released the slot")
		})
	}
}

// failAsyncStart starts a's async start (opted in, accepted) and fails it
// with an applied failed report carrying step and message, and returns the
// row the failure left.
func failAsyncStart(t *testing.T, f *reconcileFixture, a *store.Agent, step, message string) *store.Agent {
	t.Helper()
	code, body := lifecycleWithBody(t, f, a.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	row := getAgent(t, f.s, a.ID)
	require.Equal(t, store.LaunchStateActive, row.LaunchState)
	rec := postLaunchReport(t, f.srv, f.brokerID, a.ID, f.brokerID, AgentLaunchReport{
		LaunchID: row.LaunchID, InstanceID: "inst-1", State: "failed", ErrorCode: "boom", Step: step, Message: message,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), store.LaunchReportResultApplied)
	return getAgent(t, f.s, a.ID)
}

// reapAsyncStart starts a's async start with a launch budget of 1 ms and
// runs the launch reaper until it ends the launch; returns the row.
func reapAsyncStart(t *testing.T, f *reconcileFixture, d *asyncHubDispatcher, a *store.Agent) *store.Agent {
	t.Helper()
	d.launchTimeout = time.Millisecond
	defer func() { d.launchTimeout = 0 }()
	code, body := lifecycleWithBody(t, f, a.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	require.Eventually(t, func() bool {
		f.srv.launchReaperTickHandler()(context.Background())
		return getAgent(t, f.s, a.ID).LaunchState == store.LaunchStateEnded
	}, 10*time.Second, 50*time.Millisecond, "the reaper ends the launch past its deadline")
	return getAgent(t, f.s, a.ID)
}

// TestAsyncStart_ReapedProvisionedStartRests pins the reaper side of the
// D-c run-intent hook, and that a retry from a created memory row
// whose previous async attempt failed (so the row carries that attempt's
// "Start failed" message) and which the reaper times out returns the agent
// to rest with a message naming the timeout, exactly once, not nesting the
// previous failure.
func TestAsyncStart_ReapedProvisionedStartRests(t *testing.T) {
	f, d := newAsyncStartFixture(t)
	a := newD9Agent(t, f, "d9-reaped", d9Prior{"created memory", state.PhaseCreated, state.PhaseCreated})
	first := failAsyncStart(t, f, a, "pull", "image pull failed.")
	require.Equal(t, "Start failed: pull: image pull failed. "+provisionedRestingNote, first.Message, "one period, from the failed report")
	require.Equal(t, store.RunIntentStopped, first.RunIntent)

	events := recordStatusEvents(t, f.srv)
	got := reapAsyncStart(t, f, d, a)
	assert.Equal(t, store.LaunchEndReasonTimedOut, got.LaunchEndReason)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent, "a reaped provisioned start returns the agent to rest")
	assert.Equal(t, "Start failed: "+store.LaunchErrorLaunchTimeout+". "+provisionedRestingNote, got.Message,
		"the reaped retry names its own timeout, once")
	pub := events.forAgent(a.ID)
	require.NotEmpty(t, pub)
	last := pub[len(pub)-1]
	assert.Equal(t, got.Message, last.Message, "watchers get the settled row, not the reaped copy")
	assert.Equal(t, store.RunIntentStopped, last.RunIntent)
}

// TestAsyncStart_FailedReportPublishesOnce pins the report path's
// publish-once rule: an applied failed report whose
// start launch the run-intent hook settles publishes exactly one status
// event for the agent, the settled row (settleFailedProvisionedStart
// publishes it), not also the pre-settle row.
func TestAsyncStart_FailedReportPublishesOnce(t *testing.T) {
	f, _ := newAsyncStartFixture(t)
	a := newD9Agent(t, f, "d9-pubonce", d9Prior{"created memory", state.PhaseCreated, state.PhaseCreated})
	code, body := lifecycleWithBody(t, f, a.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	row := getAgent(t, f.s, a.ID)
	events := recordStatusEvents(t, f.srv)
	rec := postLaunchReport(t, f.srv, f.brokerID, a.ID, f.brokerID, AgentLaunchReport{
		LaunchID: row.LaunchID, InstanceID: "inst-1", State: "failed", ErrorCode: "boom", Step: "pull", Message: "image pull failed",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	pub := events.forAgent(a.ID)
	require.Len(t, pub, 1, "exactly one status event for the settled report")
	assert.Equal(t, store.RunIntentStopped, pub[0].RunIntent, "the event carries the settled row")
	assert.Equal(t, "Start failed: pull: image pull failed. "+provisionedRestingNote, pub[0].Message)
}

// newerIntentOnRevertStore records a newer run intent for agentID just
// before the first RevertRunIntent once its fault switch is armed (a
// one-shot), as a start recorded between a failed report's write and the
// run-intent hook's revert would. Transparent until armed.
type newerIntentOnRevertStore struct {
	store.Store
	fault   *storeFaultSwitch
	agentID string
	fired   atomic.Bool
}

func (s *newerIntentOnRevertStore) RevertRunIntent(ctx context.Context, agentID string, from store.RunIntent, at time.Time, to store.RunIntent) (bool, error) {
	if s.fault.Active() && agentID == s.agentID && s.fired.CompareAndSwap(false, true) {
		if _, _, err := s.SwapRunIntent(ctx, agentID, store.RunIntentRunning); err != nil {
			return false, err
		}
	}
	return s.Store.RevertRunIntent(ctx, agentID, from, at, to)
}

// TestAsyncStart_FailedReportLostSettlePublishesOnce pins the other side of
// the report path's publish-once rule: when the
// settle's revert loses (a newer start recorded its intent after the
// report's write), the handler publishes exactly one event, the store's
// updated row, and the newer intent is kept.
func TestAsyncStart_FailedReportLostSettlePublishesOnce(t *testing.T) {
	f, _ := newAsyncStartFixture(t)
	wrap, fault := installStoreFault(t, f.srv, func(inner store.Store, fs *storeFaultSwitch) *newerIntentOnRevertStore {
		return &newerIntentOnRevertStore{Store: inner, fault: fs}
	})
	a := newD9Agent(t, f, "d9-pubonce-lost", d9Prior{"created memory", state.PhaseCreated, state.PhaseCreated})
	wrap.agentID = a.ID
	code, body := lifecycleWithBody(t, f, a.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	row := getAgent(t, f.s, a.ID)
	events := recordStatusEvents(t, f.srv)
	fault.Arm()
	rec := postLaunchReport(t, f.srv, f.brokerID, a.ID, f.brokerID, AgentLaunchReport{
		LaunchID: row.LaunchID, InstanceID: "inst-1", State: "failed", ErrorCode: "boom", Step: "pull", Message: "image pull failed",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, wrap.fired.Load(), "the newer intent was recorded at the revert")
	pub := events.forAgent(a.ID)
	require.Len(t, pub, 1, "exactly one status event when the settle loses")
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent, "the newer intent is kept")
	assert.Equal(t, got.Message, pub[0].Message, "the event is the store's updated row")
	assert.Equal(t, "pull: image pull failed", pub[0].Message)
	assert.Equal(t, store.RunIntentRunning, pub[0].RunIntent, "the event carries the newer intent")
}

// TestAsyncStart_ReapedProvisionedStartPublishesOnce pins the reaper's
// publish-once rule: a reaper end of a provisioned
// start launch that the hook settles publishes exactly one event for the
// agent, the settled row.
func TestAsyncStart_ReapedProvisionedStartPublishesOnce(t *testing.T) {
	f, d := newAsyncStartFixture(t)
	a := newD9Agent(t, f, "d9-reap-once", d9Prior{"created memory", state.PhaseCreated, state.PhaseCreated})
	d.launchTimeout = time.Millisecond
	code, body := lifecycleWithBody(t, f, a.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	events := recordStatusEvents(t, f.srv)
	require.Eventually(t, func() bool {
		f.srv.launchReaperTickHandler()(context.Background())
		return getAgent(t, f.s, a.ID).LaunchState == store.LaunchStateEnded
	}, 10*time.Second, 50*time.Millisecond)
	pub := events.forAgent(a.ID)
	require.Len(t, pub, 1, "exactly one status event for the reaped, settled launch")
	assert.Equal(t, store.RunIntentStopped, pub[0].RunIntent)
	assert.Equal(t, "Start failed: "+store.LaunchErrorLaunchTimeout+". "+provisionedRestingNote, pub[0].Message)
}

// TestAsyncStart_FailedReportWithoutMessageUsesLaunchError pins the
// fallback: a failed report with no step and no message
// leaves "Start failed: <its launch error>. <note>".
func TestAsyncStart_FailedReportWithoutMessageUsesLaunchError(t *testing.T) {
	f, _ := newAsyncStartFixture(t)
	a := newD9Agent(t, f, "d9-nomsg", d9Prior{"created memory", state.PhaseCreated, state.PhaseCreated})
	got := failAsyncStart(t, f, a, "", "")
	assert.Equal(t, store.RunIntentStopped, got.RunIntent)
	assert.Equal(t, "Start failed: boom. "+provisionedRestingNote, got.Message, "the launch error names the failure")
}

// TestAsyncStart_LateFailedReportAfterReap pins that a
// failed report that arrives after the reaper ended the start launch (the
// store refines the ended launch and answers applied) never settles the
// run intent again, so a newer start's intent is kept; while the reap's
// settle still holds (intent stopped), the refined broker detail is put
// back in the failure message shape.
func TestAsyncStart_LateFailedReportAfterReap(t *testing.T) {
	late := func(t *testing.T, f *reconcileFixture, row *store.Agent) {
		t.Helper()
		rec := postLaunchReport(t, f.srv, f.brokerID, row.ID, f.brokerID, AgentLaunchReport{
			LaunchID: row.LaunchID, InstanceID: "inst-1", State: "failed", ErrorCode: "boom", Step: "pull", Message: "image pull failed",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), store.LaunchReportResultApplied, "the store refines the ended launch")
	}
	t.Run("newer start intent kept", func(t *testing.T) {
		f, d := newAsyncStartFixture(t)
		a := newD9Agent(t, f, "d9-late-newer", d9Prior{"created memory", state.PhaseCreated, state.PhaseCreated})
		row := reapAsyncStart(t, f, d, a)
		require.Equal(t, store.RunIntentStopped, row.RunIntent)
		// A newer start records its intent (as a synchronous start whose
		// dispatch failed ambiguously leaves it).
		fresh := getAgent(t, f.s, a.ID)
		_, err := f.srv.recordRunIntent(context.Background(), fresh, store.RunIntentRunning)
		require.NoError(t, err)
		newer := getAgent(t, f.s, a.ID)
		require.Equal(t, store.RunIntentRunning, newer.RunIntent)

		late(t, f, row)
		got := getAgent(t, f.s, a.ID)
		assert.Equal(t, store.RunIntentRunning, got.RunIntent, "a late report for the old launch keeps the newer start's intent")
		assert.Equal(t, "pull: image pull failed", got.Message, "with a newer start the refined detail is left as the broker wrote it")
		require.NotNil(t, got.RunIntentAt)
		assert.True(t, got.RunIntentAt.Equal(*newer.RunIntentAt), "the newer intent's time is unchanged")
	})
	t.Run("message shape restored", func(t *testing.T) {
		f, d := newAsyncStartFixture(t)
		a := newD9Agent(t, f, "d9-late-shape", d9Prior{"created memory", state.PhaseCreated, state.PhaseCreated})
		row := reapAsyncStart(t, f, d, a)
		events := recordStatusEvents(t, f.srv)
		late(t, f, row)
		got := getAgent(t, f.s, a.ID)
		assert.Equal(t, store.RunIntentStopped, got.RunIntent)
		assert.Equal(t, "Start failed: pull: image pull failed. "+provisionedRestingNote, got.Message,
			"the refined detail is put back in the failure message shape")
		pub := events.forAgent(a.ID)
		require.NotEmpty(t, pub)
		assert.Equal(t, got.Message, pub[len(pub)-1].Message, "the rewritten message is published")
	})
	t.Run("newer phase keeps its message", func(t *testing.T) {
		// The reapply write is conditional on the phase it read: a start
		// that moved the row on in between keeps its message.
		f, d := newAsyncStartFixture(t)
		a := newD9Agent(t, f, "d9-late-phase", d9Prior{"created memory", state.PhaseCreated, state.PhaseCreated})
		row := reapAsyncStart(t, f, d, a)
		stale := *row
		stale.Message = "pull: image pull failed"
		require.NoError(t, f.s.UpdateAgentStatus(context.Background(), a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting), Message: "a newer start"}))
		f.srv.reapplyStartLaunchFailureMessage(context.Background(), &stale)
		assert.Equal(t, "a newer start", getAgent(t, f.s, a.ID).Message, "a row that moved on keeps the newer message")
	})
}

// TestStartLaunchFailureMessage_OldShapedMessage pins that a
// failed end that left an older failure message (already in the shape) in
// place names its launch error, never nesting the old message.
func TestStartLaunchFailureMessage_OldShapedMessage(t *testing.T) {
	a := &store.Agent{
		LaunchEndReason: store.LaunchEndReasonFailed, LaunchError: "boom",
		Message: "Start failed: pull: image pull failed. " + provisionedRestingNote,
	}
	assert.Equal(t, "Start failed: boom. "+provisionedRestingNote, startLaunchFailureMessage(a), "an old shaped message is not nested")
	a.Message = "pull: image pull failed."
	assert.Equal(t, "Start failed: pull: image pull failed. "+provisionedRestingNote, startLaunchFailureMessage(a))
	// A reaper end names its launch error, whatever message
	// the row carries (here a broker progress message).
	reaped := &store.Agent{LaunchEndReason: store.LaunchEndReasonTimedOut, LaunchError: store.LaunchErrorLaunchTimeout, Message: "pulling image"}
	assert.Equal(t, "Start failed: "+store.LaunchErrorLaunchTimeout+". "+provisionedRestingNote, startLaunchFailureMessage(reaped), "a reaper end names its launch error, not the row's message")
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

// ---- Guard 0f reason and plain-dispatcher error -----------------------------

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

// TestAsyncStartGuard0f_NoopReason pins the no-op reason: while a start
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
		assert.Equal(t, false, out["applied"], "a report of another run is not applied (token run "+run+")")
		assert.Equal(t, statusUpdateReasonStartLaunchOtherRun, out["reason"], "the reason names the other run (token run "+run+")")
		assert.Equal(t, string(state.PhaseStarting), getAgent(t, s, a.ID).Phase)
	}
	out := postAgentStatusAsAgentRun(t, srv, a, "run-new", `{"phase":"running","activity":"working"}`)
	assert.Equal(t, true, out["applied"])
	assert.Equal(t, string(state.PhaseRunning), getAgent(t, s, a.ID).Phase, "the launched run's own report applies")
}

// TestAsyncStartExecDispatchStart_PlainDispatcherError pins the plain
// dispatcher case: an owner whose dispatcher takes no start options returns
// its synchronous start's error.
func TestAsyncStartExecDispatchStart_PlainDispatcherError(t *testing.T) {
	f, _, a := newClaimFixture(t)
	args, err := MarshalDispatchArgs(&StartDispatchArgs{Async: true})
	require.NoError(t, err)
	row := store.BrokerDispatch{ID: "d-plain-err", BrokerID: f.brokerID, AgentID: a.ID, ProjectID: f.projectID, Op: "start", Args: args}
	pd := &plainStartDispatcher{lastErr: fmt.Errorf("x: %w", errStartRequestNotSent)}
	f.srv.SetDispatcher(pd)
	res, err := f.srv.execDispatchStart(context.Background(), row)
	require.Error(t, err, "the plain dispatcher's error is returned")
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
	assert.Equal(t, StartDispatchOptions{Async: true, Kind: store.LaunchKindRestart, PriorPhase: "running"}, od.lastOpts, "the owner passes the intent's Kind and PriorPhase")
}

// ---- Broker start wire contract ---------------------------------------------

// d2Broker is an HTTP broker answering a start as the broker does (matching
// the broker start contract): it takes the asynchronous branch only when the
// agentId query parameter is present and the body carries asyncLaunch, a
// launchId and launchTimeoutSeconds above 20, and then answers 202
// {launchPending, launchId (echo), launchInstanceId} with no agent; otherwise
// it answers the synchronous 202 {agent, created:false}.
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

// TestAsyncStartD2Contract pins the hub side of the start contract shared
// with the broker (its async startAgent) over the real HTTP transport: the
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
		// A runtime that cannot launch asynchronously (substrate,
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

// TestAsyncStartHeartbeatGuard_OldContainerIgnored pins design wave 3
// D-AC6 / D-T5, the broker-heartbeat half (the self-report half is
// TestAsyncStartGuard0f_*): during an asynchronous start, the broker's
// heartbeat entry for the OLD container (the previous run, reporting
// stopped) does not change the agent's phase or end the launch, the launch
// still completes, and after it an old-run heartbeat does not move the
// running agent either. Two guards cover it:
//   - the live start claim (heartbeatPhaseGuarded/heartbeatGuardingClaim)
//     defers any uncounted heartbeat phase while the launch holds it;
//   - the store's run check (AgentStatusUpdate.ReportRunID) writes only
//     last-seen for a heartbeat entry of another run.
//
// Case (a), an old-run entry during the launch, is covered by both, so it
// holds if either guard alone is removed. Case (b), an entry with no run
// ID (an older broker), is covered by the claim alone, and case (d), an
// old-run entry after the launch, by the run check alone.
func TestAsyncStartHeartbeatGuard_OldContainerIgnored(t *testing.T) {
	f, d := newAsyncStartFixture(t)
	const oldRun, newRun = "run-old-container", "run-new-container"
	a := f.addAgent("as-hb-old", string(state.PhaseStopped), "")
	// The previous run, as its own start recorded it (CreateAgent does not
	// store a run ID).
	_, err := f.s.SetAgentRunID(context.Background(), a.ID, oldRun, nil)
	require.NoError(t, err)
	require.Equal(t, oldRun, getAgent(t, f.s, a.ID).RunID)
	// The start's run, as the real dispatcher's beginRun records it before
	// the send.
	d.onAccept = func(ctx context.Context, ag *store.Agent) {
		_, err := f.s.SetAgentRunID(ctx, ag.ID, newRun, nil)
		require.NoError(t, err)
	}

	code, body := lifecycleWithBody(t, f, a.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	row := requireHandedOff(t, f.s, a.ID, store.LaunchKindStart, string(state.PhaseStopped))
	require.Equal(t, newRun, row.RunID)
	require.Equal(t, store.StartClaimLive, row.StartClaimState, "the launch holds a live start claim")
	launchID := row.LaunchID

	stoppedEntry := func(runID string) {
		t.Helper()
		f.send(brokerHeartbeatRequest{
			Status: store.BrokerStatusOnline,
			Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{{
				Slug: a.Slug, Phase: string(state.PhaseStopped), ContainerStatus: "Exited (0)",
				RuntimeTarget: "docker", RunID: runID,
			}}}},
		})
	}
	requireLaunching := func(what string) {
		t.Helper()
		got := getAgent(t, f.s, a.ID)
		assert.Equal(t, string(state.PhaseStarting), got.Phase, "%s: the phase stays starting", what)
		assert.Equal(t, store.LaunchStateActive, got.LaunchState, "%s: the launch stays active", what)
		assert.Equal(t, launchID, got.LaunchID, "%s: the same launch", what)
		assert.Equal(t, store.StartClaimLive, got.StartClaimState, "%s: the claim stays live", what)
	}

	// (a) The old container's stopped entry during the launch.
	stoppedEntry(oldRun)
	requireLaunching("old-run heartbeat")

	// (b) The same entry from a broker that sends no run ID: the claim
	// alone guards it.
	stoppedEntry("")
	requireLaunching("run-less heartbeat")

	// (c) The launch completes.
	rec := postLaunchReport(t, f.srv, f.brokerID, a.ID, f.brokerID, AgentLaunchReport{
		LaunchID: launchID, InstanceID: "inst-1", State: "succeeded",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), store.LaunchReportResultApplied)
	done := getAgent(t, f.s, a.ID)
	require.Equal(t, string(state.PhaseRunning), done.Phase, "the launch completes despite the old container's reports")
	require.Equal(t, store.LaunchStateEnded, done.LaunchState)
	require.Equal(t, store.LaunchEndReasonSucceeded, done.LaunchEndReason)
	// The launch's end settles the start claim, so from here on only the
	// store's run check stands between an old-run entry and the phase:
	// case (d) below tests that guard alone.
	require.Empty(t, done.StartClaimID, "the succeeded launch released the start claim")

	// (d) After the launch, a late stopped entry of the old run does not
	// move the running agent: the run check drops it.
	stoppedEntry(oldRun)
	after := getAgent(t, f.s, a.ID)
	assert.Equal(t, string(state.PhaseRunning), after.Phase, "a late old-run heartbeat does not stop the new run")
	assert.Equal(t, newRun, after.RunID)

	// (e) Control: the new run's own entry applies as usual.
	f.send(brokerHeartbeatRequest{
		Status: store.BrokerStatusOnline,
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{{
			Slug: a.Slug, Phase: string(state.PhaseRunning), Activity: "working", RuntimeTarget: "docker", RunID: newRun,
		}}}},
	})
	ctrl := getAgent(t, f.s, a.ID)
	assert.Equal(t, string(state.PhaseRunning), ctrl.Phase)
	assert.Equal(t, "working", ctrl.Activity, "the new run's heartbeat applies")
}
