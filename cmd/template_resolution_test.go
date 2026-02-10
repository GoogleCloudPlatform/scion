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

package cmd

import (
	"testing"
)

func TestParseTemplateScope(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expectedScope string
		expectedName  string
	}{
		{
			name:          "no scope prefix",
			input:         "custom-claude",
			expectedScope: "",
			expectedName:  "custom-claude",
		},
		{
			name:          "global scope prefix",
			input:         "global:claude",
			expectedScope: "global",
			expectedName:  "claude",
		},
		{
			name:          "grove scope prefix",
			input:         "grove:custom-template",
			expectedScope: "grove",
			expectedName:  "custom-template",
		},
		{
			name:          "user scope prefix",
			input:         "user:my-template",
			expectedScope: "user",
			expectedName:  "my-template",
		},
		{
			name:          "unknown prefix treated as name",
			input:         "unknown:template",
			expectedScope: "",
			expectedName:  "unknown:template",
		},
		{
			name:          "multiple colons",
			input:         "grove:my:template",
			expectedScope: "grove",
			expectedName:  "my:template",
		},
		{
			name:          "empty string",
			input:         "",
			expectedScope: "",
			expectedName:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope, name := parseTemplateScope(tt.input)
			if scope != tt.expectedScope {
				t.Errorf("parseTemplateScope(%q) scope = %q, want %q", tt.input, scope, tt.expectedScope)
			}
			if name != tt.expectedName {
				t.Errorf("parseTemplateScope(%q) name = %q, want %q", tt.input, name, tt.expectedName)
			}
		})
	}
}

func TestTruncateHash(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "short hash unchanged",
			input:    "sha256:abc123",
			expected: "sha256:abc123",
		},
		{
			name:     "exact 20 chars unchanged",
			input:    "12345678901234567890",
			expected: "12345678901234567890",
		},
		{
			name:     "long hash truncated",
			input:    "sha256:abcdef0123456789abcdef0123456789",
			expected: "sha256:abcdef0123456...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateHash(tt.input)
			if result != tt.expected {
				t.Errorf("truncateHash(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestFormatTemplateNotFoundError(t *testing.T) {
	// Test that the error message is formatted correctly
	err := formatTemplateNotFoundError("test-template", "/some/grove/path")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	errMsg := err.Error()

	// Check that key information is present
	if !contains(errMsg, "test-template") {
		t.Error("error message should contain template name")
	}
	if !contains(errMsg, "not found") {
		t.Error("error message should indicate template not found")
	}
	if !contains(errMsg, "scion template sync") {
		t.Error("error message should provide guidance on how to create template")
	}
}

func TestFormatTemplateNotFoundErrorNoGrove(t *testing.T) {
	// Test with empty grove path
	err := formatTemplateNotFoundError("test-template", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	errMsg := err.Error()

	// Should not have grove scope line when no grove path
	if contains(errMsg, "grove scope") {
		t.Error("error message should not mention grove scope when no grove path")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
