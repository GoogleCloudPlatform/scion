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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reincarnate refuses up front, like start and restart, when the agent's
// assigned GCP service account is no longer allowed: 400 with the same
// message shape, and no claim, stop, reprovision or start. A dry run
// reports the same refusal.
func TestReincarnateAgent_RefusedBeforeStopForInadmissibleGCPSA(t *testing.T) {
	cases := []struct {
		name     string
		verified bool
		status   string
		mutate   func(t *testing.T, s store.Store, agent *store.Agent, sa *store.GCPServiceAccount)
		reason   string
	}{
		{name: "unverified", status: store.GCPVerificationUnverified, reason: "not verified"},
		{name: "failed", status: store.GCPVerificationFailed, reason: "not verified"},
		{name: "deleted", verified: true, status: store.GCPVerificationVerified, reason: "no longer available",
			mutate: func(t *testing.T, s store.Store, _ *store.Agent, sa *store.GCPServiceAccount) {
				require.NoError(t, s.DeleteGCPServiceAccount(context.Background(), sa.ID))
			}},
		{name: "email changed", verified: true, status: store.GCPVerificationVerified, reason: "no longer matches",
			mutate: func(t *testing.T, s store.Store, agent *store.Agent, _ *store.GCPServiceAccount) {
				setAgentGCPIdentityEmail(t, s, agent.ID, "other@p.iam.gserviceaccount.com")
			}},
	}
	for i, tc := range cases {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dryRun=%v", tc.name, dryRun), func(t *testing.T) {
				ctx := context.Background()
				disp := newReincarnateTestDispatcher()
				srv, s, project, broker := setupReincarnateTestServer(t, disp)
				agent := newReincarnateTestAgent(t, s, project, broker, nil)
				sa := assignAgentGCPSA(t, s, agent, fmt.Sprintf("reinc-%d-%v", i, dryRun), tc.verified, tc.status)
				if tc.mutate != nil {
					tc.mutate(t, s, agent, sa)
				}
				before, err := s.GetAgent(ctx, agent.ID)
				require.NoError(t, err)

				self := agentIdentityFor(agent.ID, project.ID)
				req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: dryRun})
				rec := httptest.NewRecorder()
				srv.handleReincarnateAgent(rec, req, agent.ID)

				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "Cannot reincarnate agent")
				assert.Contains(t, rec.Body.String(), tc.reason)

				disp.mu.Lock()
				stops, reprovisions, starts := disp.stopCalls, disp.reprovisionCalls, disp.startCalls
				disp.mu.Unlock()
				assert.Zero(t, stops, "the agent must not be stopped before refusing")
				assert.Zero(t, reprovisions, "nothing may be reprovisioned for a refused request")
				assert.Zero(t, starts, "nothing may be started for a refused request")

				after, err := s.GetAgent(ctx, agent.ID)
				require.NoError(t, err)
				assert.Equal(t, before.StateVersion, after.StateVersion, "a refused request writes nothing")
				assert.Equal(t, before.Generation, after.Generation)
				assert.Equal(t, "", after.ReincarnationState, "no claim")
				assert.Equal(t, "running", after.Phase)

				list, err := s.ListAgentReincarnations(ctx, agent.ID)
				require.NoError(t, err)
				assert.Empty(t, list, "no reincarnation record")
			})
		}
	}
}

// An allowed assignment still reincarnates, and the fresh config keeps the
// assigned GCP identity.
func TestReincarnateAgent_AllowedForAdmissibleGCPSA(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	sa := assignAgentGCPSA(t, s, agent, "reinc-ok", true, store.GCPVerificationVerified)

	self := agentIdentityFor(agent.ID, project.ID)
	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	settled := waitForReincarnationSettled(t, s, agent.ID)
	assert.Equal(t, store.AgentReincarnationStateCompleted, settled.State)

	calls, configs := disp.reprovisionSnapshot()
	require.GreaterOrEqual(t, calls, 1)
	require.NotNil(t, configs[0].GCPIdentity)
	assert.Equal(t, sa.ID, configs[0].GCPIdentity.ServiceAccountID)
}

// An agent with no GCP service account assigned is unaffected.
func TestReincarnateAgent_NoGCPSAUnaffected(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	require.Nil(t, agent.AppliedConfig.GCPIdentity)

	self := agentIdentityFor(agent.ID, project.ID)
	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	settled := waitForReincarnationSettled(t, s, agent.ID)
	assert.Equal(t, store.AgentReincarnationStateCompleted, settled.State)
}

// setAgentGCPIdentityEmail changes the email recorded on the agent's
// applied GCP identity, so it no longer matches the service account row.
func setAgentGCPIdentityEmail(t *testing.T, s store.Store, agentID, email string) {
	t.Helper()
	got, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	got.AppliedConfig.GCPIdentity.ServiceAccountEmail = email
	require.NoError(t, s.UpdateAgent(context.Background(), got))
}

// A dry-run move applies the same GCP identity refusal as start and an
// in-place reincarnate: 400 with the same message, and nothing written.
func TestReincarnateMove_DryRun_RefusedForInadmissibleGCPSA(t *testing.T) {
	cases := []struct {
		name     string
		verified bool
		status   string
		mutate   func(t *testing.T, s store.Store, agent *store.Agent)
		reason   string
	}{
		{name: "unverified", status: store.GCPVerificationUnverified, reason: "not verified"},
		{name: "email changed", verified: true, status: store.GCPVerificationVerified, reason: "no longer matches",
			mutate: func(t *testing.T, s store.Store, agent *store.Agent) {
				setAgentGCPIdentityEmail(t, s, agent.ID, "other@p.iam.gserviceaccount.com")
			}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := setupMoveFixture(t, true, nil)
			assignAgentGCPSA(t, f.s, f.agent, fmt.Sprintf("move-%d", i), tc.verified, tc.status)
			if tc.mutate != nil {
				tc.mutate(t, f.s, f.agent)
			}
			// Compare side effects against the agent as it stands now.
			current, err := f.s.GetAgent(ctx, f.agent.ID)
			require.NoError(t, err)
			f.agent = current
			count := f.agentCount(t)

			rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), ErrCodeValidationError)
			assert.Contains(t, rec.Body.String(), "Cannot reincarnate agent")
			assert.Contains(t, rec.Body.String(), tc.reason)
			f.assertNoMoveSideEffects(t, count)
		})
	}
}
