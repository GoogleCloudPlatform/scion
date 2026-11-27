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
	"crypto/sha1" //nolint:gosec // git object IDs are SHA-1
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
)

const defaultFakeReimportSHA = "1111111111111111111111111111111111111111"

// fakeReimportGitHub is a test githubfetch.Doer that serves GitHub ref, tree,
// and tarball/archive responses derived from a gzip tarball body (or returns
// an injected error). It records all requested URLs and Authorization headers,
// and separates tarball/archive requests so tests can assert no-op reimports.
type fakeReimportGitHub struct {
	mu          sync.Mutex
	body        []byte
	err         error
	commitSHA   string // optional override; if empty, extracted from PAX header or defaultFakeReimportSHA
	treeSHA     string // optional override; if empty, derived from files in subPath
	branches    []string
	tags        []string
	urls        []string
	auths       []string
	tarballURLs []string
	tarballAuth []string
}

func newFakeReimportGitHubClient(t *testing.T, f *fakeReimportGitHub) *githubfetch.Client {
	t.Helper()
	return githubfetch.New(githubfetch.Options{
		Endpoint:   githubfetch.DefaultEndpoint(),
		Doer:       f,
		Cooldown:   githubfetch.NewCooldown(nil),
		MaxRetries: 0,
	})
}

func (f *fakeReimportGitHub) setBody(body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = body
}

func (f *fakeReimportGitHub) recordedTarballURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.tarballURLs))
	copy(out, f.tarballURLs)
	return out
}

func (f *fakeReimportGitHub) recordedTarballAuths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.tarballAuth))
	copy(out, f.tarballAuth)
	return out
}

func (f *fakeReimportGitHub) recordedURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.urls))
	copy(out, f.urls)
	return out
}

func (f *fakeReimportGitHub) recordedAuths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.auths))
	copy(out, f.auths)
	return out
}

func (f *fakeReimportGitHub) Open(_ context.Context, r githubfetch.Request) (*githubfetch.Response, error) {
	f.mu.Lock()
	f.urls = append(f.urls, r.URL)
	f.auths = append(f.auths, r.Authorization)
	body := f.body
	injectedErr := f.err
	commitOverride := f.commitSHA
	treeOverride := f.treeSHA
	branches := append([]string(nil), f.branches...)
	tags := append([]string(nil), f.tags...)
	f.mu.Unlock()

	if injectedErr != nil {
		return nil, injectedErr
	}

	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, err
	}

	// Determine effective commit SHA and ensure tarball has a valid PAX comment.
	sha, normalizedTar, files := inspectAndNormalizeArchive(body, commitOverride)

	isTarball := strings.Contains(u.Path, "/tarball/") || strings.HasSuffix(u.Path, ".tar.gz")
	if isTarball {
		f.mu.Lock()
		f.tarballURLs = append(f.tarballURLs, r.URL)
		f.tarballAuth = append(f.tarballAuth, r.Authorization)
		f.mu.Unlock()
		return &githubfetch.Response{
			Status: http.StatusOK,
			Header: make(http.Header),
			Body:   io.NopCloser(bytes.NewReader(normalizedTar)),
		}, nil
	}

	// GET /repos/{o}/{r}/git/matching-refs/heads/{first}
	if idx := strings.Index(u.Path, "/git/matching-refs/heads/"); idx >= 0 {
		first := u.Path[idx+len("/git/matching-refs/heads/"):]
		if len(branches) == 0 {
			branches = []string{first}
		}
		var entries []map[string]any
		for _, b := range branches {
			if strings.HasPrefix(b, first) {
				entries = append(entries, map[string]any{
					"ref":    "refs/heads/" + b,
					"object": map[string]string{"type": "commit", "sha": sha},
				})
			}
		}
		b, _ := json.Marshal(entries)
		return &githubfetch.Response{Status: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b))}, nil
	}

	// GET /repos/{o}/{r}/git/matching-refs/tags/{first}
	if idx := strings.Index(u.Path, "/git/matching-refs/tags/"); idx >= 0 {
		first := u.Path[idx+len("/git/matching-refs/tags/"):]
		var entries []map[string]any
		for _, tg := range tags {
			if strings.HasPrefix(tg, first) {
				entries = append(entries, map[string]any{
					"ref":    "refs/tags/" + tg,
					"object": map[string]string{"type": "commit", "sha": sha},
				})
			}
		}
		b, _ := json.Marshal(entries)
		return &githubfetch.Response{Status: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b))}, nil
	}

	// GET /repos/{o}/{r}/commits/{ref}
	if strings.Contains(u.Path, "/commits/") {
		return &githubfetch.Response{
			Status: http.StatusOK,
			Header: make(http.Header),
			Body:   io.NopCloser(strings.NewReader(sha + "\n")),
		}, nil
	}

	// GET /repos/{o}/{r}/git/trees/{sha}[:{subPath}]
	if idx := strings.Index(u.Path, "/git/trees/"); idx >= 0 {
		spec := u.Path[idx+len("/git/trees/"):]
		subPath := ""
		if _, after, ok := strings.Cut(spec, ":"); ok {
			subPath = strings.Trim(after, "/")
		}
		var treeEntries []map[string]any
		h := sha1.New() //nolint:gosec // synthetic git tree SHA for tests
		for _, af := range files {
			rel := af.rel
			if subPath != "" {
				if !strings.HasPrefix(rel, subPath+"/") {
					continue
				}
				rel = strings.TrimPrefix(rel, subPath+"/")
			}
			modeStr := "100644"
			if af.mode&0o111 != 0 {
				modeStr = "100755"
			}
			treeEntries = append(treeEntries, map[string]any{
				"path": rel,
				"mode": modeStr,
				"type": "blob",
				"sha":  af.blobSHA,
				"size": af.size,
			})
			_, _ = fmt.Fprintf(h, "%s %s %s\n", modeStr, rel, af.blobSHA)
		}
		tSHA := treeOverride
		if tSHA == "" {
			tSHA = hex.EncodeToString(h.Sum(nil))
		}
		respObj := map[string]any{
			"sha":       tSHA,
			"truncated": false,
			"tree":      treeEntries,
		}
		b, _ := json.Marshal(respObj)
		return &githubfetch.Response{Status: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b))}, nil
	}

	// GET /repos/{o}/{r}
	b, _ := json.Marshal(map[string]string{"default_branch": "main"})
	return &githubfetch.Response{Status: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b))}, nil
}

type fakeArchiveFile struct {
	rel     string
	mode    int64
	size    int64
	blobSHA string
}

func inspectAndNormalizeArchive(raw []byte, commitOverride string) (string, []byte, []fakeArchiveFile) {
	if len(raw) == 0 {
		sha := commitOverride
		if sha == "" {
			sha = defaultFakeReimportSHA
		}
		return sha, raw, nil
	}
	gzr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		sha := commitOverride
		if sha == "" {
			sha = defaultFakeReimportSHA
		}
		return sha, raw, nil
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)
	var (
		paxSHA     string
		files      []fakeArchiveFile
		rawEntries []struct {
			hdr  *tar.Header
			data []byte
		}
	)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			cand := strings.TrimSpace(hdr.PAXRecords["comment"])
			if githubfetch.IsFullCommitSHA(cand) {
				paxSHA = cand
			}
			continue
		}
		var data []byte
		if hdr.Typeflag == tar.TypeReg {
			data, _ = io.ReadAll(tr)
			trimmed := strings.TrimSuffix(strings.TrimPrefix(hdr.Name, "/"), "/")
			if _, rel, ok := strings.Cut(trimmed, "/"); ok && rel != "" {
				h := sha1.New() //nolint:gosec // git blob object ID
				_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
				_, _ = h.Write(data)
				files = append(files, fakeArchiveFile{
					rel:     rel,
					mode:    hdr.Mode,
					size:    int64(len(data)),
					blobSHA: hex.EncodeToString(h.Sum(nil)),
				})
			}
		}
		hdrCopy := *hdr
		rawEntries = append(rawEntries, struct {
			hdr  *tar.Header
			data []byte
		}{hdr: &hdrCopy, data: data})
	}

	sha := commitOverride
	if sha == "" {
		sha = paxSHA
	}
	if sha == "" {
		sha = defaultFakeReimportSHA
	}

	// Rebuild tarball with the valid PAX global header comment == sha.
	var out bytes.Buffer
	gzw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzw)
	_ = tw.WriteHeader(&tar.Header{
		Name:       "pax_global_header",
		Typeflag:   tar.TypeXGlobalHeader,
		PAXRecords: map[string]string{"comment": sha},
	})
	for _, e := range rawEntries {
		_ = tw.WriteHeader(e.hdr)
		if e.hdr.Typeflag == tar.TypeReg && len(e.data) > 0 {
			_, _ = tw.Write(e.data)
		}
	}
	_ = tw.Close()
	_ = gzw.Close()
	return sha, out.Bytes(), files
}
