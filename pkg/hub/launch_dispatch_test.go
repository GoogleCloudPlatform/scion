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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// asyncLaunchClient is a broker client whose create answers the launch the
// way an async-capable broker does: launchPending with the requested ID.
type asyncLaunchClient struct {
	*mockRuntimeBrokerClient
	// answer builds the create answer from the request. nil means "echo
	// the launch as accepted".
	answer     func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error)
	wouldDefer bool
	sends      []RemoteCreateAgentRequest
}

func (c *asyncLaunchClient) respond(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	c.sends = append(c.sends, *req)
	if c.answer != nil {
		return c.answer(req)
	}
	if !req.AsyncLaunch {
		return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Phase: string(state.PhaseRunning)}, Created: true}, nil, nil
	}
	return acceptedAnswer(req, req.LaunchID), nil, nil
}

func acceptedAnswer(req *RemoteCreateAgentRequest, launchID string) *RemoteAgentResponse {
	return &RemoteAgentResponse{
		Agent:            &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Template: "tmpl-from-broker", Phase: string(state.PhaseRunning)},
		Created:          true,
		LaunchPending:    true,
		LaunchID:         launchID,
		LaunchInstanceID: "broker-instance-1",
	}
}

func (c *asyncLaunchClient) CreateAgent(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	resp, _, err := c.respond(req)
	return resp, err
}

func (c *asyncLaunchClient) CreateAgentWithGather(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	return c.respond(req)
}

func (c *asyncLaunchClient) createWithGatherWouldDefer(context.Context, string, string) bool {
	return c.wouldDefer
}

type asyncLaunchFixture struct {
	store      store.Store
	client     *asyncLaunchClient
	dispatcher *HTTPAgentDispatcher
	broker     *store.RuntimeBroker
	settings   AsyncLaunchSettings
}

func newAsyncLaunchFixture(t *testing.T, caps *store.BrokerCapabilities) *asyncLaunchFixture {
	t.Helper()
	return newAsyncLaunchFixtureOn(t, createTestStore(t), caps)
}

// newAsyncLaunchFixtureOn builds the fixture on an existing store, so a
// Server sharing the store can serve requests with f.dispatcher.
func newAsyncLaunchFixtureOn(t *testing.T, s store.Store, caps *store.BrokerCapabilities) *asyncLaunchFixture {
	t.Helper()
	ctx := context.Background()
	project := &store.Project{ID: tid("al-project"), Name: "al-project", Slug: "al-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("al-broker"), Name: "al-broker", Slug: "al-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline, Capabilities: caps,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	f := &asyncLaunchFixture{
		store:    s,
		client:   &asyncLaunchClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}},
		broker:   broker,
		settings: AsyncLaunchSettings{Enabled: true, Timeout: 5 * time.Minute, KeepaliveSeconds: 15},
	}
	f.dispatcher = NewHTTPAgentDispatcherWithClient(s, f.client, false, slog.Default())
	f.dispatcher.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return f.settings })
	return f
}

func (f *asyncLaunchFixture) agent(t *testing.T, name, phase string, optIn bool) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid("al-agent-" + name), Slug: "al-" + name, Name: "al-" + name,
		ProjectID: tid("al-project"), RuntimeBrokerID: f.broker.ID,
		Phase: phase, LaunchAsyncOptIn: optIn,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "do the thing"},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	got, err := f.store.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	return got
}

func (f *asyncLaunchFixture) row(t *testing.T, id string) *store.Agent {
	t.Helper()
	got, err := f.store.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return got
}

func TestDispatchAgentCreate_AsyncAcceptedMarksProvisioning(t *testing.T) {
	f := newAsyncLaunchFixture(t, &store.BrokerCapabilities{AsyncLaunch: true})
	agent := f.agent(t, "accepted", string(state.PhaseCreated), true)

	res, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)

	require.Len(t, f.client.sends, 1)
	sent := f.client.sends[0]
	assert.True(t, sent.AsyncLaunch)
	assert.NotEmpty(t, sent.LaunchID)
	assert.Greater(t, sent.LaunchTimeoutSeconds, 0)
	assert.LessOrEqual(t, sent.LaunchTimeoutSeconds, 300)
	assert.Equal(t, 15, sent.LaunchKeepaliveSeconds)

	accepted := res.AcceptedLaunch()
	require.NotNil(t, accepted, "an acknowledged launch must be returned")
	assert.Equal(t, sent.LaunchID, accepted.ID)
	assert.Equal(t, "broker-instance-1", accepted.Owner)

	row := f.row(t, agent.ID)
	assert.Equal(t, string(state.PhaseProvisioning), row.Phase)
	assert.Equal(t, store.LaunchStateActive, row.LaunchState)
	assert.Equal(t, sent.LaunchID, row.LaunchID)
	assert.Equal(t, "broker-instance-1", row.LaunchOwner)
	assert.True(t, row.IsInFlight())

	// The status fields of the broker's answer are not applied: the agent
	// is still launching.
	assert.NotEqual(t, string(state.PhaseRunning), agent.Phase)
	assert.Equal(t, "tmpl-from-broker", agent.Template)
}

func TestDispatchAgentCreate_SynchronousWhenNotEligible(t *testing.T) {
	cases := []struct {
		name   string
		caps   *store.BrokerCapabilities
		flag   bool
		optIn  bool
		mutate func(f *asyncLaunchFixture)
	}{
		{name: "flag-off", caps: &store.BrokerCapabilities{AsyncLaunch: true}, flag: false, optIn: true},
		{name: "no-opt-in", caps: &store.BrokerCapabilities{AsyncLaunch: true}, flag: true, optIn: false},
		{name: "broker-without-async", caps: &store.BrokerCapabilities{AsyncLaunch: false}, flag: true, optIn: true},
		{name: "zero-timeout", caps: &store.BrokerCapabilities{AsyncLaunch: true}, flag: true, optIn: true,
			mutate: func(f *asyncLaunchFixture) { f.settings.Timeout = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAsyncLaunchFixture(t, tc.caps)
			f.settings.Enabled = tc.flag
			if tc.mutate != nil {
				tc.mutate(f)
			}
			agent := f.agent(t, tc.name, string(state.PhaseCreated), tc.optIn)

			res, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
			require.NoError(t, err)
			assert.Nil(t, res.AcceptedLaunch())
			require.Len(t, f.client.sends, 1)
			assert.False(t, f.client.sends[0].AsyncLaunch)
			assert.Empty(t, f.client.sends[0].LaunchID)
			assert.Zero(t, f.client.sends[0].LaunchTimeoutSeconds)
			assert.Empty(t, f.row(t, agent.ID).LaunchID, "no launch is begun")
			assert.Equal(t, string(state.PhaseRunning), agent.Phase, "the synchronous answer is applied")
		})
	}
}

func TestDispatchAgentCreate_NilCapabilitiesTriesAsync(t *testing.T) {
	f := newAsyncLaunchFixture(t, nil)
	agent := f.agent(t, "nil-caps", string(state.PhaseCreated), true)
	res, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)
	require.NotNil(t, res.AcceptedLaunch())
}

func TestDispatchAgentCreate_SynchronousAnswerEndsLaunch(t *testing.T) {
	f := newAsyncLaunchFixture(t, nil)
	f.client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		// A broker that ignores the launch fields.
		return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Phase: string(state.PhaseRunning)}, Created: true}, nil, nil
	}
	agent := f.agent(t, "old-broker", string(state.PhaseCreated), true)

	res, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)
	assert.Nil(t, res.AcceptedLaunch())
	assert.Equal(t, string(state.PhaseRunning), agent.Phase)

	row := f.row(t, agent.ID)
	assert.Equal(t, store.LaunchStateEnded, row.LaunchState)
	assert.Equal(t, store.LaunchEndReasonNotLaunched, row.LaunchEndReason)
	assert.False(t, row.IsInFlight())
}

func TestDispatchAgentCreate_MismatchedEchoFails(t *testing.T) {
	f := newAsyncLaunchFixture(t, nil)
	f.client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		return acceptedAnswer(req, "some-other-launch"), nil, nil
	}
	agent := f.agent(t, "mismatch", string(state.PhaseCreated), true)

	_, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.ErrorIs(t, err, errLaunchEchoMismatch)
	row := f.row(t, agent.ID)
	assert.Equal(t, store.LaunchStateEnded, row.LaunchState)
	assert.Equal(t, store.LaunchEndReasonNotLaunched, row.LaunchEndReason)
}

func TestDispatchAgentCreate_SendErrorEndsLaunch(t *testing.T) {
	f := newAsyncLaunchFixture(t, nil)
	sendErr := errors.New("broker unreachable")
	f.client.answer = func(*RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		return nil, nil, sendErr
	}
	agent := f.agent(t, "send-error", string(state.PhaseCreated), true)

	_, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.ErrorIs(t, err, sendErr)
	row := f.row(t, agent.ID)
	assert.Equal(t, store.LaunchStateEnded, row.LaunchState)
	assert.Equal(t, store.LaunchEndReasonNotLaunched, row.LaunchEndReason)
}

func TestDispatchAgentCreate_InvalidPhaseIsNotSent(t *testing.T) {
	f := newAsyncLaunchFixture(t, nil)
	agent := f.agent(t, "stopped", string(state.PhaseStopped), true)

	_, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.ErrorIs(t, err, ErrLaunchInvalidPhase)
	require.ErrorIs(t, err, store.ErrInvalidPhase)
	assert.NotErrorIs(t, err, store.ErrDeleteInProgress, "a plain stop is not a delete")
	assert.Empty(t, f.client.sends, "nothing is sent for an agent that can no longer launch")
}

func TestDispatchAgentCreateWithGather_EnvRequirementsEndLaunch(t *testing.T) {
	f := newAsyncLaunchFixture(t, nil)
	f.client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		return nil, &RemoteEnvRequirementsResponse{AgentID: req.ID, Needs: []string{"API_KEY"}}, nil
	}
	agent := f.agent(t, "env-gather", string(state.PhaseCreated), true)

	res, err := f.dispatcher.DispatchAgentCreateWithGather(context.Background(), agent)
	require.NoError(t, err)
	assert.Nil(t, res.AcceptedLaunch())
	require.NotNil(t, res.EnvRequirements())
	row := f.row(t, agent.ID)
	assert.Equal(t, store.LaunchStateEnded, row.LaunchState)
	assert.Equal(t, store.LaunchEndReasonNotLaunched, row.LaunchEndReason)
}

func TestDispatchLaunching_PredictedDeferralWritesNothing(t *testing.T) {
	f := newAsyncLaunchFixture(t, nil)
	f.client.wouldDefer = true
	agent := f.agent(t, "deferred", string(state.PhaseCreated), true)
	req := &RemoteCreateAgentRequest{ID: agent.ID}
	sent := false
	send := func(context.Context, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		sent = true
		return nil, nil, nil
	}

	_, _, launch, err := f.dispatcher.dispatchLaunching(context.Background(), agent, f.broker.Endpoint, req, true, send)
	require.ErrorIs(t, err, ErrLifecycleDeferred)
	assert.Nil(t, launch)
	assert.False(t, sent)
	assert.Empty(t, f.row(t, agent.ID).LaunchID, "the requesting node writes no launch for a deferred create")

	// A send that cannot defer ignores the prediction.
	_, _, launch, err = f.dispatcher.dispatchLaunching(context.Background(), agent, f.broker.Endpoint, req, false,
		func(_ context.Context, r *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			return acceptedAnswer(r, r.LaunchID), nil, nil
		})
	require.NoError(t, err)
	require.NotNil(t, launch)
}

func TestDispatchLaunching_DeferredAfterBeginWritesNothingMore(t *testing.T) {
	f := newAsyncLaunchFixture(t, nil)
	agent := f.agent(t, "deferred-late", string(state.PhaseCreated), true)
	req := &RemoteCreateAgentRequest{ID: agent.ID}
	_, _, launch, err := f.dispatcher.dispatchLaunching(context.Background(), agent, f.broker.Endpoint, req, true,
		func(context.Context, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			return nil, nil, ErrLifecycleDeferred
		})
	require.ErrorIs(t, err, ErrLifecycleDeferred)
	assert.Nil(t, launch)
	// The owner node's BeginLaunch supersedes this launch; it is not ended
	// here.
	assert.Equal(t, store.LaunchStateActive, f.row(t, agent.ID).LaunchState)
}

func TestDispatchLaunching_ResendClearsPreviousLaunchFields(t *testing.T) {
	f := newAsyncLaunchFixture(t, nil)
	f.settings.Enabled = false
	agent := f.agent(t, "resend", string(state.PhaseCreated), true)
	req := &RemoteCreateAgentRequest{ID: agent.ID, AsyncLaunch: true, LaunchID: "stale", LaunchTimeoutSeconds: 9, LaunchKeepaliveSeconds: 9}
	var got RemoteCreateAgentRequest
	_, _, _, err := f.dispatcher.dispatchLaunching(context.Background(), agent, f.broker.Endpoint, req, false,
		func(_ context.Context, r *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			got = *r
			return &RemoteAgentResponse{}, nil, nil
		})
	require.NoError(t, err)
	assert.False(t, got.AsyncLaunch)
	assert.Empty(t, got.LaunchID)
	assert.Zero(t, got.LaunchTimeoutSeconds)
	assert.Zero(t, got.LaunchKeepaliveSeconds)
}

func TestRemainingLaunchSeconds(t *testing.T) {
	assert.Equal(t, 300, remainingLaunchSeconds(5*time.Minute, 0))
	assert.Equal(t, 300, remainingLaunchSeconds(5*time.Minute, 500*time.Millisecond))
	assert.Equal(t, 1, remainingLaunchSeconds(time.Second, 2*time.Second))
}

func TestPersistAcceptedLaunch_KeepsLaunchAndPhase(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("pal-project"), Name: "pal", Slug: "pal"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{ID: tid("pal-agent"), Slug: "pal-agent", Name: "pal-agent", ProjectID: project.ID, Phase: string(state.PhaseCreated)}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	_, err = s.MarkLaunchAccepted(ctx, agent.ID, launchID, "owner-1")
	require.NoError(t, err)

	// The dispatched copy is stale: older version, pre-launch phase.
	dispatched := *agent
	dispatched.Template = "tmpl-x"
	dispatched.Image = "image-x"
	dispatched.AppliedConfig = &store.AgentAppliedConfig{Task: "task-x"}

	persisted, err := srv.persistAcceptedLaunch(ctx, &dispatched)
	require.NoError(t, err)
	assert.Equal(t, "tmpl-x", persisted.Template)
	assert.Equal(t, "image-x", persisted.Image)
	assert.Equal(t, string(state.PhaseProvisioning), persisted.Phase)
	assert.Equal(t, launchID, persisted.LaunchID)
	assert.True(t, persisted.IsInFlight())
}

func TestDispatchLaunching_AcceptedMarkSurvivesCanceledRequest(t *testing.T) {
	f := newAsyncLaunchFixture(t, nil)
	agent := f.agent(t, "mark-canceled", string(state.PhaseCreated), true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := &RemoteCreateAgentRequest{ID: agent.ID}

	// The caller's request ends while the broker answers.
	_, _, launch, err := f.dispatcher.dispatchLaunching(ctx, agent, f.broker.Endpoint, req, false,
		func(_ context.Context, r *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			cancel()
			return acceptedAnswer(r, r.LaunchID), nil, nil
		})
	require.NoError(t, err)
	require.NotNil(t, launch)
	row := f.row(t, agent.ID)
	assert.Equal(t, launch.ID, row.LaunchID)
	assert.Equal(t, "broker-instance-1", row.LaunchOwner, "the accepted mark is written")
	assert.Equal(t, string(state.PhaseProvisioning), row.Phase)
}

// An async create persists the run it mints before the launch begins and
// before the send, sends that run with the launch, and keeps it when the
// broker accepts, even if the accepted answer reports another run: only a
// synchronous answer is adopted (ptone/scion#2550, round 6 N2).
func TestDispatchAgentCreate_AsyncKeepsMintedRunID(t *testing.T) {
	f := newAsyncLaunchFixture(t, &store.BrokerCapabilities{AsyncLaunch: true})
	agent := f.agent(t, "run-id", string(state.PhaseCreated), true)
	var atSend *store.Agent
	f.client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		atSend = f.row(t, agent.ID)
		resp := acceptedAnswer(req, req.LaunchID)
		resp.Agent.RunID = "broker-other-run"
		return resp, nil, nil
	}

	res, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)
	require.NotNil(t, res.AcceptedLaunch())
	require.Len(t, f.client.sends, 1)
	sent := f.client.sends[0]
	require.True(t, sent.AsyncLaunch)
	require.NotEmpty(t, sent.RunID, "the launch carries the minted run")
	require.NotNil(t, atSend)
	assert.Equal(t, sent.RunID, atSend.RunID, "the run is persisted before the send")
	assert.Equal(t, sent.LaunchID, atSend.LaunchID, "the launch began before the send")

	assert.Equal(t, sent.RunID, f.row(t, agent.ID).RunID, "the accepted launch keeps the minted run")
}

// A row a delete holds refuses the async create's run write: nothing is
// sent and no launch is begun (ptone/scion#2550, round 6 N2).
func TestDispatchAgentCreate_AsyncDeleteClaimedSendsNothing(t *testing.T) {
	f := newAsyncLaunchFixture(t, &store.BrokerCapabilities{AsyncLaunch: true})
	agent := f.agent(t, "claimed", string(state.PhaseCreated), true)
	seedAgentDeletion(t, f.store, agent.ID, seedLiveDeleting)

	_, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.ErrorIs(t, err, store.ErrDeleteInProgress)
	assert.Empty(t, f.client.sends, "nothing is sent")
	row := f.row(t, agent.ID)
	assert.Empty(t, row.LaunchID, "no launch is begun")
	assert.Empty(t, row.RunID, "no run is written")
}

// claimOnBeginLaunchStore lands a delete claim (phase stopping plus a live
// deleting lease, as the delete engine writes it) just before BeginLaunch:
// the window between the create's beginRun and its BeginLaunch.
type claimOnBeginLaunchStore struct {
	store.Store
	t *testing.T
}

func (s claimOnBeginLaunchStore) BeginLaunch(ctx context.Context, agentID, kind string, timeout time.Duration) (string, error) {
	row, err := s.GetAgent(ctx, agentID)
	require.NoError(s.t, err)
	row.Phase = string(state.PhaseStopping)
	require.NoError(s.t, s.UpdateAgent(ctx, row))
	seedAgentDeletion(s.t, s.Store, agentID, seedLiveDeleting)
	return s.Store.BeginLaunch(ctx, agentID, kind, timeout)
}

// A delete claim landing between beginRun and BeginLaunch is reported as
// the delete (store.ErrDeleteInProgress), still wrapped in
// ErrLaunchInvalidPhase so callers leave the row to the delete; nothing is
// sent (ptone/scion#2550, round 6 n3).
func TestDispatchAgentCreate_DeleteClaimBeforeBeginLaunch(t *testing.T) {
	base := createTestStore(t)
	f := newAsyncLaunchFixtureOn(t, claimOnBeginLaunchStore{Store: base, t: t}, &store.BrokerCapabilities{AsyncLaunch: true})
	agent := f.agent(t, "claim-before-begin", string(state.PhaseCreated), true)

	_, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.ErrorIs(t, err, ErrLaunchInvalidPhase)
	require.ErrorIs(t, err, store.ErrInvalidPhase)
	require.ErrorIs(t, err, store.ErrDeleteInProgress)
	assert.Empty(t, f.client.sends, "nothing is sent")
	assert.Empty(t, f.row(t, agent.ID).LaunchID, "no launch is begun")
}

// writeLaunchInvalidPhase answers delete_in_progress, with details.agentId,
// when the refusal was a delete, and invalid_state otherwise (round 6 n3).
func TestWriteLaunchInvalidPhase_DeleteHoldsRow(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLaunchInvalidPhase(rec, fmt.Errorf("%w: %w", ErrLaunchInvalidPhase, store.ErrDeleteInProgress), "agent-1")
	requireIntentDeleteInProgress(t, rec, "agent-1")

	rec = httptest.NewRecorder()
	writeLaunchInvalidPhase(rec, fmt.Errorf("%w: %w", ErrLaunchInvalidPhase, store.ErrInvalidPhase), "agent-1")
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid_state")
}
