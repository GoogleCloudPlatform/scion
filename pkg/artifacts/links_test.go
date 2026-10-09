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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- helpers ---

func linksPath(id string) string { return "/api/v1/artifacts/" + id + "/links" }

// mintLink creates a link on artifact id as p and returns the response.
func (f *fixture) mintLink(p principal, id string, body string) (*httptest.ResponseRecorder, CreateLinkResponse) {
	f.t.Helper()
	rec := f.do(&p, http.MethodPost, linksPath(id), []byte(body), map[string]string{"Content-Type": "application/json"})
	var resp CreateLinkResponse
	if rec.Code == http.StatusCreated {
		resp = decodeInto[CreateLinkResponse](f.t, rec)
	}
	return rec, resp
}

// mustMintLink creates a link and returns its token.
func (f *fixture) mustMintLink(p principal, id string, body string) (string, CreateLinkResponse) {
	f.t.Helper()
	rec, resp := f.mintLink(p, id, body)
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	token, ok := strings.CutPrefix(resp.URL, RouteShared)
	if !ok {
		f.t.Fatalf("link URL %q not under %s", resp.URL, RouteShared)
	}
	return token, resp
}

// shared performs an anonymous shared read from client addr.
func (f *fixture) shared(target, addr string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if addr != "" {
		r.RemoteAddr = addr
	}
	rec := httptest.NewRecorder()
	f.svc.ServeHTTP(rec, r)
	return rec
}

// newLinkFixture is a fixture with a view key, a fresh rate limiter per
// test (so tests do not share a budget) and userU's published artifact.
func newLinkFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t, false)
	f.svc.SetViewKey(testViewKey)
	f.svc.limiter = newRateLimiter(time.Now, 1000, 1000, 100000, 100000, 1000)
	id := f.publish(userU, "doc.md", []byte("# shared"), "scope=project-1").Artifact.ID
	return f, id
}

// response is the comparable part of a response: status, body and every
// header.
func response(rec *httptest.ResponseRecorder) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", rec.Code)
	for _, k := range []string{"Content-Type", "Cache-Control", "X-Content-Type-Options", "Referrer-Policy",
		"Location", "Content-Security-Policy", "Retry-After", "Content-Length", "Set-Cookie"} {
		fmt.Fprintf(&b, "%s: %q\n", k, rec.Header().Values(k))
	}
	fmt.Fprintf(&b, "headers: %d\n", len(rec.Header()))
	b.Write(rec.Body.Bytes())
	return b.String()
}

// --- tests ---

// TestLinkTokenShape: tokens are 256 bits from the CSPRNG in unpadded
// base64url, distinct, and stored only as their SHA-256.
func TestLinkTokenShape(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		tok, err := newLinkToken()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil || len(raw) != 32 || len(tok) != linkTokenLen || !validLinkToken(tok) {
			t.Fatalf("token %q: %d bytes, err %v", tok, len(raw), err)
		}
		if seen[tok] {
			t.Fatalf("token repeated")
		}
		seen[tok] = true
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 42), strings.Repeat("a", 44),
		strings.Repeat("a", 42) + "=", strings.Repeat("a", 42) + ".", strings.Repeat("a", 42) + "%"} {
		if validLinkToken(bad) {
			t.Errorf("validLinkToken(%q) = true", bad)
		}
	}
	if got := LinkTokenHash("abc"); got != sha([]byte("abc")) {
		t.Errorf("LinkTokenHash = %q", got)
	}
}

// TestLinkMintAndShare: the owner creates a link; an anonymous request
// with it is sent to the view route, which serves the entry with the view
// policy; the database holds the token's hash and never the token.
func TestLinkMintAndShare(t *testing.T) {
	f, id := newLinkFixture(t)
	before := time.Now()
	token, resp := f.mustMintLink(userU, id, "")
	if d := resp.Link.ExpiresAt.Sub(before); d < DefaultLinkTTL-time.Minute || d > DefaultLinkTTL+time.Minute {
		t.Errorf("default expiry in %v, want %v", d, DefaultLinkTTL)
	}
	if resp.ClampedToArtifactExpiry || resp.Link.CreatedBy != PrincipalRef(userU.kind, userU.ref) {
		t.Errorf("link = %+v", resp)
	}
	var refs []string
	rows, err := f.db.Query(`SELECT subject_ref FROM artifact_grant WHERE subject_kind = 'link'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		refs = append(refs, s)
	}
	_ = rows.Close()
	if len(refs) != 1 || refs[0] != LinkTokenHash(token) || strings.Contains(refs[0], token) {
		t.Fatalf("stored link refs %v", refs)
	}

	rec := f.shared(RouteShared+token, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("shared: %d %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, RouteView) || !strings.HasSuffix(loc, "/doc.md") || strings.Contains(loc, token) {
		t.Fatalf("Location %q", loc)
	}
	for k, want := range map[string]string{"Referrer-Policy": "no-referrer", "Cache-Control": "private, no-store"} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	view := f.shared(loc, "")
	if view.Code != http.StatusOK || view.Body.String() != "# shared" {
		t.Fatalf("view: %d %q", view.Code, view.Body.String())
	}
	capability := strings.Split(strings.TrimPrefix(loc, RouteView), "/")[0]
	if got, want := view.Header().Get("Content-Security-Policy"), viewCSP("example.com", capability); got != want {
		t.Errorf("CSP = %q, want the view policy %q", got, want)
	}
	if view.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("view Referrer-Policy %q", view.Header().Get("Referrer-Policy"))
	}
	// HEAD answers the same way.
	head := httptest.NewRequest(http.MethodHead, RouteShared+token, nil)
	hrec := httptest.NewRecorder()
	f.svc.ServeHTTP(hrec, head)
	if hrec.Code != http.StatusSeeOther {
		t.Errorf("HEAD shared: %d", hrec.Code)
	}
	if rec := f.do(nil, http.MethodPost, RouteShared+token, nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST shared: %d", rec.Code)
	}
}

// TestLinkSharesBundleFiles: /shared/{token}/files/{path} reaches any file
// of the current version through the view route, and a bundle's relative
// references resolve under the view path.
func TestLinkSharesBundleFiles(t *testing.T) {
	f := newFixture(t, false)
	f.svc.SetViewKey(testViewKey)
	f.svc.limiter = newRateLimiter(time.Now, 1000, 1000, 100000, 100000, 1000)
	req := htmlSite.manifest("index.html")
	req.Scope = "project-1"
	id := f.publishBundle(userU, "/api/v1/artifacts", req, htmlSite).Artifact.ID
	token, _ := f.mustMintLink(userU, id, "")

	loc := f.shared(RouteShared+token, "").Header().Get("Location")
	base := strings.TrimSuffix(loc, "index.html")
	if rec := f.shared(base+"img/a.png", ""); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), htmlSite["img/a.png"]) {
		t.Errorf("relative file under the view: %d", rec.Code)
	}
	rec := f.shared(RouteShared+token+"/files/css/s.css", "")
	if rec.Code != http.StatusSeeOther || !strings.HasSuffix(rec.Header().Get("Location"), "/css/s.css") {
		t.Fatalf("files route: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if v := f.shared(rec.Header().Get("Location"), ""); v.Code != http.StatusOK || v.Body.String() != "body{}" {
		t.Errorf("file via view: %d", v.Code)
	}
	for _, p := range []string{"/files/../index.html", "/files/", "/other"} {
		if rec := f.shared(RouteShared+token+p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
}

// recordingStore records which store methods a request called.
type recordingStore struct {
	Store
	mu    sync.Mutex
	calls []string
	// resolveHash, when set, replaces the subject ref ResolveLink returns.
	resolveHash string
	resolveErr  error
}

func (s *recordingStore) record(name string) {
	s.mu.Lock()
	s.calls = append(s.calls, name)
	s.mu.Unlock()
}

func (s *recordingStore) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.calls
	s.calls = nil
	return c
}

func (s *recordingStore) ResolveLink(ctx context.Context, hash string, now time.Time) (*Artifact, *Grant, error) {
	s.record("ResolveLink")
	if s.resolveErr != nil {
		return nil, nil, s.resolveErr
	}
	a, g, err := s.Store.ResolveLink(ctx, hash, now)
	if err == nil && s.resolveHash != "" {
		g.SubjectRef = s.resolveHash
	}
	return a, g, err
}

func (s *recordingStore) GetArtifact(ctx context.Context, id string) (*Artifact, error) {
	s.record("GetArtifact")
	return s.Store.GetArtifact(ctx, id)
}

func (s *recordingStore) GetVersion(ctx context.Context, id string, seq int) (*Version, error) {
	s.record("GetVersion")
	return s.Store.GetVersion(ctx, id, seq)
}

func (s *recordingStore) ListGrants(ctx context.Context, id string) ([]Grant, error) {
	s.record("ListGrants")
	return s.Store.ListGrants(ctx, id)
}

func (s *recordingStore) RevokeLink(ctx context.Context, artifactID, linkID string) error {
	s.record("RevokeLink")
	return s.Store.RevokeLink(ctx, artifactID, linkID)
}

func (s *recordingStore) LinkActive(ctx context.Context, artifactID, linkID string, now time.Time) (bool, error) {
	s.record("LinkActive")
	return s.Store.LinkActive(ctx, artifactID, linkID, now)
}

// TestSharedRefusalsAreUniform: an unknown, expired or revoked token, and
// a token of a deleted, expired or unpublished artifact, all get the same
// response, byte for byte, through the same path: one ResolveLink call
// and no other store access.
func TestSharedRefusalsAreUniform(t *testing.T) {
	f, id := newLinkFixture(t)
	unknown, _ := newLinkToken()
	rs := &recordingStore{Store: f.store}
	f.svc.SetStore(rs)
	baseline := f.shared(RouteShared+unknown, "")
	if baseline.Code != http.StatusNotFound {
		t.Fatalf("unknown token: %d", baseline.Code)
	}
	if got := rs.take(); len(got) != 1 || got[0] != "ResolveLink" {
		t.Fatalf("unknown token store calls %v, want [ResolveLink]", got)
	}
	want := response(baseline)

	setup := map[string]func(t *testing.T) string{
		"expired link": func(t *testing.T) string {
			tok, resp := f.mustMintLink(userU, id, "")
			f.exec(t, `UPDATE artifact_grant SET expires_at = ? WHERE id = ?`, linkPast(), resp.Link.ID)
			return tok
		},
		"revoked link": func(t *testing.T) string {
			tok, resp := f.mustMintLink(userU, id, "")
			if rec := f.do(&userU, http.MethodDelete, linksPath(id)+"/"+resp.Link.ID, nil, nil); rec.Code != http.StatusNoContent {
				t.Fatalf("revoke: %d", rec.Code)
			}
			return tok
		},
		"deleted artifact": func(t *testing.T) string {
			other := f.publish(userU, "d.md", []byte("d"), "scope=project-1").Artifact.ID
			tok, _ := f.mustMintLink(userU, other, "")
			f.exec(t, `UPDATE artifact SET deleted_at = ? WHERE id = ?`, linkPast(), other)
			return tok
		},
		"expired artifact": func(t *testing.T) string {
			other := f.publish(userU, "e.md", []byte("e"), "scope=project-1").Artifact.ID
			tok, _ := f.mustMintLink(userU, other, "")
			f.exec(t, `UPDATE artifact SET expires_at = ? WHERE id = ?`, linkPast(), other)
			return tok
		},
		"unpublished artifact": func(t *testing.T) string {
			other := f.publish(userU, "u.md", []byte("u"), "scope=project-1").Artifact.ID
			tok, _ := f.mustMintLink(userU, other, "")
			f.exec(t, `UPDATE artifact SET current_seq = NULL WHERE id = ?`, other)
			return tok
		},
		"write-permission link row": func(t *testing.T) string {
			tok, resp := f.mustMintLink(userU, id, "")
			f.exec(t, `UPDATE artifact_grant SET permission = 'write' WHERE id = ?`, resp.Link.ID)
			return tok
		},
		"link row without expiry": func(t *testing.T) string {
			tok, resp := f.mustMintLink(userU, id, "")
			f.exec(t, `UPDATE artifact_grant SET expires_at = NULL WHERE id = ?`, resp.Link.ID)
			return tok
		},
		"principal row with the token hash": func(t *testing.T) string {
			tok, resp := f.mustMintLink(userU, id, "")
			f.exec(t, `UPDATE artifact_grant SET subject_kind = 'principal' WHERE id = ?`, resp.Link.ID)
			return tok
		},
	}
	for name, mk := range setup {
		t.Run(name, func(t *testing.T) {
			f.svc.SetStore(f.store)
			tok := mk(t)
			f.svc.SetStore(rs)
			rs.take()
			rec := f.shared(RouteShared+tok, "")
			if got := response(rec); got != want {
				t.Errorf("response differs from an unknown token's:\n got %s\nwant %s", got, want)
			}
			if got := rs.take(); len(got) != 1 || got[0] != "ResolveLink" {
				t.Errorf("store calls %v, want [ResolveLink]", got)
			}
			if strings.Contains(rec.Body.String(), tok) {
				t.Errorf("body echoes the token")
			}
		})
	}
	// A malformed token never reaches the store, and answers the same.
	for _, bad := range []string{"abc", strings.Repeat("a", 44), strings.Repeat("!", 43)} {
		rec := f.shared(RouteShared+bad, "")
		if got := response(rec); got != want {
			t.Errorf("malformed %q: response differs:\n got %s\nwant %s", bad, got, want)
		}
		if got := rs.take(); len(got) != 0 {
			t.Errorf("malformed %q reached the store: %v", bad, got)
		}
	}
}

// TestSharedChecksTheHashItWasGiven: a row whose subject ref is not the
// request's hash (a store that matched loosely) is refused.
func TestSharedChecksTheHashItWasGiven(t *testing.T) {
	f, id := newLinkFixture(t)
	tok, _ := f.mustMintLink(userU, id, "")
	rs := &recordingStore{Store: f.store, resolveHash: LinkTokenHash("something else")}
	f.svc.SetStore(rs)
	if rec := f.shared(RouteShared+tok, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("mismatched hash: %d, want 404", rec.Code)
	}
}

// TestSharedNeverLogsTheToken: a failing lookup is logged and answered
// 500 without the token in the log or the body.
func TestSharedNeverLogsTheToken(t *testing.T) {
	f, id := newLinkFixture(t)
	tok, _ := f.mustMintLink(userU, id, "")
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	f.svc.SetStore(&recordingStore{Store: f.store, resolveErr: errors.New("db down")})
	rec := f.shared(RouteShared+tok, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failing store: %d", rec.Code)
	}
	if logs.Len() == 0 {
		t.Fatalf("the failure was not logged")
	}
	if strings.Contains(logs.String(), tok) || strings.Contains(rec.Body.String(), tok) ||
		strings.Contains(logs.String(), LinkTokenHash(tok)) {
		t.Errorf("token or its hash in the log or body:\n%s\n%s", logs.String(), rec.Body.String())
	}
}

// TestLinkEndsViewAccess: a view capability obtained through a link stops
// working as soon as the link is revoked or expires, or the artifact is
// deleted or expires.
func TestLinkEndsViewAccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		kill func(f *fixture, id string, link CreateLinkResponse)
	}{
		{"revoked", func(f *fixture, id string, link CreateLinkResponse) {
			if rec := f.do(&userU, http.MethodDelete, linksPath(id)+"/"+link.Link.ID, nil, nil); rec.Code != http.StatusNoContent {
				f.t.Fatalf("revoke: %d", rec.Code)
			}
		}},
		{"link expired", func(f *fixture, id string, link CreateLinkResponse) {
			f.exec(f.t, `UPDATE artifact_grant SET expires_at = ? WHERE id = ?`, linkPast(), link.Link.ID)
		}},
		{"artifact deleted", func(f *fixture, id string, link CreateLinkResponse) {
			f.exec(f.t, `UPDATE artifact SET deleted_at = ? WHERE id = ?`, linkPast(), id)
		}},
		{"artifact expired", func(f *fixture, id string, link CreateLinkResponse) {
			f.exec(f.t, `UPDATE artifact SET expires_at = ? WHERE id = ?`, linkPast(), id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, id := newLinkFixture(t)
			tok, link := f.mustMintLink(userU, id, "")
			loc := f.shared(RouteShared+tok, "").Header().Get("Location")
			if rec := f.shared(loc, ""); rec.Code != http.StatusOK {
				t.Fatalf("view before: %d", rec.Code)
			}
			tc.kill(f, id, link)
			if rec := f.shared(loc, ""); rec.Code != http.StatusNotFound {
				t.Errorf("view after: %d, want 404", rec.Code)
			}
			if rec := f.shared(RouteShared+tok, ""); rec.Code != http.StatusNotFound {
				t.Errorf("shared after: %d, want 404", rec.Code)
			}
		})
	}
}

// TestLinkViewCapabilityBounds: the capability a link yields expires no
// later than the link, and cannot be turned into a session capability or
// moved to another link.
func TestLinkViewCapabilityBounds(t *testing.T) {
	f, id := newLinkFixture(t)
	tok, link := f.mustMintLink(userU, id, `{"ttlHours": 1}`)
	f.exec(t, `UPDATE artifact_grant SET expires_at = ? WHERE id = ?`,
		time.Now().Add(10*time.Minute).UTC().Format(sqliteTimeLayout), link.Link.ID)
	loc := f.shared(RouteShared+tok, "").Header().Get("Location")
	capability := strings.Split(strings.TrimPrefix(loc, RouteView), "/")[0]
	vg, err := parseViewCapability(testViewKey, capability, time.Now())
	if err != nil || vg.linkID != link.Link.ID || vg.id != id || vg.seq != 1 {
		t.Fatalf("capability %q: %+v %v", capability, vg, err)
	}
	parts := strings.Split(capability, ".")
	exp, _ := strconv.ParseInt(parts[2], 10, 64)
	if until := time.Until(time.Unix(exp, 0)); until > 10*time.Minute {
		t.Errorf("capability outlives its link: expires in %v", until)
	}
	// Dropping the link id does not yield a valid session capability.
	session := strings.Join([]string{parts[0], parts[1], parts[2], parts[4]}, ".")
	if _, err := parseViewCapability(testViewKey, session, time.Now()); err == nil {
		t.Errorf("a link capability without its link id was accepted")
	}
	// Swapping in another link id breaks the signature.
	_, other := f.mustMintLink(userU, id, "")
	moved := strings.Join([]string{parts[0], parts[1], parts[2], other.Link.ID, parts[4]}, ".")
	if _, err := parseViewCapability(testViewKey, moved, time.Now()); err == nil {
		t.Errorf("a link capability moved to another link was accepted")
	}
	// A session capability with a link id inserted is refused.
	sess := mintViewCapability(testViewKey, id, 1, time.Now().Add(ViewTTL))
	sp := strings.Split(sess, ".")
	forged := strings.Join([]string{sp[0], sp[1], sp[2], link.Link.ID, sp[3]}, ".")
	if _, err := parseViewCapability(testViewKey, forged, time.Now()); err == nil {
		t.Errorf("a session capability with a link id was accepted")
	}
	// A 5-part capability signed under the session domain over the same
	// fields (link id included) is refused: link capabilities have their
	// own domain.
	mac := hmac.New(sha256.New, testViewKey)
	mac.Write([]byte(viewSigDomain + "\n" + parts[0] + "\n" + parts[1] + "\n" + parts[2] + "\n" + parts[3]))
	crossDomain := strings.Join([]string{parts[0], parts[1], parts[2], parts[3],
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}, ".")
	if _, err := parseViewCapability(testViewKey, crossDomain, time.Now()); err == nil {
		t.Errorf("a link capability signed under the session domain was accepted")
	}
	// A link id that is not a canonical id is refused, even correctly
	// signed, so it never reaches the store.
	for _, bad := range []string{"x", strings.ToUpper(link.Link.ID), link.Link.ID + "0"} {
		signed := mintLinkViewCapability(testViewKey, id, 1, time.Now().Add(ViewTTL), bad)
		if _, err := parseViewCapability(testViewKey, signed, time.Now()); err == nil {
			t.Errorf("link id %q was accepted", bad)
		}
	}
}

// TestLinkViewCapabilityEndsWithTheArtifact: the capability a link yields
// expires no later than the artifact.
func TestLinkViewCapabilityEndsWithTheArtifact(t *testing.T) {
	f, id := newLinkFixture(t)
	tok, _ := f.mustMintLink(userU, id, "")
	f.exec(t, `UPDATE artifact SET expires_at = ? WHERE id = ?`, time.Now().Add(5*time.Minute).UTC().Format(sqliteTimeLayout), id)
	loc := f.shared(RouteShared+tok, "").Header().Get("Location")
	parts := strings.Split(strings.Split(strings.TrimPrefix(loc, RouteView), "/")[0], ".")
	if len(parts) != 5 {
		t.Fatalf("Location %q", loc)
	}
	exp, _ := strconv.ParseInt(parts[2], 10, 64)
	if until := time.Until(time.Unix(exp, 0)); until > 5*time.Minute {
		t.Errorf("capability outlives the artifact: expires in %v", until)
	}
}

// TestLinkManagePermissions: reading comes first (an unreadable artifact
// answers like a missing one, whatever the body), then the manage check:
// owner or admin grant, a user, and a credential that permits
// artifact.manage.
func TestLinkManagePermissions(t *testing.T) {
	f, id := newLinkFixture(t)
	missing := f.do(&outside, http.MethodPost, linksPath("00000000-0000-4000-8000-000000000001"), nil, nil)
	for _, body := range []string{"", "not json", `{"ttlHours": 99999}`} {
		rec := f.do(&outside, http.MethodPost, linksPath(id), []byte(body), nil)
		if response(rec) != response(missing) {
			t.Errorf("unreadable artifact, body %q: %s, want the missing artifact's answer", body, response(rec))
		}
	}
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		if rec := f.do(&outside, m, linksPath(id), nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s by a non-reader: %d", m, rec.Code)
		}
	}
	if rec := f.do(&outside, http.MethodDelete, linksPath(id)+"/00000000-0000-4000-8000-000000000002", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE by a non-reader: %d", rec.Code)
	}

	// agentA reads the artifact (project member) but is not its owner.
	if rec, _ := f.mintLink(agentA, id, ""); rec.Code != http.StatusForbidden {
		t.Errorf("reader agent: %d, want 403", rec.Code)
	}
	// A reading user without admin.
	reader := principal{PrincipalKindUser, "user-3", ""}
	f.host.allow(reader, "project-1", PermissionRead)
	if rec, _ := f.mintLink(reader, id, ""); rec.Code != http.StatusForbidden {
		t.Errorf("reader user: %d, want 403", rec.Code)
	}
	// An admin principal grant makes it a manager.
	f.exec(t, `INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at)
		VALUES ('admin-grant', ?, 'principal', ?, 'admin', ?)`, id, PrincipalRef(reader.kind, reader.ref), linkNow())
	if rec, _ := f.mintLink(reader, id, ""); rec.Code != http.StatusCreated {
		t.Errorf("admin grant: %d, want 201", rec.Code)
	}
	// ... while it is unexpired.
	f.exec(t, `UPDATE artifact_grant SET expires_at = ? WHERE id = 'admin-grant'`, linkPast())
	if rec, _ := f.mintLink(reader, id, ""); rec.Code != http.StatusForbidden {
		t.Errorf("expired admin grant: %d, want 403", rec.Code)
	}
	// A write grant is not admin.
	f.exec(t, `UPDATE artifact_grant SET expires_at = NULL, permission = 'write' WHERE id = 'admin-grant'`)
	if rec, _ := f.mintLink(reader, id, ""); rec.Code != http.StatusForbidden {
		t.Errorf("write grant: %d, want 403", rec.Code)
	}
	// A scope admin grant works for a user the host authorizes to manage
	// in that scope, and not otherwise.
	f.exec(t, `UPDATE artifact_grant SET subject_kind = 'scope', subject_ref = 'project-9', permission = 'admin' WHERE id = 'admin-grant'`)
	if rec, _ := f.mintLink(reader, id, ""); rec.Code != http.StatusForbidden {
		t.Errorf("scope admin grant without manage in the scope: %d, want 403", rec.Code)
	}
	f.host.allow(reader, "project-9", PermissionManage)
	if rec, _ := f.mintLink(reader, id, ""); rec.Code != http.StatusCreated {
		t.Errorf("scope admin grant with manage in the scope: %d, want 201", rec.Code)
	}

	// The owner, when the credential does not permit artifact.manage.
	f.host.deny(userU, "project-1", PermissionManage)
	if rec, _ := f.mintLink(userU, id, ""); rec.Code != http.StatusForbidden {
		t.Errorf("owner without artifact.manage: %d, want 403", rec.Code)
	}
	for _, m := range []string{http.MethodGet} {
		if rec := f.do(&userU, m, linksPath(id), nil, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s by owner without artifact.manage: %d", m, rec.Code)
		}
	}

	// An agent never creates links, even on its own artifact (design D15).
	own := f.publish(agentA, "mine.md", []byte("m"), "").Artifact.ID
	if rec, _ := f.mintLink(agentA, own, ""); rec.Code != http.StatusForbidden {
		t.Errorf("agent owner: %d, want 403", rec.Code)
	}
}

// TestLinkGrantReadFailureIsLoud: a failed grant read answers 500, never
// a 403 a working read would not give.
func TestLinkGrantReadFailureIsLoud(t *testing.T) {
	f, id := newLinkFixture(t)
	reader := principal{PrincipalKindUser, "user-3", ""}
	f.host.allow(reader, "project-1", PermissionRead)
	f.svc.SetStore(failGrantsStore{f.store})
	if rec, _ := f.mintLink(reader, id, ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("failed grant read: %d, want 500", rec.Code)
	}
}

type failGrantsStore struct{ Store }

func (failGrantsStore) ListGrants(context.Context, string) ([]Grant, error) {
	return nil, errors.New("grants unavailable")
}

// TestLinkLifetimes: the default and maximum come from the limits; a
// longer lifetime is refused; the artifact's own expiry clamps the link.
func TestLinkLifetimes(t *testing.T) {
	f, id := newLinkFixture(t)
	f.svc.SetLimits(func(context.Context) Limits { return Limits{LinkDefaultTTL: 2 * time.Hour, LinkMaxTTL: 5 * time.Hour} })
	_, resp := f.mustMintLink(userU, id, "")
	if d := time.Until(resp.Link.ExpiresAt); d < 2*time.Hour-time.Minute || d > 2*time.Hour {
		t.Errorf("default from limits: expires in %v", d)
	}
	_, resp = f.mustMintLink(userU, id, `{"ttlHours": 5}`)
	if d := time.Until(resp.Link.ExpiresAt); d < 5*time.Hour-time.Minute || d > 5*time.Hour {
		t.Errorf("ttlHours 5: expires in %v", d)
	}
	for _, body := range []string{`{"ttlHours": 6}`, `{"ttlHours": -1}`, `{"ttl": 3}`, `{"ttlHours": 1} {}`, `[`} {
		if rec, _ := f.mintLink(userU, id, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: %d, want 400", body, rec.Code)
		}
	}
	if rec := f.do(&userU, http.MethodPost, linksPath(id), bytes.Repeat([]byte(" "), maxLinkRequestBytes+1), nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d", rec.Code)
	}
	// A default above the maximum is lowered to it.
	f.svc.SetLimits(func(context.Context) Limits { return Limits{LinkDefaultTTL: 9 * time.Hour, LinkMaxTTL: 3 * time.Hour} })
	_, resp = f.mustMintLink(userU, id, "")
	if d := time.Until(resp.Link.ExpiresAt); d > 3*time.Hour {
		t.Errorf("default above max: expires in %v", d)
	}
	// No limits: the compiled maximum applies.
	f.svc.SetLimits(nil)
	if rec, _ := f.mintLink(userU, id, `{"ttlHours": `+strconv.Itoa(int(DefaultLinkMaxTTL/time.Hour)+1)+`}`); rec.Code != http.StatusBadRequest {
		t.Errorf("beyond the compiled maximum: %d", rec.Code)
	}
	if rec, _ := f.mintLink(userU, id, `{"ttlHours": `+strconv.Itoa(int(DefaultLinkMaxTTL/time.Hour))+`}`); rec.Code != http.StatusCreated {
		t.Errorf("at the compiled maximum: %d", rec.Code)
	}

	// The artifact expires in an hour: a link asked for longer is cut to
	// that and says so.
	artExp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	f.exec(t, `UPDATE artifact SET expires_at = ? WHERE id = ?`, artExp.Format(sqliteTimeLayout), id)
	_, resp = f.mustMintLink(userU, id, `{"ttlHours": 24}`)
	if !resp.ClampedToArtifactExpiry || !resp.Link.ExpiresAt.Equal(artExp) {
		t.Errorf("clamp: %+v, want expiry %v", resp, artExp)
	}
	_, resp = f.mustMintLink(userU, id, `{"ttlHours": 0}`)
	if !resp.ClampedToArtifactExpiry {
		t.Errorf("default lifetime past the artifact's expiry was not clamped")
	}
}

// TestLinkUnpublishedArtifact: an artifact with no ready version cannot be
// shared.
func TestLinkUnpublishedArtifact(t *testing.T) {
	f, _ := newLinkFixture(t)
	files := bundle{"a.txt": []byte("a")}
	req := files.manifest("a.txt")
	req.Scope = "project-1"
	pend := f.createPending(userU, "/api/v1/artifacts", req)
	if rec, _ := f.mintLink(userU, pend.Artifact.ID, ""); rec.Code != http.StatusConflict {
		t.Errorf("unpublished: %d, want 409", rec.Code)
	}
}

// TestLinkCapPerArtifact: at most MaxLinksPerArtifact unexpired links;
// expired ones do not count and are dropped.
func TestLinkCapPerArtifact(t *testing.T) {
	f, id := newLinkFixture(t)
	var first CreateLinkResponse
	for i := range MaxLinksPerArtifact {
		_, resp := f.mustMintLink(userU, id, "")
		if i == 0 {
			first = resp
		}
	}
	rec, _ := f.mintLink(userU, id, "")
	if rec.Code != http.StatusConflict || errCode(t, rec) != "too_many_links" {
		t.Fatalf("over the cap: %d %s", rec.Code, rec.Body.String())
	}
	f.exec(t, `UPDATE artifact_grant SET expires_at = ? WHERE id = ?`, linkPast(), first.Link.ID)
	f.mustMintLink(userU, id, "")
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM artifact_grant WHERE artifact_id = ? AND subject_kind = 'link'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != MaxLinksPerArtifact {
		t.Errorf("%d link rows, want %d (the expired one dropped)", n, MaxLinksPerArtifact)
	}
}

// TestLinkListAndRevoke: the list shows unexpired links without tokens or
// hashes; revoking needs the link to belong to the artifact.
func TestLinkListAndRevoke(t *testing.T) {
	f, id := newLinkFixture(t)
	other := f.publish(userU, "o.md", []byte("o"), "scope=project-1").Artifact.ID
	t1, l1 := f.mustMintLink(userU, id, "")
	_, l2 := f.mustMintLink(userU, id, "")
	_, lo := f.mustMintLink(userU, other, "")
	f.exec(t, `UPDATE artifact_grant SET expires_at = ? WHERE id = ?`, linkPast(), l2.Link.ID)

	rec := f.do(&userU, http.MethodGet, linksPath(id), nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), t1) || strings.Contains(rec.Body.String(), LinkTokenHash(t1)) {
		t.Errorf("list shows a token or hash: %s", rec.Body.String())
	}
	got := decodeInto[LinkListResponse](t, rec)
	if len(got.Links) != 1 || got.Links[0].ID != l1.Link.ID {
		t.Errorf("list = %+v, want only %s", got.Links, l1.Link.ID)
	}
	if rec := f.do(&userU, http.MethodDelete, linksPath(id)+"/"+lo.Link.ID, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("revoke another artifact's link: %d, want 404", rec.Code)
	}
	rs := &recordingStore{Store: f.store}
	f.svc.SetStore(rs)
	for _, bad := range []string{"not-a-uuid", strings.ToUpper(l1.Link.ID), "%00"} {
		if rec := f.do(&userU, http.MethodDelete, linksPath(id)+"/"+bad, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("revoke malformed id %q: %d", bad, rec.Code)
		}
	}
	for _, c := range rs.take() {
		if c == "RevokeLink" {
			t.Errorf("a malformed link id reached the store")
		}
	}
	f.svc.SetStore(f.store)
	if rec := f.do(&userU, http.MethodDelete, linksPath(id)+"/"+l1.Link.ID, nil, nil); rec.Code != http.StatusNoContent {
		t.Errorf("revoke: %d", rec.Code)
	}
	if rec := f.do(&userU, http.MethodDelete, linksPath(id)+"/"+l1.Link.ID, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("revoke twice: %d", rec.Code)
	}
	if rec := f.do(&userU, http.MethodPut, linksPath(id), nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT links: %d", rec.Code)
	}
	if rec := f.do(&userU, http.MethodGet, linksPath(id)+"/"+l1.Link.ID, nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET one link: %d", rec.Code)
	}
}

// TestLinksDoNotGrantSessionReads: a link grant never makes the artifact
// readable to a session caller, nor puts it in anyone's list.
func TestLinksDoNotGrantSessionReads(t *testing.T) {
	f, id := newLinkFixture(t)
	f.mustMintLink(userU, id, "")
	if rec := f.do(&outside, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("outsider GET with a link on the artifact: %d", rec.Code)
	}
	if got := f.list(&outside, listPath); len(got.Artifacts) != 0 {
		t.Errorf("outsider list shows %d artifacts", len(got.Artifacts))
	}
}

// TestSharedRateLimitedBeforeLookup: a client over its budget is refused
// before its token is looked at, valid or not, and other clients are not.
func TestSharedRateLimitedBeforeLookup(t *testing.T) {
	f, id := newLinkFixture(t)
	tok, _ := f.mustMintLink(userU, id, "")
	f.svc.limiter = newRateLimiter(time.Now, SharedClientPerMinute, SharedClientBurst, SharedGlobalPerMinute, SharedGlobalBurst, sharedMaxClients)
	rs := &recordingStore{Store: f.store}
	f.svc.SetStore(rs)
	unknown, _ := newLinkToken()
	for i := range SharedClientBurst {
		if rec := f.shared(RouteShared+unknown, "192.0.2.1:1000"); rec.Code != http.StatusNotFound {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	rs.take()
	rec := f.shared(RouteShared+tok, "192.0.2.1:1001")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" ||
		rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("over budget with a valid token: %d %v", rec.Code, rec.Header())
	}
	if got := rs.take(); len(got) != 0 {
		t.Errorf("a limited request reached the store: %v", got)
	}
	if rec := f.shared(RouteShared+tok, "192.0.2.2:1000"); rec.Code != http.StatusSeeOther {
		t.Errorf("another client: %d", rec.Code)
	}
	// The host's client key decides who is charged.
	f.svc.SetClientKey(func(*http.Request) string { return "one" })
	if rec := f.shared(RouteShared+tok, "192.0.2.3:1"); rec.Code != http.StatusSeeOther {
		t.Errorf("custom key, first: %d", rec.Code)
	}
}

// TestRateLimiter: per-client burst and rate, the global bound, refusals
// charge nothing, the client table is bounded and sweeps only buckets that
// have refilled.
func TestRateLimiter(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }

	l := newRateLimiter(clock, 60, 3, 600, 600, 100)
	for i := range 3 {
		if _, ok := l.allow("a"); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	wait, ok := l.allow("a")
	if ok || wait != 1 {
		t.Fatalf("over burst: ok=%v wait=%d, want refused with 1s", ok, wait)
	}
	// Refusals charge nothing: after one second exactly one token is back.
	for range 5 {
		l.allow("a")
	}
	now = now.Add(time.Second)
	if _, ok := l.allow("a"); !ok {
		t.Fatalf("refilled token refused")
	}
	if _, ok := l.allow("a"); ok {
		t.Fatalf("second request after one second allowed")
	}
	if _, ok := l.allow("b"); !ok {
		t.Fatalf("another client refused")
	}

	// Global bound across clients.
	g := newRateLimiter(clock, 60, 10, 60, 5, 100)
	allowed := 0
	for i := range 20 {
		if _, ok := g.allow("c" + strconv.Itoa(i)); ok {
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("global burst: %d allowed, want 5", allowed)
	}
	// A refusal by the global bucket does not charge the client.
	now = now.Add(time.Second)
	if _, ok := g.allow("c19"); !ok {
		t.Errorf("client refused by the global bucket was charged")
	}

	// Client table bound: full of refilling buckets, a new client is not
	// tracked and is charged to the global bucket alone; once the buckets
	// refill, a sweep makes room and it is tracked.
	m := newRateLimiter(clock, 60, 2, 6000, 6000, 3)
	for _, k := range []string{"x", "y", "z"} {
		m.allow(k)
	}
	before := m.global.tokens
	if _, ok := m.allow("w"); !ok {
		t.Fatalf("untracked newcomer refused while the global bucket has tokens")
	}
	if len(m.clients) != 3 {
		t.Fatalf("table grew to %d", len(m.clients))
	}
	if _, tracked := m.clients["w"]; tracked {
		t.Fatalf("newcomer tracked in a full table")
	}
	if m.global.tokens != before-1 {
		t.Fatalf("untracked newcomer did not consume a global token: %v -> %v", before, m.global.tokens)
	}
	now = now.Add(2 * time.Second)
	if _, ok := m.allow("w"); !ok {
		t.Fatalf("new client refused after the table refilled")
	}
	if _, tracked := m.clients["w"]; !tracked || len(m.clients) > 3 {
		t.Errorf("after the sweep: tracked=%v, table %d", tracked, len(m.clients))
	}

	// An untracked newcomer refused by the global bucket gets exactly the
	// answer a tracked client refused by it gets: nothing tells the table
	// is full.
	u := newRateLimiter(clock, 60, 5, 60, 2, 1)
	u.allow("tracked")
	u.allow("tracked")
	waitTracked, okTracked := u.allow("tracked")
	globalBefore := u.global.tokens
	waitUntracked, okUntracked := u.allow("newcomer")
	if u.global.tokens != globalBefore {
		t.Errorf("a refused newcomer was charged: global %v -> %v", globalBefore, u.global.tokens)
	}
	if okTracked || okUntracked || waitTracked != waitUntracked {
		t.Errorf("tracked refusal (%v, %d) and untracked refusal (%v, %d) differ", okTracked, waitTracked, okUntracked, waitUntracked)
	}
	if _, tracked := u.clients["newcomer"]; tracked {
		t.Errorf("refused newcomer was tracked")
	}

	// Sweeps are spaced: within sharedSweepEvery of the last sweep, a full
	// table does not track a new client even when its buckets have
	// refilled.
	sp := newRateLimiter(clock, 600, 2, 6000, 6000, 3)
	for _, k := range []string{"x", "y", "z"} {
		sp.allow(k)
	}
	sp.allow("w")                       // sweeps (nothing refilled), not tracked
	now = now.Add(sharedSweepEvery / 2) // buckets refilled, sweep not due
	sp.allow("w")
	if _, tracked := sp.clients["w"]; tracked {
		t.Errorf("tracked before the next sweep was due")
	}
	now = now.Add(sharedSweepEvery / 2)
	sp.allow("w")
	if _, tracked := sp.clients["w"]; !tracked {
		t.Errorf("not tracked once the sweep was due")
	}
}

// TestSharedRandomized drives random sequences of link operations and
// checks every shared read against a model of which links are live.
func TestSharedRandomized(t *testing.T) {
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)
	f := newFixture(t, false)
	f.svc.SetViewKey(testViewKey)
	f.svc.limiter = newRateLimiter(time.Now, 100000, 100000, 100000, 100000, 1000)

	type link struct {
		token, id, artifact string
		dead                bool // revoked or expired
		revoked             bool
	}
	type art struct {
		id   string
		dead bool
	}
	var arts []*art
	var links []*link
	for i := range 3 {
		arts = append(arts, &art{id: f.publish(userU, "r"+strconv.Itoa(i)+".md", []byte("r"), "scope=project-1").Artifact.ID})
	}
	artLive := func(id string) bool {
		for _, a := range arts {
			if a.id == id {
				return !a.dead
			}
		}
		return false
	}
	live := func(l *link) bool { return !l.dead && artLive(l.artifact) }
	for step := range 200 {
		switch op := rng.Intn(6); {
		case op <= 1: // mint
			a := arts[rng.Intn(len(arts))]
			rec, resp := f.mintLink(userU, a.id, "")
			if a.dead {
				if rec.Code != http.StatusNotFound {
					t.Fatalf("step %d: mint on a dead artifact: %d", step, rec.Code)
				}
				continue
			}
			if rec.Code == http.StatusConflict {
				continue
			}
			if rec.Code != http.StatusCreated {
				t.Fatalf("step %d: mint: %d", step, rec.Code)
			}
			// Creating a link drops the artifact's expired link rows.
			for _, l := range links {
				if l.artifact == a.id && l.dead {
					l.revoked = true
				}
			}
			links = append(links, &link{token: strings.TrimPrefix(resp.URL, RouteShared), id: resp.Link.ID, artifact: a.id})
		case op == 2 && len(links) > 0: // revoke
			l := links[rng.Intn(len(links))]
			// An expired link can still be revoked; a revoked one, or any
			// link of a deleted artifact, is gone.
			rec := f.do(&userU, http.MethodDelete, linksPath(l.artifact)+"/"+l.id, nil, nil)
			exists := !l.revoked && artLive(l.artifact)
			if exists != (rec.Code == http.StatusNoContent) {
				t.Fatalf("step %d: revoke exists=%v: %d", step, exists, rec.Code)
			}
			l.dead, l.revoked = true, true
		case op == 3 && len(links) > 0: // expire
			l := links[rng.Intn(len(links))]
			f.exec(t, `UPDATE artifact_grant SET expires_at = ? WHERE id = ?`, linkPast(), l.id)
			l.dead = true
		case op == 4 && rng.Intn(4) == 0: // delete an artifact
			a := arts[rng.Intn(len(arts))]
			f.exec(t, `UPDATE artifact SET deleted_at = ? WHERE id = ?`, linkPast(), a.id)
			a.dead = true
		default: // read through every known link
			for _, l := range links {
				rec := f.shared(RouteShared+l.token, "")
				want := http.StatusNotFound
				if live(l) {
					want = http.StatusSeeOther
				}
				if rec.Code != want {
					t.Fatalf("step %d: shared read live=%v: %d", step, live(l), rec.Code)
				}
				if want == http.StatusSeeOther {
					if v := f.shared(rec.Header().Get("Location"), ""); v.Code != http.StatusOK {
						t.Fatalf("step %d: view: %d", step, v.Code)
					}
				}
			}
		}
	}
}

// --- small helpers ---

func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func linkPast() string { return time.Now().Add(-time.Minute).UTC().Format(sqliteTimeLayout) }
func linkNow() string  { return time.Now().UTC().Format(sqliteTimeLayout) }

// TestLinkDocumentedValues pins the values the reference documentation
// states.
func TestLinkDocumentedValues(t *testing.T) {
	if DefaultLinkTTL != 168*time.Hour || DefaultLinkMaxTTL != 720*time.Hour || MaxLinksPerArtifact != 50 ||
		linkTokenBytes != 32 || ViewTTL != 30*time.Minute {
		t.Errorf("documented link values changed: update docs-site reference/artifacts.md")
	}
}

// TestGrantMatchesOnlyItsSubject: a principal grant serves the principal
// it names and nobody else, for reading and for administering.
func TestGrantMatchesOnlyItsSubject(t *testing.T) {
	f, id := newLinkFixture(t)
	other := principal{PrincipalKindUser, "user-9", ""}
	f.grantPrincipal(id, outside)
	if rec := f.do(&outside, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("grantee: %d", rec.Code)
	}
	if rec := f.do(&other, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("a principal grant served another principal: %d", rec.Code)
	}
	f.exec(t, `UPDATE artifact_grant SET permission = 'admin' WHERE subject_kind = 'principal'`)
	f.host.allow(other, "project-1", PermissionRead)
	if rec, _ := f.mintLink(other, id, ""); rec.Code != http.StatusForbidden {
		t.Errorf("an admin grant served another principal: %d", rec.Code)
	}
}

// TestReadAsksHostOnceForHome: the read check asks the host about the
// home project once; the home project's own grant is not asked again.
func TestReadAsksHostOnceForHome(t *testing.T) {
	f, id := newLinkFixture(t)
	f.host.mu.Lock()
	f.host.calls = nil
	f.host.mu.Unlock()
	f.do(&outside, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil)
	n := 0
	for _, c := range f.host.calls {
		if c == "project-1 "+PermissionRead {
			n++
		}
	}
	if n != 1 {
		t.Errorf("home project asked %d times: %v", n, f.host.calls)
	}
}

// TestGrantKindsEachCount: each grant kind the shared matcher accepts
// works on its own path: an admin principal grant reads and shares with
// no role in the home project; a write grant to the home project lets its
// publishers append; an admin grant to the home project lets its managers
// share; a scope grant with an empty subject never counts, whatever the
// host says about "".
func TestGrantKindsEachCount(t *testing.T) {
	f, id := newLinkFixture(t)

	admin := principal{PrincipalKindUser, "user-5", ""}
	f.exec(t, `INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at)
		VALUES ('g-admin', ?, 'principal', ?, 'admin', ?)`, id, PrincipalRef(admin.kind, admin.ref), linkNow())
	if rec := f.do(&admin, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("admin principal grant does not read: %d", rec.Code)
	}
	if rec, _ := f.mintLink(admin, id, ""); rec.Code != http.StatusCreated {
		t.Errorf("admin principal grant does not share: %d", rec.Code)
	}

	// agentB publishes in project-1 but does not own userU's artifact.
	files := bundle{"doc.md": []byte("# v2")}
	if rec := f.postJSON(&agentB, "/api/v1/artifacts/"+id+"/versions", files.manifest("doc.md")); rec.Code != http.StatusForbidden {
		t.Fatalf("append before the home grant is write: %d", rec.Code)
	}
	f.exec(t, `UPDATE artifact_grant SET permission = 'write' WHERE artifact_id = ? AND subject_kind = 'scope' AND subject_ref = 'project-1'`, id)
	if rec := f.postJSON(&agentB, "/api/v1/artifacts/"+id+"/versions", files.manifest("doc.md")); rec.Code != http.StatusCreated {
		t.Errorf("home write grant does not let a publisher append: %d %s", rec.Code, rec.Body.String())
	}

	manager := principal{PrincipalKindUser, "user-6", ""}
	f.host.allow(manager, "project-1", PermissionRead, PermissionManage)
	if rec, _ := f.mintLink(manager, id, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("share before the home grant is admin: %d", rec.Code)
	}
	f.exec(t, `UPDATE artifact_grant SET permission = 'admin' WHERE artifact_id = ? AND subject_kind = 'scope' AND subject_ref = 'project-1'`, id)
	if rec, _ := f.mintLink(manager, id, ""); rec.Code != http.StatusCreated {
		t.Errorf("home admin grant does not let a manager share: %d", rec.Code)
	}

	empty := principal{PrincipalKindUser, "user-8", ""}
	f.host.allow(empty, "", PermissionRead, PermissionManage, PermissionCreate)
	f.exec(t, `INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at)
		VALUES ('g-empty', ?, 'scope', '', 'admin', ?)`, id, linkNow())
	if rec := f.do(&empty, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("an empty scope grant gave read: %d", rec.Code)
	}
}

func (s *recordingStore) DeleteGrant(ctx context.Context, artifactID, grantID string) error {
	s.record("DeleteGrant")
	return s.Store.DeleteGrant(ctx, artifactID, grantID)
}

// noExpiryStore returns resolved link grants without an expiry.
type noExpiryStore struct{ Store }

func (s noExpiryStore) ResolveLink(ctx context.Context, hash string, now time.Time) (*Artifact, *Grant, error) {
	a, g, err := s.Store.ResolveLink(ctx, hash, now)
	if g != nil {
		g.ExpiresAt = nil
	}
	return a, g, err
}

// TestSharedLinkWithoutExpiryRefused: a resolved link grant without an
// expiry (which ResolveLink never returns) is refused like any other.
func TestSharedLinkWithoutExpiryRefused(t *testing.T) {
	f, id := newLinkFixture(t)
	tok, _ := f.mustMintLink(userU, id, "")
	unknown, _ := newLinkToken()
	want := response(f.shared(RouteShared+unknown, ""))
	f.svc.SetStore(noExpiryStore{f.store})
	if got := response(f.shared(RouteShared+tok, "")); got != want {
		t.Errorf("link without expiry: %s, want the uniform refusal %s", got, want)
	}
}

// TestRateLimiterZeroRates: zero or negative settings are raised to 1, so
// buckets refill and the wait is finite.
func TestRateLimiterZeroRates(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newRateLimiter(func() time.Time { return now }, 0, 0, 0, 0, 0)
	if _, ok := l.allow("a"); !ok {
		t.Fatalf("first request refused")
	}
	wait, ok := l.allow("a")
	if ok || wait < 1 || wait > 60 {
		t.Errorf("second request: ok=%v wait=%d", ok, wait)
	}
	if (&tokenBucket{}).wait(0) != 60 {
		t.Errorf("wait with a zero rate")
	}
}
