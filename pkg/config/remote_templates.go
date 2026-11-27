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

package config

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/ghauth"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fs/sync"
	"github.com/rclone/rclone/fs/walk"
	// Import rclone backends for remote template support
	// GCS and local are already imported in gcp/storage.go
	// Additional backends can be added as needed
	_ "github.com/rclone/rclone/backend/googlecloudstorage"
	_ "github.com/rclone/rclone/backend/local"
)

// RemoteTemplateType represents the type of remote template source
type RemoteTemplateType int

const (
	RemoteTypeUnknown RemoteTemplateType = iota
	RemoteTypeGitHub
	RemoteTypeArchive
	RemoteTypeRclone
)

// RemoteTemplate represents a template fetched from a remote source
type RemoteTemplate struct {
	URI       string
	Type      RemoteTemplateType
	CachePath string
}

// remoteCacheDir returns the directory where remote templates are cached.
func remoteCacheDir() (string, error) {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(globalDir, "cache", "remote-templates"), nil
}

// removeAllCache removes a cache directory before a fetch refills it
// (os.RemoveAll; replaced in tests to force a failure).
var removeAllCache = os.RemoveAll

// NormalizeTemplateSourceURL normalizes a user-provided template source URL.
// It handles two cases of user convenience:
//  1. Missing scheme: if the URL doesn't start with a scheme or rclone prefix,
//     "https://" is prepended automatically (e.g. "github.com/org/repo").
//  2. Bare GitHub org/repo: if the result is a GitHub URL with only owner/repo
//     in the path (no deeper path), "/.scion/templates/" is appended so that
//     the standard template directory is used automatically.
func NormalizeTemplateSourceURL(raw string) string {
	s := strings.TrimSpace(raw)

	// Handle git+https:// and git+http:// scheme prefixes.
	if strings.HasPrefix(s, "git+https://") || strings.HasPrefix(s, "git+http://") {
		s = strings.TrimPrefix(s, "git+")
	}

	// Add https:// scheme if missing (not rclone, not already http/https,
	// and not a gs:// Cloud Storage URL, which a hosted Hub accepts when its
	// allowed_import_sources names the bucket)
	isGS := len(s) >= len("gs://") && strings.EqualFold(s[:len("gs://")], "gs://")
	if !strings.HasPrefix(s, ":") && !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") && !isGS {
		s = "https://" + s
	}

	// For GitHub URLs with just owner/repo, append /.scion/templates/
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	if strings.EqualFold(u.Host, "github.com") {
		// Split path and filter empty segments
		var parts []string
		for _, p := range strings.Split(strings.TrimPrefix(u.Path, "/"), "/") {
			if p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) == 2 {
			// Just owner/repo — point to the standard templates directory
			// Include /tree/main so the URL is a valid GitHub browsable path
			// and parseGitHubURL can correctly extract the branch and sub-path.
			u.Path = "/" + parts[0] + "/" + parts[1] + "/tree/main/.scion/templates"
			return u.String()
		}
	}

	return s
}

// IsRemoteURI checks if the given string looks like a remote template URI.
// Returns true for:
// - URLs starting with http:// or https://
// - rclone connection strings starting with ":"
func IsRemoteURI(s string) bool {
	// rclone connection string (e.g., :gcs:bucket/path)
	if strings.HasPrefix(s, ":") {
		return true
	}

	// HTTP/HTTPS URLs
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return true
	}

	return false
}

// DetectRemoteType determines the type of remote template source.
func DetectRemoteType(uri string) RemoteTemplateType {
	// rclone connection string
	if strings.HasPrefix(uri, ":") {
		return RemoteTypeRclone
	}

	// Parse URL
	u, err := url.Parse(uri)
	if err != nil {
		return RemoteTypeUnknown
	}

	// GitHub folder URL
	if strings.EqualFold(u.Hostname(), "github.com") && !isArchiveURL(uri) {
		return RemoteTypeGitHub
	}

	// Archive URL
	if isArchiveURL(uri) {
		return RemoteTypeArchive
	}

	return RemoteTypeUnknown
}

// isArchiveURL checks if the URL points to a compressed archive.
func isArchiveURL(uri string) bool {
	lower := strings.ToLower(uri)
	return strings.HasSuffix(lower, ".tgz") ||
		strings.HasSuffix(lower, ".tar.gz") ||
		strings.HasSuffix(lower, ".zip")
}

// FetchRemoteTemplate fetches a template from a remote URI and caches it locally.
// Returns the local path to the cached template.
// An optional authToken can be provided for authenticated GitHub access (e.g.
// a GitHub App installation token). Pass "" when no auth is needed.
func FetchRemoteTemplate(ctx context.Context, uri string, authToken ...string) (string, error) {
	token := ""
	if len(authToken) > 0 {
		token = authToken[0]
	}
	return FetchRemoteTemplateWithOptions(ctx, uri, FetchOptions{AuthToken: token})
}

// RemoteDownloadFunc downloads rawURL and returns its body. authorization,
// when non-empty, is the Authorization header value for requests to
// rawURL's own host only. It lets a caller that fetches on behalf of others
// (a hosted Hub) constrain every HTTP download; the implementation bounds the
// body size and the time itself.
type RemoteDownloadFunc func(ctx context.Context, rawURL, authorization string) ([]byte, error)

// FetchOptions configures FetchRemoteTemplateWithOptions. The zero value
// fetches GitHub folders via pkg/githubfetch with NetworkSystem and
// ImportLocalLimits, and non-GitHub archive URLs via http.DefaultClient.
type FetchOptions struct {
	// AuthToken authenticates GitHub folder fetches when GitHubCredential is
	// zero. "" fetches unauthenticated.
	AuthToken string
	// GitHubCredential, when non-zero, supplies the full githubfetch.Credential
	// (token, principal scope, kind, and diagnostic hint) for GitHub folder
	// fetches.
	GitHubCredential githubfetch.Credential
	// GitHubClient, when non-nil, is used for GitHub folder fetches instead of
	// the default local NetworkSystem client.
	GitHubClient *githubfetch.Client
	// GitHubLimits optionally overrides the limits for GitHub folder fetches
	// (defaults to githubfetch.ImportLocalLimits() when nil).
	GitHubLimits *githubfetch.Limits
	// Download, when set, performs non-GitHub HTTP archive downloads instead
	// of http.DefaultClient.
	Download RemoteDownloadFunc
	// MaxBytes, when positive, caps the bytes of file content written while
	// fetching non-GitHub sources (archives, rclone).
	MaxBytes int64
	// MaxFiles, when positive, caps the files and directories written while
	// fetching non-GitHub sources.
	MaxFiles int
	// MaxUnpackedBytes, when positive, caps the decompressed bytes read from
	// a non-GitHub archive in each pass over it (a gzip bomb stops here even
	// when most of its entries are skipped).
	MaxUnpackedBytes int64
	// PrivateCacheDir fetches into a new directory of this fetch's own
	// under the cache, instead of the per-URI directory, so concurrent
	// fetches of one URI do not share a tree. GitHub folder fetches always
	// use a private per-call staging directory. The caller removes it.
	PrivateCacheDir bool
}

// ErrRemoteDownloadTimeout is returned (wrapped) by a download hook when a
// download took too long; a GitHub fetch does not try other archives after
// it.
var ErrRemoteDownloadTimeout = errors.New("the source download timed out")

// ErrRemoteFetchLimit is returned (wrapped) when a fetch exceeds a limit
// set in FetchOptions.
var ErrRemoteFetchLimit = errors.New("remote template source exceeds the size or file-count limit")

// fetchBudget tracks the limits of one fetch. A nil *fetchBudget is
// unlimited.
type fetchBudget struct {
	maxBytes    int64
	maxFiles    int
	maxUnpacked int64
	bytes       int64
	files       int
	// seen holds the directories already counted, keyed by destination
	// root and relative path, so the parents a file write creates
	// implicitly count toward maxFiles once each.
	seen map[string]struct{}
}

// maxFetchEntryName caps the length of one entry name in a limited fetch,
// so deep or long names cannot be used to create many directories or hold
// much memory.
const maxFetchEntryName = 1024

// newFetchBudget returns the budget for opts, or nil when it sets no limit.
func newFetchBudget(opts FetchOptions) *fetchBudget {
	if opts.MaxBytes <= 0 && opts.MaxFiles <= 0 && opts.MaxUnpackedBytes <= 0 {
		return nil
	}
	return &fetchBudget{maxBytes: opts.MaxBytes, maxFiles: opts.MaxFiles, maxUnpacked: opts.MaxUnpackedBytes}
}

// addEntry counts one file or directory written.
func (b *fetchBudget) addEntry() error {
	if b == nil {
		return nil
	}
	b.files++
	if b.maxFiles > 0 && b.files > b.maxFiles {
		return fmt.Errorf("%w (more than %d files and directories)", ErrRemoteFetchLimit, b.maxFiles)
	}
	return nil
}

// addPath counts the entry rel (slash or OS separated, relative to the
// destination root) and every parent directory of it not counted before
// under the same root. isDir marks rel itself as a directory.
func (b *fetchBudget) addPath(root, rel string, isDir bool) error {
	if b == nil {
		return nil
	}
	if len(rel) > maxFetchEntryName {
		return fmt.Errorf("%w (an entry name is longer than %d bytes)", ErrRemoteFetchLimit, maxFetchEntryName)
	}
	if b.seen == nil {
		b.seen = make(map[string]struct{})
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	last := len(parts) - 1
	for i := range parts {
		if i == last && !isDir {
			return b.addEntry()
		}
		key := root + "\x00" + strings.Join(parts[:i+1], "/")
		if _, ok := b.seen[key]; ok {
			continue
		}
		b.seen[key] = struct{}{}
		if err := b.addEntry(); err != nil {
			return err
		}
	}
	return nil
}

// content wraps r, the content of a file being written, so that reading
// past the byte budget fails.
func (b *fetchBudget) content(r io.Reader) io.Reader {
	if b == nil || b.maxBytes <= 0 {
		return r
	}
	return &budgetReader{r: r, b: b}
}

// unpacked wraps r, a decompressed archive stream, so that reading more
// than the unpacked limit fails. Each pass over an archive gets its own cap.
func (b *fetchBudget) unpacked(r io.Reader) io.Reader {
	if b == nil || b.maxUnpacked <= 0 {
		return r
	}
	return &limitedStream{r: r, remaining: b.maxUnpacked, limit: b.maxUnpacked}
}

// rcloneLimits returns the byte and file caps for an rclone copy (0 means
// no cap).
func (b *fetchBudget) rcloneLimits() (int64, int) {
	if b == nil {
		return 0, 0
	}
	return b.maxBytes, b.maxFiles
}

type budgetReader struct {
	r io.Reader
	b *fetchBudget
}

func (br *budgetReader) Read(p []byte) (int, error) {
	if rem := br.b.maxBytes - br.b.bytes; int64(len(p)) > rem+1 {
		if rem < 0 {
			rem = 0
		}
		p = p[:rem+1]
	}
	n, err := br.r.Read(p)
	br.b.bytes += int64(n)
	if br.b.bytes > br.b.maxBytes {
		return n, fmt.Errorf("%w (more than %d bytes)", ErrRemoteFetchLimit, br.b.maxBytes)
	}
	return n, err
}

type limitedStream struct {
	r         io.Reader
	remaining int64
	limit     int64
}

func (l *limitedStream) Read(p []byte) (int, error) {
	if int64(len(p)) > l.remaining+1 {
		p = p[:l.remaining+1]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	if l.remaining < 0 {
		return n, fmt.Errorf("%w (archive unpacks to more than %d bytes)", ErrRemoteFetchLimit, l.limit)
	}
	return n, err
}

// RemoteFetchResult holds the local cache directory and optional GitHub commit
// SHA (40-char lowercase hex) captured during a remote template fetch.
type RemoteFetchResult struct {
	Path   string
	Commit string
}

// FetchRemoteTemplateWithOptions is FetchRemoteTemplate with the download
// hook, GitHub client/limits, and cache directory choice of opts. When a limit
// is set (or PrivateCacheDir, or any GitHub folder fetch) and the fetch fails,
// the partially filled cache directory is removed.
func FetchRemoteTemplateWithOptions(ctx context.Context, uri string, opts FetchOptions) (string, error) {
	res, err := FetchRemoteTemplateWithResult(ctx, uri, opts)
	if err != nil {
		return "", err
	}
	return res.Path, nil
}

// FetchRemoteTemplateWithResult is FetchRemoteTemplateWithOptions returning
// both the local cache directory and the resolved GitHub commit SHA (when
// fetched from a GitHub folder).
func FetchRemoteTemplateWithResult(ctx context.Context, uri string, opts FetchOptions) (*RemoteFetchResult, error) {
	budget := newFetchBudget(opts)
	cacheDir, err := remoteCacheDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get cache directory: %w", err)
	}

	// Create a unique cache key based on the URI
	cacheKey := generateCacheKey(uri)
	templateCachePath := filepath.Join(cacheDir, cacheKey)

	isGitHub := DetectRemoteType(uri) == RemoteTypeGitHub
	privateDir := opts.PrivateCacheDir || isGitHub
	if privateDir {
		// A directory of this fetch's own, so concurrent fetches of the
		// same URI cannot remove or mix each other's trees.
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create cache directory: %w", err)
		}
		if templateCachePath, err = os.MkdirTemp(cacheDir, cacheKey+"-*"); err != nil {
			return nil, fmt.Errorf("failed to create cache directory: %w", err)
		}
	} else {
		// For now, always fetch fresh (could add caching logic later)
		// Clean up any existing cache for this URI. If the old directory cannot
		// be removed, the fetch fails: populating it anyway would mix the new
		// content with whatever is left of the old.
		if err := removeAllCache(templateCachePath); err != nil {
			return nil, fmt.Errorf("failed to remove the old cache directory %s: %w", templateCachePath, err)
		}

		if err := os.MkdirAll(templateCachePath, 0755); err != nil {
			return nil, fmt.Errorf("failed to create cache directory: %w", err)
		}
	}

	cred := resolveFetchCredential(opts)
	f := &remoteFetcher{
		download: opts.Download,
		budget:   budget,
		ghClient: opts.GitHubClient,
		ghLimits: opts.GitHubLimits,
	}
	if err := f.fetchInto(ctx, uri, templateCachePath, cred); err != nil {
		if budget != nil || privateDir {
			_ = os.RemoveAll(templateCachePath)
		}
		return nil, err
	}

	// Whatever the fetch path, a fetched template holds regular files and
	// directories only; refuse it (and drop the cache) otherwise.
	if err := checkRegularTree(templateCachePath); err != nil {
		_ = os.RemoveAll(templateCachePath)
		return nil, err
	}

	return &RemoteFetchResult{
		Path:   templateCachePath,
		Commit: f.sourceCommit,
	}, nil
}

func resolveFetchCredential(opts FetchOptions) githubfetch.Credential {
	c := opts.GitHubCredential
	if c.Token != "" || c.Scope != "" || c.Hint != "" || c.Kind != githubfetch.CredAnonymous {
		return c
	}
	if opts.AuthToken != "" {
		return githubfetch.Credential{
			Token: opts.AuthToken,
			Scope: "env:" + githubfetch.TokenFingerprint(opts.AuthToken)[:32],
			Kind:  githubfetch.CredEnv,
		}
	}
	return githubfetch.Credential{
		Scope: githubfetch.AnonScope,
		Kind:  githubfetch.CredAnonymous,
	}
}

// EnvGitHubCredential resolves the local CLI/process environment credential
// for sourceURL in the unified order GH_{OWNER}__{REPO} > GH_{OWNER} >
// GITHUB_TOKEN, returning a githubfetch.Credential. Non-GitHub sources return
// an anonymous credential without reading environment variables.
func EnvGitHubCredential(sourceURL string) githubfetch.Credential {
	cred, _ := EnvGitHubCredentialWithName(sourceURL)
	return cred
}

// EnvGitHubCredentialWithName is EnvGitHubCredential returning the environment
// variable name that supplied the token (or "" when anonymous), so callers can
// log the variable name without ever logging its value.
func EnvGitHubCredentialWithName(sourceURL string) (githubfetch.Credential, string) {
	owner, repo, ok := GitHubRepoForURL(sourceURL)
	if !ok {
		return githubfetch.Credential{Scope: githubfetch.AnonScope, Kind: githubfetch.CredAnonymous}, ""
	}
	val, name := ghauth.Lookup(owner, repo, os.Getenv)
	if val == "" {
		val, name = os.Getenv(ghauth.DefaultKey), ghauth.DefaultKey
	}
	if val == "" {
		return githubfetch.Credential{Scope: githubfetch.AnonScope, Kind: githubfetch.CredAnonymous}, ""
	}
	return githubfetch.Credential{
		Token: val,
		Scope: "env:" + githubfetch.TokenFingerprint(val)[:32],
		Kind:  githubfetch.CredEnv,
	}, name
}

// remoteFetcher carries the download hook, GitHub client, and budget of one
// fetch.
type remoteFetcher struct {
	download     RemoteDownloadFunc
	budget       *fetchBudget
	ghClient     *githubfetch.Client
	ghLimits     *githubfetch.Limits
	sourceCommit string
}

// fetchInto fetches uri into the existing directory destPath.
func (f *remoteFetcher) fetchInto(ctx context.Context, uri, destPath string, cred githubfetch.Credential) error {
	switch DetectRemoteType(uri) {
	case RemoteTypeGitHub:
		if err := f.fetchGitHubFolder(ctx, uri, destPath, cred); err != nil {
			return fmt.Errorf("failed to fetch GitHub folder: %w", err)
		}
	case RemoteTypeArchive:
		if err := f.fetchArchive(ctx, uri, destPath); err != nil {
			return fmt.Errorf("failed to fetch archive: %w", err)
		}
	case RemoteTypeRclone:
		if err := f.fetchRclone(ctx, uri, destPath); err != nil {
			return fmt.Errorf("failed to fetch via rclone: %w", err)
		}
	default:
		if f.budget != nil {
			return errors.New("unsupported remote template type")
		}
		return fmt.Errorf("unsupported remote template type: %s", uri)
	}
	return nil
}

// remoteHTTPStatusError records a non-200 HTTP response from the default
// client download path without exposing request headers or response bodies.
type remoteHTTPStatusError struct {
	what   string
	status int
}

func (e *remoteHTTPStatusError) Error() string {
	if e.what == "archive" {
		return fmt.Sprintf("failed to download archive: HTTP %d", e.status)
	}
	return fmt.Sprintf("%s download failed: HTTP %d", e.what, e.status)
}

// get downloads rawURL with the download hook, or with http.DefaultClient
// when there is none. A non-200 answer is an error naming its status only
// for the default client; the hook reports its own errors. When authorization
// is non-empty, it is sent only to rawURL's own HTTPS host and never forwarded
// on redirects to any other host, subdomain, or plain HTTP.
func (f *remoteFetcher) get(ctx context.Context, rawURL, authorization, what string) (io.ReadCloser, error) {
	if f.download != nil {
		body, err := f.download(ctx, rawURL, authorization)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	client := http.DefaultClient
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
		c := *http.DefaultClient
		origCheck := c.CheckRedirect
		initialHost := req.URL.Host
		c.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			if origCheck != nil {
				if err := origCheck(next, via); err != nil {
					return err
				}
			} else if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			// Never forward Authorization to any other host (including
			// subdomains or codeload.github.com) or over plain HTTP.
			if !strings.EqualFold(next.URL.Scheme, "https") || !strings.EqualFold(next.URL.Host, initialHost) {
				next.Header.Del("Authorization")
			}
			return nil
		}
		client = &c
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download %s: %w", what, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, &remoteHTTPStatusError{what: what, status: resp.StatusCode}
	}
	return resp.Body, nil
}

// checkRegularTree returns an error if the tree at dir contains anything
// other than directories and regular files (for example a symbolic link).
// It does not follow links.
func checkRegularTree(dir string) error {
	return filepath.WalkDir(dir, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}
		return fmt.Errorf("fetched template contains a non-regular file: %s", filepath.ToSlash(rel))
	})
}

// generateCacheKey creates a unique, filesystem-safe key for a URI.
func generateCacheKey(uri string) string {
	hash := sha256.Sum256([]byte(uri))
	return fmt.Sprintf("%x", hash[:8]) // Use first 8 bytes (16 hex chars)
}

// defaultGitHubClientForTest, when non-nil, overrides defaultLocalGitHubClient
// so tests in pkg/config, cmd, and pkg/hub can inject an httptest-backed
// githubfetch.Client without contacting the network.
var defaultGitHubClientForTest *githubfetch.Client

// SetGitHubClientForTest replaces the default githubfetch.Client used by
// FetchRemoteTemplate for GitHub folder fetches when FetchOptions.GitHubClient
// is nil, and returns a func that restores the previous client.
//
// TEST-ONLY: never call this from production code. It mutates package-global
// state and is not safe for use from parallel tests.
func SetGitHubClientForTest(c *githubfetch.Client) (restore func()) {
	prev := defaultGitHubClientForTest
	defaultGitHubClientForTest = c
	return func() { defaultGitHubClientForTest = prev }
}

func defaultLocalGitHubClient(lim githubfetch.Limits) *githubfetch.Client {
	if defaultGitHubClientForTest != nil {
		return defaultGitHubClientForTest
	}
	ep := githubfetch.DefaultEndpoint()
	reqTimeout := lim.RequestTimeout
	if reqTimeout <= 0 {
		reqTimeout = lim.Timeout
	}
	doer := githubfetch.NewDoer(ep, remotefetch.NetworkSystem, lim.MaxDownload, reqTimeout, slog.Default())
	return githubfetch.New(githubfetch.Options{
		Endpoint:   ep,
		Doer:       doer,
		Cooldown:   githubfetch.SharedCooldown(),
		MaxRetries: 3,
	})
}

// fetchGitHubFolder fetches a folder from a GitHub repository via
// pkg/githubfetch into destPath opened as an *os.Root. No git binary is
// invoked.
func (f *remoteFetcher) fetchGitHubFolder(ctx context.Context, uri, destPath string, cred githubfetch.Credential) error {
	src, err := githubfetch.ParseURL(uri)
	if err != nil {
		return err
	}
	lim := githubfetch.ImportLocalLimits()
	if f.ghLimits != nil {
		lim = *f.ghLimits
	}
	client := f.ghClient
	if client == nil {
		client = defaultLocalGitHubClient(lim)
	}
	root, err := os.OpenRoot(destPath)
	if err != nil {
		return fmt.Errorf("failed to open staging directory: %w", err)
	}
	res, fetchErr := client.FetchFolder(ctx, src, cred, root, lim)
	closeErr := root.Close()
	if fetchErr != nil {
		return fetchErr
	}
	if closeErr != nil {
		return closeErr
	}
	f.sourceCommit = res.CommitSHA
	return nil
}

// gitLsRemoteTimeout bounds a single production `git ls-remote` invocation for
// welcome-project pinned ref resolution (execGitLsRemoteRef).
var gitLsRemoteTimeout = 30 * time.Second

// gitLsRemoteWaitDelay bounds how long execGitLsRemoteRef waits for the git
// process's output pipes to close after the timeout kills git.
var gitLsRemoteWaitDelay = 5 * time.Second

// redactedRepoName returns the "owner/repo" portion of a git remote URL with
// any credentials stripped, for use in logs and error messages.
func redactedRepoName(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil || u.Path == "" {
		return "<unknown repository>"
	}
	return strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
}

// GitHubRepoForURL returns the owner and repository of uri when
// FetchRemoteTemplate would fetch it as a GitHub folder (the only remote type
// that sends a credential). ok is false for archive, rclone and other URLs,
// and for GitHub URLs without an owner/repo path.
func GitHubRepoForURL(uri string) (owner, repo string, ok bool) {
	if DetectRemoteType(uri) != RemoteTypeGitHub {
		return "", "", false
	}
	src, err := githubfetch.ParseURL(uri)
	if err != nil || src.Owner == "" || src.Repo == "" {
		return "", "", false
	}
	return src.Owner, src.Repo, true
}

// writeFileInRoot creates (or truncates) the regular file name inside root,
// creating missing parent directories, and fills it from r. All path
// resolution goes through root, so the write cannot land outside it.
func writeFileInRoot(root *os.Root, name string, r io.Reader, mode iofs.FileMode) error {
	if dir := filepath.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// dirPerm returns the permission bits for a directory created from fetched
// content: the source's permission bits, always owner-accessible so later
// entries can be written into it.
func dirPerm(mode iofs.FileMode) iofs.FileMode {
	return mode.Perm() | 0o700
}

// fetchArchive downloads and extracts a compressed archive.
func (f *remoteFetcher) fetchArchive(ctx context.Context, uri string, destPath string) error {
	// Download the archive to a temp file
	tmpFile, err := os.CreateTemp("", "scion-archive-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	defer func() { _ = tmpFile.Close() }()

	// Download
	body, err := f.get(ctx, uri, "", "archive")
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	if _, err := io.Copy(tmpFile, body); err != nil {
		return fmt.Errorf("failed to save archive: %w", err)
	}
	_ = tmpFile.Close()

	// Extract based on file type
	lower := strings.ToLower(uri)
	if strings.HasSuffix(lower, ".zip") {
		return f.extractZip(tmpPath, destPath)
	} else if strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.gz") {
		err := f.extractTarGz(tmpPath, destPath)
		f.sourceCommit = ""
		return err
	}

	if f.budget != nil {
		return errors.New("unsupported archive format")
	}
	return fmt.Errorf("unsupported archive format: %s", uri)
}

// extractZip extracts a zip archive to the destination path.
//
// Only directories and regular files are extracted; other entry types are
// skipped (logged). Entry names are checked by sanitizePath, and every write
// goes through an os.Root opened at destPath, so all writes stay inside it.
func extractZip(zipPath, destPath string) error {
	return (&remoteFetcher{}).extractZip(zipPath, destPath)
}

func (f *remoteFetcher) extractZip(zipPath, destPath string) error {
	if f.budget != nil {
		// zip.OpenReader parses the whole central directory into memory,
		// so bound it first.
		if err := checkZipDirectory(zipPath, f.budget.maxFiles); err != nil {
			return err
		}
	}
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("failed to open zip: %w", err)
	}
	defer func() { _ = r.Close() }()

	root, err := os.OpenRoot(destPath)
	if err != nil {
		return fmt.Errorf("failed to open destination directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	// Detect if there's a common root directory
	commonRoot := detectCommonRoot(func(yield func(string) bool) {
		for _, zf := range r.File {
			if !yield(zf.Name) {
				return
			}
		}
	})

	for _, zf := range r.File {
		name := zf.Name

		// Strip common root if present
		if commonRoot != "" {
			name = strings.TrimPrefix(name, commonRoot)
			if name == "" {
				continue // Skip the root directory itself
			}
		}

		// Sanitize path to prevent zip slip attacks
		name = sanitizePath(name)
		if name == "" {
			continue
		}

		mode := zf.Mode()
		if mode.IsDir() {
			if err := f.budget.addPath(destPath, name, true); err != nil {
				return err
			}
			if err := root.MkdirAll(name, dirPerm(mode)); err != nil {
				return err
			}
			continue
		}
		if !mode.IsRegular() {
			slog.Warn("Skipping non-regular archive entry", "name", zf.Name, "type", mode.Type().String())
			continue
		}
		if err := f.budget.addPath(destPath, name, false); err != nil {
			return err
		}

		rc, err := zf.Open()
		if err != nil {
			return err
		}
		err = writeFileInRoot(root, name, f.budget.content(rc), mode)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

// Zip record layout used by checkZipDirectory.
const (
	zipEndLen           = 22         // end of central directory record, without comment
	zipEndSignature     = 0x06054b50 // end of central directory record
	zip64LocatorLen     = 20         // ZIP64 end of central directory locator
	zip64LocatorSig     = 0x07064b50 // ZIP64 end of central directory locator
	zipDirLen           = 46         // central directory record, without variable fields
	zipDirSignature     = 0x02014b50 // central directory record
	zipMaxCommentLen    = 0xffff
	zipMaxRecordExtra   = 256 // extra field and comment bytes allowed per record on average
	zipMaxRecordAverage = zipDirLen + maxFetchEntryName + zipMaxRecordExtra
)

// checkZipDirectory bounds the central directory of the zip at path before
// it is parsed: zip.OpenReader holds every record in memory, and the
// per-entry budget checks run only after that. It refuses, with
// ErrRemoteFetchLimit, a ZIP64 archive (not needed within a fetch budget),
// an end record declaring more than maxFiles entries or a directory too
// large for that many, a directory that does not end where the end record
// begins, and a directory holding more than maxFiles records or a name
// longer than maxFetchEntryName. The record walk is needed because the Go
// reader checks the declared count only modulo 65536 and reads records
// until it meets another signature. maxFiles <= 0 leaves the count
// unbounded.
func checkZipDirectory(path string, maxFiles int) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open zip: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to open zip: %w", err)
	}
	size := info.Size()
	tailLen := int64(zipEndLen + zipMaxCommentLen)
	if tailLen > size {
		tailLen = size
	}
	tail := make([]byte, tailLen)
	if _, err := file.ReadAt(tail, size-tailLen); err != nil {
		return fmt.Errorf("failed to read zip: %w", err)
	}
	// The last end-record signature, as the Go reader finds it.
	at := -1
	for i := len(tail) - zipEndLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) == zipEndSignature {
			at = i
			break
		}
	}
	if at < 0 {
		return errors.New("failed to open zip: no end of central directory record")
	}
	end := tail[at : at+zipEndLen]
	endOffset := size - tailLen + int64(at)
	count := binary.LittleEndian.Uint16(end[10:])
	dirSize := binary.LittleEndian.Uint32(end[12:])
	dirOffset := binary.LittleEndian.Uint32(end[16:])
	zip64 := count == 0xffff || binary.LittleEndian.Uint16(end[8:]) == 0xffff ||
		dirSize == 0xffffffff || dirOffset == 0xffffffff
	if !zip64 && endOffset >= zip64LocatorLen {
		// The Go reader takes the ZIP64 record's values whenever this
		// locator is present.
		var sig [4]byte
		if _, err := file.ReadAt(sig[:], endOffset-zip64LocatorLen); err == nil {
			zip64 = binary.LittleEndian.Uint32(sig[:]) == zip64LocatorSig
		}
	}
	if zip64 {
		return fmt.Errorf("%w (ZIP64 archives are not supported)", ErrRemoteFetchLimit)
	}
	if maxFiles > 0 {
		if int(count) > maxFiles {
			return fmt.Errorf("%w (more than %d archive entries)", ErrRemoteFetchLimit, maxFiles)
		}
		if int64(dirSize) > int64(maxFiles)*zipMaxRecordAverage {
			return fmt.Errorf("%w (the zip central directory is too large for %d entries)", ErrRemoteFetchLimit, maxFiles)
		}
	}
	if int64(dirOffset)+int64(dirSize) != endOffset {
		// Prepended data, or records between the directory and the end
		// record; the Go reader would guess where the directory starts.
		return fmt.Errorf("%w (the zip central directory is not where its end record says)", ErrRemoteFetchLimit)
	}
	// Walk the records as the Go reader will: from the directory offset
	// until a record is short or has another signature. The Go reader may
	// stop sooner (on a malformed extra field), never later.
	br := bufio.NewReader(io.NewSectionReader(file, int64(dirOffset), size-int64(dirOffset)))
	hdr := make([]byte, zipDirLen)
	for records := 0; ; records++ {
		if _, err := io.ReadFull(br, hdr); err != nil || binary.LittleEndian.Uint32(hdr) != zipDirSignature {
			return nil
		}
		if maxFiles > 0 && records >= maxFiles {
			return fmt.Errorf("%w (more than %d archive entries)", ErrRemoteFetchLimit, maxFiles)
		}
		nameLen := int(binary.LittleEndian.Uint16(hdr[28:]))
		if nameLen > maxFetchEntryName {
			return fmt.Errorf("%w (an entry name is longer than %d bytes)", ErrRemoteFetchLimit, maxFetchEntryName)
		}
		rest := nameLen + int(binary.LittleEndian.Uint16(hdr[30:])) + int(binary.LittleEndian.Uint16(hdr[32:]))
		if n, _ := br.Discard(rest); n < rest {
			return nil
		}
	}
}

// extractTarGz extracts a .tar.gz or .tgz archive to the destination path.
//
// Only directories and regular files are extracted; link, device and FIFO
// entries are skipped (logged). Entry names are checked by sanitizePath, and
// every write goes through an os.Root opened at destPath, so all writes stay
// inside it.
func extractTarGz(tarGzPath, destPath string) error {
	return (&remoteFetcher{}).extractTarGz(tarGzPath, destPath)
}

func (rf *remoteFetcher) extractTarGz(tarGzPath, destPath string) error {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return fmt.Errorf("failed to open archive: %w", err)
	}
	defer func() { _ = f.Close() }()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() { _ = gzr.Close() }()

	// First pass: detect common root. Only consider regular files and
	// directories — pax global headers (TypeXGlobalHeader) and other
	// metadata entries must be skipped or they break common-root detection
	// (GitHub tarballs start with a pax_global_header entry whose name
	// does not share the repo-root prefix).
	tarReader := tar.NewReader(rf.budget.unpacked(gzr))
	var entries []string
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar header: %w", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if header.Typeflag == tar.TypeDir || header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeSymlink {
			if rf.budget != nil {
				// Bound the names held for common-root detection.
				if len(header.Name) > maxFetchEntryName {
					return fmt.Errorf("%w (an entry name is longer than %d bytes)", ErrRemoteFetchLimit, maxFetchEntryName)
				}
				if rf.budget.maxFiles > 0 && len(entries) >= rf.budget.maxFiles {
					return fmt.Errorf("%w (more than %d archive entries)", ErrRemoteFetchLimit, rf.budget.maxFiles)
				}
			}
			entries = append(entries, header.Name)
		}
	}

	commonRoot := detectCommonRoot(func(yield func(string) bool) {
		for _, e := range entries {
			if !yield(e) {
				return
			}
		}
	})

	// Reopen for extraction
	_ = f.Close()
	f, err = os.Open(tarGzPath)
	if err != nil {
		return fmt.Errorf("failed to reopen archive: %w", err)
	}
	defer func() { _ = f.Close() }()

	gzr, err = gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() { _ = gzr.Close() }()

	tarReader = tar.NewReader(rf.budget.unpacked(gzr))

	root, err := os.OpenRoot(destPath)
	if err != nil {
		return fmt.Errorf("failed to open destination directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar header: %w", err)
		}

		name := header.Name

		// Strip common root if present
		if commonRoot != "" {
			name = strings.TrimPrefix(name, commonRoot)
			if name == "" {
				continue
			}
		}

		// Sanitize path
		name = sanitizePath(name)
		if name == "" {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := rf.budget.addPath(destPath, name, true); err != nil {
				return err
			}
			if err := root.MkdirAll(name, dirPerm(os.FileMode(header.Mode))); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := rf.budget.addPath(destPath, name, false); err != nil {
				return err
			}
			if err := writeFileInRoot(root, name, rf.budget.content(tarReader), os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			slog.Warn("Skipping link entry in archive", "name", header.Name)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			slog.Warn("Skipping device or FIFO entry in archive", "name", header.Name)
		}
	}

	return nil
}

// detectCommonRoot detects if all entries share a common root directory.
// This is common in archives created from a folder (e.g., repo-main/).
func detectCommonRoot(entries func(func(string) bool)) string {
	var firstDir string
	allShareRoot := true

	entries(func(name string) bool {
		// Get the first path component
		parts := strings.SplitN(name, "/", 2)
		if len(parts) == 0 {
			return true
		}

		dir := parts[0]
		if firstDir == "" {
			firstDir = dir + "/"
		} else if dir+"/" != firstDir && !strings.HasPrefix(name, firstDir) {
			allShareRoot = false
			return false // Stop iteration
		}
		return true
	})

	if allShareRoot && firstDir != "" {
		return firstDir
	}
	return ""
}

// sanitizePath prevents path traversal attacks.
func sanitizePath(name string) string {
	// Clean the path
	name = filepath.Clean(name)

	// Reject absolute paths
	if filepath.IsAbs(name) {
		return ""
	}

	// Reject paths that try to escape
	if strings.HasPrefix(name, "..") || strings.Contains(name, string(filepath.Separator)+"..") {
		return ""
	}

	return name
}

// fetchRclone fetches a template using rclone from a remote storage location.
func (f *remoteFetcher) fetchRclone(ctx context.Context, uri string, destPath string) error {
	// uri is expected to be in rclone format, e.g., :gcs:bucket/path or :s3:bucket/path
	srcFs, err := fs.NewFs(ctx, uri)
	if err != nil {
		if f.budget != nil {
			return fmt.Errorf("failed to create source fs: %w", err)
		}
		return fmt.Errorf("failed to create source fs for %s: %w", uri, err)
	}

	maxBytes, maxFiles := f.budget.rcloneLimits()
	if maxBytes > 0 || maxFiles > 0 {
		// rclone checks the transfer cap against the stats group in ctx;
		// without one of its own, that is the process-wide group, whose
		// byte count never resets, so every limited fetch gets its own and
		// deletes it on exit so groups do not accumulate.
		groupName := fmt.Sprintf("scion-template-fetch-%d", rcloneStatsSeq.Add(1))
		ctx = accounting.WithStatsGroup(ctx, groupName)
		defer deleteRcloneStatsGroup(groupName)

		// Pre-count the source before copying anything, stopping early as
		// soon as maxFiles+1 objects or >maxBytes total size is seen.
		if err := checkRcloneSourceLimits(ctx, srcFs, maxBytes, maxFiles); err != nil {
			return err
		}
		if maxBytes > 0 {
			var ci *fs.ConfigInfo
			ctx, ci = fs.AddConfig(ctx)
			ci.MaxTransfer = fs.SizeSuffix(maxBytes)
			ci.CutoffMode = fs.CutoffModeHard
		}
	}

	dstFs, err := fs.NewFs(ctx, destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination fs: %w", err)
	}
	if maxFiles > 0 {
		dstFs = &limitedDestFs{Fs: dstFs, maxFiles: int64(maxFiles)}
	}

	// Sync from remote to local
	if err := sync.Sync(ctx, dstFs, srcFs, false); err != nil {
		if errors.Is(err, accounting.ErrorMaxTransferLimitReached) || errors.Is(err, accounting.ErrorMaxTransferLimitReachedFatal) {
			return fmt.Errorf("%w (transfer limit reached)", ErrRemoteFetchLimit)
		}
		if errors.Is(err, ErrRemoteFetchLimit) {
			return err
		}
		return fmt.Errorf("rclone sync failed: %w", err)
	}

	return nil
}

// checkRcloneSourceLimits walks srcFs and returns ErrRemoteFetchLimit as soon
// as more than maxFiles objects or more than maxBytes total bytes are seen,
// without enumerating the remainder of the prefix.
func checkRcloneSourceLimits(ctx context.Context, srcFs fs.Fs, maxBytes int64, maxFiles int) error {
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var objects int64
	var size int64
	var limitErr error
	err := walk.ListR(listCtx, srcFs, "", false, -1, walk.ListObjects, func(entries fs.DirEntries) error {
		for _, entry := range entries {
			obj, ok := entry.(fs.Object)
			if !ok {
				continue
			}
			objects++
			if maxFiles > 0 && objects > int64(maxFiles) {
				limitErr = fmt.Errorf("%w (more than %d files)", ErrRemoteFetchLimit, maxFiles)
				cancel()
				return limitErr
			}
			if s := obj.Size(); s > 0 {
				size += s
			}
			if maxBytes > 0 && size > maxBytes {
				limitErr = fmt.Errorf("%w (more than %d bytes)", ErrRemoteFetchLimit, maxBytes)
				cancel()
				return limitErr
			}
		}
		return nil
	})
	if limitErr != nil {
		return limitErr
	}
	if err != nil {
		return fmt.Errorf("failed to list source: %w", err)
	}
	return nil
}

// limitedDestFs wraps a destination fs.Fs to enforce a hard file-count cap
// during sync.Sync (covering 0-byte objects and objects added after pre-count).
type limitedDestFs struct {
	fs.Fs
	maxFiles int64
	written  atomic.Int64
}

func (l *limitedDestFs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	if l.written.Add(1) > l.maxFiles {
		return nil, fserrors.FatalError(fmt.Errorf("%w (more than %d files)", ErrRemoteFetchLimit, l.maxFiles))
	}
	return l.Fs.Put(ctx, in, src, options...)
}

// deleteRcloneStatsGroup deletes a per-fetch rclone stats group so repeated
// fetches do not accumulate global stats entries.
func deleteRcloneStatsGroup(groupName string) {
	if call := rc.Calls.Get("core/stats-delete"); call != nil {
		_, _ = call.Fn(context.Background(), rc.Params{"group": groupName})
	}
}

// rcloneStatsSeq numbers the rclone stats groups of limited fetches.
var rcloneStatsSeq atomic.Uint64

// CleanRemoteTemplateCache cleans up the remote template cache.
func CleanRemoteTemplateCache() error {
	cacheDir, err := remoteCacheDir()
	if err != nil {
		return err
	}
	return os.RemoveAll(cacheDir)
}

// TODO: Future enhancement - simple template names may resolve to remote storage when
// operating with a remote hub system. The resolution could follow a pattern like:
// <bucket-name>/<scion-prefix>/<project-id>/templates/<template-name>
// This would allow templates to be shared across a team or organization via
// a central hub, using both the current project's location as well as a global
// location on the hub. The hub integration would need to provide:
// - Project ID resolution
// - Hub bucket/prefix configuration
// - Fallback chain: local project -> hub project -> hub global -> error

// rcloneURIPattern matches rclone remote URI syntax (e.g., :backend:path).
var rcloneURIPattern = regexp.MustCompile(`^:\w+:.+`)

// ValidateRemoteURI performs basic validation on a remote template URI.
func ValidateRemoteURI(uri string) error {
	if !IsRemoteURI(uri) {
		return fmt.Errorf("not a remote URI: %s", uri)
	}

	remoteType := DetectRemoteType(uri)

	switch remoteType {
	case RemoteTypeGitHub:
		// Validate GitHub URL format
		_, err := githubfetch.ParseURL(uri)
		return err

	case RemoteTypeArchive:
		// Validate URL format
		_, err := url.Parse(uri)
		return err

	case RemoteTypeRclone:
		// Basic validation - should match :backend:path format
		if !rcloneURIPattern.MatchString(uri) {
			return fmt.Errorf("invalid rclone URI format: %s (expected :backend:path)", uri)
		}
		return nil

	default:
		return fmt.Errorf("unsupported remote URI type: %s", uri)
	}
}

// ---------------------------------------------------------------------------
// Pinned GitHub resolution (ls-remote by ref, archive by commit sha)
// ---------------------------------------------------------------------------

// GitLsRemoteRefFunc runs `git ls-remote <repoURL> <ref>` anonymously and
// returns its stdout. It is the signature of the by-ref ls-remote seam used
// by ResolveGitHubRefSHA.
type GitLsRemoteRefFunc func(ctx context.Context, repoURL, ref string) ([]byte, error)

// gitLsRemoteRef is the by-ref ls-remote runner. It is a package-level
// variable only so tests can replace it (SetGitLsRemoteRefForTest).
var gitLsRemoteRef GitLsRemoteRefFunc = execGitLsRemoteRef

// gitHTTPSOnlyArgs restrict a remote git command to the https transport.
var gitHTTPSOnlyArgs = []string{"-c", "protocol.allow=never", "-c", "protocol.https.allow=always"}

// execGitLsRemoteRef is the production by-ref ls-remote runner. Like
// execGitLsRemote it runs host-side git with the remote authentication layer
// and no token, so git offers no credential and no credential helper runs;
// it also allows the https transport only. The call is bounded by
// gitLsRemoteTimeout.
func execGitLsRemoteRef(ctx context.Context, repoURL, ref string) ([]byte, error) {
	if !strings.HasPrefix(repoURL, "https://") {
		return nil, fmt.Errorf("git ls-remote: only https repositories are supported")
	}
	tctx, cancel := context.WithTimeout(ctx, gitLsRemoteTimeout)
	defer cancel()

	args := append(append(util.HostGitArgs(), gitHTTPSOnlyArgs...), "ls-remote", "--", repoURL, ref)
	cmd := exec.CommandContext(tctx, "git", args...)
	cmd.Env = util.HostGitRemoteEnv(util.HostGitEnv(os.Environ()), repoURL, "")
	cmd.WaitDelay = gitLsRemoteWaitDelay
	out, err := cmd.Output()
	if err != nil && ctx.Err() == nil && errors.Is(tctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("git ls-remote for %s timed out after %s: %w",
			redactedRepoName(repoURL), gitLsRemoteTimeout, context.DeadlineExceeded)
	}
	return out, err
}

// SetGitLsRemoteRefForTest replaces the by-ref ls-remote runner and returns
// a func that restores the previous one.
//
// TEST-ONLY: never call this from production code. It mutates package-global
// state and is not safe for use from parallel tests.
func SetGitLsRemoteRefForTest(fn GitLsRemoteRefFunc) (restore func()) {
	prev := gitLsRemoteRef
	gitLsRemoteRef = fn
	return func() { gitLsRemoteRef = prev }
}

var (
	fullGitSHARe    = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	gitRefNameRe    = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,255}$`)
	githubOwnerIDRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38})$`)
	githubRepoIDRe  = regexp.MustCompile(`^[a-z0-9._-]{1,100}$`)
)

// IsFullGitSHA reports whether s is a full 40-hex commit id.
func IsFullGitSHA(s string) bool { return fullGitSHARe.MatchString(s) }

// ErrGitRefUnresolvable reports a ref that names no commit on the remote.
var ErrGitRefUnresolvable = errors.New("ref does not resolve on the remote")

// ErrGitRefAmbiguous reports a ref that names more than one commit (for
// example a branch and a tag of the same name pointing at different commits).
var ErrGitRefAmbiguous = errors.New("ref is ambiguous on the remote")

// ResolveGitHubRefSHA resolves ref on github.com/<owner>/<repo> to a full
// commit sha with no GitHub API call:
//   - a full 40-hex sha is returned as is (lower case), without a remote call;
//   - "" means HEAD;
//   - any other ref is resolved by `git ls-remote <repo> <ref>` for that ref:
//     an exact refs/heads/<ref> or refs/tags/<ref> match (a peeled tag wins
//     over its tag object), or a full refs/... name. No match fails with
//     ErrGitRefUnresolvable and two different commits with
//     ErrGitRefAmbiguous.
//
// owner and repo must already be canonical (CanonicalGitHubRepo).
func ResolveGitHubRefSHA(ctx context.Context, owner, repo, ref string) (string, error) {
	if !githubOwnerIDRe.MatchString(owner) || !githubRepoIDRe.MatchString(repo) || repo == "." || repo == ".." {
		return "", fmt.Errorf("invalid GitHub repository")
	}
	if IsFullGitSHA(ref) {
		return strings.ToLower(ref), nil
	}
	if ref == "" {
		ref = "HEAD"
	}
	if !gitRefNameRe.MatchString(ref) || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") {
		return "", fmt.Errorf("invalid ref: %w", ErrGitRefUnresolvable)
	}
	repoURL := "https://github.com/" + owner + "/" + repo + ".git"
	out, err := gitLsRemoteRef(ctx, repoURL, ref)
	if err != nil {
		return "", fmt.Errorf("git ls-remote for %s/%s failed: %w", owner, repo, err)
	}
	return matchLsRemoteRef(out, ref)
}

// matchLsRemoteRef picks the commit ref names from `git ls-remote` output.
func matchLsRemoteRef(out []byte, ref string) (string, error) {
	shas := map[string]string{} // ref name -> sha
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		sha, name, ok := strings.Cut(line, "\t")
		if !ok || !IsFullGitSHA(sha) {
			continue
		}
		shas[name] = strings.ToLower(sha)
	}
	var candidates []string
	if ref == "HEAD" || strings.HasPrefix(ref, "refs/") {
		candidates = []string{ref}
	} else {
		candidates = []string{"refs/heads/" + ref, "refs/tags/" + ref}
	}
	found := map[string]bool{}
	for _, name := range candidates {
		if peeled, ok := shas[name+"^{}"]; ok {
			found[peeled] = true
			continue
		}
		if sha, ok := shas[name]; ok {
			found[sha] = true
		}
	}
	switch len(found) {
	case 0:
		return "", ErrGitRefUnresolvable
	case 1:
		for sha := range found {
			return sha, nil
		}
	}
	return "", ErrGitRefAmbiguous
}

// CanonicalGitHubRepo returns the canonical identity owner/repo of a GitHub
// repository URL: owner and repo lower-cased, a ".git" suffix stripped. Only
// https://github.com/<owner>/<repo>[.git][/] is accepted: no other scheme or
// host, no userinfo, no port, no query or fragment and no further path.
func CanonicalGitHubRepo(raw string) (owner, repo string, err error) {
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", fmt.Errorf("invalid repository URL")
	}
	// u.Host carries any port, so "github.com:443" is refused here too.
	if u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", "", fmt.Errorf("repository must be an https://github.com URL with no credentials, port, query or fragment")
	}
	p := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")
	parts := strings.Split(p, "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("repository URL must name exactly one owner and repository")
	}
	return CanonicalGitHubOwnerRepo(parts[0], parts[1])
}

// CanonicalGitHubOwnerRepo canonicalizes an owner and repository name pair:
// lower case, ".git" suffix stripped, GitHub name rules enforced.
func CanonicalGitHubOwnerRepo(owner, repo string) (string, string, error) {
	owner = strings.ToLower(owner)
	repo = strings.TrimSuffix(strings.ToLower(repo), ".git")
	if !githubOwnerIDRe.MatchString(owner) || !githubRepoIDRe.MatchString(repo) || repo == "." || repo == ".." {
		return "", "", fmt.Errorf("invalid GitHub owner or repository name")
	}
	return owner, repo, nil
}

// GitHubArchiveURLBySHA returns the public archive URL of a GitHub
// repository at a commit: https://github.com/<owner>/<repo>/archive/<sha>.tar.gz.
// It is the by-sha sibling of the branch tarball fetchGitHubTarball
// downloads; sha must be a full 40-hex commit id.
func GitHubArchiveURLBySHA(owner, repo, sha string) (string, error) {
	if !IsFullGitSHA(sha) {
		return "", fmt.Errorf("archive by sha needs a full commit id")
	}
	owner, repo, err := CanonicalGitHubOwnerRepo(owner, repo)
	if err != nil {
		return "", err
	}
	return "https://github.com/" + owner + "/" + repo + "/archive/" + strings.ToLower(sha) + ".tar.gz", nil
}
