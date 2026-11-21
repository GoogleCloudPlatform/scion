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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Hosted server-side imports download through the SSRF-safe remotefetch hook,
// which refuses loopback. The pkg/hub tests serve their sources from
// loopback test servers or mock http.DefaultClient's transport, so TestMain
// installs testImportDownload as importDownloadOverride
// (installTestImportDownload). The import source policy still applies: a
// test server is reached under an https://github.com/... URL registered with
// routeImportTestServer.

var (
	importTestRoutes   sync.Map // "https://github.com/scion-import-test/<n>" -> real base URL
	importTestRouteSeq atomic.Int64
)

// installTestImportDownload is called from TestMain.
func installTestImportDownload() {
	importDownloadOverride = testImportDownload
}

// testImportDownload downloads with http.DefaultClient (so transport mocks
// apply), after mapping a routed test URL to its test server.
func testImportDownload(ctx context.Context, rawURL, authorization string) ([]byte, error) {
	target := rawURL
	importTestRoutes.Range(func(k, v any) bool {
		if prefix := k.(string); strings.HasPrefix(rawURL, prefix+"/") {
			target = v.(string) + strings.TrimPrefix(rawURL, prefix)
			return false
		}
		return true
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, importMaxDownloadBytes))
}

// routeImportTestServer makes srv reachable by hub imports under an
// https://github.com/scion-import-test/<n> URL, which it stores in srv.URL,
// and returns srv's real base URL.
func routeImportTestServer(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	real := srv.URL
	prefix := fmt.Sprintf("https://github.com/scion-import-test/%d", importTestRouteSeq.Add(1))
	importTestRoutes.Store(prefix, real)
	t.Cleanup(func() { importTestRoutes.Delete(prefix) })
	srv.URL = prefix
	return real
}
