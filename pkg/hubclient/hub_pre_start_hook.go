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
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// hubPreStartHookBasePath is the hub-scoped (project-less) hook route prefix.
const hubPreStartHookBasePath = "/api/v1/pre-start-hooks"

// HubPreStartHookService handles hub-scoped pre-start hook operations. The hub
// hook is the fallback that applies to any project with no active
// project-scoped hook. Mutating calls require hub admin; reads do not.
//
// Request and response payloads are shared with ProjectPreStartHookService —
// the shapes are identical, only the scope differs.
type HubPreStartHookService interface {
	// List returns all hub-scoped pre-start hooks (active and archived).
	List(ctx context.Context) (*ListProjectPreStartHooksResponse, error)

	// Get returns a single hub-scoped hook by ID.
	Get(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error)

	// Create creates a new hub-scoped hook (archives any current active hub hook).
	Create(ctx context.Context, req *CreateProjectPreStartHookRequest) (*store.ProjectPreStartHook, error)

	// Update updates an existing hub-scoped hook's name, description, or script.
	Update(ctx context.Context, hookID string, req *UpdateProjectPreStartHookRequest) (*store.ProjectPreStartHook, error)

	// Activate marks an archived hub-scoped hook as active (archives the
	// current active hub hook).
	Activate(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error)

	// Delete deletes a hub-scoped hook. Deleting the active hook while other
	// hub hooks exist returns an error.
	Delete(ctx context.Context, hookID string) error
}

// hubPreStartHookService is the concrete implementation.
type hubPreStartHookService struct {
	c *client
}

// HubPreStartHooks returns the hub-scoped pre-start hook service.
func (c *client) HubPreStartHooks() HubPreStartHookService {
	return &hubPreStartHookService{c: c}
}

func (s *hubPreStartHookService) basePath() string {
	return hubPreStartHookBasePath
}

func (s *hubPreStartHookService) List(ctx context.Context) (*ListProjectPreStartHooksResponse, error) {
	resp, err := s.c.get(ctx, s.basePath(), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListProjectPreStartHooksResponse](resp)
}

func (s *hubPreStartHookService) Get(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error) {
	resp, err := s.c.get(ctx, s.basePath()+"/"+url.PathEscape(hookID), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[store.ProjectPreStartHook](resp)
}

func (s *hubPreStartHookService) Create(ctx context.Context, req *CreateProjectPreStartHookRequest) (*store.ProjectPreStartHook, error) {
	resp, err := s.c.post(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[store.ProjectPreStartHook](resp)
}

func (s *hubPreStartHookService) Update(ctx context.Context, hookID string, req *UpdateProjectPreStartHookRequest) (*store.ProjectPreStartHook, error) {
	resp, err := s.c.put(ctx, s.basePath()+"/"+url.PathEscape(hookID), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[store.ProjectPreStartHook](resp)
}

func (s *hubPreStartHookService) Activate(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error) {
	resp, err := s.c.post(ctx, s.basePath()+"/"+url.PathEscape(hookID)+"/activate", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[store.ProjectPreStartHook](resp)
}

func (s *hubPreStartHookService) Delete(ctx context.Context, hookID string) error {
	resp, err := s.c.delete(ctx, s.basePath()+"/"+url.PathEscape(hookID), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
