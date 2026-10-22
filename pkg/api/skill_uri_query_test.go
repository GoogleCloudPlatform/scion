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

package api

import (
	"strings"
	"testing"
)

func TestSkillURITokenParam(t *testing.T) {
	tests := []struct {
		name      string
		rawQuery  string
		wantValue string
		wantOK    bool
	}{
		{name: "empty", rawQuery: "", wantValue: "", wantOK: false},
		{name: "token only", rawQuery: "token=SKILLS_TOKEN", wantValue: "SKILLS_TOKEN", wantOK: true},
		{name: "unknown parameter ignored", rawQuery: "access_token=made-up-value-1", wantValue: "", wantOK: false},
		{name: "unknown after token ignored", rawQuery: "token=SKILLS_TOKEN&access_token=made-up-value-1", wantValue: "SKILLS_TOKEN", wantOK: true},
		{name: "unknown before token ignored", rawQuery: "access_token=made-up-value-1&token=SKILLS_TOKEN", wantValue: "SKILLS_TOKEN", wantOK: true},
		{name: "token between unknowns", rawQuery: "a=1&token=SKILLS_TOKEN&b=2", wantValue: "SKILLS_TOKEN", wantOK: true},
		{name: "second token-like name ignored", rawQuery: "token=SKILLS_TOKEN&github_token=made-up-value-2", wantValue: "SKILLS_TOKEN", wantOK: true},
		{name: "name with token prefix ignored", rawQuery: "tokens=made-up-value-3", wantValue: "", wantOK: false},
		{name: "name with token suffix ignored", rawQuery: "xtoken=made-up-value-3", wantValue: "", wantOK: false},
		{name: "repeated token keeps first", rawQuery: "token=FIRST&token=SECOND", wantValue: "FIRST", wantOK: true},
		{name: "repeated token keeps first across unknowns", rawQuery: "x=1&token=FIRST&y=2&token=SECOND", wantValue: "FIRST", wantOK: true},
		{name: "capitalised name ignored", rawQuery: "Token=SKILLS_TOKEN", wantValue: "", wantOK: false},
		{name: "upper-case name ignored", rawQuery: "TOKEN=SKILLS_TOKEN", wantValue: "", wantOK: false},
		{name: "mixed case before exact keeps exact", rawQuery: "TOKEN=OTHER&token=SKILLS_TOKEN", wantValue: "SKILLS_TOKEN", wantOK: true},
		{name: "percent-encoded name ignored", rawQuery: "%74oken=SKILLS_TOKEN", wantValue: "", wantOK: false},
		{name: "token without equals ignored", rawQuery: "token", wantValue: "", wantOK: false},
		{name: "token without equals then token", rawQuery: "token&token=SKILLS_TOKEN", wantValue: "SKILLS_TOKEN", wantOK: true},
		{name: "empty parameters ignored", rawQuery: "&&token=SKILLS_TOKEN&", wantValue: "SKILLS_TOKEN", wantOK: true},
		{name: "unknown only, several", rawQuery: "access_token=made-up-value-1&client_secret=made-up-value-4", wantValue: "", wantOK: false},
		// The value is returned byte for byte; IsValidSkillTokenSecretName checks it.
		{name: "empty value", rawQuery: "token=", wantValue: "", wantOK: true},
		{name: "invalid value verbatim", rawQuery: "token=made-up-value-5", wantValue: "made-up-value-5", wantOK: true},
		{name: "encoded value verbatim", rawQuery: "token=A%2BB", wantValue: "A%2BB", wantOK: true},
		{name: "second equals stays in value", rawQuery: "token=A=B", wantValue: "A=B", wantOK: true},
		{name: "semicolon stays in value", rawQuery: "token=GOOD;x=made-up-value-6", wantValue: "GOOD;x=made-up-value-6", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, ok := SkillURITokenParam(tt.rawQuery)
			if v != tt.wantValue || ok != tt.wantOK {
				t.Errorf("SkillURITokenParam(%q) = (%q, %v), want (%q, %v)", tt.rawQuery, v, ok, tt.wantValue, tt.wantOK)
			}
		})
	}
}

func TestIsValidSkillTokenSecretName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"SKILLS_TOKEN", true},
		{"A", true},
		{"A1_B2", true},
		{"", false},
		{"_A", false},
		{"1A", false},
		{"skills_token", false},
		{"Skills_Token", false},
		{"A-B", false},
		{"A B", false},
		{"A%2BB", false},
		{"GOOD;x=made-up-value-6", false},
		{"GOOD&access_token=made-up-value-1", false},
	}
	for _, tt := range tests {
		if got := IsValidSkillTokenSecretName(tt.name); got != tt.want {
			t.Errorf("IsValidSkillTokenSecretName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSkillURIWithoutQuery(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"https://github.com/acme/repo/tree/main/skills/alpha-skill", "https://github.com/acme/repo/tree/main/skills/alpha-skill"},
		{"https://github.com/acme/repo/tree/main/skills/alpha-skill?token=SKILLS_TOKEN", "https://github.com/acme/repo/tree/main/skills/alpha-skill"},
		{"https://github.com/acme/repo/tree/main/skills/alpha-skill?access_token=made-up-value-1", "https://github.com/acme/repo/tree/main/skills/alpha-skill"},
		{"gh://acme/repo/alpha-skill@main?token=made-up-value-5", "gh://acme/repo/alpha-skill@main"},
		{"gh://acme/repo/alpha-skill?a=1?b=made-up-value-2", "gh://acme/repo/alpha-skill"},
		{"?made-up-value-3", ""},
		{"https://github.com/acme/repo/tree/main/skills?", "https://github.com/acme/repo/tree/main/skills"},
	}
	for _, tt := range tests {
		if got := SkillURIWithoutQuery(tt.in); got != tt.want {
			t.Errorf("SkillURIWithoutQuery(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestNormalizeSkillURI_ErrorsOmitQuery checks the whole error text for every
// query-related rejection: it names the URI without its query and never
// contains the token value or any other query content.
func TestNormalizeSkillURI_ErrorsOmitQuery(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "github unsupported parameter",
			input:   "https://github.com/acme/repo/tree/main/skills/alpha-skill?access_token=made-up-value-1",
			wantErr: `invalid GitHub URL "https://github.com/acme/repo/tree/main/skills/alpha-skill": unsupported query parameter; only ?token=SECRET_NAME is supported`,
		},
		{
			name:    "github token plus unknown",
			input:   "https://github.com/acme/repo/tree/main/skills/alpha-skill?token=GOOD&access_token=made-up-value-1",
			wantErr: `invalid GitHub URL "https://github.com/acme/repo/tree/main/skills/alpha-skill": the token secret name must match [A-Z][A-Z0-9_]* (uppercase env-var style)`,
		},
		{
			name:    "github invalid token value",
			input:   "https://github.com/acme/repo/tree/main/skills/alpha-skill?token=made-up-value-5",
			wantErr: `invalid GitHub URL "https://github.com/acme/repo/tree/main/skills/alpha-skill": the token secret name must match [A-Z][A-Z0-9_]* (uppercase env-var style)`,
		},
		{
			name:    "github empty token value",
			input:   "https://github.com/acme/repo/tree/main/skills/alpha-skill?token=",
			wantErr: `invalid GitHub URL "https://github.com/acme/repo/tree/main/skills/alpha-skill": empty ?token= value`,
		},
		{
			name:    "github bad path with token",
			input:   "https://github.com/acme/repo/tree/main?token=SKILLS_TOKEN",
			wantErr: `invalid GitHub URL "https://github.com/acme/repo/tree/main": missing skill path after ref; example: https://github.com/owner/repo/tree/main/skills/my-skill`,
		},
		{
			name:    "gh unsupported parameter",
			input:   "gh://acme/repo/alpha-skill@main?access_token=made-up-value-1",
			wantErr: `invalid gh:// URI "gh://acme/repo/alpha-skill@main": unsupported query parameter; only ?token=SECRET_NAME is supported`,
		},
		{
			name:    "gh invalid token value",
			input:   "gh://acme/repo/alpha-skill@main?token=made-up-value-5",
			wantErr: `invalid gh:// URI "gh://acme/repo/alpha-skill@main": the token secret name must match [A-Z][A-Z0-9_]* (uppercase env-var style)`,
		},
		{
			name:    "gh empty token value",
			input:   "gh://acme/repo/alpha-skill@main?token=",
			wantErr: `invalid gh:// URI "gh://acme/repo/alpha-skill@main": empty ?token= value`,
		},
		{
			name:    "gh bad shape with token",
			input:   "gh://acme/alpha-skill?token=SKILLS_TOKEN",
			wantErr: `invalid gh:// URI "gh://acme/alpha-skill": expected gh://owner/repo/skill-name[@ref][?token=SECRET_NAME]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeSkillURI(tt.input)
			if err == nil {
				t.Fatalf("NormalizeSkillURI(%q) succeeded, want an error", tt.input)
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q\nwant    %q", err.Error(), tt.wantErr)
			}
			if strings.Contains(err.Error(), "made-up-value") {
				t.Errorf("error contains the query value: %q", err.Error())
			}
		})
	}
}

// TestParseSkillURI_ErrorsOmitQuery checks the whole error text for skill://
// URIs and bare names that carry a '?': the input is quoted only up to the
// '?', and nothing after it (name, scope or segment detail included) appears.
// Inputs without a '?' keep their detailed errors.
func TestParseSkillURI_ErrorsOmitQuery(t *testing.T) {
	const omitted = ": not a valid skill:// URI or bare skill name (the text from '?' on is not shown)"
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "single name with query",
			input:   "skill://my-skill?token=made-up-value-11",
			wantErr: `invalid skill URI "skill://my-skill"` + omitted,
		},
		{
			name:    "bare name with query",
			input:   "my-skill?token=made-up-value-12",
			wantErr: `invalid skill URI "my-skill"` + omitted,
		},
		{
			name:    "query inside the scope segment",
			input:   "skill://scion/co?x=made-up-value-13/re/name",
			wantErr: `invalid skill URI "skill://scion/co"` + omitted,
		},
		{
			name:    "alias with query in the name",
			input:   "skill://project/my-skill?made-up-value-14",
			wantErr: `invalid skill URI "skill://project/my-skill"` + omitted,
		},
		{
			name:    "bare name with path and query",
			input:   "a/b?made-up-value-15",
			wantErr: `invalid skill URI "a/b"` + omitted,
		},
		{
			name:    "skill:// version before the query",
			input:   "skill://my-skill@1.0?token=made-up-value-16",
			wantErr: `invalid skill URI "skill://my-skill@1.0"` + omitted,
		},
		{
			name:    "scoped skill:// version before the query",
			input:   "skill://scion/core/my-skill@^1.0?token=made-up-value-17",
			wantErr: `invalid skill URI "skill://scion/core/my-skill@^1.0"` + omitted,
		},
		{
			name:    "bare name version before the query",
			input:   "my-skill@1.0?token=made-up-value-18",
			wantErr: `invalid skill URI "my-skill@1.0"` + omitted,
		},
		{
			name:    "skill:// with a bare trailing question mark",
			input:   "skill://my-skill?",
			wantErr: `invalid skill URI "skill://my-skill"` + omitted,
		},
		{
			name:    "no query keeps the detailed error",
			input:   "skill://scion/bogus/my-skill",
			wantErr: `invalid skill URI "skill://scion/bogus/my-skill": unrecognized scope "bogus" (must be core, global, project, or user)`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSkillURI(tt.input)
			if err == nil {
				t.Fatalf("ParseSkillURI(%q) succeeded, want an error", tt.input)
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q\nwant    %q", err.Error(), tt.wantErr)
			}
			if strings.Contains(err.Error(), "made-up-value") {
				t.Errorf("error contains the query value: %q", err.Error())
			}
			// NormalizeSkillURI reaches ParseSkillURI for skill:// and bare
			// names and returns its error unchanged.
			if _, nerr := NormalizeSkillURI(tt.input); nerr == nil || nerr.Error() != tt.wantErr {
				t.Errorf("NormalizeSkillURI(%q) error = %v, want %q", tt.input, nerr, tt.wantErr)
			}
		})
	}
}

// TestParseSkillURI_WithoutQueryUnchanged checks that inputs without a '?'
// parse exactly as before the query check was added.
func TestParseSkillURI_WithoutQueryUnchanged(t *testing.T) {
	tests := []struct {
		input string
		want  SkillURI
	}{
		{"skill://my-skill@1.0", SkillURI{Raw: "skill://my-skill@1.0", Registry: "scion", Name: "my-skill", Version: "1.0"}},
		{"skill://scion/core/my-skill@^1.0", SkillURI{Raw: "skill://scion/core/my-skill@^1.0", Registry: "scion", Scope: "core", Name: "my-skill", Version: "^1.0"}},
		{"my-skill", SkillURI{Raw: "my-skill", Registry: "scion", Name: "my-skill", Version: "latest"}},
	}
	for _, tt := range tests {
		got, err := ParseSkillURI(tt.input)
		if err != nil {
			t.Errorf("ParseSkillURI(%q): %v", tt.input, err)
			continue
		}
		if *got != tt.want {
			t.Errorf("ParseSkillURI(%q) = %+v, want %+v", tt.input, *got, tt.want)
		}
	}
}
