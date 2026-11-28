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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEndpoint_RawURL_PerSegmentEscaping(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	ep := DefaultEndpoint()

	src := Source{Owner: "my org", Repo: "my#repo", Path: "templates/my app"}
	got := ep.RawURL(src, sha, "hooks/run #1?.sh")
	want := "https://raw.githubusercontent.com/my%20org/my%23repo/" + sha + "/templates/my%20app/hooks/run%20%231%3F.sh"
	if got != want {
		t.Fatalf("RawURL = %q, want %q", got, want)
	}

	// Empty Source.Path
	srcRoot := Source{Owner: "acme", Repo: "repo"}
	gotRoot := ep.RawURL(srcRoot, sha, "dir with spaces/file%20name.txt")
	wantRoot := "https://raw.githubusercontent.com/acme/repo/" + sha + "/dir%20with%20spaces/file%2520name.txt"
	if gotRoot != wantRoot {
		t.Fatalf("RawURL(root) = %q, want %q", gotRoot, wantRoot)
	}
}

func TestFetchFile_BlobVerificationAndMidStreamCap(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	goodBody := []byte("schema_version: \"1\"\nharness: claude\n")
	emptyBody := []byte{}

	t.Run("valid file passes blob SHA verification", func(t *testing.T) {
		var gotAuth, gotEscapedPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotEscapedPath = r.URL.EscapedPath()
			_, _ = w.Write(goodBody)
		}))
		t.Cleanup(srv.Close)

		ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
		client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: NewCooldown(nil)})
		src := Source{Owner: "acme", Repo: "repo", Path: "templates/my app"}
		f := File{Path: "hooks/run #1.sh", Mode: 0o755, Size: int64(len(goodBody)), BlobSHA: gitBlobSHA(goodBody)}

		var out bytes.Buffer
		if err := client.FetchFile(context.Background(), src, sha, f, Credential{Kind: CredAnonymous}, &out); err != nil {
			t.Fatalf("FetchFile: %v", err)
		}
		if !bytes.Equal(out.Bytes(), goodBody) {
			t.Fatalf("body = %q, want %q", out.Bytes(), goodBody)
		}
		if gotAuth != "" {
			t.Fatalf("anon request carried Authorization %q", gotAuth)
		}
		wantPath := "/acme/repo/" + sha + "/templates/my%20app/hooks/run%20%231.sh"
		if gotEscapedPath != wantPath {
			t.Fatalf("escaped path = %q, want %q", gotEscapedPath, wantPath)
		}
	})

	t.Run("empty file with valid empty blob SHA succeeds; non-empty body rejected mid-stream", func(t *testing.T) {
		bodyToSend := emptyBody
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bodyToSend)
		}))
		t.Cleanup(srv.Close)

		ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
		client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: NewCooldown(nil)})
		src := Source{Owner: "acme", Repo: "repo"}
		f := File{Path: "empty.txt", Mode: 0o644, Size: 0, BlobSHA: gitBlobSHA(emptyBody)}

		var out bytes.Buffer
		if err := client.FetchFile(context.Background(), src, sha, f, Credential{}, &out); err != nil {
			t.Fatalf("empty file FetchFile: %v", err)
		}
		if out.Len() != 0 {
			t.Fatalf("got %d bytes, want 0", out.Len())
		}

		bodyToSend = []byte("unexpected")
		out.Reset()
		err := client.FetchFile(context.Background(), src, sha, f, Credential{}, &out)
		assertErrorKind(t, err, KindTooLarge)
		if out.Len() != 0 {
			t.Fatalf("oversized body for 0-byte file wrote %d bytes, want 0", out.Len())
		}
	})

	t.Run("mismatched blob SHA rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			tampered := bytes.Repeat([]byte("X"), len(goodBody))
			_, _ = w.Write(tampered)
		}))
		t.Cleanup(srv.Close)

		ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
		client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: NewCooldown(nil)})
		src := Source{Owner: "acme", Repo: "repo"}
		f := File{Path: "a.txt", Mode: 0o644, Size: int64(len(goodBody)), BlobSHA: gitBlobSHA(goodBody)}

		var out bytes.Buffer
		err := client.FetchFile(context.Background(), src, sha, f, Credential{}, &out)
		assertErrorKind(t, err, KindUpstream)
	})

	t.Run("truncated raw body rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(goodBody[:5])
		}))
		t.Cleanup(srv.Close)

		ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
		client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: NewCooldown(nil)})
		src := Source{Owner: "acme", Repo: "repo"}
		f := File{Path: "a.txt", Mode: 0o644, Size: int64(len(goodBody)), BlobSHA: gitBlobSHA(goodBody)}

		var out bytes.Buffer
		err := client.FetchFile(context.Background(), src, sha, f, Credential{}, &out)
		assertErrorKind(t, err, KindUpstream)
	})

	t.Run("truncated raw body rejected without BlobSHA", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(goodBody[:5])
		}))
		t.Cleanup(srv.Close)

		ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
		client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: NewCooldown(nil)})
		src := Source{Owner: "acme", Repo: "repo"}
		f := File{Path: "a.txt", Mode: 0o644, Size: int64(len(goodBody)), BlobSHA: ""}

		var out bytes.Buffer
		err := client.FetchFile(context.Background(), src, sha, f, Credential{}, &out)
		assertErrorKind(t, err, KindUpstream)
	})

	t.Run("oversized raw body rejected mid-stream without draining", func(t *testing.T) {
		var readFromStream atomic.Int64
		pr, pw := io.Pipe()
		go func() {
			chunk := bytes.Repeat([]byte("A"), 4096)
			for i := 0; i < 1024; i++ {
				n, err := pw.Write(chunk)
				readFromStream.Add(int64(n))
				if err != nil {
					return
				}
			}
			_ = pw.Close()
		}()

		doer := funcDoer(func(_ context.Context, _ Request) (*Response, error) {
			return &Response{Status: http.StatusOK, Header: make(http.Header), Body: pr}, nil
		})
		client := New(Options{Endpoint: DefaultEndpoint(), Doer: doer, Cooldown: NewCooldown(nil)})
		src := Source{Owner: "acme", Repo: "repo"}
		f := File{Path: "small.txt", Mode: 0o644, Size: 100, BlobSHA: gitBlobSHA(bytes.Repeat([]byte("A"), 100))}

		var out bytes.Buffer
		err := client.FetchFile(context.Background(), src, sha, f, Credential{}, &out)
		assertErrorKind(t, err, KindTooLarge)
		if out.Len() > 100 {
			t.Fatalf("wrote %d bytes to writer, want <= 100", out.Len())
		}
		// Must have aborted mid-stream, far below the 4 MiB producer total.
		if got := readFromStream.Load(); got > 64*1024 {
			t.Fatalf("stream drained %d bytes before aborting, want mid-stream stop", got)
		}
	})
}

func TestFetchFile_AuthHop0AndRedirectStripping(t *testing.T) {
	const sha = "2222222222222222222222222222222222222222"
	const secretToken = "ghp_raw_secret_token_9549"
	body := []byte("private-file-content")

	var rawAuth, codeloadAuth string
	var redirectTarget string

	codeloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codeloadAuth = r.Header.Get("Authorization")
		_, _ = w.Write(body)
	}))
	t.Cleanup(codeloadSrv.Close)
	codeloadURL, _ := url.Parse(codeloadSrv.URL)

	rawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawAuth = r.Header.Get("Authorization")
		if redirectTarget != "" {
			http.Redirect(w, r, redirectTarget, http.StatusFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(rawSrv.Close)
	rawURL, _ := url.Parse(rawSrv.URL)

	combinedClient := &http.Client{
		Transport: &routingRoundTripper{
			routes: map[string]*url.URL{
				"raw.githubusercontent.com": rawURL,
				"codeload.github.com":       codeloadURL,
			},
			base: http.DefaultTransport,
		},
	}

	ep := DefaultEndpoint()
	doer := newHTTPDoer(combinedClient, ep.AllowedHosts())
	doer.requireHTTPS = true
	client := New(Options{Endpoint: ep, Doer: doer, Cooldown: NewCooldown(nil)})

	cred := Credential{Token: secretToken, Scope: "secret:rawtest", Kind: CredSecret}
	src := Source{Owner: "acme", Repo: "private", Path: "templates"}
	f := File{Path: "a.txt", Mode: 0o644, Size: int64(len(body)), BlobSHA: gitBlobSHA(body)}

	// 1. Direct raw fetch sends Authorization to raw.githubusercontent.com.
	redirectTarget = ""
	rawAuth, codeloadAuth = "", ""
	var out bytes.Buffer
	if err := client.FetchFile(context.Background(), src, sha, f, cred, &out); err != nil {
		t.Fatalf("direct credentialed raw FetchFile: %v", err)
	}
	if rawAuth != "Bearer "+secretToken {
		t.Fatalf("raw host Authorization = %q, want Bearer token", rawAuth)
	}

	// 2. Cross-host redirect strips Authorization.
	redirectTarget = "https://codeload.github.com/signed/object?token=short-lived"
	rawAuth, codeloadAuth = "", ""
	out.Reset()
	if err := client.FetchFile(context.Background(), src, sha, f, cred, &out); err != nil {
		t.Fatalf("redirected credentialed raw FetchFile: %v", err)
	}
	if rawAuth != "Bearer "+secretToken {
		t.Fatalf("hop-0 raw Authorization = %q", rawAuth)
	}
	if codeloadAuth != "" {
		t.Fatalf("Authorization leaked across redirect to codeload: %q", codeloadAuth)
	}

	// 3. Redirect to non-allowlisted host fails without leaking token.
	redirectTarget = "https://evil.example.com/steal"
	out.Reset()
	err := client.FetchFile(context.Background(), src, sha, f, cred, &out)
	assertErrorKind(t, err, KindUnreachable)
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("token leaked in error: %v", err)
	}

	// 4. Anonymous raw request sends no Authorization.
	redirectTarget = ""
	rawAuth = ""
	out.Reset()
	if err := client.FetchFile(context.Background(), src, sha, f, Credential{Kind: CredAnonymous}, &out); err != nil {
		t.Fatalf("anon raw FetchFile: %v", err)
	}
	if rawAuth != "" {
		t.Fatalf("anon raw request sent Authorization %q", rawAuth)
	}
}

func TestFetchFile_RateLimitRecordsCooldownForScopeOnly(t *testing.T) {
	const sha = "3333333333333333333333333333333333333333"
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	cd := NewCooldown(clock)

	var apiHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			apiHits.Add(1)
			_, _ = w.Write([]byte(sha + "\n"))
			return
		}
		// Raw request: rate-limit scope A only.
		if r.Header.Get("Authorization") == "Bearer tok-a" {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
	client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: cd, Clock: clock})

	credA := Credential{Token: "tok-a", Scope: "secret:scope-a", Kind: CredSecret}
	credB := Credential{Token: "tok-b", Scope: "secret:scope-b", Kind: CredSecret}
	src := Source{Owner: "acme", Repo: "repo", Ref: "main"}
	f := File{Path: "a.txt", Mode: 0o644, Size: 2, BlobSHA: gitBlobSHA([]byte("ok"))}

	var out bytes.Buffer
	err := client.FetchFile(context.Background(), src, sha, f, credA, &out)
	assertErrorKind(t, err, KindRateLimited)

	// Cooldown must now be active for scope A (blocks API call with 0 HTTP hits).
	if _, active := cd.Active(credA.Scope); !active {
		t.Fatalf("expected cooldown active for scope A after raw 429")
	}
	_, _, err = client.ResolveRef(context.Background(), src, credA)
	assertErrorKind(t, err, KindRateLimited)
	if apiHits.Load() != 0 {
		t.Fatalf("apiHits = %d, want 0 (scope A should be blocked by cooldown)", apiHits.Load())
	}

	// Scope B must be unaffected for both raw and API calls.
	if _, active := cd.Active(credB.Scope); active {
		t.Fatalf("scope B should not be in cooldown")
	}
	if err := client.FetchFile(context.Background(), src, sha, f, credB, &out); err != nil {
		t.Fatalf("scope B raw FetchFile failed: %v", err)
	}
	res, _, err := client.ResolveRef(context.Background(), src, credB)
	if err != nil || res.CommitSHA != sha {
		t.Fatalf("scope B ResolveRef = (%+v, %v), want %q", res, err, sha)
	}
	if apiHits.Load() != 1 {
		t.Fatalf("apiHits = %d, want 1", apiHits.Load())
	}
}

func TestFetchFolder_HybridStrategyAndThresholds(t *testing.T) {
	const sha = "4444444444444444444444444444444444444444"
	const treeSHA = "5555555555555555555555555555555555555555"

	makeFixture := func(t *testing.T, count int, perFileSize int, truncated bool) (map[string][]byte, []byte, []byte) {
		t.Helper()
		rawFiles := make(map[string][]byte, count)
		var treeEntries []map[string]any
		var tarEntries []tarTestEntry
		for i := 0; i < count; i++ {
			rel := fmt.Sprintf("sub/file-%03d.txt", i)
			modeStr := "100644"
			var tarMode int64 = 0o644
			if i == 0 {
				rel = "hooks/run.sh"
				modeStr = "100755"
				tarMode = 0o755
			}
			body := bytes.Repeat([]byte{byte('a' + (i % 26))}, perFileSize)
			rawFiles[rel] = body
			treeEntries = append(treeEntries, map[string]any{
				"path": rel,
				"mode": modeStr,
				"type": "blob",
				"sha":  gitBlobSHA(body),
				"size": len(body),
			})
			tarEntries = append(tarEntries, tarTestEntry{
				name: "repo-sha/templates/app/" + rel,
				body: body,
				mode: tarMode,
			})
		}
		treeJSON, _ := json.Marshal(map[string]any{
			"sha":       treeSHA,
			"truncated": truncated,
			"tree":      treeEntries,
		})
		tarBytes := buildTestTarGz(t, sha, tarEntries)
		return rawFiles, treeJSON, tarBytes
	}

	runCase := func(t *testing.T, count, perFileSize int, truncated bool, opts Options, lim Limits) (FolderResult, int32, int32, string) {
		t.Helper()
		rawFiles, treeJSON, tarBytes := makeFixture(t, count, perFileSize, truncated)
		var rawHits, tarHits atomic.Int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/repos/acme/repo/git/trees/"):
				_, _ = w.Write(treeJSON)
			case strings.Contains(r.URL.Path, "/tarball/") || strings.Contains(r.URL.Path, "/archive/"):
				tarHits.Add(1)
				_, _ = w.Write(tarBytes)
			case strings.HasPrefix(r.URL.Path, "/acme/repo/"+sha+"/templates/app/"):
				rawHits.Add(1)
				rel := strings.TrimPrefix(r.URL.Path, "/acme/repo/"+sha+"/templates/app/")
				if b, ok := rawFiles[rel]; ok {
					_, _ = w.Write(b)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)

		opts.Endpoint = Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
		opts.Doer = newHTTPDoer(srv.Client(), nil)
		opts.Cooldown = NewCooldown(nil)
		client := New(opts)

		destDir := t.TempDir()
		root, err := os.OpenRoot(destDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		src := Source{Owner: "acme", Repo: "repo", Ref: sha, Path: "templates/app"}
		res, err := client.FetchFolder(context.Background(), src, Credential{}, root, lim)
		if err != nil {
			t.Fatalf("FetchFolder: %v", err)
		}
		return res, rawHits.Load(), tarHits.Load(), destDir
	}

	t.Run("small folder uses raw with zero tarball requests and normalizes modes", func(t *testing.T) {
		res, rawHits, tarHits, destDir := runCase(t, 4, 32, false, Options{}, ImportHostedLimits())
		if res.Strategy != StrategyRaw || res.Files != 4 || res.Bytes != 128 || res.CommitSHA != sha || res.TreeSHA != treeSHA {
			t.Fatalf("unexpected result: %+v", res)
		}
		if rawHits != 4 || tarHits != 0 {
			t.Fatalf("hits raw=%d tar=%d, want raw=4 tar=0", rawHits, tarHits)
		}
		info, err := os.Stat(filepath.Join(destDir, "hooks/run.sh"))
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("run.sh mode = %v, err = %v", info.Mode(), err)
		}
		info, err = os.Stat(filepath.Join(destDir, "sub/file-001.txt"))
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Errorf("file-001.txt mode = %v, err = %v", info.Mode(), err)
		}
	})

	t.Run("exact file count and byte boundaries use raw", func(t *testing.T) {
		lim := ImportHostedLimits()
		lim.RawMaxFiles = 4
		lim.RawMaxBytes = 400
		res, rawHits, tarHits, _ := runCase(t, 4, 100, false, Options{}, lim)
		if res.Strategy != StrategyRaw || rawHits != 4 || tarHits != 0 {
			t.Fatalf("boundary (4 files, 400 bytes) got strategy=%q raw=%d tar=%d", res.Strategy, rawHits, tarHits)
		}
	})

	t.Run("file count over RawMaxFiles uses tarball with zero raw requests", func(t *testing.T) {
		lim := ImportHostedLimits()
		lim.RawMaxFiles = 4
		lim.RawMaxBytes = 10000
		res, rawHits, tarHits, _ := runCase(t, 5, 10, false, Options{}, lim)
		if res.Strategy != StrategyTarball || rawHits != 0 || tarHits != 1 || res.Files != 5 {
			t.Fatalf("over RawMaxFiles got res=%+v raw=%d tar=%d", res, rawHits, tarHits)
		}
	})

	t.Run("total bytes over RawMaxBytes uses tarball with zero raw requests", func(t *testing.T) {
		lim := ImportHostedLimits()
		lim.RawMaxFiles = 100
		lim.RawMaxBytes = 399
		res, rawHits, tarHits, _ := runCase(t, 4, 100, false, Options{}, lim)
		if res.Strategy != StrategyTarball || rawHits != 0 || tarHits != 1 || res.Files != 4 {
			t.Fatalf("over RawMaxBytes got res=%+v raw=%d tar=%d", res, rawHits, tarHits)
		}
	})

	t.Run("Options thresholds apply when Limits thresholds are zero", func(t *testing.T) {
		opts := Options{RawMaxFiles: 2, RawMaxBytes: 10000}
		res, rawHits, tarHits, _ := runCase(t, 3, 10, false, opts, ImportHostedLimits())
		if res.Strategy != StrategyTarball || rawHits != 0 || tarHits != 1 {
			t.Fatalf("Options.RawMaxFiles=2 with 3 files got strategy=%q raw=%d tar=%d", res.Strategy, rawHits, tarHits)
		}
	})

	t.Run("default RawMaxFiles boundary (64 raw, 65 tarball)", func(t *testing.T) {
		res64, raw64, tar64, _ := runCase(t, DefaultRawMaxFiles, 8, false, Options{}, ImportHostedLimits())
		if res64.Strategy != StrategyRaw || raw64 != DefaultRawMaxFiles || tar64 != 0 {
			t.Fatalf("64 files got strategy=%q raw=%d tar=%d", res64.Strategy, raw64, tar64)
		}
		res65, raw65, tar65, _ := runCase(t, DefaultRawMaxFiles+1, 8, false, Options{}, ImportHostedLimits())
		if res65.Strategy != StrategyTarball || raw65 != 0 || tar65 != 1 {
			t.Fatalf("65 files got strategy=%q raw=%d tar=%d", res65.Strategy, raw65, tar65)
		}
	})

	t.Run("truncated tree uses tarball when MaxFiles allows it", func(t *testing.T) {
		lim := ImportHostedLimits()
		lim.MaxFiles = 0 // allow truncated tree through ListTree
		res, rawHits, tarHits, _ := runCase(t, 2, 16, true, Options{}, lim)
		if res.Strategy != StrategyTarball || rawHits != 0 || tarHits != 1 {
			t.Fatalf("truncated tree got strategy=%q raw=%d tar=%d", res.Strategy, rawHits, tarHits)
		}
	})
}

func TestFetchFolder_RawBoundedParallelismAndPartialFailure(t *testing.T) {
	const sha = "6666666666666666666666666666666666666666"
	const treeSHA = "7777777777777777777777777777777777777777"

	t.Run("default parallelism bounded to <= 8 in-flight", func(t *testing.T) {
		const numFiles = 24
		rawFiles := make(map[string][]byte, numFiles)
		var treeEntries []map[string]any
		for i := 0; i < numFiles; i++ {
			rel := fmt.Sprintf("f-%02d.txt", i)
			body := []byte(fmt.Sprintf("payload-%02d", i))
			rawFiles[rel] = body
			treeEntries = append(treeEntries, map[string]any{
				"path": rel,
				"mode": "100644",
				"type": "blob",
				"sha":  gitBlobSHA(body),
				"size": len(body),
			})
		}
		treeJSON, _ := json.Marshal(map[string]any{"sha": treeSHA, "truncated": false, "tree": treeEntries})

		var inFlight, maxInFlight atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/repos/") {
				_, _ = w.Write(treeJSON)
				return
			}
			cur := inFlight.Add(1)
			for {
				prev := maxInFlight.Load()
				if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			inFlight.Add(-1)

			rel := strings.TrimPrefix(r.URL.Path, "/acme/repo/"+sha+"/pkg/")
			_, _ = w.Write(rawFiles[rel])
		}))
		t.Cleanup(srv.Close)

		ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
		client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: NewCooldown(nil)})
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		src := Source{Owner: "acme", Repo: "repo", Ref: sha, Path: "pkg"}
		res, err := client.FetchFolder(context.Background(), src, Credential{}, root, ImportHostedLimits())
		if err != nil {
			t.Fatalf("FetchFolder: %v", err)
		}
		if res.Strategy != StrategyRaw || res.Files != numFiles {
			t.Fatalf("unexpected result: %+v", res)
		}
		peak := maxInFlight.Load()
		if peak > DefaultRawConcurrency {
			t.Fatalf("max in-flight = %d, want <= %d", peak, DefaultRawConcurrency)
		}
		if peak < 2 {
			t.Fatalf("max in-flight = %d, expected concurrent downloads", peak)
		}
	})

	t.Run("partial failure returns error and zero FolderResult", func(t *testing.T) {
		goodA := []byte("good-file-a")
		goodB := []byte("good-file-b")
		treeJSON, _ := json.Marshal(map[string]any{
			"sha":       treeSHA,
			"truncated": false,
			"tree": []map[string]any{
				{"path": "a.txt", "mode": "100644", "type": "blob", "sha": gitBlobSHA(goodA), "size": len(goodA)},
				{"path": "bad.txt", "mode": "100644", "type": "blob", "sha": gitBlobSHA(goodB), "size": len(goodB)},
			},
		})

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/repos/"):
				_, _ = w.Write(treeJSON)
			case strings.HasSuffix(r.URL.Path, "/a.txt"):
				_, _ = w.Write(goodA)
			case strings.HasSuffix(r.URL.Path, "/bad.txt"):
				// Tampered content triggers blob SHA mismatch.
				_, _ = w.Write([]byte("TAMPERED-XX"))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)

		ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
		client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: NewCooldown(nil)})
		destDir := t.TempDir()
		root, err := os.OpenRoot(destDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		src := Source{Owner: "acme", Repo: "repo", Ref: sha, Path: "pkg"}
		res, err := client.FetchFolder(context.Background(), src, Credential{}, root, ImportHostedLimits())
		assertErrorKind(t, err, KindUpstream)
		if res != (FolderResult{}) {
			t.Fatalf("partial failure returned non-zero FolderResult: %+v", res)
		}
		if _, statErr := os.Stat(filepath.Join(destDir, "bad.txt")); !os.IsNotExist(statErr) {
			t.Errorf("failed raw file bad.txt should have been removed")
		}
	})
}

func TestFetchFolder_RawContextCancelAndTimeout(t *testing.T) {
	const sha = "7777777777777777777777777777777777777777"
	const treeSHA = "8888888888888888888888888888888888888888"
	bodyA := []byte("file-a-content")
	bodyB := []byte("file-b-content")
	twoFileTree := &Tree{
		TreeSHA: treeSHA,
		Files: []File{
			{Path: "a.txt", Mode: 0o644, Size: int64(len(bodyA)), BlobSHA: gitBlobSHA(bodyA)},
			{Path: "b.txt", Mode: 0o644, Size: int64(len(bodyB)), BlobSHA: gitBlobSHA(bodyB)},
		},
	}
	src := Source{Owner: "acme", Repo: "repo", Ref: sha, Path: "pkg"}

	t.Run("pre-cancelled context returns KindTimeout and zero result", func(t *testing.T) {
		var hits atomic.Int32
		doer := funcDoer(func(_ context.Context, _ Request) (*Response, error) {
			hits.Add(1)
			return nil, errors.New("should not be called")
		})
		client := New(Options{Endpoint: DefaultEndpoint(), Doer: doer, Cooldown: NewCooldown(nil)})
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		res, err := client.FetchFolderResolved(ctx, src, sha, twoFileTree, Credential{}, root, ImportHostedLimits())
		assertErrorKind(t, err, KindTimeout)
		if res != (FolderResult{}) {
			t.Fatalf("expected zero FolderResult on pre-cancelled ctx, got %+v", res)
		}
		if hits.Load() != 0 {
			t.Fatalf("hits = %d, want 0", hits.Load())
		}
	})

	t.Run("cancellation between files with RawConcurrency 1 returns KindTimeout and zero result", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var hits atomic.Int32
		doer := funcDoer(func(_ context.Context, r Request) (*Response, error) {
			hits.Add(1)
			if strings.HasSuffix(r.URL, "/a.txt") {
				// Return file A cleanly, then cancel ctx before file B starts.
				cancel()
				return &Response{Status: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(bodyA))}, nil
			}
			return &Response{Status: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(bodyB))}, nil
		})
		client := New(Options{
			Endpoint:       DefaultEndpoint(),
			Doer:           doer,
			Cooldown:       NewCooldown(nil),
			RawConcurrency: 1,
		})
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		res, err := client.FetchFolderResolved(ctx, src, sha, twoFileTree, Credential{}, root, ImportHostedLimits())
		assertErrorKind(t, err, KindTimeout)
		if res != (FolderResult{}) {
			t.Fatalf("expected zero FolderResult when cancelled between files, got %+v", res)
		}
		if hits.Load() != 1 {
			t.Fatalf("hits = %d, want 1 (second file must not be fetched after cancel)", hits.Load())
		}
	})

	t.Run("Limits.Timeout expiring between files with live parent ctx returns KindTimeout and zero result", func(t *testing.T) {
		var hits atomic.Int32
		doer := funcDoer(func(_ context.Context, r Request) (*Response, error) {
			hits.Add(1)
			if strings.HasSuffix(r.URL, "/a.txt") {
				// Let Limits.Timeout expire before returning file A's body,
				// simulating a slow first file followed by an expired folder deadline.
				time.Sleep(60 * time.Millisecond)
				return &Response{Status: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(bodyA))}, nil
			}
			return &Response{Status: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(bodyB))}, nil
		})
		client := New(Options{
			Endpoint:       DefaultEndpoint(),
			Doer:           doer,
			Cooldown:       NewCooldown(nil),
			RawConcurrency: 1,
		})
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()

		lim := ImportHostedLimits()
		lim.Timeout = 30 * time.Millisecond
		lim.RequestTimeout = 0

		res, err := client.FetchFolderResolved(context.Background(), src, sha, twoFileTree, Credential{}, root, lim)
		assertErrorKind(t, err, KindTimeout)
		if res != (FolderResult{}) {
			t.Fatalf("expected zero FolderResult when Limits.Timeout expires between files, got %+v", res)
		}
		if hits.Load() != 1 {
			t.Fatalf("hits = %d, want 1 (second file must not be fetched after timeout)", hits.Load())
		}
	})
}

func TestSkillLimitsAndTarballZeroLimitsFailClosed(t *testing.T) {
	const sha = "8888888888888888888888888888888888888888"

	t.Run("SkillLimits has finite MaxDownload and MaxUnpacked", func(t *testing.T) {
		sl := SkillLimits()
		if sl.MaxDownload <= 0 {
			t.Fatalf("SkillLimits().MaxDownload = %d, want > 0", sl.MaxDownload)
		}
		if sl.MaxUnpacked <= 0 {
			t.Fatalf("SkillLimits().MaxUnpacked = %d, want > 0", sl.MaxUnpacked)
		}
	})

	t.Run("extractTarGz fails closed when MaxUnpacked is zero or negative", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/a.txt", body: []byte("hello")},
		})
		for _, maxUnpacked := range []int64{0, -1} {
			lim := ImportHostedLimits()
			lim.MaxUnpacked = maxUnpacked
			root, _ := os.OpenRoot(t.TempDir())
			_, _, _, err := extractTarGz(bytes.NewReader(tarData), "acme/repo", "", sha, nil, root, lim)
			_ = root.Close()
			assertErrorKind(t, err, KindTooLarge)
		}
	})

	t.Run("FetchFolderResolved tarball path fails closed when MaxDownload or MaxUnpacked is zero", func(t *testing.T) {
		cases := []struct {
			name        string
			maxDownload int64
			maxUnpacked int64
		}{
			{"zero MaxDownload", 0, 1 << 20},
			{"zero MaxUnpacked", 1 << 20, 0},
			{"both zero", 0, 0},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var hits atomic.Int32
				doer := funcDoer(func(_ context.Context, _ Request) (*Response, error) {
					hits.Add(1)
					return nil, errors.New("should not be called")
				})
				client := New(Options{Endpoint: DefaultEndpoint(), Doer: doer, Cooldown: NewCooldown(nil)})
				lim := Limits{
					MaxFiles:    100,
					MaxTotal:    1 << 20,
					MaxDownload: tc.maxDownload,
					MaxUnpacked: tc.maxUnpacked,
					RawMaxFiles: -1, // force tarball path
				}
				root, _ := os.OpenRoot(t.TempDir())
				defer func() { _ = root.Close() }()

				tree := &Tree{TreeSHA: "aaa", Truncated: true}
				_, err := client.FetchFolderResolved(context.Background(), Source{Owner: "acme", Repo: "repo"}, sha, tree, Credential{}, root, lim)
				assertErrorKind(t, err, KindTooLarge)
				if hits.Load() != 0 {
					t.Fatalf("hits = %d, want 0 (must fail closed before download)", hits.Load())
				}
			})
		}
	})

	t.Run("tarball stream exceeding MaxDownload fails mid-stream even with bare Doer", func(t *testing.T) {
		tarData := buildTestTarGz(t, sha, []tarTestEntry{
			{name: "repo-sha/big.bin", body: bytes.Repeat([]byte("z"), 32*1024)},
		})
		doer := funcDoer(func(_ context.Context, _ Request) (*Response, error) {
			return &Response{Status: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(tarData))}, nil
		})
		client := New(Options{Endpoint: DefaultEndpoint(), Doer: doer, Cooldown: NewCooldown(nil)})
		lim := ImportHostedLimits()
		lim.MaxDownload = int64(len(tarData)) / 2
		lim.RawMaxFiles = -1
		root, _ := os.OpenRoot(t.TempDir())
		defer func() { _ = root.Close() }()

		_, err := client.FetchFolderResolved(context.Background(), Source{Owner: "acme", Repo: "repo"}, sha, &Tree{Truncated: true}, Credential{}, root, lim)
		assertErrorKind(t, err, KindTooLarge)
	})

	// Ensure concurrent raw fetches under -race have no data races on shared Cooldown.
	t.Run("concurrent FetchFile calls with shared Cooldown are race-free", func(t *testing.T) {
		body := []byte("ok")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(body)
		}))
		t.Cleanup(srv.Close)

		ep := Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
		client := New(Options{Endpoint: ep, Doer: newHTTPDoer(srv.Client(), nil), Cooldown: NewCooldown(nil)})
		src := Source{Owner: "acme", Repo: "repo"}
		f := File{Path: "a.txt", Mode: 0o644, Size: int64(len(body)), BlobSHA: gitBlobSHA(body)}

		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var buf bytes.Buffer
				if err := client.FetchFile(context.Background(), src, sha, f, Credential{}, &buf); err != nil {
					t.Errorf("FetchFile: %v", err)
				}
			}()
		}
		wg.Wait()
	})
}
