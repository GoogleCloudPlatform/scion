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
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
)

// Phase A (ptone/scion#7107): the hub's content host. The
// tests name the security review condition (C1-C6) and the kill check (K)
// each one pins. The hub wiring (outermost placement, the hub's credential
// headers, settings and audit) is tested in pkg/hub.

const (
	testContentURL  = "https://links.example.net"
	testContentHost = "links.example.net"
	testHubOrigin   = "https://hub.example.com"
	testHubHost     = "hub.example.com"
	testAgentHeader = "X-Scion-Agent-Token"
)

// testServesOnOrigin stands in for the hub's route check (isArtifactViewRequest
// or isArtifactSharedRequest, tested in pkg/hub): a GET or HEAD of a clean
// path with a capability segment under the share or view route.
func testServesOnOrigin(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	if path.Clean(p) != p || strings.Contains(r.URL.RawPath, "%") {
		return false
	}
	for _, route := range []string{RouteShared, RouteView} {
		if rest, ok := strings.CutPrefix(p, route); ok {
			seg, _, _ := strings.Cut(rest, "/")
			return seg != ""
		}
	}
	return false
}

// stripHeaders returns a credential stripper that removes names.
func stripHeaders(names ...string) func(http.Header) {
	return func(h http.Header) {
		for _, n := range names {
			h.Del(n)
		}
	}
}

// contentURLLimits returns limits with the given content URL and the test
// hub origin.
func contentURLLimits(lo string) func(context.Context) Limits {
	return func(context.Context) Limits { return Limits{ContentURL: lo, PublicOrigin: testHubOrigin} }
}

// guarded serves r through ContentHostGuard(lo) in front of f.svc, as the
// hub mounts it.
func (f *fixture) guarded(lo string, r *http.Request) *httptest.ResponseRecorder {
	f.t.Helper()
	h := ContentHostGuard(fixedContentHost(lo), testServesOnOrigin, stripHeaders(testAgentHeader), nil)(f.svc)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// fixedContentHost parses raw once, as the hub does at each settings apply,
// and returns the guard's view of it; "" (or an invalid value) gives the
// zero ContentHost, which matches no request.
func fixedContentHost(raw string) func(*http.Request) ContentHost {
	o, _ := ParseContentHost(raw)
	return func(*http.Request) ContentHost { return o }
}

func requestOn(method, host, target string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

// openSharedLink follows a share link through the guard on host and
// returns the view response.
func (f *fixture) openSharedLink(lo, host, token string, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := f.guarded(lo, requestOn(http.MethodGet, host, RouteShared+token, hdr))
	if rec.Code != http.StatusSeeOther {
		f.t.Fatalf("shared on %s: %d %s", host, rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, RouteView) {
		f.t.Fatalf("shared on %s: Location %q is not a relative view path", host, loc)
	}
	view := f.guarded(lo, requestOn(http.MethodGet, host, loc, hdr))
	if view.Code != http.StatusOK {
		f.t.Fatalf("view on %s: %d %s", host, view.Code, view.Body.String())
	}
	return view
}

// cspSource returns the script-src source of a view CSP.
func cspSource(t *testing.T, csp string) string {
	t.Helper()
	m := regexp.MustCompile(`script-src (\S+) 'unsafe-inline'`).FindStringSubmatch(csp)
	if m == nil {
		t.Fatalf("CSP %q has no script-src", csp)
	}
	return m[1]
}

// TestContentHostMatch: a request is a content-host request iff its
// Host names the content URL's host (case-insensitive) and port (exact,
// default implied). C1, K2.
func TestContentHostMatch(t *testing.T) {
	for _, tc := range []struct {
		lo, host string
		want     bool
	}{
		{testContentURL, testContentHost, true},
		{testContentURL, "LINKS.Example.NET", true},
		{testContentURL, "links.example.net:443", true},
		{testContentURL, "links.example.net.", true},
		{testContentURL, "links.example.net:8443", false},
		{testContentURL, "links.example.net:80", false},
		{testContentURL, "links.example.net.evil.com", false},
		{testContentURL, "xlinks.example.net", false},
		{testContentURL, "links.example", false},
		{testContentURL, testHubHost, false},
		{testContentURL, "", false},
		{"https://links.example.net:8443", "links.example.net:8443", true},
		{"https://links.example.net:8443", "links.example.net", false},
		{"https://links.example.net:8443", "links.example.net:84", false},
		{"https://links.example.net:8443", "links.example.net:84430", false},
		{"http://localhost:9000", "localhost:9000", true},
		{"http://localhost:9000", "localhost", false},
		{"", testContentHost, false},
		{"https://links.example.net/x", testContentHost, false},
		{"ftp://links.example.net", testContentHost, false},
		{"https://", "", false},
	} {
		r := requestOn(http.MethodGet, tc.host, "/", nil)
		if got := ContentHostRequest(r, tc.lo); got != tc.want {
			t.Errorf("ContentHostRequest(Host %q, %q) = %v, want %v", tc.host, tc.lo, got, tc.want)
		}
	}
}

// TestContentHostIgnoresForwardedHeaders: detection reads only the Host
// the hub received; X-Forwarded-Host and Forwarded never make or unmake a
// content-host request. C2 [r2] tests A and B, K14.
func TestContentHostIgnoresForwardedHeaders(t *testing.T) {
	for _, hdr := range []map[string]string{
		{"X-Forwarded-Host": testContentHost},
		{"Forwarded": "host=" + testContentHost},
		{"Forwarded": `for=192.0.2.1;host="` + testContentHost + `";proto=https`},
		{"X-Original-Host": testContentHost, "X-Host": testContentHost},
	} {
		if ContentHostRequest(requestOn(http.MethodGet, testHubHost, "/", hdr), testContentURL) {
			t.Errorf("hub Host with %v detected as a content-host request", hdr)
		}
	}
	for _, hdr := range []map[string]string{
		{"X-Forwarded-Host": testHubHost},
		{"Forwarded": "host=" + testHubHost},
	} {
		if !ContentHostRequest(requestOn(http.MethodGet, testContentHost, "/", hdr), testContentURL) {
			t.Errorf("content host with %v not detected as a content-host request", hdr)
		}
	}
}

// TestContentHostGuardConfines: on the content host only the share and
// view routes are served; everything else answers 404 before the inner
// handler runs. On the hub Host every request reaches it. C2, K3, K4.
func TestContentHostGuardConfines(t *testing.T) {
	refused := []struct{ method, target string }{
		{http.MethodGet, "/api/v1/artifacts/0190a2b4-0000-7000-8000-000000000001"},
		{http.MethodPost, "/api/v1/artifacts/0190a2b4-0000-7000-8000-000000000001/links"},
		{http.MethodGet, "/api/v1/artifacts"},
		{http.MethodGet, "/api/v1/agents"},
		{http.MethodGet, "/api/v1/projects"},
		{http.MethodPut, "/api/v1/admin/server-config"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/index.html"},
		{http.MethodGet, "/assets/app.js"},
		{http.MethodGet, "/auth/login/google"},
		{http.MethodGet, "/events"},
		{http.MethodGet, "/api/v1/portview/abc/"},
		{http.MethodGet, "/.well-known/openid-configuration"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/api/v1/artifacts/shared"},
		{http.MethodGet, "/api/v1/artifacts/shared/"},
		{http.MethodPost, "/api/v1/artifacts/shared/tok"},
		{http.MethodDelete, "/api/v1/artifacts/view/cap/x"},
		{http.MethodOptions, "/api/v1/artifacts/view/cap/x"},
		{http.MethodGet, "/api/v1/artifacts/sharedx/tok"},
	}
	allowed := []struct{ method, target string }{
		{http.MethodGet, "/api/v1/artifacts/shared/tok"},
		{http.MethodHead, "/api/v1/artifacts/shared/tok"},
		{http.MethodGet, "/api/v1/artifacts/shared/tok/files/a/b.png"},
		{http.MethodGet, "/api/v1/artifacts/view/cap/index.html"},
		{http.MethodHead, "/api/v1/artifacts/view/cap/index.html"},
	}
	var reached []string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	})
	guard := ContentHostGuard(fixedContentHost(testContentURL), testServesOnOrigin, stripHeaders(testAgentHeader), nil)(inner)
	serve := func(method, host, target string) int {
		r := httptest.NewRequest(method, target, nil)
		r.Host = host
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, r)
		return rec.Code
	}
	for _, rq := range refused {
		reached = nil
		if code := serve(rq.method, testContentHost, rq.target); code != http.StatusNotFound || len(reached) != 0 {
			t.Errorf("content host %s %s: %d (inner reached %v), want 404 before the handler", rq.method, rq.target, code, reached)
		}
		reached = nil
		if code := serve(rq.method, testHubHost, rq.target); code != http.StatusTeapot || len(reached) != 1 {
			t.Errorf("hub %s %s: %d, want it served as today", rq.method, rq.target, code)
		}
	}
	for _, rq := range allowed {
		for _, host := range []string{testContentHost, testHubHost} {
			reached = nil
			if code := serve(rq.method, host, rq.target); code != http.StatusTeapot || len(reached) != 1 {
				t.Errorf("%s %s %s: %d, want it served", host, rq.method, rq.target, code)
			}
		}
	}
	// No content URL: nothing is confined.
	open := ContentHostGuard(fixedContentHost(""), testServesOnOrigin, nil, nil)(inner)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	r.Host = testContentHost
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, r)
	if rec.Code != http.StatusTeapot {
		t.Errorf("empty content URL confined a request: %d", rec.Code)
	}
}

// TestContentHostGuardConfinesTheService: through the real service on the
// content host, the artifact read and link creation answer 404 while a
// share link redirects (303, relative) and its view serves (200). C2.
func TestContentHostGuardConfinesTheService(t *testing.T) {
	f, id := newLinkFixture(t)
	f.svc.SetLimits(contentURLLimits(testContentURL))
	token, _ := f.mustMintLink(userU, id, "")

	for _, rq := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/artifacts/" + id},
		{http.MethodPost, linksPath(id)},
	} {
		r := withPrincipal(requestOn(rq.method, testContentHost, rq.target, nil), userU)
		if rec := f.guarded(testContentURL, r); rec.Code != http.StatusNotFound {
			t.Errorf("content host %s %s: %d, want 404", rq.method, rq.target, rec.Code)
		}
		r = withPrincipal(requestOn(rq.method, testHubHost, rq.target, nil), userU)
		if rec := f.guarded(testContentURL, r); rec.Code == http.StatusNotFound {
			t.Errorf("hub %s %s: 404, want it served as today", rq.method, rq.target)
		}
	}
	view := f.openSharedLink(testContentURL, testContentHost, token, nil)
	if view.Body.String() != "# shared" {
		t.Errorf("view body %q", view.Body.String())
	}
}

// TestContentHostGuardDropsCredentials: on the content host the guard
// removes the Cookie, Authorization and agent-token headers (and any the
// hub names) before the inner handler, which sees no credential, and the
// response equals the same request without them. On the hub Host they
// pass. C3, K5 (in pkg/hub), K6, K7.
func TestContentHostGuardDropsCredentials(t *testing.T) {
	creds := map[string]string{
		"Cookie":                   "scion_sess=valid-session",
		"Authorization":            "Bearer user-token",
		testAgentHeader:            "agent-token",
		"X-Scion-Federation-Token": "fed",
	}
	var seen http.Header
	var marked bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, marked = contentHostCSPHost(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	guard := ContentHostGuard(fixedContentHost(testContentURL), testServesOnOrigin, stripHeaders(testAgentHeader, "X-Scion-Federation-Token"), nil)(inner)
	target := RouteShared + "tok"

	r := requestOn(http.MethodGet, testContentHost, target, creds)
	r.AddCookie(&http.Cookie{Name: "other", Value: "x"})
	guard.ServeHTTP(httptest.NewRecorder(), r)
	for name := range creds {
		if v := seen.Values(name); len(v) != 0 {
			t.Errorf("content host: inner saw %s = %v", name, v)
		}
	}
	if !marked {
		t.Error("content host: request not marked as content-host")
	}
	if r.Header.Get("Authorization") == "" {
		t.Error("the guard changed the caller's request instead of a clone")
	}

	r = requestOn(http.MethodGet, testHubHost, target, creds)
	guard.ServeHTTP(httptest.NewRecorder(), r)
	for name, want := range creds {
		if got := seen.Get(name); got != want {
			t.Errorf("hub Host: inner saw %s = %q, want %q", name, got, want)
		}
	}
	if marked {
		t.Error("hub Host: request marked as content-host")
	}
}

// TestContentHostCredentialsChangeNothing: through the real service, a
// share link opened on the content host with a session cookie, a bearer
// token and an agent token answers exactly as without them. C3, K6, K7.
func TestContentHostCredentialsChangeNothing(t *testing.T) {
	f, id := newLinkFixture(t)
	f.svc.SetLimits(contentURLLimits(testContentURL))
	token, _ := f.mustMintLink(userU, id, "")
	bare := f.openSharedLink(testContentURL, testContentHost, token, nil)
	withCreds := f.openSharedLink(testContentURL, testContentHost, token, map[string]string{
		"Cookie": "scion_sess=valid-session", "Authorization": "Bearer user-token", testAgentHeader: "agent-token",
	})
	if response(bare) != response(withCreds) {
		t.Errorf("credentials changed the response:\n%s\nvs\n%s", response(bare), response(withCreds))
	}
}

// TestViewCSPFromConfiguration: the view CSP source is the configured link
// content host for a content-host request and the hub's configured public origin on
// any other Host; the request Host never appears. C1, K1, K2.
func TestViewCSPFromConfiguration(t *testing.T) {
	f, id := newLinkFixture(t)
	f.svc.SetLimits(contentURLLimits(testContentURL))
	token, _ := f.mustMintLink(userU, id, "")

	for _, tc := range []struct{ host, wantHost string }{
		{testContentHost, testContentHost},
		{"LINKS.EXAMPLE.NET", testContentHost},
		{testHubHost, testHubHost},
		{"evil.example.org", testHubHost},
		{"links.example.net:8443", testHubHost},
		{"evil.com; script-src *", testHubHost},
	} {
		view := f.openSharedLink(testContentURL, tc.host, token, nil)
		csp := view.Header().Get("Content-Security-Policy")
		if src := cspSource(t, csp); !strings.HasPrefix(src, tc.wantHost+RouteView) {
			t.Errorf("Host %q: CSP source %q, want it on %s", tc.host, src, tc.wantHost)
		}
		if !strings.EqualFold(tc.host, tc.wantHost) && strings.Contains(strings.ToLower(csp), strings.ToLower(tc.host)) {
			t.Errorf("Host %q: CSP echoes the request host: %q", tc.host, csp)
		}
	}

	// An empty content URL: every Host gets the hub origin.
	f.svc.SetLimits(contentURLLimits(""))
	for _, host := range []string{testContentHost, "evil.example.org", testHubHost} {
		view := f.openSharedLink("", host, token, nil)
		if src := cspSource(t, view.Header().Get("Content-Security-Policy")); !strings.HasPrefix(src, testHubHost+RouteView) {
			t.Errorf("empty content URL, Host %q: CSP source %q, want the hub origin", host, src)
		}
	}

	// No configured public origin: no source at all, never the Host.
	f.svc.SetLimits(func(context.Context) Limits { return Limits{} })
	view := f.openSharedLink("", "evil.example.org", token, nil)
	if src := cspSource(t, view.Header().Get("Content-Security-Policy")); src != "'none'" {
		t.Errorf("no public origin: CSP source %q, want 'none'", src)
	}
}

// TestViewCSPIgnoresForwardedHeaders: a hub-Host request carrying
// X-Forwarded-Host or Forwarded naming the content host is served and
// secured as a hub request (CSP on the hub origin); a content-host
// request carrying the hub's host there keeps the link-origin CSP.
// C2 [r2] tests A and B, K14.
func TestViewCSPIgnoresForwardedHeaders(t *testing.T) {
	f, id := newLinkFixture(t)
	f.svc.SetLimits(contentURLLimits(testContentURL))
	token, _ := f.mustMintLink(userU, id, "")

	for _, hdr := range []map[string]string{
		{"X-Forwarded-Host": testContentHost},
		{"Forwarded": "host=" + testContentHost},
	} {
		view := f.openSharedLink(testContentURL, testHubHost, token, hdr)
		if src := cspSource(t, view.Header().Get("Content-Security-Policy")); !strings.HasPrefix(src, testHubHost+RouteView) {
			t.Errorf("test A %v: CSP source %q, want the hub origin", hdr, src)
		}
		// Still served as a hub request: the API answers.
		r := withPrincipal(requestOn(http.MethodGet, testHubHost, "/api/v1/artifacts/"+id, hdr), userU)
		if rec := f.guarded(testContentURL, r); rec.Code != http.StatusOK {
			t.Errorf("test A %v: hub API %d, want 200", hdr, rec.Code)
		}
	}

	hdr := map[string]string{"X-Forwarded-Host": testHubHost, "Forwarded": "host=" + testHubHost}
	view := f.openSharedLink(testContentURL, testContentHost, token, hdr)
	if src := cspSource(t, view.Header().Get("Content-Security-Policy")); !strings.HasPrefix(src, testContentHost+RouteView) {
		t.Errorf("test B: CSP source %q, want the content URL", src)
	}
	r := withPrincipal(requestOn(http.MethodGet, testContentHost, "/api/v1/artifacts/"+id, hdr), userU)
	if rec := f.guarded(testContentURL, r); rec.Code != http.StatusNotFound {
		t.Errorf("test B: content-host API %d, want 404", rec.Code)
	}
}

// TestViewCSPTakesNoRequestInput: viewCSP is never called with the request
// Host (C1 check: `git grep 'viewCSP(r.Host'` finds nothing).
func TestViewCSPTakesNoRequestInput(t *testing.T) {
	src, err := os.ReadFile("view.go")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`viewCSP\(\s*r\.`).Match(src) {
		t.Error("view.go builds the view CSP from the request")
	}
	if !strings.Contains(string(src), "viewCSP(b.viewSourceHost(ctx), ") {
		t.Error("view.go no longer builds the view CSP from viewSourceHost")
	}
}

// TestShareURL: shareUrl is present only with a valid content URL, equals
// the origin plus the relative link path, does not follow the request Host
// or X-Forwarded-* headers, and url stays hub-relative. C6, K12, K13.
func TestShareURL(t *testing.T) {
	f, id := newLinkFixture(t)
	mint := func(host string, hdr map[string]string) CreateLinkResponse {
		t.Helper()
		h := map[string]string{"Content-Type": "application/json"}
		for k, v := range hdr {
			h[k] = v
		}
		r := withPrincipal(requestOn(http.MethodPost, host, linksPath(id), h), userU)
		rec := httptest.NewRecorder()
		f.svc.ServeHTTP(rec, r)
		if rec.Code != http.StatusCreated {
			t.Fatalf("mint on %s: %d %s", host, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Header().Get("Location"), "/shared/") {
			t.Errorf("the token appears in a response header")
		}
		return decodeInto[CreateLinkResponse](t, rec)
	}

	for _, lo := range []string{"", "https://links.example.net/x", "ftp://links.example.net", "https://u@links.example.net"} {
		f.svc.SetLimits(contentURLLimits(lo))
		resp := mint(testHubHost, nil)
		if resp.ShareURL != "" {
			t.Errorf("content URL %q: shareUrl %q, want none", lo, resp.ShareURL)
		}
		if !strings.HasPrefix(resp.URL, RouteShared) {
			t.Errorf("content URL %q: url %q, want hub-relative", lo, resp.URL)
		}
	}

	for _, lo := range []string{testContentURL, "https://links.example.net:8443"} {
		f.svc.SetLimits(contentURLLimits(lo))
		for _, tc := range []struct {
			host string
			hdr  map[string]string
		}{
			{testHubHost, nil},
			{"evil.example.org", nil},
			{testHubHost, map[string]string{"X-Forwarded-Host": "evil.example.org", "X-Forwarded-Proto": "http", "Forwarded": "host=evil.example.org"}},
		} {
			resp := mint(tc.host, tc.hdr)
			if !strings.HasPrefix(resp.URL, RouteShared) || strings.Contains(resp.URL, "://") {
				t.Errorf("url %q, want hub-relative", resp.URL)
			}
			if resp.ShareURL != lo+resp.URL {
				t.Errorf("Host %q %v: shareUrl %q, want %q", tc.host, tc.hdr, resp.ShareURL, lo+resp.URL)
			}
		}
	}

	// The token appears only in the create response: the list does not
	// carry it.
	f.svc.SetLimits(contentURLLimits(testContentURL))
	resp := mint(testHubHost, nil)
	token := strings.TrimPrefix(resp.URL, RouteShared)
	list := f.do(&userU, http.MethodGet, linksPath(id), nil, nil)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), token) {
		t.Errorf("list: %d, token present %v", list.Code, strings.Contains(list.Body.String(), token))
	}
}

// TestContentHostGuardAlwaysDropsCookieAndAuthorization: the Cookie and
// Authorization headers are removed on the content host even when the
// host passes no stripper of its own. C3, K6.
func TestContentHostGuardAlwaysDropsCookieAndAuthorization(t *testing.T) {
	var seen http.Header
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Clone() })
	guard := ContentHostGuard(fixedContentHost(testContentURL), testServesOnOrigin, nil, nil)(inner)
	r := requestOn(http.MethodGet, testContentHost, RouteView+"cap/index.html",
		map[string]string{"Cookie": "scion_sess=valid", "Authorization": "Bearer t", "Accept": "text/html"})
	guard.ServeHTTP(httptest.NewRecorder(), r)
	if seen.Get("Cookie") != "" || seen.Get("Authorization") != "" {
		t.Errorf("inner saw Cookie %q, Authorization %q", seen.Get("Cookie"), seen.Get("Authorization"))
	}
	if seen.Get("Accept") != "text/html" {
		t.Errorf("a non-credential header was dropped: Accept %q", seen.Get("Accept"))
	}
}

// TestContentHostGuardNilRouteCheckServesNothing: without a route check the
// guard admits nothing on the content host (fail closed). C2.
func TestContentHostGuardNilRouteCheckServesNothing(t *testing.T) {
	reached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
	guard := ContentHostGuard(fixedContentHost(testContentURL), nil, nil, nil)(inner)
	rec := httptest.NewRecorder()
	guard.ServeHTTP(rec, requestOn(http.MethodGet, testContentHost, RouteShared+"tok", nil))
	if rec.Code != http.StatusNotFound || reached {
		t.Errorf("nil route check: %d, inner reached %v; want 404", rec.Code, reached)
	}
}

// TestContentHostResponseHeaders: every response on the content host
// carries Referrer-Policy: no-referrer, Cache-Control: no-store and
// Access-Control-Allow-Origin: * with no Access-Control-Allow-Credentials,
// the guard's 404, the share link's 303 and the view's 200 included,
// whatever the inner handler set (an echoed Origin and credentials
// included); a response on the hub Host is left as the handler wrote it.
// C1-1, CORS, K44, K32.
func TestContentHostResponseHeaders(t *testing.T) {
	want := map[string]string{"Referrer-Policy": "no-referrer", "Cache-Control": "no-store", "Access-Control-Allow-Origin": "*"}
	check := func(what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		for k, v := range want {
			if got := rec.Header().Values(k); len(got) != 1 || got[0] != v {
				t.Errorf("%s: %s = %q, want exactly %q", what, k, got, v)
			}
		}
		if got := rec.Header().Values("Access-Control-Allow-Credentials"); len(got) != 0 {
			t.Errorf("%s: Access-Control-Allow-Credentials = %q, want none", what, got)
		}
	}

	// An inner handler that sets weaker values, through WriteHeader and
	// through a bare Write, does not keep them on the content host.
	weak := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "unsafe-url")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		if r.URL.Path == RouteView+"cap/bare.html" {
			_, _ = w.Write([]byte("x"))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	guard := ContentHostGuard(fixedContentHost(testContentURL), testServesOnOrigin, nil, nil)(weak)
	for _, target := range []string{RouteView + "cap/index.html", RouteView + "cap/bare.html", "/api/v1/agents"} {
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, requestOn(http.MethodGet, testContentHost, target, map[string]string{"Origin": "https://evil.example.org"}))
		check("content host "+target, rec)
		hub := httptest.NewRecorder()
		guard.ServeHTTP(hub, requestOn(http.MethodGet, testHubHost, target, nil))
		if got := hub.Header().Get("Cache-Control"); target != "/api/v1/agents" && got != "public, max-age=3600" {
			t.Errorf("hub %s: Cache-Control %q, want the handler's own", target, got)
		}
	}

	// Through the real service: the share link's 303 and the view's 200.
	f, id := newLinkFixture(t)
	f.svc.SetLimits(contentURLLimits(testContentURL))
	token, _ := f.mustMintLink(userU, id, "")
	shared := f.guarded(testContentURL, requestOn(http.MethodGet, testContentHost, RouteShared+token, nil))
	if shared.Code != http.StatusSeeOther {
		t.Fatalf("share link on the content host: %d, want 303", shared.Code)
	}
	check("share 303", shared)
	view := f.guarded(testContentURL, requestOn(http.MethodGet, testContentHost, shared.Header().Get("Location"), nil))
	if view.Code != http.StatusOK {
		t.Fatalf("view on the content host: %d, want 200", view.Code)
	}
	check("view 200", view)
}

// TestContentHostGuardReportsRefused: the guard tells its refused callback
// of each request it answers 404, and of no other: not an allowed request
// on the content host, and nothing on the hub Host. The hub logs refused
// requests with the capability redacted (C1-2, tested in pkg/hub).
func TestContentHostGuardReportsRefused(t *testing.T) {
	var got []string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	guard := ContentHostGuard(fixedContentHost(testContentURL), testServesOnOrigin, nil,
		func(r *http.Request) { got = append(got, r.Method+" "+r.URL.Path) })(inner)
	for _, rq := range []struct{ method, host, target string }{
		{http.MethodPost, testContentHost, RouteView + "cap/x"},
		{http.MethodGet, testContentHost, "/api/v1/agents"},
		{http.MethodGet, testContentHost, RouteView + "cap/index.html"},
		{http.MethodPost, testHubHost, RouteView + "cap/x"},
	} {
		guard.ServeHTTP(httptest.NewRecorder(), requestOn(rq.method, rq.host, rq.target, nil))
	}
	wantGot := []string{"POST " + RouteView + "cap/x", "GET /api/v1/agents"}
	if strings.Join(got, "|") != strings.Join(wantGot, "|") {
		t.Errorf("refused calls %q, want %q", got, wantGot)
	}
}
