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
	"fmt"
)

// ScopeTypeRelationship is the scope type used in provenance output for
// relationship-derived grants. Distinguished from ScopeTypeSystem and
// ScopeTypeProject because relationship grants are inherently user-scoped
// (they derive from a user's creation of a resource with AllowProgeny),
// not system-wide role bindings.
const ScopeTypeRelationship = "relationship"

// RelationshipType identifies the kind of progeny relationship grant.
type RelationshipType string

const (
	// RelProgenySecretRead grants read access to secrets created by an ancestor.
	RelProgenySecretRead RelationshipType = "progeny_secret_read"

	// RelProgenyEnvVarRead grants read access to env vars created by an ancestor.
	RelProgenyEnvVarRead RelationshipType = "progeny_envvar_read"

	// RelProgenySkillInjectionRead grants read access to skill injections created by an ancestor.
	RelProgenySkillInjectionRead RelationshipType = "progeny_skill_injection_read"

	// RelProgenySkillRead grants an agent read access to its origin user's
	// personal (user-scoped) skills, keyed on the skill's owning bucket
	// (Resource.ScopeUserID) rather than a per-record creator field
	// (ptone/scion#2128). Consolidates the former dedicated
	// creator-user-skill grant into the common progeny evaluator.
	RelProgenySkillRead RelationshipType = "progeny_skill_read"
)

// relationshipRoleName returns a synthetic role name for provenance output.
func relationshipRoleName(relType RelationshipType) string {
	return "builtin:relationship:" + string(relType)
}

// RelationshipGrantResult is the output of a relationship grant evaluation.
type RelationshipGrantResult struct {
	// Allowed is true when the relationship grant permits access.
	Allowed bool

	// RelationshipType identifies the grant type that was evaluated.
	RelationshipType RelationshipType

	// Provenance contains the decision trace, compatible with AK1 kernel output.
	Provenance GrantProvenance

	// DenyReason explains why the grant was denied, if Allowed is false.
	DenyReason string
}

// EvaluateProgenyGrant is a pure evaluation function that checks progeny access
// without database lookups. It is used for unit testing and for integration with
// the authorization kernel where the caller has already resolved the resource
// metadata.
//
// Parameters:
//   - agent: the requesting agent identity
//   - relType: the relationship type being checked
//   - resourceID: the resource ID being accessed
//   - resourceType: the resource type (secret, envvar, skill_injection)
//   - creatorID: the user ID that created the resource
//   - allowProgeny: whether the resource has AllowProgeny enabled
func EvaluateProgenyGrant(
	agent AgentIdentity,
	relType RelationshipType,
	resourceID string,
	resourceType string,
	creatorID string,
	allowProgeny bool,
) RelationshipGrantResult {
	// Gate 1: Hub-attested ancestry.
	if !AncestryIsHubAttested(agent) {
		return denyRelationship(relType,
			"ancestry is not hub-attested (federated agent)")
	}

	ancestry := agent.Ancestry()
	if len(ancestry) == 0 {
		return denyRelationship(relType, "agent has no ancestry chain")
	}

	// Gate 2: AllowProgeny.
	if !allowProgeny {
		return denyRelationship(relType,
			"resource does not have AllowProgeny enabled")
	}

	// Gate 3: Creator.
	if creatorID == "" {
		return denyRelationship(relType, "resource has no creator")
	}

	// Gate 4: Ancestry match.
	if !isInAncestry(ancestry, creatorID) {
		return denyRelationship(relType,
			"resource creator is not in agent's ancestry chain")
	}

	return allowRelationship(relType, resourceType, resourceID, agent.ID())
}

// allowRelationship builds an allowed RelationshipGrantResult with full provenance.
func allowRelationship(relType RelationshipType, resourceType, resourceID, agentID string) RelationshipGrantResult {
	return RelationshipGrantResult{
		Allowed:          true,
		RelationshipType: relType,
		Provenance: GrantProvenance{
			BindingID:      fmt.Sprintf("relationship:%s:%s", relType, resourceID),
			RoleID:         string(relType),
			RoleName:       relationshipRoleName(relType),
			ScopeType:      ScopeTypeRelationship,
			PrincipalID:    agentID,
			PrincipalType:  "agent",
			MembershipPath: []string{agentID},
			Contributed:    true,
			Permissions:    []string{resourceType + ".read"},
		},
	}
}

// isInAncestry checks whether a given principal ID appears anywhere in the
// ancestry chain. The ancestry chain is ordered [root_user, ..., parent_agent].
func isInAncestry(ancestry []string, principalID string) bool {
	for _, id := range ancestry {
		if id == principalID {
			return true
		}
	}
	return false
}

// relationshipTypeForResource maps a resource type string to the corresponding
// RelationshipType. Both the registry type "env_var" and the original
// "envvar" map to the env var relationship. Returns an empty string for
// unsupported types.
func relationshipTypeForResource(resourceType string) RelationshipType {
	switch resourceType {
	case "secret":
		return RelProgenySecretRead
	case "envvar", "env_var":
		return RelProgenyEnvVarRead
	case "skill_injection":
		return RelProgenySkillInjectionRead
	case "skill":
		return RelProgenySkillRead
	default:
		return ""
	}
}

// denyRelationship builds a denied RelationshipGrantResult (package-level helper).
func denyRelationship(relType RelationshipType, reason string) RelationshipGrantResult {
	return RelationshipGrantResult{
		Allowed:          false,
		RelationshipType: relType,
		DenyReason:       reason,
		Provenance: GrantProvenance{
			RoleID:        string(relType),
			RoleName:      relationshipRoleName(relType),
			Contributed:   false,
			RejectReasons: []string{reason},
		},
	}
}
