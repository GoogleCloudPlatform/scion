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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P4 item F: the three sites in handlers_agents_core.go that decide whether a
// service account may be used from a project.
//
// All three previously read `sa.ScopeID == projectID`, which never consulted
// sa.Scope and so was not a scope check at all — against a hub-scoped account
// it compared a hub instance ID with a project ID and always failed. The two
// caller-supplied sites (create, PATCH) failed closed with a 400; the
// project-default site failed silently, falling through to metadata mode
// "block".
//
// These tests are written while item A is still held, so no hub-scoped account
// can be created through the API yet. They build hub-scoped accounts directly
// in the store, which is the point rather than a shortcut: the assign paths
// have to be correct BEFORE item A makes such accounts reachable, not after.
//
// They reuse the bypassAgents fixture deliberately. Those tests own the
// confinement half of these same lines, and sharing the fixture is what makes
// "still rejected" and "now admitted" comparable rather than two unrelated
// worlds.

// hubScopedSAForAgent registers a hub-scoped service account created by a
// stranger. The creator is load-bearing: gcpServiceAccountResource sets
// OwnerID from CreatedBy, so seeding the account under the caller would
// satisfy the assign-time authorization through the resource-owner
// short-circuit and the test would pass without ever exercising scope. (That
// exact mistake produced a false pass earlier in P4.)
func hubScopedSAForAgent(t *testing.T, f *bypassAgentsFixture, verified bool) *store.GCPServiceAccount {
	t.Helper()
	return hubScopedSACreatedBy(t, f, tid("a-stranger"), verified)
}

// hubScopedSACreatedBy is the same, with the creator named. Who created the
// account is not bookkeeping here: the creator is one of the two principals
// §8.2 permits to assign it, and they are admitted through the resource-owner
// bypass, so this parameter selects between the admitted and refused cases.
func hubScopedSACreatedBy(t *testing.T, f *bypassAgentsFixture, creator string, verified bool) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:    uuid.New().String(),
		Scope: store.ScopeHub,
		// Provenance only. Nothing may compare this against the hub ID; the
		// predicate keys on Scope alone.
		ScopeID:   "some-hub-instance",
		Email:     fmt.Sprintf("hub-sa-%s@proj.iam.gserviceaccount.com", uuid.New().String()[:8]),
		ProjectID: "gcp-proj",
		CreatedBy: creator,
		Verified:  verified,
		CreatedAt: time.Now(),
	}
	require.NoError(t, f.store.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

// hubAdminUser creates a hub administrator. Admins reach a hub-scoped account
// through the admin bypass, which is a different mechanism from the creator's
// resource-owner bypass — hence a distinct principal rather than a variation
// of the same one.
func hubAdminUser(t *testing.T, f *bypassAgentsFixture) *store.User {
	t.Helper()
	u := &store.User{
		ID:          tid("hub-admin"),
		Email:       "hub-admin@example.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, f.store.CreateUser(context.Background(), u))
	return u
}

// createAgentAsOwner posts to the project agent route as the project owner.
//
// It first materialises the project's members group. The bypassAgents fixture
// builds its projects directly in the store, so the group that the project
// create handler would have made does not exist, and without it the owner has
// no rights over the project at all — agent create is refused before any
// service-account logic runs. Those tests never noticed because their callers
// are agents; these tests use a human caller, which is the realistic one for
// picking a hub-wide account. The call is idempotent.
func createAgentAsOwner(t *testing.T, f *bypassAgentsFixture, req CreateAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	f.srv.createProjectMembersGroupAndPolicy(context.Background(), f.proj, f.owner.ID)
	return doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
		"/api/v1/projects/"+f.proj.ID+"/agents", req)
}

// pendingAgentForPatch creates an agent in the 'created' phase, the only phase
// in which the PATCH path will touch GCP identity.
func pendingAgentForPatch(t *testing.T, f *bypassAgentsFixture, name string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:        uuid.New().String(),
		Slug:      name,
		Name:      name,
		ProjectID: f.proj.ID,
		Phase:     string(state.PhaseCreated),
		CreatedBy: f.owner.ID,
		OwnerID:   f.owner.ID,
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

func patchAgentSAAsOwner(t *testing.T, f *bypassAgentsFixture, agentID, saID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+agentID,
		map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode":      store.GCPMetadataModeAssign,
				"service_account_id": saID,
			},
		})
}

// ============================================================================
// Site 1 — agent create
// ============================================================================

// A hub-scoped account is assignable at create by current hub members (via the
// hub member baseline, D5) and hub admins. P9 requires gcpIamCheckMode=enforce
// for hub-scoped assignment (D4), so mode is set explicitly.
//
// ⚠️ P9 CHANGES: Prior to P9, assignment was confined to the creator (via
// resource-owner bypass) and admins. P9 implements the D5 hub member baseline
// which widens the allowed population to all current hub members, while also
// suppressing the resource-owner bypass for hub-scoped SA assign (D7). The
// creator now reaches assignment through hub membership, not through the owner
// bypass. Mode=enforce is now REQUIRED (D4).
func TestAgentCreate_HubScopedSA_AssignableByCreatorAndAdmin(t *testing.T) {
	assertAssigned := func(t *testing.T, f *bypassAgentsFixture, rec *httptest.ResponseRecorder, sa *store.GCPServiceAccount) {
		t.Helper()
		require.Equal(t, http.StatusCreated, rec.Code,
			"a permitted principal must be able to assign a hub-scoped SA from any project; got: %s",
			rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)

		got, err := f.store.GetAgent(context.Background(), resp.Agent.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		require.NotNil(t, got.AppliedConfig.GCPIdentity)
		assert.Equal(t, store.GCPMetadataModeAssign, got.AppliedConfig.GCPIdentity.MetadataMode)
		assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
		assert.Equal(t, sa.Email, got.AppliedConfig.GCPIdentity.ServiceAccountEmail)
	}

	t.Run("the account's creator", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		// P9: mode=enforce required for hub-scoped assignment (D4).
		setMode(f.srv, SAAssignCheckEnforce)
		// Wire a mock token generator so saAssignCheckerFor returns the
		// configured (disabled) checker rather than the unavailable one.
		f.srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
		ensureHubMembership(context.Background(), f.store, f.owner.ID)
		// Created BY the caller. P9 suppresses the resource-owner bypass for
		// hub-scoped SA assign (D7), so the creator now reaches assignment
		// through the hub member baseline (D5) rather than the owner bypass.
		sa := hubScopedSACreatedBy(t, f, f.owner.ID, true)

		rec := createAgentAsOwner(t, f, CreateAgentRequest{
			Name: "hub-sa-agent-creator",
			GCPIdentity: &GCPIdentityAssignment{
				MetadataMode:     store.GCPMetadataModeAssign,
				ServiceAccountID: sa.ID,
			},
		})
		assertAssigned(t, f, rec, sa)
	})

	t.Run("a hub admin", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		// P9: mode=enforce required for hub-scoped assignment (D4).
		setMode(f.srv, SAAssignCheckEnforce)
		f.srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
		sa := hubScopedSAForAgent(t, f, true) // created by a stranger
		admin := hubAdminUser(t, f)

		f.srv.createProjectMembersGroupAndPolicy(context.Background(), f.proj, f.owner.ID)
		rec := doRequestAsUser(t, f.srv, admin, http.MethodPost,
			"/api/v1/projects/"+f.proj.ID+"/agents", CreateAgentRequest{
				Name: "hub-sa-agent-admin",
				GCPIdentity: &GCPIdentityAssignment{
					MetadataMode:     store.GCPMetadataModeAssign,
					ServiceAccountID: sa.ID,
				},
			})
		assertAssigned(t, f, rec, sa)
	})
}

// P9 INVERTED: a plain hub member CAN assign a hub-scoped SA when
// mode=enforce (D5 hub member baseline). This is the resolution of task #19
// ruled by ptone. The test was INVERTED as instructed by the original comment.
//
// Without mode=enforce, the assignment is denied by mode coupling (D4).
// This test verifies the allowed path; mode-off denial is tested separately.
func TestAgentCreate_HubScopedSA_PlainHubMemberAllowed(t *testing.T) {
	f := bypassAgentsSetup(t)
	setMode(f.srv, SAAssignCheckEnforce)
	f.srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := hubScopedSAForAgent(t, f, true) // created by a stranger

	rec := createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "hub-sa-agent-member",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code,
		"a current hub member must be able to assign a hub-scoped SA when mode=enforce (D5); got: %s",
		rec.Body.String())
}

// A non-hub-member is denied hub-scoped SA assignment. With mode=enforce the
// denial comes from the Hub policy layer (no hub member baseline match, no
// owner bypass for assign). With mode=off the denial comes from mode coupling
// (D4) before policy even runs.
func TestAgentCreate_HubScopedSA_NonHubMemberDenied(t *testing.T) {
	f := bypassAgentsSetup(t)
	setMode(f.srv, SAAssignCheckEnforce)
	sa := hubScopedSAForAgent(t, f, true)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "hub-sa-agent-denied",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"a caller with no hub membership must not assign a hub-scoped SA; got: %s", rec.Body.String())
	assertDeniedByAuthzNotByScope(t, rec)
}

// assertDeniedByAuthzNotByScope pins WHICH branch produced a denial, for the
// two tests above that need a 403 from Hub policy and not a 400 from the scope
// predicate rejecting a hub-scoped account (item F stopped it doing that).
//
// ⚠️ IT REPLACES A CHECK THAT COULD NO LONGER FAIL. Both call sites previously
// asserted NotContains "does not belong to this project". #48 collapsed that
// message into msgSANotAvailableInProject, so no branch emits the old string
// any more and the assertion passed unconditionally — including, had the
// regression it guards against actually occurred, against the scope denial it
// was written to catch. A NotContains on a string the code cannot produce is
// rule 15 inside the test suite: invariant over the whole behaviour range, so
// it tells you nothing about whether the thing it names is on the path.
//
// So it now names the string the scope branch really does emit, and adds the
// positive half: the body must carry the authorization message. Negative alone
// would still pass on an empty body or an unrelated 403.
func assertDeniedByAuthzNotByScope(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.Contains(t, rec.Body.String(), "You don't have permission to assign this GCP service account",
		"the denial must be the Hub-policy assignment refusal")
	assert.NotContains(t, rec.Body.String(), msgSANotAvailableInProject,
		"the denial must come from authorization, not from the scope predicate")
}

// removeHubMembership revokes the hub-members membership that
// ensureHubMembership grants. Only the test below needs it, and it needs it to
// build a principal who WAS a hub member and is not one now.
func removeHubMembership(t *testing.T, f *bypassAgentsFixture, userID string) {
	t.Helper()
	g, err := f.store.GetGroupBySlug(context.Background(), "hub-members")
	require.NoError(t, err)
	require.NoError(t, f.store.RemoveGroupMember(context.Background(), g.ID,
		store.GroupMemberTypeUser, userID))
}

// P9 UNSKIPPED: The §8.2 hole is now closed. P9 suppresses the resource-owner
// bypass for ActionAssign on hub-scoped (parentless) gcp_service_account
// resources (D7). A former hub member who created a hub-scoped SA can no
// longer assign it — the owner bypass falls through to the hub member baseline
// (D5), which requires current membership.
func TestAgentCreate_HubScopedSA_FormerHubMemberCreatorDenied(t *testing.T) {
	f := bypassAgentsSetup(t)
	setMode(f.srv, SAAssignCheckEnforce)
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := hubScopedSACreatedBy(t, f, f.owner.ID, true)
	removeHubMembership(t, f, f.owner.ID)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "hub-sa-agent-ex-member",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"a creator removed from the hub must lose the creator grant; got: %s", rec.Body.String())
}

// The confinement half, which must survive the widening: another project's
// account is still refused. Track S's tests cover this too; it is repeated
// here because "still rejected" and "now admitted" are the two halves of one
// change, and a widening that lost the first half would still pass every test
// in this file if only the second were written.
func TestAgentCreate_OtherProjectSA_StillRejected(t *testing.T) {
	f := bypassAgentsSetup(t)
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := bypassAgentsCreateSA(t, f, f.other.ID, true)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "cross-project-agent",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"another project's SA must still be rejected; got: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), msgSANotAvailableInProject)
}

// REACHABILITY, VARIED ON ITS OWN, AT CREATE. Both arms in one test because the
// point is the difference between them and a reader should not have to hold two
// test functions side by side to see it.
//
// Why it was added (sa-arch's call, #48 follow-up): create's existing pair is
// TestAgentCreate_HubScopedSA_AssignableByCreatorAndAdmin against the test above
// — and those two vary the KIND OF SCOPE as well as reachability. A pair with
// two variables cannot attribute a failure to either, and worse, it has a blind
// spot: a predicate rewritten to admit hub-scoped accounts and refuse ALL
// project-scoped ones leaves both of those arms green, because neither arm is a
// project-scoped account that ought to be admitted. That case is the common one
// in production and it had no admitted arm at create. PATCH already had one, in
// TestBypassAgents_UpdateAgentServiceAccountChecks; this is the create twin.
//
// THE BLIND SPOT IS MEASURED, NOT INFERRED, AND IT RUNS ONE WAY ONLY. Replacing
// the create predicate with `sa.Scope != store.ScopeHub` — admit hub-scoped,
// refuse every project-scoped account, which would break assignment for
// essentially every real project — left BOTH existing arms PASSING, and only
// this test failed. The mirror regression, `sa.Scope != store.ScopeProject`,
// fails three tests loudly and never needed this one. Direction matters here:
// the pair that looks symmetric is blind in exactly one of the two directions,
// and it is blind in the direction that matters more.
//
// The two accounts differ in ScopeID and nothing that any predicate reads. ID
// and Email must differ for uniqueness and are inert here — no decision on this
// path consults either, they are only echoed back. Scope, Verified and CreatedBy
// are held equal ON PURPOSE: CreatedBy in particular, because it becomes
// Resource.OwnerID and admits the caller through the resource-owner
// short-circuit. Holding it constant means that bypass cannot be what separates
// the arms; if it were varied, the refused arm might be refused by
// authorization and the test would prove nothing about scope.
func TestAgentCreate_ReachabilityIsTheOnlyVariable(t *testing.T) {
	f := bypassAgentsSetup(t)

	mkSA := func(scopeID string) *store.GCPServiceAccount {
		sa := &store.GCPServiceAccount{
			ID:        uuid.New().String(),
			Scope:     store.ScopeProject,
			ScopeID:   scopeID,
			Email:     fmt.Sprintf("reach-%s@proj.iam.gserviceaccount.com", uuid.New().String()[:8]),
			ProjectID: "gcp-proj",
			CreatedBy: f.owner.ID,
			Verified:  true,
			CreatedAt: time.Now(),
		}
		require.NoError(t, f.store.CreateGCPServiceAccount(context.Background(), sa))
		return sa
	}

	reachable := mkSA(f.proj.ID)
	unreachable := mkSA(f.other.ID)

	// Both accounts really are in the store. Stated as an assertion rather than
	// trusted, because the #48 collapse means a seeding failure and a scope
	// refusal now produce the same 400 — see msgSANotAvailableInProject. Without
	// this, the refused arm below is green whether or not its account exists.
	for _, sa := range []*store.GCPServiceAccount{reachable, unreachable} {
		got, err := f.store.GetGCPServiceAccount(context.Background(), sa.ID)
		require.NoError(t, err, "fixture did not persist; the refusal below would be meaningless")
		require.Equal(t, sa.ScopeID, got.ScopeID)
	}

	t.Run("in-project account is admitted", func(t *testing.T) {
		rec := createAgentAsOwner(t, f, CreateAgentRequest{
			Name: "reachable-sa-agent",
			GCPIdentity: &GCPIdentityAssignment{
				MetadataMode:     store.GCPMetadataModeAssign,
				ServiceAccountID: reachable.ID,
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code,
			"a verified account scoped to THIS project must be assignable at create; got: %s",
			rec.Body.String())

		// 201 alone would not prove the assignment happened — a handler that
		// dropped GCP identity silently would also return 201.
		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)
		got, err := f.store.GetAgent(context.Background(), resp.Agent.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		require.NotNil(t, got.AppliedConfig.GCPIdentity)
		assert.Equal(t, reachable.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
	})

	t.Run("the same account scoped elsewhere is refused", func(t *testing.T) {
		rec := createAgentAsOwner(t, f, CreateAgentRequest{
			Name: "unreachable-sa-agent",
			GCPIdentity: &GCPIdentityAssignment{
				MetadataMode:     store.GCPMetadataModeAssign,
				ServiceAccountID: unreachable.ID,
			},
		})
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"the only difference from the admitted arm is ScopeID; got: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), msgSANotAvailableInProject)
	})
}

// A user-scoped account must not become assignable. It was excluded before by
// accident — a user ID rarely equals a project ID — and is excluded now on
// purpose, by the predicate's fail-closed default arm. The distinction is
// worth its own test: the ScopeID given here is the project ID, so under the
// old equality this account would have been admitted.
func TestAgentCreate_UserScopedSA_RejectedEvenWhenScopeIDMatches(t *testing.T) {
	f := bypassAgentsSetup(t)
	ensureHubMembership(context.Background(), f.store, f.owner.ID)

	sa := &store.GCPServiceAccount{
		ID:        uuid.New().String(),
		Scope:     store.ScopeUser,
		ScopeID:   f.proj.ID, // deliberately collides with the project ID
		Email:     "user-scoped@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: f.owner.ID,
		Verified:  true,
		CreatedAt: time.Now(),
	}
	require.NoError(t, f.store.CreateGCPServiceAccount(context.Background(), sa))

	rec := createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "user-scoped-agent",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"a user-scoped SA must not be assignable even when its ScopeID equals the project ID; got: %s",
		rec.Body.String())
	assert.Contains(t, rec.Body.String(), msgSANotAvailableInProject)
}

// ============================================================================
// Site 2 — the PATCH twin
// ============================================================================

// The PATCH path carries a near-duplicate of the create checks precisely
// because "create clean, then PATCH the identity in" would otherwise walk
// around them. P9: mode=enforce required for hub-scoped assignment (D4).
func TestAgentPatch_HubScopedSA_AssignableByCreatorAndAdmin(t *testing.T) {
	t.Run("the account's creator", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		setMode(f.srv, SAAssignCheckEnforce)
		f.srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
		ensureHubMembership(context.Background(), f.store, f.owner.ID)
		sa := hubScopedSACreatedBy(t, f, f.owner.ID, true)
		a := pendingAgentForPatch(t, f, "hub-sa-patch-creator")

		rec := patchAgentSAAsOwner(t, f, a.ID, sa.ID)
		require.Equal(t, http.StatusOK, rec.Code,
			"PATCH must admit a hub-scoped SA for its creator (via hub member baseline), as create does; got: %s", rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		require.NotNil(t, got.AppliedConfig.GCPIdentity)
		assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
	})

	t.Run("a hub admin", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		setMode(f.srv, SAAssignCheckEnforce)
		f.srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
		sa := hubScopedSAForAgent(t, f, true) // created by a stranger
		admin := hubAdminUser(t, f)
		a := pendingAgentForPatch(t, f, "hub-sa-patch-admin")

		rec := doRequestAsUser(t, f.srv, admin, http.MethodPatch, "/api/v1/agents/"+a.ID,
			map[string]interface{}{
				"gcp_identity": map[string]interface{}{
					"metadata_mode":      store.GCPMetadataModeAssign,
					"service_account_id": sa.ID,
				},
			})
		require.Equal(t, http.StatusOK, rec.Code,
			"PATCH must admit a hub-scoped SA for an admin, as create does; got: %s", rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		require.NotNil(t, got.AppliedConfig.GCPIdentity)
		assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
	})
}

// P9 INVERTED: the PATCH twin of the create test. A hub member CAN assign
// a hub-scoped SA when mode=enforce (D5). Without mode=enforce, denied by D4.
func TestAgentPatch_HubScopedSA_PlainHubMemberAllowed(t *testing.T) {
	f := bypassAgentsSetup(t)
	setMode(f.srv, SAAssignCheckEnforce)
	f.srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := hubScopedSAForAgent(t, f, true) // created by a stranger
	a := pendingAgentForPatch(t, f, "hub-sa-patch-member")

	rec := patchAgentSAAsOwner(t, f, a.ID, sa.ID)
	require.Equal(t, http.StatusOK, rec.Code,
		"a current hub member must be able to assign via PATCH when mode=enforce (D5); got: %s", rec.Body.String())

	got, err := f.store.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
}

func TestAgentPatch_HubScopedSA_NonHubMemberDenied(t *testing.T) {
	f := bypassAgentsSetup(t)
	setMode(f.srv, SAAssignCheckEnforce)
	sa := hubScopedSAForAgent(t, f, true)
	a := pendingAgentForPatch(t, f, "hub-sa-patch-denied")

	rec := patchAgentSAAsOwner(t, f, a.ID, sa.ID)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"PATCH must apply the same authorization as create; got: %s", rec.Body.String())

	got, err := f.store.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	if got.AppliedConfig != nil {
		assert.Nil(t, got.AppliedConfig.GCPIdentity,
			"the denied service account must not have been attached")
	}
}

// Verification and scope are independent gates; opening the first must not
// shadow the second. P9: mode=enforce required for hub-scoped assignment.
func TestAgentPatch_UnverifiedHubScopedSA_StillRejected(t *testing.T) {
	f := bypassAgentsSetup(t)
	setMode(f.srv, SAAssignCheckEnforce)
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := hubScopedSAForAgent(t, f, false)
	a := pendingAgentForPatch(t, f, "hub-sa-unverified")

	rec := patchAgentSAAsOwner(t, f, a.ID, sa.ID)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"an unverified hub-scoped SA must still be rejected; got: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not verified")
}

// ============================================================================
// Site 3 — the project default
// ============================================================================

// setProjectDefaultSA configures the project's default GCP identity through
// the real settings route, and requires the route to ACCEPT it.
//
// This helper originally existed to demonstrate the opposite. Its comment read
// "nothing validates the service account ID on the way in... Site 3's failure
// was live, not latent," and that was true and correctly evidenced when it was
// written: the settings PUT wrote the ID unchecked. Defect #22 added write-time
// validation, so the demonstration no longer holds and the helper has inverted
// meaning — it now shows which defaults the PUT still admits.
//
// Only valid defaults may be set through here. A VERIFIED HUB-SCOPED account is
// one of them, deliberately: ReachableFromProject's ScopeHub arm returns true
// unconditionally (pkg/store/models.go:1552), because a hub-scoped account is
// legitimately pickable from any project. That is why this helper still has a
// caller. For defaults the PUT now refuses, see setStaleProjectDefaultSA.
func setProjectDefaultSA(t *testing.T, f *bypassAgentsFixture, saID string) {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPut,
		"/api/v1/projects/"+f.proj.ID+"/settings",
		map[string]interface{}{
			"defaultGCPIdentityMode":             store.GCPMetadataModeAssign,
			"defaultGCPIdentityServiceAccountID": saID,
		})
	require.Equal(t, http.StatusOK, rec.Code,
		"this default must remain settable through the API; got: %s", rec.Body.String())
}

// setStaleProjectDefaultSA writes the default-identity annotations straight to
// the store, bypassing the settings route.
//
// IT BYPASSES HTTP ON PURPOSE, NOT FOR CONVENIENCE. Since defect #22 the
// settings PUT validates the service account ID, so the states constructed here
// CANNOT be reached through the API at all — that is what #22 did. Do not
// "tidy" these callers back onto setProjectDefaultSA: they will fail, and the
// failure would misread as the API having regressed. If a caller's default is
// one the PUT should accept, it belongs on the HTTP helper instead, so that the
// acceptance stays covered at the API boundary.
//
// THE SCENARIOS ARE NOT DEAD, SO DO NOT DELETE THEM AS UNREACHABLE. A future
// reader can correctly observe that no API path produces this state and
// conclude the tests below are vestigial. They are not. The state arises in
// production as a STALE value: an account that was validly set as the default
// and was afterwards deleted, un-verified, or moved out of reach. #22 closed
// the write; it cannot close the drift, because the drift happens after the
// write. Guarding consumption against that is the separately-owned work on
// handlers_agents_core.go:655.
//
// Distinct from the inline annotation write in authz_bypass_agents_test.go:862,
// which constructs a VALID default directly. That one could go through the API;
// these cannot. Keeping the two apart is the point.
func setStaleProjectDefaultSA(t *testing.T, f *bypassAgentsFixture, saID string) {
	t.Helper()
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	proj.Annotations[projectSettingDefaultGCPIdentitySAID] = saID
	require.NoError(t, f.store.UpdateProject(ctx, proj))
}

// createdAgentIdentity creates an agent with no explicit GCP identity and
// returns the identity the project default produced.
func createdAgentIdentity(t *testing.T, f *bypassAgentsFixture, name string) *store.GCPIdentityConfig {
	t.Helper()
	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: name})
	require.Equal(t, http.StatusCreated, rec.Code,
		"agent creation should succeed; got: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	got, err := f.store.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig, "agent should have applied config")
	require.NotNil(t, got.AppliedConfig.GCPIdentity, "agent should have a resolved GCP identity")
	return got.AppliedConfig.GCPIdentity
}

// P10 CHANGED: Project-default assignment now runs the full authorization
// gate (ActionAssign + actAs). The account is not caller-supplied, but the
// design ruling changed: the project operator selected an available default
// but did not grant every future creator permission to act as it. The gate
// checks the immediate agent creator.
//
// Hub membership and mode=enforce are now REQUIRED for hub-scoped defaults
// because authorizeSAAssignment enforces mode coupling (D4) and Hub policy.
func TestAgentCreate_HubScopedProjectDefault_IsApplied(t *testing.T) {
	f := bypassAgentsSetup(t)
	// P10: mode=enforce + hub membership required for hub-scoped default
	setMode(f.srv, SAAssignCheckEnforce)
	f.srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := hubScopedSAForAgent(t, f, true)
	setProjectDefaultSA(t, f, sa.ID)

	identity := createdAgentIdentity(t, f, "default-sa-agent")
	assert.Equal(t, store.GCPMetadataModeAssign, identity.MetadataMode,
		"falling through to %q is the silent failure this site had", store.GCPMetadataModeBlock)
	assert.Equal(t, sa.ID, identity.ServiceAccountID)
	assert.Equal(t, sa.Email, identity.ServiceAccountEmail)
}

// The project-default site's confinement, which must also survive: a default
// naming another project's account still falls through to block.
//
// What this test defends against is narrower than its name suggests, so read
// this before changing it. The silence above is a real and separately-filed
// defect: an operator whose default is unusable sees "GCP access is
// mysteriously broken" rather than a rejection. The realistic risk is not that
// nobody fixes that — it is that it gets fixed by making the assign SUCCEED,
// because a passing assign is the obvious way to stop the complaint. This
// assertion is what stands between a usability complaint and a cross-project
// service account being handed to an agent. Fixing the silence is welcome;
// fixing it here, by admitting the account, is the bug.
//
// (Scale, for whoever sizes that work: the settings PUT is a full replace —
// setOrDelete deletes on empty, so every write restates the value and the next
// settings save on a project repairs or re-rejects a bad default. The exposed
// population is projects nobody re-saves plus defaults that went stale after
// being set validly, e.g. the account was later deleted or unverified. A slow
// leak, not a standing breakage.)
//
// Since #22 this default can no longer be INSTALLED through the API — the PUT
// refuses another project's account outright, and this test now builds the
// state directly. That narrows the exposure the paragraph above describes to
// the stale case alone, and it is why the setup bypasses HTTP. The refusal
// itself is covered at the boundary by
// TestProjectSettings_DefaultGCPIdentity_OtherProjectSAIsNotAnOracle.
// P10 CHANGED: an unreachable project default now fails agent creation with
// a 400 error instead of silently falling back to block. The operator set
// the default; if the SA is unreachable, the error surfaces immediately.
func TestAgentCreate_OtherProjectDefault_FailsWithError(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.other.ID, true)
	setStaleProjectDefaultSA(t, f, sa.ID)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "bad-default-agent"})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"P10: an unreachable project-default SA must fail agent creation, not silently degrade; got: %s",
		rec.Body.String())
	assert.Contains(t, rec.Body.String(), "project default GCP service account is not available",
		"error must indicate the project default SA is the issue")
}

// An unverified hub-scoped default is still refused. Same independence of
// gates as at the two caller-supplied sites, checked here because this site
// spells the two conditions in a single boolean expression where losing one is
// a one-character edit.
//
// This is the crossing that matters most to keep: the scope gate PASSES here
// (hub-scoped is reachable from every project) and only the verification gate
// refuses. A change that collapses the two conditions into one would still
// satisfy the scope half and this is the test that would notice.
//
// Setup is direct because #22 stops an unverified account being set as a
// default at all; the state now arises only by an account losing verification
// after it was validly set. The write-time refusal is covered at the boundary
// by TestProjectSettings_DefaultGCPIdentity_RejectsUnverifiedHubScopedSA.
// P10 CHANGED: an unverified project default now fails agent creation with
// a 400 error instead of silently falling back to block.
func TestAgentCreate_UnverifiedHubScopedDefault_FailsWithError(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := hubScopedSAForAgent(t, f, false)
	setStaleProjectDefaultSA(t, f, sa.ID)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "unverified-default-agent"})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"P10: an unverified project-default SA must fail agent creation, not silently degrade; got: %s",
		rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not verified",
		"error must indicate the SA is not verified")
}
