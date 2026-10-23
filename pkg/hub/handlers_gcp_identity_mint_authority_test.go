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

// Package hub — ptone/scion#2129 F.2h (first half): the authorization event
// at every GCP token mint (authorizeAgentGCPMint). Both mint endpoints decide
// gcp_service_account.use, whose step 10 is the account's parent ceiling (the
// recorded assignment source must currently hold gcp_service_account.assign
// on the exact account, within the recorded effect ceiling), and in enforce
// mode run actAs for that recorded source. Every test asserts both endpoints
// and, on a refusal, that the two answers are byte-identical and come from
// this layer (msgGCPMintNotAuthorized), not from an earlier record check.
package hub

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---- fixture helpers -------------------------------------------------------

// addAssigner adds an active project member (who holds
// gcp_service_account.assign on the fixture project's accounts) that is
// also a hub member, and returns its ID. It is not the agent's root, so
// changing it never changes the agent's standing.
func (f *gcpMintFixture) addAssigner(t *testing.T, name string) string {
	t.Helper()
	id := tid("assigner-" + name)
	createDCUser(t, f.Store, id, "assigner-"+name+"@test.com", f.ProjectID, store.ProjectRoleMember)
	ensureHubMembership(context.Background(), f.Store, id)
	return id
}

// recordAssignment replaces the agent's active assignment with one recorded
// from sourceUserID under a session credential with a principal ceiling,
// for the fixture account, then applies mutate.
func (f *gcpMintFixture) recordAssignment(t *testing.T, sourceUserID string, mutate func(*store.AgentServiceAccountAssignment)) *store.AgentServiceAccountAssignment {
	t.Helper()
	a := &store.AgentServiceAccountAssignment{
		AgentID: f.AgentID, ProjectID: f.ProjectID, ServiceAccountID: f.SA.ID,
		Origin:              store.SAAssignmentOriginCreateExplicit,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, sourceUserID, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	if mutate != nil {
		mutate(a)
	}
	require.NoError(t, f.Store.ReplaceAgentServiceAccountAssignment(context.Background(), a))
	return a
}

// mintProbes calls both mint endpoints with f's token.
func mintProbes(t *testing.T, srv *Server, token string) (access, identity oracleProbe) {
	t.Helper()
	access = probe(doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/gcp-token", nil, token))
	identity = probe(doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/gcp-identity-token",
		map[string]string{"audience": "https://example.com"}, token))
	return access, identity
}

// assertMintRefusedByAuthority asserts both endpoints refuse with 403 from
// authorizeAgentGCPMint, with byte-identical bodies.
func assertMintRefusedByAuthority(t *testing.T, f *gcpMintFixture) {
	t.Helper()
	requireMintRefusedByAuthority(t, f.Server, f.Token)
}

func requireMintRefusedByAuthority(t *testing.T, srv *Server, token string) {
	t.Helper()
	access, identity := mintProbes(t, srv, token)
	require.Equal(t, http.StatusForbidden, access.status, "access token: %s", access.body)
	require.Contains(t, access.body, msgGCPMintNotAuthorized, "the refusal comes from the mint authority layer")
	require.Equal(t, access, identity, "both endpoints give the same answer")
}

func requireMintAllowedOn(t *testing.T, srv *Server, token string) {
	t.Helper()
	access, identity := mintProbes(t, srv, token)
	require.Equal(t, http.StatusOK, access.status, "access token: %s", access.body)
	require.Equal(t, http.StatusOK, identity.status, "identity token: %s", identity.body)
}

// ---- parent ceiling: allow -------------------------------------------------

// TestAgentGCPToken_RecordedSourceWithAssignAuthorityAdmitted: a live
// recorded source holding assign on the exact account admits both endpoints.
func TestAgentGCPToken_RecordedSourceWithAssignAuthorityAdmitted(t *testing.T) {
	f := newGCPMintFixture(t, "source-allowed")
	assigner := f.addAssigner(t, "source-allowed")
	f.recordAssignment(t, assigner, nil)
	assertMintAllowed(t, f)
}

// TestAgentGCPToken_TokenCreatedAgentBoundedByIssuingCeiling: an assignment
// recorded from a user access token is bounded by that token's frozen
// ceiling. A ceiling without assign refuses even though the user's live role
// holds assign; the same user with assign in the ceiling is admitted.
func TestAgentGCPToken_TokenCreatedAgentBoundedByIssuingCeiling(t *testing.T) {
	f := newGCPMintFixture(t, "uat-ceiling")
	assigner := f.addAssigner(t, "uat-ceiling")
	uat := func(ids ...string) func(*store.AgentServiceAccountAssignment) {
		return func(a *store.AgentServiceAccountAssignment) {
			a.SourceCredentialKind = store.SourceCredentialUAT
			a.SourceCredentialID = tid("uat-ceiling-token")
			a.EffectCeiling = boundedCeiling(ids...)
		}
	}

	f.recordAssignment(t, assigner, uat("agent.create", "agent.read"))
	assertMintRefusedByAuthority(t, f)

	f.recordAssignment(t, assigner, uat("agent.create", "gcp_service_account.assign"))
	assertMintAllowed(t, f)
}

// ---- parent ceiling: refused above the ceiling -----------------------------

// TestAgentGCPToken_CreatorWithoutAssignAuthorityDenied (T-R8-4): the
// recorded source is an active project principal without assign.
func TestAgentGCPToken_CreatorWithoutAssignAuthorityDenied(t *testing.T) {
	f := newGCPMintFixture(t, "no-assign")
	noAssign := tid("no-assign-source")
	scaCreateDelegatorWithoutAssign(t, f.Store, noAssign, "no-assign-source@test.com", f.ProjectID)
	f.recordAssignment(t, noAssign, nil)
	assertMintRefusedByAuthority(t, f)
}

// TestAgentGCPToken_SourceLosesAssignAfterCreationDenied: a mint after the
// recorded source lost assign is refused; a past successful assignment is
// not enough.
func TestAgentGCPToken_SourceLosesAssignAfterCreationDenied(t *testing.T) {
	f := newGCPMintFixture(t, "assign-lost")
	assigner := f.addAssigner(t, "assign-lost")
	f.recordAssignment(t, assigner, nil)
	assertMintAllowed(t, f)

	_, err := f.Store.DeleteRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, assigner)
	require.NoError(t, err)
	assertMintRefusedByAuthority(t, f)
}

// TestAgentGCPToken_DefaultChangeGrantsNoAssignAuthority (T-R8-6): an
// assignment that came from a configured default records the requester as
// its source; the default itself grants nothing, so a source without assign
// is refused whatever the origin says.
func TestAgentGCPToken_DefaultChangeGrantsNoAssignAuthority(t *testing.T) {
	for _, origin := range []store.SAAssignmentOrigin{
		store.SAAssignmentOriginCreateProjectDefault,
		store.SAAssignmentOriginCreateProjectProfileDefault,
		store.SAAssignmentOriginCreateHubDefault,
	} {
		t.Run(string(origin), func(t *testing.T) {
			name := strings.ReplaceAll(string(origin), "_", "-")
			f := newGCPMintFixture(t, "default-"+name)
			noAssign := tid("default-no-assign-" + name)
			scaCreateDelegatorWithoutAssign(t, f.Store, noAssign, noAssign+"@test.com", f.ProjectID)
			f.recordAssignment(t, noAssign, func(a *store.AgentServiceAccountAssignment) { a.Origin = origin })
			assertMintRefusedByAuthority(t, f)
		})
	}
}

// TestAgentGCPToken_SourceNotLiveDenied: a suspended or missing recorded
// source is refused.
func TestAgentGCPToken_SourceNotLiveDenied(t *testing.T) {
	t.Run("suspended", func(t *testing.T) {
		f := newGCPMintFixture(t, "source-suspended")
		assigner := f.addAssigner(t, "source-suspended")
		f.recordAssignment(t, assigner, nil)
		u, err := f.Store.GetUser(context.Background(), assigner)
		require.NoError(t, err)
		u.Status = "suspended"
		require.NoError(t, f.Store.UpdateUser(context.Background(), u))
		assertMintRefusedByAuthority(t, f)
	})
	t.Run("missing", func(t *testing.T) {
		f := newGCPMintFixture(t, "source-missing")
		f.recordAssignment(t, tid("source-missing-user"), nil)
		assertMintRefusedByAuthority(t, f)
	})
}

// ---- parent ceiling: refused for missing provenance ------------------------

// TestAgentGCPToken_RevokedDelegationDenied: the assignment was deactivated
// (cleared) while the agent's applied identity still names the account.
func TestAgentGCPToken_RevokedDelegationDenied(t *testing.T) {
	f := newGCPMintFixture(t, "assignment-cleared")
	_, err := f.Store.DeactivateAgentServiceAccountAssignments(context.Background(), f.AgentID,
		store.Deactivation{Cause: store.EdgeDeactivationSACleared, OpID: "op-clear"})
	require.NoError(t, err)
	assertMintRefusedByAuthority(t, f)
}

// TestAgentGCPToken_MissingProvenanceDenied: an assignment whose provenance
// is not recorded, or whose source is not an accepted credential, is
// refused. No legacy "no row" allowance applies at the mint.
func TestAgentGCPToken_MissingProvenanceDenied(t *testing.T) {
	cases := map[string]func(*store.AgentServiceAccountAssignment){
		"provenance version 0": func(a *store.AgentServiceAccountAssignment) { a.ProvenanceVersion = 0 },
		"unrecorded ceiling":   func(a *store.AgentServiceAccountAssignment) { a.EffectCeiling = store.EffectCeiling{} },
		"migration source": func(a *store.AgentServiceAccountAssignment) {
			a.SourceCredentialKind = store.SourceCredentialSystemMigration
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newGCPMintFixture(t, "prov-"+strings.ReplaceAll(name, " ", "-"))
			f.recordAssignment(t, f.UserID, mutate)
			assertMintRefusedByAuthority(t, f)
		})
	}
}

// TestAgentGCPToken_AssignmentForAnotherAccountDenied: the recorded
// assignment names another account than the one the agent mints for.
func TestAgentGCPToken_AssignmentForAnotherAccountDenied(t *testing.T) {
	f := newGCPMintFixture(t, "assignment-other-sa")
	other := wiringSA(t, f.Store, store.ScopeProject, f.ProjectID, "assignment-other@my-gcp-project.iam.gserviceaccount.com")
	f.recordAssignment(t, f.UserID, func(a *store.AgentServiceAccountAssignment) { a.ServiceAccountID = other.ID })
	assertMintRefusedByAuthority(t, f)
}

// ---- lookup errors ---------------------------------------------------------

// mintAuthorityFaultStore fails the assignment or user lookup the parent
// ceiling makes. Installed on the AuthzService only, so the handler's own
// record checks still pass and only the authority layer sees the fault.
type mintAuthorityFaultStore struct {
	store.Store
	assignments bool
	userID      string
}

func (s *mintAuthorityFaultStore) GetActiveAgentServiceAccountAssignments(ctx context.Context, agentID string) ([]store.AgentServiceAccountAssignment, error) {
	if s.assignments {
		return nil, errors.New("injected store fault")
	}
	return s.Store.GetActiveAgentServiceAccountAssignments(ctx, agentID)
}

func (s *mintAuthorityFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.userID != "" && id == s.userID {
		return nil, errors.New("injected store fault")
	}
	return s.Store.GetUser(ctx, id)
}

// TestAgentGCPToken_AuthorityLookupErrorDenies (T-R8-5): a store fault in
// the parent ceiling refuses, never 500 and never an allow.
func TestAgentGCPToken_AuthorityLookupErrorDenies(t *testing.T) {
	t.Run("assignment lookup", func(t *testing.T) {
		f := newGCPMintFixture(t, "authority-fault-assign")
		f.Server.authzService.store = &mintAuthorityFaultStore{Store: f.Store, assignments: true}
		assertMintRefusedByAuthority(t, f)
	})
	t.Run("source lookup", func(t *testing.T) {
		f := newGCPMintFixture(t, "authority-fault-source")
		assigner := f.addAssigner(t, "authority-fault-source")
		f.recordAssignment(t, assigner, nil)
		f.Server.authzService.store = &mintAuthorityFaultStore{Store: f.Store, userID: assigner}
		assertMintRefusedByAuthority(t, f)
	})
}

// ---- actAs layer -----------------------------------------------------------

// TestAgentGCPToken_ActAsEnforceMode: with gcpIamCheckMode enforce, actAs is
// checked for the recorded source (never the agent) on the account; only a
// positive allow admits.
func TestAgentGCPToken_ActAsEnforceMode(t *testing.T) {
	cases := []struct {
		name    string
		script  func(c *store.FakeCallerPermissionChecker, email string)
		allowed bool
	}{
		{"allowed", func(c *store.FakeCallerPermissionChecker, email string) { c.AllowTarget(email) }, true},
		{"denied", func(c *store.FakeCallerPermissionChecker, email string) { c.DenyTarget(email, "no actAs grant") }, false},
		{"indeterminate", func(c *store.FakeCallerPermissionChecker, email string) {
			c.IndeterminateTarget(email, "troubleshooter returned UNKNOWN_INFO")
		}, false},
		{"check failed", func(c *store.FakeCallerPermissionChecker, email string) {
			c.FailTarget(email, errors.New("iam api timeout"))
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGCPMintFixture(t, "actas-"+strings.ReplaceAll(tc.name, " ", "-"))
			assigner := f.addAssigner(t, "actas-"+strings.ReplaceAll(tc.name, " ", "-"))
			f.recordAssignment(t, assigner, nil)
			checker := store.NewFakeCallerPermissionChecker()
			tc.script(checker, f.SA.Email)
			enforceSAAssign(f.Server, checker)

			if tc.allowed {
				requireMintAllowedOn(t, f.Server, f.Token)
			} else {
				requireMintRefusedByAuthority(t, f.Server, f.Token)
			}
			calls := checker.Calls()
			require.NotEmpty(t, calls, "actAs was checked")
			for _, c := range calls {
				assert.Equal(t, store.PrincipalUser, c.Caller.Kind, "the recorded source is the actAs caller")
				assert.Equal(t, assigner, c.Caller.ID)
				assert.Equal(t, f.SA.ID, c.TargetSAID)
			}
		})
	}
}

// TestAgentGCPToken_ActAsCheckerUnwiredDenies: enforce mode with no checker
// wired refuses.
func TestAgentGCPToken_ActAsCheckerUnwiredDenies(t *testing.T) {
	f := newGCPMintFixture(t, "actas-unwired")
	enforceSAAssign(f.Server, nil)
	assertMintRefusedByAuthority(t, f)
}

// TestAgentGCPToken_ActAsOffModeSkipsCheck: with mode off the Hub policy
// layer alone applies, as at assignment; a checker that would deny is not
// consulted.
func TestAgentGCPToken_ActAsOffModeSkipsCheck(t *testing.T) {
	f := newGCPMintFixture(t, "actas-off")
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(f.SA.Email, "would deny")
	f.Server.mu.Lock()
	f.Server.saAssignCheckMode = SAAssignCheckOff
	f.Server.saAssignChecker = checker
	f.Server.mu.Unlock()
	assertMintAllowed(t, f)
	assert.Zero(t, checker.CallCount(), "mode off makes no actAs call")
}

// TestAgentGCPToken_ActAsNotReachedWhenPolicyDenies: the Hub policy layer
// runs first; a refused use decision makes no actAs call.
func TestAgentGCPToken_ActAsNotReachedWhenPolicyDenies(t *testing.T) {
	f := newGCPMintFixture(t, "actas-after-policy")
	noAssign := tid("actas-after-policy-source")
	scaCreateDelegatorWithoutAssign(t, f.Store, noAssign, "actas-after-policy-source@test.com", f.ProjectID)
	f.recordAssignment(t, noAssign, nil)
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(f.SA.Email)
	enforceSAAssign(f.Server, checker)
	assertMintRefusedByAuthority(t, f)
	assert.Zero(t, checker.CallCount())
}

// ---- passthrough translation (OQ-1) ----------------------------------------

// ptrMintAgent is a translated passthrough agent of the ptr fixture whose
// root is the fixture owner and whose assignment source is sourceID, with an
// agent token naming the translation record.
func ptrMintAgent(t *testing.T, f *ptrFixture, slug, sourceID string) string {
	t.Helper()
	a := f.translatedAgent(t, slug, f.owner.ID, true)
	got := mustGetAgent(t, f.store, a.ID)
	got.Ancestry = []string{f.owner.ID}
	require.NoError(t, f.store.UpdateAgent(context.Background(), got))
	row := seedAssignment(t, f.store, got, sourceID, f.record.ID)
	require.Equal(t, sourceID, row.SourcePrincipalID)
	token, err := f.srv.agentTokenService.GenerateAgentToken(a.ID, f.project.ID,
		[]AgentTokenScope{GCPTokenScopeForSA(f.record.ID)}, []string{f.owner.ID})
	require.NoError(t, err)
	return token
}

// TestAgentGCPToken_PassthroughTranslationFollowsRecordedSource (OQ-1): a
// Cloud Run sandbox host-account passthrough records the requesting
// principal as the assignment source. A mint is admitted while that source
// live-holds assign on the exact host account and may act as it, and is
// refused once it loses assign or actAs. Passthrough is not exempt.
func TestAgentGCPToken_PassthroughTranslationFollowsRecordedSource(t *testing.T) {
	t.Run("source holds assign", func(t *testing.T) {
		f := setupPTR(t, "ptm-allow")
		token := ptrMintAgent(t, f, "ptm-allow-agent", f.other.ID)
		requireMintAllowedOn(t, f.srv, token)
	})

	t.Run("source loses assign after creation", func(t *testing.T) {
		f := setupPTR(t, "ptm-lost")
		token := ptrMintAgent(t, f, "ptm-lost-agent", f.other.ID)
		requireMintAllowedOn(t, f.srv, token)

		ctx := context.Background()
		group, err := f.store.GetGroupBySlug(ctx, hubMembersSlug)
		require.NoError(t, err)
		require.NoError(t, f.store.RemoveGroupMember(ctx, group.ID, store.GroupMemberTypeUser, f.other.ID))
		requireMintRefusedByAuthority(t, f.srv, token)
	})

	t.Run("source may not act as the host account", func(t *testing.T) {
		f := setupPTR(t, "ptm-actas")
		token := ptrMintAgent(t, f, "ptm-actas-agent", f.other.ID)
		enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().DenyTarget(ptrHostSA, "no actAs grant"))
		requireMintRefusedByAuthority(t, f.srv, token)
	})

	t.Run("source not a hub member", func(t *testing.T) {
		f := setupPTR(t, "ptm-outsider")
		outsider := &store.User{
			ID: tid("ptm-outsider-user"), Email: "ptm-outsider@test.com", DisplayName: "outsider",
			Role: store.UserRoleMember, Status: "active", Created: time.Now(),
		}
		require.NoError(t, f.store.CreateUser(context.Background(), outsider))
		token := ptrMintAgent(t, f, "ptm-outsider-agent", outsider.ID)
		requireMintRefusedByAuthority(t, f.srv, token)
	})
}

// ---- kernel wiring ---------------------------------------------------------

// TestGCPServiceAccountUse_DecisionCarriesParentProof: an allowed use
// decision names the parent proof in its reason, carries it for the actAs
// composition, and is always audited; a refused one carries the
// evaluator's cause.
func TestGCPServiceAccountUse_DecisionCarriesParentProof(t *testing.T) {
	f := newGCPMintFixture(t, "decision-proof")
	assigner := f.addAssigner(t, "decision-proof")
	row := f.recordAssignment(t, assigner, nil)
	agent := newFullAgentIdentity(f.AgentID, f.ProjectID, nil, []AgentTokenScope{GCPTokenScopeForSA(f.SA.ID)})
	req := AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpServiceAccountResource(f.SA),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	}

	d := f.Server.authzService.Decide(context.Background(), req)
	require.True(t, d.Allowed, "cause=%q reason=%q", d.DenyCause, d.Reason)
	assert.True(t, d.AlwaysAudit, "the use decision is never sampled away")
	assert.Contains(t, d.Reason, "parent proof: gcp_service_account.assign")
	require.NotNil(t, d.parentProof)
	assert.Equal(t, ParentProof{
		Permission:           "gcp_service_account.assign",
		SourcePrincipal:      ProvenancePrincipal{Kind: store.DelegationPrincipalUser, ID: assigner},
		SourceCredentialKind: store.SourceCredentialSession,
		AssignmentID:         row.ID,
		ServiceAccountID:     f.SA.ID,
	}, *d.parentProof)

	_, err := f.Store.DeactivateAgentServiceAccountAssignments(context.Background(), f.AgentID,
		store.Deactivation{Cause: store.EdgeDeactivationSACleared, OpID: "op-clear"})
	require.NoError(t, err)
	d = f.Server.authzService.Decide(context.Background(), req)
	require.False(t, d.Allowed)
	assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
	assert.Equal(t, DenyCauseCeilingProvenanceMissing, d.DenyCause)
	require.NotNil(t, d.parentProof)
}

// TestEvaluateResourceParentCeiling_UnmappedOrMismatchedDenies: a resource of
// another type, or a mapping without an evaluator, denies with
// ceiling_error and never reaches an evaluator.
func TestEvaluateResourceParentCeiling_UnmappedOrMismatchedDenies(t *testing.T) {
	authz, _ := authzTestSetup(t)
	m, ok := ResourceParentCeilingFor("gcp_service_account.use")
	require.True(t, ok)

	d := authz.evaluateResourceParentCeiling(context.Background(), m, tid("agent"), Resource{Type: "secret", ID: tid("sa")})
	assert.False(t, d.Allowed)
	assert.Equal(t, DenyCauseCeilingError, d.DenyCause)

	unknown := ResourceParentCeiling{DecisionPermission: "made.up", ParentPermission: "made.parent", ResourceType: "secret"}
	d = authz.evaluateResourceParentCeiling(context.Background(), unknown, tid("agent"), Resource{Type: "secret", ID: tid("x")})
	assert.False(t, d.Allowed)
	assert.Equal(t, DenyCauseCeilingError, d.DenyCause)
}

// TestParentSourceActAsPrincipal covers the actAs caller built from a
// recorded source: a user with its email; an agent with its own assign-mode
// email or none; a deleted agent, an unknown kind or an empty ID is an error.
func TestParentSourceActAsPrincipal(t *testing.T) {
	f := newGCPMintFixture(t, "source-principal")
	ctx := context.Background()

	p, err := f.Server.parentSourceActAsPrincipal(ctx, ProvenancePrincipal{Kind: store.DelegationPrincipalUser, ID: f.UserID})
	require.NoError(t, err)
	assert.Equal(t, store.Principal{Kind: store.PrincipalUser, ID: f.UserID, Email: "source-principal@test.com"}, p)

	p, err = f.Server.parentSourceActAsPrincipal(ctx, ProvenancePrincipal{Kind: store.DelegationPrincipalAgent, ID: f.AgentID})
	require.NoError(t, err)
	assert.Equal(t, store.PrincipalAgent, p.Kind)
	assert.Equal(t, f.SA.Email, p.ServiceAccountEmail, "an assign-mode agent acts as its own account")

	f.mutateAgent(t, func(a *store.Agent) { a.AppliedConfig.GCPIdentity.MetadataMode = store.GCPMetadataModePassthrough })
	p, err = f.Server.parentSourceActAsPrincipal(ctx, ProvenancePrincipal{Kind: store.DelegationPrincipalAgent, ID: f.AgentID})
	require.NoError(t, err)
	assert.Empty(t, p.ServiceAccountEmail, "a passthrough agent has no account of its own")

	f.mutateAgent(t, func(a *store.Agent) { a.DeletedAt = time.Now() })
	_, err = f.Server.parentSourceActAsPrincipal(ctx, ProvenancePrincipal{Kind: store.DelegationPrincipalAgent, ID: f.AgentID})
	assert.Error(t, err)

	_, err = f.Server.parentSourceActAsPrincipal(ctx, ProvenancePrincipal{Kind: "group", ID: f.UserID})
	assert.Error(t, err)
	_, err = f.Server.parentSourceActAsPrincipal(ctx, ProvenancePrincipal{Kind: store.DelegationPrincipalUser})
	assert.Error(t, err)
	_, err = f.Server.parentSourceActAsPrincipal(ctx, ProvenancePrincipal{Kind: store.DelegationPrincipalUser, ID: tid("nobody")})
	assert.Error(t, err)
}

// TestAgentGCPToken_EndpointsCallMintAuthority pins that both mint handlers
// call authorizeAgentGCPMint, so neither can drift from the other.
func TestAgentGCPToken_EndpointsCallMintAuthority(t *testing.T) {
	src, err := os.ReadFile("handlers_gcp_identity.go")
	require.NoError(t, err)
	fset := gotoken.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers_gcp_identity.go", src, 0)
	require.NoError(t, err)
	bodies := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		bodies[fn.Name.Name] = string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
	}
	for _, name := range []string{"handleAgentGCPToken", "handleAgentGCPIdentityToken"} {
		body, ok := bodies[name]
		require.True(t, ok, "%s exists", name)
		assert.Contains(t, body, "s.authorizeAgentGCPMint(", "%s calls the mint authority", name)
	}
}
