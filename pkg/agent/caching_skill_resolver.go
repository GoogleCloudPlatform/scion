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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// CachingSkillResolver wraps a SkillResolver with content-hash caching.
// Resolution always delegates to the inner resolver (so latest/range
// constraints see the current state), but installOneSkill checks the
// cache before downloading and populates it after a successful install.
// The cache is passed to installOneSkill via context.
type CachingSkillResolver struct {
	inner SkillResolver
	cache *templatecache.Cache
}

// NewCachingSkillResolver returns a decorator that injects the skill
// cache into the context before delegating resolution to inner.
func NewCachingSkillResolver(inner SkillResolver, cache *templatecache.Cache) *CachingSkillResolver {
	if cache == nil {
		panic("NewCachingSkillResolver: cache must not be nil")
	}
	return &CachingSkillResolver{inner: inner, cache: cache}
}

func (r *CachingSkillResolver) ResolverName() string {
	return resolverName(r.inner)
}

func (r *CachingSkillResolver) Resolve(ctx context.Context, refs []api.SkillReference, opts ResolveOpts) (*ResolveResult, error) {
	ctx = ContextWithSkillCache(ctx, r.cache)

	result, err := r.inner.Resolve(ctx, refs, opts)
	if err != nil {
		return nil, err
	}

	for _, skill := range result.Resolved {
		if skill.Hash == "" {
			continue
		}
		if _, hit := r.cache.Get(skill.Hash); hit {
			util.Debugf("skill cache hit: %s@%s (%s)", skill.Name, skill.Version, truncHash(skill.Hash))
		} else {
			util.Debugf("skill cache miss: %s@%s (%s)", skill.Name, skill.Version, truncHash(skill.Hash))
		}
	}

	return result, nil
}

func truncHash(hash string) string {
	if len(hash) > 16 {
		return hash[:16]
	}
	return hash
}

// --- Skill cache context injection ---

type skillCacheContextKey struct{}

// ContextWithSkillCache returns a context carrying the skill cache for
// use by installOneSkill.
func ContextWithSkillCache(ctx context.Context, cache *templatecache.Cache) context.Context {
	return context.WithValue(ctx, skillCacheContextKey{}, cache)
}

// SkillCacheFromContext retrieves the skill cache, or nil if not set.
func SkillCacheFromContext(ctx context.Context) *templatecache.Cache {
	c, _ := ctx.Value(skillCacheContextKey{}).(*templatecache.Cache)
	return c
}
