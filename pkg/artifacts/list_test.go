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

package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const listPath = "/api/v1/artifacts?mine=1"

// list GETs target as p and decodes a 200 response.
func (f *fixture) list(p *principal, target string) ArtifactListResponse {
	f.t.Helper()
	rec := f.do(p, http.MethodGet, target, nil, nil)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("list %s: status %d: %s", target, rec.Code, rec.Body.String())
	}
	var resp ArtifactListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.t.Fatal(err)
	}
	if resp.Artifacts == nil {
		f.t.Fatalf("list %s: artifacts is null, want []", target)
	}
	return resp
}

func listIDs(r ArtifactListResponse) []string {
	out := make([]string, 0, len(r.Artifacts))
	for _, a := range r.Artifacts {
		out = append(out, a.ID)
	}
	return out
}

// grantPrincipal inserts a principal read grant on artifact id for p.
func (f *fixture) grantPrincipal(id string, p principal) {
	f.t.Helper()
	if _, err := f.db.Exec(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at)
		VALUES (?, ?, 'principal', ?, 'read', ?)`, "g-"+id+"-"+p.ref, id, PrincipalRef(p.kind, p.ref),
		time.Now().UTC().Format(sqliteTimeLayout)); err != nil {
		f.t.Fatal(err)
	}
}

// TestListVisibility: the list shows exactly what GET would let the caller
// read among its owned, granted and project artifacts, and nothing else.
func TestListVisibility(t *testing.T) {
	f := newFixture(t, false)
	a1 := f.publish(agentA, "a1.md", []byte("1"), "").Artifact.ID
	a2 := f.publish(agentX, "x1.md", []byte("2"), "").Artifact.ID

	// Owner, same-project agent and project-member user see a1 only.
	for name, p := range map[string]principal{"owner": agentA, "same project": agentB, "member user": userU} {
		if got := listIDs(f.list(&p, listPath)); !slices.Equal(got, []string{a1}) {
			t.Errorf("%s: %v, want [a1]", name, got)
		}
	}
	// Another project's agent and an outsider see nothing of project-1.
	if got := listIDs(f.list(&agentX, listPath)); !slices.Equal(got, []string{a2}) {
		t.Errorf("other project agent: %v, want only its own", got)
	}
	if got := listIDs(f.list(&outside, listPath)); len(got) != 0 {
		t.Errorf("outsider: %v, want none", got)
	}
	// Unauthenticated: empty, not an error, same as having nothing.
	if got := listIDs(f.list(nil, listPath)); len(got) != 0 {
		t.Errorf("unauthenticated: %v, want none", got)
	}

	// A principal grant adds the artifact for exactly that principal.
	f.grantPrincipal(a2, outside)
	if got := listIDs(f.list(&outside, listPath)); !slices.Equal(got, []string{a2}) {
		t.Errorf("granted outsider: %v, want [x1]", got)
	}
	// Every listed artifact is readable by GET for the same caller.
	for _, p := range []principal{agentA, agentB, agentX, userU, outside} {
		for _, id := range listIDs(f.list(&p, listPath)) {
			if rec := f.do(&p, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK {
				t.Errorf("%s lists %s but GET answers %d", p.ref, id, rec.Code)
			}
		}
	}
}

// TestListCredentialCheckFirst: when the credential does not permit reads
// in the home scope, the artifact is absent even for its owner, and the
// credential is asked before anything else.
func TestListCredentialCheckFirst(t *testing.T) {
	f := newFixture(t, false)
	id := f.publish(agentA, "a.md", []byte("1"), "").Artifact.ID
	f.grantPrincipal(id, userU)
	for _, p := range []principal{agentA, userU} {
		f.host.deny(p, "project-1", PermissionRead)
		f.host.mu.Lock()
		f.host.calls = nil
		f.host.mu.Unlock()
		if got := listIDs(f.list(&p, listPath)); len(got) != 0 {
			t.Errorf("%s with read not permitted: %v, want none", p.ref, got)
		}
		f.host.mu.Lock()
		calls := slices.Clone(f.host.calls)
		f.host.mu.Unlock()
		if len(calls) == 0 || calls[0] != "permits project-1 "+PermissionRead {
			t.Errorf("%s: first host call %v, want the credential check", p.ref, calls)
		}
		if slices.Contains(calls, "project-1 "+PermissionRead) {
			t.Errorf("%s: Authorize ran although Permits refused: %v", p.ref, calls)
		}
	}
}

// TestListAfterMembershipLost: an artifact homed in a project the caller
// left drops out unless it owns it or holds a direct grant.
func TestListAfterMembershipLost(t *testing.T) {
	f := newFixture(t, false)
	shared := f.publish(agentA, "shared.md", []byte("1"), "").Artifact.ID
	direct := f.publish(agentA, "direct.md", []byte("2"), "").Artifact.ID
	f.grantPrincipal(direct, userU)
	if got := f.list(&userU, listPath); len(got.Artifacts) != 2 {
		t.Fatalf("member: %d artifacts, want 2", len(got.Artifacts))
	}
	f.host.mu.Lock()
	f.host.perms = map[string]map[string]map[string]bool{}
	f.host.mu.Unlock()
	if got := listIDs(f.list(&userU, listPath)); !slices.Equal(got, []string{direct}) {
		t.Errorf("after leaving: %v, want only the directly granted one", got)
	}
	if got := listIDs(f.list(&agentA, listPath)); len(got) != 2 || !slices.Contains(got, shared) {
		t.Errorf("owner after scope loss: %v, want both", got)
	}
}

func TestListFilters(t *testing.T) {
	f := newFixture(t, false)
	own := f.publish(userU, "Weekly Report.md", []byte("1"), "scope=project-1").Artifact.ID
	theirs := f.publish(agentA, "design-notes.md", []byte("2"), "title=Design+notes").Artifact.ID
	if _, err := f.db.Exec("UPDATE artifact_version SET kind = ? WHERE artifact_id = ?", VersionKindReview, theirs); err != nil {
		t.Fatal(err)
	}

	all := f.list(&userU, listPath)
	if len(all.Artifacts) != 2 {
		t.Fatalf("all: %d, want 2", len(all.Artifacts))
	}
	for _, a := range all.Artifacts {
		if a.ReviewPending != (a.ID == theirs) {
			t.Errorf("%s reviewPending = %v", a.Title, a.ReviewPending)
		}
	}
	for q, want := range map[string][]string{
		"&review_pending=1": {theirs},
		"&owner=me":         {own},
		"&q=weekly":         {own},
		"&q=DESIGN":         {theirs},
		"&q=nomatch":        {},
	} {
		if got := listIDs(f.list(&userU, listPath+q)); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", q, got, want)
		}
	}
}

func TestListBadRequests(t *testing.T) {
	f := newFixture(t, false)
	for _, target := range []string{
		"/api/v1/artifacts",
		"/api/v1/artifacts?mine=0",
		listPath + "&limit=0",
		listPath + "&limit=" + strconv.Itoa(MaxListLimit+1),
		listPath + "&limit=x",
		listPath + "&review_pending=maybe",
		listPath + "&owner=bob",
		listPath + "&q=%00",
		listPath + "&q=" + url.QueryEscape(string(slices.Repeat([]rune("é"), maxSearchLength+1))),
	} {
		rec := f.do(&userU, http.MethodGet, target, nil, nil)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "bad_request" {
			t.Errorf("%s: %d %s, want 400 bad_request", target, rec.Code, rec.Body.String())
		}
	}
	if rec := f.do(&userU, http.MethodGet, listPath+"&q="+url.QueryEscape(string(slices.Repeat([]rune("é"), maxSearchLength))), nil, nil); rec.Code != http.StatusOK {
		t.Errorf("q at the length limit: %d, want 200", rec.Code)
	}
	rec := f.do(&userU, http.MethodPut, "/api/v1/artifacts", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD, POST" {
		t.Errorf("PUT collection: %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	f.host.memberErr = errors.New("boom")
	if rec := f.do(&userU, http.MethodGet, listPath, nil, nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("member scopes failure: %d, want 500", rec.Code)
	}
	if rec := f.do(&userU, http.MethodGet, listPath+"&owner=me", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("owner=me does not need member scopes: %d, want 200", rec.Code)
	}
}

func TestListPagination(t *testing.T) {
	f := newFixture(t, false)
	var want []string
	for i := range 5 {
		want = append(want, f.publish(agentA, "f"+strconv.Itoa(i)+".md", []byte{byte(i)}, "").Artifact.ID)
	}
	var got []string
	target := listPath + "&limit=2"
	pages := 0
	for {
		page := f.list(&userU, target)
		pages++
		got = append(got, listIDs(page)...)
		if page.NextCursor == "" {
			break
		}
		if pages > 5 {
			t.Fatal("pagination does not terminate")
		}
		target = listPath + "&limit=2&cursor=" + url.QueryEscape(page.NextCursor)
	}
	if pages != 3 || len(got) != 5 {
		t.Fatalf("%d pages, %d items; want 3 pages of 5 items", pages, len(got))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("walk returned %v, want %v", got, want)
	}

	first := f.list(&userU, listPath+"&limit=2")
	cursor := url.QueryEscape(first.NextCursor)
	for name, tc := range map[string]struct {
		p      principal
		target string
	}{
		"another caller":  {agentB, listPath + "&limit=2&cursor=" + cursor},
		"another query":   {userU, listPath + "&q=f&cursor=" + cursor},
		"another filter":  {userU, listPath + "&review_pending=1&cursor=" + cursor},
		"tampered cursor": {userU, listPath + "&cursor=" + cursor + "x"},
		"garbage cursor":  {userU, listPath + "&cursor=nope"},
	} {
		rec := f.do(&tc.p, http.MethodGet, tc.target, nil, nil)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_cursor" {
			t.Errorf("%s: %d %s, want 400 invalid_cursor", name, rec.Code, rec.Body.String())
		}
	}
	// A different page size keeps the walk valid.
	if rec := f.do(&userU, http.MethodGet, listPath+"&limit=3&cursor="+cursor, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("changed limit: %d, want 200", rec.Code)
	}
}

// TestListScanCap: candidates the caller cannot read still count toward
// the per-request scan cap; a capped page is short and its cursor resumes
// after the last row examined, so the walk still reaches every readable row.
func TestListScanCap(t *testing.T) {
	old := maxListScan
	maxListScan = 3
	t.Cleanup(func() { maxListScan = old })

	f := newFixture(t, false)
	f.host.allow(userU, "project-3", PermissionRead, PermissionCreate)
	readable := f.publish(userU, "old.md", []byte("old"), "scope=project-1").Artifact.ID
	for i := range 5 {
		f.publish(userU, "hidden"+strconv.Itoa(i)+".md", []byte{byte(i)}, "scope=project-3")
	}
	f.host.deny(userU, "project-3", PermissionRead)

	first := f.list(&userU, listPath)
	if len(first.Artifacts) != 0 || first.NextCursor == "" {
		t.Fatalf("first page: %d items, cursor %q; want a short page with a cursor", len(first.Artifacts), first.NextCursor)
	}
	// The scan-position cursor goes through the host's sealing, bound to
	// the caller and the query like any other cursor.
	if !strings.HasPrefix(first.NextCursor, "fake.") {
		t.Errorf("cursor %q was not sealed by the host", first.NextCursor)
	}
	for name, tc := range map[string]struct {
		p      principal
		target string
	}{
		"another caller": {outside, listPath + "&cursor=" + url.QueryEscape(first.NextCursor)},
		"another query":  {userU, listPath + "&q=hidden&cursor=" + url.QueryEscape(first.NextCursor)},
	} {
		rec := f.do(&tc.p, http.MethodGet, tc.target, nil, nil)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_cursor" {
			t.Errorf("scan cursor, %s: %d %s, want 400 invalid_cursor", name, rec.Code, rec.Body.String())
		}
	}
	var got []string
	target := listPath + "&cursor=" + url.QueryEscape(first.NextCursor)
	for range 5 {
		page := f.list(&userU, target)
		got = append(got, listIDs(page)...)
		if page.NextCursor == "" {
			break
		}
		target = listPath + "&cursor=" + url.QueryEscape(page.NextCursor)
	}
	if !slices.Equal(got, []string{readable}) {
		t.Errorf("walk past the cap: %v, want [old]", got)
	}
}

// TestListMemoizesHost: a page of artifacts from one project costs one
// credential check and one authorization check, not one per row.
func TestListMemoizesHost(t *testing.T) {
	f := newFixture(t, false)
	for i := range 4 {
		f.publish(agentA, "m"+strconv.Itoa(i)+".md", []byte{byte(i)}, "")
	}
	f.host.mu.Lock()
	f.host.calls = nil
	f.host.mu.Unlock()
	if got := f.list(&userU, listPath); len(got.Artifacts) != 4 {
		t.Fatalf("%d artifacts, want 4", len(got.Artifacts))
	}
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	counts := map[string]int{}
	for _, c := range f.host.calls {
		counts[c]++
	}
	if counts["permits project-1 "+PermissionRead] != 1 || counts["project-1 "+PermissionRead] != 1 {
		t.Errorf("host calls %v, want one Permits and one Authorize", counts)
	}
}

// countingStore counts the queries the list endpoint makes.
type countingStore struct {
	Store
	mu         sync.Mutex
	candidates int
	grantsFor  int
	grants     int
}

func (c *countingStore) ListCandidates(ctx context.Context, q CandidateQuery) ([]Candidate, error) {
	c.mu.Lock()
	c.candidates++
	c.mu.Unlock()
	return c.Store.ListCandidates(ctx, q)
}

func (c *countingStore) ListGrantsFor(ctx context.Context, ids []string) (map[string][]Grant, error) {
	c.mu.Lock()
	c.grantsFor++
	c.mu.Unlock()
	return c.Store.ListGrantsFor(ctx, ids)
}

func (c *countingStore) ListGrants(ctx context.Context, id string) ([]Grant, error) {
	c.mu.Lock()
	c.grants++
	c.mu.Unlock()
	return c.Store.ListGrants(ctx, id)
}

func (f *fixture) countStore() *countingStore {
	cs := &countingStore{Store: f.store}
	f.svc.SetStore(cs)
	return cs
}

// TestListStoreQueriesBounded: one list request makes one candidate query
// and at most one grants query, however many candidates the caller cannot
// read and however small the page.
func TestListStoreQueriesBounded(t *testing.T) {
	old := maxListScan
	maxListScan = 6
	t.Cleanup(func() { maxListScan = old })

	f := newFixture(t, false)
	f.host.allow(userU, "project-3", PermissionRead, PermissionCreate)
	for i := range 10 {
		f.publish(userU, "hidden"+strconv.Itoa(i)+".md", []byte{byte(i)}, "scope=project-3")
	}
	f.host.deny(userU, "project-3", PermissionRead)
	cs := f.countStore()

	page := f.list(&userU, listPath+"&limit=1")
	if len(page.Artifacts) != 0 || page.NextCursor == "" {
		t.Fatalf("page: %d items, cursor %q; want an empty page with a cursor", len(page.Artifacts), page.NextCursor)
	}
	if cs.candidates != 1 {
		t.Errorf("one request made %d candidate queries, want 1", cs.candidates)
	}

	// Rows reached through grants: one grants query for the whole page,
	// never one per row.
	for i := range 4 {
		id := f.publish(agentX, "x"+strconv.Itoa(i)+".md", []byte{byte(i)}, "").Artifact.ID
		f.grantPrincipal(id, outside)
	}
	cs.candidates, cs.grantsFor, cs.grants = 0, 0, 0
	if got := f.list(&outside, listPath); len(got.Artifacts) != 4 {
		t.Fatalf("granted rows: %d, want 4", len(got.Artifacts))
	}
	if cs.candidates != 1 || cs.grantsFor != 1 || cs.grants != 0 {
		t.Errorf("queries: candidates=%d grantsFor=%d grants=%d, want 1, 1, 0", cs.candidates, cs.grantsFor, cs.grants)
	}
	// Owned rows need no grants at all.
	cs.candidates, cs.grantsFor, cs.grants = 0, 0, 0
	f.list(&agentX, listPath+"&owner=me")
	if cs.grantsFor != 0 || cs.grants != 0 {
		t.Errorf("owned rows read grants: grantsFor=%d grants=%d", cs.grantsFor, cs.grants)
	}
}

// TestListScanBudgetFitsGrantBatch: the grants of a whole scan budget fit
// in one ListGrantsFor call.
func TestListScanBudgetFitsGrantBatch(t *testing.T) {
	if maxListScan > MaxGrantsForIDs {
		t.Fatalf("maxListScan %d exceeds MaxGrantsForIDs %d", maxListScan, MaxGrantsForIDs)
	}
}
