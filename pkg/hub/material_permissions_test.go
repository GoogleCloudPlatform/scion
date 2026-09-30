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

// Package hub — registry and admission tests for the material delivery and
// runtime-use permissions (ptone/scion#2129): the five new rows, the
// explicit secret.use <-> project:secret:read mapping and its consequences,
// and that class/permission validation for the newly registered material
// classes still runs before either admission branch.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/require"
)

// TestMaterialPermissions_Registered pins the five new registry
// rows' shape against the design table: resource, action, the explicit
// AgentScopes mapping (secret.use only), an empty UATScope, and a reviewed
// true ProjectTargetApplicability entry for each.
func TestMaterialPermissions_Registered(t *testing.T) {
	want := map[string]struct {
		resource    string
		action      string
		agentScopes []string
	}{
		"secret.deliver":          {"secret", "deliver", nil},
		"env_var.deliver":         {"env_var", "deliver", nil},
		"skill_injection.deliver": {"skill_injection", "deliver", nil},
		"secret.use":              {"secret", "use", []string{"project:secret:read"}},
		"gcp_service_account.use": {"gcp_service_account", "use", nil},
	}
	byID := map[string]permissions.Permission{}
	for _, p := range permissions.Registry {
		byID[p.ID] = p
	}
	for id, w := range want {
		p, ok := byID[id]
		if !ok {
			t.Errorf("permission %q is not registered", id)
			continue
		}
		if p.Resource != w.resource {
			t.Errorf("%s: Resource = %q, want %q", id, p.Resource, w.resource)
		}
		if p.Action != w.action {
			t.Errorf("%s: Action = %q, want %q", id, p.Action, w.action)
		}
		if !slices.Equal(p.AgentScopes, w.agentScopes) {
			t.Errorf("%s: AgentScopes = %v, want %v", id, p.AgentScopes, w.agentScopes)
		}
		if p.UATScope != "" {
			t.Errorf("%s: UATScope = %q, want empty", id, p.UATScope)
		}
		applies, reviewed := permissions.AppliesToExistingProjectTarget(id)
		if !reviewed || !applies {
			t.Errorf("%s: ProjectTargetApplicability = (applies=%v, reviewed=%v), want (true, true)", id, applies, reviewed)
		}
	}
}

// TestMaterialPermissions_AgentScopeMappingExplicit pins the explicit
// agent-scope mapping's exclusivity: no permission other than
// project.secret_read and secret.use
// carries the project:secret:read agent scope, and none of the five new
// rows has a UATScope, so none of them can ever appear as a selector in
// permissions.ResolveSelector.
func TestMaterialPermissions_AgentScopeMappingExplicit(t *testing.T) {
	for _, p := range permissions.Registry {
		if p.ID == "project.secret_read" || p.ID == "secret.use" {
			continue
		}
		if slices.Contains(p.AgentScopes, "project:secret:read") {
			t.Errorf("permission %q unexpectedly carries the project:secret:read agent scope", p.ID)
		}
	}
	for _, id := range []string{"secret.deliver", "env_var.deliver", "skill_injection.deliver", "secret.use", "gcp_service_account.use"} {
		if _, ok := permissions.ResolveSelector(id); ok {
			t.Errorf("permission %q unexpectedly resolves as a selector; none of the material rows has a UATScope", id)
		}
	}
}

// TestMaterialPermissions_NotInCuratedSeedRoles pins the rule that none of
// these permissions is added to a curated role directly against the curated
// role builders: none of secret.deliver, env_var.deliver,
// skill_injection.deliver or gcp_service_account.use is ever included in the
// seeded hub-admin, hub-member or hub-viewer permission sets. secret.use is
// excluded from this table because its explicit project:secret:read mapping
// deliberately reaches agent JWTs through AgentScopes, not through any
// curated role list.
func TestMaterialPermissions_NotInCuratedSeedRoles(t *testing.T) {
	roles := map[string][]string{
		"hub-admin":  hubAdminPermissionIDs(),
		"hub-member": hubMemberPermissionIDs(),
		"hub-viewer": hubViewerPermissionIDs(),
	}
	unwanted := []string{"secret.deliver", "env_var.deliver", "skill_injection.deliver", "gcp_service_account.use"}
	for roleName, perms := range roles {
		permSet := make(map[string]bool, len(perms))
		for _, p := range perms {
			permSet[p] = true
		}
		for _, id := range unwanted {
			if permSet[id] {
				t.Errorf("seeded role %q unexpectedly includes %q", roleName, id)
			}
		}
	}
}

// TestSecretUse_AgentScopeDoesNotGrantUserMaterial covers the explicit
// agent-scope mapping's limit: a full-role agent's project:secret:read scope gives it a synthetic
// binding for secret.use, but that binding is project-scoped
// (buildAgentSyntheticBindings), so it does not reach a user-owned resource
// with no progeny relationship. A raw Decide call for secret.use against
// such a resource still denies with no candidate binding at all -- not
// merely a ceiling denial that a different delegator could satisfy.
func TestSecretUse_AgentScopeDoesNotGrantUserMaterial(t *testing.T) {
	f := newMaterialFixture(t, "secretuse-no-user-material")
	ctx := context.Background()

	ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	d := f.Server.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(ident),
		Credential: credentialContextForIdentity(ident),
		Resource:   Resource{Type: "secret", ID: tid("some-user-secret"), OwnerID: f.UserID, ParentType: "user", ParentID: f.UserID},
		Action:     ActionUse,
		Permission: "secret.use",
	})
	wantReason := `no active binding grants permission "secret.use"`
	if d.Allowed || d.Reason != wantReason {
		t.Fatalf("expected deny with reason %q: a full-role agent's project:secret:read scope must not grant a user-scope secret with no progeny relationship, got allowed=%v reason=%q", wantReason, d.Allowed, d.Reason)
	}
}

// TestSecretUse_ProjectSecretRequiresProjectSecretRead pins the raw-Decide
// half of the two-permission composite for project-scope material: an
// ordinary project owner's role holds project.secret_read (seeded), not
// secret.use (among the seeded user roles, held only by super-admin,
// through every registry permission).
// So a raw Decide call for secret.use on the project resource still denies
// through the delegation ceiling -- checkUserHoldsPermission checks the
// delegator against the exact requested permission, and an owner delegator
// does not hold secret.use -- even though the identical request for
// project.secret_read on the same resource, from the same delegator, is
// admitted. This shows the two permissions decide independently; it does not
// exercise the runtime read path itself. See
// TestAgentSecretRead_SystemAuthorityForSecretUseDoesNotSubstituteForProjectSecretRead
// below for the end-to-end consequence: once the delegation-edge backfill
// has completed, a root admitted at check 5 only through secret.use system
// authority still cannot read a project secret, because check 7 decides on
// project.secret_read specifically.
func TestSecretUse_ProjectSecretRequiresProjectSecretRead(t *testing.T) {
	f := newMaterialFixture(t, "secretuse-composite")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)

	ownerDelegator := tid("secretuse-owner-delegator")
	createDCUser(t, f.Store, ownerDelegator, "secretuse-owner-delegator@test.com", f.ProjectID, store.ProjectRoleOwner)
	ownerAgentID := tid("secretuse-owner-delegate-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: ownerAgentID, Slug: "secretuse-owner-delegate", Name: "owner", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{ownerDelegator},
		Created: time.Now(), Updated: time.Now(),
	}))
	createDCEdge(t, f.Store, store.DelegationPrincipalUser, ownerDelegator, store.DelegationPrincipalAgent, ownerAgentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	ident := newFullAgentIdentity(ownerAgentID, f.ProjectID, []string{ownerDelegator}, []AgentTokenScope{ScopeProjectSecretRead})
	resource := Resource{Type: "project", ID: f.ProjectID}

	dUse := f.Server.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(ident),
		Credential: credentialContextForIdentity(ident),
		Resource:   resource,
		Action:     ActionUse,
		Permission: "secret.use",
	})
	if dUse.Allowed {
		t.Fatalf("expected deny: an owner delegator holds project.secret_read, not secret.use, so a raw secret.use decision must still deny (reason=%q)", dUse.Reason)
	}

	dRead := f.Server.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(ident),
		Credential: credentialContextForIdentity(ident),
		Resource:   resource,
		Action:     actionProjectSecretRead,
		Permission: "project.secret_read",
	})
	if !dRead.Allowed {
		t.Fatalf("expected allow: the same owner delegator holds project.secret_read (reason=%q)", dRead.Reason)
	}
}

// TestAgentSecretRead_SystemAuthorityForSecretUseDoesNotSubstituteForProjectSecretRead
// pins the end-to-end consequence of the two-permission composite: a root
// admitted at check 5 only through secret.use system authority (a custom
// system role holding secret.use, no project membership and no
// project.secret_read anywhere) still cannot read a project secret once the
// delegation-edge backfill has completed (the test sets the marker and
// records an edge). Check 7 always decides on project.secret_read
// specifically, and this root's delegate (an agent with an active
// delegation edge) is denied by the ceiling for that exact permission, so
// the read reports not_found with no value, and the audited item names
// project.secret_read as the permission it was decided on.
func TestAgentSecretRead_SystemAuthorityForSecretUseDoesNotSubstituteForProjectSecretRead(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-sysauth-composite")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-sysauth-composite", Created: time.Now(), Updated: time.Now(),
	}))
	userID := tid("user-sysauth-composite")
	systemRoleUserWithPermissions(t, s, userID, []string{"secret.use"})
	agentID := tid("agent-sysauth-composite")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-sysauth-composite", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))
	setBackfillCompleted(t, s)
	createDCEdge(t, s, store.DelegationPrincipalUser, userID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	token, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{userID})
	require.NoError(t, err)
	seedSecret(t, srv.secretBackend, "SYSAUTH_KEY", "v", "", "", projectID)

	auditor := newRecordingMaterialAuditor()
	srv.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"SYSAUTH_KEY"}}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp secretFetchResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	if len(resp.Secrets) != 1 || resp.Secrets[0].Status != "not_found" || resp.Secrets[0].Value != "" {
		t.Fatalf("expected not_found with no value (secret.use system authority does not substitute for project.secret_read), got %+v", resp.Secrets)
	}

	if len(auditor.events) == 0 || len(auditor.events[len(auditor.events)-1].Items) == 0 {
		t.Fatal("expected a material selection audit event with at least one item")
	}
	item := auditor.events[len(auditor.events)-1].Items[0]
	if item.Permission != "project.secret_read" {
		t.Errorf("expected the audited item's permission to be project.secret_read, got %q", item.Permission)
	}
}

// TestMaterialUse_CeilingStoreErrorDenies pins that secret.use participates
// in the same fail-closed delegation-ceiling behaviour as any other
// non-read-only action: a genuine edge-lookup store fault denies rather than
// allowing, even though the agent's own JWT scope would otherwise satisfy
// the kernel decision. The positive control (no injected fault) confirms the
// same request is allowed with a real delegation edge and a delegator that
// holds secret.use, so the later denial is caused by the fault and not by an
// edge or permission that was already missing.
func TestMaterialUse_CeilingStoreErrorDenies(t *testing.T) {
	f := newMaterialFixture(t, "secretuse-ceiling-store-error")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)

	// A super-admin delegator holds secret.use through allPermissionIDs, and
	// a recorded delegation edge exists, so the positive control below can
	// only pass because the ceiling's normal edge lookup succeeds -- not
	// because no delegator holds the permission or no edge was ever checked.
	superAdminDelegator := tid("secretuse-ceiling-superadmin")
	createTestUserWithRole(t, f.Store, superAdminDelegator, "secretuse-ceiling-superadmin@test.com", "member", store.SystemRoleSuperAdmin)
	delegateAgentID := tid("secretuse-ceiling-delegate-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: delegateAgentID, Slug: "secretuse-ceiling-delegate", Name: "delegate", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{superAdminDelegator},
		Created: time.Now(), Updated: time.Now(),
	}))
	createDCEdge(t, f.Store, store.DelegationPrincipalUser, superAdminDelegator, store.DelegationPrincipalAgent, delegateAgentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	ident := newFullAgentIdentity(delegateAgentID, f.ProjectID, []string{superAdminDelegator}, []AgentTokenScope{ScopeProjectSecretRead})
	request := AuthzRequest{
		Principal:  principalContextForIdentity(ident),
		Credential: credentialContextForIdentity(ident),
		Resource:   Resource{Type: "project", ID: f.ProjectID},
		Action:     ActionUse,
		Permission: "secret.use",
	}

	dControl := f.Server.authzService.Decide(ctx, request)
	if !dControl.Allowed {
		t.Fatalf("positive control: expected allow with a real delegation edge and a super-admin delegator holding secret.use, got deny (reason=%q)", dControl.Reason)
	}

	failing := &materialFailingStore{
		Store:                            f.Store,
		getDelegationEdgesForDelegateErr: errors.New("injected edge lookup failure"),
	}
	f.Server.authzService = NewAuthzService(failing, logging.Subsystem("hub.auth"))

	d := f.Server.authzService.Decide(ctx, request)
	if d.Allowed {
		t.Fatal("expected deny: a genuine edge-lookup fault must fail closed for a non-read-only action")
	}
}

// TestMaterialPermissions_SuperAdminHoldsDeliverButNeedsAssociation pins
// that holding a *.deliver permission through a role is not the whole
// story: super-admin holds every registry permission, including
// secret.deliver, through allPermissionIDs. The positive control shows the
// same super-admin, built from a real identity, admitted for an ordinary
// permission (project.update), so the principal itself is not the reason a
// deliver decision would deny. The delivery-only assertion is skipped: the
// internal hub_delivery credential kind and its restriction (denying
// *.deliver for every other credential kind, including this one) are a
// separate change tracked at ptone/scion#2228, not yet on this branch. Until
// that restriction lands, ordinary role evaluation can still reach these
// rows; no endpoint in this change consumes them.
func TestMaterialPermissions_SuperAdminHoldsDeliverButNeedsAssociation(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	adminID := tid("superadmin-deliver")
	createTestUserWithRole(t, s, adminID, "superadmin-deliver@test.com", "member", store.SystemRoleSuperAdmin)
	projectID := tid("project-superadmin-deliver")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-superadmin-deliver", Created: time.Now(), Updated: time.Now(),
	}))

	admin := principalContextForIdentity(NewAuthenticatedUser(adminID, "superadmin-deliver@test.com", "Admin", "admin", "cli"))
	credential := CredentialContext{Kind: CredentialKindInteractive}

	dControl := authz.Decide(ctx, AuthzRequest{
		Principal:  admin,
		Credential: credential,
		Resource:   Resource{Type: "project", ID: projectID},
		Action:     ActionUpdate,
		Permission: "project.update",
	})
	if !dControl.Allowed || dControl.Reason == "missing principal" {
		t.Fatalf("positive control: expected super-admin allowed for project.update with a real identity, got allowed=%v reason=%q", dControl.Allowed, dControl.Reason)
	}

	t.Skip("secret.deliver enforcement depends on the hub_delivery credential restriction, ptone/scion#2228 (not yet on this branch); un-skip and assert deny once that restriction lands")

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  admin,
		Credential: credential,
		Resource:   Resource{Type: "secret", ID: tid("superadmin-deliver-secret"), ParentType: "project", ParentID: projectID},
		Action:     ActionDeliver,
		Permission: "secret.deliver",
	})
	if d.Allowed {
		t.Fatalf("expected deny: super-admin holds secret.deliver through a role, but delivery needs a grant this base does not yet provide (reason=%q)", d.Reason)
	}
}

// TestAgentToken_CannotSatisfyDeliveryPermission pins that *.deliver has no
// AgentScopes at all: an agent identity carrying every registered agent
// scope still cannot satisfy a delivery permission through
// agentScopeRestriction, because no scope is ever declared for these rows.
func TestAgentToken_CannotSatisfyDeliveryPermission(t *testing.T) {
	agent := newFullAgentIdentity(tid("deliver-scope-agent"), tid("deliver-scope-project"),
		[]string{tid("deliver-scope-user")}, allRegisteredAgentScopes())
	restriction := agentScopeRestriction(agent, Resource{})
	for _, id := range []string{"secret.deliver", "env_var.deliver", "skill_injection.deliver"} {
		if restriction.Check(id) {
			t.Errorf("agent JWT scope restriction unexpectedly allows %q even with every registered scope present", id)
		}
	}
}

// TestDispatchDelivery_UnreviewedClassDeniedBeforeMembership pins
// ProjectAdmissionForClass's ordering guarantee (it validates
// class/permission coherence before either the membership or the
// system-authority branch)
// against the newly registered material classes: a principal with real
// project membership is still denied outright for an unreviewed ScopeKind.
func TestDispatchDelivery_UnreviewedClassDeniedBeforeMembership(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("unreviewed-class-member")
	projectID := tid("unreviewed-class-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-unreviewed-class", Created: time.Now(), Updated: time.Now(),
	}))
	createDCUser(t, s, userID, "unreviewed-class@test.com", projectID, store.ProjectRoleOwner)

	_, err := authz.ProjectAdmissionForClass(ctx, activeUserPrincipal(userID), projectID, "secret.deliver",
		ProjectTargetClass{ResourceType: "secret", ScopeKind: "bogus-scope-kind"}, nil)
	if err == nil {
		t.Fatal("expected an error for an unreviewed scope kind, even though the principal has real project membership")
	}
}

// TestDispatchDelivery_UnreviewedPermissionDeniedBeforeMembership is the
// same ordering guarantee for an unregistered permission ID.
func TestDispatchDelivery_UnreviewedPermissionDeniedBeforeMembership(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("unreviewed-perm-member")
	projectID := tid("unreviewed-perm-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-unreviewed-perm", Created: time.Now(), Updated: time.Now(),
	}))
	createDCUser(t, s, userID, "unreviewed-perm@test.com", projectID, store.ProjectRoleOwner)

	_, err := authz.ProjectAdmissionForClass(ctx, activeUserPrincipal(userID), projectID, "does.not.exist.in.registry",
		ProjectTargetClass{ResourceType: "secret", ScopeKind: "project"}, nil)
	if err == nil {
		t.Fatal("expected an error for an unregistered permission ID, even though the principal has real project membership")
	}
}
