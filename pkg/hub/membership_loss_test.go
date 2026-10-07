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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingChecks claims every claimable check with a tiny lease and returns
// them; the claims expire at once, so the checks stay claimable.
func pendingChecks(t *testing.T, s store.Store) []*store.MembershipLossCheck {
	t.Helper()
	cs, err := s.ClaimMembershipLossChecks(context.Background(), 100, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	return cs
}

func countAudits(t *testing.T, s store.Store, mutationType, targetID string) int {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationType, Limit: 1000})
	require.NoError(t, err)
	n := 0
	for _, r := range recs {
		if targetID == "" || r.TargetID == targetID {
			n++
		}
	}
	return n
}

// The entire descendant tree is held: user agent, agent child,
// a scheduled grandchild recorded only by created_by, and a soft-deleted
// great-grandchild. Credentials are revoked and run intent stopped.
func TestHold_DescendantTree(t *testing.T) {
	f := newMSFixture(t, "tree")
	ctx := context.Background()
	sched := f.newAgentRow("tree-sched", "", f.childC.ID, nil)
	deleted := f.newAgentRow("tree-deleted", sched.ID, sched.ID, nil)
	deleted.DeletedAt = time.Now()
	require.NoError(t, f.s.UpdateAgent(ctx, deleted))
	otherProject := tid("ms-tree-other-project")
	createRS1Project(t, f.s, otherProject, tid("ms-tree-other-owner"))

	tok := f.agentToken(f.agentA)
	claims, err := f.srv.agentTokenService.ValidateAgentToken(tok)
	require.NoError(t, err)

	f.removeAndProcess(f.userID)

	for _, a := range []*store.Agent{f.agentA, f.childC, sched, deleted} {
		assert.True(t, f.held(a.ID), "agent %s must be held", a.Slug)
		assert.Equal(t, 1, countAudits(t, f.s, mutationTypeAgentHoldSet, a.ID), a.Slug)
	}
	for _, a := range []*store.Agent{f.agentA, f.childC, sched} {
		got, err := f.s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.RunIntentStopped, got.RunIntent, a.Slug)
	}
	cred, err := f.s.GetAgentCredentialByJTIHash(ctx, hashJTI(claims.ID))
	if err == nil {
		assert.NotNil(t, cred.RevokedAt, "credential must be revoked in the hold transaction")
	}
	assert.Equal(t, 1, countAudits(t, f.s, mutationTypeMembershipLossProcessed, f.userID))
	assert.Empty(t, pendingChecks(t, f.s), "a fully processed check is completed")
}

// Still admitted (for example a role change that keeps access): nothing held.
func TestMembershipLoss_AdmittedIsNoop(t *testing.T) {
	f := newMSFixture(t, "admitted")
	ctx := context.Background()
	require.NoError(t, enqueueMembershipLossTx(ctx, f.s, f.userID, f.projectID, store.MembershipLossTriggerMemberRoleChange, AuditActor{}))
	f.srv.drainMembershipLossChecks(ctx)
	assert.False(t, f.held(f.agentA.ID))
	assert.False(t, f.held(f.childC.ID))
	assert.Empty(t, pendingChecks(t, f.s))
}

// A re-add committed before processing means no hold; a re-add after does not
// lift the hold.
func TestMembershipLoss_ReAdd(t *testing.T) {
	f := newMSFixture(t, "readd")
	ctx := context.Background()
	f.dropBindings(f.userID)
	require.NoError(t, enqueueMembershipLossTx(ctx, f.s, f.userID, f.projectID, store.MembershipLossTriggerMemberRemove, AuditActor{}))
	f.addMember(f.userID, store.ProjectRoleMember)
	f.srv.drainMembershipLossChecks(ctx)
	assert.False(t, f.held(f.agentA.ID), "re-added before processing: no hold")

	f.removeAndProcess(f.userID)
	require.True(t, f.held(f.agentA.ID))
	f.addMember(f.userID, store.ProjectRoleMember)
	f.srv.drainMembershipLossChecks(ctx)
	assert.True(t, f.held(f.agentA.ID), "re-add after the hold does not lift it")
	requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonAgentHeld)
}

// Agents of other projects are untouched.
func TestMembershipLoss_OtherProjectUntouched(t *testing.T) {
	f := newMSFixture(t, "otherproj")
	other := tid("ms-otherproj-p2")
	createRS1Project(t, f.s, other, tid("ms-otherproj-o2"))
	g := &msFixture{t: t, srv: f.srv, s: f.s, projectID: other}
	g.addMember(f.userID, store.ProjectRoleMember)
	b := g.userAgent("otherproj-b", f.userID)
	f.removeAndProcess(f.userID)
	assert.True(t, f.held(f.agentA.ID))
	assert.False(t, f.held(b.ID))
	require.NoError(t, f.srv.agentStanding(context.Background(), b.ID))
}

// With a node bound smaller than the tree, the processor holds what the
// walk returns and walks again until the full tree is held.
func TestMembershipLoss_WalkProgressSmallMaxNodes(t *testing.T) {
	f := newMSFixture(t, "smallnodes")
	parent := f.childC
	var all []*store.Agent
	for i := 0; i < 5; i++ {
		parent = f.childAgent(fmt.Sprintf("small-%d", i), parent)
		all = append(all, parent)
		all = append(all, f.childAgent(fmt.Sprintf("small-sib-%d", i), parent))
	}
	orig := descendantQueryBounds
	descendantQueryBounds.MaxNodes = 2
	t.Cleanup(func() { descendantQueryBounds = orig })

	f.removeAndProcess(f.userID)
	for _, a := range append(all, f.agentA, f.childC) {
		assert.True(t, f.held(a.ID), "agent %s must be held", a.Slug)
	}
	assert.Empty(t, pendingChecks(t, f.s))
}

// A walk that reaches the depth bound fails the check (it is not
// completed) after holding what it found; after repeated claims the check is
// parked visibly and completed. The live check refuses the deeper agents.
func TestMembershipLoss_DepthLimitFailsThenParks(t *testing.T) {
	f := newMSFixture(t, "depth")
	ctx := context.Background()
	grand := f.childAgent("depth-grand", f.childC)
	orig := descendantQueryBounds
	descendantQueryBounds.MaxDepth = 1
	t.Cleanup(func() { descendantQueryBounds = orig })
	origLease := membershipLossLease
	membershipLossLease = time.Millisecond
	t.Cleanup(func() { membershipLossLease = origLease })

	f.dropBindings(f.userID)
	require.NoError(t, enqueueMembershipLossTx(ctx, f.s, f.userID, f.projectID, store.MembershipLossTriggerMemberRemove, AuditActor{}))
	f.srv.drainMembershipLossChecks(ctx)
	assert.True(t, f.held(f.agentA.ID), "the agents found before the bound are held")
	assert.False(t, f.held(grand.ID))
	requireStandingReason(t, f.srv.agentStanding(ctx, grand.ID), standingReasonChainHeld)

	parked := false
	for i := 0; i < membershipLossDepthParkAttempts+2 && !parked; i++ {
		time.Sleep(3 * time.Millisecond)
		f.srv.drainMembershipLossChecks(ctx)
		parked = countAudits(t, f.s, mutationTypeMembershipLossParked, f.userID) > 0
	}
	require.True(t, parked, "a check that keeps reaching the depth bound is parked")
	time.Sleep(3 * time.Millisecond)
	assert.Empty(t, pendingChecks(t, f.s), "a parked check is completed")
}

// walkFaultStore fails ListDelegationDescendants, inside transactions too.
type walkFaultStore struct {
	store.Store
}

func (w *walkFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return w.Store.WithTx(ctx, func(tx store.Store) error { return fn(&walkFaultStore{Store: tx}) })
}

func (w *walkFaultStore) ListDelegationDescendants(context.Context, store.DescendantQuery) (store.DescendantResult, error) {
	return store.DescendantResult{}, errors.New("injected walk fault")
}

// Any walk error fails the check: nothing is held, and the check is kept
// (failed, with its error and attempt recorded), not completed.
func TestMembershipLoss_WalkErrorFailsCheck(t *testing.T) {
	f := newMSFixture(t, "walkerr")
	ctx := context.Background()
	origLease := membershipLossLease
	membershipLossLease = time.Millisecond
	t.Cleanup(func() { membershipLossLease = origLease })
	f.dropBindings(f.userID)
	require.NoError(t, enqueueMembershipLossTx(ctx, f.s, f.userID, f.projectID, store.MembershipLossTriggerMemberRemove, AuditActor{}))
	orig := f.srv.store
	f.srv.store = &walkFaultStore{Store: orig}
	f.srv.drainMembershipLossChecks(ctx)
	f.srv.store = orig
	assert.False(t, f.held(f.agentA.ID))
	time.Sleep(5 * time.Millisecond)
	checks, err := f.s.ClaimMembershipLossChecks(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, checks, 1, "the failed check is kept, not completed")
	assert.Contains(t, checks[0].LastError, "injected walk fault")
	assert.GreaterOrEqual(t, checks[0].Attempts, 2, "claimed by the drain and again here")
}

// walkFaultProjectStore fails ListDelegationDescendants for one project,
// inside transactions too.
type walkFaultProjectStore struct {
	store.Store
	failProject string
}

func (w *walkFaultProjectStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return w.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&walkFaultProjectStore{Store: tx, failProject: w.failProject})
	})
}

func (w *walkFaultProjectStore) ListDelegationDescendants(ctx context.Context, q store.DescendantQuery) (store.DescendantResult, error) {
	if q.ProjectID == w.failProject {
		return store.DescendantResult{}, errors.New("injected walk fault for one project")
	}
	return w.Store.ListDelegationDescendants(ctx, q)
}

// A check covering several projects is parked only when every project hit
// the depth bound: with one project at the depth bound and another failing
// for another reason, the check stays failed and is retried.
func TestMembershipLoss_MixedErrorsNotParked(t *testing.T) {
	f := newMSFixture(t, "mixed")
	ctx := context.Background()
	other := tid("ms-mixed-p2")
	createRS1Project(t, f.s, other, tid("ms-mixed-o2"))
	g := &msFixture{t: t, srv: f.srv, s: f.s, projectID: other}
	g.addMember(f.userID, store.ProjectRoleMember)
	b := g.userAgent("mixed-b", f.userID)
	f.dropBindings(f.userID)
	g.dropBindings(f.userID)

	origBounds := descendantQueryBounds
	descendantQueryBounds.MaxDepth = 1 // C sits at depth 2 in the first project
	t.Cleanup(func() { descendantQueryBounds = origBounds })
	origLease := membershipLossLease
	membershipLossLease = time.Millisecond
	t.Cleanup(func() { membershipLossLease = origLease })

	require.NoError(t, enqueueMembershipLossTx(ctx, f.s, f.userID, "", store.MembershipLossTriggerGroupChange, AuditActor{}))
	orig := f.srv.store
	f.srv.store = &walkFaultProjectStore{Store: orig, failProject: other}
	for i := 0; i < membershipLossDepthParkAttempts+3; i++ {
		time.Sleep(3 * time.Millisecond)
		f.srv.drainMembershipLossChecks(ctx)
	}
	f.srv.store = orig

	assert.Equal(t, 0, countAudits(t, f.s, mutationTypeMembershipLossParked, f.userID), "not parked while another project fails")
	assert.False(t, f.held(b.ID), "the failing project's agents are not held yet")
	time.Sleep(3 * time.Millisecond)
	checks, err := f.s.ClaimMembershipLossChecks(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, checks, 1, "the check is still pending")
	assert.Contains(t, checks[0].LastError, "injected walk fault for one project")
}

// A check for a project that no longer exists completes as a no-op.
func TestMembershipLoss_MissingProjectCompletes(t *testing.T) {
	f := newMSFixture(t, "noproject")
	ctx := context.Background()
	require.NoError(t, enqueueMembershipLossTx(ctx, f.s, f.userID, uuid.NewString(), store.MembershipLossTriggerMemberRemove, AuditActor{}))
	f.srv.drainMembershipLossChecks(ctx)
	assert.Empty(t, pendingChecks(t, f.s))
	assert.False(t, f.held(f.agentA.ID))
}

// A claim lost before completion is not a failure: no error is recorded
// on the check, which the other claimer owns.
func TestMembershipLoss_ClaimLostIsNotFailure(t *testing.T) {
	f := newMSFixture(t, "claimlost")
	ctx := context.Background()
	require.NoError(t, enqueueMembershipLossTx(ctx, f.s, f.userID, f.projectID, store.MembershipLossTriggerMemberRoleChange, AuditActor{}))
	first, err := f.s.ClaimMembershipLossChecks(ctx, 1, time.Millisecond)
	require.NoError(t, err)
	require.Len(t, first, 1)
	time.Sleep(5 * time.Millisecond)
	second, err := f.s.ClaimMembershipLossChecks(ctx, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, second, 1)

	assert.False(t, f.srv.processClaimedMembershipLossCheck(ctx, first[0]), "the stale claim does not complete the check")
	// The second claimer still owns it and can complete it.
	require.NoError(t, f.s.CompleteMembershipLossCheck(ctx, second[0].ID, second[0].Attempts))
}

// msStopDispatcher records stops and fails them while failing is set.
type msStopDispatcher struct {
	*createAgentDispatcher
	failing atomic.Bool
	stops   atomic.Int32
}

func (d *msStopDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	d.stops.Add(1)
	if d.failing.Load() {
		return errors.New("broker unreachable")
	}
	return nil
}

// With the broker unreachable the hold and the credential
// revoke still commit; the stop is retried and the phase moves only once the
// stop succeeds.
func TestHold_BrokerOffline_RevokeCommittedStopRetried(t *testing.T) {
	f := newMSFixture(t, "offline")
	ctx := context.Background()
	d := &msStopDispatcher{createAgentDispatcher: &createAgentDispatcher{}}
	d.failing.Store(true)
	f.srv.SetDispatcher(d)

	f.removeAndProcess(f.userID)
	require.True(t, f.held(f.agentA.ID))
	assert.GreaterOrEqual(t, int(d.stops.Load()), 1)
	assert.GreaterOrEqual(t, countAudits(t, f.s, mutationTypeAgentHoldCredentialRevoke, f.agentA.ID), 1)
	got, err := f.s.GetAgent(ctx, f.agentA.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "the phase moves only once the stop is confirmed")
	assert.True(t, heldAgentNeedsStop(got))

	d.failing.Store(false)
	f.srv.retryHeldAgentStops(ctx)
	got, err = f.s.GetAgent(ctx, f.agentA.ID)
	require.NoError(t, err)
	assert.Contains(t, []string{string(state.PhaseSuspended), string(state.PhaseStopped)}, got.Phase)
	assert.False(t, heldAgentNeedsStop(got))
}

// The full sweep counts the agents it will hold, and the agents
// with no resolvable root, before it enqueues; then it holds them.
func TestMembershipSweep_MeasuresThenHolds(t *testing.T) {
	f := newMSFixture(t, "sweep")
	ctx := context.Background()
	f.newAgentRow("sweep-orphan", "", "", nil)
	f.dropBindings(f.userID) // removed before the upgrade: no check written

	res, err := f.srv.membershipFullSweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, res.WouldHold, "A and C")
	assert.Equal(t, 1, res.Unresolved)
	assert.Equal(t, 1, res.NotAdmittedPairs)
	assert.Equal(t, 1, res.Enqueued)
	assert.True(t, f.held(f.agentA.ID))
	assert.True(t, f.held(f.childC.ID))

	again, err := f.srv.membershipFullSweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, again.WouldHold, "already held agents are not counted again")
	assert.Equal(t, 0, again.Enqueued)
}

// Expiry scan (path 10): a project binding that expired recently is picked up.
func TestMembershipExpiryScan_HoldsAfterExpiry(t *testing.T) {
	f := newMSFixture(t, "expiry")
	ctx := context.Background()
	f.dropBindings(f.userID)
	rd, err := f.s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	expired := time.Now().Add(-time.Minute)
	_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, CreatedBy: "test", ExpiresAt: &expired,
	})
	require.NoError(t, err)
	require.NoError(t, f.srv.membershipExpiryScan(ctx, time.Now()))
	assert.True(t, f.held(f.agentA.ID))
	assert.True(t, f.held(f.childC.ID))
}

// Restore hook: a soft-deleted agent restored after its root lost access
// comes back held.
func TestRestoreHook_HoldsWhenRootNotAdmitted(t *testing.T) {
	f := newMSFixture(t, "restore")
	ctx := context.Background()
	f.dropBindings(f.userID)
	require.NoError(t, f.s.WithTx(ctx, func(tx store.Store) error {
		return f.srv.membershipRestoreHook(ctx, tx, f.agentA, AuditActor{PrincipalKind: "user", PrincipalID: f.ownerID})
	}))
	assert.True(t, f.held(f.agentA.ID))
	assert.Equal(t, 1, countAudits(t, f.s, mutationTypeAgentHoldSet, f.agentA.ID))

	g := newMSFixture(t, "restore-member")
	require.NoError(t, g.s.WithTx(ctx, func(tx store.Store) error {
		return g.srv.membershipRestoreHook(ctx, tx, g.agentA, AuditActor{})
	}))
	assert.False(t, g.held(g.agentA.ID), "a member's agent is restored without a hold")
}

func TestRestoreHook_Registered(t *testing.T) {
	srv, _ := testServer(t)
	found := false
	for _, h := range srv.lifecycleTxHooks.snapshot(&srv.lifecycleTxHooks.restore) {
		if h.name == membershipRestoreHookName {
			found = true
		}
	}
	assert.True(t, found)
}

// No agent principal can clear a hold, and an agent's own
// requests do not lift it.
func TestHeldAgent_CannotWakeOrStartItself(t *testing.T) {
	f := newMSFixture(t, "selfresume")
	tok := f.agentToken(f.agentA)
	f.hold(f.agentA.ID, f.userID)
	for _, path := range []string{"/start", "/restart", "/reincarnate", "/hold/lift"} {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+path, nil, tok)
		assert.NotEqual(t, http.StatusOK, rec.Code, path)
	}
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+f.agentA.ID,
		map[string]interface{}{"annotations": map[string]string{"scion.dev/suspension": "none"}}, tok)
	assert.NotEqual(t, http.StatusOK, rec.Code)
	assert.True(t, f.held(f.agentA.ID))
	_, err := f.s.ClearAgentHolds(context.Background(), f.agentA.ID, store.ClearActor{Kind: "agent", ID: f.agentA.ID}, "self")
	require.ErrorIs(t, err, store.ErrInvalidActor)
}

// The hub-admin lift clears holds only when every hold's root user
// is admitted again; it does not start the agent.
func TestAgentHoldLift(t *testing.T) {
	f := newMSFixture(t, "lift")
	f.removeAndProcess(f.userID)
	require.True(t, f.held(f.agentA.ID))

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/hold/lift", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.True(t, f.held(f.agentA.ID))

	f.addMember(f.userID, store.ProjectRoleMember)
	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/hold/lift", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, f.held(f.agentA.ID))
	assert.Equal(t, 1, countAudits(t, f.s, mutationTypeAgentHoldCleared, f.agentA.ID))
	got, err := f.s.GetAgent(context.Background(), f.agentA.ID)
	require.NoError(t, err)
	assert.NotEqual(t, store.RunIntentRunning, got.RunIntent, "the lift does not start the agent")

	// A non-admin user is refused.
	member := &store.User{ID: f.userID, Email: f.userID + "@test.com", Role: "member", Status: store.UserStatusActive}
	rec = doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/agents/"+f.childC.ID+"/hold/lift", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// The API shows a suspension view while the agent is held.
func TestAgentSuspensionField(t *testing.T) {
	f := newMSFixture(t, "field")
	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/agents/"+f.agentA.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"suspension"`)
	f.hold(f.agentA.ID, f.userID)
	rec = doRequest(t, f.srv, http.MethodGet, "/api/v1/agents/"+f.agentA.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"suspension":{"held":true`)
	assert.NotContains(t, rec.Body.String(), f.userID+`"`+`,"cause"`)
	rec = doRequest(t, f.srv, http.MethodGet, "/api/v1/agents?projectId="+f.projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"suspension":{"held":true`)
}

// The agent create path enforces the creator's hold and its root's
// membership at create time.
func TestAgentCreate_CreatorStandingEnforced(t *testing.T) {
	// A held creator: its token is refused at authentication, and the
	// create gate itself refuses it (the creator's chain admits the create;
	// the hold refuses it).
	f := newMSFixture(t, "create")
	f.childC.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)}
	require.NoError(t, f.s.UpdateAgent(context.Background(), f.childC))
	tok := f.agentToken(f.childC)
	f.hold(f.childC.ID, f.userID)
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents",
		map[string]interface{}{"name": "create-child", "projectId": f.projectID}, tok)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	direct := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), f.agentIdentity(f.childC)))
	assert.False(t, f.srv.authorizeAgentCreate(direct, req, f.projectID))
	assert.Equal(t, http.StatusForbidden, direct.Code, direct.Body.String())

	// A creator whose root is no longer a member is refused at create.
	g := newMSFixture(t, "create-removed")
	tokG := g.agentToken(g.agentA)
	g.dropBindings(g.userID)
	rec = doRequestWithAgentToken(t, g.srv, http.MethodPost, "/api/v1/agents",
		map[string]interface{}{"name": "create-child2", "projectId": g.projectID}, tokG)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// Principal IDs written to edges and ancestry are stored canonically,
// so the walk finds agents however the writer spelled the IDs.
func TestCanonicalPrincipalIDs_WalkFindsUpperCaseWrites(t *testing.T) {
	f := newMSFixture(t, "canonical")
	ctx := context.Background()
	upper := func(s string) string {
		out := []byte(s)
		for i, c := range out {
			if c >= 'a' && c <= 'f' {
				out[i] = c - 32
			}
		}
		return string(out)
	}
	a := f.newAgentRow("canon", f.userID, f.userID, []string{upper(f.userID)})
	f.edge(store.DelegationPrincipalUser, upper(f.userID), a)
	got, err := f.s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, []string{f.userID}, got.Ancestry)
	edges, err := f.s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, a.ID)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, f.userID, edges[0].DelegatorID)
	assert.Equal(t, f.projectID, edges[0].ScopeID)

	f.removeAndProcess(f.userID)
	assert.True(t, f.held(a.ID))
}

// A hold lookup error on the ceiling agent hop denies with
// the resolution cause.
type holdFaultStore struct {
	store.Store
	mu   sync.Mutex
	fail bool
}

func (h *holdFaultStore) HasActiveAgentHold(ctx context.Context, id string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail {
		return false, errors.New("injected hold lookup fault")
	}
	return h.Store.HasActiveAgentHold(ctx, id)
}

// A hold lookup fault on the ceiling agent hop is returned as an error, so
// the decision denies with the ceiling-error cause.
func TestCeilingAgentHop_HoldLookupFaultIsCeilingError(t *testing.T) {
	f := newMSFixture(t, "holdfault")
	ctx := context.Background()
	hs := &holdFaultStore{Store: f.s, fail: true}
	orig := f.srv.authzService.store
	f.srv.authzService.store = hs
	t.Cleanup(func() { f.srv.authzService.store = orig })

	_, _, err := f.srv.authzService.checkAgentHoldsPermission(ctx, f.agentA.ID, "agent.read", store.RoleScopeProject, f.projectID)
	require.ErrorIs(t, err, errCeilingHoldLookup)

	allowed, _, cerr := f.srv.authzService.checkDelegationCeiling(ctx, agentCeilingRequest(f.childC), "agent.read", f.childC.ID, nil, nil)
	require.ErrorIs(t, cerr, errCeilingHoldLookup)
	assert.False(t, allowed)
	// Decide tags every error the ceiling returns as DenyCauseCeilingError
	// (authz.go, step 10).
}

func TestCeilingAgentHop_HeldDelegatorNotLive(t *testing.T) {
	f := newMSFixture(t, "heldhop")
	ctx := context.Background()
	f.hold(f.agentA.ID, f.userID)
	var cause DenyCause
	allowed, _, err := f.srv.authzService.checkDelegationCeiling(ctx, agentCeilingRequest(f.childC), "agent.read", f.childC.ID, nil, &cause)
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Equal(t, DenyCauseCeilingOrphaned, cause)
}

// agentCeilingRequest is a read of the agent itself by the agent, for driving
// the delegation ceiling directly.
func agentCeilingRequest(a *store.Agent) AuthzRequest {
	return AuthzRequest{
		Principal:  PrincipalContext{Kind: PrincipalKindAgent, ID: a.ID, Identity: &storedAgentIdentity{agent: a}},
		Resource:   agentResource(a),
		Action:     ActionRead,
		Permission: "agent.read",
	}
}

// lockTrackingStore is an AdvisoryLocker that really holds keys: a key held
// by one task cannot be taken by another until released.
type lockTrackingStore struct {
	store.Store
	mu       sync.Mutex
	held     map[store.AdvisoryLockKey]bool
	acquired []store.AdvisoryLockKey
}

func (l *lockTrackingStore) TryAdvisoryLock(_ context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[key] {
		return false, nil, nil
	}
	l.held[key] = true
	l.acquired = append(l.acquired, key)
	return true, func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.held, key)
		return nil
	}, nil
}

func (l *lockTrackingStore) TryAdvisoryLockObject(_ context.Context, _ store.AdvisoryLockKey, _ int32) (bool, func() error, error) {
	return true, func() error { return nil }, nil
}

func (l *lockTrackingStore) took(key store.AdvisoryLockKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range l.acquired {
		if k == key {
			return true
		}
	}
	return false
}

// The expiry scan, the full sweep and the stop retry each hold their own
// lock: either can run while another holds its lock (they can fire on the
// same tick).
func TestMembershipReconciler_TasksHoldDistinctLocks(t *testing.T) {
	f := newMSFixture(t, "locks")
	lts := &lockTrackingStore{Store: f.s, held: map[store.AdvisoryLockKey]bool{}}
	f.srv.scheduler = NewScheduler(lts, slog.Default())
	f.srv.registerMembershipStandingReconciler()
	handlers := map[string]RecurringHandler{}
	for _, h := range f.srv.scheduler.recurring {
		handlers[h.Name] = h
	}
	expiry, sweep, stops := handlers["membership-expiry-scan"], handlers["membership-standing-sweep"], handlers["membership-hold-stop-retry"]
	require.NotNil(t, expiry.Fn)
	require.NotNil(t, sweep.Fn)
	require.NotNil(t, stops.Fn)
	assert.True(t, expiry.Singleton && sweep.Singleton && stops.Singleton)
	ctx := context.Background()

	// The sweep holds its lock; the expiry scan and the stop retry still run.
	lts.held[store.LockMembershipStandingSweep] = true
	expiry.Fn(ctx)
	stops.Fn(ctx)
	assert.True(t, lts.took(store.LockMembershipExpiryScan), "the expiry scan ran while the sweep held its lock")
	assert.True(t, lts.took(store.LockMembershipStopRetry), "the stop retry ran while the sweep held its lock")

	// The expiry scan holds its lock; the sweep still runs.
	delete(lts.held, store.LockMembershipStandingSweep)
	lts.held[store.LockMembershipExpiryScan] = true
	sweep.Fn(ctx)
	assert.True(t, lts.took(store.LockMembershipStandingSweep), "the sweep ran while the expiry scan held its lock")
}

// userFaultStore fails GetUser for one user ID.
type userFaultStore struct {
	store.Store
	failID string
}

func (u *userFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == u.failID {
		return nil, errors.New("injected user lookup fault")
	}
	return u.Store.GetUser(ctx, id)
}

// One pair whose lookups fail does not stop the sweep for the others: the
// other user's agents are still held, the failure is counted and returned.
func TestMembershipSweep_PairFaultDoesNotStopOthers(t *testing.T) {
	f := newMSFixture(t, "sweepfault")
	ctx := context.Background()
	other := tid("ms-sweepfault-other")
	f.addUser(other)
	f.addMember(other, store.ProjectRoleMember)
	b := f.userAgent("sweepfault-b", other)
	f.dropBindings(f.userID)
	f.dropBindings(other)

	orig := f.srv.store
	f.srv.store = &userFaultStore{Store: orig, failID: f.userID}
	res, err := f.srv.membershipFullSweep(ctx)
	f.srv.store = orig
	require.Error(t, err, "the failed pair is reported")
	assert.GreaterOrEqual(t, res.Failed, 1)
	assert.True(t, f.held(b.ID), "the other user's agent is still held")
}
