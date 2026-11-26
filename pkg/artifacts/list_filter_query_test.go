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
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery/schemas"
)

func TestListFilterQueryDialect_GlobalAndPureText(t *testing.T) {
	f := newFixture(t, false)
	f.host.slugs = map[string]string{
		"proj-one": "project-1",
		"proj-two": "project-2",
	}
	// Allow userU to read in project-1 (already set by newFixture), not project-2.
	// Publish artifacts:
	// 1. aMine: owned by userU in project-1, review pending, title "Weekly Report"
	aMine := f.publish(userU, "weekly.md", []byte("1"), "scope=project-1&title="+url.QueryEscape("Weekly Report")).Artifact.ID
	if _, err := f.db.Exec(`UPDATE artifact_version SET kind = ? WHERE artifact_id = ?`, VersionKindReview, aMine); err != nil {
		t.Fatal(err)
	}
	// 2. aProj: owned by agentA in project-1, not review, title "Design Draft -v1"
	aProj := f.publish(agentA, "design.md", []byte("2"), "title="+url.QueryEscape("Design Draft -v1")).Artifact.ID
	// 3. aShared: owned by agentX in project-2, granted to userU (AccessShared), title "Shared Report"
	aShared := f.publish(agentX, "shared.md", []byte("3"), "title="+url.QueryEscape("Shared Report")).Artifact.ID
	f.grantPrincipal(aShared, userU)
	// 4. aPrivate: owned by agentX in project-2, NOT granted to userU (unreadable!)
	_ = f.publish(agentX, "secret.md", []byte("4"), "title="+url.QueryEscape("Secret Report")).Artifact.ID

	qList := func(q string) []string {
		t.Helper()
		got := listIDs(f.list(&userU, listPath+"&q="+url.QueryEscape(q)))
		slices.Sort(got)
		return got
	}
	want := func(ids ...string) []string {
		slices.Sort(ids)
		return ids
	}

	// Pure-text fallback: inputs without known keys search title/key verbatim.
	if got := qList("report"); !slices.Equal(got, want(aMine, aShared)) {
		t.Errorf("pure-text report = %v, want %v", got, want(aMine, aShared))
	}
	if got := qList("-v1"); !slices.Equal(got, want(aProj)) {
		t.Errorf("pure-text -v1 = %v, want %v", got, want(aProj))
	}
	if got := qList("design"); !slices.Equal(got, want(aProj)) {
		t.Errorf("pure-text design = %v, want %v", got, want(aProj))
	}
	if got := qList("nonexistent:key"); len(got) != 0 {
		t.Errorf("pure-text nonexistent:key = %v, want empty", got)
	}

	if got := qList("is:reviewed"); !slices.Equal(got, want(aMine)) {
		t.Errorf("is:reviewed = %v, want %v", got, want(aMine))
	}
	if got := qList("-is:reviewed"); !slices.Equal(got, want(aProj, aShared)) {
		t.Errorf("-is:reviewed = %v, want %v", got, want(aProj, aShared))
	}
	if got := qList("is:mine"); !slices.Equal(got, want(aMine)) {
		t.Errorf("is:mine = %v, want %v", got, want(aMine))
	}
	if got := qList("-is:mine"); !slices.Equal(got, want(aProj, aShared)) {
		t.Errorf("-is:mine = %v, want %v", got, want(aProj, aShared))
	}
	if got := qList("is:shared"); !slices.Equal(got, want(aShared)) {
		t.Errorf("is:shared = %v, want %v", got, want(aShared))
	}
	if got := qList("-is:shared"); !slices.Equal(got, want(aMine, aProj)) {
		t.Errorf("-is:shared = %v, want %v", got, want(aMine, aProj))
	}
	if got := qList("is:reviewed,shared"); !slices.Equal(got, want(aMine, aShared)) {
		t.Errorf("is:reviewed,shared = %v, want %v", got, want(aMine, aShared))
	}
	if got := qList("-is:reviewed,shared"); !slices.Equal(got, want(aProj)) {
		t.Errorf("-is:reviewed,shared = %v, want %v", got, want(aProj))
	}
	if got := qList("project:proj-one"); !slices.Equal(got, want(aMine, aProj)) {
		t.Errorf("project:proj-one = %v, want %v", got, want(aMine, aProj))
	}
	if got := qList("-project:proj-one"); !slices.Equal(got, want(aShared)) {
		t.Errorf("-project:proj-one = %v, want %v", got, want(aShared))
	}
	// Unauthorized slug (proj-two) and nonexistent slug must both return empty 200.
	if got := qList("project:proj-two"); len(got) != 0 {
		t.Errorf("unauthorized project:proj-two = %v, want empty", got)
	}
	if got := qList("project:does-not-exist"); len(got) != 0 {
		t.Errorf("nonexistent project = %v, want empty", got)
	}
	// Negated unauthorized/nonexistent slug returns all readable rows (no oracle).
	if got := qList("-project:proj-two"); !slices.Equal(got, want(aMine, aProj, aShared)) {
		t.Errorf("-project:proj-two = %v, want all readable", got)
	}
	// Combined structured + text + excluded text.
	if got := qList("project:proj-one -draft"); !slices.Equal(got, want(aMine)) {
		t.Errorf("project:proj-one -draft = %v, want %v", got, want(aMine))
	}
	// Legacy params ANDed with q.
	gotAnded := listIDs(f.list(&userU, listPath+"&owner=me&q="+url.QueryEscape("is:reviewed")))
	if !slices.Equal(gotAnded, want(aMine)) {
		t.Errorf("owner=me AND is:reviewed = %v, want %v", gotAnded, want(aMine))
	}
}

func TestListFilterQueryDialect_ProjectScoped(t *testing.T) {
	f := newFixture(t, false)

	aHomed := f.publish(agentA, "homed.md", []byte("1"), "title=Homed").Artifact.ID
	aShared := f.publish(agentX, "shared.md", []byte("2"), "title=Shared").Artifact.ID
	if _, err := f.db.Exec(`UPDATE artifact_version SET kind = ? WHERE artifact_id = ?`, VersionKindReview, aHomed); err != nil {
		t.Fatal(err)
	}
	// Grant project-1 read access to aShared (shared with project-1).
	if _, err := f.db.Exec(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at)
		VALUES (?, ?, 'scope', ?, 'read', ?)`, "g-shared-p1", aShared, "project-1",
		time.Now().UTC().Format(sqliteTimeLayout)); err != nil {
		t.Fatal(err)
	}

	base := listPath + "&scope=project-1"
	if got := listIDs(f.list(&userU, base+"&q="+url.QueryEscape("is:shared"))); !slices.Equal(got, []string{aShared}) {
		t.Errorf("project is:shared = %v, want [%s]", got, aShared)
	}
	if got := listIDs(f.list(&userU, base+"&q="+url.QueryEscape("-is:shared"))); !slices.Equal(got, []string{aHomed}) {
		t.Errorf("project -is:shared = %v, want [%s]", got, aHomed)
	}
	if got := listIDs(f.list(&userU, base+"&q="+url.QueryEscape("is:reviewed"))); !slices.Equal(got, []string{aHomed}) {
		t.Errorf("project is:reviewed = %v, want [%s]", got, aHomed)
	}
	if got := listIDs(f.list(&userU, base+"&q="+url.QueryEscape("-is:reviewed"))); !slices.Equal(got, []string{aShared}) {
		t.Errorf("project -is:reviewed = %v, want [%s]", got, aShared)
	}
	// Caller without read access in project-1 gets empty 200 for is:shared.
	if got := listIDs(f.list(&outside, base+"&q="+url.QueryEscape("is:shared"))); len(got) != 0 {
		t.Errorf("unauthorized project is:shared = %v, want empty", got)
	}
	// is:mine and legacy is:review are not in the project schema -> 400 invalid_filter_query.
	for _, bad := range []string{"is:mine", "is:review"} {
		rec := f.do(&userU, http.MethodGet, base+"&q="+url.QueryEscape(bad), nil, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("project %s status = %d, want 400", bad, rec.Code)
		}
		var errResp errorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
			t.Fatal(err)
		}
		if errResp.Error.Code != "invalid_filter_query" {
			t.Errorf("project %s error code = %q, want invalid_filter_query", bad, errResp.Error.Code)
		}
	}
}

func TestListFilterQueryDialect_ValidationErrorsAndCursorBinding(t *testing.T) {
	f := newFixture(t, false)
	f.host.slugs = map[string]string{"proj-one": "project-1"}

	// Validation error cases -> 400 invalid_filter_query with offset/length.
	for _, bad := range []string{
		"is:",
		"is:bogus",
		"is:review",
		"is:mine is:shared",
		"is:mine,shared",
		"is:reviewed OR is:mine",
		strings.Repeat("a", 201),
	} {
		rec := f.do(&userU, http.MethodGet, listPath+"&q="+url.QueryEscape(bad), nil, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("q=%q: status = %d, want 400", bad, rec.Code)
			continue
		}
		var errResp errorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("q=%q: unmarshal error: %v", bad, err)
		}
		if errResp.Error.Code != "invalid_filter_query" {
			t.Errorf("q=%q: code = %q, want invalid_filter_query", bad, errResp.Error.Code)
		}
		if _, ok := errResp.Error.Details["offset"]; !ok {
			t.Errorf("q=%q: missing details.offset", bad)
		}
		if _, ok := errResp.Error.Details["length"]; !ok {
			t.Errorf("q=%q: missing details.length", bad)
		}
	}

	// Cursor binding: reordered/case-folded equivalent query shares the same canonical HMAC.
	a1 := f.publish(agentA, "p1.md", []byte("1"), "title=First").Artifact.ID
	time.Sleep(2 * time.Millisecond)
	a2 := f.publish(agentA, "p2.md", []byte("2"), "title=Second").Artifact.ID
	if _, err := f.db.Exec(`UPDATE artifact_version SET kind = ? WHERE artifact_id IN (?, ?)`, VersionKindReview, a1, a2); err != nil {
		t.Fatal(err)
	}

	page1 := f.list(&userU, listPath+"&limit=1&q="+url.QueryEscape("is:reviewed project:proj-one"))
	if len(page1.Artifacts) != 1 || page1.NextCursor == "" {
		t.Fatalf("page1 = %+v, want 1 item and nextCursor", page1)
	}
	// Page 2 with reordered/uppercased query must accept the cursor.
	page2 := f.list(&userU, listPath+"&limit=1&q="+url.QueryEscape("PROJECT:PROJ-ONE IS:REVIEWED")+"&cursor="+url.QueryEscape(page1.NextCursor))
	if len(page2.Artifacts) != 1 {
		t.Fatalf("page2 = %+v, want 1 item", page2)
	}
	// Reusing cursor with a different query must fail with 400 invalid_cursor.
	rec := f.do(&userU, http.MethodGet, listPath+"&limit=1&q="+url.QueryEscape("is:mine")+"&cursor="+url.QueryEscape(page1.NextCursor), nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched cursor status = %d, want 400", rec.Code)
	}
	var errResp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatal(err)
	}
	if errResp.Error.Code != "invalid_cursor" {
		t.Errorf("mismatched cursor code = %q, want invalid_cursor", errResp.Error.Code)
	}
}

func TestListCandidates_UnknownQueryFieldReturnsError(t *testing.T) {
	f := newFixture(t, false)
	_, err := f.store.ListCandidates(context.Background(), CandidateQuery{
		PrincipalKind: PrincipalKindUser,
		PrincipalRef:  userU.ref,
		Limit:         10,
		Now:           time.Now(),
		Query: &QueryTerms{
			Terms: []QueryTerm{{Field: "unknown_field", Values: []string{"x"}}},
		},
	})
	if err == nil {
		t.Fatal("expected error for unknown filter field, got nil")
	}
}

// TestListCandidates_FilterQuerySQLPushdown verifies that ListCandidates
// pushes filter-query terms down into SQL itself (not merely via collect()
// post-filtering), including is:reviewed, -is:reviewed (with NULL current_seq),
// is:mine, -is:mine, project:, negated project:, project-scoped is:shared,
// and text/excludeText.
func TestListCandidates_FilterQuerySQLPushdown(t *testing.T) {
	f := newFixture(t, false)
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	seedList(t, f.db, f.store, listSeed{
		title: "mine-review-p1", ownerKind: PrincipalKindUser, ownerRef: "u1",
		scope: "p1", updated: t0.Add(4 * time.Minute), kind: VersionKindReview,
	})
	seedList(t, f.db, f.store, listSeed{
		title: "mine-pub-p1", ownerKind: PrincipalKindUser, ownerRef: "u1",
		scope: "p1", updated: t0.Add(3 * time.Minute), kind: VersionKindPublish,
	})
	seedList(t, f.db, f.store, listSeed{
		title: "other-review-p2", ownerKind: PrincipalKindUser, ownerRef: "u2",
		scope: "p2", updated: t0.Add(2 * time.Minute), kind: VersionKindReview,
		grants: []Grant{{SubjectKind: SubjectScope, SubjectRef: "p1", Permission: GrantRead}},
	})
	seedList(t, f.db, f.store, listSeed{
		title: "other-pub-p2", ownerKind: PrincipalKindUser, ownerRef: "u2",
		scope: "p2", updated: t0.Add(1 * time.Minute), kind: VersionKindPublish,
		grants: []Grant{{SubjectKind: SubjectPrincipal, SubjectRef: PrincipalRef(PrincipalKindUser, "u1"), Permission: GrantRead}},
	})
	// Row with no current ready version (current_seq IS NULL -> LEFT JOIN v.kind IS NULL).
	nullSeqID := seedList(t, f.db, f.store, listSeed{
		title: "mine-null-ver-p1", ownerKind: PrincipalKindUser, ownerRef: "u1",
		scope: "p1", updated: t0, kind: VersionKindPublish,
	})
	if _, err := f.db.Exec(`UPDATE artifact SET current_seq = NULL WHERE id = ?`, nullSeqID); err != nil {
		t.Fatal(err)
	}

	base := CandidateQuery{
		PrincipalKind: PrincipalKindUser,
		PrincipalRef:  "u1",
		ScopeRefs:     []string{"p1"},
		Limit:         100,
		Now:           t0.Add(10 * time.Minute),
	}

	cases := []struct {
		name  string
		query *QueryTerms
		scope string
		want  []string
	}{
		{
			name:  "is:reviewed",
			query: &QueryTerms{Terms: []QueryTerm{{Field: "is", Values: []string{"reviewed"}}}},
			want:  []string{"mine-review-p1", "other-review-p2"},
		},
		{
			name:  "-is:reviewed includes NULL v.kind",
			query: &QueryTerms{Terms: []QueryTerm{{Field: "is", Values: []string{"reviewed"}, Negate: true}}},
			want:  []string{"mine-null-ver-p1", "mine-pub-p1", "other-pub-p2"},
		},
		{
			name:  "is:mine",
			query: &QueryTerms{Terms: []QueryTerm{{Field: "is", Values: []string{"mine"}}}},
			want:  []string{"mine-null-ver-p1", "mine-pub-p1", "mine-review-p1"},
		},
		{
			name:  "-is:mine",
			query: &QueryTerms{Terms: []QueryTerm{{Field: "is", Values: []string{"mine"}, Negate: true}}},
			want:  []string{"other-pub-p2", "other-review-p2"},
		},
		{
			name:  "is:reviewed,mine comma OR",
			query: &QueryTerms{Terms: []QueryTerm{{Field: "is", Values: []string{"mine", "reviewed"}}}},
			want:  []string{"mine-null-ver-p1", "mine-pub-p1", "mine-review-p1", "other-review-p2"},
		},
		{
			name:  "-is:reviewed,mine negated comma OR",
			query: &QueryTerms{Terms: []QueryTerm{{Field: "is", Values: []string{"mine", "reviewed"}, Negate: true}}},
			want:  []string{"other-pub-p2"},
		},
		{
			name:  "project:p1",
			query: &QueryTerms{Terms: []QueryTerm{{Field: "project", Values: []string{"p1"}}}},
			want:  []string{"mine-null-ver-p1", "mine-pub-p1", "mine-review-p1"},
		},
		{
			name:  "-project:p1",
			query: &QueryTerms{Terms: []QueryTerm{{Field: "project", Values: []string{"p1"}, Negate: true}}},
			want:  []string{"other-pub-p2", "other-review-p2"},
		},
		{
			name:  "project-scoped is:shared",
			scope: "p1",
			query: &QueryTerms{Terms: []QueryTerm{{Field: "is", Values: []string{"shared"}}}},
			want:  []string{"other-review-p2"},
		},
		{
			name:  "project-scoped -is:shared",
			scope: "p1",
			query: &QueryTerms{Terms: []QueryTerm{{Field: "is", Values: []string{"shared"}, Negate: true}}},
			want:  []string{"mine-null-ver-p1", "mine-pub-p1", "mine-review-p1"},
		},
		{
			name:  "text and excludeText",
			query: &QueryTerms{Text: "mine", ExcludeText: []string{"null"}},
			want:  []string{"mine-pub-p1", "mine-review-p1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := base
			q.Query = tc.query
			if tc.scope != "" {
				q.HomeScope = tc.scope
				q.ScopeShares = true
			}
			got := candidateTitles(t, f.store, q)
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ListCandidates(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

type countingScopeHost struct {
	*fakeHost
	resolved map[string]string
	calls    map[string]int
	total    int
}

func (c *countingScopeHost) ResolveScope(_ context.Context, slugOrID string) (string, error) {
	if c.calls == nil {
		c.calls = make(map[string]int)
	}
	c.calls[slugOrID]++
	c.total++
	return c.resolved[slugOrID], nil
}

func TestFilterQueryToArtifactStoreTerms_MemoisesProjectLookups(t *testing.T) {
	base := newFixture(t, false)
	resolved := map[string]string{
		"foo": "project-foo",
		"bar": "project-bar",
		"a":   "project-a",
	}

	parseValidated := func(raw string) filterquery.Query {
		t.Helper()
		q, err := filterquery.Parse(raw, schemas.Artifacts.HasKey)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if err := schemas.Artifacts.Validate(q); err != nil {
			t.Fatalf("Validate(%q): %v", raw, err)
		}
		return q
	}

	t.Run("repeats within term, across terms, and case variants", func(t *testing.T) {
		ch := &countingScopeHost{fakeHost: base.host, resolved: resolved}
		q := parseValidated("project:Foo,foo,FOO project:foo,BAR -project:bar,Bar")
		got, err := filterQueryToArtifactStoreTerms(context.Background(), ch, q)
		if err != nil {
			t.Fatalf("filterQueryToArtifactStoreTerms: %v", err)
		}
		if ch.total != 2 {
			t.Fatalf("ResolveScope total calls = %d, want 2 (calls=%v)", ch.total, ch.calls)
		}
		if ch.calls["foo"] != 1 || ch.calls["bar"] != 1 {
			t.Fatalf("ResolveScope per-value calls = %v, want foo=1 bar=1", ch.calls)
		}

		// Results must be identical to an un-duplicated query.
		chClean := &countingScopeHost{fakeHost: base.host, resolved: resolved}
		qClean := parseValidated("project:foo project:foo,bar -project:bar")
		want, err := filterQueryToArtifactStoreTerms(context.Background(), chClean, qClean)
		if err != nil {
			t.Fatalf("filterQueryToArtifactStoreTerms (clean): %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("translated QueryTerms mismatch: got %+v, want %+v", got, want)
		}
	})

	t.Run("200 repeated project values resolve in 1 lookup", func(t *testing.T) {
		ch := &countingScopeHost{fakeHost: base.host, resolved: resolved}
		twenty := "project:" + strings.Repeat("a,", 19) + "a"
		raw := strings.TrimSpace(strings.Repeat(twenty+" ", 10))
		q := parseValidated(raw)
		got, err := filterQueryToArtifactStoreTerms(context.Background(), ch, q)
		if err != nil {
			t.Fatalf("filterQueryToArtifactStoreTerms: %v", err)
		}
		if ch.total != 1 || ch.calls["a"] != 1 {
			t.Fatalf("ResolveScope calls = %d (%v), want 1 call for 'a'", ch.total, ch.calls)
		}
		if len(got.Terms) != 10 {
			t.Fatalf("terms count = %d, want 10", len(got.Terms))
		}
		for _, term := range got.Terms {
			if !slices.Equal(term.Values, []string{"project-a"}) {
				t.Errorf("term values = %v, want [project-a]", term.Values)
			}
		}
	})
}
