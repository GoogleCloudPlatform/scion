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
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests of the credentials a suspend revokes, on both suspend paths (the
// suspend route and the scheduler's auto-suspend): every credential of the
// agent, whatever run it was issued for.

// insertRunTestAgentCredential records an active credential for jti,
// issued for runID ("" for a credential issued without a run).
func insertRunTestAgentCredential(t *testing.T, s store.AgentCredentialStore, agentID, projectID, jti, runID string) *store.AgentCredential {
	t.Helper()
	now := time.Now()
	cred := &store.AgentCredential{
		AgentID:      agentID,
		ProjectID:    projectID,
		TokenJTIHash: hashJTI(jti),
		RunID:        runID,
		IssuedAt:     now,
		ExpiresAt:    now.Add(10 * time.Hour),
	}
	require.NoError(t, s.CreateAgentCredential(context.Background(), cred))
	return cred
}

// suspendCredentialAgent creates, on srv's store s, a running agent on an
// online broker whose owner is a member of its project (so the agent is in
// good standing and can refresh its token), with current run runID. It
// returns the agent and its owner.
func suspendCredentialAgent(t *testing.T, srv *Server, s store.Store, name, runID string) (*store.Agent, *store.User) {
	t.Helper()
	ctx := context.Background()
	_, _, user, project := setupCredentialTestServerOn(t, srv, s)
	broker := &store.RuntimeBroker{
		ID:       tid("broker-" + name),
		Name:     "Broker " + name,
		Slug:     "broker-" + name,
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://localhost:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	agentID := tid("agent-" + name)
	agent := &store.Agent{
		ID:              agentID,
		Name:            "agent-" + agentID[:8],
		Slug:            "agent-" + agentID[:8],
		ProjectID:       project.ID,
		OwnerID:         user.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	_, err := s.SetAgentRunID(ctx, agentID, runID, nil)
	require.NoError(t, err)
	return mustGetAgent(t, s, agentID), user
}

// signTokenForRun signs a token for agent issued for runID, as a start does,
// and returns it with its credential, not yet recorded.
func signTokenForRun(srv *Server, agent *store.Agent, runID string) (*AgentTokenClaims, *store.AgentCredential, error) {
	token, cred, err := srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Scopes: ScopesForRole(AgentRoleFull),
	}, runID)
	if err != nil {
		return nil, nil, err
	}
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	return claims, cred, err
}

// issueRunToken is signTokenForRun with the credential recorded.
func issueRunToken(t *testing.T, srv *Server, s store.Store, agent *store.Agent, runID string) (*AgentTokenClaims, *store.AgentCredential) {
	t.Helper()
	claims, cred, err := signTokenForRun(srv, agent, runID)
	require.NoError(t, err)
	require.NoError(t, s.CreateAgentCredential(context.Background(), cred))
	return claims, cred
}

// agentTokenRefusalFor runs the credential checks an agent token passes
// before it authenticates a request.
func agentTokenRefusalFor(srv *Server, claims *AgentTokenClaims) agentTokenRefusal {
	_, refusal := verifyAgentTokenCredential(context.Background(), srv.authConfig, slog.Default(), claims, runScopeRequest{}, runScopeSourceHTTP)
	return refusal
}

// assertCredentialSuspendRevoked asserts that the credential id is revoked
// for a suspension.
func assertCredentialSuspendRevoked(t *testing.T, s store.Store, id, what string) {
	t.Helper()
	cred, err := s.GetAgentCredentialByID(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, cred.RevokedAt, "%s is revoked", what)
	require.NotNil(t, cred.RevokeReason)
	assert.Equal(t, "agent_suspended", *cred.RevokeReason, what)
}

// Manual suspend and auto-suspend both revoke the agent's credentials: its
// token is refused as revoked.
func TestSuspendPathsRevokeAgentCredentials(t *testing.T) {
	for i, path := range lifecycleSuspendPaths {
		t.Run(path.name, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			agent, _ := suspendCredentialAgent(t, srv, s, fmt.Sprintf("suspcred-paths-%d", i), "run-1")
			claims, cred := issueRunToken(t, srv, s, agent, "run-1")
			require.Equal(t, agentTokenAccepted, agentTokenRefusalFor(srv, claims), "the token is accepted before the suspension")

			path.suspend(t, srv, s, agent.ID)

			require.Equal(t, string(state.PhaseSuspended), mustGetAgent(t, s, agent.ID).Phase)
			assertCredentialSuspendRevoked(t, s, cred.ID, "the agent's credential")
			assert.Equal(t, agentTokenRevoked, agentTokenRefusalFor(srv, claims), "the agent's token is refused as revoked")
		})
	}
}

// The scheduler's auto-suspend runs with no request principal, yet it
// records its credential revoke once, naming the system actor, as a manual
// suspend records it naming the suspending user (ptone/scion#2403): without
// an actor the audit write fails and leaves only a warning.
func TestAutoSuspendRecordsCredentialRevokeAsSystem(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv, s, _, _ := engineTestServer(t)
	agent, _ := suspendCredentialAgent(t, srv, s, "suspcred-autoaudit", "run-1")
	token, err := srv.GenerateAgentToken(agent.ID, agent.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	markAgentStalled(t, s, agent.ID)
	srv.autoSuspendStalledAgents(context.Background(), []store.Agent{*mustGetAgent(t, s, agent.ID)})
	require.Equal(t, string(state.PhaseSuspended), mustGetAgent(t, s, agent.ID).Phase)

	revokes := func() []*store.MutationAuditRecord {
		recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			MutationType: "agent_credential_revoke",
			TargetID:     agent.ID,
		})
		require.NoError(t, err)
		return recs
	}
	// The record is written in a fire-and-forget goroutine.
	require.Eventually(t, func() bool { return len(revokes()) > 0 }, 5*time.Second, 10*time.Millisecond,
		"the auto-suspend's credential revoke is not recorded; logs:\n%s", logs)
	recs := revokes()
	require.Len(t, recs, 1, "the auto-suspend's credential revoke is recorded exactly once")
	assert.Equal(t, "agent_credential", recs[0].TargetType)
	assert.Equal(t, mintAuditSystemActorKind, recs[0].ActorPrincipalKind)
	assert.Equal(t, mintAuditSystemActorID, recs[0].ActorPrincipalID)
	// Only the credential revoke's own write is checked: the auto-suspend's
	// other mutation audits are not part of this test.
	for _, line := range strings.Split(logs.String(), "\n") {
		var l struct {
			Msg          string `json:"msg"`
			MutationType string `json:"mutation_type"`
		}
		if json.Unmarshal([]byte(line), &l) == nil && l.Msg == "failed to emit mutation audit record" {
			assert.NotEqual(t, "agent_credential_revoke", l.MutationType, "the credential revoke's audit write failed: %s", line)
		}
	}
	assert.NotContains(t, logs.String(), token, "the token must not appear in the logs")
}

// A resume racing the suspend, on the unwrapped store s: as beginRun does,
// it records the new run runID and its credential in one write.
func resumeWithCredential(srv *Server, s store.Store, agentID, runID string) (*AgentTokenClaims, *store.AgentCredential, error) {
	ctx := context.Background()
	agent, err := s.GetAgent(ctx, agentID)
	if err != nil {
		return nil, nil, err
	}
	claims, cred, err := signTokenForRun(srv, agent, runID)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.SetAgentRunID(ctx, agentID, runID, cred); err != nil {
		return nil, nil, err
	}
	return claims, cred, nil
}

// A suspend revokes every credential of the agent, including one a resume
// minted for a new run after the suspension was recorded: that resume loses
// its token and starts again.
func TestSuspendRevokesAResumedRunsCredential(t *testing.T) {
	for i, path := range lifecycleSuspendPaths {
		t.Run(path.name, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			w := installResumeAfterSuspend(t, srv, nil)
			agent, _ := suspendCredentialAgent(t, srv, s, fmt.Sprintf("suspcred-resumed-%d", i), "run-1")
			_, stopped := issueRunToken(t, srv, s, agent, "run-1")

			var claims *AgentTokenClaims
			var cred *store.AgentCredential
			var resumeErr error
			w.onRecorded = func() {
				claims, cred, resumeErr = resumeWithCredential(srv, s, agent.ID, "run-2")
			}

			path.suspend(t, srv, s, agent.ID)

			require.True(t, w.fired.Load(), "the suspension was recorded")
			require.NoError(t, resumeErr)
			require.NotNil(t, cred)
			assertCredentialSuspendRevoked(t, s, stopped.ID, "the stopped run's credential")
			assertCredentialSuspendRevoked(t, s, cred.ID, "the resumed run's credential")
			assert.Equal(t, agentTokenRevoked, agentTokenRefusalFor(srv, claims), "the resumed run's token is refused")
		})
	}
}

// A suspend revokes every credential of the agent, whatever run it was
// issued for.
func TestSuspendRevokesEveryRunsCredentials(t *testing.T) {
	for i, path := range lifecycleSuspendPaths {
		t.Run(path.name, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			agent, _ := suspendCredentialAgent(t, srv, s, fmt.Sprintf("suspcred-every-%d", i), "run-1")
			_, current := issueRunToken(t, srv, s, agent, "run-1")
			earlier := insertRunTestAgentCredential(t, s, agent.ID, agent.ProjectID, "jti-earlier-"+agent.ID, "run-0")
			unbound := insertRunTestAgentCredential(t, s, agent.ID, agent.ProjectID, "jti-unbound-"+agent.ID, "")

			path.suspend(t, srv, s, agent.ID)

			require.Equal(t, string(state.PhaseSuspended), mustGetAgent(t, s, agent.ID).Phase)
			assertCredentialSuspendRevoked(t, s, current.ID, "the stopped run's credential")
			assertCredentialSuspendRevoked(t, s, earlier.ID, "an earlier run's credential")
			assertCredentialSuspendRevoked(t, s, unbound.ID, "a credential issued without a run")
		})
	}
}
