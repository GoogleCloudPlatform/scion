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
	"fmt"
	"os"
	"strings"
	"testing"
)

// unparsedURLCase is one line of testdata/url_redact_unparsed.tsv, the
// table of URLs that url.Parse rejects. pkg/apiclient's redact_test.go
// reads the same file for its copy of the rules.
type unparsedURLCase struct{ name, in, want, secret string }

func loadUnparsedURLCases(t *testing.T) []unparsedURLCase {
	t.Helper()
	data, err := os.ReadFile("testdata/url_redact_unparsed.tsv")
	if err != nil {
		t.Fatal(err)
	}
	var cases []unparsedURLCase
	for i, line := range strings.Split(string(data), "\n") {
		// A CRLF checkout (Windows) leaves a "\r" at the end of each line.
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			t.Fatalf("testdata/url_redact_unparsed.tsv:%d: %d fields, want 4", i+1, len(f))
		}
		cases = append(cases, unparsedURLCase{f[0], f[1], f[2], f[3]})
	}
	if len(cases) == 0 {
		t.Fatal("testdata/url_redact_unparsed.tsv: no cases")
	}
	return cases
}

// checkUnparsedURLCases runs the table against redact: each output is
// exactly the wanted one and shows no 4-byte part of the case's secret.
func checkUnparsedURLCases(t *testing.T, fn string, redact func(string) string) {
	t.Helper()
	for _, tc := range loadUnparsedURLCases(t) {
		got := redact(tc.in)
		if got != tc.want {
			t.Errorf("%s: %s(%q) = %q, want %q", tc.name, fn, tc.in, got, tc.want)
		}
		if tc.secret == "-" {
			continue
		}
		for i := 0; i+4 <= len(tc.secret); i++ {
			if strings.Contains(got, tc.secret[i:i+4]) {
				t.Errorf("%s: %s(%q) = %q, shows %q", tc.name, fn, tc.in, got, tc.secret[i:i+4])
			}
		}
	}
}

// A URL that does not parse, whose user info the fallback rule cannot
// delimit, shows only its scheme and a mask; URLs the rules already
// handled, and ordinary URLs, keep their output.
func TestRedactURL_Unparsed(t *testing.T) {
	checkUnparsedURLCases(t, "RedactURL", RedactURL)
}

// In a text, a URL whose user info holds a '/', '?' or '#' shows only its
// scheme and a mask, and the text around it is kept; a URL with an '@' in
// its path is unchanged.
func TestRedactURLsIn_UndelimitedUserInfo(t *testing.T) {
	tests := []struct{ name, in, want, secret string }{
		{"slash", `Get "https://alice:fake/Pw9q@hub.example.com/api": refused`, `Get "https://xxxxx": refused`, "fake/Pw9q"},
		{"question mark", "dial https://alice:fake?Pw9q@hub.example.com/api: refused", "dial https://xxxxx: refused", "fake?Pw9q"},
		{"hash", "hub at https://alice:fake#Pw9q@hub.example.com is down.", "hub at https://xxxxx is down.", "fake#Pw9q"},
		{"user name with slash", "see https://tok_fake/Pw9q@hub.example.com/x, then", "see https://xxxxx, then", "tok_fake/Pw9q"},
		{"'@' then ':' and '/' in password", `Get "https://alice:p@ss:x/w0rd@hub.example.com/api": refused`, `Get "https://xxxxx": refused`, "p@ss:x/w0rd"},
		{"two '@' in password, bad port", "dial http://user:s3@cret@127.0.0.1:99999x/x: refused", "dial http://xxxxx@127.0.0.1:99999x/x: refused", "s3@cret"},
		{"user info and '@' in the path", "dial https://alice:p@ss/w0rd@hub.example.com: refused", "dial https://xxxxx: refused", "p@ss/w0rd"},
		{"user info and '@' in the query", "see https://alice:p@ss?w0rd@hub.example.com.", "see https://xxxxx.", "p@ss?w0rd"},
		{"user info and no '@' after the host", "dial https://alice:s3cretPw@hub.example.com/u: refused", "dial https://alice:xxxxx@hub.example.com/u: refused", "s3cretPw"},
		{"two URLs", "https://alice:fake/Pw9q@one.example.com and https://two.example.com/u/@me", "https://xxxxx and https://two.example.com/u/@me", "fake/Pw9q"},
		{"e-mail in a path", "see https://hub.example.com/users/a@b.example.com/profile now", "see https://hub.example.com/users/a@b.example.com/profile now", "-"},
	}
	for _, tc := range tests {
		got := RedactURLsIn(tc.in)
		if got != tc.want {
			t.Errorf("%s: RedactURLsIn(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if tc.secret == "-" {
			continue
		}
		for i := 0; i+4 <= len(tc.secret); i++ {
			if strings.Contains(got, tc.secret[i:i+4]) {
				t.Errorf("%s: RedactURLsIn(%q) = %q, shows %q", tc.name, tc.in, got, tc.secret[i:i+4])
			}
		}
	}
}

// ValidateHubURL refuses a hub URL with an '@' anywhere, and its error,
// and the same URL quoted in a text and passed through RedactURLsIn, show
// no part of the password, for passwords with '/', '?' or '#'. A password
// that parses as a port and a path is refused at entry without being
// shown; RedactURLsIn cannot tell it from a port and a path, so its text
// form is not checked.
func TestValidateHubURL_RefusalShowsNoPassword(t *testing.T) {
	for _, tc := range []struct {
		in, secret string
		text       bool
	}{
		{"https://alice:fake/Pw9q@hub.example.com", "fake/Pw9q", true},
		{"https://alice:fake?Pw9q@hub.example.com", "fake?Pw9q", true},
		{"https://alice:fake#Pw9q@hub.example.com", "fake#Pw9q", true},
		{"  https://tok_fake/Pw9q@hub.example.com  ", "tok_fake/Pw9q", true},
		{"https://alice:1234/Pw9q@hub.example.com", "1234/Pw9q", false},
	} {
		err := ValidateHubURL(tc.in)
		if err == nil {
			t.Errorf("ValidateHubURL(%q) accepted the URL", tc.in)
			continue
		}
		if want := `invalid hub URL "https://xxxxx": a hub URL must not contain '@'`; err.Error() != want {
			t.Errorf("ValidateHubURL(%q) = %q, want %q", tc.in, err.Error(), want)
		}
		outs := []string{err.Error()}
		if tc.text {
			outs = append(outs, RedactURLsIn(fmt.Sprintf("invalid hub URL %q", strings.TrimSpace(tc.in))))
		}
		for _, out := range outs {
			for i := 0; i+4 <= len(tc.secret); i++ {
				if strings.Contains(out, tc.secret[i:i+4]) {
					t.Errorf("%q: %q shows %q", tc.in, out, tc.secret[i:i+4])
				}
			}
		}
	}
}

// An '@' anywhere in a hub URL is refused, by hub add (AddHub) and by
// ValidateHubURL, even outside the user info.
func TestCLIContextAddHub_RefusesAt(t *testing.T) {
	for _, u := range []string{
		"https://hub.example.com/@x",
		"https://hub.example.com/u/a@b.example.com",
		"https://alice:1234/Pw9q@hub.example.com",
		"https://tok@hub.example.com",
	} {
		err := newCLIContext().AddHub("prod", u)
		if err == nil || !strings.Contains(err.Error(), "a hub URL must not contain '@'") {
			t.Errorf("AddHub(prod, %q) = %v, want the '@' refusal", u, err)
		}
	}
}

// A global hub.endpoint with an '@' outside the user info is not imported.
func TestLoadCLIContextImported_EndpointWithAtNotImported(t *testing.T) {
	cliContextTestHome(t)
	writeGlobalSettingsFile(t, "hub:\n  endpoint: https://hub.example.com/u/a@b.example.com\n")
	c, err := LoadCLIContextImported(importNamer)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hubs) != 0 || c.GlobalImported() {
		t.Errorf("context = %+v, want nothing imported", c)
	}
}

// A linked project whose hub.endpoint has an '@' outside the user info
// gets no link entry; the import is recorded.
func TestLinkEntryForProject_EndpointWithAtNotLinked(t *testing.T) {
	cliContextTestHome(t)
	projectDir, _, linkPath := linkImportProject(t, "hub:\n  endpoint: https://alice:1234/Pw9q@proj.example.com\n  projectId: hub-1\n  linked: true\n")
	c := newCLIContext()
	p, h, err := LinkEntryForProject(c, projectDir)
	if err != nil || p != nil || h != nil {
		t.Fatalf("= %+v, %+v, %v; want nothing", p, h, err)
	}
	if len(c.Hubs) != 0 || !c.ProjectImported(linkPath) {
		t.Errorf("context = %+v, want no hub and the import recorded", c)
	}
}
