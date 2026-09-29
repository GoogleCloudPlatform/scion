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

// legacyUATScopeToPermissionID is a FROZEN literal snapshot, taken
// 2026-09-29, of the UATScope -> canonical permission ID mapping for every
// scope a CeilingVersionUnspecified row can hold.
//
// This table must NEVER be regenerated from, or fall back to, the live
// Registry: a later Registry addition or UATScope retarget must not
// silently change what an already-minted legacy token means. A change here
// requires a new CeilingVersion, not an edit to this map — see
// TestLegacyUATScopeToPermissionID_Golden, which fails on any modification.
//
// Every entry a legacy row's stored Scopes can actually contain is a key
// here: minting has always validated each stored scope against a
// Registry-derived UATValidScopes set (itself built from Permission.UATScope
// and UATManageAliases), and a manage alias is expanded to its concrete
// scopes before being persisted, so it is never stored raw. Every
// Permission.UATScope value equals Resource+":"+Action (see
// TestPermissionRegistry_UATScopeMatchesResourceAction), which is what makes
// this table a faithful snapshot of that mapping.
var legacyUATScopeToPermissionID = map[string]string{
	"agent:attach":               "agent.attach",
	"agent:create":               "agent.create",
	"agent:delete":               "agent.delete",
	"agent:lifecycle":            "agent.lifecycle",
	"agent:list":                 "agent.list",
	"agent:message":              "agent.message",
	"agent:port_access":          "agent.port_access",
	"agent:read":                 "agent.read",
	"broker:list":                "broker.list",
	"broker:read":                "broker.read",
	"gcp_service_account:assign": "gcp_service_account.assign",
	"gcp_service_account:list":   "gcp_service_account.list",
	"gcp_service_account:read":   "gcp_service_account.read",
	"gcp_service_account:verify": "gcp_service_account.verify",
	"group:addMember":            "group.addMember",
	"group:create":               "group.create",
	"group:delete":               "group.delete",
	"group:list":                 "group.list",
	"group:read":                 "group.read",
	"group:removeMember":         "group.removeMember",
	"group:update":               "group.update",
	"harness_config:create":      "harness_config.create",
	"harness_config:delete":      "harness_config.delete",
	"harness_config:list":        "harness_config.list",
	"harness_config:read":        "harness_config.read",
	"harness_config:update":      "harness_config.update",
	"project:clone":              "project.clone",
	"project:manage":             "project.manage",
	"project:read":               "project.read",
	"project:update":             "project.update",
	"skill:create":               "skill.create",
	"skill:delete":               "skill.delete",
	"skill:list":                 "skill.list",
	"skill:read":                 "skill.read",
	"skill:register":             "skill.register",
	"skill:update":               "skill.update",
	"template:create":            "template.create",
	"template:delete":            "template.delete",
	"template:list":              "template.list",
	"template:read":              "template.read",
	"template:update":            "template.update",
	"user:invite":                "user.invite",
	"user:list":                  "user.list",
	"user:read":                  "user.read",
}

// NormalizeLegacyUATScopes maps a CeilingVersionUnspecified row's raw stored
// Scopes to canonical permission IDs using the frozen snapshot above, never
// the live, mutable ResolveSelector: a scope grants exactly the one
// permission it names, and no scope implies another.
//
// A scope with no entry (never valid, or valid only after this snapshot was
// taken) is dropped rather than denying the whole ceiling: dropping an
// unrecognized scope can only shrink the resulting permission set. Duplicate
// permission IDs (multiple scopes resolving to the same ID — not possible
// today, but not relied upon) are deduplicated. The result is never nil,
// even for empty input, so callers can distinguish "resolved to nothing"
// from "not yet resolved" without a special case.
func NormalizeLegacyUATScopes(scopes []string) []string {
	ids := make([]string, 0, len(scopes))
	seen := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		id, ok := legacyUATScopeToPermissionID[scope]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}
