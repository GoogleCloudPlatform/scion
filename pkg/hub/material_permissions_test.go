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
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
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

// TestSecretUse_AgentScopeDoesNotGrantUserMaterial covers the explicit
// agent-scope mapping's limit: a full-role agent's project:secret:read scope gives it a synthetic
// binding for secret.use, but that binding is project-scoped
// (buildAgentSyntheticBindings), so it does not reach a user-owned resource
// with no progeny relationship. A raw Decide call for secret.use against
// such a resource still denies.
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
	if d.Allowed {
		t.Fatalf("expected deny: a full-role agent's project:secret:read scope must not grant a user-scope secret with no progeny relationship, got allowed (reason=%q)", d.Reason)
	}
}

// TestSecretUse_ProjectSecretRequiresProjectSecretRead pins the two-permission
// composite for project-scope material: an ordinary project owner's role
// holds project.secret_read (seeded), not secret.use (held only by
// super-admin, through every registry permission). So a raw Decide call for
// secret.use on the project resource still denies through the delegation
// ceiling -- checkUserHoldsPermission checks the delegator against the
// exact requested permission, and an owner delegator does not hold
// secret.use -- even though the identical request for project.secret_read
// on the same resource, from the same delegator, is admitted. Project-scope
// material access needs project.secret_read specifically; secret.use alone
// is not enough.
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

// TestMaterialUse_CeilingStoreErrorDenies pins that secret.use participates
// in the same fail-closed delegation-ceiling behaviour as any other
// non-read-only action: a genuine edge-lookup store fault denies rather than
// allowing, even though the agent's own JWT scope would otherwise satisfy
// the kernel decision.
func TestMaterialUse_CeilingStoreErrorDenies(t *testing.T) {
	f := newMaterialFixture(t, "secretuse-ceiling-store-error")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)

	failing := &materialFailingStore{
		Store:                            f.Store,
		getDelegationEdgesForDelegateErr: errors.New("injected edge lookup failure"),
	}
	f.Server.authzService = NewAuthzService(failing, logging.Subsystem("hub.auth"))

	ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	d := f.Server.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(ident),
		Credential: credentialContextForIdentity(ident),
		Resource:   Resource{Type: "project", ID: f.ProjectID},
		Action:     ActionUse,
		Permission: "secret.use",
	})
	if d.Allowed {
		t.Fatal("expected deny: a genuine edge-lookup fault must fail closed for a non-read-only action")
	}
}

// TestMaterialPermissions_SuperAdminHoldsDeliverButNeedsAssociation pins
// that holding a *.deliver permission through a role is not the whole
// story: super-admin holds every registry permission, including
// secret.deliver, through allPermissionIDs, but this base has no
// association/progeny/skill_default relationship grant and no hub-delivery
// credential concept yet (both land with the delivery pipeline), so a raw
// Decide call for a super-admin user principal still denies for lack of any
// grant on the item.
func TestMaterialPermissions_SuperAdminHoldsDeliverButNeedsAssociation(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	adminID := tid("superadmin-deliver")
	createTestUserWithRole(t, s, adminID, "superadmin-deliver@test.com", "member", store.SystemRoleSuperAdmin)
	projectID := tid("project-superadmin-deliver")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-superadmin-deliver", Created: time.Now(), Updated: time.Now(),
	}))

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  activeUserPrincipal(adminID),
		Credential: CredentialContext{Kind: CredentialKindInteractive},
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
	restriction := agentScopeRestriction(agent)
	for _, id := range []string{"secret.deliver", "env_var.deliver", "skill_injection.deliver"} {
		if restriction.Check(id) {
			t.Errorf("agent JWT scope restriction unexpectedly allows %q even with every registered scope present", id)
		}
	}
}

// TestDispatchDelivery_UnreviewedClassDeniedBeforeMembership pins the A.1
// ordering guarantee (ProjectAdmissionForClass validates class/permission
// coherence before either the membership or the system-authority branch)
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
