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

	// InviteBatch invites a list of addresses in one request through
	// POST /api/v1/admin/users/invite/bulk, optionally with project roles
	// for every invited address. The response has one result per input
	// entry. A 403 batch_projects_not_grantable response is returned as an
	// *InviteBatchError; nothing was created then.
	InviteBatch(ctx context.Context, req *InviteBatchRequest) (*InviteBatchResponse, error)

	// DeleteBlockers lists what blocks deleting a user, without deleting,
	// through GET /api/v1/users/{id}/delete-blockers. A plain user delete
	// is refused with 409 user_delete_blocked while either list is not
	// empty.
	DeleteBlockers(ctx context.Context, userID string) (*UserDeleteBlockers, error)

	// ForceDelete deletes a user whom agents or active delegations depend
	// on, through DELETE /api/v1/users/{id}?force=true (behind the
	// hub.user_delete_force_cascade experiment; while it is off the hub
	// ignores force and the request is a plain delete). It returns the deletion in progress
	// (202), or nil when nothing blocked and the user was deleted at once
	// (204). A repeated call while the deletion is in progress returns the
	// same deletion. A 403 user_delete_force_forbidden (agents the caller
	// may not delete) and a 409 user_delete_blockers_changed are returned
	// as API errors.
	ForceDelete(ctx context.Context, userID string) (*UserDeletion, error)

	// GetDeletion returns the forced deletion of a user through
	// GET /api/v1/users/{id}/deletion. It answers after the user is
	// deleted; a user with no deletion is a 404 API error.
	GetDeletion(ctx context.Context, userID string) (*UserDeletion, error)
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

// UserDeleteBlockers is the response of DeleteBlockers, and the same shape
// as the details of a 409 user_delete_blocked refusal. Each list holds at
// most 50 entries; Truncated reports that either has more.
type UserDeleteBlockers struct {
	Agents    []UserDeleteBlockingAgent `json:"agents"`
	Edges     []UserDeleteBlockingEdge  `json:"edges"`
	Truncated bool                      `json:"truncated"`
	// InProgress names a forced deletion of the user in progress, or is
	// nil (DeleteBlockers only).
	InProgress *UserDeletionRef `json:"inProgress,omitempty"`
}

// UserDeletionRef names a forced user deletion.
type UserDeletionRef struct {
	DeletionID string `json:"deletionId"`
	State      string `json:"state"`
}

// UserDeletion is a forced user deletion. State is stopping until every
// agent is stopped or removed, then completed (the user is deleted).
type UserDeletion struct {
	DeletionID       string                    `json:"deletionId"`
	UserID           string                    `json:"userId"`
	State            string                    `json:"state"`
	RequestedAt      time.Time                 `json:"requestedAt"`
	RequestedBy      UserDeletionPrincipal     `json:"requestedBy"`
	CompletedAt      *time.Time                `json:"completedAt"`
	LastReconciledAt *time.Time                `json:"lastReconciledAt"`
	LastError        string                    `json:"lastError"`
	Agents           []UserDeletionAgent       `json:"agents"`
	Counts           UserDeletionCounts        `json:"counts"`
	OrphanedByRevoke []UserDeletionRevokedEdge `json:"orphanedByRevoke"`
}

// UserDeletionPrincipal names who requested a deletion.
type UserDeletionPrincipal struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// UserDeletionAgent is one agent of a forced user deletion. Outcome is
// pending, stopping, failed, stopped (the delete engine confirmed the
// runtime teardown) or removed (the agent's record is gone without that
// confirmation).
type UserDeletionAgent struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	ProjectID string `json:"projectId"`
	Outcome   string `json:"outcome"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"lastError"`
}

// UserDeletionCounts counts a deletion's agents by outcome.
type UserDeletionCounts struct {
	Pending  int `json:"pending"`
	Stopping int `json:"stopping"`
	Failed   int `json:"failed"`
	Stopped  int `json:"stopped"`
	Removed  int `json:"removed"`
}

// UserDeletionRevokedEdge is a delegation the deletion revoked whose
// delegate agent it does not stop (another user's agent).
type UserDeletionRevokedEdge struct {
	AgentID string `json:"agentId"`
	EdgeID  string `json:"edgeId"`
}

// ForceDelete force-deletes a user.
func (s *userService) ForceDelete(ctx context.Context, userID string) (*UserDeletion, error) {
	resp, err := s.c.delete(ctx, "/api/v1/users/"+url.PathEscape(userID)+"?force=true", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[UserDeletion](resp)
}

// GetDeletion returns the forced deletion of a user.
func (s *userService) GetDeletion(ctx context.Context, userID string) (*UserDeletion, error) {
	resp, err := s.c.get(ctx, "/api/v1/users/"+url.PathEscape(userID)+"/deletion", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[UserDeletion](resp)
}

// UserDeleteBlockingAgent is an agent of the user that blocks the delete.
// DeletionState is "" when no delete was asked for the agent, otherwise
// deleting, finalizing or failed.
type UserDeleteBlockingAgent struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	ProjectID     string `json:"projectId"`
	DeletionState string `json:"deletionState"`
}

// UserDeleteBlockingEdge is an active delegation from the user that blocks
// the delete.
type UserDeleteBlockingEdge struct {
	ID           string `json:"id"`
	DelegateType string `json:"delegateType"`
	DelegateID   string `json:"delegateId"`
	ScopeType    string `json:"scopeType"`
	ScopeID      string `json:"scopeId"`
}

// DeleteBlockers lists what blocks deleting a user.
func (s *userService) DeleteBlockers(ctx context.Context, userID string) (*UserDeleteBlockers, error) {
	resp, err := s.c.get(ctx, "/api/v1/users/"+url.PathEscape(userID)+"/delete-blockers", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[UserDeleteBlockers](resp)
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

// InviteBatchRequest is the JSON request of
// POST /api/v1/admin/users/invite/bulk. There is no hub role field: the
// hub role of an invited user is assigned at first sign-in.
type InviteBatchRequest struct {
	Emails   []InviteBatchEmail   `json:"emails"`
	Projects []InviteBatchProject `json:"projects,omitempty"`
}

// InviteBatchEmail is one address of a batch invite, with an optional
// admin note.
type InviteBatchEmail struct {
	Email string `json:"email"`
	Note  string `json:"note,omitempty"`
}

// InviteBatchProject is a project and the project role definition IDs
// every created or already invited address of the batch receives.
type InviteBatchProject struct {
	ProjectID         string   `json:"projectId"`
	RoleDefinitionIDs []string `json:"roleDefinitionIds"`
}

// Address statuses of a batch invite result.
const (
	InviteBatchStatusCreated        = "created"
	InviteBatchStatusAlreadyInvited = "already_invited"
	InviteBatchStatusAlreadyActive  = "already_active"
	InviteBatchStatusSuspended      = "suspended"
	InviteBatchStatusExists         = "exists"
	InviteBatchStatusInvalid        = "invalid"
	InviteBatchStatusDuplicate      = "duplicate"
	InviteBatchStatusFailed         = "failed"
)

// Membership statuses of a batch invite result.
const (
	InviteBatchMembershipApplied       = "applied"
	InviteBatchMembershipAlreadyMember = "already_member"
	InviteBatchMembershipConflict      = "conflict"
	InviteBatchMembershipFailed        = "failed"
	InviteBatchMembershipSkipped       = "skipped"
)

// InviteBatchReasonProjectsNotGrantable is details.reason of a 403 batch
// invite refusal.
const InviteBatchReasonProjectsNotGrantable = "batch_projects_not_grantable"

// InviteBatchCounts counts the address statuses of a batch invite, plus
// the failed memberships. Exists is set only for a caller without
// user.read, whose results show "exists" for every existing user.
type InviteBatchCounts struct {
	Created          int `json:"created"`
	AlreadyInvited   int `json:"already_invited"`
	AlreadyActive    int `json:"already_active"`
	Suspended        int `json:"suspended"`
	Exists           int `json:"exists,omitempty"`
	Invalid          int `json:"invalid"`
	Duplicate        int `json:"duplicate"`
	Failed           int `json:"failed"`
	MembershipFailed int `json:"membership_failed"`
}

// InviteBatchMembership is the outcome of one requested project
// membership for one address.
type InviteBatchMembership struct {
	ProjectID                string   `json:"projectId"`
	Status                   string   `json:"status"`
	Code                     string   `json:"code,omitempty"`
	Cause                    string   `json:"cause,omitempty"`
	CurrentRoleDefinitionIDs []string `json:"currentRoleDefinitionIds,omitempty"`
}

// InviteBatchResult is the outcome for one input entry, in input order.
type InviteBatchResult struct {
	Index       int                     `json:"index"`
	Email       string                  `json:"email,omitempty"`
	Input       string                  `json:"input,omitempty"`
	Status      string                  `json:"status"`
	Reason      string                  `json:"reason,omitempty"`
	UserID      string                  `json:"userId,omitempty"`
	DuplicateOf *int                    `json:"duplicateOf,omitempty"`
	Memberships []InviteBatchMembership `json:"memberships,omitempty"`
}

// InviteBatchResponse is the response of a batch invite. Complete is false
// when any address or membership failed or was not processed; running the
// same request again retries them.
type InviteBatchResponse struct {
	BatchID  string              `json:"batchId"`
	Invited  int                 `json:"invited"`
	Skipped  int                 `json:"skipped"`
	Total    int                 `json:"total"`
	Complete bool                `json:"complete"`
	Counts   InviteBatchCounts   `json:"counts"`
	Results  []InviteBatchResult `json:"results"`
}

// InviteBatchProjectRefusal is one refused project entry, or one refused
// role in it, of a 403 batch_projects_not_grantable response.
type InviteBatchProjectRefusal struct {
	Index            int                    `json:"index"`
	ProjectID        string                 `json:"projectId"`
	Reason           string                 `json:"reason"`
	RoleDefinitionID string                 `json:"roleDefinitionId,omitempty"`
	DenialCode       string                 `json:"denialCode,omitempty"`
	Details          map[string]interface{} `json:"details,omitempty"`
}

// InviteBatchError is a 403 batch_projects_not_grantable response: the
// caller cannot grant one or more requested project memberships, and
// nothing was created.
type InviteBatchError struct {
	*apiclient.APIError
	// BatchID is details.batchId; the hub's audit record carries it too.
	BatchID string
	// Projects lists every refused entry.
	Projects []InviteBatchProjectRefusal
}

// Unwrap returns the underlying API error.
func (e *InviteBatchError) Unwrap() error { return e.APIError }

// InviteBatch invites a list of addresses in one request.
func (s *userService) InviteBatch(ctx context.Context, req *InviteBatchRequest) (*InviteBatchResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/admin/users/invite/bulk", req, nil)
	if err != nil {
		return nil, err
	}
	out, err := apiclient.DecodeRequired[InviteBatchResponse](resp)
	if err != nil {
		var apiErr *apiclient.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden && apiErr.Details != nil {
			if reason, _ := apiErr.Details["reason"].(string); reason == InviteBatchReasonProjectsNotGrantable {
				be := &InviteBatchError{APIError: apiErr}
				be.BatchID, _ = apiErr.Details["batchId"].(string)
				if raw, ok := apiErr.Details["projects"]; ok {
					if b, mErr := json.Marshal(raw); mErr == nil {
						_ = json.Unmarshal(b, &be.Projects)
					}
				}
				return nil, be
			}
		}
		return nil, err
	}
	return out, nil
}
