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

package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// Test references: one with userinfo and a query, a gh:// one with a
// query, and a public one.
const (
	redactTestHTTPRef  = "https://user:x@example.com/skills/s?token=x"
	redactTestGHRef    = "gh://owner/repo/s?token=x"
	redactTestPlainRef = "gh://owner/repo/plain-skill@v1"
)

// redactCases are declared references and their logged form.
var redactCases = []struct {
	name, ref, want string
}{
	{"https with userinfo and query", redactTestHTTPRef, "https://(redacted URL)"},
	{"gh with query", redactTestGHRef, RedactedURL},
	{"gh with userinfo", "gh://user:x@owner/repo/s", RedactedURL},
	{"other scheme", "ssh://user:x@example.com/s", RedactedURL},
	{"plain gh", redactTestPlainRef, redactTestPlainRef},
	{"plain skill", "skill://scion/core/s@1.0", "skill://scion/core/s@1.0"},
	{"plain name", "my-skill", "my-skill"},
	{"https path only", "https://example.com/s", "https://example.com/s"},
}

func TestLoggedSkillRef_StringAndFormat(t *testing.T) {
	for _, tc := range redactCases {
		t.Run(tc.name, func(t *testing.T) {
			r := LoggedSkillRef(tc.ref)
			if got := r.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			if got := RedactSkillRef(tc.ref); got != tc.want {
				t.Errorf("RedactSkillRef = %q, want %q", got, tc.want)
			}
			if got, want := fmt.Sprintf("[%s]", r), "["+tc.want+"]"; got != want {
				t.Errorf("%%s = %q, want %q", got, want)
			}
			if got, want := fmt.Sprintf("[%v]", r), "["+tc.want+"]"; got != want {
				t.Errorf("%%v = %q, want %q", got, want)
			}
		})
	}
}

func TestLoggedSkillRef_LogValue(t *testing.T) {
	for _, tc := range redactCases {
		t.Run(tc.name, func(t *testing.T) {
			v := LoggedSkillRef(tc.ref).LogValue()
			if v.Kind() != slog.KindString || v.String() != tc.want {
				t.Errorf("LogValue() = %v (%v), want string %q", v, v.Kind(), tc.want)
			}
		})
	}
}

// TestLoggedSkillRef_Handlers checks that slog's JSON and text handlers
// both write the redacted form.
func TestLoggedSkillRef_Handlers(t *testing.T) {
	for _, tc := range redactCases {
		t.Run(tc.name, func(t *testing.T) {
			for name, newHandler := range map[string]func(*bytes.Buffer) slog.Handler{
				"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
				"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
			} {
				var buf bytes.Buffer
				slog.New(newHandler(&buf)).InfoContext(context.Background(), "ref", "skill_uri", LoggedSkillRef(tc.ref))
				line := buf.String()
				if !strings.Contains(line, tc.want) {
					t.Errorf("%s handler: %q does not hold %q", name, line, tc.want)
				}
				if tc.want != tc.ref && strings.Contains(line, tc.ref) {
					t.Errorf("%s handler: %q holds the declared reference", name, line)
				}
				for _, part := range []string{"user:x", "token=x"} {
					if strings.Contains(line, part) {
						t.Errorf("%s handler: %q holds %q", name, line, part)
					}
				}
			}
		})
	}
}

func TestSkillRefRedacted(t *testing.T) {
	for _, tc := range redactCases {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := SkillRefRedacted(tc.ref), tc.want != tc.ref; got != want {
				t.Errorf("SkillRefRedacted(%q) = %v, want %v", tc.ref, got, want)
			}
		})
	}
}

func TestResolverReasonForLog(t *testing.T) {
	for _, tc := range []struct {
		name, ref, reason, want string
	}{
		{"redacted ref withholds", redactTestHTTPRef, "skill not found", ResolverMessageWithheld},
		{"redacted ref withholds a quoting reason", redactTestGHRef, "bad ref " + redactTestGHRef, ResolverMessageWithheld},
		{"plain ref keeps the reason", redactTestPlainRef, "skill not found", "skill not found"},
		{"plain ref redacts URLs in the reason", redactTestPlainRef,
			`Get "https://example.com/r?sig=x": EOF`, `Get "https://(redacted URL) EOF`},
		{"plain ref redacts userinfo in the reason", redactTestPlainRef,
			"fetch https://user:x@example.com/r failed", "fetch https://(redacted URL) failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolverReasonForLog(tc.ref, tc.reason); got != tc.want {
				t.Errorf("ResolverReasonForLog(%q, %q) = %q, want %q", tc.ref, tc.reason, got, tc.want)
			}
		})
	}
}

func TestSkillSchemeRest_ASCIIOnly(t *testing.T) {
	if _, ok := skillSchemeRest("SKILL://scope/name"); !ok {
		t.Error("upper-case ASCII scheme not recognised")
	}
	if _, ok := skillSchemeRest("s\u212aill://scope/name"); ok {
		t.Error("a Kelvin sign matched the ASCII k of skill://")
	}
	if got := RedactSkillRef("s\u212aill://scope/name"); got != RedactedURL {
		t.Errorf("RedactSkillRef(Kelvin-sign scheme) = %q, want %q", got, RedactedURL)
	}
	if got := RedactSkillRef("gh://u%3Apw-1@o/r/s"); got != RedactedURL {
		t.Errorf("RedactSkillRef(percent-encoded colon) = %q, want %q", got, RedactedURL)
	}
}
