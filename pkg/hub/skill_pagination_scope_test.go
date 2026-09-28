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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#1901 pagination follow-up (uat PG-pagination-default50).
//
// store.ListSkills applied the default LIMIT (50, newest first) before the
// per-row scope/capability filter, and set totalCount from the already-
// truncated, post-filter slice. A caller with more than 50 newer
// out-of-scope skills in the system got an empty page, no nextCursor, and
// their own (and the global catalog's) skills vanished — because the LIMIT
// picked the newest 50 rows hub-wide, the capability filter then discarded
// nearly all of them, and nothing told the caller a further page existed.
//
// The fix pushes the scope predicate into the store query itself, ahead of
// COUNT and LIMIT/cursor, and implements real keyset pagination so a caller
// can walk every visible row via nextCursor regardless of how much
// out-of-scope noise exists. These tests seed more out-of-scope skills than
// the default page size and must fail against ac8fc87a6 (the pre-follow-up
// commit), which has neither the pushed-down predicate nor a working
// cursor.
// ============================================================================

// decodeSkillsPage performs one GET against path as user and decodes the
// ListSkillsResponse.
func decodeSkillsPage(t *testing.T, srv *Server, user *store.User, path string) ListSkillsResponse {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}

// skillIDSet extracts the set of skill IDs from a page of results.
func skillIDSet(skills []SkillWithCapabilities) map[string]bool {
	set := make(map[string]bool, len(skills))
	for _, sk := range skills {
		set[sk.ID] = true
	}
	return set
}

// TestSkillListPagination_OwnerAndGlobalSurviveOutOfScopeNoise is the direct
// INFO-1-pagination regression test: alice's own user-scoped skill and a
// hub-scoped (global) skill must both appear on page 1 for alice, even
// though many more out-of-scope (carol's private user-scoped) skills were
// created more recently and would otherwise fill the newest-first LIMIT
// window before the scope boundary is ever applied.
func TestSkillListPagination_OwnerAndGlobalSurviveOutOfScopeNoise(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)

	aliceSkill := createTestSkill(t, s, "alice-pagination-own", store.SkillScopeUser, alice.ID, alice.ID)
	globalSkill := createTestSkill(t, s, "pagination-global-catalog", store.SkillScopeGlobal, "", alice.ID)

	// More out-of-scope noise than the default page size (50), created
	// after alice's and the global skill so it sorts first (newest-first).
	const noiseCount = 60
	noiseIDs := make(map[string]bool, noiseCount)
	for i := 0; i < noiseCount; i++ {
		sk := createTestSkill(t, s, fmt.Sprintf("carol-noise-%03d", i), store.SkillScopeUser, carol.ID, carol.ID)
		noiseIDs[sk.ID] = true
	}

	resp := decodeSkillsPage(t, srv, alice, "/api/v1/skills?status=active")

	ids := skillIDSet(resp.Skills)
	assert.True(t, ids[aliceSkill.ID], "alice's own user-scoped skill must survive pagination past carol's noise")
	assert.True(t, ids[globalSkill.ID], "the hub-scoped skill must survive pagination past carol's noise")
	for id := range noiseIDs {
		assert.False(t, ids[id], "carol's out-of-scope skill %s must not appear in alice's page", id)
	}
	assert.Equal(t, 2, resp.TotalCount, "totalCount must reflect only what alice can see, not the noise")
}

// TestSkillListPagination_ScopeUserFilterWithoutScopeIDReturnsOwner covers
// roadmap-lead's point 3: "?scope=user" (with no explicit scopeId) must
// still surface the caller's own skills once out-of-scope noise exceeds a
// page, because the access-scope predicate — not just the scope=user query
// filter — is what narrows the candidate set down to the caller.
func TestSkillListPagination_ScopeUserFilterWithoutScopeIDReturnsOwner(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)

	aliceSkill := createTestSkill(t, s, "alice-scope-user-own", store.SkillScopeUser, alice.ID, alice.ID)

	const noiseCount = 60
	for i := 0; i < noiseCount; i++ {
		createTestSkill(t, s, fmt.Sprintf("carol-scope-noise-%03d", i), store.SkillScopeUser, carol.ID, carol.ID)
	}

	resp := decodeSkillsPage(t, srv, alice, "/api/v1/skills?status=active&scope=user")

	ids := skillIDSet(resp.Skills)
	assert.True(t, ids[aliceSkill.ID], "?scope=user without scopeId must still return the caller's own skill")
	assert.Equal(t, 1, resp.TotalCount)
}

// TestSkillListPagination_CursorWalkReachesAllVisibleNoneHidden seeds more
// project-scoped skills than fit in one small page, plus more out-of-scope
// noise (a different project's skills) than the default page size, and
// walks nextCursor to completion. The visited set must equal exactly the
// project member's visible skills: every one reached, none of the hidden
// ones, no duplicates.
func TestSkillListPagination_CursorWalkReachesAllVisibleNoneHidden(t *testing.T) {
	srv, s, alice, _, project := setupSkillScopeTest(t)

	dave := createNamedTestUser(t, s, "pagination-dave", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, dave.ID)
	createTestUserWithProjectRole(t, s, dave.ID, dave.Email, project.ID, store.ProjectRoleMember)

	const visibleCount = 7
	visible := make(map[string]bool, visibleCount)
	for i := 0; i < visibleCount; i++ {
		sk := createTestSkill(t, s, fmt.Sprintf("dave-visible-%02d", i), store.SkillScopeProject, project.ID, alice.ID)
		visible[sk.ID] = true
	}

	otherProject := &store.Project{ID: tid("pagination-other-project"), Name: "Other Project", Slug: tid("pagination-other-project")}
	require.NoError(t, s.CreateProject(context.Background(), otherProject))

	const noiseCount = 55
	hidden := make(map[string]bool, noiseCount)
	for i := 0; i < noiseCount; i++ {
		sk := createTestSkill(t, s, fmt.Sprintf("other-project-noise-%03d", i), store.SkillScopeProject, otherProject.ID, alice.ID)
		hidden[sk.ID] = true
	}

	seen := make(map[string]bool, visibleCount)
	cursor := ""
	pages := 0
	var totalCount int
	for {
		pages++
		require.LessOrEqual(t, pages, visibleCount+1, "cursor walk did not terminate in a reasonable number of pages")

		q := url.Values{"status": {"active"}, "limit": {"3"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		resp := decodeSkillsPage(t, srv, dave, "/api/v1/skills?"+q.Encode())
		if pages == 1 {
			totalCount = resp.TotalCount
		}

		for _, sk := range resp.Skills {
			require.False(t, hidden[sk.ID], "dave must never see another project's skill: %s", sk.Name)
			if visible[sk.ID] {
				assert.False(t, seen[sk.ID], "duplicate skill %s across pages", sk.Name)
				seen[sk.ID] = true
			}
		}

		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}

	assert.Len(t, seen, visibleCount, "cursor walk must reach every visible skill exactly once")
	assert.Equal(t, visibleCount, totalCount, "totalCount must equal the visible count, not the unfiltered total")
}

// TestSkillListPagination_VaryingLimitIsNotACountOracle closes the uat C21-23
// / F1 "count oracle" side effect: with the LIMIT applied before the scope
// filter, varying ?limit changed both which items and how many showed up on
// page 1 (e.g. limit=2 → 1 visible, limit=3 → 2 visible), letting a caller
// infer how many hidden out-of-scope rows sit between their own by probing
// different limits. Walking the full cursor to completion at any limit must
// yield the exact same visible set and the exact same totalCount — neither
// may depend on the page size.
func TestSkillListPagination_VaryingLimitIsNotACountOracle(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)

	aliceSkill := createTestSkill(t, s, "alice-count-oracle-own", store.SkillScopeUser, alice.ID, alice.ID)
	globalSkill := createTestSkill(t, s, "count-oracle-global-catalog", store.SkillScopeGlobal, "", alice.ID)
	want := map[string]bool{aliceSkill.ID: true, globalSkill.ID: true}

	const noiseCount = 55
	for i := 0; i < noiseCount; i++ {
		createTestSkill(t, s, fmt.Sprintf("carol-count-oracle-noise-%03d", i), store.SkillScopeUser, carol.ID, carol.ID)
	}

	for _, limit := range []int{1, 2, 3, 5, 10, 50, 200} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			seen := make(map[string]bool, len(want))
			cursor := ""
			var totalCount int
			for pages := 0; ; pages++ {
				require.LessOrEqual(t, pages, len(want)+noiseCount+1, "cursor walk did not terminate")

				q := url.Values{"status": {"active"}}
				q.Set("limit", fmt.Sprintf("%d", limit))
				if cursor != "" {
					q.Set("cursor", cursor)
				}
				resp := decodeSkillsPage(t, srv, alice, "/api/v1/skills?"+q.Encode())
				if pages == 0 {
					totalCount = resp.TotalCount
				}
				for _, sk := range resp.Skills {
					seen[sk.ID] = true
				}
				if resp.NextCursor == "" {
					break
				}
				cursor = resp.NextCursor
			}

			assert.Equal(t, len(want), totalCount, "totalCount must not vary with limit=%d", limit)
			assert.Len(t, seen, len(want), "the fully-walked visible set must not vary with limit=%d", limit)
			for id := range want {
				assert.True(t, seen[id], "limit=%d must still surface %s once the cursor is fully walked", limit, id)
			}
		})
	}
}
