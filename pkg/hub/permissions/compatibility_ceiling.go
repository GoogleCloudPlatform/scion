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

import "sort"

// CompatibilityPolicyVersion identifies a frozen compatibility policy: the
// rules for giving a delegation edge written before authority provenance was
// recorded an explicit bounded ceiling. A policy is a literal table and is
// never recomputed from the Registry, AgentScopes or role scopes, so a later
// Registry, alias or role-scope change cannot widen an edge adopted under
// it. A different table requires a new version (and a new adoption marker);
// it never re-adopts rows adopted under an earlier version.
type CompatibilityPolicyVersion int

// CompatibilityPolicyV1 is the first compatibility policy.
const CompatibilityPolicyV1 CompatibilityPolicyVersion = 1

// compatibilityCeilingV1 is the FROZEN per-role table of CompatibilityPolicyV1.
// Each row lists the registry permissions the role's agent scopes covered
// when the table was taken, plus, for full, gcp_service_account.use and the
// three hub delivery permissions. agent.identity_token is deliberately absent
// from every row. See TestCompatibilityCeilingV1IsPinned, which fails on any
// edit.
var compatibilityCeilingV1 = map[string][]string{
	"readonly": {
		"harness_config.list",
		"harness_config.read",
		"project.read",
		"skill.list",
		"skill.read",
		"template.list",
		"template.read",
	},
	"baseline": {
		"agent.notify",
		"agent.port_forward",
		"agent.status_update",
		"agent.token_refresh",
		"harness_config.list",
		"harness_config.read",
		"project.read",
		"skill.list",
		"skill.read",
		"template.list",
		"template.read",
	},
	"full": {
		"agent.attach",
		"agent.create",
		"agent.delete",
		"agent.lifecycle",
		"agent.notify",
		"agent.port_forward",
		"agent.set_message_mode",
		"agent.status_update",
		"agent.token_refresh",
		"env_var.deliver",
		"gcp_service_account.assign",
		"gcp_service_account.use",
		"harness_config.list",
		"harness_config.read",
		"project.read",
		"project.secret_read",
		"secret.deliver",
		"secret.use",
		"skill.list",
		"skill.read",
		"skill_injection.deliver",
		"template.create",
		"template.list",
		"template.read",
		"template.update",
	},
}

// compatibilityAssignedSAV1 is added under CompatibilityPolicyV1 for an
// agent whose applied configuration already carries an assign-mode service
// account, so the agent keeps the token scope for its own account.
var compatibilityAssignedSAV1 = []string{
	"gcp_service_account.assign",
	"gcp_service_account.use",
}

// CompatibilityCeiling returns the sorted, de-duplicated permission IDs of
// policy version v for role. hasAssignedSA adds the assigned-service-account
// permissions. ok is false for an unknown version or role (including
// "none"); callers must then leave the edge unadopted.
func CompatibilityCeiling(v CompatibilityPolicyVersion, role string, hasAssignedSA bool) (ids []string, ok bool) {
	if v != CompatibilityPolicyV1 {
		return nil, false
	}
	row, known := compatibilityCeilingV1[role]
	if !known {
		return nil, false
	}
	seen := make(map[string]bool, len(row)+len(compatibilityAssignedSAV1))
	out := make([]string, 0, len(row)+len(compatibilityAssignedSAV1))
	add := func(list []string) {
		for _, id := range list {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	add(row)
	if hasAssignedSA {
		add(compatibilityAssignedSAV1)
	}
	sort.Strings(out)
	return out, true
}

// CompatibilityRoles returns the roles CompatibilityPolicyV1 defines, lowest
// first.
func CompatibilityRoles() []string {
	return []string{"readonly", "baseline", "full"}
}
