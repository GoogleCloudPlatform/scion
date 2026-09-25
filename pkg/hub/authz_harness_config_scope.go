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

// curatedHarnessConfigDirectoryRoles are the built-in, system-scoped roles
// whose harness_config.read/harness_config.list permission
// (hubMemberPermissionIDs, hubViewerPermissionIDs in seed.go) exists purely
// so every hub member can browse the hub-wide (global) harness-config
// catalog. It is a directory convenience for hub-scoped resources, not a
// scope override for user- or project-scoped harness configs.
var curatedHarnessConfigDirectoryRoles = map[string]struct{}{
	store.SystemRoleHubMember: {},
	store.SystemRoleHubViewer: {},
}

// filterHubWideHarnessConfigGrants removes system-scoped candidate bindings
// for the curated hub-member/hub-viewer roles unless the target harness
// config is itself hub-scoped (scope "global").
//
// ptone/scion#1916: a hub member must not be able to read another user's
// user-scoped harness config, or another project's project-scoped harness
// config, merely because the hub-member role carries
// harness_config.read/harness_config.list at system scope. That grant is
// meant to cover the hub-wide catalog only — mirrors filterHubWideSkillGrants
// (ptone/scion#1901), which established the same rule for skills.
//
// Left untouched, by construction:
//   - Project-scoped bindings (project-member/owner/admin): already
//     correctly contained by the kernel's scope containment check
//     (scopeApplies), which only applies a project-scoped grant when its
//     ScopeID matches the resource's project.
//   - Elevated system-scoped roles (hub-admin, super-admin): unaffected —
//     not in curatedHarnessConfigDirectoryRoles.
//   - The resource-owner relationship grant (checkRelationshipGrants),
//     evaluated separately after the kernel: an owner keeps access to their
//     own user-scoped harness config even once the hub-member grant no
//     longer applies to it here.
//
// harnessConfigScope is the harness config's own Scope value
// (store.HarnessConfigScope*), taken from Resource.ScopeKind. This fails
// closed, mirroring filterHubWideSkillGrants: only "global" is exempt.
// Project, user, an empty string, and any future or unrecognized value are
// all filtered like project/user. Every ad hoc "harness_config" Resource
// must be built through harnessConfigResource or harnessConfigScopeResource
// (never a bare literal) so ScopeKind is always set correctly — see
// TestHarnessConfigResourceLiterals_AllUseCanonicalConstructor.
func filterHubWideHarnessConfigGrants(candidates []CandidateBinding, roleDefs map[string]*RolePermissions, harnessConfigScope string) []CandidateBinding {
	if len(candidates) == 0 {
		return candidates
	}
	if harnessConfigScope == store.HarnessConfigScopeGlobal {
		return candidates
	}

	filtered := make([]CandidateBinding, 0, len(candidates))
	for _, cb := range candidates {
		if cb.ScopeType == ScopeTypeSystem {
			if role := roleDefs[cb.RoleDefinitionID]; role != nil {
				if _, curated := curatedHarnessConfigDirectoryRoles[role.RoleName]; curated {
					continue
				}
			}
		}
		filtered = append(filtered, cb)
	}
	return filtered
}
