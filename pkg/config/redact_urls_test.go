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
