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

// load reads the row for cacheKey, returning (nil, nil) when there is none.
func (s *GitHubResolutionStore) load(ctx context.Context, cacheKey string) (*GitHubCacheEntry, error) {
	row, err := s.client.GitHubResolutionCache.
		Query().
		Where(githubresolutioncache.CacheKeyEQ(cacheKey)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &GitHubCacheEntry{
		CommitSHA:   row.CommitSha,
		FileEntries: row.FileEntries,
		BundleHash:  row.BundleHash,
		TokenScope:  row.TokenScope,
		ExpiresAt:   row.ExpiresAt,
		OriginalURI: row.OriginalURI,
	}, nil
}

// fileEntriesHaveModes reports whether every file entry records a mode.
// Entries written since the hub reads GitHub trees always do; a row written
// before has none (ptone/scion#5781).
func fileEntriesHaveModes(files []GitHubFileEntry) bool {
	for _, f := range files {
		if f.Mode == "" {
			return false
		}
	}
	return true
}

// Get retrieves a cache entry by key, returning (entry, true, nil) on hit,
// (nil, false, nil) on miss or expiration, or (nil, false, error) on DB error.
//
// A row written before file modes were recorded (see GetModeless) is a
// miss, so the ref is resolved again with its modes.
func (s *GitHubResolutionStore) Get(ctx context.Context, cacheKey string) (*GitHubCacheEntry, bool, error) {
	entry, err := s.load(ctx, cacheKey)
	if err != nil || entry == nil {
		return nil, false, err
	}
	if time.Now().After(entry.ExpiresAt) || !fileEntriesHaveModes(entry.FileEntries) {
		return nil, false, nil
	}
	return entry, true, nil
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
// Returns (nil, false, nil) when the row does not exist, is older than
// maxStaleAge or records no file modes (see GetModeless), and
// (nil, false, error) on a DB error.
func (s *GitHubResolutionStore) GetStale(ctx context.Context, cacheKey string, lastTTL, maxStaleAge time.Duration) (*GitHubCacheEntry, bool, error) {
	entry, err := s.load(ctx, cacheKey)
	if err != nil || entry == nil {
		return nil, false, err
	}
	if !entry.ExpiresAt.After(staleCutoff(time.Now(), lastTTL, maxStaleAge)) || !fileEntriesHaveModes(entry.FileEntries) {
		return nil, false, nil
	}
	return entry, true, nil
}

// GetModeless returns the row for cacheKey if it was written before file
// modes were recorded and could still be served: unexpired, or, when
// isBranchRef, within the stale window GetStale uses. Get and GetStale
// treat such a row as a miss; resolveGitHubSkill serves it only when
// re-resolving the ref failed, as it was served before (ptone/scion#5781).
func (s *GitHubResolutionStore) GetModeless(ctx context.Context, cacheKey string, isBranchRef bool, lastTTL, maxStaleAge time.Duration) (*GitHubCacheEntry, bool, error) {
	entry, err := s.load(ctx, cacheKey)
	if err != nil || entry == nil {
		return nil, false, err
	}
	if fileEntriesHaveModes(entry.FileEntries) {
		return nil, false, nil
	}
	now := time.Now()
	fresh := !now.After(entry.ExpiresAt)
	stale := isBranchRef && entry.ExpiresAt.After(staleCutoff(now, lastTTL, maxStaleAge))
	if !fresh && !stale {
		return nil, false, nil
	}
	return entry, true, nil
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
		return "", &ghStatusError{status: resp.StatusCode, msg: fmt.Sprintf("GitHub API error %d resolving %s@%s: %s", resp.StatusCode, repo, ref, ghErrorBody(body))}
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

// ghListTree lists every regular file of the skill directory at path, at any
// depth, with its mode, in one call: GET
// /repos/{owner}/{repo}/git/trees/{commitSHA}:{path}?recursive=1 (see
// agent.DecodeGitHubSkillTree for how symlinks, submodules, unsafe paths
// and a truncated listing are handled). Each entry gets a permanent raw
// content URL, its git blob SHA and its normalised mode ("0755" or
// "0644").
//
// rawBase is the origin for raw content URLs (githubRawBase in production;
// overridden by tests). A URL built from the resolved commit SHA is
// permanent, and the caller authenticates it with its own credential.
//
// Like ghResolveCommitSHA, the request goes through cooldown for identity.
func ghListTree(ctx context.Context, cooldown *agent.GitHubCooldown, identity, apiBase, rawBase, owner, repo, path, commitSHA, token string) ([]GitHubFileEntry, error) {
	url := apiBase + agent.GitHubSkillTreePath(owner, repo, path, commitSHA)
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
		return nil, &ghStatusError{status: resp.StatusCode, msg: fmt.Sprintf("GitHub API error %d listing %s/%s at %s: %s", resp.StatusCode, owner, repo, path, ghErrorBody(body))}
	}

	// The caller (fetchAndCacheGitHubSkill) already prefixes "failed to
	// list contents", so a decode error is returned as it is.
	files, err := agent.DecodeGitHubSkillTree(resp.Body)
	if err != nil {
		return nil, err
	}

	entries := make([]GitHubFileEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, GitHubFileEntry{
			Path: f.Path,
			URL:  ghRawContentURL(rawBase, owner, repo, commitSHA, path+"/"+f.Path),
			Hash: f.SHA, // Git blob SHA; verified by the broker after download.
			Size: f.Size,
			Mode: f.Mode,
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
