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

	// group.* — create/list always authorize against a parentless collection
	// Resource (createGroup uses Resource{Type:"group"} with no ParentID;
	// project_agents groups are auto-created internally with no Decide call
	// at all) and are reviewed false. read/update/delete/addMember/
	// removeMember are reviewed true: groupResource (capabilities.go) sets
	// ParentType="project" for project_agents groups, and BOTH real
	// enforcement (updateGroup/deleteGroup/addGroupMember/removeGroupMember
	// each call s.authorize against that exact resource) AND capability
	// computation (getGroup/listGroups call ComputeCapabilities[Batch]
	// against it, which evaluates every ResourceActions["group"] entry —
	// read/update/delete/addMember/removeMember) run Decide against that
	// project-parented target today; this table records that existing Decide
	// behavior as admission metadata rather than granting anything new.
	"group.create": false, "group.read": true, "group.update": true, "group.delete": true,
	"group.list": false, "group.addMember": true, "group.removeMember": true,

	// user.* — hub-wide.
	"user.read": false, "user.update": false, "user.invite": false, "user.suspend": false,
	"user.promote": false, "user.delete": false, "user.list": false,

	// policy.* — hub-wide.
	"policy.create": false, "policy.read": false, "policy.update": false,
	"policy.delete": false, "policy.list": false,

	// broker.* — user-owned hub resource, not project-contained.
	"broker.create": false, "broker.read": false, "broker.update": false,
	"broker.delete": false, "broker.list": false, "broker.dispatch": false,

	// gcp_service_account.* — create/list/mint are CapabilityScope: create
	// has no existing SA yet, list is the hub collection view, and mint (a
	// CapabilityScope action) is never evaluated per-instance — all three
	// stay reviewed false. read/delete/verify/assign are CapabilityResource:
	// each authorizes against the EXISTING service account
	// (gcpServiceAccountResource, capabilities.go), whose own
	// ParentType/ParentID is a project scope whenever sa.Scope ==
	// store.ScopeProject. ComputeCapabilities/ComputeCapabilitiesBatch
	// (capabilities.go) evaluate every ResourceActions["gcp_service_account"]
	// entry — read, delete, verify, assign — against that exact
	// project-parented resource today (getGCPServiceAccount,
	// listGCPServiceAccounts, handlers_gcp_identity_scoped.go), so all four
	// are reviewed true — this table records that existing Decide behavior
	// as admission metadata rather than granting anything new. Note that
	// the actual MUTATING enforcement gate for project-scoped SAs
	// (gcpServiceAccountVerdict, handlers_gcp_identity.go) authorizes
	// project-scope management through project.manage instead of these
	// permission IDs directly — the true-here classification reflects the
	// capability-computation Decide call, a real and separate code path,
	// not a claim about which permission the mutating handler itself checks.
	"gcp_service_account.create": false, "gcp_service_account.read": true,
	"gcp_service_account.delete": true, "gcp_service_account.list": false,
	"gcp_service_account.verify": true, "gcp_service_account.mint": false,
	"gcp_service_account.assign": true,

	// hub.* — hub-wide by definition, every entry false.
	"hub.settings.read": false, "hub.settings.update": false, "hub.config.read": false,
	"hub.config.update": false, "hub.maintenance.execute": false, "hub.diagnostics.read": false,
	"hub.health.read": false, "hub.admin_mode.read": false, "hub.admin_mode.update": false,
	"hub.integrations.read": false, "hub.integrations.update": false,
	"hub.lifecycle_hooks.read": false, "hub.lifecycle_hooks.update": false,
	"hub.allow_list.read": false, "hub.allow_list.update": false,
	"hub.project_defaults.read": false, "hub.project_defaults.update": false,
	"hub.messaging.update": false, "hub.experiments.update": false, "hub.auth_reset.execute": false,
	"hub.scheduler.read": false, "hub.scheduler.update": false,
	"hub.federation.read": false, "hub.federation.update": false,
	"hub.teams_manifest.read": false, "hub.teams_manifest.update": false,
	"hub.validate.execute": false, "hub.github_app.read": false, "hub.github_app.update": false,
	"hub.metrics.read": false, "hub.audit.read": false,

	// quota.* — every live route (handlers_quota.go) authorizes against
	// Resource{Type:"quota", ID:"hub"}, matching this family's own
	// CollectionTargetClasses (HubResource for all four). Reviewed false: no
	// project target exists for these permissions today.
	"quota.read": false, "quota.create": false, "quota.update": false, "quota.delete": false,

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

	// Material delivery and runtime-use permissions (ptone/scion#2129): every
	// one of them can apply to an existing project target (a project-scope
	// secret/env var/skill, or a project-parented GCP service account).
	"secret.deliver": true, "env_var.deliver": true, "skill_injection.deliver": true,
	"secret.use": true, "gcp_service_account.use": true,
}

// AppliesToExistingProjectTarget reports the reviewed disposition for
// permissionID. reviewed is false when permissionID has no entry — callers
// (e.g. hub.SystemAuthorityProof, hub.hasAnyProjectBinding) must treat that
// as "not established," never "true because unreviewed."
func AppliesToExistingProjectTarget(permissionID string) (applies bool, reviewed bool) {
	v, ok := ProjectTargetApplicability[permissionID]
	return v, ok
}

// PermissionAllowedBoundaries is a SEPARATE hand-reviewed table: which
// boundary kinds may select this permission's UAT scope at MINT time.
// Covers every permission with a non-empty Permission.UATScope today (that
// is the current universe of selectors) — a drift test requires an entry
// for every such Registry row — plus a small, explicit allowlist of
// permissions pre-reviewed ahead of their UATScope landing (see
// permissionAllowedBoundariesPreReviewedWithoutUATScope in registry_test.go,
// e.g. broker.create); a stale-key test rejects any other key.
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

	// broker.create's selector "broker:create" is hub-only: a broker is a
	// hub-level resource.
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
	// instance of the permission's resource type (skill/template/
	// harness_config's own project scope-kind, or any other project-
	// applicable resource type with no scope-kind split at all — agent,
	// gcp_service_account.assign's existing-SA project scope).
	TargetClassKindProjectScoped TargetClassKind = "project_scoped"
	// TargetClassKindGlobalCatalog represents the hub-wide (global/core)
	// catalog instance space, for the resource types that have one
	// (skill/template/harness_config read/list).
	TargetClassKindGlobalCatalog TargetClassKind = "global_catalog"
	// TargetClassKindHubResource represents an ordinary hub-scoped resource
	// with no project-scoped variant at all and no curated hub-wide-catalog
	// carve-out to apply (group, user, broker, gcp_service_account other
	// than assign, and any hub-only collection action such as
	// project.clone/register). applyHubWideScopeFilters passes these
	// resource types through unchanged regardless of class value; this
	// class exists so MintTimeSystemGrant has an explicit, reviewed entry
	// to iterate for these permissions instead of silently having none.
	TargetClassKindHubResource TargetClassKind = "hub_resource"
)

// SupportedTargetClasses is an explicit, reviewed, per-permission-ID list of
// target classes a permission can legitimately apply to, covering EVERY
// permission with a non-empty Permission.UATScope (the current universe of
// mintable selectors) — including hub-only permissions such as user.invite.
// Used ONLY for hub-boundary mint-time contemplation
// (hub.MintTimeSystemGrant) — NEVER inferred from ProjectTargetApplicability
// or PermissionAllowedBoundaries: a permission can legitimately support
// BOTH a global-catalog and a project-scoped class (skill/template/
// harness_config read/list), and a
// hub-only permission with no entry here must deny rather than silently
// inherit a guessed class — an unreviewed or absent permission ID returns
// nil from SupportedTargetClassesFor, which MintTimeSystemGrant treats as
// "no eligible class, deny." A drift test requires an entry for every
// Registry row with a non-empty UATScope.
var SupportedTargetClasses = map[string][]TargetClassKind{
	// agent.* — no scope-kind split.
	"agent.create": {TargetClassKindProjectScoped}, "agent.read": {TargetClassKindProjectScoped},
	"agent.list": {TargetClassKindProjectScoped}, "agent.delete": {TargetClassKindProjectScoped},
	"agent.attach": {TargetClassKindProjectScoped}, "agent.lifecycle": {TargetClassKindProjectScoped},
	"agent.port_access": {TargetClassKindProjectScoped}, "agent.message": {TargetClassKindProjectScoped},

	// project.* — read/update/manage target an existing project; clone is a
	// hub-level collection action (reviewed false in ProjectTargetApplicability)
	// even though it is mintable.
	"project.read": {TargetClassKindProjectScoped}, "project.update": {TargetClassKindProjectScoped},
	"project.manage": {TargetClassKindProjectScoped}, "project.clone": {TargetClassKindHubResource},

	// skill.* — read/list support both project and global catalog classes;
	// create/update/delete are project-scoped only; register is a hub-level
	// registry action.
	"skill.create": {TargetClassKindProjectScoped},
	"skill.read":   {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"skill.update": {TargetClassKindProjectScoped}, "skill.delete": {TargetClassKindProjectScoped},
	"skill.list":     {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"skill.register": {TargetClassKindHubResource},

	// template.*, harness_config.* — unlike skill, a single create
	// permission covers every scope (templateScopeResource/
	// templateUserScopeResource/harnessConfigScopeResource all route through
	// template.create/harness_config.create regardless of scope), so create
	// supports GlobalCatalog too, not just ProjectScoped.
	"template.create": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"template.read":   {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"template.update": {TargetClassKindProjectScoped}, "template.delete": {TargetClassKindProjectScoped},
	"template.list": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},

	"harness_config.create": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"harness_config.read":   {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},
	"harness_config.update": {TargetClassKindProjectScoped}, "harness_config.delete": {TargetClassKindProjectScoped},
	"harness_config.list": {TargetClassKindProjectScoped, TargetClassKindGlobalCatalog},

	// group.* — create/list are the hub collection actions (no project-scoped
	// variant reaches Decide, see ProjectTargetApplicability). read/update/
	// delete/addMember/removeMember support BOTH classes: PermissionAllowedBoundaries
	// keeps these Hub-only at MINT time (unchanged, pending a separate mint
	// review per the ruling), but a hub-boundary token's mint-time
	// contemplation must still list ProjectScoped here because
	// SystemAuthorityProof now admits a real project-parented target for
	// these IDs (see ProjectTargetApplicability) — mint and use must read the
	// same per-permission facts.
	"group.create": {TargetClassKindHubResource}, "group.read": {TargetClassKindHubResource, TargetClassKindProjectScoped},
	"group.update": {TargetClassKindHubResource, TargetClassKindProjectScoped}, "group.delete": {TargetClassKindHubResource, TargetClassKindProjectScoped},
	"group.list": {TargetClassKindHubResource}, "group.addMember": {TargetClassKindHubResource, TargetClassKindProjectScoped},
	"group.removeMember": {TargetClassKindHubResource, TargetClassKindProjectScoped},

	// user.* — hub-wide. user.invite is the reviewed super-admin hub-only
	// mint case this table must cover explicitly, not by omission.
	"user.read": {TargetClassKindHubResource}, "user.invite": {TargetClassKindHubResource},
	"user.list": {TargetClassKindHubResource},

	// broker.* — user-owned hub resource.
	"broker.read": {TargetClassKindHubResource}, "broker.list": {TargetClassKindHubResource},

	// gcp_service_account.* — read/verify/assign are all mixed-class: each
	// authorizes against the existing SA (gcpServiceAccountResource), which
	// carries a project scope whenever sa.Scope == store.ScopeProject (see
	// ProjectTargetApplicability). list has no per-instance variant (hub
	// collection view only). gcp_service_account.delete has no UATScope, so
	// it is not a selector and has no entry in this UATScope-only table,
	// even though ProjectTargetApplicability[gcp_service_account.delete] is
	// also true.
	"gcp_service_account.read":   {TargetClassKindHubResource, TargetClassKindProjectScoped},
	"gcp_service_account.list":   {TargetClassKindHubResource},
	"gcp_service_account.verify": {TargetClassKindHubResource, TargetClassKindProjectScoped},
	"gcp_service_account.assign": {TargetClassKindProjectScoped},

	// broker.create targets the hub-level broker collection only.
	"broker.create": {TargetClassKindHubResource},
}

// SupportedTargetClassesFor returns the reviewed classes for permissionID.
// An unreviewed or absent permission ID returns nil — MintTimeSystemGrant
// treats that as "no eligible class for hub-boundary mint-time
// contemplation," never a guessed default from ProjectTargetApplicability
// or PermissionAllowedBoundaries.
func SupportedTargetClassesFor(permissionID string) []TargetClassKind {
	return SupportedTargetClasses[permissionID]
}
