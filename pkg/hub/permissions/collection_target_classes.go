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
// collection-level example). Using a boolean here was the exact bug
// pat-refactor's R1 review caught: skill.list is reviewed
// ProjectTargetApplicability=true (it CAN target an existing project) AND
// separately, legitimately, resolves Hub-scope collection evidence for
// listing the global catalog — a single "applies" bool cannot represent
// both. A permission absent from this table denies collection-evidence
// resolution outright (Unknown), regardless of what CollectionScope the
// caller claims.
//
// Values mirror the same per-permission review already performed for
// ProjectTargetApplicability/PermissionAllowedBoundaries/SupportedTargetClasses,
// re-expressed as a class set for this distinct question; the three tables
// are maintained independently and are not derived from one another.
var CollectionTargetClasses = map[string][]TargetClassKind{
	// agent.* — every instance is project-contained; agent.create is the
	// canonical "create a new agent inside an already-identified project"
	// collection case.
	"agent.create": {TargetClassKindProjectScoped}, "agent.read": {TargetClassKindProjectScoped},
	"agent.list": {TargetClassKindProjectScoped}, "agent.update": {TargetClassKindProjectScoped},
	"agent.delete": {TargetClassKindProjectScoped}, "agent.attach": {TargetClassKindProjectScoped},
	"agent.lifecycle": {TargetClassKindProjectScoped}, "agent.port_access": {TargetClassKindProjectScoped},
	"agent.stop_all": {TargetClassKindProjectScoped}, "agent.message": {TargetClassKindProjectScoped},
	"agent.set_message_mode": {TargetClassKindProjectScoped}, "agent.grant_hub_mode": {TargetClassKindProjectScoped},
	"agent.status_update": {TargetClassKindProjectScoped}, "agent.log_append": {TargetClassKindProjectScoped},
	"agent.notify": {TargetClassKindProjectScoped}, "agent.token_refresh": {TargetClassKindProjectScoped},
	"agent.port_forward": {TargetClassKindProjectScoped}, "agent.identity_token": {TargetClassKindProjectScoped},

	// project.* — create/register/clone/list are hub-level collection
	// actions (no existing project targeted); read/update/delete/manage/
	// set_messaging_policy target an existing project.
	"project.create": {TargetClassKindHubResource}, "project.read": {TargetClassKindProjectScoped},
	"project.update": {TargetClassKindProjectScoped}, "project.delete": {TargetClassKindProjectScoped},
	"project.manage": {TargetClassKindProjectScoped}, "project.register": {TargetClassKindHubResource},
	"project.set_messaging_policy": {TargetClassKindProjectScoped}, "project.clone": {TargetClassKindHubResource},
	"project.list": {TargetClassKindHubResource},

	// skill.* — read/list genuinely support BOTH an existing project's
	// skills and the hub-wide catalog (the exact case R1 flagged); create/
	// update/delete are project-scoped only; create_global/register are
	// hub-level collection actions.
	"skill.create": {TargetClassKindProjectScoped},
	"skill.create_global": {TargetClassKindHubResource},
	"skill.read": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"skill.update": {TargetClassKindProjectScoped}, "skill.delete": {TargetClassKindProjectScoped},
	"skill.list":     {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"skill.register": {TargetClassKindHubResource},

	// template.*, harness_config.* — same shape as skill.
	"template.create": {TargetClassKindProjectScoped},
	"template.read":   {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"template.update": {TargetClassKindProjectScoped}, "template.delete": {TargetClassKindProjectScoped},
	"template.list": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},

	"harness_config.create": {TargetClassKindProjectScoped},
	"harness_config.read":   {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"harness_config.update": {TargetClassKindProjectScoped}, "harness_config.delete": {TargetClassKindProjectScoped},
	"harness_config.list": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},

	// group.* — hub-wide resource, no project-scoped variant.
	"group.create": {TargetClassKindHubResource}, "group.read": {TargetClassKindHubResource},
	"group.update": {TargetClassKindHubResource}, "group.delete": {TargetClassKindHubResource},
	"group.list": {TargetClassKindHubResource}, "group.addMember": {TargetClassKindHubResource},
	"group.removeMember": {TargetClassKindHubResource},

	// user.* — hub-wide.
	"user.read": {TargetClassKindHubResource}, "user.update": {TargetClassKindHubResource},
	"user.invite": {TargetClassKindHubResource}, "user.suspend": {TargetClassKindHubResource},
	"user.promote": {TargetClassKindHubResource}, "user.delete": {TargetClassKindHubResource},
	"user.list": {TargetClassKindHubResource},

	// policy.* — hub-wide.
	"policy.create": {TargetClassKindHubResource}, "policy.read": {TargetClassKindHubResource},
	"policy.update": {TargetClassKindHubResource}, "policy.delete": {TargetClassKindHubResource},
	"policy.list": {TargetClassKindHubResource},

	// broker.* — user-owned hub resource, not project-contained.
	"broker.create": {TargetClassKindHubResource}, "broker.read": {TargetClassKindHubResource},
	"broker.update": {TargetClassKindHubResource}, "broker.delete": {TargetClassKindHubResource},
	"broker.list": {TargetClassKindHubResource}, "broker.dispatch": {TargetClassKindHubResource},

	// gcp_service_account.* — hub/user resource; assign is the reviewed
	// exception, targeting an agent inside an existing project.
	"gcp_service_account.create": {TargetClassKindHubResource},
	"gcp_service_account.read":   {TargetClassKindHubResource},
	"gcp_service_account.delete": {TargetClassKindHubResource},
	"gcp_service_account.list":   {TargetClassKindHubResource},
	"gcp_service_account.verify": {TargetClassKindHubResource},
	"gcp_service_account.mint":   {TargetClassKindHubResource},
	"gcp_service_account.assign": {TargetClassKindProjectScoped},

	// hub.* — hub-wide by definition.
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
	"hub.metrics.read": {TargetClassKindHubResource}, "hub.audit.read": {TargetClassKindHubResource},

	// quota.* — hub-wide definitions; entitlement bindings reviewed
	// conservatively hub-scoped for collection-evidence purposes.
	"quota.read": {TargetClassKindHubResource}, "quota.create": {TargetClassKindHubResource},
	"quota.update": {TargetClassKindHubResource}, "quota.delete": {TargetClassKindHubResource},

	// role.* — hub-wide definitions; role_binding.*/access_constraint.* can
	// target an existing project's binding/constraint.
	"role.read": {TargetClassKindHubResource}, "role.create": {TargetClassKindHubResource},
	"role.update": {TargetClassKindHubResource}, "role.delete": {TargetClassKindHubResource},
	"role_binding.read":   {TargetClassKindProjectScoped},
	"role_binding.create": {TargetClassKindProjectScoped},
	"role_binding.delete": {TargetClassKindProjectScoped},

	"access_constraint.admin": {TargetClassKindProjectScoped},
	"access_constraint.read":  {TargetClassKindProjectScoped},

	// scheduled_event.* — always scoped to a project.
	"scheduled_event.read": {TargetClassKindProjectScoped}, "scheduled_event.list": {TargetClassKindProjectScoped},
	"scheduled_event.create": {TargetClassKindProjectScoped}, "scheduled_event.delete": {TargetClassKindProjectScoped},
	"scheduled_event.update": {TargetClassKindProjectScoped},

	"project.secret_read": {TargetClassKindProjectScoped},
}

// CollectionTargetClassesFor returns the reviewed classes for permissionID
// and whether it has been reviewed at all. An unreviewed permission ID
// (reviewed=false) must deny collection-evidence resolution outright.
func CollectionTargetClassesFor(permissionID string) (classes []TargetClassKind, reviewed bool) {
	classes, reviewed = CollectionTargetClasses[permissionID]
	return classes, reviewed
}
