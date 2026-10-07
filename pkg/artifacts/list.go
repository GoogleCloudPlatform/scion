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

package artifacts

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// List endpoint limits.
const (
	// DefaultListLimit is the page size when ?limit= is absent.
	DefaultListLimit = 50
	// MaxListLimit is the largest page size a caller may ask for.
	MaxListLimit = 100
	// maxSearchLength bounds ?q= in characters.
	maxSearchLength = 200
)

// maxListScan caps the candidate rows one request examines, readable or
// not, so a caller whose candidates are mostly unreadable (for example a
// token bounded to one project) cannot make a request scan without bound.
// When it is reached the page may be short, and the cursor resumes after
// the last row examined; the host seals the cursor, so that position is
// not revealed. A variable so tests can lower it.
var maxListScan = 500

// ArtifactListItem is one row of the list response: the artifact and
// whether its current version is a review awaiting the owner.
type ArtifactListItem struct {
	ArtifactInfo
	ReviewPending bool `json:"reviewPending"`
}

// ArtifactListResponse is the body of GET /api/v1/artifacts?mine=1.
type ArtifactListResponse struct {
	Artifacts []ArtifactListItem `json:"artifacts"`
	// NextCursor resumes the walk; absent on the last page. It is opaque.
	NextCursor string `json:"nextCursor,omitempty"`
}

// listParams is a validated list request.
type listParams struct {
	search        string
	reviewPending bool
	ownedOnly     bool
	limit         int
	cursor        string
}

// binding is the normalized query a cursor is bound to. The page size is
// left out: changing it mid-walk is harmless.
func (p listParams) binding() string {
	v := url.Values{}
	v.Set("q", p.search)
	v.Set("review_pending", strconv.FormatBool(p.reviewPending))
	v.Set("owned", strconv.FormatBool(p.ownedOnly))
	return v.Encode()
}

func parseListParams(q url.Values) (listParams, string) {
	p := listParams{limit: DefaultListLimit}
	switch q.Get("mine") {
	case "1", "true":
	default:
		return p, "mine=1 is required; only the caller's own and shared artifacts can be listed"
	}
	p.search = strings.TrimSpace(q.Get("q"))
	if !utf8.ValidString(p.search) || strings.ContainsRune(p.search, 0) {
		return p, "q must be valid text"
	}
	if utf8.RuneCountInString(p.search) > maxSearchLength {
		return p, "q must be at most " + strconv.Itoa(maxSearchLength) + " characters"
	}
	switch q.Get("review_pending") {
	case "", "0", "false":
	case "1", "true":
		p.reviewPending = true
	default:
		return p, "review_pending must be 1 or 0"
	}
	switch q.Get("owner") {
	case "":
	case "me":
		p.ownedOnly = true
	default:
		return p, "owner must be me"
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxListLimit {
			return p, "limit must be between 1 and " + strconv.Itoa(MaxListLimit)
		}
		p.limit = n
	}
	p.cursor = q.Get("cursor")
	return p, ""
}

// handleList serves GET /api/v1/artifacts?mine=1: the artifacts the caller
// owns, or that a grant to the caller or to one of its projects lets it
// read. Each candidate passes the same check a GET of that artifact runs
// (canRead), so the list never shows an artifact the caller could not open,
// and a caller the host does not serve gets an empty list.
func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	p, msg := parseListParams(r.URL.Query())
	if msg != "" {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	b, ok := s.backend()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "artifact storage is not configured")
		return
	}
	ctx := r.Context()
	host := newMemoHost(s.host)
	kind, ref, _, ok := host.Principal(ctx)
	if !ok {
		writeJSON(w, http.StatusOK, ArtifactListResponse{Artifacts: []ArtifactListItem{}})
		return
	}

	var after *Position
	if p.cursor != "" {
		pos, err := host.OpenCursor(ctx, p.cursor, p.binding())
		if err == nil {
			after, err = parsePosition(pos)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "invalid cursor")
			return
		}
	}

	var scopes []string
	if !p.ownedOnly {
		var err error
		scopes, err = host.MemberScopes(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: member scopes failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not list artifacts")
			return
		}
		scopes = candidateScopes(ctx, scopes)
	}

	items, next, err := s.collect(ctx, host, b, CandidateQuery{
		PrincipalKind: kind, PrincipalRef: ref, ScopeRefs: scopes, OwnedOnly: p.ownedOnly,
		Search: p.search, ReviewPending: p.reviewPending, After: after,
	}, p.limit)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not list artifacts")
		return
	}
	resp := ArtifactListResponse{Artifacts: items}
	if next != nil {
		cursor, err := host.SealCursor(ctx, formatPosition(*next), p.binding())
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: seal cursor failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not list artifacts")
			return
		}
		resp.NextCursor = cursor
	}
	writeJSON(w, http.StatusOK, resp)
}

// collect fetches up to maxListScan candidates in store order with one
// store query, and keeps those canReadWith allows until it has a full page.
// next is where the following page starts: after the last returned row when
// the page is full, after the last examined row when the scan budget ran
// out first (the host seals it, so that position is not revealed), and nil
// at the end.
//
// The whole budget is fetched at once because the rows that fill the page
// may come after any number of rows the caller cannot read; fetching only
// the page remainder would cost one store query per unreadable row or two.
// Rows fetched past a full page are dropped.
func (s *Service) collect(ctx context.Context, host Host, b backend, q CandidateQuery, limit int) ([]ArtifactListItem, *Position, error) {
	q.Limit = maxListScan
	q.Now = time.Now()
	rows, err := b.store.ListCandidates(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	grants := batchGrants(ctx, b.store, rows)
	items := []ArtifactListItem{}
	var last *Position
	for i := range rows {
		c := &rows[i]
		last = &Position{UpdatedAt: c.UpdatedAt, ID: c.ID}
		if !canReadWith(ctx, host, &c.Artifact, func() ([]Grant, error) { return grants(c.ID) }) {
			continue
		}
		items = append(items, ArtifactListItem{
			ArtifactInfo:  artifactInfo(&c.Artifact),
			ReviewPending: c.CurrentKind == VersionKindReview,
		})
		if len(items) > limit {
			items = items[:limit]
			end := items[limit-1]
			return items, &Position{UpdatedAt: end.UpdatedAt, ID: end.ID}, nil
		}
	}
	if len(rows) < q.Limit {
		return items, nil, nil
	}
	return items, last, nil
}

// batchGrants returns a loader for the grants of one candidate. The first
// call reads the grants of every candidate in rows with one store query;
// later calls are answered from that result. If no row needs its grants
// (every row owned, or authorized in its home scope) nothing is read.
func batchGrants(ctx context.Context, st Store, rows []Candidate) func(id string) ([]Grant, error) {
	var (
		loaded bool
		byID   map[string][]Grant
		err    error
	)
	return func(id string) ([]Grant, error) {
		if !loaded {
			loaded = true
			ids := make([]string, len(rows))
			for i := range rows {
				ids[i] = rows[i].ID
			}
			byID, err = st.ListGrantsFor(ctx, ids)
		}
		return byID[id], err
	}
}

// truncatedScopesOnce limits the truncation warning to one per process, so
// a caller in very many projects does not log on every request.
var truncatedScopesOnce sync.Once

// candidateScopes sorts and deduplicates scope refs and caps them at what
// one store query binds. Dropping scopes only loses completeness: a
// dropped scope's artifacts are still readable by GET.
func candidateScopes(ctx context.Context, scopes []string) []string {
	seen := make(map[string]bool, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		if sc != "" && !seen[sc] {
			seen[sc] = true
			out = append(out, sc)
		}
	}
	sort.Strings(out)
	if len(out) > maxCandidateScopes {
		truncatedScopesOnce.Do(func() {
			slog.WarnContext(ctx, "artifacts: list truncated member scopes (logged once per process)", "scopes", len(out), "max", maxCandidateScopes)
		})
		out = out[:maxCandidateScopes]
	}
	return out
}

// formatPosition encodes a position as "<RFC 3339 time>,<id>". It is
// sealed by the host before it reaches a client.
func formatPosition(p Position) string {
	return p.UpdatedAt.UTC().Format(time.RFC3339Nano) + "," + p.ID
}

func parsePosition(v string) (*Position, error) {
	ts, id, ok := strings.Cut(v, ",")
	if !ok || !canonicalID(id) {
		return nil, ErrBadRef
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, err
	}
	return &Position{UpdatedAt: t.UTC(), ID: id}, nil
}

// memoHost remembers a Host's answers for one request, so a page of
// artifacts homed in a few projects costs a few authorization checks, not
// one per row. Answers do not outlive the request.
type memoHost struct {
	Host
	principal  *memoPrincipal
	permits    map[[2]string]bool
	authorized map[[2]string]bool
}

type memoPrincipal struct {
	kind, ref, home string
	ok              bool
}

func newMemoHost(h Host) *memoHost {
	return &memoHost{Host: h, permits: map[[2]string]bool{}, authorized: map[[2]string]bool{}}
}

func (m *memoHost) Principal(ctx context.Context) (string, string, string, bool) {
	if m.principal == nil {
		k, r, h, ok := m.Host.Principal(ctx)
		m.principal = &memoPrincipal{k, r, h, ok}
	}
	return m.principal.kind, m.principal.ref, m.principal.home, m.principal.ok
}

func (m *memoHost) Permits(ctx context.Context, scopeRef, permission string) bool {
	k := [2]string{scopeRef, permission}
	v, ok := m.permits[k]
	if !ok {
		v = m.Host.Permits(ctx, scopeRef, permission)
		m.permits[k] = v
	}
	return v
}

func (m *memoHost) Authorize(ctx context.Context, scopeRef, permission string) bool {
	k := [2]string{scopeRef, permission}
	v, ok := m.authorized[k]
	if !ok {
		v = m.Host.Authorize(ctx, scopeRef, permission)
		m.authorized[k] = v
	}
	return v
}
