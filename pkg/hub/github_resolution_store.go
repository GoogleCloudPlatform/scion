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

package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/githubresolutioncache"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/schema"
)

// GitHubFileEntry represents a single file in a GitHub skill resolution result.
// This is a value type (not an ent entity) stored as JSON.
type GitHubFileEntry = schema.GitHubFileEntry

// GitHubCacheEntry represents a cached GitHub skill resolution result.
type GitHubCacheEntry struct {
	CommitSHA   string
	FileEntries []GitHubFileEntry
	BundleHash  string
	TokenScope  string
	ExpiresAt   time.Time
	OriginalURI string
}

// GitHubResolutionStore wraps the ent client for GitHub skill resolution cache operations.
type GitHubResolutionStore struct {
	client *ent.Client
}

// NewGitHubResolutionStore creates a new GitHubResolutionStore.
func NewGitHubResolutionStore(client *ent.Client) *GitHubResolutionStore {
	return &GitHubResolutionStore{client: client}
}

// Get retrieves a cache entry by key, returning (entry, true, nil) on hit,
// (nil, false, nil) on miss or expiration, or (nil, false, error) on DB error.
func (s *GitHubResolutionStore) Get(ctx context.Context, cacheKey string) (*GitHubCacheEntry, bool, error) {
	row, err := s.client.GitHubResolutionCache.
		Query().
		Where(githubresolutioncache.CacheKeyEQ(cacheKey)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	// Check expiration
	if time.Now().After(row.ExpiresAt) {
		return nil, false, nil
	}

	return &GitHubCacheEntry{
		CommitSHA:   row.CommitSha,
		FileEntries: row.FileEntries,
		BundleHash:  row.BundleHash,
		TokenScope:  row.TokenScope,
		ExpiresAt:   row.ExpiresAt,
		OriginalURI: row.OriginalURI,
	}, true, nil
}

// Put upserts a cache entry. If an entry with the same cache_key exists, it is updated.
func (s *GitHubResolutionStore) Put(ctx context.Context, cacheKey string, entry GitHubCacheEntry) error {
	return s.client.GitHubResolutionCache.
		Create().
		SetCacheKey(cacheKey).
		SetOriginalURI(entry.OriginalURI).
		SetCommitSha(entry.CommitSHA).
		SetFileEntries(entry.FileEntries).
		SetBundleHash(entry.BundleHash).
		SetTokenScope(entry.TokenScope).
		SetExpiresAt(entry.ExpiresAt).
		OnConflict().
		UpdateNewValues().
		Exec(ctx)
}

// PurgeExpired deletes all cache entries where expires_at < now.
func (s *GitHubResolutionStore) PurgeExpired(ctx context.Context) error {
	_, err := s.client.GitHubResolutionCache.
		Delete().
		Where(githubresolutioncache.ExpiresAtLT(time.Now())).
		Exec(ctx)
	return err
}

// computeCacheKey computes a deterministic cache key from the normalized URI and token scope.
// Returns sha256(normalized_uri + ":" + token_scope_id).
func computeCacheKey(owner, repo, skillPath, ref, tokenScope string) string {
	// Normalize: lowercase owner/repo, consistent format
	normalized := fmt.Sprintf("gh://%s/%s/%s@%s",
		strings.ToLower(owner),
		strings.ToLower(repo),
		skillPath,
		ref,
	)
	h := sha256.Sum256([]byte(normalized + ":" + tokenScope))
	return hex.EncodeToString(h[:])
}

// ghResolveCommitSHA resolves a GitHub ref (branch, tag, or SHA) to a full 40-char commit SHA.
// If the ref is already a full 40-char lowercase hex SHA, it is returned as-is.
// Otherwise, calls GET /repos/{owner}/{repo}/commits/{ref} with Accept: application/vnd.github.v3.sha.
func ghResolveCommitSHA(ctx context.Context, apiBase, owner, repo, ref, token string) (string, error) {
	// Short-circuit if ref is already a full 40-char SHA
	if isFullCommitSHA(ref) {
		return ref, nil
	}

	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s", apiBase, owner, repo, ref)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Accept", "application/vnd.github.v3.sha")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to resolve commit SHA for %s@%s: %w", repo, ref, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GitHub API error %d resolving %s@%s: %s", resp.StatusCode, repo, ref, string(body))
	}

	shaBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read commit SHA response: %w", err)
	}

	sha := strings.TrimSpace(string(shaBytes))
	if !isFullCommitSHA(sha) {
		return "", fmt.Errorf("GitHub returned invalid SHA %q for %s@%s", sha, repo, ref)
	}

	return sha, nil
}

// ghListContents calls GET /repos/{owner}/{repo}/contents/{path}?ref={sha}
// and returns a list of file entries with their raw CDN download URLs and git blob SHAs.
func ghListContents(ctx context.Context, apiBase, owner, repo, path, commitSHA, token string) ([]GitHubFileEntry, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", apiBase, owner, repo, path, commitSHA)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to list contents for %s/%s at %s: %w", owner, repo, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API error %d listing %s/%s at %s: %s", resp.StatusCode, owner, repo, path, string(body))
	}

	var apiResponse []struct {
		Name        string `json:"name"`
		Path        string `json:"path"`
		SHA         string `json:"sha"` // Git blob SHA
		Size        int64  `json:"size"`
		Type        string `json:"type"`
		DownloadURL string `json:"download_url"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&apiResponse); err != nil {
		return nil, fmt.Errorf("failed to decode contents response: %w", err)
	}

	var entries []GitHubFileEntry
	for _, item := range apiResponse {
		if item.Type != "file" {
			continue // Skip directories
		}
		// Use relative path within the skill directory
		relPath := strings.TrimPrefix(item.Path, path+"/")
		if relPath == item.Path && !strings.HasPrefix(path, item.Path) {
			relPath = item.Name
		}
		entries = append(entries, GitHubFileEntry{
			Path: relPath,
			URL:  item.DownloadURL,
			Hash: item.SHA, // Git blob SHA for content addressing
			Size: item.Size,
		})
	}

	return entries, nil
}

// isFullCommitSHA reports whether s is a complete 40-character lowercase hexadecimal SHA.
var fullSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func isFullCommitSHA(s string) bool {
	return len(s) == 40 && fullSHAPattern.MatchString(s)
}
