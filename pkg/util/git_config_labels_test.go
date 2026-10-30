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

package util

import (
	"strings"
	"testing"
)

// labelFormCases are subsection forms with a marker in a value-bearing part,
// and the label each must print.
var labelFormCases = []struct{ name, sub, want string }{
	{"opaque user:pass@host", "user:labelmarker-t1@host.example", labelRedacted},
	{"scp with password", "git:labelmarker-t1@host.example:o/r.git", labelRedacted},
	{"unencoded slash in password", "https://u:p/labelmarker-t1@host.example/", labelRedacted},
	{"query token", "https://host.example/?access_token=labelmarker-t1", "https://host.example/" + labelRedacted},
	{"fragment", "https://host.example/#labelmarker-t1", "https://host.example/" + labelRedacted},
	{"path token", "https://host.example/labelmarker-t1/x", "https://host.example/" + labelRedacted},
	{"percent-encoded userinfo", "https://u%3Alabelmarker-t1@host.example/", "https://host.example/" + labelRedacted},
	{"ipv6 with userinfo", "https://u:labelmarker-t1@[::1]:8080/", "https://[::1]:8080/" + labelRedacted},
	{"remote helper", "http::https://u:labelmarker-t1@host.example/", labelRedacted},
}

func TestSafeSubsectionLabel_Forms(t *testing.T) {
	for _, tc := range labelFormCases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeSubsectionLabel("url", tc.sub)
			if got != tc.want {
				t.Errorf("safeSubsectionLabel(%q) = %q, want %q", tc.sub, got, tc.want)
			}
			if strings.Contains(got, "labelmarker-t1") {
				t.Errorf("label leaks the marker: %q", got)
			}
		})
	}
	for _, tc := range []struct{ section, sub, want string }{
		{"url", "https://[::1]:8080/", "https://[::1]:8080/"},
		{"url", "HTTPS://Host.example/", "https://Host.example/"},
		{"url", "https://host.example", "https://host.example"},
		{"includeif", "HASCONFIG:remote.*.url:https://u:labelmarker-t1@host.example/**", "hasconfig:remote.*.url:https://host.example/" + labelRedacted},
		{"includeif", "HasConfig:remote.*.url:https://host.example/**", "hasconfig:remote.*.url:https://host.example/" + labelRedacted},
		{"credential", "https://host.example", labelRedacted},
		{"includeif", "gitdir:/w/", labelRedacted},
	} {
		if got := safeSubsectionLabel(tc.section, tc.sub); got != tc.want {
			t.Errorf("safeSubsectionLabel(%q, %q) = %q, want %q", tc.section, tc.sub, got, tc.want)
		}
	}
}

// Plain subsections and keys without one are printed byte for byte, including
// the special refusal keys.
func TestNotedKeyName_PlainUnchanged(t *testing.T) {
	for _, key := range []string{"core.sshcommand", "branch.feature/x.remote", "remote.origin.url", "submodule.libs/a.url", "x..y"} {
		if got := refusalKeyName(key); got != key {
			t.Errorf("refusalKeyName(%q) = %q, want it unchanged", key, got)
		}
	}
	for _, key := range []string{".git/commondir", ".git/gitdir"} {
		if got := refusalKeyName(key); got != key {
			t.Errorf("refusalKeyName(%q) = %q, want it unchanged", key, got)
		}
	}
}

// Each form is absent from a refusal's Key and Error(), and from the stripped
// list RedactGitConfigSecrets returns (through the real redactGitConfigVars).
func TestSafeSubsectionLabel_RefusalAndStrippedList(t *testing.T) {
	for _, tc := range labelFormCases {
		t.Run(tc.name, func(t *testing.T) {
			raw := "[url \"" + escapeConfigSubsection(tc.sub) + "\"]\n\tinsteadOf = https://github.com/\n"
			_ = mustRefuseSeed(t, raw, "url."+tc.want+".insteadof", "labelmarker-t1")
			// http.<sub>.extraHeader is dropped, so its name is always noted.
			drop := "[http \"" + escapeConfigSubsection(tc.sub) + "\"]\n\textraHeader = x\n"
			_, stripped, err := RedactGitConfigSecrets([]byte(drop))
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(stripped, "\n")
			if strings.Contains(joined, "labelmarker-t1") {
				t.Errorf("stripped list leaks the marker: %v", stripped)
			}
			if want := "http." + tc.want + ".extraheader"; !strings.Contains(joined, want) {
				t.Errorf("stripped = %v, want %s", stripped, want)
			}
		})
	}
}

// Kept content is not rewritten by the label rule: a url.<base> subsection
// with a path is serialized verbatim.
func TestRedactGitConfigSecrets_KeptSubsectionVerbatim(t *testing.T) {
	out, stripped, err := RedactGitConfigSecrets([]byte("[url \"https://github.com/org/\"]\n\tinsteadOf = https://mirror.example/org/\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "[url \"https://github.com/org/\"]") || len(stripped) != 0 {
		t.Errorf("kept subsection changed: %q, stripped %v", out, stripped)
	}
}

// The kept-content hasconfig prefix is matched case-insensitively: upper- and
// mixed-case forms get the same rewrite decision as the lower-case form, keep
// their prefix bytes, and leak the marker neither in the output nor in the
// stripped list.
func TestRedactGitConfigSecrets_HasconfigPrefixAnyCase(t *testing.T) {
	const url = "https://u:casemarker-u1@host.example/**"
	decide := func(prefix string) (string, []string) {
		t.Helper()
		in := "[includeIf \"" + prefix + url + "\"]\n\tpath = /x/.gitconfig\n"
		out, stripped, err := RedactGitConfigSecrets([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "casemarker-u1") || strings.Contains(strings.Join(stripped, "\n"), "casemarker-u1") {
			t.Errorf("prefix %q: marker survived: %q stripped %v", prefix, out, stripped)
		}
		return string(out), stripped
	}
	lowerOut, lowerStripped := decide("hasconfig:remote.*.url:")
	for _, prefix := range []string{"HASCONFIG:remote.*.url:", "HasConfig:Remote.*.URL:"} {
		out, stripped := decide(prefix)
		if want := strings.Replace(lowerOut, "hasconfig:remote.*.url:", prefix, 1); out != want {
			t.Errorf("prefix %q: out = %q, want %q", prefix, out, want)
		}
		if strings.Join(stripped, "\n") != strings.Join(lowerStripped, "\n") {
			t.Errorf("prefix %q: stripped = %v, want %v", prefix, stripped, lowerStripped)
		}
	}
}
