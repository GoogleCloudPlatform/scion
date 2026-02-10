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
	"os"
	"path/filepath"
	"testing"
)

type TestConfig struct {
	Foo string `json:"foo"`
	Bar int    `json:"bar"`
}

func TestUnmarshalJSONC(t *testing.T) {
	jsonc := []byte(`{
		"foo": "baz", // comment
		"bar": 123 /* block
		comment */
	}`)

	var cfg TestConfig
	if err := UnmarshalJSONC(jsonc, &cfg); err != nil {
		t.Fatalf("UnmarshalJSONC failed: %v", err)
	}

	if cfg.Foo != "baz" {
		t.Errorf("expected foo=baz, got %s", cfg.Foo)
	}
	if cfg.Bar != 123 {
		t.Errorf("expected bar=123, got %d", cfg.Bar)
	}
}

func TestReadJSONC(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.jsonc")
	content := []byte(`{
		// header comment
		"foo": "file",
		"bar": 456
	}`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	var cfg TestConfig
	if err := ReadJSONC(path, &cfg); err != nil {
		t.Fatalf("ReadJSONC failed: %v", err)
	}

	if cfg.Foo != "file" {
		t.Errorf("expected foo=file, got %s", cfg.Foo)
	}
	if cfg.Bar != 456 {
		t.Errorf("expected bar=456, got %d", cfg.Bar)
	}
}

func TestStripComments(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no comments",
			input:    `{"foo": "bar"}`,
			expected: `{"foo": "bar"}`,
		},
		{
			name:     "line comment",
			input:    `{"foo": "bar"} // comment`,
			expected: `{"foo": "bar"} `,
		},
		{
			name:     "block comment",
			input:    `{"foo": /* comment */ "bar"}`,
			expected: `{"foo":  "bar"}`,
		},
		{
			name:     "multiline block comment",
			input:    "{\n/* multi\nline */\n\"foo\": 1}",
			expected: "{\n\n\"foo\": 1}",
		},
		{
			name:     "comment-like in string preserved",
			input:    `{"url": "http://example.com"}`,
			expected: `{"url": "http://example.com"}`,
		},
		{
			name:     "block comment marker in string preserved",
			input:    `{"pattern": "/* not a comment */"}`,
			expected: `{"pattern": "/* not a comment */"}`,
		},
		{
			name:     "escaped quotes in string",
			input:    `{"msg": "say \"hello\""}`,
			expected: `{"msg": "say \"hello\""}`,
		},
		{
			name:     "comment after escaped quote",
			input:    `{"msg": "test\""} // comment`,
			expected: `{"msg": "test\""} `,
		},
		{
			name:     "empty input",
			input:    ``,
			expected: ``,
		},
		{
			name:     "only comment",
			input:    `// just a comment`,
			expected: ``,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := string(StripComments([]byte(tt.input)))
			if result != tt.expected {
				t.Errorf("StripComments(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestStripTrailingCommas(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no trailing comma",
			input:    `{"foo": 1}`,
			expected: `{"foo": 1}`,
		},
		{
			name:     "trailing comma in object",
			input:    `{"foo": 1,}`,
			expected: `{"foo": 1}`,
		},
		{
			name:     "trailing comma in array",
			input:    `[1, 2, 3,]`,
			expected: `[1, 2, 3]`,
		},
		{
			name:     "trailing comma with whitespace",
			input:    `{"foo": 1,  }`,
			expected: `{"foo": 1  }`,
		},
		{
			name:     "trailing comma with newline",
			input:    "{\n\"foo\": 1,\n}",
			expected: "{\n\"foo\": 1\n}",
		},
		{
			name:     "comma in string preserved",
			input:    `{"msg": "a,}"}`,
			expected: `{"msg": "a,}"}`,
		},
		{
			name:     "nested trailing commas",
			input:    `{"arr": [1,], "obj": {"a": 1,},}`,
			expected: `{"arr": [1], "obj": {"a": 1}}`,
		},
		{
			name:     "empty input",
			input:    ``,
			expected: ``,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := string(StripTrailingCommas([]byte(tt.input)))
			if result != tt.expected {
				t.Errorf("StripTrailingCommas(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestUnmarshalJSONC_TrailingCommas(t *testing.T) {
	jsonc := []byte(`{
		"foo": "test",
		"bar": 999,
	}`)

	var cfg TestConfig
	if err := UnmarshalJSONC(jsonc, &cfg); err != nil {
		t.Fatalf("UnmarshalJSONC with trailing comma failed: %v", err)
	}

	if cfg.Foo != "test" {
		t.Errorf("expected foo=test, got %s", cfg.Foo)
	}
	if cfg.Bar != 999 {
		t.Errorf("expected bar=999, got %d", cfg.Bar)
	}
}

func TestUnmarshalJSONC_InvalidJSON(t *testing.T) {
	jsonc := []byte(`{"foo": }`) // missing value

	var cfg TestConfig
	if err := UnmarshalJSONC(jsonc, &cfg); err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestReadJSONC_FileNotFound(t *testing.T) {
	var cfg TestConfig
	err := ReadJSONC("/nonexistent/path/file.json", &cfg)
	if err == nil {
		t.Error("expected error for nonexistent file, got nil")
	}
}
