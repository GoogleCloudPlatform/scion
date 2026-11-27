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
	"crypto/sha1" //nolint:gosec // git blob object IDs are SHA-1
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildTestGitHubTarGz(t *testing.T, paxComment string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if paxComment != "" {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeXGlobalHeader,
			Name:       "pax_global_header",
			PAXRecords: map[string]string{"comment": paxComment},
		}))
	}
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
		}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	return buf.Bytes()
}

func testGitBlobSHA(content string) string {
	h := sha1.New() //nolint:gosec // git blob object IDs are SHA-1
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(content))
	_, _ = h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

type testHTTPDoer struct {
	client *http.Client
	mu     sync.Mutex
	reqs   []githubfetch.Request
}

func (d *testHTTPDoer) Open(ctx context.Context, r githubfetch.Request) (*githubfetch.Response, error) {
	d.mu.Lock()
	d.reqs = append(d.reqs, r)
	d.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	if r.Authorization != "" {
		req.Header.Set("Authorization", r.Authorization)
	}
	for k, vals := range r.Header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	return &githubfetch.Response{
		Status: resp.StatusCode,
		Header: resp.Header,
		Body:   resp.Body,
	}, nil
}

// newFakeGitHubClient stands up an httptest fake GitHub supporting commits,
// matching-refs, git/trees, REST tarball, and public archive endpoints.
func newFakeGitHubClient(t *testing.T, refs map[string]string, treePath string, files map[string]string) (*githubfetch.Client, *testHTTPDoer) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/git/matching-refs/heads/"):
			var entries []map[string]any
			for refName, sha := range refs {
				if strings.HasPrefix(refName, "refs/heads/") {
					entries = append(entries, map[string]any{
						"ref":    refName,
						"object": map[string]any{"type": "commit", "sha": sha},
					})
				}
			}
			_ = json.NewEncoder(w).Encode(entries)
		case strings.Contains(p, "/git/matching-refs/tags/"):
			var entries []map[string]any
			for refName, sha := range refs {
				if strings.HasPrefix(refName, "refs/tags/") {
					entries = append(entries, map[string]any{
						"ref":    refName,
						"object": map[string]any{"type": "commit", "sha": sha},
					})
				}
			}
			_ = json.NewEncoder(w).Encode(entries)
		case strings.Contains(p, "/commits/"):
			idx := strings.Index(p, "/commits/")
			ref := p[idx+len("/commits/"):]
			sha, ok := refs[ref]
			if !ok {
				sha, ok = refs["refs/heads/"+ref]
			}
			if !ok {
				sha, ok = refs["refs/tags/"+ref]
			}
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(sha))
		case strings.Contains(p, "/git/trees/"):
			var treeEntries []map[string]any
			for rel, body := range files {
				treeEntries = append(treeEntries, map[string]any{
					"path": rel,
					"mode": "100644",
					"type": "blob",
					"sha":  testGitBlobSHA(body),
					"size": len(body),
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sha":       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"truncated": false,
				"tree":      treeEntries,
			})
		case strings.Contains(p, "/tarball/") || strings.Contains(p, "/archive/"):
			var sha string
			if idx := strings.Index(p, "/tarball/"); idx >= 0 {
				sha = p[idx+len("/tarball/"):]
			} else if idx := strings.Index(p, "/archive/"); idx >= 0 {
				sha = strings.TrimSuffix(p[idx+len("/archive/"):], ".tar.gz")
			}
			tarFiles := make(map[string]string, len(files))
			prefix := "repo-" + sha + "/"
			if treePath != "" {
				prefix += strings.Trim(treePath, "/") + "/"
			}
			for rel, body := range files {
				tarFiles[prefix+rel] = body
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(buildTestGitHubTarGz(t, sha, tarFiles))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	doer := &testHTTPDoer{client: srv.Client()}
	ep := githubfetch.Endpoint{
		APIBase:  srv.URL,
		RawBase:  srv.URL,
		WebHost:  srv.URL,
		Codeload: srv.URL,
	}
	client := githubfetch.New(githubfetch.Options{
		Endpoint: ep,
		Doer:     doer,
		Cooldown: githubfetch.NewCooldown(nil),
	})
	return client, doer
}

func TestFetchRemoteTemplateWithResult_GitHubCommitAndPrivateStaging(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Verify no git binary is needed on PATH.
	t.Setenv("PATH", "")

	const wantSHA = "0123456789abcdef0123456789abcdef01234567"
	client, doer := newFakeGitHubClient(t, map[string]string{
		"refs/heads/main":     wantSHA,
		"refs/heads/feat/x/y": wantSHA,
		"refs/tags/v1.2.3":    wantSHA,
	}, "templates/demo", map[string]string{
		"scion-agent.yaml": "harness: claude\n",
	})

	t.Run("resolves main, records commit SHA, uses private per-call staging dir, anonymous uses archive URL", func(t *testing.T) {
		res1, err := FetchRemoteTemplateWithResult(context.Background(),
			"https://github.com/acme/repo/tree/main/templates/demo",
			FetchOptions{GitHubClient: client})
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(res1.Path) })

		res2, err := FetchRemoteTemplateWithResult(context.Background(),
			"https://github.com/acme/repo/tree/main/templates/demo",
			FetchOptions{GitHubClient: client})
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(res2.Path) })

		assert.Equal(t, wantSHA, res1.Commit)
		assert.FileExists(t, filepath.Join(res1.Path, "scion-agent.yaml"))
		assert.NotEqual(t, res1.Path, res2.Path, "each GitHub fetch must use its own private staging dir")

		// Anonymous fetch must use archive URL and send no Authorization header.
		sawArchive := false
		for _, r := range doer.reqs {
			assert.Empty(t, r.Authorization, "anonymous fetch must send no Authorization")
			if strings.Contains(r.URL, "/archive/"+wantSHA+".tar.gz") {
				sawArchive = true
			}
			assert.NotContains(t, r.URL, "/tarball/", "anonymous fetch must not use REST tarball")
		}
		assert.True(t, sawArchive, "expected anonymous archive URL request")
	})

	t.Run("slashed branch feat/x/y, tag, and 40-hex SHA with credential", func(t *testing.T) {
		for _, uri := range []string{
			"https://github.com/acme/repo/tree/feat/x/y/templates/demo",
			"https://github.com/acme/repo/tree/v1.2.3/templates/demo",
			"https://github.com/acme/repo/tree/" + wantSHA + "/templates/demo",
		} {
			res, err := FetchRemoteTemplateWithResult(context.Background(), uri, FetchOptions{
				GitHubClient: client,
				AuthToken:    "ghp_test_token",
			})
			require.NoError(t, err, uri)
			assert.Equal(t, wantSHA, res.Commit, uri)
			assert.FileExists(t, filepath.Join(res.Path, "scion-agent.yaml"))
			_ = os.RemoveAll(res.Path)
		}
	})

	t.Run("non-GitHub archive URL never records a source commit", func(t *testing.T) {
		tarball := buildTestGitHubTarGz(t, wantSHA, map[string]string{
			"demo/scion-agent.yaml": "harness: claude\n",
		})
		res, err := FetchRemoteTemplateWithResult(context.Background(),
			"https://example.com/templates/demo.tar.gz",
			FetchOptions{
				PrivateCacheDir: true,
				Download: func(_ context.Context, _, _ string) ([]byte, error) {
					return tarball, nil
				},
			})
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(res.Path) })
		assert.Empty(t, res.Commit)
	})
}

func TestFindTemplateWithContext_UsesEnvGitHubCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "")
	t.Setenv("GH_ACME__PRIV", "ghp_env_repo_secret")
	t.Setenv("GITHUB_TOKEN", "ghp_env_default")

	const sha = "1111111111111111111111111111111111111111"
	client, doer := newFakeGitHubClient(t, map[string]string{
		"refs/heads/main": sha,
	}, "templates/demo", map[string]string{
		"scion-agent.yaml": "harness: claude\n",
	})
	t.Cleanup(SetGitHubClientForTest(client))

	tpl, err := FindTemplateWithContext(context.Background(), "https://github.com/acme/priv/tree/main/templates/demo")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tpl.Path) })
	assert.Equal(t, "demo", tpl.Name)
	assert.FileExists(t, filepath.Join(tpl.Path, "scion-agent.yaml"))

	require.NotEmpty(t, doer.reqs)
	for _, r := range doer.reqs {
		assert.Equal(t, "Bearer ghp_env_repo_secret", r.Authorization)
	}
}
