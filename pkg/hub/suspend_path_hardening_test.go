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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
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

	panicOnGetAgent          atomic.Bool
	armGetAgentPanicPostCred atomic.Bool
	blockOnGetAgent          atomic.Bool
	sawGetAgentDeadline      atomic.Bool
	getAgentCalled           atomic.Bool

	panicOnReleaseClaim atomic.Bool
	releaseClaimCalled  atomic.Bool

	panicOnGetUser          atomic.Bool
	armGetUserPanicPostCred atomic.Bool
	blockOnGetUser          atomic.Bool
	sawGetUserDeadline      atomic.Bool
	getUserCalled           atomic.Bool

	panicClearIdleFor    atomic.Value // string agentID; if non-empty, panics on ClearIdleSuspendClaim for that agent
	errClearIdleFor      atomic.Value // string agentID; if non-empty, returns error on ClearIdleSuspendClaim for that agent
	clearIdleClaimCalled atomic.Bool

	panicOnHasDeliveryHold atomic.Bool
	hasDeliveryHoldCalled  atomic.Bool

	armGetAgentPanicPostStatusUpdate atomic.Bool
	returnErrRunChanged              atomic.Bool
	armGetAgentPanicOnRunChanged     atomic.Bool
	panicOnUpdateStatusFor           atomic.Value // string agentID; if non-empty, panics on UpdateAgentStatus for that agent

	panicOnPutSuspendScreen     atomic.Bool
	blockOnPutSuspendScreen     atomic.Bool
	sawPutSuspendScreenDeadline atomic.Bool
	putSuspendScreenCalled      atomic.Bool
	panicOnDeleteSuspendScreen  atomic.Value // string agentID; "*" panics for any agent
	deleteSuspendScreenCalled   atomic.Bool
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
	n, err := w.Store.RevokeAgentCredentialsByAgent(ctx, agentID, revokedBy, reason)
	if w.armGetAgentPanicPostCred.Load() {
		w.panicOnGetAgent.Store(true)
	}
	if w.armGetUserPanicPostCred.Load() {
		w.panicOnGetUser.Store(true)
	}
	return n, err
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

func (w *suspendPathHardeningStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if !w.fault.Active() {
		return w.Store.GetUser(ctx, id)
	}
	w.getUserCalled.Store(true)
	if w.panicOnGetUser.Load() {
		panic("injected get user panic")
	}
	if w.blockOnGetUser.Load() {
		if _, ok := ctx.Deadline(); ok {
			w.sawGetUserDeadline.Store(true)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return w.Store.GetUser(ctx, id)
}

func (w *suspendPathHardeningStore) ClearIdleSuspendClaim(ctx context.Context, agentID string) error {
	if !w.fault.Active() {
		return w.Store.ClearIdleSuspendClaim(ctx, agentID)
	}
	w.clearIdleClaimCalled.Store(true)
	if target, ok := w.panicClearIdleFor.Load().(string); ok && target == agentID {
		panic("injected clear idle suspend claim panic")
	}
	if target, ok := w.errClearIdleFor.Load().(string); ok && target == agentID {
		return errors.New("injected clear idle suspend claim error")
	}
	return w.Store.ClearIdleSuspendClaim(ctx, agentID)
}

func (w *suspendPathHardeningStore) UpdateAgentStatus(ctx context.Context, id string, update store.AgentStatusUpdate) error {
	if !w.fault.Active() {
		return w.Store.UpdateAgentStatus(ctx, id, update)
	}
	if target, ok := w.panicOnUpdateStatusFor.Load().(string); ok && target == id {
		panic("injected update agent status panic")
	}
	if w.returnErrRunChanged.Load() {
		if w.armGetAgentPanicOnRunChanged.Load() {
			w.panicOnGetAgent.Store(true)
		}
		return store.ErrRunChanged
	}
	err := w.Store.UpdateAgentStatus(ctx, id, update)
	if err == nil && w.armGetAgentPanicPostStatusUpdate.Load() {
		w.panicOnGetAgent.Store(true)
	}
	return err
}

func (w *suspendPathHardeningStore) PutAgentSuspendScreen(ctx context.Context, screen *store.AgentSuspendScreen) error {
	if !w.fault.Active() {
		return w.Store.PutAgentSuspendScreen(ctx, screen)
	}
	w.putSuspendScreenCalled.Store(true)
	if w.panicOnPutSuspendScreen.Load() {
		panic("injected put suspend screen panic")
	}
	if w.blockOnPutSuspendScreen.Load() {
		if _, ok := ctx.Deadline(); ok {
			w.sawPutSuspendScreenDeadline.Store(true)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return w.Store.PutAgentSuspendScreen(ctx, screen)
}

func (w *suspendPathHardeningStore) DeleteAgentSuspendScreen(ctx context.Context, agentID string) error {
	if !w.fault.Active() {
		return w.Store.DeleteAgentSuspendScreen(ctx, agentID)
	}
	w.deleteSuspendScreenCalled.Store(true)
	if target, ok := w.panicOnDeleteSuspendScreen.Load().(string); ok && (target == "*" || target == agentID) {
		panic("injected delete suspend screen panic")
	}
	return w.Store.DeleteAgentSuspendScreen(ctx, agentID)
}

func (w *suspendPathHardeningStore) HasAgentDeliveryHold(ctx context.Context, agentID string) (bool, error) {
	if !w.fault.Active() {
		return w.Store.HasAgentDeliveryHold(ctx, agentID)
	}
	w.hasDeliveryHoldCalled.Store(true)
	if w.panicOnHasDeliveryHold.Load() {
		panic("injected has delivery hold panic")
	}
	return w.Store.HasAgentDeliveryHold(ctx, agentID)
}

type statusRecordingPublisher struct {
	EventPublisher
	mu       sync.Mutex
	statuses map[string][]string // agentID -> phases
}

func newStatusRecordingPublisher(inner EventPublisher) *statusRecordingPublisher {
	return &statusRecordingPublisher{
		EventPublisher: inner,
		statuses:       make(map[string][]string),
	}
}

func (p *statusRecordingPublisher) PublishAgentStatus(ctx context.Context, agent *store.Agent) {
	if agent != nil {
		p.mu.Lock()
		p.statuses[agent.ID] = append(p.statuses[agent.ID], agent.Phase)
		p.mu.Unlock()
	}
	if p.EventPublisher != nil {
		p.EventPublisher.PublishAgentStatus(ctx, agent)
	}
}

func (p *statusRecordingPublisher) publishedPhases(agentID string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.statuses[agentID]...)
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

	t.Run("nil_logger_detects_newer_run", func(t *testing.T) {
		srv, s := testServer(t)
		agent := setupBrokerAgentInPhase(t, s, "stop-run-nillog", state.PhaseRunning)
		oldRunID := tid("run-nillog-old")
		agent.RunID = oldRunID
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		agent.RunID = tid("run-nillog-newer")
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		srv.agentLifecycleLog = nil

		var stillCurrent bool
		require.NotPanics(t, func() {
			stillCurrent = srv.stopRunStillCurrent(context.Background(), agent.ID, oldRunID, "suspend (credential revoke)")
		})
		assert.False(t, stillCurrent, "nil agentLifecycleLog must not panic and flip stopRunStillCurrent to true")
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

func TestSyncAgentAfterStopRecord_Hardening(t *testing.T) {
	t.Run("suspend_get_agent_panic_still_releases_quotas_and_claim", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		bus := NewChannelEventPublisher()
		t.Cleanup(bus.Close)
		pub := newStatusRecordingPublisher(bus)
		srv.events = pub
		srv.SetDispatcher(&engineStubDispatcher{})
		srv.quotaService = &QuotaService{store: s, logger: slog.Default()}

		ctx := context.Background()
		agent := setupBrokerAgentInPhase(t, s, "sync-panic-susp", state.PhaseRunning)
		_, err := s.ClaimAgentStart(ctx, agent.ID, "hub-sync-panic", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
		agent = mustGetAgent(t, s, agent.ID)

		projDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, agent.ProjectID, agent.ProjectID, agent.ID)
		brokerDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.RuntimeBrokerID, agent.ID)

		wrapped.armGetAgentPanicPostCred.Store(true)
		fault.Arm()

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		assert.True(t, wrapped.getAgentCalled.Load(), "GetAgent refresh must have been attempted")
		after := mustGetAgent(t, s, agent.ID)
		assert.Equal(t, string(state.PhaseSuspended), after.Phase)
		assert.Empty(t, after.StartClaimID, "start claim must still be released after GetAgent panic")

		activeBroker, err := s.CountActiveReservations(ctx, brokerDef.ID, agent.RuntimeBrokerID, store.QuotaScopeBroker, agent.RuntimeBrokerID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeBroker, "broker quota must still be released after GetAgent panic")

		activeProj, err := s.CountActiveReservations(ctx, projDef.ID, agent.ProjectID, store.QuotaScopeProject, agent.ProjectID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeProj, "project quota must still be released after GetAgent panic")

		assert.Contains(t, pub.publishedPhases(agent.ID), string(state.PhaseSuspended), "PublishAgentStatus must still run after GetAgent panic")
		assertHasWarnLog(t, logs.String(), "Recovered panic refreshing agent after stop", agent.ID, "injected get agent panic")
	})

	t.Run("auto_suspend_get_agent_panic_still_releases_quotas_and_claim", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		bus := NewChannelEventPublisher()
		t.Cleanup(bus.Close)
		pub := newStatusRecordingPublisher(bus)
		srv.events = pub
		disp := &engineStubDispatcher{}
		srv.SetDispatcher(disp)
		srv.quotaService = &QuotaService{store: s, logger: slog.Default()}

		ctx := context.Background()
		agent := setupBrokerAgentInPhase(t, s, "autosusp-sync-panic", state.PhaseRunning)
		agent.Activity = string(state.ActivityStalled)
		require.NoError(t, s.UpdateAgent(ctx, agent))
		_, err := s.ClaimAgentStart(ctx, agent.ID, "hub-autosusp-panic", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
		agent = mustGetAgent(t, s, agent.ID)

		projDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, agent.ProjectID, agent.ProjectID, agent.ID)
		brokerDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.RuntimeBrokerID, agent.ID)

		wrapped.armGetAgentPanicPostCred.Store(true)
		fault.Arm()

		var ok bool
		require.NotPanics(t, func() {
			ok = srv.autoSuspendAgent(ctx, disp, agent, state.SuspendReasonStalled)
		})
		require.True(t, ok, "autoSuspendAgent must still complete after GetAgent panic")

		assert.True(t, wrapped.getAgentCalled.Load())
		after := mustGetAgent(t, s, agent.ID)
		assert.Equal(t, string(state.PhaseSuspended), after.Phase)
		assert.Empty(t, after.StartClaimID, "start claim must still be released after GetAgent panic in autoSuspendAgent")

		activeBroker, err := s.CountActiveReservations(ctx, brokerDef.ID, agent.RuntimeBrokerID, store.QuotaScopeBroker, agent.RuntimeBrokerID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeBroker, "broker quota must still be released after GetAgent panic in autoSuspendAgent")

		activeProj, err := s.CountActiveReservations(ctx, projDef.ID, agent.ProjectID, store.QuotaScopeProject, agent.ProjectID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeProj, "project quota must still be released after GetAgent panic in autoSuspendAgent")

		assert.Contains(t, pub.publishedPhases(agent.ID), string(state.PhaseSuspended), "PublishAgentStatus must still run after GetAgent panic in autoSuspendAgent")
		assertHasWarnLog(t, logs.String(), "Recovered panic refreshing agent after stop", agent.ID, "injected get agent panic")
	})

	t.Run("cancelled_context_and_timeout_bounds_slow_refresh", func(t *testing.T) {
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		ctx := context.Background()
		agent := setupBrokerAgentInPhase(t, s, "sync-cancel-timeout", state.PhaseSuspended)
		agent.Activity = string(state.ActivityWaitingForInput)
		require.NoError(t, s.UpdateAgent(ctx, agent))

		// Cancelled context must still refresh from the store.
		localCopy := *agent
		localCopy.Activity = ""
		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()
		srv.syncAgentAfterStopRecord(cancelledCtx, &localCopy, string(state.PhaseSuspended), string(state.SuspendReasonUser))
		assert.Equal(t, string(state.ActivityWaitingForInput), localCopy.Activity, "detached syncAgentAfterStopRecord must still refresh when caller context is cancelled")

		// Slow store read must be bounded by finalizationContext deadline.
		wrapped.blockOnGetAgent.Store(true)
		fault.Arm()
		slowCopy := *agent
		synctest.Test(t, func(t *testing.T) {
			srv.syncAgentAfterStopRecord(context.Background(), &slowCopy, string(state.PhaseSuspended), string(state.SuspendReasonUser))
			synctest.Wait()
		})
		assert.True(t, wrapped.getAgentCalled.Load())
		assert.True(t, wrapped.sawGetAgentDeadline.Load(), "syncAgentAfterStopRecord must run with a bounded deadline")
	})
}

func TestNotifyThirdPartyLifecycleAction_Hardening(t *testing.T) {
	t.Run("suspend_sync_panic_recovers_publishes_status_and_returns_200", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		bus := NewChannelEventPublisher()
		t.Cleanup(bus.Close)
		pub := newStatusRecordingPublisher(bus)
		srv.events = pub
		srv.SetDispatcher(&engineStubDispatcher{})
		srv.quotaService = &QuotaService{store: s, logger: slog.Default()}
		srv.notificationDispatcher = NewNotificationDispatcher(wrapped, pub, srv.GetDispatcher, slog.Default())

		ctx := context.Background()
		creatorID := tid("tp-panic-creator")
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID:          creatorID,
			Email:       creatorID + "@example.com",
			DisplayName: "Creator User",
			Role:        store.UserRoleMember,
			Status:      store.UserStatusActive,
		}))

		seed := setupBrokerAgentInPhase(t, s, "tp-panic-seed", state.PhaseRunning)
		agent := &store.Agent{
			ID:              tid("tp-panic-susp"),
			Slug:            "tp-panic-susp",
			Name:            "tp-panic-susp",
			ProjectID:       seed.ProjectID,
			RuntimeBrokerID: seed.RuntimeBrokerID,
			Phase:           string(state.PhaseRunning),
			Template:        "claude",
			CreatedBy:       creatorID,
		}
		require.NoError(t, s.CreateAgent(ctx, agent))
		_, err := s.ClaimAgentStart(ctx, agent.ID, "hub-tp-panic", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)

		projDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, agent.ProjectID, agent.ProjectID, agent.ID)
		brokerDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.RuntimeBrokerID, agent.ID)

		wrapped.armGetUserPanicPostCred.Store(true)
		fault.Arm()

		// Dev auth acts as a third-party admin (ID != creatorID).
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		assert.True(t, wrapped.getUserCalled.Load(), "synchronous GetUser in notifyThirdPartyLifecycleAction must have been attempted")
		after := mustGetAgent(t, s, agent.ID)
		assert.Equal(t, string(state.PhaseSuspended), after.Phase)
		assert.Empty(t, after.StartClaimID, "start claim must still be released")

		activeBroker, err := s.CountActiveReservations(ctx, brokerDef.ID, agent.RuntimeBrokerID, store.QuotaScopeBroker, agent.RuntimeBrokerID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeBroker)
		activeProj, err := s.CountActiveReservations(ctx, projDef.ID, agent.ProjectID, store.QuotaScopeProject, agent.ProjectID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeProj)

		assert.Contains(t, pub.publishedPhases(agent.ID), string(state.PhaseSuspended), "PublishAgentStatus must still run after sync notify panic")
		assertHasWarnLog(t, logs.String(), "Recovered panic preparing third-party lifecycle notification", agent.ID, "injected get user panic")
	})

	t.Run("cancelled_context_still_persists_notification", func(t *testing.T) {
		srv, s := testServer(t)
		bus := NewChannelEventPublisher()
		t.Cleanup(bus.Close)
		srv.events = bus
		srv.notificationDispatcher = NewNotificationDispatcher(s, bus, srv.GetDispatcher, slog.Default())

		ctx := context.Background()
		creatorID := tid("tp-cancel-creator")
		actorID := tid("tp-cancel-actor")
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID:          creatorID,
			Email:       creatorID + "@example.com",
			DisplayName: "Creator",
			Role:        store.UserRoleMember,
			Status:      store.UserStatusActive,
		}))
		agent := setupBrokerAgentInPhase(t, s, "tp-cancel-susp", state.PhaseSuspended)
		agent.CreatedBy = creatorID
		require.NoError(t, s.UpdateAgent(ctx, agent))

		cancelledCtx, cancel := context.WithCancel(
			contextWithIdentity(ctx, NewAuthenticatedUser(actorID, "actor@example.com", "Actor", store.UserRoleAdmin, "web")),
		)
		cancel()

		srv.notifyThirdPartyLifecycleAction(cancelledCtx, agent, api.AgentActionSuspend, "maintenance")
		srv.thirdPartyStopDispatches.Wait()

		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		require.Len(t, notifs, 1, "synchronous third-party notification persistence must detach from cancelled caller context")
	})

	t.Run("timeout_bounds_slow_sync_lookup", func(t *testing.T) {
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		bus := NewChannelEventPublisher()
		t.Cleanup(bus.Close)
		srv.events = bus
		srv.notificationDispatcher = NewNotificationDispatcher(wrapped, bus, srv.GetDispatcher, slog.Default())

		ctx := context.Background()
		agent := setupBrokerAgentInPhase(t, s, "tp-timeout-susp", state.PhaseSuspended)
		agent.CreatedBy = tid("tp-timeout-creator")
		require.NoError(t, s.UpdateAgent(ctx, agent))

		actorCtx := contextWithIdentity(ctx, NewAuthenticatedUser(tid("tp-timeout-actor"), "actor@example.com", "Actor", store.UserRoleAdmin, "web"))
		wrapped.blockOnGetUser.Store(true)
		fault.Arm()

		synctest.Test(t, func(t *testing.T) {
			srv.notifyThirdPartyLifecycleAction(actorCtx, agent, api.AgentActionSuspend, "slow")
			synctest.Wait()
		})

		assert.True(t, wrapped.getUserCalled.Load())
		assert.True(t, wrapped.sawGetUserDeadline.Load(), "synchronous third-party notification lookup must run with a bounded deadline")
	})
}

func TestClearIdleSuspendClaim_Hardening(t *testing.T) {
	t.Run("panic_isolated_per_agent_publishes_and_continues_batch", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		bus := NewChannelEventPublisher()
		t.Cleanup(bus.Close)
		pub := newStatusRecordingPublisher(bus)
		srv.events = pub
		srv.SetDispatcher(&engineStubDispatcher{})

		// Enable IdleSuspend experiment and settings so autoSuspendAgent accepts SuspendReasonIdle.
		var activeExps []experiments.Experiment
		for _, e := range experiments.Default().All() {
			if e.Name == experiments.IdleSuspend {
				e.Default = true
			}
			activeExps = append(activeExps, e)
		}
		reg, err := experiments.NewRegistry(activeExps, nil)
		require.NoError(t, err)
		srv.experiments = reg
		srv.mu.Lock()
		srv.config.AutoSuspendIdle = true
		srv.config.IdleSuspendAfter = DefaultIdleSuspendAfter
		srv.mu.Unlock()

		ctx := context.Background()
		idleSince := time.Now().UTC().Add(-(DefaultIdleSuspendAfter + 10*time.Minute))
		freshSeen := time.Now().UTC().Add(-10 * time.Second)
		db := s.(*entadapter.CompositeStore).DB()

		agent1 := setupBrokerAgentInPhase(t, s, "idle-clear-panic-1", state.PhaseRunning)
		_, err = db.ExecContext(ctx,
			"UPDATE agents SET activity = ?, activity_since = ?, last_seen = ? WHERE id = ?",
			string(state.ActivityIdle), idleSince, freshSeen, agent1.ID,
		)
		require.NoError(t, err)
		agent1 = mustGetAgent(t, s, agent1.ID)

		agent2 := setupBrokerAgentInPhase(t, s, "idle-clear-panic-2", state.PhaseRunning)
		_, err = db.ExecContext(ctx,
			"UPDATE agents SET activity = ?, activity_since = ?, last_seen = ? WHERE id = ?",
			string(state.ActivityIdle), idleSince, freshSeen, agent2.ID,
		)
		require.NoError(t, err)
		agent2 = mustGetAgent(t, s, agent2.ID)

		wrapped.panicClearIdleFor.Store(agent1.ID)
		fault.Arm()

		require.NotPanics(t, func() {
			srv.autoSuspendAgents(ctx, []autoSuspendItem{
				{agent: *agent1, reason: state.SuspendReasonIdle},
				{agent: *agent2, reason: state.SuspendReasonIdle},
			})
		})

		assert.True(t, wrapped.clearIdleClaimCalled.Load())
		after1 := mustGetAgent(t, s, agent1.ID)
		after2 := mustGetAgent(t, s, agent2.ID)
		assert.Equal(t, string(state.PhaseSuspended), after1.Phase)
		assert.Equal(t, string(state.PhaseSuspended), after2.Phase, "second agent in batch must still be auto-suspended after ClearIdleSuspendClaim panic on first agent")
		assert.Nil(t, after2.IdleSuspendClaimUntil, "second agent idle claim must be cleared")

		assert.Contains(t, pub.publishedPhases(agent1.ID), string(state.PhaseSuspended), "PublishAgentStatus must still run for agent1 after ClearIdleSuspendClaim panic")
		assert.Contains(t, pub.publishedPhases(agent2.ID), string(state.PhaseSuspended), "PublishAgentStatus must run for agent2")
		assertHasWarnLog(t, logs.String(), "Recovered panic clearing idle-suspend claim", agent1.ID, "injected clear idle suspend claim panic")
	})

	t.Run("non_panic_store_error_logged_at_warn", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		srv.agentLifecycleLog = slog.Default()

		agent := setupBrokerAgentInPhase(t, s, "idle-clear-err", state.PhaseRunning)
		wrapped.errClearIdleFor.Store(agent.ID)
		fault.Arm()

		srv.clearIdleSuspendClaimBestEffort(context.Background(), agent.ID)
		assert.True(t, wrapped.clearIdleClaimCalled.Load())
		assertHasWarnLog(t, logs.String(), "Best-effort cleanup step failed", agent.ID, "injected clear idle suspend claim error")
	})
}

func TestExpireHeldMessagesForAgent_Hardening(t *testing.T) {
	t.Run("panic_recovers_and_continues_suspend", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		bus := NewChannelEventPublisher()
		t.Cleanup(bus.Close)
		pub := newStatusRecordingPublisher(bus)
		srv.events = pub
		srv.SetDispatcher(&engineStubDispatcher{})
		srv.quotaService = &QuotaService{store: s, logger: slog.Default()}

		ctx := context.Background()
		agent := setupBrokerAgentInPhase(t, s, "expire-hold-panic", state.PhaseRunning)
		_, err := s.ClaimAgentStart(ctx, agent.ID, "hub-expire-hold", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)

		projDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, agent.ProjectID, agent.ProjectID, agent.ID)
		brokerDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.RuntimeBrokerID, agent.ID)

		wrapped.panicOnHasDeliveryHold.Store(true)
		fault.Arm()

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		assert.True(t, wrapped.hasDeliveryHoldCalled.Load())
		after := mustGetAgent(t, s, agent.ID)
		assert.Equal(t, string(state.PhaseSuspended), after.Phase)
		assert.Empty(t, after.StartClaimID, "start claim must still be released after expireHeldMessagesForAgent panic")

		activeBroker, err := s.CountActiveReservations(ctx, brokerDef.ID, agent.RuntimeBrokerID, store.QuotaScopeBroker, agent.RuntimeBrokerID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeBroker)
		activeProj, err := s.CountActiveReservations(ctx, projDef.ID, agent.ProjectID, store.QuotaScopeProject, agent.ProjectID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeProj)

		assert.Contains(t, pub.publishedPhases(agent.ID), string(state.PhaseSuspended))
		assertHasWarnLog(t, logs.String(), "Recovered panic expiring held messages for agent", agent.ID, "injected has delivery hold panic")
	})
}

func TestStopAllSettleLifecycleWrite_Hardening(t *testing.T) {
	t.Run("settle_panic_still_releases_quotas_and_claim", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		srv.agentLifecycleLog = slog.Default()
		bus := NewChannelEventPublisher()
		t.Cleanup(bus.Close)
		pub := newStatusRecordingPublisher(bus)
		srv.events = pub
		srv.SetDispatcher(&engineStubDispatcher{})
		srv.quotaService = &QuotaService{store: s, logger: slog.Default()}

		ctx := context.Background()
		agent := setupBrokerAgentInPhase(t, s, "stopall-settle-panic", state.PhaseRunning)
		_, err := s.ClaimAgentStart(ctx, agent.ID, "hub-stopall-settle", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)

		projDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, agent.ProjectID, agent.ProjectID, agent.ID)
		brokerDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.RuntimeBrokerID, agent.ID)

		wrapped.armGetAgentPanicPostStatusUpdate.Store(true)
		fault.Arm()

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+agent.ProjectID+"/agents/stop-all", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp StopAllAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, 1, resp.Stopped, "stop-all must report agent stopped even when settleLifecycleWrite panics")
		assert.Equal(t, 0, resp.Failed)

		after := mustGetAgent(t, s, agent.ID)
		assert.Equal(t, string(state.PhaseStopped), after.Phase)
		assert.Empty(t, after.StartClaimID, "start claim must still be released after settleLifecycleWrite panic")

		activeBroker, err := s.CountActiveReservations(ctx, brokerDef.ID, agent.RuntimeBrokerID, store.QuotaScopeBroker, agent.RuntimeBrokerID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeBroker, "broker quota must be released before/despite settleLifecycleWrite panic")

		activeProj, err := s.CountActiveReservations(ctx, projDef.ID, agent.ProjectID, store.QuotaScopeProject, agent.ProjectID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeProj, "project quota must be released before/despite settleLifecycleWrite panic")

		assert.Contains(t, pub.publishedPhases(agent.ID), string(state.PhaseStopped), "PublishAgentStatus must still run after settleLifecycleWrite panic")
		assertHasWarnLog(t, logs.String(), "Recovered panic settling lifecycle write", agent.ID, "injected get agent panic")
	})

	t.Run("goroutine_panic_recovers_without_crashing_hub", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		srv.agentLifecycleLog = slog.Default()
		srv.SetDispatcher(&engineStubDispatcher{})

		ctx := context.Background()
		agent1 := setupBrokerAgentInPhase(t, s, "stopall-goroutine-panic-1", state.PhaseRunning)
		agent2 := &store.Agent{
			ID:              tid("stopall-goroutine-panic-2"),
			Slug:            "stopall-goroutine-panic-2",
			Name:            "stopall-goroutine-panic-2",
			ProjectID:       agent1.ProjectID,
			RuntimeBrokerID: agent1.RuntimeBrokerID,
			Phase:           string(state.PhaseRunning),
			ContainerStatus: "running",
			RunID:           tid("run-2"),
			Created:         time.Now(),
			Updated:         time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, agent2))

		wrapped.panicOnUpdateStatusFor.Store(agent1.ID)
		fault.Arm()

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+agent1.ProjectID+"/agents/stop-all", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp StopAllAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, 1, resp.Stopped, "second agent must still stop when first agent's goroutine panics")
		assert.Equal(t, 1, resp.Failed, "panicking agent must be recorded as failed")

		after2 := mustGetAgent(t, s, agent2.ID)
		assert.Equal(t, string(state.PhaseStopped), after2.Phase)
		assert.Contains(t, logs.String(), "stop-all: recovered panic stopping agent")
		assert.NotContains(t, rec.Body.String(), "injected update agent status panic", "raw panic details must not leak in HTTP response")
	})

	t.Run("settle_cancelled_context_and_timeout_bounds_slow_reload", func(t *testing.T) {
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		srv.agentLifecycleLog = slog.Default()
		agent := setupBrokerAgentInPhase(t, s, "settle-ctx-test", state.PhaseStopped)

		// Cancelled caller context still reloads via detached context.
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		err := srv.settleLifecycleWrite(cancelled, agent, string(state.PhaseStopped))
		assert.NoError(t, err, "settleLifecycleWrite must detach from cancelled caller context")

		// Slow GetAgent is bounded by finalizationTimeout.
		synctest.Test(t, func(t *testing.T) {
			wrapped.blockOnGetAgent.Store(true)
			wrapped.sawGetAgentDeadline.Store(false)
			fault.Arm()

			start := time.Now()
			err := srv.settleLifecycleWrite(context.Background(), agent, string(state.PhaseStopped))
			elapsed := time.Since(start)

			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.True(t, wrapped.sawGetAgentDeadline.Load(), "settleLifecycleWrite must set a bounded deadline")
			assert.GreaterOrEqual(t, elapsed, finalizationTimeout)
		})
	})
}

func TestRecordStopStatusErrRunChanged_Hardening(t *testing.T) {
	t.Run("nil_logger_and_get_agent_panic_returns_false_nil", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		srv.agentLifecycleLog = nil
		agent := setupBrokerAgentInPhase(t, s, "run-changed-panic", state.PhaseRunning)

		wrapped.returnErrRunChanged.Store(true)
		wrapped.armGetAgentPanicOnRunChanged.Store(true)
		fault.Arm()

		var recorded bool
		var err error
		require.NotPanics(t, func() {
			recorded, err = srv.recordStopStatus(context.Background(), agent.ID, agent.RunID, "stop", store.AgentStatusUpdate{
				Phase: string(state.PhaseStopped),
			})
		})
		assert.False(t, recorded)
		assert.NoError(t, err)
		assert.True(t, wrapped.getAgentCalled.Load())
		assertHasWarnLog(t, logs.String(), "Recovered panic refreshing agent after stop", agent.ID, "injected get agent panic")
	})

	t.Run("queued_stop_err_run_changed_panic_still_releases_claim", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		srv.agentLifecycleLog = nil
		srv.SetDispatcher(&engineStubDispatcher{})

		ctx := context.Background()
		agent := setupBrokerAgentInPhase(t, s, "queued-stop-run-changed-panic", state.PhaseStopped)
		require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
			ContainerStatus: containerStatusStopQueued,
			Message:         offlineStopMessage,
		}))
		claim, err := s.ClaimAgentStart(ctx, agent.ID, "hub-queued-stop", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
		intentAt, err := srv.recordRunIntent(ctx, agent, store.RunIntentStopped)
		require.NoError(t, err)

		args, err := MarshalDispatchArgs(StopDispatchArgs{
			IntentAt:        &intentAt,
			SupersedesClaim: claim.ID,
			RunID:           agent.RunID,
		})
		require.NoError(t, err)

		wrapped.returnErrRunChanged.Store(true)
		wrapped.armGetAgentPanicOnRunChanged.Store(true)
		fault.Arm()

		require.NotPanics(t, func() {
			_, err = srv.execDispatchStop(ctx, store.BrokerDispatch{
				ID:        tid("dispatch-run-changed-panic"),
				BrokerID:  agent.RuntimeBrokerID,
				AgentID:   agent.ID,
				AgentSlug: agent.Slug,
				ProjectID: agent.ProjectID,
				Op:        "stop",
				Args:      args,
			})
		})
		require.NoError(t, err)

		after := mustGetAgent(t, s, agent.ID)
		assert.Empty(t, after.StartClaimID, "releaseSupersededClaim must still run after ErrRunChanged GetAgent panic in execDispatchStop")
		assertHasWarnLog(t, logs.String(), "Recovered panic refreshing agent after stop", agent.ID, "injected get agent panic")
	})
}

func TestRunBestEffortCleanupStep(t *testing.T) {
	t.Run("nil_ctx_and_logger_panic_recovers_with_step_attr", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		var sawDeadline bool
		//nolint:staticcheck // intentionally testing nil context guard
		err := runBestEffortCleanupStep(nil, 0, nil, "test_step", "", []any{"agent_id", "ag-1"}, func(stepCtx context.Context) error {
			_, sawDeadline = stepCtx.Deadline()
			panic("boom")
		})
		assert.ErrorIs(t, err, errCleanupStepPanicked)
		assert.True(t, sawDeadline, "default timeout must set a deadline")
		assertHasWarnLog(t, logs.String(), "Recovered panic in best-effort cleanup step", "ag-1", `"step":"test_step"`)
	})

	t.Run("cancelled_parent_detaches_and_bounds_timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := captureSuspendHardeningLogs(t)
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()

			start := time.Now()
			err := runBestEffortCleanupStep(cancelled, 0, nil, "slow_step", "", []any{"agent_id", "ag-2"}, func(stepCtx context.Context) error {
				require.NoError(t, stepCtx.Err(), "detached context must not inherit parent cancellation")
				<-stepCtx.Done()
				return stepCtx.Err()
			})
			elapsed := time.Since(start)
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.GreaterOrEqual(t, elapsed, finalizationTimeout)
			assertHasWarnLog(t, logs.String(), "Best-effort cleanup step failed", "ag-2", `"step":"slow_step"`)
		})
	})
}

func TestPersistSuspendScreen_Hardening(t *testing.T) {
	enableExperiments := func(t *testing.T, srv *Server, names ...string) {
		t.Helper()
		want := make(map[string]bool, len(names))
		for _, n := range names {
			want[n] = true
		}
		var activeExps []experiments.Experiment
		for _, e := range experiments.Default().All() {
			if want[e.Name] {
				e.Default = true
			}
			activeExps = append(activeExps, e)
		}
		reg, err := experiments.NewRegistry(activeExps, nil)
		require.NoError(t, err)
		srv.experiments = reg
	}

	t.Run("manual_suspend_panic_still_releases_quotas_and_claim", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		srv.agentLifecycleLog = slog.Default()
		enableExperiments(t, srv, experiments.SuspendScreen)
		bus := NewChannelEventPublisher()
		t.Cleanup(bus.Close)
		pub := newStatusRecordingPublisher(bus)
		srv.events = pub
		srv.SetDispatcher(&engineStubDispatcher{})
		srv.quotaService = &QuotaService{store: s, logger: slog.Default()}

		ctx := context.Background()
		agent := setupBrokerAgentInPhase(t, s, "suspend-screen-panic", state.PhaseRunning)
		_, err := s.ClaimAgentStart(ctx, agent.ID, "hub-screen-panic", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)

		projDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, agent.ProjectID, agent.ProjectID, agent.ID)
		brokerDef := seedActiveQuotaReservation(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.RuntimeBrokerID, agent.ID)

		wrapped.panicOnDeleteSuspendScreen.Store(agent.ID)
		fault.Arm()

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		assert.True(t, wrapped.deleteSuspendScreenCalled.Load())
		after := mustGetAgent(t, s, agent.ID)
		assert.Equal(t, string(state.PhaseSuspended), after.Phase)
		assert.Empty(t, after.StartClaimID, "start claim must still be released after persistSuspendScreen panic")

		activeBroker, err := s.CountActiveReservations(ctx, brokerDef.ID, agent.RuntimeBrokerID, store.QuotaScopeBroker, agent.RuntimeBrokerID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeBroker, "broker quota must still be released after persistSuspendScreen panic")
		activeProj, err := s.CountActiveReservations(ctx, projDef.ID, agent.ProjectID, store.QuotaScopeProject, agent.ProjectID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), activeProj, "project quota must still be released after persistSuspendScreen panic")

		assert.Contains(t, pub.publishedPhases(agent.ID), string(state.PhaseSuspended), "PublishAgentStatus must still run after persistSuspendScreen panic")
		assertHasWarnLog(t, logs.String(), "Recovered panic persisting pre-suspend terminal screen", agent.ID, "injected delete suspend screen panic")
	})

	t.Run("put_panic_detaches_cancelled_ctx_and_bounds_timeout", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		srv.agentLifecycleLog = slog.Default()
		enableExperiments(t, srv, experiments.SuspendScreen)

		agent := setupBrokerAgentInPhase(t, s, "suspend-screen-put", state.PhaseSuspended)
		cap := &capturedScreen{
			Content:    "sensitive-terminal-output-do-not-log",
			CapturedAt: time.Now().UTC().Truncate(time.Microsecond),
		}

		// Panic on PutAgentSuspendScreen recovers without logging screen content.
		wrapped.panicOnPutSuspendScreen.Store(true)
		fault.Arm()
		require.NotPanics(t, func() {
			srv.persistSuspendScreen(context.Background(), agent.ID, cap)
		})
		assert.True(t, wrapped.putSuspendScreenCalled.Load())
		assertHasWarnLog(t, logs.String(), "Recovered panic persisting pre-suspend terminal screen", agent.ID, "injected put suspend screen panic")
		assert.NotContains(t, logs.String(), cap.Content, "screen content must never be logged")

		// Cancelled caller context still detaches and slow Put is bounded by suspendScreenWriteTimeout.
		wrapped.panicOnPutSuspendScreen.Store(false)
		synctest.Test(t, func(t *testing.T) {
			wrapped.blockOnPutSuspendScreen.Store(true)
			wrapped.sawPutSuspendScreenDeadline.Store(false)

			cancelled, cancel := context.WithCancel(context.Background())
			cancel()

			start := time.Now()
			srv.persistSuspendScreen(cancelled, agent.ID, cap)
			elapsed := time.Since(start)

			assert.True(t, wrapped.sawPutSuspendScreenDeadline.Load(), "persistSuspendScreen must set a bounded deadline")
			assert.GreaterOrEqual(t, elapsed, suspendScreenWriteTimeout)
		})
	})

	t.Run("auto_suspend_panic_still_clears_claim_and_continues_batch", func(t *testing.T) {
		logs := captureSuspendHardeningLogs(t)
		srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendPathHardeningStore {
			return &suspendPathHardeningStore{Store: inner, fault: f}
		})
		srv.agentLifecycleLog = slog.Default()
		enableExperiments(t, srv, experiments.IdleSuspend, experiments.SuspendScreen)
		bus := NewChannelEventPublisher()
		t.Cleanup(bus.Close)
		pub := newStatusRecordingPublisher(bus)
		srv.events = pub
		srv.SetDispatcher(&engineStubDispatcher{})

		srv.mu.Lock()
		srv.config.AutoSuspendIdle = true
		srv.config.IdleSuspendAfter = DefaultIdleSuspendAfter
		srv.mu.Unlock()

		ctx := context.Background()
		idleSince := time.Now().UTC().Add(-(DefaultIdleSuspendAfter + 10*time.Minute))
		freshSeen := time.Now().UTC().Add(-10 * time.Second)
		db := s.(*entadapter.CompositeStore).DB()

		agent1 := setupBrokerAgentInPhase(t, s, "idle-screen-panic-1", state.PhaseRunning)
		_, err := db.ExecContext(ctx,
			"UPDATE agents SET activity = ?, activity_since = ?, last_seen = ? WHERE id = ?",
			string(state.ActivityIdle), idleSince, freshSeen, agent1.ID,
		)
		require.NoError(t, err)
		agent1 = mustGetAgent(t, s, agent1.ID)

		agent2 := setupBrokerAgentInPhase(t, s, "idle-screen-panic-2", state.PhaseRunning)
		_, err = db.ExecContext(ctx,
			"UPDATE agents SET activity = ?, activity_since = ?, last_seen = ? WHERE id = ?",
			string(state.ActivityIdle), idleSince, freshSeen, agent2.ID,
		)
		require.NoError(t, err)
		agent2 = mustGetAgent(t, s, agent2.ID)

		wrapped.panicOnDeleteSuspendScreen.Store(agent1.ID)
		fault.Arm()

		require.NotPanics(t, func() {
			srv.autoSuspendAgents(ctx, []autoSuspendItem{
				{agent: *agent1, reason: state.SuspendReasonIdle},
				{agent: *agent2, reason: state.SuspendReasonIdle},
			})
		})

		assert.True(t, wrapped.deleteSuspendScreenCalled.Load())
		after1 := mustGetAgent(t, s, agent1.ID)
		after2 := mustGetAgent(t, s, agent2.ID)
		assert.Equal(t, string(state.PhaseSuspended), after1.Phase)
		assert.Nil(t, after1.IdleSuspendClaimUntil, "agent1 idle claim must still be cleared after persistSuspendScreen panic")
		assert.Equal(t, string(state.PhaseSuspended), after2.Phase, "agent2 in batch must still be auto-suspended")
		assert.Nil(t, after2.IdleSuspendClaimUntil, "agent2 idle claim must be cleared")

		assert.Contains(t, pub.publishedPhases(agent1.ID), string(state.PhaseSuspended))
		assert.Contains(t, pub.publishedPhases(agent2.ID), string(state.PhaseSuspended))
		assertHasWarnLog(t, logs.String(), "Recovered panic persisting pre-suspend terminal screen", agent1.ID, "injected delete suspend screen panic")
	})
}
