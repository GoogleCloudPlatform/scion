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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// ProjectVisitorService handles a project's visitor grants: access for
// users of a trusted issuer, granted by email (ptone/scion#4603). The
// routes need an interactive session and project.manage_visitors, and
// answer 404 while the hub's federation.project_visitors experiment is off.
type ProjectVisitorService interface {
	// List returns the project's visitor grants.
	List(ctx context.Context) ([]ProjectVisitor, error)

	// Add grants visitor access to the user of an issuer with an email.
	Add(ctx context.Context, req *AddProjectVisitorRequest) (*ProjectVisitor, error)

	// Update replaces a grant's window; a nil bound is cleared.
	Update(ctx context.Context, visitorID string, req *UpdateProjectVisitorRequest) (*ProjectVisitor, error)

	// Remove revokes a grant.
	Remove(ctx context.Context, visitorID string) error
}

// ProjectVisitor is one visitor grant of a project.
type ProjectVisitor struct {
	// ID is the visitor's account ID on the hub; Update and Remove take it.
	ID        string     `json:"id"`
	BindingID string     `json:"bindingId"`
	Email     string     `json:"email"`
	Issuer    string     `json:"issuer"`
	Status    string     `json:"status"` // pending, active or suspended
	GrantedBy string     `json:"grantedBy,omitempty"`
	NotBefore *time.Time `json:"notBefore,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// ListProjectVisitorsResponse is the list response body.
type ListProjectVisitorsResponse struct {
	Visitors []ProjectVisitor `json:"visitors"`
}

// AddProjectVisitorRequest is the request body for granting a visitor.
type AddProjectVisitorRequest struct {
	Issuer    string     `json:"issuer"`
	Email     string     `json:"email"`
	NotBefore *time.Time `json:"notBefore,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// UpdateProjectVisitorRequest is the request body for replacing a grant's
// window.
type UpdateProjectVisitorRequest struct {
	NotBefore *time.Time `json:"notBefore"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// projectVisitorService is the concrete implementation.
type projectVisitorService struct {
	c         *client
	projectID string
}

// ProjectVisitors returns a ProjectVisitorService scoped to a project.
func (c *client) ProjectVisitors(projectID string) ProjectVisitorService {
	return &projectVisitorService{c: c, projectID: projectID}
}

func (s *projectVisitorService) basePath() string {
	return fmt.Sprintf("/api/v1/projects/%s/visitors", url.PathEscape(s.projectID))
}

func (s *projectVisitorService) List(ctx context.Context) ([]ProjectVisitor, error) {
	resp, err := s.c.get(ctx, s.basePath(), nil)
	if err != nil {
		return nil, err
	}
	out, err := apiclient.DecodeRequired[ListProjectVisitorsResponse](resp)
	if err != nil {
		return nil, err
	}
	return out.Visitors, nil
}

func (s *projectVisitorService) Add(ctx context.Context, req *AddProjectVisitorRequest) (*ProjectVisitor, error) {
	// Not retried: a repeated grant answers 409.
	resp, err := s.c.postNoRetry(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ProjectVisitor](resp)
}

func (s *projectVisitorService) Update(ctx context.Context, visitorID string, req *UpdateProjectVisitorRequest) (*ProjectVisitor, error) {
	resp, err := s.c.patch(ctx, s.basePath()+"/"+url.PathEscape(visitorID), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ProjectVisitor](resp)
}

func (s *projectVisitorService) Remove(ctx context.Context, visitorID string) error {
	resp, err := s.c.delete(ctx, s.basePath()+"/"+url.PathEscape(visitorID), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
