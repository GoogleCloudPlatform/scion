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

package hub

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test helpers
// =============================================================================

// testRelAgentIdentity is a test-only AgentIdentity for relationship grant tests.
// It carries a configurable ancestry chain and is hub-attested (non-federated).
type testRelAgentIdentity struct {
	id        string
	projectID string
	ancestry  []string
}

func (a *testRelAgentIdentity) ID() string                    { return a.id }
func (a *testRelAgentIdentity) Type() string                  { return "agent" }
func (a *testRelAgentIdentity) ProjectID() string             { return a.projectID }
func (a *testRelAgentIdentity) Scopes() []AgentTokenScope     { return nil }
func (a *testRelAgentIdentity) HasScope(AgentTokenScope) bool { return false }
func (a *testRelAgentIdentity) Ancestry() []string            { return a.ancestry }
func (a *testRelAgentIdentity) OriginUserID() string {
	if len(a.ancestry) > 0 {
		return a.ancestry[0]
	}
	return ""
}
func (a *testRelAgentIdentity) TokenID() string { return "" }

// =============================================================================
// Merge gate: Forged/federated ancestry cannot grant local authority
// =============================================================================

func TestRelationshipGrant_FederatedAncestryDenied(t *testing.T) {
	// A federated agent claiming ancestry from a local user must be denied.
	// The ancestry is a remote claim and cannot be trusted for local delegation.
	federatedAgent := NewFederatedAgentIdentity(
		"https://remote-hub.example.com",
		"remote-agent-1",
		"remote-project",
		"Remote Agent",
		"local-user-id",
		[]string{"local-user-id"},
		nil,
	)

	result := EvaluateProgenyGrant(
		federatedAgent,
		RelProgenySecretRead,
		"secret-123",
		"secret",
		"local-user-id",
		true,
	)

	assert.False(t, result.Allowed, "federated agent must not get progeny access")
	assert.Contains(t, result.DenyReason, "hub-attested")
	assert.Contains(t, result.Provenance.RejectReasons[0], "hub-attested")
}

func TestRelationshipGrant_FederatedAncestryDenied_AllTypes(t *testing.T) {
	federatedAgent := NewFederatedAgentIdentity(
		"https://evil.example.com", "agent-x", "", "Evil Agent",
		"victim-user", []string{"victim-user"}, nil,
	)

	tests := []struct {
		name         string
		relType      RelationshipType
		resourceType string
	}{
		{"secret", RelProgenySecretRead, "secret"},
		{"envvar", RelProgenyEnvVarRead, "envvar"},
		{"skill_injection", RelProgenySkillInjectionRead, "skill_injection"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EvaluateProgenyGrant(
				federatedAgent, tt.relType, "resource-1", tt.resourceType,
				"victim-user", true,
			)
			assert.False(t, result.Allowed,
				"federated agent must not get progeny access for %s", tt.name)
			assert.Equal(t, tt.relType, result.RelationshipType)
		})
	}
}

// =============================================================================
// Merge gate: Relationship access revoked when source/delegation edge removed
// =============================================================================

func TestRelationshipGrant_RevokedWhenAllowProgenyDisabled(t *testing.T) {
	agent := &testRelAgentIdentity{
		id:        "agent-1",
		projectID: "project-1",
		ancestry:  []string{"user-creator"},
	}

	// With AllowProgeny=true: access granted
	result := EvaluateProgenyGrant(
		agent, RelProgenySecretRead, "secret-1", "secret",
		"user-creator", true,
	)
	assert.True(t, result.Allowed, "should be granted when AllowProgeny=true")

	// With AllowProgeny=false: access revoked immediately
	result = EvaluateProgenyGrant(
		agent, RelProgenySecretRead, "secret-1", "secret",
		"user-creator", false,
	)
	assert.False(t, result.Allowed, "should be revoked when AllowProgeny=false")
	assert.Contains(t, result.DenyReason, "AllowProgeny")
}

func TestRelationshipGrant_RevokedWhenAncestryRemoved(t *testing.T) {
	// Agent with creator in ancestry: allowed
	agentWithAncestry := &testRelAgentIdentity{
		id:        "agent-1",
		projectID: "project-1",
		ancestry:  []string{"user-creator"},
	}
	result := EvaluateProgenyGrant(
		agentWithAncestry, RelProgenySecretRead, "secret-1", "secret",
		"user-creator", true,
	)
	assert.True(t, result.Allowed)

	// Agent with empty ancestry (edge removed): denied
	agentNoAncestry := &testRelAgentIdentity{
		id:        "agent-1",
		projectID: "project-1",
		ancestry:  []string{},
	}
	result = EvaluateProgenyGrant(
		agentNoAncestry, RelProgenySecretRead, "secret-1", "secret",
		"user-creator", true,
	)
	assert.False(t, result.Allowed, "should be denied when ancestry is empty")

	// Agent with different ancestry (delegation edge changed): denied
	agentDiffAncestry := &testRelAgentIdentity{
		id:        "agent-1",
		projectID: "project-1",
		ancestry:  []string{"different-user"},
	}
	result = EvaluateProgenyGrant(
		agentDiffAncestry, RelProgenySecretRead, "secret-1", "secret",
		"user-creator", true,
	)
	assert.False(t, result.Allowed, "should be denied when creator not in ancestry")
	assert.Contains(t, result.DenyReason, "not in agent's ancestry")
}

// =============================================================================
// Merge gate: Relationship grants produce proper provenance
// =============================================================================

func TestRelationshipGrant_ProvenanceFormat(t *testing.T) {
	agent := &testRelAgentIdentity{
		id:        "agent-progeny-1",
		projectID: "project-alpha",
		ancestry:  []string{"user-creator-1"},
	}

	tests := []struct {
		name         string
		relType      RelationshipType
		resourceType string
		resourceID   string
	}{
		{"secret", RelProgenySecretRead, "secret", "secret-abc"},
		{"envvar", RelProgenyEnvVarRead, "envvar", "envvar-def"},
		{"skill_injection", RelProgenySkillInjectionRead, "skill_injection", "skill-ghi"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EvaluateProgenyGrant(
				agent, tt.relType, tt.resourceID, tt.resourceType,
				"user-creator-1", true,
			)
			require.True(t, result.Allowed)

			prov := result.Provenance
			// Same fields as a RoleBinding GrantProvenance
			assert.NotEmpty(t, prov.BindingID, "BindingID must be set")
			assert.Equal(t, string(tt.relType), prov.RoleID, "RoleID must match relationship type")
			assert.Equal(t, "builtin:relationship:"+string(tt.relType), prov.RoleName,
				"RoleName must follow builtin:relationship: naming")
			assert.Equal(t, ScopeTypeRelationship, prov.ScopeType)
			assert.Equal(t, "agent-progeny-1", prov.PrincipalID)
			assert.Equal(t, "agent", prov.PrincipalType)
			assert.Equal(t, []string{"agent-progeny-1"}, prov.MembershipPath)
			assert.True(t, prov.Contributed)
			assert.Contains(t, prov.Permissions, tt.resourceType+".read")
			assert.Empty(t, prov.RejectReasons, "no reject reasons for allowed grant")
		})
	}
}

func TestRelationshipGrant_DeniedProvenanceFormat(t *testing.T) {
	agent := &testRelAgentIdentity{
		id:        "agent-outsider",
		projectID: "project-beta",
		ancestry:  []string{"other-user"},
	}

	result := EvaluateProgenyGrant(
		agent, RelProgenySecretRead, "secret-abc", "secret",
		"user-creator-1", true,
	)
	assert.False(t, result.Allowed)

	prov := result.Provenance
	assert.False(t, prov.Contributed)
	assert.NotEmpty(t, prov.RejectReasons)
	assert.Equal(t, string(RelProgenySecretRead), prov.RoleID)
	assert.Equal(t, "builtin:relationship:progeny_secret_read", prov.RoleName)
}

// =============================================================================
// Merge gate: Existing progeny access patterns continue to work
// =============================================================================

func TestRelationshipGrant_SecretRead(t *testing.T) {
	agent := &testRelAgentIdentity{
		id:        "agent-child-1",
		projectID: "project-1",
		ancestry:  []string{"root-user", "intermediate-agent"},
	}

	result := EvaluateProgenyGrant(
		agent, RelProgenySecretRead, "secret-xyz", "secret",
		"root-user", true,
	)
	assert.True(t, result.Allowed, "progeny agent should access secrets created by ancestor")
	assert.Equal(t, RelProgenySecretRead, result.RelationshipType)
}

func TestRelationshipGrant_EnvVarRead(t *testing.T) {
	agent := &testRelAgentIdentity{
		id:        "agent-child-1",
		projectID: "project-1",
		ancestry:  []string{"root-user", "intermediate-agent"},
	}

	result := EvaluateProgenyGrant(
		agent, RelProgenyEnvVarRead, "envvar-xyz", "envvar",
		"root-user", true,
	)
	assert.True(t, result.Allowed, "progeny agent should access env vars created by ancestor")
	assert.Equal(t, RelProgenyEnvVarRead, result.RelationshipType)
}

func TestRelationshipGrant_SkillInjectionRead(t *testing.T) {
	agent := &testRelAgentIdentity{
		id:        "agent-child-1",
		projectID: "project-1",
		ancestry:  []string{"root-user", "intermediate-agent"},
	}

	result := EvaluateProgenyGrant(
		agent, RelProgenySkillInjectionRead, "skill-xyz", "skill_injection",
		"root-user", true,
	)
	assert.True(t, result.Allowed, "progeny agent should access skill injections created by ancestor")
	assert.Equal(t, RelProgenySkillInjectionRead, result.RelationshipType)
}

func TestRelationshipGrant_TransitiveAncestry(t *testing.T) {
	// Agent with deep ancestry chain: root-user → parent-agent → grandchild-agent
	// The secret is created by root-user. The grandchild should have access
	// because root-user is in its ancestry chain.
	agent := &testRelAgentIdentity{
		id:        "grandchild-agent",
		projectID: "project-1",
		ancestry:  []string{"root-user", "parent-agent", "uncle-agent"},
	}

	result := EvaluateProgenyGrant(
		agent, RelProgenySecretRead, "secret-1", "secret",
		"root-user", true,
	)
	assert.True(t, result.Allowed,
		"grandchild agent should access secrets via transitive ancestry")
}

func TestRelationshipGrant_IntermediateAncestorCreator(t *testing.T) {
	// Agent with ancestry [root-user, intermediate-agent].
	// The secret is created by intermediate-agent (not root-user).
	// This should be denied because DelegatedFrom only supports user creators,
	// and intermediate agents in the ancestry are agent IDs, not user IDs.
	//
	// However, EvaluateProgenyGrant just checks if the creatorID is in the ancestry.
	// If the system passes an agent ID as the creator, it will match the ancestry.
	agent := &testRelAgentIdentity{
		id:        "child-agent",
		projectID: "project-1",
		ancestry:  []string{"root-user", "intermediate-agent"},
	}

	// Creator is the intermediate agent (which IS in ancestry) — allowed
	result := EvaluateProgenyGrant(
		agent, RelProgenySecretRead, "secret-1", "secret",
		"intermediate-agent", true,
	)
	assert.True(t, result.Allowed,
		"agent should access secrets created by any ID in its ancestry chain")

	// Creator is not in ancestry — denied
	result = EvaluateProgenyGrant(
		agent, RelProgenySecretRead, "secret-1", "secret",
		"unrelated-user", true,
	)
	assert.False(t, result.Allowed)
}

// =============================================================================
// Edge cases and security boundaries
// =============================================================================

func TestRelationshipGrant_OnlyReadAction(t *testing.T) {
	agent := &testRelAgentIdentity{
		id:        "agent-1",
		projectID: "project-1",
		ancestry:  []string{"creator-user"},
	}

	resolver := NewRelationshipGrantResolver(nil)

	// Read action: eligible (would be allowed if store was available)
	result := resolver.CheckProgenyAccess(context.TODO(), agent,
		Resource{Type: "secret", ID: "s1"}, ActionRead)
	// Will fail because store is nil, but won't fail on the action check
	assert.Contains(t, result.DenyReason, "store not available")

	// Write action: not eligible, regardless
	result = resolver.CheckProgenyAccess(context.TODO(), agent,
		Resource{Type: "secret", ID: "s1"}, ActionUpdate)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.DenyReason, "only read action")

	// Delete action: not eligible
	result = resolver.CheckProgenyAccess(context.TODO(), agent,
		Resource{Type: "secret", ID: "s1"}, ActionDelete)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.DenyReason, "only read action")
}

func TestRelationshipGrant_UnsupportedResourceType(t *testing.T) {
	agent := &testRelAgentIdentity{
		id:        "agent-1",
		projectID: "project-1",
		ancestry:  []string{"creator-user"},
	}

	resolver := NewRelationshipGrantResolver(nil)

	result := resolver.CheckProgenyAccess(context.TODO(), agent,
		Resource{Type: "project", ID: "p1"}, ActionRead)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.DenyReason, "not eligible for progeny grants")
}

func TestRelationshipGrant_NilAgent(t *testing.T) {
	// EvaluateProgenyGrant with nil agent should fail (AncestryIsHubAttested returns false for nil)
	result := EvaluateProgenyGrant(
		nil, RelProgenySecretRead, "s1", "secret", "user-1", true,
	)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.DenyReason, "hub-attested")
}

func TestRelationshipGrant_EmptyCreator(t *testing.T) {
	agent := &testRelAgentIdentity{
		id:        "agent-1",
		projectID: "project-1",
		ancestry:  []string{"root-user"},
	}

	result := EvaluateProgenyGrant(
		agent, RelProgenySecretRead, "s1", "secret", "", true,
	)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.DenyReason, "no creator")
}

func TestRelationshipGrant_AllowProgenyFalse(t *testing.T) {
	agent := &testRelAgentIdentity{
		id:        "agent-1",
		projectID: "project-1",
		ancestry:  []string{"creator-user"},
	}

	result := EvaluateProgenyGrant(
		agent, RelProgenySecretRead, "s1", "secret", "creator-user", false,
	)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.DenyReason, "AllowProgeny")
}

// =============================================================================
// Relationship type mapping
// =============================================================================

func TestRelationshipTypeForResource(t *testing.T) {
	tests := []struct {
		resourceType string
		expected     RelationshipType
	}{
		{"secret", RelProgenySecretRead},
		{"envvar", RelProgenyEnvVarRead},
		{"skill_injection", RelProgenySkillInjectionRead},
		{"project", ""},
		{"agent", ""},
		{"group", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.resourceType, func(t *testing.T) {
			assert.Equal(t, tt.expected, relationshipTypeForResource(tt.resourceType))
		})
	}
}

func TestRelationshipRoleName(t *testing.T) {
	assert.Equal(t, "builtin:relationship:progeny_secret_read",
		relationshipRoleName(RelProgenySecretRead))
	assert.Equal(t, "builtin:relationship:progeny_envvar_read",
		relationshipRoleName(RelProgenyEnvVarRead))
	assert.Equal(t, "builtin:relationship:progeny_skill_injection_read",
		relationshipRoleName(RelProgenySkillInjectionRead))
}

// =============================================================================
// Lineage resolution
// =============================================================================

func TestIsInAncestry(t *testing.T) {
	tests := []struct {
		name        string
		ancestry    []string
		principalID string
		expected    bool
	}{
		{"found at start", []string{"user-1", "agent-1"}, "user-1", true},
		{"found at end", []string{"user-1", "agent-1"}, "agent-1", true},
		{"found in middle", []string{"user-1", "agent-1", "agent-2"}, "agent-1", true},
		{"not found", []string{"user-1", "agent-1"}, "user-2", false},
		{"empty ancestry", []string{}, "user-1", false},
		{"empty principal", []string{"user-1"}, "", false},
		{"single match", []string{"user-1"}, "user-1", true},
		{"single no match", []string{"user-1"}, "user-2", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isInAncestry(tt.ancestry, tt.principalID))
		})
	}
}

// =============================================================================
// Resolver operates independently of Policy rows
// =============================================================================
// The pure evaluation function (EvaluateProgenyGrant) produces correct access
// decisions using only the resource metadata (AllowProgeny, CreatedBy) and the
// agent's ancestry chain — no Policy or PolicyBinding lookups are involved.
// Handler-level Policy creation is retained until CO1 cutover wires the resolver
// into the evaluator; this test verifies the resolver's independent operation.

func TestRelationshipGrant_ResolverIndependentOfPolicies(t *testing.T) {
	// The pure evaluation function operates without any store or policy lookup.
	// It demonstrates that the resolver can produce a correct decision using
	// only resource metadata and agent ancestry, which is the target model
	// for CO1 cutover.
	agent := &testRelAgentIdentity{
		id:        "agent-test",
		projectID: "project-1",
		ancestry:  []string{"user-creator"},
	}

	result := EvaluateProgenyGrant(
		agent, RelProgenySecretRead, "secret-1", "secret",
		"user-creator", true,
	)
	require.True(t, result.Allowed)
	// Provenance uses relationship binding ID, not a policy ID
	assert.Contains(t, result.Provenance.BindingID, "relationship:")
	assert.NotContains(t, result.Provenance.BindingID, "policy")
}

// =============================================================================
// CheckProgenyAccess with nil store (fail-closed)
// =============================================================================

func TestRelationshipGrant_CheckProgenyAccess_NilStore(t *testing.T) {
	agent := &testRelAgentIdentity{
		id:        "agent-1",
		projectID: "project-1",
		ancestry:  []string{"user-1"},
	}

	resolver := NewRelationshipGrantResolver(nil)
	result := resolver.CheckProgenyAccess(context.TODO(), agent,
		Resource{Type: "secret", ID: "s1"}, ActionRead)

	assert.False(t, result.Allowed, "must fail closed when store is nil")
	assert.Contains(t, result.DenyReason, "store not available")
}

func TestRelationshipGrant_CheckProgenyAccess_FederatedDenied(t *testing.T) {
	fedAgent := NewFederatedAgentIdentity(
		"https://evil.com", "agent-x", "", "Evil",
		"local-user", []string{"local-user"}, nil,
	)

	resolver := NewRelationshipGrantResolver(nil)
	result := resolver.CheckProgenyAccess(context.TODO(), fedAgent,
		Resource{Type: "secret", ID: "s1"}, ActionRead)

	assert.False(t, result.Allowed, "federated agent must be denied")
	assert.Contains(t, result.DenyReason, "hub-attested")
}
