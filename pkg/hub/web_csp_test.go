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

package hub

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"
)

// cspDirective returns the source list of one CSP directive, or nil if
// the policy does not contain it.
func cspDirective(policy, name string) []string {
	for _, d := range strings.Split(policy, ";") {
		fields := strings.Fields(d)
		if len(fields) > 0 && fields[0] == name {
			return fields[1:]
		}
	}
	return nil
}

func containsSource(sources []string, want string) bool {
	for _, s := range sources {
		if s == want {
			return true
		}
	}
	return false
}

// TestSecurityHeadersCSPAllowsBlobImages guards the chat file preview,
// which renders fetched image bytes as <img src="blob:...">. Without
// blob: in img-src the browser blocks every such image.
func TestSecurityHeadersCSPAllowsBlobImages(t *testing.T) {
	ws := &WebServer{}
	h := ws.securityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))

	policy := rec.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("Content-Security-Policy header not set")
	}

	img := cspDirective(policy, "img-src")
	for _, want := range []string{"'self'", "data:", "blob:", "https:"} {
		if !containsSource(img, want) {
			t.Errorf("img-src %v is missing %s", img, want)
		}
	}

	// blob: is for images only. Script and the default fallback must
	// not pick it up.
	for _, name := range []string{"default-src", "script-src"} {
		if src := cspDirective(policy, name); containsSource(src, "blob:") {
			t.Errorf("%s must not allow blob:, got %v", name, src)
		}
	}
}

// scriptSourcesOf returns the script-src source list of policy, failing
// the test if the policy has none.
func scriptSourcesOf(t *testing.T, policy string) []string {
	t.Helper()
	src := cspDirective(policy, "script-src")
	if src == nil {
		t.Fatalf("policy %q has no script-src", policy)
	}
	return src
}

// policyNonce returns the one 'nonce-<n>' source of script-src, failing
// the test unless there is exactly one.
func policyNonce(t *testing.T, policy string) string {
	t.Helper()
	var nonces []string
	for _, s := range scriptSourcesOf(t, policy) {
		if strings.HasPrefix(s, "'nonce-") && strings.HasSuffix(s, "'") {
			nonces = append(nonces, strings.TrimSuffix(strings.TrimPrefix(s, "'nonce-"), "'"))
		}
	}
	if len(nonces) != 1 {
		t.Fatalf("script-src of %q has %d nonces, want exactly 1", policy, len(nonces))
	}
	return nonces[0]
}

// assertNoInlineAllowance fails if script-src (or default-src, its
// fallback) lets inline script or string evaluation run.
func assertNoInlineAllowance(t *testing.T, policy string) {
	t.Helper()
	for _, name := range []string{"script-src", "default-src"} {
		for _, bad := range []string{"'unsafe-inline'", "'unsafe-eval'", "'unsafe-hashes'", "*", "data:", "blob:"} {
			if containsSource(cspDirective(policy, name), bad) {
				t.Errorf("%s must not contain %s: %q", name, bad, policy)
			}
		}
	}
	for _, name := range []string{"script-src-elem", "script-src-attr"} {
		if cspDirective(policy, name) != nil {
			t.Errorf("policy must not carry %s, which would override script-src: %q", name, policy)
		}
	}
}

// shellScript is one <script> element of a shell document.
type shellScript struct {
	attrs map[string]string
	text  string
}

// shellScripts parses doc and returns its script elements in order.
func shellScripts(t *testing.T, doc string) []shellScript {
	t.Helper()
	root, err := xhtml.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parsing shell: %v", err)
	}
	var out []shellScript
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "script" {
			s := shellScript{attrs: map[string]string{}}
			for _, a := range n.Attr {
				s.attrs[a.Key] = a.Val
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == xhtml.TextNode {
					s.text += c.Data
				}
			}
			out = append(out, s)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

// inlineExecutableScripts returns the scripts a browser would run from
// the document itself: no src, and a JavaScript type (none, or a
// JavaScript MIME type, or "module").
func inlineExecutableScripts(scripts []shellScript) []shellScript {
	var out []shellScript
	for _, s := range scripts {
		if _, hasSrc := s.attrs["src"]; hasSrc {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(s.attrs["type"])) {
		case "", "module", "text/javascript", "application/javascript":
			out = append(out, s)
		}
	}
	return out
}

// pathSubtestName turns a request path into a subtest name without
// slashes, so each subtest is one name level ("root" for "/").
func pathSubtestName(path string) string {
	name := strings.ReplaceAll(strings.Trim(path, "/"), "/", "_")
	if name == "" {
		return "root"
	}
	return name
}

// shellPaths are SPA routes served by the shell, one per shell variant.
var shellPaths = []string{"/", "/login", "/invite", "/projects/p-1", "/agents/a-1/terminal"}

// TestSPAShellScriptPolicy is the regression test for the page script
// policy: every SPA shell response allows inline script only through a
// nonce, never through 'unsafe-inline'; the shell's inline scripts all
// carry that nonce; and the report-only policy and its report endpoint
// are announced.
func TestSPAShellScriptPolicy(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{AssetsDir: writeFingerprintFixture(t, shellBootManifest)})
	h := ws.securityHeadersMiddleware(ws.spaHandler())
	for _, path := range shellPaths {
		t.Run(pathSubtestName(path), func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			policy := rec.Header().Get("Content-Security-Policy")
			if got := rec.Header().Values("Content-Security-Policy"); len(got) != 1 {
				t.Fatalf("got %d Content-Security-Policy headers, want 1", len(got))
			}
			assertNoInlineAllowance(t, policy)
			nonce := policyNonce(t, policy)
			if !containsSource(scriptSourcesOf(t, policy), "'self'") {
				t.Errorf("script-src must keep 'self' for the bundled scripts: %q", policy)
			}

			scripts := shellScripts(t, rec.Body.String())
			inline := inlineExecutableScripts(scripts)
			// Positive control: the shell does have its theme script, so
			// the nonce checks below cannot pass on an empty page.
			if len(inline) != 1 || !strings.Contains(inline[0].text, "scion-theme") {
				t.Fatalf("want exactly one inline script (theme detection), got %d", len(inline))
			}
			for _, s := range inline {
				if s.attrs["nonce"] != nonce {
					t.Errorf("inline script nonce = %q, want the policy's %q", s.attrs["nonce"], nonce)
				}
			}
			// The module entry and the page data block are unchanged.
			var sawEntry, sawData bool
			for _, s := range scripts {
				if s.attrs["src"] == "/assets/main.js" && s.attrs["type"] == "module" {
					sawEntry = true
				}
				if s.attrs["id"] == "__SCION_DATA__" && s.attrs["type"] == "application/json" {
					sawData = true
				}
			}
			if !sawEntry || !sawData {
				t.Errorf("entry script present = %v, data block present = %v; want both", sawEntry, sawData)
			}

			if got := rec.Header().Get("Content-Security-Policy-Report-Only"); got != "require-trusted-types-for 'script'; report-to csp" {
				t.Errorf("Content-Security-Policy-Report-Only = %q", got)
			}
			if got := rec.Header().Get("Reporting-Endpoints"); got != `csp="/api/v1/csp-reports"` {
				t.Errorf("Reporting-Endpoints = %q", got)
			}
		})
	}
}

// TestSPAShellScriptNonceIsFreshPerResponse: each response gets its own
// nonce, and each shell carries the nonce of its own response.
func TestSPAShellScriptNonceIsFreshPerResponse(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{AssetsDir: writeFingerprintFixture(t, shellBootManifest)})
	h := ws.securityHeadersMiddleware(ws.spaHandler())
	seen := map[string]bool{}
	const responses = 20
	for i := 0; i < responses; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/projects", nil))
		nonce := policyNonce(t, rec.Header().Get("Content-Security-Policy"))
		raw, err := base64.RawURLEncoding.DecodeString(nonce)
		if err != nil || len(raw) != scriptNonceBytes {
			t.Fatalf("nonce %q is not %d base64 bytes (err %v)", nonce, scriptNonceBytes, err)
		}
		inline := inlineExecutableScripts(shellScripts(t, rec.Body.String()))
		if len(inline) != 1 || inline[0].attrs["nonce"] != nonce {
			t.Fatalf("response %d: shell script does not carry its response's nonce %q", i, nonce)
		}
		seen[nonce] = true
	}
	if len(seen) != responses {
		t.Fatalf("%d responses produced %d distinct nonces", responses, len(seen))
	}
}

// TestSPAShellWithoutMiddlewareStillMatches: the shell served without the
// security headers middleware sets a policy itself, with the nonce its
// inline script carries.
func TestSPAShellWithoutMiddlewareStillMatches(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{AssetsDir: writeFingerprintFixture(t, shellBootManifest)})
	rec := httptest.NewRecorder()
	ws.spaHandler()(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	policy := rec.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("no Content-Security-Policy on a shell served without the middleware")
	}
	assertNoInlineAllowance(t, policy)
	nonce := policyNonce(t, policy)
	inline := inlineExecutableScripts(shellScripts(t, rec.Body.String()))
	if len(inline) != 1 || inline[0].attrs["nonce"] != nonce {
		t.Fatalf("shell script does not carry the policy nonce %q", nonce)
	}
}

// TestWebResponsesScriptPolicy: the policy on the full web handler chain,
// for the shell and for responses other than the shell. Each carries a
// nonce and no inline allowance; only the shell announces the report-only
// policy.
func TestWebResponsesScriptPolicy(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{AssetsDir: writeFingerprintFixture(t, shellBootManifest)})
	ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}), func(context.Context) error { return nil })
	h := ws.Handler()

	cases := []struct {
		path   string
		shell  bool
		status int // checked when not zero
	}{
		{"/login", true, http.StatusOK},
		// Not signed in and not a browser: refused, not a shell response.
		{"/", false, http.StatusUnauthorized},
		{"/assets/main.js", false, http.StatusOK},
		{"/api/v1/settings/public", false, http.StatusOK},
		{"/healthz", false, 0},
	}
	for _, tc := range cases {
		t.Run(pathSubtestName(tc.path), func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if tc.status != 0 && rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
			policy := rec.Header().Get("Content-Security-Policy")
			assertNoInlineAllowance(t, policy)
			nonce := policyNonce(t, policy)
			_, hasReportOnly := rec.Header()["Content-Security-Policy-Report-Only"]
			if hasReportOnly != tc.shell {
				t.Errorf("report-only policy present = %v, want %v", hasReportOnly, tc.shell)
			}
			if tc.shell {
				inline := inlineExecutableScripts(shellScripts(t, rec.Body.String()))
				if len(inline) != 1 || inline[0].attrs["nonce"] != nonce {
					t.Fatalf("shell script does not carry the policy nonce %q", nonce)
				}
			}
		})
	}
}

// TestAgentPortProxyStripsEveryHubPageHeader: every header the web
// server's security middleware sets is one the agent port proxies do not
// relay from the app, so the hub's own page headers are never combined
// with the app's, including a header added to the middleware later.
func TestAgentPortProxyStripsEveryHubPageHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	(&WebServer{}).securityHeadersMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if len(rec.Header()) == 0 {
		t.Fatal("the middleware set no headers; the check would pass vacuously")
	}
	for name := range rec.Header() {
		if !stripFromProxyResponse(name) {
			t.Errorf("%s is set by the web server but relayed from agent port apps", name)
		}
	}
	// Positive control: a header the hub does not set is relayed.
	if stripFromProxyResponse("X-Agent-Kept") {
		t.Error("X-Agent-Kept is not a hub header but is stripped")
	}
}

// TestUntrustedContentPolicyRefusesEmbedding: pages served under the
// sandbox policy (agent ports, workspace files, attachments) can't be
// embedded in other pages, with or without the web frontend.
func TestUntrustedContentPolicyRefusesEmbedding(t *testing.T) {
	for _, csp := range []string{untrustedContentSandboxCSP, agentServedPageSandboxCSP} {
		if got := cspDirective(csp, "frame-ancestors"); len(got) != 1 || got[0] != "'none'" {
			t.Errorf("%q: frame-ancestors = %v, want ['none']", csp, got)
		}
		// Positive control: the sandbox directive is still there, scripts
		// allowed and never allow-same-origin.
		sandbox := cspDirective(csp, "sandbox")
		if !containsSource(sandbox, "allow-scripts") {
			t.Errorf("sandbox = %v, want allow-scripts", sandbox)
		}
		if containsSource(sandbox, "allow-same-origin") {
			t.Errorf("sandbox = %v must not allow-same-origin", sandbox)
		}
	}
}
