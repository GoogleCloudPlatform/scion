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
