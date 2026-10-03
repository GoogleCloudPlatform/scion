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
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
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

// GetStale returns a cache entry for cacheKey even though its TTL has
// expired, provided it is within maxStaleAge of its last successful
// resolution. lastTTL is the TTL that was used to compute the row's
// ExpiresAt when it was last written — the schema has no separate
// "last resolved at" column, so the last resolution time is derived as
// ExpiresAt - lastTTL instead. Callers must only use this for branch refs
// (a known, fixed TTL); a commit-SHA entry's TTL differs and, being
// immutable, has no use for staleness in the first place.
//
// Returns (nil, false, nil) when the row does not exist or is older than
// maxStaleAge, and (nil, false, error) on a DB error.
func (s *GitHubResolutionStore) GetStale(ctx context.Context, cacheKey string, lastTTL, maxStaleAge time.Duration) (*GitHubCacheEntry, bool, error) {
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

	if !row.ExpiresAt.After(staleCutoff(time.Now(), lastTTL, maxStaleAge)) {
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
//
// OnConflictColumns names cache_key (the table's unique index, see the
// GitHubResolutionCache schema) as the conflict target explicitly. Without
// it, ent emits "INSERT ... ON CONFLICT DO UPDATE SET ..." with no inference
// specification: Postgres rejects that unconditionally ("ON CONFLICT DO
// UPDATE requires inference specification or constraint name"), so every
// write failed there and the cache was never populated. SQLite 3.35+ accepts
// the same statement by inferring the lone eligible unique index, which is
// why this went unnoticed in SQLite-only tests.
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
		OnConflictColumns(githubresolutioncache.FieldCacheKey).
		UpdateNewValues().
		Exec(ctx)
}

// staleCutoff returns the ExpiresAt threshold at or below which a row can no
// longer be served stale by GetStale(ctx, cacheKey, lastTTL, maxStaleAge): a
// row's last successful resolution time is ExpiresAt - lastTTL (see
// GetStale's own comment), and GetStale keeps serving it stale while now is
// before lastResolvedAt + maxStaleAge — equivalently, while ExpiresAt is
// after this cutoff. GetStale and PurgeExpired both call this so the two
// never disagree about where that line is.
//
// Uses agent.MaxJitteredTTL(lastTTL), not lastTTL itself: ExpiresAt was
// written as time.Now().Add(agent.JitteredTTL(ttl, ...)) (see
// fetchAndCacheGitHubSkill), so the true lastTTL any given row was written
// with is no longer recoverable from the stored ExpiresAt alone. Using the
// jitter's upper bound derives a lastResolvedAt that is never later than the
// row's true one, so a row is never treated as fresher — and so never kept
// stale-servable longer — than its actual age; the cost is that, in the
// worst case (a row whose jitter pushed it to the fast/short end), it stops
// being stale-servable up to the jitter amount before the nominal
// maxStaleAge boundary, never after it.
func staleCutoff(now time.Time, lastTTL, maxStaleAge time.Duration) time.Time {
	return now.Add(agent.MaxJitteredTTL(lastTTL) - maxStaleAge)
}

// PurgeExpired deletes cache entries that can no longer be served stale even
// under a branch ref's own (longer) retention — agent.DefaultResolutionCacheTTL
// and agent.MaxResolutionStaleAge, the only values resolveGitHubSkill ever
// passes to GetStale — using the same staleCutoff GetStale itself checks
// against, so a row GetStale could still have served is never purged out from
// under it. The schema has no column recording whether a row is a branch or a
// commit-SHA ref, so this one cutoff is applied to every row: a SHA-ref row
// (whose own TTL is unrelated to staleness, since GetStale is never consulted
// for one) may then survive somewhat longer than its own TTL before purge,
// which is harmless — its content is immutable, so an unnecessarily long wait
// before deletion costs only a little extra storage, never a wrong answer.
func (s *GitHubResolutionStore) PurgeExpired(ctx context.Context) error {
	cutoff := staleCutoff(time.Now(), agent.DefaultResolutionCacheTTL, agent.MaxResolutionStaleAge)
	_, err := s.client.GitHubResolutionCache.
		Delete().
		Where(githubresolutioncache.ExpiresAtLTE(cutoff)).
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
//
// The request goes through cooldown for identity (see agent.GitHubCooldown):
// while identity is rate limited no request is sent, and a rate-limit
// response starts a cooldown; both come back as *agent.GitHubRateLimitError.
func ghResolveCommitSHA(ctx context.Context, cooldown *agent.GitHubCooldown, identity, apiBase, owner, repo, ref, token string) (string, error) {
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
	resp, err := cooldown.Do(client, req, identity)
	if err != nil {
		return "", fmt.Errorf("failed to resolve commit SHA for %s@%s: %w", repo, ref, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		return "", &ghStatusError{status: resp.StatusCode, msg: fmt.Sprintf("GitHub API error %d resolving %s@%s: %s", resp.StatusCode, repo, ref, string(body))}
	}

	shaBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
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
// and returns a list of file entries with permanent raw content URLs and git
// blob SHAs.
//
// rawBase is the origin for raw content URLs (githubRawBase in production;
// overridden by tests). It is deliberately not the download_url the Contents
// API returns: for private repos that field is a CDN link carrying a
// short-lived signed token, which would be dead long before this entry's
// cache TTL expires. A URL built from the resolved commit SHA is permanent,
// and the caller authenticates it with its own credential.
//
// Like ghResolveCommitSHA, the request goes through cooldown for identity.
func ghListContents(ctx context.Context, cooldown *agent.GitHubCooldown, identity, apiBase, rawBase, owner, repo, path, commitSHA, token string) ([]GitHubFileEntry, error) {
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
	resp, err := cooldown.Do(client, req, identity)
	if err != nil {
		return nil, fmt.Errorf("failed to list contents for %s/%s at %s: %w", owner, repo, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		return nil, &ghStatusError{status: resp.StatusCode, msg: fmt.Sprintf("GitHub API error %d listing %s/%s at %s: %s", resp.StatusCode, owner, repo, path, string(body))}
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
			URL:  ghRawContentURL(rawBase, owner, repo, commitSHA, item.Path),
			Hash: item.SHA, // Git blob SHA; verified by the broker after download.
			Size: item.Size,
		})
	}

	return entries, nil
}

// ghRawContentURL builds a permanent raw content URL for a file at a pinned
// commit SHA. Because the commit SHA is immutable, the URL stays valid for as
// long as the commit is reachable — unlike the Contents API's download_url,
// which expires within minutes for private repos.
func ghRawContentURL(rawBase, owner, repo, commitSHA, filePath string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		strings.TrimSuffix(rawBase, "/"), owner, repo, commitSHA, ghEscapePathSegments(filePath))
}

// ghEscapePathSegments percent-encodes each path segment while leaving the
// separators intact.
func ghEscapePathSegments(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

// isFullCommitSHA reports whether s is a complete 40-character lowercase hexadecimal SHA.
var fullSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func isFullCommitSHA(s string) bool {
	return len(s) == 40 && fullSHAPattern.MatchString(s)
}
