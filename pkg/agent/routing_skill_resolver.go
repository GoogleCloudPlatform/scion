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
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// fallbackTimeout is the budget for a detached fallback call. The local GitHub
// resolver makes two metadata API calls plus one raw download per file in each
// skill; a 15-file skill is ~17 HTTP requests, all with a 30s per-request
// timeout. The fallback receives the whole retry set (up to 50 skills) in a
// single call, so the budget must cover the batch. Two minutes is generous for
// small batches; larger batches may hit the ceiling, in which case the budget
// should be scaled by the number of refs being retried rather than raised
// wholesale.
const fallbackTimeout = 2 * time.Minute

// fallbackMinBudget is the minimum remaining lifetime a caller context must have
// to be worth inheriting. A context with less than this is treated as effectively
// spent: the fallback is detached from it and given a fresh fallbackTimeout budget
// instead. This prevents a Hub that is merely slow (rather than fully blocking)
// from leaving the fallback with an unserviceable sliver of time.
const fallbackMinBudget = 10 * time.Second

// fallbackContext returns a context for the fallback resolver. If the primary
// ctx is still healthy and has a usable budget left it is used as-is, so the
// caller's cancellation and deadline are preserved. If it is already cancelled
// or expired — or so close to its deadline that the fallback could not finish
// (e.g. the Hub call consumed nearly all of the caller's budget) — the context
// is detached from the cancellation signal and given a bounded budget so the
// fallback can still complete. Values (logging, tracing) are preserved either
// way.
func fallbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) >= fallbackMinBudget {
			// Healthy with a usable budget — inherit deadline and cancellation.
			return ctx, func() {}
		}
	}
	// Spent, or too little budget left — detach with a bounded budget.
	return context.WithTimeout(context.WithoutCancel(ctx), fallbackTimeout)
}

// RoutingSkillResolver dispatches SkillReferences to scheme-specific resolvers.
// It groups incoming refs by URI scheme, sends each group to the registered
// resolver for that scheme, and merges the results.
type RoutingSkillResolver struct {
	resolvers map[string]SkillResolver // scheme → resolver
	fallback  SkillResolver            // for "skill" scheme and bare names

	// fallbackResolvers holds scheme → resolver used only when the primary
	// fallback (Hub) fails for that scheme. Registering a scheme here also
	// routes it through Hub first, so Hub-side caching applies.
	fallbackResolvers map[string]SkillResolver
}

// NewRoutingSkillResolver creates a routing resolver that uses hub as the
// fallback for skill:// URIs and bare names.
func NewRoutingSkillResolver(hub SkillResolver) *RoutingSkillResolver {
	return &RoutingSkillResolver{
		resolvers:         make(map[string]SkillResolver),
		fallback:          hub,
		fallbackResolvers: make(map[string]SkillResolver),
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

// RegisterFallback registers a scheme-specific fallback resolver. URIs with
// this scheme are routed to the primary fallback (Hub) first — so that Hub-side
// caching and credential minting apply — and only if Hub fails (transport error
// or per-URI errors) is the registered resolver used for the affected URIs.
//
// A resolver registered via Register for the same scheme takes precedence and
// disables the Hub-first routing for that scheme.
//
// Panics if scheme is empty or already registered as a fallback (catches wiring
// bugs at startup, not at request time).
func (r *RoutingSkillResolver) RegisterFallback(scheme string, resolver SkillResolver) {
	if scheme == "" {
		panic("RoutingSkillResolver.RegisterFallback: scheme must not be empty")
	}
	if r.fallbackResolvers == nil {
		r.fallbackResolvers = make(map[string]SkillResolver)
	}
	if _, exists := r.fallbackResolvers[scheme]; exists {
		panic(fmt.Sprintf("RoutingSkillResolver.RegisterFallback: scheme %q already registered", scheme))
	}
	r.fallbackResolvers[scheme] = resolver
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

		// fb is the scheme's fallback resolver, used only when the primary
		// resolver for this group is Hub and Hub fails.
		var fb SkillResolver

		resolver := r.resolvers[scheme]
		if resolver == nil {
			if schemeFB, hasFB := r.fallbackResolvers[scheme]; hasFB {
				// Hub-first routing: Hub is primary, schemeFB is the backstop.
				resolver, fb = r.fallback, schemeFB
				if resolver == nil {
					// No Hub configured — use the fallback directly.
					resolver, fb = schemeFB, nil
				}
			} else if scheme == "skill" || scheme == "" {
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
			if fb == nil {
				return nil, fmt.Errorf("resolver for scheme %q failed: %w", scheme, err)
			}
			// Transport-level failure: retry the whole group with the fallback.
			slog.Warn("primary skill resolver failed, retrying with fallback resolver",
				"scheme", scheme,
				"primary", resolverNameOf(resolver),
				"fallback", resolverNameOf(fb),
				"refs", len(schemeRefs),
				"error", err)
			// A Hub timeout or deadline expiry on the primary call must not
			// immediately cancel the fallback: the caller's semantic intent is
			// "resolve this skill", not "resolve it only if the Hub responds in
			// time". fallbackContext keeps a healthy caller context and swaps a
			// spent one for a bounded budget.
			fbCtx, fbCancel := fallbackContext(ctx)
			sr, err = fb.Resolve(fbCtx, schemeRefs, opts)
			fbCancel()
			if err != nil {
				return nil, fmt.Errorf("fallback resolver for scheme %q failed: %w", scheme, err)
			}
		} else if fb != nil && len(sr.Resolved) < len(schemeRefs) {
			// Fewer resolved skills than refs means the primary either reported
			// per-URI errors or silently omitted refs (e.g. dropped a duplicate
			// URI carrying a distinct As alias). Both cases need a fallback retry.
			sr = r.retryErrorsWithFallback(ctx, scheme, resolver, fb, schemeRefs, sr, opts)
		}
		result.Resolved = append(result.Resolved, sr.Resolved...)
		result.Errors = append(result.Errors, sr.Errors...)
	}

	return result, nil
}

// retryErrorsWithFallback re-resolves every ref the primary resolver did not
// account for, using the scheme's fallback resolver. A ref is considered
// unaccounted for when the primary returned no ResolvedSkill matching its
// (URI, As) pair — whether because the primary reported an explicit per-URI
// error, or because it silently omitted the ref from its result (for example
// by collapsing two aliases of the same URI into a single entry).
//
// Errors for retried URIs are replaced by the fallback's outcome; errors for
// URIs the fallback was not asked about are preserved. If the fallback itself
// fails, the primary's result is returned unchanged.
func (r *RoutingSkillResolver) retryErrorsWithFallback(
	ctx context.Context,
	scheme string,
	primary, fb SkillResolver,
	schemeRefs []api.SkillReference,
	sr *ResolveResult,
	opts ResolveOpts,
) *ResolveResult {
	// Key resolved skills by (URI, As) so that two aliases of the same URI are
	// tracked independently: resolving one alias must not mask the other's
	// absence. Counts handle the case where the same (URI, As) pair legitimately
	// appears more than once in the request.
	resolvedRefs := make(map[string]int, len(sr.Resolved))
	for _, rs := range sr.Resolved {
		resolvedRefs[refKey(rs.URI, rs.As)]++
	}

	retryRefs := make([]api.SkillReference, 0, len(schemeRefs)-len(sr.Resolved))
	retriedURIs := make(map[string]bool, len(schemeRefs))
	for _, ref := range schemeRefs {
		key := refKey(ref.URI, ref.As)
		if resolvedRefs[key] > 0 {
			resolvedRefs[key]--
			continue
		}
		retryRefs = append(retryRefs, ref)
		retriedURIs[ref.URI] = true
	}
	if len(retryRefs) == 0 {
		return sr
	}

	slog.Info("primary skill resolver did not resolve all refs, retrying with fallback resolver",
		"scheme", scheme,
		"primary", resolverNameOf(primary),
		"fallback", resolverNameOf(fb),
		"refs", len(retryRefs))

	// fallbackContext for the same reason as the transport-level retry above: a
	// Hub deadline that has already expired must not pre-empt the fallback.
	fbCtx, fbCancel := fallbackContext(ctx)
	defer fbCancel()

	fr, err := fb.Resolve(fbCtx, retryRefs, opts)
	if err != nil {
		slog.Warn("fallback skill resolver failed, keeping primary errors",
			"scheme", scheme,
			"fallback", resolverNameOf(fb),
			"error", err)
		return sr
	}

	merged := &ResolveResult{Resolved: sr.Resolved}
	for _, e := range sr.Errors {
		// Every unresolved alias of a retried URI went to the fallback, so the
		// fallback's outcome supersedes the primary's error. Errors for URIs that
		// had no matching ref (and so were never retried) are preserved.
		if !retriedURIs[e.URI] {
			merged.Errors = append(merged.Errors, e)
		}
	}
	merged.Resolved = append(merged.Resolved, fr.Resolved...)
	merged.Errors = append(merged.Errors, fr.Errors...)
	return merged
}

// refKey builds a map key identifying a skill reference by URI and alias. The
// NUL separator cannot appear in either component, so the key is unambiguous.
func refKey(uri, as string) string {
	return uri + "\x00" + as
}

// resolverNameOf returns a resolver's name for logging. SkillResolver does not
// require a name, so this falls back to the concrete type when unavailable.
func resolverNameOf(r SkillResolver) string {
	if r == nil {
		return "<nil>"
	}
	if named, ok := r.(interface{ ResolverName() string }); ok {
		return named.ResolverName()
	}
	return fmt.Sprintf("%T", r)
}

// detectScheme extracts the routing scheme from a skill URI.
func detectScheme(uri string) string {
	lower := strings.ToLower(uri)
	if strings.HasPrefix(lower, "gh://") {
		return "gh"
	}
	if strings.HasPrefix(lower, "gcp-skill://") {
		return "gcp-skill"
	}
	if strings.HasPrefix(lower, "https://github.com/") || strings.HasPrefix(lower, "http://github.com/") {
		return "gh"
	}
	if strings.HasPrefix(lower, "skill://") || !strings.Contains(lower, "://") {
		return "skill"
	}
	if idx := strings.Index(lower, "://"); idx > 0 {
		return lower[:idx]
	}
	return ""
}
