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
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#1916 follow-up (C3): authorizedList's bounded per-resource scan
// (authorized_list.go) is exact — a real total and a real page from
// cursor-paginated store batches — but authorizedListMaxCandidates used to
// abort that scan with a hard 503 once the hub-wide candidate count (not the
// caller's own scoped count) crossed the cap. A non-admin member could trip
// this for every other non-admin caller merely by owning enough private
// templates/harness-configs. This suite pins the interim fix: a caller whose
// own accessible item sits behind more hidden candidates than the cap must
// still reach it — over as many paged requests as it takes — with a correct
// (or explicitly flagged-approximate) total and never a leaked hidden row.
//
// The list is created oldest-first (visible item, then hidden ones) so that
// the store's newest-first ordering puts every hidden candidate ahead of the
// visible one in scan order — the worst case for a bounded scan, and the one
// that actually exercises the resume-cursor path across multiple requests.
// ============================================================================

const authorizedListScaleHiddenCount = authorizedListMaxCandidates + 200

func TestAuthorizedList_Template_AboveCandidateCap_PagesToVisibleItem(t *testing.T) {
	srv, s, alice, carol, project := setupTemplateScopeTest(t)

	// Visible: created first, so it sorts last (newest-first ordering) and
	// the scan must page through every hidden candidate to reach it.
	visible := createAuthzTestTemplate(t, s, "scale-visible-tmpl", store.TemplateScopeUser, carol.ID, carol.ID)

	// Hidden: private templates in alice's project, out of carol's scope.
	// More of them, hub-wide, than authorizedListMaxCandidates.
	for i := 0; i < authorizedListScaleHiddenCount; i++ {
		createAuthzTestTemplate(t, s, fmt.Sprintf("scale-hidden-tmpl-%d", i), store.TemplateScopeProject, project.ID, alice.ID)
	}

	found, sawApproximate, pages := paginateTemplatesUntilFound(t, srv, carol, visible.ID)
	require.True(t, found, "carol's own visible template must eventually surface via cursor paging, not get lost behind the hidden pool")
	assert.Greater(t, pages, 1, "the visible item sits behind more hidden candidates than one page's scan budget can cover, so this must take more than one request")
	// The total is allowed to be exact or approximate (never a hard failure);
	// given the hidden pool exceeds the cap, the count pass is expected to
	// report it approximate — assert that rather than a specific number.
	assert.True(t, sawApproximate, "with a hub-wide pool larger than the cap, the total should be flagged approximate rather than silently wrong")
}

func TestAuthorizedList_HarnessConfig_AboveCandidateCap_PagesToVisibleItem(t *testing.T) {
	srv, s, alice, carol, project := setupHarnessConfigScopeTest(t)

	visible := createAuthzTestHarnessConfig(t, s, "scale-visible-hc", store.HarnessConfigScopeUser, carol.ID, carol.ID)

	for i := 0; i < authorizedListScaleHiddenCount; i++ {
		createAuthzTestHarnessConfig(t, s, fmt.Sprintf("scale-hidden-hc-%d", i), store.HarnessConfigScopeProject, project.ID, alice.ID)
	}

	found, sawApproximate, pages := paginateHarnessConfigsUntilFound(t, srv, carol, visible.ID)
	require.True(t, found, "carol's own visible harness config must eventually surface via cursor paging, not get lost behind the hidden pool")
	assert.Greater(t, pages, 1, "the visible item sits behind more hidden candidates than one page's scan budget can cover, so this must take more than one request")
	assert.True(t, sawApproximate, "with a hub-wide pool larger than the cap, the total should be flagged approximate rather than silently wrong")
}

// paginateTemplatesUntilFound walks /api/v1/templates as user, following
// NextCursor, until target's ID appears or the list is exhausted. It fails
// the test immediately if any response is not 200, or if any hidden
// ("scale-hidden-tmpl") template ever appears in a page. Returns whether the
// target was found, whether any page's total was flagged approximate, and
// how many requests (pages) it took.
func paginateTemplatesUntilFound(t *testing.T, srv *Server, user *store.User, targetID string) (found, sawApproximate bool, pages int) {
	t.Helper()
	cursor := ""
	for {
		path := "/api/v1/templates?limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := doRequestAsUser(t, srv, user, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "page %d: got: %s", pages+1, rec.Body.String())
		pages++

		var resp ListTemplatesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		if resp.TotalCountApproximate {
			sawApproximate = true
		}
		for _, tpl := range resp.Templates {
			require.NotContains(t, tpl.Name, "scale-hidden", "no hidden template may leak into any page")
			if tpl.ID == targetID {
				found = true
			}
		}
		if found || resp.NextCursor == "" {
			return found, sawApproximate, pages
		}
		cursor = resp.NextCursor
		require.Less(t, pages, 50, "did not converge within a sane number of pages")
	}
}

// paginateHarnessConfigsUntilFound is the harness-config twin of
// paginateTemplatesUntilFound.
func paginateHarnessConfigsUntilFound(t *testing.T, srv *Server, user *store.User, targetID string) (found, sawApproximate bool, pages int) {
	t.Helper()
	cursor := ""
	for {
		path := "/api/v1/harness-configs?limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := doRequestAsUser(t, srv, user, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "page %d: got: %s", pages+1, rec.Body.String())
		pages++

		var resp ListHarnessConfigsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		if resp.TotalCountApproximate {
			sawApproximate = true
		}
		for _, hc := range resp.HarnessConfigs {
			require.NotContains(t, hc.Name, "scale-hidden", "no hidden harness config may leak into any page")
			if hc.ID == targetID {
				found = true
			}
		}
		if found || resp.NextCursor == "" {
			return found, sawApproximate, pages
		}
		cursor = resp.NextCursor
		require.Less(t, pages, 50, "did not converge within a sane number of pages")
	}
}
