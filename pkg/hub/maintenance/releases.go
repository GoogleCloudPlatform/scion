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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	// GitHubTokenEnv optionally authenticates GitHub API calls (higher rate
	// limit for hubs sharing a NAT address).
	GitHubTokenEnv = "SCION_MAINTENANCE_GITHUB_TOKEN"

	// DefaultGitHubAPIBase is the GitHub REST API root.
	DefaultGitHubAPIBase = "https://api.github.com"

	// releaseAPITimeout bounds one GitHub API request.
	releaseAPITimeout = 15 * time.Second

	maxReleaseJSONSize = 4 << 20
	maxChecksumsSize   = 1 << 20

	// ChecksumsAssetName is the release asset holding sha256sum output.
	ChecksumsAssetName = "SHA256SUMS"

	// emptySHA256 is the SHA-256 of zero bytes. A release-workflow race
	// (fixed upstream in #4120) could list it; no real tarball has it.
	emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Release is one GitHub release, reduced to what the updater needs.
type Release struct {
	Tag         string
	PublishedAt time.Time
	Draft       bool
	Assets      []ReleaseAsset
}

// ReleaseAsset is a downloadable file attached to a release.
type ReleaseAsset struct {
	Name string
	URL  string // browser_download_url
	Size int64
}

// Asset returns the asset called name, if present.
func (r *Release) Asset(name string) (ReleaseAsset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return ReleaseAsset{}, false
}

// ErrReleaseNotFound is returned by ReleaseSource when the tag has no release.
var ErrReleaseNotFound = errors.New("release not found")

// ReleaseSource looks up a single release by tag.
type ReleaseSource interface {
	ReleaseByTag(ctx context.Context, tag string) (*Release, error)
}

// GitHubReleases is the ReleaseSource backed by
// GET <APIBase>/repos/<Repo>/releases/tags/<tag>.
type GitHubReleases struct {
	Repo    string       // owner/name
	APIBase string       // default DefaultGitHubAPIBase
	Token   string       // optional bearer token
	Client  *http.Client // default: a client with a 15 s timeout
}

// NewGitHubReleases returns a GitHubReleases for repo, authenticated with
// $SCION_MAINTENANCE_GITHUB_TOKEN when it is set.
func NewGitHubReleases(repo string) *GitHubReleases {
	return &GitHubReleases{Repo: repo, Token: os.Getenv(GitHubTokenEnv)}
}

// ReleaseByTag implements ReleaseSource.
func (g *GitHubReleases) ReleaseByTag(ctx context.Context, tag string) (*Release, error) {
	if !validRepo(g.Repo) {
		return nil, fmt.Errorf("invalid GitHub repo %q (want owner/name)", g.Repo)
	}
	base := g.APIBase
	if base == "" {
		base = DefaultGitHubAPIBase
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: releaseAPITimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, releaseAPITimeout)
	defer cancel()

	apiURL := fmt.Sprintf("%s/repos/%s/releases/tags/%s", strings.TrimRight(base, "/"), g.Repo, url.PathEscape(tag))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create GitHub release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch GitHub release %s: %w", tag, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s in %s", ErrReleaseNotFound, tag, g.Repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub release API returned %s for %s", resp.Status, tag)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseJSONSize+1))
	if err != nil {
		return nil, fmt.Errorf("read GitHub release %s: %w", tag, err)
	}
	if len(body) > maxReleaseJSONSize {
		return nil, fmt.Errorf("GitHub release %s response exceeds %d bytes", tag, maxReleaseJSONSize)
	}
	var raw struct {
		TagName     string    `json:"tag_name"`
		Draft       bool      `json:"draft"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Size               int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode GitHub release %s: %w", tag, err)
	}
	rel := &Release{Tag: raw.TagName, Draft: raw.Draft, PublishedAt: raw.PublishedAt}
	for _, a := range raw.Assets {
		rel.Assets = append(rel.Assets, ReleaseAsset{Name: a.Name, URL: a.BrowserDownloadURL, Size: a.Size})
	}
	return rel, nil
}

var repoRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)

func validRepo(repo string) bool {
	return repoRE.MatchString(repo) && !strings.Contains(repo, "..")
}

// BinaryAssetName is the release tarball for this platform.
func BinaryAssetName() string {
	return fmt.Sprintf("scion-linux-%s.tar.gz", runtime.GOARCH)
}

// ReleaseDownloadPrefix is the only URL prefix release assets of repo@tag may
// be downloaded from.
func ReleaseDownloadPrefix(repo, tag string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/", repo, tag)
}

// CheckAssetURL fails unless assetURL is an asset of repo@tag on github.com:
// it must start with ReleaseDownloadPrefix(repo, tag) and name a single file.
func CheckAssetURL(repo, tag, assetURL string) error {
	prefix := ReleaseDownloadPrefix(repo, tag)
	if !strings.HasPrefix(assetURL, prefix) {
		return fmt.Errorf("asset URL %q is not under %s", assetURL, prefix)
	}
	u, err := url.Parse(assetURL)
	if err != nil || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("asset URL %q is not a plain github.com release asset", assetURL)
	}
	name := strings.TrimPrefix(assetURL, prefix)
	if name == "" || strings.ContainsAny(name, "/\\%") || name == "." || name == ".." {
		return fmt.Errorf("asset URL %q does not name a single release file", assetURL)
	}
	return nil
}

// isAllowedRedirectHost reports whether a release download may be redirected
// to host: GitHub serves release assets from *.githubusercontent.com.
func isAllowedRedirectHost(host string) bool {
	host = strings.ToLower(host)
	return host == "githubusercontent.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

// checkReleaseRedirect is the http.Client CheckRedirect for asset downloads:
// HTTPS only, to *.githubusercontent.com only, at most 5 hops.
func checkReleaseRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" || !isAllowedRedirectHost(req.URL.Hostname()) {
		return fmt.Errorf("refusing redirect to %s://%s (only https://*.githubusercontent.com is allowed)", req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// downloadClient returns a copy of base (or a new client) that follows only
// release-asset redirects.
func downloadClient(base *http.Client) *http.Client {
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	c.CheckRedirect = checkReleaseRedirect
	return c
}

// ErrDownloadTooLarge is returned when a download exceeds its size cap.
var ErrDownloadTooLarge = errors.New("download exceeds size limit")

// fetchCapped GETs u with client and writes at most maxBytes of the body to
// w. A larger body (by Content-Length or by count) fails with
// ErrDownloadTooLarge.
func fetchCapped(ctx context.Context, client *http.Client, u string, maxBytes int64, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("create download request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("download %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download %s: HTTP %s", u, resp.Status)
	}
	if resp.ContentLength > maxBytes {
		return 0, fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrDownloadTooLarge, u, resp.ContentLength, maxBytes)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return n, fmt.Errorf("download %s: %w", u, err)
	}
	if n > maxBytes {
		return n, fmt.Errorf("%w: %s is over %d bytes", ErrDownloadTooLarge, u, maxBytes)
	}
	return n, nil
}

// sumsLineRE is one sha256sum output line: 64 hex digits, a space, then a
// space (text mode) or "*" (binary mode), then the file name.
var sumsLineRE = regexp.MustCompile(`^([0-9a-fA-F]{64}) [ *](\S.*)$`)

// ExpectedChecksum returns the SHA-256 listed for assetName in the contents
// of a SHA256SUMS file. Only well-formed lines naming assetName are
// considered; every other line, including a bogus entry for SHA256SUMS
// itself, is ignored. It fails closed when there is no such line, when lines
// disagree, or when the listed hash is the empty-file hash.
func ExpectedChecksum(sums []byte, assetName string) (string, error) {
	var found string
	for _, line := range strings.Split(string(sums), "\n") {
		m := sumsLineRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil || m[2] != assetName {
			continue
		}
		h := strings.ToLower(m[1])
		if found != "" && found != h {
			return "", fmt.Errorf("conflicting checksum entries for %q in %s", assetName, ChecksumsAssetName)
		}
		found = h
	}
	if found == "" {
		return "", fmt.Errorf("no well-formed checksum entry for %q in %s", assetName, ChecksumsAssetName)
	}
	if found == emptySHA256 {
		return "", fmt.Errorf("checksum entry for %q in %s is the empty-file hash; refusing to install", assetName, ChecksumsAssetName)
	}
	return found, nil
}
