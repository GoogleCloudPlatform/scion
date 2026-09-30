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

// Package hub — ptone/scion#2129: the resource-aware agent-credential path
// for gcp_service_account.use. These tests exercise the kernel-level grant
// (gcpServiceAccountUseBinding) and restriction
// (agentGCPServiceAccountUseScopeMatch/agentScopeRestriction) directly
// through Decide, independent of the HTTP handlers -- see
// handlers_gcp_identity_mint_authz_test.go for the handler-level record and
// parity tests.
package hub

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// gcpUseResource builds the Resource Decide sees for one gcp_service_account
// row, mirroring gcpServiceAccountResource without requiring a persisted row.
func gcpUseResource(id string) Resource {
	return Resource{Type: permissions.ResourceGCPServiceAccount, ID: id}
}

func TestGCPServiceAccountUse_MatchingScopeAdmits(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	agent := newFullAgentIdentity(tid("agent-match"), tid("project-match"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if !d.Allowed {
		t.Fatalf("expected admission for the exact matching SA, denied: %s", d.Reason)
	}
}

func TestGCPServiceAccountUse_AnotherAccountScopeDenied(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX, saY := tid("sa-x"), tid("sa-y")
	agent := newFullAgentIdentity(tid("agent-other"), tid("project-other"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saY),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: agent's scope names a different service account")
	}
}

func TestGCPServiceAccountUse_NoScopeDenied(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	agent := newFullAgentIdentity(tid("agent-noscope"), tid("project-noscope"), nil, nil)

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: agent JWT has no scopes at all")
	}
}

func TestGCPServiceAccountUse_PrefixOnlyScopeDenied(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	// The bare prefix, with no SA ID suffix at all -- never a real scope any
	// dispatch path issues, but exactly the shape a prefix-based admission
	// mistake would accept.
	agent := newFullAgentIdentity(tid("agent-prefix"), tid("project-prefix"), nil, []AgentTokenScope{ScopeGCPTokenPrefix})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: a bare scope prefix must not satisfy the exact per-SA match")
	}
}

func TestGCPServiceAccountUse_EmptyResourceIDDenied(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	agent := newFullAgentIdentity(tid("agent-emptyid"), tid("project-emptyid"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(""),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: empty resource ID must fail closed")
	}
}

func TestGCPServiceAccountUse_WrongResourceTypeDenied(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	agent := newFullAgentIdentity(tid("agent-wrongtype"), tid("project-wrongtype"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   Resource{Type: "secret", ID: saX},
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: a non-gcp_service_account resource type must not satisfy this permission")
	}
}

// TestGCPServiceAccountUse_WrongPermissionUnaffected pins that the
// per-instance path is keyed on the permission ID, not merely the resource
// type: deciding a different permission over the same gcp_service_account
// resource follows the ordinary (denying, since gcp_service_account.read has
// no AgentScopes either) path rather than being accidentally admitted.
func TestGCPServiceAccountUse_WrongPermissionUnaffected(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	agent := newFullAgentIdentity(tid("agent-wrongperm"), tid("project-wrongperm"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionRead,
		Permission: "gcp_service_account.read",
	})
	if d.Allowed {
		t.Fatalf("expected deny: gcp_service_account.read must not be admitted by a gcp_service_account.use scope")
	}
}

// TestGCPServiceAccountUse_ScopeAndParentKindUnrelated proves the per-instance
// match is decided from Resource.Type and Resource.ID alone: a
// gcp_service_account.use resource built the way gcpServiceAccountResource
// builds it for a hub-, project- or user-scoped row (which vary ParentType/
// ParentID, never Type/ID) all admit identically when the exact scope
// matches.
func TestGCPServiceAccountUse_ScopeAndParentKindUnrelated(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saID := tid("sa-any-scope")
	agent := newFullAgentIdentity(tid("agent-anyscope"), tid("project-anyscope"), nil, []AgentTokenScope{GCPTokenScopeForSA(saID)})

	for _, sa := range []*store.GCPServiceAccount{
		{ID: saID, Scope: store.ScopeHub, ScopeID: tid("hub")},
		{ID: saID, Scope: store.ScopeProject, ScopeID: tid("project-any")},
		{ID: saID, Scope: store.ScopeUser, ScopeID: tid("user-any")},
	} {
		d := authz.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(agent),
			Credential: credentialContextForIdentity(agent),
			Resource:   gcpServiceAccountResource(sa),
			Action:     ActionUse,
			Permission: permissions.PermissionGCPServiceAccountUse,
		})
		if !d.Allowed {
			t.Errorf("scope=%s: expected admission for the exact matching SA regardless of the account's own scope, denied: %s", sa.Scope, d.Reason)
		}
	}
}

// TestGCPServiceAccountUse_ExplainNamesPermission pins that an explained
// decision over this permission names it, the same as any other permission.
func TestGCPServiceAccountUse_ExplainNamesPermission(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-explain")
	agent := newFullAgentIdentity(tid("agent-explain"), tid("project-explain"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
		Explain:    true,
	})
	if !d.Allowed {
		t.Fatalf("expected admission, denied: %s", d.Reason)
	}
	if d.PermissionID != permissions.PermissionGCPServiceAccountUse {
		t.Errorf("Decision.PermissionID = %q, want %q", d.PermissionID, permissions.PermissionGCPServiceAccountUse)
	}
	if d.Provenance == nil || d.Provenance.Permission != permissions.PermissionGCPServiceAccountUse {
		t.Errorf("explain provenance does not name the permission: %+v", d.Provenance)
	}
}

// TestGCPServiceAccountUse_IndependentGrantStillRestricted proves the
// step-7b restriction is load-bearing even against a grant that did not come
// from the request-local synthetic binding: an agent holding a REAL role
// binding for gcp_service_account.use, independent of gcpServiceAccountUseBinding
// entirely, is still denied on a service account its JWT scope does not name.
// Without agentScopeRestriction's per-instance check this would incorrectly
// allow, since ordinary restrictions never gate a role-granted permission by
// resource ID.
func TestGCPServiceAccountUse_IndependentGrantStillRestricted(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("project-independent-grant")
	if err := s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "independent-grant", Slug: "independent-grant", Created: time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agentID := tid("agent-independent-grant")
	saX, saY := tid("sa-independent-x"), tid("sa-independent-y")

	// CreateRoleBinding validates that an "agent" principal exists, so the
	// role-granted path this test exercises needs a real persisted agent, not
	// just the in-memory AgentIdentity used for the Decide call below.
	if err := s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "agent-independent-grant", Name: "Agent Independent Grant",
		ProjectID: projectID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "test-gcp-sa-use-" + agentID,
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{permissions.PermissionGCPServiceAccountUse},
	})
	if err != nil {
		t.Fatalf("CreateRoleDefinition: %v", err)
	}
	if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      agentID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	}); err != nil {
		t.Fatalf("CreateRoleBinding: %v", err)
	}

	// The agent's JWT names SA X, not SA Y.
	agent := newFullAgentIdentity(agentID, projectID, nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saY),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: a role-granted gcp_service_account.use must still be restricted to the JWT's own SA")
	}

	// Sanity: the same role binding DOES admit the SA the JWT actually names
	// (the request-local grant is not required when a role already grants
	// it; the restriction is what narrows it, not what widens it).
	d = authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if !d.Allowed {
		t.Fatalf("expected admission for the SA the JWT names, denied: %s", d.Reason)
	}
}

// TestGCPServiceAccountUse_TargetAgnosticGrantNeverIncludesIt pins that
// agentScopesToPermissionIDs -- the target-agnostic function that also feeds
// CanDelegate's caveat intersection -- never grants gcp_service_account.use
// from a per-SA scope. The request-local grant in decide() (Step 5b3) is
// built by a different, resource-aware function and must never be folded
// into this one.
func TestGCPServiceAccountUse_TargetAgnosticGrantNeverIncludesIt(t *testing.T) {
	scopes := []AgentTokenScope{GCPTokenScopeForSA(tid("sa-agnostic"))}
	for _, id := range agentScopesToPermissionIDs(scopes) {
		if id == permissions.PermissionGCPServiceAccountUse {
			t.Fatalf("agentScopesToPermissionIDs must never grant gcp_service_account.use target-agnostically")
		}
	}
}

// TestGCPServiceAccountUse_CanDelegateUnaffected pins that CanDelegate's
// credential-caveat intersection -- which calls agentScopeRestriction with a
// zero Resource because delegation reasons about a permission set, not one
// resource instance -- cannot be satisfied by a per-SA scope. An agent must
// not be able to delegate "use this one service account" as if it were
// general gcp_service_account.use authority.
func TestGCPServiceAccountUse_CanDelegateUnaffected(t *testing.T) {
	authz, _ := authzTestSetup(t)
	saX := tid("sa-candelegate")
	agent := newFullAgentIdentity(tid("agent-candelegate"), tid("project-candelegate"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	filtered := authz.intersectCredentialCaveats(agent, []string{permissions.PermissionGCPServiceAccountUse})
	if len(filtered) != 0 {
		t.Fatalf("CanDelegate's caveat intersection must not admit gcp_service_account.use from a per-SA scope, got %v", filtered)
	}
}

// TestGCPServiceAccountUse_ScopeCapabilityNeverAdmitsUnscoped pins G4: a
// scope-level (instance-less) capability computation -- ComputeScopeCapabilities,
// whose Resource never carries an ID -- must never report gcp_service_account.use
// as held, no matter what per-SA scope the agent's JWT carries. Only a
// concrete-resource capability check (ComputeCapabilities/ComputeCapabilitiesBatch
// against one SA's own resource) can ever show it.
func TestGCPServiceAccountUse_ScopeCapabilityNeverAdmitsUnscoped(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-scope-cap")
	agent := newFullAgentIdentity(tid("agent-scope-cap"), tid("project-scope-cap"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	caps := authz.ComputeScopeCapabilities(ctx, agent, "project", tid("project-scope-cap"), permissions.ResourceGCPServiceAccount)
	for _, a := range caps.Actions {
		if a == string(ActionUse) {
			t.Fatalf("scope-level (unscoped) capabilities must not report %q as held: %v", ActionUse, caps.Actions)
		}
	}
}

// TestGCPServiceAccountUse_OnlyPermissionOnPerInstancePath is the Registry-walk
// regression test: exactly one permission (gcp_service_account.use) is
// decided by the per-instance resource match, and every other registered
// permission's agent-scope restriction behavior is byte-identical to the
// pre-existing static AgentScopes lookup.
func TestGCPServiceAccountUse_OnlyPermissionOnPerInstancePath(t *testing.T) {
	saID := tid("sa-registry-walk")
	scopes := append(append([]AgentTokenScope{}, allRegisteredAgentScopes()...), GCPTokenScopeForSA(saID))
	agent := newFullAgentIdentity(tid("agent-registry-walk"), tid("project-registry-walk"), nil, scopes)

	restriction := agentScopeRestriction(agent, gcpUseResource(saID))

	scopeSet := make(map[string]bool, len(scopes))
	for _, sc := range scopes {
		scopeSet[string(sc)] = true
	}
	staticAllowed := make(map[string]bool)
	for _, p := range permissions.Registry {
		for _, sc := range p.AgentScopes {
			if scopeSet[sc] {
				staticAllowed[p.ID] = true
				break
			}
		}
	}

	perInstanceCount := 0
	for _, p := range permissions.Registry {
		got := restriction.Check(p.ID)
		if p.ID == permissions.PermissionGCPServiceAccountUse {
			perInstanceCount++
			if !got {
				t.Errorf("%s: expected per-instance admission for the exact matching SA, got denied", p.ID)
			}
			continue
		}
		if got != staticAllowed[p.ID] {
			t.Errorf("%s: agent-scope restriction diverged from the static AgentScopes path (got %v, want %v)", p.ID, got, staticAllowed[p.ID])
		}
	}
	if perInstanceCount != 1 {
		t.Fatalf("expected exactly one permission on the per-instance path, found %d", perInstanceCount)
	}
}

// TestGCPServiceAccountUse_MutationIDComparisonMatters pins that
// agentGCPServiceAccountUseScopeMatch compares the exact resource ID, not
// merely the presence of some GCP token scope. This test fails if that
// comparison is weakened to "the agent holds ANY project:gcp:token:* scope"
// -- confirmed manually by editing the comparison out and observing this
// test (and TestGCPServiceAccountUse_AnotherAccountScopeDenied) fail; see
// the dev report for this change.
func TestGCPServiceAccountUse_MutationIDComparisonMatters(t *testing.T) {
	saX, saY := tid("sa-mutation-x"), tid("sa-mutation-y")
	agent := newFullAgentIdentity(tid("agent-mutation"), tid("project-mutation"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	if !agentGCPServiceAccountUseScopeMatch(agent, permissions.PermissionGCPServiceAccountUse, gcpUseResource(saX)) {
		t.Fatalf("expected match for the exact SA the JWT names")
	}
	if agentGCPServiceAccountUseScopeMatch(agent, permissions.PermissionGCPServiceAccountUse, gcpUseResource(saY)) {
		t.Fatalf("expected no match for a different SA ID -- the ID comparison must be exact")
	}
}
