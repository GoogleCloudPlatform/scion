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

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

const (
	githubAPIBase     = "https://api.github.com"
	githubRawBase     = "https://raw.githubusercontent.com"
	githubAPITimeout  = 30 * time.Second
	githubMaxFileSize = 10 * 1024 * 1024 // 10MB per file

	githubMaxRetries    = 4
	githubBaseBackoff   = 1 * time.Second
	githubMaxBackoff    = 30 * time.Second
	githubBackoffFactor = 2.0

	// githubRequestTimeout bounds a single metadata HTTP attempt (resolving a
	// commit SHA or listing directory contents), independent of both
	// githubAPITimeout above and the caller's own ctx deadline. Before this, a
	// stalled connection relied solely on githubAPITimeout (30s) to give up —
	// the same order of magnitude as the ~30s agent-create deadline, so one
	// hung request could consume the entire budget before doWithRetry's
	// budget-fitting logic (below) ever got a chance to back off or fail
	// fast. GitHub API requests normally complete in well under a second even
	// from a loaded broker, so 5s is generous slack for a single attempt
	// while still leaving room for several attempts inside a 30s create
	// deadline.
	githubRequestTimeout = 5 * time.Second

	// githubDownloadRequestTimeout bounds a single raw-file download attempt.
	// It is longer than githubRequestTimeout because a download transfers up
	// to githubMaxFileSize (10MB), not a small JSON/text response: at the 5s
	// metadata timeout, finishing a 10MB body would require ~16 Mbit/s from
	// the broker, which regressed the pre-fix 30s allowance for no benefit
	// (a legitimate slow-but-progressing transfer would be killed early). A
	// connection that never responds at all is still caught quickly via the
	// httpClient's ResponseHeaderTimeout (set in NewGitHubSkillResolver to
	// githubRequestTimeout), so this longer ctx timeout only ever bounds a
	// download that is actually receiving bytes (#2546 O2).
	githubDownloadRequestTimeout = 20 * time.Second

	// githubResolveBudget bounds a Resolve call when the caller's ctx carries
	// no deadline — the production shape: the broker's create ctx is built
	// with context.WithCancel(context.Background()) on the control-channel
	// path and r.Context() on the direct-HTTP path, and nothing between
	// createAgent and GitHubSkillResolver.Resolve ever adds a deadline. With
	// no deadline, ctx.Deadline() in doWithRetry below always reports
	// ok=false, so the budget-fitting fail-fast check — the actual fix for
	// #2546 — was dead code in production; only the CLI's outer ~30s
	// http.Client.Timeout eventually gave up, as a bare "context canceled".
	// 20s leaves slack under that 30s for hub/broker overhead (auth checks,
	// the control-channel tunnel round trip, cleanup) while still giving
	// doWithRetry's existing deadline-based logic a real deadline to work
	// with (#2546 R1).
	githubResolveBudget = 20 * time.Second
)

// GitHubSkillResolver resolves skills from GitHub repositories
// using the GitHub Contents API.
type GitHubSkillResolver struct {
	httpClient           *http.Client
	token                string            // Default GITHUB_TOKEN for authenticated requests
	provisionCredentials map[string]string // Per-URI named credentials from ProvisionCredentials
	apiBase              string            // Default: githubAPIBase, override in tests
	rawBase              string            // Default: githubRawBase, override in tests
	resolutionCache      *GitHubResolutionCache

	// Zero values fall back to githubResolveBudget, githubRequestTimeout and
	// githubDownloadRequestTimeout; tests shrink them to run quickly.
	resolveBudget   time.Duration
	requestTimeout  time.Duration
	downloadTimeout time.Duration
}

// durationOr returns d, or def when d is unset.
func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// newGitHubHTTPClient clones http.DefaultTransport, so proxy settings
// (ProxyFromEnvironment), dial/TLS timeouts and pooling survive, and adds a
// ResponseHeaderTimeout of stall: a connection that never responds at all
// fails within stall, independent of the per-attempt ctx timeout, so the
// longer raw-download timeout (#2546 O2) does not reintroduce slow failure.
func newGitHubHTTPClient(stall time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = stall
	return &http.Client{Timeout: githubAPITimeout, Transport: tr}
}

// stallBound is how long an attempt may go without any response before it
// fails: the transport's ResponseHeaderTimeout, as set by
// newGitHubHTTPClient. Reading it from the transport keeps doWithRetry's
// budget reservation in step with the real bound (#2546 N1). Clients
// without one (e.g. test servers' clients) fall back to the metadata
// attempt timeout, which then bounds a stall instead.
func (r *GitHubSkillResolver) stallBound() time.Duration {
	if r.httpClient != nil {
		if tr, ok := r.httpClient.Transport.(*http.Transport); ok && tr.ResponseHeaderTimeout > 0 {
			return tr.ResponseHeaderTimeout
		}
	}
	return durationOr(r.requestTimeout, githubRequestTimeout)
}

// NewGitHubSkillResolver creates a resolver for gh:// and GitHub URL skills.
// Reads GITHUB_TOKEN from environment for authenticated API access.
// If a resolution cache directory is available, cached resolution results
// are reused to avoid redundant GitHub API calls.
func NewGitHubSkillResolver() *GitHubSkillResolver {
	var cache *GitHubResolutionCache
	cacheDir, cacheDirErr := GitHubResolutionCacheDir()
	if cacheDirErr != nil {
		// Print to stderr unconditionally: without a cache every request hits the
		// GitHub API fresh, directly contributing to rate-limit exhaustion.
		fmt.Fprintf(os.Stderr, "github: WARNING: failed to determine resolution cache dir: %v; proceeding without cache\n", cacheDirErr)
	} else {
		var cacheErr error
		cache, cacheErr = NewGitHubResolutionCache(cacheDir, DefaultResolutionCacheTTL)
		if cacheErr != nil {
			// Same rationale: operators need to see cache failures in production logs.
			fmt.Fprintf(os.Stderr, "github: WARNING: failed to initialize resolution cache at %s: %v (proceeding without cache)\n", cacheDir, cacheErr)
		}
	}
	return &GitHubSkillResolver{
		httpClient:      newGitHubHTTPClient(githubRequestTimeout),
		token:           os.Getenv("GITHUB_TOKEN"),
		apiBase:         githubAPIBase,
		rawBase:         githubRawBase,
		resolutionCache: cache,
	}
}

// NewGitHubSkillResolverWithCredentials constructs a resolver with an explicit
// default token and a named-credential map for per-URI lookup.
//
// Token resolution order for bare gh:// URIs (no ?token= suffix):
//  1. defaultToken (from req.ResolvedEnv["GITHUB_TOKEN"] — GitHub App or env-type secret).
//  2. GITHUB_TOKEN from the broker process environment (os.Getenv, set by NewGitHubSkillResolver).
//  3. GITHUB_TOKEN from provisionCredentials (project secret of any type).
//  4. No token — unauthenticated calls, subject to GitHub's 60 req/hr per-IP limit.
//
// provisionCredentials maps secret name → value; may be nil.
// If cache is non-nil, it is used as the singleton resolution cache (e.g., from
// the broker server struct) instead of the per-resolver cache created by
// NewGitHubSkillResolver. Pass nil to get the default per-resolver cache behavior.
func NewGitHubSkillResolverWithCredentials(defaultToken string, provisionCredentials map[string]string, cache *GitHubResolutionCache) *GitHubSkillResolver {
	r := NewGitHubSkillResolver()
	if defaultToken != "" {
		r.token = defaultToken
	} else if r.token == "" {
		// Neither an explicit token nor the broker-env GITHUB_TOKEN is available.
		// Fall back to a project-scoped provision credential named GITHUB_TOKEN.
		// This covers projects that store GITHUB_TOKEN as a secret but not as an
		// env-type secret (which would have been included in req.ResolvedEnv).
		if val := provisionCredentials["GITHUB_TOKEN"]; val != "" {
			r.token = val
		}
	}
	r.provisionCredentials = provisionCredentials
	// If a singleton cache is provided (e.g., from the broker server struct),
	// use it instead of the per-resolver cache created by NewGitHubSkillResolver.
	if cache != nil {
		r.resolutionCache = cache
	}
	return r
}

// normalizeGitHubName uppercases a GitHub name and replaces hyphens and dots
// with underscores to produce a valid env-var-style key segment.
func normalizeGitHubName(name string) string {
	s := strings.ToUpper(name)
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

// deriveGitHubTokenKey converts a GitHub owner/repo pair into the conventional
// project secret key name: GH_{OWNER}__{REPO}.
func deriveGitHubTokenKey(owner, repo string) string {
	return "GH_" + normalizeGitHubName(owner) + "__" + normalizeGitHubName(repo)
}

// deriveGitHubOwnerKey converts a GitHub owner into the owner-level
// fallback key: GH_{OWNER}.
func deriveGitHubOwnerKey(owner string) string {
	return "GH_" + normalizeGitHubName(owner)
}

// tokenForRef returns the appropriate GitHub token for the given ref.
//
// Resolution precedence:
//  1. Explicit ?token=SECRET_NAME on the URI — looked up in provisionCredentials.
//  2. Repo-specific convention key GH_{OWNER}__{REPO} from provisionCredentials.
//  3. Owner-level convention key GH_{OWNER} from provisionCredentials.
//  4. Default GITHUB_TOKEN cascade (r.token).
//  5. Empty string — unauthenticated; works for public repos.
//
// If ?token= is present but the named secret is missing, an error is returned.
// Missing convention keys are not errors — the resolver silently falls through.
func (r *GitHubSkillResolver) tokenForRef(ref *GitHubSkillRef) (string, error) {
	// Priority 1: Explicit ?token= override.
	if ref.TokenSecretName != "" {
		// In Go, reading from a nil map is safe and returns "". Both nil map and
		// missing/empty key produce the same error: the secret is unavailable.
		if val := r.provisionCredentials[ref.TokenSecretName]; val != "" {
			return val, nil
		}
		return "", fmt.Errorf("secret %q not found in ProvisionCredentials; ensure it is set at project scope", ref.TokenSecretName)
	}

	// Priority 2: Repo-specific convention key (GH_OWNER__REPO).
	repoKey := deriveGitHubTokenKey(ref.Owner, ref.Repo)
	if val := r.provisionCredentials[repoKey]; val != "" {
		util.Debugf("github: using credential %s for %s/%s", repoKey, ref.Owner, ref.Repo)
		return val, nil
	}

	// Priority 3: Owner-level convention key (GH_OWNER).
	ownerKey := deriveGitHubOwnerKey(ref.Owner)
	if val := r.provisionCredentials[ownerKey]; val != "" {
		util.Debugf("github: using credential %s for %s/%s", ownerKey, ref.Owner, ref.Repo)
		return val, nil
	}

	// Priority 4: Default GITHUB_TOKEN cascade.
	if r.token != "" {
		util.Debugf("github: no convention credential for %s/%s, using default", ref.Owner, ref.Repo)
		return r.token, nil
	}

	// Priority 5: No credential available — unauthenticated.
	fmt.Fprintf(os.Stderr, "github: WARNING: no credential available for %s/%s, attempting unauthenticated\n", ref.Owner, ref.Repo)
	return "", nil
}

func (r *GitHubSkillResolver) ResolverName() string { return "github" }

func (r *GitHubSkillResolver) Resolve(ctx context.Context, refs []api.SkillReference, opts ResolveOpts) (*ResolveResult, error) {
	// Impose our own budget when the caller gave none, so the fail-fast logic
	// in doWithRetry — which only ever fires when ctx.Deadline() reports a
	// deadline — actually runs in production (#2546 R1).
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, durationOr(r.resolveBudget, githubResolveBudget))
		defer cancel()
	}

	result := &ResolveResult{}

	for _, ref := range refs {
		ghRef, err := ParseGitHubSkillURI(ref.URI)
		if err != nil {
			result.Errors = append(result.Errors, ResolveError{
				URI: ref.URI, Code: "invalid_uri", Message: err.Error(),
			})
			continue
		}

		resolved, err := r.resolveOne(ctx, ghRef, ref)
		if err != nil {
			// Classify the failure into a stable cause code when possible
			// (set by doWithRetry/listContents/resolveCommitSHA below), so
			// the create path can map a required-skill failure to the right
			// status instead of a generic 500/502 (#2546).
			code := "resolve_failed"
			var retryAfter string
			var rerr *githubResolveError
			if errors.As(err, &rerr) {
				code = rerr.code
				retryAfter = rerr.retryAfter
			}
			result.Errors = append(result.Errors, ResolveError{
				URI: ref.URI, Code: code, Message: err.Error(), RetryAfter: retryAfter,
			})
			continue
		}
		result.Resolved = append(result.Resolved, *resolved)
	}

	return result, nil
}

// resolutionCacheKey returns a canonical cache key for a skill ref.
// A SHA-256 hash of the token is included so that different credentials
// produce separate cache entries, preventing cross-credential cache sharing
// of private content. The raw token is never used as a key value.
func resolutionCacheKey(ghRef *GitHubSkillRef, token string) string {
	var tokenSuffix string
	if token != "" {
		h := sha256.Sum256([]byte(token))
		tokenSuffix = "#" + hex.EncodeToString(h[:8]) // 16-char hex prefix of hash
	}
	return fmt.Sprintf("gh://%s/%s/%s@%s%s",
		ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, tokenSuffix)
}

func (r *GitHubSkillResolver) resolveOne(ctx context.Context, ghRef *GitHubSkillRef, ref api.SkillReference) (*ResolvedSkill, error) {
	// Resolve credential first — before cache check.
	// This ensures: (1) missing credentials fail immediately, (2) the token
	// hash is available for the cache key, isolating cache entries per credential.
	token, err := r.tokenForRef(ghRef)
	if err != nil {
		return nil, err
	}

	cacheKey := resolutionCacheKey(ghRef, token)
	if r.resolutionCache != nil {
		if cached, ok := r.resolutionCache.Get(cacheKey); ok {
			// No separate tokenForRef needed here — token already validated above.
			util.Debugf("github: resolution cache hit for %s", ref.URI)
			result := cached
			result.As = ref.As
			return &result, nil
		}
	}

	commitSHA, err := r.resolveCommitSHA(ctx, ghRef, token)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve ref for %s: %w", ghRef.Raw, err)
	}

	contents, err := r.listContents(ctx, ghRef, commitSHA, token)
	if err != nil {
		return nil, fmt.Errorf("failed to list skill contents for %s: %w", ghRef.Raw, err)
	}

	if len(contents) == 0 {
		return nil, fmt.Errorf("skill %q not found in repo %s/%s (empty directory at %s)",
			ghRef.SkillName, ghRef.Owner, ghRef.Repo, ghRef.SkillPath)
	}

	var resolvedFiles []ResolvedFile
	var fileInfos []transfer.FileInfo

	expectedPrefix := ghRef.SkillPath + "/"
	for _, entry := range contents {
		if entry.Type != "file" {
			continue
		}
		if !strings.HasPrefix(entry.Path, expectedPrefix) {
			continue
		}

		content, err := r.downloadRawFile(ctx, ghRef, commitSHA, entry.Path, token)
		if err != nil {
			return nil, fmt.Errorf("failed to download %s for skill %s: %w", entry.Path, ghRef.Raw, err)
		}

		hash := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
		relPath := strings.TrimPrefix(entry.Path, ghRef.SkillPath+"/")

		resolvedFiles = append(resolvedFiles, ResolvedFile{
			Path:    relPath,
			URL:     r.rawContentURL(ghRef, commitSHA, entry.Path),
			Hash:    hash,
			Size:    int64(len(content)),
			Content: content, // Carry bytes so install phase skips unauthenticated re-download.
		})
		fileInfos = append(fileInfos, transfer.FileInfo{Path: relPath, Hash: hash})
	}

	if len(resolvedFiles) == 0 {
		return nil, fmt.Errorf("skill %q in repo %s/%s contains no files",
			ghRef.SkillName, ghRef.Owner, ghRef.Repo)
	}

	bundleHash := transfer.ComputeContentHash(fileInfos)

	resolved := &ResolvedSkill{
		Name:     ghRef.SkillName,
		URI:      ghRef.Raw,
		As:       ref.As,
		Version:  commitSHA[:12],
		Hash:     bundleHash,
		Scope:    ref.Scope,
		Files:    resolvedFiles,
		Optional: ref.Optional,
	}

	// Store in resolution cache under the token-hashed key.
	if r.resolutionCache != nil {
		r.resolutionCache.Put(cacheKey, *resolved)
	}

	return resolved, nil
}

// githubContentEntry is the JSON structure returned by the GitHub Contents API.
type githubContentEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	Size        int    `json:"size"`
	DownloadURL string `json:"download_url"`
}

// isFullCommitSHA reports whether s is a complete 40-character lowercase
// hexadecimal commit SHA. Such a ref is already fully resolved and requires
// no GitHub API call to "resolve" it further.
func isFullCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (r *GitHubSkillResolver) resolveCommitSHA(ctx context.Context, ghRef *GitHubSkillRef, token string) (string, error) {
	ref := ghRef.Ref
	if ref == "" {
		ref = "HEAD"
	}

	// Warn before any API call path — including listContents and downloadRawFile
	// called by the parent resolveOne after this function returns. Even when the
	// full-SHA short-circuit below skips the SHA-lookup call, those subsequent
	// calls still go out unauthenticated; the operator needs advance notice.
	// Unauthenticated GitHub API calls are limited to 60/hr per outbound IP
	// (shared across all broker instances on Cloud Run / Cloud NAT).
	// To fix: set GITHUB_TOKEN in the project secrets or the broker's environment.
	if token == "" {
		fmt.Fprintf(os.Stderr, "github: WARNING: no GITHUB_TOKEN configured for %s; "+
			"making unauthenticated GitHub API call (limit: 60 req/hr per IP). "+
			"Set a GITHUB_TOKEN project secret or broker env var to increase the limit.\n", ghRef.Raw)
	}

	// Short-circuit: if the ref is already a full 40-char lowercase hex commit SHA,
	// no API call is needed — the ref IS the resolved SHA.
	if isFullCommitSHA(ref) {
		util.Debugf("github: ref %s is already a full SHA, skipping resolveCommitSHA API call", ref)
		return ref, nil
	}

	reqURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s", r.apiBase,
		url.PathEscape(ghRef.Owner), url.PathEscape(ghRef.Repo), url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3.sha")
	r.setAuthHeader(req, token)

	resp, err := r.doWithRetry(ctx, req, durationOr(r.requestTimeout, githubRequestTimeout))
	if err != nil {
		return "", fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", &githubResolveError{
			code: SkillErrCodeNotFound,
			msg:  fmt.Sprintf("ref %q not found in repo %s/%s", ghRef.Ref, ghRef.Owner, ghRef.Repo),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", r.apiError(resp, "resolve commit")
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", fmt.Errorf("failed to read commit SHA: %w", err)
	}
	sha := strings.TrimSpace(string(body))
	if len(sha) != 40 {
		return "", fmt.Errorf("unexpected commit SHA format: %q", sha)
	}
	return sha, nil
}

func (r *GitHubSkillResolver) listContents(ctx context.Context, ghRef *GitHubSkillRef, commitSHA string, token string) ([]githubContentEntry, error) {
	escapedPath := escapePathSegments(ghRef.SkillPath)
	reqURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		r.apiBase, url.PathEscape(ghRef.Owner), url.PathEscape(ghRef.Repo), escapedPath, url.QueryEscape(commitSHA))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	r.setAuthHeader(req, token)

	resp, err := r.doWithRetry(ctx, req, durationOr(r.requestTimeout, githubRequestTimeout))
	if err != nil {
		return nil, fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, &githubResolveError{
			code: SkillErrCodeNotFound,
			msg: fmt.Sprintf("skill %q not found in repo %s/%s at ref %s (expected directory at %s)",
				ghRef.SkillName, ghRef.Owner, ghRef.Repo, commitSHA[:12], ghRef.SkillPath),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, r.apiError(resp, "list contents")
	}

	var entries []githubContentEntry
	limited := io.LimitReader(resp.Body, 5*1024*1024)
	if err := json.NewDecoder(limited).Decode(&entries); err != nil {
		return nil, fmt.Errorf("failed to decode GitHub API response: %w", err)
	}
	return entries, nil
}

func (r *GitHubSkillResolver) downloadRawFile(ctx context.Context, ghRef *GitHubSkillRef, commitSHA, filePath string, token string) ([]byte, error) {
	reqURL := r.rawContentURL(ghRef, commitSHA, filePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	r.setAuthHeader(req, token)

	resp, err := r.doWithRetry(ctx, req, durationOr(r.downloadTimeout, githubDownloadRequestTimeout))
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, &githubResolveError{
			code: SkillErrCodeNotFound,
			msg:  fmt.Sprintf("file %s not found in repo %s/%s at %s", filePath, ghRef.Owner, ghRef.Repo, commitSHA[:12]),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, r.apiError(resp, fmt.Sprintf("downloading %s", filePath))
	}

	content, err := io.ReadAll(io.LimitReader(resp.Body, int64(githubMaxFileSize)+1))
	if err != nil {
		// A transfer cut off by the per-attempt or resolve-budget deadline is
		// a timeout, so classify it as such (mapping to 504/408) rather than
		// leaving it as an unclassified resolve_failed; other read errors
		// stay unclassified (#2546 O1).
		if classifyNetworkError(err) == SkillErrCodeTimeout {
			return nil, &githubResolveError{
				code: SkillErrCodeTimeout,
				msg:  fmt.Sprintf("failed to read %s: %v", filePath, err),
			}
		}
		return nil, fmt.Errorf("failed to read file content: %w", err)
	}
	if int64(len(content)) > int64(githubMaxFileSize) {
		return nil, fmt.Errorf("file %s exceeds maximum size of %d bytes", filePath, githubMaxFileSize)
	}
	return content, nil
}

func (r *GitHubSkillResolver) rawContentURL(ghRef *GitHubSkillRef, commitSHA, filePath string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		r.rawBase, ghRef.Owner, ghRef.Repo, commitSHA, escapePathSegments(filePath))
}

func escapePathSegments(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

func (r *GitHubSkillResolver) setAuthHeader(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// githubResolveError is a classified GitHub resolution failure, carrying a
// stable cause code (see the SkillErrCode* constants in skill_resolver.go) so
// Resolve can set ResolveError.Code — and provision.go's SkillResolutionError
// can in turn pick an HTTP status — without string-matching msg. retryAfter
// carries the raw Retry-After header value when the cause is
// SkillErrCodeRateLimited and the server sent one; empty otherwise.
type githubResolveError struct {
	code       string
	msg        string
	retryAfter string
}

func (e *githubResolveError) Error() string { return e.msg }

// classifyRetryCause maps the response (and, when resp is nil, the
// network-level error) that triggered a retry or a fail-fast to a stable
// cause code:
//   - 429, or 403 with rate-limit exhaustion: rate_limited.
//   - anything else (in practice a 5xx): upstream_unavailable — GitHub
//     itself is failing, not the caller (#2546 R3).
//   - no response at all: delegated to classifyNetworkError, which tells a
//     genuine deadline apart from a DNS/connection/TLS failure.
func classifyRetryCause(resp *http.Response, err error) string {
	if resp == nil {
		return classifyNetworkError(err)
	}
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0") {
		return SkillErrCodeRateLimited
	}
	// Defensive default: isRetryableResponse only admits the two cases
	// above, so no other status reaches here today.
	return SkillErrCodeUpstreamUnavailable
}

// classifyNetworkError distinguishes "no response arrived in time" — the
// overall ctx deadline expiring, or the httpClient's own ResponseHeaderTimeout
// firing on a stalled raw download (#2546 O2) — from every other
// network-level failure: DNS resolution, connection refused, TLS handshake
// errors. Only the latter group gets the unreachable cause; a timeout is kept
// distinct rather than folded into a blanket "unreachable" (#2546 N1). A nil
// err (not expected in practice; doWithRetry only reaches this classification
// when an attempt actually failed) is treated as a deadline for safety.
func classifyNetworkError(err error) string {
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		return SkillErrCodeTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return SkillErrCodeTimeout
	}
	return SkillErrCodeUnreachable
}

// retryAfterDuration parses resp's Retry-After header (integer seconds, as
// GitHub sends it) without capping it at githubMaxBackoff, so callers can
// tell a merely-long backoff apart from one longer than we are ever willing
// to wait (#2546 O3). ok is false when resp is nil or carries no parseable
// Retry-After.
func retryAfterDuration(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 0, false
	}
	seconds, err := strconv.Atoi(ra)
	if err != nil || seconds < 0 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// cancelOnCloseBody ties a per-attempt context's cancel func to the lifetime
// of the response body it guards: doOnce's bounded timeout must stay alive
// while the body is being read, but must not leak once the caller is done
// with it. Closing the body is the caller's existing signal for "done
// reading" (every call site already defers resp.Body.Close()), so piggy-
// backing cancellation on it needs no new caller-visible API.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// doOnce performs a single HTTP attempt bounded by attemptTimeout (see
// githubRequestTimeout and githubDownloadRequestTimeout). The timeout is
// applied via the request context rather than relying solely on
// r.httpClient's own Timeout, so it composes with (and is independent of) the
// caller's ctx deadline: whichever is shorter wins.
func (r *GitHubSkillResolver) doOnce(ctx context.Context, req *http.Request, attemptTimeout time.Duration) (*http.Response, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	cloned := req.Clone(attemptCtx)
	resp, err := r.httpClient.Do(cloned)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// doWithRetry executes an HTTP request with retry and exponential backoff
// for rate-limited (403 with X-RateLimit-Remaining: 0, 429) and transient
// server errors (5xx). On retryable responses it respects the Retry-After
// header when present.
//
// Each attempt is bounded by attemptTimeout (see doOnce), so a single stalled
// request cannot by itself consume the whole create-deadline budget. Before
// sleeping for a backoff, doWithRetry also checks whether the delay still
// fits inside ctx's remaining deadline; if it doesn't, it fails immediately
// with a classified, ref-naming error instead of sleeping into a context
// cancellation that would otherwise surface only as an opaque "context
// canceled" (#2546). Separately, when the server's own Retry-After exceeds
// githubMaxBackoff, doWithRetry fails fast rather than sleeping the capped
// backoff and retrying straight into another rate limit (#2546 O3).
func (r *GitHubSkillResolver) doWithRetry(ctx context.Context, req *http.Request, attemptTimeout time.Duration) (*http.Response, error) {
	var lastResp *http.Response
	var lastErr error
	noun := "GitHub API request to"
	if r.rawBase != "" && strings.HasPrefix(req.URL.String(), r.rawBase) {
		noun = "GitHub raw download of"
	}

	for attempt := 0; attempt <= githubMaxRetries; attempt++ {
		if attempt > 0 {
			status := -1
			retryAfterHeader := ""
			if lastResp != nil {
				status = lastResp.StatusCode
				retryAfterHeader = lastResp.Header.Get("Retry-After")
			}

			// The server has said explicitly when to come back; if that is
			// further out than our backoff cap, retrying now (capped at
			// githubMaxBackoff) would just run into another rate limit.
			if ra, ok := retryAfterDuration(lastResp); ok && ra > githubMaxBackoff {
				slog.Warn("github: retry-after exceeds backoff cap, failing fast",
					"method", req.Method, "path", req.URL.Path,
					"status", status, "retry_after", retryAfterHeader, "cap", githubMaxBackoff)
				return nil, &githubResolveError{
					code:       classifyRetryCause(lastResp, nil),
					retryAfter: retryAfterHeader,
					msg: fmt.Sprintf(
						"%s %s asked to retry after %s, past the %s backoff cap: "+
							"failing fast instead of retrying into another rate limit",
						noun, req.URL.Path, ra, githubMaxBackoff),
				}
			}

			delay := retryDelay(lastResp, attempt)

			// Fail fast when the backoff, plus the stall bound a further
			// attempt needs to show it is alive, would run past ctx's
			// deadline, rather than sleeping most or all of it away only to
			// have the next request canceled. Reserving the stall bound, not
			// the full attempt timeout, keeps raw downloads retryable: their
			// attempt timeout equals the default budget, and a transfer still
			// in progress is bounded by ctx itself (#2546 RQ1).
			if dl, ok := ctx.Deadline(); ok {
				stall := r.stallBound()
				if remaining := time.Until(dl); delay+stall >= remaining {
					slog.Warn("github: skill resolution out of budget before backoff, failing fast",
						"method", req.Method, "path", req.URL.Path,
						"status", status, "retry_after", retryAfterHeader,
						"backoff", delay, "remaining_budget", remaining)
					return nil, &githubResolveError{
						code:       classifyRetryCause(lastResp, lastErr),
						retryAfter: retryAfterHeader,
						msg:        budgetExceededMessage(noun, req.URL.Path, status, retryAfterHeader, delay, remaining),
					}
				}
			}

			slog.Warn("github: retrying request after backoff",
				"kind", noun, "method", req.Method, "path", req.URL.Path,
				"status", status, "retry_after", retryAfterHeader,
				"backoff", delay, "attempt", attempt, "max_attempts", githubMaxRetries)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		resp, err := r.doOnce(ctx, req, attemptTimeout)
		if err != nil {
			lastErr = err
			lastResp = nil
			continue
		}

		if !isRetryableResponse(resp) {
			return resp, nil
		}

		if attempt == githubMaxRetries {
			return resp, nil
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		lastResp = resp
		lastErr = nil
	}

	// The only way to reach here is attempt == githubMaxRetries having just
	// `continue`d past a doOnce network-level error, which always sets
	// lastErr — classify it (DNS/connection/TLS vs. a genuine deadline) so
	// retries-exhausted network failures are as actionable as any other cause
	// (#2546 N1).
	return nil, &githubResolveError{code: classifyNetworkError(lastErr), msg: lastErr.Error()}
}

// budgetExceededMessage builds the error text for doWithRetry's ctx-budget
// fail-fast path. status is -1 when the previous attempt failed at the
// network level (no response at all) rather than returning an HTTP status, in
// which case retryAfter is also always empty; both render as "no response"
// rather than the misleading "status -1, retry-after " (#2546 N3). The
// deadline is described as the "request" deadline, not the "create" deadline,
// since the resolver also runs outside of create — on start, restart and
// reprovision (#2546 N2). noun names the path kind ("GitHub API request to"
// or "GitHub raw download of"), and a status is reported as a plain failure,
// not as rate limiting, since 5xx responses take this path too.
func budgetExceededMessage(noun, path string, status int, retryAfter string, delay, remaining time.Duration) string {
	outcome := fmt.Sprintf("failed (status %d)", status)
	if retryAfter != "" {
		outcome = fmt.Sprintf("failed (status %d, retry-after %s)", status, retryAfter)
	}
	if status == -1 {
		outcome = "got no response"
	}
	return fmt.Sprintf(
		"%s %s %s: next backoff %s would exceed the %s left before the request deadline",
		noun, path, outcome, delay, remaining)
}

// isRetryableResponse returns true for HTTP responses that should be retried:
// 429 (Too Many Requests), 403 with rate-limit exhaustion, and 5xx server errors.
func isRetryableResponse(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return true
	}
	if resp.StatusCode >= 500 {
		return true
	}
	return false
}

// retryDelay calculates the backoff duration for a retry attempt.
// Uses the Retry-After header when present, otherwise exponential backoff.
func retryDelay(resp *http.Response, attempt int) time.Duration {
	if resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if seconds, err := strconv.Atoi(ra); err == nil && seconds >= 0 {
				d := time.Duration(seconds) * time.Second
				if d > githubMaxBackoff {
					d = githubMaxBackoff
				}
				return d
			}
		}
		// For rate limits, check X-RateLimit-Reset (Unix timestamp)
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			if resetStr := resp.Header.Get("X-RateLimit-Reset"); resetStr != "" {
				if resetUnix, err := strconv.ParseInt(resetStr, 10, 64); err == nil {
					wait := time.Until(time.Unix(resetUnix, 0))
					if wait > 0 {
						if wait > githubMaxBackoff {
							return githubMaxBackoff
						}
						return wait
					}
				}
			}
		}
	}
	backoff := time.Duration(float64(githubBaseBackoff) * math.Pow(githubBackoffFactor, float64(attempt-1)))
	if backoff > githubMaxBackoff {
		backoff = githubMaxBackoff
	}
	return backoff
}

// apiError builds the error for a terminal non-OK response: either the last
// of githubMaxRetries retryable responses, or a non-retryable error status.
// Rate-limit responses (429, or 403 with X-RateLimit-Remaining: 0) and
// retries-exhausted 5xx responses are classified via githubResolveError so
// callers can map them to the right status without string-matching; every
// other status (401, 422, ...) keeps the existing uncategorized error, which
// resolves to the same 5xx path it always has (#2546 R3).
func (r *GitHubSkillResolver) apiError(resp *http.Response, action string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0") {
		return &githubResolveError{
			code:       SkillErrCodeRateLimited,
			retryAfter: resp.Header.Get("Retry-After"),
			msg: fmt.Sprintf("GitHub API rate limited while %s (retry-after=%s, resets at %s); set GITHUB_TOKEN for higher limits",
				action, resp.Header.Get("Retry-After"), resp.Header.Get("X-RateLimit-Reset")),
		}
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return &githubResolveError{
			code: SkillErrCodeUpstreamUnavailable,
			msg:  fmt.Sprintf("GitHub API error (%d) while %s, retries exhausted: %s", resp.StatusCode, action, string(body)),
		}
	}
	return fmt.Errorf("GitHub API error (%d) while %s: %s", resp.StatusCode, action, string(body))
}
