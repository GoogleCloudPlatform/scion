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

package agent

import (
	"testing"
)

func TestParseGHShorthand(t *testing.T) {
	tests := []struct {
		name      string
		uri       string
		want      *GitHubSkillRef
		wantError bool
	}{
		{
			name: "basic without ref",
			uri:  "gh://addyosmani/agent-skills/code-simplification",
			want: &GitHubSkillRef{
				Owner:     "addyosmani",
				Repo:      "agent-skills",
				SkillName: "code-simplification",
				Ref:       "",
				SkillPath: "skills/code-simplification",
			},
		},
		{
			name: "with branch ref",
			uri:  "gh://addyosmani/agent-skills/code-simplification@main",
			want: &GitHubSkillRef{
				Owner:     "addyosmani",
				Repo:      "agent-skills",
				SkillName: "code-simplification",
				Ref:       "main",
				SkillPath: "skills/code-simplification",
			},
		},
		{
			name: "with tag ref",
			uri:  "gh://addyosmani/agent-skills/code-simplification@v1.0.0",
			want: &GitHubSkillRef{
				Owner:     "addyosmani",
				Repo:      "agent-skills",
				SkillName: "code-simplification",
				Ref:       "v1.0.0",
				SkillPath: "skills/code-simplification",
			},
		},
		{
			name: "with commit SHA ref",
			uri:  "gh://addyosmani/agent-skills/code-simplification@abc123f",
			want: &GitHubSkillRef{
				Owner:     "addyosmani",
				Repo:      "agent-skills",
				SkillName: "code-simplification",
				Ref:       "abc123f",
				SkillPath: "skills/code-simplification",
			},
		},
		{
			name:      "missing skill name",
			uri:       "gh://owner/repo",
			wantError: true,
		},
		{
			name:      "too many segments",
			uri:       "gh://owner/repo/skill/extra",
			wantError: true,
		},
		{
			name:      "empty ref after @",
			uri:       "gh://owner/repo/skill@",
			wantError: true,
		},
		{
			name:      "empty owner",
			uri:       "gh:///repo/skill",
			wantError: true,
		},
		{
			name:      "not a gh URI",
			uri:       "skill://my-skill",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGitHubSkillURI(tt.uri)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Owner != tt.want.Owner {
				t.Errorf("Owner = %q, want %q", got.Owner, tt.want.Owner)
			}
			if got.Repo != tt.want.Repo {
				t.Errorf("Repo = %q, want %q", got.Repo, tt.want.Repo)
			}
			if got.SkillName != tt.want.SkillName {
				t.Errorf("SkillName = %q, want %q", got.SkillName, tt.want.SkillName)
			}
			if got.Ref != tt.want.Ref {
				t.Errorf("Ref = %q, want %q", got.Ref, tt.want.Ref)
			}
			if got.SkillPath != tt.want.SkillPath {
				t.Errorf("SkillPath = %q, want %q", got.SkillPath, tt.want.SkillPath)
			}
			if got.Raw != tt.uri {
				t.Errorf("Raw = %q, want %q", got.Raw, tt.uri)
			}
		})
	}
}

func TestParseGitHubFullURL(t *testing.T) {
	tests := []struct {
		name      string
		uri       string
		want      *GitHubSkillRef
		wantError bool
	}{
		{
			name: "standard skills path",
			uri:  "https://github.com/owner/repo/tree/main/skills/my-skill",
			want: &GitHubSkillRef{
				Owner:     "owner",
				Repo:      "repo",
				SkillName: "my-skill",
				Ref:       "main",
				SkillPath: "skills/my-skill",
			},
		},
		{
			name: "with tag ref",
			uri:  "https://github.com/owner/repo/tree/v1.0/skills/my-skill",
			want: &GitHubSkillRef{
				Owner:     "owner",
				Repo:      "repo",
				SkillName: "my-skill",
				Ref:       "v1.0",
				SkillPath: "skills/my-skill",
			},
		},
		{
			name: "custom path",
			uri:  "https://github.com/owner/repo/tree/abc123/custom/path/skill",
			want: &GitHubSkillRef{
				Owner:     "owner",
				Repo:      "repo",
				SkillName: "skill",
				Ref:       "abc123",
				SkillPath: "custom/path/skill",
			},
		},
		{
			name:      "missing tree segment",
			uri:       "https://github.com/owner/repo",
			wantError: true,
		},
		{
			name:      "blob instead of tree",
			uri:       "https://github.com/owner/repo/blob/main/file.go",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGitHubSkillURI(tt.uri)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Owner != tt.want.Owner {
				t.Errorf("Owner = %q, want %q", got.Owner, tt.want.Owner)
			}
			if got.Repo != tt.want.Repo {
				t.Errorf("Repo = %q, want %q", got.Repo, tt.want.Repo)
			}
			if got.SkillName != tt.want.SkillName {
				t.Errorf("SkillName = %q, want %q", got.SkillName, tt.want.SkillName)
			}
			if got.Ref != tt.want.Ref {
				t.Errorf("Ref = %q, want %q", got.Ref, tt.want.Ref)
			}
			if got.SkillPath != tt.want.SkillPath {
				t.Errorf("SkillPath = %q, want %q", got.SkillPath, tt.want.SkillPath)
			}
			if got.Raw != tt.uri {
				t.Errorf("Raw = %q, want %q", got.Raw, tt.uri)
			}
		})
	}
}
