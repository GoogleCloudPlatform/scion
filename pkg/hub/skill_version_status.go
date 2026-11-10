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

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Skill version status filter (ptone/scion#6297).
//
// Reads that address a skill version directly (a file read by version
// number, the single-version details, the version list) consider only
// published and deprecated versions, the same set that semver and
// content-hash resolution use (store.ResolveSkillVersion). A caller who can
// edit the skill sees its versions in every status, so a draft stays
// readable while it is being prepared, and an archived version stays
// readable to the people who retired it.
//
// A version outside the filter is reported exactly as a missing one.

// skillVersionListedStatuses are the version statuses every reader of a
// skill sees.
var skillVersionListedStatuses = []string{
	store.SkillVersionStatusPublished,
	store.SkillVersionStatusDeprecated,
}

// skillVersionStatusListed reports whether status is in
// skillVersionListedStatuses.
func skillVersionStatusListed(status string) bool {
	for _, s := range skillVersionListedStatuses {
		if status == s {
			return true
		}
	}
	return false
}

// skillVersionsAllStatuses reports whether identity sees the versions of
// skill in every status: the skill's owner, its creator, or a caller with
// update access to the skill.
func (s *Server) skillVersionsAllStatuses(ctx context.Context, identity Identity, skill *store.Skill) bool {
	if identity == nil || skill == nil {
		return false
	}
	if id := identity.ID(); id != "" && (id == skill.OwnerID || id == skill.CreatedBy) {
		return true
	}
	return s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionUpdate).Allowed
}

// skillVersionVisible reports whether identity may read sv, a version of
// skill, through a version-addressed read. It runs the authorization check
// only for versions outside the listed statuses.
func (s *Server) skillVersionVisible(ctx context.Context, identity Identity, skill *store.Skill, sv *store.SkillVersion) bool {
	if sv == nil {
		return false
	}
	if skillVersionStatusListed(sv.Status) {
		return true
	}
	return s.skillVersionsAllStatuses(ctx, identity, skill)
}
