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

// artifactPathCases are spellings of artifact credential paths (and a few
// other artifact paths); every one must be redacted on every sink.
var artifactPathCases = []string{
	"/api/v1/artifacts/shared/TOKEN",
	"/api/v1/artifacts/shared/TOKEN/files/a/b.png",
	"/api/v1/artifacts/view/TOKEN/index.html",
	"/api/v1/artifacts/shared/../shared/TOKEN",
	"/api/v1//artifacts/shared/TOKEN",
	"/api/v1/artifacts/x/../shared/TOKEN",
	"/api/v1/artifacts/%73hared/TOKEN",
	"/api/v1/artifacts/a%2Fb/../shared/TOKEN",
	"/api/v1/artifacts/a%2fb/..%2fview%2fTOKEN",
	"/api/v1/artifacts/%2e%2e/../shared/TOKEN",
	"/api/v1/artifacts/%2E%2E/../shared/TOKEN",
	"/api/v1/artifacts/.%2e/../shared/TOKEN",
	"/api/v1/%61rtifacts/shared/TOKEN",
	"/api/v1/%2561rtifacts/shared/TOKEN",
	"/API/V1/ARTIFACTS/SHARED/TOKEN",
	"/api/v1/artifacts/00000000-0000-4000-8000-000000000001/files/TOKEN.md",
}

// TestArtifactPathsRedactedOnEverySink: for every spelling, no part of the
// token survives in RequestPath, RedactURL or the trace predicate, whether
// the request arrives with the spelling as its raw path or parsed.
func TestArtifactPathsRedactedOnEverySink(t *testing.T) {
	const tok = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCd"
	for _, c := range artifactPathCases {
		raw := strings.ReplaceAll(c, "TOKEN", tok)
		u, err := url.Parse("https://hub.example" + raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		r := httptest.NewRequest(http.MethodGet, raw, nil)
		if got := RequestPath(r); strings.Contains(got, tok) || got != RedactedArtifactPath {
			t.Errorf("RequestPath(%s) = %q", raw, got)
		}
		if got := RedactURL(u); strings.Contains(got, tok) {
			t.Errorf("RedactURL(%s) = %q", raw, got)
		}
		if !IsCredentialURL(u) || !IsCredentialURL(r.URL) {
			t.Errorf("IsCredentialURL(%s) = false", raw)
		}
	}
}

// TestNonArtifactPathsKept: other request paths are logged as they are.
func TestNonArtifactPathsKept(t *testing.T) {
	for _, p := range []string{"/api/v1/agents/a1", "/api/v1/projects/p/shared-dirs", "/", "/healthz"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		if got := RequestPath(r); got != p {
			t.Errorf("RequestPath(%s) = %q", p, got)
		}
		if IsCredentialURL(r.URL) {
			t.Errorf("IsCredentialURL(%s) = true", p)
		}
	}
	if RequestPath(nil) != "" {
		t.Errorf("RequestPath(nil)")
	}
}
