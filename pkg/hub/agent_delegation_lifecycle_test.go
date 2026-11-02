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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Agent delegation grant lifecycle (.design/agent-delegation.md §16): the
// four lifecycle hooks, the best-effort cascades and the project delete
// step, through the real lifecycle paths.

// lifecycleGrant is a grant written directly for an agent, with one live
// credential exchanged from it.
type lifecycleGrant struct {
	grant *store.AgentDelegationGrant
	cred  *store.AgentDelegatedCredential
}

// seedLifecycleGrant writes an active hub grant for agent, issued by
// issuerID, and one unexpired credential exchanged from it.
func seedLifecycleGrant(t *testing.T, s store.Store, agent *store.Agent, issuerID string) lifecycleGrant {
	t.Helper()
	ctx := context.Background()
	g := &store.AgentDelegationGrant{
		AgentID: agent.ID, AgentProjectID: agent.ProjectID, AgentGeneration: agent.Generation,
		AgentStateVersion: agent.StateVersion, IssuerUserID: issuerID, BoundaryKind: "hub",
		CeilingVersion: 1, CeilingPermissionIDs: []string{"agent.read"}, Name: "lifecycle",
		MaxCredentialTTLSeconds: agentDelegationCredentialDefaultTTL, ExpiresAt: time.Now().Add(time.Hour),
		IssuanceAuditID: uuid.NewString(),
	}
	require.NoError(t, s.CreateAgentDelegationGrant(ctx, g))
	c := &store.AgentDelegatedCredential{
		GrantID: g.ID, AgentID: agent.ID, KeyHash: "lifecycle-" + uuid.NewString(), Prefix: delegatedCredentialPrefix,
		Audience: "scion-hub", CeilingPermissionIDs: []string{"agent.read"}, ExchangeAgentCredentialID: uuid.NewString(),
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	require.NoError(t, s.CreateAgentDelegatedCredential(ctx, c))
	return lifecycleGrant{grant: g, cred: c}
}

// assertGrantRevoked asserts that lg's grant and credential are revoked
// with reason, and that exactly one agent_delegation_grant_revoke record,
// the grant's revocation_audit_id, names the agent, the issuer, the grant
// and the credential revoked with it.
func assertGrantRevoked(t *testing.T, s store.Store, lg lifecycleGrant, reason string) {
	t.Helper()
	ctx := context.Background()
	g, err := s.GetAgentDelegationGrant(ctx, lg.grant.ID)
	require.NoError(t, err, "the grant row survives the agent")
	require.NotNil(t, g.RevokedAt, "the grant is revoked")
	assert.Equal(t, reason, g.RevokeReason)
	assert.NotEmpty(t, g.RevokedBy)
	c, err := s.GetAgentDelegatedCredential(ctx, lg.cred.ID)
	require.NoError(t, err)
	require.NotNil(t, c.RevokedAt, "the credential is revoked with the grant")
	assert.Equal(t, reason, c.RevokeReason)

	recs, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutationAgentDelegationGrantRevoke, TargetID: g.ID})
	require.NoError(t, err)
	require.Len(t, recs, 1, "one revocation record")
	rec := recs[0]
	assert.Equal(t, g.RevocationAuditID, rec.ID, "the grant records its revocation audit")
	assert.Equal(t, auditTargetAgentDelegationGrant, rec.TargetType)
	assert.Equal(t, g.AgentID, rec.ActorAgentID)
	assert.Equal(t, g.IssuerUserID, rec.AuthorizingUserID)
	assert.Equal(t, g.ID, rec.SourceGrantID)
	assert.NotEmpty(t, rec.ActorPrincipalKind, "the record names who caused the revocation")
	var before struct {
		GrantID       string   `json:"grant_id"`
		Reason        string   `json:"revoke_reason"`
		CredentialIDs []string `json:"revoked_credential_ids"`
	}
	require.NoError(t, json.Unmarshal([]byte(rec.BeforeSummary), &before))
	assert.Equal(t, g.ID, before.GrantID)
	assert.Equal(t, reason, before.Reason)
	assert.Equal(t, []string{lg.cred.ID}, before.CredentialIDs)
}

// assertGrantActive asserts that lg's grant and credential are unrevoked
// and that no revocation record exists for the grant.
func assertGrantActive(t *testing.T, s store.Store, lg lifecycleGrant) {
	t.Helper()
	ctx := context.Background()
	g, err := s.GetAgentDelegationGrant(ctx, lg.grant.ID)
	require.NoError(t, err)
	assert.Nil(t, g.RevokedAt, "the grant is still active")
	c, err := s.GetAgentDelegatedCredential(ctx, lg.cred.ID)
	require.NoError(t, err)
	assert.Nil(t, c.RevokedAt, "the credential is still active")
	recs, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutationAgentDelegationGrantRevoke, TargetID: g.ID})
	require.NoError(t, err)
	assert.Empty(t, recs, "no revocation record")
}

// errInjectedGrantRevoke and errInjectedGrantRevokeAudit are returned by
// adtLifecycleFaultStore while armed.
var (
	errInjectedGrantRevoke      = errors.New("injected grant revocation fault")
	errInjectedGrantRevokeAudit = errors.New("injected grant revocation audit fault")
)

// adtLifecycleFaultStore fails, on a transaction-scoped store, every
// RevokeAgentDelegationGrant while armed, and every
// agent_delegation_grant_revoke mutation audit write while auditArmed:
// inside WithTx and inside the delete engine's finalize transaction, the
// two places lifecycle hooks run. Other audit records and every call
// outside a transaction are untouched.
type adtLifecycleFaultStore struct {
	store.Store
	armed      atomic.Bool
	auditArmed atomic.Bool
}

func (s *adtLifecycleFaultStore) wrap(tx store.Store) store.Store {
	return &adtLifecycleFaultTx{Store: tx, parent: s}
}

func (s *adtLifecycleFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error { return fn(s.wrap(tx)) })
}

func (s *adtLifecycleFaultStore) FinalizeAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, mode store.DeletionFinalizeMode, set store.DeletionFields, hook store.DeletionFinalizeHook) (int, error) {
	return s.Store.FinalizeAgentDeletion(ctx, id, pred, mode, set, func(ctx context.Context, tx store.Store, a *store.Agent, m store.DeletionFinalizeMode) error {
		return hook(ctx, s.wrap(tx), a, m)
	})
}

type adtLifecycleFaultTx struct {
	store.Store
	parent *adtLifecycleFaultStore
}

func (t *adtLifecycleFaultTx) RevokeAgentDelegationGrant(ctx context.Context, id, revokedBy, reason, auditID string, at time.Time) (bool, error) {
	if t.parent.armed.Load() {
		return false, errInjectedGrantRevoke
	}
	return t.Store.RevokeAgentDelegationGrant(ctx, id, revokedBy, reason, auditID, at)
}

func (t *adtLifecycleFaultTx) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if t.parent.auditArmed.Load() && r.MutationType == mutationAgentDelegationGrantRevoke {
		return errInjectedGrantRevokeAudit
	}
	return t.Store.CreateMutationAudit(ctx, r)
}

// installLifecycleFault wraps srv.store in an adtLifecycleFaultStore whose
// revocation fault is armed.
func installLifecycleFault(t *testing.T, srv *Server) *adtLifecycleFaultStore {
	t.Helper()
	fault := installLifecycleFaultStore(t, srv)
	fault.armed.Store(true)
	return fault
}

// installLifecycleAuditFault wraps srv.store in an adtLifecycleFaultStore
// whose revocation audit fault is armed.
func installLifecycleAuditFault(t *testing.T, srv *Server) *adtLifecycleFaultStore {
	t.Helper()
	fault := installLifecycleFaultStore(t, srv)
	fault.auditArmed.Store(true)
	return fault
}

func installLifecycleFaultStore(t *testing.T, srv *Server) *adtLifecycleFaultStore {
	t.Helper()
	fault, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *adtLifecycleFaultStore {
		return &adtLifecycleFaultStore{Store: inner}
	})
	return fault
}

// lifecycleIssuer creates a hub member who issues the seeded grants.
func lifecycleIssuer(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	return hubMemberUser(t, s, name+"-issuer")
}

func TestAgentDelegationLifecycle_HookRegisteredInEveryRegistry(t *testing.T) {
	srv, _ := testServer(t)
	for name, list := range map[string]*[]namedAgentTxHook{
		"soft delete":       &srv.lifecycleTxHooks.softDelete,
		"hard delete":       &srv.lifecycleTxHooks.hardDelete,
		"restore":           &srv.lifecycleTxHooks.restore,
		"reincarnate claim": &srv.lifecycleTxHooks.reincarnate,
	} {
		found := 0
		for _, h := range srv.lifecycleTxHooks.snapshot(list) {
			if h.name == agentDelegationLifecycleHookName {
				found++
			}
		}
		assert.Equal(t, 1, found, "%s registry holds the grant revocation hook once", name)
	}
}

// A soft delete revokes the agent's active grants and their credentials in
// its own transaction (reason agent_deleted), leaves a grant that was
// already revoked as it was, and leaves another agent's grant alone.
func TestAgentDelegationLifecycle_SoftDeleteRevokesGrants(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	ctx := context.Background()
	agent := setupBrokerAgentInPhase(t, s, "adtlc-soft", state.PhaseStopped)
	other := setupBrokerAgentInPhase(t, s, "adtlc-soft-other", state.PhaseStopped)
	issuer := lifecycleIssuer(t, s, "adtlc-soft")
	active := seedLifecycleGrant(t, s, agent, issuer.ID)
	earlier := seedLifecycleGrant(t, s, agent, issuer.ID)
	_, err := s.RevokeAgentDelegationGrant(ctx, earlier.grant.ID, "user:"+issuer.ID, revokeReasonRevoked, "", time.Now())
	require.NoError(t, err)
	bystander := seedLifecycleGrant(t, s, other, issuer.ID)

	softDeleteForTest(t, srv, agent.ID)

	assert.False(t, mustGetAgent(t, s, agent.ID).DeletedAt.IsZero())
	assertGrantRevoked(t, s, active, revokeReasonAgentDeleted)
	g, err := s.GetAgentDelegationGrant(ctx, earlier.grant.ID)
	require.NoError(t, err)
	assert.Equal(t, revokeReasonRevoked, g.RevokeReason, "an already revoked grant keeps its reason")
	assertGrantActive(t, s, bystander)
}

// A hard delete revokes the grants of the row as it was before the delete;
// the grant rows outlive the agent row.
func TestAgentDelegationLifecycle_HardDeleteRevokesGrants(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "adtlc-hard", state.PhaseStopped)
	lg := seedLifecycleGrant(t, s, agent, lifecycleIssuer(t, s, "adtlc-hard").ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.True(t, agentGone(t, s, agent.ID))
	assertGrantRevoked(t, s, lg, revokeReasonAgentDeleted)
}

// A restore revokes any grant still active (reason agent_restored), and a
// grant the soft delete revoked comes back revoked: grants are never
// revived.
func TestAgentDelegationLifecycle_RestoreRevokesAndNeverRevives(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, "adtlc-restore", state.PhaseStopped)
	issuer := lifecycleIssuer(t, s, "adtlc-restore")
	revokedAtDelete := seedLifecycleGrant(t, s, agent, issuer.ID)
	softDeleteForTest(t, srv, agent.ID)
	assertGrantRevoked(t, s, revokedAtDelete, revokeReasonAgentDeleted)
	// A grant written while the agent was soft-deleted (no path issues
	// one; the hook fails closed anyway).
	survivor := seedLifecycleGrant(t, s, mustGetAgent(t, s, agent.ID), issuer.ID)

	rec := restoreForTest(t, srv, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.True(t, mustGetAgent(t, s, agent.ID).DeletedAt.IsZero())
	assertGrantRevoked(t, s, survivor, revokeReasonAgentRestored)
	assertGrantRevoked(t, s, revokedAtDelete, revokeReasonAgentDeleted)
}

// The reincarnation claim revokes the agent's grants in the claim
// transaction (reason agent_reincarnated), so none is usable once the
// claim commits, whatever the worker does afterwards.
func TestAgentDelegationLifecycle_ReincarnateClaimRevokesGrants(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	seedAgentEdge(t, s, tid("delegator"), agent)
	coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("coordinator-" + t.Name())
		a.Slug = "coordinator-" + tidSlugSafe(t.Name())
	})
	lg := seedLifecycleGrant(t, s, agent, lifecycleIssuer(t, s, "adtlc-reinc").ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, delegatingRequesterFor(coordinator.ID, project.ID), ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	assertGrantRevoked(t, s, lg, revokeReasonAgentReincarnated)
}

// --- a failing revocation rolls the lifecycle operation back (AC-5) ---

func TestAgentDelegationLifecycle_SoftDeleteRollsBackWhenRevocationFails(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, "adtlc-soft-fail", state.PhaseStopped)
	lg := seedLifecycleGrant(t, s, agent, lifecycleIssuer(t, s, "adtlc-soft-fail").ID)
	fault := installLifecycleFault(t, srv)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	_, details := errorBody(t, rec)
	assert.Equal(t, store.DeletionCodeFinalizeFailed, details["deletionCode"])

	got := mustGetAgent(t, s, agent.ID)
	assert.True(t, got.DeletedAt.IsZero(), "not soft-deleted")
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentSoftDelete, agent.ID))
	assertGrantActive(t, s, lg)

	fault.armed.Store(false)
	softDeleteForTest(t, srv, agent.ID)
	assertGrantRevoked(t, s, lg, revokeReasonAgentDeleted)
}

func TestAgentDelegationLifecycle_HardDeleteRollsBackWhenRevocationFails(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "adtlc-hard-fail", state.PhaseStopped)
	lg := seedLifecycleGrant(t, s, agent, lifecycleIssuer(t, s, "adtlc-hard-fail").ID)
	installLifecycleFault(t, srv)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	_, details := errorBody(t, rec)
	assert.Equal(t, store.DeletionCodeFinalizeFailed, details["deletionCode"])

	assert.False(t, agentGone(t, s, agent.ID), "the row is present")
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentHardDelete, agent.ID))
	assertGrantActive(t, s, lg)
}

// A failed revocation audit write rolls the soft delete back too, with the
// grant revocation (AC-5).
func TestAgentDelegationLifecycle_SoftDeleteRollsBackWhenRevocationAuditFails(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, "adtlc-soft-auditfail", state.PhaseStopped)
	lg := seedLifecycleGrant(t, s, agent, lifecycleIssuer(t, s, "adtlc-soft-auditfail").ID)
	fault := installLifecycleAuditFault(t, srv)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	_, details := errorBody(t, rec)
	assert.Equal(t, store.DeletionCodeFinalizeFailed, details["deletionCode"])

	assert.True(t, mustGetAgent(t, s, agent.ID).DeletedAt.IsZero(), "not soft-deleted")
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentSoftDelete, agent.ID))
	assertGrantActive(t, s, lg)

	fault.auditArmed.Store(false)
	softDeleteForTest(t, srv, agent.ID)
	assertGrantRevoked(t, s, lg, revokeReasonAgentDeleted)
}

func TestAgentDelegationLifecycle_RestoreRollsBackWhenRevocationFails(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, "adtlc-restore-fail", state.PhaseStopped)
	softDeleteForTest(t, srv, agent.ID)
	before := mustGetAgent(t, s, agent.ID)
	lg := seedLifecycleGrant(t, s, before, lifecycleIssuer(t, s, "adtlc-restore-fail").ID)
	installLifecycleFault(t, srv)

	rec := restoreForTest(t, srv, agent.ID)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

	got := mustGetAgent(t, s, agent.ID)
	assert.False(t, got.DeletedAt.IsZero(), "the row stays soft-deleted")
	assert.Equal(t, before.StateVersion, got.StateVersion)
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentRestore, agent.ID))
	assertGrantActive(t, s, lg)
}

func TestAgentDelegationLifecycle_ReincarnateClaimRollsBackWhenRevocationFails(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	edge := seedAgentEdge(t, s, tid("delegator"), agent)
	coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("coordinator-" + t.Name())
		a.Slug = "coordinator-" + tidSlugSafe(t.Name())
	})
	lg := seedLifecycleGrant(t, s, agent, lifecycleIssuer(t, s, "adtlc-reinc-fail").ID)
	installLifecycleFault(t, srv)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, delegatingRequesterFor(coordinator.ID, project.ID), ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

	assertNothingClaimed(t, s, agent, edge)
	assertGrantActive(t, s, lg)
}

// --- best-effort cascades (§16.3) ---

// An agent suspend revokes the agent's grants after the suspension is
// recorded (reason agent_suspended).
func TestAgentDelegationLifecycle_SuspendRevokesGrants(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "adtlc-suspend", state.PhaseRunning)
	lg := seedLifecycleGrant(t, s, agent, lifecycleIssuer(t, s, "adtlc-suspend").ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, string(state.PhaseSuspended), mustGetAgent(t, s, agent.ID).Phase)
	assertGrantRevoked(t, s, lg, revokeReasonAgentSuspended)
}

// The scheduler's auto-suspend revokes the grants as suspend does.
func TestAgentDelegationLifecycle_AutoSuspendRevokesGrants(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	ctx := context.Background()
	agent := setupBrokerAgentInPhase(t, s, "adtlc-autosuspend", state.PhaseRunning)
	lg := seedLifecycleGrant(t, s, agent, lifecycleIssuer(t, s, "adtlc-autosuspend").ID)
	markAgentStalled(t, s, agent.ID)
	loaded, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})

	require.Equal(t, string(state.PhaseSuspended), mustGetAgent(t, s, agent.ID).Phase)
	assertGrantRevoked(t, s, lg, revokeReasonAgentSuspended)
}

// claimTakeoverStore calls onRelease once, right after a
// ReleaseSupersededStart that released the claim: the first point at which
// a new start can take it.
type claimTakeoverStore struct {
	store.Store
	onRelease func()
	fired     atomic.Bool
}

func (c *claimTakeoverStore) ReleaseSupersededStart(ctx context.Context, agentID, claimID string, stopIntentAt time.Time) (bool, error) {
	released, err := c.Store.ReleaseSupersededStart(ctx, agentID, claimID, stopIntentAt)
	if err == nil && released && c.fired.CompareAndSwap(false, true) {
		c.onRelease()
	}
	return released, err
}

// The scheduler's auto-suspend revokes the suspended run's grants before it
// releases the start claim, as suspend does: a start that takes the released
// claim keeps the grants issued to its new run.
func TestAgentDelegationLifecycle_AutoSuspendKeepsNewRunGrants(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	ctx := context.Background()
	agent := setupBrokerAgentInPhase(t, s, "adtlc-autosuspend-newrun", state.PhaseRunning)
	issuer := lifecycleIssuer(t, s, "adtlc-autosuspend-newrun")
	_, err := s.ClaimAgentStart(ctx, agent.ID, "hub-suspended-run", store.StartClaimUser, "", time.Minute)
	require.NoError(t, err)
	suspendedRun := seedLifecycleGrant(t, s, agent, issuer.ID)
	markAgentStalled(t, s, agent.ID)
	loaded, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotEmpty(t, loaded.StartClaimID, "the suspended run holds a start claim")

	// The new start runs on the unwrapped store, inside the release, before
	// the auto-suspend takes its next step.
	var newRun lifecycleGrant
	var newClaim store.StartClaim
	var takeoverErr error
	takeover, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *claimTakeoverStore {
		return &claimTakeoverStore{Store: inner}
	})
	takeover.onRelease = func() {
		if newClaim, takeoverErr = s.ClaimAgentStart(ctx, agent.ID, "hub-new-run", store.StartClaimUser, "", time.Minute); takeoverErr != nil {
			return
		}
		newRun = seedLifecycleGrant(t, s, mustGetAgent(t, s, agent.ID), issuer.ID)
	}

	srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})

	require.True(t, takeover.fired.Load(), "the suspended run's claim was released")
	require.NoError(t, takeoverErr, "the new start took the released claim")
	require.NotNil(t, newRun.grant)
	after := mustGetAgent(t, s, agent.ID)
	require.Equal(t, string(state.PhaseSuspended), after.Phase)
	require.NotEmpty(t, newClaim.ID)
	require.Equal(t, newClaim.ID, after.StartClaimID, "the new start's claim survives the auto-suspend")
	assertGrantRevoked(t, s, suspendedRun, revokeReasonAgentSuspended)
	assertGrantActive(t, s, newRun)
}

// A failed best-effort revocation never fails the suspend; the grant stays
// active, and exchange and use deny on the suspended phase.
func TestAgentDelegationLifecycle_SuspendSucceedsWhenRevocationFails(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "adtlc-suspend-fail", state.PhaseRunning)
	lg := seedLifecycleGrant(t, s, agent, lifecycleIssuer(t, s, "adtlc-suspend-fail").ID)
	installLifecycleFault(t, srv)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, string(state.PhaseSuspended), mustGetAgent(t, s, agent.ID).Phase)
	assertGrantActive(t, s, lg)
}

// A failed best-effort revocation never fails the issuer suspend; the
// grant stays active, and exchange and use deny a suspended issuer.
func TestAgentDelegationLifecycle_IssuerSuspendSucceedsWhenRevocationFails(t *testing.T) {
	srv, s := testServer(t)
	issuer := lifecycleIssuer(t, s, "adtlc-issuer-suspend-fail")
	lg := seedLifecycleGrant(t, s, setupBrokerAgentInPhase(t, s, "adtlc-isf", state.PhaseStopped), issuer.ID)
	installLifecycleFault(t, srv)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+issuer.ID, map[string]string{"status": "suspended"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	u, err := s.GetUser(context.Background(), issuer.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusSuspended, u.Status)
	assertGrantActive(t, s, lg)
}

// An issuer suspend revokes every grant the issuer issued (reason
// issuer_suspended), on any agent, and reactivating the issuer does not
// revive them.
func TestAgentDelegationLifecycle_IssuerSuspendRevokesGrants(t *testing.T) {
	srv, s := testServer(t)
	issuer := lifecycleIssuer(t, s, "adtlc-issuer-suspend")
	other := lifecycleIssuer(t, s, "adtlc-issuer-suspend-other")
	a1 := setupBrokerAgentInPhase(t, s, "adtlc-is-1", state.PhaseStopped)
	a2 := setupBrokerAgentInPhase(t, s, "adtlc-is-2", state.PhaseStopped)
	g1 := seedLifecycleGrant(t, s, a1, issuer.ID)
	g2 := seedLifecycleGrant(t, s, a2, issuer.ID)
	notTheirs := seedLifecycleGrant(t, s, a1, other.ID)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+issuer.ID, map[string]string{"status": "suspended"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assertGrantRevoked(t, s, g1, revokeReasonIssuerSuspended)
	assertGrantRevoked(t, s, g2, revokeReasonIssuerSuspended)
	assertGrantActive(t, s, notTheirs)

	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+issuer.ID, map[string]string{"status": "active"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assertGrantRevoked(t, s, g1, revokeReasonIssuerSuspended)
}

// A profile change that does not suspend the issuer revokes nothing.
func TestAgentDelegationLifecycle_IssuerUpdateWithoutSuspendKeepsGrants(t *testing.T) {
	srv, s := testServer(t)
	issuer := lifecycleIssuer(t, s, "adtlc-issuer-update")
	lg := seedLifecycleGrant(t, s, setupBrokerAgentInPhase(t, s, "adtlc-iu", state.PhaseStopped), issuer.ID)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+issuer.ID, map[string]string{"displayName": "Renamed"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assertGrantActive(t, s, lg)
}

// An issuer delete revokes the issuer's grants after the delete commits
// (reason issuer_deleted); a grant never blocks the delete.
func TestAgentDelegationLifecycle_IssuerDeleteRevokesGrants(t *testing.T) {
	srv, s := testServer(t)
	issuer := lifecycleIssuer(t, s, "adtlc-issuer-delete")
	lg := seedLifecycleGrant(t, s, setupBrokerAgentInPhase(t, s, "adtlc-id", state.PhaseStopped), issuer.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+issuer.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := s.GetUser(context.Background(), issuer.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	assertGrantRevoked(t, s, lg, revokeReasonIssuerDeleted)
}

// A failed best-effort revocation never fails the user delete.
func TestAgentDelegationLifecycle_IssuerDeleteSucceedsWhenRevocationFails(t *testing.T) {
	srv, s := testServer(t)
	issuer := lifecycleIssuer(t, s, "adtlc-issuer-delete-fail")
	lg := seedLifecycleGrant(t, s, setupBrokerAgentInPhase(t, s, "adtlc-idf", state.PhaseStopped), issuer.ID)
	installLifecycleFault(t, srv)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+issuer.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := s.GetUser(context.Background(), issuer.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	assertGrantActive(t, s, lg)
}

// --- project delete (§16.3, §17.2 #41) ---

// A project delete revokes the grants of every agent of the project,
// soft-deleted ones included, inside its transaction (reason
// project_deleted), records the count in the project_delete summary, and
// leaves the grants of agents in other projects alone.
func TestAgentDelegationLifecycle_ProjectDeleteRevokesGrants(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	f := newProjectEdgeFixture(t, s, "adtlc-pd")
	issuer := lifecycleIssuer(t, s, "adtlc-pd")
	live := seedLifecycleGrant(t, s, f.live, issuer.ID)
	soft := seedLifecycleGrant(t, s, f.soft, issuer.ID)
	outside := seedLifecycleGrant(t, s, f.outside, issuer.ID)

	req := f.request()
	_, decision := srv.deletionService.Delete(setTestIdentity(ctx, req.Actor), req)
	require.Nil(t, decision)

	assertGrantRevoked(t, s, live, revokeReasonProjectDeleted)
	assertGrantRevoked(t, s, soft, revokeReasonProjectDeleted)
	assertGrantActive(t, s, outside)
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "project_delete", TargetID: f.projectID})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	var sum map[string]any
	require.NoError(t, json.Unmarshal([]byte(audits[0].AfterSummary), &sum))
	assert.EqualValues(t, 2, sum["agent_delegation_grants_revoked"])
}

// A revocation failure fails the project delete and rolls back the project
// row and every grant revocation.
func TestAgentDelegationLifecycle_ProjectDeleteRollsBackWhenRevocationFails(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	f := newProjectEdgeFixture(t, s, "adtlc-pd-fail")
	lg := seedLifecycleGrant(t, s, f.live, lifecycleIssuer(t, s, "adtlc-pd-fail").ID)
	fault := &adtLifecycleFaultStore{Store: s}
	fault.armed.Store(true)

	svc := NewProjectDeletionService(fault, newTestAuthzService(s), slog.Default())
	req := f.request()
	result, decision := svc.Delete(setTestIdentity(ctx, req.Actor), req)
	require.Nil(t, result)
	require.NotNil(t, decision, "the revocation fault fails the delete")
	assert.Equal(t, http.StatusInternalServerError, decision.HTTPStatus)

	_, err := s.GetProject(ctx, f.projectID)
	require.NoError(t, err, "the project row is kept")
	assertGrantActive(t, s, lg)
}

// A failed revocation audit write fails the project delete and rolls back
// the project row and the grant revocation (AC-5).
func TestAgentDelegationLifecycle_ProjectDeleteRollsBackWhenRevocationAuditFails(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	f := newProjectEdgeFixture(t, s, "adtlc-pd-auditfail")
	lg := seedLifecycleGrant(t, s, f.live, lifecycleIssuer(t, s, "adtlc-pd-auditfail").ID)
	fault := &adtLifecycleFaultStore{Store: s}
	fault.auditArmed.Store(true)

	svc := NewProjectDeletionService(fault, newTestAuthzService(s), slog.Default())
	req := f.request()
	result, decision := svc.Delete(setTestIdentity(ctx, req.Actor), req)
	require.Nil(t, result)
	require.NotNil(t, decision, "the audit fault fails the delete")
	assert.Equal(t, http.StatusInternalServerError, decision.HTTPStatus)

	_, err := s.GetProject(ctx, f.projectID)
	require.NoError(t, err, "the project row is kept")
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "project_delete", TargetID: f.projectID})
	require.NoError(t, err)
	assert.Empty(t, audits, "no project_delete record")
	assertGrantActive(t, s, lg)
}

// --- end to end through the delegation routes ---

// After a lifecycle revocation, the agent's delegated credential is
// refused at its next use and the grant can no longer be exchanged, and
// the restore does not bring either back (§17.2 #4, #32).
func TestAgentDelegationLifecycle_RevokedCredentialDeniesAtUse(t *testing.T) {
	f := newADTFixture(t, "adtlc-e2e")
	ctx := context.Background()
	f.srv.config.SoftDeleteRetention = time.Hour
	grant := f.hubGrant(t)
	cred := f.delegated(t, grant.ID)
	require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code)
	agentToken := f.agentJWT(t, f.agentA)

	rec := f.do(t, http.MethodDelete, "/api/v1/agents/"+f.agentA.ID, nil, adtBearer(f.session(t, f.alice)))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	g, err := f.store.GetAgentDelegationGrant(ctx, grant.ID)
	require.NoError(t, err)
	require.NotNil(t, g.RevokedAt)
	assert.Equal(t, revokeReasonAgentDeleted, g.RevokeReason)
	assert.Equal(t, "user:"+f.alice.ID, g.RevokedBy, "the user whose delete caused the revocation")
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, cred.Token, f.agentB.ID).Code, "the credential is refused")

	rec = f.do(t, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/restore", nil, adtBearer(f.session(t, f.alice)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, cred.Token, f.agentB.ID).Code, "still refused after the restore")
	rec = f.exchange(t, agentToken, f.agentA.ID, grant.ID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
	assert.NotEqual(t, http.StatusOK, rec.Code, "the revoked grant cannot be exchanged: %s", rec.Body.String())
}

// An issuer suspend through the users route ends the credentials of the
// issuer's grants at their next use, and they stay refused after the
// issuer is reactivated (§17.2 #5).
func TestAgentDelegationLifecycle_IssuerSuspendDeniesAtUse(t *testing.T) {
	f := newADTFixture(t, "adtlc-e2e-issuer")
	d, dave, _ := f.transferredAgent(t, "adtlc-e2e-issuer")
	grant := f.grantFor(t, f.session(t, dave), d.ID)
	cred := f.credentialFor(t, d, grant.ID)
	require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code)

	rec := doRequest(t, f.srv, http.MethodPatch, "/api/v1/users/"+dave.ID, map[string]string{"status": "suspended"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	g, err := f.store.GetAgentDelegationGrant(context.Background(), grant.ID)
	require.NoError(t, err)
	require.NotNil(t, g.RevokedAt)
	assert.Equal(t, revokeReasonIssuerSuspended, g.RevokeReason)

	rec = doRequest(t, f.srv, http.MethodPatch, "/api/v1/users/"+dave.ID, map[string]string{"status": "active"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, cred.Token, f.agentB.ID).Code, "a revoked grant is not revived by reactivation")
}
