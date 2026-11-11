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
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// PreResolvedSkillResolver serves skill references the Hub already resolved
// at dispatch time and delegates everything else to next.
//
// The Hub resolves Hub-registry skills as the agent's creator and ships the
// result with the create request (#1784). The broker's own Hub identity cannot
// read non-public skills, so those results must be used verbatim: a URI with
// a pre-resolved outcome — success or error — is never re-resolved through
// next. Pre-resolved errors (e.g. the creator lacks read access) are
// authoritative and surface as per-skill errors so that required skills fail
// with the Hub's message and optional skills are skipped.
type PreResolvedSkillResolver struct {
	pre  *hubclient.ResolveSkillsResponse
	next SkillResolver
	// hubEndpoint absolutizes Hub-relative download URLs (local storage).
	hubEndpoint string
}

// NewPreResolvedSkillResolver returns a resolver that answers from pre and
// falls through to next (which may be nil) for URIs pre does not cover.
// hubEndpoint is the Hub base URL used to absolutize Hub-relative file URLs.
func NewPreResolvedSkillResolver(pre *hubclient.ResolveSkillsResponse, next SkillResolver, hubEndpoint string) *PreResolvedSkillResolver {
	return &PreResolvedSkillResolver{pre: pre, next: next, hubEndpoint: hubEndpoint}
}

// ResolverName reports "hub": pre-resolved skills come from the Hub.
func (r *PreResolvedSkillResolver) ResolverName() string { return "hub" }

// Resolve implements SkillResolver.
func (r *PreResolvedSkillResolver) Resolve(ctx context.Context, refs []api.SkillReference, opts ResolveOpts) (*ResolveResult, error) {
	covered := make(map[string]bool)
	if r.pre != nil {
		for _, rs := range r.pre.Resolved {
			covered[rs.URI] = true
		}
		for _, re := range r.pre.Errors {
			covered[re.URI] = true
		}
	}

	var preRefs, rest []api.SkillReference
	for _, ref := range refs {
		if covered[ref.URI] {
			preRefs = append(preRefs, ref)
		} else {
			rest = append(rest, ref)
		}
	}

	result := &ResolveResult{}
	if len(preRefs) > 0 {
		requested := make(map[string]bool, len(preRefs))
		for _, ref := range preRefs {
			requested[ref.URI] = true
		}
		sub := &hubclient.ResolveSkillsResponse{}
		for _, rs := range r.pre.Resolved {
			if requested[rs.URI] {
				sub.Resolved = append(sub.Resolved, rs)
			}
		}
		for _, re := range r.pre.Errors {
			if requested[re.URI] {
				sub.Errors = append(sub.Errors, re)
			}
		}
		converted := hubResolveResponseToResult(preRefs, sub)
		for _, rs := range converted.Resolved {
			if err := absolutizeFileURLs(rs.Files, r.hubEndpoint); err != nil {
				result.Errors = append(result.Errors, ResolveError{URI: rs.URI, Code: "invalid_url", Message: err.Error()})
				continue
			}
			result.Resolved = append(result.Resolved, rs)
		}
		result.Errors = append(result.Errors, converted.Errors...)
	}

	if len(rest) > 0 {
		if r.next == nil {
			for _, ref := range rest {
				result.Errors = append(result.Errors, ResolveError{
					URI: ref.URI, Code: "no_resolver",
					Message: "skill was not resolved by the Hub and no skill resolver is available",
				})
			}
			return result, nil
		}
		sub, err := r.next.Resolve(ctx, rest, opts)
		if err != nil {
			return nil, err
		}
		if sub != nil {
			result.Resolved = append(result.Resolved, sub.Resolved...)
			result.Errors = append(result.Errors, sub.Errors...)
		}
	}
	return result, nil
}

// absolutizeFileURLs rewrites Hub-relative download paths ("/api/v1/...",
// emitted by the Hub for local storage) in place as absolute URLs under
// hubEndpoint, mirroring the Hub's own local-storage URL rewrite. Absolute
// URLs are left unchanged.
func absolutizeFileURLs(files []ResolvedFile, hubEndpoint string) error {
	for i := range files {
		if !strings.HasPrefix(files[i].URL, "/") {
			continue
		}
		if hubEndpoint == "" {
			return fmt.Errorf("file %s has a hub-relative download URL but no hub endpoint is known", files[i].Path)
		}
		files[i].URL = strings.TrimRight(hubEndpoint, "/") + files[i].URL
	}
	return nil
}
