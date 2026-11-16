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

package hubclient

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

const githubAppRepoRoutingBasePath = "/api/v1/github-app/repo-routing"

// GitHubAppService provides administrative operations on the Hub's GitHub App
// integration.
type GitHubAppService interface {
	// ListRepoRouting lists primary-project trigger routing entries across
	// repositories with candidate projects (optional installation filter).
	ListRepoRouting(ctx context.Context, installationID *int64) ([]GitHubRepoRoutingEntry, error)

	// SetRepoPrimary sets or updates the primary project for trigger webhook
	// fan-out on owner/repo under req.InstallationID.
	SetRepoPrimary(ctx context.Context, owner, repo string, req *SetGitHubRepoPrimaryRequest) (*GitHubRepoRoutingEntry, error)

	// DeleteRepoPrimary removes the primary-project routing entry for owner/repo
	// (optionally scoped to installationID).
	DeleteRepoPrimary(ctx context.Context, owner, repo string, installationID *int64) error
}

type githubAppService struct {
	c *client
}

// GitHubRepoRoutingProject summarizes one candidate project on a repository
// routing row.
type GitHubRepoRoutingProject struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	HasGitHubTriggers bool   `json:"has_github_triggers"`
}

// GitHubRepoRoutingEntry describes primary-project routing for one repository
// under a GitHub App installation.
type GitHubRepoRoutingEntry struct {
	InstallationID   int64                      `json:"installation_id"`
	Repo             string                     `json:"repo"`
	Projects         []GitHubRepoRoutingProject `json:"projects"`
	PrimaryProjectID string                     `json:"primary_project_id,omitempty"`
	Status           string                     `json:"status"`
}

// SetGitHubRepoPrimaryRequest is the request body for SetRepoPrimary.
type SetGitHubRepoPrimaryRequest struct {
	InstallationID   int64  `json:"installation_id"`
	PrimaryProjectID string `json:"primary_project_id"`
}

func githubAppRepoRoutingItemPath(owner, repo string) string {
	return githubAppRepoRoutingBasePath + "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

func (s *githubAppService) ListRepoRouting(ctx context.Context, installationID *int64) ([]GitHubRepoRoutingEntry, error) {
	query := url.Values{}
	if installationID != nil {
		query.Set("installation", strconv.FormatInt(*installationID, 10))
	}
	resp, err := s.c.getWithQuery(ctx, githubAppRepoRoutingBasePath, query, nil)
	if err != nil {
		return nil, err
	}
	entries, err := apiclient.DecodeRequired[[]GitHubRepoRoutingEntry](resp)
	if err != nil {
		return nil, err
	}
	return *entries, nil
}

func (s *githubAppService) SetRepoPrimary(ctx context.Context, owner, repo string, req *SetGitHubRepoPrimaryRequest) (*GitHubRepoRoutingEntry, error) {
	if owner == "" || repo == "" {
		return nil, errors.New("owner and repo are required")
	}
	if req == nil {
		return nil, errors.New("set repo primary request is required")
	}
	resp, err := s.c.put(ctx, githubAppRepoRoutingItemPath(owner, repo), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[GitHubRepoRoutingEntry](resp)
}

func (s *githubAppService) DeleteRepoPrimary(ctx context.Context, owner, repo string, installationID *int64) error {
	if owner == "" || repo == "" {
		return errors.New("owner and repo are required")
	}
	path := githubAppRepoRoutingItemPath(owner, repo)
	if installationID != nil {
		q := url.Values{}
		q.Set("installation", strconv.FormatInt(*installationID, 10))
		path += "?" + q.Encode()
	}
	resp, err := s.c.delete(ctx, path, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
