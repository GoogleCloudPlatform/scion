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

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// ProjectGitHub is the client representation of GET /api/v1/projects/{id}/github.
type ProjectGitHub struct {
	ProjectID   string                    `json:"project_id"`
	App         ProjectGitHubApp          `json:"app"`
	Remote      ProjectGitHubRemote       `json:"remote"`
	Association ProjectGitHubAssociation  `json:"association"`
	Status      *GitHubAppProjectStatus   `json:"status"`
	Permissions ProjectGitHubPermissions  `json:"permissions"`
	Can         ProjectGitHubCapabilities `json:"can"`
}

// ProjectGitHubApp describes non-sensitive GitHub App configuration.
type ProjectGitHubApp struct {
	Configured bool   `json:"configured"`
	Slug       string `json:"slug"`
	WebHost    string `json:"web_host"`
}

// ProjectGitHubRemote describes the project's parsed GitHub remote.
type ProjectGitHubRemote struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	OnAppHost bool   `json:"on_app_host"`
}

// ProjectGitHubAssociation describes the project's installation association.
type ProjectGitHubAssociation struct {
	InstallationID *int64  `json:"installation_id"`
	AccountLogin   string  `json:"account_login"`
	Basis          *string `json:"basis"`
	WriteAllowed   bool    `json:"write_allowed"`
	BoundBy        string  `json:"bound_by,omitempty"`
	BoundAt        string  `json:"bound_at,omitempty"`
	Authorized     bool    `json:"authorized"`
	ErrorCode      string  `json:"error_code"`
	Reason         string  `json:"reason"`
}

// ProjectGitHubPermissions describes requested, App-granted, and effective
// token permissions plus any caps applied.
type ProjectGitHubPermissions struct {
	Mode       string            `json:"mode"`
	Requested  map[string]string `json:"requested"`
	AppGranted map[string]string `json:"app_granted"`
	Effective  map[string]string `json:"effective"`
	Capped     []string          `json:"capped"`
	CapReasons map[string]string `json:"cap_reasons"`
}

// ProjectGitHubCapabilities describes caller capabilities on the project's
// GitHub integration.
type ProjectGitHubCapabilities struct {
	Bind            bool `json:"bind"`
	EditPermissions bool `json:"edit_permissions"`
	EditExtraRepos  bool `json:"edit_extra_repos"`
	Discover        bool `json:"discover"`
}

// BindGitHubInstallationResponse is returned by PUT /api/v1/projects/{id}/github-installation.
type BindGitHubInstallationResponse struct {
	ProjectID      string `json:"project_id"`
	InstallationID int64  `json:"installation_id"`
	Status         string `json:"status"`
}

func (s *projectService) GetGitHub(ctx context.Context, projectID string) (*ProjectGitHub, error) {
	if projectID == "" {
		return nil, errors.New("project ID is required")
	}
	path := "/api/v1/projects/" + url.PathEscape(projectID) + "/github"
	resp, err := s.c.get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ProjectGitHub](resp)
}

func (s *projectService) BindGitHubInstallation(ctx context.Context, projectID string, installationID int64) (*BindGitHubInstallationResponse, error) {
	if projectID == "" {
		return nil, errors.New("project ID is required")
	}
	if installationID <= 0 {
		return nil, errors.New("installation ID must be positive")
	}
	path := "/api/v1/projects/" + url.PathEscape(projectID) + "/github-installation"
	body := struct {
		InstallationID int64 `json:"installation_id"`
	}{InstallationID: installationID}
	resp, err := s.c.put(ctx, path, body, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[BindGitHubInstallationResponse](resp)
}

func (s *projectService) UnbindGitHubInstallation(ctx context.Context, projectID string) error {
	if projectID == "" {
		return errors.New("project ID is required")
	}
	path := "/api/v1/projects/" + url.PathEscape(projectID) + "/github-installation"
	resp, err := s.c.delete(ctx, path, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
