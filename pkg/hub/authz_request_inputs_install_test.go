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

// Tests for the production install sites of the request-local
// authorization input memo, and for the masks that keep lookups made on
// behalf of principals other than the requester off the memo.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callerFuncNames returns the fully qualified function names on the
// calling goroutine's stack, innermost first.
func callerFuncNames() []string {
	pcs := make([]uintptr, 512)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var out []string
	for {
		f, more := frames.Next()
		out = append(out, f.Function)
		if !more {
			break
		}
	}
	return out
}

// frameMatches reports whether a fully qualified function name is the
// named function or one of its closures.
func frameMatches(fn, name string) bool {
	return strings.HasSuffix(fn, "."+name) || strings.Contains(fn, "."+name+".func")
}

// stackHas reports whether any frame in stack is the named function.
func stackHas(stack []string, name string) bool {
	for _, fn := range stack {
		if frameMatches(fn, name) {
			return true
		}
	}
	return false
}

// onStack reports whether the named function is on the current stack.
func onStack(name string) bool {
	return stackHas(callerFuncNames(), name)
}

// systemAuthoritySourceFixture is an agent reading a progeny skill whose
// source user has no membership in the agent's project but holds skill.read
// through a system-scoped role. Execution-project admission for that user
// therefore falls through to SystemAuthorityProof, which loads the access
// constraint table for the source user.
type systemAuthoritySourceFixture struct {
	projectID    string
	sourceUserID string
	agentID      string
	agent        AgentIdentity
	skillRes     Resource
}

func newSystemAuthoritySourceFixture(t *testing.T, s store.Store, name string) *systemAuthoritySourceFixture {
	t.Helper()
	ctx := context.Background()
	projectID := tid(name + "-project")
	sourceUserID := tid(name + "-source")
	agentID := tid(name + "-agent")
	injectionID := tid(name + "-skill")

	createDCProject(t, s, projectID, name+"-proj")
	systemRoleUserWithPermissions(t, s, sourceUserID, []string{"skill.read"})
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: name + "-agent", Name: name + "-agent",
		ProjectID: projectID, Phase: "running",
		OwnerID: sourceUserID, CreatedBy: sourceUserID, Ancestry: []string{sourceUserID},
	}))
	createDCEdge(t, s, store.DelegationPrincipalUser, sourceUserID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))
	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		ID: injectionID, Scope: "user", ScopeID: sourceUserID,
		SkillURI: "test://" + name, AllowProgeny: true, CreatedBy: sourceUserID,
	}))

	return &systemAuthoritySourceFixture{
		projectID:    projectID,
		sourceUserID: sourceUserID,
		agentID:      agentID,
		agent: &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: agentID},
			ProjectID: projectID,
			Ancestry:  []string{sourceUserID},
			Scopes:    allRegisteredAgentScopes(),
		}},
		skillRes: Resource{Type: "skill", ID: injectionID, ScopeKind: store.SkillScopeUser, ScopeUserID: sourceUserID},
	}
}

// sourceAgentChainFixture is a child agent reading a progeny secret that
// its parent agent created. The parent's delegation edge leads to a
// non-admin user delegator with an in-scope project binding, so the
// sharing-source check walks the parent's delegation chain and loads that
// delegator's effective permissions, including the access constraint
// table. The child's own source user is the same project member, so
// execution-project admission passes on membership evidence. Both edges
// carry recorded session provenance, which the delegation ceiling requires
// for project.secret_read.
type sourceAgentChainFixture struct {
	projectID   string
	delegatorID string
	parentID    string
	childID     string
	child       AgentIdentity
	secretRes   Resource
	perm        permissions.Permission
}

func newSourceAgentChainFixture(t *testing.T, s store.Store, name string) *sourceAgentChainFixture {
	t.Helper()
	ctx := context.Background()
	projectID := tid(name + "-project")
	delegatorID := tid(name + "-delegator")
	parentID := tid(name + "-parent")
	childID := tid(name + "-child")
	secretID := tid(name + "-secret")

	createDCProject(t, s, projectID, name+"-proj")
	createDCUser(t, s, delegatorID, name+"-delegator@test.com", projectID, store.ProjectRoleAdmin)
	createDCAgent(t, s, parentID, projectID, delegatorID, AgentRoleFull)
	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, delegatorID, store.DelegationPrincipalAgent, parentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: childID, Slug: name + "-child", Name: name + "-child",
		ProjectID: projectID, Phase: "running",
		OwnerID: delegatorID, CreatedBy: parentID, Ancestry: []string{delegatorID, parentID},
	}))
	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, delegatorID, store.DelegationPrincipalAgent, childID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID: secretID, Key: name + "-secret", Scope: "user", ScopeID: delegatorID,
		AllowProgeny: true, CreatedBy: parentID,
	}))

	return &sourceAgentChainFixture{
		projectID:   projectID,
		delegatorID: delegatorID,
		parentID:    parentID,
		childID:     childID,
		child: &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: childID},
			ProjectID: projectID,
			Ancestry:  []string{delegatorID, parentID},
			Scopes:    allRegisteredAgentScopes(),
		}},
		secretRes: Resource{Type: "secret", ID: secretID},
		perm:      permissions.Permission{ID: "project.secret_read", Action: string(ActionRead)},
	}
}

func (f *sourceAgentChainFixture) decide(authz *AuthzService, ctx context.Context) Decision {
	return authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(f.child),
		Credential: credentialContextForIdentity(f.child),
		Resource:   f.secretRes,
		Action:     ActionRead,
		Permission: f.perm.ID,
	})
}

// countingFault returns a fault func that fails method's calls made while
// frame is on the stack, starting with the failOnN-th such call (failOnN
// <= 1 fails every one). reached counts the matching calls.
func countingFault(method, frame string, failOnN int, reached *int) func(string, int, context.Context) error {
	return func(m string, _ int, _ context.Context) error {
		if m != method || !onStack(frame) {
			return nil
		}
		*reached++
		if failOnN <= 1 || *reached == failOnN {
			return fmt.Errorf("injected %s fault under %s (call %d): %w", method, frame, *reached, errInjected)
		}
		return nil
	}
}

// onlyAtCall returns a fault func that fails only the n-th matching call.
func onlyAtCall(method, frame string, n int, reached *int) func(string, int, context.Context) error {
	return func(m string, _ int, _ context.Context) error {
		if m != method || !onStack(frame) {
			return nil
		}
		*reached++
		if *reached == n {
			return fmt.Errorf("injected %s fault under %s (call %d): %w", method, frame, n, errInjected)
		}
		return nil
	}
}

// normalizedAudits copies records with the per-emission fields cleared so
// two runs of the same decisions compare equal.
func normalizedAudits(records []*store.DecisionAuditRecord) []store.DecisionAuditRecord {
	out := make([]store.DecisionAuditRecord, len(records))
	for i, r := range records {
		c := *r
		c.ID, c.Timestamp, c.CorrelationID = "", time.Time{}, ""
		out[i] = c
	}
	return out
}

// isRelationshipCandidateCall reports whether stack is inside the
// relationship candidates decide evaluates for the requester (as opposed
// to a nested evaluation for a delegator inside the delegation ceiling).
func isRelationshipCandidateCall(stack []string) bool {
	for i := 0; i+1 < len(stack); i++ {
		if frameMatches(stack[i], "evaluateRelationshipCandidates") && frameMatches(stack[i+1], "decide") {
			return true
		}
	}
	return false
}

// isDecideConstraintLoad reports whether stack is decide's own access
// constraint load for the requester.
func isDecideConstraintLoad(stack []string) bool {
	for i := 0; i+1 < len(stack); i++ {
		if frameMatches(stack[i], "accessConstraintRestrictions") && frameMatches(stack[i+1], "decide") {
			return true
		}
	}
	return false
}

// isPrincipalInputsLoad reports whether stack is one of the memo-backed
// principal input loaders.
func isPrincipalInputsLoad(stack []string) bool {
	for _, fn := range stack {
		if strings.Contains(fn, "(*principalInputs).") {
			return true
		}
	}
	return false
}

// TestMemoInstall_RelationshipCandidatesNeverObserveMemo records every
// intercepted store call made by decisions that reach the relationship
// candidates (a progeny skill whose source user is admitted on system
// authority, a progeny secret whose source agent's delegation chain is
// walked), the delegation ceiling, and a list-scope resolution, all under
// an installed memo. It asserts that no call made for a relationship
// candidate observes either memo key, except the project-access stage
// (relationshipProjectAccessStage), which evaluates the requester's own
// project access and must read the requester's input memo (and never looks
// up delegation edges); user owners admitted through membership and through system
// authority exercise it and must decide as without the memo. It also
// asserts that the delegation ceiling never observes the input memo, and
// that the memoized loaders observe the input memo only from their known
// consumers.
func TestMemoInstall_RelationshipCandidatesNeverObserveMemo(t *testing.T) {
	_, s := authzTestSetup(t)
	sa := newSystemAuthoritySourceFixture(t, s, "rec-sa")
	chain := newSourceAgentChainFixture(t, s, "rec-chain")
	delegated := newA1Fixture(t, s, "rec-delegated")
	member := newP1Fixture(t, s, "rec-member")

	mstore := newMemoTestStore(s)
	mstore.recordCalls = true
	authz, _ := newRecordingAuthz(mstore)
	bg := context.Background()

	mctx := withAuthzInputMemo(bg)
	for i := 0; i < 2; i++ {
		d := authz.CheckAccess(mctx, sa.agent, sa.skillRes, ActionRead)
		require.True(t, d.Allowed, "system-authority progeny skill read must be allowed: %q", d.Reason)
	}
	authz.ComputeCapabilitiesBatch(bg, sa.agent, []Resource{sa.skillRes}, "skill")
	_, err := authz.AuthorizeReadBatch(bg, sa.agent, []Resource{sa.skillRes})
	require.NoError(t, err)

	cctx := withAuthzInputMemo(bg)
	for i := 0; i < 2; i++ {
		d := chain.decide(authz, cctx)
		require.True(t, d.Allowed, "source-agent progeny secret read must be allowed: %q", d.Reason)
	}

	actx := withAuthzInputMemo(bg)
	for _, tp := range agentGrantableTuples(delegated) {
		authz.CheckAccess(actx, delegated.agent, tp.resource, tp.action)
	}

	pctx := withAuthzInputMemo(bg)
	for _, tp := range agentResourceTuples(member.agentRes) {
		authz.CheckAccess(pctx, member.user, tp.resource, tp.action)
	}
	_, err = authz.ResolveListScopes(pctx, member.user, "agent.list")
	require.NoError(t, err)

	// User owners reach the project-access stage (stage 2c), which reads the
	// requester's memo by design: one owner admitted through project
	// membership (attach comes only from the owner relationship) and one
	// through system authority (the kernel allows; Explain evaluates the
	// candidates). Each decision must equal its no-memo baseline.
	ownedByMember := Resource{Type: "agent", ID: tid("rec-member-owned-agent"), ParentType: "project", ParentID: member.projectID, OwnerID: member.userID}
	sysOwnerID := tid("rec-sys-owner")
	createTestUserWithRole(t, s, sysOwnerID, sysOwnerID+"@test.com", "member", store.SystemRoleSuperAdmin)
	sysOwner := NewAuthenticatedUser(sysOwnerID, sysOwnerID+"@test.com", "Sys Owner", "member", "api")
	ownedBySys := Resource{Type: "agent", ID: tid("rec-sys-owned-agent"), ParentType: "project", ParentID: member.projectID, OwnerID: sysOwnerID}
	ownerDecide := func(ctx context.Context, ident Identity, res Resource) Decision {
		return authz.Decide(ctx, AuthzRequest{
			Principal: principalContextForIdentity(ident), Credential: credentialContextForIdentity(ident),
			Resource: res, Action: ActionAttach, Permission: "agent.attach", Explain: true,
		})
	}
	octx := withAuthzInputMemo(bg)
	for _, tc := range []struct {
		ident Identity
		res   Resource
	}{{member.user, ownedByMember}, {sysOwner, ownedBySys}} {
		want := ownerDecide(bg, tc.ident, tc.res)
		got := ownerDecide(octx, tc.ident, tc.res)
		require.True(t, got.Allowed, "owner with project access must be allowed: %q", got.Reason)
		assert.Equal(t, want.Allowed, got.Allowed)
		assert.Equal(t, want.Reason, got.Reason)
		assert.Equal(t, want.MatchedGrant, got.MatchedGrant)
		require.NotNil(t, got.Provenance)
		var ownerAccepted bool
		for _, r := range got.Provenance.Relationships {
			if r.Rule == RelationshipRuleOwner && r.Accepted {
				ownerAccepted = true
			}
		}
		assert.True(t, ownerAccepted, "the owner candidate must pass the project-access stage")
	}

	memoLoaders := map[string]bool{
		"GetEffectiveGroups":            true,
		"GetEffectiveGroupsForAgent":    true,
		"ListRoleBindingsForPrincipals": true,
		"GetRoleDefinitionsByIDs":       true,
		"ListAccessConstraints":         true,
	}
	var candidateViaSystemAuthority, candidateViaDelegatorPermissions, candidateEdges, inputMemoSeen, ceilingEdgesMemo int
	var projectAccessStageMemo int
	for _, rec := range mstore.recordedCalls() {
		candidate := isRelationshipCandidateCall(rec.stack)
		ceiling := stackHas(rec.stack, "checkDelegationCeiling")
		// The project-access stage is the one relationship stage that reads
		// the requester's memo: it evaluates the requester's own access.
		// The delegation ceiling's user hop (userRelationshipAuthority)
		// also runs the stage, for the delegator rather than the requester
		// (ptone/scion#3433): it reads no memo, like every other stage.
		ceilingHopStage := stackHas(rec.stack, "userRelationshipAuthority")
		projectAccessStage := candidate && stackHas(rec.stack, "relationshipProjectAccessStage") && !ceilingHopStage
		if projectAccessStage {
			// The request context carries both keys; the stage must never
			// reach the edges memo's only consumer, the delegation-edge
			// lookup.
			assert.NotEqual(t, "GetDelegationEdgesForDelegate", rec.method,
				"#%d: the project-access stage must not look up delegation edges", rec.n)
			if rec.hasInputMemo {
				projectAccessStageMemo++
			}
		}
		if candidate && !projectAccessStage {
			assert.False(t, rec.hasInputMemo, "%s #%d inside a relationship candidate observed the input memo", rec.method, rec.n)
			assert.False(t, rec.hasEdgesMemo, "%s #%d inside a relationship candidate observed the edges memo", rec.method, rec.n)
			switch {
			case rec.method == "ListAccessConstraints" && stackHas(rec.stack, "SystemAuthorityProof"):
				candidateViaSystemAuthority++
			case rec.method == "ListAccessConstraints" && stackHas(rec.stack, "getEffectivePermissions"):
				candidateViaDelegatorPermissions++
			case rec.method == "GetDelegationEdgesForDelegate":
				candidateEdges++
			}
		}
		if ceiling {
			assert.False(t, rec.hasInputMemo, "%s #%d inside the delegation ceiling observed the input memo", rec.method, rec.n)
			if rec.hasEdgesMemo {
				ceilingEdgesMemo++
			}
		}
		if rec.hasEdgesMemo && !rec.hasInputMemo {
			assert.True(t, ceiling, "%s #%d saw only the edges memo outside the delegation ceiling", rec.method, rec.n)
		}
		if rec.hasInputMemo && memoLoaders[rec.method] {
			inputMemoSeen++
			known := isPrincipalInputsLoad(rec.stack) || isDecideConstraintLoad(rec.stack) ||
				stackHas(rec.stack, "applyListScopeConstraints") || stackHas(rec.stack, "relationshipProjectAccessStage")
			assert.True(t, known, "%s #%d observed the input memo outside its known consumers; stack: %v", rec.method, rec.n, rec.stack)
		}
	}
	assert.Positive(t, candidateViaSystemAuthority, "a relationship candidate must load access constraints through system authority")
	assert.Positive(t, candidateViaDelegatorPermissions, "a relationship candidate must load access constraints for a source agent's delegator")
	assert.Positive(t, candidateEdges, "a relationship candidate must look up a source agent's delegation edges")
	assert.Positive(t, inputMemoSeen, "the requester's own loads must observe the input memo")
	assert.Positive(t, ceilingEdgesMemo, "the delegation ceiling must observe the edges memo")
	assert.Positive(t, projectAccessStageMemo, "the project-access stage must read the requester's memo")
}

// TestMemoInstall_SystemAuthorityConstraintFaultStillDenies fails the
// access constraint load made while proving a progeny source user's system
// authority. The requester's own constraint load succeeds first, so a memo
// visible to that proof would serve it and allow; every decision must
// instead equal the no-memo reference and deny.
func TestMemoInstall_SystemAuthorityConstraintFaultStillDenies(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newSystemAuthoritySourceFixture(t, s, "sa-fault")
	bg := context.Background()
	const n = 3

	clean, _ := newRecordingAuthz(newMemoTestStore(s))
	base := clean.CheckAccess(bg, f.agent, f.skillRes, ActionRead)
	require.True(t, base.Allowed, "precondition: the progeny skill read is allowed with no fault: %q", base.Reason)
	require.Equal(t, "relationship grant: progeny_skill_read", base.Reason)

	run := func(ctx context.Context) ([]Decision, []*store.DecisionAuditRecord, int) {
		reached := 0
		st := newMemoTestStore(s)
		st.fault = countingFault("ListAccessConstraints", "SystemAuthorityProof", 0, &reached)
		authz, emit := newRecordingAuthz(st)
		var out []Decision
		for i := 0; i < n; i++ {
			out = append(out, authz.CheckAccess(ctx, f.agent, f.skillRes, ActionRead))
		}
		return out, emit.snapshot(), reached
	}
	ref, refAudit, refReached := run(bg)
	cand, candAudit, candReached := run(withAuthzInputMemo(bg))

	for i := range ref {
		require.False(t, ref[i].Allowed, "reference decision %d must deny under the fault", i)
		assert.Equal(t, "relationship grant restricted by execution_project", ref[i].Reason, "reference decision %d reason", i)
		assertDecisionsEqual(t, ref[i], cand[i], "decision %d", i)
	}
	assertAuditSequenceEqual(t, refAudit, candAudit)
	assert.Equal(t, n, refReached, "the fault must fire once per reference decision")
	assert.Equal(t, n, candReached, "every memo-run decision must reach the store for the source user's constraints")

	t.Run("capability_batch", func(t *testing.T) {
		batch := func(ctx context.Context) ([]*Capabilities, []*store.DecisionAuditRecord) {
			reached := 0
			st := newMemoTestStore(s)
			st.fault = countingFault("ListAccessConstraints", "SystemAuthorityProof", 0, &reached)
			authz, emit := newRecordingAuthz(st)
			caps := authz.ComputeCapabilitiesBatch(ctx, f.agent, []Resource{f.skillRes, f.skillRes}, "skill")
			require.Positive(t, reached, "the fault must fire")
			return caps, emit.snapshot()
		}
		refCaps, refAudit := batch(maskAllAuthzMemo(bg))
		candCaps, candAudit := batch(bg)
		require.Len(t, candCaps, 2)
		for i := range refCaps {
			assert.NotContains(t, refCaps[i].Actions, string(ActionRead), "reference must not grant read under the fault")
			assert.Equal(t, refCaps[i].Actions, candCaps[i].Actions, "resource %d capabilities", i)
		}
		assertAuditSequenceEqual(t, refAudit, candAudit)
	})

	t.Run("authorize_read_batch", func(t *testing.T) {
		read := func(ctx context.Context) ([]bool, []*store.DecisionAuditRecord, int) {
			reached := 0
			st := newMemoTestStore(s)
			st.fault = countingFault("ListAccessConstraints", "SystemAuthorityProof", 0, &reached)
			authz, emit := newRecordingAuthz(st)
			got, err := authz.AuthorizeReadBatch(ctx, f.agent, []Resource{f.skillRes, f.skillRes, f.skillRes})
			require.NoError(t, err)
			return got, emit.snapshot(), reached
		}
		refGot, refAudit, refReached := read(maskAllAuthzMemo(bg))
		candGot, candAudit, candReached := read(bg)
		assert.Equal(t, []bool{false, false, false}, refGot, "reference must deny every read under the fault")
		assert.Equal(t, refGot, candGot)
		assertAuditSequenceEqual(t, refAudit, candAudit)
		assert.Equal(t, refReached, candReached, "every decision must reach the store for the source user's constraints")
	})
}

// TestMemoInstall_SourceAgentConstraintFaultStillDenies fails the access
// constraint load made for the delegator of a progeny secret's source agent.
// The requester's own constraint load succeeds first, so a memo visible to
// the source agent's delegation walk would serve it and allow; every
// decision must instead equal the no-memo reference and deny.
func TestMemoInstall_SourceAgentConstraintFaultStillDenies(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newSourceAgentChainFixture(t, s, "chain-fault")
	bg := context.Background()
	const n = 3

	clean, _ := newRecordingAuthz(newMemoTestStore(s))
	base := f.decide(clean, bg)
	require.True(t, base.Allowed, "precondition: the progeny secret read is allowed with no fault: %q", base.Reason)
	require.Equal(t, "relationship grant: progeny_secret_read", base.Reason)

	run := func(ctx context.Context) ([]Decision, []*store.DecisionAuditRecord, int) {
		reached := 0
		st := newMemoTestStore(s)
		st.fault = countingFault("ListAccessConstraints", "relationshipSourceDelegationHolds", 0, &reached)
		authz, emit := newRecordingAuthz(st)
		var out []Decision
		for i := 0; i < n; i++ {
			out = append(out, f.decide(authz, ctx))
		}
		return out, emit.snapshot(), reached
	}
	ref, refAudit, refReached := run(bg)
	cand, candAudit, candReached := run(withAuthzInputMemo(bg))

	for i := range ref {
		require.False(t, ref[i].Allowed, "reference decision %d must deny under the fault", i)
		assert.Equal(t, "relationship grant restricted by source_inactive", ref[i].Reason, "reference decision %d reason", i)
		assertDecisionsEqual(t, ref[i], cand[i], "decision %d", i)
	}
	assertAuditSequenceEqual(t, refAudit, candAudit)
	assert.GreaterOrEqual(t, refReached, n, "the fault must fire on every reference decision")
	assert.Equal(t, refReached, candReached, "every memo-run decision must reach the store for the delegator's constraints")
}

// TestMemoInstall_SourceAgentEdgeFaultNotServedFromMemo fails the second
// delegation edge lookup made while walking a progeny secret's source agent
// chain. The first decision's walk succeeds; if the walk shared the edges
// memo, the second decision would be served from it and allow. It must
// instead reach the store, observe the fault and deny, exactly like the
// no-memo reference.
func TestMemoInstall_SourceAgentEdgeFaultNotServedFromMemo(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newSourceAgentChainFixture(t, s, "chain-edge")
	bg := context.Background()

	run := func(ctx context.Context) ([]Decision, []*store.DecisionAuditRecord, int) {
		reached := 0
		st := newMemoTestStore(s)
		st.fault = onlyAtCall("GetDelegationEdgesForDelegate", "relationshipSourceDelegationHolds", 2, &reached)
		authz, emit := newRecordingAuthz(st)
		out := []Decision{f.decide(authz, ctx), f.decide(authz, ctx)}
		return out, emit.snapshot(), reached
	}
	ref, refAudit, refReached := run(bg)
	cand, candAudit, candReached := run(withAuthzInputMemo(bg))

	require.True(t, ref[0].Allowed, "reference decision 1 precedes the fault and must allow: %q", ref[0].Reason)
	require.False(t, ref[1].Allowed, "reference decision 2 must deny on the edge fault")
	assert.Equal(t, "relationship grant restricted by source_inactive", ref[1].Reason)
	for i := range ref {
		assertDecisionsEqual(t, ref[i], cand[i], "decision %d", i)
	}
	assertAuditSequenceEqual(t, refAudit, candAudit)
	assert.Equal(t, 2, refReached, "the reference walk must reach the store on both decisions")
	assert.Equal(t, 2, candReached, "the memo-run walk must reach the store on both decisions")
}

// TestMemoInstall_DelegationChainWalkMasksInputsIntrinsically calls the
// delegation chain walk directly on a memo ctx with no outer mask, after
// priming the memo with the delegator's own inputs. The walk must load the
// delegator's inputs and constraints from the store without observing the
// input memo, and may share delegation edges through the edges memo.
func TestMemoInstall_DelegationChainWalkMasksInputsIntrinsically(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newA1Fixture(t, s, "walk-mask")
	bg := context.Background()

	mstore := newMemoTestStore(s)
	authz, _ := newRecordingAuthz(mstore)
	mctx := withAuthzInputMemo(bg)
	delegator := NewAuthenticatedUser(f.delegatorID, "walk-mask-delegator@test.com", "delegator", "member", "api")
	projectRes := Resource{Type: "project", ID: f.projectID}
	require.True(t, authz.CheckAccess(mctx, delegator, projectRes, ActionRead).Allowed, "priming: the delegator reads its project")
	require.True(t, authz.CheckAccess(mctx, f.agent, projectRes, ActionRead).Allowed, "priming: the agent reads its project")

	mstore.mu.Lock()
	mstore.recordCalls = true
	mstore.mu.Unlock()
	before := mstore.snapshotCounts()
	for i := 0; i < 2; i++ {
		allowed, reason, err := authz.walkDelegationChain(mctx, projectRes, ActionRead, "project.read", f.agentID, true, store.RoleScopeProject, f.projectID, nil)
		require.NoError(t, err)
		require.True(t, allowed, "walk %d must allow: %q", i, reason)
	}
	after := mstore.snapshotCounts()

	recs := mstore.recordedCalls()
	require.NotEmpty(t, recs)
	for _, rec := range recs {
		assert.False(t, rec.hasInputMemo, "%s #%d inside the walk observed the input memo", rec.method, rec.n)
		if rec.method == "GetDelegationEdgesForDelegate" {
			assert.True(t, rec.hasEdgesMemo, "edge lookups inside the walk share the edges memo")
		}
	}
	assert.GreaterOrEqual(t, after["ListAccessConstraints"]-before["ListAccessConstraints"], 2, "each walk loads the delegator's constraints from the store")
	assert.GreaterOrEqual(t, after["ListRoleBindingsForPrincipals"]-before["ListRoleBindingsForPrincipals"], 2, "each walk loads the delegator's bindings from the store")
	assert.Equal(t, 0, after["GetDelegationEdgesForDelegate"]-before["GetDelegationEdgesForDelegate"], "the agent's edges were memoized by the priming decision")
}

// TestMemoInstall_ExecutionProjectAdmissionMasksInputsIntrinsically calls
// execution-project admission directly on a memo ctx with no outer mask,
// after priming the memo's constraint slot. Admitting the source user on
// system authority must load the constraint table from the store without
// observing the input memo.
func TestMemoInstall_ExecutionProjectAdmissionMasksInputsIntrinsically(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newSystemAuthoritySourceFixture(t, s, "admission-mask")
	bg := context.Background()

	mstore := newMemoTestStore(s)
	authz, _ := newRecordingAuthz(mstore)
	mctx := withAuthzInputMemo(bg)
	source := NewAuthenticatedUser(f.sourceUserID, "admission-mask@test.com", "source", "member", "api")
	authz.CheckAccess(mctx, source, f.skillRes, ActionRead)
	authz.CheckAccess(mctx, f.agent, f.skillRes, ActionRead)

	mstore.mu.Lock()
	mstore.recordCalls = true
	mstore.mu.Unlock()
	before := mstore.countOf("ListAccessConstraints")
	ok, reason := authz.executionProjectAdmission(mctx, principalContextForIdentity(f.agent), "skill.read")
	require.True(t, ok, "admission must pass on system authority: %q", reason)

	recs := mstore.recordedCalls()
	require.NotEmpty(t, recs)
	for _, rec := range recs {
		assert.False(t, rec.hasInputMemo, "%s #%d inside admission observed the input memo", rec.method, rec.n)
	}
	assert.Equal(t, 1, mstore.countOf("ListAccessConstraints")-before, "admission loads the constraint table from the store")
}

// TestMemoInstall_ExecutionSourceResolverIsNotCached pins that resolving a
// progeny item's execution source reads delegation edges from the store on
// every decision, even under an installed memo, exactly as often as with
// the memo masked.
func TestMemoInstall_ExecutionSourceResolverIsNotCached(t *testing.T) {
	_, s := authzTestSetup(t)
	sa := newSystemAuthoritySourceFixture(t, s, "resolver-uncached")
	const decisions = 3
	resolverEdgeLoads := func(ctx context.Context) int {
		mstore := newMemoTestStore(s)
		mstore.recordCalls = true
		authz, _ := newRecordingAuthz(mstore)
		for i := 0; i < decisions; i++ {
			d := authz.CheckAccess(ctx, sa.agent, sa.skillRes, ActionRead)
			require.True(t, d.Allowed, "system-authority progeny skill read must be allowed: %q", d.Reason)
		}
		n := 0
		for _, rec := range mstore.recordedCalls() {
			if rec.method == "GetDelegationEdgesForDelegate" && stackHas(rec.stack, "ResolveExecutionSource") {
				n++
			}
		}
		return n
	}
	bg := context.Background()
	ref := resolverEdgeLoads(maskAllAuthzMemo(bg))
	got := resolverEdgeLoads(withAuthzInputMemo(bg))
	assert.GreaterOrEqual(t, ref, decisions, "the resolver reads edges on every decision")
	assert.Equal(t, ref, got, "the resolver's edge reads are not served from the memo")

	// Admission reached directly on a memo ctx, outside the decision path's
	// outer mask, with the agent's edges already in the memo: the resolver
	// must still read every link from the store.
	admissionEdgeLoads := func(ctx context.Context) int {
		mstore := newMemoTestStore(s)
		authz, _ := newRecordingAuthz(mstore)
		_, err := authz.getCachedDelegationEdges(ctx, store.DelegationPrincipalAgent, sa.agentID)
		require.NoError(t, err)
		mstore.mu.Lock()
		mstore.recordCalls = true
		mstore.mu.Unlock()
		for i := 0; i < decisions; i++ {
			ok, reason := authz.executionProjectAdmission(ctx, principalContextForIdentity(sa.agent), "skill.read")
			require.True(t, ok, "admission must pass on system authority: %q", reason)
		}
		n := 0
		for _, rec := range mstore.recordedCalls() {
			if rec.method == "GetDelegationEdgesForDelegate" && stackHas(rec.stack, "ResolveExecutionSource") {
				n++
			}
		}
		return n
	}
	assert.Equal(t, decisions, admissionEdgeLoads(maskAllAuthzMemo(bg)), "masked: one edge read per admission")
	assert.Equal(t, decisions, admissionEdgeLoads(withAuthzInputMemo(bg)), "memo: the resolver does not read edges through the memo")
}

// TestMemoInstall_LaterEdgeFaultServedFromMemoAtInstallSites pins the
// accepted behaviour of the production install sites for a transient
// delegation edge failure that follows a successful lookup in the same
// call: the requester's later decisions are served from the memo and match
// a no-fault run, for read and write decisions alike, while the per-decision
// reference observes the failure and fails closed. A failure on the first
// lookup is not memoized and still denies.
func TestMemoInstall_LaterEdgeFaultServedFromMemoAtInstallSites(t *testing.T) {
	bg := context.Background()
	edgeFault := func(n int) func(string, int, context.Context) error {
		return callNFault("GetDelegationEdgesForDelegate", n)
	}

	t.Run("read", func(t *testing.T) {
		_, s := authzTestSetup(t)
		f := newA1Fixture(t, s, "install-edge-read")
		projectRes := Resource{Type: "project", ID: f.projectID}
		resources := []Resource{projectRes, projectRes}

		refStore := newMemoTestStore(s)
		refStore.fault = edgeFault(2)
		refAuthz, _ := newRecordingAuthz(refStore)
		rctx := contextWithIdentity(bg, f.agent)
		ref := []Decision{refAuthz.DecideFromContext(rctx, projectRes, ActionRead), refAuthz.DecideFromContext(rctx, projectRes, ActionRead)}
		require.True(t, ref[0].Allowed, "reference read 1 precedes the fault: %q", ref[0].Reason)
		require.False(t, ref[1].Allowed, "reference read 2 fails closed on the edge fault")
		require.Equal(t, DenyCauseCeilingError, ref[1].DenyCause)

		cleanAuthz, _ := newRecordingAuthz(newMemoTestStore(s))
		clean, err := cleanAuthz.AuthorizeReadBatch(bg, f.agent, resources)
		require.NoError(t, err)
		require.Equal(t, []bool{true, true}, clean)

		candStore := newMemoTestStore(s)
		candStore.fault = edgeFault(2)
		candAuthz, _ := newRecordingAuthz(candStore)
		got, err := candAuthz.AuthorizeReadBatch(bg, f.agent, resources)
		require.NoError(t, err)
		assert.Equal(t, clean, got, "the install site serves the second read from the memo")
		assert.Equal(t, 1, candStore.countOf("GetDelegationEdgesForDelegate"), "the second lookup never reaches the store")

		firstStore := newMemoTestStore(s)
		firstStore.fault = edgeFault(1)
		firstAuthz, _ := newRecordingAuthz(firstStore)
		first, err := firstAuthz.AuthorizeReadBatch(bg, f.agent, resources)
		require.NoError(t, err)
		assert.Equal(t, []bool{false, true}, first, "a failure on the first lookup is not memoized and denies that read")
	})

	t.Run("write", func(t *testing.T) {
		_, s := authzTestSetup(t)
		f := newA1Fixture(t, s, "install-edge-write")
		resources := []Resource{f.resource, f.resource}
		actions := []Action{ActionLifecycle}

		refStore := newMemoTestStore(s)
		refStore.fault = edgeFault(2)
		refAuthz, _ := newRecordingAuthz(refStore)
		ref := []Decision{refAuthz.CheckAccess(bg, f.agent, f.resource, ActionLifecycle), refAuthz.CheckAccess(bg, f.agent, f.resource, ActionLifecycle)}
		require.True(t, ref[0].Allowed, "reference write 1 precedes the fault: %q", ref[0].Reason)
		require.False(t, ref[1].Allowed, "reference write 2 fails closed on the edge fault")
		require.Equal(t, DenyCauseCeilingError, ref[1].DenyCause)

		cleanAuthz, _ := newRecordingAuthz(newMemoTestStore(s))
		clean := cleanAuthz.ComputeCapabilitiesForActions(bg, f.agent, resources, actions)
		require.Equal(t, []string{string(ActionLifecycle)}, clean[1].Actions)

		candStore := newMemoTestStore(s)
		candStore.fault = edgeFault(2)
		candAuthz, _ := newRecordingAuthz(candStore)
		got := candAuthz.ComputeCapabilitiesForActions(bg, f.agent, resources, actions)
		for i := range clean {
			assert.Equal(t, clean[i].Actions, got[i].Actions, "resource %d: the install site serves the second write from the memo", i)
		}
		assert.Equal(t, 1, candStore.countOf("GetDelegationEdgesForDelegate"), "the second lookup never reaches the store")

		maskedStore := newMemoTestStore(s)
		maskedStore.fault = edgeFault(2)
		maskedAuthz, _ := newRecordingAuthz(maskedStore)
		masked := maskedAuthz.ComputeCapabilitiesForActions(maskAllAuthzMemo(bg), f.agent, resources, actions)
		assert.Equal(t, []string{string(ActionLifecycle)}, masked[0].Actions, "with the memo masked the first write is unaffected")
		assert.Empty(t, masked[1].Actions, "with the memo masked the second write fails closed, as in the reference")
	})
}

// capsParityPrincipal is one principal for the capability parity tests,
// with the agent resources to evaluate it against.
type capsParityPrincipal struct {
	name      string
	identity  Identity
	resources []Resource
	user      bool // a user principal, whose input loads are all memoized
}

func capsParityPrincipals(t *testing.T, s store.Store) []capsParityPrincipal {
	t.Helper()
	ctx := context.Background()
	var out []capsParityPrincipal

	groupFx := newP1Fixture(t, s, "caps-group-member")
	out = append(out, capsParityPrincipal{"group_member", groupFx.user, []Resource{groupFx.agentRes}, true})

	directFx := newProjectPrincipalFixture(t, s, "caps-direct-member", store.ProjectRoleMember)
	out = append(out, capsParityPrincipal{"direct_member", directFx.user, []Resource{directFx.agentRes}, true})

	ownerFx := newProjectPrincipalFixture(t, s, "caps-owner", store.ProjectRoleOwner)
	ownedID := tid("caps-owner-owned")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: ownedID, Slug: "caps-owner-owned", Name: "caps-owner-owned", ProjectID: ownerFx.projectID, Phase: "running", OwnerID: ownerFx.userID, Ancestry: []string{ownerFx.userID}}))
	owned := Resource{Type: "agent", ID: ownedID, ParentType: "project", ParentID: ownerFx.projectID, OwnerID: ownerFx.userID}
	out = append(out, capsParityPrincipal{"project_owner", ownerFx.user, []Resource{owned, ownerFx.agentRes}, true})

	adminFx := newProjectPrincipalFixture(t, s, "caps-admin", store.ProjectRoleAdmin)
	out = append(out, capsParityPrincipal{"project_admin", adminFx.user, []Resource{adminFx.agentRes}, true})

	hubAdminID := tid("caps-hub-admin")
	createTestUserWithRole(t, s, hubAdminID, "caps-hub-admin@test.com", "admin", store.SystemRoleHubAdmin)
	out = append(out, capsParityPrincipal{"hub_admin", NewAuthenticatedUser(hubAdminID, "caps-hub-admin@test.com", "hub-admin", "admin", "api"), []Resource{groupFx.agentRes, adminFx.agentRes}, true})

	superID := tid("caps-super-admin")
	createTestUserWithRole(t, s, superID, "caps-super-admin@test.com", "admin", store.SystemRoleSuperAdmin)
	out = append(out, capsParityPrincipal{"super_admin", NewAuthenticatedUser(superID, "caps-super-admin@test.com", "super-admin", "admin", "api"), []Resource{groupFx.agentRes}, true})

	nonMemberID := tid("caps-non-member")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: nonMemberID, Email: "caps-non-member@test.com", DisplayName: "non-member", Role: "member", Status: "active"}))
	out = append(out, capsParityPrincipal{"non_member", NewAuthenticatedUser(nonMemberID, "caps-non-member@test.com", "non-member", "member", "api"), []Resource{groupFx.agentRes}, true})

	agentFx := newA1Fixture(t, s, "caps-agent")
	ownRes := Resource{Type: "agent", ID: agentFx.agentID, ParentType: "project", ParentID: agentFx.projectID, OwnerID: agentFx.delegatorID}
	out = append(out, capsParityPrincipal{"agent", agentFx.agent, []Resource{agentFx.resource, ownRes}, false})

	ceiling, ok := permissions.BuildCeilingFromSelectors([]string{"agent:read"})
	require.True(t, ok)
	scoped := NewScopedUserIdentityWithCeiling(groupFx.user, groupFx.projectID, []string{"agent:read"}, "caps-uat", ceiling)
	out = append(out, capsParityPrincipal{"scoped_token", scoped, []Resource{groupFx.agentRes}, true})
	return out
}

// TestMemoInstall_CapabilitiesForActionsMatchesBaseline compares the
// per-action capability function, which installs the memo itself, with a
// baseline run whose memo is masked (so every decision loads its inputs
// from the store, as with no memo at all), and with a run nested inside an
// outer memo, for each principal class and for the read-only, remaining
// and full action sets. Capabilities and the audit sequence must match;
// the full action set must also match the batch function, and merging the
// read and remaining results must reproduce it.
func TestMemoInstall_CapabilitiesForActionsMatchesBaseline(t *testing.T) {
	_, s := authzTestSetup(t)
	bg := context.Background()
	all := ResourceActions["agent"]
	require.Equal(t, ActionRead, all[0], "the agent action order starts with read")
	sets := map[string][]Action{"read": all[:1], "rest": all[1:], "all": all}

	for _, p := range capsParityPrincipals(t, s) {
		for _, setName := range []string{"read", "rest", "all"} {
			actions := sets[setName]
			t.Run(p.name+"/"+setName, func(t *testing.T) {
				run := func(ctx context.Context) ([]*Capabilities, []*store.DecisionAuditRecord, *memoTestStore) {
					st := newMemoTestStore(s)
					authz, emit := newRecordingAuthz(st)
					caps := authz.ComputeCapabilitiesForActions(ctx, p.identity, p.resources, actions)
					return caps, emit.snapshot(), st
				}
				refCaps, refAudit, refStore := run(maskAllAuthzMemo(bg))
				selfCaps, selfAudit, selfStore := run(bg)
				outerCaps, outerAudit, _ := run(withAuthzInputMemo(bg))

				decisions := len(p.resources) * len(actions)
				require.Len(t, refAudit, decisions, "one audit record per decision")
				require.Len(t, refCaps, len(p.resources))
				for i := range refCaps {
					assert.Equal(t, refCaps[i].Actions, selfCaps[i].Actions, "resource %d: self-installed memo", i)
					assert.Equal(t, refCaps[i].Actions, outerCaps[i].Actions, "resource %d: outer memo", i)
				}
				assertAuditSequenceEqual(t, refAudit, selfAudit, "self-installed memo")
				assertAuditSequenceEqual(t, refAudit, outerAudit, "outer memo")

				if p.user {
					for _, m := range []string{"GetEffectiveGroups", "ListRoleBindingsForPrincipals", "GetRoleDefinitionsByIDs", "ListAccessConstraints"} {
						assert.LessOrEqual(t, selfStore.countOf(m), 1, "%s at most once per call", m)
					}
					if refStore.countOf("GetEffectiveGroups") > 0 {
						assert.Equal(t, 1, selfStore.countOf("GetEffectiveGroups"), "groups loaded exactly once per call")
					}
					if p.name == "group_member" {
						assert.Equal(t, decisions, refStore.countOf("GetEffectiveGroups"), "the masked baseline loads groups once per decision")
					}
				}
			})
		}

		t.Run(p.name+"/matches_batch", func(t *testing.T) {
			authz, _ := newRecordingAuthz(newMemoTestStore(s))
			for _, ctx := range []context.Context{bg, maskAllAuthzMemo(bg)} {
				batch := authz.ComputeCapabilitiesBatch(ctx, p.identity, p.resources, "agent")
				full := authz.ComputeCapabilitiesForActions(ctx, p.identity, p.resources, all)
				read := authz.ComputeCapabilitiesForActions(ctx, p.identity, p.resources, all[:1])
				rest := authz.ComputeCapabilitiesForActions(ctx, p.identity, p.resources, all[1:])
				for i := range batch {
					assert.Equal(t, batch[i].Actions, full[i].Actions, "resource %d: full action set equals the batch", i)
					assert.Equal(t, batch[i].Actions, mergeCapabilities(all, read[i], rest[i]).Actions, "resource %d: merged read and remaining equal the batch", i)
				}
			}
		})
	}
}

// TestMemoInstall_BrokerOnBehalfOfMatchesBaseline runs the per-action
// capability function and the batch read authorizer on the ctx a broker
// installs when acting on behalf of a local user, with the memo masked,
// self-installed and installed by an outer caller. Capabilities, read
// results and the audit sequence must match, and a plain request and an
// on-behalf-of request for the same user inside one memo share its loads.
func TestMemoInstall_BrokerOnBehalfOfMatchesBaseline(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "install-obo")
	brokerID := tid("install-obo-broker")
	broker := NewBrokerIdentity(brokerID)
	onBehalfOf := func(ctx context.Context) context.Context {
		ctx = contextWithBrokerIdentity(ctx, broker)
		ctx = contextWithBrokerOnBehalfOf(ctx, BrokerOnBehalfOf{Broker: broker, BrokerID: brokerID})
		return contextWithIdentity(ctx, f.user)
	}
	oboCtx := onBehalfOf(context.Background())
	_, ok := BrokerOnBehalfOfFromContext(oboCtx)
	require.True(t, ok, "the ctx carries the on-behalf-of marker")

	actions := ResourceActions["agent"]
	resources := []Resource{f.agentRes, f.agentRes}
	// run returns the GetEffectiveGroups count after each of the two calls,
	// so a missing install at either call is visible on its own.
	run := func(ctx context.Context) ([]*Capabilities, []bool, []*store.DecisionAuditRecord, [2]int) {
		st := newMemoTestStore(s)
		authz, emit := newRecordingAuthz(st)
		var groups [2]int
		caps := authz.ComputeCapabilitiesForActions(ctx, f.user, resources, actions)
		groups[0] = st.countOf("GetEffectiveGroups")
		reads, err := authz.AuthorizeReadBatch(ctx, f.user, resources)
		require.NoError(t, err)
		groups[1] = st.countOf("GetEffectiveGroups") - groups[0]
		return caps, reads, emit.snapshot(), groups
	}
	refCaps, refReads, refAudit, refGroups := run(maskAllAuthzMemo(oboCtx))
	require.Equal(t, [2]int{len(resources) * len(actions), len(resources)}, refGroups, "masked: every decision loads groups")
	require.Len(t, refAudit, len(resources)*(len(actions)+1), "one audit record per decision")
	require.Contains(t, refCaps[0].Actions, string(ActionRead), "the member can read the agent")
	require.Equal(t, []bool{true, true}, refReads)
	for name, tc := range map[string]struct {
		ctx    context.Context
		groups [2]int
	}{
		"self":  {oboCtx, [2]int{1, 1}},
		"outer": {withAuthzInputMemo(oboCtx), [2]int{1, 0}},
	} {
		caps, reads, audit, groups := run(tc.ctx)
		for i := range refCaps {
			assert.Equal(t, refCaps[i].Actions, caps[i].Actions, "%s memo: resource %d capabilities", name, i)
		}
		assert.Equal(t, refReads, reads, "%s memo: read results", name)
		assertAuditSequenceEqual(t, refAudit, audit, name+" memo")
		assert.Equal(t, tc.groups, groups, "%s memo: groups loads per call", name)
	}

	st := newMemoTestStore(s)
	authz, _ := newRecordingAuthz(st)
	plainCtx := withAuthzInputMemo(context.Background())
	authz.ComputeCapabilitiesForActions(onBehalfOf(plainCtx), f.user, resources, actions)
	authz.CheckAccess(plainCtx, f.user, f.agentRes, ActionRead)
	assert.Equal(t, 1, st.countOf("GetEffectiveGroups"), "plain and on-behalf-of decisions for one user share one memo entry")
}

// listRequestAuth sets a request's credential.
type listRequestAuth func(r *http.Request)

func userListAuth(t *testing.T, srv *Server, u *store.User) listRequestAuth {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
	require.NoError(t, err)
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func bearerListAuth(key string) listRequestAuth {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }
}

func agentListAuth(token string) listRequestAuth {
	return func(r *http.Request) { r.Header.Set("X-Scion-Agent-Token", token) }
}

// doListRequest issues a GET through the full handler chain. With masked
// set, the request ctx carries a masked memo, so the handler's install is
// a no-op and every authorization decision loads its own inputs, as with
// no memo at all.
func doListRequest(srv *Server, path string, auth listRequestAuth, masked bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	auth(req)
	if masked {
		req = req.WithContext(maskAllAuthzMemo(req.Context()))
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// listRun is one list request's observable result.
type listRun struct {
	status int
	body   string
	audits []store.DecisionAuditRecord
}

func runListRequest(t *testing.T, srv *Server, path string, auth listRequestAuth, masked bool) listRun {
	t.Helper()
	emit := &parityRecordingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emit)
	srv.authzService.DecisionAuditSampleRate = 1.0
	rec := doListRequest(srv, path, auth, masked)
	return listRun{status: rec.Code, body: withoutServerTime(t, rec.Body.Bytes()), audits: normalizedAudits(emit.snapshot())}
}

// withoutServerTime returns a JSON body re-encoded without its top-level
// serverTime field, the only part of a list response that depends on when
// it was produced. Non-object bodies are returned unchanged.
func withoutServerTime(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return string(body)
	}
	delete(obj, "serverTime")
	out, err := json.Marshal(obj)
	require.NoError(t, err)
	return string(out)
}

func assertListRunsEqual(t *testing.T, ref, got listRun, label string) {
	t.Helper()
	assert.Equal(t, ref.status, got.status, "%s: status", label)
	assert.Equal(t, ref.body, got.body, "%s: response body", label)
	assert.Equal(t, ref.audits, got.audits, "%s: decision audit records", label)
}

// listEndpointFixture extends the sorted-list fixture with agents owned by
// two users, a project admin, a hub admin outside the project, a hub admin
// who is also a project member, a hub member outside the project, a scoped
// user token and an agent token.
type listEndpointFixture struct {
	*sortedListFixture
	admin          *store.User
	hubAdmin       *store.User
	hubAdminMember *store.User
	nonMember      *store.User
	uatKey         string
	agentToken     string
}

func newListEndpointFixture(t *testing.T) *listEndpointFixture {
	t.Helper()
	f := &listEndpointFixture{sortedListFixture: sortedListSetup(t)}
	ctx := context.Background()
	f.createAgent(t, "owner-a", "running", map[string]string{"team": "a"})
	f.createAgent(t, "owner-b", "stopped", map[string]string{"team": "b"})
	self := f.createAgent(t, "owner-c", "running", nil)
	for i := 0; i < 2; i++ {
		a := &store.Agent{
			ID: tid(fmt.Sprintf("sl-member-agent-%d", i)), Slug: fmt.Sprintf("member-agent-%d", i), Name: fmt.Sprintf("member-agent-%d", i),
			ProjectID: f.project.ID, Phase: "running", CreatedBy: f.member.ID, OwnerID: f.member.ID,
		}
		require.NoError(t, f.store.CreateAgent(ctx, a))
	}

	f.admin = &store.User{ID: tid("sl-admin"), Email: "sl-admin@test.com", DisplayName: "Admin", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, f.store.CreateUser(ctx, f.admin))
	ensureHubMembership(ctx, f.store, f.admin.ID)
	createTestUserWithProjectRole(t, f.store, f.admin.ID, f.admin.Email, f.project.ID, store.ProjectRoleAdmin)

	createTestUserWithRole(t, f.store, tid("sl-hub-admin"), "sl-hub-admin@test.com", store.UserRoleAdmin, store.SystemRoleHubAdmin)
	hubAdmin, err := f.store.GetUser(ctx, tid("sl-hub-admin"))
	require.NoError(t, err)
	ensureHubMembership(ctx, f.store, hubAdmin.ID)
	f.hubAdmin = hubAdmin

	createTestUserWithRole(t, f.store, tid("sl-hub-admin-member"), "sl-hub-admin-member@test.com", store.UserRoleAdmin, store.SystemRoleHubAdmin)
	msgAuthzAddProjectMember(t, f.store, tid("sl-hub-admin-member"), f.project.ID, f.project.Slug, store.GroupMemberRoleMember)
	hubAdminMember, err := f.store.GetUser(ctx, tid("sl-hub-admin-member"))
	require.NoError(t, err)
	ensureHubMembership(ctx, f.store, hubAdminMember.ID)
	f.hubAdminMember = hubAdminMember

	f.nonMember = &store.User{ID: tid("sl-non-member"), Email: "sl-non-member@test.com", DisplayName: "Non-member", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, f.store.CreateUser(ctx, f.nonMember))
	ensureHubMembership(ctx, f.store, f.nonMember.ID)

	f.uatKey = mintScopedUAT(t, f.srv, f.owner.ID, f.project.ID, []string{"agent:read", "agent:list", "project:read"})
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	tok, err := svc.GenerateAgentToken(self.ID, f.project.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)
	f.agentToken = tok
	return f
}

// TestMemoInstall_ProjectAgentListMatchesBaseline compares project agent
// list responses and decision audits with and without the handler's memo,
// in legacy mode and in both sorted branches (complete, and paged with a
// cursor), for each caller class.
func TestMemoInstall_ProjectAgentListMatchesBaseline(t *testing.T) {
	f := newListEndpointFixture(t)
	callers := []struct {
		name    string
		auth    listRequestAuth
		allowed bool // the caller may list this project's agents
	}{
		{"owner", userListAuth(t, f.srv, f.owner), true},
		{"member", userListAuth(t, f.srv, f.member), true},
		{"admin", userListAuth(t, f.srv, f.admin), true},
		{"hub_admin", userListAuth(t, f.srv, f.hubAdmin), false},
		{"hub_admin_member", userListAuth(t, f.srv, f.hubAdminMember), true},
		{"non_member", userListAuth(t, f.srv, f.nonMember), false},
		{"scoped_token", bearerListAuth(f.uatKey), true},
		{"agent", agentListAuth(f.agentToken), true},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			queries := []string{"", "sort=updated&fit=500", "sort=updated&dir=asc&limit=2"}
			for _, q := range queries {
				path := f.listPath(q)
				ref := runListRequest(t, f.srv, path, c.auth, true)
				got := runListRequest(t, f.srv, path, c.auth, false)
				assertListRunsEqual(t, ref, got, "query "+q)
				if !c.allowed {
					assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, got.status, "query %q: %s", q, got.body)
					continue
				}
				require.Equal(t, http.StatusOK, got.status, "query %q: %s", q, got.body)
				var resp ListAgentsResponse
				require.NoError(t, json.Unmarshal([]byte(got.body), &resp))
				require.NotEmpty(t, ref.audits, "query %q: decisions were made", q)
				if strings.Contains(q, "limit=2") && resp.NextCursor != "" {
					next := f.listPath(q + "&cursor=" + url.QueryEscape(resp.NextCursor))
					assertListRunsEqual(t, runListRequest(t, f.srv, next, c.auth, true), runListRequest(t, f.srv, next, c.auth, false), "next page")
				} else if strings.Contains(q, "limit=2") {
					assert.Fail(t, "the paged query must return a cursor", "caller %s", c.name)
				}
			}
		})
	}
}

// TestMemoInstall_AgentListMatchesBaseline compares the hub-wide agent
// list response and decision audits with and without the handler's memo.
func TestMemoInstall_AgentListMatchesBaseline(t *testing.T) {
	f := newListEndpointFixture(t)
	callers := map[string]listRequestAuth{
		"owner":            userListAuth(t, f.srv, f.owner),
		"member":           userListAuth(t, f.srv, f.member),
		"admin":            userListAuth(t, f.srv, f.admin),
		"hub_admin":        userListAuth(t, f.srv, f.hubAdmin),
		"hub_admin_member": userListAuth(t, f.srv, f.hubAdminMember),
		"non_member":       userListAuth(t, f.srv, f.nonMember),
		"scoped_token":     bearerListAuth(f.uatKey),
		"agent":            agentListAuth(f.agentToken),
	}
	// Callers that see the project's agents, so that the project-filtered
	// requests make per-agent decisions.
	seesAgents := map[string]bool{"owner": true, "member": true, "admin": true, "hub_admin_member": true, "scoped_token": true}
	paths := []string{
		"/api/v1/agents",
		"/api/v1/agents?projectId=" + url.QueryEscape(f.project.ID),
		"/api/v1/agents?projectId=" + url.QueryEscape(f.project.Slug),
		"/api/v1/agents?projectId=no-such-project",
	}
	for _, name := range []string{"owner", "member", "admin", "hub_admin", "hub_admin_member", "non_member", "scoped_token", "agent"} {
		t.Run(name, func(t *testing.T) {
			bodies := map[string]string{}
			for _, path := range paths {
				ref := runListRequest(t, f.srv, path, callers[name], true)
				got := runListRequest(t, f.srv, path, callers[name], false)
				assertListRunsEqual(t, ref, got, path)
				assert.Equal(t, http.StatusOK, got.status, "%s: %s", path, got.body)
				if seesAgents[name] && path != paths[3] {
					assert.Greater(t, len(ref.audits), 4, "%s: per-agent decisions were made", path)
				}
				bodies[path] = got.body
			}
			assert.Equal(t, bodies[paths[1]], bodies[paths[2]], "a project slug filter matches the same agents as its ID")
			if seesAgents[name] {
				var resp ListAgentsResponse
				require.NoError(t, json.Unmarshal([]byte(bodies[paths[1]]), &resp))
				assert.NotEmpty(t, resp.Agents, "the project filter returns agents")
			}
		})
	}
}

// raceListResult is the part of a sorted list response that does not
// depend on fixture timestamps.
type raceListResult struct {
	status int
	ids    []string
	caps   [][]string
	scope  *Capabilities
	audits []store.DecisionAuditRecord
}

// withStablePolicyIDs replaces the role binding IDs audit records name,
// which are generated afresh for each fixture, with their order of first
// appearance, so records from two identical fixtures compare equal.
func withStablePolicyIDs(records []store.DecisionAuditRecord) []store.DecisionAuditRecord {
	seen := map[string]string{}
	stable := func(id string) string {
		if id == "" {
			return ""
		}
		if v, ok := seen[id]; ok {
			return v
		}
		v := fmt.Sprintf("policy-%d", len(seen)+1)
		seen[id] = v
		return v
	}
	out := make([]store.DecisionAuditRecord, len(records))
	for i, r := range records {
		r.MatchedPolicy, r.PolicyID, r.MatchedGrant = stable(r.MatchedPolicy), stable(r.PolicyID), stable(r.MatchedGrant)
		out[i] = r
	}
	return out
}

// TestMemoInstall_SortedListRaceRedecisionMatchesBaseline changes an agent
// between the sorted list's member read and its full-row read, so the
// handler re-decides that agent on the full row, and compares the outcome
// with and without the handler's memo. Each run builds a fresh, identical
// fixture because the change is applied once.
func TestMemoInstall_SortedListRaceRedecisionMatchesBaseline(t *testing.T) {
	run := func(t *testing.T, masked bool, ownerChange bool) raceListResult {
		f, raced, fault := sortedListSetupWithFault(t, newFieldMutatingAfterMembersStore)
		ctx := context.Background()
		caller := &store.User{ID: tid("sl-race-caller"), Email: "sl-race@test.com", DisplayName: "Caller", Role: store.UserRoleMember, Status: "active"}
		require.NoError(t, f.store.CreateUser(ctx, caller))
		ensureHubMembership(ctx, f.store, caller.ID)
		grantProjectListOnly(t, f.store, caller.ID, f.project.ID, "sl-race-role")
		a := &store.Agent{
			ID: tid("sl-race-agent"), Slug: "race-agent", Name: "race-agent", ProjectID: f.project.ID,
			Phase: "stopped", CreatedBy: caller.ID, OwnerID: caller.ID, Labels: map[string]string{"team": "a", "v": "1"},
		}
		require.NoError(t, f.store.CreateAgent(ctx, a))
		f.createAgent(t, "race-other", "stopped", map[string]string{"team": "a"})
		// fieldMutatingAfterMembersStore generalizes the owner- and
		// label-changing wrappers: same one-shot GetAgent/mutate/UpdateAgent
		// after the first member read.
		raced.agentID = a.ID
		if ownerChange {
			raced.mutate = func(row *store.Agent) { row.OwnerID = f.owner.ID }
		} else {
			raced.mutate = func(row *store.Agent) { row.Labels = map[string]string{"team": "a", "v": "2"} }
		}
		fault.Arm()
		r := runListRequest(t, f.srv, f.listPath("sort=updated&fit=500&label=team=a"), userListAuth(t, f.srv, caller), masked)
		out := raceListResult{status: r.status, audits: withStablePolicyIDs(r.audits)}
		if r.status == http.StatusOK {
			var resp ListAgentsResponse
			require.NoError(t, json.Unmarshal([]byte(r.body), &resp))
			for _, item := range resp.Agents {
				out.ids = append(out.ids, item.ID)
				var acts []string
				if item.Cap != nil {
					acts = item.Cap.Actions
				}
				out.caps = append(out.caps, acts)
			}
			out.scope = resp.Capabilities
		}
		return out
	}
	for _, tc := range []struct {
		name        string
		ownerChange bool
	}{{"owner_change", true}, {"label_change", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ref := run(t, true, tc.ownerChange)
			got := run(t, false, tc.ownerChange)
			require.Equal(t, http.StatusOK, ref.status)
			assert.Equal(t, ref, got)
			assert.Greater(t, len(ref.audits), 8, "the raced agent was re-decided on the full row")
		})
	}
}

// TestMemoInstall_ListEndpointsLoadInputsOncePerRequest counts the
// memoized store loads behind whole list requests from a user caller: with
// the handler's memo each request loads groups, bindings and role
// definitions once and the access constraint table once, however many
// agents are listed; a masked request pays them once per decision.
func TestMemoInstall_ListEndpointsLoadInputsOncePerRequest(t *testing.T) {
	loaders := []string{"GetEffectiveGroups", "ListRoleBindingsForPrincipals", "GetRoleDefinitionsByIDs", "ListAccessConstraints"}
	for _, n := range []int{1, 25} {
		f := sortedListSetup(t)
		f.createAgentsBulk(t, n, fmt.Sprintf("cnt-%d", n), "running", nil)
		auth := userListAuth(t, f.srv, f.owner)
		for _, ep := range []struct{ name, path string }{
			{"project_legacy", f.listPath("")},
			{"project_sorted_complete", f.listPath("sort=updated&fit=500")},
			{"project_sorted_paged", f.listPath("sort=updated&limit=10")},
			{"hub_agents", "/api/v1/agents"},
		} {
			t.Run(fmt.Sprintf("%s/N=%d", ep.name, n), func(t *testing.T) {
				counts := func(masked bool) (map[string]int, int) {
					mstore := newMemoTestStore(f.store)
					orig := f.srv.authzService.store
					f.srv.authzService.store = mstore
					defer func() { f.srv.authzService.store = orig }()
					r := runListRequest(t, f.srv, ep.path, auth, masked)
					require.Equal(t, http.StatusOK, r.status, r.body)
					return mstore.snapshotCounts(), len(r.audits)
				}
				memo, memoDecisions := counts(false)
				masked, maskedDecisions := counts(true)
				assert.Equal(t, maskedDecisions, memoDecisions, "the memo does not change the number of decisions")
				for _, m := range loaders {
					assert.Equal(t, 1, memo[m], "%s once per request with the memo", m)
				}
				assert.Greater(t, masked["GetEffectiveGroups"], memoDecisions/2, "the masked request loads groups per decision")
				t.Logf("%s N=%d decisions=%d loads with memo=%v masked=%v", ep.name, n, memoDecisions,
					[]int{memo[loaders[0]], memo[loaders[1]], memo[loaders[2]], memo[loaders[3]]},
					[]int{masked[loaders[0]], masked[loaders[1]], masked[loaders[2]], masked[loaders[3]]})
			})
		}

		t.Run(fmt.Sprintf("project_legacy_agent_caller/N=%d", n), func(t *testing.T) {
			selfID := tid(fmt.Sprintf("cnt-self-%d", n))
			createDCAgent(t, f.store, selfID, f.project.ID, f.owner.ID, AgentRoleFull)
			createDCEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, store.DelegationPrincipalAgent, selfID,
				store.RoleScopeProject, f.project.ID, string(AgentRoleFull))
			svc := f.srv.GetAgentTokenService()
			require.NotNil(t, svc)
			tok, err := svc.GenerateAgentToken(selfID, f.project.ID, ScopesForRole(AgentRoleFull), []string{f.owner.ID})
			require.NoError(t, err)
			counts := func(masked bool) (map[string]int, int) {
				mstore := newMemoTestStore(f.store)
				orig := f.srv.authzService.store
				f.srv.authzService.store = mstore
				defer func() { f.srv.authzService.store = orig }()
				r := runListRequest(t, f.srv, f.listPath(""), agentListAuth(tok), masked)
				require.Equal(t, http.StatusOK, r.status, r.body)
				return mstore.snapshotCounts(), len(r.audits)
			}
			memo, memoDecisions := counts(false)
			masked, maskedDecisions := counts(true)
			assert.Equal(t, maskedDecisions, memoDecisions, "the memo does not change the number of decisions")
			assert.Equal(t, 1, memo["GetEffectiveGroupsForAgent"], "the agent's groups are loaded once per request")
			assert.Equal(t, memoDecisions, masked["GetEffectiveGroupsForAgent"], "the masked request loads the agent's groups per decision")
			assert.Equal(t, 1, memo["GetDelegationEdgesForDelegate"], "the agent's delegation edges are loaded once per request")
			assert.Greater(t, masked["GetDelegationEdgesForDelegate"], 1, "the masked request loads delegation edges per allowed decision")
			for _, m := range []string{"GetEffectiveGroups", "GetUser", "GetRoleDefinition"} {
				assert.Equal(t, masked[m], memo[m], "%s: the delegator side of the delegation ceiling is not memoized", m)
			}
			t.Logf("agent caller N=%d decisions=%d memo=%v masked=%v", n, memoDecisions, memo, masked)
		})
	}
}

// TestMemoInstall_CapabilityFunctionsLoadInputsOncePerCall checks that
// each capability function, called with no outer memo, loads the caller's
// inputs once for all of its decisions and returns the same capabilities
// and audit records as a run with the memo masked.
func TestMemoInstall_CapabilityFunctionsLoadInputsOncePerCall(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "per-call-caps")
	bg := context.Background()
	other := f.agentRes
	other.ID = tid("per-call-caps-other")
	require.NoError(t, s.CreateAgent(bg, &store.Agent{ID: other.ID, Slug: "per-call-caps-other", Name: "per-call-caps-other", ProjectID: f.projectID, Phase: "running", OwnerID: f.agentRes.OwnerID}))

	calls := map[string]func(*AuthzService, context.Context) []*Capabilities{
		"single": func(a *AuthzService, ctx context.Context) []*Capabilities {
			return []*Capabilities{a.ComputeCapabilities(ctx, f.user, f.agentRes)}
		},
		"scope": func(a *AuthzService, ctx context.Context) []*Capabilities {
			return []*Capabilities{a.ComputeScopeCapabilities(ctx, f.user, "project", f.projectID, "agent")}
		},
		"batch": func(a *AuthzService, ctx context.Context) []*Capabilities {
			return a.ComputeCapabilitiesBatch(ctx, f.user, []Resource{f.agentRes, other}, "agent")
		},
		"actions": func(a *AuthzService, ctx context.Context) []*Capabilities {
			return a.ComputeCapabilitiesForActions(ctx, f.user, []Resource{f.agentRes, other}, ResourceActions["agent"])
		},
	}
	for _, name := range []string{"single", "scope", "batch", "actions"} {
		t.Run(name, func(t *testing.T) {
			run := func(ctx context.Context) ([]*Capabilities, []*store.DecisionAuditRecord, *memoTestStore) {
				st := newMemoTestStore(s)
				authz, emit := newRecordingAuthz(st)
				caps := calls[name](authz, ctx)
				return caps, emit.snapshot(), st
			}
			refCaps, refAudit, refStore := run(maskAllAuthzMemo(bg))
			caps, audit, st := run(bg)
			require.Greater(t, len(refAudit), 1, "the call makes several decisions")
			for i := range refCaps {
				assert.Equal(t, refCaps[i].Actions, caps[i].Actions, "capabilities %d", i)
			}
			assertAuditSequenceEqual(t, refAudit, audit)
			for _, m := range []string{"GetEffectiveGroups", "ListRoleBindingsForPrincipals", "GetRoleDefinitionsByIDs", "ListAccessConstraints"} {
				assert.Equal(t, 1, st.countOf(m), "%s once per call", m)
				assert.Equal(t, len(refAudit), refStore.countOf(m), "%s once per decision with the memo masked", m)
			}
		})
	}
}
