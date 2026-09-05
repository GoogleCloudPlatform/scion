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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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

// RelationshipGrantResolver resolves progeny relationship grants. It evaluates
// whether an agent can access a resource based on creator-progeny ancestry,
// without creating Policy or PolicyBinding rows.
//
// The resolver enforces:
//   - Hub-attested ancestry only (federated ancestry is rejected)
//   - AllowProgeny opt-in on the resource
//   - Creator ancestry match (the resource creator must be in the agent's ancestry)
//   - User-scope resources only (project/system scope resources are not eligible)
//
// The resolver emits GrantProvenance compatible with AK1's decision kernel so
// that explain output can show the relationship grant alongside RoleBinding grants.
type RelationshipGrantResolver struct {
	store store.Store
}

// NewRelationshipGrantResolver creates a new resolver. The store is used for
// resource lookups via ListProgeny* methods. It may be nil for unit testing
// with the pure evaluation function EvaluateProgenyGrant.
func NewRelationshipGrantResolver(s store.Store) *RelationshipGrantResolver {
	return &RelationshipGrantResolver{
		store: s,
	}
}

// CheckProgenyAccess is the main entry point for relationship grant evaluation.
// It checks whether the given agent identity has progeny access to the specified
// resource via a named relationship grant.
//
// This replaces the legacy DelegatedFrom policy pattern. Instead of creating a
// Policy row per resource, the resolver derives the grant from:
//   - The resource's AllowProgeny flag and CreatedBy field (queried via store)
//   - The agent's hub-attested ancestry chain
//
// The store lookup uses the existing ListProgeny* methods which filter for
// AllowProgeny=true and CreatedBy IN ancestry. If the target resource ID
// appears in the result set, the grant is allowed.
func (r *RelationshipGrantResolver) CheckProgenyAccess(
	ctx context.Context,
	agent AgentIdentity,
	resource Resource,
	action Action,
) RelationshipGrantResult {
	// Only read actions are eligible for progeny grants.
	if action != ActionRead {
		return denyRelationship(relationshipTypeForResource(resource.Type),
			"only read action is eligible for progeny grants")
	}

	relType := relationshipTypeForResource(resource.Type)
	if relType == "" {
		return denyRelationship("",
			"resource type not eligible for progeny grants: "+resource.Type)
	}

	// Gate 1: Hub-attested ancestry only. Federated agents cannot use
	// progeny grants because their ancestry is a remote claim.
	if !AncestryIsHubAttested(agent) {
		return denyRelationship(relType,
			"ancestry is not hub-attested (federated agent)")
	}

	ancestry := agent.Ancestry()
	if len(ancestry) == 0 {
		return denyRelationship(relType, "agent has no ancestry chain")
	}

	if r.store == nil {
		return denyRelationship(relType, "store not available (fail-closed)")
	}

	// Gate 2+3: Look up progeny-eligible resources for this agent's ancestry
	// and check if the target resource is among them.
	found, err := r.isProgenyResource(ctx, relType, resource.ID, ancestry)
	if err != nil {
		// Fail closed on resolution errors.
		return denyRelationship(relType,
			fmt.Sprintf("resource resolution error (fail-closed): %v", err))
	}
	if !found {
		return denyRelationship(relType,
			"resource is not a progeny-eligible resource for this agent's ancestry")
	}

	// All gates passed — grant access.
	return allowRelationship(relType, resource.Type, resource.ID, agent.ID())
}

// isProgenyResource checks whether the given resource ID appears in the set
// of progeny-eligible resources for the given ancestry chain.
//
// Performance note: This does a list-then-scan using existing ListProgeny*
// store methods. For a single authorization check this is acceptable; if CO1
// calls CheckProgenyAccess per-resource in a list endpoint, a point-lookup
// store method (e.g. HasProgenySecret(ctx, resourceID, ancestorIDs)) would
// avoid loading all progeny resources.
func (r *RelationshipGrantResolver) isProgenyResource(
	ctx context.Context,
	relType RelationshipType,
	resourceID string,
	ancestry []string,
) (bool, error) {
	switch relType {
	case RelProgenySecretRead:
		secrets, err := r.store.ListProgenySecrets(ctx, ancestry)
		if err != nil {
			return false, fmt.Errorf("ListProgenySecrets: %w", err)
		}
		for _, s := range secrets {
			if s.ID == resourceID {
				return true, nil
			}
		}
		return false, nil

	case RelProgenyEnvVarRead:
		envVars, err := r.store.ListProgenyEnvVars(ctx, ancestry)
		if err != nil {
			return false, fmt.Errorf("ListProgenyEnvVars: %w", err)
		}
		for _, ev := range envVars {
			if ev.ID == resourceID {
				return true, nil
			}
		}
		return false, nil

	case RelProgenySkillInjectionRead:
		skills, err := r.store.ListProgenySkillInjections(ctx, ancestry)
		if err != nil {
			return false, fmt.Errorf("ListProgenySkillInjections: %w", err)
		}
		for _, si := range skills {
			if si.ID == resourceID {
				return true, nil
			}
		}
		return false, nil

	default:
		return false, fmt.Errorf("unsupported relationship type: %s", relType)
	}
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
// RelationshipType. Returns an empty string for unsupported types.
func relationshipTypeForResource(resourceType string) RelationshipType {
	switch resourceType {
	case "secret":
		return RelProgenySecretRead
	case "envvar":
		return RelProgenyEnvVarRead
	case "skill_injection":
		return RelProgenySkillInjectionRead
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
