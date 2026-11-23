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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// suspendRevokeHardeningStore intercepts credential and delegation-grant
// revocations for panic, cancellation, and timeout hardening tests.
type suspendRevokeHardeningStore struct {
	store.Store
	fault *storeFaultSwitch

	panicOnCredRevoke  atomic.Bool
	panicOnNextWithTx  atomic.Bool
	armWithTxAfterCred atomic.Bool

	blockOnCredRevoke atomic.Bool
	blockOnWithTx     atomic.Bool
	sawCredDeadline   atomic.Bool
	sawWithTxDeadline atomic.Bool
	credRevokeCalled  atomic.Bool
	withTxCalled      atomic.Bool
}

func (w *suspendRevokeHardeningStore) RevokeAgentCredentialsByAgent(ctx context.Context, agentID, revokedBy, reason string) (int, error) {
	if !w.fault.Active() {
		return w.Store.RevokeAgentCredentialsByAgent(ctx, agentID, revokedBy, reason)
	}
	w.credRevokeCalled.Store(true)
	if w.panicOnCredRevoke.Load() {
		panic("injected credential revoke panic")
	}
	if w.blockOnCredRevoke.Load() {
		if _, ok := ctx.Deadline(); ok {
			w.sawCredDeadline.Store(true)
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	n, err := w.Store.RevokeAgentCredentialsByAgent(ctx, agentID, revokedBy, reason)
	if w.armWithTxAfterCred.Load() {
		w.panicOnNextWithTx.Store(true)
	}
	return n, err
}

func (w *suspendRevokeHardeningStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	if !w.fault.Active() {
		return w.Store.WithTx(ctx, fn)
	}
	if w.panicOnNextWithTx.CompareAndSwap(true, false) {
		w.withTxCalled.Store(true)
		panic("injected delegation revoke panic")
	}
	if w.blockOnWithTx.Load() {
		w.withTxCalled.Store(true)
		if _, ok := ctx.Deadline(); ok {
			w.sawWithTxDeadline.Store(true)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return w.Store.WithTx(ctx, fn)
}

func seedSuspendHardeningGrant(t *testing.T, s store.Store, agent *store.Agent, issuerID string) (*store.AgentDelegationGrant, *store.AgentDelegatedCredential) {
	t.Helper()
	ctx := context.Background()
	g := &store.AgentDelegationGrant{
		AgentID:                 agent.ID,
		AgentProjectID:          agent.ProjectID,
		AgentGeneration:         agent.Generation,
		AgentStateVersion:       agent.StateVersion,
		IssuedRunID:             agent.RunID,
		IssuerUserID:            issuerID,
		BoundaryKind:            "hub",
		CeilingVersion:          1,
		CeilingPermissionIDs:    []string{"agent.read"},
		Name:                    "suspend-hardening",
		MaxCredentialTTLSeconds: agentDelegationCredentialDefaultTTL,
		ExpiresAt:               time.Now().Add(time.Hour),
		IssuanceAuditID:         uuid.NewString(),
	}
	require.NoError(t, s.CreateAgentDelegationGrant(ctx, g))
	c := &store.AgentDelegatedCredential{
		GrantID:                   g.ID,
		AgentID:                   agent.ID,
		KeyHash:                   "suspend-hardening-" + uuid.NewString(),
		Prefix:                    delegatedCredentialPrefix,
		Audience:                  "scion-hub",
		CeilingPermissionIDs:      []string{"agent.read"},
		ExchangeAgentCredentialID: uuid.NewString(),
		ExpiresAt:                 time.Now().Add(10 * time.Minute),
	}
	require.NoError(t, s.CreateAgentDelegatedCredential(ctx, c))
	return g, c
}

func captureSuspendHardeningLogs(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

func assertHasWarnLog(t *testing.T, logs, wantMsg, wantAgentID, wantSubstr string) {
	t.Helper()
	for _, line := range bytes.Split([]byte(logs), []byte("\n")) {
		var entry map[string]any
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		if entry["msg"] != wantMsg {
			continue
		}
		assert.Equal(t, "WARN", entry["level"])
		if wantAgentID != "" {
			assert.Equal(t, wantAgentID, entry["agent_id"])
		}
		if wantSubstr != "" {
			assert.Contains(t, string(line), wantSubstr)
		}
		return
	}
	t.Fatalf("expected WARN log %q (agent_id=%q, substr=%q) not found; logs:\n%s", wantMsg, wantAgentID, wantSubstr, logs)
}

func TestSuspendRevoke_CredentialPanicRecoversAndContinues(t *testing.T) {
	for _, path := range []string{"suspend", "auto-suspend"} {
		t.Run(path, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			wrapped, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *suspendRevokeHardeningStore {
				return &suspendRevokeHardeningStore{Store: inner, fault: f}
			})

			ctx := context.Background()
			agent := setupBrokerAgentInPhase(t, s, "susp-cred-panic-"+path, state.PhaseRunning)
			_, err := s.ClaimAgentStart(ctx, agent.ID, "hub-panic-test", store.StartClaimUser, "", time.Minute)
			require.NoError(t, err)
			agent = mustGetAgent(t, s, agent.ID)
			require.NotEmpty(t, agent.StartClaimID)

			token, err := srv.GenerateAgentToken(agent.ID, agent.ProjectID, nil, AgentRoleFull, nil)
			require.NoError(t, err)
			issuer := &store.User{
				ID:          tid("issuer-cred-panic-" + path),
				Email:       "issuer-cred-panic-" + path + "@example.com",
				DisplayName: "Issuer",
				Role:        store.UserRoleMember,
				Status:      "active",
				Created:     time.Now(),
			}
			require.NoError(t, s.CreateUser(ctx, issuer))
			grant, _ := seedSuspendHardeningGrant(t, s, agent, issuer.ID)

			logs := captureSuspendHardeningLogs(t)
			wrapped.panicOnCredRevoke.Store(true)
			fault.Arm()

			if path == "suspend" {
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			} else {
				markAgentStalled(t, s, agent.ID)
				loaded := mustGetAgent(t, s, agent.ID)
				require.NotPanics(t, func() {
					srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})
				})
			}

			require.True(t, wrapped.credRevokeCalled.Load(), "credential revoke must have been attempted")
			after := mustGetAgent(t, s, agent.ID)
			assert.Equal(t, string(state.PhaseSuspended), after.Phase)
			assert.Empty(t, after.StartClaimID, "start claim must still be released after credential revoke panic")

			gotGrant, err := s.GetAgentDelegationGrant(ctx, grant.ID)
			require.NoError(t, err)
			require.NotNil(t, gotGrant.RevokedAt, "delegation grant must still be revoked after credential revoke panic")
			assert.Equal(t, revokeReasonAgentSuspended, gotGrant.RevokeReason)

			audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
				MutationType: "agent_credential_revoke",
				TargetID:     agent.ID,
			})
			require.NoError(t, err)
			assert.Empty(t, audits, "panicked credential revoke must not emit a revoke audit")

			logStr := logs.String()
			assertHasWarnLog(t, logStr, "Recovered panic revoking agent credentials on suspend", agent.ID, "injected credential revoke panic")
			assert.NotContains(t, logStr, token, "token secret must not appear in logs")
		})
	}
}

func TestSuspendRevoke_DelegationPanicRecoversAndContinues(t *testing.T) {
	for _, path := range []string{"suspend", "auto-suspend"} {
		t.Run(path, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			wrapped, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *suspendRevokeHardeningStore {
				return &suspendRevokeHardeningStore{Store: inner, fault: f}
			})

			ctx := context.Background()
			agent := setupBrokerAgentInPhase(t, s, "susp-deleg-panic-"+path, state.PhaseRunning)
			_, err := s.ClaimAgentStart(ctx, agent.ID, "hub-deleg-panic-test", store.StartClaimUser, "", time.Minute)
			require.NoError(t, err)
			agent = mustGetAgent(t, s, agent.ID)
			require.NotEmpty(t, agent.StartClaimID)

			token, err := srv.GenerateAgentToken(agent.ID, agent.ProjectID, nil, AgentRoleFull, nil)
			require.NoError(t, err)
			claims, err := srv.agentTokenService.ValidateAgentToken(token)
			require.NoError(t, err)

			logs := captureSuspendHardeningLogs(t)
			wrapped.armWithTxAfterCred.Store(true)
			fault.Arm()

			if path == "suspend" {
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			} else {
				markAgentStalled(t, s, agent.ID)
				loaded := mustGetAgent(t, s, agent.ID)
				require.NotPanics(t, func() {
					srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})
				})
			}

			require.True(t, wrapped.withTxCalled.Load(), "delegation grant revoke must have been attempted")
			after := mustGetAgent(t, s, agent.ID)
			assert.Equal(t, string(state.PhaseSuspended), after.Phase)
			assert.Empty(t, after.StartClaimID, "start claim must still be released after delegation revoke panic")

			cred, err := s.GetAgentCredentialByJTIHash(ctx, hashJTI(claims.ID))
			require.NoError(t, err)
			require.NotNil(t, cred.RevokedAt, "credential must remain revoked")

			logStr := logs.String()
			assertHasWarnLog(t, logStr, "Recovered panic revoking agent delegation grants", agent.ID, "injected delegation revoke panic")
			assert.NotContains(t, logStr, token, "token secret must not appear in logs")
		})
	}
}

func TestSuspendRevoke_CancelledContextStillRevokes(t *testing.T) {
	t.Run("credentials", func(t *testing.T) {
		srv, s := testServer(t)
		srv, s, user, project := setupCredentialTestServerOn(t, srv, s)
		agentID := tid("agent-susp-cancelled-cred")
		createCredTestAgent(t, s, agentID, project.ID, user.ID)
		token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
		require.NoError(t, err)
		claims, err := srv.agentTokenService.ValidateAgentToken(token)
		require.NoError(t, err)

		identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api")
		cancelledCtx, cancel := context.WithCancel(contextWithIdentity(context.Background(), identity))
		cancel()

		synctest.Test(t, func(t *testing.T) {
			srv.revokeSuspendedCredentials(cancelledCtx, agentID)
			synctest.Wait()
		})

		cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
		require.NoError(t, err)
		require.NotNil(t, cred.RevokedAt, "credential must be revoked even when caller context is cancelled")
		require.NotNil(t, cred.RevokeReason)
		assert.Equal(t, "agent_suspended", *cred.RevokeReason)

		audits, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			MutationType: "agent_credential_revoke",
			TargetID:     agentID,
		})
		require.NoError(t, err)
		require.Len(t, audits, 1, "audit record must be emitted with preserved actor context")
		assert.Equal(t, "user", audits[0].ActorPrincipalKind)
		assert.Equal(t, user.ID, audits[0].ActorPrincipalID)
	})

	t.Run("delegation_grants", func(t *testing.T) {
		srv, s := testServer(t)
		srv, s, user, project := setupCredentialTestServerOn(t, srv, s)
		agent := createCredTestAgent(t, s, tid("agent-susp-cancelled-deleg"), project.ID, user.ID)
		grant, _ := seedSuspendHardeningGrant(t, s, agent, user.ID)

		identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api")
		cancelledCtx, cancel := context.WithCancel(contextWithIdentity(context.Background(), identity))
		cancel()

		srv.revokeSuspendedRunDelegationGrantsBestEffort(cancelledCtx, agent.ID, agent.RunID)

		gotGrant, err := s.GetAgentDelegationGrant(context.Background(), grant.ID)
		require.NoError(t, err)
		require.NotNil(t, gotGrant.RevokedAt, "delegation grant must be revoked even when caller context is cancelled")
		assert.Equal(t, revokeReasonAgentSuspended, gotGrant.RevokeReason)
		assert.Equal(t, "user:"+user.ID, gotGrant.RevokedBy, "actor context values must be preserved on detached context")
	})
}

func TestSuspendRevoke_TimeoutBoundsSlowRevoker(t *testing.T) {
	t.Run("credentials", func(t *testing.T) {
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendRevokeHardeningStore {
			return &suspendRevokeHardeningStore{Store: inner, fault: f}
		})
		srv, s, user, project := setupCredentialTestServerOn(t, srv, s)
		agentID := tid("agent-susp-timeout-cred")
		createCredTestAgent(t, s, agentID, project.ID, user.ID)
		token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
		require.NoError(t, err)
		claims, err := srv.agentTokenService.ValidateAgentToken(token)
		require.NoError(t, err)

		logs := captureSuspendHardeningLogs(t)
		wrapped.blockOnCredRevoke.Store(true)
		fault.Arm()

		synctest.Test(t, func(t *testing.T) {
			srv.revokeSuspendedCredentials(context.Background(), agentID)
			synctest.Wait()
		})

		assert.True(t, wrapped.credRevokeCalled.Load())
		assert.True(t, wrapped.sawCredDeadline.Load(), "credential revoke must run with a bounded deadline")

		cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
		require.NoError(t, err)
		assert.Nil(t, cred.RevokedAt, "timed-out revoke must leave credential unrevoked")

		audits, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			MutationType: "agent_credential_revoke",
			TargetID:     agentID,
		})
		require.NoError(t, err)
		assert.Empty(t, audits, "timed-out revoke must not emit a revoke audit")

		assertHasWarnLog(t, logs.String(), "Failed to revoke agent credentials on suspend", agentID, context.DeadlineExceeded.Error())
	})

	t.Run("delegation_grants", func(t *testing.T) {
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendRevokeHardeningStore {
			return &suspendRevokeHardeningStore{Store: inner, fault: f}
		})
		srv, s, user, project := setupCredentialTestServerOn(t, srv, s)
		agent := createCredTestAgent(t, s, tid("agent-susp-timeout-deleg"), project.ID, user.ID)
		grant, _ := seedSuspendHardeningGrant(t, s, agent, user.ID)

		logs := captureSuspendHardeningLogs(t)
		wrapped.blockOnWithTx.Store(true)
		fault.Arm()

		synctest.Test(t, func(t *testing.T) {
			srv.revokeSuspendedRunDelegationGrantsBestEffort(context.Background(), agent.ID, agent.RunID)
			synctest.Wait()
		})

		assert.True(t, wrapped.withTxCalled.Load())
		assert.True(t, wrapped.sawWithTxDeadline.Load(), "delegation grant revoke must run with a bounded deadline")

		gotGrant, err := s.GetAgentDelegationGrant(context.Background(), grant.ID)
		require.NoError(t, err)
		assert.Nil(t, gotGrant.RevokedAt, "timed-out revoke must leave grant unrevoked")

		assertHasWarnLog(t, logs.String(), "agent delegation grants not revoked", agent.ID, context.DeadlineExceeded.Error())
	})

	t.Run("delegation_grants_narrow_timeout", func(t *testing.T) {
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendRevokeHardeningStore {
			return &suspendRevokeHardeningStore{Store: inner, fault: f}
		})
		srv, s, user, project := setupCredentialTestServerOn(t, srv, s)
		agent := createCredTestAgent(t, s, tid("agent-susp-timeout-narrow"), project.ID, user.ID)
		grant, _ := seedSuspendHardeningGrant(t, s, agent, user.ID)

		logs := captureSuspendHardeningLogs(t)
		var txCalls atomic.Int32
		var narrowSawDeadline atomic.Bool
		fault.Arm()

		synctest.Test(t, func(t *testing.T) {
			srv.revokeAgentDelegationGrantsBestEffortWith(
				context.Background(),
				store.AgentDelegationGrantFilter{AgentID: agent.ID},
				revokeReasonAgentSuspended,
				func(ctx context.Context, _ store.Store) (store.AgentDelegationGrantFilter, error) {
					txCalls.Add(1)
					if _, ok := ctx.Deadline(); ok {
						narrowSawDeadline.Store(true)
					}
					<-ctx.Done()
					return store.AgentDelegationGrantFilter{AgentID: agent.ID}, ctx.Err()
				},
			)
			synctest.Wait()
		})

		assert.False(t, wrapped.withTxCalled.Load())
		assert.Equal(t, int32(1), txCalls.Load(), "narrow timeout must not trigger un-narrowed fallback attempt")
		assert.True(t, narrowSawDeadline.Load(), "narrow step must receive bounded revokeCtx")

		gotGrant, err := s.GetAgentDelegationGrant(context.Background(), grant.ID)
		require.NoError(t, err)
		assert.Nil(t, gotGrant.RevokedAt, "timed-out narrow revoke must leave grant unrevoked")

		logStr := logs.String()
		assertHasWarnLog(t, logStr, "agent delegation grants not revoked", agent.ID, context.DeadlineExceeded.Error())
		assert.NotContains(t, logStr, "agent delegation revocation not narrowed",
			"timed-out narrow step must skip the fallback warning")
	})
}

func TestSuspendRevoke_NormalPathUnchanged(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	ctx := context.Background()
	agent := setupBrokerAgentInPhase(t, s, "susp-normal", state.PhaseRunning)
	_, err := s.ClaimAgentStart(ctx, agent.ID, "hub-normal-test", store.StartClaimUser, "", time.Minute)
	require.NoError(t, err)
	agent = mustGetAgent(t, s, agent.ID)

	token, err := srv.GenerateAgentToken(agent.ID, agent.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)

	issuer := &store.User{
		ID:          tid("issuer-normal"),
		Email:       "issuer-normal@example.com",
		DisplayName: "Issuer",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, issuer))
	grant, _ := seedSuspendHardeningGrant(t, s, agent, issuer.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after := mustGetAgent(t, s, agent.ID)
	assert.Equal(t, string(state.PhaseSuspended), after.Phase)
	assert.Empty(t, after.StartClaimID)

	cred, err := s.GetAgentCredentialByJTIHash(ctx, hashJTI(claims.ID))
	require.NoError(t, err)
	require.NotNil(t, cred.RevokedAt)

	gotGrant, err := s.GetAgentDelegationGrant(ctx, grant.ID)
	require.NoError(t, err)
	require.NotNil(t, gotGrant.RevokedAt)
	assert.Equal(t, revokeReasonAgentSuspended, gotGrant.RevokeReason)
}
