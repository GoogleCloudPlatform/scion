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
			name:     "simple lowercase",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "mixed case",
			input:    "Hello World",
			expected: "hello-world",
		},
		{
			name:     "special characters",
			input:    "Hello! @World#",
			expected: "hello-world",
		},
		{
			name:     "unicode accents",
			input:    "Caf\u00e9",
			expected: "cafe",
		},
		{
			name:     "numbers",
			input:    "agent-123",
			expected: "agent-123",
		},
		{
			name:     "leading/trailing spaces",
			input:    "  hello world  ",
			expected: "hello-world",
		},
		{
			name:     "multiple dashes",
			input:    "hello---world",
			expected: "hello-world",
		},
		{
			name:     "underscores become dashes",
			input:    "hello_world",
			expected: "hello-world",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "only special chars",
			input:    "!@#$%",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Slugify(tt.input)
			if result != tt.expected {
				t.Errorf("Slugify(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestSlugifyLengthLimit(t *testing.T) {
	longInput := strings.Repeat("a", 100)
	result := Slugify(longInput)

	if len(result) > MaxSlugLength {
		t.Errorf("Slugify produced slug of length %d, want <= %d", len(result), MaxSlugLength)
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
			name:     "simple with suffix",
			input:    "my-agent",
			suffix:   "abc123",
			expected: "my-agent-abc123",
		},
		{
			name:     "empty suffix",
			input:    "my-agent",
			suffix:   "",
			expected: "my-agent",
		},
		{
			name:     "long input truncated",
			input:    strings.Repeat("a", 100),
			suffix:   "xyz",
			expected: strings.Repeat("a", MaxSlugLength-4) + "-xyz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SlugifyWithSuffix(tt.input, tt.suffix)
			if result != tt.expected {
				t.Errorf("SlugifyWithSuffix(%q, %q) = %q, want %q", tt.input, tt.suffix, result, tt.expected)
			}
			if len(result) > MaxSlugLength {
				t.Errorf("SlugifyWithSuffix produced slug of length %d, want <= %d", len(result), MaxSlugLength)
			}
		})
	}
}

func TestNewUUID(t *testing.T) {
	id := NewUUID()
	if len(id) != 36 {
		t.Errorf("NewUUID() = %q, want 36 char UUID", id)
	}

	// Should have 4 dashes at correct positions
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Errorf("NewUUID() = %q, not valid UUID format", id)
	}
}

func TestNewShortID(t *testing.T) {
	id := NewShortID()
	if len(id) != 8 {
		t.Errorf("NewShortID() = %q, want 8 chars", id)
	}
}

func TestMakeGroveID(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		groveName string
		wantPrefix string
		wantSuffix string
	}{
		{
			name:       "with provided ID",
			id:         "abc123",
			groveName:  "My Project",
			wantPrefix: "abc123",
			wantSuffix: "my-project",
		},
		{
			name:       "empty ID generates new",
			id:         "",
			groveName:  "Test Grove",
			wantPrefix: "", // Will be a UUID
			wantSuffix: "test-grove",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MakeGroveID(tt.id, tt.groveName)

			if tt.wantPrefix != "" {
				expected := tt.wantPrefix + GroveIDSeparator + tt.wantSuffix
				if result != expected {
					t.Errorf("MakeGroveID(%q, %q) = %q, want %q", tt.id, tt.groveName, result, expected)
				}
			} else {
				// Should end with the slug
				if !strings.HasSuffix(result, GroveIDSeparator+tt.wantSuffix) {
					t.Errorf("MakeGroveID(%q, %q) = %q, want suffix %q", tt.id, tt.groveName, result, tt.wantSuffix)
				}
			}
		})
	}
}

func TestParseGroveID(t *testing.T) {
	tests := []struct {
		name     string
		groveID  string
		wantID   string
		wantSlug string
		wantOK   bool
	}{
		{
			name:     "hosted format",
			groveID:  "abc123__my-project",
			wantID:   "abc123",
			wantSlug: "my-project",
			wantOK:   true,
		},
		{
			name:     "local format",
			groveID:  "my-local-project",
			wantID:   "",
			wantSlug: "my-local-project",
			wantOK:   false,
		},
		{
			name:     "multiple separators",
			groveID:  "abc123__my__project",
			wantID:   "abc123",
			wantSlug: "my__project",
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, slug, ok := ParseGroveID(tt.groveID)
			if id != tt.wantID || slug != tt.wantSlug || ok != tt.wantOK {
				t.Errorf("ParseGroveID(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.groveID, id, slug, ok, tt.wantID, tt.wantSlug, tt.wantOK)
			}
		})
	}
}

func TestIsHostedGroveID(t *testing.T) {
	tests := []struct {
		groveID string
		want    bool
	}{
		{"abc123__my-project", true},
		{"my-local-project", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.groveID, func(t *testing.T) {
			if got := IsHostedGroveID(tt.groveID); got != tt.want {
				t.Errorf("IsHostedGroveID(%q) = %v, want %v", tt.groveID, got, tt.want)
			}
		})
	}
}
