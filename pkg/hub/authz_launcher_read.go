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
	"slices"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Launcher status read (ptone/scion#3409, ptone/scion#4568).
//
// An agent may read the status of an agent it launched, directly or
// through its descendants, in the same project, through the single-agent
// GET routes. Nothing else is granted: the rule admits only agent.read
// with ActionRead, and only for a Resource built by
// agentStatusReadResource. The same predicate is the agent-scope
// exception (authz.go) and the delegation-ceiling exception
// (checkDelegationCeiling), so the three cannot drift apart. Logs,
// streams, events and every other agent.read route build agentResource
// without the stored target and are not covered.

// RelationshipRuleLauncher names the launcher status-read relationship.
const RelationshipRuleLauncher RelationshipRuleID = "launcher"

// permissionAgentRead is the only permission the launcher rule admits.
const permissionAgentRead = "agent.read"

// agentStatusReadResource is agentResource for the single-agent GET
// routes. It carries the agent record the handler read from the store so
// the launcher rule decides from that record.
func agentStatusReadResource(a *store.Agent) Resource {
	r := agentResource(a)
	r.launchedTarget = a
	return r
}

// descendantStatusReadHolds reports whether caller launched the target agent
// of resource, directly or through its descendants, in caller's project,
// for a single-agent read.
//
// Every fact about the target comes from resource.launchedTarget, the
// handler's store record: the caller is an entry of its stored Ancestry
// (written at create from the creator's authenticated identity and its
// own stored Ancestry, and never rewritten), and its stored ProjectID must
// equal the caller's project.
// The caller contributes only its authenticated ID and project; any
// ancestry the caller's credential carries is not consulted.
func descendantStatusReadHolds(caller AgentIdentity, resource Resource, action Action, permissionID string) bool {
	if caller == nil || action != ActionRead || permissionID != permissionAgentRead {
		return false
	}
	if _, ok := caller.(*hubDeliveryIdentity); ok {
		return false
	}
	target := resource.launchedTarget
	if target == nil || resource.Type != "agent" || target.ID == "" || target.ID != resource.ID {
		return false
	}
	callerID := caller.ID()
	if callerID == "" || callerID == target.ID {
		return false
	}
	if !slices.Contains(target.Ancestry, callerID) {
		return false
	}
	projectID := caller.ProjectID()
	return projectID != "" && target.ProjectID == projectID
}

// launcherCandidate returns the launcher relationship candidate when
// descendantStatusReadHolds, or false.
func launcherCandidate(principal PrincipalContext, resource Resource, action Action, permissionID string) (relationshipCandidate, bool) {
	if principal.Kind != PrincipalKindAgent {
		return relationshipCandidate{}, false
	}
	caller, ok := principal.Identity.(AgentIdentity)
	if !ok || caller.ID() != principal.ID || !descendantStatusReadHolds(caller, resource, action, permissionID) {
		return relationshipCandidate{}, false
	}
	return relationshipCandidate{
		rule: RelationshipRuleLauncher,
		decision: Decision{
			Allowed:      true,
			Reason:       "relationship grant: launcher status read",
			Scope:        ScopeTypeRelationship,
			MatchedGrant: "launcher",
		},
	}, true
}
