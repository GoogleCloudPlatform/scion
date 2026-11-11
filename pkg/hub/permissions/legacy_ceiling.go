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
// 2026-09-30, of the resource:action -> canonical permission ID mapping for
// every scope a CeilingVersionUnspecified row can hold. It covers the whole
// Registry, not only permissions with a UATScope: authorization has always
// matched a stored scope string against Resource+":"+Action across every
// permission, independent of whether that permission is a currently
// mintable selector, so freezing only the UATScope-bearing subset would
// deny scopes an existing credential — or a caller that constructs an
// identity directly from a raw scope string — has always been evaluated
// against.
//
// This table must NEVER be regenerated from, or fall back to, the live
// Registry: a later Registry addition must not silently change what an
// already-issued scope means. A change here requires a new CeilingVersion,
// not an edit to this map — see TestLegacyUATScopeToPermissionID_Golden,
// which fails on any modification.
//
// Three pairs (hub:execute, hub:read, hub:update) are shared by several
// Registry permissions; each entry here keeps the first matching Registry
// permission. None of these three pairs has ever been a mintable scope, and
// a project-bound credential is denied every hub-level resource before this
// ceiling is consulted, so these entries do not affect request
// authorization.
var legacyUATScopeToPermissionID = map[string]string{
	"access_constraint:manage":     "access_constraint.admin",
	"access_constraint:read":       "access_constraint.read",
	"agent:attach":                 "agent.attach",
	"agent:create":                 "agent.create",
	"agent:delete":                 "agent.delete",
	"agent:grant_hub_mode":         "agent.grant_hub_mode",
	"agent:identity_token":         "agent.identity_token",
	"agent:lifecycle":              "agent.lifecycle",
	"agent:list":                   "agent.list",
	"agent:log_append":             "agent.log_append",
	"agent:message":                "agent.message",
	"agent:notify":                 "agent.notify",
	"agent:port_access":            "agent.port_access",
	"agent:port_forward":           "agent.port_forward",
	"agent:read":                   "agent.read",
	"agent:set_message_mode":       "agent.set_message_mode",
	"agent:status_update":          "agent.status_update",
	"agent:stop_all":               "agent.stop_all",
	"agent:token_refresh":          "agent.token_refresh",
	"agent:update":                 "agent.update",
	"broker:create":                "broker.create",
	"broker:delete":                "broker.delete",
	"broker:dispatch":              "broker.dispatch",
	"broker:list":                  "broker.list",
	"broker:read":                  "broker.read",
	"broker:update":                "broker.update",
	"gcp_service_account:assign":   "gcp_service_account.assign",
	"gcp_service_account:create":   "gcp_service_account.create",
	"gcp_service_account:delete":   "gcp_service_account.delete",
	"gcp_service_account:list":     "gcp_service_account.list",
	"gcp_service_account:mint":     "gcp_service_account.mint",
	"gcp_service_account:read":     "gcp_service_account.read",
	"gcp_service_account:verify":   "gcp_service_account.verify",
	"group:addMember":              "group.addMember",
	"group:create":                 "group.create",
	"group:delete":                 "group.delete",
	"group:list":                   "group.list",
	"group:read":                   "group.read",
	"group:removeMember":           "group.removeMember",
	"group:update":                 "group.update",
	"harness_config:create":        "harness_config.create",
	"harness_config:delete":        "harness_config.delete",
	"harness_config:list":          "harness_config.list",
	"harness_config:read":          "harness_config.read",
	"harness_config:update":        "harness_config.update",
	"hub:execute":                  "hub.maintenance.execute",
	"hub:manage":                   "hub.audit.read",
	"hub:read":                     "hub.settings.read",
	"hub:update":                   "hub.settings.update",
	"policy:create":                "policy.create",
	"policy:delete":                "policy.delete",
	"policy:list":                  "policy.list",
	"policy:read":                  "policy.read",
	"policy:update":                "policy.update",
	"project:clone":                "project.clone",
	"project:create":               "project.create",
	"project:delete":               "project.delete",
	"project:list":                 "project.list",
	"project:manage":               "project.manage",
	"project:read":                 "project.read",
	"project:register":             "project.register",
	"project:secret_read":          "project.secret_read",
	"project:set_messaging_policy": "project.set_messaging_policy",
	"project:update":               "project.update",
	"quota:create":                 "quota.create",
	"quota:delete":                 "quota.delete",
	"quota:read":                   "quota.read",
	"quota:update":                 "quota.update",
	"role:create":                  "role.create",
	"role:delete":                  "role.delete",
	"role:read":                    "role.read",
	"role:update":                  "role.update",
	"role_binding:create":          "role_binding.create",
	"role_binding:delete":          "role_binding.delete",
	"role_binding:read":            "role_binding.read",
	"scheduled_event:create":       "scheduled_event.create",
	"scheduled_event:delete":       "scheduled_event.delete",
	"scheduled_event:list":         "scheduled_event.list",
	"scheduled_event:read":         "scheduled_event.read",
	"scheduled_event:update":       "scheduled_event.update",
	"skill:create":                 "skill.create",
	"skill:create_global":          "skill.create_global",
	"skill:delete":                 "skill.delete",
	"skill:list":                   "skill.list",
	"skill:read":                   "skill.read",
	"skill:register":               "skill.register",
	"skill:update":                 "skill.update",
	"template:create":              "template.create",
	"template:delete":              "template.delete",
	"template:list":                "template.list",
	"template:read":                "template.read",
	"template:update":              "template.update",
	"user:delete":                  "user.delete",
	"user:invite":                  "user.invite",
	"user:list":                    "user.list",
	"user:promote":                 "user.promote",
	"user:read":                    "user.read",
	"user:suspend":                 "user.suspend",
	"user:update":                  "user.update",
}

// NormalizeLegacyUATScopes maps a CeilingVersionUnspecified row's raw stored
// Scopes to canonical permission IDs using the frozen snapshot above, never
// the live, mutable Registry: a scope grants exactly the one permission it
// names, and no scope implies another.
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
