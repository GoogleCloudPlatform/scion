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

package permissions

// RelationshipPolicy is the one authoring surface for "what can an
// owner/ancestor/progeny/etc. relationship do." B.1's runtime
// relationship-grant evaluator and A.1's MintEligibilityDescriptor both
// reference the same rows here, so there is never a second, drifting
// action allowlist for the same rule. At most two rows per (Relationship,
// PrincipalKind, ResourceType), and only with disjoint PermissionIDs and
// differing MintEligible (enforced by TestRelationshipPolicy_Consistency in
// pkg/hub); ResourceType is always explicit, never a wildcard.
type RelationshipPolicy struct {
	// Relationship is the canonical rule name: "owner", "ancestor",
	// "progeny", "hub_member_sa_assign", "creator_user_skill" — matching
	// hub.RelationshipType / B.1's RelationshipRuleID strings.
	Relationship string
	// PrincipalKinds lists which principal kinds this row applies to, e.g.
	// "user", "agent" (an ancestor rule for an agent principal differs from
	// one for a user principal).
	PrincipalKinds []string
	// ResourceType is the canonical resource type, e.g. ResourceAgent.
	ResourceType string
	// PermissionIDs is the explicit, canonical action allowlist for this
	// row — permission IDs from Registry only, never a resource:action
	// reconstruction.
	PermissionIDs []string
	// ReadOnly, when true, asserts every ID in PermissionIDs is a
	// read/list/verify action (checked by a consistency test).
	ReadOnly bool
	// MintEligible reports whether this row may justify resource-relative
	// UAT mint eligibility. A MintEligibilityDescriptor relationship-type
	// reference must resolve to a MintEligible==true row.
	MintEligible bool
}

// RelationshipPolicies ships with the two rows A.1 needs for agent
// owner/ancestor attach and port access. B.1 extends this table (progeny,
// hub_member_sa_assign, creator_user_skill, additional PrincipalKinds such
// as agent-ancestor) from its characterization of today's
// owner/ancestor/progeny-reachable permissions.
var RelationshipPolicies = []RelationshipPolicy{
	{
		Relationship:   "owner",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceAgent,
		PermissionIDs:  []string{"agent.attach", "agent.port_access"},
		MintEligible:   true,
	},
	{
		Relationship:   "ancestor",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceAgent,
		PermissionIDs:  []string{"agent.attach", "agent.port_access"},
		MintEligible:   true,
	},
	// Rows below are appended by ptone/scion#2119 (B.1). Each row is pinned
	// by relationshipCharacterizedAllowlist in
	// pkg/hub/authz_relationship_characterization_test.go and checked by
	// TestRelationshipPolicy_MatchesCharacterization.
	{
		// owner/user/agent: the remaining agent permissions (not mint-eligible).
		// agent.manage is a reviewed unregistered ID (relationshipPolicyReviewedExceptions).
		Relationship:   "owner",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceAgent,
		PermissionIDs: []string{
			"agent.create", "agent.read", "agent.list", "agent.update", "agent.delete",
			"agent.lifecycle", "agent.stop_all", "agent.message", "agent.set_message_mode",
			"agent.grant_hub_mode", "agent.status_update", "agent.log_append", "agent.notify",
			"agent.token_refresh", "agent.port_forward", "agent.identity_token", "agent.manage",
		},
	},
	{
		// owner/user/project (TestRelationshipCharacterization_Owner).
		Relationship:   "owner",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceProject,
		PermissionIDs: []string{
			"project.create", "project.read", "project.update", "project.delete",
			"project.manage", "project.register", "project.set_messaging_policy",
			"project.clone", "project.list", "project.secret_read",
		},
	},
	{
		// owner/user/template (TestRelationshipCharacterization_Owner).
		Relationship:   "owner",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceTemplate,
		PermissionIDs:  []string{"template.create", "template.read", "template.update", "template.delete", "template.list"},
	},
	{
		// owner/user/harness_config (TestRelationshipCharacterization_Owner).
		Relationship:   "owner",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceHarnessConfig,
		PermissionIDs: []string{
			"harness_config.create", "harness_config.read", "harness_config.update",
			"harness_config.delete", "harness_config.list",
		},
	},
	{
		// owner/user/group (TestRelationshipCharacterization_Owner).
		Relationship:   "owner",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceGroup,
		PermissionIDs: []string{
			"group.create", "group.read", "group.update", "group.delete", "group.list",
			"group.addMember", "group.removeMember",
		},
	},
	{
		// owner/user/broker (TestRelationshipCharacterization_Owner).
		Relationship:   "owner",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceBroker,
		PermissionIDs: []string{
			"broker.create", "broker.read", "broker.update", "broker.delete", "broker.list", "broker.dispatch",
		},
	},
	{
		// owner/user/gcp_service_account (TestRelationshipCharacterization_Owner).
		// The owner rule does not produce an assign candidate for a hub-scoped
		// account; hub_member_sa_assign covers that shape.
		Relationship:   "owner",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceGCPServiceAccount,
		PermissionIDs: []string{
			"gcp_service_account.create", "gcp_service_account.read", "gcp_service_account.delete",
			"gcp_service_account.list", "gcp_service_account.verify", "gcp_service_account.mint",
			"gcp_service_account.assign",
		},
	},
	{
		// owner/user/skill (TestRelationshipCharacterization_Owner).
		Relationship:   "owner",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceSkill,
		PermissionIDs: []string{
			"skill.create", "skill.create_global", "skill.read", "skill.update", "skill.delete",
			"skill.list", "skill.register",
		},
	},
	{
		// ancestor/user/agent: the remaining agent permissions (not mint-eligible)
		// (TestRelationshipCharacterization_UserAncestor).
		Relationship:   "ancestor",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceAgent,
		PermissionIDs: []string{
			"agent.create", "agent.read", "agent.list", "agent.update", "agent.delete",
			"agent.lifecycle", "agent.stop_all", "agent.message", "agent.set_message_mode",
			"agent.grant_hub_mode", "agent.status_update", "agent.log_append", "agent.notify",
			"agent.token_refresh", "agent.port_forward", "agent.identity_token", "agent.manage",
		},
	},
	{
		// ancestor/agent/agent (TestRelationshipCharacterization_AgentAncestor).
		// Agent JWT scopes restrict this further at evaluation time.
		Relationship:   "ancestor",
		PrincipalKinds: []string{"agent"},
		ResourceType:   ResourceAgent,
		PermissionIDs: []string{
			"agent.create", "agent.delete", "agent.attach", "agent.lifecycle",
			"agent.set_message_mode", "agent.status_update", "agent.log_append",
			"agent.notify", "agent.token_refresh", "agent.port_forward", "agent.identity_token",
		},
	},
	{
		// progeny/agent/secret (TestRelationshipCharacterization_Progeny). The
		// Decide resource type is "secret"; the permission is registered on
		// project (reviewed cross-type cell).
		Relationship:   "progeny",
		PrincipalKinds: []string{"agent"},
		ResourceType:   "secret",
		PermissionIDs:  []string{"project.secret_read"},
		ReadOnly:       true,
	},
	{
		// hub_member_sa_assign/user/gcp_service_account
		// (TestRelationshipCharacterization_HubMemberSAAssign).
		Relationship:   "hub_member_sa_assign",
		PrincipalKinds: []string{"user"},
		ResourceType:   ResourceGCPServiceAccount,
		PermissionIDs:  []string{"gcp_service_account.assign"},
	},
	{
		// creator_user_skill/agent/skill (TestRelationshipCharacterization_CreatorUserSkill).
		Relationship:   "creator_user_skill",
		PrincipalKinds: []string{"agent"},
		ResourceType:   ResourceSkill,
		PermissionIDs:  []string{"skill.read"},
		ReadOnly:       true,
	},
}

// RelationshipPolicyAllows reports whether relationship (for principalKind,
// against resourceType) permits permissionID. Unknown relationship,
// principalKind, resourceType, or permissionID combination returns false —
// no wildcard matching.
func RelationshipPolicyAllows(relationship, principalKind, resourceType, permissionID string) bool {
	for _, p := range RelationshipPolicies {
		if p.Relationship != relationship || p.ResourceType != resourceType {
			continue
		}
		if !containsString(p.PrincipalKinds, principalKind) {
			continue
		}
		if containsString(p.PermissionIDs, permissionID) {
			return true
		}
	}
	return false
}

// RelationshipPolicyMintEligible reports whether a MintEligible row exists
// for relationship against principalKind/resourceType/permissionID. Checks
// the actual principal kind, same as RelationshipPolicyAllows — a mint
// eligibility reference must not silently match a row scoped to a different
// principal kind (e.g. an agent-only ancestor row must not make a permission
// mint-eligible for a user principal).
func RelationshipPolicyMintEligible(relationship, principalKind, resourceType, permissionID string) bool {
	for _, p := range RelationshipPolicies {
		if !p.MintEligible {
			continue
		}
		if p.Relationship != relationship || p.ResourceType != resourceType {
			continue
		}
		if !containsString(p.PrincipalKinds, principalKind) {
			continue
		}
		if containsString(p.PermissionIDs, permissionID) {
			return true
		}
	}
	return false
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// RelationshipPrincipalKind maps a runtime principal kind string to the
// canonical kind RelationshipPolicies rows are authored against: user, dev
// and federated_user all collapse to "user"; agent and federated_agent
// collapse to "agent". Any other value passes through unchanged. The
// relationship mint path (hubPermissionEligible/selectorMintEligible,
// authz_boundary.go) must normalize through this function before calling
// RelationshipPolicyAllows/RelationshipPolicyMintEligible, so a dev or
// federated principal is not silently unmatched against a row authored for
// its base kind. This mapping only changes which existing row a call
// matches — it does not widen which principal kinds may reach the
// relationship or mint paths at all; that gate is enforced earlier, by each
// caller's own principal check.
//
// This is the SAME mapping hub.NormalizePrincipalType implements for the
// flat mint path and Decide's constraint matching — duplicated, not shared,
// because this package cannot import hub. The two must never diverge;
// hub's TestNormalizePrincipalType_AgreesWithRelationshipPrincipalKind pins
// agreement across every PrincipalKind constant plus an unknown value.
func RelationshipPrincipalKind(kind string) string {
	switch kind {
	case "user", "dev", "federated_user":
		return "user"
	case "agent", "federated_agent":
		return "agent"
	default:
		return kind
	}
}
