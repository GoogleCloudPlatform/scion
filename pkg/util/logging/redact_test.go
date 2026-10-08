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

package logging

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRedactQuery(t *testing.T) {
	cases := map[string]string{
		"":                                 "",
		"raw=1&version=1.0.0":              "raw=1&version=1.0.0",
		"raw=1&version=1.0.0&exp=5&sig=AB": "raw=1&version=1.0.0&exp=5&sig=REDACTED",
		"sig=AB&raw=1":                     "sig=REDACTED&raw=1",
		"sig=AB&sig=CD":                    "sig=REDACTED&sig=REDACTED",
		"%73ig=AB":                         "%73ig=REDACTED",
		"SIG=AB":                           "SIG=REDACTED",
		"sig":                              "sig",
		"sig=":                             "sig=REDACTED",
		"signature=keep&xsig=keep":         "signature=keep&xsig=keep",
		"%zz=AB&sig=CD":                    "REDACTED",
	}
	for in, want := range cases {
		if got := RedactQuery(in); got != want {
			t.Errorf("RedactQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	u, err := url.Parse("https://hub.example.com/api/v1/skills/x/files/SKILL.md?raw=1&exp=5&sig=SECRETSIG")
	if err != nil {
		t.Fatal(err)
	}
	got := RedactURL(u)
	if strings.Contains(got, "SECRETSIG") {
		t.Fatalf("signature leaked: %s", got)
	}
	if want := "https://hub.example.com/api/v1/skills/x/files/SKILL.md?raw=1&exp=5&sig=REDACTED"; got != want {
		t.Fatalf("RedactURL = %q, want %q", got, want)
	}
	if u.RawQuery != "raw=1&exp=5&sig=SECRETSIG" {
		t.Fatalf("input URL was modified: %q", u.RawQuery)
	}
	if RedactURL(nil) != "" {
		t.Fatal("RedactURL(nil) should be empty")
	}
}

func TestRequestLogMiddleware_RedactsSignature(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := RequestLogMiddleware(logger, "hub", HubPathPatterns(), 0)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills/x/files/SKILL.md?raw=1&version=1.0.0&exp=5&sig=SECRETSIG", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if strings.Contains(out, "SECRETSIG") {
		t.Fatalf("request log leaked the signature: %s", out)
	}
	if !strings.Contains(out, "sig=REDACTED") {
		t.Fatalf("request log missing redacted URL: %s", out)
	}
}

func TestRedactPath(t *testing.T) {
	const tok = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCd"
	for in, want := range map[string]string{
		"/api/v1/artifacts/shared/" + tok:                        "/api/v1/artifacts/shared/REDACTED",
		"/api/v1/artifacts/shared/" + tok + "/files/a/b.png":     "/api/v1/artifacts/shared/REDACTED",
		"/api/v1/artifacts/shared/../shared/" + tok:              "/api/v1/artifacts/shared/REDACTED",
		"/api/v1//artifacts/shared/" + tok:                       "/api/v1/artifacts/shared/REDACTED",
		"/api/v1/artifacts/x/../shared/" + tok:                   "/api/v1/artifacts/shared/REDACTED",
		"/api/v1/artifacts/view/id.1.2.sig/index.html":           "/api/v1/artifacts/view/REDACTED",
		"/api/v1/artifacts/00000000-0000-4000-8000-000000000001": "/api/v1/artifacts/00000000-0000-4000-8000-000000000001",
		"/api/v1/agents/a1":                                      "/api/v1/agents/a1",
		"":                                                       "",
	} {
		if got := RedactPath(in); got != want {
			t.Errorf("RedactPath(%q) = %q, want %q", in, got, want)
		}
		if got := RedactPath(in); strings.Contains(got, tok) {
			t.Errorf("RedactPath(%q) keeps the token", in)
		}
	}
	u, _ := url.Parse("https://hub.example/api/v1/artifacts/%73hared/" + tok + "?sig=x&a=1")
	got := RedactURL(u)
	if strings.Contains(got, tok) || strings.Contains(got, "sig=x") {
		t.Errorf("RedactURL = %q", got)
	}
	if !IsCredentialPath("/api/v1/artifacts/shared/x") || IsCredentialPath("/api/v1/artifacts/abc") {
		t.Errorf("IsCredentialPath")
	}
}

// TestRedactPathEscapedSlash: a path whose escaped and decoded forms clean
// to different places is redacted whichever form a caller passes.
func TestRedactPathEscapedSlash(t *testing.T) {
	const tok = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCd"
	raw := "/api/v1/artifacts/a%2Fb/../shared/" + tok
	u, err := url.Parse("https://hub.example" + raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{u.Path, u.EscapedPath(), raw, "/api/v1/artifacts/a%2fb/..%2fview%2f" + tok} {
		if got := RedactPath(p); strings.Contains(got, tok) || !IsCredentialPath(p) {
			t.Errorf("RedactPath(%q) = %q", p, got)
		}
	}
	if got := RedactURL(u); strings.Contains(got, tok) {
		t.Errorf("RedactURL = %q", got)
	}
	// Paths outside the artifact routes are left alone.
	for _, p := range []string{"/api/v1/agents/a/shared/x", "/shared/x", "/api/v1/projects/p/view/x"} {
		if RedactPath(p) != p {
			t.Errorf("RedactPath(%q) = %q, want it unchanged", p, RedactPath(p))
		}
	}
}
