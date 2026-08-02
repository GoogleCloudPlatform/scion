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

package gcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseProjectID(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "valid SA key JSON",
			data: []byte(`{"type":"service_account","project_id":"my-project-123","private_key_id":"abc"}`),
			want: "my-project-123",
		},
		{
			name: "empty project_id",
			data: []byte(`{"type":"service_account","project_id":"","private_key_id":"abc"}`),
			want: "",
		},
		{
			name: "missing project_id field",
			data: []byte(`{"type":"service_account","private_key_id":"abc"}`),
			want: "",
		},
		{
			name: "invalid JSON",
			data: []byte(`not json at all`),
			want: "",
		},
		{
			name: "empty input",
			data: []byte{},
			want: "",
		},
		{
			name: "nil input",
			data: nil,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseProjectID(tt.data)
			if got != tt.want {
				t.Errorf("ParseProjectID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractProjectID(t *testing.T) {
	t.Run("valid credentials file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "sa-key.json")
		data := []byte(`{"type":"service_account","project_id":"test-project","private_key_id":"xyz"}`)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		got := ExtractProjectID(path)
		if got != "test-project" {
			t.Errorf("ExtractProjectID() = %q, want %q", got, "test-project")
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		got := ExtractProjectID("/nonexistent/path/to/creds.json")
		if got != "" {
			t.Errorf("ExtractProjectID() = %q, want empty", got)
		}
	})

	t.Run("invalid JSON file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
			t.Fatal(err)
		}
		got := ExtractProjectID(path)
		if got != "" {
			t.Errorf("ExtractProjectID() = %q, want empty", got)
		}
	})
}
