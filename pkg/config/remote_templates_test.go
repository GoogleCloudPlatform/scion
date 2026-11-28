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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

// TestGitHubRepoForURL verifies that only GitHub folder URLs yield an
// owner/repo for credential lookup; archives (even on github.com), rclone and
// other hosts do not.
func TestGitHubRepoForURL(t *testing.T) {
	tests := []struct {
		name      string
		uri       string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{"tree URL", "https://github.com/acme-corp/templates/tree/main/.scion/templates", "acme-corp", "templates", true},
		{"repo root", "https://github.com/acme-corp/templates", "acme-corp", "templates", true},
		{"direct path", "https://github.com/acme-corp/templates/harnesses/claude", "acme-corp", "templates", true},
		{"clone URL strips .git", "https://github.com/acme-corp/templates.git", "acme-corp", "templates", true},
		{"github archive", "https://github.com/acme-corp/templates/archive/main.tar.gz", "", "", false},
		{"owner only", "https://github.com/acme-corp", "", "", false},
		{"other host", "https://example.com/acme-corp/templates", "", "", false},
		{"archive elsewhere", "https://example.com/template.tgz", "", "", false},
		{"rclone", ":gcs:bucket/path", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, ok := GitHubRepoForURL(tt.uri)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
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
		{
			name:     "git+https:// prefix is stripped",
			input:    "git+https://github.com/org/repo/tree/main/path",
			expected: "https://github.com/org/repo/tree/main/path",
		},
		{
			name:     "git+http:// prefix is stripped and kept as http",
			input:    "git+http://example.com/template.tgz",
			expected: "http://example.com/template.tgz",
		},
		{
			name:     "git+https:// harness config URL normalizes correctly",
			input:    "git+https://github.com/GoogleCloudPlatform/scion/harnesses/claude",
			expected: "https://github.com/GoogleCloudPlatform/scion/harnesses/claude",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeTemplateSourceURL(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNormalizeAndDetect_GitPlusHTTPS(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType RemoteTemplateType
	}{
		{
			name:     "git+https github harness URL detected as GitHub",
			input:    "git+https://github.com/GoogleCloudPlatform/scion/harnesses/claude",
			wantType: RemoteTypeGitHub,
		},
		{
			name:     "git+https github repo URL detected as GitHub",
			input:    "git+https://github.com/org/repo/tree/main/templates",
			wantType: RemoteTypeGitHub,
		},
		{
			name:     "git+http github URL detected as GitHub after normalize",
			input:    "git+http://github.com/org/repo/tree/main/templates",
			wantType: RemoteTypeGitHub,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized := NormalizeTemplateSourceURL(tt.input)
			got := DetectRemoteType(normalized)
			assert.Equal(t, tt.wantType, got, "normalized URL: %s", normalized)
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

func BenchmarkValidateRemoteURI_Rclone(b *testing.B) {
	uri := ":gcs:bucket/path/to/template"
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ValidateRemoteURI(uri)
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
		{
			name:     "pax_global_header must be filtered before calling detectCommonRoot",
			entries:  []string{"pax_global_header", "scion-main/harnesses/copilot/config.yaml"},
			expected: "",
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

// githubTarballFixture builds a gzipped tarball shaped like a GitHub archive
// download: a leading pax global header followed by entries under a single
// "<repo>-<ref>/" root directory.
func githubTarballFixture(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag:   tar.TypeXGlobalHeader,
		Name:       "pax_global_header",
		PAXRecords: map[string]string{"comment": "0123456789abcdef"},
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: root + "/", Mode: 0o755}))
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	dirs := map[string]bool{}
	for _, name := range names {
		for d := filepath.Dir(name); d != "."; d = filepath.Dir(d) {
			if !dirs[d] {
				dirs[d] = true
				require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: root + "/" + d + "/", Mode: 0o755}))
			}
		}
		body := files[name]
		require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: root + "/" + name, Mode: 0o644, Size: int64(len(body))}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// readTree returns every regular file under dir keyed by slash-separated
// relative path.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = string(b)
		return nil
	}))
	return got
}

type remoteTemplatesRoundTripFunc func(*http.Request) (*http.Response, error)

func (f remoteTemplatesRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
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
