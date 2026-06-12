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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// RoutingSkillResolver dispatches SkillReferences to scheme-specific resolvers.
// It groups incoming refs by URI scheme, sends each group to the registered
// resolver for that scheme, and merges the results.
type RoutingSkillResolver struct {
	resolvers map[string]SkillResolver // scheme → resolver
	fallback  SkillResolver            // for "skill" scheme and bare names
}

// NewRoutingSkillResolver creates a routing resolver that uses hub as the
// fallback for skill:// URIs and bare names.
func NewRoutingSkillResolver(hub SkillResolver) *RoutingSkillResolver {
	return &RoutingSkillResolver{
		resolvers: make(map[string]SkillResolver),
		fallback:  hub,
	}
}

// Register adds a scheme-specific resolver. Panics if scheme is empty or
// already registered (catches wiring bugs at startup, not at request time).
func (r *RoutingSkillResolver) Register(scheme string, resolver SkillResolver) {
	if scheme == "" {
		panic("RoutingSkillResolver.Register: scheme must not be empty")
	}
	if _, exists := r.resolvers[scheme]; exists {
		panic(fmt.Sprintf("RoutingSkillResolver.Register: scheme %q already registered", scheme))
	}
	r.resolvers[scheme] = resolver
}

func (r *RoutingSkillResolver) ResolverName() string { return "routing" }

func (r *RoutingSkillResolver) Resolve(ctx context.Context, refs []api.SkillReference, opts ResolveOpts) (*ResolveResult, error) {
	type indexedRef struct {
		ref   api.SkillReference
		index int
	}
	groups := make(map[string][]indexedRef)
	for i, ref := range refs {
		scheme := detectScheme(ref.URI)
		groups[scheme] = append(groups[scheme], indexedRef{ref: ref, index: i})
	}

	result := &ResolveResult{}

	for scheme, irefs := range groups {
		schemeRefs := make([]api.SkillReference, len(irefs))
		for i, ir := range irefs {
			schemeRefs[i] = ir.ref
		}

		resolver := r.resolvers[scheme]
		if resolver == nil {
			if scheme == "skill" || scheme == "" {
				resolver = r.fallback
			}
		}

		if resolver == nil {
			for _, ref := range schemeRefs {
				result.Errors = append(result.Errors, ResolveError{
					URI:     ref.URI,
					Code:    "unsupported_scheme",
					Message: fmt.Sprintf("no resolver registered for scheme %q", scheme),
				})
			}
			continue
		}

		sr, err := resolver.Resolve(ctx, schemeRefs, opts)
		if err != nil {
			return nil, fmt.Errorf("resolver for scheme %q failed: %w", scheme, err)
		}
		result.Resolved = append(result.Resolved, sr.Resolved...)
		result.Errors = append(result.Errors, sr.Errors...)
	}

	return result, nil
}

// detectScheme extracts the routing scheme from a skill URI.
func detectScheme(uri string) string {
	if strings.HasPrefix(uri, "gh://") {
		return "gh"
	}
	if strings.HasPrefix(uri, "gcp-skill://") {
		return "gcp-skill"
	}
	if strings.HasPrefix(uri, "https://github.com/") || strings.HasPrefix(uri, "http://github.com/") {
		return "gh"
	}
	if strings.HasPrefix(uri, "skill://") || !strings.Contains(uri, "://") {
		return "skill"
	}
	if idx := strings.Index(uri, "://"); idx > 0 {
		return uri[:idx]
	}
	return ""
}
