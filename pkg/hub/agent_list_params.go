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
	"strings"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
	// ids, when non-nil, narrows the request to these agent ids (the
	// ids= parameter, see parseAgentListIDs). It is applied as
	// store.AgentFilter.IDs, ANDed with every other filter, before the
	// per-row read pass, so it can only narrow the result.
	ids []string
}

// maxAgentListIDs bounds the ids= parameter regardless of limit: the web
// client asks for one page of a frozen walk order, and its largest page
// size is 100. Keeping a request well under the paged branch's decision
// budget also means an ids request is always answered in one page.
const maxAgentListIDs = 100

// parseAgentListIDs validates the ids= parameter: a comma-separated list
// of canonical agent UUIDs, at most min(limit, 100) entries, duplicates
// included (duplicates are then dropped), never together with cursor or
// fit. It only checks the format of the request, before any store or
// authorization call, and its error messages never name an id, so a 400
// says nothing about whether any id exists or is readable. An absent or
// empty ids= returns nil.
//
// The web client uses it to page a frozen walk order: page 0 returns the
// readable population in sort order (stats=1), and later pages ask for the
// next slice of those ids, so an agent whose sort key changes mid-walk is
// neither skipped nor repeated (ptone/scion#3744).
func parseAgentListIDs(query url.Values, limit int) ([]string, string) {
	raw := query.Get("ids")
	if raw == "" {
		return nil, ""
	}
	if query.Get("cursor") != "" {
		return nil, "ids is not valid together with cursor"
	}
	if query.Get("fit") != "" {
		return nil, "ids is not valid together with fit"
	}
	maxIDs := maxAgentListIDs
	if limit > maxSortedLimit {
		limit = maxSortedLimit
	}
	if limit > 0 && limit < maxIDs {
		maxIDs = limit
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxIDs {
		return nil, "too many ids"
	}
	seen := make(map[string]struct{}, len(parts))
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		parsed, err := uuid.Parse(part)
		if err != nil || parsed.String() != part {
			return nil, "invalid ids"
		}
		if _, dup := seen[part]; dup {
			continue
		}
		seen[part] = struct{}{}
		ids = append(ids, part)
	}
	return ids, ""
}

// narrowFilterByIDs applies the ids= set to filter as one more ANDed
// restriction: when filter.IDs is already set (the global endpoint's id=
// filter), the result is the intersection, which may be a non-nil empty
// slice that matches nothing; otherwise it is ids itself. ids never widens
// the filter.
func narrowFilterByIDs(filter *store.AgentFilter, ids []string) {
	if ids == nil {
		return
	}
	if filter.IDs == nil {
		filter.IDs = append([]string{}, ids...)
		return
	}
	existing := make(map[string]struct{}, len(filter.IDs))
	for _, id := range filter.IDs {
		existing[id] = struct{}{}
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := existing[id]; ok {
			out = append(out, id)
		}
	}
	filter.IDs = out
}

// validateAgentListIDs runs parseAgentListIDs for a list request before any
// store or authorization call, writing the 400 itself. ids is only valid
// in sorted mode; the legacy (unsorted) pages reject it rather than
// silently ignoring it.
func validateAgentListIDs(w http.ResponseWriter, query url.Values, limit int, sorted bool) bool {
	if query.Get("ids") == "" {
		return true
	}
	if !sorted {
		BadRequest(w, "ids requires sort")
		return false
	}
	if _, msg := parseAgentListIDs(query, limit); msg != "" {
		BadRequest(w, msg)
		return false
	}
	return true
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

	ids, msg := parseAgentListIDs(query, limit)
	if msg != "" {
		BadRequest(w, msg)
		return p, false
	}
	p.ids = ids

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
