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
	"sort"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const defaultPooledGroupCacheTTL = 60 * time.Second

type pooledGroupCacheEntry struct {
	groupIDs  []string
	expiresAt time.Time
}

// PooledGroupResolver resolves the set of active pooled-budget user groups for
// billing users at ingest and admission time (Tokenomics P3.3, §4.4).
//
// Effective membership is computed via store.GetEffectiveGroupsForUsers
// (recursive nested explicit groups, skipping expired memberships and
// project_agents groups) and intersected with explicit groups holding at least
// one active pooledGroupLimit binding (value > 0).
//
// Caching: per-replica 60s TTL cache for both the active pooled-group ID set
// and per-user resolved slices. In-process mutations (group membership edits,
// group deletes, and pooled entitlement binding CRUD) invalidate immediately.
// Cross-replica staleness is bounded by the 60s TTL (covered by the +2m seeder
// pass in C2 / D6).
type PooledGroupResolver struct {
	store store.Store
	ttl   time.Duration
	nowFn func() time.Time

	mu              sync.RWMutex
	activeLoaded    bool
	activeExpiresAt time.Time
	activeGroupSet  map[string]struct{}
	userCache       map[string]pooledGroupCacheEntry
}

// NewPooledGroupResolver constructs a PooledGroupResolver with the default 60s TTL.
func NewPooledGroupResolver(st store.Store) *PooledGroupResolver {
	return &PooledGroupResolver{
		store:     st,
		ttl:       defaultPooledGroupCacheTTL,
		nowFn:     func() time.Time { return time.Now().UTC() },
		userCache: make(map[string]pooledGroupCacheEntry),
	}
}

func (r *PooledGroupResolver) now() time.Time {
	if r != nil && r.nowFn != nil {
		return r.nowFn().UTC()
	}
	return time.Now().UTC()
}

// InvalidateAll clears both the active pooled-group set and all per-user cache
// entries on this replica. Called on pooled binding CRUD, child-group edge
// mutations, and group deletions.
func (r *PooledGroupResolver) InvalidateAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.activeLoaded = false
	r.activeExpiresAt = time.Time{}
	r.activeGroupSet = nil
	r.userCache = make(map[string]pooledGroupCacheEntry)
	r.mu.Unlock()
}

// InvalidateUser evicts a single user's cached effective pooled groups on this
// replica. Called on direct user membership add/remove/delete.
func (r *PooledGroupResolver) InvalidateUser(userID string) {
	if r == nil || userID == "" {
		return
	}
	r.mu.Lock()
	delete(r.userCache, userID)
	r.mu.Unlock()
}

// activePooledGroups returns the set of explicit group IDs that currently hold
// at least one positive pooledGroupLimit entitlement binding.
func (r *PooledGroupResolver) activePooledGroups(ctx context.Context) (map[string]struct{}, error) {
	now := r.now()

	r.mu.RLock()
	if r.activeLoaded && now.Before(r.activeExpiresAt) {
		set := r.activeGroupSet
		r.mu.RUnlock()
		return set, nil
	}
	r.mu.RUnlock()

	limitNames := []string{
		store.LimitLLMTokensPerGroupDay,
		store.LimitLLMTokensPerGroupMonth,
	}
	candidateIDs := make(map[string]struct{})
	for _, name := range limitNames {
		def, err := r.store.GetLimitDefinitionByName(ctx, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("lookup limit definition %q: %w", name, err)
		}
		if def == nil {
			continue
		}
		bindings, err := r.store.ListEntitlementBindings(ctx, def.ID)
		if err != nil {
			return nil, fmt.Errorf("list entitlement bindings for %q: %w", name, err)
		}
		for _, b := range bindings {
			if b != nil && b.SubjectType == store.EntitlementSubjectGroup && b.SubjectID != "" && b.Value > 0 {
				candidateIDs[b.SubjectID] = struct{}{}
			}
		}
	}

	activeSet := make(map[string]struct{}, len(candidateIDs))
	if len(candidateIDs) > 0 {
		ids := make([]string, 0, len(candidateIDs))
		for id := range candidateIDs {
			ids = append(ids, id)
		}
		groups, err := r.store.GetGroupsByIDs(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("load pooled groups: %w", err)
		}
		for i := range groups {
			// Skip project_agents groups defensively; only explicit user groups
			// participate in pooled group token budgets (§4.4).
			if groups[i].GroupType == store.GroupTypeExplicit {
				activeSet[groups[i].ID] = struct{}{}
			}
		}
	}

	r.mu.Lock()
	r.activeLoaded = true
	r.activeExpiresAt = now.Add(r.ttl)
	r.activeGroupSet = activeSet
	r.mu.Unlock()
	return activeSet, nil
}

// ResolveForUser returns the sorted, deduplicated list of active pooled group
// IDs for a single billing user.
func (r *PooledGroupResolver) ResolveForUser(ctx context.Context, userID string) ([]string, error) {
	if r == nil || r.store == nil || userID == "" {
		return nil, nil
	}
	res, err := r.ResolveForUsers(ctx, []string{userID})
	if err != nil {
		return nil, err
	}
	return res[userID], nil
}

// ResolveForUsers resolves active pooled group IDs for a batch of distinct
// billing users in one pass: active pooled bindings are loaded once, cached
// users are served from memory, and cache misses are fetched in a single
// batched GetEffectiveGroupsForUsers call.
func (r *PooledGroupResolver) ResolveForUsers(ctx context.Context, userIDs []string) (map[string][]string, error) {
	if r == nil || r.store == nil || len(userIDs) == 0 {
		return nil, nil
	}
	activeSet, err := r.activePooledGroups(ctx)
	if err != nil {
		return nil, err
	}
	if len(activeSet) == 0 {
		return map[string][]string{}, nil
	}

	now := r.now()
	result := make(map[string][]string, len(userIDs))
	seen := make(map[string]struct{}, len(userIDs))
	var misses []string

	r.mu.RLock()
	for _, uid := range userIDs {
		if uid == "" {
			continue
		}
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		if entry, ok := r.userCache[uid]; ok && now.Before(entry.expiresAt) {
			if len(entry.groupIDs) > 0 {
				cp := make([]string, len(entry.groupIDs))
				copy(cp, entry.groupIDs)
				result[uid] = cp
			}
			continue
		}
		misses = append(misses, uid)
	}
	r.mu.RUnlock()

	if len(misses) == 0 {
		return result, nil
	}

	effByUser, err := r.store.GetEffectiveGroupsForUsers(ctx, misses)
	if err != nil {
		return nil, fmt.Errorf("resolve effective groups for users: %w", err)
	}

	exp := now.Add(r.ttl)
	computed := make(map[string][]string, len(misses))
	for _, uid := range misses {
		eff := effByUser[uid]
		var matched []string
		if len(eff) > 0 {
			seenG := make(map[string]struct{}, len(eff))
			for _, gid := range eff {
				if _, active := activeSet[gid]; !active {
					continue
				}
				if _, dup := seenG[gid]; dup {
					continue
				}
				seenG[gid] = struct{}{}
				matched = append(matched, gid)
			}
			sort.Strings(matched)
		}
		computed[uid] = matched
		if len(matched) > 0 {
			cp := make([]string, len(matched))
			copy(cp, matched)
			result[uid] = cp
		}
	}

	r.mu.Lock()
	for uid, matched := range computed {
		r.userCache[uid] = pooledGroupCacheEntry{
			groupIDs:  matched,
			expiresAt: exp,
		}
	}
	r.mu.Unlock()

	return result, nil
}

// PooledGroupResolver returns (lazily constructing on first call) the Hub's
// cached pooled-group resolver. Uses an RLock fast path so hot-path ingest
// requests never contend on the global write lock after initialization.
func (s *Server) PooledGroupResolver() *PooledGroupResolver {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	r := s.pooledGroupResolver
	s.mu.RUnlock()
	if r != nil {
		return r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pooledGroupResolver == nil && s.store != nil {
		s.pooledGroupResolver = NewPooledGroupResolver(s.store)
	}
	return s.pooledGroupResolver
}

// InvalidatePooledGroupCacheAll clears this replica's pooled-group cache.
func (s *Server) InvalidatePooledGroupCacheAll() {
	if s == nil {
		return
	}
	s.mu.RLock()
	r := s.pooledGroupResolver
	s.mu.RUnlock()
	if r != nil {
		r.InvalidateAll()
	}
}

// InvalidatePooledGroupCacheForUser evicts userID from this replica's
// pooled-group cache.
func (s *Server) InvalidatePooledGroupCacheForUser(userID string) {
	if s == nil || userID == "" {
		return
	}
	s.mu.RLock()
	r := s.pooledGroupResolver
	s.mu.RUnlock()
	if r != nil {
		r.InvalidateUser(userID)
	}
}
