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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// InviteService handles invite code operations.
type InviteService interface {
	Create(ctx context.Context, req *InviteCreateRequest) (*InviteCreateResponse, error)
	List(ctx context.Context, cursor string) (*InviteListResponse, error)
	Get(ctx context.Context, id string) (*InviteCode, error)
	Revoke(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
}

type inviteService struct {
	c *client
}

// InviteCode represents an invite code returned by the API.
type InviteCode struct {
	ID         string    `json:"id"`
	CodePrefix string    `json:"codePrefix"`
	MaxUses    int       `json:"maxUses"`
	UseCount   int       `json:"useCount"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Revoked    bool      `json:"revoked"`
	CreatedBy  string    `json:"createdBy"`
	Note       string    `json:"note"`
	Created    time.Time `json:"created"`
}

// InviteCreateRequest is the request for creating an invite code.
type InviteCreateRequest struct {
	ExpiresIn string `json:"expiresIn"`
	MaxUses   int    `json:"maxUses"`
	Note      string `json:"note"`
}

// InviteCreateResponse is the response from creating an invite code.
type InviteCreateResponse struct {
	Code      string      `json:"code"`
	InviteURL string      `json:"inviteUrl"`
	Invite    *InviteCode `json:"invite"`
}

// InviteListResponse is the response from listing invite codes.
type InviteListResponse struct {
	Items      []InviteCode `json:"items"`
	TotalCount int          `json:"totalCount"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

func (s *inviteService) Create(ctx context.Context, req *InviteCreateRequest) (*InviteCreateResponse, error) {
	resp, err := s.c.transport.Post(ctx, "/api/v1/admin/invites", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[InviteCreateResponse](resp)
}

func (s *inviteService) List(ctx context.Context, cursor string) (*InviteListResponse, error) {
	path := "/api/v1/admin/invites"
	if cursor != "" {
		path += "?cursor=" + url.QueryEscape(cursor)
	}
	resp, err := s.c.transport.Get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[InviteListResponse](resp)
}

func (s *inviteService) Get(ctx context.Context, id string) (*InviteCode, error) {
	resp, err := s.c.transport.Get(ctx, "/api/v1/admin/invites/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[InviteCode](resp)
}

func (s *inviteService) Revoke(ctx context.Context, id string) error {
	resp, err := s.c.transport.Post(ctx, "/api/v1/admin/invites/"+url.PathEscape(id)+"/revoke", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

func (s *inviteService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.transport.Delete(ctx, "/api/v1/admin/invites/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
