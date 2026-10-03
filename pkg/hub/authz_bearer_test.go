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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bearerFixture is two projects, each with its own owner, and an agent in
// each project owned by that project's owner.
type bearerFixture struct {
	srv      *Server
	store    store.Store
	projectA string
	projectB string
	ownerA   string
	ownerB   string
	agentA   *store.Agent
	agentB   *store.Agent
}

func newBearerFixture(t *testing.T, name string) bearerFixture {
	t.Helper()
	srv, s := testServer(t)
	f := bearerFixture{
		srv:      srv,
		store:    s,
		projectA: tid("bearer-" + name + "-project-a"),
		projectB: tid("bearer-" + name + "-project-b"),
		ownerA:   tid("bearer-" + name + "-owner-a"),
		ownerB:   tid("bearer-" + name + "-owner-b"),
	}
	createRS1Project(t, s, f.projectA, f.ownerA)
	createRS1Project(t, s, f.projectB, f.ownerB)
	f.agentA = uatpAgent(t, s, f.projectA, f.ownerA, name+"-a", f.ownerA)
	f.agentB = uatpAgent(t, s, f.projectB, f.ownerB, name+"-b", f.ownerB)
	return f
}

func bearerUser(id string) *AuthenticatedUser {
	return NewAuthenticatedUser(id, id+"@test.com", "User", "member", "api")
}

func bearerCeiling(t *testing.T, selectors ...string) permissions.FrozenPermissionCeiling {
	t.Helper()
	ceiling, ok := permissions.BuildCeilingFromSelectors(selectors)
	require.True(t, ok, "selectors must resolve: %v", selectors)
	return ceiling
}

func hubBoundary() TokenBoundary { return TokenBoundary{Kind: BoundaryKindHub} }

func projectBoundary(projectID string) TokenBoundary {
	return TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}
}

// TestEvaluateBearerCeiling_MatchesUATRequestDecision pins that the
// callable and a real UAT request carrying the same boundary and ceiling
// reach the same decision with the same reason, through the same gate.
func TestEvaluateBearerCeiling_MatchesUATRequestDecision(t *testing.T) {
	f := newBearerFixture(t, "equiv")
	ctx := context.Background()

	cases := []struct {
		name      string
		userID    string
		boundary  TokenBoundary
		selectors []string
		target    *store.Agent
		permID    string
		wantAllow bool
		wantStage string
	}{
		{"owner on own project with hub boundary allows", f.ownerA, hubBoundary(), []string{"agent:delete"}, f.agentA, "agent.delete", true, ""},
		{"owner on own project with project boundary allows", f.ownerA, projectBoundary(f.projectA), []string{"agent:delete"}, f.agentA, "agent.delete", true, ""},
		{"project boundary denies a target in another project", f.ownerA, projectBoundary(f.projectA), []string{"agent:delete"}, f.agentB, "agent.delete", false, BearerStageOutsideBoundary},
		{"ceiling without the permission denies", f.ownerA, hubBoundary(), []string{"agent:read"}, f.agentA, "agent.delete", false, BearerStageCeiling},
		{"hub boundary denies a project the holder cannot access", f.ownerA, hubBoundary(), []string{"agent:delete"}, f.agentB, "agent.delete", false, BearerStageProjectAccess},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ceiling := bearerCeiling(t, tc.selectors...)
			user := bearerUser(tc.userID)
			action, ok := registryActionFor(tc.permID)
			require.True(t, ok)

			scoped := NewScopedUserIdentityWithBoundaryAndDecoration(user, tc.boundary, tc.selectors, tid("bearer-cred-"+tc.name), ceiling, nil)
			viaRequest := f.srv.authzService.CheckAccess(ctx, scoped, agentResource(tc.target), action)

			eval := f.srv.authzService.EvaluateBearerCeiling(ctx, PrincipalContext{Identity: user}, tc.boundary, ceiling, tc.permID, agentResource(tc.target), BearerOptions{})

			assert.Equal(t, tc.wantAllow, viaRequest.Allowed, "UAT request: %s", viaRequest.Reason)
			assert.Equal(t, viaRequest.Allowed, eval.Decision.Allowed, "callable and UAT request must agree: %s / %s", viaRequest.Reason, eval.Decision.Reason)
			assert.Equal(t, viaRequest.Reason, eval.Decision.Reason)
			assert.Equal(t, tc.wantStage, eval.Stage)
		})
	}
}

// TestEvaluateBearerCeiling_LiveAuthorityDeniesAfterGate pins that passing
// the gate is not sufficient: the holder's live authority on the target
// still decides, and the stage reports that.
func TestEvaluateBearerCeiling_LiveAuthorityDeniesAfterGate(t *testing.T) {
	f := newBearerFixture(t, "authority")
	ctx := context.Background()
	memberID := tid("bearer-authority-member")
	uatpMember(t, f.store, f.projectA, memberID)

	eval := f.srv.authzService.EvaluateBearerCeiling(ctx, PrincipalContext{Identity: bearerUser(memberID)}, hubBoundary(), bearerCeiling(t, "agent:delete"), "agent.delete", agentResource(f.agentA), BearerOptions{})
	assert.False(t, eval.Decision.Allowed, "a member without delete authority on another member's agent must be denied: %s", eval.Decision.Reason)
	assert.Equal(t, BearerStageAuthority, eval.Stage)
	assert.Equal(t, TargetScope{Kind: TargetScopeProject, ProjectID: f.projectA}, eval.TargetScope)
	assert.Equal(t, ProjectAccessSourceMembership, eval.AccessSource)
}

// TestEvaluateBearerCeiling_RequiresLocalUserPrincipal pins that only a
// local interactive user principal is evaluated; every other principal is
// denied before the gate.
func TestEvaluateBearerCeiling_RequiresLocalUserPrincipal(t *testing.T) {
	f := newBearerFixture(t, "principal")
	ctx := context.Background()
	ceiling := bearerCeiling(t, "agent:delete")
	scoped := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(f.ownerA), hubBoundary(), []string{"agent:delete"}, tid("bearer-principal-cred"), ceiling, nil)

	cases := []struct {
		name     string
		identity Identity
	}{
		{"nil identity", nil},
		{"typed-nil user", (*AuthenticatedUser)(nil)},
		{"UAT-backed identity", scoped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eval := f.srv.authzService.EvaluateBearerCeiling(ctx, PrincipalContext{Identity: tc.identity}, hubBoundary(), ceiling, "agent.delete", agentResource(f.agentA), BearerOptions{})
			assert.False(t, eval.Decision.Allowed)
			assert.Equal(t, BearerStageError, eval.Stage)
		})
	}
}

// TestEvaluateBearerCeiling_RejectsInvalidInputs pins that an invalid
// boundary, an unresolvable target, and an empty or unknown permission ID
// each deny with the stage that names the failing input.
func TestEvaluateBearerCeiling_RejectsInvalidInputs(t *testing.T) {
	f := newBearerFixture(t, "inputs")
	ctx := context.Background()
	user := PrincipalContext{Identity: bearerUser(f.ownerA)}
	ceiling := bearerCeiling(t, "agent:delete")
	target := agentResource(f.agentA)

	cases := []struct {
		name      string
		boundary  TokenBoundary
		permID    string
		target    Resource
		wantStage string
	}{
		{"empty boundary kind", TokenBoundary{}, "agent.delete", target, BearerStageBoundaryInvalid},
		{"project boundary without project ID", TokenBoundary{Kind: BoundaryKindProject}, "agent.delete", target, BearerStageBoundaryInvalid},
		{"hub boundary carrying a project ID", TokenBoundary{Kind: BoundaryKindHub, ProjectID: f.projectA}, "agent.delete", target, BearerStageBoundaryInvalid},
		{"agent target without a parent project", hubBoundary(), "agent.delete", Resource{Type: "agent", ID: f.agentA.ID}, BearerStageTargetUnknown},
		{"empty permission ID", hubBoundary(), "", target, BearerStageError},
		{"unknown permission ID", hubBoundary(), "agent.unknown_action", target, BearerStageError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eval := f.srv.authzService.EvaluateBearerCeiling(ctx, user, tc.boundary, ceiling, tc.permID, tc.target, BearerOptions{})
			assert.False(t, eval.Decision.Allowed)
			assert.Equal(t, tc.wantStage, eval.Stage, "reason: %s", eval.Decision.Reason)
		})
	}
}

// TestEvaluateBearerCeiling_EmitsNoDecisionAudit pins that the callable
// emits no decision audit, while Decide on the same request audits exactly
// once.
func TestEvaluateBearerCeiling_EmitsNoDecisionAudit(t *testing.T) {
	f := newBearerFixture(t, "audit")
	ctx := context.Background()
	emitter := &capturingAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	user := bearerUser(f.ownerA)
	ceiling := bearerCeiling(t, "agent:delete")
	for _, target := range []*store.Agent{f.agentA, f.agentB} {
		f.srv.authzService.EvaluateBearerCeiling(ctx, PrincipalContext{Identity: user}, hubBoundary(), ceiling, "agent.delete", agentResource(target), BearerOptions{})
	}
	emitter.mu.Lock()
	assert.Empty(t, emitter.records, "EvaluateBearerCeiling must not emit decision audit")
	emitter.mu.Unlock()

	scoped := NewScopedUserIdentityWithBoundaryAndDecoration(user, hubBoundary(), []string{"agent:delete"}, tid("bearer-audit-cred"), ceiling, nil)
	b := hubBoundary()
	f.srv.authzService.Decide(ctx, AuthzRequest{
		Principal:  PrincipalContext{Identity: scoped},
		Credential: CredentialContext{Kind: CredentialKindUAT, Boundary: &b, Ceiling: ceiling},
		Resource:   agentResource(f.agentA),
		Action:     ActionDelete,
		Permission: "agent.delete",
	})
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	assert.Len(t, emitter.records, 1, "Decide must audit exactly once")
}

// TestEvaluateBearerCeiling_StoreFaultDenies pins that a store fault at the
// project access stage or in the kernel denies and is reported as an
// indeterminate error, never as an allow.
func TestEvaluateBearerCeiling_StoreFaultDenies(t *testing.T) {
	f := newBearerFixture(t, "fault")
	ctx := context.Background()
	injected := errors.New("injected store fault")

	stores := map[string]store.Store{
		"role bindings":    &failBindingsStore{Store: f.store, failErr: injected},
		"effective groups": &failEffectiveGroupsStore{Store: f.store, failErr: injected},
		"constraints":      &failConstraintsStore{Store: f.store, failErr: injected},
	}
	for name, failing := range stores {
		t.Run(name, func(t *testing.T) {
			authz := NewAuthzService(failing, slog.Default())
			eval := authz.EvaluateBearerCeiling(ctx, PrincipalContext{Identity: bearerUser(f.ownerA)}, hubBoundary(), bearerCeiling(t, "agent:delete"), "agent.delete", agentResource(f.agentA), BearerOptions{})
			assert.False(t, eval.Decision.Allowed, "a store fault must deny")
			assert.True(t, eval.Decision.IsIndeterminate(), "a store fault must be reported as indeterminate: %s", eval.Decision.Reason)
			assert.Equal(t, BearerStageError, eval.Stage)
		})
	}
}

// TestEvaluateBearerCeiling_EvaluatesExactPermissionID pins that the
// permission evaluated is exactly the one passed, with its action taken
// from the registry, and never one inferred from the target.
func TestEvaluateBearerCeiling_EvaluatesExactPermissionID(t *testing.T) {
	f := newBearerFixture(t, "exactperm")
	ctx := context.Background()
	user := PrincipalContext{Identity: bearerUser(f.ownerA)}
	ceiling := bearerCeiling(t, "agent:read")
	target := agentResource(f.agentA)

	read := f.srv.authzService.EvaluateBearerCeiling(ctx, user, hubBoundary(), ceiling, "agent.read", target, BearerOptions{})
	assert.True(t, read.Decision.Allowed, "agent.read is in the ceiling: %s", read.Decision.Reason)

	del := f.srv.authzService.EvaluateBearerCeiling(ctx, user, hubBoundary(), ceiling, "agent.delete", target, BearerOptions{})
	assert.False(t, del.Decision.Allowed)
	assert.Equal(t, BearerStageCeiling, del.Stage)
}

// TestEvaluateBearerCeiling_IgnoresContextIdentity pins that the callable
// reads only its arguments: an identity or credential carried by ctx does
// not change the result.
func TestEvaluateBearerCeiling_IgnoresContextIdentity(t *testing.T) {
	f := newBearerFixture(t, "ctxident")
	user := PrincipalContext{Identity: bearerUser(f.ownerA)}
	ceiling := bearerCeiling(t, "agent:delete")
	target := agentResource(f.agentA)

	base := f.srv.authzService.EvaluateBearerCeiling(context.Background(), user, hubBoundary(), ceiling, "agent.delete", target, BearerOptions{})
	require.True(t, base.Decision.Allowed, base.Decision.Reason)

	narrow := projectBoundary(f.projectB)
	narrowCeiling := bearerCeiling(t, "agent:read")
	ctxScoped := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(f.ownerB), narrow, []string{"agent:read"}, tid("bearer-ctxident-cred"), narrowCeiling, nil)
	ctx := contextWithIdentity(context.Background(), ctxScoped)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindUAT, Boundary: &narrow, Ceiling: narrowCeiling})

	withCtx := f.srv.authzService.EvaluateBearerCeiling(ctx, user, hubBoundary(), ceiling, "agent.delete", target, BearerOptions{})
	assert.Equal(t, base.Decision.Allowed, withCtx.Decision.Allowed, withCtx.Decision.Reason)
	assert.Equal(t, base.Stage, withCtx.Stage)
}

// TestUATGate_TargetOutsideBoundaryDenied pins that a project-bound UAT
// cannot reach a target in another project, even when the holder has full
// authority there.
func TestUATGate_TargetOutsideBoundaryDenied(t *testing.T) {
	f := newBearerFixture(t, "gate-outside")
	ctx := context.Background()
	// ownerA is also a member of project B.
	uatpMember(t, f.store, f.projectB, f.ownerA)

	scoped := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(f.ownerA), projectBoundary(f.projectA), []string{"agent:read"}, tid("bearer-gate-outside-cred"), bearerCeiling(t, "agent:read"), nil)
	decision := f.srv.authzService.CheckAccess(ctx, scoped, agentResource(f.agentB), ActionRead)
	assert.False(t, decision.Allowed)
	assert.Equal(t, bearerReasonOutsideProject, decision.Reason)
}

// TestUATGate_HubBoundaryReachesAccessibleProjects pins that a hub-bound
// UAT reaches targets in every project the holder can access.
func TestUATGate_HubBoundaryReachesAccessibleProjects(t *testing.T) {
	f := newBearerFixture(t, "gate-hub")
	ctx := context.Background()
	uatpMember(t, f.store, f.projectB, f.ownerA)

	scoped := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(f.ownerA), hubBoundary(), []string{"agent:read"}, tid("bearer-gate-hub-cred"), bearerCeiling(t, "agent:read"), nil)
	for _, agent := range []*store.Agent{f.agentA, f.agentB} {
		decision := f.srv.authzService.CheckAccess(ctx, scoped, agentResource(agent), ActionRead)
		assert.True(t, decision.Allowed, "agent %s: %s", agent.ID, decision.Reason)
	}
}

// TestUATGate_ProjectTargetRequiresCurrentAccess pins that a hub-bound UAT
// reaches a project target only while the holder has active access to that
// project, evaluated at use.
func TestUATGate_ProjectTargetRequiresCurrentAccess(t *testing.T) {
	f := newBearerFixture(t, "gate-access")
	ctx := context.Background()
	scoped := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(f.ownerA), hubBoundary(), []string{"agent:read"}, tid("bearer-gate-access-cred"), bearerCeiling(t, "agent:read"), nil)

	denied := f.srv.authzService.CheckAccess(ctx, scoped, agentResource(f.agentB), ActionRead)
	assert.False(t, denied.Allowed)
	assert.Equal(t, bearerReasonProjectAccessDenied, denied.Reason)

	uatpMember(t, f.store, f.projectB, f.ownerA)
	allowed := f.srv.authzService.CheckAccess(ctx, scoped, agentResource(f.agentB), ActionRead)
	assert.True(t, allowed.Allowed, allowed.Reason)

	uatpDeleteProjectBinding(t, f.store, f.ownerA, f.projectB)
	again := f.srv.authzService.CheckAccess(ctx, scoped, agentResource(f.agentB), ActionRead)
	assert.False(t, again.Allowed)
	assert.Equal(t, bearerReasonProjectAccessDenied, again.Reason)
}

// TestUATGate_UnresolvableTargetDenied pins that a UAT request whose target
// scope cannot be resolved is denied under every boundary.
func TestUATGate_UnresolvableTargetDenied(t *testing.T) {
	f := newBearerFixture(t, "gate-unknown")
	ctx := context.Background()
	for _, boundary := range []TokenBoundary{hubBoundary(), projectBoundary(f.projectA)} {
		scoped := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(f.ownerA), boundary, []string{"agent:read"}, tid("bearer-gate-unknown-cred-"+string(boundary.Kind)), bearerCeiling(t, "agent:read"), nil)
		decision := f.srv.authzService.CheckAccess(ctx, scoped, Resource{Type: "agent", ID: f.agentA.ID}, ActionRead)
		assert.False(t, decision.Allowed, "boundary %s", boundary.Kind)
	}
}

// gateStages are the bearer stages a request denied by the bearer gate
// reports. A request that passed the gate reports none of them.
var gateStages = []string{BearerStageBoundaryInvalid, BearerStageTargetUnknown, BearerStageOutsideBoundary, BearerStageCeiling, BearerStageProjectAccess}

func hubCollectionEvidence(permissionID string) TargetScopeEvidence {
	return TargetScopeEvidence{IsCollectionLevel: true, CollectionScope: TargetScopeHub, PermissionID: permissionID}
}

func projectCollectionEvidence(projectID, permissionID string) TargetScopeEvidence {
	return TargetScopeEvidence{IsCollectionLevel: true, CollectionScope: TargetScopeProject, CollectionProjectID: projectID, PermissionID: permissionID}
}

// TestEvaluateBearerCeiling_TargetEvidenceClassifiesCollectionRequests pins
// how collection-level evidence classifies a request that names no
// existing resource: hub evidence resolves a hub scope that a hub boundary
// allows and a project boundary does not; project evidence resolves that
// project; a project resource without an ID and without evidence is
// unresolvable.
func TestEvaluateBearerCeiling_TargetEvidenceClassifiesCollectionRequests(t *testing.T) {
	f := newBearerFixture(t, "evidence")
	ctx := context.Background()
	user := PrincipalContext{Identity: bearerUser(f.ownerA)}
	hubScope := TargetScope{Kind: TargetScopeHub}
	projectScopeA := TargetScope{Kind: TargetScopeProject, ProjectID: f.projectA}

	t.Run("hub evidence passes the boundary stage of a hub boundary", func(t *testing.T) {
		// project.create carries no token selector, so the ceiling stage
		// decides after the boundary stage has allowed the hub scope.
		eval := f.srv.authzService.EvaluateBearerCeiling(ctx, user, hubBoundary(), bearerCeiling(t, "agent:read"), "project.create", Resource{}, BearerOptions{Evidence: hubCollectionEvidence("project.create")})
		assert.False(t, eval.Decision.Allowed)
		assert.Equal(t, BearerStageCeiling, eval.Stage, eval.Decision.Reason)
		assert.Equal(t, hubScope, eval.TargetScope)
	})

	t.Run("hub evidence with the permission in the ceiling passes the gate", func(t *testing.T) {
		eval := f.srv.authzService.EvaluateBearerCeiling(ctx, user, hubBoundary(), bearerCeiling(t, "skill:list"), "skill.list", Resource{}, BearerOptions{Evidence: hubCollectionEvidence("skill.list")})
		assert.NotContains(t, gateStages, eval.Stage, eval.Decision.Reason)
		assert.Equal(t, hubScope, eval.TargetScope)
	})

	t.Run("project resource without an ID and without evidence is unresolvable", func(t *testing.T) {
		eval := f.srv.authzService.EvaluateBearerCeiling(ctx, user, hubBoundary(), bearerCeiling(t, "project:read"), "project.read", Resource{Type: "project"}, BearerOptions{})
		assert.False(t, eval.Decision.Allowed)
		assert.Equal(t, BearerStageTargetUnknown, eval.Stage, eval.Decision.Reason)
	})

	t.Run("project evidence for the token project passes the gate", func(t *testing.T) {
		target := Resource{Type: "agent", ParentType: "project", ParentID: f.projectA}
		eval := f.srv.authzService.EvaluateBearerCeiling(ctx, user, projectBoundary(f.projectA), bearerCeiling(t, "agent:list"), "agent.list", target, BearerOptions{Evidence: projectCollectionEvidence(f.projectA, "agent.list")})
		assert.NotContains(t, gateStages, eval.Stage, eval.Decision.Reason)
		assert.Equal(t, projectScopeA, eval.TargetScope)
	})

	t.Run("hub evidence under a project boundary is outside the boundary", func(t *testing.T) {
		eval := f.srv.authzService.EvaluateBearerCeiling(ctx, user, projectBoundary(f.projectA), bearerCeiling(t, "skill:list"), "skill.list", Resource{}, BearerOptions{Evidence: hubCollectionEvidence("skill.list")})
		assert.False(t, eval.Decision.Allowed)
		assert.Equal(t, BearerStageOutsideBoundary, eval.Stage, eval.Decision.Reason)
		assert.Equal(t, hubScope, eval.TargetScope)
	})
}

// TestEvaluateBearerCeiling_EvidenceMustNameEvaluatedPermission pins that
// collection-level evidence classifies a request only for the permission
// it names: evidence for any other permission denies at the target stage,
// so it can never resolve a scope for the evaluated permission.
func TestEvaluateBearerCeiling_EvidenceMustNameEvaluatedPermission(t *testing.T) {
	f := newBearerFixture(t, "evidence-perm")
	ctx := context.Background()
	user := PrincipalContext{Identity: bearerUser(f.ownerA)}
	ceiling := bearerCeiling(t, "agent:list", "skill:list")

	cases := []struct {
		name     string
		boundary TokenBoundary
		target   Resource
		evidence TargetScopeEvidence
	}{
		{"hub evidence for another permission", hubBoundary(), Resource{}, hubCollectionEvidence("skill.list")},
		{"project evidence for another permission", projectBoundary(f.projectA), Resource{Type: "agent", ParentType: "project", ParentID: f.projectA}, projectCollectionEvidence(f.projectA, "agent.create")},
		{"hub boundary with project evidence for another permission", hubBoundary(), Resource{}, projectCollectionEvidence(f.projectA, "skill.list")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eval := f.srv.authzService.EvaluateBearerCeiling(ctx, user, tc.boundary, ceiling, "agent.list", tc.target, BearerOptions{Evidence: tc.evidence})
			assert.False(t, eval.Decision.Allowed)
			assert.Equal(t, BearerStageTargetUnknown, eval.Stage, eval.Decision.Reason)
			assert.Equal(t, bearerReasonTargetUnknown, eval.Decision.Reason)
		})
	}
}

// TestUATGate_RequestTargetEvidence pins that AuthzRequest.TargetEvidence
// reaches the bearer gate of a UAT request: evidence naming the evaluated
// permission classifies the target, and evidence naming another
// permission denies.
func TestUATGate_RequestTargetEvidence(t *testing.T) {
	f := newBearerFixture(t, "gate-evidence")
	ctx := context.Background()
	selectors := []string{"agent:list", "skill:list"}
	ceiling := bearerCeiling(t, selectors...)

	decide := func(boundary TokenBoundary, permID string, target Resource, evidence TargetScopeEvidence) Decision {
		scoped := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(f.ownerA), boundary, selectors, tid("bearer-gate-evidence-cred-"+string(boundary.Kind)), ceiling, nil)
		action, ok := registryActionFor(permID)
		require.True(t, ok)
		return f.srv.authzService.Decide(ctx, AuthzRequest{
			Principal:      PrincipalContext{Identity: scoped},
			Resource:       target,
			Action:         action,
			Permission:     permID,
			TargetEvidence: evidence,
		})
	}

	outside := decide(projectBoundary(f.projectA), "skill.list", Resource{}, hubCollectionEvidence("skill.list"))
	assert.False(t, outside.Allowed)
	assert.Equal(t, bearerReasonHubLevelResource, outside.Reason)

	mismatched := decide(hubBoundary(), "agent.list", Resource{}, hubCollectionEvidence("skill.list"))
	assert.False(t, mismatched.Allowed)
	assert.Equal(t, bearerReasonTargetUnknown, mismatched.Reason)

	project := decide(projectBoundary(f.projectA), "agent.list", Resource{Type: "agent", ParentType: "project", ParentID: f.projectA}, projectCollectionEvidence(f.projectA, "agent.list"))
	assert.True(t, project.Allowed, project.Reason)
}

// TestEvaluateBearerCeiling_PreGatePolicyDenyIsNotIndeterminate pins that a
// policy deny decide applies before the bearer gate reports
// BearerStageError without being indeterminate: the delivery credential
// gate denies a deliver permission for a bearer credential.
func TestEvaluateBearerCeiling_PreGatePolicyDenyIsNotIndeterminate(t *testing.T) {
	f := newBearerFixture(t, "pregate")
	eval := f.srv.authzService.EvaluateBearerCeiling(context.Background(), PrincipalContext{Identity: bearerUser(f.ownerA)}, hubBoundary(), bearerCeiling(t, "agent:read"), "secret.deliver", agentResource(f.agentA), BearerOptions{})
	assert.False(t, eval.Decision.Allowed)
	assert.Equal(t, BearerStageError, eval.Stage)
	assert.Equal(t, deliveryGateReason, eval.Decision.Reason)
	assert.False(t, eval.Decision.IsIndeterminate(), "a policy deny is not indeterminate")
}
