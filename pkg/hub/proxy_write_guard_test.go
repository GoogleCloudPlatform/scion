package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// guardFor builds the proxyModeWriteGuard for a hub with the given AuthMode,
// wrapping a handler that records whether the request reached it.
func guardFor(mode string, reached *bool) http.Handler {
	s := &Server{authConfig: AuthConfig{AuthMode: mode}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
	return s.proxyModeWriteGuard(next)
}

// doGuardBody runs one request (with a body) through the guard and returns the
// status and whether the inner handler was reached.
func doGuardBody(mode, method, path, body string, headers map[string]string) (int, bool) {
	var reached bool
	h := guardFor(mode, &reached)
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "http://hub.example"+path, nil)
	} else {
		r = httptest.NewRequest(method, "http://hub.example"+path, strings.NewReader(body))
	}
	r.Host = "hub.example"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, reached
}

// doGuard runs a bodyless request through the guard.
func doGuard(mode, method, path string, headers map[string]string) (int, bool) {
	return doGuardBody(mode, method, path, "", headers)
}

const jsonBody = `{"x":1}`

func TestProxyWriteGuard_NoopWhenNotProxyMode(t *testing.T) {
	for _, mode := range []string{"oauth", "dev", ""} {
		code, reached := doGuardBody(mode, http.MethodPost, "/api/v1/projects", jsonBody, map[string]string{
			"Sec-Fetch-Site": "cross-site",
			"Content-Type":   "text/plain",
		})
		if !reached || code != http.StatusOK {
			t.Fatalf("mode %q: want pass-through, got code=%d reached=%v", mode, code, reached)
		}
	}
}

func TestProxyWriteGuard_GETNeverBlocked(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		code, reached := doGuard(proxyAuthMode, method, "/api/v1/projects", map[string]string{
			"Sec-Fetch-Site": "cross-site",
			"Origin":         "null",
		})
		if !reached || code != http.StatusOK {
			t.Fatalf("method %s: want pass-through, got code=%d reached=%v", method, code, reached)
		}
	}
}

func TestProxyWriteGuard_NonAPIPathNotBlocked(t *testing.T) {
	code, reached := doGuardBody(proxyAuthMode, http.MethodPost, "/auth/login", "a=b", map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"Content-Type":   "application/x-www-form-urlencoded",
	})
	if !reached || code != http.StatusOK {
		t.Fatalf("want pass-through for non-api path, got code=%d reached=%v", code, reached)
	}
}

func TestProxyWriteGuard_PortViewExempt(t *testing.T) {
	// The capability-only page-view route is not checked, for any method or
	// content type.
	for _, tc := range []struct {
		method, body string
		headers      map[string]string
	}{
		{http.MethodPost, "hello", map[string]string{"Origin": "null", "Sec-Fetch-Site": "cross-site", "Content-Type": "text/plain"}},
		{http.MethodPut, "bytes", map[string]string{"Origin": "null", "Content-Type": "application/octet-stream"}},
	} {
		code, reached := doGuardBody(proxyAuthMode, tc.method, "/api/v1/portview/CAP/api/ping", tc.body, tc.headers)
		if !reached || code != http.StatusOK {
			t.Fatalf("portview %s: want pass-through, got code=%d reached=%v", tc.method, code, reached)
		}
	}
}

func TestProxyWriteGuard_CrossSiteRefused(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"sec-fetch-cross-site", map[string]string{"Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"}},
		{"origin-null", map[string]string{"Origin": "null", "Content-Type": "application/json"}},
		{"foreign-origin", map[string]string{"Origin": "https://foreign.example", "Content-Type": "application/json"}},
		{"same-site-foreign-origin", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://sibling.example", "Content-Type": "application/json"}},
		{"same-site-no-origin", map[string]string{"Sec-Fetch-Site": "same-site", "Content-Type": "application/json"}},
		{"unknown-sec-fetch", map[string]string{"Sec-Fetch-Site": "weird", "Content-Type": "application/json"}},
	}
	for _, c := range cases {
		code, reached := doGuardBody(proxyAuthMode, http.MethodPost, "/api/v1/projects", jsonBody, c.headers)
		if reached || code != http.StatusForbidden {
			t.Fatalf("%s: want 403 and not reached, got code=%d reached=%v", c.name, code, reached)
		}
	}
}

func TestProxyWriteGuard_SameSiteAndNonBrowserAllowed(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"sec-fetch-same-origin", map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}},
		{"sec-fetch-same-site-matching-origin", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://hub.example", "Content-Type": "application/json"}},
		{"sec-fetch-none", map[string]string{"Sec-Fetch-Site": "none", "Content-Type": "application/json"}},
		{"same-origin-header", map[string]string{"Origin": "http://hub.example", "Content-Type": "application/json"}},
		{"no-browser-headers-json", map[string]string{"Content-Type": "application/json"}},
	}
	for _, c := range cases {
		code, reached := doGuardBody(proxyAuthMode, http.MethodPost, "/api/v1/projects", jsonBody, c.headers)
		if !reached || code != http.StatusOK {
			t.Fatalf("%s: want pass-through, got code=%d reached=%v", c.name, code, reached)
		}
	}
}

// Second check: a browser POST with a body must be application/json (or
// multipart on an upload route). text/plain, form, octet-stream, an absent or
// empty content type and malformed content types get 415.
func TestProxyWriteGuard_Belt2RefusedContentTypes(t *testing.T) {
	bad := []string{
		"text/plain",
		"text/plain; charset=utf-8",
		"text/plain; charset",    // malformed param, essence text/plain
		"text/plain;;",           // empty params, essence text/plain
		"TEXT/PLAIN; q=1",        // case-insensitive essence text/plain
		"text/plain ; charset=x", // space before ';', essence text/plain
		"application/x-www-form-urlencoded",
		"application/x-www-form-urlencoded; charset=utf-8",
		"application/octet-stream",
		"garbage",      // no subtype -> unparseable
		"/json",        // empty type -> unparseable
		"application/", // empty subtype -> unparseable
		"",             // absent/empty content type with a body
	}
	for _, ct := range bad {
		h := map[string]string{"Sec-Fetch-Site": "same-origin"}
		if ct != "" {
			h["Content-Type"] = ct
		}
		code, reached := doGuardBody(proxyAuthMode, http.MethodPost, "/api/v1/projects", jsonBody, h)
		if reached || code != http.StatusUnsupportedMediaType {
			t.Fatalf("content-type %q: want 415 not reached, got code=%d reached=%v", ct, code, reached)
		}
	}
}

// A Content-Type whose essence is application/json is accepted even when its
// parameters are malformed: only the essence decides.
func TestProxyWriteGuard_Belt2JSONEssenceMalformedParamsAllowed(t *testing.T) {
	for _, ct := range []string{
		"application/json; q=\"unterminated",
		"application/json;;charset=utf-8;charset=utf-8",
		"application/json ; charset=utf-8",
	} {
		code, reached := doGuardBody(proxyAuthMode, http.MethodPost, "/api/v1/projects", jsonBody, map[string]string{
			"Sec-Fetch-Site": "same-origin",
			"Content-Type":   ct,
		})
		if !reached || code != http.StatusOK {
			t.Fatalf("content-type %q: want pass-through (json essence), got code=%d reached=%v", ct, code, reached)
		}
	}
}

func TestProxyWriteGuard_Belt2JSONAllowed(t *testing.T) {
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "APPLICATION/JSON"} {
		code, reached := doGuardBody(proxyAuthMode, http.MethodPost, "/api/v1/projects", jsonBody, map[string]string{
			"Sec-Fetch-Site": "same-origin",
			"Content-Type":   ct,
		})
		if !reached || code != http.StatusOK {
			t.Fatalf("content-type %q: want pass-through, got code=%d reached=%v", ct, code, reached)
		}
	}
}

func TestProxyWriteGuard_Belt2BodylessPostAllowed(t *testing.T) {
	// A browser POST with no body (e.g. an action trigger) is allowed even
	// with no content type.
	code, reached := doGuard(proxyAuthMode, http.MethodPost, "/api/v1/agents/abc/start", map[string]string{
		"Sec-Fetch-Site": "same-origin",
	})
	if !reached || code != http.StatusOK {
		t.Fatalf("bodyless POST: want pass-through, got code=%d reached=%v", code, reached)
	}
}

func TestProxyWriteGuard_Belt2PostOnly(t *testing.T) {
	// The second check does not apply to PUT/PATCH/DELETE (they legitimately
	// carry non-JSON file bodies). These requests carry Sec-Fetch-Site:
	// same-origin, so the first check passes.
	for _, m := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, ct := range []string{"text/plain", "application/octet-stream"} {
			code, reached := doGuardBody(proxyAuthMode, m, "/api/v1/projects/p/workspace/files/a.txt", "data", map[string]string{
				"Sec-Fetch-Site": "same-origin",
				"Content-Type":   ct,
			})
			if !reached || code != http.StatusOK {
				t.Fatalf("%s %q: want pass-through (the content-type check is POST-only), got code=%d reached=%v", m, ct, code, reached)
			}
		}
	}
}

func TestProxyWriteGuard_Belt2BrowserOnly(t *testing.T) {
	// A POST that is not from a browser (neither Origin nor Sec-Fetch-Site) is
	// not subject to the second check.
	for _, ct := range []string{"text/plain", "application/octet-stream", "application/x-www-form-urlencoded"} {
		code, reached := doGuardBody(proxyAuthMode, http.MethodPost, "/api/v1/projects", jsonBody, map[string]string{
			"Content-Type": ct,
		})
		if !reached || code != http.StatusOK {
			t.Fatalf("non-browser POST %q: want pass-through, got code=%d reached=%v", ct, code, reached)
		}
	}
}

// uploadRouteExamples is one concrete request path per multipart upload pattern.
var uploadRouteExamples = []string{
	"/api/v1/admin/allow-list/import",
	"/api/v1/admin/users/invite/bulk",
	"/api/v1/chat/attachments",
	"/api/v1/templates/tpl123/files",
	"/api/v1/harness-configs/hc123/files",
	"/api/v1/skills/skill123/versions",
	"/api/v1/projects/proj123/workspace/files",
	"/api/v1/projects/proj123/shared-dirs/shared1/files",
}

// siblingNonUploadPaths are paths near an upload pattern that must NOT be
// treated as upload routes (no over-broad matching).
var siblingNonUploadPaths = []string{
	"/api/v1/projects",
	"/api/v1/projects/proj123",
	"/api/v1/admin/allow-list",                         // JSON single-add (not the CSV import)
	"/api/v1/templates/tpl123/files/readme.md",         // a file under the upload dir (deeper)
	"/api/v1/skills/skill123/upload",                   // JSON two-phase upload-URL endpoint
	"/api/v1/projects/proj123/workspace",               // workspace default (no handler)
	"/api/v1/projects/proj123/workspace/files/sub.txt", // a workspace file write (deeper)
	"/api/v1/projects/proj123/shared-dirs/shared1",     // shared-dir itself
	"/api/v1/harness-configs/hc123",                    // the config itself
}

func TestProxyWriteGuard_MultipartOnlyOnUploadRoutes(t *testing.T) {
	mp := "multipart/form-data; boundary=x"
	for _, p := range siblingNonUploadPaths {
		code, reached := doGuardBody(proxyAuthMode, http.MethodPost, p, jsonBody, map[string]string{
			"Sec-Fetch-Site": "same-origin", "Content-Type": mp,
		})
		if reached || code != http.StatusUnsupportedMediaType {
			t.Fatalf("sibling %q multipart: want 415 not reached, got code=%d reached=%v", p, code, reached)
		}
	}
	for _, route := range uploadRouteExamples {
		codeOK, reachedOK := doGuardBody(proxyAuthMode, http.MethodPost, route, jsonBody, map[string]string{
			"Sec-Fetch-Site": "same-origin", "Content-Type": mp,
		})
		if !reachedOK || codeOK != http.StatusOK {
			t.Fatalf("upload route %q same-site multipart: want pass-through, got code=%d reached=%v", route, codeOK, reachedOK)
		}
		// The first check still applies on an upload route.
		codeXS, reachedXS := doGuardBody(proxyAuthMode, http.MethodPost, route, jsonBody, map[string]string{
			"Origin": "null", "Content-Type": mp,
		})
		if reachedXS || codeXS != http.StatusForbidden {
			t.Fatalf("upload route %q cross-site multipart: want 403 not reached, got code=%d reached=%v", route, codeXS, reachedXS)
		}
		// An upload route still refuses text/plain and form (only multipart is
		// added).
		for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded"} {
			codeCT, reachedCT := doGuardBody(proxyAuthMode, http.MethodPost, route, jsonBody, map[string]string{
				"Sec-Fetch-Site": "same-origin", "Content-Type": ct,
			})
			if reachedCT || codeCT != http.StatusUnsupportedMediaType {
				t.Fatalf("upload route %q %q: want 415 not reached, got code=%d reached=%v", route, ct, codeCT, reachedCT)
			}
		}
	}
}

func TestIsMultipartUploadRoute_Units(t *testing.T) {
	for _, p := range uploadRouteExamples {
		if !isMultipartUploadRoute(p) {
			t.Fatalf("isMultipartUploadRoute(%q) = false, want true", p)
		}
		if !isMultipartUploadRoute(p + "/") {
			t.Fatalf("isMultipartUploadRoute(%q + slash) = false, want true", p)
		}
	}
	for _, p := range siblingNonUploadPaths {
		if isMultipartUploadRoute(p) {
			t.Fatalf("isMultipartUploadRoute(%q) = true, want false", p)
		}
	}
	if isMultipartUploadRoute("/api/v1/templates//files") {
		t.Fatalf("empty id segment matched an upload pattern")
	}
}

// TestProxyWriteGuard_SandboxedPageWriteRefused: a text/plain write to a JSON
// endpoint from a sandboxed page is refused before it reaches the handler.
func TestProxyWriteGuard_SandboxedPageWriteRefused(t *testing.T) {
	code, reached := doGuardBody(proxyAuthMode, http.MethodPost, "/api/v1/projects", jsonBody, map[string]string{
		"Origin":         "null",
		"Sec-Fetch-Site": "cross-site",
		"Content-Type":   "text/plain",
	})
	if reached || code != http.StatusForbidden {
		t.Fatalf("sandboxed page write: want 403 and not reached, got code=%d reached=%v", code, reached)
	}
}

func TestProxyWriteGuard_AllStateChangingMethods(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		code, reached := doGuard(proxyAuthMode, m, "/api/v1/agents/abc", map[string]string{
			"Sec-Fetch-Site": "cross-site",
		})
		if reached || code != http.StatusForbidden {
			t.Fatalf("method %s cross-site: want 403 not reached, got code=%d reached=%v", m, code, reached)
		}
	}
}

func TestRequestIsCrossSite_Units(t *testing.T) {
	mk := func(h map[string]string, host string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://"+host+"/api/v1/x", nil)
		r.Host = host
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		name string
		req  *http.Request
		want bool
	}{
		{"sec-fetch same-origin wins over null origin", mk(map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "null"}, "hub.example"), false},
		{"sec-fetch cross-site", mk(map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://hub.example"}, "hub.example"), true},
		{"same-site matching origin", mk(map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://hub.example"}, "hub.example"), false},
		{"same-site foreign origin", mk(map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://sib.example"}, "hub.example"), true},
		{"same-site no origin", mk(map[string]string{"Sec-Fetch-Site": "same-site"}, "hub.example"), true},
		{"none", mk(map[string]string{"Sec-Fetch-Site": "none"}, "hub.example"), false},
		{"unknown sec-fetch value", mk(map[string]string{"Sec-Fetch-Site": "whatever"}, "hub.example"), true},
		{"origin null no sec-fetch", mk(map[string]string{"Origin": "null"}, "hub.example"), true},
		{"origin match", mk(map[string]string{"Origin": "http://hub.example"}, "hub.example"), false},
		{"origin mismatch", mk(map[string]string{"Origin": "http://other.example"}, "hub.example"), true},
		{"no headers", mk(map[string]string{}, "hub.example"), false},
		{"malformed origin", mk(map[string]string{"Origin": "://::"}, "hub.example"), true},
	}
	for _, c := range cases {
		if got := requestIsCrossSite(c.req); got != c.want {
			t.Fatalf("%s: requestIsCrossSite = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRequestMediaType_Units(t *testing.T) {
	mk := func(ct string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://h/api/v1/x", strings.NewReader("x"))
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		return r
	}
	cases := []struct {
		ct   string
		want string
		ok   bool
	}{
		{"application/json", "application/json", true},
		{"application/json; charset=utf-8", "application/json", true},
		{"TEXT/Plain", "text/plain", true},
		{"text/plain; charset", "text/plain", true},
		{"multipart/form-data; boundary=x", "multipart/form-data", true},
		{"", "", false},
		{"garbage", "", false},
		{"/json", "", false},
		{"application/", "", false},
	}
	for _, c := range cases {
		mt, ok := requestMediaType(mk(c.ct))
		if mt != c.want || ok != c.ok {
			t.Fatalf("requestMediaType(%q) = (%q,%v), want (%q,%v)", c.ct, mt, ok, c.want, c.ok)
		}
	}
}

// policyReportTypes are the two media types browsers send policy reports in.
var policyReportTypes = []string{
	"application/reports+json",
	"application/csp-report",
	"Application/Reports+JSON; charset=utf-8",
}

const policyReportBody = `[{"type":"csp-violation","body":{}}]`

func TestProxyWriteGuard_PolicyReportTypesOnReportRoute(t *testing.T) {
	for _, ct := range policyReportTypes {
		for _, sig := range []map[string]string{
			{"Sec-Fetch-Site": "same-origin", "Content-Type": ct},
			{"Origin": "http://hub.example", "Content-Type": ct},
		} {
			code, reached := doGuardBody(proxyAuthMode, http.MethodPost, cspReportPath, policyReportBody, sig)
			if !reached || code != http.StatusOK {
				t.Fatalf("report route %q %v: want pass-through, got code=%d reached=%v", ct, sig, code, reached)
			}
		}
		// The first check still applies on the report route.
		for _, origin := range []string{"null", "https://other.example"} {
			code, reached := doGuardBody(proxyAuthMode, http.MethodPost, cspReportPath, policyReportBody, map[string]string{
				"Origin": origin, "Content-Type": ct,
			})
			if reached || code != http.StatusForbidden {
				t.Fatalf("report route %q origin %q: want 403 not reached, got code=%d reached=%v", ct, origin, code, reached)
			}
		}
	}
}

func TestProxyWriteGuard_PolicyReportTypesRefusedElsewhere(t *testing.T) {
	paths := []string{
		"/api/v1/csp-reports/",
		"/api/v1/csp-reports/x",
		"/api/v1/csp-reportsx",
		"/api/v1/chat/messages",
		"/api/v1/agents",
	}
	for _, p := range append(paths, uploadRouteExamples...) {
		for _, ct := range policyReportTypes {
			code, reached := doGuardBody(proxyAuthMode, http.MethodPost, p, policyReportBody, map[string]string{
				"Sec-Fetch-Site": "same-origin", "Content-Type": ct,
			})
			if reached || code != http.StatusUnsupportedMediaType {
				t.Fatalf("%q %q: want 415 not reached, got code=%d reached=%v", p, ct, code, reached)
			}
		}
	}
}

func TestProxyWriteGuard_PolicyReportRouteRefusesOtherTypes(t *testing.T) {
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		code, reached := doGuardBody(proxyAuthMode, http.MethodPost, cspReportPath, policyReportBody, map[string]string{
			"Sec-Fetch-Site": "same-origin", "Content-Type": ct,
		})
		if reached || code != http.StatusUnsupportedMediaType {
			t.Fatalf("report route %q: want 415 not reached, got code=%d reached=%v", ct, code, reached)
		}
	}
}

func TestIsPolicyReportRoute_Units(t *testing.T) {
	if cspReportPath != "/api/v1/csp-reports" {
		t.Fatalf("cspReportPath = %q, want /api/v1/csp-reports", cspReportPath)
	}
	if !isPolicyReportRoute("/api/v1/csp-reports") {
		t.Fatal("exact report route not matched")
	}
	for _, p := range []string{"/api/v1/csp-reports/", "/api/v1/csp-reports/a", "/api/v1/CSP-REPORTS", "/api/v1/csp-report", ""} {
		if isPolicyReportRoute(p) {
			t.Fatalf("%q matched, want only the exact route", p)
		}
	}
}
