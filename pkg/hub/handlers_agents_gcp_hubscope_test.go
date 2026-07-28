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
	sa := &store.GCPServiceAccount{
		ID:    uuid.New().String(),
		Scope: store.ScopeHub,
		// Provenance only. Nothing may compare this against the hub ID; the
		// predicate keys on Scope alone.
		ScopeID:   "some-hub-instance",
		Email:     fmt.Sprintf("hub-sa-%s@proj.iam.gserviceaccount.com", uuid.New().String()[:8]),
		ProjectID: "gcp-proj",
		CreatedBy: tid("a-stranger"),
		Verified:  verified,
		CreatedAt: time.Now(),
	}
	require.NoError(t, f.store.CreateGCPServiceAccount(context.Background(), sa))
	return sa
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

// A hub-scoped account is assignable at create by a caller who can read it.
//
// Hub membership is granted explicitly. A hub-scoped account is parentless, so
// the assign-time ActionRead check cannot be satisfied by project membership;
// it needs a hub-scoped policy, which is what hub-members carries. That is not
// incidental setup — it is the mechanism by which hub-scoped accounts are
// gated, and a version of this test that passed without it would mean the
// authorization was not running.
func TestAgentCreate_HubScopedSA_IsAssignable(t *testing.T) {
	f := bypassAgentsSetup(t)
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := hubScopedSAForAgent(t, f, true)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "hub-sa-agent",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code,
		"a hub-scoped SA must be assignable from any project; got: %s", rec.Body.String())

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

// The same request from a caller who is not a hub member is refused, and
// refused by authorization rather than by scope.
//
// This is the test that gives the one above its meaning: it shows the scope
// widening did not make hub-scoped accounts free for all, and it pins which
// gate is doing the work. Asserting the status alone would not distinguish the
// two failure modes, so the scope message is asserted absent.
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
// around them. So the widening is asserted separately at both sites rather
// than assumed to travel.
func TestAgentPatch_HubScopedSA_IsAssignable(t *testing.T) {
	f := bypassAgentsSetup(t)
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := hubScopedSAForAgent(t, f, true)
	a := pendingAgentForPatch(t, f, "hub-sa-patch")

	rec := patchAgentSAAsOwner(t, f, a.ID, sa.ID)
	require.Equal(t, http.StatusOK, rec.Code,
		"PATCH must admit a hub-scoped SA just as create does; got: %s", rec.Body.String())

	got, err := f.store.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
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
// The silence is a separate, separately-filed defect and is not fixed here.
// The assertion pins the confinement so that fixing the silence later cannot
// quietly drop it as well.
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
