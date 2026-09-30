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
// a project — a permission whose resource family can live in a project
// (ProjectTargetApplicability=true) is not automatically collection-capable
// itself (agent.attach/delete/token_refresh, for example, always target a
// specific existing agent and must never be accepted as a declared
// collection target). CollectionTargetClassesFor returns reviewed=false for
// a permission ID absent from this map entirely (unreviewed) —
// ResolveTargetScope denies in both cases (unreviewed OR reviewed-empty),
// but the two are represented distinctly here because they are reviewed
// differently: "not yet reviewed" vs. "reviewed and confirmed never
// collection-level."
//
// Reviewed against ACTUAL live routes and handlers, not inferred from
// Permission.CapabilityKind: CapabilityKind records whether a permission
// conceptually applies to an individual resource or a collection/scope, but
// it is not a collection-route inventory and cannot decide these rows by
// itself — concrete counterexamples: `handlers_roles.go`'s GET
// role-bindings list and POST role-binding create both authorize against a
// hard-coded `Resource{Type:"role_binding", ID:"hub"}` regardless of
// whether the binding being listed/created is project- or system-scoped
// (`role_binding.read`/`role_binding.create` are Hub-only in practice, not
// ProjectScoped, despite CapabilityKind=Scope suggesting otherwise);
// `handlers_access_constraints.go`'s admin actions authorize the same way
// against `Resource{Type:"access_constraint", ID:"hub"}` regardless of the
// constraint record's own ScopeType (`access_constraint.admin`/`.read` are
// also Hub-only, even though a constraint RECORD can have ScopeType=project
// — administering constraints is a hub-level permission, independent of
// which scope a given constraint governs); `handlers_quota.go`'s
// create/update/delete all authorize against `Resource{Type:"quota",
// ID:"hub"}` (confirmed HubResource, as already reviewed).
// CapabilityKind remains a useful STARTING heuristic for which permissions
// are instance-only (single-resource) actions that can never be
// collection-level at all (agent.attach/delete/token_refresh, etc. — no
// route ever authorizes these except against a specific existing
// resource), but every CapabilityScope-or-otherwise-collection-shaped
// entry below is checked against its actual authorization call site before
// being assigned a non-empty class set.
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
	// creation-like). read/update/delete/manage/set_messaging_policy are
	// CapabilityResource.
	"project.create": {TargetClassKindHubResource}, "project.read": {},
	"project.update": {}, "project.delete": {}, "project.manage": {},
	"project.register": {}, "project.set_messaging_policy": {}, "project.clone": {},
	"project.list": {TargetClassKindHubResource},

	// skill.* — create/create_global/list/register are CapabilityScope;
	// read/update/delete are CapabilityResource (always an existing skill).
	"skill.create":        {TargetClassKindProjectScoped},
	"skill.create_global": {TargetClassKindHubResource},
	"skill.read":          {}, "skill.update": {}, "skill.delete": {},
	"skill.list":     {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"skill.register": {TargetClassKindHubResource},

	// template.*, harness_config.* — same CapabilityKind shape as skill,
	// but unlike skill, there is no separate *_create_global permission ID:
	// the single template.create/harness_config.create permission covers
	// every scope. templateScopeResource(store.TemplateScopeGlobal, "")
	// (template_handlers.go, global create) and templateUserScopeResource
	// (template_handlers.go, user-scope create) both build a parentless
	// Resource with ScopeKind global/user — computeTargetFacts maps both to
	// the same hasGlobalScope fact, so both need TargetClassKindGlobalCatalog
	// here alongside the project-scoped create path
	// (templateScopeResource(store.TemplateScopeProject, projectID)).
	// harnessConfigScopeResource (harness_config_handlers.go) is the same
	// shape for harness_config.create.
	"template.create": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"template.read":   {}, "template.update": {}, "template.delete": {},
	"template.list": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},

	"harness_config.create": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"harness_config.read":   {}, "harness_config.update": {}, "harness_config.delete": {},
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

	// gcp_service_account.* — create/list/mint are CapabilityScope, hub-wide.
	// assign is CapabilityResource, confirmed against its actual
	// authorization call site: evaluateSAAssignment/authorizeSAAssignment
	// (sa_assign_gate.go, invoked from handlers_agents_core.go during agent
	// create/patch) authorizes using gcpServiceAccountResource(sa)
	// (gcpServiceAccountResource, capabilities.go), whose ID is the EXISTING gcp_service_account
	// being assigned and whose ParentType/ParentID (when set) is that SA's
	// OWN scope — NOT the new agent being created/patched. assign always
	// targets an existing SA instance, so its entry is empty (never
	// collection-level); classifying it from the call site's context (agent
	// creation) rather than the actual Resource authorized would be wrong.
	"gcp_service_account.create": {TargetClassKindHubResource},
	"gcp_service_account.read":   {}, "gcp_service_account.delete": {},
	"gcp_service_account.list":   {TargetClassKindHubResource},
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
	"hub.experiments.update": {TargetClassKindHubResource},
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
	// role_binding.* — reviewed against handlers_roles.go's actual
	// authorization call sites, not CapabilityKind (both are Scope, which
	// would incorrectly suggest ProjectScoped): handleAdminRoleBindings's
	// GET (list) authorizes role_binding.read against a hard-coded
	// Resource{Type:"role_binding", ID:"hub"} UNCONDITIONALLY — regardless
	// of whether the bindings being listed are project- or system-scoped —
	// so role_binding.read is Hub-only for collection purposes.
	// createRoleBindingScopeAware's POST authorizes role_binding.create the
	// same way ONLY for system-scoped requests; a project-scoped POST
	// defers entirely to project.manage instead (a different permission),
	// so role_binding.create itself is also only ever evaluated at Hub
	// scope. role_binding.delete's handler
	// (requireWritePermissionForRoleBinding) authorizes against the same
	// hard-coded hub-scope Resource regardless of the target binding's
	// scope, and always targets an existing binding by ID — reviewed empty
	// (never a collection-level evidence case; enforced via a fixed
	// hub-scope gate in the handler, not per-instance/per-collection
	// resolution).
	"role_binding.read": {TargetClassKindHubResource}, "role_binding.create": {TargetClassKindHubResource},
	"role_binding.delete": {},

	// access_constraint.* — reviewed against handlers_access_constraints.go:
	// requireConstraintAdminPermission (create/update/delete/preview) and
	// the read path both authorize against a hard-coded
	// Resource{Type:"access_constraint", ID:"hub"} regardless of the
	// CONSTRAINT RECORD's own ScopeType (a constraint's data can be
	// project-scoped, but administering/reading constraints is itself a
	// hub-level permission). Hub-only for collection purposes, not
	// ProjectScoped.
	"access_constraint.admin": {TargetClassKindHubResource},
	"access_constraint.read":  {TargetClassKindHubResource},

	// scheduled_event.* — list/create are CapabilityScope; read/delete/
	// update target an existing scheduled event.
	"scheduled_event.read": {}, "scheduled_event.list": {TargetClassKindProjectScoped},
	"scheduled_event.create": {TargetClassKindProjectScoped}, "scheduled_event.delete": {},
	"scheduled_event.update": {},

	// project.secret_read — agent self-service, no CapabilityKind, always
	// an existing project's secret.
	"project.secret_read": {},

	// Material delivery and runtime-use permissions (ptone/scion#2129):
	// each always targets one existing secret, environment variable, skill
	// reference or GCP service account. None has a collection/list shape, so
	// every entry is reviewed empty, matching project.secret_read above.
	"secret.deliver": {}, "env_var.deliver": {}, "skill_injection.deliver": {},
	"secret.use": {}, "gcp_service_account.use": {},
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
