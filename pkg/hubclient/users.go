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

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// UserService handles user operations.
type UserService interface {
	// List returns users.
	List(ctx context.Context, opts *apiclient.PageOptions) (*ListUsersResponse, error)

	// Get returns a user by ID.
	Get(ctx context.Context, userID string) (*User, error)

	// Update updates a user.
	Update(ctx context.Context, userID string, req *UpdateUserRequest) (*User, error)
}

// userService is the implementation of UserService.
type userService struct {
	c *client
}

// ListUsersResponse is the response from listing users.
type ListUsersResponse struct {
	Users []User
	Page  apiclient.PageResult
}

// UpdateUserRequest is the request for updating a user.
type UpdateUserRequest struct {
	DisplayName string           `json:"displayName,omitempty"`
	Preferences *UserPreferences `json:"preferences,omitempty"`
}

// List returns users.
func (s *userService) List(ctx context.Context, opts *apiclient.PageOptions) (*ListUsersResponse, error) {
	query := opts.ToQuery(nil)

	resp, err := s.c.getWithQuery(ctx, "/api/v1/users", query, nil)
	if err != nil {
		return nil, err
	}

	type listResponse struct {
		Users      []User `json:"users"`
		NextCursor string `json:"nextCursor,omitempty"`
		TotalCount int    `json:"totalCount,omitempty"`
	}

	result, err := apiclient.DecodeResponse[listResponse](resp)
	if err != nil {
		return nil, err
	}

	return &ListUsersResponse{
		Users: result.Users,
		Page: apiclient.PageResult{
			NextCursor: result.NextCursor,
			TotalCount: result.TotalCount,
		},
	}, nil
}

// Get returns a user by ID.
func (s *userService) Get(ctx context.Context, userID string) (*User, error) {
	resp, err := s.c.get(ctx, "/api/v1/users/"+userID, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[User](resp)
}

// Update updates a user.
func (s *userService) Update(ctx context.Context, userID string, req *UpdateUserRequest) (*User, error) {
	resp, err := s.c.patch(ctx, "/api/v1/users/"+userID, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[User](resp)
}
