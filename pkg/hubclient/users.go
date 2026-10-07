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
	"net/http"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// UserService handles user operations.
type UserService interface {
	// List returns users matching opts.
	List(ctx context.Context, opts *ListUsersOptions) (*ListUsersResponse, error)

	// Get returns a user by ID.
	Get(ctx context.Context, userID string) (*User, error)

	// Update updates a user.
	Update(ctx context.Context, userID string, req *UpdateUserRequest) (*User, error)

	// Provision pre-registers a user (status invited) through
	// POST /api/v1/users. It returns Created=false on an identical replay.
	// A 409 or 422 response is returned as a *ProvisionError.
	Provision(ctx context.Context, req *ProvisionUserRequest) (*ProvisionUserResponse, error)
}

// userService is the implementation of UserService.
type userService struct {
	c *client
}

// ListUsersOptions configures user list filtering.
type ListUsersOptions struct {
	// Search matches against a user's email or display name (server-side
	// substring match, case-insensitive). Used to resolve a name/email to an
	// ID, e.g. for the CLI `--owner` flag (ptone/scion#2146).
	Search string
	Page   apiclient.PageOptions
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

// List returns users matching opts.
func (s *userService) List(ctx context.Context, opts *ListUsersOptions) (*ListUsersResponse, error) {
	var query url.Values
	if opts != nil {
		query = opts.Page.ToQuery(nil)
		if opts.Search != "" {
			query.Set("search", opts.Search)
		}
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/users", query, nil)
	if err != nil {
		return nil, err
	}

	type listResponse struct {
		Users      []User `json:"users"`
		NextCursor string `json:"nextCursor,omitempty"`
		TotalCount int    `json:"totalCount,omitempty"`
	}

	result, err := apiclient.DecodeRequired[listResponse](resp)
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
	return apiclient.DecodeRequired[User](resp)
}

// Update updates a user.
func (s *userService) Update(ctx context.Context, userID string, req *UpdateUserRequest) (*User, error) {
	resp, err := s.c.patch(ctx, "/api/v1/users/"+userID, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[User](resp)
}

// ProvisionUserRequest is the request for POST /api/v1/users. There is no
// role field: the role of a provisioned user is assigned at first sign-in.
type ProvisionUserRequest struct {
	Email       string  `json:"email"`
	DisplayName *string `json:"displayName,omitempty"`
	Note        *string `json:"note,omitempty"`
}

// ProvisionedUser is a provisioned (invited) user. A caller without
// user.read receives only Email and Status on a replay; every other field
// is then empty.
type ProvisionedUser struct {
	ID          string     `json:"id,omitempty"`
	Email       string     `json:"email"`
	Status      string     `json:"status"`
	DisplayName string     `json:"displayName,omitempty"`
	InvitedBy   string     `json:"invitedBy,omitempty"`
	InviteNote  *string    `json:"inviteNote,omitempty"`
	Created     *time.Time `json:"created,omitempty"`
}

// ProvisionUserResponse is the response of POST /api/v1/users.
type ProvisionUserResponse struct {
	User ProvisionedUser `json:"user"`
	// Created is false on an identical replay of an existing pending user.
	Created bool `json:"created"`
	// Warnings are advisory: the email may be refused at sign-in.
	Warnings []string `json:"warnings,omitempty"`
}

// details.reason values of a provisioning conflict (409) or refusal (422).
const (
	ProvisionReasonUserExists          = "user_exists"
	ProvisionReasonPendingUserExists   = "pending_user_exists"
	ProvisionReasonSuspendedUserExists = "user_suspended_exists"
	ProvisionReasonPrivilegedRole      = "privileged_role_not_provisionable"
	ProvisionReasonRoleNotSupported    = "role_selection_not_supported"
)

// ProvisionError is a 409 or 422 response of POST /api/v1/users.
type ProvisionError struct {
	*apiclient.APIError
	// Reason is details.reason. For a 409 it is one of user_exists,
	// pending_user_exists and user_suspended_exists; a caller without
	// user.read always gets user_exists.
	Reason string
	// UserID is details.userId, set only for pending_user_exists.
	UserID string
}

// Unwrap returns the underlying API error.
func (e *ProvisionError) Unwrap() error { return e.APIError }

// Provision pre-registers a user (status invited).
func (s *userService) Provision(ctx context.Context, req *ProvisionUserRequest) (*ProvisionUserResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/users", req, nil)
	if err != nil {
		return nil, err
	}
	out, err := apiclient.DecodeRequired[ProvisionUserResponse](resp)
	if err != nil {
		var apiErr *apiclient.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusConflict || apiErr.StatusCode == http.StatusUnprocessableEntity) {
			pe := &ProvisionError{APIError: apiErr}
			if apiErr.Details != nil {
				pe.Reason, _ = apiErr.Details["reason"].(string)
				pe.UserID, _ = apiErr.Details["userId"].(string)
			}
			return nil, pe
		}
		return nil, err
	}
	return out, nil
}
