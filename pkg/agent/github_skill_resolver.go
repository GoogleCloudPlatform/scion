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
	httpClient      *http.Client
	token           string // GITHUB_TOKEN for authenticated requests
	apiBase         string // Default: githubAPIBase, override in tests
	rawBase         string // Default: githubRawBase, override in tests
	resolutionCache *GitHubResolutionCache
}

// NewGitHubSkillResolver creates a resolver for gh:// and GitHub URL skills.
// Reads GITHUB_TOKEN from environment for authenticated API access.
// If a resolution cache directory is available, cached resolution results
// are reused to avoid redundant GitHub API calls.
func NewGitHubSkillResolver() *GitHubSkillResolver {
	var cache *GitHubResolutionCache
	if cacheDir, err := githubResolutionCacheDir(); err == nil {
		cache, _ = NewGitHubResolutionCache(cacheDir, DefaultResolutionCacheTTL)
	}
	return &GitHubSkillResolver{
		httpClient:      &http.Client{Timeout: githubAPITimeout},
		token:           os.Getenv("GITHUB_TOKEN"),
		apiBase:         githubAPIBase,
		rawBase:         githubRawBase,
		resolutionCache: cache,
	}
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

func (r *GitHubSkillResolver) resolveOne(ctx context.Context, ghRef *GitHubSkillRef, ref api.SkillReference) (*ResolvedSkill, error) {
	// Check resolution cache first
	if r.resolutionCache != nil {
		if cached, ok := r.resolutionCache.Get(ref.URI); ok {
			util.Debugf("github: resolution cache hit for %s", ref.URI)
			result := cached
			result.As = ref.As
			return &result, nil
		}
	}

	commitSHA, err := r.resolveCommitSHA(ctx, ghRef)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve ref for %s: %w", ghRef.Raw, err)
	}

	contents, err := r.listContents(ctx, ghRef, commitSHA)
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

		content, err := r.downloadRawFile(ctx, ghRef, commitSHA, entry.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to download %s: %w", entry.Path, err)
		}

		hash := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
		relPath := strings.TrimPrefix(entry.Path, ghRef.SkillPath+"/")

		resolvedFiles = append(resolvedFiles, ResolvedFile{
			Path: relPath,
			URL:  r.rawContentURL(ghRef, commitSHA, entry.Path),
			Hash: hash,
			Size: int64(len(content)),
		})
		fileInfos = append(fileInfos, transfer.FileInfo{Path: relPath, Hash: hash})
	}

	if len(resolvedFiles) == 0 {
		return nil, fmt.Errorf("skill %q in repo %s/%s contains no files",
			ghRef.SkillName, ghRef.Owner, ghRef.Repo)
	}

	bundleHash := transfer.ComputeContentHash(fileInfos)

	resolved := &ResolvedSkill{
		Name:    ghRef.SkillName,
		URI:     ghRef.Raw,
		As:      ref.As,
		Version: commitSHA[:12],
		Hash:    bundleHash,
		Files:   resolvedFiles,
	}

	// Store in resolution cache
	if r.resolutionCache != nil {
		r.resolutionCache.Put(ref.URI, *resolved)
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

func (r *GitHubSkillResolver) resolveCommitSHA(ctx context.Context, ghRef *GitHubSkillRef) (string, error) {
	ref := ghRef.Ref
	if ref == "" {
		ref = "HEAD"
	}

	reqURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s", r.apiBase,
		url.PathEscape(ghRef.Owner), url.PathEscape(ghRef.Repo), url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3.sha")
	r.setAuthHeader(req)

	resp, err := r.doWithRetry(ctx, req)
	if err != nil {
		return "", fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()

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

func (r *GitHubSkillResolver) listContents(ctx context.Context, ghRef *GitHubSkillRef, commitSHA string) ([]githubContentEntry, error) {
	escapedPath := escapePathSegments(ghRef.SkillPath)
	reqURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		r.apiBase, url.PathEscape(ghRef.Owner), url.PathEscape(ghRef.Repo), escapedPath, url.QueryEscape(commitSHA))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	r.setAuthHeader(req)

	resp, err := r.doWithRetry(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()

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

func (r *GitHubSkillResolver) downloadRawFile(ctx context.Context, ghRef *GitHubSkillRef, commitSHA, filePath string) ([]byte, error) {
	reqURL := r.rawContentURL(ghRef, commitSHA, filePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	r.setAuthHeader(req)

	resp, err := r.doWithRetry(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

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

func (r *GitHubSkillResolver) setAuthHeader(req *http.Request) {
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
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
