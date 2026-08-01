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
	"fmt"
	"io"
	"math"
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
		httpClient:      &http.Client{Timeout: githubAPITimeout},
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
			result.Errors = append(result.Errors, ResolveError{
				URI: ref.URI, Code: "resolve_failed", Message: err.Error(),
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
		return nil, err
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
			return nil, fmt.Errorf("failed to download %s: %w", entry.Path, err)
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

	resp, err := r.doWithRetry(ctx, req)
	if err != nil {
		return "", fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("ref %q not found in repo %s/%s", ghRef.Ref, ghRef.Owner, ghRef.Repo)
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

	resp, err := r.doWithRetry(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("skill %q not found in repo %s/%s at ref %s (expected directory at %s)",
			ghRef.SkillName, ghRef.Owner, ghRef.Repo, commitSHA[:12], ghRef.SkillPath)
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

	resp, err := r.doWithRetry(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed with status %d for %s", resp.StatusCode, filePath)
	}

	content, err := io.ReadAll(io.LimitReader(resp.Body, int64(githubMaxFileSize)+1))
	if err != nil {
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

// doWithRetry executes an HTTP request with retry and exponential backoff
// for rate-limited (403 with X-RateLimit-Remaining: 0, 429) and transient
// server errors (5xx). On retryable responses it respects the Retry-After
// header when present.
func (r *GitHubSkillResolver) doWithRetry(ctx context.Context, req *http.Request) (*http.Response, error) {
	var lastResp *http.Response
	var lastErr error

	for attempt := 0; attempt <= githubMaxRetries; attempt++ {
		if attempt > 0 {
			delay := retryDelay(lastResp, attempt)
			util.Debugf("github: retrying request (attempt %d/%d) after %v: %s %s",
				attempt, githubMaxRetries, delay, req.Method, req.URL.Path)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		cloned := req.Clone(ctx)
		resp, err := r.httpClient.Do(cloned)
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

	return nil, lastErr
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

func (r *GitHubSkillResolver) apiError(resp *http.Response, action string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return fmt.Errorf("GitHub API rate limit exceeded while %s (resets at %s); set GITHUB_TOKEN for higher limits",
			action, resp.Header.Get("X-RateLimit-Reset"))
	}
	return fmt.Errorf("GitHub API error (%d) while %s: %s", resp.StatusCode, action, string(body))
}
