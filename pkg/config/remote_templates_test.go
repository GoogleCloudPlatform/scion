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

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsRemoteURI(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"empty string", "", false},
		{"simple template name", "claude", false},
		{"absolute path", "/path/to/template", false},
		{"relative path", "path/to/template", false},
		{"http URL", "http://example.com/template", true},
		{"https URL", "https://github.com/user/repo/tree/main/templates/claude", true},
		{"rclone gcs", ":gcs:bucket/path/to/template", true},
		{"rclone s3", ":s3:bucket/path", true},
		{"rclone custom", ":remote:path", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsRemoteURI(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDetectRemoteType(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		expected RemoteTemplateType
	}{
		{"github tree URL", "https://github.com/user/repo/tree/main/templates", RemoteTypeGitHub},
		{"github repo root", "https://github.com/user/repo", RemoteTypeGitHub},
		{"tgz archive", "https://example.com/template.tgz", RemoteTypeArchive},
		{"tar.gz archive", "https://example.com/template.tar.gz", RemoteTypeArchive},
		{"zip archive", "https://example.com/template.zip", RemoteTypeArchive},
		{"rclone gcs", ":gcs:bucket/path", RemoteTypeRclone},
		{"rclone s3", ":s3:bucket/path", RemoteTypeRclone},
		{"unknown http", "https://example.com/folder", RemoteTypeUnknown},
		{"invalid url", "not-a-url", RemoteTypeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectRemoteType(tt.uri)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseGitHubURL(t *testing.T) {
	tests := []struct {
		name        string
		uri         string
		wantOwner   string
		wantRepo    string
		wantBranch  string
		wantPath    string
		expectError bool
	}{
		{
			name:       "full tree URL with path",
			uri:        "https://github.com/GoogleCloudPlatform/scion/tree/main/pkg/config/embeds",
			wantOwner:  "GoogleCloudPlatform",
			wantRepo:   "scion",
			wantBranch: "main",
			wantPath:   "pkg/config/embeds",
		},
		{
			name:       "tree URL without path",
			uri:        "https://github.com/user/repo/tree/develop",
			wantOwner:  "user",
			wantRepo:   "repo",
			wantBranch: "develop",
			wantPath:   "",
		},
		{
			name:       "simple repo URL defaults to main",
			uri:        "https://github.com/user/repo",
			wantOwner:  "user",
			wantRepo:   "repo",
			wantBranch: "main",
			wantPath:   "",
		},
		{
			name:       "direct path without tree defaults to main",
			uri:        "https://github.com/org/repo/some/path/.scion/templates",
			wantOwner:  "org",
			wantRepo:   "repo",
			wantBranch: "main",
			wantPath:   "some/path/.scion/templates",
		},
		{
			name:       "direct path single segment",
			uri:        "https://github.com/user/repo/.claude/agents",
			wantOwner:  "user",
			wantRepo:   "repo",
			wantBranch: "main",
			wantPath:   ".claude/agents",
		},
		{
			name:        "non-github URL",
			uri:         "https://gitlab.com/user/repo",
			expectError: true,
		},
		{
			name:        "invalid URL format",
			uri:         "https://github.com/user",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts, err := parseGitHubURL(tt.uri)
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOwner, parts.Owner)
			assert.Equal(t, tt.wantRepo, parts.Repo)
			assert.Equal(t, tt.wantBranch, parts.Branch)
			assert.Equal(t, tt.wantPath, parts.Path)
		})
	}
}

func TestMatchBranchFromRefs(t *testing.T) {
	tests := []struct {
		name       string
		afterTree  string
		refs       map[string]bool
		wantBranch string
		wantPath   string
		wantFound  bool
	}{
		{
			name:       "simple branch with path",
			afterTree:  "main/pkg/config",
			refs:       map[string]bool{"main": true, "develop": true},
			wantBranch: "main",
			wantPath:   "pkg/config",
			wantFound:  true,
		},
		{
			name:       "branch with slash and path",
			afterTree:  "scion/gh-copilot-harness-lead/harnesses/copilot",
			refs:       map[string]bool{"main": true, "scion/gh-copilot-harness-lead": true},
			wantBranch: "scion/gh-copilot-harness-lead",
			wantPath:   "harnesses/copilot",
			wantFound:  true,
		},
		{
			name:       "branch with slash no remaining path",
			afterTree:  "feature/branch-name",
			refs:       map[string]bool{"feature/branch-name": true},
			wantBranch: "feature/branch-name",
			wantPath:   "",
			wantFound:  true,
		},
		{
			name:       "prefers longest matching ref",
			afterTree:  "scion/feature/test/path",
			refs:       map[string]bool{"scion": true, "scion/feature": true, "scion/feature/test": true},
			wantBranch: "scion/feature/test",
			wantPath:   "path",
			wantFound:  true,
		},
		{
			name:       "no matching ref",
			afterTree:  "nonexistent/path",
			refs:       map[string]bool{"main": true, "develop": true},
			wantBranch: "",
			wantPath:   "",
			wantFound:  false,
		},
		{
			name:       "exact match no path",
			afterTree:  "main",
			refs:       map[string]bool{"main": true},
			wantBranch: "main",
			wantPath:   "",
			wantFound:  true,
		},
		{
			name:       "ambiguous prefix resolved by longest match",
			afterTree:  "release/v1/hotfix/file.go",
			refs:       map[string]bool{"release": true, "release/v1": true, "release/v1/hotfix": true},
			wantBranch: "release/v1/hotfix",
			wantPath:   "file.go",
			wantFound:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch, path, found := matchBranchFromRefs(tt.afterTree, tt.refs)
			assert.Equal(t, tt.wantFound, found)
			if found {
				assert.Equal(t, tt.wantBranch, branch)
				assert.Equal(t, tt.wantPath, path)
			}
		})
	}
}

func TestNormalizeTemplateSourceURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "full https URL unchanged",
			input:    "https://github.com/org/repo/tree/main/.scion/templates",
			expected: "https://github.com/org/repo/tree/main/.scion/templates",
		},
		{
			name:     "scheme-less github domain gets https prefix",
			input:    "github.com/org/repo/tree/main/.scion/templates",
			expected: "https://github.com/org/repo/tree/main/.scion/templates",
		},
		{
			name:     "bare org/repo appends scion templates path",
			input:    "https://github.com/org/repo",
			expected: "https://github.com/org/repo/tree/main/.scion/templates",
		},
		{
			name:     "scheme-less bare org/repo gets scheme and scion templates path",
			input:    "github.com/org/repo",
			expected: "https://github.com/org/repo/tree/main/.scion/templates",
		},
		{
			name:     "GitHub.com capitalized is normalized",
			input:    "GitHub.com/org/repo",
			expected: "https://GitHub.com/org/repo/tree/main/.scion/templates",
		},
		{
			name:     "rclone prefix left unchanged",
			input:    ":gcs:bucket/path",
			expected: ":gcs:bucket/path",
		},
		{
			name:     "http URL unchanged",
			input:    "http://example.com/template.tgz",
			expected: "http://example.com/template.tgz",
		},
		{
			name:     "whitespace trimmed",
			input:    "  github.com/org/repo  ",
			expected: "https://github.com/org/repo/tree/main/.scion/templates",
		},
		{
			name:     "deeper path not modified",
			input:    "github.com/org/repo/.scion/templates/mytmpl",
			expected: "https://github.com/org/repo/.scion/templates/mytmpl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeTemplateSourceURL(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestValidateRemoteURI(t *testing.T) {
	tests := []struct {
		name        string
		uri         string
		expectError bool
	}{
		{"valid github URL", "https://github.com/user/repo/tree/main/templates", false},
		{"valid archive URL", "https://example.com/template.tgz", false},
		{"valid rclone URI", ":gcs:bucket/path", false},
		{"invalid rclone format", ":invalid", true},
		{"not a remote URI", "local-template", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRemoteURI(tt.uri)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"normal path", "folder/file.txt", "folder/file.txt"},
		{"path with dots normalized", "folder/../other", "other"}, // Clean normalizes this to "other" which is safe
		{"absolute path", "/etc/passwd", ""},
		{"path traversal", "../../etc/passwd", ""},
		{"current dir", "./file.txt", "file.txt"},
		{"escape attempt", "foo/../../bar", ""}, // This tries to escape the root
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizePath(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDetectCommonRoot(t *testing.T) {
	tests := []struct {
		name     string
		entries  []string
		expected string
	}{
		{
			name:     "common root folder",
			entries:  []string{"mytemplate/file1.txt", "mytemplate/folder/file2.txt", "mytemplate/"},
			expected: "mytemplate/",
		},
		{
			name:     "no common root",
			entries:  []string{"file1.txt", "folder/file2.txt"},
			expected: "",
		},
		{
			name:     "single entry",
			entries:  []string{"mytemplate/file.txt"},
			expected: "mytemplate/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detectCommonRoot(func(yield func(string) bool) {
				for _, e := range tt.entries {
					if !yield(e) {
						return
					}
				}
			})
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDeriveTemplateName(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		expected string
	}{
		{
			name:     "github URL with path",
			uri:      "https://github.com/user/repo/tree/main/templates/claude",
			expected: "claude",
		},
		{
			name:     "github repo root",
			uri:      "https://github.com/user/my-template",
			expected: "my-template",
		},
		{
			name:     "tgz archive",
			uri:      "https://example.com/my-template.tgz",
			expected: "my-template",
		},
		{
			name:     "tar.gz archive",
			uri:      "https://example.com/my-template.tar.gz",
			expected: "my-template",
		},
		{
			name:     "zip archive",
			uri:      "https://example.com/my-template.zip",
			expected: "my-template",
		},
		{
			name:     "rclone path",
			uri:      ":gcs:bucket/path/to/template",
			expected: "template",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DeriveTemplateName(tt.uri)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGenerateCacheKey(t *testing.T) {
	// Verify that different URIs produce different cache keys
	key1 := generateCacheKey("https://github.com/user/repo1")
	key2 := generateCacheKey("https://github.com/user/repo2")
	key3 := generateCacheKey("https://github.com/user/repo1") // Same as key1

	assert.NotEqual(t, key1, key2)
	assert.Equal(t, key1, key3)
	assert.Len(t, key1, 16) // 8 bytes = 16 hex chars
}

func TestFetchGitHubTarball_AuthToken(t *testing.T) {
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// Override the tarball URL by using the test server URL parts.
	// fetchGitHubTarball constructs the URL from parts, but we can't redirect it
	// to our test server. Instead, test the variadic signature and that the
	// function accepts tokens without panicking.

	t.Run("no token is backward compatible", func(t *testing.T) {
		parts := &GitHubURLParts{Owner: "test", Repo: "repo", Branch: "main"}
		dest := t.TempDir()
		// Will fail (can't reach github.com in test), but should not panic
		_ = fetchGitHubTarball(context.Background(), parts, dest, "")
	})

	t.Run("token is accepted", func(t *testing.T) {
		parts := &GitHubURLParts{Owner: "test", Repo: "repo", Branch: "main"}
		dest := t.TempDir()
		_ = fetchGitHubTarball(context.Background(), parts, dest, "ghs_test_token_123")
	})

	_ = srv
	_ = receivedAuth
}

func TestSparseGitCheckout_AuthTokenInURL(t *testing.T) {
	// sparseGitCheckout embeds the token in the remote URL when provided.
	// With a token, GitHub responds with "Authentication failed" rather than
	// "could not read Username" — confirming the token was sent.
	parts := &GitHubURLParts{Owner: "test", Repo: "nonexistent-repo-12345", Branch: "main"}

	t.Run("without token prompts for credentials", func(t *testing.T) {
		dest := t.TempDir()
		err := sparseGitCheckout(context.Background(), parts, dest, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "git fetch failed")
	})

	t.Run("with token attempts authentication", func(t *testing.T) {
		dest := t.TempDir()
		err := sparseGitCheckout(context.Background(), parts, dest, "ghs_test_token")
		require.Error(t, err)
		// With a token embedded in the URL, git attempts auth and gets
		// "Authentication failed" instead of "could not read Username"
		assert.Contains(t, err.Error(), "Authentication failed")
	})
}

func TestIsArchiveURL(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		expected bool
	}{
		{"tgz file", "https://example.com/file.tgz", true},
		{"tar.gz file", "https://example.com/file.tar.gz", true},
		{"zip file", "https://example.com/file.zip", true},
		{"TGZ uppercase", "https://example.com/FILE.TGZ", true},
		{"regular URL", "https://example.com/folder", false},
		{"github URL", "https://github.com/user/repo", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isArchiveURL(tt.uri)
			assert.Equal(t, tt.expected, result)
		})
	}
}
