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

// A hub-scoped account is assignable at create by the two principals §8.2
// permits: the account's creator and a hub admin. Nobody else.
//
// This test previously asserted the opposite of its denial half — that ANY hub
// member could assign ANY hub-scoped account — and passed, because ActionRead
// on a parentless resource is satisfied by the seeded hub-member-read-all
// policy ("*", read+list). That was §8.2's hole, and the test documented the
// mechanism of the exposure as though it were the design. Step 2's conversion
// of this gate to ActionAssign closes it: ActionAssign is granted only by
// project-scoped policies, which matchesResource will not match against a
// parentless resource, so the policy path admits nobody and only the two
// bypasses remain.
//
// The two principals are therefore reached by DIFFERENT mechanisms, which is
// why both are exercised: the creator through the resource-owner bypass
// (gcpServiceAccountResource carries OwnerID from CreatedBy), the admin
// through the admin bypass. A change that broke either one would leave the
// other passing.
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
		// Created BY the caller this time. The stranger-created fixture used
		// elsewhere in this file exists to defeat the resource-owner
		// short-circuit; here that short-circuit is the admitting path, because
		// the creator is one of the two principals §8.2 permits.
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

// A plain hub member — not the creator, not an admin — is refused.
//
// This is the assertion that closes §8.2's hole, and it is the one to look at
// if a future change makes hub-scoped accounts assignable again. The caller
// here holds hub membership, so the seeded hub-member-read-all policy applies
// to them; the request must still fail. Reverting the gate to ActionRead makes
// this test fail and is the intended tripwire.
//
// The scope message is asserted absent so that a denial arriving for the wrong
// reason — the scope predicate rejecting a hub-scoped account, which item F
// deliberately stopped doing — cannot be mistaken for this test passing.
func TestAgentCreate_HubScopedSA_PlainHubMemberDenied(t *testing.T) {
	// THIS ASSERTS THE CURRENT RULED ANSWER TO A QUESTION THAT IS STILL OPEN.
	// §8.2 confines hub-scoped assignment to admins and the account's creator;
	// task #19 (with ptone) may yet open it up. If it does, THIS TEST SHOULD
	// FAIL, legitimately — and the correct response is to INVERT it and name
	// the ruling that authorised the change in the commit message, NOT to
	// delete it. Deleting it implements the new ruling and removes the
	// safeguard against the old hole in one motion, leaving nothing that would
	// notice if the widening later went further than #19 permitted.
	f := bypassAgentsSetup(t)
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := hubScopedSAForAgent(t, f, true) // created by a stranger

	rec := createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "hub-sa-agent-member",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"hub membership alone must not confer assignment of a hub-scoped SA; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "does not belong to this project",
		"the denial must come from authorization, not from the scope predicate")
}

// The same request from a caller who is not a hub member at all is refused
// too. Kept alongside the test above because they fail for different reasons —
// this one has no hub-scoped policy applying to it, that one has one that no
// longer grants the action — and a regression that restores the hub-member
// path would leave this test passing.
func TestAgentCreate_HubScopedSA_NonHubMemberDenied(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := hubScopedSAForAgent(t, f, true)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "hub-sa-agent-denied",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"a caller with no hub-scoped read must not assign a hub-scoped SA; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "does not belong to this project",
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

// A KNOWN HOLE IN §8.2, RECORDED AS A FAILING TEST RATHER THAN AS AN ABSENCE.
// Skipped, so it does not block step 2. Raised by sa-arch, who owns it and is
// taking it to ptone alongside task #19; not p3's to fix in the conversion and
// not mine to fix in P4.
//
// §8.2 grants assignment to "the account's creator". That is served by the
// resource-owner bypass at authz.go:133-139, resource.OwnerID == user.ID(),
// which CONSULTS NO MEMBERSHIP OF ANY KIND. So the grant is to whoever created
// the account, permanently, and removing them from the hub does not remove it.
// Authority captured at write time and never re-checked — the same shape as
// the lifecycle-hook escalation, which makes it a pattern here rather than a
// one-off.
//
// Worth knowing before reading the rest of this file: the hole is WIDER than
// this test's setup implies. TestAgentCreate_HubScopedSA_AssignableByCreatorAndAdmin's
// "the account's creator" subtest never grants hub membership at all, and
// passes — the creator does not need to have been a member even once. This
// test uses grant-then-revoke because that is the case with a victim: access
// deliberately withdrawn, and still working.
//
// It asserts the behaviour we would want, so it fails today. Unskip it when
// #19 is answered; if the answer keeps a creator grant, it should be scoped to
// creators who are still members, and this test is then the check that it is.
func TestAgentCreate_HubScopedSA_FormerHubMemberCreatorDenied(t *testing.T) {
	t.Skip("known §8.2 hole: the creator grant is a resource-owner bypass and " +
		"survives removal from the hub; with sa-arch and ptone under task #19")

	f := bypassAgentsSetup(t)
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
	assert.Contains(t, rec.Body.String(), "does not belong to this project")
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
	assert.Contains(t, rec.Body.String(), "does not belong to this project")
}

// ============================================================================
// Site 2 — the PATCH twin
// ============================================================================

// The PATCH path carries a near-duplicate of the create checks precisely
// because "create clean, then PATCH the identity in" would otherwise walk
// around them. Every assertion made about create is therefore made here too:
// a gate that closed only at create would be no gate at all.
func TestAgentPatch_HubScopedSA_AssignableByCreatorAndAdmin(t *testing.T) {
	t.Run("the account's creator", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		sa := hubScopedSACreatedBy(t, f, f.owner.ID, true)
		a := pendingAgentForPatch(t, f, "hub-sa-patch-creator")

		rec := patchAgentSAAsOwner(t, f, a.ID, sa.ID)
		require.Equal(t, http.StatusOK, rec.Code,
			"PATCH must admit a hub-scoped SA for its creator, as create does; got: %s", rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		require.NotNil(t, got.AppliedConfig.GCPIdentity)
		assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
	})

	t.Run("a hub admin", func(t *testing.T) {
		f := bypassAgentsSetup(t)
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

// The PATCH twin of the confinement that closes §8.2's hole. If only one of
// the two sites is checked, "create clean then PATCH the identity in" is the
// way around it — which is the reason both sites carry the gate at all.
func TestAgentPatch_HubScopedSA_PlainHubMemberDenied(t *testing.T) {
	// Encodes an OPEN question's current answer — see the note in
	// TestAgentCreate_HubScopedSA_PlainHubMemberDenied. If task #19 opens
	// hub-scope assignment, invert this test, do not delete it. Inverting one
	// of the pair and deleting the other is worse than either: it leaves the
	// create site guarded and the PATCH site not, which is precisely the
	// asymmetry this twin exists to prevent.
	f := bypassAgentsSetup(t)
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := hubScopedSAForAgent(t, f, true) // created by a stranger
	a := pendingAgentForPatch(t, f, "hub-sa-patch-member")

	rec := patchAgentSAAsOwner(t, f, a.ID, sa.ID)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"hub membership alone must not confer assignment via PATCH; got: %s", rec.Body.String())

	got, err := f.store.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	if got.AppliedConfig != nil {
		assert.Nil(t, got.AppliedConfig.GCPIdentity,
			"the denied service account must not have been attached")
	}
}

func TestAgentPatch_HubScopedSA_NonHubMemberDenied(t *testing.T) {
	f := bypassAgentsSetup(t)
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
// shadow the second.
func TestAgentPatch_UnverifiedHubScopedSA_StillRejected(t *testing.T) {
	f := bypassAgentsSetup(t)
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
// the real settings route.
//
// Going through HTTP rather than writing annotations directly is the point of
// this helper: it demonstrates that nothing validates the service account ID
// on the way in, so a hub-scoped default is configurable on the current branch
// today. Site 3's failure was live, not latent.
func setProjectDefaultSA(t *testing.T, f *bypassAgentsFixture, saID string) {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPut,
		"/api/v1/projects/"+f.proj.ID+"/settings",
		map[string]interface{}{
			"defaultGCPIdentityMode":             store.GCPMetadataModeAssign,
			"defaultGCPIdentityServiceAccountID": saID,
		})
	require.Equal(t, http.StatusOK, rec.Code,
		"project settings accept no validation of the SA id; got: %s", rec.Body.String())
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

// The silent one. A project whose default service account was hub-scoped fell
// through to metadata mode "block": every agent created there got no identity
// and no error was surfaced anywhere, so the operator sees "GCP access is
// mysteriously broken" rather than a rejection.
//
// Note there is no authorization check at this site and this test must not
// grow one. The account is not caller-supplied — it comes from project
// settings — so there is no caller-elected privilege to authorize, and gating
// it on the caller would turn a project-admin decision into a per-caller
// lottery. Hub membership is therefore deliberately NOT granted here.
func TestAgentCreate_HubScopedProjectDefault_IsApplied(t *testing.T) {
	f := bypassAgentsSetup(t)
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
func TestAgentCreate_OtherProjectDefault_StillFallsThroughToBlock(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.other.ID, true)
	setProjectDefaultSA(t, f, sa.ID)

	identity := createdAgentIdentity(t, f, "bad-default-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"another project's SA must not be applied as this project's default")
	assert.Empty(t, identity.ServiceAccountID)
}

// An unverified hub-scoped default is still refused. Same independence of
// gates as at the two caller-supplied sites, checked here because this site
// spells the two conditions in a single boolean expression where losing one is
// a one-character edit.
func TestAgentCreate_UnverifiedHubScopedDefault_FallsThroughToBlock(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := hubScopedSAForAgent(t, f, false)
	setProjectDefaultSA(t, f, sa.ID)

	identity := createdAgentIdentity(t, f, "unverified-default-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"an unverified hub-scoped default must not be applied")
}
