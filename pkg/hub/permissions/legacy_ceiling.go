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

// legacyUATScopeToPermissionID is a FROZEN snapshot, taken 2026-09-29, of the
// UATScope -> canonical permission ID mapping that CeilingVersionUnspecified
// (pre-normalization) rows were minted and enforced under.
//
// This table must NEVER be regenerated from the live Registry. The whole
// point of normalizing legacy rows into an explicit, persisted
// FrozenPermissionCeiling is that a later Registry addition or UATScope
// retarget must not silently change what an already-minted legacy token
// means — that is exactly what calling the live, Registry-derived
// ResolveSelector at load time would do (see NormalizeLegacyUATScopes).
//
// Every entry a legacy row's stored Scopes can actually contain is a key
// here: minting has always validated each stored scope against a
// Registry-derived UATValidScopes set (itself built from Permission.UATScope
// and UATManageAliases), and UserAccessTokenService.CreateToken has always
// called expandScopes before persisting, so a manage alias is never stored
// raw — only its expanded concrete scopes are. Every Permission.UATScope
// value has always equalled Resource+":"+Action (verified by
// TestLegacyUATScopeToPermissionID_MatchesUATScopeField), which is what
// makes this table equivalent, for every scope a legacy row can hold, to
// the pre-A.2 scopeToPermissionIDs Registry scan it replaces.
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
// Scopes to canonical permission IDs using the frozen snapshot above —
// never the live, mutable ResolveSelector — so a later Registry change
// cannot retroactively reinterpret an already-minted legacy token (A.2
// ruling, a2-legacy-attach-ruling.md). Deliberately no
// LegacyUATScopeImplications expansion: that map was never honored on the
// Decide path, and this normalization freezes Decide's actual observed
// behavior, not CanDelegate's historically inconsistent one.
//
// A scope with no entry (never valid, or valid only after this snapshot was
// taken) is dropped rather than denying the whole ceiling: dropping an
// unrecognized scope can only shrink the resulting permission set, so it
// stays inside the narrowing-only mandate. Duplicate permission IDs
// (multiple scopes resolving to the same ID — not possible today, but not
// relied upon) are deduplicated.
func NormalizeLegacyUATScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(scopes))
	var ids []string
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
