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

package apiclient

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// closedServerURL returns the URL of a server that is already closed, with
// the user info userinfo.
func closedServerURL(t *testing.T, userinfo string) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return strings.Replace(srv.URL, "http://", "http://"+userinfo+"@", 1)
}

func assertNoURLSecrets(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", what)
	}
	for _, s := range []string{"s3cret", "tok3n"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("%s: error %q shows %q", what, err.Error(), s)
		}
	}
	if !strings.Contains(err.Error(), "xxxxx@") {
		t.Errorf("%s: error %q does not show the masked user info", what, err.Error())
	}
}

// Transport errors quote the request URL; the hub client shows it without
// user info, whatever the request path.
func TestTransportErrors_RedactURLUserInfo(t *testing.T) {
	ctx := context.Background()
	for _, userinfo := range []string{"tok3n", "alice:s3cret"} {
		tr := NewTransport(closedServerURL(t, userinfo))
		_, err := tr.Get(ctx, "/healthz", nil)
		assertNoURLSecrets(t, "Get "+userinfo, err)
		_, err = tr.Post(ctx, "/api/v1/x", map[string]string{"a": "b"}, nil)
		assertNoURLSecrets(t, "Post "+userinfo, err)
		_, err = tr.PostNoRetry(ctx, "/api/v1/x", nil, nil)
		assertNoURLSecrets(t, "PostNoRetry "+userinfo, err)
		_, err = tr.Delete(ctx, "/api/v1/x", nil)
		assertNoURLSecrets(t, "Delete "+userinfo, err)
	}
}

// Errors from building a request show the base URL without user info.
func TestTransportRequestErrors_RedactURLUserInfo(t *testing.T) {
	ctx := context.Background()
	tr := NewTransport("http://tok3n@hub example.com")
	_, err := tr.Get(ctx, "/healthz", nil)
	assertNoURLSecrets(t, "Get", err)
	_, err = tr.Delete(ctx, "/x", nil)
	assertNoURLSecrets(t, "Delete", err)
	_, err = tr.Put(ctx, "/x", nil, nil)
	assertNoURLSecrets(t, "Put", err)
}

func TestRedactURLError(t *testing.T) {
	inner := &url.Error{Op: "Get", URL: "https://tok3n@hub.example.com/x", Err: errors.New("refused")}
	// As the transport does: redact the *url.Error, then wrap it.
	err := fmt.Errorf("wrapped: %w", redactURLError(inner))
	if want := `wrapped: Get "https://xxxxx@hub.example.com/x": refused`; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	plain := errors.New("no URL error")
	if got := redactURLError(plain); got != plain {
		t.Errorf("an error without a url.Error changed: %v", got)
	}
	for in, want := range map[string]string{
		"https://hub.example.com/x":              "https://hub.example.com/x",
		"https://alice:s3cret@hub.example.com/x": "https://alice:xxxxx@hub.example.com/x",
		"https://tok3n@hub.example.com":          "https://xxxxx@hub.example.com",
		"https://a@s3cret@hub example.com/%zz":   "https://xxxxx@hub example.com/%zz",
	} {
		if got := redactURLCredentials(in); got != want {
			t.Errorf("redactURLCredentials(%q) = %q, want %q", in, got, want)
		}
	}
}

// A URL with a user name and no password.
func TestTransportGet_RedactsUserName(t *testing.T) {
	_, err := NewTransport(closedServerURL(t, "tok3n")).Get(context.Background(), "/healthz", nil)
	if err == nil || strings.Contains(err.Error(), "tok3n") || !strings.Contains(err.Error(), `"http://xxxxx@127.0.0.1:`) {
		t.Errorf("error = %v, want the URL without its user name", err)
	}
}

// Errors from building a request show the base URL without user info.
func TestTransportRequestErrors_BaseURL(t *testing.T) {
	const password = "s3cr@tp4ss"
	_, err := NewTransport("http://user:"+password+"@hub.example.com:99999x").Get(context.Background(), "/healthz", nil)
	if err == nil || !strings.Contains(err.Error(), "http://xxxxx@hub.example.com:99999x") {
		t.Fatalf("error = %v, want the URL without its user info", err)
	}
	for i := 0; i+4 <= len(password); i++ {
		if strings.Contains(err.Error(), password[i:i+4]) {
			t.Errorf("error %q shows %q", err.Error(), password[i:i+4])
		}
	}
}

// urlCredentialCases is the table of config's redact_urls_test.go: this
// package keeps a copy of config.RedactURL's rules (ptone/scion#5356).
var urlCredentialCases = []struct{ name, in, want string }{
	{"no query", "https://hub.example.com/api", "https://hub.example.com/api"},
	{"plain params kept", "https://hub.example.com/api?limit=5&sort=name", "https://hub.example.com/api?limit=5&sort=name"},
	{"token", "https://hub.example.com/x?token=tok3n", "https://hub.example.com/x?token=xxxxx"},
	{"access_token among others", "https://hub.example.com/x?a=1&access_token=tok3n&b=2", "https://hub.example.com/x?a=1&access_token=xxxxx&b=2"},
	{"key", "https://hub.example.com/x?key=s3cret", "https://hub.example.com/x?key=xxxxx"},
	{"api_key", "https://hub.example.com/x?api_key=s3cret", "https://hub.example.com/x?api_key=xxxxx"},
	{"secret", "https://hub.example.com/x?client_secret=s3cret", "https://hub.example.com/x?client_secret=xxxxx"},
	{"signature", "https://hub.example.com/x?signature=s1gnat", "https://hub.example.com/x?signature=xxxxx"},
	{"sig", "https://hub.example.com/x?sig=s1gnat", "https://hub.example.com/x?sig=xxxxx"},
	{"signed URL", "https://storage.example.com/b/o?X-Goog-Algorithm=A&X-Goog-Credential=s3cret&X-Goog-Signature=s1gnat", "https://storage.example.com/b/o?X-Goog-Algorithm=A&X-Goog-Credential=xxxxx&X-Goog-Signature=xxxxx"},
	{"amz security token", "https://s3.example.com/o?X-Amz-Security-Token=tok3n&X-Amz-Date=1", "https://s3.example.com/o?X-Amz-Security-Token=xxxxx&X-Amz-Date=1"},
	{"case-insensitive name", "https://hub.example.com/x?TOKEN=tok3n", "https://hub.example.com/x?TOKEN=xxxxx"},
	{"escaped name", "https://hub.example.com/x?acc%65ss_token=tok3n", "https://hub.example.com/x?acc%65ss_token=xxxxx"},
	{"repeated param", "https://hub.example.com/x?token=tok3n&token=s3cret", "https://hub.example.com/x?token=xxxxx&token=xxxxx"},
	{"empty value", "https://hub.example.com/x?token=", "https://hub.example.com/x?token=xxxxx"},
	{"name without value", "https://hub.example.com/x?token", "https://hub.example.com/x?token"},
	{"fragment", "https://hub.example.com/cb#access_token=tok3n&state=s", "https://hub.example.com/cb#access_token=xxxxx&state=s"},
	{"plain fragment", "https://hub.example.com/docs#section-2", "https://hub.example.com/docs#section-2"},
	{"user info and query", "https://alice:s3cret@hub.example.com/x?token=tok3n", "https://alice:xxxxx@hub.example.com/x?token=xxxxx"},
	{"unparsable URL", "https://alice:s3cret@hub example.com/%zz?token=tok3n", "https://xxxxx@hub example.com/%zz?token=xxxxx"},
	{"semicolon separator", "https://hub.example.com/x?a=1;token=tok3n;b=2", "https://hub.example.com/x?a=1;token=xxxxx;b=2"},
	{"json-escaped ampersand", `https://hub.example.com/x?a=1\u0026token=tok3n\u0026b=2`, `https://hub.example.com/x?a=1\u0026token=xxxxx\u0026b=2`},
	{"json-escaped equals", `https://hub.example.com/x?token\u003dtok3n`, `https://hub.example.com/x?token\u003dxxxxx`},
	{"json-escaped uppercase hex", `https://hub.example.com/x?a=1&TOKEN\u003Dtok3n`, `https://hub.example.com/x?a=1&TOKEN\u003Dxxxxx`},
	{"hash route query", "https://hub.example.com/#/callback?code=s3cret&state=s", "https://hub.example.com/#/callback?code=xxxxx&state=s"},
	{"hash route without query", "https://hub.example.com/#/settings/profile", "https://hub.example.com/#/settings/profile"},
	{"pwd and pass", "https://hub.example.com/x?user=u&pwd=s3cret&pass=s3cret", "https://hub.example.com/x?user=u&pwd=xxxxx&pass=xxxxx"},
	{"jwt", "https://hub.example.com/x?jwt=tok3n&id_jwt=tok3n", "https://hub.example.com/x?jwt=xxxxx&id_jwt=xxxxx"},
	{"invalid UTF-8 before ?", "https://h/\xff\xff\xff?a=1&token=tok3n", "https://h/\xff\xff\xff?a=1&token=xxxxx"},
	{"invalid UTF-8 before &", "https://h/x?a=\xff&token=tok3n&b=2", "https://h/x?a=\xff&token=xxxxx&b=2"},
	{"invalid UTF-8 before =", "https://h/x?\xc3=1&token=tok3n", "https://h/x?\xc3=1&token=xxxxx"},
	{"U+023A before ?", "https://h/\u023a\u023a?token=tok3n", "https://h/\u023a\u023a?token=xxxxx"},
	{"U+023A before =", "https://h/x?\u023a=1&token=tok3n", "https://h/x?\u023a=1&token=xxxxx"},
	{"U+212A before &", "https://h/x?a=\u212a&token=tok3n", "https://h/x?a=\u212a&token=xxxxx"},
	{"U+212A in the value", "https://h/x?token=\u212atok3n&b=2", "https://h/x?token=xxxxx&b=2"},
	{"non-ASCII path", "https://h/caf\u00e9/\u65e5?token=tok3n", "https://h/caf\u00e9/\u65e5?token=xxxxx"},
	{"nested URL token", "https://hub.example.com/login?next=https://app.example.com/cb?token=tok3n", "https://hub.example.com/login?next=https://app.example.com/cb?token=xxxxx"},
	{"nested URL destination readable", "https://hub.example.com/login?next=https://app.example.com/cb/x?state=s&code=s3cret&n=1", "https://hub.example.com/login?next=https://app.example.com/cb/x?state=s&code=xxxxx&n=1"},
	{"nested URL second level left alone", "https://hub.example.com/a?next=https://b.example.com/?u=https://c.example.com/?code=deepvalue", "https://hub.example.com/a?next=https://b.example.com/?u=https://c.example.com/?code=deepvalue"},
	{"nested URL without query", "https://hub.example.com/login?next=https://app.example.com/cb", "https://hub.example.com/login?next=https://app.example.com/cb"},
	{"credential name with a URL value", "https://hub.example.com/x?token=https://app.example.com/?a=1", "https://hub.example.com/x?token=xxxxx"},
	{"nested URL in a fragment", "https://hub.example.com/#next=https://app.example.com/cb?access_token=tok3n", "https://hub.example.com/#next=https://app.example.com/cb?access_token=xxxxx"},
	{"jsessionid path parameter", "https://hub.example.com/app;jsessionid=s3cret/page?x=1", "https://hub.example.com/app;jsessionid=xxxxx/page?x=1"},
	{"jsessionid last path parameter", "https://hub.example.com/app;JSESSIONID=s3cret", "https://hub.example.com/app;JSESSIONID=xxxxx"},
	{"other path parameter kept", "https://hub.example.com/a;v=2/b", "https://hub.example.com/a;v=2/b"},
	{"phpsessid query", "https://hub.example.com/x?PHPSESSID=s3cret&a=1", "https://hub.example.com/x?PHPSESSID=xxxxx&a=1"},
	{"pass is an exact name only", "https://hub.example.com/x?bypass=1&compass=n&passthrough=yes&pass=s3cret", "https://hub.example.com/x?bypass=1&compass=n&passthrough=yes&pass=xxxxx"},
	{"session and sid are not masked", "https://hub.example.com/x?session=abc&sid=def", "https://hub.example.com/x?session=abc&sid=def"},
	{"path param before an embedded URL", "https://hub.example.com/app;jsessionid=s3cret/r/https://x.example.com/y", "https://hub.example.com/app;jsessionid=xxxxx/r/https://x.example.com/y"},
	{"path params before and inside an embedded URL", "https://hub.example.com/app;jsessionid=s3cret/https://y.example.com/p;jsessionid=s3cret", "https://hub.example.com/app;jsessionid=xxxxx/https://y.example.com/p;jsessionid=xxxxx"},
	{"several path params in one segment", "https://hub.example.com/a;v=1;jsessionid=s3cret/b?x=1", "https://hub.example.com/a;v=1;jsessionid=xxxxx/b?x=1"},
	{"path param after an embedded URL", "https://hub.example.com/r/https://x.example.com/p;token=s3cret", "https://hub.example.com/r/https://x.example.com/p;token=xxxxx"},
	{"trailing embedded URL", "https://hub.example.com/app;jsessionid=s3cret/https://y.example.com/", "https://hub.example.com/app;jsessionid=xxxxx/https://y.example.com/"},
	{"no path, semicolon in authority", "https://hub.example.com;v=1", "https://hub.example.com;v=1"},
	{"question mark in a fragment value", "https://hub.example.com/cb#access_token=tok3n&state=a?b", "https://hub.example.com/cb#access_token=xxxxx&state=a?b"},
}

func TestRedactURLCredentials_Query(t *testing.T) {
	for _, tc := range urlCredentialCases {
		got := redactURLCredentials(tc.in)
		if got != tc.want {
			t.Errorf("%s: redactURLCredentials(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// A transport error for a request whose path carries a credential query
// parameter shows the URL without its value.
func TestTransportErrors_RedactQueryCredentials(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	_, err := NewTransport(srv.URL).Get(context.Background(), "/api/v1/x?token=tok3n&limit=5", nil)
	if err == nil || strings.Contains(err.Error(), "tok3n") || !strings.Contains(err.Error(), "token=xxxxx&limit=5") {
		t.Errorf("error = %v, want the query token masked", err)
	}
}

// redactPropertyAlphabet is the junk the property tests build URLs from:
// delimiters, escapes, non-ASCII letters whose lower case has another byte
// length (U+023A, U+212A), and invalid UTF-8.
var redactPropertyAlphabet = []string{
	"a", "Z", "/", "?", "#", "&", ";", "=", "%", "%zz", "@", ":", ".",
	`\u0026`, `\u003D`, `\u003f`, `\u0023`,
	"\xff", "\xc3", "\u023a", "\u212a", "\u00e9",
}

func redactPropertyJunk(r *rand.Rand) string {
	var b strings.Builder
	for n := r.Intn(12); n > 0; n-- {
		b.WriteString(redactPropertyAlphabet[r.Intn(len(redactPropertyAlphabet))])
	}
	return b.String()
}

// For any junk around it, a credential parameter between "&"s is masked,
// and nothing panics (ptone/scion#5356 review).
func TestRedactURLCredentials_PropertyNoPanicNoLeak(t *testing.T) {
	r := rand.New(rand.NewSource(5356))
	for i := 0; i < 5000; i++ {
		in := "https://hub.example.com/" + redactPropertyJunk(r) + "?a=1&token=S3CRETVALUE&" + redactPropertyJunk(r)
		got := redactURLCredentials(in)
		if strings.Contains(got, "S3CRETVALUE") {
			t.Fatalf("redactURLCredentials(%q) = %q, shows the token", in, got)
		}
	}
}

// redactPropertySafe is junk for a nested URL's path: no delimiter, so the
// nested query starts where the test puts it.
var redactPropertySafe = []string{"a", "Z", "/", ".", "-", "%", "%zz", "\xff", "\xc3", "\u023a", "\u212a", "\u00e9"}

// A credential parameter in the query of a URL inside a parameter value
// (one level deep) is masked, for any junk path, and nothing panics
// (ptone/scion#5356 security review).
func TestRedactURLCredentials_PropertyNestedURL(t *testing.T) {
	r := rand.New(rand.NewSource(53561))
	junk := func() string {
		var b strings.Builder
		for n := r.Intn(10); n > 0; n-- {
			b.WriteString(redactPropertySafe[r.Intn(len(redactPropertySafe))])
		}
		return b.String()
	}
	for i := 0; i < 5000; i++ {
		// The outer path is delimiter-free too: a "?" and "=" in it would
		// make next=... itself a nested value, and its query the second
		// nesting level, which is left alone by design.
		in := "https://hub.example.com/" + junk() + "?next=https://app.example.com/" + junk() + "?token=S3CRETVALUE&" + redactPropertyJunk(r)
		got := redactURLCredentials(in)
		if strings.Contains(got, "S3CRETVALUE") {
			t.Fatalf("redactURLCredentials(%q) = %q, shows the token", in, got)
		}
		if !strings.Contains(got, "next=https://app.example.com/") {
			t.Fatalf("redactURLCredentials(%q) = %q, lost the nested destination", in, got)
		}
	}
}

// A credential path parameter before a URL embedded in the path is masked,
// whatever follows, through RedactURLCredentials (ptone/scion#5356 review).
func TestRedactURLCredentials_PropertyEmbeddedURLInPath(t *testing.T) {
	r := rand.New(rand.NewSource(53562))
	for i := 0; i < 5000; i++ {
		in := "https://hub.example.com/" + redactPropertySafeJunk(r) + ";jsessionid=S3CRETVALUE/r/https://x.example.com/" + redactPropertyJunk(r)
		if got := redactURLCredentials(in); strings.Contains(got, "S3CRETVALUE") {
			t.Fatalf("redactURLCredentials(%q) = %q, shows the token", in, got)
		}

	}
}

func redactPropertySafeJunk(r *rand.Rand) string {
	var b strings.Builder
	for n := r.Intn(10); n > 0; n-- {
		b.WriteString(redactPropertySafe[r.Intn(len(redactPropertySafe))])
	}
	return b.String()
}
