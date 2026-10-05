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

import "testing"

func TestNormalizeCloneURL(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"empty", "", ""},
		{"git scheme preserved", "git://172.17.0.1:9418/org/repo", "git://172.17.0.1:9418/org/repo"},
		{"ssh scheme preserved", "ssh://git@github.com/org/repo.git", "ssh://git@github.com/org/repo.git"},
		{"scp-style ssh preserved", "git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		{"https preserved", "https://github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"https without .git preserved", "https://github.com/org/repo", "https://github.com/org/repo"},
		{"http preserved", "http://forgejo:3000/org/repo.git", "http://forgejo:3000/org/repo.git"},
		{"uppercase scheme preserved", "HTTPS://github.com/org/repo", "HTTPS://github.com/org/repo"},
		{"schemeless normalized to https", "github.com/org/repo", "https://github.com/org/repo.git"},
		{"schemeless with .git", "github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"absolute path preserved", "/tmp/source-repo", "/tmp/source-repo"},
		{"relative path preserved", "./repo", "./repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeCloneURL(tt.input); got != tt.want {
				t.Fatalf("NormalizeCloneURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveCloneURL(t *testing.T) {
	tests := []struct {
		name, override, remote, want string
	}{
		{"override wins", "git://host/org/repo", "github.com/org/repo", "git://host/org/repo"},
		{"schemeless override normalized", "github.com/other/repo", "github.com/org/repo", "https://github.com/other/repo.git"},
		{"falls back to remote", "", "github.com/org/repo", "https://github.com/org/repo.git"},
		{"remote scheme not doubled", "", "https://github.com/org/repo", "https://github.com/org/repo"},
		{"both empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveCloneURL(tt.override, tt.remote); got != tt.want {
				t.Fatalf("ResolveCloneURL(%q, %q) = %q, want %q", tt.override, tt.remote, got, tt.want)
			}
		})
	}
}
