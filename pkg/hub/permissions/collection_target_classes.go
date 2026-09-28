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

// CollectionTargetClasses is an explicit, hand-reviewed, per-permission-ID
// SET of target classes a COLLECTION-LEVEL request (no existing resource
// instance) for this permission can legitimately resolve to. This is a
// SEPARATE table from ProjectTargetApplicability (a coarse boolean used
// only for system-role admission evidence) and from SupportedTargetClasses
// (mint-eligibility-only, covering just UATScope-bearing permissions):
// ResolveTargetScope's collection-evidence cross-check needs a reviewed set
// covering EVERY permission that can appear as evidence.PermissionID,
// including non-mintable ones (project.create has no UATScope at all, so
// it is absent from SupportedTargetClasses, yet is the canonical
// collection-level example).
//
// A permission's entry is EXPLICITLY EMPTY (`{}`, present in the map with a
// zero-length slice, distinct from an absent key) when it is
// instance-only: it always targets an already-existing resource and can
// NEVER legitimately appear as evidence.PermissionID for a collection-level
// request, regardless of whether its resource family can otherwise live in
// a project (pat-refactor R1, 2026-09-28 — the exact bug this fixes:
// agent.attach/delete/token_refresh etc. were previously assigned
// non-empty sets merely because ProjectTargetApplicability["agent.*"] is
// true, which would have let a malformed missing-instance request for one
// of those permissions be accepted as a declared collection target).
// CollectionTargetClassesFor returns reviewed=false for a permission ID
// absent from this map entirely (unreviewed) — ResolveTargetScope denies in
// both cases (unreviewed OR reviewed-empty), but the two are represented
// distinctly here because they are reviewed differently: "not yet reviewed"
// vs. "reviewed and confirmed never collection-level."
//
// The instance-only/collection-capable split below is drawn from each
// permission's OWN existing, independently authored Permission.CapabilityKind
// field (CapabilityScope = applies to a collection/scope, CapabilityResource
// or unset = applies to one individual, already-existing resource) — not a
// new inference invented for this table, but the same distinction
// Permission.CapabilityKind's own doc comment already declares. Every
// CapabilityScope permission below is then hand-reviewed for WHICH
// classes it supports (ProjectScoped / GlobalCatalog / HubResource), the
// same way ProjectTargetApplicability/PermissionAllowedBoundaries/
// SupportedTargetClasses were reviewed; the three tables remain
// independently maintained.
var CollectionTargetClasses = map[string][]TargetClassKind{
	// agent.* — create/list/stop_all/message are CapabilityScope (no
	// existing instance targeted); every other agent.* permission is
	// CapabilityResource or has no CapabilityKind at all (self-service
	// endpoints) and always targets an existing, specific agent.
	"agent.create": {TargetClassKindProjectScoped}, "agent.read": {},
	"agent.list": {TargetClassKindProjectScoped}, "agent.update": {},
	"agent.delete": {}, "agent.attach": {}, "agent.lifecycle": {}, "agent.port_access": {},
	"agent.stop_all": {TargetClassKindProjectScoped}, "agent.message": {TargetClassKindProjectScoped},
	"agent.set_message_mode": {}, "agent.grant_hub_mode": {},
	"agent.status_update": {}, "agent.log_append": {}, "agent.notify": {},
	"agent.token_refresh": {}, "agent.port_forward": {}, "agent.identity_token": {},

	// project.* — create/list are CapabilityScope; register/clone are
	// CapabilityResource (register/clone target an EXISTING project — the
	// one being registered or cloned FROM — despite superficially sounding
	// creation-like; reviewed down from an earlier, incorrect
	// classification). read/update/delete/manage/set_messaging_policy are
	// CapabilityResource.
	"project.create": {TargetClassKindHubResource}, "project.read": {},
	"project.update": {}, "project.delete": {}, "project.manage": {},
	"project.register": {}, "project.set_messaging_policy": {}, "project.clone": {},
	"project.list": {TargetClassKindHubResource},

	// skill.* — create/create_global/list/register are CapabilityScope;
	// read/update/delete are CapabilityResource (always an existing skill).
	"skill.create": {TargetClassKindProjectScoped},
	"skill.create_global": {TargetClassKindHubResource},
	"skill.read": {}, "skill.update": {}, "skill.delete": {},
	"skill.list":     {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"skill.register": {TargetClassKindHubResource},

	// template.*, harness_config.* — same CapabilityKind shape as skill.
	"template.create": {TargetClassKindProjectScoped},
	"template.read": {}, "template.update": {}, "template.delete": {},
	"template.list": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},

	"harness_config.create": {TargetClassKindProjectScoped},
	"harness_config.read": {}, "harness_config.update": {}, "harness_config.delete": {},
	"harness_config.list": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},

	// group.* — create/list are CapabilityScope; everything else targets
	// an existing group.
	"group.create": {TargetClassKindHubResource}, "group.read": {},
	"group.update": {}, "group.delete": {},
	"group.list": {TargetClassKindHubResource}, "group.addMember": {}, "group.removeMember": {},

	// user.* — invite/list are CapabilityScope (inviting creates a new
	// user record; listing targets no single user); read/update/suspend/
	// promote/delete always target an existing user.
	"user.read": {}, "user.update": {}, "user.invite": {TargetClassKindHubResource},
	"user.suspend": {}, "user.promote": {}, "user.delete": {},
	"user.list": {TargetClassKindHubResource},

	// policy.* — create/list are CapabilityScope; read/update/delete
	// target an existing policy.
	"policy.create": {TargetClassKindHubResource}, "policy.read": {},
	"policy.update": {}, "policy.delete": {}, "policy.list": {TargetClassKindHubResource},

	// broker.* — create/list are CapabilityScope; everything else
	// (including dispatch, which targets an existing broker) does not.
	"broker.create": {TargetClassKindHubResource}, "broker.read": {},
	"broker.update": {}, "broker.delete": {},
	"broker.list": {TargetClassKindHubResource}, "broker.dispatch": {},

	// gcp_service_account.* — create/list/mint are CapabilityScope; read/
	// delete/verify/assign are CapabilityResource (assign targets an
	// EXISTING GCP service account AND an existing agent — reviewed down
	// from an earlier, incorrect classification).
	"gcp_service_account.create": {TargetClassKindHubResource},
	"gcp_service_account.read": {}, "gcp_service_account.delete": {},
	"gcp_service_account.list": {TargetClassKindHubResource},
	"gcp_service_account.verify": {}, "gcp_service_account.assign": {},
	"gcp_service_account.mint": {TargetClassKindHubResource},

	// hub.* — every entry is CapabilityScope (there is no per-instance
	// concept for hub-wide configuration) except hub.audit.read, which has
	// no CapabilityKind (an explain/audit action, not a collection target).
	"hub.settings.read": {TargetClassKindHubResource}, "hub.settings.update": {TargetClassKindHubResource},
	"hub.config.read": {TargetClassKindHubResource}, "hub.config.update": {TargetClassKindHubResource},
	"hub.maintenance.execute": {TargetClassKindHubResource}, "hub.diagnostics.read": {TargetClassKindHubResource},
	"hub.health.read": {TargetClassKindHubResource}, "hub.admin_mode.read": {TargetClassKindHubResource},
	"hub.admin_mode.update": {TargetClassKindHubResource}, "hub.integrations.read": {TargetClassKindHubResource},
	"hub.integrations.update": {TargetClassKindHubResource}, "hub.lifecycle_hooks.read": {TargetClassKindHubResource},
	"hub.lifecycle_hooks.update": {TargetClassKindHubResource}, "hub.allow_list.read": {TargetClassKindHubResource},
	"hub.allow_list.update": {TargetClassKindHubResource}, "hub.project_defaults.read": {TargetClassKindHubResource},
	"hub.project_defaults.update": {TargetClassKindHubResource}, "hub.messaging.update": {TargetClassKindHubResource},
	"hub.auth_reset.execute": {TargetClassKindHubResource}, "hub.scheduler.read": {TargetClassKindHubResource},
	"hub.scheduler.update": {TargetClassKindHubResource}, "hub.federation.read": {TargetClassKindHubResource},
	"hub.federation.update": {TargetClassKindHubResource}, "hub.teams_manifest.read": {TargetClassKindHubResource},
	"hub.teams_manifest.update": {TargetClassKindHubResource}, "hub.validate.execute": {TargetClassKindHubResource},
	"hub.github_app.read": {TargetClassKindHubResource}, "hub.github_app.update": {TargetClassKindHubResource},
	"hub.metrics.read": {TargetClassKindHubResource}, "hub.audit.read": {},

	// quota.* — every entry is CapabilityScope.
	"quota.read": {TargetClassKindHubResource}, "quota.create": {TargetClassKindHubResource},
	"quota.update": {TargetClassKindHubResource}, "quota.delete": {TargetClassKindHubResource},

	// role.* — role DEFINITIONS: every entry is CapabilityScope (hub-wide).
	"role.read": {TargetClassKindHubResource}, "role.create": {TargetClassKindHubResource},
	"role.update": {TargetClassKindHubResource}, "role.delete": {TargetClassKindHubResource},
	// role_binding.* — all three are CapabilityScope per the registry's own
	// existing modeling; a binding's scope can be a project.
	"role_binding.read": {TargetClassKindProjectScoped}, "role_binding.create": {TargetClassKindProjectScoped},
	"role_binding.delete": {TargetClassKindProjectScoped},

	// access_constraint.* — both CapabilityScope; a constraint's scope can
	// be a project.
	"access_constraint.admin": {TargetClassKindProjectScoped},
	"access_constraint.read":  {TargetClassKindProjectScoped},

	// scheduled_event.* — list/create are CapabilityScope; read/delete/
	// update target an existing scheduled event.
	"scheduled_event.read": {}, "scheduled_event.list": {TargetClassKindProjectScoped},
	"scheduled_event.create": {TargetClassKindProjectScoped}, "scheduled_event.delete": {},
	"scheduled_event.update": {},

	// project.secret_read — agent self-service, no CapabilityKind, always
	// an existing project's secret.
	"project.secret_read": {},
}

// CollectionTargetClassesFor returns the reviewed classes for permissionID
// and whether it has been reviewed at all. An unreviewed permission ID
// (reviewed=false) must deny collection-evidence resolution outright — as
// must a REVIEWED but empty set (reviewed=true, len(classes)==0), which
// means this permission is confirmed instance-only and never legitimately
// collection-level.
func CollectionTargetClassesFor(permissionID string) (classes []TargetClassKind, reviewed bool) {
	classes, reviewed = CollectionTargetClasses[permissionID]
	return classes, reviewed
}
