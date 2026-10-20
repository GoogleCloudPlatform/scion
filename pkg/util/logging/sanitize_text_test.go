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
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain runtime text kept", "docker run scion-ctr-1: exit status 125", "docker run scion-ctr-1: exit status 125"},
		{"kubernetes refusal kept",
			`pods "agent-pod-4555" is forbidden: User "system:serviceaccount:team-ns:scion-sa" cannot get resource "pods"`,
			`pods "agent-pod-4555" is forbidden: User "system:serviceaccount:team-ns:scion-sa" cannot get resource "pods"`},
		{"long lower-case name kept", "pod scion-agent-a-rather-long-generated-name-0001 failed", "pod scion-agent-a-rather-long-generated-name-0001 failed"},
		{"env value blanked, key kept", "run failed: FOO=bar BAZ=qux", "run failed: FOO=[REDACTED] BAZ=[REDACTED]"},
		{"unquoted value with spaces", "env KEY=a b c, next", "env KEY=[REDACTED], next"},
		{"prose after a value is blanked", "run failed: FOO=bar BAZ=qux done", "run failed: FOO=[REDACTED] BAZ=[REDACTED]"},
		{"spaced separator ends a value", "TOKEN=a SECRET =b", "TOKEN=[REDACTED] SECRET =[REDACTED]"},
		{"separator on the next line ends a value", "TOKEN=a SECRET\n=b", "TOKEN=[REDACTED] SECRET\n=[REDACTED]"},
		{"spaced colon ends a value", "TOKEN=a value : b", "TOKEN=[REDACTED] value : [REDACTED]"},
		{"earlier marker inside a value", "FOO=x Bearer abcdefghijkl end", "FOO=[REDACTED]"},
		{"quoted value blanked", `bad env X="a b c"`, "bad env X=[REDACTED]"},
		{"flag value blanked", "unknown flag --env=FOO=bar", "unknown flag --env=[REDACTED]"},
		{"credential-named JSON key", `spec {"DB_PASSWORD": "hunter2-value"}`, `spec {"DB_PASSWORD": [REDACTED]}`},
		{"SCION_ YAML key", "SCION_HUB_ENDPOINT: https://hub.example.com", "SCION_HUB_ENDPOINT: [REDACTED]"},
		{"URL userinfo", "pull https://deploy:pw-value@registry.example.com/img:1 failed", "pull https://[REDACTED]@registry.example.com/img:1 failed"},
		{"authorization scheme", "header Authorization: Bearer abcdefghijklmnop0123 refused", "header Authorization: Bearer [REDACTED] refused"},
		{"JWT-shaped string", "got eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl here", "got [REDACTED] here"},
		{"PEM block", "key -----BEGIN PRIVATE KEY-----\nMIIBVQIBADANBg\n-----END PRIVATE KEY----- end", "key [REDACTED] end"},
		{"unterminated PEM block", "key -----BEGIN CERTIFICATE-----\nMIIBVQIBADANBg", "key [REDACTED]"},
		{"long hex run", "container 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef exited", "container [REDACTED] exited"},
		{"single-quoted key", "spec {'password': 'hunter2-value'}", "spec {'password': [REDACTED]}"},
		{"passphrase key", "key_passphrase: hunter2-value", "key_passphrase: [REDACTED]"},
		{"lower-case colon key", "spec password: hunter2-value; end", "spec password: [REDACTED]; end"},
		{"hyphenated header key", "header x-api-key: abc123, sent", "header x-api-key: [REDACTED], sent"},
		{"capitalized key, no space", "Password:hunter2", "Password:[REDACTED]"},
		{"auth as a whole key word", "auth-header: abc123 X_AUTH=xyz", "auth-header: [REDACTED] X_AUTH=[REDACTED]"},
		{"registry refusal kept", "unauthorized: authentication required", "unauthorized: authentication required"},
		{"EnvVar JSON value", `env [{"name":"GITHUB_PAT","value":"hunter2"}]`, `env [{"name":"GITHUB_PAT","value":[REDACTED]}]`},
		{"EnvVar %+v value", "env {Name:GITHUB_PAT Value:hunter2 ValueFrom:nil}", "env {Name:GITHUB_PAT Value:[REDACTED] ValueFrom:nil}"},
		{"basic credentials after Authorization", "Authorization: Basic dXNlcjpwYXNz", "Authorization: Basic [REDACTED]"},
		{"basic encoded credentials", "sent basic dXNlcjpwYXNzd29yZA== upstream", "sent basic [REDACTED] upstream"},
		{"basic prose kept", "basic validation failed", "basic validation failed"},
		{"long base64 run", "blob QWxhZGRpbjpvcGVuIHNlc2FtZQ0123456789AbCdEf end", "blob [REDACTED] end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeText(tt.in); got != tt.want {
				t.Errorf("SanitizeText(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeText_Truncates(t *testing.T) {
	in := strings.Repeat("output line ", 300)
	got := SanitizeText(in)
	suffix := fmt.Sprintf("...[truncated, %d bytes]", len(in))
	if !strings.HasSuffix(got, suffix) {
		t.Fatalf("got %q, want suffix %q", got[len(got)-60:], suffix)
	}
	if body := strings.TrimSuffix(got, suffix); len(body) != MaxSanitizedTextBytes || !strings.HasPrefix(in, body) {
		t.Errorf("kept %d bytes, want the first %d", len(body), MaxSanitizedTextBytes)
	}

	// The cut never splits a multi-byte rune.
	multi := strings.Repeat("é", MaxSanitizedTextBytes)
	got = SanitizeText(multi)
	if !utf8.ValidString(got) {
		t.Errorf("truncated text is not valid UTF-8")
	}
}

// TestSanitizeText_KeyRegexGroups pins the shape blankKeyValues relies on:
// every key regex has exactly three groups (key, separator, value).
func TestSanitizeText_KeyRegexGroups(t *testing.T) {
	for name, re := range map[string]interface{ NumSubexp() int }{
		"bare credential key":   textBareCredentialKeyRe,
		"quoted credential key": textQuotedCredentialKeyRe,
		"value key":             textValueKeyRe,
		"key=value":             textKeyValueRe,
	} {
		if n := re.NumSubexp(); n != 3 {
			t.Errorf("%s regex has %d groups, want 3", name, n)
		}
	}
}

// TestSanitizeText_ValueEndMatchesDelimiters keeps isValueEnd in step
// with textValueDelimiters.
func TestSanitizeText_ValueEndMatchesDelimiters(t *testing.T) {
	for c := 0; c < 256; c++ {
		want := strings.IndexByte(textValueDelimiters+" \t\r\n", byte(c)) >= 0
		if got := isValueEnd(byte(c)); got != want {
			t.Errorf("isValueEnd(%q) = %v, want %v", byte(c), got, want)
		}
	}
}
