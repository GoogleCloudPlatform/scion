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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// suspendPathHardeningStore intercepts store methods used by adjacent suspend
// and stop cleanup steps for panic, cancellation, and timeout tests.
type suspendPathHardeningStore struct {
	store.Store
	fault *storeFaultSwitch

	panicLimitName      atomic.Value // string; if non-empty, panics on GetLimitDefinitionByName for that limit
	blockOnQuotaRelease atomic.Bool
	sawQuotaDeadline    atomic.Bool
	quotaReleaseCalled  atomic.Bool

	panicOnSingleRevoke atomic.Bool
	blockOnSingleRevoke atomic.Bool
	sawSingleDeadline   atomic.Bool
	singleRevokeCalled  atomic.Bool

	panicOnBulkRevoke atomic.Bool
	blockOnBulkRevoke atomic.Bool
	sawBulkDeadline   atomic.Bool
	bulkRevokeCalled  atomic.Bool

	panicOnGetAgent     atomic.Bool
	blockOnGetAgent     atomic.Bool
	sawGetAgentDeadline atomic.Bool
	getAgentCalled      atomic.Bool

	panicOnReleaseClaim atomic.Bool
	releaseClaimCalled  atomic.Bool
}

func (w *suspendPathHardeningStore) GetLimitDefinitionByName(ctx context.Context, name string) (*store.LimitDefinition, error) {
	if w.fault.Active() {
		if target, ok := w.panicLimitName.Load().(string); ok && target == name {
			w.quotaReleaseCalled.Store(true)
			panic("injected quota limit lookup panic")
		}
		if w.blockOnQuotaRelease.Load() {
			w.quotaReleaseCalled.Store(true)
			if _, ok := ctx.Deadline(); ok {
				w.sawQuotaDeadline.Store(true)
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	return w.Store.GetLimitDefinitionByName(ctx, name)
}

func (w *suspendPathHardeningStore) RevokeAgentCredential(ctx context.Context, id, revokedBy, reason string) error {
	if !w.fault.Active() {
		return w.Store.RevokeAgentCredential(ctx, id, revokedBy, reason)
	}
	w.singleRevokeCalled.Store(true)
	if w.panicOnSingleRevoke.Load() {
		panic("injected single credential revoke panic")
	}
	if w.blockOnSingleRevoke.Load() {
		if _, ok := ctx.Deadline(); ok {
			w.sawSingleDeadline.Store(true)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return w.Store.RevokeAgentCredential(ctx, id, revokedBy, reason)
}

func (w *suspendPathHardeningStore) RevokeAgentCredentialsByAgent(ctx context.Context, agentID, revokedBy, reason string) (int, error) {
	if !w.fault.Active() {
		return w.Store.RevokeAgentCredentialsByAgent(ctx, agentID, revokedBy, reason)
	}
	w.bulkRevokeCalled.Store(true)
	if w.panicOnBulkRevoke.Load() {
		panic("injected bulk credential revoke panic")
	}
	if w.blockOnBulkRevoke.Load() {
		if _, ok := ctx.Deadline(); ok {
			w.sawBulkDeadline.Store(true)
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return w.Store.RevokeAgentCredentialsByAgent(ctx, agentID, revokedBy, reason)
}

func (w *suspendPathHardeningStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if !w.fault.Active() {
		return w.Store.GetAgent(ctx, id)
	}
	w.getAgentCalled.Store(true)
	if w.panicOnGetAgent.Load() {
		panic("injected get agent panic")
	}
	if w.blockOnGetAgent.Load() {
		if _, ok := ctx.Deadline(); ok {
			w.sawGetAgentDeadline.Store(true)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return w.Store.GetAgent(ctx, id)
}

func (w *suspendPathHardeningStore) ReleaseSupersededStart(ctx context.Context, agentID, claimID string, stopIntentAt time.Time) (bool, error) {
	if !w.fault.Active() {
		return w.Store.ReleaseSupersededStart(ctx, agentID, claimID, stopIntentAt)
	}
	w.releaseClaimCalled.Store(true)
	if w.panicOnReleaseClaim.Load() {
		panic("injected release superseded start panic")
	}
	return w.Store.ReleaseSupersededStart(ctx, agentID, claimID, stopIntentAt)
}

func seedActiveQuotaReservation(t *testing.T, s store.Store, limitName, scopeType, scopeID, subject, resourceID string) *store.LimitDefinition {
	t.Helper()
	ctx := context.Background()
	def, err := s.GetLimitDefinitionByName(ctx, limitName)
	if err != nil {
		def, err = s.CreateLimitDefinition(ctx, &store.LimitDefinition{
			Name:         limitName,
			ResourceType: "agent",
			Unit:         "count",
			Description:  "test limit: " + limitName,
			DefaultValue: 10,
			System:       true,
		})
		require.NoError(t, err)
	}
	res := &store.UsageReservation{
		LimitDefinitionID: def.ID,
		SubjectID:         subject,
		ScopeType:         scopeType,
		ScopeID:           scopeID,
		ResourceID:        resourceID,
		Reserved:          1,
	}
	_, err = s.CreateUsageReservation(ctx, res)
	require.NoError(t, err)
	return def
}

func TestQuotaRelease_Hardening(t *testing.T) {
	t.Run("panic_recovers_and_continues_suspend", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, _, _ := engineTestServer(t)
		wrapped := &suspendPathHardeningStore{Store: s, fault: &storeFaultSwitch{}}
		srv.quotaService = &QuotaService{store: wrapped, logger: slog.Default()}

		ctx := context.Background()
		agent := setupBrokerAgentInPhase(t, s, "quota-panic-susp", state.PhaseRunning)
		_, err := s.ClaimAgentStart(ctx, agent.ID, "hub-quota-panic", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
		agent = mustGetAgent(t, s, agent.ID)

		projDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, agent.ProjectID, agent.ProjectID, agent.ID)
		_ = seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.RuntimeBrokerID, agent.ID)

		wrapped.panicLimitName.Store(store.LimitMaxAgentsPerBroker)
		wrapped.fault.Arm()

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		assert.True(t, wrapped.quotaReleaseCalled.Load(), "broker quota release must have been attempted")
		after := mustGetAgent(t, s, agent.ID)
		assert.Equal(t, string(state.PhaseSuspended), after.Phase)
		assert.Empty(t, after.StartClaimID, "start claim must still be released after broker quota panic")

		activeProj, err := s.CountActiveReservations(ctx, projDef.ID, agent.ProjectID, store.QuotaScopeProject, agent.ProjectID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeProj, "project quota must still be released after broker quota panic")

		assertHasWarnLog(t, logs.String(), "recovered panic releasing quota reservation", "", "injected quota limit lookup panic")
	})

	t.Run("cancelled_context_still_releases", func(t *testing.T) {
		_, s := testServer(t)
		qs := &QuotaService{store: s, logger: slog.Default()}
		ctx := context.Background()
		projID := tid("proj-quota-cancel")
		agentID := tid("agent-quota-cancel")
		def := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, projID, projID, agentID)

		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()

		qs.Release(cancelledCtx, store.LimitMaxAgentsPerProject, agentID)

		active, err := s.CountActiveReservations(ctx, def.ID, projID, store.QuotaScopeProject, projID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), active, "quota reservation must be released even when caller context is cancelled")
	})

	t.Run("timeout_bounds_slow_release", func(t *testing.T) {
		_, s := testServer(t)
		fault := &storeFaultSwitch{}
		wrapped := &suspendPathHardeningStore{Store: s, fault: fault}
		logs := captureSuspendHardeningLogs(t)
		qs := &QuotaService{store: wrapped, logger: slog.Default()}

		wrapped.blockOnQuotaRelease.Store(true)
		fault.Arm()

		synctest.Test(t, func(t *testing.T) {
			qs.Release(context.Background(), store.LimitMaxAgentsPerBroker, tid("agent-quota-timeout"))
			synctest.Wait()
		})

		assert.True(t, wrapped.quotaReleaseCalled.Load())
		assert.True(t, wrapped.sawQuotaDeadline.Load(), "quota release must run with a bounded deadline")
		assertHasWarnLog(t, logs.String(), "failed to release quota reservation: limit lookup failed", "", context.DeadlineExceeded.Error())
	})

	t.Run("nil_context_and_store_safe", func(t *testing.T) {
		var nilQS *QuotaService
		require.NotPanics(t, func() {
			nilQS.Release(nil, store.LimitMaxAgentsPerBroker, "agent-nil") //nolint:staticcheck // intentional nil ctx test
		})
		emptyQS := &QuotaService{}
		require.NotPanics(t, func() {
			emptyQS.Release(nil, store.LimitMaxAgentsPerBroker, "agent-empty") //nolint:staticcheck // intentional nil ctx test
		})
	})
}

func TestRevokeAgentCredentialsBestEffort_Hardening(t *testing.T) {
	t.Run("panic_recovers_and_logs_without_secret", func(t *testing.T) {
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		srv, s, user, project := setupCredentialTestServerOn(t, srv, s)
		agentID := tid("agent-besteffort-panic")
		createCredTestAgent(t, s, agentID, project.ID, user.ID)
		token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
		require.NoError(t, err)

		logs := captureSuspendHardeningLogs(t)
		wrapped.panicOnBulkRevoke.Store(true)
		fault.Arm()

		require.NotPanics(t, func() {
			revokeAgentCredentialsBestEffort(context.Background(), wrapped, agentID, agentCredentialRevokeReasonStartFailed)
		})

		assert.True(t, wrapped.bulkRevokeCalled.Load())
		logStr := logs.String()
		assertHasWarnLog(t, logStr, "Recovered panic revoking agent credentials", agentID, "injected bulk credential revoke panic")
		assert.NotContains(t, logStr, token, "token secret must not appear in logs")
	})

	t.Run("nil_context_still_revokes", func(t *testing.T) {
		srv, s, user, project := setupCredentialTestServer(t)
		agentID := tid("agent-besteffort-nilctx")
		createCredTestAgent(t, s, agentID, project.ID, user.ID)
		token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
		require.NoError(t, err)
		claims, err := srv.agentTokenService.ValidateAgentToken(token)
		require.NoError(t, err)

		require.NotPanics(t, func() {
			revokeAgentCredentialsBestEffort(nil, s, agentID, agentCredentialRevokeReasonCreateFailed) //nolint:staticcheck // intentional nil ctx test
		})

		cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
		require.NoError(t, err)
		require.NotNil(t, cred.RevokedAt, "credential must be revoked even when ctx is nil")
	})

	t.Run("timeout_bounds_slow_revoke", func(t *testing.T) {
		_, s := testServer(t)
		fault := &storeFaultSwitch{}
		wrapped := &suspendPathHardeningStore{Store: s, fault: fault}
		logs := captureSuspendHardeningLogs(t)
		agentID := tid("agent-besteffort-timeout")

		wrapped.blockOnBulkRevoke.Store(true)
		fault.Arm()

		synctest.Test(t, func(t *testing.T) {
			revokeAgentCredentialsBestEffort(context.Background(), wrapped, agentID, agentCredentialRevokeReasonStartFailed)
			synctest.Wait()
		})

		assert.True(t, wrapped.bulkRevokeCalled.Load())
		assert.True(t, wrapped.sawBulkDeadline.Load(), "best-effort credential revoke must run with a bounded deadline")
		assertHasWarnLog(t, logs.String(), "Failed to revoke agent credentials", agentID, context.DeadlineExceeded.Error())
	})
}

func TestTokenRefreshRevoke_Hardening(t *testing.T) {
	t.Run("panic_recovers_and_returns_refreshed_token", func(t *testing.T) {
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		srv, s, user, project := setupCredentialTestServerOn(t, srv, s)
		agentID := tid("agent-refresh-panic")
		createCredTestAgent(t, s, agentID, project.ID, user.ID)
		token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
		require.NoError(t, err)
		claims, err := srv.agentTokenService.ValidateAgentToken(token)
		require.NoError(t, err)

		logs := captureSuspendHardeningLogs(t)
		wrapped.panicOnSingleRevoke.Store(true)
		fault.Arm()

		req := buildAgentRefreshRequest(agentID, claims, "")
		rec := httptest.NewRecorder()
		require.NotPanics(t, func() {
			srv.handleAgentTokenRefresh(rec, req, agentID)
		})

		require.Equal(t, http.StatusOK, rec.Code, "refresh must still succeed if old credential revoke panics; body: %s", rec.Body.String())
		assert.True(t, wrapped.singleRevokeCalled.Load())

		logStr := logs.String()
		assertHasWarnLog(t, logStr, "Recovered panic revoking old credential after refresh", agentID, "injected single credential revoke panic")
		assert.NotContains(t, logStr, token, "token secret must not appear in logs")
	})

	t.Run("cancelled_context_still_revokes", func(t *testing.T) {
		srv, s, user, project := setupCredentialTestServer(t)
		agentID := tid("agent-refresh-cancel")
		createCredTestAgent(t, s, agentID, project.ID, user.ID)
		token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
		require.NoError(t, err)
		claims, err := srv.agentTokenService.ValidateAgentToken(token)
		require.NoError(t, err)
		cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
		require.NoError(t, err)

		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()

		revokeAgentCredentialBestEffort(cancelledCtx, s, agentID, cred.ID, "refreshed")

		got, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
		require.NoError(t, err)
		require.NotNil(t, got.RevokedAt, "old credential must still be revoked when caller context is cancelled")
	})

	t.Run("timeout_bounds_slow_revoke", func(t *testing.T) {
		_, s := testServer(t)
		fault := &storeFaultSwitch{}
		wrapped := &suspendPathHardeningStore{Store: s, fault: fault}
		logs := captureSuspendHardeningLogs(t)
		agentID := tid("agent-refresh-timeout")

		wrapped.blockOnSingleRevoke.Store(true)
		fault.Arm()

		synctest.Test(t, func(t *testing.T) {
			revokeAgentCredentialBestEffort(context.Background(), wrapped, agentID, tid("cred-timeout"), "refreshed")
			synctest.Wait()
		})

		assert.True(t, wrapped.singleRevokeCalled.Load())
		assert.True(t, wrapped.sawSingleDeadline.Load(), "single credential revoke must run with a bounded deadline")
		assertHasWarnLog(t, logs.String(), "Failed to revoke old credential after refresh", agentID, context.DeadlineExceeded.Error())
	})
}

func TestStopRunStillCurrent_Hardening(t *testing.T) {
	t.Run("panic_recovers_fail_open", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		agent := setupBrokerAgentInPhase(t, s, "stop-run-panic", state.PhaseRunning)
		agent.RunID = tid("run-panic-initial")
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		wrapped.panicOnGetAgent.Store(true)
		fault.Arm()

		var stillCurrent bool
		require.NotPanics(t, func() {
			stillCurrent = srv.stopRunStillCurrent(context.Background(), agent.ID, agent.RunID, "suspend (credential revoke)")
		})

		assert.True(t, wrapped.getAgentCalled.Load())
		assert.True(t, stillCurrent, "panic in stopRunStillCurrent must fail open so stopped container credentials are still revoked")
		assertHasWarnLog(t, logs.String(), "Recovered panic checking stop run currency", agent.ID, "injected get agent panic")
	})

	t.Run("cancelled_context_detects_newer_run", func(t *testing.T) {
		srv, s := testServer(t)
		agent := setupBrokerAgentInPhase(t, s, "stop-run-cancel", state.PhaseRunning)
		oldRunID := tid("run-old")
		agent.RunID = oldRunID
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		// Simulate a newer run starting on the row before stopRunStillCurrent runs.
		agent.RunID = tid("run-newer")
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()

		stillCurrent := srv.stopRunStillCurrent(cancelledCtx, agent.ID, oldRunID, "suspend (credential revoke)")
		assert.False(t, stillCurrent, "detached stopRunStillCurrent must still read the row and detect a newer run when caller context is cancelled")
	})

	t.Run("timeout_bounds_slow_read", func(t *testing.T) {
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		agent := setupBrokerAgentInPhase(t, s, "stop-run-timeout", state.PhaseRunning)
		agent.RunID = tid("run-timeout-initial")
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		wrapped.blockOnGetAgent.Store(true)
		fault.Arm()

		var stillCurrent bool
		synctest.Test(t, func(t *testing.T) {
			stillCurrent = srv.stopRunStillCurrent(context.Background(), agent.ID, agent.RunID, "suspend (credential revoke)")
			synctest.Wait()
		})

		assert.True(t, wrapped.getAgentCalled.Load())
		assert.True(t, wrapped.sawGetAgentDeadline.Load(), "stopRunStillCurrent must run with a bounded deadline")
		assert.True(t, stillCurrent, "timed-out read must fail open")
	})
}

func TestReleaseSupersededClaim_Hardening(t *testing.T) {
	t.Run("panic_recovers_and_logs", func(t *testing.T) {
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		agent := setupBrokerAgentInPhase(t, s, "claim-panic", state.PhaseRunning)
		claim, err := s.ClaimAgentStart(context.Background(), agent.ID, "hub-claim-panic", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)

		logs := captureSuspendHardeningLogs(t)
		wrapped.panicOnReleaseClaim.Store(true)
		fault.Arm()

		require.NotPanics(t, func() {
			srv.releaseSupersededClaim(context.Background(), agent.ID, claim.ID, time.Now().UTC())
		})

		assert.True(t, wrapped.releaseClaimCalled.Load())
		assertHasWarnLog(t, logs.String(), "Recovered panic releasing superseded start claim", agent.ID, "injected release superseded start panic")
	})

	t.Run("nil_context_still_releases", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		agent := setupBrokerAgentInPhase(t, s, "claim-nilctx", state.PhaseRunning)
		claim, err := s.ClaimAgentStart(ctx, agent.ID, "hub-claim-nilctx", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
		intentAt, err := srv.recordRunIntent(ctx, agent, store.RunIntentStopped)
		require.NoError(t, err)

		require.NotPanics(t, func() {
			srv.releaseSupersededClaim(nil, agent.ID, claim.ID, intentAt) //nolint:staticcheck // intentional nil ctx test
		})

		after := mustGetAgent(t, s, agent.ID)
		assert.Empty(t, after.StartClaimID, "start claim must be released even when ctx is nil")
	})
}
