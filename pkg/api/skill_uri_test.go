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

func TestValidateSkillName(t *testing.T) {
	valid := []string{
		"a",
		"scion",
		"security-audit",
		"my-skill-123",
		"a1",
		"abc",
		"a-b",
	}
	for _, name := range valid {
		if err := ValidateSkillName(name); err != nil {
			t.Errorf("ValidateSkillName(%q) unexpected error: %v", name, err)
		}
	}

	invalid := []struct {
		name string
		desc string
	}{
		{"", "empty"},
		{"-leading", "leading hyphen"},
		{"trailing-", "trailing hyphen"},
		{"My_Skill", "uppercase and underscore"},
		{"UPPER", "all uppercase"},
		{"has space", "contains space"},
		{"has.dot", "contains dot"},
		{"has/slash", "contains slash"},
		{strings.Repeat("a", 65), "too long"},
	}
	for _, tc := range invalid {
		if err := ValidateSkillName(tc.name); err == nil {
			t.Errorf("ValidateSkillName(%q) [%s] expected error, got nil", tc.name, tc.desc)
		}
	}
}

func TestParseSkillURI_ValidForms(t *testing.T) {
	tests := []struct {
		input    string
		registry string
		scope    string
		scopeID  string
		name     string
		version  string
	}{
		// Full canonical
		{"skill://scion/core/scion@^1.0", "scion", "core", "", "scion", "^1.0"},
		// No registry (empty → default scion)
		{"skill:///core/scion@^1.0", "scion", "core", "", "scion", "^1.0"},
		// No version → latest
		{"skill://scion/core/scion", "scion", "core", "", "scion", "latest"},
		// With scope ID
		{"skill://scion/project/my-proj/my-skill@1.0.0", "scion", "project", "my-proj", "my-skill", "1.0.0"},
		// User scope
		{"skill://scion/user/alice/my-skill@latest", "scion", "user", "alice", "my-skill", "latest"},
		// Global scope
		{"skill://scion/global/shared-tool@~1.2", "scion", "global", "", "shared-tool", "~1.2"},
		// No scope (search order)
		{"skill://scion/my-skill@latest", "scion", "", "", "my-skill", "latest"},
		// Registry alias: project
		{"skill://project/my-skill@latest", "scion", "project", "", "my-skill", "latest"},
		// Registry alias: user
		{"skill://user/my-skill@1.0", "scion", "user", "", "my-skill", "1.0"},
		// Registry alias: project with scope ID
		{"skill://project/my-proj-id/my-skill@1.0", "scion", "project", "my-proj-id", "my-skill", "1.0"},
		// Bare name
		{"scion", "scion", "", "", "scion", "latest"},
		{"security-audit", "scion", "", "", "security-audit", "latest"},
		{"my-skill-123", "scion", "", "", "my-skill-123", "latest"},
		// Version: exact semver
		{"skill://scion/core/scion@1.2.3", "scion", "core", "", "scion", "1.2.3"},
		// Version: caret
		{"skill://scion/core/scion@^1.0", "scion", "core", "", "scion", "^1.0"},
		// Version: tilde
		{"skill://scion/core/scion@~1.2", "scion", "core", "", "scion", "~1.2"},
		// Version: sha256
		{"skill://scion/core/scion@sha256:abc123", "scion", "core", "", "scion", "sha256:abc123"},
		// Version: v prefix stripped
		{"skill://scion/core/scion@v1.2.3", "scion", "core", "", "scion", "1.2.3"},
		// Custom registry hostname
		{"skill://registry.example.com/core/my-skill@1.0", "registry.example.com", "core", "", "my-skill", "1.0"},
		// No scope, no version
		{"skill://scion/my-skill", "scion", "", "", "my-skill", "latest"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseSkillURI(tc.input)
			if err != nil {
				t.Fatalf("ParseSkillURI(%q) unexpected error: %v", tc.input, err)
			}
			if got.Registry != tc.registry {
				t.Errorf("Registry = %q, want %q", got.Registry, tc.registry)
			}
			if got.Scope != tc.scope {
				t.Errorf("Scope = %q, want %q", got.Scope, tc.scope)
			}
			if got.ScopeID != tc.scopeID {
				t.Errorf("ScopeID = %q, want %q", got.ScopeID, tc.scopeID)
			}
			if got.Name != tc.name {
				t.Errorf("Name = %q, want %q", got.Name, tc.name)
			}
			if got.Version != tc.version {
				t.Errorf("Version = %q, want %q", got.Version, tc.version)
			}
			if got.Raw != tc.input {
				t.Errorf("Raw = %q, want %q", got.Raw, tc.input)
			}
		})
	}
}

func TestParseSkillURI_InvalidForms(t *testing.T) {
	tests := []struct {
		input string
		desc  string
	}{
		{"", "empty URI"},
		{"skill://scion/core/@^1.0", "empty name"},
		{"skill://scion/core/My_Skill@1.0", "name not kebab-case"},
		{"skill://scion/invalid-scope/team/name@1.0", "invalid-scope is not a valid scope"},
		{"skill://scion/core/name@", "empty version after @"},
		{"skill://scion/unknown-scope/name@1.0", "unrecognized scope keyword"},
		{"../traversal", "path traversal in bare name"},
		{"path/name", "slash in bare name"},
		{"http://example.com/skill", "wrong scheme"},
		{"skill://scion/a/b/c/d@1.0", "too many segments"},
		{"UPPER", "uppercase bare name"},
		{"-leading-hyphen", "leading hyphen in bare name"},
		{"skill://scion/core/" + strings.Repeat("a", 65) + "@1.0", "name too long"},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := ParseSkillURI(tc.input)
			if err == nil {
				t.Errorf("ParseSkillURI(%q) expected error for %s, got nil", tc.input, tc.desc)
			}
		})
	}
}

func TestSkillURIScheme(t *testing.T) {
	tests := []struct {
		uri    string
		scheme string
	}{
		{"skill://scion/core/my-skill", "skill"},
		{"gh://owner/repo/name", "gh"},
		{"gcp-skill://alias/ID", "gcp-skill"},
		{"https://github.com/owner/repo/tree/main/skills/s", "https"},
		{"my-skill", "skill"},
	}
	for _, tt := range tests {
		t.Run(tt.uri, func(t *testing.T) {
			if got := SkillURIScheme(tt.uri); got != tt.scheme {
				t.Errorf("SkillURIScheme(%q) = %q, want %q", tt.uri, got, tt.scheme)
			}
		})
	}
}
