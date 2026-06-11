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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// HubSkillResolver resolves skills via the Hub API.
type HubSkillResolver struct {
	client hubclient.SkillService
}

// NewHubSkillResolver creates a resolver that delegates to the Hub's skill resolve endpoint.
func NewHubSkillResolver(client hubclient.SkillService) *HubSkillResolver {
	return &HubSkillResolver{client: client}
}

func (r *HubSkillResolver) ResolverName() string { return "hub" }

func (r *HubSkillResolver) Resolve(ctx context.Context, refs []api.SkillReference, opts ResolveOpts) (*ResolveResult, error) {
	skillRefs := make([]hubclient.ResolveSkillRef, len(refs))
	for i, ref := range refs {
		skillRefs[i] = hubclient.ResolveSkillRef{URI: ref.URI}
	}
	req := &hubclient.ResolveSkillsRequest{
		Skills:    skillRefs,
		ProjectID: opts.ProjectID,
		UserID:    opts.UserID,
	}

	resp, err := r.client.Resolve(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("hub skill resolution failed: %w", err)
	}

	result := &ResolveResult{}

	refByURI := make(map[string]api.SkillReference, len(refs))
	for _, ref := range refs {
		refByURI[ref.URI] = ref
	}

	for _, rs := range resp.Resolved {
		ref, ok := refByURI[rs.URI]
		if !ok {
			continue
		}
		files := make([]ResolvedFile, len(rs.Files))
		for i, f := range rs.Files {
			files[i] = ResolvedFile{
				Path: f.Path,
				URL:  f.URL,
				Hash: f.Hash,
				Size: f.Size,
			}
		}
		result.Resolved = append(result.Resolved, ResolvedSkill{
			Name:               rs.Name,
			URI:                rs.URI,
			As:                 ref.As,
			Version:            rs.ResolvedVersion,
			Hash:               rs.ContentHash,
			Files:              files,
			Deprecated:         rs.Deprecated,
			DeprecationMessage: rs.DeprecationMessage,
			ReplacementURI:     rs.ReplacementURI,
		})
	}

	for _, re := range resp.Errors {
		result.Errors = append(result.Errors, ResolveError{
			URI:     re.URI,
			Code:    re.Code,
			Message: re.Message,
		})
	}

	return result, nil
}
