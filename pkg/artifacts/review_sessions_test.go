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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// openReviewHost adds the optional FeatureGate and PrincipalNamer to the
// test host.
type openReviewHost struct {
	*fakeHost
	gateMu sync.Mutex
	on     bool
	names  map[string]string
}

func (h *openReviewHost) FeatureEnabled(_ context.Context, name string) bool {
	h.gateMu.Lock()
	defer h.gateMu.Unlock()
	return h.on && name == FeatureOpenReviewCheck
}

func (h *openReviewHost) setOn(on bool) {
	h.gateMu.Lock()
	h.on = on
	h.gateMu.Unlock()
}

func (h *openReviewHost) PrincipalDisplayNames(_ context.Context, refs []string) map[string]string {
	out := map[string]string{}
	for _, r := range refs {
		if n, ok := h.names[r]; ok {
			out[r] = n
		}
	}
	return out
}

// gateOnlyHost has the feature gate (always on) but no namer.
type gateOnlyHost struct{ *fakeHost }

func (gateOnlyHost) FeatureEnabled(context.Context, string) bool { return true }

// readerR reads project-1 artifacts through the home scope grant and holds
// no grant on the fixture's artifact: it may neither publish to it nor
// review it.
var readerR = principal{PrincipalKindUser, "user-3", ""}

const sidA = "AAAAAAAAAAAAAAAAAAAAAA"

// newOpenReviewFixture publishes doc.md as agentA (v1) and gives userU a
// review grant and agentB a write grant, with the experiment on.
func newOpenReviewFixture(t *testing.T) (*fixture, *openReviewHost, string) {
	t.Helper()
	f := newFixture(t, false)
	f.host.allow(readerR, "project-1", PermissionRead, PermissionCreate)
	h := &openReviewHost{fakeHost: f.host, on: true, names: map[string]string{
		PrincipalRef(userU.kind, userU.ref):   "Una User",
		PrincipalRef(agentB.kind, agentB.ref): "builder-agent",
	}}
	f.svc.host = h
	id := f.publish(agentA, "doc.md", []byte("# Doc\n"), "").Artifact.ID
	f.addGrant(id, userU, GrantReview)
	f.addGrant(id, agentB, GrantWrite)
	return f, h, id
}

func (f *fixture) addGrant(id string, p principal, perm string) {
	f.t.Helper()
	f.execSQL(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), id, SubjectPrincipal, PrincipalRef(p.kind, p.ref), perm, time.Now().UTC().Format(sqliteTimeLayout))
}

func sessionPath(id, sid string) string { return "/api/v1/artifacts/" + id + "/review-sessions/" + sid }

func (f *fixture) beat(p *principal, id, sid, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(p, http.MethodPut, sessionPath(id, sid), []byte(body), map[string]string{"Content-Type": "application/json"})
}

const dirtyBeat = `{"base":1,"edited":true,"dirty":true}`

func publishV2(force bool) CreateVersionRequest {
	req := bundle{"doc.md": []byte("# Doc v2\n")}.manifest("doc.md")
	req.Force = force
	return req
}

func (f *fixture) versionRows(id string) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM artifact_version WHERE artifact_id = ?`, id).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) sessions(id string) []ReviewSession {
	f.t.Helper()
	got, err := f.store.ListOpenReviewSessions(context.Background(), id, time.Time{})
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

// TestReviewSessionAuthorization checks the order of checks on PUT and DELETE:
// the experiment, the session id, the caller, the read check, the write
// check, for PUT a published version, then the limiter, then the body. Refused callers create no
// limiter entry, and no refused caller's body is read.
func TestReviewSessionAuthorization(t *testing.T) {
	f, h, id := newOpenReviewFixture(t)
	lim := f.svc.principalLim()
	oversize := strings.Repeat("x", 4*maxReviewBeatBytes)

	// Experiment off: 404, nothing recorded, no limiter entry.
	h.setOn(false)
	if rec := f.beat(&userU, id, sidA, dirtyBeat); rec.Code != http.StatusNotFound {
		t.Errorf("experiment off: %d, want 404", rec.Code)
	}
	if rec := f.do(&userU, http.MethodDelete, sessionPath(id, sidA), nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("experiment off DELETE: %d, want 404", rec.Code)
	}
	h.setOn(true)

	cases := []struct {
		name string
		p    *principal
		sid  string
		body string
		want int
	}{
		{"unauthenticated", nil, sidA, dirtyBeat, http.StatusNotFound},
		{"cannot read", &outside, sidA, dirtyBeat, http.StatusNotFound},
		{"other project", &agentX, sidA, dirtyBeat, http.StatusNotFound},
		{"cannot read, oversize body", &outside, sidA, oversize, http.StatusNotFound},
		{"reader without review", &readerR, sidA, dirtyBeat, http.StatusForbidden},
		{"reader without review, oversize body", &readerR, sidA, oversize, http.StatusForbidden},
		{"short session id", &userU, "short", dirtyBeat, http.StatusNotFound},
		{"session id with a dot", &userU, "AAAAAAAAAAAAAAAAAAAAA.", dirtyBeat, http.StatusNotFound},
	}
	for _, c := range cases {
		if rec := f.beat(c.p, id, c.sid, c.body); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
	if n := lim.tracked(LimitReviewBeat); n != 0 {
		t.Errorf("refused callers created %d limiter entries", n)
	}
	if got := f.sessions(id); len(got) != 0 {
		t.Fatalf("refused beats recorded %d sessions", len(got))
	}

	// A review grantee, a write grantee and the owner may beat.
	for _, p := range []principal{userU, agentB, agentA} {
		rec := f.beat(&p, id, sidA, dirtyBeat)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"currentSeq":1}` {
			t.Errorf("%s beat: %d %s", p.ref, rec.Code, rec.Body.String())
		}
	}
	if n := lim.tracked(LimitReviewBeat); n != 3 {
		t.Errorf("%d limiter entries, want 3 (one per authorized principal)", n)
	}
	for _, f2 := range []LimitFamily{LimitReviewList, LimitDraftWrite} {
		if n := lim.tracked(f2); n != 0 {
			t.Errorf("beats created %d %s entries", n, f2)
		}
	}
	// An authorized caller's oversize body is refused after the limiter.
	if rec := f.beat(&userU, id, sidA, oversize); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("authorized oversize: %d, want 413", rec.Code)
	}

	// DELETE addresses only the caller's own row.
	if rec := f.do(&agentB, http.MethodDelete, sessionPath(id, sidA), nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("agentB DELETE: %d %s", rec.Code, rec.Body.String())
	}
	left := map[string]bool{}
	for _, s := range f.sessions(id) {
		left[s.PrincipalRef] = true
	}
	if !left[userU.ref] || !left[agentA.ref] || left[agentB.ref] {
		t.Errorf("after agentB's DELETE the sessions of %v remain", left)
	}
	if rec := f.do(&readerR, http.MethodDelete, sessionPath(id, sidA), nil, nil); rec.Code != http.StatusForbidden {
		t.Errorf("reader DELETE: %d, want 403", rec.Code)
	}
	if rec := f.do(&userU, http.MethodGet, sessionPath(id, sidA), nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d, want 405", rec.Code)
	}
	// A credential that does not permit publishing in the home project
	// cannot beat, whatever its grants.
	f.host.deny(userU, "project-1", PermissionCreate)
	if rec := f.beat(&userU, id, sidA, dirtyBeat); rec.Code != http.StatusForbidden {
		t.Errorf("credential without artifact.create: %d, want 403", rec.Code)
	}
}

func TestReviewSessionBeatValidation(t *testing.T) {
	f, _, id := newOpenReviewFixture(t)
	const marker = "MARKER-not-echoed"
	cases := []struct {
		name, body string
		want       int
	}{
		{"zero base", `{"base":0,"edited":false,"dirty":false}`, http.StatusBadRequest},
		{"newer than current", `{"base":2,"edited":false,"dirty":false}`, http.StatusBadRequest},
		{"unknown field", `{"base":1,"edited":false,"dirty":false,"who":"` + marker + `"}`, http.StatusBadRequest},
		{"malformed", `{"base":` + marker, http.StatusBadRequest},
		{"trailing data", `{"base":1} {"base":1}`, http.StatusBadRequest},
		{"invalid UTF-8", "{\"base\":1,\"x\":\"\xff" + marker + "\"}", http.StatusBadRequest},
		{"NUL", "{\"base\":1,\"x\":\"\x00" + marker + "\"}", http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := f.beat(&userU, id, sidA, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
		if strings.Contains(rec.Body.String(), marker) {
			t.Errorf("%s: the response echoes the body: %s", c.name, rec.Body.String())
		}
	}
	if got := f.sessions(id); len(got) != 0 {
		t.Errorf("invalid beats recorded %d sessions", len(got))
	}
	// An artifact with no ready version: 409 nothing_to_review.
	pend := f.createPending(agentA, "/api/v1/artifacts", bundle{"n.md": []byte("# n")}.manifest("n.md"))
	rec := f.beat(&agentA, pend.Artifact.ID, sidA, dirtyBeat)
	if rec.Code != http.StatusConflict || errCode(t, rec) != CodeNothingToReview {
		t.Errorf("no ready version: %d %s, want 409 nothing_to_review", rec.Code, rec.Body.String())
	}
}

func TestReviewSessionRateLimit(t *testing.T) {
	f, _, id := newOpenReviewFixture(t)
	burst := principalFamilyLimits[LimitReviewBeat].burst
	for i := 0; i < burst; i++ {
		if rec := f.beat(&userU, id, sidA, dirtyBeat); rec.Code != http.StatusOK {
			t.Fatalf("beat %d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := f.beat(&userU, id, sidA, dirtyBeat)
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec) != "rate_limited" {
		t.Fatalf("over the limit: %d %s, want 429 rate_limited", rec.Code, rec.Body.String())
	}
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 {
		t.Errorf("Retry-After %q", rec.Header().Get("Retry-After"))
	}
	// DELETE is in the same family.
	if rec := f.do(&userU, http.MethodDelete, sessionPath(id, sidA), nil, nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("DELETE over the limit: %d, want 429", rec.Code)
	}
	// Another principal is not limited.
	if rec := f.beat(&agentB, id, sidA, dirtyBeat); rec.Code != http.StatusOK {
		t.Errorf("another principal: %d", rec.Code)
	}
}

// TestReviewSessionEndLogged: a DELETE that deletes the caller's session
// row writes one INFO decision entry (the audit obligation of
// artifact.review.session.write), also for a row that no longer counts as
// open but is not reaped yet; a DELETE that finds no row answers 204 too
// and writes none; a beat writes none, the row-creating one included; and
// the entry never carries the session id.
func TestReviewSessionEndLogged(t *testing.T) {
	f, _, id := newOpenReviewFixture(t)
	c := captureLog(f, slog.LevelInfo)
	for i := 0; i < 2; i++ {
		if rec := f.beat(&userU, id, sidA, dirtyBeat); rec.Code != http.StatusOK {
			t.Fatalf("beat %d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if entries, _ := c.take(t); len(entries) != 0 {
		t.Errorf("beats wrote decision entries: %v", entries)
	}
	del := func(what string, p principal, sid string) ([]map[string]any, string) {
		t.Helper()
		if rec := f.do(&p, http.MethodDelete, sessionPath(id, sid), nil, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: DELETE %d %s", what, rec.Code, rec.Body.String())
		}
		return c.take(t)
	}
	ended := func(what string, p principal, sid string, want map[string]any) {
		t.Helper()
		entries, raw := del(what, p, sid)
		fields := map[string]any{
			logKeyAction:        logActionReviewEnded,
			logKeyOutcome:       logOutcomeAllowed,
			logKeyRoute:         "DELETE /api/v1/artifacts/{id}/review-sessions/{sid}",
			logKeyPrincipalKind: p.kind,
			logKeyPrincipalID:   p.ref,
			logKeyArtifactID:    id,
			logKeyProjectID:     "project-1",
			logKeyReason:        nil,
			logKeyVersionSeq:    nil,
		}
		for k, v := range want {
			fields[k] = v
		}
		checkEntry(t, what, one(t, what, entries), "INFO", fields)
		if strings.Contains(raw, sid) {
			t.Errorf("%s: the decision log carries the session id: %s", what, raw)
		}
	}
	ended("open session", userU, sidA, map[string]any{logKeyPermission: GrantReview, logKeyAuthority: nil})
	if got := f.sessions(id); len(got) != 0 {
		t.Errorf("after DELETE %d sessions remain", len(got))
	}
	// A DELETE that finds no row is answered 204 and not logged: a repeat,
	// and a session id that never beat.
	for _, tc := range []struct{ what, sid string }{{"repeat", sidA}, {"never beaten", sid(5)}} {
		if entries, _ := del(tc.what, userU, tc.sid); len(entries) != 0 {
			t.Errorf("%s: DELETE that found no row wrote %v", tc.what, entries)
		}
	}
	// A row last seen past ReviewLiveWindow no longer counts as open, but
	// the Hub keeps it until the reaper deletes it, at least
	// ReviewSessionRetention after its last beat: deleting it is a real
	// deletion and is logged.
	stale := time.Now().UTC().Add(-(ReviewLiveWindow + 10*time.Minute))
	if err := f.store.TouchReviewSession(context.Background(), ReviewSession{
		ArtifactID: id, PrincipalKind: userU.kind, PrincipalRef: userU.ref, SessionID: sid(6), BaseSeq: 1, Dirty: true,
	}, false, stale, MaxReviewSessionsPerPrincipal); err != nil {
		t.Fatal(err)
	}
	if open, err := f.store.ListOpenReviewSessions(context.Background(), id, time.Now().UTC().Add(-ReviewLiveWindow)); err != nil || len(open) != 0 {
		t.Fatalf("the stale row counts as open: %v, %v", open, err)
	}
	ended("stale row", userU, sid(6), map[string]any{logKeyPermission: GrantReview, logKeyAuthority: nil})
	if got := f.sessions(id); len(got) != 0 {
		t.Errorf("after the stale row's DELETE %d sessions remain", len(got))
	}
	// The owner ends with authority owner and no permission; its beat is
	// silent too.
	if rec := f.beat(&agentA, id, sidA, dirtyBeat); rec.Code != http.StatusOK {
		t.Fatalf("owner beat: %d %s", rec.Code, rec.Body.String())
	}
	if entries, _ := c.take(t); len(entries) != 0 {
		t.Errorf("the owner's beat wrote decision entries: %v", entries)
	}
	ended("owner", agentA, sidA, map[string]any{logKeyAuthority: logAuthorityOwner, logKeyPermission: nil})
}

func TestReviewSessionCapHandler(t *testing.T) {
	f, _, id := newOpenReviewFixture(t)
	for i := 0; i < MaxReviewSessionsPerPrincipal; i++ {
		if rec := f.beat(&userU, id, sid(i), dirtyBeat); rec.Code != http.StatusOK {
			t.Fatalf("session %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := f.beat(&userU, id, sid(99), dirtyBeat)
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec) != CodeTooManyReviewSessions {
		t.Fatalf("past the cap: %d %s, want 429 too_many_review_sessions", rec.Code, rec.Body.String())
	}
	if rec := f.beat(&userU, id, sid(0), dirtyBeat); rec.Code != http.StatusOK {
		t.Errorf("an existing session past the cap: %d", rec.Code)
	}
}

// reviewOpenBody is the decoded 409 review_open.
type reviewOpenBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Reviews   []ReviewOpenEntry `json:"reviews"`
			Truncated bool              `json:"truncated"`
			Base      int               `json:"base"`
		} `json:"details"`
	} `json:"error"`
}

func TestOpenReviewRefusalAndForce(t *testing.T) {
	f, _, id := newOpenReviewFixture(t)
	c := captureLog(f, slog.LevelInfo)
	if rec := f.beat(&userU, id, sidA, dirtyBeat); rec.Code != http.StatusOK {
		t.Fatalf("beat: %d %s", rec.Code, rec.Body.String())
	}
	before := time.Now().UTC()
	c.take(t)

	target := "/api/v1/artifacts/" + id + "/versions"
	rec := f.postJSON(&agentA, target, publishV2(false))
	if rec.Code != http.StatusConflict {
		t.Fatalf("publish over an open review: %d %s, want 409", rec.Code, rec.Body.String())
	}
	body := decodeInto[reviewOpenBody](t, rec)
	if body.Error.Code != CodeReviewOpen || body.Error.Details.Base != 1 || body.Error.Details.Truncated {
		t.Errorf("refusal %+v", body.Error)
	}
	if !strings.HasPrefix(body.Error.Message, "1 review of this artifact is open") {
		t.Errorf("message %q", body.Error.Message)
	}
	if len(body.Error.Details.Reviews) != 1 {
		t.Fatalf("reviews %+v", body.Error.Details.Reviews)
	}
	e := body.Error.Details.Reviews[0]
	if e.Reviewer != "Una User" || !e.UnsavedChanges || e.LastActivityAt.Second() != 0 || e.LastActivityAt.Nanosecond() != 0 ||
		e.LastActivityAt.After(before) || before.Sub(e.LastActivityAt) > 2*time.Minute {
		t.Errorf("entry %+v", e)
	}
	for _, id := range []string{userU.ref, sidA, PrincipalKindUser + ":"} {
		if strings.Contains(rec.Body.String(), id) {
			t.Errorf("the refusal carries %q: %s", id, rec.Body.String())
		}
	}
	if n := f.versionRows(id); n != 1 {
		t.Errorf("%d version rows after a refusal, want 1: nothing pending", n)
	}
	if entries, _ := c.take(t); len(entries) != 0 {
		t.Errorf("a refusal is not a decision entry: %v", entries)
	}

	// Forced: the create proceeds, with a warning and a count-only entry.
	rec = f.postJSON(&agentA, target, publishV2(true))
	if rec.Code != http.StatusCreated {
		t.Fatalf("forced publish: %d %s", rec.Code, rec.Body.String())
	}
	pend := decodeInto[PendingVersionResponse](t, rec)
	if len(pend.Warnings) != 1 || pend.Warnings[0] != "published while 1 review was open (Una User)" {
		t.Errorf("warnings %q", pend.Warnings)
	}
	entries, raw := c.take(t)
	var forced map[string]any
	for _, e := range entries {
		if e[logKeyAction] == logActionVersionForced {
			forced = e
		}
	}
	if forced == nil {
		t.Fatalf("no %s entry in %v", logActionVersionForced, entries)
	}
	checkEntry(t, "forced", forced, "INFO", map[string]any{logKeyCount: 1.0, logKeyArtifactID: id})
	if strings.Contains(raw, "Una User") || strings.Contains(raw, userU.ref) {
		t.Errorf("the decision log names the reviewer: %s", raw)
	}
	if rec := f.put(agentA, id, pend.Version.Seq, "doc.md", []byte("# Doc v2\n")); rec.Code != http.StatusNoContent {
		t.Fatalf("upload: %d", rec.Code)
	}
	if rec := f.finalize(agentA, id, pend.Version.Seq); rec.Code != http.StatusOK {
		t.Fatalf("finalize: %d %s", rec.Code, rec.Body.String())
	}

	// userU's session is now on an older base: it no longer counts, and
	// its beat answers the newer version.
	if rec := f.postJSON(&agentB, target, publishV2(false)); rec.Code != http.StatusCreated {
		t.Errorf("publish over a stale-base session: %d %s, want 201", rec.Code, rec.Body.String())
	}
	if rec := f.beat(&userU, id, sidA, dirtyBeat); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"currentSeq":2`) {
		t.Errorf("beat after the publish: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOpenReviewIgnoresOwnSessionsAndReviews(t *testing.T) {
	f, _, id := newOpenReviewFixture(t)
	target := "/api/v1/artifacts/" + id + "/versions"
	// The publisher's own session does not warn it.
	if rec := f.beat(&agentA, id, sidA, dirtyBeat); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	rec := f.postJSON(&agentA, target, publishV2(false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("own session: %d %s, want 201", rec.Code, rec.Body.String())
	}
	if w := decodeInto[PendingVersionResponse](t, rec).Warnings; len(w) != 0 {
		t.Errorf("own session warned: %q", w)
	}
	// Another reviewer's open session does not refuse a review.
	if rec := f.beat(&userU, id, sidA, dirtyBeat); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	review := publishV2(false)
	review.Kind = VersionKindReview
	if rec := f.postJSON(&agentB, target, review); rec.Code != http.StatusCreated {
		t.Errorf("review over an open review: %d %s, want 201", rec.Code, rec.Body.String())
	}
}

func TestOpenReviewKeyedAppendIsChecked(t *testing.T) {
	f, _, _ := newOpenReviewFixture(t)
	req := bundle{"k.md": []byte("# k\n")}.manifest("k.md")
	req.Key = "plan"
	art := f.publishBundle(agentA, "/api/v1/artifacts", req, bundle{"k.md": []byte("# k\n")})
	f.addGrant(art.Artifact.ID, userU, GrantReview)
	if rec := f.beat(&userU, art.Artifact.ID, sidA, dirtyBeat); rec.Code != http.StatusOK {
		t.Fatalf("beat: %d %s", rec.Code, rec.Body.String())
	}
	next := bundle{"k.md": []byte("# k2\n")}.manifest("k.md")
	next.Key = "plan"
	if rec := f.postJSON(&agentA, "/api/v1/artifacts", next); rec.Code != http.StatusConflict || errCode(t, rec) != CodeReviewOpen {
		t.Errorf("keyed append over an open review: %d %s, want 409 review_open", rec.Code, rec.Body.String())
	}
	next.Force = true
	if rec := f.postJSON(&agentA, "/api/v1/artifacts", next); rec.Code != http.StatusCreated {
		t.Errorf("forced keyed append: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOpenReviewExperimentOff(t *testing.T) {
	f, h, id := newOpenReviewFixture(t)
	if rec := f.beat(&userU, id, sidA, dirtyBeat); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	h.setOn(false)
	target := "/api/v1/artifacts/" + id + "/versions"
	rec := f.postJSON(&agentA, target, publishV2(false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("experiment off: %d %s, want 201", rec.Code, rec.Body.String())
	}
	if w := decodeInto[PendingVersionResponse](t, rec).Warnings; len(w) != 0 {
		t.Errorf("experiment off warned: %q", w)
	}
	// force is accepted and ignored.
	if rec := f.postJSON(&agentB, target, publishV2(true)); rec.Code != http.StatusCreated ||
		len(decodeInto[PendingVersionResponse](t, rec).Warnings) != 0 {
		t.Errorf("force with the experiment off: %d %s", rec.Code, rec.Body.String())
	}
}

// TestOpenReviewHostWithoutExtensions: without a namer every reviewer is
// "a reviewer"; without a gate the feature is off (fail closed).
func TestOpenReviewHostWithoutExtensions(t *testing.T) {
	f, _, id := newOpenReviewFixture(t)
	f.svc.host = gateOnlyHost{f.host}
	if rec := f.beat(&userU, id, sidA, dirtyBeat); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	rec := f.postJSON(&agentA, "/api/v1/artifacts/"+id+"/versions", publishV2(false))
	body := decodeInto[reviewOpenBody](t, rec)
	if rec.Code != http.StatusConflict || len(body.Error.Details.Reviews) != 1 || body.Error.Details.Reviews[0].Reviewer != unnamedReviewer {
		t.Errorf("no namer: %d %s", rec.Code, rec.Body.String())
	}
	f.svc.host = f.host
	if rec := f.beat(&userU, id, sidA, dirtyBeat); rec.Code != http.StatusNotFound {
		t.Errorf("no gate, beat: %d, want 404", rec.Code)
	}
	if rec := f.postJSON(&agentA, "/api/v1/artifacts/"+id+"/versions", publishV2(false)); rec.Code != http.StatusCreated {
		t.Errorf("no gate, publish: %d, want 201", rec.Code)
	}
}

func TestOpenReviewTruncatesAndOrders(t *testing.T) {
	f, h, id := newOpenReviewFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < maxReviewOpenListed+2; i++ {
		ref := "reviewer-" + strconv.Itoa(i)
		h.names[PrincipalRef(PrincipalKindUser, ref)] = "Reviewer " + strconv.Itoa(i)
		started := now.Add(-time.Duration(20-i) * time.Minute)
		if err := f.store.TouchReviewSession(ctx, ReviewSession{ArtifactID: id, PrincipalKind: PrincipalKindUser,
			PrincipalRef: ref, SessionID: sidA, BaseSeq: 1}, false, started, 8); err != nil {
			t.Fatal(err)
		}
		if err := f.store.TouchReviewSession(ctx, ReviewSession{ArtifactID: id, PrincipalKind: PrincipalKindUser,
			PrincipalRef: ref, SessionID: sidA, BaseSeq: 1}, false, now, 8); err != nil {
			t.Fatal(err)
		}
	}
	rec := f.postJSON(&agentA, "/api/v1/artifacts/"+id+"/versions", publishV2(false))
	body := decodeInto[reviewOpenBody](t, rec)
	if rec.Code != http.StatusConflict || !body.Error.Details.Truncated || len(body.Error.Details.Reviews) != maxReviewOpenListed {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(body.Error.Message, strconv.Itoa(maxReviewOpenListed+2)+" reviews of this artifact are open") {
		t.Errorf("message %q", body.Error.Message)
	}
	// Newest activity (the latest start) first.
	if got := body.Error.Details.Reviews[0].Reviewer; got != "Reviewer "+strconv.Itoa(maxReviewOpenListed+1) {
		t.Errorf("first entry %q", got)
	}
	rec = f.postJSON(&agentA, "/api/v1/artifacts/"+id+"/versions", publishV2(true))
	if w := decodeInto[PendingVersionResponse](t, rec).Warnings; len(w) != 1 || !strings.HasSuffix(w[0], "and 2 more)") ||
		!strings.HasPrefix(w[0], "published while 7 reviews were open (") {
		t.Errorf("forced warnings %q", w)
	}
}

// TestOpenReviewsWindow checks the active-window rule with a fixed clock.
func TestOpenReviewsWindow(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	at := func(d time.Duration) *time.Time { t := ago(d); return &t }
	mk := func(ref string, seen, started time.Duration, edit *time.Time, dirty bool, base int) ReviewSession {
		return ReviewSession{ArtifactID: "a", PrincipalKind: PrincipalKindUser, PrincipalRef: ref, SessionID: sidA,
			BaseSeq: base, StartedAt: ago(started), LastSeenAt: ago(seen), LastEditAt: edit, Dirty: dirty}
	}
	cases := []struct {
		name string
		s    ReviewSession
		want bool
	}{
		{"seen just now, dirty", mk("u", 0, 3*time.Hour, nil, true, 3), true},
		{"seen exactly 5 min ago", mk("u", ReviewLiveWindow, time.Minute, nil, true, 3), true},
		{"seen 5 min 1 s ago", mk("u", ReviewLiveWindow+time.Second, time.Minute, nil, true, 3), false},
		{"clean, started 29 min ago", mk("u", 0, 29*time.Minute, nil, false, 3), true},
		{"clean, started exactly 30 min ago", mk("u", 0, ReviewIdleWindow, nil, false, 3), true},
		{"clean, started 30 min 1 s ago", mk("u", 0, ReviewIdleWindow+time.Second, nil, false, 3), false},
		{"clean, old start, edited 10 min ago", mk("u", 0, 2*time.Hour, at(10*time.Minute), false, 3), true},
		{"clean, old start, edited 31 min ago", mk("u", 0, 2*time.Hour, at(31*time.Minute), false, 3), false},
		{"older base", mk("u", 0, time.Minute, nil, true, 2), false},
		{"the publisher's own", mk("pub", 0, time.Minute, nil, true, 3), false},
	}
	for _, c := range cases {
		got := openReviews([]ReviewSession{c.s}, 3, PrincipalKindUser, "pub", now)
		if (len(got) == 1) != c.want {
			t.Errorf("%s: counted=%v, want %v", c.name, len(got) == 1, c.want)
		}
	}
	// An agent with the publisher's ref is another principal.
	agent := mk("pub", 0, time.Minute, nil, true, 3)
	agent.PrincipalKind = PrincipalKindAgent
	if got := openReviews([]ReviewSession{agent}, 3, PrincipalKindUser, "pub", now); len(got) != 1 {
		t.Errorf("an agent with the publisher's ref was skipped")
	}
	// Sessions of one principal collapse: latest activity, dirty if any.
	clean := mk("u", 0, 20*time.Minute, at(2*time.Minute), false, 3)
	dirty := mk("u", 0, 25*time.Minute, nil, true, 3)
	dirty.SessionID = "BBBBBBBBBBBBBBBBBBBBBB"
	other := mk("v", 0, 10*time.Minute, nil, false, 3)
	got := openReviews([]ReviewSession{dirty, other, clean}, 3, PrincipalKindUser, "pub", now)
	if len(got) != 2 {
		t.Fatalf("%d reviews, want 2: %+v", len(got), got)
	}
	if got[0].ref != "user:u" || !got[0].dirty || !got[0].lastActivity.Equal(ago(2*time.Minute)) {
		t.Errorf("collapsed %+v", got[0])
	}
	if got[1].ref != "user:v" || got[1].dirty {
		t.Errorf("second %+v", got[1])
	}
}

func TestReviewSessionRouteTemplateHidesSessionID(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, sessionPath("4c1f0e8e-1c87-4d8a-9a2b-0f5f7f0b8b11", sidA), nil)
	if got := routeTemplate(r); got != "/api/v1/artifacts/{id}/review-sessions/{sid}" {
		t.Errorf("route template %q", got)
	}
}
