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
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schedFire is a scheduled dispatch fixture: a project with a member user
// who may create agents (the creator), and no dispatcher unless a test
// installs one.
type schedFire struct {
	*uatCreateFixture
}

func newSchedFire(t *testing.T, name string) *schedFire {
	t.Helper()
	return &schedFire{uatCreateFixture: newUATCreateFixture(t, name)}
}

// event returns a dispatch_agent event for agentName whose history creator
// is the fixture creator and which carries no authorization revision.
func (f *schedFire) event(agentName string) store.ScheduledEvent {
	return store.ScheduledEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.proj.ID,
		EventType:  "dispatch_agent",
		Payload:    `{"agentName":"` + agentName + `"}`,
		CreatedBy:  f.creator.ID,
		ScheduleID: "sched-" + agentName,
		FireAt:     time.Now(),
	}
}

func (f *schedFire) fire(t *testing.T, evt store.ScheduledEvent) error {
	t.Helper()
	return f.srv.dispatchAgentEventHandler()(context.Background(), evt)
}

// child returns the scheduled child for slug and its single active edge.
func (f *schedFire) child(t *testing.T, slug string) (*store.Agent, *store.DelegationEdge) {
	t.Helper()
	agent, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, slug)
	require.NoError(t, err, "scheduled child %s not created", slug)
	edges := activeEdgesFor(t, f.store, agent.ID)
	require.Len(t, edges, 1)
	return agent, edges[0]
}

func (f *schedFire) assertNoChild(t *testing.T, slug string) {
	t.Helper()
	_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "no scheduled child %s", slug)
}

// sessionAgent creates an agent with the creator's session and returns it.
func (f *schedFire) sessionAgent(t *testing.T, slug string) *store.Agent {
	t.Helper()
	a, _ := f.createdAgent(t, f.create(t, authUser(f.creator), CreateAgentRequest{Name: slug, AgentRole: string(AgentRoleFull)}), slug)
	return a
}

// storedUAT stores a live access token for the creator holding selectors
// and returns it.
func (f *schedFire) storedUAT(t *testing.T, selectors ...string) *store.UserAccessToken {
	t.Helper()
	return f.storedUATExpiring(t, time.Now().Add(24*time.Hour), selectors...)
}

// storedUATExpiring is storedUAT with an explicit expiry.
func (f *schedFire) storedUATExpiring(t *testing.T, exp time.Time, selectors ...string) *store.UserAccessToken {
	t.Helper()
	c := uatCeilingFromSelectors(t, selectors...)
	tok := &store.UserAccessToken{
		ID:                   api.NewUUID(),
		UserID:               f.creator.ID,
		Name:                 "sched-uat",
		Prefix:               "scion_pat_x",
		KeyHash:              api.NewUUID(),
		BoundaryKind:         string(permissions.BoundaryKindProject),
		ProjectID:            f.proj.ID,
		Scopes:               selectors,
		CeilingVersion:       permissions.CeilingVersionV1,
		CeilingPermissionIDs: c.PermissionIDs,
		ExpiresAt:            &exp,
		Created:              time.Now(),
	}
	require.NoError(t, f.store.CreateUserAccessToken(context.Background(), tok))
	return tok
}

// withUATRevision returns evt carrying the revision a create by tok writes:
// uat attribution and the token's frozen ceiling.
func withUATRevision(evt store.ScheduledEvent, tok *store.UserAccessToken) store.ScheduledEvent {
	evt.InitiatorAttribution = store.InitiatorAttribution{
		InitiatorPrincipalKind:  store.DelegationPrincipalUser,
		InitiatorPrincipalID:    tok.UserID,
		InitiatorCredentialKind: store.InitiatorCredentialKindUAT,
		InitiatorCredentialID:   tok.ID,
		AttributionVersion:      1,
		AuthorizationRevision:   3,
	}
	evt.AuthorityCeiling = store.EffectCeiling{
		Kind:              store.EffectCeilingBounded,
		Version:           tok.CeilingVersion,
		PermissionIDs:     append([]string(nil), tok.CeilingPermissionIDs...),
		BoundaryKind:      tok.BoundaryKind,
		BoundaryProjectID: tok.ProjectID,
		SourceExpiresAt:   tok.ExpiresAt,
	}
	return evt
}

// withDevLocalRevision returns evt carrying a dev_local revision for
// principalID with the principal ceiling, recorded with the "dev" identity
// kind as authoring by the local development user records it.
func withDevLocalRevision(evt store.ScheduledEvent, principalID string) store.ScheduledEvent {
	evt.InitiatorAttribution = store.InitiatorAttribution{
		InitiatorPrincipalKind:  string(PrincipalKindDev),
		InitiatorPrincipalID:    principalID,
		InitiatorCredentialKind: store.InitiatorCredentialKindDevLocal,
		AttributionVersion:      1,
		AuthorizationRevision:   1,
	}
	evt.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	return evt
}

// seedDevLocalUser stores the local development user with a project role
// and turns local development authority on or off.
func (f *schedFire) seedDevLocalUser(t *testing.T, status string, enabled bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.GetUser(ctx, DevUserID); errors.Is(err, store.ErrNotFound) {
		require.NoError(t, f.store.CreateUser(ctx, &store.User{ID: DevUserID, Email: "dev@localhost", DisplayName: "Dev", Role: store.UserRoleMember, Status: status, Created: time.Now()}))
		grantFixtureRole(t, f.bypassAgentsFixture, DevUserID, store.ProjectRoleMember)
	} else {
		u, err := f.store.GetUser(ctx, DevUserID)
		require.NoError(t, err)
		u.Status = status
		require.NoError(t, f.store.UpdateUser(ctx, u))
	}
	f.srv.authzService.setDevLocalAuthorityEnabled(enabled)
}

func assertSchedulerProvenance(t *testing.T, evt store.ScheduledEvent, edge *store.DelegationEdge, principalKind, principalID string) {
	t.Helper()
	assert.Equal(t, principalKind, edge.DelegatorType)
	assert.Equal(t, principalID, edge.DelegatorID)
	assert.Equal(t, store.ProvenanceVersionV1, edge.ProvenanceVersion)
	assert.Equal(t, store.SourceCredentialScheduler, edge.SourceCredentialKind)
	assert.Equal(t, principalKind, edge.SourcePrincipalKind)
	assert.Equal(t, principalID, edge.SourcePrincipalID)
	assert.Empty(t, edge.SourceCredentialID)
	assert.Equal(t, evt.ID, edge.SourceEventID)
	assert.Equal(t, evt.ScheduleID, edge.SourceScheduleID)
	assert.Equal(t, evt.AuthorizationRevision, edge.SourceAuthorizationRevision)
	assert.Equal(t, evt.InitiatorPrincipalKind, edge.InitiatorPrincipalKind)
	assert.Equal(t, evt.InitiatorPrincipalID, edge.InitiatorPrincipalID)
	assert.Equal(t, evt.InitiatorCredentialKind, edge.InitiatorCredentialKind)
	assert.Equal(t, evt.InitiatorCredentialID, edge.InitiatorCredentialID)
}

// --- user and agent creators -----------------------------------------------

// A session revision fires as its user: the child's edge is the typed user
// delegator with scheduler provenance and the principal ceiling, and the
// child keeps role none with NoAuth.
func TestSchedUserCreator(t *testing.T) {
	f := newSchedFire(t, "sched-user")
	evt := withSessionRevision(f.event("sched-user-child"), f.creator.ID)
	require.NoError(t, f.fire(t, evt))

	child, edge := f.child(t, "sched-user-child")
	assertSchedulerProvenance(t, evt, edge, store.DelegationPrincipalUser, f.creator.ID)
	assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, edge.EffectCeiling)
	assert.Equal(t, string(AgentRoleNone), child.AppliedConfig.AgentRole)
	assert.Equal(t, string(AgentRoleNone), edge.Role)
	assert.True(t, child.AppliedConfig.NoAuth)
	assert.Equal(t, f.creator.Email, child.AppliedConfig.CreatorName)

	audits := agentAudits(t, f.store, mutationTypeAgentDelegation, child.ID)
	require.Len(t, audits, 1)
	assert.Equal(t, "user", audits[0].ActorPrincipalKind)
	assert.Equal(t, f.creator.ID, audits[0].ActorPrincipalID)
}

// After user B re-authorizes a schedule created by A, the fire runs as B: A's
// later loss of authority does not affect it, B's loss does.
func TestSchedFireUsesRevisionPrincipalNotCreatedBy(t *testing.T) {
	f := newSchedFire(t, "sched-rev")
	ctx := context.Background()
	b := hubMemberUser(t, f.store, "sched-rev-b")
	grantFixtureRole(t, f.bypassAgentsFixture, b.ID, store.ProjectRoleMember)

	evt := withSessionRevision(f.event("sched-rev-1"), b.ID)
	require.Equal(t, f.creator.ID, evt.CreatedBy)
	require.NoError(t, f.fire(t, evt))
	child, edge := f.child(t, "sched-rev-1")
	assert.Equal(t, b.ID, edge.DelegatorID)
	assert.Equal(t, b.ID, child.CreatedBy, "the child's creator is the revision principal")

	// A (the history creator) is deleted: the fire runs as B.
	require.NoError(t, f.store.DeleteUser(ctx, f.creator.ID))
	evt2 := withSessionRevision(f.event("sched-rev-2"), b.ID)
	require.NoError(t, f.fire(t, evt2))
	child, edge = f.child(t, "sched-rev-2")
	assert.Equal(t, b.ID, edge.DelegatorID)
	assert.Equal(t, b.ID, child.CreatedBy)

	// B loses authority: the fire is denied.
	b.Status = store.UserStatusSuspended
	require.NoError(t, f.store.UpdateUser(ctx, b))
	evt3 := withSessionRevision(f.event("sched-rev-3"), b.ID)
	require.Error(t, f.fire(t, evt3))
	f.assertNoChild(t, "sched-rev-3")
}

// A revision principal without current project admission is denied.
func TestSchedUserCreatorWithoutAdmissionDenied(t *testing.T) {
	f := newSchedFire(t, "sched-noadm")
	outsider := hubMemberUser(t, f.store, "sched-noadm-outsider")
	err := f.fire(t, withSessionRevision(f.event("sched-noadm-child"), outsider.ID))
	require.Error(t, err)
	assert.ErrorIs(t, err, errScheduledAuthorityDenied)
	f.assertNoChild(t, "sched-noadm-child")
}

// An agent-principal revision evaluates the agent's chain at fire time: it
// fires while the agent's delegating user is live and is denied once that
// user is gone. The child's edge names the agent as typed delegator.
func TestSchedAgentCreatorChainEvaluated(t *testing.T) {
	f := newSchedFire(t, "sched-agent")
	markEdgeBackfillComplete(t, f.store)
	parent := f.sessionAgent(t, "sched-agent-p")

	evt := withAgentRevision(t, f.srv, f.event("sched-agent-c1"), parent.ID)
	require.NoError(t, f.fire(t, evt))
	child, edge := f.child(t, "sched-agent-c1")
	assertSchedulerProvenance(t, evt, edge, store.DelegationPrincipalAgent, parent.ID)
	assert.Equal(t, parent.Name, child.AppliedConfig.CreatorName)

	evt2 := withAgentRevision(t, f.srv, f.event("sched-agent-c2"), parent.ID)
	require.NoError(t, f.store.DeleteUser(context.Background(), f.creator.ID))
	require.Error(t, f.fire(t, evt2))
	f.assertNoChild(t, "sched-agent-c2")
}

// The agent principal's fire identity carries the agent's stored ancestry.
func TestSchedAgentPrincipalCarriesStoredAncestry(t *testing.T) {
	f := newSchedFire(t, "sched-anc")
	parent := f.sessionAgent(t, "sched-anc-p")
	require.NotEmpty(t, parent.Ancestry)

	_, identity, err := f.srv.resolveScheduledAuthority(context.Background(), withAgentRevision(t, f.srv, f.event("sched-anc-c"), parent.ID))
	require.NoError(t, err)
	agentIdent, ok := identity.(*agentIdentityWrapper)
	require.True(t, ok)
	assert.Equal(t, parent.Ancestry, agentIdent.AgentTokenClaims.Ancestry)
	want, err := f.srv.authzService.ceilingFilteredAgentScopes(context.Background(), parent, f.srv.authzService.mintCandidateScopes(parent))
	require.NoError(t, err)
	assert.Equal(t, want, agentIdent.AgentTokenClaims.Scopes)
}

// The scheduled child's ancestry stays empty (the edge is the record of who
// authorized it), and no new agent fields are set from the revision.
func TestSchedChildAncestryEmpty(t *testing.T) {
	f := newSchedFire(t, "sched-anc-empty")
	require.NoError(t, f.fire(t, withSessionRevision(f.event("sched-anc-empty-c"), f.creator.ID)))
	child, _ := f.child(t, "sched-anc-empty-c")
	assert.Empty(t, child.Ancestry)
}

// The child gets no user-material standing from its scheduled provenance:
// its ancestry is empty, so no user appears as its progeny root.
func TestScheduledChildNoUserMaterial(t *testing.T) {
	f := newSchedFire(t, "sched-nomat")
	parent := f.sessionAgent(t, "sched-nomat-p")
	require.NoError(t, f.fire(t, withAgentRevision(t, f.srv, f.event("sched-nomat-c"), parent.ID)))
	child, _ := f.child(t, "sched-nomat-c")
	assert.Empty(t, child.Ancestry)
	assert.NotContains(t, child.Ancestry, f.creator.ID)
	assert.True(t, child.AppliedConfig.NoAuth)
}

// --- UAT revisions -----------------------------------------------------------

// A UAT-authored dispatch_agent fire creates a child whose edge ceiling is
// the revision ceiling.
func TestSchedUATAuthoredFireBoundedByCeiling(t *testing.T) {
	f := newSchedFire(t, "sched-uat")
	tok := f.storedUAT(t, minimalSelectors(t)...)
	evt := withUATRevision(f.event("sched-uat-c"), tok)
	require.NoError(t, f.fire(t, evt))

	_, edge := f.child(t, "sched-uat-c")
	assertSchedulerProvenance(t, evt, edge, store.DelegationPrincipalUser, f.creator.ID)
	assert.Equal(t, store.EffectCeilingBounded, edge.Kind)
	assert.Equal(t, evt.AuthorityCeiling.Version, edge.Version)
	assert.Equal(t, evt.AuthorityCeiling.PermissionIDs, edge.PermissionIDs)
	assert.Equal(t, evt.AuthorityCeiling.BoundaryProjectID, edge.BoundaryProjectID)
}

// A UAT-sourced revision requires the token itself to be live at fire time:
// a revoked or expired token denies, and the fired event records failed.
func TestSchedUATRevokedOrExpiredDeniesFire(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		token func(*schedFire) *store.UserAccessToken
	}{
		{"revoked", func(f *schedFire) *store.UserAccessToken {
			tok := f.storedUAT(t, minimalSelectors(t)...)
			require.NoError(t, f.store.RevokeUserAccessToken(ctx, tok.ID))
			return tok
		}},
		{"expired", func(f *schedFire) *store.UserAccessToken {
			return f.storedUATExpiring(t, time.Now().Add(-time.Minute), minimalSelectors(t)...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFire(t, "sched-uat-"+tc.name)
			tok := tc.token(f)
			evt := withUATRevision(f.event("sched-uat-"+tc.name+"-c"), tok)

			err := f.fire(t, evt)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), tc.name)
			f.assertNoChild(t, "sched-uat-"+tc.name+"-c")

			// Through the recurring path the event records failed.
			sched := &store.Schedule{
				ID: api.NewUUID(), ProjectID: f.proj.ID, Name: "uat-" + tc.name, CronExpr: "0 * * * *",
				EventType: "dispatch_agent", Payload: `{"agentName":"sched-uat-` + tc.name + `-r"}`,
				Status: store.ScheduleStatusActive, CreatedBy: f.creator.ID,
				InitiatorAttribution: evt.InitiatorAttribution, AuthorityCeiling: evt.AuthorityCeiling,
			}
			require.NoError(t, f.store.CreateSchedule(ctx, sched))
			f.srv.scheduler = NewScheduler(f.store, slog.Default())
			f.srv.scheduler.RegisterEventHandler("dispatch_agent", f.srv.dispatchAgentEventHandler())
			f.srv.executeSchedule(ctx, *sched, time.Now())
			res, err := f.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{ScheduleID: sched.ID}, store.ListOptions{})
			require.NoError(t, err)
			require.Len(t, res.Items, 1)
			assert.Equal(t, store.ScheduledEventFailed, res.Items[0].Status)
			assert.Contains(t, res.Items[0].Error, "schedule authority denied")
			f.assertNoChild(t, "sched-uat-"+tc.name+"-r")
		})
	}
}

// --- recorded-revision rules -------------------------------------------------

// An event materialized before a re-authorization fires under its own
// snapshot, not the schedule's newer revision.
func TestSchedEventKeepsSnapshot(t *testing.T) {
	f := newSchedFire(t, "sched-snap")
	ctx := context.Background()
	b := hubMemberUser(t, f.store, "sched-snap-b")
	grantFixtureRole(t, f.bypassAgentsFixture, b.ID, store.ProjectRoleMember)

	sched := &store.Schedule{
		ID: api.NewUUID(), ProjectID: f.proj.ID, Name: "snap", CronExpr: "0 * * * *",
		EventType: "dispatch_agent", Payload: `{"agentName":"sched-snap-c"}`,
		Status: store.ScheduleStatusActive, CreatedBy: f.creator.ID,
	}
	snap := withSessionRevision(store.ScheduledEvent{}, f.creator.ID)
	sched.InitiatorAttribution, sched.AuthorityCeiling = snap.InitiatorAttribution, snap.AuthorityCeiling
	require.NoError(t, f.store.CreateSchedule(ctx, sched))

	// Materialize the event from the schedule's current revision.
	evt := f.event("sched-snap-c")
	evt.ScheduleID = sched.ID
	evt.InitiatorAttribution, evt.AuthorityCeiling = sched.InitiatorAttribution, sched.AuthorityCeiling

	// B re-authorizes the schedule afterwards.
	newer := withSessionRevision(store.ScheduledEvent{}, b.ID)
	attr := newer.InitiatorAttribution
	attr.AuthorizationRevision = 2
	sched.AuthorityCeiling = newer.AuthorityCeiling
	require.NoError(t, f.store.UpdateSchedule(ctx, sched, store.ScheduleFieldMask{}, 1, true, &attr))

	require.NoError(t, f.fire(t, evt))
	_, edge := f.child(t, "sched-snap-c")
	assert.Equal(t, f.creator.ID, edge.DelegatorID)
	assert.Equal(t, 1, edge.SourceAuthorizationRevision)
}

// A legacy_unknown dispatch_agent event fails with no child; CreatedBy is
// never a fallback.
func TestSchedLegacyDispatchAgentDenied(t *testing.T) {
	f := newSchedFire(t, "sched-legacy")
	evt := f.event("sched-legacy-c")
	evt.InitiatorAttribution = store.InitiatorAttribution{InitiatorCredentialKind: store.InitiatorCredentialKindLegacyUnknown}
	err := f.fire(t, evt)
	require.ErrorIs(t, err, errScheduledAuthorityUnrecorded)
	assert.Contains(t, err.Error(), "re-save the schedule")
	f.assertNoChild(t, "sched-legacy-c")
}

// A recorded attribution with an unrecorded ceiling denies, for a one-shot
// event and for a recurring schedule's fire, whose event records failed.
func TestSchedRevisionWithoutCeilingDenied(t *testing.T) {
	f := newSchedFire(t, "sched-noceil")
	ctx := context.Background()
	evt := withSessionRevision(f.event("sched-noceil-c"), f.creator.ID)
	evt.AuthorityCeiling = store.EffectCeiling{}
	require.ErrorIs(t, f.fire(t, evt), errScheduledAuthorityUnrecorded)
	f.assertNoChild(t, "sched-noceil-c")

	// A ceiling of a kind the credential cannot produce also denies.
	evt = withSessionRevision(f.event("sched-noceil-b"), f.creator.ID)
	evt.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: []string{"agent.create"}}
	require.ErrorIs(t, f.fire(t, evt), errScheduledAuthorityUnrecorded)
	f.assertNoChild(t, "sched-noceil-b")

	sched := &store.Schedule{
		ID: api.NewUUID(), ProjectID: f.proj.ID, Name: "noceil", CronExpr: "0 * * * *",
		EventType: "dispatch_agent", Payload: `{"agentName":"sched-noceil-r"}`,
		Status: store.ScheduleStatusActive, CreatedBy: f.creator.ID,
		InitiatorAttribution: withSessionRevision(store.ScheduledEvent{}, f.creator.ID).InitiatorAttribution,
	}
	require.NoError(t, f.store.CreateSchedule(ctx, sched))
	f.srv.scheduler = NewScheduler(f.store, slog.Default())
	f.srv.scheduler.RegisterEventHandler("dispatch_agent", f.srv.dispatchAgentEventHandler())
	f.srv.executeSchedule(ctx, *sched, time.Now())
	res, err := f.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{ScheduleID: sched.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, store.ScheduledEventFailed, res.Items[0].Status)
	assert.Contains(t, res.Items[0].Error, "schedule authority not recorded")
	f.assertNoChild(t, "sched-noceil-r")
}

// --- dev_local revisions -----------------------------------------------------

// A dev_local revision fires as the seeded dev user with the principal
// ceiling while local development authority is on; a suspended dev user or
// a dev_local row naming another principal denies.
func TestSchedDevLocalRevisionFires(t *testing.T) {
	f := newSchedFire(t, "sched-dev")
	f.seedDevLocalUser(t, store.UserStatusActive, true)

	evt := withDevLocalRevision(f.event("sched-dev-c"), DevUserID)
	require.NoError(t, f.fire(t, evt))
	_, edge := f.child(t, "sched-dev-c")
	assertSchedulerProvenance(t, evt, edge, store.DelegationPrincipalUser, DevUserID)
	assert.Equal(t, store.InitiatorCredentialKindDevLocal, edge.InitiatorCredentialKind)
	assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, edge.EffectCeiling)

	t.Run("other principal", func(t *testing.T) {
		err := f.fire(t, withDevLocalRevision(f.event("sched-dev-other"), f.creator.ID))
		require.ErrorIs(t, err, errScheduledAuthorityDenied)
		f.assertNoChild(t, "sched-dev-other")
	})
	t.Run("dev_local row with a user principal kind", func(t *testing.T) {
		e := withDevLocalRevision(f.event("sched-dev-userkind"), DevUserID)
		e.InitiatorPrincipalKind = store.DelegationPrincipalUser
		require.ErrorIs(t, f.fire(t, e), errScheduledAuthorityDenied)
		f.assertNoChild(t, "sched-dev-userkind")
	})
	t.Run("suspended dev user", func(t *testing.T) {
		f.seedDevLocalUser(t, store.UserStatusSuspended, true)
		err := f.fire(t, withDevLocalRevision(f.event("sched-dev-susp"), DevUserID))
		require.ErrorIs(t, err, errScheduledAuthorityDenied)
		f.assertNoChild(t, "sched-dev-susp")
	})
}

// With local development authority off, a dev_local revision fails with the
// neutral reason and no child.
func TestSchedDevLocalRevisionDeniedWhenDevAuthDisabled(t *testing.T) {
	f := newSchedFire(t, "sched-dev-off")
	f.seedDevLocalUser(t, store.UserStatusActive, false)
	err := f.fire(t, withDevLocalRevision(f.event("sched-dev-off-c"), DevUserID))
	require.ErrorIs(t, err, errScheduledAuthorityDenied)
	assert.Contains(t, err.Error(), reasonDevLocalDisabled)
	f.assertNoChild(t, "sched-dev-off-c")
}

// A legacy_unknown row whose descriptive principal kind is dev denies:
// nothing is inferred from descriptive fields.
func TestSchedLegacyUnknownDevRowDenied(t *testing.T) {
	f := newSchedFire(t, "sched-dev-legacy")
	f.seedDevLocalUser(t, store.UserStatusActive, true)
	evt := f.event("sched-dev-legacy-c")
	evt.InitiatorAttribution = store.InitiatorAttribution{
		InitiatorPrincipalKind:  "dev",
		InitiatorPrincipalID:    DevUserID,
		InitiatorCredentialKind: store.InitiatorCredentialKindLegacyUnknown,
		AttributionVersion:      1,
	}
	evt.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	require.ErrorIs(t, f.fire(t, evt), errScheduledAuthorityUnrecorded)
	f.assertNoChild(t, "sched-dev-legacy-c")

	evt.InitiatorAttribution.AttributionVersion = 0
	evt.InitiatorAttribution.InitiatorCredentialKind = store.InitiatorCredentialKindDevLocal
	require.ErrorIs(t, f.fire(t, evt), errScheduledAuthorityUnrecorded)
	f.assertNoChild(t, "sched-dev-legacy-c")
}

// --- the scheduled child's edge ----------------------------------------------

// edgeWriteFailingStore fails CreateDelegationEdge, including inside WithTx.
type edgeWriteFailingStore struct {
	store.Store
}

func (s *edgeWriteFailingStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error { return fn(&edgeWriteFailingStore{Store: tx}) })
}

func (s *edgeWriteFailingStore) CreateDelegationEdge(context.Context, *store.DelegationEdge) error {
	return errors.New("injected delegation edge write fault")
}

// The child's row, identity key, edge and audit commit in one transaction:
// an edge-write failure fails the fire and leaves none of them.
func TestSchedChildEdgeInSameTx(t *testing.T) {
	f := newSchedFire(t, "sched-tx")
	f.srv.store = &edgeWriteFailingStore{Store: f.store}
	err := f.fire(t, withSessionRevision(f.event("sched-tx-c"), f.creator.ID))
	f.srv.store = f.store
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected delegation edge write fault")
	f.assertNoChild(t, "sched-tx-c")
	recs, _, err := f.store.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentDelegation})
	require.NoError(t, err)
	assert.Empty(t, recs, "no create audit record")
	keys, err := f.store.ListAgentIdentityKeys(context.Background(), f.proj.ID)
	require.NoError(t, err)
	for _, k := range keys {
		assert.NotEqual(t, "sched-tx-c", k.Key, "no identity key")
	}
}

// The edge delegator is the revision's typed principal, with no probe: a
// user whose ID collides with an agent's ID is recorded as a user.
func TestSchedChildEdgeTypedDelegator(t *testing.T) {
	f := newSchedFire(t, "sched-typed")
	other := f.sessionAgent(t, "sched-typed-agent")
	collide := &store.User{ID: other.ID, Email: "collide@example.com", DisplayName: "Collide", Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
	require.NoError(t, f.store.CreateUser(context.Background(), collide))
	grantFixtureRole(t, f.bypassAgentsFixture, collide.ID, store.ProjectRoleMember)

	require.NoError(t, f.fire(t, withSessionRevision(f.event("sched-typed-c"), collide.ID)))
	_, edge := f.child(t, "sched-typed-c")
	assert.Equal(t, store.DelegationPrincipalUser, edge.DelegatorType)
	assert.Equal(t, other.ID, edge.DelegatorID)
}

// For each revision credential the child's edge records scheduler
// provenance, the event, schedule and revision references, the initiator
// fields verbatim, and the frozen revision ceiling (for an agent principal,
// intersected with the agent's write ceiling at fire time), never a live UAT
// or agent ceiling on its own.
func TestSchedChildEdgeProvenanceFields(t *testing.T) {
	t.Run("session", func(t *testing.T) {
		f := newSchedFire(t, "sched-pf-session")
		evt := withSessionRevision(f.event("sched-pf-session-c"), f.creator.ID)
		require.NoError(t, f.fire(t, evt))
		_, edge := f.child(t, "sched-pf-session-c")
		assertSchedulerProvenance(t, evt, edge, store.DelegationPrincipalUser, f.creator.ID)
		assert.Equal(t, store.EffectCeilingPrincipal, edge.Kind)
	})
	t.Run("dev_local", func(t *testing.T) {
		f := newSchedFire(t, "sched-pf-dev")
		f.seedDevLocalUser(t, store.UserStatusActive, true)
		evt := withDevLocalRevision(f.event("sched-pf-dev-c"), DevUserID)
		require.NoError(t, f.fire(t, evt))
		_, edge := f.child(t, "sched-pf-dev-c")
		assertSchedulerProvenance(t, evt, edge, store.DelegationPrincipalUser, DevUserID)
		assert.Equal(t, store.EffectCeilingPrincipal, edge.Kind)
	})
	t.Run("uat", func(t *testing.T) {
		f := newSchedFire(t, "sched-pf-uat")
		// The live token holds more than the revision recorded: the edge
		// carries the frozen revision ceiling, not the live one.
		tok := f.storedUAT(t, append(minimalSelectors(t), "agent:lifecycle")...)
		evt := withUATRevision(f.event("sched-pf-uat-c"), tok)
		evt.AuthorityCeiling.PermissionIDs = uatCeilingFromSelectors(t, minimalSelectors(t)...).PermissionIDs
		require.NoError(t, f.fire(t, evt))
		_, edge := f.child(t, "sched-pf-uat-c")
		assertSchedulerProvenance(t, evt, edge, store.DelegationPrincipalUser, f.creator.ID)
		assert.Equal(t, evt.AuthorityCeiling.PermissionIDs, edge.PermissionIDs)
		assert.NotContains(t, edge.PermissionIDs, "agent.lifecycle")
	})
	t.Run("agent", func(t *testing.T) {
		f := newSchedFire(t, "sched-pf-agent")
		parent := f.sessionAgent(t, "sched-pf-agent-p")
		evt := withAgentRevision(t, f.srv, f.event("sched-pf-agent-c"), parent.ID)
		// The revision recorded a narrower ceiling than the agent's live
		// write ceiling: the edge keeps only what both allow.
		evt.AuthorityCeiling.PermissionIDs = []string{"agent.create", "project.read"}
		require.NoError(t, f.fire(t, evt))
		_, edge := f.child(t, "sched-pf-agent-c")
		assertSchedulerProvenance(t, evt, edge, store.DelegationPrincipalAgent, parent.ID)
		assert.Equal(t, store.EffectCeilingBounded, edge.Kind)
		assert.Equal(t, permissions.CeilingVersionV1, edge.Version)
		assert.Equal(t, []string{"agent.create", "project.read"}, edge.PermissionIDs)
	})
}

// The scheduled child's edge records provenance the same way the
// request create path does for the same user, apart from the scheduler
// credential and its references.
func TestSchedChildProvenanceParityWithRequestCreate(t *testing.T) {
	f := newSchedFire(t, "sched-parity")
	_, direct := f.createdAgent(t, f.create(t, authUser(f.creator), CreateAgentRequest{Name: "sched-parity-i", AgentRole: string(AgentRoleNone)}), "sched-parity-i")

	evt := withSessionRevision(f.event("sched-parity-s"), f.creator.ID)
	require.NoError(t, f.fire(t, evt))
	_, scheduled := f.child(t, "sched-parity-s")

	assert.Equal(t, direct.DelegatorType, scheduled.DelegatorType)
	assert.Equal(t, direct.DelegatorID, scheduled.DelegatorID)
	assert.Equal(t, direct.DelegateType, scheduled.DelegateType)
	assert.Equal(t, direct.ScopeType, scheduled.ScopeType)
	assert.Equal(t, direct.ScopeID, scheduled.ScopeID)
	assert.Equal(t, direct.Role, scheduled.Role)
	assert.Equal(t, direct.Active, scheduled.Active)
	assert.Equal(t, direct.EffectCeiling, scheduled.EffectCeiling)
	assert.Equal(t, direct.ProvenanceVersion, scheduled.ProvenanceVersion)
	assert.Equal(t, direct.SourcePrincipalKind, scheduled.SourcePrincipalKind)
	assert.Equal(t, direct.SourcePrincipalID, scheduled.SourcePrincipalID)
	assert.Equal(t, store.SourceCredentialSession, direct.SourceCredentialKind)
	assert.Equal(t, store.SourceCredentialScheduler, scheduled.SourceCredentialKind)
	assert.Equal(t, evt.ID, scheduled.SourceEventID)
}

// An agent-authored revision of an agent with a principal chain carries the
// three deliver IDs, and so does the child's edge. Once the agent's own
// edge is replaced by a bounded edge without them (the shape a token
// reincarnation records), the next fire's child carries none.
func TestSchedAgentDeliverIDsIntersect(t *testing.T) {
	f := newSchedFire(t, "sched-deliver")
	ctx := context.Background()
	parent := f.sessionAgent(t, "sched-deliver-p")
	evt := withAgentRevision(t, f.srv, f.event("sched-deliver-c1"), parent.ID)
	for _, d := range hubDeliveryPermissionList {
		require.Contains(t, evt.AuthorityCeiling.PermissionIDs, d)
	}
	require.NoError(t, f.fire(t, evt))
	_, edge := f.child(t, "sched-deliver-c1")
	for _, d := range hubDeliveryPermissionList {
		assert.Contains(t, edge.PermissionIDs, d)
	}

	// Replace P's edge with a UAT-sourced bounded edge (no deliver ID).
	now := time.Now()
	_, err := f.store.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, parent.ID,
		store.Deactivation{Cause: store.EdgeDeactivationReincarnateReplaced, At: &now, OpID: api.NewUUID()})
	require.NoError(t, err)
	// A bounded edge without any deliver ID, covering the agent's stored
	// role otherwise (store-seeded; the shape a reincarnation by a token
	// records, since no token selector covers a deliver permission).
	var noDeliver []string
	for _, p := range permissions.Registry {
		if !containsString(hubDeliveryPermissionList, p.ID) {
			noDeliver = append(noDeliver, p.ID)
		}
	}
	require.NoError(t, f.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.creator.ID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: parent.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.proj.ID, Role: parent.AppliedConfig.AgentRole, Active: true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion: store.ProvenanceVersionV1, SourcePrincipalKind: store.DelegationPrincipalUser,
			SourcePrincipalID: f.creator.ID, SourceCredentialKind: store.SourceCredentialUAT, SourceCredentialID: "uat-reinc",
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: sortedUniqueIDs(noDeliver)},
	}))

	// The revision recorded earlier holds the deliver IDs; the fire
	// intersects it with P's current chain, which holds none.
	evt2 := evt
	evt2.ID = api.NewUUID()
	evt2.Payload = `{"agentName":"sched-deliver-c2"}`
	require.NoError(t, f.fire(t, evt2), "guard: the fire creates the child")
	_, edge2 := f.child(t, "sched-deliver-c2")
	for _, d := range hubDeliveryPermissionList {
		assert.NotContains(t, edge2.PermissionIDs, d)
	}
	assert.Contains(t, edge2.PermissionIDs, "agent.create")
}

// --- dispatch failure ----------------------------------------------------------

// A scheduled child whose dispatch fails is compensated: the broker delete
// is called, the agent row deleted, its edge deactivated with
// create_compensation, and an agent_create_dispatch_failed record written.
func TestSchedDispatchFailureCompensates(t *testing.T) {
	f := newSchedFire(t, "sched-dispfail")
	client := f.withDispatcher(t)
	client.returnErr = errors.New("broker unavailable")

	err := f.fire(t, withSessionRevision(f.event("sched-dispfail-c"), f.creator.ID))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker unavailable")
	f.assertNoChild(t, "sched-dispfail-c")
	require.NotNil(t, client.lastCreateReq)
	childID := client.lastCreateReq.ID
	require.NotEmpty(t, childID)
	assert.True(t, client.deleteCalled, "broker delete called for the dispatched create")
	sum := assertCompensated(t, f.store, childID)
	assert.Equal(t, createStageDispatch, sum.Stage)
	assert.Contains(t, sum.Error, "broker unavailable")
}

// --- fire-time writes never use the request ceiling ---------------------------

// The fire path takes its ceiling and provenance from scheduledEffectCeiling:
// none of its functions calls sourceEffectCeiling.
func TestSchedFireNeverCallsSourceEffectCeiling(t *testing.T) {
	firePath := map[string][]string{
		"server.go": {"dispatchAgentEventHandler", "authorizeScheduledAgentCreate", "scheduledCreatorIdentity", "applyScheduledProjectDefaultGCPIdentity", "executeSchedule"},
		"scheduled_authority.go": {"resolveScheduledAuthority", "resolveScheduledUser", "resolveScheduledAgent",
			"scheduledEffectCeiling", "revisionCeilingConsistent", "intersectEffectCeilings"},
	}
	fset := token.NewFileSet()
	for file, funcs := range firePath {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err)
		found := map[string]bool{}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			for _, want := range funcs {
				if fn.Name.Name != want {
					continue
				}
				found[want] = true
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "sourceEffectCeiling" {
						t.Errorf("%s (%s) calls sourceEffectCeiling at %s", want, file, fset.Position(sel.Pos()))
					}
					return true
				})
			}
		}
		for _, want := range funcs {
			assert.True(t, found[want], "%s not found in %s", want, file)
		}
	}
}

// softDeletingScheduledDispatcher soft-deletes the child while its
// synchronous dispatch is in flight.
type softDeletingScheduledDispatcher struct {
	AgentDispatcher
	srv *Server
	err error
}

func (d *softDeletingScheduledDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	res, err := d.AgentDispatcher.DispatchAgentCreate(ctx, agent)
	d.err = d.srv.store.WithTx(context.Background(), func(tx store.Store) error {
		row, gerr := tx.GetAgent(context.Background(), agent.ID)
		if gerr != nil {
			return gerr
		}
		row.DeletedAt = time.Now()
		if uerr := tx.UpdateAgent(context.Background(), row); uerr != nil {
			return uerr
		}
		return d.srv.softDeleteAgentTx(context.Background(), tx, row, AuditActor{})
	})
	return res, err
}

// A scheduled child deleted while its synchronous dispatch was in flight
// fails the fire (the synchronous create's 409 delete_in_progress contract)
// and is left to the delete: no compensation record is written.
func TestSchedChildDeletedDuringDispatchFailsFire(t *testing.T) {
	f := newSchedFire(t, "sched-delrace")
	f.withDispatcher(t)
	disp := &softDeletingScheduledDispatcher{AgentDispatcher: f.srv.GetDispatcher(), srv: f.srv}
	f.srv.SetDispatcher(disp)

	err := f.fire(t, withSessionRevision(f.event("sched-delrace-c"), f.creator.ID))
	require.NoError(t, disp.err, "the soft delete during dispatch")
	require.ErrorIs(t, err, errScheduledChildDeletedDuringCreate)

	recs, _, lerr := f.store.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentCreateDispatchFailed})
	require.NoError(t, lerr)
	assert.Empty(t, recs, "the delete owns the record; no compensation")
}
