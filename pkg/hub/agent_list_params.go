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
	"net/http"
	"net/url"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
)

// agentListParams is the parsed, validated sorted-mode query shared by the
// global and project agent list endpoints: sort, dir, limit, fit, stats and
// cursor. Both endpoints validate identically, so there is exactly one place
// that can get a 400 condition wrong between them.
type agentListParams struct {
	sort   string
	dir    string
	limit  int
	fit    int // 0 means "fit" was not supplied
	hasFit bool
	stats  bool
	cursor string
	// view is agentListViewFull or agentListViewCompact. It selects only
	// the per-item response shape, so it is not part of the cursor
	// binding: a cursor works across views.
	view string
}

// maxSortedLimit is the sorted-mode page size ceiling: limit stays in
// 1..500, unchanged from the legacy endpoint's own range. The legacy path
// gets this for free from
// the store's own clamp (entadapter/agent_store.go's maxAgentListLimit);
// sorted mode slices or pages explicitly, so it must clamp for itself or an
// unbounded limit lets a caller multiply the per-page decision cost (7 or 8
// actions per page item, depending on endpoint) without bound.
const maxSortedLimit = 500

// parseSortAndDir validates the sort and dir parameters of a sorted-mode
// request. dir is checked before sort, so a request with both invalid
// reports "invalid dir". It writes the 400 response itself on failure.
func parseSortAndDir(w http.ResponseWriter, query url.Values) (sort, dir string, ok bool) {
	dir = query.Get("dir")
	if dir == "" {
		dir = agentsort.Desc
	}
	if dir != agentsort.Asc && dir != agentsort.Desc {
		BadRequest(w, "invalid dir")
		return "", "", false
	}
	sort = query.Get("sort")
	if sort != agentsort.Created && sort != agentsort.Updated {
		BadRequest(w, "invalid sort")
		return "", "", false
	}
	return sort, dir, true
}

// parseAgentListParams validates every sorted-mode parameter for a request
// that has already been identified as sorted mode (query.Get("sort") != ""):
// sort and dir first (parseSortAndDir), then the rest
// (parseAgentListParamsAfterSortDir). It writes the 400 response itself on
// failure so callers can just check the returned ok.
func parseAgentListParams(w http.ResponseWriter, query url.Values, limit int) (agentListParams, bool) {
	sort, dir, ok := parseSortAndDir(w, query)
	if !ok {
		return agentListParams{}, false
	}
	return parseAgentListParamsAfterSortDir(w, query, limit, sort, dir)
}

// parseAgentListParamsAfterSortDir validates the sorted-mode parameters
// other than sort and dir, for a caller that has already validated those
// with parseSortAndDir and passes the result in, so they are parsed exactly
// once per request. filter/limit parsing shared with legacy mode happens in
// the caller; this only clamps limit and validates fit, stats, view and
// the fit/cursor exclusion. It writes the 400 response itself on failure.
//
// sort accepts both "created" and "updated" on both endpoints.
func parseAgentListParamsAfterSortDir(w http.ResponseWriter, query url.Values, limit int, sort, dir string) (agentListParams, bool) {
	// Clamp first, before the fit>=limit check below, so clamping never
	// turns a request that would have been valid at the clamped value into
	// a 400: the limit range is unchanged semantics, i.e. a silent
	// clamp, not an error, matching the legacy store path.
	if limit > maxSortedLimit {
		limit = maxSortedLimit
	}
	p := agentListParams{sort: sort, dir: dir, limit: limit}

	p.cursor = query.Get("cursor")
	if fitStr := query.Get("fit"); fitStr != "" {
		if p.cursor != "" {
			BadRequest(w, "fit is not valid together with cursor")
			return p, false
		}
		parsed, err := strconv.Atoi(fitStr)
		if err != nil || parsed < 1 || parsed > 500 {
			BadRequest(w, "invalid fit")
			return p, false
		}
		if parsed < p.limit {
			BadRequest(w, "fit must be at least limit")
			return p, false
		}
		p.fit = parsed
		p.hasFit = true
	}

	p.stats = query.Get("stats") == "1"

	view, ok := parseSortedAgentListView(w, query)
	if !ok {
		return p, false
	}
	p.view = view
	return p, true
}

// agentListLimit parses the global agents endpoint's limit parameter: a
// positive integer, or the default of 500 when absent or not a positive
// integer. It never fails; sorted mode clamps the result to maxSortedLimit.
func agentListLimit(query url.Values) int {
	limit := 500
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	return limit
}
