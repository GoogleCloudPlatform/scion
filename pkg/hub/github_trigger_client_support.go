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
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
)

const (
	// githubCommentRatePerSecond and githubCommentBurst bound outbound GitHub
	// comments posted by the hub on behalf of a single project (§7.1: 1/s, burst 5).
	githubCommentRatePerSecond = 1.0
	githubCommentBurst         = 5.0
	githubCommentBucketIdleTTL = 10 * time.Minute
	githubCommentMaxBuckets    = 10000

	// githubInstallationTokenSafetyMargin is subtracted from an installation
	// token's expires_at when deciding whether a cached token may still be
	// handed out (§7.3: valid until expires_at - 5 min).
	githubInstallationTokenSafetyMargin    = 5 * time.Minute
	githubInstallationTokenCacheMaxEntries = 10000
)

var (
	// ErrGitHubCommentRateLimited is returned by AllowGitHubComment when a
	// project has exhausted its 1/s burst-5 outbound comment budget (§7.1).
	ErrGitHubCommentRateLimited = errors.New("comment rate limited")

	// ErrGitHubWriteNotPermitted is returned when a caller requests a
	// write-capable GitHub App installation token under a read-only
	// association grant (§7.1).
	ErrGitHubWriteNotPermitted = errors.New("github write not permitted")
)

// githubCommentRateLimiter enforces a bounded in-memory per-project token
// bucket (1/s refill, burst 5). Its zero value is ready to use.
type githubCommentRateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*keysRateBucket
	lastSweep  time.Time
	now        func() time.Time
	maxBuckets int
}

func (l *githubCommentRateLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return timeNow()
}

func (l *githubCommentRateLimiter) capLimit() int {
	if l.maxBuckets > 0 {
		return l.maxBuckets
	}
	return githubCommentMaxBuckets
}

func (l *githubCommentRateLimiter) allow(projectID string) bool {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock()
	if l.buckets == nil {
		l.buckets = make(map[string]*keysRateBucket)
		l.lastSweep = now
	}
	l.sweepLocked(now)

	b, ok := l.buckets[projectID]
	if !ok {
		b = &keysRateBucket{tokens: githubCommentBurst, last: now}
		l.buckets[projectID] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(githubCommentBurst, b.tokens+elapsed*githubCommentRatePerSecond)
		}
		b.last = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *githubCommentRateLimiter) sweepLocked(now time.Time) {
	capLimit := l.capLimit()
	atCap := len(l.buckets) >= capLimit
	if !atCap && now.Sub(l.lastSweep) < githubCommentBucketIdleTTL {
		return
	}
	l.lastSweep = now

	for k, b := range l.buckets {
		if now.Sub(b.last) >= githubCommentBucketIdleTTL {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) < capLimit {
		return
	}

	keys := make([]string, 0, len(l.buckets))
	for k := range l.buckets {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := l.buckets[a].last.Compare(l.buckets[b].last); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	drop := len(keys) / 2
	if drop == 0 && len(keys) > 0 {
		drop = 1
	}
	for _, k := range keys[:drop] {
		delete(l.buckets, k)
	}
}

// AllowGitHubComment consumes one token from projectID's outbound GitHub
// comment bucket (1/s refill, burst 5). When exhausted, it returns
// ErrGitHubCommentRateLimited immediately without calling GitHub.
func (s *Server) AllowGitHubComment(projectID string) error {
	if !s.githubCommentLimiter.allow(projectID) {
		return ErrGitHubCommentRateLimited
	}
	return nil
}

// PostGitHubCommentIdempotent posts an idempotent GitHub issue/PR or
// review-thread comment for a trigger fire, populating opts.ExpectedAuthorLogin
// from G5's GitHub App identity cache (and falling back to suffix-only marker
// matching when the App bot login is unknown).
func (s *Server) PostGitHubCommentIdempotent(
	ctx context.Context,
	installationToken string,
	opts githubapp.PostCommentOptions,
) (*githubapp.IssueComment, error) {
	client, err := s.getGitHubAppClient()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.ExpectedAuthorLogin) == "" {
		opts.ExpectedAuthorLogin = s.GitHubAppBotLogin(ctx)
	}
	return client.PostCommentIdempotent(ctx, installationToken, opts)
}

type githubInstallationTokenCacheKey struct {
	projectID string
	permKey   string
}

type githubInstallationTokenCacheEntry struct {
	token          string
	expiresAt      time.Time
	appID          int64
	apiBase        string
	installationID int64
	ownerRepo      string
	cachedAt       time.Time
}

// githubInstallationTokenCache stores short-lived minted installation tokens
// in memory only, keyed by (project_id, permission_set) and valid until
// expires_at - 5m (§7.3). Its zero value is ready to use.
type githubInstallationTokenCache struct {
	mu         sync.Mutex
	entries    map[githubInstallationTokenCacheKey]githubInstallationTokenCacheEntry
	now        func() time.Time
	maxEntries int
}

func (c *githubInstallationTokenCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return timeNow()
}

func (c *githubInstallationTokenCache) capLimit() int {
	if c.maxEntries > 0 {
		return c.maxEntries
	}
	return githubInstallationTokenCacheMaxEntries
}

func (c *githubInstallationTokenCache) get(
	projectID, permKey string,
	appID int64,
	apiBase string,
	installationID int64,
	ownerRepo string,
) (string, time.Time, bool) {
	if projectID == "" || permKey == "" {
		return "", time.Time{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	key := githubInstallationTokenCacheKey{projectID: projectID, permKey: permKey}
	entry, ok := c.entries[key]
	if !ok {
		return "", time.Time{}, false
	}

	now := c.clock()
	if !now.Before(entry.expiresAt.Add(-githubInstallationTokenSafetyMargin)) ||
		entry.appID != appID ||
		!strings.EqualFold(entry.apiBase, apiBase) ||
		entry.installationID != installationID ||
		!strings.EqualFold(entry.ownerRepo, ownerRepo) {
		delete(c.entries, key)
		return "", time.Time{}, false
	}
	return entry.token, entry.expiresAt, true
}

func (c *githubInstallationTokenCache) put(
	projectID, permKey string,
	appID int64,
	apiBase string,
	installationID int64,
	ownerRepo string,
	token string,
	expiresAt time.Time,
) {
	if projectID == "" || permKey == "" || token == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.clock()
	if !now.Before(expiresAt.Add(-githubInstallationTokenSafetyMargin)) {
		return
	}
	if c.entries == nil {
		c.entries = make(map[githubInstallationTokenCacheKey]githubInstallationTokenCacheEntry)
	}

	capLimit := c.capLimit()
	if len(c.entries) >= capLimit {
		for k, e := range c.entries {
			if !now.Before(e.expiresAt.Add(-githubInstallationTokenSafetyMargin)) {
				delete(c.entries, k)
			}
		}
	}
	if len(c.entries) >= capLimit {
		keys := make([]githubInstallationTokenCacheKey, 0, len(c.entries))
		for k := range c.entries {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b githubInstallationTokenCacheKey) int {
			if cmp := c.entries[a].cachedAt.Compare(c.entries[b].cachedAt); cmp != 0 {
				return cmp
			}
			if a.projectID != b.projectID {
				return strings.Compare(a.projectID, b.projectID)
			}
			return strings.Compare(a.permKey, b.permKey)
		})
		drop := len(keys) / 2
		if drop == 0 && len(keys) > 0 {
			drop = 1
		}
		for _, k := range keys[:drop] {
			delete(c.entries, k)
		}
	}

	c.entries[githubInstallationTokenCacheKey{projectID: projectID, permKey: permKey}] = githubInstallationTokenCacheEntry{
		token:          token,
		expiresAt:      expiresAt,
		appID:          appID,
		apiBase:        apiBase,
		installationID: installationID,
		ownerRepo:      ownerRepo,
		cachedAt:       now,
	}
}

func (c *githubInstallationTokenCache) invalidateProject(projectID string) {
	if projectID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.projectID == projectID {
			delete(c.entries, k)
		}
	}
}

func (s *Server) githubAppConfiguredID() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config.GitHubAppConfig.AppID
}

// canonicalGitHubTokenPermKey builds a deterministic cache key component for a
// GitHub installation token permission set.
func canonicalGitHubTokenPermKey(perms githubapp.TokenPermissions) string {
	var parts []string
	add := func(name, val string) {
		v := strings.ToLower(strings.TrimSpace(val))
		if v != "" {
			parts = append(parts, name+"="+v)
		}
	}
	add("actions", perms.Actions)
	add("checks", perms.Checks)
	add("contents", perms.Contents)
	add("issues", perms.Issues)
	add("metadata", perms.Metadata)
	add("pull_requests", perms.PullRequests)
	return strings.Join(parts, ";")
}

// scopeRequestedGitHubTokenPermissions validates an explicit permission set
// against grant. Any "write" level is lowered to "read" when !grant.WriteAllowed,
// unknown levels are rejected, and an empty set is refused.
func scopeRequestedGitHubTokenPermissions(
	requested githubapp.TokenPermissions,
	grant githubAssociationGrant,
) (githubapp.TokenPermissions, error) {
	level := func(field, raw string) (string, error) {
		v := strings.ToLower(strings.TrimSpace(raw))
		switch v {
		case "":
			return "", nil
		case "read":
			return "read", nil
		case "write":
			if grant.WriteAllowed {
				return "write", nil
			}
			return "read", nil
		default:
			return "", fmt.Errorf("invalid GitHub token permission %q for %s", raw, field)
		}
	}

	var out githubapp.TokenPermissions
	var err error
	if out.Contents, err = level("contents", requested.Contents); err != nil {
		return githubapp.TokenPermissions{}, err
	}
	if out.PullRequests, err = level("pull_requests", requested.PullRequests); err != nil {
		return githubapp.TokenPermissions{}, err
	}
	if out.Issues, err = level("issues", requested.Issues); err != nil {
		return githubapp.TokenPermissions{}, err
	}
	if out.Metadata, err = level("metadata", requested.Metadata); err != nil {
		return githubapp.TokenPermissions{}, err
	}
	if out.Checks, err = level("checks", requested.Checks); err != nil {
		return githubapp.TokenPermissions{}, err
	}
	if out.Actions, err = level("actions", requested.Actions); err != nil {
		return githubapp.TokenPermissions{}, err
	}
	if out == (githubapp.TokenPermissions{}) {
		return githubapp.TokenPermissions{}, fmt.Errorf("explicit GitHub token permissions are required")
	}
	return out, nil
}
