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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newClaimedAsyncCreate builds a server with start claims on whose HTTP
// dispatcher launches opted-in creates asynchronously, and an opted-in agent
// in phase created on its broker.
func newClaimedAsyncCreate(t *testing.T, name string) (*Server, store.Store, *asyncLaunchClient, *store.Agent) {
	t.Helper()
	srv, s, project, client := newAsyncCreateServer(t, true)
	srv.startClaimsOn = true
	a := &store.Agent{
		ID: tid("cuc-" + name), Slug: "cuc-" + name, Name: "cuc-" + name,
		ProjectID: project.ID, RuntimeBrokerID: tid("broker-create"),
		Phase: string(state.PhaseCreated), LaunchAsyncOptIn: true,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "do the thing"},
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return srv, s, client, mustAgent(t, s, a.ID)
}

// A create send that times out after BeginLaunch ends the launch
// not_launched, but the broker may still have admitted the create: the
// holder keeps its create claim unconfirmed, so a second start is refused
// (ptone/scion#4709).
func TestCreateUnderClaim_SendTimeoutAfterBeginLaunchKeepsClaimUnconfirmed(t *testing.T) {
	srv, s, client, a := newClaimedAsyncCreate(t, "send-timeout")
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		require.True(t, req.AsyncLaunch, "the send is an async launch")
		require.NotEmpty(t, req.LaunchID)
		return nil, nil, context.DeadlineExceeded
	}

	_, err := srv.createUnderClaim(context.Background(), a, func(ctx context.Context) (*CreateDispatchResult, error) {
		return srv.GetDispatcher().DispatchAgentCreate(ctx, a)
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, errStartClaimLost)
	require.Len(t, client.sends, 1)

	got := mustAgent(t, s, a.ID)
	assert.Equal(t, store.LaunchStateEnded, got.LaunchState)
	assert.Equal(t, store.LaunchEndReasonNotLaunched, got.LaunchEndReason)
	require.NotEmpty(t, got.StartClaimID, "the agent still holds its create claim")
	assert.Equal(t, store.StartClaimCreate, got.StartClaimKind)
	assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "an ambiguous send keeps the claim unconfirmed")
	require.NotNil(t, got.StartClaimHoldUntil, "the unconfirmed claim has a hold")
	require.NotNil(t, got.StartClaimUnconfirmedAt)
	assert.Equal(t, srv.startClaimSettings().CreateUnconfirmedHold, got.StartClaimHoldUntil.Sub(*got.StartClaimUnconfirmedAt))

	_, err = s.ClaimAgentStart(context.Background(), a.ID, "other-hub", store.StartClaimUser, "", time.Minute)
	var held *store.ClaimHeldError
	require.ErrorAs(t, err, &held, "a second start is refused while the create may be running")
	assert.Equal(t, store.StartClaimCreate, held.Kind)
}

// An old broker answers the create synchronously, which ends the launch
// not_launched. A claim renewal after that end still finds the claim held,
// so the create is not reported as abandoned (ptone/scion#4709).
func TestCreateUnderClaim_SyncAnswerOutlivingRenewIsNotLost(t *testing.T) {
	srv, s, client, a := newClaimedAsyncCreate(t, "sync-renew")
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		// A broker without async support ignores the launch fields.
		return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Phase: string(state.PhaseRunning)}, Created: true}, nil, nil
	}
	const ttl = 600 * time.Millisecond
	fastClaims(srv, ttl)

	created, err := srv.createUnderClaim(context.Background(), a, func(ctx context.Context) (*CreateDispatchResult, error) {
		res, err := srv.GetDispatcher().DispatchAgentCreate(ctx, a)
		if err != nil {
			return res, err
		}
		// The post-dispatch writes outlast the renew interval, so the
		// claim renews after the launch ended not_launched.
		require.Equal(t, store.LaunchEndReasonNotLaunched, mustAgent(t, s, a.ID).LaunchEndReason)
		time.Sleep(2 * ttl / 3)
		return res, nil
	})
	require.NoError(t, err, "a create that succeeded is not reported as an abandoned start")
	// A synchronous success returns no dispatch result (nil, nil).
	assert.Nil(t, created.AcceptedLaunch(), "a synchronous answer is not an accepted launch")

	got := mustAgent(t, s, a.ID)
	assert.Equal(t, store.LaunchEndReasonNotLaunched, got.LaunchEndReason)
	assert.Empty(t, got.StartClaimID, "the holder released its claim on success")
}
