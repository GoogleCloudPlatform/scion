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
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	githubtrigger "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
)

const githubEnrichmentTimeout = 5 * time.Second

var githubEnrichmentPermissions = githubapp.TokenPermissions{
	PullRequests: "read",
	Metadata:     "read",
}

// NeedsEnrichment implements trigger.SourceEnricher for Stage 3 §5.5:
// returns true for GitHub issue_comment events on pull requests that have not
// yet been enriched.
func (c serverTriggerCore) NeedsEnrichment(ev *hubfire.FireEvent) bool {
	if ev == nil || c.srv == nil {
		return false
	}
	return githubtrigger.NeedsPullRequestEnrichment(ev.SourceKind, ev.Attributes)
}

// Enrich implements trigger.SourceEnricher for Stage 3 §5.5: fetches the pull
// request via one bounded (5 s) GET /repos/{owner}/{repo}/pulls/{number} call
// using a cached read-only installation token, merges the PR facts and
// lists.enriched provenance within the 8 KiB ceilings, and returns content-free
// errors on failure.
func (c serverTriggerCore) Enrich(ctx context.Context, ev *hubfire.FireEvent) (map[string]string, map[string][]string, error) {
	if ev == nil || c.srv == nil || c.srv.store == nil {
		return nil, nil, errors.New("github enrichment: hub unavailable")
	}
	return c.srv.enrichGitHubTriggerEvent(ctx, ev)
}

func (s *Server) enrichGitHubTriggerEvent(ctx context.Context, ev *hubfire.FireEvent) (map[string]string, map[string][]string, error) {
	failWith := func(err error) (map[string]string, map[string][]string, error) {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		failAttrs, failLists, ceilErr := githubtrigger.ApplyPullRequestEnrichmentFailure(ev.Attributes, ev.Lists)
		if ceilErr != nil {
			// Existing protected attributes alone already exceed the 8 KiB ceiling,
			// so even adding enriched="failed" cannot fit without dropping protected
			// keys; return the existing non-nil map so the dispatcher does not add
			// enriched="failed" past the bound.
			return ev.Attributes, ev.Lists, err
		}
		return failAttrs, failLists, err
	}

	repoFullName := strings.TrimSpace(ev.Attributes["repo"])
	owner, repo, hasSlash := strings.Cut(repoFullName, "/")
	if !hasSlash || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return failWith(errors.New("github enrichment: invalid event repo attribute"))
	}
	prNumber, err := strconv.Atoi(strings.TrimSpace(ev.Attributes["number"]))
	if err != nil || prNumber <= 0 {
		return failWith(errors.New("github enrichment: invalid event pull request number"))
	}

	enrichCtx, cancel := context.WithTimeout(ctx, githubEnrichmentTimeout)
	defer cancel()

	project, err := s.store.GetProject(enrichCtx, ev.ProjectID)
	if err != nil {
		return failWith(errors.New("github enrichment: load project failed"))
	}

	token, _, err := s.MintGitHubAppTokenWithPermissions(enrichCtx, project, githubEnrichmentPermissions)
	if err != nil {
		return failWith(classifyEnrichmentMintError(err))
	}

	client, err := s.getGitHubAppClient()
	if err != nil {
		return failWith(errors.New("github enrichment: github app client unavailable"))
	}

	prBody, err := client.GetPullRequestJSON(enrichCtx, token, owner, repo, prNumber)
	if err != nil {
		var apiErr *githubapp.PullRequestAPIError
		if errors.As(err, &apiErr) {
			return failWith(fmt.Errorf("github enrichment: %s", apiErr.ErrorCode))
		}
		return failWith(errors.New("github enrichment: pull request fetch failed"))
	}

	attrs, lists, err := githubtrigger.ApplyPullRequestEnrichment(ev.Attributes, ev.Lists, prBody)
	if err != nil {
		if errors.Is(err, githubtrigger.ErrAttributesCeilingExceeded) {
			return failWith(githubtrigger.ErrAttributesCeilingExceeded)
		}
		return failWith(errors.New("github enrichment: pull request normalization failed"))
	}
	return attrs, lists, nil
}

func classifyEnrichmentMintError(err error) error {
	var mintErr *githubapp.TokenMintError
	if errors.As(err, &mintErr) {
		return fmt.Errorf("github enrichment: token mint failed (%s)", mintErr.ErrorCode)
	}
	if errors.Is(err, errGitHubAssociationUnauthorized) {
		return errors.New("github enrichment: association unauthorized")
	}
	if errors.Is(err, errGitHubAssociationCheckFailed) {
		return errors.New("github enrichment: association check failed")
	}
	if errors.Is(err, ErrGitHubWriteNotPermitted) {
		return errors.New("github enrichment: permission denied")
	}
	return errors.New("github enrichment: token mint failed")
}
