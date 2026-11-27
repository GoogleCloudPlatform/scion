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

package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // git blob object IDs are SHA-1
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	credTestRepoKey  = "GH_ACME_CORP__PRIVATE_CONFIGS"
	credTestOwnerKey = "GH_ACME_CORP"
	credTestSource   = "https://github.com/acme-corp/private.configs/tree/main/harness-configs/custom"
)

// clearGitHubCredentialEnv unsets the variables remoteFetchCredential reads,
// so a credential leaked from the container cannot decide a test.
func clearGitHubCredentialEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{credTestRepoKey, credTestOwnerKey, "GITHUB_TOKEN", "GH_OTHER_ORG"} {
		t.Setenv(name, "")
	}
}

// TestRemoteFetchCredential verifies the CLI cascade: GH_{OWNER}__{REPO}
// beats GH_{OWNER}, which beats GITHUB_TOKEN; with none set the fetch is
// unauthenticated; and the debug log names the variable, never its value.
func TestRemoteFetchCredential(t *testing.T) {
	t.Cleanup(func() { loglevel.Reset(false) })
	loglevel.Apply(loglevel.Spec{Default: slog.LevelDebug}, loglevel.SourceFlag)
	require.True(t, util.DebugEnabled(), "debug output must be on for the log assertions")

	tests := []struct {
		name     string
		env      map[string]string
		want     string
		wantName string
	}{
		{"repo key beats owner and default", map[string]string{credTestRepoKey: "fake-repo-cred", credTestOwnerKey: "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"}, "fake-repo-cred", credTestRepoKey},
		{"owner key beats default", map[string]string{credTestOwnerKey: "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"}, "fake-owner-cred", credTestOwnerKey},
		{"default when no convention key", map[string]string{"GITHUB_TOKEN": "fake-default-cred"}, "fake-default-cred", "GITHUB_TOKEN"},
		{"keys for another owner are ignored", map[string]string{"GH_OTHER_ORG": "fake-other-cred"}, "", ""},
		{"unauthenticated when nothing is set", map[string]string{}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGitHubCredentialEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			var got githubfetch.Credential
			stderr := captureStderr(t, func() { got = remoteFetchCredential(credTestSource) })
			assert.Equal(t, tt.want, got.Token)
			if tt.want == "" {
				assert.Equal(t, githubfetch.CredAnonymous, got.Kind)
				assert.Equal(t, githubfetch.AnonScope, got.Scope)
			} else {
				assert.Equal(t, githubfetch.CredEnv, got.Kind)
				assert.True(t, strings.HasPrefix(got.Scope, "env:"))
			}
			if tt.wantName != "" {
				assert.Contains(t, stderr, "using credential "+tt.wantName)
			}
			for k, v := range tt.env {
				assert.NotContains(t, stderr, v, "log contains the value of %s", k)
			}
		})
	}
}

// TestRemoteFetchCredential_NonGitHubReadsNothing verifies that sources
// which are not GitHub folders get no credential even when all are set.
func TestRemoteFetchCredential_NonGitHubReadsNothing(t *testing.T) {
	clearGitHubCredentialEnv(t)
	t.Setenv("GH_ACME_CORP__CONFIGS", "fake-repo-cred")
	t.Setenv(credTestOwnerKey, "fake-owner-cred")
	t.Setenv("GITHUB_TOKEN", "fake-default-cred")
	for _, source := range []string{
		"https://github.com/acme-corp/configs/archive/main.tar.gz",
		"https://example.com/acme-corp/configs.tgz",
		":gcs:bucket/acme-corp/configs",
	} {
		cred := remoteFetchCredential(source)
		assert.Empty(t, cred.Token, source)
		assert.Equal(t, githubfetch.CredAnonymous, cred.Kind, source)
	}
}

type cmdTestHTTPDoer struct {
	client *http.Client
	mu     sync.Mutex
	reqs   []githubfetch.Request
}

func (d *cmdTestHTTPDoer) Open(ctx context.Context, r githubfetch.Request) (*githubfetch.Response, error) {
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
	return &githubfetch.Response{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

// stubGitHubFetch stands up an httptest fake GitHub and wires it into
// config.SetGitHubClientForTest so CLI commands run hermetically without git
// on PATH. Returns a func reporting the tarball request path and Authorization.
func stubGitHubFetch(t *testing.T, subPath, marker, body string) func() (tarballPath, tarballAuth string) {
	t.Helper()
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	blobH := sha1.New() //nolint:gosec // git blob object IDs are SHA-1
	_, _ = fmt.Fprintf(blobH, "blob %d\x00", len(body))
	_, _ = blobH.Write([]byte(body))
	blobSHA := hex.EncodeToString(blobH.Sum(nil))

	var tarBuf bytes.Buffer
	gzw := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gzw)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag:   tar.TypeXGlobalHeader,
		Name:       "pax_global_header",
		PAXRecords: map[string]string{"comment": sha},
	}))
	entryName := "repo-" + sha + "/" + strings.Trim(subPath, "/") + "/" + marker
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     entryName,
		Mode:     0o644,
		Size:     int64(len(body)),
	}))
	_, err := tw.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	tarBytes := tarBuf.Bytes()

	var mu sync.Mutex
	var tarballPath, tarballAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/git/matching-refs/heads/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"ref": "refs/heads/main", "object": map[string]any{"type": "commit", "sha": sha}},
			})
		case strings.Contains(p, "/commits/"):
			_, _ = w.Write([]byte(sha))
		case strings.Contains(p, "/git/trees/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sha":       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				"truncated": false,
				"tree": []map[string]any{
					{"path": marker, "mode": "100644", "type": "blob", "sha": blobSHA, "size": len(body)},
				},
			})
		case strings.Contains(p, "/tarball/") || strings.Contains(p, "/archive/"):
			mu.Lock()
			tarballPath = p
			tarballAuth = r.Header.Get("Authorization")
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tarBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	doer := &cmdTestHTTPDoer{client: srv.Client()}
	ep := githubfetch.Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
	client := githubfetch.New(githubfetch.Options{Endpoint: ep, Doer: doer, Cooldown: githubfetch.NewCooldown(nil)})
	t.Cleanup(config.SetGitHubClientForTest(client))

	return func() (string, string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		require.NotEmpty(t, tarballPath, "the fetch sent no tarball request")
		return tarballPath, tarballAuth
	}
}

// TestResolveInstallSource_SendsGitHubCredential verifies that
// 'harness-config install' passes the CLI credential to githubfetch and works
// without git on PATH.
func TestResolveInstallSource_SendsGitHubCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "")
	clearGitHubCredentialEnv(t)
	t.Setenv(credTestOwnerKey, "fake-owner-cred")
	t.Setenv("GITHUB_TOKEN", "fake-default-cred")
	sent := stubGitHubFetch(t, "harness-configs/custom", "config.yaml", "harness: claude\n")

	localPath, cleanup, err := resolveInstallSource(credTestSource)
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	defer cleanup()
	assert.FileExists(t, filepath.Join(localPath, "config.yaml"))
	tarballPath, tarballAuth := sent()
	assert.Equal(t, "/repos/acme-corp/private.configs/tarball/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", tarballPath)
	assert.Equal(t, "Bearer fake-owner-cred", tarballAuth)
}

// TestRunTemplateImport_SendsGitHubCredential verifies that 'templates
// import' passes the CLI credential to githubfetch and works without git on
// PATH.
func TestRunTemplateImport_SendsGitHubCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "")
	t.Chdir(t.TempDir())
	clearGitHubCredentialEnv(t)
	t.Setenv("GH_ACME_CORP__PRIVATE_TEMPLATES", "fake-repo-cred")
	t.Setenv(credTestOwnerKey, "fake-owner-cred")
	sent := stubGitHubFetch(t, ".scion/templates/reviewer", "scion-agent.yaml", "harness: claude\n")

	cmd := newTemplateImportTestCmd(t)
	require.NoError(t, cmd.Flags().Set("dry-run", "true"))
	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runTemplateImport(cmd, []string{"https://github.com/acme-corp/private.templates/tree/main/.scion/templates/reviewer"})
	})
	require.NoError(t, runErr)
	assert.Contains(t, stdout, "claude", "dry run should report the fetched template")
	tarballPath, tarballAuth := sent()
	assert.Equal(t, "/repos/acme-corp/private.templates/tarball/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", tarballPath)
	assert.Equal(t, "Bearer fake-repo-cred", tarballAuth)
}

// TestCLI_UsesHTTPSProxyWithoutGit verifies that the default CLI GitHub fetch
// path uses remotefetch.NetworkSystem (honouring HTTPS_PROXY) and does not
// require git on PATH.
func TestCLI_UsesHTTPSProxyWithoutGit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "")

	var proxyHit bool
	var mu sync.Mutex
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxyHit = true
		mu.Unlock()
		// Refuse the CONNECT tunnel so the request fails fast without hitting
		// the real network, while proving NetworkSystem contacted HTTPS_PROXY.
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	t.Cleanup(proxySrv.Close)
	t.Setenv("HTTPS_PROXY", proxySrv.URL)
	t.Setenv("NO_PROXY", "")

	lim := githubfetch.ImportLocalLimits()
	lim.MaxDownload = 1 << 20
	_, err := config.FetchRemoteTemplateWithOptions(context.Background(),
		"https://github.com/acme/repo/tree/main/templates/demo",
		config.FetchOptions{GitHubLimits: &lim})
	require.Error(t, err)
	mu.Lock()
	gotHit := proxyHit
	mu.Unlock()
	assert.True(t, gotHit, "CLI NetworkSystem transport must route through HTTPS_PROXY")
}

// newTemplateImportTestCmd returns a command carrying the flags
// runTemplateImport reads, at their defaults.
func newTemplateImportTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "import"}
	cmd.Flags().String("harness", "", "")
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("force", false, "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().Bool("all", false, "")
	return cmd
}
