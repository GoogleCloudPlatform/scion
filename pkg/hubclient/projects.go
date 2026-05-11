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
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// ProjectService handles project operations.
type ProjectService interface {
	// List returns projects matching the filter criteria.
	List(ctx context.Context, opts *ListProjectsOptions) (*ListProjectsResponse, error)

	// Get returns a single project by ID.
	Get(ctx context.Context, projectID string) (*Project, error)

	// Register registers a project (upsert based on git remote).
	Register(ctx context.Context, req *RegisterProjectRequest) (*RegisterProjectResponse, error)

	// Create creates a project without a contributing broker.
	Create(ctx context.Context, req *CreateProjectRequest) (*Project, error)

	// Update updates project metadata.
	Update(ctx context.Context, projectID string, req *UpdateProjectRequest) (*Project, error)

	// Delete removes a project and all its agents.
	Delete(ctx context.Context, projectID string) error

	// ListAgents returns agents in a project.
	ListAgents(ctx context.Context, projectID string, opts *ListAgentsOptions) (*ListAgentsResponse, error)

	// ListProviders returns runtime brokers providing services to a project.
	ListProviders(ctx context.Context, projectID string) (*ListProvidersResponse, error)

	// AddProvider adds a broker as a provider to a project.
	AddProvider(ctx context.Context, projectID string, req *AddProviderRequest) (*AddProviderResponse, error)

	// RemoveProvider removes a broker from a project.
	RemoveProvider(ctx context.Context, projectID, brokerID string) error

	// GetAgent returns an agent by ID or slug within a project.
	GetAgent(ctx context.Context, projectID, agentID string) (*Agent, error)

	// DeleteAgent removes an agent by ID or slug within a project.
	DeleteAgent(ctx context.Context, projectID, agentID string, opts *DeleteAgentOptions) error

	// GetSettings retrieves project settings.
	GetSettings(ctx context.Context, projectID string) (*ProjectSettings, error)

	// UpdateSettings updates project settings.
	UpdateSettings(ctx context.Context, projectID string, settings *ProjectSettings) (*ProjectSettings, error)

	// RefreshCache triggers a cache refresh for a linked project.
	// The hub pulls the workspace from a connected provider broker.
	RefreshCache(ctx context.Context, projectID string) (*ProjectCacheRefreshResponse, error)

	// GetCacheStatus returns the cache status for a project workspace.
	GetCacheStatus(ctx context.Context, projectID string) (*ProjectCacheStatusResponse, error)
}

// projectService is the implementation of ProjectService.
type projectService struct {
	c *client
}

// ListProjectsOptions configures project list filtering.
type ListProjectsOptions struct {
	Visibility string // Filter by visibility
	GitRemote  string // Filter by git remote (exact or prefix)
	BrokerID   string // Filter by contributing broker
	Name       string // Filter by exact name (case-insensitive)
	Slug       string // Filter by exact slug (case-insensitive)
	Labels     map[string]string
	Page       apiclient.PageOptions
}

// ListProjectsResponse is the response from listing projects.
type ListProjectsResponse struct {
	Projects []Project
	Page     apiclient.PageResult
}

// RegisterProjectRequest is the request for registering a project.
type RegisterProjectRequest struct {
	ID        string            `json:"id,omitempty"` // Client-provided project ID (from project_id setting)
	Name      string            `json:"name"`
	GitRemote string            `json:"gitRemote"`
	Path      string            `json:"path,omitempty"`
	BrokerID  string            `json:"brokerId,omitempty"` // Link to existing broker (two-phase flow)
	Broker    *BrokerInfo       `json:"broker,omitempty"`   // DEPRECATED: Use BrokerID with two-phase registration
	Profiles  []string          `json:"profiles,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// BrokerInfo describes the registering broker.
type BrokerInfo struct {
	ID           string              `json:"id,omitempty"`
	Name         string              `json:"name"`
	Version      string              `json:"version"`
	Capabilities *BrokerCapabilities `json:"capabilities,omitempty"`
	Profiles     []BrokerProfile     `json:"profiles,omitempty"`
}

// RegisterProjectResponse is the response from registering a project.
type RegisterProjectResponse struct {
	Project     *Project       `json:"project"`
	LegacyProject *Project     `json:"grove,omitempty"`      // Legacy alias for compatibility
	Broker      *RuntimeBroker `json:"broker,omitempty"`      // Populated if brokerId or broker provided
	Created     bool           `json:"created"`               // True if project was newly created
	Matches     []ProjectMatch `json:"matches,omitempty"`     // Populated when multiple projects share the same git remote
	BrokerToken string         `json:"brokerToken,omitempty"` // DEPRECATED: use two-phase registration
	SecretKey   string         `json:"secretKey,omitempty"`   // DEPRECATED: secrets only from /brokers/join
}

// UnmarshalJSON implements custom unmarshaling to support legacy grove field.
func (r *RegisterProjectResponse) UnmarshalJSON(data []byte) error {
	type alias RegisterProjectResponse
	var aux alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = RegisterProjectResponse(aux)
	if r.Project == nil && r.LegacyProject != nil {
		r.Project = r.LegacyProject
	}
	return nil
}

// ProjectMatch holds summary information about a project for disambiguation.
type ProjectMatch struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// CreateProjectRequest is the request for creating a project without a broker.
type CreateProjectRequest struct {
	ID         string            `json:"id,omitempty"`
	Slug       string            `json:"slug,omitempty"`
	Name       string            `json:"name"`
	GitRemote  string            `json:"gitRemote,omitempty"`
	Visibility string            `json:"visibility,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// UpdateProjectRequest is the request for updating a project.
type UpdateProjectRequest struct {
	Name                   string            `json:"name,omitempty"`
	Labels                 map[string]string `json:"labels,omitempty"`
	Annotations            map[string]string `json:"annotations,omitempty"`
	Visibility             string            `json:"visibility,omitempty"`
	DefaultRuntimeBrokerID string            `json:"defaultRuntimeBrokerId,omitempty"`
}

// ListProvidersResponse is the response from listing project providers.
type ListProvidersResponse struct {
	Providers []ProjectProvider `json:"providers"`
}

// ProviderCount returns the number of providers.
func (r *ListProvidersResponse) ProviderCount() int {
	if r == nil {
		return 0
	}
	return len(r.Providers)
}

// ProviderNames returns the broker names of all providers.
func (r *ListProvidersResponse) ProviderNames() []string {
	if r == nil {
		return nil
	}
	names := make([]string, len(r.Providers))
	for i, p := range r.Providers {
		names[i] = p.BrokerName
	}
	return names
}

// AddProviderRequest is the request for adding a broker as a project provider.
type AddProviderRequest struct {
	BrokerID  string `json:"brokerId"`
	LocalPath string `json:"localPath,omitempty"`
}

// AddProviderResponse is the response after adding a provider.
type AddProviderResponse struct {
	Provider *ProjectProvider `json:"provider"`
}

// List returns projects matching the filter criteria.
func (s *projectService) List(ctx context.Context, opts *ListProjectsOptions) (*ListProjectsResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Visibility != "" {
			query.Set("visibility", opts.Visibility)
		}
		if opts.GitRemote != "" {
			query.Set("gitRemote", opts.GitRemote)
		}
		if opts.BrokerID != "" {
			query.Set("brokerId", opts.BrokerID)
		}
		if opts.Name != "" {
			query.Set("name", opts.Name)
		}
		if opts.Slug != "" {
			query.Set("slug", opts.Slug)
		}
		for k, v := range opts.Labels {
			query.Add("label", fmt.Sprintf("%s=%s", k, v))
		}
		opts.Page.ToQuery(query)
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/projects", query, nil)
	if err != nil {
		return nil, err
	}

	type listResponse struct {
		Projects       []Project `json:"projects"`
		LegacyProjects []Project `json:"groves,omitempty"`
		NextCursor     string    `json:"nextCursor,omitempty"`
		TotalCount     int       `json:"totalCount,omitempty"`
	}

	result, err := apiclient.DecodeResponse[listResponse](resp)
	if err != nil {
		return nil, err
	}

	projects := result.Projects
	if len(projects) == 0 && len(result.LegacyProjects) > 0 {
		projects = result.LegacyProjects
	}

	return &ListProjectsResponse{
		Projects: projects,
		Page: apiclient.PageResult{
			NextCursor: result.NextCursor,
			TotalCount: result.TotalCount,
		},
	}, nil
}

// Get returns a single project by ID.
func (s *projectService) Get(ctx context.Context, projectID string) (*Project, error) {
	resp, err := s.c.get(ctx, "/api/v1/projects/"+projectID, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Project](resp)
}

// Register registers a project (upsert based on git remote).
func (s *projectService) Register(ctx context.Context, req *RegisterProjectRequest) (*RegisterProjectResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/projects/register", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[RegisterProjectResponse](resp)
}

// Create creates a project without a contributing broker.
func (s *projectService) Create(ctx context.Context, req *CreateProjectRequest) (*Project, error) {
	resp, err := s.c.post(ctx, "/api/v1/projects", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Project](resp)
}

// Update updates project metadata.
func (s *projectService) Update(ctx context.Context, projectID string, req *UpdateProjectRequest) (*Project, error) {
	resp, err := s.c.patch(ctx, "/api/v1/projects/"+projectID, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Project](resp)
}

// Delete removes a project and all its agents.
func (s *projectService) Delete(ctx context.Context, projectID string) error {
	path := "/api/v1/projects/" + projectID
	resp, err := s.c.delete(ctx, path, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// ListAgents returns agents in a project.
func (s *projectService) ListAgents(ctx context.Context, projectID string, opts *ListAgentsOptions) (*ListAgentsResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Phase != "" {
			query.Set("phase", opts.Phase)
		}
		if opts.RuntimeBrokerID != "" {
			query.Set("runtimeBrokerId", opts.RuntimeBrokerID)
		}
		for k, v := range opts.Labels {
			query.Add("label", fmt.Sprintf("%s=%s", k, v))
		}
		opts.Page.ToQuery(query)
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/projects/"+projectID+"/agents", query, nil)
	if err != nil {
		return nil, err
	}

	type listResponse struct {
		Agents     []Agent `json:"agents"`
		NextCursor string  `json:"nextCursor,omitempty"`
		TotalCount int       `json:"totalCount,omitempty"`
	}

	result, err := apiclient.DecodeResponse[listResponse](resp)
	if err != nil {
		return nil, err
	}

	return &ListAgentsResponse{
		Agents: result.Agents,
		Page: apiclient.PageResult{
			NextCursor: result.NextCursor,
			TotalCount: result.TotalCount,
		},
	}, nil
}

// ListProviders returns runtime brokers providing services to a project.
func (s *projectService) ListProviders(ctx context.Context, projectID string) (*ListProvidersResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/projects/"+projectID+"/providers", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListProvidersResponse](resp)
}

// AddProvider adds a broker as a provider to a project.
func (s *projectService) AddProvider(ctx context.Context, projectID string, req *AddProviderRequest) (*AddProviderResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/projects/"+projectID+"/providers", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[AddProviderResponse](resp)
}

// RemoveProvider removes a broker from a project.
func (s *projectService) RemoveProvider(ctx context.Context, projectID, brokerID string) error {
	resp, err := s.c.delete(ctx, "/api/v1/projects/"+projectID+"/providers/"+brokerID, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// GetSettings retrieves project settings.
func (s *projectService) GetSettings(ctx context.Context, projectID string) (*ProjectSettings, error) {
	resp, err := s.c.get(ctx, "/api/v1/projects/"+projectID+"/settings", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ProjectSettings](resp)
}

// UpdateSettings updates project settings.
func (s *projectService) UpdateSettings(ctx context.Context, projectID string, settings *ProjectSettings) (*ProjectSettings, error) {
	resp, err := s.c.put(ctx, "/api/v1/projects/"+projectID+"/settings", settings, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ProjectSettings](resp)
}

// GetAgent returns an agent by ID or slug within a project.
func (s *projectService) GetAgent(ctx context.Context, projectID, agentID string) (*Agent, error) {
	resp, err := s.c.get(ctx, "/api/v1/projects/"+projectID+"/agents/"+agentID, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Agent](resp)
}

// DeleteAgent removes an agent by ID or slug within a project.
func (s *projectService) DeleteAgent(ctx context.Context, projectID, agentID string, opts *DeleteAgentOptions) error {
	path := "/api/v1/projects/" + projectID + "/agents/" + agentID
	if opts != nil {
		query := url.Values{}
		if opts.DeleteFiles {
			query.Set("deleteFiles", "true")
		}
		if opts.RemoveBranch {
			query.Set("removeBranch", "true")
		}
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
	}

	resp, err := s.c.delete(ctx, path, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// ProjectCacheRefreshResponse is the response for a project cache refresh operation.
type ProjectCacheRefreshResponse struct {
	ProjectID  string    `json:"projectId"`
	BrokerID   string    `json:"brokerId"`
	FileCount  int       `json:"fileCount"`
	TotalBytes int64     `json:"totalBytes"`
	CachedAt   time.Time `json:"cachedAt"`
}

// MarshalJSON implements custom marshaling to support legacy groveId field.
func (r ProjectCacheRefreshResponse) MarshalJSON() ([]byte, error) {
	type Alias ProjectCacheRefreshResponse
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId"`
	}{
		Alias:   Alias(r),
		GroveID: r.ProjectID,
	})
}

// UnmarshalJSON implements custom unmarshaling to support legacy groveId field.
func (r *ProjectCacheRefreshResponse) UnmarshalJSON(data []byte) error {
	type Alias ProjectCacheRefreshResponse
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ProjectID == "" && aux.GroveID != "" {
		r.ProjectID = aux.GroveID
	}
	return nil
}

// ProjectCacheStatusResponse is the response for project cache status.
type ProjectCacheStatusResponse struct {
	ProjectID   string     `json:"projectId"`
	Cached      bool       `json:"cached"`
	BrokerID    string     `json:"brokerId,omitempty"`
	FileCount   int        `json:"fileCount"`
	TotalBytes  int64      `json:"totalBytes"`
	LastRefresh *time.Time `json:"lastRefresh,omitempty"`
}

// MarshalJSON implements custom marshaling to support legacy groveId field.
func (r ProjectCacheStatusResponse) MarshalJSON() ([]byte, error) {
	type Alias ProjectCacheStatusResponse
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId"`
	}{
		Alias:   Alias(r),
		GroveID: r.ProjectID,
	})
}

// UnmarshalJSON implements custom unmarshaling to support legacy groveId field.
func (r *ProjectCacheStatusResponse) UnmarshalJSON(data []byte) error {
	type Alias ProjectCacheStatusResponse
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ProjectID == "" && aux.GroveID != "" {
		r.ProjectID = aux.GroveID
	}
	return nil
}

// RefreshCache triggers a cache refresh for a linked project.
func (s *projectService) RefreshCache(ctx context.Context, projectID string) (*ProjectCacheRefreshResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/projects/"+projectID+"/workspace/cache/refresh", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ProjectCacheRefreshResponse](resp)
}

// GetCacheStatus returns the cache status for a project workspace.
func (s *projectService) GetCacheStatus(ctx context.Context, projectID string) (*ProjectCacheStatusResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/projects/"+projectID+"/workspace/cache/status", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ProjectCacheStatusResponse](resp)
}
