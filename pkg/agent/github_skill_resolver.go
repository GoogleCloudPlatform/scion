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
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

const (
	githubAPIBase     = "https://api.github.com"
	githubRawBase     = "https://raw.githubusercontent.com"
	githubAPITimeout  = 30 * time.Second
	githubMaxFileSize = 10 * 1024 * 1024 // 10MB per file
)

// GitHubSkillResolver resolves skills from GitHub repositories
// using the GitHub Contents API.
type GitHubSkillResolver struct {
	httpClient *http.Client
	token      string // GITHUB_TOKEN for authenticated requests
	apiBase    string // Default: githubAPIBase, override in tests
	rawBase    string // Default: githubRawBase, override in tests
}

// NewGitHubSkillResolver creates a resolver for gh:// and GitHub URL skills.
// Reads GITHUB_TOKEN from environment for authenticated API access.
func NewGitHubSkillResolver() *GitHubSkillResolver {
	return &GitHubSkillResolver{
		httpClient: &http.Client{Timeout: githubAPITimeout},
		token:      os.Getenv("GITHUB_TOKEN"),
		apiBase:    githubAPIBase,
		rawBase:    githubRawBase,
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

	return &ResolvedSkill{
		Name:    ghRef.SkillName,
		URI:     ghRef.Raw,
		As:      ref.As,
		Version: commitSHA[:12],
		Hash:    bundleHash,
		Files:   resolvedFiles,
	}, nil
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

	resp, err := r.httpClient.Do(req)
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

	resp, err := r.httpClient.Do(req)
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

	resp, err := r.httpClient.Do(req)
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

func (r *GitHubSkillResolver) apiError(resp *http.Response, action string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return fmt.Errorf("GitHub API rate limit exceeded while %s (resets at %s); set GITHUB_TOKEN for higher limits",
			action, resp.Header.Get("X-RateLimit-Reset"))
	}
	return fmt.Errorf("GitHub API error (%d) while %s: %s", resp.StatusCode, action, string(body))
}
