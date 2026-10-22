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

package config

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactURLsIn(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"text 1", "nothing to see", "nothing to see"},
		{"text 2", "Get https://hub.example.com/api: refused", "Get https://hub.example.com/api: refused"},
		{"text 3", "see https://hub.example.com/u/@me and a@b", "see https://hub.example.com/u/@me and a@b"},
		{"text 4", "dial https://alice:s3cret@hub.example.com/x", "dial https://alice:xxxxx@hub.example.com/x"},
		{"text 5", `Get "https://tok3n@hub.example.com/api/v1/agents": refused`, `Get "https://xxxxx@hub.example.com/api/v1/agents": refused`},
		{"text 6", `Get "https://alice:xxxxx@hub.example.com/healthz"`, `Get "https://alice:xxxxx@hub.example.com/healthz"`},
		{"text 7", "at HTTP://tok3n@hub.example.com", "at http://xxxxx@hub.example.com"},
		{"text 8", "hub at https://tok3n@hub.example.com:port is down", "hub at https://xxxxx@hub.example.com:port is down"},
		{"text 9", "https://a@s3cret@hub.example.com:port/x", "https://xxxxx@hub.example.com:port/x"},
		{
			"text 10",
			"https://alice:s3cret@one.example.com and http://tok3n@two.example.com?q=1, then https://three.example.com",
			"https://alice:xxxxx@one.example.com and http://xxxxx@two.example.com?q=1, then https://three.example.com",
		},
		{"text 11", `invalid hub endpoint "https://tok3n@hub.example.com": no`, `invalid hub endpoint "https://xxxxx@hub.example.com": no`},
		{"text 12", "https://tok3n@hub.example.com#frag", "https://xxxxx@hub.example.com#frag"},
		{"text 13", "dial https://alice:s3@cret@hub.example.com/x", "dial https://alice:xxxxx@hub.example.com/x"},
		{"text 14", "dial https://alice:s3@cret@hub.example.com:port/x", "dial https://xxxxx@hub.example.com:port/x"},
		{
			"text 15",
			`parse "https://alice:s3\"cret@hub.example.com": net/url: invalid userinfo`,
			`parse "https://xxxxx@hub.example.com": net/url: invalid userinfo`,
		},
		{"text 16", `"https://tok3n\u00e9@hub.example.com"`, `"https://xxxxx@hub.example.com"`},
		{"text 17", "hub at https://t'ok3n@hub.example.com is down", "hub at https://xxxxx@hub.example.com is down"},
		{"text 18", `Get \"https://tok3n@hub.example.com\": refused`, `Get \"https://xxxxx@hub.example.com\": refused`},
	}
	for _, tc := range tests {
		got := RedactURLsIn(tc.in)
		if got != tc.want {
			t.Errorf("%s: RedactURLsIn(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		for _, secret := range []string{"s3cret", "s3@cret", "cret@", "tok3n"} {
			if strings.Contains(got, secret) {
				t.Errorf("%s: RedactURLsIn(%q) = %q, still shows %q", tc.name, tc.in, got, secret)
			}
		}
	}
}

// The whole user info is replaced.
func TestRedactURL_WholeUserInfo(t *testing.T) {
	got := RedactURL("https://a@s3cret@hub.example.com:port/x")
	if want := "https://xxxxx@hub.example.com:port/x"; got != want {
		t.Errorf("RedactURL = %q, want %q", got, want)
	}
}

// A URL with a user name and no password.
func TestRedactURLsIn_MasksUserName(t *testing.T) {
	got := RedactURLsIn(`Get "https://tok3n@hub.example.com/api": refused`)
	if want := `Get "https://xxxxx@hub.example.com/api": refused`; got != want {
		t.Errorf("RedactURLsIn = %q, want %q", got, want)
	}
}

// A hub URL in an error text is shown without its user info.
func TestRedactURLsIn_ErrorText(t *testing.T) {
	got := RedactURLsIn(`invalid hub URL "https://alice:s3\"cret@hub.example.com"`)
	if want := `invalid hub URL "https://xxxxx@hub.example.com"`; got != want {
		t.Errorf("RedactURLsIn = %q, want %q", got, want)
	}
}

// User info of any valid form is shown masked.
func TestRedactURLsIn_UserInfoCharacters(t *testing.T) {
	const password = "s3!$&'()*+,;=%41cret"
	const token = "t0k!$&'()*+,;=%41en"
	for _, tc := range []struct{ in, secret string }{
		{"hub at https://u:" + password + "@hub.example.com is not responding", password},
		{"hub at https://al'ice!$&()*+,;=:s3cret@hub.example.com is not responding", "s3cret"},
		{"hub at https://" + token + "@hub.example.com is not responding", token},
		{`Get "https://u:` + password + `@hub.example.com/api": refused`, password},
	} {
		got := RedactURLsIn(tc.in)
		if !strings.Contains(got, "xxxxx@hub.example.com") {
			t.Errorf("RedactURLsIn(%q) = %q, want the user info masked", tc.in, got)
		}
		for i := 0; i+4 <= len(tc.secret); i++ {
			if strings.Contains(got, tc.secret[i:i+4]) {
				t.Errorf("RedactURLsIn(%q) = %q, shows %q", tc.in, got, tc.secret[i:i+4])
			}
		}
	}
}

// urlCredentialCases are the query and fragment cases of RedactURL
// (ptone/scion#5356). pkg/apiclient's redact_test.go has the same table
// for its copy of the rules.
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

func TestRedactURL_QueryCredentials(t *testing.T) {
	for _, tc := range urlCredentialCases {
		got := RedactURL(tc.in)
		if got != tc.want {
			t.Errorf("%s: RedactURL(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		for _, secret := range []string{"tok3n", "s3cret", "s1gnat"} {
			if strings.Contains(got, secret) {
				t.Errorf("%s: RedactURL(%q) = %q, still shows %q", tc.name, tc.in, got, secret)
			}
		}
	}
}

// In a text, the query of each URL is redacted too, and the punctuation
// after a URL is kept.
func TestRedactURLsIn_QueryCredentials(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"error text", `Get "https://hub.example.com/api?token=tok3n": refused`, `Get "https://hub.example.com/api?token=xxxxx": refused`},
		{"sentence end", "see https://hub.example.com/x?key=s3cret.", "see https://hub.example.com/x?key=xxxxx."},
		{"comma and paren", "(https://hub.example.com/x?sig=s1gnat), then", "(https://hub.example.com/x?sig=xxxxx), then"},
		{"two URLs", "https://a.example.com/?token=tok3n and http://u:s3cret@b.example.com/p?q=1", "https://a.example.com/?token=xxxxx and http://u:xxxxx@b.example.com/p?q=1"},
		{"no query untouched", "dial https://hub.example.com/path/to/x: refused", "dial https://hub.example.com/path/to/x: refused"},
		{"fragment", "redirected to https://hub.example.com/cb#access_token=tok3n", "redirected to https://hub.example.com/cb#access_token=xxxxx"},
		{"json text", `{"url":"https://hub.example.com/cb?state=s\u0026access_token=tok3n","n":1}`, `{"url":"https://hub.example.com/cb?state=s\u0026access_token=xxxxx","n":1}`},
		{"json escaped slash", `{"url":"https://hub.example.com\/cb\/x?token=tok3n"}`, `{"url":"https://hub.example.com\/cb\/x?token=xxxxx"}`},
		{"go quoted ends at backslash", `Get \"https://hub.example.com/x?token=tok3n\": refused`, `Get \"https://hub.example.com/x?token=xxxxx\": refused`},
	}
	for _, tc := range tests {
		got := RedactURLsIn(tc.in)
		if got != tc.want {
			t.Errorf("%s: RedactURLsIn(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	for _, tc := range urlCredentialCases {
		if tc.name == "unparsable URL" {
			continue // a space ends a URL in a text
		}
		if got := RedactURLsIn("at " + tc.in + " now"); got != "at "+tc.want+" now" {
			t.Errorf("%s: RedactURLsIn = %q, want %q", tc.name, got, "at "+tc.want+" now")
		}
	}
}

// pkg/apiclient keeps a copy of the query and fragment rules (it imports
// no scion package). The two copies must stay identical.
func TestURLRedactRules_ApiclientCopyIdentical(t *testing.T) {
	const marker = "// urlCredentialParamNames are query"
	cfg, err := os.ReadFile("url_redact.go")
	if err != nil {
		t.Fatal(err)
	}
	api, err := os.ReadFile(filepath.Join("..", "apiclient", "redact.go"))
	if err != nil {
		t.Fatal(err)
	}
	ci := strings.Index(string(cfg), marker)
	ai := strings.Index(string(api), marker)
	if ci < 0 || ai < 0 {
		t.Fatalf("marker %q not found (config %d, apiclient %d)", marker, ci, ai)
	}
	want := strings.TrimRight(string(cfg[ci:]), "\n")
	got := string(api[ai:])
	end := strings.Index(got, "// redactURLError returns")
	if end < 0 {
		t.Fatal("end marker not found in apiclient/redact.go")
	}
	got = strings.TrimRight(got[:end], "\n")
	if got != want {
		t.Errorf("pkg/apiclient/redact.go's copy of the URL redaction rules differs from pkg/config/url_redact.go")
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
func TestRedactURL_PropertyNoPanicNoLeak(t *testing.T) {
	r := rand.New(rand.NewSource(5356))
	for i := 0; i < 5000; i++ {
		in := "https://hub.example.com/" + redactPropertyJunk(r) + "?a=1&token=S3CRETVALUE&" + redactPropertyJunk(r)
		got := RedactURL(in)
		if strings.Contains(got, "S3CRETVALUE") {
			t.Fatalf("RedactURL(%q) = %q, shows the token", in, got)
		}
	}
}

// The same property for RedactURLsIn, with the URL inside a sentence (the
// junk has no white space or quote, which end a URL in a text).
func TestRedactURLsIn_PropertyNoPanicNoLeak(t *testing.T) {
	r := rand.New(rand.NewSource(53560))
	for i := 0; i < 5000; i++ {
		in := "see https://hub.example.com/" + redactPropertyJunk(r) + "?a=1&token=S3CRETVALUE&" + redactPropertyJunk(r) + " now"
		got := RedactURLsIn(in)
		if strings.Contains(got, "S3CRETVALUE") {
			t.Fatalf("RedactURLsIn(%q) = %q, shows the token", in, got)
		}
	}
}

// redactPropertySafe is junk for a nested URL's path: no delimiter, so the
// nested query starts where the test puts it.
var redactPropertySafe = []string{"a", "Z", "/", ".", "-", "%", "%zz", "\xff", "\xc3", "\u023a", "\u212a", "\u00e9"}

// A credential parameter in the query of a URL inside a parameter value
// (one level deep) is masked, for any junk path, and nothing panics
// (ptone/scion#5356 security review).
func TestRedactURL_PropertyNestedURL(t *testing.T) {
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
		got := RedactURL(in)
		if strings.Contains(got, "S3CRETVALUE") {
			t.Fatalf("RedactURL(%q) = %q, shows the token", in, got)
		}
		if !strings.Contains(got, "next=https://app.example.com/") {
			t.Fatalf("RedactURL(%q) = %q, lost the nested destination", in, got)
		}
	}
}

// A credential path parameter before a URL embedded in the path is masked,
// whatever follows, through RedactURL (ptone/scion#5356 review).
func TestRedactURL_PropertyEmbeddedURLInPath(t *testing.T) {
	r := rand.New(rand.NewSource(53562))
	for i := 0; i < 5000; i++ {
		in := "https://hub.example.com/" + redactPropertySafeJunk(r) + ";jsessionid=S3CRETVALUE/r/https://x.example.com/" + redactPropertyJunk(r)
		if got := RedactURL(in); strings.Contains(got, "S3CRETVALUE") {
			t.Fatalf("RedactURL(%q) = %q, shows the token", in, got)
		}
		if got := RedactURLsIn("see " + in + " now"); strings.Contains(got, "S3CRETVALUE") {
			t.Fatalf("RedactURLsIn(%q) = %q, shows the token", in, got)
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
