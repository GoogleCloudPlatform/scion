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

func TestSlugify(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "basic",
			input:    "Hello World",
			expected: "hello-world",
		},
		{
			name:     "special characters",
			input:    "Hello!@#$%^&*()_+World",
			expected: "hello-world",
		},
		{
			name:     "unicode",
			input:    "Héllö Wörld",
			expected: "hello-world",
		},
		{
			name:     "leading and trailing dashes",
			input:    "-Hello World-",
			expected: "hello-world",
		},
		{
			name:     "multiple dashes",
			input:    "Hello---World",
			expected: "hello-world",
		},
		{
			name:     "long string",
			input:    strings.Repeat("a", 100),
			expected: strings.Repeat("a", MaxSlugLength),
		},
		{
			name:     "numbers",
			input:    "Agent 007",
			expected: "agent-007",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Slugify(tt.input); got != tt.expected {
				t.Errorf("Slugify(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestValidateAgentName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "valid",
			input:   "My Agent",
			wantErr: false,
		},
		{
			name:    "invalid empty",
			input:   "",
			wantErr: true,
		},
		{
			name:    "invalid special chars",
			input:   "!@#$%",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateAgentName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateAgentName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestSlugifyWithSuffix(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		suffix   string
		expected string
	}{
		{
			name:     "basic",
			input:    "Hello World",
			suffix:   "123",
			expected: "hello-world-123",
		},
		{
			name:     "no suffix",
			input:    "Hello World",
			suffix:   "",
			expected: "hello-world",
		},
		{
			name:     "truncation",
			input:    strings.Repeat("a", MaxSlugLength),
			suffix:   "abc",
			expected: strings.Repeat("a", MaxSlugLength-len("abc")-1) + "-abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SlugifyWithSuffix(tt.input, tt.suffix); got != tt.expected {
				t.Errorf("SlugifyWithSuffix(%q, %q) = %q, want %q", tt.input, tt.suffix, got, tt.expected)
			}
		})
	}
}

func TestDisplayNameWithSerial(t *testing.T) {
	tests := []struct {
		name     string
		baseName string
		slug     string
		baseSlug string
		expected string
	}{
		{
			name:     "no serial",
			baseName: "My Agent",
			slug:     "my-agent",
			baseSlug: "my-agent",
			expected: "My Agent",
		},
		{
			name:     "with serial",
			baseName: "My Agent",
			slug:     "my-agent-2",
			baseSlug: "my-agent",
			expected: "My Agent (2)",
		},
		{
			name:     "unrelated slug",
			baseName: "My Agent",
			slug:     "different-slug",
			baseSlug: "my-agent",
			expected: "My Agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DisplayNameWithSerial(tt.baseName, tt.slug, tt.baseSlug); got != tt.expected {
				t.Errorf("DisplayNameWithSerial(%q, %q, %q) = %q, want %q", tt.baseName, tt.slug, tt.baseSlug, got, tt.expected)
			}
		})
	}
}

func TestMakeProjectID(t *testing.T) {
	tests := []struct {
		name        string
		id          string
		projectName string
		wantPrefix  string
		wantSuffix  string
	}{
		{
			name:        "with ID",
			id:          "abc123",
			projectName: "My Project",
			wantPrefix:  "abc123",
			wantSuffix:  "my-project",
		},
		{
			name:        "without ID",
			id:          "",
			projectName: "Test Project",
			wantSuffix:  "test-project",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MakeProjectID(tt.id, tt.projectName)
			if tt.id != "" {
				expected := tt.wantPrefix + ProjectIDSeparator + tt.wantSuffix
				if result != expected {
					t.Errorf("MakeProjectID(%q, %q) = %q, want %q", tt.id, tt.projectName, result, expected)
				}
			} else {
				// Should have generated a UUID
				if !strings.HasSuffix(result, ProjectIDSeparator+tt.wantSuffix) {
					t.Errorf("MakeProjectID(%q, %q) = %q, want suffix %q", tt.id, tt.projectName, result, tt.wantSuffix)
				}
			}
		})
	}
}

func TestParseProjectID(t *testing.T) {
	tests := []struct {
		name      string
		projectID string
		wantID    string
		wantSlug  string
		wantOK    bool
	}{
		{
			name:      "hosted format",
			projectID: "abc123__my-project",
			wantID:    "abc123",
			wantSlug:  "my-project",
			wantOK:    true,
		},
		{
			name:      "simple format",
			projectID: "my-local-project",
			wantID:    "",
			wantSlug:  "my-local-project",
			wantOK:    false,
		},
		{
			name:      "multiple separators",
			projectID: "abc123__my__project",
			wantID:    "abc123",
			wantSlug:  "my__project",
			wantOK:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, slug, ok := ParseProjectID(tt.projectID)
			if id != tt.wantID || slug != tt.wantSlug || ok != tt.wantOK {
				t.Errorf("ParseProjectID(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.projectID, id, slug, ok, tt.wantID, tt.wantSlug, tt.wantOK)
			}
		})
	}
}

func TestIsHostedProjectID(t *testing.T) {
	tests := []struct {
		projectID string
		want      bool
	}{
		{"abc__def", true},
		{"abc", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.projectID, func(t *testing.T) {
			if got := IsHostedProjectID(tt.projectID); got != tt.want {
				t.Errorf("IsHostedProjectID(%q) = %v, want %v", tt.projectID, got, tt.want)
			}
		})
	}
}
