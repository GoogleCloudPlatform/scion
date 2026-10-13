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
	"testing"
	"time"

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

func TestIsUnknownDispatchOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dispatch deadline", fmt.Errorf("create: %w", context.DeadlineExceeded), true},
		{"control channel response timeout", fmt.Errorf("control channel request failed: request timeout after 2m0s (%w)", errBrokerResponseTimeout), true},
		{"control channel connection lost", fmt.Errorf("control channel request failed: connection closed (%w)", errBrokerConnectionLost), true},
		{"http read timeout", fmt.Errorf("post: %w", timeoutNetErr{}), true},
		{"nil", nil, false},
		{"cancelled", context.Canceled, false},
		{"dial failure", &net.OpError{Op: "dial", Err: timeoutNetErr{}}, false},
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
}

// createUnknownOutcome runs a create whose dispatch fails with err and
// returns the response, the store and the dispatcher.
func createUnknownOutcome(t *testing.T, err error, gather bool) (*Server, store.Store, *failingCreateDispatcher, *httptest.ResponseRecorder) {
	t.Helper()
	disp := &failingCreateDispatcher{createErr: err}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.startClaimsOn = true
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "outcome-unknown-agent",
		ProjectID: project.ID,
		Task:      "do something",
		GatherEnv: gather,
	})
	require.NotNil(t, disp.capturedAgent, "the dispatcher must have observed the create")
	return srv, s, disp, rec
}

func assertKeptUnknownOutcome(t *testing.T, s store.Store, disp *failingCreateDispatcher, rec *httptest.ResponseRecorder) {
	t.Helper()
	ctx := context.Background()
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
}

func TestCreateAgent_GatherEnvDispatchDeadline_KeepsRow(t *testing.T) {
	_, s, disp, rec := createUnknownOutcome(t, fmt.Errorf("create: %w", context.DeadlineExceeded), true)
	assertKeptUnknownOutcome(t, s, disp, rec)
}

func TestCreateAgent_DispatchDeadline_KeepsRow(t *testing.T) {
	_, s, disp, rec := createUnknownOutcome(t, fmt.Errorf("control channel request failed: %w", errBrokerResponseTimeout), false)
	assertKeptUnknownOutcome(t, s, disp, rec)
}

// MUST 1 (agent-delete-lead): a kept row can be deleted at once while its
// claim is unconfirmed. The delete supersedes the claim, revokes and
// releases the quotas.
func TestDeleteAgent_KeptOutcomeUnknownRow(t *testing.T) {
	srv, s, disp, rec := createUnknownOutcome(t, fmt.Errorf("create: %w", context.DeadlineExceeded), true)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	id := disp.capturedAgent.ID
	ctx := context.Background()

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
}

// A dispatch that definitely failed is still rolled back.
func TestCreateAgent_DefiniteDispatchFailure_StillRollsBack(t *testing.T) {
	_, s, disp, rec := createUnknownOutcome(t, errors.New("auth resolution failed"), true)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.True(t, disp.deleteCalled)
	_, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// The dispatcher keeps the credential it minted when the outcome is
// unknown (the broker may be starting the agent with it), and still
// revokes it on a definite failure.
func TestHTTPAgentDispatcher_CreateWithGather_UnknownOutcomeKeepsCredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		revoke bool
	}{
		{"deadline keeps", fmt.Errorf("send: %w", context.DeadlineExceeded), false},
		{"refusal revokes", errors.New("broker unavailable"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			memStore := createTestStore(t)
			broker := newTestBrokerForRevoke(t, memStore, "outcome-gather-host")
			dispatcher := NewHTTPAgentDispatcherWithClient(memStore, &mockRuntimeBrokerClient{returnErr: tc.err}, false, slog.Default())
			gen := &fakeMintingTokenGenerator{store: memStore}
			dispatcher.SetTokenGenerator(gen)
			agent := &store.Agent{ID: tid("outcome-gather-agent"), Name: "a", Slug: "a", ProjectID: tid("outcome-gather-project"), RuntimeBrokerID: broker.ID}

			_, err := dispatcher.DispatchAgentCreateWithGather(ctx, agent)
			require.Error(t, err)
			require.Len(t, gen.jtis, 1)
			cred := getTestAgentCredential(t, memStore, gen.lastJTI())
			assert.Equal(t, tc.revoke, cred.RevokedAt != nil)
		})
	}
}

func TestHTTPAgentDispatcher_FinalizeEnv_UnknownOutcomeKeepsCredential(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "outcome-finalize-host")
	mockClient := &mockRuntimeBrokerClient{
		createWithGatherFunc: func(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			return nil, nil, fmt.Errorf("control channel request failed: %w", errBrokerConnectionLost)
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	dispatcher.SetTokenGenerator(gen)
	agent := &store.Agent{ID: tid("outcome-finalize-agent"), Name: "a", Slug: "a", ProjectID: tid("outcome-finalize-project"), RuntimeBrokerID: broker.ID}

	_, err := dispatcher.DispatchFinalizeEnv(ctx, agent, map[string]string{"K": "v"})
	require.Error(t, err)
	require.Len(t, gen.jtis, 1)
	cred := getTestAgentCredential(t, memStore, gen.lastJTI())
	assert.Nil(t, cred.RevokedAt, "an unknown finalize outcome keeps the credential")
}

// The reaper revokes the credential of a create whose outcome was unknown
// once a fresh inventory shows nothing came up.
func TestStartClaimReaper_UnknownCreateNothingRunningRevokes(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	noObservationLag(t)
	ctx := context.Background()
	a := f.addAgent("kept-create", "created", "")
	_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	cred := &store.AgentCredential{
		ID: tid("kept-create-cred"), AgentID: a.ID, ProjectID: a.ProjectID, TokenJTIHash: hashJTI("kept-create-jti"),
		IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, f.s.CreateAgentCredential(ctx, cred))
	unconfirmedClaim(t, f, a, store.StartClaimCreate, time.Hour)

	f.heartbeat(completeInventory()) // nothing running, nothing in flight
	f.srv.reapStartClaims(ctx)

	got := getAgent(t, f.s, a.ID)
	assert.Empty(t, got.StartClaimID, "the claim is released")
	assert.Equal(t, store.RunIntentStopped, got.RunIntent)
	c := getTestAgentCredential(t, f.s, "kept-create-jti")
	require.NotNil(t, c.RevokedAt, "nothing came up: the kept credential is revoked")
	require.NotNil(t, c.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *c.RevokeReason)
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
// start_outcome_unknown and keeps the agent (the finalize never deletes),
// with the unknown outcome recorded on it.
func TestSubmitAgentEnv_DispatchDeadline_StartOutcomeUnknown(t *testing.T) {
	rec, got := submitEnvWithFinalizeErrAgent(t, "env-outcome-unknown", fmt.Errorf("finalize: %w", context.DeadlineExceeded))
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"`+ErrCodeStartOutcomeUnknown+`"`)
	assert.Contains(t, got.Message, "start outcome unknown")
}
