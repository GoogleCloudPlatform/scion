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

// Tests for skillProgenyAdapter (ptone/scion#2128), which consolidates the
// former dedicated creator-user-skill grant into the common progeny
// evaluator. TestAgentCreatorUserSkillGrant_Conditions (which called the
// retired agentCreatorUserSkillGrant directly) is replaced by
// TestSkillProgenyRead_Conditions below, exercising the same shapes at the
// relationship stage (evaluateRelationshipCandidates), isolated from the
// rest of Decide. Decide-level coverage for this grant lives in the
// characterization test and in skill_agent_read_test.go — see the doc on
// TestSkillProgenyRead_Conditions.

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- pure adapter unit tests ---

func TestSkillProgenyAdapter_Sources(t *testing.T) {
	a := skillProgenyAdapter{}
	assert.Equal(t, "skill", a.Kind())
	assert.Equal(t, []string{"skill.read"}, a.ReadPermissions())

	srcs, err := a.Sources(context.Background(), ProgenyQuery{Kind: "skill", ResourceID: "", Ancestry: []string{"u"}})
	require.NoError(t, err)
	assert.Empty(t, srcs, "an ID-less query without a bucket key synthesizes nothing")

	srcs, err = a.Sources(context.Background(), ProgenyQuery{Kind: "skill", ResourceID: "u", Ancestry: []string{"u"}})
	require.NoError(t, err)
	require.Len(t, srcs, 1)
	assert.Equal(t, SharingSource{Kind: "skill", ID: "u", OwnerID: "u", Policy: SharingPolicyOriginDescendants}, srcs[0],
		"the adapter never consults Resource.OwnerID/CreatedBy — the bucket key is both ID and OwnerID")
}

func TestSkillProgenyAdapter_FactResourceID(t *testing.T) {
	a := skillProgenyAdapter{}
	for _, tc := range []struct {
		name     string
		resource Resource
		wantID   string
		wantOK   bool
	}{
		{"user scope", Resource{Type: "skill", ScopeKind: store.SkillScopeUser, ScopeUserID: "u"}, "u", true},
		{"user scope without owner", Resource{Type: "skill", ScopeKind: store.SkillScopeUser, ScopeUserID: ""}, "", false},
		{"no scope kind but ScopeUserID set", Resource{Type: "skill", ScopeUserID: "u"}, "", false},
		{"global scope", Resource{Type: "skill", ScopeKind: store.SkillScopeGlobal}, "", false},
		{"project scope", Resource{Type: "skill", ScopeKind: store.SkillScopeProject, ScopeUserID: "u"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := a.FactResourceID(tc.resource)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

// TestSkillProgenyRead_Conditions ports TestAgentCreatorUserSkillGrant_Conditions
// (retired with agentCreatorUserSkillGrant) to exercise the same shapes
// through evaluateRelationshipCandidates, the same isolation level the
// retired pure-function test had, instead of full Decide (see the isolation
// note in the loop below). The personal-skill progeny read matches only a
// read of a user-scoped skill owned by a hub-attested agent's origin user. A
// child agent (ancestry [U, parent]) gets U's bucket, never its parent's or
// anyone else's. The agents are stored with delegation edges in a project
// where U is a member, because the execution-project stage resolves the
// stored agent's source user and requires U's admission to that project.
//
// Decide-level (end-to-end) coverage for this grant, beyond the isolated
// candidate shapes tested here, lives in:
//   - TestRelationshipCharacterization_ProgenySkillRead (origin allow /
//     other-user deny, provenance)
//   - TestAgentSkillRead_ChildAgentSeesParentGrantedSet (child agent)
//   - TestAgentSkillRead_CreatorSuspendedOrDeletedLosesUserSkills and
//     TestRelationshipRules_SkillProgenySourceInactive (suspended/deleted
//     source)
//   - skill_agent_read_test.go's write-action assertions (writes never
//     widened)
func TestSkillProgenyRead_Conditions(t *testing.T) {
	authz, s := authzTestSetup(t)
	u := createCharacterizationUser(t, s, tid("sp-user-u"))
	v := createCharacterizationUser(t, s, tid("sp-user-v"))
	parent := tid("sp-parent")
	ctx := context.Background()
	project := tid("sp-proj")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: project, Name: "sp-proj", Slug: "sp-proj", CreatedBy: u.ID()}))
	createTestUserWithProjectRole(t, s, u.ID(), u.ID()+"@relchar.test", project, store.ProjectRoleMember)
	agentID, childID := tid("sp-agent"), tid("sp-child")
	seedExecutionAgent(t, s, agentID, project, []string{u.ID()}, []string{u.ID()})
	seedExecutionAgent(t, s, childID, project, []string{u.ID(), parent}, []string{u.ID(), parent})

	newAgent := func(ancestry ...string) Identity {
		id := agentID
		if len(ancestry) > 1 {
			id = childID
		}
		return &agentIdentityWrapper{&AgentTokenClaims{
			Claims: jwt.Claims{Subject: id}, ProjectID: project,
			Scopes: allRegisteredAgentScopes(), Ancestry: ancestry,
		}}
	}
	su := skillScopeResource(store.SkillScopeUser, u.ID())
	sv := skillScopeResource(store.SkillScopeUser, v.ID())
	fed := NewFederatedAgentIdentity("https://other.example", tid("sp-fed"), tid("sp-proj"), "fed", u.ID(), []string{u.ID()}, allRegisteredAgentScopes())

	// noProgenyCandidate marks a negative case where the shape never produces
	// a RelationshipRuleProgeny candidate at all (so there is no rejection
	// stage to name), as distinct from a candidate that is produced and then
	// rejected at a named stage.
	const noProgenyCandidate = "no_candidate"

	cases := []struct {
		name     string
		identity Identity
		resource Resource
		action   Action
		perm     string
		want     bool
		// rejectedBy is checked only when want is false. It names the stage
		// (RelationshipCandidateResult.RejectedBy) that rejected the
		// RelationshipRuleProgeny candidate, or noProgenyCandidate when the
		// shape produces no such candidate.
		rejectedBy string
	}{
		{"creator skill", newAgent(u.ID()), su, ActionRead, "skill.read", true, ""},
		{"child agent gets origin user's skill", newAgent(u.ID(), parent), su, ActionRead, "skill.read", true, ""},
		{"other user's skill", newAgent(u.ID()), sv, ActionRead, "skill.read", false, RelationshipRejectFact},
		{"parent agent id is not a user bucket", newAgent(u.ID(), parent), skillScopeResource(store.SkillScopeUser, parent), ActionRead, "skill.read", false, RelationshipRejectFact},
		{"no ancestry", newAgent(), su, ActionRead, "skill.read", false, RelationshipRejectFact},
		{"update", newAgent(u.ID()), su, ActionUpdate, "skill.update", false, noProgenyCandidate},
		{"delete", newAgent(u.ID()), su, ActionDelete, "skill.delete", false, noProgenyCandidate},
		{"global skill", newAgent(u.ID()), skillScopeResource(store.SkillScopeGlobal, ""), ActionRead, "skill.read", false, noProgenyCandidate},
		{"project skill", newAgent(u.ID()), skillScopeResource(store.SkillScopeProject, u.ID()), ActionRead, "skill.read", false, noProgenyCandidate},
		{"user scope without owner", newAgent(u.ID()), skillScopeResource(store.SkillScopeUser, ""), ActionRead, "skill.read", false, noProgenyCandidate},
		{"no scope kind", newAgent(u.ID()), Resource{Type: "skill", ScopeUserID: u.ID()}, ActionRead, "skill.read", false, noProgenyCandidate},
		{"not a skill", newAgent(u.ID()), Resource{Type: "secret", ScopeKind: store.SkillScopeUser, ScopeUserID: u.ID()}, ActionRead, permissionProjectSecretRead, false, RelationshipRejectFact},
		{"federated agent", fed, su, ActionRead, "skill.read", false, RelationshipRejectUntrustedAncestry},
		{"user principal", u, su, ActionRead, "skill.read", false, noProgenyCandidate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Isolate the progeny relationship candidate itself, the same
			// isolation level the retired pure-function test had. Full
			// Decide is not used here: a global/core skill is separately
			// allowed for any agent through the unrelated synthetic
			// agent-skill-catalog kernel binding (ptone/scion#1968), which
			// would otherwise mask what this grant does or does not admit.
			out := authz.evaluateRelationshipCandidates(ctx,
				principalContextForIdentity(tc.identity), tc.resource, tc.action, tc.perm, nil, false)
			got := out.accepted != nil && out.accepted.Allowed
			assert.Equal(t, tc.want, got, "candidates: %+v", out.results)

			if tc.want {
				return
			}
			// A rejected candidate must be rejected at its intended stage,
			// not at RelationshipRejectExecutionProject: a stage-order
			// regression that made execution-project run first would still
			// leave got == false (vacuously) without this check.
			var progeny *RelationshipCandidateResult
			for i := range out.results {
				if out.results[i].Rule == RelationshipRuleProgeny {
					progeny = &out.results[i]
					break
				}
			}
			if tc.rejectedBy == noProgenyCandidate {
				assert.Nil(t, progeny, "expected no progeny candidate: %+v", out.results)
				return
			}
			if assert.NotNil(t, progeny, "expected a progeny candidate: %+v", out.results) {
				assert.Equal(t, tc.rejectedBy, progeny.RejectedBy)
			}
		})
	}
}

// TestSkillProgenyRead_ScopeOwnerDiffersFromCreator pins that the grant
// keys only on Resource.ScopeUserID: a skill's OwnerID/CreatedBy (which
// Resource does not even carry) never widens or narrows it. An agent of the
// scope owner is allowed; an agent of the creator/owner is not. Both agents
// are stored in a project their source user is a member of, so only the
// bucket key separates them.
func TestSkillProgenyRead_ScopeOwnerDiffersFromCreator(t *testing.T) {
	f := newGoldenFixture(t)
	creator := createCharacterizationUser(t, f.store, tid("sp-creator"))
	sk := createTestSkill(t, f.store, "sp-mismatched", store.SkillScopeUser, f.projectOwnerID, creator.ID())
	require.Equal(t, f.projectOwnerID, sk.ScopeID)
	require.Equal(t, creator.ID(), sk.OwnerID)
	// Pin OwnerID/CreatedBy literally, per the acceptance criterion wording,
	// even though the guarantee is structural: Resource carries no CreatedBy
	// at all, so the adapter cannot consult it regardless of this value.
	sk.CreatedBy = creator.ID()
	require.NoError(t, f.store.UpdateSkill(context.Background(), sk))

	createTestUserWithProjectRole(t, f.store, creator.ID(), creator.ID()+"@relchar.test", f.projectAlpha.ID, store.ProjectRoleMember)
	scopeOwnerAgentID, creatorAgentID := tid("sp-agent-scope-owner"), tid("sp-agent-creator")
	seedExecutionAgent(t, f.store, scopeOwnerAgentID, f.projectAlpha.ID, []string{f.projectOwnerID}, []string{f.projectOwnerID})
	seedExecutionAgent(t, f.store, creatorAgentID, f.projectAlpha.ID, []string{creator.ID()}, []string{creator.ID()})
	agentOfScopeOwner := execAgent(scopeOwnerAgentID, f.projectAlpha.ID, []string{f.projectOwnerID})
	agentOfCreator := execAgent(creatorAgentID, f.projectAlpha.ID, []string{creator.ID()})

	d := decidePerm(f.authz, agentOfScopeOwner, skillResource(sk), ActionRead, "skill.read", false)
	assert.True(t, d.Allowed, "agent of the scope owner: reason %q", d.Reason)
	d = decidePerm(f.authz, agentOfCreator, skillResource(sk), ActionRead, "skill.read", false)
	assert.False(t, d.Allowed, "agent of the creator (not the scope owner) must be denied")
}

// TestSkillProgenyRead_ListAndPointReadAgree asserts the bucket-level
// ID-less probe agentSkillAccessScope relies on and a concrete point read
// agree, using the same fact evaluation (ProgenyListPredicate/EvaluateProgeny
// share one code path with Decide — see TestProgeny_ListAndPointReadConsistent
// for the analogous secret-kind test).
func TestSkillProgenyRead_ListAndPointReadAgree(t *testing.T) {
	f := newGoldenFixture(t)
	agentID := tid("sp-list-point-agent")
	seedExecutionAgent(t, f.store, agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, []string{f.projectOwnerID})
	agent := execAgent(agentID, f.projectAlpha.ID, []string{f.projectOwnerID})
	ctx := context.Background()
	principal := principalContextForIdentity(agent)

	// The bucket probe (ID-less, as agentSkillAccessScope issues it).
	bucketAllowed := f.authz.CheckAccess(ctx, agent, skillScopeResource(store.SkillScopeUser, f.projectOwnerID), ActionRead).Allowed
	require.True(t, bucketAllowed, "origin user's bucket probe must be allowed")

	// A concrete skill in that bucket: point read and the shared predicate
	// must agree with the bucket probe.
	sk := createTestSkill(t, f.store, "sp-list-point", store.SkillScopeUser, f.projectOwnerID, f.projectOwnerID)
	pointAllowed := f.authz.CheckAccess(ctx, agent, skillResource(sk), ActionRead).Allowed
	assert.Equal(t, bucketAllowed, pointAllowed)

	src := SharingSource{Kind: "skill", ID: f.projectOwnerID, OwnerID: f.projectOwnerID, Policy: SharingPolicyOriginDescendants}
	pred := f.authz.ProgenyListPredicate(ctx, principal, "skill")
	assert.Equal(t, pointAllowed, pred.Matches(src))
	assert.Equal(t, pointAllowed, f.authz.EvaluateProgeny(ctx, principal, src))

	// A different user's bucket disagrees, consistently across all three.
	otherBucketAllowed := f.authz.CheckAccess(ctx, agent, skillScopeResource(store.SkillScopeUser, f.memberNoneID), ActionRead).Allowed
	otherSrc := SharingSource{Kind: "skill", ID: f.memberNoneID, OwnerID: f.memberNoneID, Policy: SharingPolicyOriginDescendants}
	assert.False(t, otherBucketAllowed)
	assert.Equal(t, otherBucketAllowed, pred.Matches(otherSrc))
	assert.Equal(t, otherBucketAllowed, f.authz.EvaluateProgeny(ctx, principal, otherSrc))
}

// TestSkillProgenyRead_ProvenanceNamesGrantAndSource pins the provenance
// acceptance criterion ("common path used; provenance names the progeny
// grant and source U"): an accepted candidate must report MatchedGrant
// "builtin:relationship:progeny_skill_read" and a RelationshipSource with
// Kind "skill" and OwnerID/ID equal to U. Checked for both the bucket-level
// probe agentSkillAccessScope issues and a concrete point read, since the
// point read is the shape where Source.ID (U) differs from Resource.ID (the
// skill record).
func TestSkillProgenyRead_ProvenanceNamesGrantAndSource(t *testing.T) {
	f := newGoldenFixture(t)
	agentID := tid("sp-provenance-agent")
	seedExecutionAgent(t, f.store, agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, []string{f.projectOwnerID})
	agent := execAgent(agentID, f.projectAlpha.ID, []string{f.projectOwnerID})

	d := decidePerm(f.authz, agent, skillScopeResource(store.SkillScopeUser, f.projectOwnerID), ActionRead, "skill.read", true)
	require.True(t, d.Allowed, "bucket probe: reason %q", d.Reason)
	assert.Equal(t, "builtin:relationship:progeny_skill_read", d.MatchedGrant)
	r := relationshipResult(t, d, RelationshipRuleProgeny)
	require.True(t, r.Accepted)
	require.NotNil(t, r.Source)
	assert.Equal(t, "skill", r.Source.Kind)
	assert.Equal(t, f.projectOwnerID, r.Source.OwnerID)
	assert.Equal(t, f.projectOwnerID, r.Source.ID)

	sk := createTestSkill(t, f.store, "sp-provenance-skill", store.SkillScopeUser, f.projectOwnerID, f.projectOwnerID)
	d = decidePerm(f.authz, agent, skillResource(sk), ActionRead, "skill.read", true)
	require.True(t, d.Allowed, "point read: reason %q", d.Reason)
	assert.Equal(t, "builtin:relationship:progeny_skill_read", d.MatchedGrant)
	r = relationshipResult(t, d, RelationshipRuleProgeny)
	require.True(t, r.Accepted)
	require.NotNil(t, r.Source)
	assert.Equal(t, "skill", r.Source.Kind)
	assert.Equal(t, f.projectOwnerID, r.Source.OwnerID)
	assert.Equal(t, f.projectOwnerID, r.Source.ID)
	assert.NotEqual(t, sk.ID, r.Source.ID, "the source ID is the owning user, not the skill record ID")
}

// TestSkillProgenyRead_ProjectAccessRemovedDenies pins the execution-project
// stage for a personal skill: an agent reads its origin user U's personal
// skills only while U holds admission to the agent's project. When U's
// project role binding is removed while U stays active, the bucket probe and
// the point read both deny at the execution_project stage.
func TestSkillProgenyRead_ProjectAccessRemovedDenies(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	u := createCharacterizationUser(t, s, tid("sp-projgone-u"))
	project := tid("sp-projgone-proj")
	proj := &store.Project{ID: project, Name: "sp-projgone", Slug: "sp-projgone", CreatedBy: u.ID()}
	require.NoError(t, s.CreateProject(ctx, proj))
	createTestUserWithProjectRole(t, s, u.ID(), u.ID()+"@relchar.test", project, store.ProjectRoleMember)

	agentID := tid("sp-projgone-agent")
	seedExecutionAgent(t, s, agentID, project, []string{u.ID()}, []string{u.ID()})
	agent := execAgent(agentID, project, []string{u.ID()})
	sk := createTestSkill(t, s, "sp-projgone-skill", store.SkillScopeUser, u.ID(), u.ID())

	// With U admitted to the project, both reads are allowed.
	d := authz.CheckAccess(ctx, agent, skillScopeResource(store.SkillScopeUser, u.ID()), ActionRead)
	assert.True(t, d.Allowed, "bucket probe before removal: reason %q", d.Reason)
	assert.Equal(t, "relationship grant: progeny_skill_read", d.Reason)
	d = authz.CheckAccess(ctx, agent, skillResource(sk), ActionRead)
	assert.True(t, d.Allowed, "point read before removal: reason %q", d.Reason)

	// Remove U's project membership while U itself stays active.
	n, err := s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID())
	require.NoError(t, err)
	require.Positive(t, n, "the user's project membership must actually be removed")
	after := authz.CheckAccess(ctx, u, projectResource(proj), ActionRead)
	assert.False(t, after.Allowed, "U should have lost project read access after removal: reason %q", after.Reason)

	for name, res := range map[string]Resource{
		"bucket probe": skillScopeResource(store.SkillScopeUser, u.ID()),
		"point read":   skillResource(sk),
	} {
		d := decidePerm(authz, agent, res, ActionRead, "skill.read", true)
		assert.False(t, d.Allowed, "%s after removal: reason %q", name, d.Reason)
		r := relationshipResult(t, d, RelationshipRuleProgeny)
		assert.Equal(t, RelationshipRejectExecutionProject, r.RejectedBy, "%s: detail %q", name, r.Detail)
	}
}

// TestSkillProgenyRead_ListScopeAgreesForChildAndSuspendedSource extends the
// point/list parity assertion to the two cases the grant's own tests
// exercise at the Decide/HTTP level (child agent, suspended source),
// against agentSkillAccessScope's CallerID specifically — the value
// listSkills (skill_handlers.go) actually uses, rather than the
// ProgenyListPredicate/EvaluateProgeny path TestSkillProgenyRead_ListAndPointReadAgree
// already covers.
func TestSkillProgenyRead_ListScopeAgreesForChildAndSuspendedSource(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	u := createCharacterizationUser(t, s, tid("sp-scope-u"))
	sk := createTestSkill(t, s, "sp-scope-skill", store.SkillScopeUser, u.ID(), u.ID())
	project := tid("sp-scope-proj")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: project, Name: "sp-scope", Slug: "sp-scope", CreatedBy: u.ID()}))
	createTestUserWithProjectRole(t, s, u.ID(), u.ID()+"@relchar.test", project, store.ProjectRoleMember)

	// A grandchild agent (ancestry [U, parent]) gets U's bucket, exactly as
	// a direct child would.
	childID, parentID := tid("sp-scope-child"), tid("sp-scope-parent")
	seedExecutionAgent(t, s, childID, project, []string{u.ID(), parentID}, []string{u.ID(), parentID})
	child := execAgent(childID, project, []string{u.ID(), parentID})
	scope := srv.agentSkillAccessScope(ctx, child)
	pointAllowed := srv.authzService.CheckAccess(ctx, child, skillResource(sk), ActionRead).Allowed
	assert.True(t, pointAllowed, "grandchild reads origin user's skill")
	assert.Equal(t, pointAllowed, scope.CallerID == u.ID())

	// Suspend the source user: the bucket scope and the point read must
	// drop together.
	fresh, err := s.GetUser(ctx, u.ID())
	require.NoError(t, err)
	fresh.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(ctx, fresh))

	scope = srv.agentSkillAccessScope(ctx, child)
	pointAllowed = srv.authzService.CheckAccess(ctx, child, skillResource(sk), ActionRead).Allowed
	assert.False(t, pointAllowed, "suspended source denies the point read")
	assert.Empty(t, scope.CallerID, "suspended source drops the bucket scope too")
}
