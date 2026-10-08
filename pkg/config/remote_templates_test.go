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
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
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
			name:       "trailing slash on tree URL path is stripped",
			uri:        "https://github.com/GoogleCloudPlatform/scion/tree/main/harnesses/",
			wantOwner:  "GoogleCloudPlatform",
			wantRepo:   "scion",
			wantBranch: "main",
			wantPath:   "harnesses",
		},
		{
			name:       "trailing slash on direct path is stripped",
			uri:        "https://github.com/org/repo/some/path/",
			wantOwner:  "org",
			wantRepo:   "repo",
			wantBranch: "main",
			wantPath:   "some/path",
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

// TestFetchGitHubTarball pins the GitHub archive download without network
// access (ptone/scion#3812): http.DefaultClient's transport is replaced so the
// request aimed at github.com is served by a local httptest server. It asserts
// the request that was sent (method, host, archive path for the ref, auth
// header) and the outcome (extracted files, sub-path selection, and errors for
// non-200 responses and missing sub-paths).
func TestFetchGitHubTarball(t *testing.T) {
	type seenRequest struct {
		method, scheme, host, path, auth string
	}
	var seen []seenRequest
	var status int
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[len(seen)-1].host = r.Host
		seen[len(seen)-1].path = r.URL.Path
		seen[len(seen)-1].auth = r.Header.Get("Authorization")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	srvURL, err := url.Parse(srv.URL)
	require.NoError(t, err)

	oldTransport := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = oldTransport })
	http.DefaultClient.Transport = remoteTemplatesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, seenRequest{method: r.Method, scheme: r.URL.Scheme})
		// Redirect to the local server; r.Host stays "github.com", so the
		// server still sees the host the production code targeted.
		out := r.Clone(r.Context())
		out.URL.Scheme = srvURL.Scheme
		out.URL.Host = srvURL.Host
		return srv.Client().Transport.RoundTrip(out)
	})

	files := map[string]string{
		"README.md":                      "# repo\n",
		"templates/foo/scion-agent.yaml": "schema_version: \"1\"\n",
		"templates/foo/home/.bashrc":     "export FOO=1\n",
		"templates/bar/scion-agent.yaml": "schema_version: \"2\"\n",
	}

	tests := []struct {
		name      string
		parts     GitHubURLParts
		token     string
		status    int
		root      string
		wantPath  string
		wantAuth  string
		wantFiles map[string]string
		wantErr   string
	}{
		{
			name:      "no token, default branch, whole repo",
			parts:     GitHubURLParts{Owner: "acme", Repo: "repo"},
			status:    http.StatusOK,
			root:      "repo-main",
			wantPath:  "/acme/repo/archive/refs/heads/main.tar.gz",
			wantFiles: files,
		},
		{
			name:     "token, explicit branch, sub-path",
			parts:    GitHubURLParts{Owner: "acme", Repo: "repo", Branch: "dev", Path: "templates/foo/"},
			token:    "ghs_test_token_123",
			status:   http.StatusOK,
			root:     "repo-dev",
			wantPath: "/acme/repo/archive/refs/heads/dev.tar.gz",
			wantAuth: "Bearer ghs_test_token_123",
			wantFiles: map[string]string{
				"scion-agent.yaml": "schema_version: \"1\"\n",
				"home/.bashrc":     "export FOO=1\n",
			},
		},
		{
			name:     "non-200 response is an error",
			parts:    GitHubURLParts{Owner: "acme", Repo: "private", Branch: "main"},
			token:    "ghs_test_token_123",
			status:   http.StatusNotFound,
			root:     "private-main",
			wantPath: "/acme/private/archive/refs/heads/main.tar.gz",
			wantAuth: "Bearer ghs_test_token_123",
			wantErr:  "tarball download failed: HTTP 404",
		},
		{
			name:     "missing sub-path is an error",
			parts:    GitHubURLParts{Owner: "acme", Repo: "repo", Branch: "main", Path: "templates/missing"},
			status:   http.StatusOK,
			root:     "repo-main",
			wantPath: "/acme/repo/archive/refs/heads/main.tar.gz",
			wantErr:  "path templates/missing not found in repository",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen = nil
			status = tt.status
			body = githubTarballFixture(t, tt.root, files)
			if tt.status != http.StatusOK {
				body = []byte("Not Found")
			}
			dest := t.TempDir()
			parts := tt.parts

			err := fetchGitHubTarball(context.Background(), &parts, dest, tt.token)

			require.Len(t, seen, 1, "exactly one HTTP request")
			assert.Equal(t, seenRequest{
				method: http.MethodGet,
				scheme: "https",
				host:   "github.com",
				path:   tt.wantPath,
				auth:   tt.wantAuth,
			}, seen[0])
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Empty(t, readTree(t, dest), "nothing is extracted on error")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantFiles, readTree(t, dest))
		})
	}
}

// installFakeGit puts a fake `git` first (and only) on PATH for the rest of
// the test. It appends each invocation's argv to a log file, succeeds for
// every subcommand except `fetch`, which fails with an auth-style error, so
// execSparseGitCheckout runs end to end without touching the network
// (ptone/scion#3750). It returns a func that reads the logged invocations.
func installFakeGit(t *testing.T) (calls func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake git shell script requires a POSIX shell")
	}
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "git-calls.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$SCION_FAKE_GIT_LOG"
if [ "$1" = "fetch" ]; then
  echo "fatal: Authentication failed (fake git)" >&2
  exit 128
fi
exit 0
`
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", binDir)
	t.Setenv("SCION_FAKE_GIT_LOG", logPath)
	return func() []string {
		data, err := os.ReadFile(logPath)
		if os.IsNotExist(err) {
			return nil
		}
		require.NoError(t, err)
		return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
}

func TestSparseGitCheckout_AuthTokenInURL(t *testing.T) {
	// execSparseGitCheckout embeds the token in the remote URL when provided.
	// A fake git on PATH records the remote URL so this runs offline.
	parts := &GitHubURLParts{Owner: "test", Repo: "nonexistent-repo-12345", Branch: "main", Path: "templates"}

	tests := []struct {
		name       string
		token      string
		wantRemote string
	}{
		{"without token uses anonymous URL", "", "remote add origin https://github.com/test/nonexistent-repo-12345.git"},
		{"with token embeds it in the remote URL", "ghs_test_token", "remote add origin https://x-access-token:ghs_test_token@github.com/test/nonexistent-repo-12345.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := installFakeGit(t)
			err := execSparseGitCheckout(context.Background(), parts, t.TempDir(), tt.token)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "git fetch failed")
			assert.Contains(t, err.Error(), "Authentication failed")
			if tt.token != "" {
				assert.NotContains(t, err.Error(), tt.token, "token must never appear in error text")
			}
			assert.Equal(t, []string{
				"init",
				tt.wantRemote,
				"config core.sparseCheckout true",
				"fetch --depth=1 origin main",
			}, calls())
		})
	}
}

type remoteTemplatesRoundTripFunc func(*http.Request) (*http.Response, error)

func (f remoteTemplatesRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// TestFetchGitHubFolder_FallsBackToSparseCheckoutSeam pins that the git
// fallback goes through the sparseGitCheckout seam (so tests in other
// packages can stub it via SetGitSparseCheckoutForTest) with the parsed ref
// and the auth token, and that its result is returned as-is.
func TestFetchGitHubFolder_FallsBackToSparseCheckoutSeam(t *testing.T) {
	calls := installFakeGit(t) // any real git exec would be recorded here
	t.Cleanup(SetGitLsRemoteForTest(func(context.Context, string) ([]byte, error) {
		return nil, errors.New("ls-remote disabled in test")
	}))

	oldTransport := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = oldTransport })
	http.DefaultClient.Transport = remoteTemplatesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})

	var gotParts GitHubURLParts
	var gotDest, gotToken string
	n := 0
	t.Cleanup(SetGitSparseCheckoutForTest(func(_ context.Context, parts *GitHubURLParts, destPath, token string) error {
		n++
		gotParts, gotDest, gotToken = *parts, destPath, token
		return os.WriteFile(filepath.Join(destPath, "scion-agent.yaml"), []byte("schema_version: \"1\"\n"), 0o644)
	}))

	dest := t.TempDir()
	err := fetchGitHubFolder(context.Background(), "https://github.com/acme/repo/tree/main/templates/foo", dest, "ghs_tok")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, GitHubURLParts{Owner: "acme", Repo: "repo", Branch: "main", Path: "templates/foo"}, gotParts)
	assert.Equal(t, dest, gotDest)
	assert.Equal(t, "ghs_tok", gotToken)
	assert.FileExists(t, filepath.Join(dest, "scion-agent.yaml"))
	assert.Empty(t, calls(), "no real git may be executed")

	// Errors from the seam propagate unchanged.
	wantErr := errors.New("stub checkout failed")
	t.Cleanup(SetGitSparseCheckoutForTest(func(context.Context, *GitHubURLParts, string, string) error { return wantErr }))
	err = fetchGitHubFolder(context.Background(), "https://github.com/acme/repo/tree/main/templates/foo", t.TempDir(), "")
	assert.ErrorIs(t, err, wantErr)
}

// TestSetGitSparseCheckoutForTest_Restores pins that the sparse-checkout seam
// defaults to the production runner, installs the given stub, and that the
// returned restore func puts the production runner back (ptone/scion#3750).
func TestSetGitSparseCheckoutForTest_Restores(t *testing.T) {
	before := sparseGitCheckout
	// The default runner must be the production implementation.
	assert.Equal(t, reflect.ValueOf(execSparseGitCheckout).Pointer(), reflect.ValueOf(before).Pointer())
	stub := GitSparseCheckoutFunc(func(context.Context, *GitHubURLParts, string, string) error { return nil })
	restore := SetGitSparseCheckoutForTest(stub)
	// The stub must be the runner actually installed.
	assert.Equal(t, reflect.ValueOf(stub).Pointer(), reflect.ValueOf(sparseGitCheckout).Pointer())
	restore()
	// The production runner must be back in place.
	assert.Equal(t, reflect.ValueOf(before).Pointer(), reflect.ValueOf(sparseGitCheckout).Pointer())
	assert.Equal(t, reflect.ValueOf(execSparseGitCheckout).Pointer(), reflect.ValueOf(sparseGitCheckout).Pointer())
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
