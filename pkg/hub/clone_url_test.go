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

package hub

import "testing"

func TestNormalizeCloneURLLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "preserves local path",
			input: "/tmp/source-repo",
			want:  "/tmp/source-repo",
		},
		{
			name:  "normalizes schemeless remote",
			input: "github.com/example/repo",
			want:  "https://github.com/example/repo.git",
		},
		{
			name:  "preserves explicit http override",
			input: "http://forgejo-http.forgejo.svc.cluster.local:3000/carverauto/serviceradar.git",
			want:  "http://forgejo-http.forgejo.svc.cluster.local:3000/carverauto/serviceradar.git",
		},
		{
			name:  "preserves explicit ssh shorthand override",
			input: "git@github.com:example/repo.git",
			want:  "git@github.com:example/repo.git",
		},
	}

	for i := range tests {
		tt := tests[i]
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeCloneURLLabel(tt.input); got != tt.want {
				t.Fatalf("normalizeCloneURLLabel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveCloneURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		override  string
		gitRemote string
		want      string
	}{
		{
			name:      "uses explicit http override",
			override:  "http://forgejo-http.forgejo.svc.cluster.local:3000/carverauto/serviceradar.git",
			gitRemote: "github.com/example/repo",
			want:      "http://forgejo-http.forgejo.svc.cluster.local:3000/carverauto/serviceradar.git",
		},
		{
			name:      "falls back to normalized git remote",
			override:  "",
			gitRemote: "github.com/example/repo",
			want:      "https://github.com/example/repo.git",
		},
		{
			name:      "falls back to explicit ssh remote without rewriting",
			override:  "",
			gitRemote: "git@github.com:example/repo.git",
			want:      "git@github.com:example/repo.git",
		},
		{
			name:      "falls back to local path without rewriting",
			override:  "",
			gitRemote: "/tmp/source-repo",
			want:      "/tmp/source-repo",
		},
	}

	for i := range tests {
		tt := tests[i]
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveCloneURL(tt.override, tt.gitRemote); got != tt.want {
				t.Fatalf("resolveCloneURL(%q, %q) = %q, want %q", tt.override, tt.gitRemote, got, tt.want)
			}
		})
	}
}
