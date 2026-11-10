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
		"code=AB&next=%2F":                 "code=REDACTED&next=%2F",
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

// portViewPathCases are spellings of port view paths; every one must be
// redacted on every sink.
var portViewPathCases = []string{
	"/api/v1/portview/TOKEN/",
	"/api/v1/portview/TOKEN/index.html",
	"/api/v1/portview/TOKEN/assets/app.js",
	"/api/v1//portview/TOKEN/x",
	"/api/v1/x/../portview/TOKEN/x",
	"/api/v1/%70ortview/TOKEN/x",
	"/api/v1/%2570ortview/TOKEN/x",
	"/API/V1/PORTVIEW/TOKEN/X",
	"/api/v1/portview%2fTOKEN%2fx",
}

// TestPortViewPathsRedactedOnEverySink: for every spelling, no part of the
// page link survives in RequestPath, RedactURL or the trace predicate.
func TestPortViewPathsRedactedOnEverySink(t *testing.T) {
	const tok = "v1.0b6f2c1e-4a8d-4f5e-9c3b-2d1e0f9a8b7c.8080.7d3e9f10-2b4c-4d6e-8f01-a2b3c4d5e6f7.1800000000.AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbC"
	for _, c := range portViewPathCases {
		raw := strings.ReplaceAll(c, "TOKEN", tok)
		u, err := url.Parse("https://hub.example" + raw + "?a=1")
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		r := httptest.NewRequest(http.MethodGet, raw, nil)
		if got := RequestPath(r); strings.Contains(got, tok) || got != RedactedPortViewPath {
			t.Errorf("RequestPath(%s) = %q", raw, got)
		}
		if got := RedactURL(u); strings.Contains(got, tok) || got != "https://hub.example"+RedactedPortViewPath+"?a=1" {
			t.Errorf("RedactURL(%s) = %q", raw, got)
		}
		if !IsCredentialURL(u) || !IsCredentialURL(r.URL) {
			t.Errorf("IsCredentialURL(%s) = false", raw)
		}
	}
}

// TestRequestLogMiddleware_RedactsPortViewPath: the request log line of a
// port view request holds no part of the page link.
func TestRequestLogMiddleware_RedactsPortViewPath(t *testing.T) {
	const tok = "v1.SECRETLINK"
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := RequestLogMiddleware(logger, "hub", HubPathPatterns(), 0)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/portview/"+tok+"/app.js?x=1", nil))
	out := buf.String()
	if out == "" {
		t.Fatal("no request log line")
	}
	if strings.Contains(out, "SECRETLINK") {
		t.Fatalf("request log holds the page link: %s", out)
	}
	if !strings.Contains(out, RedactedPortViewPath) {
		t.Fatalf("request log missing the redacted path: %s", out)
	}
}

// TestRedactReferer: a Referer naming a port view or artifact URL is
// logged with its path redacted; one that does not parse is replaced;
// others are kept.
func TestRedactReferer(t *testing.T) {
	const tok = "v1.SECRETLINK"
	cases := map[string]string{
		"":                                     "",
		"https://hub.example/api/v1/agents/a1": "https://hub.example/api/v1/agents/a1",
		"https://hub.example/web/agents?x=1":   "https://hub.example/web/agents?x=1",
		"https://hub.example/api/v1/portview/" + tok + "/index.html?a=1": "https://hub.example" + RedactedPortViewPath + "?a=1",
		"https://hub.example/api/v1/%70ortview/" + tok + "/x":            "https://hub.example" + RedactedPortViewPath,
		"https://hub.example/api/v1/artifacts/view/" + tok + "/i.html":   "https://hub.example" + RedactedArtifactPath,
		"https://hub.example/x?sig=" + tok:                               "https://hub.example/x?sig=REDACTED",
		"https://hub.example/api/v1/portview/" + tok + "/%zz":            "REDACTED",
		"://bad referer " + tok:                                          "REDACTED",
	}
	for in, want := range cases {
		got := RedactReferer(in)
		if got != want {
			t.Errorf("RedactReferer(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "SECRETLINK") {
			t.Errorf("RedactReferer(%q) kept the link: %q", in, got)
		}
	}
}

// TestRequestLogMiddleware_RedactsPortViewReferer: a request to any route
// whose Referer is a page link is logged without the link.
func TestRequestLogMiddleware_RedactsPortViewReferer(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := RequestLogMiddleware(logger, "hub", HubPathPatterns(), 0)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Referer", "https://hub.example/api/v1/portview/v1.SECRETLINK/index.html")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if strings.Contains(out, "SECRETLINK") {
		t.Fatalf("request log holds the page link: %s", out)
	}
	if !strings.Contains(out, `"referer":"https://hub.example`+RedactedPortViewPath+`"`) {
		t.Fatalf("request log missing the redacted referer: %s", out)
	}
}

// TestCredentialLinkInQueryRedacted: a query (or fragment) that names a
// page or artifact link, in any spelling, is replaced whole, the path is
// kept, and the request is not traced.
func TestCredentialLinkInQueryRedacted(t *testing.T) {
	const tok = "v1.SECRETLINK"
	queries := []string{
		"next=/api/v1/portview/" + tok + "/x",
		"next=https://hub.example/api/v1/portview/" + tok + "/x&a=1",
		"next=%2Fapi%2Fv1%2Fportview%2F" + tok,
		"next=%252Fapi%252Fv1%252F%2570ortview%252F" + tok,
		"u=https%3A%2F%2Fhub.example%2Fapi%2Fv1%2Fartifacts%2Fview%2F" + tok,
		"a=1&PORTVIEW=" + tok,
		"x=%zz&next=%2Fapi%2Fv1%2F%70ortview%2F" + tok,
		"next=%2Fapi%2Fv1%2F%70ortview%2F" + tok + "&x=%zz",
		"x=%&next=%2Fapi%2Fv1%2F%61rtifacts%2Fview%2F" + tok,
		"next=%2Fapi%2Fv1%2F%70ortview%2F" + tok + ";x=%zz",
		"next=/api/v1/attachment-deliveries/" + tok,
		"next=%2Fapi%2Fv1%2Fattachment%2Ddeliveries%2F" + tok,
	}
	for _, q := range queries {
		if got := RedactQuery(q); got != "REDACTED" {
			t.Errorf("RedactQuery(%q) = %q", q, got)
		}
		u, err := url.Parse("https://hub.example/api/v1/agents?" + q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got := RedactURL(u); got != "https://hub.example/api/v1/agents?REDACTED" {
			t.Errorf("RedactURL(%s) = %q", q, got)
		}
		if !IsCredentialURL(u) {
			t.Errorf("IsCredentialURL(%s) = false", q)
		}
		r := httptest.NewRequest(http.MethodGet, "/api/v1/agents?"+q, nil)
		if got := RequestPath(r); got != "/api/v1/agents" {
			t.Errorf("RequestPath(%s) = %q", q, got)
		}
		if got := RedactReferer("https://hub.example/web/x?" + q); strings.Contains(got, "SECRETLINK") {
			t.Errorf("RedactReferer(%s) = %q", q, got)
		}
	}
	if got := RedactReferer("https://hub.example/web/x?x=%zz&next=%2Fapi%2Fv1%2F%70ortview%2F" + tok); strings.Contains(got, "SECRETLINK") {
		t.Errorf("RedactReferer with a malformed parameter kept the link: %q", got)
	}
	u, _ := url.Parse("https://hub.example/web/x#/api/v1/portview/" + tok)
	if got := RedactURL(u); strings.Contains(got, "SECRETLINK") {
		t.Errorf("RedactURL with a link in the fragment = %q", got)
	}
	plain, _ := url.Parse("https://hub.example/api/v1/agents?limit=5")
	if IsCredentialURL(plain) || RedactURL(plain) != "https://hub.example/api/v1/agents?limit=5" {
		t.Errorf("a plain query changed: %q", RedactURL(plain))
	}
}

// TestRequestLogMiddleware_RedactsLinkInQuery: a request whose query holds
// a page link is logged without it.
func TestRequestLogMiddleware_RedactsLinkInQuery(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := RequestLogMiddleware(logger, "hub", HubPathPatterns(), 0)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/api/v1/agents?next=%2Fapi%2Fv1%2Fportview%2Fv1.SECRETLINK%2Fx", nil))
	out := buf.String()
	if strings.Contains(out, "SECRETLINK") {
		t.Fatalf("request log holds the page link: %s", out)
	}
	if !strings.Contains(out, `/api/v1/agents?REDACTED`) {
		t.Fatalf("request log missing the redacted URL: %s", out)
	}
}

// TestEncodedBeyondDecodeRoundsRedacted: a link encoded more often than
// the decoder unwraps, or with a sequence that does not decode, still
// counts as one, in a path, a query value and a sub-path.
func TestEncodedBeyondDecodeRoundsRedacted(t *testing.T) {
	const tok = "v1.SECRETLINK"
	nested := "%2525252570ortview" // "portview" encoded five times over its first letter
	r := httptest.NewRequest(http.MethodGet, "/api/v1/x/"+nested+"/"+tok, nil)
	if got := RequestPath(r); strings.Contains(got, tok) {
		t.Errorf("RequestPath(nested) = %q", got)
	}
	if got := RedactQuery("next=/api/v1/" + nested + "/" + tok); got != "REDACTED" {
		t.Errorf("RedactQuery(nested) = %q", got)
	}
	if got := SubPath("/api/v1/" + nested + "/" + tok); got != RedactedPath {
		t.Errorf("SubPath(nested) = %q", got)
	}
}

// TestSubPath: an agent-side path naming a page or artifact link is
// logged redacted; others are kept.
func TestSubPath(t *testing.T) {
	const tok = "v1.SECRETLINK"
	for _, p := range []string{
		"/api/v1/portview/" + tok + "/app.js",
		"/x/%70ortview/" + tok,
		"/api/v1/artifacts/view/" + tok + "/i.html",
		"/x/%zz/" + tok,
		"/api/v1/attachment-deliveries/" + tok,
		"/x/attachment%2Ddeliveries/" + tok,
	} {
		if got := SubPath(p); got != RedactedPath {
			t.Errorf("SubPath(%q) = %q", p, got)
		}
	}
	for _, p := range []string{"/", "/index.html", "/api/ping", "/img/dot.png"} {
		if got := SubPath(p); got != p {
			t.Errorf("SubPath(%q) = %q", p, got)
		}
	}
}

// TestStillEncodedPathRedactedNeutral: a path that keeps an undecodable
// sequence is logged as RedactedPath, not as an artifact or port view
// path, and is not traced.
//
// A NUL byte that appears at some decoding step does not end the check:
// the path is still judged by what is left when decoding stops.
func TestStillEncodedPathRedactedNeutral(t *testing.T) {
	for _, p := range []string{
		"/api/v1/agents/a1/x%2525zz",
		"/api/v1/agents/a1/x%00%2525zz",
		"/api/v1/agents/a1/x%2500%252525zz",
	} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		if got := RequestPath(r); got != RedactedPath {
			t.Errorf("RequestPath(%q) = %q, want %q", p, got, RedactedPath)
		}
		if got := RedactURL(r.URL); got != RedactedPath {
			t.Errorf("RedactURL(%q) = %q, want %q", p, got, RedactedPath)
		}
		if !IsCredentialURL(r.URL) {
			t.Errorf("IsCredentialURL(%q) = false", p)
		}
	}
	if !stillEncoded("x\x00%zz") {
		t.Error(`stillEncoded("x\x00%zz") = false`)
	}
	if stillEncoded("x\x00y") {
		t.Error(`stillEncoded("x\x00y") = true`)
	}
}

// TestDeliveryPathRedacted: the attachment delivery route's path carries a
// delivery capability (ptone/scion#4596). No spelling of it reaches a log
// line, a redacted URL or a trace whole, and the request log records the
// redacted path.
func TestDeliveryPathRedacted(t *testing.T) {
	for _, p := range []string{
		"/api/v1/attachment-deliveries/TOKENSECRET",
		"/api/v1//attachment-deliveries/TOKENSECRET",
		"/api/v1/attachment%2Ddeliveries/TOKENSECRET",
		"/api/v1/ATTACHMENT-DELIVERIES/TOKENSECRET",
		"/api/v1/attachment%252Ddeliveries/TOKENSECRET",
		"/api/v1/attachment-deliveries%2FTOKENSECRET",
		"/api/v1/attachment-deliveries/TOKENSECRET/",
	} {
		u := &url.URL{Path: p}
		if raw, err := url.Parse("https://hub.example.com" + p); err == nil {
			u = raw
		}
		r := &http.Request{URL: u}
		if got := RequestPath(r); got != RedactedDeliveryPath {
			t.Errorf("RequestPath(%q) = %q, want %q", p, got, RedactedDeliveryPath)
		}
		if got := RedactURL(u); strings.Contains(got, "TOKENSECRET") {
			t.Errorf("RedactURL(%q) = %q, keeps the capability", p, got)
		}
		if !IsCredentialURL(u) {
			t.Errorf("IsCredentialURL(%q) = false", p)
		}
	}
	for _, p := range []string{"/api/v1/agents/x/message", "/api/v1/chat/attachments", "/api/v1/chat/attachments/abc"} {
		if got := RequestPath(&http.Request{URL: &url.URL{Path: p}}); got != p {
			t.Errorf("an ordinary path is redacted: %q -> %q", p, got)
		}
	}
	// The host and a non-credential query are kept.
	u, err := url.Parse("https://hub.example.com/api/v1/attachment-deliveries/TOKENSECRET/?trace=1&sig=SIGSECRET")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := RedactURL(u), "https://hub.example.com"+RedactedDeliveryPath+"?trace=1&sig=REDACTED"; got != want {
		t.Errorf("RedactURL = %q, want %q", got, want)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := RequestLogMiddleware(logger, "hub", HubPathPatterns(), 0)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/attachment-deliveries/TOKENSECRET", nil))
	if out := buf.String(); strings.Contains(out, "TOKENSECRET") {
		t.Fatalf("the request log records the capability: %s", out)
	}
}
