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
	"slices"
	"strings"
	"testing"
	"time"
)

func TestListFilterQueryDialect_OffIsUnchanged(t *testing.T) {
	f := newFixture(t, false)
	// Dialect disabled (default).
	aReview := f.publish(agentA, "doc.md", []byte("1"), "title=Normal+Review").Artifact.ID
	// Mark aReview as review pending.
	if _, err := f.db.Exec(`UPDATE artifact_version SET kind = ? WHERE artifact_id = ?`, VersionKindReview, aReview); err != nil {
		t.Fatal(err)
	}
	aLiteral := f.publish(agentA, "lit.md", []byte("2"), "title="+url.QueryEscape("Notes about is:review")).Artifact.ID

	// With dialect OFF, q=is:review searches literally for "is:review" in title/key.
	got := listIDs(f.list(&userU, listPath+"&q="+url.QueryEscape("is:review")))
	if !slices.Equal(got, []string{aLiteral}) {
		t.Fatalf("dialect off q=is:review = %v, want [%s]", got, aLiteral)
	}
}

func TestListFilterQueryDialect_GlobalAndPureTextParity(t *testing.T) {
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

	// Pure-text parity: dialect OFF vs ON must return identical IDs.
	for _, raw := range []string{"report", "-v1", "design", "nonexistent:key"} {
		f.svc.SetFilterQueryDialectEnabled(func(context.Context) bool { return false })
		offIDs := listIDs(f.list(&userU, listPath+"&q="+url.QueryEscape(raw)))
		f.svc.SetFilterQueryDialectEnabled(func(context.Context) bool { return true })
		onIDs := listIDs(f.list(&userU, listPath+"&q="+url.QueryEscape(raw)))
		if !slices.Equal(offIDs, onIDs) {
			t.Errorf("pure-text parity %q: off=%v on=%v", raw, offIDs, onIDs)
		}
	}

	// Enable dialect for structured tests.
	f.svc.SetFilterQueryDialectEnabled(func(context.Context) bool { return true })
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

	if got := qList("is:review"); !slices.Equal(got, want(aMine)) {
		t.Errorf("is:review = %v, want %v", got, want(aMine))
	}
	if got := qList("-is:review"); !slices.Equal(got, want(aProj, aShared)) {
		t.Errorf("-is:review = %v, want %v", got, want(aProj, aShared))
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
	if got := qList("is:review,shared"); !slices.Equal(got, want(aMine, aShared)) {
		t.Errorf("is:review,shared = %v, want %v", got, want(aMine, aShared))
	}
	if got := qList("-is:review,shared"); !slices.Equal(got, want(aProj)) {
		t.Errorf("-is:review,shared = %v, want %v", got, want(aProj))
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
	gotAnded := listIDs(f.list(&userU, listPath+"&owner=me&q="+url.QueryEscape("is:review")))
	if !slices.Equal(gotAnded, want(aMine)) {
		t.Errorf("owner=me AND is:review = %v, want %v", gotAnded, want(aMine))
	}
}

func TestListFilterQueryDialect_ProjectScoped(t *testing.T) {
	f := newFixture(t, false)
	f.svc.SetFilterQueryDialectEnabled(func(context.Context) bool { return true })

	aHomed := f.publish(agentA, "homed.md", []byte("1"), "title=Homed").Artifact.ID
	aShared := f.publish(agentX, "shared.md", []byte("2"), "title=Shared").Artifact.ID
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
	// Caller without read access in project-1 gets empty 200 for is:shared.
	if got := listIDs(f.list(&outside, base+"&q="+url.QueryEscape("is:shared"))); len(got) != 0 {
		t.Errorf("unauthorized project is:shared = %v, want empty", got)
	}
	// is:mine is not in the project schema -> 400 invalid_filter_query.
	rec := f.do(&userU, http.MethodGet, base+"&q="+url.QueryEscape("is:mine"), nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("project is:mine status = %d, want 400", rec.Code)
	}
	var errResp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatal(err)
	}
	if errResp.Error.Code != "invalid_filter_query" {
		t.Errorf("error code = %q, want invalid_filter_query", errResp.Error.Code)
	}
}

func TestListFilterQueryDialect_ValidationErrorsAndCursorBinding(t *testing.T) {
	f := newFixture(t, false)
	f.svc.SetFilterQueryDialectEnabled(func(context.Context) bool { return true })
	f.host.slugs = map[string]string{"proj-one": "project-1"}

	// Validation error cases -> 400 invalid_filter_query with offset/length.
	for _, bad := range []string{
		"is:",
		"is:bogus",
		"is:mine is:shared",
		"is:mine,shared",
		"is:review OR is:mine",
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

	page1 := f.list(&userU, listPath+"&limit=1&q="+url.QueryEscape("is:review project:proj-one"))
	if len(page1.Artifacts) != 1 || page1.NextCursor == "" {
		t.Fatalf("page1 = %+v, want 1 item and nextCursor", page1)
	}
	// Page 2 with reordered/uppercased query must accept the cursor.
	page2 := f.list(&userU, listPath+"&limit=1&q="+url.QueryEscape("PROJECT:PROJ-ONE IS:REVIEW")+"&cursor="+url.QueryEscape(page1.NextCursor))
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
