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

func TestReplaceUserInfo(t *testing.T) {
	for in, want := range map[string]string{
		"https://alice:s3cretPw@hub.example.com/a?b=c": "https://[M]@hub.example.com/a?b=c",
		"//alice:s3cretPw@hub.example.com/a":           "//[M]@hub.example.com/a",
		"https://t%40k@hub.example.com":                "https://[M]@hub.example.com",
		"http://user:s3@cret@127.0.0.1:99999x/x":       "http://[M]@127.0.0.1:99999x/x",
		"https://alice:fake/Pw9q@hub.example.com/api":  "https://[M]",
		"https://hub.example.com/users/a@b.example":    "https://hub.example.com/users/a@b.example",
		"https://hub.example.com/api":                  "https://hub.example.com/api",
		"":                                             "",
	} {
		if got := ReplaceUserInfo(in, "[M]"); got != want {
			t.Errorf("ReplaceUserInfo(%q) = %q, want %q", in, got, want)
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
	} {
		if got := RedactUserInfoIn(tc.in, "[M]"); got != tc.want {
			t.Errorf("%s: RedactUserInfoIn(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// redactPropertyAlphabet is the junk the property test puts around a
// made-up secret in a password: URL delimiters, quotes, brackets, escapes
// and invalid UTF-8, never white space (the only URL boundary in a text).
var redactPropertyAlphabet = []string{
	"a", "Z", "0", "/", "?", "#", "@", ":", ".", "&", ";", "=", "%", "%zz", "%40",
	"'", `"`, ")", "]", "[", "<", ">", "\\", "`", ",", "\xff", "é",
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
			"ReplaceUserInfo":  ReplaceUserInfo(in, Mask),
		} {
			for k := 0; k+4 <= len(secret); k++ {
				if strings.Contains(got, secret[k:k+4]) {
					t.Fatalf("%s(%q) = %q, shows %q", name, in, got, secret[k:k+4])
				}
			}
		}
	}
}
