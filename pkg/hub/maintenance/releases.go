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
	// maxReleaseListSize caps one page (100 releases with their assets) of
	// GET /repos/<repo>/releases.
	maxReleaseListSize = 16 << 20
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
	HTMLURL     string // the release notes page (html_url)
	Assets      []ReleaseAsset
}

// ReleaseAsset is a downloadable file attached to a release.
type ReleaseAsset struct {
	Name      string
	URL       string // browser_download_url
	Size      int64
	UpdatedAt time.Time // changes when the asset is re-uploaded
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
// GET <APIBase>/repos/<Repo>/releases/tags/<tag>. It is also the
// ReleaseLister of the release catalog (GET <APIBase>/repos/<Repo>/releases
// and the SHA256SUMS download).
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

func (g *GitHubReleases) apiBase() string {
	base := g.APIBase
	if base == "" {
		base = DefaultGitHubAPIBase
	}
	return strings.TrimRight(base, "/")
}

func (g *GitHubReleases) httpClient() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: releaseAPITimeout}
}

// newAPIRequest builds a GitHub API GET request with the JSON media type and
// the optional token.
func (g *GitHubReleases) newAPIRequest(ctx context.Context, apiURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create GitHub release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	return req, nil
}

// ReleaseByTag implements ReleaseSource.
func (g *GitHubReleases) ReleaseByTag(ctx context.Context, tag string) (*Release, error) {
	if !validRepo(g.Repo) {
		return nil, fmt.Errorf("invalid GitHub repo %q (want owner/name)", g.Repo)
	}
	client := g.httpClient()
	ctx, cancel := context.WithTimeout(ctx, releaseAPITimeout)
	defer cancel()

	apiURL := fmt.Sprintf("%s/repos/%s/releases/tags/%s", g.apiBase(), g.Repo, url.PathEscape(tag))
	req, err := g.newAPIRequest(ctx, apiURL)
	if err != nil {
		return nil, err
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
	var raw githubRelease
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode GitHub release %s: %w", tag, err)
	}
	rel := raw.release()
	return &rel, nil
}

// githubRelease is the subset of GitHub's release JSON the updater reads.
type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []struct {
		Name               string    `json:"name"`
		BrowserDownloadURL string    `json:"browser_download_url"`
		Size               int64     `json:"size"`
		UpdatedAt          time.Time `json:"updated_at"`
	} `json:"assets"`
}

func (raw githubRelease) release() Release {
	rel := Release{Tag: raw.TagName, Draft: raw.Draft, PublishedAt: raw.PublishedAt, HTMLURL: raw.HTMLURL}
	for _, a := range raw.Assets {
		rel.Assets = append(rel.Assets, ReleaseAsset{Name: a.Name, URL: a.BrowserDownloadURL, Size: a.Size, UpdatedAt: a.UpdatedAt})
	}
	return rel
}

// ReleasePage is one page of GET /repos/<repo>/releases.
type ReleasePage struct {
	Releases []Release
	// Next is the URL of the next page, "" on the last page.
	Next string
	// ETag identifies this page's content for a later If-None-Match.
	ETag string
	// NotModified is set on a 304 answer to If-None-Match: the page is
	// unchanged, and Releases and Next are empty (reuse the cached page).
	// GitHub does not count a 304 against the primary rate limit.
	NotModified bool
}

// ReleaseLister is what the release catalog needs from GitHub.
type ReleaseLister interface {
	// ListReleases fetches one page of the release list, newest first.
	// pageURL "" is the first page; otherwise it is a ReleasePage.Next from
	// an earlier call. A non-empty etag is sent as If-None-Match.
	ListReleases(ctx context.Context, pageURL, etag string) (*ReleasePage, error)
	// FetchChecksums downloads the SHA256SUMS asset of release tag.
	FetchChecksums(ctx context.Context, tag string, asset ReleaseAsset) ([]byte, error)
}

// ListReleases implements ReleaseLister. Page URLs (the given one and the
// rel="next" link of the answer) must be on the API host, so neither a
// response header nor a persisted cache can send the token elsewhere.
func (g *GitHubReleases) ListReleases(ctx context.Context, pageURL, etag string) (*ReleasePage, error) {
	if !validRepo(g.Repo) {
		return nil, fmt.Errorf("invalid GitHub repo %q (want owner/name)", g.Repo)
	}
	base := g.apiBase()
	if pageURL == "" {
		pageURL = fmt.Sprintf("%s/repos/%s/releases?per_page=100", base, g.Repo)
	} else if err := checkReleasePageURL(base, pageURL); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, releaseAPITimeout)
	defer cancel()
	req, err := g.newAPIRequest(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("list GitHub releases of %s: %w", g.Repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified {
		return &ReleasePage{NotModified: true, ETag: etag}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub release list API returned %s for %s", resp.Status, g.Repo)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseListSize+1))
	if err != nil {
		return nil, fmt.Errorf("read GitHub release list of %s: %w", g.Repo, err)
	}
	if len(body) > maxReleaseListSize {
		return nil, fmt.Errorf("GitHub release list page of %s exceeds %d bytes", g.Repo, maxReleaseListSize)
	}
	var raw []githubRelease
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode GitHub release list of %s: %w", g.Repo, err)
	}
	page := &ReleasePage{ETag: resp.Header.Get("ETag"), Next: nextPageLink(resp.Header.Values("Link"))}
	if page.Next != "" {
		if err := checkReleasePageURL(base, page.Next); err != nil {
			return nil, fmt.Errorf("GitHub release list of %s: next page: %w", g.Repo, err)
		}
	}
	for _, r := range raw {
		page.Releases = append(page.Releases, r.release())
	}
	return page, nil
}

// FetchChecksums implements ReleaseLister. The asset URL must be pinned to
// repo@tag (CheckAssetURL); redirects follow the release-download policy and
// the body is capped at 1 MiB. No token is sent to the download host.
func (g *GitHubReleases) FetchChecksums(ctx context.Context, tag string, asset ReleaseAsset) ([]byte, error) {
	if err := CheckAssetURL(g.Repo, tag, asset.URL); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, releaseAPITimeout)
	defer cancel()
	var buf bytes.Buffer
	if _, err := fetchCapped(ctx, downloadClient(g.httpClient()), asset.URL, maxChecksumsSize, &buf); err != nil {
		return nil, fmt.Errorf("fetch %s of %s: %w", ChecksumsAssetName, tag, err)
	}
	return buf.Bytes(), nil
}

// checkReleasePageURL fails unless u is an https (or, for a test API base,
// the same scheme) URL on the API host whose path ends in /releases. GitHub's
// rel="next" links use /repositories/<id>/releases, so the path prefix is not
// checked.
func checkReleasePageURL(apiBase, u string) error {
	b, err := url.Parse(apiBase)
	if err != nil {
		return fmt.Errorf("parse GitHub API base: %w", err)
	}
	p, err := url.Parse(u)
	if err != nil || p.Scheme != b.Scheme || !strings.EqualFold(p.Host, b.Host) || p.User != nil ||
		p.Fragment != "" || !strings.HasSuffix(p.Path, "/releases") {
		return fmt.Errorf("release list page URL %q is not a release list on %s", u, apiBase)
	}
	return nil
}

// nextPageLink returns the rel="next" URL of RFC 8288 Link header values, or
// "".
func nextPageLink(values []string) string {
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			segs := strings.Split(part, ";")
			target := strings.TrimSpace(segs[0])
			if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				continue
			}
			for _, param := range segs[1:] {
				k, val, ok := strings.Cut(strings.TrimSpace(param), "=")
				if ok && strings.EqualFold(strings.TrimSpace(k), "rel") && strings.Trim(strings.TrimSpace(val), `"`) == "next" {
					return target[1 : len(target)-1]
				}
			}
		}
	}
	return ""
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
// GitHub owner and repository names are case-insensitive, so the
// "https://github.com/<repo>/" part is compared ignoring case; the rest of
// the prefix (releases/download/<tag>/) must match exactly.
func CheckAssetURL(repo, tag, assetURL string) error {
	prefix := ReleaseDownloadPrefix(repo, tag)
	repoPart := "https://github.com/" + repo + "/"
	if len(assetURL) < len(prefix) ||
		!strings.EqualFold(assetURL[:len(repoPart)], repoPart) ||
		assetURL[len(repoPart):len(prefix)] != prefix[len(repoPart):] {
		return fmt.Errorf("asset URL %q is not under %s", assetURL, prefix)
	}
	u, err := url.Parse(assetURL)
	if err != nil || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("asset URL %q is not a plain github.com release asset", assetURL)
	}
	name := assetURL[len(prefix):]
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

// MaxReleaseRedirects is how many redirects an asset download follows. A
// release download is normally one hop (github.com → *.githubusercontent.com).
const MaxReleaseRedirects = 5

// checkReleaseRedirect is the http.Client CheckRedirect for asset downloads:
// HTTPS only, to *.githubusercontent.com only, at most MaxReleaseRedirects
// redirects. via holds the requests already made, so the Nth redirect is
// checked with len(via) == N.
func checkReleaseRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > MaxReleaseRedirects {
		return fmt.Errorf("too many redirects (more than %d)", MaxReleaseRedirects)
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

// Checksum-entry failure kinds, for errors.Is on ExpectedChecksum errors.
var (
	// ErrChecksumMissing: SHA256SUMS has no well-formed line for the asset.
	ErrChecksumMissing = errors.New("checksum entry missing")
	// ErrChecksumBad: the asset's lines conflict or list the empty-file hash.
	ErrChecksumBad = errors.New("bad checksum entry")
)

// checksumError keeps ExpectedChecksum's message and wraps its kind.
type checksumError struct {
	kind error
	msg  string
}

func (e *checksumError) Error() string { return e.msg }
func (e *checksumError) Unwrap() error { return e.kind }

// sumsLineRE is one sha256sum output line: 64 hex digits, a space, then a
// space (text mode) or "*" (binary mode), then the file name.
var sumsLineRE = regexp.MustCompile(`^([0-9a-fA-F]{64}) [ *](\S.*)$`)

// ExpectedChecksum returns the SHA-256 listed for assetName in the contents
// of a SHA256SUMS file. Only well-formed lines naming assetName are
// considered; every other line, including a bogus entry for SHA256SUMS
// itself, is ignored. It fails closed when there is no such line
// (ErrChecksumMissing), when lines disagree, or when the listed hash is the
// empty-file hash (both ErrChecksumBad).
func ExpectedChecksum(sums []byte, assetName string) (string, error) {
	var found string
	for _, line := range strings.Split(string(sums), "\n") {
		m := sumsLineRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil || m[2] != assetName {
			continue
		}
		h := strings.ToLower(m[1])
		if found != "" && found != h {
			return "", &checksumError{ErrChecksumBad, fmt.Sprintf("conflicting checksum entries for %q in %s", assetName, ChecksumsAssetName)}
		}
		found = h
	}
	if found == "" {
		return "", &checksumError{ErrChecksumMissing, fmt.Sprintf("no well-formed checksum entry for %q in %s", assetName, ChecksumsAssetName)}
	}
	if found == emptySHA256 {
		return "", &checksumError{ErrChecksumBad, fmt.Sprintf("checksum entry for %q in %s is the empty-file hash; refusing to install", assetName, ChecksumsAssetName)}
	}
	return found, nil
}
