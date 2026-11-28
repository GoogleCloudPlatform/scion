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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Creator direct-child label edit (ptone/scion#9867).
//
// A local agent may edit non-reserved labels on its direct children in the
// same project through the merge label routes (PATCH .../labels). Nothing else
// is granted: the rule admits only agent.update with ActionUpdate, and only
// for a Resource built by agentLabelsUpdateResource. The same predicate is the
// agent-scope exception (authz.go) and the delegation-ceiling exception
// (checkDelegationCeiling), so the three cannot drift apart. Full PATCH
// (/api/v1/agents/{id}) and every other agent.update route build agentResource
// without labelEditTarget and remain closed to agent callers.

// RelationshipRuleCreatorLabel names the direct-creator label edit relationship.
const RelationshipRuleCreatorLabel RelationshipRuleID = "creator_label"

// permissionAgentUpdate is the only permission the creator_label rule admits.
const permissionAgentUpdate = "agent.update"

// agentLabelsUpdateResource is agentResource for the merge label PATCH routes.
// It carries the agent record the handler read from the store so the
// creator_label rule decides from that record alone.
func agentLabelsUpdateResource(a *store.Agent) Resource {
	r := agentResource(a)
	r.labelEditTarget = a
	return r
}

// creatorLabelEditHolds reports whether caller is the direct creator of the
// target agent of resource, in caller's project, for a label merge update.
//
// Every fact about the target comes from resource.labelEditTarget, the
// handler's stored record: target.CreatedBy is written once at create from
// the creator's authenticated identity and is immutable in the schema.
// Self-edits, grandparents, siblings, parents, cross-project targets, and
// internal hub-delivery identities are all refused.
func creatorLabelEditHolds(caller AgentIdentity, resource Resource, action Action, permissionID string) bool {
	if caller == nil || action != ActionUpdate || permissionID != permissionAgentUpdate {
		return false
	}
	if _, ok := caller.(*hubDeliveryIdentity); ok {
		return false
	}
	target := resource.labelEditTarget
	if target == nil || resource.Type != "agent" || target.ID == "" || target.ID != resource.ID {
		return false
	}
	callerID := caller.ID()
	if callerID == "" || callerID == target.ID {
		return false
	}
	if target.CreatedBy == "" || target.CreatedBy != callerID {
		return false
	}
	projectID := caller.ProjectID()
	return projectID != "" && target.ProjectID == projectID
}

// creatorLabelCandidate returns the creator_label relationship candidate when
// principal is a local agent and creatorLabelEditHolds, or false.
// Federated agents and delegated agents have different PrincipalKind values
// and are refused here. The candidate's fact verifies that the calling creator
// agent still exists and is not deleted (fail closed on orphans/deletion race).
func (a *AuthzService) creatorLabelCandidate(principal PrincipalContext, resource Resource, action Action, permissionID string) (relationshipCandidate, bool) {
	if principal.Kind != PrincipalKindAgent {
		return relationshipCandidate{}, false
	}
	caller, ok := principal.Identity.(AgentIdentity)
	if !ok || caller.ID() != principal.ID || !creatorLabelEditHolds(caller, resource, action, permissionID) {
		return relationshipCandidate{}, false
	}
	callerID := caller.ID()
	return relationshipCandidate{
		rule: RelationshipRuleCreatorLabel,
		fact: func(ctx context.Context) (*SharingSource, bool, string) {
			if a == nil || a.store == nil {
				return nil, false, "creator agent store unavailable"
			}
			creator, err := a.store.GetAgent(ctx, callerID)
			if err != nil || creator == nil || !creator.DeletedAt.IsZero() {
				return nil, false, "creator agent is not live"
			}
			return nil, true, ""
		},
		decision: Decision{
			Allowed:      true,
			Reason:       "relationship grant: creator label edit",
			Scope:        ScopeTypeRelationship,
			MatchedGrant: "creator_label",
		},
	}, true
}
