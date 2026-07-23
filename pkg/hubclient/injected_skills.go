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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// InjectedSkillsService handles injected-skills operations for a specific scope
// (project or user).
type InjectedSkillsService interface {
	// List returns the injected-skills list for this scope.
	List(ctx context.Context) (*api.SkillInjectionList, error)

	// Add adds a single skill entry to the injected-skills list.
	Add(ctx context.Context, req *AddInjectedSkillRequest) (*api.SkillInjectionEntry, error)

	// Set replaces the entire injected-skills list atomically.
	Set(ctx context.Context, entries []api.SkillInjectionEntry) (*api.SkillInjectionList, error)

	// Remove deletes a single injected-skills entry by ID.
	Remove(ctx context.Context, entryID string) error
}

// AddInjectedSkillRequest is the request body for adding a single injected-skill entry.
type AddInjectedSkillRequest struct {
	SkillURI  string `json:"skillUri"`
	SkillAs   string `json:"skillAs,omitempty"`
	Optional  bool   `json:"optional,omitempty"`
	SortOrder int    `json:"sortOrder,omitempty"`
}

// setInjectedSkillsRequest is the request body for bulk-replacing injected skills.
type setInjectedSkillsRequest struct {
	Entries []api.SkillInjectionEntry `json:"entries"`
}

// projectInjectedSkillsService implements InjectedSkillsService for project scope.
type projectInjectedSkillsService struct {
	c         *client
	projectID string
}

// userInjectedSkillsService implements InjectedSkillsService for user/me scope.
type userInjectedSkillsService struct {
	c *client
}

// ProjectInjectedSkills returns an InjectedSkillsService scoped to a project.
func (c *client) ProjectInjectedSkills(projectID string) InjectedSkillsService {
	return &projectInjectedSkillsService{c: c, projectID: projectID}
}

// UserInjectedSkills returns an InjectedSkillsService scoped to the current user.
func (c *client) UserInjectedSkills() InjectedSkillsService {
	return &userInjectedSkillsService{c: c}
}

// projectPath returns the API path prefix for this project's injected-skills.
func (s *projectInjectedSkillsService) basePath() string {
	return fmt.Sprintf("/api/v1/projects/%s/injected-skills", s.projectID)
}

// List returns the project-scoped injected-skills list.
func (s *projectInjectedSkillsService) List(ctx context.Context) (*api.SkillInjectionList, error) {
	resp, err := s.c.get(ctx, s.basePath(), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[api.SkillInjectionList](resp)
}

// Add adds a single skill entry to the project's injected-skills list.
func (s *projectInjectedSkillsService) Add(ctx context.Context, req *AddInjectedSkillRequest) (*api.SkillInjectionEntry, error) {
	resp, err := s.c.post(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[api.SkillInjectionEntry](resp)
}

// Set replaces the entire project injected-skills list atomically.
func (s *projectInjectedSkillsService) Set(ctx context.Context, entries []api.SkillInjectionEntry) (*api.SkillInjectionList, error) {
	body := setInjectedSkillsRequest{Entries: entries}
	resp, err := s.c.put(ctx, s.basePath(), body, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[api.SkillInjectionList](resp)
}

// Remove deletes a single entry from the project's injected-skills list.
func (s *projectInjectedSkillsService) Remove(ctx context.Context, entryID string) error {
	resp, err := s.c.delete(ctx, s.basePath()+"/"+url.PathEscape(entryID), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// userBasePath returns the API path prefix for user/me injected-skills.
func (s *userInjectedSkillsService) basePath() string {
	return "/api/v1/users/me/injected-skills"
}

// List returns the user-scoped injected-skills list.
func (s *userInjectedSkillsService) List(ctx context.Context) (*api.SkillInjectionList, error) {
	resp, err := s.c.get(ctx, s.basePath(), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[api.SkillInjectionList](resp)
}

// Add adds a single skill entry to the user's injected-skills list.
func (s *userInjectedSkillsService) Add(ctx context.Context, req *AddInjectedSkillRequest) (*api.SkillInjectionEntry, error) {
	resp, err := s.c.post(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[api.SkillInjectionEntry](resp)
}

// Set replaces the entire user injected-skills list atomically.
func (s *userInjectedSkillsService) Set(ctx context.Context, entries []api.SkillInjectionEntry) (*api.SkillInjectionList, error) {
	body := setInjectedSkillsRequest{Entries: entries}
	resp, err := s.c.put(ctx, s.basePath(), body, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[api.SkillInjectionList](resp)
}

// Remove deletes a single entry from the user's injected-skills list.
func (s *userInjectedSkillsService) Remove(ctx context.Context, entryID string) error {
	resp, err := s.c.delete(ctx, s.basePath()+"/"+url.PathEscape(entryID), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
