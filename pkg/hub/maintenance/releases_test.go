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

package maintenance

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGitHubReleases_ReleaseByTag(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		if r.URL.Path == "/repos/o/r/releases/tags/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"nightly-20261023","draft":false,
			"published_at":"2026-10-23T04:05:06Z",
			"assets":[{"name":"SHA256SUMS","browser_download_url":"https://github.com/o/r/releases/download/nightly-20261023/SHA256SUMS","size":10}]}`))
	}))
	defer srv.Close()

	g := &GitHubReleases{Repo: "o/r", APIBase: srv.URL, Token: "tok"}
	rel, err := g.ReleaseByTag(context.Background(), "nightly-20261023")
	if err != nil {
		t.Fatalf("ReleaseByTag: %v", err)
	}
	if gotPath != "/repos/o/r/releases/tags/nightly-20261023" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if rel.Tag != "nightly-20261023" || rel.PublishedAt.IsZero() {
		t.Errorf("release = %+v", rel)
	}
	if a, ok := rel.Asset("SHA256SUMS"); !ok || a.Size != 10 {
		t.Errorf("asset = %+v %v", a, ok)
	}

	g.Token = ""
	if _, err := g.ReleaseByTag(context.Background(), "missing"); !errors.Is(err, ErrReleaseNotFound) {
		t.Errorf("missing tag err = %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization sent without a token: %q", gotAuth)
	}

	if _, err := (&GitHubReleases{Repo: "../evil", APIBase: srv.URL}).ReleaseByTag(context.Background(), "v1.0.0"); err == nil {
		t.Error("invalid repo accepted")
	}
}

func TestNewGitHubReleases_TokenFromEnv(t *testing.T) {
	t.Setenv(GitHubTokenEnv, "from-env")
	if g := NewGitHubReleases("o/r"); g.Token != "from-env" {
		t.Fatalf("Token = %q", g.Token)
	}
}

func TestCheckAssetURL(t *testing.T) {
	const repo, tag = "GoogleCloudPlatform/scion", "nightly-20261023"
	good := "https://github.com/GoogleCloudPlatform/scion/releases/download/nightly-20261023/scion-linux-amd64.tar.gz"
	if err := CheckAssetURL(repo, tag, good); err != nil {
		t.Fatalf("good URL refused: %v", err)
	}
	for name, u := range map[string]string{
		"foreign host":     "https://evil.example.com/GoogleCloudPlatform/scion/releases/download/nightly-20261023/scion-linux-amd64.tar.gz",
		"http":             "http://github.com/GoogleCloudPlatform/scion/releases/download/nightly-20261023/scion-linux-amd64.tar.gz",
		"wrong tag":        "https://github.com/GoogleCloudPlatform/scion/releases/download/nightly-20261022/scion-linux-amd64.tar.gz",
		"tag prefix":       "https://github.com/GoogleCloudPlatform/scion/releases/download/nightly-20261023.1/scion-linux-amd64.tar.gz",
		"other repo":       "https://github.com/evil/scion/releases/download/nightly-20261023/scion-linux-amd64.tar.gz",
		"userinfo host":    "https://github.com/GoogleCloudPlatform/scion/releases/download/nightly-20261023/@evil.com/x",
		"nested path":      "https://github.com/GoogleCloudPlatform/scion/releases/download/nightly-20261023/a/b",
		"encoded traverse": "https://github.com/GoogleCloudPlatform/scion/releases/download/nightly-20261023/%2e%2e%2fx",
		"query":            good + "?x=1",
		"empty name":       "https://github.com/GoogleCloudPlatform/scion/releases/download/nightly-20261023/",
	} {
		if err := CheckAssetURL(repo, tag, u); err == nil {
			t.Errorf("%s: %q accepted", name, u)
		}
	}
}

func TestCheckReleaseRedirect(t *testing.T) {
	req := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	if err := checkReleaseRedirect(req("https://objects.githubusercontent.com/x"), nil); err != nil {
		t.Errorf("githubusercontent redirect refused: %v", err)
	}
	if err := checkReleaseRedirect(req("https://release-assets.githubusercontent.com/x"), nil); err != nil {
		t.Errorf("githubusercontent redirect refused: %v", err)
	}
	for _, bad := range []string{
		"https://evil.example.com/x",
		"https://githubusercontent.com.evil.com/x",
		"https://evilgithubusercontent.com/x",
		"http://objects.githubusercontent.com/x",
		"https://github.com/x", // only githubusercontent hosts after the first hop
	} {
		if err := checkReleaseRedirect(req(bad), nil); err == nil {
			t.Errorf("redirect to %q allowed", bad)
		}
	}
	via := make([]*http.Request, 5)
	if err := checkReleaseRedirect(req("https://objects.githubusercontent.com/x"), via); err == nil {
		t.Error("6th redirect allowed")
	}
}

func TestFetchCapped(t *testing.T) {
	body := strings.Repeat("x", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chunked" {
			// No Content-Length: the cap must hold while streaming.
			w.(http.Flusher).Flush()
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if n, err := fetchCapped(context.Background(), srv.Client(), srv.URL+"/ok", 100, &buf); err != nil || n != 100 {
		t.Fatalf("at cap: n=%d err=%v", n, err)
	}
	for _, path := range []string{"/ok", "/chunked"} {
		buf.Reset()
		_, err := fetchCapped(context.Background(), srv.Client(), srv.URL+path, 99, &buf)
		if !errors.Is(err, ErrDownloadTooLarge) {
			t.Errorf("%s over cap: err = %v", path, err)
		}
		if buf.Len() > 100 {
			t.Errorf("%s: wrote %d bytes past the cap", path, buf.Len())
		}
	}
}

func TestExpectedChecksum(t *testing.T) {
	const asset = "scion-linux-amd64.tar.gz"
	h := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)

	t.Run("bogus SHA256SUMS self-entry is ignored", func(t *testing.T) {
		sums := emptySHA256 + "  SHA256SUMS\n" + h + "  " + asset + "\n" + other + "  scion-linux-arm64.tar.gz\n"
		got, err := ExpectedChecksum([]byte(sums), asset)
		if err != nil || got != h {
			t.Fatalf("got %q, %v; want %q", got, err, h)
		}
	})
	t.Run("binary-mode marker and CRLF", func(t *testing.T) {
		got, err := ExpectedChecksum([]byte(strings.ToUpper(h)+" *"+asset+"\r\n"), asset)
		if err != nil || got != h {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	for name, sums := range map[string]string{
		"empty-file hash for the asset": emptySHA256 + "  " + asset + "\n",
		"missing entry":                 other + "  scion-linux-arm64.tar.gz\n",
		"short hash":                    h[:63] + "  " + asset + "\n",
		"non-hex hash":                  strings.Repeat("zz", 32) + "  " + asset + "\n",
		"single space":                  h + " " + asset + "\n",
		"name prefix only":              h + "  " + asset + ".sig\n",
		"conflicting entries":           h + "  " + asset + "\n" + other + "  " + asset + "\n",
		"empty file":                    "",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := ExpectedChecksum([]byte(sums), asset); err == nil {
				t.Fatalf("accepted %q", got)
			}
		})
	}
}
