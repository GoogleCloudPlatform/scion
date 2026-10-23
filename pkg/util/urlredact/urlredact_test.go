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
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

func TestTable_RedactUserInfo(t *testing.T) {
	urlredacttest.Check(t, urlredacttest.URLRedactRedactUserInfo, RedactUserInfo)
}

func TestTable_RedactUserInfoIn(t *testing.T) {
	urlredacttest.Check(t, urlredacttest.URLRedactRedactUserInfoIn, func(in string) string {
		return RedactUserInfoIn(urlredacttest.Text(in), Mask)
	})
}

// Every row has an output for every column and for no other, and every
// column's runner file calls Check for it, so a redactor cannot be added to
// the table without a test, or dropped from its test unnoticed.
func TestTableColumnsHaveRunners(t *testing.T) {
	names := map[string]bool{}
	for _, c := range urlredacttest.Columns {
		if names[c.Name] {
			t.Errorf("column %s listed twice", c.Name)
		}
		names[c.Name] = true
		src, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(c.Runner)))
		if err != nil {
			t.Errorf("%s: runner: %v", c.Name, err)
			continue
		}
		if call := "urlredacttest.Check(t, urlredacttest." + c.Const + ","; !strings.Contains(string(src), call) {
			t.Errorf("%s: %s does not contain %q", c.Name, c.Runner, call)
		}
	}
	for c := range urlredacttest.KnownUnmigrated {
		if !names[c] {
			t.Errorf("KnownUnmigrated column %s is not in Columns", c)
		}
	}
	for _, r := range urlredacttest.Rows {
		if len(r.Want) != len(names) {
			t.Errorf("%s: %d outputs, want %d", r.Name, len(r.Want), len(names))
		}
		for c := range r.Want {
			if !names[c] {
				t.Errorf("%s: output for unknown column %s", r.Name, c)
			}
		}
		for _, c := range r.Residual {
			if !names[c] {
				t.Errorf("%s: residual for unknown column %s", r.Name, c)
			}
		}
	}
}

// Every redactor on the table uses the shared rule: none is listed as
// unmigrated. A redactor added to the table before it is moved is listed
// in KnownUnmigrated, and this test changed with it, saying why.
func TestTableHasNoUnmigratedColumns(t *testing.T) {
	for c := range urlredacttest.KnownUnmigrated {
		t.Errorf("column %s is listed as not using the shared rule", c)
	}
}

func TestUndelimited(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"https://hub.example.com/api", false},
		{"https://alice:s3cretPw@hub.example.com/api", false},
		{"https://alice:fa@ke9q@hub.example.com/api", false},
		{"https://hub.example.com/users/a@b.example.com", false},
		{"http://user:s3@cret@127.0.0.1:99999x/x", false},
		{"https://alice:fake/Pw9q@hub.example.com/api", true},
		{"https://alice:fake?Pw9q@hub.example.com/api", true},
		{"https://alice:fake#Pw9q@hub.example.com/api", true},
		{"https://alice:p@ss/w0rd@hub.example.com", true},
		{"https://alice:p@ss:x/w0rd@hub.example.com", true},
		{"https://hub.example.com/%zz/alice@x", true},
		// By the rule: indistinguishable from an e-mail path.
		{"https://u:1234/x@h.example", false},
		{"https://tok9/x@h.example", false},
	} {
		if got := Undelimited(tc.in); got != tc.want {
			t.Errorf("Undelimited(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestMaskWhole(t *testing.T) {
	for in, want := range map[string]string{
		"https://alice:fake/Pw9q@hub.example.com": "https://[M]",
		"//alice:fake/Pw9q@hub.example.com":       "[M]",
		"alice:fake/Pw9q@hub.example.com":         "[M]",
	} {
		if got := MaskWhole(in, "[M]"); got != want {
			t.Errorf("MaskWhole(%q) = %q, want %q", in, got, want)
		}
	}
}

// The user info located in a single URL: the whole of it, user name too,
// is replaced and every other byte kept.
func TestRedactUserInfoIn_SingleURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://alice:s3cretPw@hub.example.com/a?b=c": "https://[M]@hub.example.com/a?b=c",
		"https://t%40k@hub.example.com":                "https://[M]@hub.example.com",
		"http://user:s3@cret@127.0.0.1:99999x/x":       "http://[M]@127.0.0.1:99999x/x",
		"https://alice:fake/Pw9q@hub.example.com/api":  "https://[M]",
		"https://hub.example.com/users/a@b.example":    "https://hub.example.com/users/a@b.example",
		"https://hub.example.com/api":                  "https://hub.example.com/api",
		"":                                             "",
	} {
		if got := RedactUserInfoIn(in, "[M]"); got != want {
			t.Errorf("RedactUserInfoIn(%q) = %q, want %q", in, got, want)
		}
	}
	// Without a scheme the span starts after "//". A schemeless
	// "user:pass@host" parses as an opaque URL with no user info (a known
	// residual of the rule, recorded with D215).
	for in, want := range map[string][3]int{
		"//alice:s3cretPw@hub.example.com/a": {userInfoSpan, 2, 16},
		"//hub.example.com/a@b":              {noUserInfo, 0, 0},
		"alice:s3cretPw@hub.example.com":     {noUserInfo, 0, 0},
	} {
		if k, s, e := locateUserInfo(in); [3]int{k, s, e} != want {
			t.Errorf("locateUserInfo(%q) = %d %d %d, want %v", in, k, s, e, want)
		}
	}
}

// RedactUserInfoIn: white space is the only boundary of a URL, the
// punctuation after it is kept, and every URL in a text is checked,
// including one inside another.
func TestRedactUserInfoIn(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"none", "no URL here: just text", "no URL here: just text"},
		{"scheme only", "net/http: request canceled; see https://", "net/http: request canceled; see https://"},
		{"sentence end", "see https://alice:s3cretPw@hub.example.com.", "see https://[M]@hub.example.com."},
		{"brackets", "(https://alice:s3cretPw@hub.example.com/x)", "(https://[M]@hub.example.com/x)"},
		{"JSON-escaped quotes", `{"e":"Get \"https://alice:s3cretPw@hub.example.com/x\": EOF"}`, `{"e":"Get \"https://[M]@hub.example.com/x\": EOF"}`},
		{"quote in password", "dial https://alice:p'ss)w0rd@hub.example.com/x now", "dial https://[M]@hub.example.com/x now"},
		{"two URLs", "https://a:s3cretPw@one.example and https://b:fake/Pw9q@two.example/x", "https://[M]@one.example and https://[M]"},
		{"joined URLs", "https://one.example/x,https://b:s3cretPw@two.example/x", "https://one.example/x,https://[M]@two.example/x"},
		{"nested, undelimited", "https://hub.example.com/r?next=https://b:fake/Pw9q@two.example/x", "https://hub.example.com/r?next=https://[M]"},
		{"outer masked whole", "https://a:p@ss/w0rd@one.example/r?next=https://b:s3cretPw@two.example", "https://[M]"},
		{"other scheme", "git clone ssh://git:s3cretPw@git.example.com/r.git", "git clone ssh://[M]@git.example.com/r.git"},
		{"tab boundary", "a\thttps://alice:s3cretPw@hub.example.com\tb", "a\thttps://[M]@hub.example.com\tb"},
		{"non-breaking space in password", "fatal: unable to access 'https://u:pa\u00a0ss9q@h.example/r'", "fatal: unable to access 'https://[M]@h.example/r'"},
		{"line separator in password", "dial https://u:pa\u2028ss9q@h.example/r: refused", "dial https://[M]@h.example/r: refused"},
		{"vertical tab boundary", "a\vhttps://alice:s3cretPw@hub.example.com\vb", "a\vhttps://[M]@hub.example.com\vb"},
	} {
		if got := RedactUserInfoIn(tc.in, "[M]"); got != tc.want {
			t.Errorf("%s: RedactUserInfoIn(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// redactPropertyAlphabet is the junk the property test puts around a
// made-up secret in a password: URL delimiters, quotes, brackets, escapes,
// invalid UTF-8 and non-ASCII spaces, never ASCII white space (the only URL
// boundary in a text).
var redactPropertyAlphabet = []string{
	"a", "Z", "0", "/", "?", "#", "@", ":", ".", "&", ";", "=", "%", "%zz", "%40",
	"'", `"`, ")", "]", "[", "<", ">", "\\", "`", ",", "\xff", "é", "\u00a0", "\u2028",
}

func redactPropertyJunk(r *rand.Rand) string {
	var b strings.Builder
	for n := r.Intn(8); n > 0; n-- {
		b.WriteString(redactPropertyAlphabet[r.Intn(len(redactPropertyAlphabet))])
	}
	return b.String()
}

// For any password that starts with a letter (so it never reads as a port)
// and holds any junk around a made-up secret, neither RedactUserInfo nor
// RedactUserInfoIn shows any 4-byte part of the secret.
func TestRedact_PropertyPasswordJunk(t *testing.T) {
	const secret = "S3CRETVAL"
	r := rand.New(rand.NewSource(219))
	for i := 0; i < 20000; i++ {
		in := "https://alice:p" + redactPropertyJunk(r) + secret + redactPropertyJunk(r) + "@hub.example.com/" + redactPropertyJunk(r)
		for name, got := range map[string]string{
			"RedactUserInfo":   RedactUserInfo(in),
			"RedactUserInfoIn": RedactUserInfoIn(urlredacttest.Text(in), Mask),
		} {
			for k := 0; k+4 <= len(secret); k++ {
				if strings.Contains(got, secret[k:k+4]) {
					t.Fatalf("%s(%q) = %q, shows %q", name, in, got, secret[k:k+4])
				}
			}
		}
	}
}

// adversarialText returns n bytes of "a://" repeated (a scheme start every
// 4 bytes, no white space), followed by tail.
func adversarialText(n int, tail string) string {
	return strings.Repeat("a://", n/4) + tail
}

// RedactUserInfoIn takes time linear in its input: 1 MiB of dense scheme
// starts with no white space, with and without an '@' at the end, finishes
// well within a bound that a quadratic scan (minutes at this size) cannot
// meet, and the run with an '@' is masked whole (the URL is longer than
// MaxTextURLLen).
func TestRedactUserInfoIn_LinearTime(t *testing.T) {
	const size = 1 << 20
	for _, tc := range []struct{ name, in, want string }{
		{"no '@'", adversarialText(size, ""), adversarialText(size, "")},
		{"'@' at the end", adversarialText(size, "@h.example"), "a://[M]"},
		{"'@' at the end, then text", adversarialText(size, "@h.example") + " done", "a://[M] done"},
		{"many runs", strings.Repeat("a://b@c ", size/8), strings.Repeat("a://[M]@c ", size/8)},
	} {
		start := time.Now()
		got := RedactUserInfoIn(tc.in, "[M]")
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: RedactUserInfoIn on %d bytes took %v", tc.name, len(tc.in), d)
		}
		if got != tc.want {
			t.Errorf("%s: RedactUserInfoIn on %d bytes = %d bytes starting %q, want %d bytes starting %q", tc.name, len(tc.in), len(got), prefix(got), len(tc.want), prefix(tc.want))
		}
	}
}

func prefix(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

// Past MaxTextURLLen, or past MaxTextURLsPerRun URLs in one run, the rest of
// the run is masked whole when an '@' follows; text after the run is kept.
func TestRedactUserInfoIn_Limits(t *testing.T) {
	long := "https://alice:s3cretPw@hub.example.com/" + strings.Repeat("x", MaxTextURLLen)
	if got, want := RedactUserInfoIn("see "+long+" now", "[M]"), "see https://[M] now"; got != want {
		t.Errorf("long URL: got %q, want %q", prefix(got), want)
	}
	atMax := "https://alice:s3cretPw@hub.example.com/" + strings.Repeat("x", MaxTextURLLen-len("https://alice:s3cretPw@hub.example.com/"))
	if got, want := RedactUserInfoIn(atMax, "[M]"), "https://[M]@hub.example.com/"+strings.Repeat("x", MaxTextURLLen-len("https://alice:s3cretPw@hub.example.com/")); got != want {
		t.Errorf("URL of MaxTextURLLen bytes: got %q, want its user info masked", prefix(got))
	}
	var nested, want strings.Builder
	for i := 0; i < MaxTextURLsPerRun; i++ {
		nested.WriteString("https://h.example/r?next=")
		want.WriteString("https://h.example/r?next=")
	}
	nested.WriteString("https://alice:s3cretPw@last.example/x")
	want.WriteString("https://[M]")
	if got := RedactUserInfoIn(nested.String()+" end", "[M]"); got != want.String()+" end" {
		t.Errorf("more than MaxTextURLsPerRun URLs: got %q, want %q", got, want.String()+" end")
	}
	// At the limit, the last nested URL is still checked on its own.
	inner := strings.Repeat("https://h.example/r?next=", MaxTextURLsPerRun-1) + "https://alice:s3cretPw@last.example/x"
	if got, want := RedactUserInfoIn(inner, "[M]"), strings.Repeat("https://h.example/r?next=", MaxTextURLsPerRun-1)+"https://[M]@last.example/x"; got != want {
		t.Errorf("MaxTextURLsPerRun URLs: got %q, want %q", got, want)
	}
}

// BenchmarkRedactUserInfoIn measures the scanner on 64 KiB of dense scheme
// starts ending in an '@'.
func BenchmarkRedactUserInfoIn(b *testing.B) {
	in := adversarialText(64<<10, "@h.example")
	b.SetBytes(int64(len(in)))
	for i := 0; i < b.N; i++ {
		RedactUserInfoIn(in, Mask)
	}
}
