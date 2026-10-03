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
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// RelationshipRejectExecutionProject names the relationship stage that
// requires the agent's authoritative source user to hold live admission to
// the agent's current project for the exact permission.
const RelationshipRejectExecutionProject = "execution_project"

// executionProjectRule reports whether rule derives an agent's access from
// its source user's resources and therefore requires execution-project
// admission. The progeny rule covers every such read, including a personal
// skill owned by the agent's origin user (ptone/scion#2128).
func executionProjectRule(rule RelationshipRuleID) bool {
	return rule == RelationshipRuleProgeny
}

// ExecutionSourceResolver identifies the single authoritative local source
// user of a stored agent. Implementations fail closed: an absent, ambiguous
// or unclassifiable source is an error, never a fallback.
type ExecutionSourceResolver interface {
	ResolveExecutionSource(ctx context.Context, agent *store.Agent) (*store.User, error)
}

// errNoAuthoritativeSource is returned when a stored agent has no single
// authoritative local source user.
var errNoAuthoritativeSource = errors.New("no single authoritative source user")

// edgeChainSourceResolver resolves the source user from the stored agent row
// and typed delegation edges in the agent's project: exactly one active edge
// at each link, agent links that exist and are not deleted, and a terminal
// user delegator that exists. The backfill sentinel, a missing or duplicate
// edge, a cycle, or an over-long chain is not a source.
type edgeChainSourceResolver struct {
	store store.Store
}

func (r edgeChainSourceResolver) ResolveExecutionSource(ctx context.Context, agent *store.Agent) (*store.User, error) {
	if agent == nil || agent.ProjectID == "" {
		return nil, errNoAuthoritativeSource
	}
	visited := map[string]bool{}
	delegateID := agent.ID
	for depth := 0; depth <= maxDelegationDepth; depth++ {
		if visited[delegateID] {
			return nil, fmt.Errorf("%w: delegation cycle", errNoAuthoritativeSource)
		}
		visited[delegateID] = true

		edges, err := r.store.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, delegateID)
		if err != nil {
			return nil, err
		}
		var active []*store.DelegationEdge
		for _, e := range filterEdgesByScope(edges, store.RoleScopeProject, agent.ProjectID) {
			if e.Active {
				active = append(active, e)
			}
		}
		if len(active) != 1 {
			return nil, fmt.Errorf("%w: %d active edges for agent %s", errNoAuthoritativeSource, len(active), delegateID)
		}
		edge := active[0]
		if isMigrationSentinel(edge) {
			return nil, fmt.Errorf("%w: migration-provenance edge", errNoAuthoritativeSource)
		}
		switch edge.DelegatorType {
		case store.DelegationPrincipalUser:
			user, err := r.store.GetUser(ctx, edge.DelegatorID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil, fmt.Errorf("%w: source user does not exist", errNoAuthoritativeSource)
				}
				return nil, err
			}
			if user == nil {
				return nil, fmt.Errorf("%w: source user does not exist", errNoAuthoritativeSource)
			}
			return user, nil
		case store.DelegationPrincipalAgent:
			parent, err := r.store.GetAgent(ctx, edge.DelegatorID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil, fmt.Errorf("%w: intermediate agent does not exist", errNoAuthoritativeSource)
				}
				return nil, err
			}
			if parent == nil || !parent.DeletedAt.IsZero() {
				return nil, fmt.Errorf("%w: intermediate agent is deleted", errNoAuthoritativeSource)
			}
			delegateID = parent.ID
		default:
			return nil, fmt.Errorf("%w: unsupported delegator type %q", errNoAuthoritativeSource, edge.DelegatorType)
		}
	}
	return nil, fmt.Errorf("%w: chain exceeds maximum depth", errNoAuthoritativeSource)
}

// executionSourceResolver returns the resolver used by the execution-project
// stage.
func (a *AuthzService) executionSourceResolver() ExecutionSourceResolver {
	if a.sourceResolver != nil {
		return a.sourceResolver
	}
	return edgeChainSourceResolver{store: a.store}
}

// executionProjectClass returns the project-scoped class used for
// execution-project admission of permissionID: the permission's registry
// resource type, with the project scope kind for resource types that carry
// scope-kind semantics.
func executionProjectClass(permissionID string) ProjectTargetClass {
	rt := registryResourceType(permissionID)
	class := ProjectTargetClass{ResourceType: rt}
	if kinds, ok := validRealProjectScopeKinds[rt]; ok && len(kinds) > 0 {
		class.ScopeKind = kinds[0]
	}
	return class
}

// executionProjectAdmission requires, for a local agent principal, that the
// agent's stored row is live and in the principal's project, that the agent
// has a single authoritative local source user who is active, and that this
// user holds live admission to the agent's current project for the exact
// permission (project membership evidence, or system authority that applies
// to the project-scoped class). Any lookup failure, mismatch or ambiguity
// denies.
func (a *AuthzService) executionProjectAdmission(ctx context.Context, principal PrincipalContext, permissionID string) (bool, string) {
	// Admission is evaluated for the agent's source user, not the
	// requester, so it must never read the requester's memoized principals
	// or access constraints.
	ctx = maskAuthzInputs(ctx)
	agentIdent, ok := principal.Identity.(AgentIdentity)
	if !ok || agentIdent.ID() == "" {
		return false, "principal is not a local agent"
	}
	if a.store == nil {
		return false, "store not available"
	}
	stored, err := a.store.GetAgent(ctx, agentIdent.ID())
	if err != nil || stored == nil {
		return false, "execution agent lookup failed"
	}
	if !stored.DeletedAt.IsZero() {
		return false, "execution agent is deleted"
	}
	if stored.ProjectID == "" || stored.ProjectID != agentIdent.ProjectID() {
		return false, "execution project does not match the agent's project"
	}

	source, err := a.executionSourceResolver().ResolveExecutionSource(ctx, stored)
	if err != nil || source == nil {
		return false, "execution agent has no authoritative source user"
	}
	if source.Status != store.UserStatusActive {
		return false, "execution source user is not active"
	}

	sourcePC := PrincipalContext{
		Kind:     PrincipalKindUser,
		ID:       source.ID,
		Identity: NewAuthenticatedUser(source.ID, source.Email, source.DisplayName, source.Role, ""),
	}
	res, err := a.ProjectAdmissionForClass(ctx, sourcePC, stored.ProjectID, permissionID, executionProjectClass(permissionID), nil)
	if err != nil {
		return false, "execution project admission check failed"
	}
	if !res.Admitted {
		return false, "execution source user lacks admission to the agent's project"
	}
	return true, ""
}
