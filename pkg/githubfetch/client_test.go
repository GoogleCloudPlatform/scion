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

package githubfetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // git blob object IDs are SHA-1
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
)

func gitBlobSHA(content []byte) string {
	h := sha1.New() //nolint:gosec // git blob object IDs are SHA-1
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(content))
	_, _ = h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

type tarTestEntry struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
	mode     int64
}

func buildTestTarGz(t *testing.T, paxSHA string, entries []tarTestEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if paxSHA != "" {
		if err := tw.WriteHeader(&tar.Header{
			Name:       "pax_global_header",
			Typeflag:   tar.TypeXGlobalHeader,
			PAXRecords: map[string]string{"comment": paxSHA},
		}); err != nil {
			t.Fatalf("write pax header: %v", err)
		}
	}
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			if tf == tar.TypeDir {
				mode = 0o755
			} else {
				mode = 0o644
			}
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: tf,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
		}
		if tf == tar.TypeDir || tf == tar.TypeSymlink || tf == tar.TypeLink || tf == tar.TypeChar || tf == tar.TypeBlock || tf == tar.TypeFifo {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %q: %v", e.name, err)
		}
		if tf == tar.TypeReg && len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("write tar body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// httpDoer adapts an *httptest.Server (with redirect following matching
// remotefetch's hop-0-only auth & AllowedHosts rules) as a Doer.
type httpDoer struct {
	client       *http.Client
	allowedHosts map[string]bool
	maxRedirects int
	requireHTTPS bool
}

func newHTTPDoer(client *http.Client, allowed []string) *httpDoer {
	m := make(map[string]bool, len(allowed))
	for _, h := range allowed {
		m[strings.ToLower(h)] = true
	}
	return &httpDoer{
		client:       client,
		allowedHosts: m,
		maxRedirects: 3,
	}
}

func (d *httpDoer) Open(ctx context.Context, r Request) (*Response, error) {
	current := r.URL
	authHost := ""
	for hop := 0; ; hop++ {
		u, err := url.Parse(current)
		if err != nil {
			return nil, &remotefetch.Error{Reason: remotefetch.ReasonBadURL, Detail: "bad url"}
		}
		if d.requireHTTPS && !strings.EqualFold(u.Scheme, "https") {
			return nil, &remotefetch.Error{Reason: remotefetch.ReasonScheme, Detail: "scheme not https"}
		}
		if len(d.allowedHosts) > 0 && !d.allowedHosts[strings.ToLower(u.Hostname())] {
			return nil, &remotefetch.Error{Reason: remotefetch.ReasonHostNotAllowed, Detail: "host not allowed"}
		}
		if hop == 0 {
			authHost = u.Hostname()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return nil, err
		}
		for k, vals := range r.Header {
			for _, v := range vals {
				req.Header.Add(k, v)
			}
		}
		if r.Authorization != "" && strings.EqualFold(u.Hostname(), authHost) {
			req.Header.Set("Authorization", r.Authorization)
		}
		c := &http.Client{
			Transport: d.client.Transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "" {
			loc := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if hop >= d.maxRedirects {
				return nil, &remotefetch.Error{Reason: remotefetch.ReasonRedirects, Detail: "too many redirects"}
			}
			next, err := u.Parse(loc)
			if err != nil {
				return nil, &remotefetch.Error{Reason: remotefetch.ReasonBadURL, Detail: "bad redirect"}
			}
			current = next.String()
			continue
		}
		return &Response{
			Status: resp.StatusCode,
			Header: resp.Header,
			Body:   resp.Body,
		}, nil
	}
}

type recordedReq struct {
	Host   string
	Path   string
	Query  string
	Accept string
	Auth   string
}

func TestResolveRef_AllForms(t *testing.T) {
	const (
		shaMain   = "1111111111111111111111111111111111111111"
		shaFeat   = "2222222222222222222222222222222222222222"
		shaTag    = "3333333333333333333333333333333333333333"
		shaTagObj = "4444444444444444444444444444444444444444"
		shaDirect = "5555555555555555555555555555555555555555"
	)

	var mu sync.Mutex
	var reqs []recordedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, recordedReq{
			Host:   r.Host,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Accept: r.Header.Get("Accept"),
			Auth:   r.Header.Get("Authorization"),
		})
		mu.Unlock()

		switch r.URL.EscapedPath() {
		case "/repos/acme/repo":
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
		case "/repos/acme/repo/commits/main":
			w.Header().Set("ETag", `"etag-main"`)
			_, _ = w.Write([]byte(shaMain + "\n"))
		case "/repos/acme/repo/commits/v1.0.0":
			_, _ = w.Write([]byte(shaTag + "\n"))
		case "/repos/acme/repo/commits/releases%2Fv2":
			_, _ = w.Write([]byte(shaTag + "\n"))
		case "/repos/acme/repo/git/matching-refs/heads/feat":
			// Both "feat/x" and "feat/x/y" exist; longest prefix "feat/x/y" must win!
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"ref": "refs/heads/feat/x", "object": map[string]string{"type": "commit", "sha": shaMain}},
				{"ref": "refs/heads/feat/x/y", "object": map[string]string{"type": "commit", "sha": shaFeat}},
				{"ref": "refs/heads/feat/x/y/z/extra", "object": map[string]string{"type": "commit", "sha": shaDirect}},
			})
		case "/repos/acme/repo/git/matching-refs/heads/releases":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/repo/git/matching-refs/tags/releases":
			// Annotated tag (object.type == "tag") must dereference via commits/{ref}.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"ref": "refs/tags/releases/v2", "object": map[string]string{"type": "tag", "sha": shaTagObj}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
	client := New(Options{
		Endpoint: ep,
		Doer:     newHTTPDoer(srv.Client(), nil),
		Cooldown: NewCooldown(nil),
	})
	cred := Credential{Token: "ghp_secret", Scope: "secret:abc", Kind: CredSecret}

	t.Run("full 40-hex SHA makes zero HTTP calls", func(t *testing.T) {
		mu.Lock()
		reqs = nil
		mu.Unlock()

		src, err := ParseURL("https://github.com/acme/repo/tree/" + shaDirect + "/templates/foo")
		if err != nil {
			t.Fatal(err)
		}
		res, gotSrc, err := client.ResolveRef(context.Background(), src, cred)
		if err != nil {
			t.Fatalf("ResolveRef: %v", err)
		}
		if res.CommitSHA != shaDirect || res.Kind != RefSHA || gotSrc.Path != "templates/foo" {
			t.Fatalf("got res=%+v src=%+v", res, gotSrc)
		}
		mu.Lock()
		n := len(reqs)
		mu.Unlock()
		if n != 0 {
			t.Fatalf("expected 0 HTTP calls for full SHA, got %d", n)
		}
	})

	t.Run("empty ref resolves default branch", func(t *testing.T) {
		src, _ := ParseURL("https://github.com/acme/repo")
		res, gotSrc, err := client.ResolveRef(context.Background(), src, cred)
		if err != nil {
			t.Fatalf("ResolveRef: %v", err)
		}
		if res.CommitSHA != shaMain || res.Kind != RefDefault || gotSrc.Ref != "main" || res.ETag != `"etag-main"` {
			t.Fatalf("got res=%+v src=%+v", res, gotSrc)
		}
	})

	t.Run("unambiguous branch or tag", func(t *testing.T) {
		src, _ := ParseURL("https://github.com/acme/repo/tree/v1.0.0")
		res, _, err := client.ResolveRef(context.Background(), src, cred)
		if err != nil {
			t.Fatalf("ResolveRef: %v", err)
		}
		if res.CommitSHA != shaTag {
			t.Fatalf("CommitSHA = %q, want %q", res.CommitSHA, shaTag)
		}
	})

	t.Run("slashed branch feat/x/y wins longest prefix over feat/x", func(t *testing.T) {
		src, _ := ParseURL("https://github.com/acme/repo/tree/feat/x/y/templates/app")
		res, gotSrc, err := client.ResolveRef(context.Background(), src, cred)
		if err != nil {
			t.Fatalf("ResolveRef: %v", err)
		}
		if res.CommitSHA != shaFeat || res.Ref != "feat/x/y" || gotSrc.Ref != "feat/x/y" || gotSrc.Path != "templates/app" {
			t.Fatalf("got res=%+v src=%+v", res, gotSrc)
		}
	})

	t.Run("slashed annotated tag falls back from heads to tags and dereferences commit", func(t *testing.T) {
		src, _ := ParseURL("https://github.com/acme/repo/tree/releases/v2/templates/app")
		res, gotSrc, err := client.ResolveRef(context.Background(), src, cred)
		if err != nil {
			t.Fatalf("ResolveRef: %v", err)
		}
		if res.CommitSHA != shaTag || res.Kind != RefTag || gotSrc.Ref != "releases/v2" || gotSrc.Path != "templates/app" {
			t.Fatalf("got res=%+v src=%+v", res, gotSrc)
		}
	})
}

func TestFetchFolder_AnonVsCredAndRedirectAuthStripping(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const treeSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const secretToken = "ghs_super_secret_token_9549"

	yamlBody := []byte("schema_version: \"1\"\nharness: claude\n")
	scriptBody := []byte("#!/bin/sh\necho hi\n")
	tarBytes := buildTestTarGz(t, sha, []tarTestEntry{
		{name: "acme-repo-aaa/templates/my-app/scion-agent.yaml", body: yamlBody, mode: 0o664},
		{name: "acme-repo-aaa/templates/my-app/hooks/run.sh", body: scriptBody, mode: 0o777},
		{name: "acme-repo-aaa/other/ignored.txt", body: []byte("outside")},
	})

	treeJSON, _ := json.Marshal(map[string]any{
		"sha":       treeSHA,
		"truncated": false,
		"tree": []map[string]any{
			{"path": "scion-agent.yaml", "mode": "100644", "type": "blob", "sha": gitBlobSHA(yamlBody), "size": len(yamlBody)},
			{"path": "hooks/run.sh", "mode": "100755", "type": "blob", "sha": gitBlobSHA(scriptBody), "size": len(scriptBody)},
		},
	})

	t.Run("anonymous uses archive URL and sends no Authorization", func(t *testing.T) {
		var seen []recordedReq
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, recordedReq{Path: r.URL.Path, Auth: r.Header.Get("Authorization")})
			switch r.URL.EscapedPath() {
			case "/repos/acme/repo/git/matching-refs/heads/main":
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"ref": "refs/heads/main", "object": map[string]string{"type": "commit", "sha": sha}},
				})
			case "/repos/acme/repo/git/trees/" + sha + ":templates/my-app":
				_, _ = w.Write(treeJSON)
			case "/acme/repo/archive/" + sha + ".tar.gz":
				_, _ = w.Write(tarBytes)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)

		ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
		client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: NewCooldown(nil)})

		src, _ := ParseURL("https://github.com/acme/repo/tree/main/templates/my-app")
		destDir := t.TempDir()
		root, err := os.OpenRoot(destDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		res, err := client.FetchFolder(context.Background(), src, Credential{Kind: CredAnonymous}, root, ImportHostedLimits())
		if err != nil {
			t.Fatalf("FetchFolder anon: %v", err)
		}
		if res.CommitSHA != sha || res.TreeSHA != treeSHA || res.Files != 2 || res.Strategy != "tarball" {
			t.Fatalf("unexpected result: %+v", res)
		}
		var sawArchive bool
		for _, r := range seen {
			if r.Auth != "" {
				t.Errorf("anon request %q sent Authorization %q", r.Path, r.Auth)
			}
			if r.Path == "/acme/repo/archive/"+sha+".tar.gz" {
				sawArchive = true
			}
		}
		if !sawArchive {
			t.Fatalf("expected archive URL request, saw %+v", seen)
		}
		// Verify normalized file modes and that outside/ignored.txt was not extracted.
		info, err := os.Stat(filepath.Join(destDir, "hooks/run.sh"))
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("run.sh mode = %v, err = %v", info.Mode(), err)
		}
		info, err = os.Stat(filepath.Join(destDir, "scion-agent.yaml"))
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Errorf("scion-agent.yaml mode = %v, err = %v", info.Mode(), err)
		}
		if _, err := os.Stat(filepath.Join(destDir, "other/ignored.txt")); !os.IsNotExist(err) {
			t.Errorf("outside file was extracted")
		}
	})

	// Ported from PR #9548: credentialed redirect to codeload, subdomain, and
	// http:// downgrade must NEVER forward Authorization, non-allowlisted
	// redirect fails as unreachable, and >3 redirects fail.
	t.Run("credentialed redirect strips auth and enforces allowlist and hop cap", func(t *testing.T) {
		var codeloadAuth string
		codeloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			codeloadAuth = r.Header.Get("Authorization")
			_, _ = w.Write(tarBytes)
		}))
		t.Cleanup(codeloadSrv.Close)
		codeloadURL, _ := url.Parse(codeloadSrv.URL)

		var apiTarballAuth string
		var redirectTarget string
		apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.EscapedPath() {
			case "/repos/acme/private/git/trees/" + sha + ":templates/my-app":
				_, _ = w.Write(treeJSON)
			case "/repos/acme/private/tarball/" + sha:
				apiTarballAuth = r.Header.Get("Authorization")
				http.Redirect(w, r, redirectTarget, http.StatusFound)
			case "/hop1":
				http.Redirect(w, r, "/hop2", http.StatusFound)
			case "/hop2":
				http.Redirect(w, r, "/hop3", http.StatusFound)
			case "/hop3":
				http.Redirect(w, r, "/hop4", http.StatusFound)
			case "/hop4":
				http.Redirect(w, r, "/hop5", http.StatusFound)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(apiSrv.Close)
		apiURL, _ := url.Parse(apiSrv.URL)

		combinedTransport := &routingRoundTripper{
			routes: map[string]*url.URL{
				"api.github.com":      apiURL,
				"codeload.github.com": codeloadURL,
			},
			base: http.DefaultTransport,
		}
		combinedClient := &http.Client{Transport: combinedTransport}

		cred := Credential{Token: secretToken, Scope: "secret:test", Kind: CredSecret}
		src, _ := ParseURL("https://github.com/acme/private/tree/" + sha + "/templates/my-app")

		// 1. Redirect api.github.com -> codeload.github.com succeeds and strips Authorization.
		redirectTarget = "https://codeload.github.com/acme/private/legacy.tar.gz?token=short-lived-codeload-secret"
		ep := DefaultEndpoint()
		doer := newHTTPDoer(combinedClient, ep.AllowedHosts())
		doer.requireHTTPS = true
		client := New(Options{
			Endpoint: ep,
			Doer:     doer,
			Cooldown: NewCooldown(nil),
		})
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		_, err = client.FetchFolder(context.Background(), src, cred, root, ImportHostedLimits())
		if err != nil {
			t.Fatalf("codeload redirect FetchFolder: %v", err)
		}
		if apiTarballAuth != "Bearer "+secretToken {
			t.Errorf("api tarball auth = %q", apiTarballAuth)
		}
		if codeloadAuth != "" {
			t.Errorf("Authorization leaked to codeload: %q", codeloadAuth)
		}

		// 2. Redirect to non-allowlisted subdomain (sub.api.github.com) fails as unreachable.
		redirectTarget = "https://sub.api.github.com/evil/tarball"
		root2, _ := os.OpenRoot(t.TempDir())
		defer func() { _ = root2.Close() }()
		_, err = client.FetchFolder(context.Background(), src, cred, root2, ImportHostedLimits())
		var ghe *Error
		if !errors.As(err, &ghe) || ghe.Kind != KindUnreachable {
			t.Fatalf("non-allowlisted redirect err = %v, want KindUnreachable", err)
		}
		if strings.Contains(err.Error(), secretToken) {
			t.Errorf("token leaked in error: %v", err)
		}

		// 3. Redirect downgrading to plain http:// fails as unreachable.
		redirectTarget = "http://api.github.com/repos/acme/private/tarball/" + sha
		root3, _ := os.OpenRoot(t.TempDir())
		defer func() { _ = root3.Close() }()
		_, err = client.FetchFolder(context.Background(), src, cred, root3, ImportHostedLimits())
		if !errors.As(err, &ghe) || ghe.Kind != KindUnreachable {
			t.Fatalf("http downgrade redirect err = %v, want KindUnreachable", err)
		}

		// 4. >3 redirects fails as unreachable.
		redirectTarget = "https://api.github.com/hop1"
		root4, _ := os.OpenRoot(t.TempDir())
		defer func() { _ = root4.Close() }()
		_, err = client.FetchFolder(context.Background(), src, cred, root4, ImportHostedLimits())
		if !errors.As(err, &ghe) || ghe.Kind != KindUnreachable {
			t.Fatalf(">3 redirects err = %v, want KindUnreachable", err)
		}
	})
}

type routingRoundTripper struct {
	routes map[string]*url.URL
	base   http.RoundTripper
}

func (rt *routingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if target, ok := rt.routes[r.URL.Hostname()]; ok {
		out := r.Clone(r.Context())
		out.URL.Scheme = target.Scheme
		out.URL.Host = target.Host
		out.Host = r.URL.Host
		return rt.base.RoundTrip(out)
	}
	return nil, fmt.Errorf("unexpected host %q", r.URL.Host)
}

func TestExtractTarGz_SecurityAndIntegrity(t *testing.T) {
	const sha = "cccccccccccccccccccccccccccccccccccccccc"
	content := []byte("hello")
	validTree := &Tree{
		TreeSHA: "dddddddddddddddddddddddddddddddddddddddd",
		Files:   []File{{Path: "a.txt", Mode: 0o644, Size: int64(len(content)), BlobSHA: gitBlobSHA(content)}},
	}

	t.Run("dot-dot path rejected", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/../escape.txt", body: []byte("boom")},
		})
		root, _ := os.OpenRoot(t.TempDir())
		defer func() { _ = root.Close() }()
		_, _, _, err := extractTarGz(bytes.NewReader(tarData), "acme/repo", "", sha, nil, root, ImportHostedLimits())
		assertErrorKind(t, err, KindUnsupportedContent)
	})

	t.Run("absolute path rejected", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "/etc/passwd", body: []byte("root:x:0:0")},
		})
		root, _ := os.OpenRoot(t.TempDir())
		defer func() { _ = root.Close() }()
		_, _, _, err := extractTarGz(bytes.NewReader(tarData), "acme/repo", "", sha, nil, root, ImportHostedLimits())
		assertErrorKind(t, err, KindUnsupportedContent)
	})

	t.Run("symlink and hardlink skipped and counted", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/t/a.txt", body: content},
			{name: "repo-sha/t/sym", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
			{name: "repo-sha/t/hard", typeflag: tar.TypeLink, linkname: "repo-sha/t/a.txt"},
			{name: "repo-sha/t/.git/config", body: []byte("ignored")},
		})
		dir := t.TempDir()
		root, _ := os.OpenRoot(dir)
		defer func() { _ = root.Close() }()
		files, _, skipped, err := extractTarGz(bytes.NewReader(tarData), "acme/repo", "t", sha, validTree, root, ImportHostedLimits())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if files != 1 || skipped.Symlinks != 1 || skipped.Hardlinks != 1 || skipped.DotGit != 1 {
			t.Fatalf("got files=%d skipped=%+v", files, skipped)
		}
		if _, err := os.Lstat(filepath.Join(dir, "sym")); !os.IsNotExist(err) {
			t.Errorf("symlink was written")
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
			t.Errorf(".git was written")
		}
	})

	t.Run("device and FIFO refused", func(t *testing.T) {
		for _, tf := range []byte{tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
			tarData := buildTestTarGz(t, sha, []tarTestEntry{
				{name: "repo-sha/t/dev", typeflag: tf},
			})
			root, _ := os.OpenRoot(t.TempDir())
			_, _, _, err := extractTarGz(bytes.NewReader(tarData), "acme/repo", "t", sha, nil, root, ImportHostedLimits())
			_ = root.Close()
			assertErrorKind(t, err, KindUnsupportedContent)
		}
	})

	t.Run("oversized name rejected", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/t/" + strings.Repeat("a", 80), body: []byte("x")},
		})
		lim := ImportHostedLimits()
		lim.MaxNameBytes = 64
		root, _ := os.OpenRoot(t.TempDir())
		defer func() { _ = root.Close() }()
		_, _, _, err := extractTarGz(bytes.NewReader(tarData), "acme/repo", "t", sha, nil, root, lim)
		assertErrorKind(t, err, KindTooLarge)
	})

	t.Run("gzip bomb over MaxUnpacked rejected", func(t *testing.T) {
		big := bytes.Repeat([]byte{0}, 128*1024)
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/t/big.bin", body: big},
		})
		lim := ImportHostedLimits()
		lim.MaxUnpacked = 16 * 1024
		root, _ := os.OpenRoot(t.TempDir())
		defer func() { _ = root.Close() }()
		_, _, _, err := extractTarGz(bytes.NewReader(tarData), "acme/repo", "t", sha, nil, root, lim)
		assertErrorKind(t, err, KindTooLarge)
	})

	t.Run("PAX SHA mismatch rejected", func(t *testing.T) {
		tarData := buildTestTarGz(t, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", []tarTestEntry{
			{name: "repo-sha/t/a.txt", body: content},
		})
		root, _ := os.OpenRoot(t.TempDir())
		defer func() { _ = root.Close() }()
		_, _, _, err := extractTarGz(bytes.NewReader(tarData), "acme/repo", "t", sha, validTree, root, ImportHostedLimits())
		assertErrorKind(t, err, KindUpstream)
	})

	t.Run("blob SHA mismatch rejected", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/t/a.txt", body: []byte("tampered!")},
		})
		root, _ := os.OpenRoot(t.TempDir())
		defer func() { _ = root.Close() }()
		_, _, _, err := extractTarGz(bytes.NewReader(tarData), "acme/repo", "t", sha, validTree, root, ImportHostedLimits())
		assertErrorKind(t, err, KindUpstream)
	})
}

func TestListTree_TruncatedAndOversizedFailBeforeDownload(t *testing.T) {
	const sha = "1234567890abcdef1234567890abcdef12345678"

	cases := []struct {
		name     string
		treeResp map[string]any
		lim      Limits
		wantKind Kind
	}{
		{
			name:     "truncated tree fails before download",
			treeResp: map[string]any{"sha": "aaa", "truncated": true, "tree": []any{}},
			lim:      ImportHostedLimits(),
			wantKind: KindTooLarge,
		},
		{
			name: "too many files fails before download",
			treeResp: map[string]any{
				"sha": "aaa",
				"tree": []map[string]any{
					{"path": "a.txt", "mode": "100644", "type": "blob", "sha": "111", "size": 1},
					{"path": "b.txt", "mode": "100644", "type": "blob", "sha": "222", "size": 1},
				},
			},
			lim:      Limits{MaxFiles: 1, MaxFileBytes: 1024, MaxTotal: 1024},
			wantKind: KindTooLarge,
		},
		{
			name: "submodule rejected when PolicyReject",
			treeResp: map[string]any{
				"sha": "aaa",
				"tree": []map[string]any{
					{"path": "sub", "mode": "160000", "type": "commit", "sha": "111"},
				},
			},
			lim:      SkillLimits(),
			wantKind: KindUnsupportedContent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tarballHits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/tarball/") || strings.Contains(r.URL.Path, "/archive/") {
					tarballHits++
				}
				_ = json.NewEncoder(w).Encode(tc.treeResp)
			}))
			t.Cleanup(srv.Close)

			ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
			client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: NewCooldown(nil)})
			src := Source{Owner: "acme", Repo: "repo", Ref: sha, Path: "t"}
			root, _ := os.OpenRoot(t.TempDir())
			defer func() { _ = root.Close() }()

			_, err := client.FetchFolder(context.Background(), src, Credential{}, root, tc.lim)
			assertErrorKind(t, err, tc.wantKind)
			if tarballHits != 0 {
				t.Fatalf("tarballHits = %d, want 0 (must fail before download)", tarballHits)
			}
		})
	}
}

func TestErrorClassificationAndNoTokenLeak(t *testing.T) {
	const secretToken = "ghp_never_leak_this_token_xyz"
	now := time.Now()
	anonCred := Credential{Kind: CredAnonymous, Scope: AnonScope}
	secretCred := Credential{Token: secretToken, Kind: CredSecret, Scope: "secret:1234", Hint: "app token mint failed; fell back to GITHUB_TOKEN"}

	cases := []struct {
		name     string
		status   int
		headers  map[string]string
		body     string
		cred     Credential
		wantKind Kind
		wantSub  string
	}{
		{"404 anon", 404, nil, "", anonCred, KindNotFound, "no credential is configured"},
		{"404 cred", 404, nil, "", secretCred, KindNotFound, "configured credential cannot read it"},
		{"401 cred", 401, nil, "", secretCred, KindAuthRejected, "app token mint failed"},
		{"403 SSO", 403, map[string]string{"X-GitHub-SSO": "required; url=https://github.com/orgs/acme/sso"}, "", secretCred, KindForbidden, "SAML SSO"},
		{"403 plain", 403, nil, "forbidden", secretCred, KindForbidden, "denied access"},
		{"403 rate limit header", 403, map[string]string{"X-RateLimit-Remaining": "0", "Retry-After": "45"}, "", secretCred, KindRateLimited, "rate limit"},
		{"403 secondary rate limit", 403, nil, "You have exceeded a secondary rate limit", secretCred, KindRateLimited, "rate limit"},
		{"429", 429, map[string]string{"Retry-After": "30"}, "", secretCred, KindRateLimited, "rate limit"},
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := make(http.Header)
			for k, v := range tc.headers {
				hdr.Set(k, v)
			}
			ghe := ClassifyStatus(tc.status, hdr, []byte(tc.body), "acme/repo", tc.cred, now)
			if ghe.Kind != tc.wantKind {
				t.Fatalf("Kind = %s, want %s", ghe.Kind, tc.wantKind)
			}
			msg := ghe.Error()
			if !strings.Contains(msg, tc.wantSub) {
				t.Errorf("Error() = %q, want substring %q", msg, tc.wantSub)
			}
			logger.Warn("classified", "error", ghe)
			if strings.Contains(msg, secretToken) || strings.Contains(logBuf.String(), secretToken) {
				t.Fatalf("secret token leaked")
			}
		})
	}
}

func TestCooldown_ScopeIsolation(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	cd := NewCooldown(clock)

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") == "Bearer tok-a" {
			w.Header().Set("Retry-After", "90")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("1111111111111111111111111111111111111111\n"))
	}))
	t.Cleanup(srv.Close)

	ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
	client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: cd, Clock: clock})

	credA := Credential{Token: "tok-a", Scope: "secret:aaa", Kind: CredSecret}
	credB := Credential{Token: "tok-b", Scope: "secret:bbb", Kind: CredSecret}
	src := Source{Owner: "acme", Repo: "repo", Ref: "main"}

	// First call for scope A gets 429 and starts cooldown.
	_, _, err := client.ResolveRef(context.Background(), src, credA)
	assertErrorKind(t, err, KindRateLimited)
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}

	// Second call for scope A is blocked locally with 0 HTTP hits.
	_, _, err = client.ResolveRef(context.Background(), src, credA)
	assertErrorKind(t, err, KindRateLimited)
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (cooldown should block before HTTP)", hits)
	}

	// Scope B is unaffected and succeeds.
	res, _, err := client.ResolveRef(context.Background(), src, credB)
	if err != nil {
		t.Fatalf("scope B should not be rate limited: %v", err)
	}
	if res.CommitSHA != "1111111111111111111111111111111111111111" {
		t.Fatalf("unexpected SHA: %q", res.CommitSHA)
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}
}

// TestExtractTarGz_StreamingDoesNotBufferWholeArchive verifies that streaming a
// 48 MiB tarball through extractTarGz keeps peak live heap well below the
// tarball's size (entries outside subPath are discarded on the fly).
func TestExtractTarGz_StreamingDoesNotBufferWholeArchive(t *testing.T) {
	const sha = "9999999999999999999999999999999999999999"
	const payloadSize = 48 << 20 // 48 MiB streamed outside subPath

	pr, pw := io.Pipe()
	go func() {
		gzw, _ := gzip.NewWriterLevel(pw, gzip.BestSpeed)
		tw := tar.NewWriter(gzw)
		_ = tw.WriteHeader(&tar.Header{
			Name:       "pax_global_header",
			Typeflag:   tar.TypeXGlobalHeader,
			PAXRecords: map[string]string{"comment": sha},
		})
		// Stream a 48 MiB file outside the target subPath in 32 KiB chunks.
		_ = tw.WriteHeader(&tar.Header{
			Name:     "repo-sha/other/huge.bin",
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     payloadSize,
		})
		chunk := make([]byte, 32*1024)
		for rem := payloadSize; rem > 0; {
			n := len(chunk)
			if rem < n {
				n = rem
			}
			if _, err := tw.Write(chunk[:n]); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			rem -= n
		}
		// One small file inside the target subPath.
		small := []byte("schema_version: \"1\"\n")
		_ = tw.WriteHeader(&tar.Header{
			Name:     "repo-sha/templates/app/scion-agent.yaml",
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(small)),
		})
		_, _ = tw.Write(small)
		_ = tw.Close()
		_ = gzw.Close()
		_ = pw.Close()
	}()

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	files, written, _, err := extractTarGz(pr, "acme/repo", "templates/app", sha, nil, root, ImportHostedLimits())
	if err != nil {
		t.Fatalf("streaming extractTarGz: %v", err)
	}
	if files != 1 || written != 20 {
		t.Fatalf("got files=%d written=%d", files, written)
	}

	runtime.GC()
	runtime.ReadMemStats(&after)
	// Peak retained heap growth must be nowhere near the 48 MiB streamed archive.
	var growth uint64
	if after.HeapAlloc > before.HeapAlloc {
		growth = after.HeapAlloc - before.HeapAlloc
	}
	if growth > 16<<20 {
		t.Fatalf("heap grew by %d bytes for a %d-byte streamed archive; expected streaming without buffering", growth, payloadSize)
	}
}

func assertErrorKind(t *testing.T, err error, want Kind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error kind %s, got nil", want)
	}
	var ghe *Error
	if !errors.As(err, &ghe) {
		t.Fatalf("expected *githubfetch.Error, got %T: %v", err, err)
	}
	if ghe.Kind != want {
		t.Fatalf("error kind = %s, want %s (err: %v)", ghe.Kind, want, err)
	}
}
