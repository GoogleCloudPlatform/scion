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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // git blob object IDs are SHA-1
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
)

// Hosted server-side imports download through the SSRF-safe remotefetch hook,
// which refuses loopback. The pkg/hub tests serve their sources from
// loopback test servers or mock http.DefaultClient's transport, so TestMain
// installs testImportDownload as importDownloadOverride and a DefaultClient-
// backed githubfetch.Client as importGitHubClientOverride
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
	importGitHubClientOverride = githubfetch.New(githubfetch.Options{
		Endpoint:   githubfetch.DefaultEndpoint(),
		Doer:       &testImportGitHubDoer{},
		Cooldown:   githubfetch.NewCooldown(nil),
		MaxRetries: 0,
	})
}

// testImportDownload downloads non-GitHub archive URLs with http.DefaultClient
// (so transport mocks apply), after mapping a routed test URL to its test server.
func testImportDownload(ctx context.Context, rawURL, authorization string) ([]byte, error) {
	target := rewriteImportTestRoute(rawURL)
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

func rewriteImportTestRoute(rawURL string) string {
	target := rawURL
	importTestRoutes.Range(func(k, v any) bool {
		if prefix := k.(string); strings.HasPrefix(rawURL, prefix+"/") {
			target = v.(string) + strings.TrimPrefix(rawURL, prefix)
			return false
		}
		return true
	})
	return target
}

const legacyTestDefaultSHA = "4444444444444444444444444444444444444444"

// testImportGitHubDoer adapts http.DefaultClient (including tests that install
// a tarball-only mockRoundTripper on http.DefaultClient.Transport) to
// githubfetch.Doer. Tests that want full fake-GitHub control set
// srv.templateGitHubClient directly.
type testImportGitHubDoer struct{}

func (d *testImportGitHubDoer) Open(ctx context.Context, r githubfetch.Request) (*githubfetch.Response, error) {
	target := rewriteImportTestRoute(r.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, &remotefetch.Error{
			Reason: remotefetch.ReasonStatus,
			Detail: fmt.Sprintf("status %d", resp.StatusCode),
			Status: resp.StatusCode,
			Header: resp.Header,
			Body:   snippet,
		}
	}

	// Peek at the first two bytes to detect legacy tarball-only mockRoundTrippers
	// that return a gzip tarball regardless of the GitHub API endpoint requested.
	bodyBytes, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	isGzip := len(bodyBytes) >= 2 && bodyBytes[0] == 0x1f && bodyBytes[1] == 0x8b
	if !isGzip {
		return &githubfetch.Response{
			Status: resp.StatusCode,
			Header: resp.Header,
			Body:   io.NopCloser(bytes.NewReader(bodyBytes)),
		}, nil
	}

	u, _ := url.Parse(r.URL)
	p := u.Path
	switch {
	case strings.Contains(p, "/git/matching-refs/heads/"):
		idx := strings.Index(p, "/git/matching-refs/heads/")
		firstSeg := p[idx+len("/git/matching-refs/heads/"):]
		sha := extractLegacyTarballSHA(bodyBytes)
		payload, _ := json.Marshal([]map[string]any{
			{"ref": "refs/heads/" + firstSeg, "object": map[string]any{"type": "commit", "sha": sha}},
		})
		return &githubfetch.Response{Status: http.StatusOK, Header: resp.Header, Body: io.NopCloser(bytes.NewReader(payload))}, nil
	case strings.Contains(p, "/commits/"):
		sha := extractLegacyTarballSHA(bodyBytes)
		return &githubfetch.Response{Status: http.StatusOK, Header: resp.Header, Body: io.NopCloser(strings.NewReader(sha))}, nil
	case strings.Contains(p, "/repos/") && !strings.Contains(p, "/git/") && !strings.Contains(p, "/tarball/"):
		payload := []byte(`{"default_branch":"main"}`)
		return &githubfetch.Response{Status: http.StatusOK, Header: resp.Header, Body: io.NopCloser(bytes.NewReader(payload))}, nil
	case strings.Contains(p, "/git/trees/"):
		idx := strings.Index(p, "/git/trees/")
		treeish := p[idx+len("/git/trees/"):]
		subPath := ""
		if _, after, ok := strings.Cut(treeish, ":"); ok {
			subPath, _ = url.PathUnescape(after)
		}
		payload := synthesizeTreeFromTarball(bodyBytes, subPath)
		return &githubfetch.Response{Status: http.StatusOK, Header: resp.Header, Body: io.NopCloser(bytes.NewReader(payload))}, nil
	case strings.Contains(p, "/tarball/") || strings.Contains(p, "/archive/"):
		if !tarballHasPAXSHA(bodyBytes) {
			bodyBytes = prependPAXSHAToTarball(bodyBytes, legacyTestDefaultSHA)
		}
		return &githubfetch.Response{Status: http.StatusOK, Header: resp.Header, Body: io.NopCloser(bytes.NewReader(bodyBytes))}, nil
	default:
		return &githubfetch.Response{Status: resp.StatusCode, Header: resp.Header, Body: io.NopCloser(bytes.NewReader(bodyBytes))}, nil
	}
}

func extractLegacyTarballSHA(gz []byte) string {
	gzr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return legacyTestDefaultSHA
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			if c := strings.TrimSpace(hdr.PAXRecords["comment"]); githubfetch.IsFullCommitSHA(c) {
				return c
			}
		}
	}
	return legacyTestDefaultSHA
}

func tarballHasPAXSHA(gz []byte) bool {
	gzr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return false
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader && githubfetch.IsFullCommitSHA(strings.TrimSpace(hdr.PAXRecords["comment"])) {
			return true
		}
	}
	return false
}

func prependPAXSHAToTarball(gz []byte, sha string) []byte {
	gzr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return gz
	}
	defer func() { _ = gzr.Close() }()
	var out bytes.Buffer
	gzw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzw)
	_ = tw.WriteHeader(&tar.Header{
		Typeflag:   tar.TypeXGlobalHeader,
		Name:       "pax_global_header",
		PAXRecords: map[string]string{"comment": sha},
	})
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(tr)
		_ = tw.WriteHeader(hdr)
		if len(body) > 0 {
			_, _ = tw.Write(body)
		}
	}
	_ = tw.Close()
	_ = gzw.Close()
	return out.Bytes()
}

func synthesizeTreeFromTarball(gz []byte, subPath string) []byte {
	subPath = strings.Trim(subPath, "/")
	gzr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return []byte(`{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","truncated":false,"tree":[]}`)
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(gzr)
	var entries []map[string]any
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		trimmed := strings.TrimSuffix(hdr.Name, "/")
		_, rest, ok := strings.Cut(trimmed, "/")
		if !ok || rest == "" {
			continue
		}
		var rel string
		if subPath == "" {
			rel = rest
		} else if strings.HasPrefix(rest, subPath+"/") {
			rel = strings.TrimPrefix(rest, subPath+"/")
		} else {
			continue
		}
		body, _ := io.ReadAll(tr)
		h := sha1.New() //nolint:gosec // git blob object IDs are SHA-1
		_, _ = fmt.Fprintf(h, "blob %d\x00", len(body))
		_, _ = h.Write(body)
		mode := "100644"
		if hdr.Mode&0o111 != 0 {
			mode = "100755"
		}
		entries = append(entries, map[string]any{
			"path": rel,
			"mode": mode,
			"type": "blob",
			"sha":  hex.EncodeToString(h.Sum(nil)),
			"size": len(body),
		})
	}
	payload, _ := json.Marshal(map[string]any{
		"sha":       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"truncated": false,
		"tree":      entries,
	})
	return payload
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
