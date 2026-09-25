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

// curatedTemplateDirectoryRoles are the built-in, system-scoped roles whose
// template.read/template.list permission (hubMemberPermissionIDs,
// hubViewerPermissionIDs in seed.go) exists purely so every hub member can
// browse the hub-wide (global) template catalog. It is a directory
// convenience for hub-scoped resources, not a scope override for user- or
// project-scoped templates.
var curatedTemplateDirectoryRoles = map[string]struct{}{
	store.SystemRoleHubMember: {},
	store.SystemRoleHubViewer: {},
}

// filterHubWideTemplateGrants removes system-scoped candidate bindings for
// the curated hub-member/hub-viewer roles unless the target template is
// itself hub-scoped (scope "global").
//
// ptone/scion#1916: a hub member must not be able to read another user's
// user-scoped template, or another project's project-scoped template,
// merely because the hub-member role carries template.read/template.list at
// system scope. That grant is meant to cover the hub-wide catalog only —
// mirrors filterHubWideSkillGrants (ptone/scion#1901), which established the
// same rule for skills.
//
// Left untouched, by construction:
//   - Project-scoped bindings (project-member/owner/admin): already
//     correctly contained by the kernel's scope containment check
//     (scopeApplies), which only applies a project-scoped grant when its
//     ScopeID matches the resource's project.
//   - Elevated system-scoped roles (hub-admin, super-admin): unaffected —
//     not in curatedTemplateDirectoryRoles.
//   - The resource-owner relationship grant (checkRelationshipGrants),
//     evaluated separately after the kernel: an owner keeps access to their
//     own user-scoped template even once the hub-member grant no longer
//     applies to it here.
//
// templateScope is the template's own Scope value (store.TemplateScope*),
// taken from Resource.ScopeKind. This fails closed, mirroring
// filterHubWideSkillGrants: only "global" is exempt. Project, user, an
// empty string, and any future or unrecognized value are all filtered like
// project/user. Every ad hoc "template" Resource must be built through
// templateResource or templateScopeResource (never a bare literal) so
// ScopeKind is always set correctly — see
// TestTemplateResourceLiterals_AllUseCanonicalConstructor.
func filterHubWideTemplateGrants(candidates []CandidateBinding, roleDefs map[string]*RolePermissions, templateScope string) []CandidateBinding {
	if len(candidates) == 0 {
		return candidates
	}
	if templateScope == store.TemplateScopeGlobal {
		return candidates
	}

	filtered := make([]CandidateBinding, 0, len(candidates))
	for _, cb := range candidates {
		if cb.ScopeType == ScopeTypeSystem {
			if role := roleDefs[cb.RoleDefinitionID]; role != nil {
				if _, curated := curatedTemplateDirectoryRoles[role.RoleName]; curated {
					continue
				}
			}
		}
		filtered = append(filtered, cb)
	}
	return filtered
}
