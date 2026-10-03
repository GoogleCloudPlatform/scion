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

// Package hub — ptone/scion#2129: handler-level tests for the live-mint
// record recheck in handleAgentGCPToken and handleAgentGCPIdentityToken. Each
// test starts from newGCPMintFixture's "everything current" baseline (a
// verified, project-scoped, reachable service account assigned to a live
// agent whose JWT carries the matching per-SA scope) and perturbs exactly one
// fact away from it, then asserts both mint endpoints deny identically. See
// authz_gcp_service_account_use_test.go for the kernel-level Decide tests
// behind the scope-mapping checks.
package hub

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// gcpMintFixture bundles a project, an owner-role user, a verified
// project-scoped GCP service account, and an agent assigned to it with a
// matching agent-token scope.
type gcpMintFixture struct {
	Server    *Server
	Store     store.Store
	ProjectID string
	UserID    string
	AgentID   string
	SA        *store.GCPServiceAccount
	Token     string
}

func newGCPMintFixture(t *testing.T, name string) *gcpMintFixture {
	t.Helper()
	srv, s := testServer(t)
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub-sa@test-hub-project.iam.gserviceaccount.com"})
	ctx := context.Background()

	projectID := tid("project-" + name)
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: name, Slug: "proj-" + name, Created: time.Now(), Updated: time.Now(),
	}))

	userID := tid("user-" + name)
	createDCUser(t, s, userID, name+"@test.com", projectID, store.ProjectRoleOwner)

	saID := tid("sa-" + name)
	saEmail := name + "-sa@my-gcp-project.iam.gserviceaccount.com"
	sa := &store.GCPServiceAccount{
		ID: saID, Scope: store.ScopeProject, ScopeID: projectID,
		Email: saEmail, ProjectID: "my-gcp-project",
		Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: userID, CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	agentID := tid("agent-" + name)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "agent-" + name, Name: "Agent " + name,
		ProjectID: projectID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Ancestry: []string{userID},
		AppliedConfig: &store.AgentAppliedConfig{
			GCPIdentity: &store.GCPIdentityConfig{
				MetadataMode:        store.GCPMetadataModeAssign,
				ServiceAccountID:    saID,
				ServiceAccountEmail: saEmail,
				ProjectID:           "my-gcp-project",
			},
		},
		Created: time.Now(), Updated: time.Now(),
	}))

	token, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, []AgentTokenScope{GCPTokenScopeForSA(saID)}, []string{userID})
	require.NoError(t, err)

	return &gcpMintFixture{Server: srv, Store: s, ProjectID: projectID, UserID: userID, AgentID: agentID, SA: sa, Token: token}
}

// getAgent reloads the fixture's agent record from the store.
func (f *gcpMintFixture) getAgent(t *testing.T) *store.Agent {
	t.Helper()
	a, err := f.Store.GetAgent(context.Background(), f.AgentID)
	require.NoError(t, err)
	return a
}

// mutateAgent reloads the agent, applies mutate, and writes it back.
func (f *gcpMintFixture) mutateAgent(t *testing.T, mutate func(*store.Agent)) {
	t.Helper()
	a := f.getAgent(t)
	mutate(a)
	require.NoError(t, f.Store.UpdateAgent(context.Background(), a))
}

// mutateSA reloads the fixture's SA row, applies mutate, and writes it back.
// Scope and ScopeID are NOT among the fields UpdateGCPServiceAccount persists
// (pkg/store/entadapter/external_store.go's UpdateGCPServiceAccount has no
// SetScope/SetScopeID call) -- a test that needs a different scope or
// reachability must create a new row via reassignSA instead of mutating
// those two fields here.
func (f *gcpMintFixture) mutateSA(t *testing.T, mutate func(*store.GCPServiceAccount)) {
	t.Helper()
	sa, err := f.Store.GetGCPServiceAccount(context.Background(), f.SA.ID)
	require.NoError(t, err)
	mutate(sa)
	require.NoError(t, f.Store.UpdateGCPServiceAccount(context.Background(), sa))
}

// reassignSA creates a brand-new, verified GCP service account row with the
// given scope/scopeID/email, points the fixture's agent at it
// (AppliedConfig.GCPIdentity), reissues the agent token with the new SA's
// per-instance scope, and updates f.SA/f.Token to match. Used instead of
// mutateSA when a test needs to vary Scope or ScopeID, which
// UpdateGCPServiceAccount cannot persist.
func (f *gcpMintFixture) reassignSA(t *testing.T, scope, scopeID, email string) {
	t.Helper()
	ctx := context.Background()

	saID := tid("sa-reassigned-" + scope)
	sa := &store.GCPServiceAccount{
		ID: saID, Scope: scope, ScopeID: scopeID,
		Email: email, ProjectID: "my-gcp-project",
		Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: f.UserID, CreatedAt: time.Now(),
	}
	require.NoError(t, f.Store.CreateGCPServiceAccount(ctx, sa))

	f.mutateAgent(t, func(a *store.Agent) {
		a.AppliedConfig.GCPIdentity.ServiceAccountID = saID
		a.AppliedConfig.GCPIdentity.ServiceAccountEmail = email
	})

	token, err := f.Server.agentTokenService.GenerateAgentToken(f.AgentID, f.ProjectID, []AgentTokenScope{GCPTokenScopeForSA(saID)}, []string{f.UserID})
	require.NoError(t, err)

	f.SA = sa
	f.Token = token
}

// assertMintDenied asserts both mint endpoints deny f's current state with a
// 403. This is the handler-level parity check every record-recheck test relies on.
func assertMintDenied(t *testing.T, f *gcpMintFixture) {
	t.Helper()
	t.Run("access-token", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/gcp-token", nil, f.Token)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403, got %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("identity-token", func(t *testing.T) {
		body := map[string]string{"audience": "https://example.com"}
		rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/gcp-identity-token", body, f.Token)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// assertMintAllowed asserts both mint endpoints admit f's current state.
func assertMintAllowed(t *testing.T, f *gcpMintFixture) {
	t.Helper()
	t.Run("access-token", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/gcp-token", nil, f.Token)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("identity-token", func(t *testing.T) {
		body := map[string]string{"audience": "https://example.com"}
		rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/gcp-identity-token", body, f.Token)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestAgentGCPToken_BaselineFixtureAdmits pins that newGCPMintFixture's
// "everything current" starting state is itself admitted, so every deny
// test below is known to be perturbing exactly one fact away from a
// passing baseline rather than starting from an already-broken one.
func TestAgentGCPToken_BaselineFixtureAdmits(t *testing.T) {
	f := newGCPMintFixture(t, "baseline")
	assertMintAllowed(t, f)
}

// TestAgentGCPToken_DeletedAgentDenied is the deleted-agent deny case: GetAgent
// returns soft-deleted rows (store.Agent.DeletedAt), so the handler must check
// it explicitly the same way material_runtime.go does.
func TestAgentGCPToken_DeletedAgentDenied(t *testing.T) {
	f := newGCPMintFixture(t, "deleted-agent")
	f.mutateAgent(t, func(a *store.Agent) { a.DeletedAt = time.Now() })
	assertMintDenied(t, f)
}

// TestAgentGCPToken_MetadataModeNotAssignedDenied covers
// AppliedConfig.GCPIdentity.MetadataMode != assign.
func TestAgentGCPToken_MetadataModeNotAssignedDenied(t *testing.T) {
	f := newGCPMintFixture(t, "mode-mismatch")
	f.mutateAgent(t, func(a *store.Agent) {
		a.AppliedConfig.GCPIdentity.MetadataMode = store.GCPMetadataModeBlock
	})
	assertMintDenied(t, f)
}

// TestResolveAgentGCPMintFacts_NilIdentityDenied is a direct unit test of
// resolveAgentGCPMintFacts: a nil *store.GCPIdentityConfig must deny rather
// than dereference ServiceAccountID. Both handlers in handlers_gcp_identity.go
// call resolveAgentGCPMintFacts only after resolveAgentGCPAssignment returns
// ok == true, which never pairs with a nil config, so this case is exercised
// here directly rather than through a handler.
func TestResolveAgentGCPMintFacts_NilIdentityDenied(t *testing.T) {
	srv, _ := testServer(t)
	allowed := srv.resolveAgentGCPMintFacts(context.Background(), nil, "some-project")
	require.False(t, allowed)
}

// TestAgentGCPToken_RequiresCurrentServiceAccountRecord: the
// assigned SA row must still exist. A deleted record denies exactly like a
// missing one.
func TestAgentGCPToken_RequiresCurrentServiceAccountRecord(t *testing.T) {
	f := newGCPMintFixture(t, "sa-deleted")
	require.NoError(t, f.Store.DeleteGCPServiceAccount(context.Background(), f.SA.ID))
	assertMintDenied(t, f)
}

// TestAgentGCPToken_UnverifiedServiceAccountDenied: Verified=false
// and/or VerificationStatus != verified must deny even though the row exists
// and the JWT scope still names it.
func TestAgentGCPToken_UnverifiedServiceAccountDenied(t *testing.T) {
	f := newGCPMintFixture(t, "sa-unverified")
	f.mutateSA(t, func(sa *store.GCPServiceAccount) {
		sa.Verified = false
		sa.VerificationStatus = store.GCPVerificationUnverified
	})
	assertMintDenied(t, f)
}

// TestAgentGCPToken_VerificationStatusFailedDenied is
// UnverifiedServiceAccountDenied's other half: a verification attempt that ran
// and failed (VerificationStatus == failed) denies exactly like a row that was
// never verified. entadapter.normalizeGCPVerification
// (pkg/store/entadapter/external_store.go) forces VerificationStatus back to
// "verified" whenever Verified is true, so this state requires Verified ==
// false too -- the same store invariant runGCPServiceAccountVerification
// relies on when it persists a failed check.
func TestAgentGCPToken_VerificationStatusFailedDenied(t *testing.T) {
	f := newGCPMintFixture(t, "sa-verification-failed")
	f.mutateSA(t, func(sa *store.GCPServiceAccount) {
		sa.Verified = false
		sa.VerificationStatus = store.GCPVerificationFailed
	})
	assertMintDenied(t, f)
}

// TestAgentGCPToken_ServiceAccountEmailMismatchDenied: the SA row's current
// email must match the denormalized AppliedConfig.GCPIdentity.ServiceAccountEmail
// captured at assignment time. If the SA's email was changed since (or
// re-registered under new ownership), the mint denies rather than trusting
// the stale denormalized copy.
func TestAgentGCPToken_ServiceAccountEmailMismatchDenied(t *testing.T) {
	f := newGCPMintFixture(t, "sa-email-mismatch")
	f.mutateSA(t, func(sa *store.GCPServiceAccount) {
		sa.Email = "someone-else@my-gcp-project.iam.gserviceaccount.com"
	})
	assertMintDenied(t, f)
}

// TestAgentGCPToken_ServiceAccountNotReachableFromProjectDenied: a
// project-scoped SA whose ScopeID no longer matches the agent's project
// denies, even though the row is otherwise verified and current.
func TestAgentGCPToken_ServiceAccountNotReachableFromProjectDenied(t *testing.T) {
	f := newGCPMintFixture(t, "sa-unreachable")
	otherProjectID := tid("project-unreachable-other")
	require.NoError(t, f.Store.CreateProject(context.Background(), &store.Project{
		ID: otherProjectID, Name: "other", Slug: "other-unreachable", Created: time.Now(), Updated: time.Now(),
	}))
	f.reassignSA(t, store.ScopeProject, otherProjectID, "unreachable-sa@my-gcp-project.iam.gserviceaccount.com")
	assertMintDenied(t, f)
}

// TestAgentGCPToken_HubScopedRequiresEnforceMode: a hub-scoped SA is
// reachable from every project (store.GCPServiceAccount.ReachableFromProject),
// so it additionally requires saAssignCheckMode == enforce. Mode "off" (the
// test server default) denies; enforce admits.
func TestAgentGCPToken_HubScopedRequiresEnforceMode(t *testing.T) {
	f := newGCPMintFixture(t, "hub-scoped")
	f.reassignSA(t, store.ScopeHub, tid("hub-scoped-hub"), "hub-scoped-sa@my-gcp-project.iam.gserviceaccount.com")

	f.Server.mu.Lock()
	f.Server.saAssignCheckMode = SAAssignCheckOff
	f.Server.mu.Unlock()
	assertMintDenied(t, f)

	f.Server.mu.Lock()
	f.Server.saAssignCheckMode = SAAssignCheckEnforce
	f.Server.mu.Unlock()
	assertMintAllowed(t, f)
}

// TestAgentGCPToken_AgentLookupErrorDenied pins that an agent lookup fault
// ahead of the record recheck denies with the handler's "agent not found"
// response, rather than falling through as a pass or a 500.
func TestAgentGCPToken_AgentLookupErrorDenied(t *testing.T) {
	f := newGCPMintFixture(t, "agent-lookup-err")
	f.Server.store = &gcpMintFailingStore{Store: f.Store, getAgentErr: errors.New("injected store fault")}

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/gcp-token", nil, f.Token)
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "agent not found")

	rec = doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/gcp-identity-token",
		map[string]string{"audience": "https://example.com"}, f.Token)
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "agent not found")
}

// TestAgentGCPToken_ServiceAccountLookupErrorDenied: a genuine store fault on
// the SA lookup (inside resolveAgentGCPMintFacts) must deny, not 500, and
// reads identically to "no GCP identity assigned" -- the same denial the
// handler already renders for a missing assignment -- so the fault discloses
// nothing about the store beneath it.
func TestAgentGCPToken_ServiceAccountLookupErrorDenied(t *testing.T) {
	f := newGCPMintFixture(t, "sa-lookup-err")
	f.Server.store = &gcpMintFailingStore{Store: f.Store, getGCPServiceAccountErr: errors.New("injected store fault")}

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/gcp-token", nil, f.Token)
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "no GCP identity assigned")

	rec = doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/gcp-identity-token",
		map[string]string{"audience": "https://example.com"}, f.Token)
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "no GCP identity assigned")
}

// TestAgentGCPToken_AnotherServiceAccountScopeDenied is the handler-level half
// of the scope-mapping requirement: an agent JWT scoped for a DIFFERENT
// service account than the one it is currently assigned must be denied, even
// though every record/verification/reachability/mode fact for the assigned
// SA is current.
func TestAgentGCPToken_AnotherServiceAccountScopeDenied(t *testing.T) {
	f := newGCPMintFixture(t, "wrong-sa-scope")
	otherToken, err := f.Server.agentTokenService.GenerateAgentToken(
		f.AgentID, f.ProjectID, []AgentTokenScope{GCPTokenScopeForSA(tid("some-other-sa"))}, []string{f.UserID})
	require.NoError(t, err)
	f.Token = otherToken
	assertMintDenied(t, f)
}

// TestAgentGCPToken_NoServiceAccountScopeDenied is the scope-mapping
// requirement's "no scope" case: an agent JWT with no GCP token scope at all
// denies.
func TestAgentGCPToken_NoServiceAccountScopeDenied(t *testing.T) {
	f := newGCPMintFixture(t, "no-sa-scope")
	noScopeToken, err := f.Server.agentTokenService.GenerateAgentToken(f.AgentID, f.ProjectID, nil, []string{f.UserID})
	require.NoError(t, err)
	f.Token = noScopeToken
	assertMintDenied(t, f)
}

// TestGCPServiceAccountUse_MintSucceedsWhilePermissionDecisionDenies is the
// production-like characterization this test pins: with the delegation-edge
// backfill migration complete (this test restores the marker that testServer
// removes, unlike every other test in this file) and a real owner-to-agent
// delegation edge recorded --
// the shape every hub-attested agent has post-backfill -- a direct decision
// against gcp_service_account.use through the agent's own credential still
// denies, because the delegating owner does not itself hold
// gcp_service_account.use. The mint endpoints are unaffected by that: they
// do not decide this permission, so they succeed purely on the live record
// recheck and the exact token-scope compare. This is the rule enforced: an
// agent's own credential can name the exact assigned service account;
// composing it with proof that the delegating authority currently holds
// authority to assign that account is a separate change.
func TestGCPServiceAccountUse_MintSucceedsWhilePermissionDecisionDenies(t *testing.T) {
	f := newGCPMintFixture(t, "parent-authority-pending")
	setBackfillCompleted(t, f.Store)
	createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, f.AgentID,
		store.RoleScopeProject, f.ProjectID, store.ProjectRoleOwner)

	// The mint endpoints succeed purely on the live record and exact
	// token-scope checks: they do not call Decide for gcp_service_account.use.
	assertMintAllowed(t, f)

	// A direct decision against the same permission, for the same agent and
	// service account, still denies: the delegating owner (a project owner,
	// holding gcp_service_account.assign but not gcp_service_account.use)
	// does not satisfy the delegation ceiling for this permission.
	agent := newFullAgentIdentity(f.AgentID, f.ProjectID, nil, []AgentTokenScope{GCPTokenScopeForSA(f.SA.ID)})
	d := f.Server.authzService.Decide(context.Background(), AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpServiceAccountResource(f.SA),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: the delegating owner does not hold gcp_service_account.use, so the parent proof is missing, got allowed: %s", d.Reason)
	}
	if d.DenyCause != DenyCauseCeilingDelegatorLacksPermission {
		t.Fatalf("expected the delegation-ceiling deny (delegator lacks %s), got cause=%q reason=%q",
			permissions.PermissionGCPServiceAccountUse, d.DenyCause, d.Reason)
	}
}

// gcpMintFailingStore wraps a store.Store and injects an error into exactly one
// of the two record lookups (GetAgent, GetGCPServiceAccount), for the "every
// lookup error denies" tests above. Kept local to this file: it is specific to
// the two record lookups on the mint path (GetAgent in the handlers,
// GetGCPServiceAccount in resolveAgentGCPMintFacts) and is not a general-purpose
// fixture other test files need.
type gcpMintFailingStore struct {
	store.Store
	getAgentErr             error
	getGCPServiceAccountErr error
}

func (f *gcpMintFailingStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if f.getAgentErr != nil {
		return nil, f.getAgentErr
	}
	return f.Store.GetAgent(ctx, id)
}

func (f *gcpMintFailingStore) GetGCPServiceAccount(ctx context.Context, id string) (*store.GCPServiceAccount, error) {
	if f.getGCPServiceAccountErr != nil {
		return nil, f.getGCPServiceAccountErr
	}
	return f.Store.GetGCPServiceAccount(ctx, id)
}
