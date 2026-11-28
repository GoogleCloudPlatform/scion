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
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImportDownloader_RefusesInternalAddresses: the production download
// hook refuses loopback, private and link-local (metadata) addresses and
// plain http, and its error names neither the address nor a status.
func TestImportDownloader_RefusesInternalAddresses(t *testing.T) {
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal-body"))
	}))
	t.Cleanup(tls.Close)

	download := newImportDownloader(nil)
	for _, rawURL := range []string{
		tls.URL + "/t.tar.gz",
		"https://127.0.0.1/t.tar.gz",
		"https://10.0.0.1/t.tar.gz",
		"https://169.254.169.254/computeMetadata/v1/t.tar.gz",
		"https://[::1]/t.tar.gz",
		"http://example.com/t.tar.gz",
	} {
		_, err := download(context.Background(), rawURL, "")
		require.Error(t, err, rawURL)
		assert.Equal(t, errImportDownloadFailed, err, rawURL)
		_, err = download(context.Background(), rawURL, "Bearer tok")
		require.Error(t, err, rawURL)
		assert.NotContains(t, err.Error(), "internal-body")
		assert.NotContains(t, err.Error(), "500")
	}
}

func TestIsGitHubArchiveDownloadURL(t *testing.T) {
	assert.True(t, isGitHubArchiveDownloadURL("https://github.com/acme/repo/archive/main.tar.gz"))
	assert.True(t, isGitHubArchiveDownloadURL("https://GITHUB.COM/acme/repo/archive/refs/tags/v1.0.tar.gz"))
	assert.False(t, isGitHubArchiveDownloadURL("http://github.com/acme/repo/archive/main.tar.gz"))
	assert.False(t, isGitHubArchiveDownloadURL("https://codeload.github.com/acme/repo/tar.gz/main"))
	assert.False(t, isGitHubArchiveDownloadURL("https://example.com/archive/main.tar.gz"))
}

type stubImportBytesFetcher struct {
	urls []string
	body []byte
	err  error
}

func (f *stubImportBytesFetcher) FetchBytes(_ context.Context, rawURL string) (*remotefetch.Result, error) {
	f.urls = append(f.urls, rawURL)
	if f.err != nil {
		return nil, f.err
	}
	return &remotefetch.Result{Body: f.body}, nil
}

// TestNewImportDownloader_GitHubArchiveConfigAndDispatch pins the production
// githubArchive remotefetch.Config and verifies newImportDownloaderWithFetchers
// routes https://github.com/... archive URLs to the githubArchive fetcher and
// non-GitHub URLs to the no-redirect direct fetcher.
func TestNewImportDownloader_GitHubArchiveConfigAndDispatch(t *testing.T) {
	cfg := githubArchiveFetchConfig(nil)
	assert.Equal(t, []string{"github.com", "codeload.github.com"}, cfg.AllowedHosts)
	assert.Equal(t, 3, cfg.MaxRedirects)
	assert.Equal(t, remotefetch.NetworkVetted, cfg.Network)
	assert.False(t, cfg.DisableRedirects)

	direct := &stubImportBytesFetcher{body: []byte("direct-body")}
	ghArchive := &stubImportBytesFetcher{body: []byte("gh-archive-body")}
	dl := newImportDownloaderWithFetchers(direct, ghArchive)

	body, err := dl(context.Background(), "https://github.com/acme/repo/archive/main.tar.gz", "Bearer ignored-secret")
	require.NoError(t, err)
	assert.Equal(t, []byte("gh-archive-body"), body)
	assert.Equal(t, []string{"https://github.com/acme/repo/archive/main.tar.gz"}, ghArchive.urls)
	assert.Empty(t, direct.urls, "github.com archive URL must dispatch to githubArchive fetcher, not direct")

	body, err = dl(context.Background(), "https://artifacts.example.com/pkg.tar.gz", "Bearer ignored-secret")
	require.NoError(t, err)
	assert.Equal(t, []byte("direct-body"), body)
	assert.Equal(t, []string{"https://artifacts.example.com/pkg.tar.gz"}, direct.urls)
	assert.Len(t, ghArchive.urls, 1, "non-GitHub URL must dispatch to direct fetcher")
}
