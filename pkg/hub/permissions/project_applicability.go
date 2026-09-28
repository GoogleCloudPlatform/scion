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

// ProjectTargetApplicability is a hand-reviewed disposition per canonical
// permission ID: can exercising this permission ever apply to an EXISTING
// project target (as opposed to a hub-level collection action, or a
// global/hub-catalog action that happens to share a resource type with
// project-scoped resources — project.create and skill.create_global are
// both reviewed false here despite Resource == "project"/"skill")?
//
// A true entry means "can apply," not "every use of this ID has a project
// target" — the actual target/action check for one specific request remains
// Decide's job, unchanged by this table.
//
// This is a SEPARATE, independently hand-reviewed table from
// PermissionAllowedBoundaries (below) and is not derived from
// Permission.Resource/Action or from any heuristic classification of
// resource types: two permissions on the same resource type can have
// different dispositions (project.create vs project.read), so no
// resource-type-level shortcut is safe. Every permissions.Registry entry
// must have an explicit entry here — an unreviewed ID is a bug, not a
// default answer either way (see AppliesToExistingProjectTarget).
var ProjectTargetApplicability = map[string]bool{
	// agent.* — every action targets an agent inside an existing project.
	"agent.create": true, "agent.read": true, "agent.list": true, "agent.update": true,
	"agent.delete": true, "agent.attach": true, "agent.lifecycle": true,
	"agent.port_access": true, "agent.stop_all": true, "agent.message": true,
	"agent.set_message_mode": true, "agent.grant_hub_mode": true,
	"agent.status_update": true, "agent.log_append": true, "agent.notify": true,
	"agent.token_refresh": true, "agent.port_forward": true, "agent.identity_token": true,

	// project.* — read/update/delete/manage/set_messaging_policy target an
	// existing project; create/register/clone/list are hub-level collection
	// actions (create: no project exists yet; register/clone/list reviewed
	// conservatively false — flagged for D.2 confirmation, not blocking).
	"project.create": false, "project.read": true, "project.update": true,
	"project.delete": true, "project.manage": true, "project.register": false,
	"project.set_messaging_policy": true, "project.clone": false, "project.list": false,

	// skill.* — create/read/update/delete/list can target a project-scoped
	// skill; create_global and register are explicitly hub-catalog actions.
	"skill.create": true, "skill.create_global": false, "skill.read": true,
	"skill.update": true, "skill.delete": true, "skill.list": true, "skill.register": false,

	// template.*, harness_config.* — no *_global counterpart registered
	// today; project/user-scoped, reviewed true (ScopeKind still governs
	// ResolveTargetScope's actual per-instance classification).
	"template.create": true, "template.read": true, "template.update": true,
	"template.delete": true, "template.list": true,
	"harness_config.create": true, "harness_config.read": true, "harness_config.update": true,
	"harness_config.delete": true, "harness_config.list": true,

	// group.* — hub-wide resource, no project-scoped variant exists.
	"group.create": false, "group.read": false, "group.update": false, "group.delete": false,
	"group.list": false, "group.addMember": false, "group.removeMember": false,

	// user.* — hub-wide.
	"user.read": false, "user.update": false, "user.invite": false, "user.suspend": false,
	"user.promote": false, "user.delete": false, "user.list": false,

	// policy.* — hub-wide.
	"policy.create": false, "policy.read": false, "policy.update": false,
	"policy.delete": false, "policy.list": false,

	// broker.* — user-owned hub resource, not project-contained.
	"broker.create": false, "broker.read": false, "broker.update": false,
	"broker.delete": false, "broker.list": false, "broker.dispatch": false,

	// gcp_service_account.* — hub/user resource; assign targets an agent
	// inside an existing project, reviewed true as the one exception in
	// this family (same pattern as project.create/skill.create_global: the
	// resource family default does not decide every member).
	"gcp_service_account.create": false, "gcp_service_account.read": false,
	"gcp_service_account.delete": false, "gcp_service_account.list": false,
	"gcp_service_account.verify": false, "gcp_service_account.mint": false,
	"gcp_service_account.assign": true,

	// hub.* — hub-wide by definition, every entry false.
	"hub.settings.read": false, "hub.settings.update": false, "hub.config.read": false,
	"hub.config.update": false, "hub.maintenance.execute": false, "hub.diagnostics.read": false,
	"hub.health.read": false, "hub.admin_mode.read": false, "hub.admin_mode.update": false,
	"hub.integrations.read": false, "hub.integrations.update": false,
	"hub.lifecycle_hooks.read": false, "hub.lifecycle_hooks.update": false,
	"hub.allow_list.read": false, "hub.allow_list.update": false,
	"hub.project_defaults.read": false, "hub.project_defaults.update": false,
	"hub.messaging.update": false, "hub.auth_reset.execute": false,
	"hub.scheduler.read": false, "hub.scheduler.update": false,
	"hub.federation.read": false, "hub.federation.update": false,
	"hub.teams_manifest.read": false, "hub.teams_manifest.update": false,
	"hub.validate.execute": false, "hub.github_app.read": false, "hub.github_app.update": false,
	"hub.metrics.read": false, "hub.audit.read": false,

	// quota.* — entitlements can bind to a project's usage; reviewed true,
	// flagged lower-confidence for D.2 confirmation (definitions themselves
	// are global, but the table is per-permission-ID, not per-record).
	"quota.read": true, "quota.create": true, "quota.update": true, "quota.delete": true,

	// role.* — role DEFINITIONS are hub-wide, never project-scoped.
	"role.read": false, "role.create": false, "role.update": false, "role.delete": false,
	// role_binding.* — a binding's SCOPE can be a project; reviewed true
	// (do not blanket this resource type as hub-only merely because some
	// bindings are system-scoped).
	"role_binding.read": true, "role_binding.create": true, "role_binding.delete": true,

	// access_constraint.* — a constraint's scope can be a project; reviewed
	// true for the same reason as role_binding.
	"access_constraint.admin": true, "access_constraint.read": true,

	// scheduled_event.* — always scoped to a project.
	"scheduled_event.read": true, "scheduled_event.list": true, "scheduled_event.create": true,
	"scheduled_event.delete": true, "scheduled_event.update": true,

	"project.secret_read": true,
}

// AppliesToExistingProjectTarget reports the reviewed disposition for
// permissionID. reviewed is false when permissionID has no entry — callers
// (e.g. hub.ActiveProjectAccess) must treat that as "not established,"
// never "true because unreviewed."
func AppliesToExistingProjectTarget(permissionID string) (applies bool, reviewed bool) {
	v, ok := ProjectTargetApplicability[permissionID]
	return v, ok
}

// PermissionAllowedBoundaries is a SEPARATE hand-reviewed table: which
// boundary kinds may select this permission's UAT scope at MINT time. Only
// covers permissions with a non-empty Permission.UATScope today (that is
// the current universe of selectors); a drift test requires an entry for
// every such Registry row.
//
// "Hub-only" families (group/user/policy/broker/gcp_service_account except
// assign) get []BoundaryKind{BoundaryKindHub}; everything else reviewed
// gets both Project and Hub, since hub scope is a strict superset of
// project use ("hub scope includes cross-project use") and restricting an
// ordinary project-contained permission to Hub-only would be a new,
// unrequested UX restriction that A.1 is not authorized to introduce.
var PermissionAllowedBoundaries = map[string][]BoundaryKind{
	"agent.create": {BoundaryKindProject, BoundaryKindHub}, "agent.read": {BoundaryKindProject, BoundaryKindHub},
	"agent.list": {BoundaryKindProject, BoundaryKindHub}, "agent.delete": {BoundaryKindProject, BoundaryKindHub},
	"agent.attach": {BoundaryKindProject, BoundaryKindHub}, "agent.lifecycle": {BoundaryKindProject, BoundaryKindHub},
	"agent.port_access": {BoundaryKindProject, BoundaryKindHub}, "agent.message": {BoundaryKindProject, BoundaryKindHub},
	"project.read": {BoundaryKindProject, BoundaryKindHub}, "project.update": {BoundaryKindProject, BoundaryKindHub},
	"project.manage": {BoundaryKindProject, BoundaryKindHub}, "project.clone": {BoundaryKindProject, BoundaryKindHub},
	"skill.create": {BoundaryKindProject, BoundaryKindHub}, "skill.read": {BoundaryKindProject, BoundaryKindHub},
	"skill.update": {BoundaryKindProject, BoundaryKindHub}, "skill.delete": {BoundaryKindProject, BoundaryKindHub},
	"skill.list": {BoundaryKindProject, BoundaryKindHub}, "skill.register": {BoundaryKindHub},
	"template.create": {BoundaryKindProject, BoundaryKindHub}, "template.read": {BoundaryKindProject, BoundaryKindHub},
	"template.update": {BoundaryKindProject, BoundaryKindHub}, "template.delete": {BoundaryKindProject, BoundaryKindHub},
	"template.list":         {BoundaryKindProject, BoundaryKindHub},
	"harness_config.create": {BoundaryKindProject, BoundaryKindHub}, "harness_config.read": {BoundaryKindProject, BoundaryKindHub},
	"harness_config.update": {BoundaryKindProject, BoundaryKindHub}, "harness_config.delete": {BoundaryKindProject, BoundaryKindHub},
	"harness_config.list": {BoundaryKindProject, BoundaryKindHub},
	"group.create":        {BoundaryKindHub}, "group.read": {BoundaryKindHub}, "group.update": {BoundaryKindHub},
	"group.delete": {BoundaryKindHub}, "group.list": {BoundaryKindHub}, "group.addMember": {BoundaryKindHub},
	"group.removeMember": {BoundaryKindHub},
	"user.read":          {BoundaryKindHub}, "user.invite": {BoundaryKindHub}, "user.list": {BoundaryKindHub},
	"broker.read": {BoundaryKindHub}, "broker.list": {BoundaryKindHub},
	"gcp_service_account.read": {BoundaryKindHub}, "gcp_service_account.list": {BoundaryKindHub},
	"gcp_service_account.verify": {BoundaryKindHub}, "gcp_service_account.assign": {BoundaryKindProject, BoundaryKindHub},

	// broker.create has no Permission.UATScope yet (not a resolvable
	// selector today) but is pre-reviewed here as hub-only per pat-a-lead:
	// when D.1 adds UATScope: "broker:create" to that Registry row,
	// ResolveSelector starts succeeding immediately with the correct
	// boundary, no second A.1-side change required.
	"broker.create": {BoundaryKindHub},
}

// SelectorAllowedBoundaries returns the reviewed boundary kinds for a single
// permission ID's own selector. Unreviewed (not present) returns
// reviewed=false; callers must fail closed (the selector cannot be
// resolved), never assume a default.
func SelectorAllowedBoundaries(permissionID string) (kinds []BoundaryKind, reviewed bool) {
	k, ok := PermissionAllowedBoundaries[permissionID]
	return k, ok
}

// ValidBoundary is the single shared rule for whether a (kind, projectID)
// combination is a well-formed token boundary: a project boundary requires
// a non-empty project ID, a hub boundary requires an empty one, and any
// other kind is invalid. It lives here (not in pkg/hub) so pkg/store's
// boundary-column validation can call the exact same rule pkg/hub's
// TokenBoundary.Valid() calls — pkg/store cannot import pkg/hub, but both
// layers must agree, pinned by a shared table test.
func ValidBoundary(kind BoundaryKind, projectID string) bool {
	switch kind {
	case BoundaryKindProject:
		return projectID != ""
	case BoundaryKindHub:
		return projectID == ""
	default:
		return false
	}
}

// TargetClassKind classifies a permission's target for hub-boundary
// MINT-TIME contemplation only (no real target exists yet at mint time).
type TargetClassKind string

const (
	// TargetClassKindProjectScoped represents an ordinary project-contained
	// instance of the permission's resource type.
	TargetClassKindProjectScoped TargetClassKind = "project_scoped"
	// TargetClassKindGlobalCatalog represents the hub-wide (global/core)
	// catalog instance space, for the resource types that have one.
	TargetClassKindGlobalCatalog TargetClassKind = "global_catalog"
)

// SupportedTargetClasses is an explicit, reviewed, per-permission-ID list of
// target classes a permission can legitimately apply to. Used ONLY for
// hub-boundary mint-time contemplation (hub.MintTimeSystemGrant) — never
// inferred from ProjectTargetApplicability or PermissionAllowedBoundaries,
// because one permission can legitimately support BOTH a global-catalog and
// a project-scoped class (skill/template/harness_config read/list); a
// single ContemplatedProjectClass answer would wrongly strip a seeded
// hub-member's legitimate catalog-only grant when the operation actually
// targets the global catalog (pat-refactor F-4 catalog-read correction,
// 2026-09-28).
var SupportedTargetClasses = map[string][]TargetClassKind{
	"skill.read":          {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"skill.list":          {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"template.read":       {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"template.list":       {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"harness_config.read": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"harness_config.list": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
}

// SupportedTargetClassesFor returns the reviewed classes for permissionID,
// defaulting to {TargetClassKindProjectScoped} when the permission is
// project-applicable but has no explicit multi-class entry above (its
// resource type has no global-catalog variant), and nil when the permission
// is not project-applicable at all (reviewed=false from
// AppliesToExistingProjectTarget).
func SupportedTargetClassesFor(permissionID string) []TargetClassKind {
	if classes, ok := SupportedTargetClasses[permissionID]; ok {
		return classes
	}
	if applies, reviewed := AppliesToExistingProjectTarget(permissionID); reviewed && applies {
		return []TargetClassKind{TargetClassKindProjectScoped}
	}
	return nil
}
