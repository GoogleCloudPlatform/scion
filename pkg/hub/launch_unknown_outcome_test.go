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

// ptone/scion#4866: an async launch whose send ends with an unknown outcome
// is kept as accepted for the broker's claim or the launch reaper, instead
// of being ended as not_launched while the broker may be launching.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// launchUnknownSendErrors are send errors whose outcome is unknown: the
// request may have reached the broker.
func launchUnknownSendErrors() map[string]error {
	return map[string]error{
		"dispatch deadline":                fmt.Errorf("failed to send request: %w", context.DeadlineExceeded),
		"control channel response timeout": fmt.Errorf("control channel request failed: %w", &brokerTransportError{msg: "request timeout after 2m0s", sentinel: errBrokerResponseTimeout}),
		"control channel connection lost":  fmt.Errorf("control channel request failed: %w", &brokerTransportError{msg: "connection closed", sentinel: errBrokerConnectionLost}),
		"http response timeout":            fmt.Errorf("post: %w", timeoutNetError{}),
	}
}

// launchDialTimeoutErr is the error a dial that ran out of its deadline
// gives: *url.Error{*net.OpError{Op: "dial"}}, which also matches
// context.DeadlineExceeded although nothing was sent.
func launchDialTimeoutErr(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:1")
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded, "a dial timeout matches DeadlineExceeded")
	return &url.Error{Op: "Post", URL: "http://broker/api/v1/agents", Err: err}
}

// withMintingTokens makes f's dispatcher mint a recorded credential per
// create, so a test can check whether it was revoked.
func withMintingTokens(f *asyncLaunchFixture) *fakeMintingTokenGenerator {
	gen := &fakeMintingTokenGenerator{store: f.store}
	f.dispatcher.SetTokenGenerator(gen)
	return gen
}

// assertKeptAcceptedLaunch checks a launch kept after an unknown send
// outcome: accepted with no owner, the row provisioning and in flight with
// the sent launch active and no owner, the credential live.
func assertKeptAcceptedLaunch(t *testing.T, f *asyncLaunchFixture, gen *fakeMintingTokenGenerator, agentID string, accepted *LaunchAccepted) {
	t.Helper()
	require.Len(t, f.client.sends, 1)
	sent := f.client.sends[0]
	require.True(t, sent.AsyncLaunch)
	require.NotNil(t, accepted, "the launch must be returned as accepted")
	assert.Equal(t, sent.LaunchID, accepted.ID)
	assert.Empty(t, accepted.Owner, "no broker acknowledged the launch")

	row := f.row(t, agentID)
	assert.Equal(t, store.LaunchStateActive, row.LaunchState)
	assert.Empty(t, row.LaunchEndReason)
	assert.Equal(t, sent.LaunchID, row.LaunchID)
	assert.Empty(t, row.LaunchOwner, "the broker's first claim records the owner")
	assert.Equal(t, string(state.PhaseProvisioning), row.Phase)
	assert.True(t, row.IsInFlight())

	require.Len(t, gen.jtis, 1)
	assert.Nil(t, getTestAgentCredential(t, f.store, gen.lastJTI()).RevokedAt, "the broker may be launching with the credential")
}

func TestDispatchAgentCreate_UnknownSendOutcomeKeepsLaunch(t *testing.T) {
	for name, sendErr := range launchUnknownSendErrors() {
		t.Run(name, func(t *testing.T) {
			f := newAsyncLaunchFixture(t, &store.BrokerCapabilities{AsyncLaunch: true})
			gen := withMintingTokens(f)
			f.client.answer = func(*RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				return nil, nil, sendErr
			}
			agent := f.agent(t, "unknown", string(state.PhaseCreated), true)

			ctx, warns := withDispatchWarnings(context.Background())
			res, err := f.dispatcher.DispatchAgentCreate(ctx, agent)
			require.NoError(t, err)
			assertKeptAcceptedLaunch(t, f, gen, agent.ID, res.AcceptedLaunch())
			assert.Contains(t, warns.Warnings(), launchOutcomeUnknownWarning)
		})
	}
}

func TestDispatchAgentCreateWithGather_UnknownSendOutcomeKeepsLaunch(t *testing.T) {
	f := newAsyncLaunchFixture(t, &store.BrokerCapabilities{AsyncLaunch: true})
	gen := withMintingTokens(f)
	f.client.answer = func(*RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		return nil, nil, fmt.Errorf("control channel request failed: %w", &brokerTransportError{msg: "connection closed", sentinel: errBrokerConnectionLost})
	}
	agent := f.agent(t, "unknown-gather", string(state.PhaseCreated), true)

	res, err := f.dispatcher.DispatchAgentCreateWithGather(context.Background(), agent)
	require.NoError(t, err)
	assert.Nil(t, res.EnvRequirements())
	assertKeptAcceptedLaunch(t, f, gen, agent.ID, res.AcceptedLaunch())
}

// A send error that means the request was not delivered, or that the
// broker answered, still ends the launch as not_launched and revokes.
func TestDispatchAgentCreate_DefiniteSendErrorEndsLaunch(t *testing.T) {
	cases := map[string]error{
		"dial timeout":         launchDialTimeoutErr(t),
		"request not sent":     fmt.Errorf("x: %w (%w)", context.DeadlineExceeded, errStartRequestNotSent),
		"broker not connected": fmt.Errorf("x: %w", errStartBrokerNotConnected),
		"broker refusal":       &brokerStatusError{StatusCode: http.StatusUnprocessableEntity},
		"cancelled":            fmt.Errorf("x: %w", context.Canceled),
		"plain failure":        errors.New("broker unreachable"),
		"text-only timeout":    errors.New("request timeout after 2m0s"),
	}
	for name, sendErr := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAsyncLaunchFixture(t, &store.BrokerCapabilities{AsyncLaunch: true})
			gen := withMintingTokens(f)
			f.client.answer = func(*RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				return nil, nil, sendErr
			}
			agent := f.agent(t, "definite", string(state.PhaseCreated), true)

			res, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
			require.ErrorIs(t, err, sendErr)
			assert.Nil(t, res.AcceptedLaunch())
			row := f.row(t, agent.ID)
			assert.Equal(t, store.LaunchStateEnded, row.LaunchState)
			assert.Equal(t, store.LaunchEndReasonNotLaunched, row.LaunchEndReason)
			require.Len(t, gen.jtis, 1)
			assert.NotNil(t, getTestAgentCredential(t, f.store, gen.lastJTI()).RevokedAt)
		})
	}
}

// The broker did get the request: its first claim takes the kept launch
// over and records the owner, as for an acknowledged launch.
func TestUnknownSendOutcome_BrokerClaimTakesLaunchOver(t *testing.T) {
	f := newAsyncLaunchFixture(t, &store.BrokerCapabilities{AsyncLaunch: true})
	f.client.answer = func(*RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		return nil, nil, fmt.Errorf("failed to send request: %w", context.DeadlineExceeded)
	}
	agent := f.agent(t, "unknown-claimed", string(state.PhaseCreated), true)
	res, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)
	launchID := res.AcceptedLaunch().ID

	ans, _, err := f.store.ApplyLaunchReport(context.Background(), agent.ID, f.broker.ID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "broker-instance-1", Seq: 1, State: store.LaunchReportStateClaim,
		Phase: string(state.PhaseProvisioning), Step: "claim",
	})
	require.NoError(t, err)
	require.Equal(t, 0, ans.HTTPStatus, "the claim must be accepted, not answered 409 not_launched")
	row := f.row(t, agent.ID)
	assert.Equal(t, store.LaunchStateActive, row.LaunchState)
	assert.Equal(t, "broker-instance-1", row.LaunchOwner)
}

// The request never arrived: no claim comes, and the launch reaper ends
// the kept launch at its deadline, like any accepted launch the broker
// never reports; the credential is handled as for any reaped launch.
func TestUnknownSendOutcome_NeverArrivedEndsByLaunchReaper(t *testing.T) {
	srv, s := testServer(t)
	f := newAsyncLaunchFixtureOn(t, s, &store.BrokerCapabilities{AsyncLaunch: true})
	gen := withMintingTokens(f)
	f.settings.Timeout = time.Millisecond
	f.client.answer = func(*RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		return nil, nil, fmt.Errorf("control channel request failed: %w", &brokerTransportError{msg: "request timeout after 2m0s", sentinel: errBrokerResponseTimeout})
	}
	agent := f.agent(t, "unknown-lost", string(state.PhaseCreated), true)
	res, err := f.dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)
	launchID := res.AcceptedLaunch().ID
	require.True(t, f.row(t, agent.ID).IsInFlight())

	time.Sleep(20 * time.Millisecond)
	srv.launchReaperTickHandler()(context.Background())

	row := f.row(t, agent.ID)
	assert.Equal(t, launchID, row.LaunchID)
	assert.Equal(t, store.LaunchStateEnded, row.LaunchState)
	assert.Equal(t, store.LaunchEndReasonTimedOut, row.LaunchEndReason)
	assert.Equal(t, store.LaunchErrorLaunchTimeout, row.LaunchError)
	assert.Equal(t, string(state.PhaseError), row.Phase)
	assert.True(t, row.IsIncompleteCreate(), "the row is kept as an incomplete create (Option R)")
	assert.True(t, isUnconfirmedLaunchError(row.LaunchError))
	require.Len(t, gen.jtis, 1)
	assert.Nil(t, getTestAgentCredential(t, s, gen.lastJTI()).RevokedAt, "handled as for any reaped launch")
}

// Handler: the create answers exactly as for an acknowledged launch (201,
// provisioning, launch active), with the warning, and nothing is rolled
// back, so the CLI and the web follow the launch as any accepted one.
func TestCreateAgent_AsyncLaunchUnknownSendOutcome(t *testing.T) {
	srv, s, project, client := newAsyncCreateServer(t, true)
	client.answer = func(*RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		return nil, nil, fmt.Errorf("failed to send request: %w", context.DeadlineExceeded)
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "async-unknown", "projectId": project.ID, "task": "do it", "acceptAsyncLaunch": true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.Equal(t, []string{launchOutcomeUnknownWarning}, resp.Warnings)
	assert.Equal(t, string(state.PhaseProvisioning), resp.Agent.Phase)
	require.NotNil(t, resp.Agent.Launch)
	assert.True(t, resp.Agent.Launch.Active)

	require.Len(t, client.sends, 1)
	assert.False(t, client.deleteCalled, "no dispatch-failure delete")
	row := mustAgent(t, s, resp.Agent.ID)
	assert.True(t, row.IsInFlight())
	assert.Equal(t, client.sends[0].LaunchID, row.LaunchID)
	assert.Equal(t, resp.Agent.Launch.ID, row.LaunchID)
}
