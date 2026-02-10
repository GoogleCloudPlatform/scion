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
			uri:        "https://github.com/ptone/scion-agent/tree/main/pkg/config/embeds",
			wantOwner:  "ptone",
			wantRepo:   "scion-agent",
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

func TestConvertToSvnURL(t *testing.T) {
	tests := []struct {
		name     string
		parts    *GitHubURLParts
		expected string
	}{
		{
			name: "main branch with path",
			parts: &GitHubURLParts{
				Owner:  "user",
				Repo:   "repo",
				Branch: "main",
				Path:   "path/to/folder",
			},
			expected: "https://github.com/user/repo/trunk/path/to/folder",
		},
		{
			name: "master branch with path",
			parts: &GitHubURLParts{
				Owner:  "user",
				Repo:   "repo",
				Branch: "master",
				Path:   "templates",
			},
			expected: "https://github.com/user/repo/trunk/templates",
		},
		{
			name: "feature branch with path",
			parts: &GitHubURLParts{
				Owner:  "user",
				Repo:   "repo",
				Branch: "feature/new-stuff",
				Path:   "templates",
			},
			expected: "https://github.com/user/repo/branches/feature/new-stuff/templates",
		},
		{
			name: "branch without path",
			parts: &GitHubURLParts{
				Owner:  "user",
				Repo:   "repo",
				Branch: "main",
				Path:   "",
			},
			expected: "https://github.com/user/repo/trunk",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertToSvnURL(tt.parts)
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
			result := deriveTemplateName(tt.uri)
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
