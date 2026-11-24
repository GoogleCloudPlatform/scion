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
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery/schemas"
)

// ErrBackendUnavailable is returned by Service methods called directly in Go
// when the artifact store or storage backend is not configured.
var ErrBackendUnavailable = errors.New("artifacts: storage is not configured")

// List endpoint limits.
const (
	// DefaultListLimit is the page size when ?limit= is absent.
	DefaultListLimit = 50
	// MaxListLimit is the largest page size a caller may ask for.
	MaxListLimit = 100
	// maxSearchLength bounds ?q= in characters.
	maxSearchLength = 200
	// maxScopeLength bounds ?scope= in bytes.
	maxScopeLength = 256
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
	// Access is why the caller sees the artifact: AccessOwned,
	// AccessProject (it may read artifacts homed in the artifact's
	// project) or AccessShared (a grant to it or to one of its projects).
	Access string `json:"access"`
	// SharedWithScope is true, in a list narrowed to a project with
	// scope=, for an artifact homed elsewhere and shared with that
	// project.
	SharedWithScope bool `json:"sharedWithScope,omitempty"`
	// ScopeDeleted is true when the artifact's home project no longer
	// exists (deleting a project never deletes its artifacts).
	ScopeDeleted bool `json:"scopeDeleted,omitempty"`
	// CanManage is set only on rows with ScopeDeleted: true when the
	// caller may move the artifact to another project (see canAdministerWith)
	// and moves are turned on (cross-project sharing).
	CanManage bool `json:"canManage,omitempty"`

	// src is the stored artifact of the row.
	src *Artifact
}

// List item access values.
const (
	AccessOwned   = "owned"
	AccessProject = "project"
	AccessShared  = "shared"
)

// ArtifactListResponse is the body of GET /api/v1/artifacts?mine=1.
type ArtifactListResponse struct {
	Artifacts []ArtifactListItem `json:"artifacts"`
	// NextCursor resumes the walk; absent on the last page. It is opaque.
	NextCursor string `json:"nextCursor,omitempty"`
	// CanCreate, on a list narrowed to a project with scope=, is true when
	// the caller may publish artifacts in that project: the decision a
	// publish there makes (canCreateIn). It is absent without scope=.
	CanCreate bool `json:"canCreate,omitempty"`
}

// listParams is a validated list request.
type listParams struct {
	search        string
	rawQuery      string
	canonicalQ    string
	dialect       bool
	reviewPending bool
	ownedOnly     bool
	scope         string
	sharedOnly    bool
	limit         int
	cursor        string
}

// invalidCursorMessage is the message of the 400 invalid_cursor answer for
// a list cursor that cannot be used. It matches apierr.InvalidCursorMessage
// (pkg/hub/apierr), so every hub list answers a bad cursor the same way
// (ptone/scion#3062); this package does not import pkg/hub/apierr.
const invalidCursorMessage = "invalid cursor: restart pagination from the first page"

// impossibleScopeID is substituted when a `project:` slug cannot be resolved
// within the caller's authorized scope, so unknown and unauthorized slugs are
// externally indistinguishable (empty 200, never 400 or 403).
const impossibleScopeID = "00000000-0000-0000-0000-000000000000"

// binding is the normalized query a cursor is bound to. The page size is
// left out: changing it mid-walk is harmless. When the filter query dialect is
// enabled, q is bound to the server-computed filterquery.Canonical(ast).
func (p listParams) binding() string {
	v := url.Values{}
	if p.dialect {
		v.Set("q", p.canonicalQ)
	} else {
		v.Set("q", p.search)
	}
	v.Set("review_pending", strconv.FormatBool(p.reviewPending))
	v.Set("owned", strconv.FormatBool(p.ownedOnly))
	if p.scope != "" {
		v.Set("scope", p.scope)
	}
	if p.sharedOnly {
		v.Set("shared", "true")
	}
	return v.Encode()
}

func parseListParams(q url.Values, dialect bool) (listParams, string) {
	p := listParams{limit: DefaultListLimit, dialect: dialect}
	switch q.Get("mine") {
	case "1", "true":
	default:
		return p, "mine=1 is required; only the caller's own and shared artifacts can be listed"
	}
	rawQ := q.Get("q")
	if !utf8.ValidString(rawQ) || strings.ContainsRune(rawQ, 0) {
		return p, "q must be valid text"
	}
	if dialect {
		p.rawQuery = rawQ
	} else {
		p.search = strings.TrimSpace(rawQ)
		if utf8.RuneCountInString(p.search) > maxSearchLength {
			return p, "q must be at most " + strconv.Itoa(maxSearchLength) + " characters"
		}
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
	p.scope = q.Get("scope")
	if !utf8.ValidString(p.scope) || strings.ContainsRune(p.scope, 0) || len(p.scope) > maxScopeLength {
		return p, "scope must be a project id"
	}
	switch q.Get("shared") {
	case "", "0", "false":
	case "1", "true":
		p.sharedOnly = true
	default:
		return p, "shared must be 1 or 0"
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

func writeInvalidFilterQuery(w http.ResponseWriter, fqErr *filterquery.Error) {
	writeErrorDetails(w, http.StatusBadRequest, "invalid_filter_query", fqErr.Message, map[string]any{
		"offset": fqErr.Offset,
		"length": fqErr.Length,
	})
}

// handleList serves GET /api/v1/artifacts?mine=1: the artifacts the caller
// owns, or that a grant to the caller or to one of its projects lets it
// read. Each candidate passes the same check a GET of that artifact runs
// (canReadErr), so the list never shows an artifact the caller could not open,
// and a caller the host does not serve gets an empty list.
func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dialect := s.isFilterQueryDialectEnabled(ctx)
	p, msg := parseListParams(r.URL.Query(), dialect)
	if msg != "" {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
		return
	}

	var parsedQuery filterquery.Query
	var hasParsedQuery bool
	if dialect && p.rawQuery != "" {
		schema := schemas.Artifacts
		if p.scope != "" {
			schema = schemas.ProjectArtifacts
		}
		parsed, fqErr := filterquery.Parse(p.rawQuery, schema.HasKey)
		if fqErr != nil {
			writeInvalidFilterQuery(w, fqErr)
			return
		}
		if vErr := schema.Validate(parsed); vErr != nil {
			writeInvalidFilterQuery(w, vErr)
			return
		}
		p.canonicalQ = filterquery.Canonical(parsed)
		if !parsed.IsEmpty() {
			parsedQuery = parsed
			hasParsedQuery = true
		}
	}

	b, ok := s.backend()
	if !ok {
		writeBackendUnavailable(w, b, "artifact storage is not configured")
		return
	}
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
			writeError(w, http.StatusBadRequest, "invalid_cursor", invalidCursorMessage)
			return
		}
	}

	var queryTerms *QueryTerms
	if hasParsedQuery {
		var err error
		queryTerms, err = filterQueryToArtifactStoreTerms(ctx, s.host, parsedQuery)
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: resolve filter query failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not list artifacts")
			return
		}
	}

	ownedOnly := p.ownedOnly || queryRequiresOwnedOnly(queryTerms)

	var scopes []string
	if !ownedOnly {
		var err error
		scopes, err = host.MemberScopes(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: member scopes failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not list artifacts")
			return
		}
		scopes = candidateScopes(ctx, scopes)
	}

	// What is shared with a project is shown only to a caller that may read
	// in it. Otherwise scope= narrows to artifacts homed there, and asking
	// for only the shared ones gives an empty list.
	scopeShares := p.scope != "" && host.Authorize(ctx, p.scope, PermissionRead)
	if p.scope != "" && !scopeShares && (p.sharedOnly || queryRequiresProjectShared(queryTerms)) {
		writeJSON(w, http.StatusOK, ArtifactListResponse{Artifacts: []ArtifactListItem{}, CanCreate: canCreateIn(ctx, host, p.scope)})
		return
	}
	items, next, err := s.collect(ctx, host, b, CandidateQuery{
		PrincipalKind: kind, PrincipalRef: ref, ScopeRefs: scopes, OwnedOnly: ownedOnly,
		Search: p.search, ReviewPending: p.reviewPending, HomeScope: p.scope, ScopeShares: scopeShares,
		SharedOnly: p.sharedOnly, Query: queryTerms, After: after,
	}, p.limit)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not list artifacts")
		return
	}
	if err := s.markDeletedScopes(ctx, b, items); err != nil {
		slog.ErrorContext(ctx, "artifacts: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not list artifacts")
		return
	}
	resp := ArtifactListResponse{Artifacts: items, CanCreate: p.scope != "" && canCreateIn(ctx, host, p.scope)}
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

// grantWindowMin is the fewest candidates one grants query covers. With
// maxListScan it bounds the grants queries of one request at
// ceil(maxListScan/grantWindowMin). A variable so tests can lower it.
var grantWindowMin = 100

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
// Rows fetched past a full page are dropped. Grants are read only for rows
// that reach the grant step, a window of rows at a time (grantsWindow), so a
// page that fills early reads few grants and a long walk reads a few
// windows. A failed grants read fails the request instead of hiding rows.
func (s *Service) collect(ctx context.Context, host Host, b backend, q CandidateQuery, limit int) ([]ArtifactListItem, *Position, error) {
	q.Limit = maxListScan
	q.Now = time.Now()
	rows, err := b.store.ListCandidates(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	grants := newGrantsWindow(ctx, b.store, rows, max(2*(limit+1), grantWindowMin))
	items := []ArtifactListItem{}
	var last *Position
	for i := range rows {
		c := &rows[i]
		last = &Position{UpdatedAt: c.UpdatedAt, ID: c.ID}
		readable := canReadWith(ctx, host, &c.Artifact, func() ([]Grant, error) { return grants.forRow(i) })
		if grants.err != nil {
			return nil, nil, grants.err
		}
		if !readable {
			continue
		}
		access := listAccess(ctx, host, &c.Artifact)
		if q.SharedOnly && q.HomeScope == "" && access != AccessShared {
			continue
		}
		reviewPending := c.CurrentKind == VersionKindReview
		if q.Query != nil && q.HomeScope == "" && !matchesQueryIsTerms(q.Query, reviewPending, access) {
			continue
		}
		items = append(items, ArtifactListItem{
			ArtifactInfo:    artifactInfo(&c.Artifact),
			ReviewPending:   reviewPending,
			Access:          access,
			SharedWithScope: q.HomeScope != "" && c.ScopeRef != q.HomeScope,
			src:             &c.Artifact,
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

// filterQueryToArtifactStoreTerms translates a validated artifacts filter query
// into QueryTerms, resolving `project:` referents through ScopeResolver
// (substituting impossibleScopeID for unknown/unauthorized referents).
func filterQueryToArtifactStoreTerms(ctx context.Context, host Host, q filterquery.Query) (*QueryTerms, error) {
	if q.IsEmpty() {
		return nil, nil
	}
	qt := &QueryTerms{
		Text: q.Text,
	}
	if len(q.ExcludeText) > 0 {
		excl := append([]string(nil), q.ExcludeText...)
		sort.Strings(excl)
		qt.ExcludeText = slices.Compact(excl)
	}
	resolver, hasResolver := host.(ScopeResolver)
	if len(q.Terms) > 0 {
		terms := make([]QueryTerm, 0, len(q.Terms))
		for _, t := range q.Terms {
			key := strings.ToLower(t.Key)
			vals := make([]string, len(t.Values))
			for i, v := range t.Values {
				lower := strings.ToLower(v)
				if key == "project" {
					resolved, err := resolveProjectValue(ctx, resolver, hasResolver, lower)
					if err != nil {
						return nil, err
					}
					vals[i] = resolved
				} else {
					vals[i] = lower
				}
			}
			sort.Strings(vals)
			vals = slices.Compact(vals)
			terms = append(terms, QueryTerm{
				Field:  key,
				Values: vals,
				Negate: t.Negate,
			})
		}
		sort.Slice(terms, func(i, j int) bool {
			if terms[i].Field != terms[j].Field {
				return terms[i].Field < terms[j].Field
			}
			if terms[i].Negate != terms[j].Negate {
				return !terms[i].Negate
			}
			return strings.Join(terms[i].Values, ",") < strings.Join(terms[j].Values, ",")
		})
		qt.Terms = terms
	}
	return qt, nil
}

func resolveProjectValue(ctx context.Context, resolver ScopeResolver, hasResolver bool, val string) (string, error) {
	if canonicalID(val) {
		return val, nil
	}
	if !hasResolver {
		return impossibleScopeID, nil
	}
	id, err := resolver.ResolveScope(ctx, val)
	if err != nil {
		return "", err
	}
	if id == "" {
		return impossibleScopeID, nil
	}
	return id, nil
}

func queryRequiresOwnedOnly(qt *QueryTerms) bool {
	if qt == nil {
		return false
	}
	for _, t := range qt.Terms {
		if t.Field == "is" && !t.Negate && len(t.Values) == 1 && t.Values[0] == "mine" {
			return true
		}
	}
	return false
}

func queryRequiresProjectShared(qt *QueryTerms) bool {
	if qt == nil {
		return false
	}
	for _, t := range qt.Terms {
		if t.Field == "is" && !t.Negate && len(t.Values) == 1 && t.Values[0] == "shared" {
			return true
		}
	}
	return false
}

// matchesQueryIsTerms evaluates global-list `is:` terms against a row's
// resolved ReviewPending and Access classification (required for `is:shared`
// and `-is:shared`, since AccessShared depends on host.Authorize per home
// project).
func matchesQueryIsTerms(qt *QueryTerms, reviewPending bool, access string) bool {
	for _, t := range qt.Terms {
		if t.Field != "is" {
			continue
		}
		matched := false
		for _, v := range t.Values {
			switch v {
			case "review":
				if reviewPending {
					matched = true
				}
			case "mine":
				if access == AccessOwned {
					matched = true
				}
			case "shared":
				if access == AccessShared {
					matched = true
				}
			}
		}
		if t.Negate {
			if matched {
				return false
			}
		} else if !matched {
			return false
		}
	}
	return true
}

// grantsWindow loads the grants of candidates on demand. The first row that
// needs its grants loads the grants of that row and the next size-1 rows
// with one query; a later row past that window loads the next window. A row
// is only ever given the grants whose artifact id is its own.
type grantsWindow struct {
	ctx      context.Context
	st       Store
	rows     []Candidate
	size     int
	from, to int // rows[from:to] are loaded
	byID     map[string][]Grant
	// err is the first load failure. Once set, every call returns it.
	err error
}

func newGrantsWindow(ctx context.Context, st Store, rows []Candidate, size int) *grantsWindow {
	return &grantsWindow{ctx: ctx, st: st, rows: rows, size: max(size, 1)}
}

// forRow returns the grants of rows[i].
func (w *grantsWindow) forRow(i int) ([]Grant, error) {
	if w.err != nil {
		return nil, w.err
	}
	if i < w.from || i >= w.to {
		w.from, w.to = i, min(len(w.rows), i+w.size)
		ids := make([]string, 0, w.to-w.from)
		for _, r := range w.rows[w.from:w.to] {
			ids = append(ids, r.ID)
		}
		w.byID, w.err = w.st.ListGrantsFor(w.ctx, ids)
		if w.err != nil {
			w.from, w.to, w.byID = 0, 0, nil
			return nil, w.err
		}
	}
	return w.byID[w.rows[i].ID], nil
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

// listAccess says why the caller, who can read a, sees it: it owns a, the
// host lets it read a's home project, or a grant does. host memoizes, so
// the home project is asked once per request.
func listAccess(ctx context.Context, host Host, a *Artifact) string {
	kind, ref, _, _ := host.Principal(ctx)
	switch {
	case kind == a.OwnerKind && ref == a.OwnerRef:
		return AccessOwned
	case host.Authorize(ctx, a.ScopeRef, PermissionRead):
		return AccessProject
	default:
		return AccessShared
	}
}

// markDeletedScopes sets ScopeDeleted on the items whose home project no
// longer exists, when the host can tell (ScopeChecker), and CanManage on
// those the caller may move. While cross-project sharing is off no move
// can succeed, so CanManage stays false rather than offer one. It asks the
// host once per page, and reads the grants of the page's deleted-project
// rows with one store query, only if a row's decision needs them (the
// caller does not own it). A failed read fails the request.
func (s *Service) markDeletedScopes(ctx context.Context, b backend, items []ArtifactListItem) error {
	checker, ok := s.host.(ScopeChecker)
	if !ok || len(items) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var refs []string
	for _, it := range items {
		if it.ScopeKind == ScopeKindProject && !seen[it.ScopeRef] {
			seen[it.ScopeRef] = true
			refs = append(refs, it.ScopeRef)
		}
	}
	exists, err := checker.ScopesExist(ctx, refs)
	if err != nil {
		return err
	}
	var deleted []int
	for i := range items {
		it := &items[i]
		if it.ScopeKind != ScopeKindProject {
			continue
		}
		if e, known := exists[it.ScopeRef]; !known || e {
			continue
		}
		it.ScopeDeleted = true
		deleted = append(deleted, i)
	}
	if len(deleted) == 0 || !s.crossScopeAllowed(ctx) {
		return nil
	}
	// One page holds at most MaxListLimit rows, below MaxGrantsForIDs.
	var byID map[string][]Grant
	var loadErr error
	loaded := false
	grantsOf := func(id string) func() ([]Grant, error) {
		return func() ([]Grant, error) {
			if !loaded {
				loaded = true
				ids := make([]string, 0, len(deleted))
				for _, i := range deleted {
					ids = append(ids, items[i].ID)
				}
				byID, loadErr = b.store.ListGrantsFor(ctx, ids)
			}
			return byID[id], loadErr
		}
	}
	for _, i := range deleted {
		it := &items[i]
		if it.CanManage, err = s.canAdministerWith(ctx, it.src, grantsOf(it.ID)); err != nil {
			return err
		}
	}
	return nil
}

// ReviewPendingSummary returns the count of live, unexpired, non-attachment
// artifacts owned by the caller of ctx whose current version is a submitted
// review awaiting owner action, whether the collect scan budget (maxListScan)
// was reached, and up to previews newest items.
func (s *Service) ReviewPendingSummary(ctx context.Context, previews int) (count int, capped bool, items []ArtifactListItem, err error) {
	if previews < 0 {
		previews = 0
	}
	b, ok := s.backend()
	if !ok {
		return 0, false, nil, ErrBackendUnavailable
	}
	host := newMemoHost(s.host)
	kind, ref, _, ok := host.Principal(ctx)
	if !ok {
		return 0, false, []ArtifactListItem{}, nil
	}
	all, next, err := s.collect(ctx, host, b, CandidateQuery{
		PrincipalKind: kind,
		PrincipalRef:  ref,
		OwnedOnly:     true,
		ReviewPending: true,
	}, maxListScan)
	if err != nil {
		return 0, false, nil, err
	}
	count = len(all)
	capped = next != nil
	if previews == 0 || len(all) == 0 {
		return count, capped, []ArtifactListItem{}, nil
	}
	if len(all) > previews {
		all = all[:previews]
	}
	return count, capped, all, nil
}

// ReadableArtifacts returns the ArtifactInfo for each artifact ID in ids that
// exists, is live, unexpired, not an attachment, and readable by the caller of
// ctx under the same read check as GET /api/v1/artifacts/{id} (canReadWith).
// Grants are loaded in batches via grantsWindow and host checks are memoized
// for the duration of the call.
func (s *Service) ReadableArtifacts(ctx context.Context, ids []string) (map[string]ArtifactInfo, error) {
	out := make(map[string]ArtifactInfo)
	if len(ids) == 0 {
		return out, nil
	}
	b, ok := s.backend()
	if !ok {
		return nil, ErrBackendUnavailable
	}
	host := newMemoHost(s.host)
	if _, _, _, ok := host.Principal(ctx); !ok {
		return out, nil
	}

	seen := make(map[string]bool, len(ids))
	candidates := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		if seen[id] || !canonicalID(id) {
			continue
		}
		seen[id] = true
		a, err := b.store.GetArtifact(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if IsAttachmentScope(a.ScopeKind) {
			continue
		}
		candidates = append(candidates, Candidate{Artifact: *a})
	}
	if len(candidates) == 0 {
		return out, nil
	}

	grants := newGrantsWindow(ctx, b.store, candidates, max(len(candidates), grantWindowMin))
	for i := range candidates {
		a := &candidates[i].Artifact
		readable := canReadWith(ctx, host, a, func() ([]Grant, error) { return grants.forRow(i) })
		if grants.err != nil {
			return nil, grants.err
		}
		if readable {
			out[a.ID] = artifactInfo(a)
		}
	}
	return out, nil
}

// notifyInboxChanged tells the host, when it implements InboxNotifier, that
// userID's artifact-reviews inbox may have changed.
func (s *Service) notifyInboxChanged(ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	if n, ok := s.host.(InboxNotifier); ok {
		n.InboxChanged(ctx, userID)
	}
}
