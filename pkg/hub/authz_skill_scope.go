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

import "github.com/GoogleCloudPlatform/scion/pkg/store"

// curatedSkillDirectoryRoles are the built-in, system-scoped roles whose
// skill.read/skill.list permission (hubMemberPermissionIDs,
// hubViewerPermissionIDs in seed.go) exists purely so every hub member can
// browse the hub-wide (global/core) skill catalog. It is a directory
// convenience for hub-scoped resources, not a scope override for user- or
// project-scoped skills.
var curatedSkillDirectoryRoles = map[string]struct{}{
	store.SystemRoleHubMember: {},
	store.SystemRoleHubViewer: {},
}

// filterHubWideSkillGrants removes system-scoped candidate bindings for the
// curated hub-member/hub-viewer roles unless the target skill is itself
// hub-scoped (scope "global" or "core").
//
// ptone/scion#1901: a hub member must not be able to read another user's
// user-scoped skill, or another project's project-scoped skill, merely
// because the hub-member role carries skill.read/skill.list at system
// scope. That grant is meant to cover the hub-wide catalog only — per the
// ptone/scion#1793 ruling, "the hub-scope grant must cover hub-scope
// resources only."
//
// Left untouched, by construction:
//   - Project-scoped bindings (project-member/owner/admin): already
//     correctly contained by the kernel's scope containment check
//     (scopeApplies), which only applies a project-scoped grant when its
//     ScopeID matches the resource's project.
//   - Elevated system-scoped roles (hub-admin, super-admin): hub admins
//     retain visibility into every skill regardless of scope, per the
//     ptone/scion#1901 ruling ("readable only by the owning user and hub
//     admins").
//   - The resource-owner relationship grant (checkRelationshipGrants),
//     evaluated separately after the kernel: an owner keeps access to their
//     own user-scoped skill even once the hub-member grant no longer
//     applies to it here.
//
// skillScope is the skill's own Scope value (store.SkillScope*), taken from
// Resource.ScopeKind. ptone/scion#1901 finding F4: this fails closed. Only
// "global" and "core" are exempt; user, project, an empty string, and any
// future or unrecognized value are all filtered like project/user. Before
// this fix, an ad hoc Resource{Type:"skill"} literal that forgot to set
// ScopeKind (see finding F3, canUseProjectGitHubToken) silently kept the
// curated hub-member/hub-viewer grant in force — exactly the #1901 leak,
// reopened through a different call site. Legitimate create-time checks
// that have no existing skill to read still pass an explicit scope via
// skillScopeResource (never a bare literal), so they are unaffected.
func filterHubWideSkillGrants(candidates []CandidateBinding, roleDefs map[string]*RolePermissions, skillScope string) []CandidateBinding {
	if len(candidates) == 0 {
		return candidates
	}
	// Fail closed (ptone/scion#1901 finding F4): only an explicit hub-scope
	// value (global/core) is exempt from filtering. Anything else — user,
	// project, an unrecognized future scope, or a caller-built Resource
	// literal that forgot to set ScopeKind — is filtered the same as
	// user/project. A hand-built ad hoc Resource{Type:"skill"} literal that
	// omits ScopeKind (the exact shape of finding F3) must not silently fail
	// open and re-admit the curated hub-member/hub-viewer grant; every
	// legitimate caller builds its Resource via skillResource or
	// skillScopeResource, both of which always set ScopeKind explicitly.
	if skillScope == store.SkillScopeGlobal || skillScope == store.SkillScopeCore {
		return candidates
	}

	filtered := make([]CandidateBinding, 0, len(candidates))
	for _, cb := range candidates {
		if cb.ScopeType == ScopeTypeSystem {
			if role := roleDefs[cb.RoleDefinitionID]; role != nil {
				if _, curated := curatedSkillDirectoryRoles[role.RoleName]; curated {
					continue
				}
			}
		}
		filtered = append(filtered, cb)
	}
	return filtered
}
