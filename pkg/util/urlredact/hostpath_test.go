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

package urlredact

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// Every value is made up.

func TestHostAndPath(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string // "" when rejected
	}{
		{"https://hub.example.com/api", "https://hub.example.com/api"},
		{"https://alice:s3cretPw@hub.example.com/a?b=c#f", "https://hub.example.com/a"},
		{"https://tok3nVal9@github.com/acme/repo.git", "https://github.com/acme/repo.git"},
		{"https://x-access-token:s3cretPw@github.com/acme/repo", "https://github.com/acme/repo"},
		{"https://alice:fa@ke9q@hub.example.com/api", "https://hub.example.com/api"},
		{"https://hub.example.com/a?", "https://hub.example.com/a"},
		{"https://hub.example.com/users/a%40b.example", "https://hub.example.com/users/a%40b.example"},
		{"//alice:s3cretPw@hub.example.com/x", "//hub.example.com/x"},
		{"/srv/git/repo", "/srv/git/repo"},
		// An '@' the rule cannot place: rejected.
		{"https://alice:fake/Pw9q@hub.example.com/api", ""},
		{"https://alice:p@ss/w0rd@hub.example.com/api", ""},
		{"https://alice:p@ss?w0rd@hub.example.com/api", ""},
		{"https://u:1234/x@h.example", ""},
		{"https://tok9/x@h.example", ""},
		{"https://u:12?34@h.example/r", ""},
		{"https://tok9#x@h.example/r", ""},
		{"https://hub.example.com/users/a@b.example.com", ""},
		{"https://hub.example.com/api?mail=a@b.example.com", ""},
		{"https://hub.example.com/login?next=https://alice:s3cretPw@app.example.com/x", ""},
		{"git@github.com:acme/repo.git", ""},
		{"/srv/a@b/repo", ""},
		// Does not parse.
		{"https://alice:pa ss9q@hub.example.com/api", ""},
		{"https://hub.example.com:99x/api", ""},
	} {
		u, ok := HostAndPath(tc.in)
		got := ""
		if ok {
			got = u.String()
		}
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("HostAndPath(%q) = %q, %v; want %q", tc.in, got, ok, tc.want)
		}
	}
}

// RedactURLIn on a URL HostAndPath accepts: every rendering of it is
// shown as given, and its user info, query and fragment are removed from a
// URL built from it.
func TestRedactURLIn_Delimited(t *testing.T) {
	const raw = "https://al'ice:s3cretPw@hub.example.com/a?tok=q7Val#frag9"
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"as given":       "Get " + raw + ": refused",
		"quoted":         "Get " + strconv.Quote(raw) + ": refused",
		"url.String":     "Get " + u.String() + ": refused",
		"url.Redacted":   "Get " + u.Redacted() + ": refused",
		"net/http":       "Get " + netHTTPForm(u) + ": refused",
		"built from raw": "dial wss://al'ice:s3cretPw@hub.example.com/a/conduit?tok=q7Val#frag9: refused",
		"user info":      "auth al%27ice:s3cretPw@ failed",
	} {
		got := RedactURLIn(text, raw, "[SHOWN]", "[M]")
		for _, secret := range []string{"s3cretPw", "q7Val", "frag9"} {
			if f := urlredacttest.SecretFragment(got, secret); f != "" {
				t.Errorf("%s: RedactURLIn(%q) = %q shows %q", name, text, got, f)
			}
		}
	}
	if got, want := RedactURLIn(`Get "`+raw+`": refused`, raw, "[SHOWN]", "[M]"), `Get "[SHOWN]": refused`; got != want {
		t.Errorf("as given: got %q, want %q", got, want)
	}
	if got, want := RedactURLIn("dial wss://al'ice:s3cretPw@hub.example.com/a/conduit?tok=q7Val: x", raw, "[SHOWN]", "[M]"), "dial wss://hub.example.com/a/conduit: x"; got != want {
		t.Errorf("built from raw: got %q, want %q", got, want)
	}
}

// RedactURLIn on a URL HostAndPath rejects: the renderings are replaced by
// shown, and a URL built from it, or printed with part of its user info
// removed, shows no part of the credential either.
func TestRedactURLIn_Rejected(t *testing.T) {
	for _, tc := range []struct{ raw, secret string }{
		{"https://alice:fake/Pw9q@hub.example.com/api", "fake/Pw9q"},
		{"https://alice:fake?Pw9q@hub.example.com/api", "fake?Pw9q"},
		{"https://alice:fake#Pw9q@hub.example.com/api", "fake#Pw9q"},
		{"https://alice:p@ss/w0rd@hub.example.com/api", "p@ss/w0rd"},
		{"https://alice:p@ss?w0rd@hub.example.com/api", "p@ss?w0rd"},
		{"https://u:1234/x@h.example", "1234/x"},
		{"https://u:12?34@h.example/r", "12?34"},
		{"https://tok9/x@h.example", "tok9/x"},
		{"https://tok9#x@h.example/r", "tok9#x"},
		{"https://x-access-token:fake/Pw9q@github.com/acme/repo", "fake/Pw9q"},
	} {
		authority, _, _ := cutRawAuthority(tc.raw)
		after := tc.raw[len("https://"):]
		texts := []string{
			"Get " + tc.raw + ": refused",
			"Get " + strconv.Quote(tc.raw) + ": refused",
			"dial wss://" + after + "/conduit: refused",
			// A URL built from raw with its query and fragment cleared.
			"dial wss://" + authority + "/conduit: refused",
		}
		if i := strings.IndexByte(authority, '@'); i >= 0 {
			// git prints a URL with its user info removed up to the first '@'.
			texts = append(texts, "fatal: unable to access 'https://"+after[i+1:]+"/': 403")
		}
		if b, err := json.Marshal(tc.raw); err == nil {
			texts = append(texts, `{"url":`+string(b)+`}`)
		}
		for _, text := range texts {
			got := RedactURLIn(text, tc.raw, "[SHOWN]", "[M]")
			if f := urlredacttest.SecretFragment(got, tc.secret); f != "" {
				t.Errorf("RedactURLIn(%q, %q) = %q shows %q", text, tc.raw, got, f)
			}
		}
		if got, want := RedactURLIn("Get "+tc.raw+": refused", tc.raw, "[SHOWN]", "[M]"), "Get [SHOWN]: refused"; got != want {
			t.Errorf("RedactURLIn(%q) = %q, want %q", tc.raw, got, want)
		}
	}
}

// A raw without a scheme or "//" (an scp-style clone URL) has no part that
// is replaced on its own, so text that merely contains its user name is
// kept; the user info of any URL in text is still masked by the shared
// rule, and a raw of "" applies that rule alone.
func TestRedactURLIn_NoAuthority(t *testing.T) {
	const raw = "git@github.com:acme/repo.git"
	got := RedactURLIn("git clone "+raw+": git exited 128", raw, "github.com:acme/repo.git", "[M]")
	if want := "git clone github.com:acme/repo.git: git exited 128"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got = RedactURLIn("see https://bob:s3cretPw@other.example/x", "", "", "[M]")
	if want := "see https://[M]@other.example/x"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got = RedactURLIn("see https://bob:s3cretPw@other.example/x", raw, "[SHOWN]", "[M]")
	if want := "see https://[M]@other.example/x"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A part shorter than minSecretLen is not replaced on its own.
func TestRedactURLIn_ShortPart(t *testing.T) {
	const raw = "https://ab/c@h.example"
	if got, want := RedactURLIn("ab and abc/x", raw, "[SHOWN]", "[M]"), "ab and abc/x"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := RedactURLIn("path ab/c here", raw, "[SHOWN]", "[M]"), "path [M] here"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
