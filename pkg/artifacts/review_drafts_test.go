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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type reviewDraftHost struct {
	*fakeHost
	gateMu sync.Mutex
	on     bool
}

type draftReapFailStore struct{ Store }

func (draftReapFailStore) ReapReviewDrafts(context.Context, time.Time, int) (int, error) {
	return 0, errStoreDown
}

func (h *reviewDraftHost) FeatureEnabled(_ context.Context, name string) bool {
	h.gateMu.Lock()
	defer h.gateMu.Unlock()
	return h.on && name == FeatureReviewDrafts
}

func (h *reviewDraftHost) setOn(on bool) {
	h.gateMu.Lock()
	h.on = on
	h.gateMu.Unlock()
}

func draftPath(id string) string { return "/api/v1/artifacts/" + id + "/review-draft" }

// newReviewDraftFixture publishes doc.md as agentA (v1) in project-1, grants
// userU review access and agentB write access, and allows readerR to read in
// project-1 without review/write access.
func newReviewDraftFixture(t *testing.T) (*fixture, *reviewDraftHost, string) {
	t.Helper()
	f := newFixture(t, false)
	f.host.allow(readerR, "project-1", PermissionRead, PermissionCreate)
	h := &reviewDraftHost{fakeHost: f.host, on: true}
	f.svc.host = h
	id := f.publish(agentA, "doc.md", []byte(reviewParent), "").Artifact.ID
	f.addGrant(id, userU, GrantReview)
	f.addGrant(id, agentB, GrantWrite)
	return f, h, id
}

func (f *fixture) putDraftRaw(p *principal, id string, body []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(p, http.MethodPut, draftPath(id), body, map[string]string{"Content-Type": "application/json"})
}

func (f *fixture) getDraft(p *principal, id string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(p, http.MethodGet, draftPath(id), nil, nil)
}

func (f *fixture) deleteDraft(p *principal, id, query string) *httptest.ResponseRecorder {
	f.t.Helper()
	target := draftPath(id)
	if query != "" {
		target += "?" + query
	}
	return f.do(p, http.MethodDelete, target, nil, nil)
}

// countingReader counts bytes read so tests can prove C2 (no body byte read
// before authorization and limiter checks).
type countingReader struct {
	r     io.Reader
	bytes atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.bytes.Add(int64(n))
	return n, err
}

// TestReviewDraftAuthorizationOrder verifies C2 on GET, PUT and DELETE:
// FeatureEnabled -> Principal -> readableArtifact -> canWriteErr -> (PUT)
// CurrentSeq > 0 -> (PUT/DELETE) LimitDraftWrite -> (PUT) MaxBytesReader body.
// Refused callers create zero limiter entries and have zero body bytes read.
func TestReviewDraftAuthorizationOrder(t *testing.T) {
	f, h, id := newReviewDraftFixture(t)
	lim := f.svc.principalLim()
	validPut := []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"# Plan\n","note":""}`)
	oversize := []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"` + strings.Repeat("x", 3*MaxDraftBodyBytes) + `","note":""}`)

	doCountedPut := func(p *principal, artID string, payload []byte) (*httptest.ResponseRecorder, int64) {
		t.Helper()
		cr := &countingReader{r: bytes.NewReader(payload)}
		req := httptest.NewRequest(http.MethodPut, draftPath(artID), cr)
		req.Header.Set("Content-Type", "application/json")
		if p != nil {
			req = withPrincipal(req, *p)
		}
		rec := httptest.NewRecorder()
		f.svc.ServeHTTP(rec, req)
		return rec, cr.bytes.Load()
	}

	// Step 0a: Experiment off -> 404 on every method (including unsupported
	// POST), zero body bytes read, zero limiter entries.
	h.setOn(false)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPost} {
		rec := f.do(&userU, m, draftPath(id), validPut, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("experiment off %s: %d %s, want 404", m, rec.Code, rec.Body.String())
		}
	}
	if rec, read := doCountedPut(&userU, id, oversize); rec.Code != http.StatusNotFound || read != 0 {
		t.Errorf("experiment off oversize PUT: status=%d read=%d, want 404 and 0", rec.Code, read)
	}
	h.setOn(true)

	// Unsupported methods when experiment is on -> 405 with Allow.
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodHead} {
		rec := f.do(&userU, m, draftPath(id), nil, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: %d, want 405", m, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, PUT, DELETE" {
			t.Errorf("method %s Allow = %q, want %q", m, allow, "GET, PUT, DELETE")
		}
	}

	// Steps 0b, 1, 2: unauthenticated -> 404; unreadable -> 404; readable
	// without review/write -> 403. Oversize bodies get 404/403 (never 413),
	// 0 body bytes are read, and 0 limiter entries are created.
	for _, tc := range []struct {
		name string
		p    *principal
		want int
	}{
		{"unauthenticated", nil, http.StatusNotFound},
		{"outsider cannot read", &outside, http.StatusNotFound},
		{"other project agent", &agentX, http.StatusNotFound},
		{"reader without review", &readerR, http.StatusForbidden},
	} {
		if rec := f.getDraft(tc.p, id); rec.Code != tc.want {
			t.Errorf("%s GET: %d %s, want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
		if rec, read := doCountedPut(tc.p, id, oversize); rec.Code != tc.want || read != 0 {
			t.Errorf("%s oversize PUT: status=%d read=%d, want %d and 0 bytes read", tc.name, rec.Code, read, tc.want)
		}
		if rec := f.deleteDraft(tc.p, id, ""); rec.Code != tc.want {
			t.Errorf("%s DELETE: %d %s, want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}

	// Step 3: PUT on an artifact with no ready version -> 409 nothing_to_review
	// BEFORE charging the limiter or reading the body.
	pend := f.createPending(agentA, "/api/v1/artifacts", bundle{"n.md": []byte("# n")}.manifest("n.md"))
	if rec, read := doCountedPut(&agentA, pend.Artifact.ID, oversize); rec.Code != http.StatusConflict || errCode(t, rec) != CodeNothingToReview || read != 0 {
		t.Errorf("no ready version PUT: status=%d code=%q read=%d, want 409 nothing_to_review and 0 bytes read",
			rec.Code, errCode(t, rec), read)
	}

	if n := lim.tracked(LimitDraftWrite); n != 0 {
		t.Fatalf("refused requests created %d LimitDraftWrite entries, want 0", n)
	}

	// Share-link and view routes never reach review-draft.
	f.svc.SetViewKey(testViewKey)
	ownedID := f.publish(userU, "owned.md", []byte("# owned\n"), "scope=project-1").Artifact.ID
	linkTok, _ := f.mustMintLink(userU, ownedID, `{}`)
	for _, path := range []string{
		RouteShared + linkTok + "/review-draft",
		RouteView + "any-capability/review-draft",
	} {
		if rec := f.do(nil, http.MethodGet, path, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, rec.Code)
		}
	}

	// Step 4 & 5: Authorized callers (review grantee, write grantee, owner)
	// pass: GET does NOT charge LimitDraftWrite; PUT and DELETE do.
	if rec := f.getDraft(&userU, id); rec.Code != http.StatusOK {
		t.Fatalf("authorized GET: %d %s", rec.Code, rec.Body.String())
	}
	if n := lim.tracked(LimitDraftWrite); n != 0 {
		t.Errorf("GET created %d LimitDraftWrite entries, want 0", n)
	}

	for _, p := range []principal{userU, agentB, agentA} {
		if rec := f.putDraftRaw(&p, id, validPut); rec.Code != http.StatusOK {
			t.Fatalf("%s PUT: %d %s", p.ref, rec.Code, rec.Body.String())
		}
	}
	if n := lim.tracked(LimitDraftWrite); n != 3 {
		t.Errorf("authorized PUTs created %d LimitDraftWrite entries, want 3", n)
	}
	for _, otherFamily := range []LimitFamily{LimitReviewBeat, LimitReviewList} {
		if n := lim.tracked(otherFamily); n != 0 {
			t.Errorf("draft writes created %d %s entries, want 0", n, otherFamily)
		}
	}

	// Authorized oversize envelope is caught by MaxBytesReader -> 413 draft_too_large with limit.
	rec := f.putDraftRaw(&userU, id, oversize)
	if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != CodeDraftTooLarge {
		t.Fatalf("authorized oversize envelope: %d %s, want 413 draft_too_large", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"limit":1048576`) {
		t.Errorf("413 response missing limit=1048576: %s", rec.Body.String())
	}
}

func TestReviewDraftRoundTripConcurrencyAndCaps(t *testing.T) {
	f, _, id := newReviewDraftFixture(t)

	// Initial GET returns 200 with draft: null and currentSeq: 1.
	rec := f.getDraft(&userU, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("initial GET: %d %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != `{"draft":null,"currentSeq":1}` {
		t.Errorf("initial GET body = %s, want {\"draft\":null,\"currentSeq\":1}", rec.Body.String())
	}

	// Create draft (rev 0 -> rev 1) with non-ASCII CriticMarkup and clientSavedAt.
	body1 := "# Plan\n\nWe {~~ship~>launch 🚀~~} in Q3.{>>check date<<}\nOwners: docs.\n"
	put1 := map[string]any{
		"rev":           0,
		"base":          1,
		"entry":         "doc.md",
		"body":          body1,
		"note":          "",
		"clientSavedAt": "2026-11-19T18:01:00.250Z",
	}
	b1, _ := json.Marshal(put1)
	rec = f.putDraftRaw(&userU, id, b1)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT rev=0: %d %s", rec.Code, rec.Body.String())
	}
	resp1 := decodeInto[PutReviewDraftResponse](t, rec)
	if resp1.Rev != 1 || resp1.CurrentSeq != 1 || resp1.SavedAt.IsZero() {
		t.Fatalf("PUT rev=0 response = %+v", resp1)
	}

	// GET returns exact JSON contract including null carried and empty note.
	rec = f.getDraft(&userU, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after create: %d %s", rec.Code, rec.Body.String())
	}
	var rawGet map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rawGet); err != nil {
		t.Fatal(err)
	}
	draftMap, ok := rawGet["draft"].(map[string]any)
	if !ok {
		t.Fatalf("expected object draft, got %v", rawGet)
	}
	for _, key := range []string{"rev", "base", "entry", "body", "note", "carried", "savedAt", "createdAt"} {
		if _, present := draftMap[key]; !present {
			t.Errorf("GET draft missing required key %q: %s", key, rec.Body.String())
		}
	}
	if draftMap["carried"] != nil || draftMap["note"] != "" || draftMap["body"] != body1 {
		t.Errorf("GET draft values = %+v", draftMap)
	}

	// Publish v2 as agentA so we can test older-base drafts and carried text.
	v2 := bundle{"doc.md": []byte("# Plan v2\n")}
	f.publishBundle(agentA, "/api/v1/artifacts/"+id+"/versions", v2.manifest("doc.md"), v2)

	// Stale base (base=1 while currentSeq=2) is still accepted on PUT; response
	// reports currentSeq=2.
	put2, _ := json.Marshal(map[string]any{
		"rev":   1,
		"base":  1,
		"entry": "doc.md",
		"body":  body1 + "{++more++}",
		"note":  "my note",
	})
	rec = f.putDraftRaw(&userU, id, put2)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT on older base: %d %s", rec.Code, rec.Body.String())
	}
	resp2 := decodeInto[PutReviewDraftResponse](t, rec)
	if resp2.Rev != 2 || resp2.CurrentSeq != 2 {
		t.Fatalf("PUT rev=1 response = %+v", resp2)
	}

	// Move to v2 with carried text from v1.
	put3, _ := json.Marshal(map[string]any{
		"rev":   2,
		"base":  2,
		"entry": "doc.md",
		"body":  "# Plan v2\n{>>fresh<<}\n",
		"note":  "moved note",
		"carried": map[string]any{
			"base": 1,
			"body": body1,
		},
	})
	rec = f.putDraftRaw(&userU, id, put3)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with carried: %d %s", rec.Code, rec.Body.String())
	}
	resp3 := decodeInto[PutReviewDraftResponse](t, rec)
	if resp3.Rev != 3 {
		t.Fatalf("PUT rev=2 -> %+v, want rev=3", resp3)
	}

	got3 := decodeInto[ReviewDraftResponse](t, f.getDraft(&userU, id))
	if got3.CurrentSeq != 2 || got3.Draft == nil || got3.Draft.Rev != 3 || got3.Draft.Carried == nil ||
		got3.Draft.Carried.Base != 1 || got3.Draft.Carried.Body != body1 {
		t.Fatalf("GET with carried = %+v", got3)
	}

	// 409 draft_conflict on stale PUT carries {rev, savedAt, base} and NO body/note.
	stalePut, _ := json.Marshal(map[string]any{
		"rev":   1,
		"base":  2,
		"entry": "doc.md",
		"body":  "stale attempt",
		"note":  "stale note",
	})
	rec = f.putDraftRaw(&userU, id, stalePut)
	if rec.Code != http.StatusConflict || errCode(t, rec) != CodeDraftConflict {
		t.Fatalf("stale PUT: %d %s, want 409 draft_conflict", rec.Code, rec.Body.String())
	}
	var conflictEnv errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &conflictEnv); err != nil {
		t.Fatal(err)
	}
	if conflictEnv.Error.Details["rev"] != float64(3) || conflictEnv.Error.Details["base"] != float64(2) || conflictEnv.Error.Details["savedAt"] == nil {
		t.Errorf("409 details = %+v, want rev=3 base=2 savedAt!=nil", conflictEnv.Error.Details)
	}
	for _, forbidden := range []string{"Plan v2", "moved note", "launch", "stale attempt"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("409 response leaked %q: %s", forbidden, rec.Body.String())
		}
	}

	// Conditional DELETE with wrong rev -> 409 draft_conflict without body; draft survives.
	rec = f.deleteDraft(&userU, id, "rev=2")
	if rec.Code != http.StatusConflict || errCode(t, rec) != CodeDraftConflict {
		t.Fatalf("DELETE ?rev=2: %d %s, want 409 draft_conflict", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Plan v2") || strings.Contains(rec.Body.String(), "moved note") {
		t.Errorf("DELETE 409 leaked draft content: %s", rec.Body.String())
	}

	// Conditional DELETE with matching rev -> 204; repeat -> 204 (idempotent).
	if rec := f.deleteDraft(&userU, id, "rev=3"); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE ?rev=3: %d %s, want 204", rec.Code, rec.Body.String())
	}
	if rec := f.deleteDraft(&userU, id, "rev=3"); rec.Code != http.StatusNoContent {
		t.Fatalf("repeat DELETE ?rev=3: %d %s, want 204", rec.Code, rec.Body.String())
	}
	if got := decodeInto[ReviewDraftResponse](t, f.getDraft(&userU, id)); got.Draft != nil {
		t.Errorf("draft survived DELETE: %+v", got.Draft)
	}
}

func TestReviewDraftValidation413And429(t *testing.T) {
	f, _, id := newReviewDraftFixture(t)
	// Add v2 so carried.base checks have multiple versions.
	v2 := bundle{"doc.md": []byte("# Plan v2\n")}
	f.publishBundle(agentA, "/api/v1/artifacts/"+id+"/versions", v2.manifest("doc.md"), v2)

	// Validation 400 cases.
	badCases := []struct {
		name string
		body string
	}{
		{"missing rev", `{"base":1,"entry":"doc.md","body":"x","note":""}`},
		{"negative rev", `{"rev":-1,"base":1,"entry":"doc.md","body":"x","note":""}`},
		{"missing body", `{"rev":0,"base":1,"entry":"doc.md","note":""}`},
		{"zero base", `{"rev":0,"base":0,"entry":"doc.md","body":"x","note":""}`},
		{"base above currentSeq", `{"rev":0,"base":3,"entry":"doc.md","body":"x","note":""}`},
		{"wrong entry path", `{"rev":0,"base":1,"entry":"other.md","body":"x","note":""}`},
		{"empty entry path", `{"rev":0,"base":1,"entry":"","body":"x","note":""}`},
		{"note over 2000 runes", fmt.Sprintf(`{"rev":0,"base":1,"entry":"doc.md","body":"x","note":%q}`, strings.Repeat("é", maxNoteRunes+1))},
		{"carried base >= base", `{"rev":0,"base":2,"entry":"doc.md","body":"x","note":"","carried":{"base":2,"body":"old"}}`},
		{"carried base 0", `{"rev":0,"base":2,"entry":"doc.md","body":"x","note":"","carried":{"base":0,"body":"old"}}`},
		{"carried missing body", `{"rev":0,"base":2,"entry":"doc.md","body":"x","note":"","carried":{"base":1}}`},
		{"bad clientSavedAt", `{"rev":0,"base":1,"entry":"doc.md","body":"x","note":"","clientSavedAt":"not-a-time"}`},
	}
	for _, tc := range badCases {
		// Reset limiter between validation probes so we test 400, not 429.
		f.svc.principalLimiter = newPrincipalLimiter(time.Now)
		rec := f.putDraftRaw(&userU, id, []byte(tc.body))
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "bad_request" {
			t.Errorf("%s: %d %s, want 400 bad_request", tc.name, rec.Code, rec.Body.String())
		}
	}

	// DELETE bad rev query values -> 400 bad_request.
	for _, q := range []string{"rev=0", "rev=-1", "rev=abc", "rev=", "rev=1&rev=2"} {
		f.svc.principalLimiter = newPrincipalLimiter(time.Now)
		if rec := f.deleteDraft(&userU, id, q); rec.Code != http.StatusBadRequest {
			t.Errorf("DELETE ?%s: %d %s, want 400", q, rec.Code, rec.Body.String())
		}
	}

	// 413 draft_too_large for body > MaxDraftBodyBytes and carried.body > MaxDraftBodyBytes.
	overBody := strings.Repeat("a", MaxDraftBodyBytes+1)
	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"body over 1 MiB", map[string]any{"rev": 0, "base": 2, "entry": "doc.md", "body": overBody, "note": ""}},
		{"carried.body over 1 MiB", map[string]any{
			"rev": 0, "base": 2, "entry": "doc.md", "body": "ok", "note": "",
			"carried": map[string]any{"base": 1, "body": overBody},
		}},
	} {
		f.svc.principalLimiter = newPrincipalLimiter(time.Now)
		raw, _ := json.Marshal(tc.payload)
		rec := f.putDraftRaw(&userU, id, raw)
		if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != CodeDraftTooLarge {
			t.Errorf("%s: %d %s, want 413 draft_too_large", tc.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"limit":1048576`) {
			t.Errorf("%s missing limit detail: %s", tc.name, rec.Body.String())
		}
	}

	// Exact boundary: MaxDraftBodyBytes (1 MiB) body and 2000-rune note succeed.
	f.svc.principalLimiter = newPrincipalLimiter(time.Now)
	exactPayload, _ := json.Marshal(map[string]any{
		"rev":   0,
		"base":  2,
		"entry": "doc.md",
		"body":  strings.Repeat("b", MaxDraftBodyBytes),
		"note":  strings.Repeat("é", maxNoteRunes),
	})
	if rec := f.putDraftRaw(&userU, id, exactPayload); rec.Code != http.StatusOK {
		t.Fatalf("exact limit PUT: %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := f.deleteDraft(&userU, id, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}

	// 429 rate_limited + Retry-After when exceeding LimitDraftWrite burst (12).
	f.svc.principalLimiter = newPrincipalLimiter(time.Now)
	burst := principalFamilyLimits[LimitDraftWrite].burst
	for i := 0; i < burst; i++ {
		raw, _ := json.Marshal(map[string]any{"rev": i, "base": 2, "entry": "doc.md", "body": "x", "note": ""})
		if rec := f.putDraftRaw(&userU, id, raw); rec.Code != http.StatusOK {
			t.Fatalf("burst write %d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	raw, _ := json.Marshal(map[string]any{"rev": burst, "base": 2, "entry": "doc.md", "body": "x", "note": ""})
	rec := f.putDraftRaw(&userU, id, raw)
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec) != "rate_limited" {
		t.Fatalf("over LimitDraftWrite: %d %s, want 429 rate_limited", rec.Code, rec.Body.String())
	}
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 {
		t.Errorf("Retry-After = %q, want positive integer", rec.Header().Get("Retry-After"))
	}
	// DELETE shares LimitDraftWrite; GET does not.
	if rec := f.deleteDraft(&userU, id, ""); rec.Code != http.StatusTooManyRequests {
		t.Errorf("DELETE while rate limited: %d, want 429", rec.Code)
	}
	if rec := f.getDraft(&userU, id); rec.Code != http.StatusOK {
		t.Errorf("GET while write-rate-limited: %d, want 200", rec.Code)
	}

	// 429 too_many_drafts when principal holds MaxDraftsPerPrincipal (200) drafts.
	f.svc.principalLimiter = newPrincipalLimiter(time.Now)
	ctx := context.Background()
	now := time.Now().UTC()
	// userU already holds 1 draft (on id); seed 199 more directly in store.
	for i := 1; i < MaxDraftsPerPrincipal; i++ {
		art, _, _, _ := seedArtifact(t, f.store, "")
		if _, err := f.store.PutReviewDraft(ctx, testDraft(art, userU.kind, userU.ref, 1, "doc.md", "s", ""), 0, now, MaxDraftsPerPrincipal); err != nil {
			t.Fatalf("seed draft %d: %v", i, err)
		}
	}
	extraID := f.publish(agentA, "extra.md", []byte("# extra\n"), "").Artifact.ID
	f.addGrant(extraID, userU, GrantReview)
	overCapPut, _ := json.Marshal(map[string]any{"rev": 0, "base": 1, "entry": "extra.md", "body": "201st", "note": ""})
	rec = f.putDraftRaw(&userU, extraID, overCapPut)
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec) != CodeTooManyDrafts {
		t.Fatalf("201st draft: %d %s, want 429 too_many_drafts", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "you have 200 saved draft reviews; submit or discard some") {
		t.Errorf("too_many_drafts message = %s", rec.Body.String())
	}
}

// TestReviewDraftCrossPrincipalAndAgentCreatorIsolation verifies C1 and S4 at
// the service boundary: no principal (owner, admin, project owner/steward,
// delegating creator of an agent, or the agent itself) can read, overwrite or
// delete another principal's draft, and a narrower credential that fails
// read/write checks gets 404/403 while leaving the stored draft intact.
func TestReviewDraftCrossPrincipalAndAgentCreatorIsolation(t *testing.T) {
	f, _, id := newReviewDraftFixture(t)
	// Record userU as sponsor/delegating creator of agentA, and make projectAdmin
	// an admin/owner of project-1 with an admin grant on id.
	projectAdmin := principal{PrincipalKindUser, "user-proj-admin", ""}
	f.host.allow(projectAdmin, "project-1", PermissionRead, PermissionCreate, PermissionManage)
	f.host.allowAdmin(projectAdmin, "project-1")
	f.addGrant(id, projectAdmin, GrantAdmin)
	f.execSQL(`UPDATE artifact SET sponsor_ref = ? WHERE id = ?`, userU.ref, id)

	// 1. Creator userU saves a draft; agentA (created/sponsored by userU),
	// agentB, and projectAdmin cannot see, overwrite, or delete userU's draft.
	uBody := "creator-private-draft"
	uPut, _ := json.Marshal(map[string]any{"rev": 0, "base": 1, "entry": "doc.md", "body": uBody, "note": "u-note"})
	if rec := f.putDraftRaw(&userU, id, uPut); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	for _, other := range []principal{agentA, agentB, projectAdmin} {
		got := decodeInto[ReviewDraftResponse](t, f.getDraft(&other, id))
		if got.Draft != nil {
			t.Errorf("%s saw userU's draft: %+v", other.ref, got.Draft)
		}
		// Sending rev=1 as other gets 409 (other has no row -> rev=0) and never
		// touches userU's row.
		tryOverwrite, _ := json.Marshal(map[string]any{"rev": 1, "base": 1, "entry": "doc.md", "body": "hijack", "note": ""})
		if rec := f.putDraftRaw(&other, id, tryOverwrite); rec.Code != http.StatusConflict || errCode(t, rec) != CodeDraftConflict {
			t.Errorf("%s PUT rev=1: %d %s, want 409 draft_conflict", other.ref, rec.Code, rec.Body.String())
		}
		// Unconditional DELETE by other deletes nothing of userU's.
		if rec := f.deleteDraft(&other, id, ""); rec.Code != http.StatusNoContent {
			t.Errorf("%s DELETE: %d", other.ref, rec.Code)
		}
	}
	stillU := decodeInto[ReviewDraftResponse](t, f.getDraft(&userU, id))
	if stillU.Draft == nil || stillU.Draft.Rev != 1 || stillU.Draft.Body != uBody {
		t.Fatalf("userU's draft changed after other principals' calls: %+v", stillU.Draft)
	}

	// 2. Reverse: agentA saves its own draft; its creator/sponsor userU and
	// projectAdmin only ever address their own rows, never agentA's.
	aBody := "agent-private-draft"
	aPut, _ := json.Marshal(map[string]any{"rev": 0, "base": 1, "entry": "doc.md", "body": aBody, "note": "a-note"})
	if rec := f.putDraftRaw(&agentA, id, aPut); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if rec := f.deleteDraft(&userU, id, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	gotUAfterDel := decodeInto[ReviewDraftResponse](t, f.getDraft(&userU, id))
	if gotUAfterDel.Draft != nil {
		t.Fatalf("userU draft after own DELETE = %+v, want nil", gotUAfterDel.Draft)
	}
	stillA := decodeInto[ReviewDraftResponse](t, f.getDraft(&agentA, id))
	if stillA.Draft == nil || stillA.Draft.Rev != 1 || stillA.Draft.Body != aBody {
		t.Fatalf("agentA's draft altered by creator userU: %+v", stillA.Draft)
	}

	// 3. Narrower credential / revoked permission (S4, C1): put userU's draft
	// back, then narrow userU's credential (first missing artifact.create ->
	// 403, then missing artifact.read / excluded project boundary -> 404).
	// Neither GET, PUT nor DELETE may delete the stored draft row.
	if rec := f.putDraftRaw(&userU, id, uPut); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	f.host.deny(userU, "project-1", PermissionCreate)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if rec := f.do(&userU, m, draftPath(id), uPut, nil); rec.Code != http.StatusForbidden {
			t.Errorf("without artifact.create %s: %d, want 403", m, rec.Code)
		}
	}
	f.host.deny(userU, "project-1", PermissionRead)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if rec := f.do(&userU, m, draftPath(id), uPut, nil); rec.Code != http.StatusNotFound {
			t.Errorf("without artifact.read %s: %d, want 404", m, rec.Code)
		}
	}
	// Restore userU's credential ceiling: the draft row survived intact!
	f.host.mu.Lock()
	delete(f.host.denied, PrincipalRef(userU.kind, userU.ref)+" project-1 "+PermissionRead)
	delete(f.host.denied, PrincipalRef(userU.kind, userU.ref)+" project-1 "+PermissionCreate)
	f.host.mu.Unlock()
	survivedRec := f.getDraft(&userU, id)
	if survivedRec.Code != http.StatusOK {
		t.Fatalf("GET after restoring credential: %d %s", survivedRec.Code, survivedRec.Body.String())
	}
	survived := decodeInto[ReviewDraftResponse](t, survivedRec)
	if survived.Draft == nil || survived.Draft.Rev != 1 || survived.Draft.Body != uBody {
		t.Fatalf("draft did not survive narrower-credential refusals: %+v", survived.Draft)
	}
}

// TestReviewDraftHostWithoutFeatureGateFailsClosed verifies C5 when the Host
// does not implement FeatureGate or returns false for unknown names.
func TestReviewDraftHostWithoutFeatureGateFailsClosed(t *testing.T) {
	f, h, id := newReviewDraftFixture(t)
	if h.FeatureEnabled(context.Background(), "hub.unknown_experiment") {
		t.Fatal("unknown experiment name enabled")
	}
	f.svc.host = f.host // plain fakeHost without FeatureGate
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := f.do(&userU, m, draftPath(id), []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"x","note":""}`), nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("host without FeatureGate %s: %d, want 404", m, rec.Code)
		}
	}
}

// TestReviewDraftNoContentInLogsOrErrors verifies C7 and S8: no fragment of a
// draft body, carried body or note ever appears in any decision-log line,
// slog output, artifact_event detail, or error/conflict HTTP response —
// including malformed JSON, unknown fields, trailing data, invalid UTF-8, NUL
// bytes, and oversize bodies.
func TestReviewDraftNoContentInLogsOrErrors(t *testing.T) {
	f, _, id := newReviewDraftFixture(t)
	v2 := bundle{"doc.md": []byte("# Plan v2\n")}
	f.publishBundle(agentA, "/api/v1/artifacts/"+id+"/versions", v2.manifest("doc.md"), v2)

	decisionCap := captureLog(f, slog.LevelDebug)
	slogCap := &logCapture{}
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(slogCap, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	const (
		bodySecret    = "SECRET_DRAFT_BODY_7116_ALPHA"
		carriedSecret = "SECRET_CARRIED_BODY_7116_BETA"
		noteSecret    = "SECRET_REVIEW_NOTE_7116_GAMMA"
		rawSecret     = "SECRET_RAW_PAYLOAD_7116_DELTA"
	)
	secrets := []string{bodySecret, carriedSecret, noteSecret, rawSecret}
	assertNoLeak := func(where, text string) {
		t.Helper()
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("%s leaked secret %q: %s", where, s, text)
			}
		}
	}

	// 1. Valid create + update with carried + conflict PUT + conflict DELETE + valid DELETE.
	createPayload, _ := json.Marshal(map[string]any{
		"rev": 0, "base": 2, "entry": "doc.md",
		"body": bodySecret, "note": noteSecret,
		"carried": map[string]any{"base": 1, "body": carriedSecret},
	})
	if rec := f.putDraftRaw(&userU, id, createPayload); rec.Code != http.StatusOK {
		t.Fatalf("create PUT: %d %s", rec.Code, rec.Body.String())
	}
	assertNoLeak("create PUT response", f.putDraftRaw(&userU, id, createPayload).Body.String()) // 409 conflict!
	if entries, raw := decisionCap.take(t); len(entries) != 0 || raw != "" {
		t.Errorf("PUTs wrote decision log entries: %s", raw)
	}

	delConflict := f.deleteDraft(&userU, id, "rev=99")
	if delConflict.Code != http.StatusConflict {
		t.Fatalf("DELETE ?rev=99: %d", delConflict.Code)
	}
	assertNoLeak("DELETE 409 response", delConflict.Body.String())

	delOK := f.deleteDraft(&userU, id, "rev=1")
	if delOK.Code != http.StatusNoContent {
		t.Fatalf("DELETE ?rev=1: %d", delOK.Code)
	}
	entries, rawLog := decisionCap.take(t)
	checkEntry(t, "draft deleted decision", one(t, "draft deleted", entries), "INFO", map[string]any{
		logKeyAction:        logActionReviewDraftDeleted,
		logKeyOutcome:       logOutcomeAllowed,
		logKeyRoute:         "DELETE /api/v1/artifacts/{id}/review-draft",
		logKeyPrincipalKind: userU.kind,
		logKeyPrincipalID:   userU.ref,
		logKeyArtifactID:    id,
		logKeyProjectID:     "project-1",
		logKeyPermission:    GrantReview,
		logKeyAuthority:     nil,
		logKeyVersionSeq:    nil,
	})
	assertNoLeak("DELETE decision log", rawLog)
	// Repeat DELETE when absent writes no decision entry.
	if rec := f.deleteDraft(&userU, id, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if entries, _ := decisionCap.take(t); len(entries) != 0 {
		t.Errorf("absent DELETE wrote decision entry: %v", entries)
	}

	// 2. Error inputs: malformed JSON, unknown field, trailing data, invalid
	// UTF-8, raw and escaped NUL, oversize body, oversize carried, oversize
	// HTTP envelope, over-length note, bad entry, bad clientSavedAt, bad query.
	errCases := []struct {
		name string
		raw  []byte
		want int
	}{
		{"malformed JSON", []byte(`{"rev":0,"base":1,"body":"` + bodySecret), http.StatusBadRequest},
		{"unknown field value", []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"x","note":"","extra":"` + rawSecret + `"}`), http.StatusBadRequest},
		{"unknown field name", []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"x","` + rawSecret + `":1}`), http.StatusBadRequest},
		{"trailing JSON", []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"x","note":""} "` + rawSecret + `"`), http.StatusBadRequest},
		{"raw invalid UTF-8 in body", []byte("{\"rev\":0,\"base\":1,\"entry\":\"doc.md\",\"body\":\"\xff" + bodySecret + "\",\"note\":\"" + noteSecret + "\"}"), http.StatusBadRequest},
		{"raw invalid UTF-8 in note", []byte("{\"rev\":0,\"base\":1,\"entry\":\"doc.md\",\"body\":\"ok\",\"note\":\"\xfe" + noteSecret + "\"}"), http.StatusBadRequest},
		{"raw NUL byte", []byte("{\"rev\":0,\"base\":1,\"entry\":\"doc.md\",\"body\":\"\x00" + bodySecret + "\",\"note\":\"\"}"), http.StatusBadRequest},
		{"JSON-escaped NUL in body", []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"\u0000` + bodySecret + `","note":""}`), http.StatusBadRequest},
		{"JSON-escaped NUL in carried", []byte(`{"rev":0,"base":2,"entry":"doc.md","body":"ok","note":"","carried":{"base":1,"body":"\u0000` + carriedSecret + `"}}`), http.StatusBadRequest},
		{"JSON-escaped NUL in note", []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"ok","note":"\u0000` + noteSecret + `"}`), http.StatusBadRequest},
		{"oversize body", []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"` + bodySecret + strings.Repeat("z", MaxDraftBodyBytes) + `","note":"` + noteSecret + `"}`), http.StatusRequestEntityTooLarge},
		{"oversize carried", []byte(`{"rev":0,"base":2,"entry":"doc.md","body":"ok","note":"` + noteSecret + `","carried":{"base":1,"body":"` + carriedSecret + strings.Repeat("z", MaxDraftBodyBytes) + `"}}`), http.StatusRequestEntityTooLarge},
		{"oversize envelope", []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"` + bodySecret + strings.Repeat("z", 3*MaxDraftBodyBytes) + `","note":"` + noteSecret + `"}`), http.StatusRequestEntityTooLarge},
		{"note over limit", []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"` + bodySecret + `","note":"` + noteSecret + strings.Repeat("n", maxNoteRunes) + `"}`), http.StatusBadRequest},
		{"wrong entry", []byte(`{"rev":0,"base":1,"entry":"` + rawSecret + `","body":"` + bodySecret + `","note":""}`), http.StatusBadRequest},
		{"bad clientSavedAt", []byte(`{"rev":0,"base":1,"entry":"doc.md","body":"` + bodySecret + `","note":"","clientSavedAt":"` + rawSecret + `"}`), http.StatusBadRequest},
	}
	for _, tc := range errCases {
		f.svc.principalLimiter = newPrincipalLimiter(time.Now)
		rec := f.putDraftRaw(&userU, id, tc.raw)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d (%s), want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
		assertNoLeak(tc.name+" HTTP response", rec.Body.String())
	}
	f.svc.principalLimiter = newPrincipalLimiter(time.Now)
	badDel := f.deleteDraft(&userU, id, "rev="+rawSecret)
	if badDel.Code != http.StatusBadRequest {
		t.Errorf("bad DELETE rev: %d", badDel.Code)
	}
	assertNoLeak("bad DELETE response", badDel.Body.String())

	// 3. TTL reaper and artifact delete log counts only, never content.
	now := time.Now().UTC()
	a1, _ := f.store.GetArtifact(context.Background(), id)
	if _, err := f.store.PutReviewDraft(context.Background(), ReviewDraft{
		ArtifactID: a1.ID, PrincipalKind: userU.kind, PrincipalRef: userU.ref,
		BaseSeq: 1, EntryPath: "doc.md", Body: bodySecret, Note: noteSecret,
	}, 0, now.Add(-31*24*time.Hour), MaxDraftsPerPrincipal); err != nil {
		t.Fatal(err)
	}
	reapLogger := slog.New(slog.NewJSONHandler(decisionCap, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reaped, err := ReapReviewDrafts(context.Background(), f.store, now, 100, reapLogger)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapReviewDrafts = %d, %v; want 1, nil", reaped, err)
	}
	reapEntries, reapRaw := decisionCap.take(t)
	checkEntry(t, "reap summary", one(t, "reap summary", reapEntries), "INFO", map[string]any{
		logKeyAction:        logActionDraftReapPass,
		logKeyOutcome:       logOutcomeAllowed,
		logKeyPrincipalKind: ActorKindSystem,
		logKeyPrincipalID:   AuthorityRetention,
		logKeyAuthority:     AuthorityRetention,
		logKeyCount:         1.0,
	})
	assertNoLeak("reap decision log", reapRaw)

	// Store error during reap logs a WARN summary with store_error and no content.
	if _, rerr := ReapReviewDrafts(context.Background(), draftReapFailStore{f.store}, now, 100, reapLogger); !errors.Is(rerr, errStoreDown) {
		t.Fatalf("ReapReviewDrafts fault = %v, want errStoreDown", rerr)
	}
	errEntries, errRaw := decisionCap.take(t)
	checkEntry(t, "reap fault summary", one(t, "reap fault", errEntries), "WARN", map[string]any{
		logKeyAction:  logActionDraftReapPass,
		logKeyOutcome: logOutcomeError,
		logKeyReason:  logReasonStoreError,
	})
	assertNoLeak("reap fault decision log", errRaw)

	_, slogRaw := slogCap.take(t)
	assertNoLeak("slog output", slogRaw)

	// Route template check.
	req := httptest.NewRequest(http.MethodPut, draftPath(uuid.NewString()), nil)
	if got := routeTemplate(req); got != "/api/v1/artifacts/{id}/review-draft" {
		t.Errorf("routeTemplate = %q", got)
	}
}

// TestReviewDraftNeverNotifiesOwner proves delta §1 / A4: draft PUT, GET and
// DELETE never invoke the review notifier; only a finalized review version
// invokes it once.
func TestReviewDraftNeverNotifiesOwner(t *testing.T) {
	rf := newReviewFixture(t)
	h := &reviewDraftHost{fakeHost: rf.host, on: true}
	rf.svc.host = h

	for rev := 0; rev < 3; rev++ {
		raw, _ := json.Marshal(map[string]any{
			"rev":   rev,
			"base":  1,
			"entry": "plan.md",
			"body":  reviewMarked,
			"note":  fmt.Sprintf("draft note %d", rev),
		})
		if rec := rf.putDraftRaw(&userU, rf.id, raw); rec.Code != http.StatusOK {
			t.Fatalf("PUT rev=%d: %d %s", rev, rec.Code, rec.Body.String())
		}
	}
	if rec := rf.getDraft(&userU, rf.id); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	if rec := rf.deleteDraft(&userU, rf.id, "rev=3"); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	rf.mu.Lock()
	gotAfterDrafts := len(rf.notices)
	rf.mu.Unlock()
	if gotAfterDrafts != 0 {
		t.Fatalf("draft PUT/GET/DELETE triggered %d review notifications, want 0", gotAfterDrafts)
	}

	// One finalized review version triggers exactly one ReviewNotice.
	_, res := rf.review(userU, bundle{"plan.md": []byte(reviewMarked), "chart.png": rf.v1["chart.png"]}, "plan.md")
	if res.code != http.StatusOK {
		t.Fatalf("finalize review: %d %s", res.code, res.body)
	}
	rf.mu.Lock()
	gotAfterSubmit := len(rf.notices)
	rf.mu.Unlock()
	if gotAfterSubmit != 1 {
		t.Fatalf("after one finalized review: %d notifications, want 1", gotAfterSubmit)
	}
}

// TestRefusedUnmarkedChangesLeavesReviewDraftUnchanged verifies the server-side
// contract of ptone/scion#7506 part 4 (delta §2.1 / A2 / A3):
//   - Draft PUT skips checkReview, so work with out-of-mark edits or zero marks
//     can be saved durably;
//   - When review finalize is refused with 422 unmarked_changes (or 409
//     stale_review), the saved draft row on the Hub keeps its rev, body and
//     note completely unchanged, and no review notification is emitted.
func TestRefusedUnmarkedChangesLeavesReviewDraftUnchanged(t *testing.T) {
	rf := newReviewFixture(t)
	h := &reviewDraftHost{fakeHost: rf.host, on: true}
	rf.svc.host = h

	// 1. Save a draft with zero marks (plain base text) -> allowed (rev 1).
	zeroMarksPut, _ := json.Marshal(map[string]any{
		"rev": 0, "base": 1, "entry": "plan.md", "body": reviewParent, "note": "",
	})
	if rec := rf.putDraftRaw(&userU, rf.id, zeroMarksPut); rec.Code != http.StatusOK {
		t.Fatalf("draft PUT with 0 marks: %d %s", rec.Code, rec.Body.String())
	}

	// 2. Save a draft containing both CriticMarkup marks AND an accidental
	// out-of-mark edit ("Q3" -> "Q4") -> allowed (rev 2), because draft PUT
	// skips checkReview.
	dirtyBody := "# Plan\n\nWe {~~ship~>launch~~} in Q4.{>>date?<<}\nOwners: docs.\n"
	dirtyNote := "in-progress review note"
	dirtyPut, _ := json.Marshal(map[string]any{
		"rev": 1, "base": 1, "entry": "plan.md", "body": dirtyBody, "note": dirtyNote,
	})
	rec := rf.putDraftRaw(&userU, rf.id, dirtyPut)
	if rec.Code != http.StatusOK {
		t.Fatalf("draft PUT with unmarked edits: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeInto[PutReviewDraftResponse](t, rec); got.Rev != 2 {
		t.Fatalf("draft rev = %d, want 2", got.Rev)
	}

	// 3. Attempting to finalize that text as a review version is refused with
	// 422 unmarked_changes naming plan.md and its changed line.
	_, res := rf.review(userU, bundle{"plan.md": []byte(dirtyBody), "chart.png": rf.v1["chart.png"]}, "plan.md")
	if res.code != http.StatusUnprocessableEntity {
		t.Fatalf("finalize with unmarked changes: %d %s, want 422", res.code, res.body)
	}
	if !strings.Contains(res.body, CodeUnmarkedChanges) || !strings.Contains(res.body, `"plan.md"`) {
		t.Errorf("422 response missing unmarked_changes details: %s", res.body)
	}

	// 4. The stored draft on the Hub is untouched: rev is still 2, body and
	// note are byte-exact.
	after422 := decodeInto[ReviewDraftResponse](t, rf.getDraft(&userU, rf.id))
	if after422.Draft == nil || after422.Draft.Rev != 2 || after422.Draft.Body != dirtyBody || after422.Draft.Note != dirtyNote {
		t.Fatalf("draft after 422 unmarked_changes = %+v, want rev=2 with intact body and note", after422.Draft)
	}

	// 5. Publish v2 as owner and attempt finalize on stale base 1 -> refused
	// 409 stale_review; stored draft rev=2 remains untouched as well.
	seq := rf.startReview(userU, bundle{"plan.md": []byte(reviewMarked), "chart.png": rf.v1["chart.png"]}, "plan.md")
	v2 := bundle{"plan.md": []byte("# Plan v2\n"), "chart.png": rf.v1["chart.png"]}
	rf.publishBundle(agentA, "/api/v1/artifacts/"+rf.id+"/versions", v2.manifest("plan.md"), v2)
	staleRec := rf.finalizeReview(userU, seq, 1)
	if staleRec.Code != http.StatusConflict || errCode(t, staleRec) != CodeStaleReview {
		t.Fatalf("stale finalize: %d %s, want 409 stale_review", staleRec.Code, staleRec.Body.String())
	}
	afterStale := decodeInto[ReviewDraftResponse](t, rf.getDraft(&userU, rf.id))
	if afterStale.Draft == nil || afterStale.Draft.Rev != 2 || afterStale.Draft.Body != dirtyBody || afterStale.Draft.Note != dirtyNote {
		t.Fatalf("draft after 409 stale_review = %+v, want rev=2 intact", afterStale.Draft)
	}

	rf.mu.Lock()
	notices := len(rf.notices)
	rf.mu.Unlock()
	if notices != 0 {
		t.Errorf("refused reviews emitted %d notifications, want 0", notices)
	}
}
