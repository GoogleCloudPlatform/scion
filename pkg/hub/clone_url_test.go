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

import (
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

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
		{
			name:  "strips trailing .git from ADO https clone URL",
			input: "https://dev.azure.com/org/proj/_git/repo.git",
			want:  "https://dev.azure.com/org/proj/_git/repo",
		},
		{
			name:  "strips userinfo and trailing .git from ADO https clone URL",
			input: "https://org@dev.azure.com/org/proj/_git/repo.git",
			want:  "https://dev.azure.com/org/proj/_git/repo",
		},
		{
			name:  "strips trailing .git from ADO visualstudio.com clone URL",
			input: "https://myorg.visualstudio.com/proj/_git/repo.git",
			want:  "https://myorg.visualstudio.com/proj/_git/repo",
		},
		{
			name:  "strips trailing .git from on-prem ADO /_git/ URL",
			input: "http://tfs.internal/DefaultCollection/proj/_git/repo.git",
			want:  "http://tfs.internal/DefaultCollection/proj/_git/repo",
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
		{
			name:      "strips trailing .git from ADO override",
			override:  "https://dev.azure.com/org/proj/_git/repo.git",
			gitRemote: "dev.azure.com/org/proj/_git/repo",
			want:      "https://dev.azure.com/org/proj/_git/repo",
		},
		{
			name:      "strips userinfo and trailing .git from ADO override",
			override:  "https://org@dev.azure.com/org/proj/_git/repo.git",
			gitRemote: "",
			want:      "https://dev.azure.com/org/proj/_git/repo",
		},
		{
			name:      "falls back to ADO remote without .git",
			override:  "",
			gitRemote: "dev.azure.com/org/proj/_git/repo.git",
			want:      "https://dev.azure.com/org/proj/_git/repo",
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

func TestNormalizeADOCloneURLLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"ado dev.azure.com .git stripped", "https://dev.azure.com/org/proj/_git/repo.git", "https://dev.azure.com/org/proj/_git/repo"},
		{"ado visualstudio.com .git stripped", "https://myorg.visualstudio.com/proj/_git/repo.git", "https://myorg.visualstudio.com/proj/_git/repo"},
		{"ado without .git unchanged", "https://dev.azure.com/org/proj/_git/repo", "https://dev.azure.com/org/proj/_git/repo"},
		{"github .git unchanged", "https://github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"schemeless non-ado unchanged", "github.com/org/repo", "github.com/org/repo"},
	}

	for i := range tests {
		tt := tests[i]
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			labels := map[string]string{store.LabelCloneURL: tt.input}
			normalizeADOCloneURLLabel(labels)
			if got := labels[store.LabelCloneURL]; got != tt.want {
				t.Fatalf("normalizeADOCloneURLLabel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSharedWorkspaceCloneError_HostGuidance(t *testing.T) {
	t.Parallel()

	t.Run("ADO TF401019 classified as not found with dev.azure.com guidance", func(t *testing.T) {
		t.Parallel()
		err := sharedWorkspaceCloneError(
			errors.New("remote: TF401019: The Git repository with name or identifier repo.git does not exist.\nfatal: repository 'https://dev.azure.com/org/proj/_git/repo.git/' not found"),
			"https://dev.azure.com/org/proj/_git/repo",
		)
		var gitErr *util.GitError
		if !errors.As(err, &gitErr) {
			t.Fatalf("expected *util.GitError, got %T", err)
		}
		if gitErr.Kind != util.GitErrNotFound {
			t.Fatalf("Kind = %v, want GitErrNotFound", gitErr.Kind)
		}
		guidance := gitErr.UserGuidance()
		if !strings.Contains(guidance, "dev.azure.com") {
			t.Errorf("UserGuidance() = %q, want dev.azure.com", guidance)
		}
		if strings.Contains(guidance, "GitHub") || strings.Contains(guidance, "GITHUB_TOKEN") {
			t.Errorf("UserGuidance() = %q, must not mention GitHub/GITHUB_TOKEN", guidance)
		}
	})

	t.Run("ADO auth failure names dev.azure.com without GitHub", func(t *testing.T) {
		t.Parallel()
		err := sharedWorkspaceCloneError(
			errors.New("fatal: Authentication failed"),
			"https://dev.azure.com/org/proj/_git/repo",
		)
		var gitErr *util.GitError
		if !errors.As(err, &gitErr) {
			t.Fatalf("expected *util.GitError, got %T", err)
		}
		if gitErr.Kind != util.GitErrAuth {
			t.Fatalf("Kind = %v, want GitErrAuth", gitErr.Kind)
		}
		guidance := gitErr.UserGuidance()
		if !strings.Contains(guidance, "dev.azure.com") {
			t.Errorf("UserGuidance() = %q, want dev.azure.com", guidance)
		}
		if strings.Contains(guidance, "GitHub") || strings.Contains(guidance, "GITHUB_TOKEN") {
			t.Errorf("UserGuidance() = %q, must not mention GitHub/GITHUB_TOKEN", guidance)
		}
	})
}
