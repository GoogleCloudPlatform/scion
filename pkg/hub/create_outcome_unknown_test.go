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

// ptone/scion#4817: a create (or env finalize) whose synchronous dispatch
// ends with an unknown outcome keeps the agent for the start-claim reaper
// instead of rolling it back while the broker may still be starting it.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// timeoutNetErr is a net.Error reporting a timeout, as the direct HTTP
// transport returns after the request was sent.
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

// realDialTimeout returns the error a dial that ran out of its deadline
// gives: *url.Error{*net.OpError{Op: "dial", Err: <net timeout>}}. Its net
// timeout matches context.DeadlineExceeded (net's timeoutError.Is).
func realDialTimeout(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:1")
	require.Error(t, err)
	var opErr *net.OpError
	require.ErrorAs(t, err, &opErr)
	require.Equal(t, "dial", opErr.Op)
	require.ErrorIs(t, err, context.DeadlineExceeded, "a dial timeout matches DeadlineExceeded")
	return &url.Error{Op: "Post", URL: "http://broker/api/v1/agents", Err: err}
}

func TestIsUnknownDispatchOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dispatch deadline", fmt.Errorf("create: %w", context.DeadlineExceeded), true},
		{"control channel response timeout", fmt.Errorf("control channel request failed: %w", &brokerTransportError{msg: "request timeout after 2m0s", sentinel: errBrokerResponseTimeout}), true},
		{"control channel connection lost", fmt.Errorf("control channel request failed: %w", &brokerTransportError{msg: "connection closed", sentinel: errBrokerConnectionLost}), true},
		{"cross-node wait expired", fmt.Errorf("create: %w", ErrDispatchFailed), true},
		{"http read timeout", fmt.Errorf("post: %w", timeoutNetErr{}), true},
		{"nil", nil, false},
		{"cancelled", context.Canceled, false},
		{"real dial timeout", realDialTimeout(t), false},
		{"dial failure", &net.OpError{Op: "dial", Err: timeoutNetErr{}}, false},
		{"pre-send secret resolution deadline", &secretResolutionError{Verb: "created", Err: context.DeadlineExceeded}, false},
		{"request never sent", fmt.Errorf("x: %w (%w)", context.DeadlineExceeded, errStartRequestNotSent), false},
		{"broker not connected", fmt.Errorf("x: %w", errStartBrokerNotConnected), false},
		{"broker refusal", &brokerStatusError{StatusCode: http.StatusUnprocessableEntity}, false},
		{"plain failure", errors.New("auth resolution failed"), false},
		{"text-only timeout", errors.New("request timeout after 2m0s"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isUnknownDispatchOutcome(tc.err))
		})
	}
	// The transports' errors keep the text they always had.
	assert.Equal(t, "request timeout after 2m0s", (&brokerTransportError{msg: "request timeout after 2m0s", sentinel: errBrokerResponseTimeout}).Error())
}

// Only a caller that keeps the agent keeps the credential; every other
// caller keeps the dispatcher's revoke on any error.
func TestKeepCredentialOnDispatchError(t *testing.T) {
	unknown := fmt.Errorf("x: %w", context.DeadlineExceeded)
	assert.True(t, keepCredentialOnDispatchError(withKeepCredentialOnUnknownOutcome(context.Background()), unknown))
	assert.False(t, keepCredentialOnDispatchError(context.Background(), unknown), "an unmarked caller revokes")
	assert.False(t, keepCredentialOnDispatchError(withKeepCredentialOnUnknownOutcome(context.Background()), errors.New("refused")), "a definite failure revokes")
}

// keepRecordingCreateDispatcher is failingCreateDispatcher that records
// whether the create's dispatch ctx keeps the credential on its error
// (keepCredentialOnDispatchError): the handler's keep-flag wiring, through
// the start claim's run ctx and syncDispatch.
type keepRecordingCreateDispatcher struct {
	failingCreateDispatcher
	keepsCredential bool
}

func (d *keepRecordingCreateDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.keepsCredential = keepCredentialOnDispatchError(ctx, d.createErr)
	return d.failingCreateDispatcher.DispatchAgentCreateWithGather(ctx, agent)
}

// keepRecordingFinalizeDispatcher is finalizeEnvErrDispatcher that records
// the same for the env finalize.
type keepRecordingFinalizeDispatcher struct {
	finalizeEnvErrDispatcher
	keepsCredential bool
}

func (d *keepRecordingFinalizeDispatcher) DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string) (*CreateDispatchResult, error) {
	d.keepsCredential = keepCredentialOnDispatchError(ctx, d.err)
	return d.finalizeEnvErrDispatcher.DispatchFinalizeEnv(ctx, agent, env)
}

// createUnknownOutcome runs a create whose dispatch fails with err and
// returns the response, the store and the dispatcher.
func createUnknownOutcome(t *testing.T, err error, gather bool) (*Server, store.Store, *keepRecordingCreateDispatcher, *httptest.ResponseRecorder) {
	t.Helper()
	disp := &keepRecordingCreateDispatcher{failingCreateDispatcher: failingCreateDispatcher{createErr: err}}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.startClaimsOn = true
	srv.events = &createdCountingPublisher{EventPublisher: srv.events}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "outcome-unknown-agent",
		ProjectID: project.ID,
		Task:      "do something",
		GatherEnv: gather,
	})
	require.NotNil(t, disp.capturedAgent, "the dispatcher must have observed the create")
	return srv, s, disp, rec
}

func assertKeptUnknownOutcome(t *testing.T, s store.Store, disp *keepRecordingCreateDispatcher, rec *httptest.ResponseRecorder) {
	t.Helper()
	ctx := context.Background()
	assert.True(t, disp.keepsCredential, "the create's dispatch keeps the credential it mints on an unknown outcome")
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeStartOutcomeUnknown, errResp.Error.Code)
	assert.Equal(t, disp.capturedAgent.ID, errResp.Error.Details["agentId"])

	assert.False(t, disp.deleteCalled, "an unknown outcome must not delete the runtime")
	got, err := s.GetAgent(ctx, disp.capturedAgent.ID)
	require.NoError(t, err, "an unknown outcome keeps the agent")
	assert.Contains(t, got.Message, "start outcome unknown")
	assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "the start-claim reaper settles it")
	assert.Equal(t, store.StartClaimCreate, got.StartClaimKind)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, got.ID), "quotas stay held")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, got.ID), "quotas stay held")
	require.NotNil(t, got.StartClaimHoldUntil)
	require.NotNil(t, got.StartClaimUnconfirmedAt)
	assert.GreaterOrEqual(t, got.StartClaimHoldUntil.Sub(*got.StartClaimUnconfirmedAt), api.SyncStartCeiling,
		"the reaper does not stop the start before the broker's start ceiling")
	require.Eventually(t, func() bool {
		recs, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutationTypeAgentCreateOutcomeUnknown, TargetID: got.ID})
		return err == nil && len(recs) == 1
	}, 5*time.Second, 20*time.Millisecond, "the unknown outcome is audited")
}

// createdCountingPublisher counts agent.created publishes.
type createdCountingPublisher struct {
	EventPublisher
	created atomic.Int32
}

func (p *createdCountingPublisher) PublishAgentCreated(ctx context.Context, a *store.Agent) {
	p.created.Add(1)
	p.EventPublisher.PublishAgentCreated(ctx, a)
}

func assertCreatedPublished(t *testing.T, srv *Server) {
	t.Helper()
	pub, ok := srv.events.(*createdCountingPublisher)
	require.True(t, ok)
	assert.Equal(t, int32(1), pub.created.Load(), "the kept agent is published as created")
}

func TestCreateAgent_GatherEnvDispatchDeadline_KeepsRow(t *testing.T) {
	srv, s, disp, rec := createUnknownOutcome(t, fmt.Errorf("create: %w", context.DeadlineExceeded), true)
	assertKeptUnknownOutcome(t, s, disp, rec)
	assertCreatedPublished(t, srv)
}

func TestCreateAgent_DispatchDeadline_KeepsRow(t *testing.T) {
	srv, s, disp, rec := createUnknownOutcome(t, fmt.Errorf("control channel request failed: %w", &brokerTransportError{msg: "request timeout after 2m0s", sentinel: errBrokerResponseTimeout}), false)
	assertKeptUnknownOutcome(t, s, disp, rec)
	assertCreatedPublished(t, srv)
}

// The cross-node wait expiring (multi-replica hub, the broker connected to
// another node) is an unknown outcome too: the intent is still open.
func TestCreateAgent_CrossNodeWaitExpired_KeepsRow(t *testing.T) {
	_, s, disp, rec := createUnknownOutcome(t, fmt.Errorf("create: %w", ErrDispatchFailed), true)
	assertKeptUnknownOutcome(t, s, disp, rec)
	assert.Contains(t, rec.Body.String(), "did not report the outcome in time")
}

// A dial timeout sent nothing: the create is rolled back, never kept
// without a claim.
func TestCreateAgent_DialTimeout_RollsBack(t *testing.T) {
	_, s, disp, rec := createUnknownOutcome(t, realDialTimeout(t), true)
	assert.NotEqual(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	assert.True(t, disp.deleteCalled, "the create is rolled back")
	_, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "no agent is left without a claim")
}

// MUST 1 (agent-delete-lead): a kept row can be deleted at once while its
// claim is unconfirmed. The delete supersedes the claim, revokes and
// releases the quotas.
func TestDeleteAgent_KeptOutcomeUnknownRow(t *testing.T) {
	srv, s, disp, rec := createUnknownOutcome(t, fmt.Errorf("create: %w", context.DeadlineExceeded), true)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	id := disp.capturedAgent.ID
	ctx := context.Background()
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	seedLiveCredential(t, s, got, "kept-delete-jti")

	del := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+id, nil)
	require.Less(t, del.Code, 300, del.Body.String())
	require.Eventually(t, func() bool {
		a, err := s.GetAgent(ctx, id)
		return errors.Is(err, store.ErrNotFound) || (err == nil && !a.DeletedAt.IsZero())
	}, 10*time.Second, 20*time.Millisecond, "the kept row is deleted (hard or soft)")
	assert.True(t, disp.deleteCalled, "the delete removes the runtime")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, id), "the delete releases the per-broker reservation")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, id), "the delete releases the per-project reservation")
	claimed, err := s.ListAgentsWithStartClaim(ctx)
	require.NoError(t, err)
	for _, a := range claimed {
		assert.NotEqual(t, id, a.ID, "no claim is left for the deleted agent")
	}
	c := getTestAgentCredential(t, s, "kept-delete-jti")
	assert.NotNil(t, c.RevokedAt, "the delete revokes the kept credential")
}

// seedLiveCredential records a live credential for a, as the create's
// dispatch would have minted.
func seedLiveCredential(t *testing.T, s store.Store, a *store.Agent, jti string) {
	t.Helper()
	require.NoError(t, s.CreateAgentCredential(context.Background(), &store.AgentCredential{
		ID: tid(jti), AgentID: a.ID, ProjectID: a.ProjectID, TokenJTIHash: hashJTI(jti),
		IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}))
}

// A dispatch that definitely failed is still rolled back.
func TestCreateAgent_DefiniteDispatchFailure_StillRollsBack(t *testing.T) {
	_, s, disp, rec := createUnknownOutcome(t, errors.New("auth resolution failed"), true)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.False(t, disp.keepsCredential, "a definite failure revokes")
	assert.True(t, disp.deleteCalled)
	_, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// newCredentialDispatcher returns an HTTP dispatcher whose broker client
// fails every create with err, the token generator recording what it
// minted, and the agent to dispatch.
func newCredentialDispatcher(t *testing.T, name string, client *mockRuntimeBrokerClient) (store.Store, *HTTPAgentDispatcher, *fakeMintingTokenGenerator, *store.Agent) {
	t.Helper()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, name+"-host")
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, client, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)
	agent := &store.Agent{ID: tid(name + "-agent"), Name: "a", Slug: "a", ProjectID: tid(name + "-project"), RuntimeBrokerID: broker.ID}
	return memStore, dispatcher, gen, agent
}

// The dispatchers keep the credential they minted on an unknown outcome
// only for a caller that keeps the agent (withKeepCredentialOnUnknownOutcome),
// and revoke it on a definite failure or for any other caller.
func TestHTTPAgentDispatcher_UnknownOutcomeCredential(t *testing.T) {
	unknown := fmt.Errorf("send: %w", context.DeadlineExceeded)
	cases := []struct {
		name   string
		keep   bool
		err    error
		revoke bool
	}{
		{"keeping caller, unknown outcome: kept", true, unknown, false},
		{"keeping caller, definite failure: revoked", true, errors.New("broker unavailable"), true},
		{"other caller, unknown outcome: revoked", false, unknown, true},
	}
	dispatches := map[string]func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error{
		"create": func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchAgentCreate(ctx, a)
			return err
		},
		"create-with-gather": func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchAgentCreateWithGather(ctx, a)
			return err
		},
		"finalize-env": func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchFinalizeEnv(ctx, a, map[string]string{"K": "v"})
			return err
		},
	}
	for dname, dispatch := range dispatches {
		for _, tc := range cases {
			t.Run(dname+"/"+tc.name, func(t *testing.T) {
				failErr := tc.err
				client := &mockRuntimeBrokerClient{
					returnErr: failErr,
					createWithGatherFunc: func(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
						return nil, nil, failErr
					},
				}
				memStore, dispatcher, gen, agent := newCredentialDispatcher(t, "outcome-"+dname, client)
				ctx := context.Background()
				if tc.keep {
					ctx = withKeepCredentialOnUnknownOutcome(ctx)
				}
				require.Error(t, dispatch(ctx, dispatcher, agent))
				require.Len(t, gen.jtis, 1)
				cred := getTestAgentCredential(t, memStore, gen.lastJTI())
				assert.Equal(t, tc.revoke, cred.RevokedAt != nil)
			})
		}
	}
}

// unknownCreateClaim adds an agent in phase created with run intent
// running, a live credential and an unconfirmed claim of kind for hold.
func unknownCreateClaim(t *testing.T, f *reconcileFixture, slug string, kind store.StartClaimKind, hold time.Duration) *store.Agent {
	t.Helper()
	ctx := context.Background()
	a := f.addAgent(slug, "created", "")
	_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	seedLiveCredential(t, f.s, a, slug+"-jti")
	unconfirmedClaim(t, f, a, kind, hold)
	return a
}

// The reaper revokes the credential of a create whose outcome was unknown
// once a fresh inventory shows nothing came up, and only for a create.
func TestStartClaimReaper_UnknownCreateNothingRunningRevokes(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	noObservationLag(t)
	ctx := context.Background()
	create := unknownCreateClaim(t, f, "kept-create", store.StartClaimCreate, time.Hour)
	user := unknownCreateClaim(t, f, "kept-user", store.StartClaimUser, time.Hour)

	f.heartbeat(completeInventory()) // nothing running, nothing in flight
	f.srv.reapStartClaims(ctx)

	got := getAgent(t, f.s, create.ID)
	assert.Empty(t, got.StartClaimID, "the claim is released")
	assert.Equal(t, store.RunIntentStopped, got.RunIntent)
	c := getTestAgentCredential(t, f.s, "kept-create-jti")
	require.NotNil(t, c.RevokedAt, "nothing came up: the kept credential is revoked")
	require.NotNil(t, c.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *c.RevokeReason)

	assert.Empty(t, getAgent(t, f.s, user.ID).StartClaimID, "a user start's claim is released too")
	assert.Nil(t, getTestAgentCredential(t, f.s, "kept-user-jti").RevokedAt, "only a create's credential is revoked")
}

// A create that came up keeps its credential when its claim is released.
func TestStartClaimReaper_UnknownCreateReportedRunningKeepsCredential(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	noObservationLag(t)
	ctx := context.Background()
	a := unknownCreateClaim(t, f, "came-up", store.StartClaimCreate, time.Hour)
	require.NoError(t, f.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running", Heartbeat: true}))
	f.heartbeat(completeInventory(), a.Slug)
	f.srv.reapStartClaims(ctx)

	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the start succeeded: the claim is released")
	assert.Nil(t, getTestAgentCredential(t, f.s, "came-up-jti").RevokedAt, "the running agent keeps its credential")
}

// The revoke acts only while the listed claim is still held: once another
// claim replaced it, the credential (possibly a newer start's) is kept.
func TestRevokeUnconfirmedCreateCredential_StaleClaim(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	ctx := context.Background()
	a := unknownCreateClaim(t, f, "stale-claim", store.StartClaimCreate, time.Hour)
	listed := getAgent(t, f.s, a.ID)
	listed.StartClaimID = "some-earlier-claim"
	f.srv.revokeUnconfirmedCreateCredential(ctx, listed)
	assert.Nil(t, getTestAgentCredential(t, f.s, "stale-claim-jti").RevokedAt, "a claim that is no longer held does not revoke")

	f.srv.revokeUnconfirmedCreateCredential(ctx, getAgent(t, f.s, a.ID))
	assert.NotNil(t, getTestAgentCredential(t, f.s, "stale-claim-jti").RevokedAt, "the held claim revokes")

	// A live (not unconfirmed) claim with the listed ID does not revoke.
	live := f.addAgent("live-claim", "created", "")
	seedLiveCredential(t, f.s, live, "live-claim-jti")
	_, err := f.s.ClaimAgentStart(ctx, live.ID, "hub-x", store.StartClaimCreate, "", time.Minute)
	require.NoError(t, err)
	f.srv.revokeUnconfirmedCreateCredential(ctx, getAgent(t, f.s, live.ID))
	assert.Nil(t, getTestAgentCredential(t, f.s, "live-claim-jti").RevokedAt, "a live claim (the start is still running) does not revoke")
}

// A kept create whose start is still in flight is not stopped within the
// create hold, which covers the broker's start ceiling, and is stopped
// once the hold has passed.
func TestStartClaimReaper_UnknownCreateInFlightStoppedOnlyPastHold(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	noObservationLag(t)
	ctx := context.Background()
	hold := f.srv.startClaimSettings().Holds().For(store.StartClaimCreate)
	require.GreaterOrEqual(t, hold, api.SyncStartCeiling)
	slow := unknownCreateClaim(t, f, "slow-create", store.StartClaimCreate, hold)
	expired := unknownCreateClaim(t, f, "expired-create", store.StartClaimCreate, time.Millisecond)
	time.Sleep(5 * time.Millisecond)

	f.send(brokerHeartbeatRequest{
		Status:         store.BrokerStatusOnline,
		Inventory:      completeInventory(),
		Capabilities:   &store.BrokerCapabilities{StartsInFlight: true},
		StartsInFlight: []brokerStartInFlight{{ProjectID: f.projectID, Slug: slow.Slug}, {ProjectID: f.projectID, Slug: expired.Slug}},
		Projects:       []brokerProjectHeartbeat{{ProjectID: f.projectID}},
	})
	f.srv.reapStartClaims(ctx)
	require.Eventually(t, func() bool { return d.stops.Load() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the create past its hold is stopped")
	f.srv.reapStartClaims(ctx)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), d.stops.Load(), "the create within its hold is not stopped")
	assert.Equal(t, store.StartClaimUnconfirmed, getAgent(t, f.s, slow.ID).StartClaimState)
}

// The broker transports mark their own response timeout, so a create
// whose request was sent but not answered reads as an unknown outcome.
func TestTunnelRequest_TimeoutIsUnknownOutcome(t *testing.T) {
	runOverSilentBrokers(t, func(t *testing.T, tr silentBrokerTransport) {
		b := tr.new(t, 100*time.Millisecond)
		req := &wsprotocol.RequestEnvelope{Type: wsprotocol.TypeRequest, RequestID: "create-req-unknown", Method: "POST", Path: "/api/v1/agents"}
		_, err := b.tunnel().TunnelRequest(context.Background(), "broker-1", req)
		require.Error(t, err)
		b.received(t)
		assert.ErrorIs(t, err, errBrokerResponseTimeout)
		assert.True(t, isUnknownDispatchOutcome(err))
		assert.Contains(t, err.Error(), "request timeout after")
	})
}

// An env submit whose finalize dispatch hits its deadline answers 504
// start_outcome_unknown and keeps the agent (the finalize never deletes)
// under an unconfirmed create claim, with the unknown outcome recorded.
func TestSubmitAgentEnv_DispatchDeadline_StartOutcomeUnknown(t *testing.T) {
	disp := &keepRecordingFinalizeDispatcher{finalizeEnvErrDispatcher: finalizeEnvErrDispatcher{err: fmt.Errorf("finalize: %w", context.DeadlineExceeded)}}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.startClaimsOn = true
	ctx := context.Background()
	agent := &store.Agent{
		ID: tid("agent-env-outcome-unknown"), Name: "env-outcome-unknown", Slug: "env-outcome-unknown",
		ProjectID: project.ID, RuntimeBrokerID: project.DefaultRuntimeBrokerID, Phase: string(state.PhaseProvisioning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/"+agent.Slug+"/env",
		SubmitEnvRequest{Env: map[string]string{"API_KEY": "v"}})

	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"`+ErrCodeStartOutcomeUnknown+`"`)
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Contains(t, got.Message, "start outcome unknown")
	assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "the start-claim reaper settles it")
	assert.Equal(t, store.StartClaimCreate, got.StartClaimKind)
	assert.True(t, disp.keepsCredential, "the finalize's dispatch keeps the credential it mints on an unknown outcome")
}

// The control channel's connection closing while a request waits is an
// unknown outcome, with the text it always had.
func TestTunnelRequest_ConnectionLostIsUnknownOutcome(t *testing.T) {
	b, ok := newSilentControlChannelBroker(t, time.Minute).(*silentControlChannelBroker)
	require.True(t, ok)
	connCtx, cancel := context.WithCancel(context.Background())
	b.hc.ctx, b.hc.cancel = connCtx, cancel
	req := &wsprotocol.RequestEnvelope{Type: wsprotocol.TypeRequest, RequestID: "create-req-lost", Method: "POST", Path: "/api/v1/agents"}
	errCh := make(chan error, 1)
	go func() {
		_, err := b.hc.TunnelRequest(context.Background(), req)
		errCh <- err
	}()
	b.received(t)
	b.hc.Close()
	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.ErrorIs(t, err, errBrokerConnectionLost)
		assert.True(t, isUnknownDispatchOutcome(err))
		assert.Equal(t, "connection closed", err.Error())
	case <-time.After(10 * time.Second):
		t.Fatal("TunnelRequest did not return after the connection closed")
	}
}

// crossNodeFixture is a server whose HTTP dispatcher reaches a broker
// through mockClient, recording the credentials it mints, with an agent to
// dispatch, as the node owning the broker connection runs a cross-node
// dispatch (execDispatchCreate, execDispatchFinalizeEnv).
func crossNodeFixture(t *testing.T, name string, client *mockRuntimeBrokerClient) (*Server, store.Store, *fakeMintingTokenGenerator, *store.Agent) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid(name + "-project"), Name: name, Slug: name}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := newTestBrokerForRevoke(t, s, name+"-host")
	agent := &store.Agent{ID: tid(name + "-agent"), Name: name, Slug: name, ProjectID: project.ID, RuntimeBrokerID: broker.ID, Phase: string(state.PhaseProvisioning)}
	require.NoError(t, s.CreateAgent(ctx, agent))
	dispatcher := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: s}
	dispatcher.SetTokenGenerator(gen)
	srv.SetDispatcher(dispatcher)
	return srv, s, gen, agent
}

// The owning node keeps the credential on an unknown outcome, as the
// requesting node keeps the agent when its wait expires (ErrDispatchFailed),
// and revokes it on a definite failure.
func TestExecDispatchCreate_UnknownOutcomeKeepsCredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		revoke bool
	}{
		{"unknown outcome keeps", &brokerTransportError{msg: "request timeout after 2m0s", sentinel: errBrokerResponseTimeout}, false},
		{"definite failure revokes", errors.New("broker unavailable"), true},
	} {
		for _, op := range []string{"create", "finalize_env"} {
			t.Run(op+"/"+tc.name, func(t *testing.T) {
				failErr := tc.err
				client := &mockRuntimeBrokerClient{createWithGatherFunc: func(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
					return nil, nil, failErr
				}}
				srv, s, gen, agent := crossNodeFixture(t, "owner-"+op, client)
				d := store.BrokerDispatch{AgentID: agent.ID, Op: op}
				var err error
				if op == "create" {
					_, err = srv.execDispatchCreate(context.Background(), d)
				} else {
					_, err = srv.execDispatchFinalizeEnv(context.Background(), d)
				}
				require.Error(t, err)
				require.Len(t, gen.jtis, 1)
				assert.Equal(t, tc.revoke, getTestAgentCredential(t, s, gen.lastJTI()).RevokedAt != nil)
			})
		}
	}
}

// The requesting node revokes every credential of the agent, including one
// the owning node kept, when the cross-node create ends with a definite
// failure: its dispatch's own revoke covers the agent, before the create
// handler rolls the agent back.
func TestHTTPAgentDispatcher_DeferredDefiniteFailureRevokesAll(t *testing.T) {
	client := &mockRuntimeBrokerClient{createWithGatherFunc: func(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		return nil, nil, ErrLifecycleDeferred
	}}
	memStore, dispatcher, gen, agent := newCredentialDispatcher(t, "deferred-definite", client)
	seedLiveCredential(t, memStore, agent, "owner-kept-jti")

	_, err := dispatcher.DispatchAgentCreateWithGather(withKeepCredentialOnUnknownOutcome(context.Background()), agent)
	require.Error(t, err)
	require.False(t, isUnknownDispatchOutcome(err), "the deferred failure is definite: %v", err)
	require.Len(t, gen.jtis, 1)
	assert.NotNil(t, getTestAgentCredential(t, memStore, gen.lastJTI()).RevokedAt, "the requester's own credential is revoked")
	assert.NotNil(t, getTestAgentCredential(t, memStore, "owner-kept-jti").RevokedAt, "the credential the owning node kept is revoked too")
}
