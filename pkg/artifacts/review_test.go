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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/critic"
)

const (
	reviewParent = "# Plan\n\nWe ship in Q3.\nOwners: docs.\n"
	reviewMarked = "# Plan\n\nWe {~~ship~>launch~~} in Q3.{>>date?<<}\nOwners: {==docs==}{>>which?<<}.{++\nRisks: none.++}\n"
)

// reviewFixture publishes v1 of a two-file artifact owned by agentA and
// gives userU a write grant, so userU can review it.
type reviewFixture struct {
	*fixture
	id      string
	v1      bundle
	notices []ReviewNotice
	mu      sync.Mutex
	// base is the current version when the last review was started.
	base int
}

func newReviewFixture(t *testing.T) *reviewFixture {
	f := &reviewFixture{fixture: newFixture(t, false)}
	f.v1 = bundle{"plan.md": []byte(reviewParent), "chart.png": []byte("\x89PNG\r\n\x1a\nfake")}
	pub := f.publishBundle(agentA, "/api/v1/artifacts", f.v1.manifest("plan.md"), f.v1)
	f.id = pub.Artifact.ID
	f.grant(userU, GrantWrite)
	f.svc.SetReviewNotifier(func(_ context.Context, n ReviewNotice) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.notices = append(f.notices, n)
	})
	return f
}

func (f *reviewFixture) grant(p principal, perm string) {
	f.t.Helper()
	if _, err := f.db.Exec("INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		"g-"+p.ref, f.id, SubjectPrincipal, PrincipalRef(p.kind, p.ref), perm, time.Now().UTC().Format(sqliteTimeLayout)); err != nil {
		f.t.Fatal(err)
	}
}

// finalizeReview finalizes review version seq naming base as the version it
// was started from.
func (f *reviewFixture) finalizeReview(p principal, seq, base int) *httptest.ResponseRecorder {
	f.t.Helper()
	body := []byte(fmt.Sprintf(`{"base":%d}`, base))
	return f.do(&p, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/finalize", f.id, seq), body, nil)
}

// startReview creates a pending review version and uploads its files. It
// records the current version as the review's base.
func (f *reviewFixture) startReview(p principal, files bundle, entry string) int {
	f.t.Helper()
	f.base = f.current()
	req := files.manifest(entry)
	req.Kind = VersionKindReview
	pend := f.createPending(p, "/api/v1/artifacts/"+f.id+"/versions", req)
	if pend.Version.Kind != VersionKindReview {
		f.t.Fatalf("pending kind %q", pend.Version.Kind)
	}
	for _, path := range pend.Upload.Required {
		if rec := f.put(p, f.id, pend.Version.Seq, path, files[path]); rec.Code != http.StatusNoContent {
			f.t.Fatalf("PUT %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	return pend.Version.Seq
}

func (f *reviewFixture) review(p principal, files bundle, entry string) (int, *httpResult) {
	seq := f.startReview(p, files, entry)
	return seq, f.result(f.finalizeReview(p, seq, f.base))
}

type httpResult struct {
	code int
	body string
}

func (f *reviewFixture) result(rec interface {
	Result() *http.Response
}) *httpResult {
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return &httpResult{code: resp.StatusCode, body: sb.String()}
}

func (f *reviewFixture) current() int {
	f.t.Helper()
	a, err := f.store.GetArtifact(context.Background(), f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	return a.CurrentSeq
}

func (f *reviewFixture) state(seq int) string {
	f.t.Helper()
	v, err := f.store.GetVersion(context.Background(), f.id, seq)
	if err != nil {
		f.t.Fatal(err)
	}
	return v.State
}

// unmarked decodes a 422 unmarked_changes body.
func unmarked(t *testing.T, r *httpResult) ([]UnmarkedFile, bool) {
	t.Helper()
	if r.code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", r.code, r.body)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Files     []UnmarkedFile `json:"files"`
				Truncated bool           `json:"truncated"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.body), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != CodeUnmarkedChanges {
		t.Fatalf("code %q", e.Error.Code)
	}
	return e.Error.Details.Files, e.Error.Details.Truncated
}

func with(b bundle, path, content string) bundle {
	out := bundle{}
	for k, v := range b {
		out[k] = v
	}
	out[path] = []byte(content)
	return out
}

func TestReviewAccepted(t *testing.T) {
	f := newReviewFixture(t)
	seq, r := f.review(userU, with(f.v1, "plan.md", reviewMarked), "plan.md")
	if r.code != http.StatusOK {
		t.Fatalf("finalize: %d %s", r.code, r.body)
	}
	if seq != 2 || f.current() != 2 {
		t.Fatalf("seq %d current %d, want 2 (a review becomes current)", seq, f.current())
	}
	// clean(review) is the parent, byte for byte.
	rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+f.id+"/files/plan.md?resolve=clean", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != reviewParent {
		t.Fatalf("clean: %d %q", rec.Code, rec.Body.String())
	}
	if len(f.notices) != 1 {
		t.Fatalf("notices %+v", f.notices)
	}
	n := f.notices[0]
	if n.ArtifactID != f.id || n.Seq != 2 || n.OwnerKind != agentA.kind || n.OwnerRef != agentA.ref ||
		n.ReviewerKind != userU.kind || n.ReviewerRef != userU.ref || n.Ref() != FormatRef(f.id, 2) {
		t.Fatalf("notice %+v", n)
	}
}

func TestReviewNormalisesLineEndingsAndNFC(t *testing.T) {
	f := newReviewFixture(t)
	crlf := strings.ReplaceAll(reviewMarked, "\n", "\r\n")
	if _, r := f.review(userU, with(f.v1, "plan.md", crlf), "plan.md"); r.code != http.StatusOK {
		t.Fatalf("CRLF review: %d %s", r.code, r.body)
	}
	// A decomposed é in the review equals a composed one in the parent.
	g := newReviewFixture(t)
	g.v1 = with(g.v1, "plan.md", "café\n")
	g.publishBundle(agentA, "/api/v1/artifacts/"+g.id+"/versions", g.v1.manifest("plan.md"), g.v1)
	if _, r := g.review(userU, with(g.v1, "plan.md", "cafe\u0301{>>ok<<}\n"), "plan.md"); r.code != http.StatusOK {
		t.Fatalf("NFD review: %d %s", r.code, r.body)
	}
}

func TestReviewRejectsUnmarkedChanges(t *testing.T) {
	f := newReviewFixture(t)
	edited := strings.Replace(reviewMarked, "Owners", "Owner", 1)
	seq, r := f.review(userU, with(f.v1, "plan.md", edited), "plan.md")
	files, truncated := unmarked(t, r)
	if truncated || len(files) != 1 || files[0].Path != "plan.md" || files[0].Change != ChangeModified ||
		len(files[0].Hunks) != 1 || files[0].Hunks[0].Line != 4 || files[0].Hunks[0].Parent != "Owners: docs.\n" ||
		files[0].Hunks[0].Clean != "Owner: docs.\n" {
		t.Fatalf("files %+v", files)
	}
	// current_seq unchanged, the pending version discarded, nobody told.
	if f.current() != 1 || f.state(seq) != VersionStateFailed {
		t.Fatalf("current %d state %q", f.current(), f.state(seq))
	}
	if rec := f.do(&userU, http.MethodGet, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", f.id, seq), nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("discarded version readable: %d", rec.Code)
	}
	if files, err := f.store.ListFiles(context.Background(), mustVersion(t, f, seq).ID); err != nil || len(files) != 0 {
		t.Fatalf("discarded manifest kept: %v %v", files, err)
	}
	if len(f.notices) != 0 {
		t.Fatalf("rejected review announced: %+v", f.notices)
	}
}

func mustVersion(t *testing.T, f *reviewFixture, seq int) *Version {
	t.Helper()
	v, err := f.store.GetVersion(context.Background(), f.id, seq)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReviewRejectsBundleChanges(t *testing.T) {
	cases := map[string]struct {
		files  func(bundle) bundle
		entry  string
		change string
		path   string
	}{
		"added file": {func(b bundle) bundle { return with(b, "extra.md", "new\n") }, "plan.md", ChangeAdded, "extra.md"},
		"removed file": {func(b bundle) bundle {
			out := with(b, "plan.md", reviewParent)
			delete(out, "chart.png")
			return out
		}, "plan.md", ChangeRemoved, "chart.png"},
		"changed non-text": {func(b bundle) bundle { return with(b, "chart.png", "\x89PNG\r\n\x1a\nother") }, "plan.md", ChangeModified, "chart.png"},
		"moved entry":      {func(b bundle) bundle { return b }, "chart.png", ChangeEntry, "chart.png"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newReviewFixture(t)
			_, r := f.review(userU, c.files(f.v1), c.entry)
			files, _ := unmarked(t, r)
			if len(files) != 1 || files[0].Change != c.change || files[0].Path != c.path || files[0].Hunks != nil {
				t.Fatalf("files %+v", files)
			}
			if f.current() != 1 {
				t.Fatalf("current %d", f.current())
			}
		})
	}
}

// TestReviewFileCap defends maxUnmarkedFiles: more differing files than the
// cap list exactly the cap and report truncation.
func TestReviewFileCap(t *testing.T) {
	f := newReviewFixture(t)
	big := bundle{}
	for i := 0; i < maxUnmarkedFiles+5; i++ {
		big[fmt.Sprintf("f%02d.txt", i)] = []byte("x\n")
	}
	big["plan.md"] = []byte(reviewParent)
	big["chart.png"] = f.v1["chart.png"]
	_, r := f.review(userU, big, "plan.md")
	files, truncated := unmarked(t, r)
	if len(files) != maxUnmarkedFiles || !truncated {
		t.Fatalf("%d files, truncated=%v; want %d, true", len(files), truncated, maxUnmarkedFiles)
	}
}

func TestReviewOfReview(t *testing.T) {
	f := newReviewFixture(t)
	if _, r := f.review(userU, with(f.v1, "plan.md", reviewMarked), "plan.md"); r.code != http.StatusOK {
		t.Fatalf("first review: %d %s", r.code, r.body)
	}
	// A second reviewer adds a mark to the marked copy: its baseline is
	// still v1.
	second := strings.Replace(reviewMarked, "# Plan", "# Plan{>>title ok<<}", 1)
	if _, r := f.review(userU, with(f.v1, "plan.md", second), "plan.md"); r.code != http.StatusOK {
		t.Fatalf("second review: %d %s", r.code, r.body)
	}
	// An unmarked edit on top of a review is still refused.
	third := strings.Replace(second, "Q3", "Q4", 1)
	_, r := f.review(userU, with(f.v1, "plan.md", third), "plan.md")
	if files, _ := unmarked(t, r); len(files) != 1 || files[0].Hunks[0].Parent != "We ship in Q3.\n" {
		t.Fatalf("files %+v", files)
	}
	if f.current() != 3 {
		t.Fatalf("current %d, want 3", f.current())
	}
}

func TestReviewIgnoresRemoteImageRows(t *testing.T) {
	f := newReviewFixture(t)
	v1 := mustVersion(t, f, 1)
	if _, err := f.db.Exec(`INSERT INTO artifact_file (version_id, path, size, media_type, origin, source_url, fetch_status, received)
		VALUES (?, '_remote/abc', 0, 'image/png', 'remote', 'https://example.com/a.png', 'failed', 1)`, v1.ID); err != nil {
		t.Fatal(err)
	}
	if _, r := f.review(userU, with(f.v1, "plan.md", reviewMarked), "plan.md"); r.code != http.StatusOK {
		t.Fatalf("review: %d %s", r.code, r.body)
	}
}

func TestReviewStale(t *testing.T) {
	f := newReviewFixture(t)
	seq := f.startReview(userU, with(f.v1, "plan.md", reviewMarked), "plan.md")
	// The owner publishes v3 while the review is pending.
	v3 := with(f.v1, "plan.md", reviewParent+"More.\n")
	f.publishBundle(agentA, "/api/v1/artifacts/"+f.id+"/versions", v3.manifest("plan.md"), v3)
	r := f.result(f.finalizeReview(userU, seq, 1))
	if r.code != http.StatusConflict || !strings.Contains(r.body, CodeStaleReview) {
		t.Fatalf("stale review: %d %s", r.code, r.body)
	}
	if f.current() != 3 || f.state(seq) != VersionStateFailed {
		t.Fatalf("current %d state %q", f.current(), f.state(seq))
	}
}

// staleBaseStore makes FinalizeVersion report that the current version
// moved after the review check, as a concurrent publish would.
type staleBaseStore struct{ Store }

func (s staleBaseStore) FinalizeVersion(ctx context.Context, id string, seq int, claim time.Time, extra []File, base int) (*Artifact, error) {
	if base > 0 {
		return nil, ErrStaleBase
	}
	return s.Store.FinalizeVersion(ctx, id, seq, claim, extra, base)
}

func TestReviewStaleAfterCheck(t *testing.T) {
	f := newReviewFixture(t)
	f.svc.SetStore(staleBaseStore{f.store})
	seq, r := f.review(userU, with(f.v1, "plan.md", reviewMarked), "plan.md")
	if r.code != http.StatusConflict || !strings.Contains(r.body, CodeStaleReview) {
		t.Fatalf("stale after check: %d %s", r.code, r.body)
	}
	if f.current() != 1 || f.state(seq) != VersionStateFailed {
		t.Fatalf("current %d state %q", f.current(), f.state(seq))
	}
}

// TestReviewCheckReadFailure: a review that cannot be checked is neither
// accepted nor discarded; the claim is released so finalize can be retried.
func TestReviewCheckReadFailure(t *testing.T) {
	f := newReviewFixture(t)
	blob := f.local.ObjectFSPath(BlobPath("hub-1", sha([]byte(reviewParent))))
	saved, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	seq, r := f.review(userU, with(f.v1, "plan.md", reviewMarked), "plan.md")
	if r.code != http.StatusInternalServerError {
		t.Fatalf("read failure: %d %s", r.code, r.body)
	}
	if f.current() != 1 || f.state(seq) != VersionStatePending {
		t.Fatalf("current %d state %q, want 1 pending", f.current(), f.state(seq))
	}
	if err := os.WriteFile(blob, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := f.result(f.finalizeReview(userU, seq, 1)); r.code != http.StatusOK {
		t.Fatalf("retry: %d %s", r.code, r.body)
	}
}

func TestReviewNeedsAPublishedVersion(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.md": []byte("a\n")}
	req := files.manifest("a.md")
	req.Kind = VersionKindReview
	req.Key = "fresh"
	rec := f.postJSON(&agentA, "/api/v1/artifacts", req)
	if rec.Code != http.StatusConflict || errCode(t, rec) != CodeNothingToReview {
		t.Fatalf("review creating an artifact: %d %s", rec.Code, rec.Body.String())
	}
	// An artifact whose first version is still pending has nothing to review.
	pub := files.manifest("a.md")
	pub.Key = "pending"
	pend := f.createPending(agentA, "/api/v1/artifacts", pub)
	rec = f.postJSON(&agentA, "/api/v1/artifacts/"+pend.Artifact.ID+"/versions", req2(files))
	if rec.Code != http.StatusConflict || errCode(t, rec) != CodeNothingToReview {
		t.Fatalf("review of pending artifact: %d %s", rec.Code, rec.Body.String())
	}
}

func req2(files bundle) CreateVersionRequest {
	r := files.manifest("a.md")
	r.Kind = VersionKindReview
	return r
}

func TestOwnReviewNotAnnounced(t *testing.T) {
	f := newReviewFixture(t)
	if _, r := f.review(agentA, with(f.v1, "plan.md", reviewMarked), "plan.md"); r.code != http.StatusOK {
		t.Fatalf("own review: %d %s", r.code, r.body)
	}
	if len(f.notices) != 0 {
		t.Fatalf("own review announced: %+v", f.notices)
	}
}

// TestReviewReadBeforeWrite: a principal with a write grant whose credential
// does not permit reading the artifact gets the missing-artifact 404 on
// every review step, so the 422 hunks (which quote the base) are reachable
// only by callers who can read it.
func TestReviewReadBeforeWrite(t *testing.T) {
	f := newReviewFixture(t)
	missing := f.do(&agentX, http.MethodPost, "/api/v1/artifacts/00000000-0000-4000-8000-000000000000/versions", []byte(`{}`), nil)
	f.grant(agentX, GrantWrite)
	f.host.allow(agentX, "project-1", PermissionCreate)
	f.host.deny(agentX, "project-1", PermissionRead)
	req := with(f.v1, "plan.md", "anything").manifest("plan.md")
	req.Kind = VersionKindReview
	rec := f.postJSON(&agentX, "/api/v1/artifacts/"+f.id+"/versions", req)
	if rec.Code != http.StatusNotFound || rec.Body.String() != missing.Body.String() {
		t.Fatalf("create: %d %q, want %q", rec.Code, rec.Body.String(), missing.Body.String())
	}
	// A pending review it somehow holds cannot be finalized either.
	delete(f.host.denied, PrincipalRef(agentX.kind, agentX.ref)+" project-1 "+PermissionRead)
	seq := f.startReview(agentX, with(f.v1, "plan.md", "edited\n"), "plan.md")
	f.host.deny(agentX, "project-1", PermissionRead)
	rec = f.finalizeReview(agentX, seq, 1)
	if rec.Code != http.StatusNotFound || rec.Body.String() != missing.Body.String() {
		t.Fatalf("finalize: %d %q", rec.Code, rec.Body.String())
	}
	if f.state(seq) != VersionStatePending {
		t.Fatalf("state %q", f.state(seq))
	}
}

// --- ?resolve= ---

func TestResolveProjections(t *testing.T) {
	f := newReviewFixture(t)
	if _, r := f.review(userU, with(f.v1, "plan.md", reviewMarked), "plan.md"); r.code != http.StatusOK {
		t.Fatalf("review: %d", r.code)
	}
	base := "/api/v1/artifacts/" + f.id + "/files/plan.md"
	for _, c := range []struct{ q, want, header string }{
		{"", reviewMarked, ""},
		{"?resolve=raw", reviewMarked, ""},
		{"?resolve=clean", reviewParent, "clean"},
		{"?resolve=accept", string(critic.AcceptText([]byte(reviewMarked))), "accept"},
	} {
		rec := f.do(&agentB, http.MethodGet, base+c.q, nil, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != c.want || rec.Header().Get(HeaderResolve) != c.header {
			t.Errorf("%q: %d %q header %q", c.q, rec.Code, rec.Body.String(), rec.Header().Get(HeaderResolve))
		}
		if c.header != "" {
			if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(c.want)) {
				t.Errorf("%q: Content-Length %s", c.q, got)
			}
			if !strings.HasSuffix(rec.Header().Get("ETag"), ";"+c.header+`"`) || rec.Header().Get("Content-Security-Policy") != fileCSP ||
				rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("%q: headers %v", c.q, rec.Header())
			}
		}
	}
	// A pinned version resolves too; v1 has no marks.
	rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+f.id+"/versions/1/files/plan.md?resolve=accept", nil, nil)
	if rec.Body.String() != reviewParent {
		t.Errorf("v1 accept: %q", rec.Body.String())
	}
	// HEAD carries the projected length and no body.
	rec = f.do(&agentB, http.MethodHead, base+"?resolve=clean", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != fmt.Sprint(len(reviewParent)) {
		t.Errorf("HEAD: %d len %d CL %s", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
	// Conditional request.
	etag := f.do(&agentB, http.MethodGet, base+"?resolve=clean", nil, nil).Header().Get("ETag")
	if rec := f.do(&agentB, http.MethodGet, base+"?resolve=clean", nil, map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", rec.Code)
	}
}

func TestResolveIgnoredForNonText(t *testing.T) {
	f := newReviewFixture(t)
	rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+f.id+"/files/chart.png?resolve=clean", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != string(f.v1["chart.png"]) || rec.Header().Get(HeaderResolve) != "" {
		t.Fatalf("png: %d header %q", rec.Code, rec.Header().Get(HeaderResolve))
	}
}

// TestResolveStreamsOnRedirectingStorage: a projection is produced by the
// hub, so it never redirects to the object store.
func TestResolveStreamsOnRedirectingStorage(t *testing.T) {
	f := newFixture(t, true)
	pub := f.publish(agentA, "doc.md", []byte("a{++b++}c"), "")
	base := "/api/v1/artifacts/" + pub.Artifact.ID + "/files/doc.md"
	if rec := f.do(&agentA, http.MethodGet, base, nil, nil); rec.Code != http.StatusFound {
		t.Fatalf("plain read: %d, want 302", rec.Code)
	}
	rec := f.do(&agentA, http.MethodGet, base+"?resolve=accept", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "abc" {
		t.Fatalf("resolve: %d %q", rec.Code, rec.Body.String())
	}
}

// TestResolveNoExistenceSignal: for a caller who cannot read the artifact,
// every resolve request answers exactly as for a missing artifact, and an
// invalid resolve value answers the same 400 whatever the id.
func TestResolveNoExistenceSignal(t *testing.T) {
	f := newReviewFixture(t)
	missingID := "00000000-0000-4000-8000-000000000000"
	for _, q := range []string{"?resolve=clean", "?resolve=accept", "?resolve=bogus"} {
		missing := f.do(&agentX, http.MethodGet, "/api/v1/artifacts/"+missingID+"/files/plan.md"+q, nil, nil)
		hidden := f.do(&agentX, http.MethodGet, "/api/v1/artifacts/"+f.id+"/files/plan.md"+q, nil, nil)
		if missing.Code != hidden.Code || missing.Body.String() != hidden.Body.String() {
			t.Errorf("%s: missing %d %q vs unreadable %d %q", q, missing.Code, missing.Body.String(), hidden.Code, hidden.Body.String())
		}
		for _, h := range []string{HeaderResolve, "ETag", "Content-Length", HeaderRemoteStatus} {
			if missing.Header().Get(h) != hidden.Header().Get(h) {
				t.Errorf("%s: header %s differs", q, h)
			}
		}
	}
	if rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+f.id+"/files/plan.md?resolve=bogus", nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bogus resolve for a reader: %d, want 400", rec.Code)
	}
}

// TestResolveBlobSizeMismatch: a stored blob whose length differs from the
// manifest fails loud instead of serving a partial projection.
func TestResolveBlobSizeMismatch(t *testing.T) {
	f := newFixture(t, false)
	pub := f.publish(agentA, "doc.md", []byte("hello"), "")
	blob := f.local.ObjectFSPath(BlobPath("hub-1", sha([]byte("hello"))))
	if err := os.WriteFile(blob, []byte("hello, longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+pub.Artifact.ID+"/files/doc.md?resolve=clean", nil, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("size mismatch: %d %q", rec.Code, rec.Body.String())
	}
}

// TestReviewBase covers the base a review names at finalize: it is
// required, it must be the current version, and no version newer than it
// may be pending, so a review is checked only against the version its
// reviewer started from and never becomes current over a newer version.
func TestReviewBase(t *testing.T) {
	t.Run("required", func(t *testing.T) {
		f := newReviewFixture(t)
		seq := f.startReview(userU, with(f.v1, "plan.md", reviewMarked), "plan.md")
		rec := f.finalize(userU, f.id, seq)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeBaseRequired {
			t.Fatalf("no base: %d %s", rec.Code, rec.Body.String())
		}
		if f.state(seq) != VersionStatePending {
			t.Fatalf("state %q, want pending (nothing claimed)", f.state(seq))
		}
	})
	t.Run("not the version it started from", func(t *testing.T) {
		f := newReviewFixture(t)
		v2 := with(f.v1, "plan.md", reviewParent+"More.\n")
		f.publishBundle(agentA, "/api/v1/artifacts/"+f.id+"/versions", v2.manifest("plan.md"), v2)
		// The reviewer marked up v1 but v2 is current now.
		seq := f.startReview(userU, with(f.v1, "plan.md", reviewMarked), "plan.md")
		r := f.result(f.finalizeReview(userU, seq, 1))
		if r.code != http.StatusConflict || !strings.Contains(r.body, CodeStaleReview) {
			t.Fatalf("wrong base: %d %s", r.code, r.body)
		}
		if f.current() != 2 || f.state(seq) != VersionStateFailed {
			t.Fatalf("current %d state %q", f.current(), f.state(seq))
		}
		// A stale review is refused as stale before its text is compared:
		// an unmarked edit in it does not turn the answer into a 422.
		edited := strings.Replace(reviewMarked, "Owners", "Owner", 1)
		seq = f.startReview(userU, with(f.v1, "plan.md", edited), "plan.md")
		r = f.result(f.finalizeReview(userU, seq, 1))
		if r.code != http.StatusConflict || !strings.Contains(r.body, CodeStaleReview) {
			t.Fatalf("stale review with an unmarked edit: %d %s", r.code, r.body)
		}
	})
	t.Run("a newer version is pending", func(t *testing.T) {
		f := newReviewFixture(t)
		// The owner starts v2 and has not finalized it when the review of
		// v1 (seq 3) is finalized.
		v2 := with(f.v1, "plan.md", reviewParent+"More.\n")
		pend := f.createPending(agentA, "/api/v1/artifacts/"+f.id+"/versions", v2.manifest("plan.md"))
		seq := f.startReview(userU, with(f.v1, "plan.md", reviewMarked), "plan.md")
		if pend.Version.Seq != 2 || seq != 3 {
			t.Fatalf("seqs %d %d", pend.Version.Seq, seq)
		}
		r := f.result(f.finalizeReview(userU, seq, 1))
		if r.code != http.StatusConflict || !strings.Contains(r.body, CodeStaleReview) ||
			!strings.Contains(r.body, "a newer version of the artifact is being published") {
			t.Fatalf("pending newer version: %d %s", r.code, r.body)
		}
		if f.current() != 1 || f.state(seq) != VersionStateFailed {
			t.Fatalf("current %d state %q", f.current(), f.state(seq))
		}
		// Once v2 is published, it is current; the review never shadowed it.
		for _, p := range pend.Upload.Required {
			if rec := f.put(agentA, f.id, 2, p, v2[p]); rec.Code != http.StatusNoContent {
				t.Fatalf("PUT: %d", rec.Code)
			}
		}
		if rec := f.finalize(agentA, f.id, 2); rec.Code != http.StatusOK {
			t.Fatalf("finalize v2: %d %s", rec.Code, rec.Body.String())
		}
		if f.current() != 2 {
			t.Fatalf("current %d, want 2", f.current())
		}
	})
	t.Run("an abandoned review does not block another", func(t *testing.T) {
		f := newReviewFixture(t)
		// A review of v1 is left pending (an upload that never finished).
		abandoned := f.startReview(userU, with(f.v1, "plan.md", reviewMarked), "plan.md")
		seq, r := f.review(userU, with(f.v1, "plan.md", reviewMarked), "plan.md")
		if r.code != http.StatusOK {
			t.Fatalf("second review: %d %s", r.code, r.body)
		}
		if f.current() != seq || f.state(abandoned) != VersionStatePending {
			t.Fatalf("current %d, abandoned %q", f.current(), f.state(abandoned))
		}
	})
	t.Run("base at or above the review's own seq", func(t *testing.T) {
		f := newReviewFixture(t)
		v2 := with(f.v1, "plan.md", reviewParent)
		v2["chart.png"] = []byte("\x89PNG\r\n\x1a\nother")
		f.publishBundle(agentA, "/api/v1/artifacts/"+f.id+"/versions", v2.manifest("plan.md"), v2)
		// Reviews seq 3 (marks only) and seq 4 (with an unmarked edit) are
		// made against v2; then v5, with the same text as v2, is published.
		seq := f.startReview(userU, with(v2, "plan.md", reviewMarked), "plan.md")
		edited := strings.Replace(reviewMarked, "Owners", "Owner", 1)
		seqEdited := f.startReview(userU, with(v2, "plan.md", edited), "plan.md")
		f.publishBundle(agentA, "/api/v1/artifacts/"+f.id+"/versions", v2.manifest("plan.md"), bundle{})
		if seq != 3 || seqEdited != 4 || f.current() != 5 {
			t.Fatalf("seqs %d %d current %d", seq, seqEdited, f.current())
		}
		// Naming v5 as the base would pass the text check, but the review
		// could only become a ready, non-current version under v5.
		r := f.result(f.finalizeReview(userU, seq, 5))
		if r.code != http.StatusConflict || !strings.Contains(r.body, CodeStaleReview) {
			t.Fatalf("base above the review: %d %s", r.code, r.body)
		}
		if f.current() != 5 || f.state(seq) != VersionStateFailed || len(f.notices) != 0 {
			t.Fatalf("current %d state %q notices %d", f.current(), f.state(seq), len(f.notices))
		}
		// The handler refuses it before comparing text: with an unmarked
		// edit it is still a 409, not a 422.
		r = f.result(f.finalizeReview(userU, seqEdited, 5))
		if r.code != http.StatusConflict || !strings.Contains(r.body, CodeStaleReview) {
			t.Fatalf("base above the review with an unmarked edit: %d %s", r.code, r.body)
		}
	})
	t.Run("invalid body answers the same for every id", func(t *testing.T) {
		f := newReviewFixture(t)
		missing := f.do(&agentX, http.MethodPost, "/api/v1/artifacts/00000000-0000-4000-8000-000000000000/versions/2/finalize", []byte(`{"base":-1}`), nil)
		hidden := f.do(&agentX, http.MethodPost, "/api/v1/artifacts/"+f.id+"/versions/2/finalize", []byte(`{"base":-1}`), nil)
		if missing.Code != http.StatusBadRequest || missing.Body.String() != hidden.Body.String() {
			t.Fatalf("missing %d %q vs unreadable %d %q", missing.Code, missing.Body.String(), hidden.Code, hidden.Body.String())
		}
	})
}
