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

package config

import "testing"

func TestDeriveResourceName(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "github deep URL single resource (antigravity bug)",
			source: "https://github.com/org/repo/tree/main/antigravity",
			want:   "antigravity",
		},
		{
			name:   "github tree path nested",
			source: "https://github.com/org/repo/tree/main/harness-configs/custom-claude",
			want:   "custom-claude",
		},
		{
			name:   "github shorthand without scheme",
			source: "github.com/org/repo/tree/main/harness-configs/prod-config",
			want:   "prod-config",
		},
		{
			name:   "bare github repo",
			source: "https://github.com/org/repo",
			want:   "repo",
		},
		{
			name:   "https archive URL",
			source: "https://example.com/downloads/custom-harness.tgz",
			want:   "custom-harness.tgz",
		},
		{
			name:   "rclone GCS URI",
			source: ":gcs:my-bucket/harness-configs/prod-claude",
			want:   "prod-claude",
		},
		{
			name:   "rclone GCS trailing slash",
			source: ":gcs:my-bucket/harness-configs/prod-claude/",
			want:   "prod-claude",
		},
		{
			name:   "file URL absolute",
			source: "file:///path/to/my-config",
			want:   "my-config",
		},
		{
			name:   "file URL trailing slash",
			source: "file:///path/to/my-config/",
			want:   "my-config",
		},
		{
			name:   "plain absolute path",
			source: "/home/user/configs/my-harness",
			want:   "my-harness",
		},
		{
			name:   "relative local path",
			source: "configs/my-harness",
			want:   "my-harness",
		},
		{
			name:   "github URL trailing slash on leaf",
			source: "https://github.com/org/repo/tree/main/antigravity/",
			want:   "antigravity",
		},
		{
			name:   "empty string",
			source: "",
			want:   "",
		},
		{
			name:   "whitespace only",
			source: "   ",
			want:   "",
		},
		{
			name:   "root path",
			source: "/",
			want:   "",
		},
		{
			name:   "dot",
			source: ".",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveResourceName(tt.source); got != tt.want {
				t.Errorf("DeriveResourceName(%q) = %q, want %q", tt.source, got, tt.want)
			}
		})
	}
}
