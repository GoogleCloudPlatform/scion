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
	"fmt"
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ProjectPreStartHookService handles pre-start hook operations for a project.
type ProjectPreStartHookService interface {
	// List returns all pre-start hooks for the project (active and archived).
	List(ctx context.Context) (*ListProjectPreStartHooksResponse, error)

	// Get returns a single hook by ID.
	Get(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error)

	// Create creates a new pre-start hook (archives any current active hook).
	Create(ctx context.Context, req *CreateProjectPreStartHookRequest) (*store.ProjectPreStartHook, error)

	// Update updates an existing hook's name, description, or script.
	Update(ctx context.Context, hookID string, req *UpdateProjectPreStartHookRequest) (*store.ProjectPreStartHook, error)

	// Activate marks an archived hook as active (archives the current active hook).
	Activate(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error)

	// Delete deletes an archived hook. Deleting the active hook returns an error.
	Delete(ctx context.Context, hookID string) error
}

// ListProjectPreStartHooksResponse is the response body for listing hooks.
type ListProjectPreStartHooksResponse struct {
	Hooks []store.ProjectPreStartHook `json:"hooks"`
}

// CreateProjectPreStartHookRequest is the request body for creating a hook.
type CreateProjectPreStartHookRequest struct {
	Name        string `json:"name"`
	Slug        string `json:"slug,omitempty"`
	Description string `json:"description,omitempty"`
	Script      string `json:"script"`
}

// UpdateProjectPreStartHookRequest is the request body for updating a hook.
type UpdateProjectPreStartHookRequest struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Script      *string `json:"script,omitempty"`
}

// projectPreStartHookService is the concrete implementation.
type projectPreStartHookService struct {
	c         *client
	projectID string
}

// ProjectPreStartHooks returns a ProjectPreStartHookService scoped to a project.
func (c *client) ProjectPreStartHooks(projectID string) ProjectPreStartHookService {
	return &projectPreStartHookService{c: c, projectID: projectID}
}

func (s *projectPreStartHookService) basePath() string {
	return fmt.Sprintf("/api/v1/projects/%s/pre-start-hooks", s.projectID)
}

func (s *projectPreStartHookService) List(ctx context.Context) (*ListProjectPreStartHooksResponse, error) {
	resp, err := s.c.get(ctx, s.basePath(), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListProjectPreStartHooksResponse](resp)
}

func (s *projectPreStartHookService) Get(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error) {
	resp, err := s.c.get(ctx, s.basePath()+"/"+url.PathEscape(hookID), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[store.ProjectPreStartHook](resp)
}

func (s *projectPreStartHookService) Create(ctx context.Context, req *CreateProjectPreStartHookRequest) (*store.ProjectPreStartHook, error) {
	resp, err := s.c.post(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[store.ProjectPreStartHook](resp)
}

func (s *projectPreStartHookService) Update(ctx context.Context, hookID string, req *UpdateProjectPreStartHookRequest) (*store.ProjectPreStartHook, error) {
	resp, err := s.c.put(ctx, s.basePath()+"/"+url.PathEscape(hookID), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[store.ProjectPreStartHook](resp)
}

func (s *projectPreStartHookService) Activate(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error) {
	resp, err := s.c.post(ctx, s.basePath()+"/"+url.PathEscape(hookID)+"/activate", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[store.ProjectPreStartHook](resp)
}

func (s *projectPreStartHookService) Delete(ctx context.Context, hookID string) error {
	resp, err := s.c.delete(ctx, s.basePath()+"/"+url.PathEscape(hookID), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
