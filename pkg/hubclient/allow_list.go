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

// AllowListService handles allow list operations.
type AllowListService interface {
	List(ctx context.Context, cursor string) (*AllowListResponse, error)
	Add(ctx context.Context, email, note string) (*AllowListEntry, error)
	Remove(ctx context.Context, email string) error
	BulkAdd(ctx context.Context, emails []AllowListAddRequest) (*AllowListBulkAddResponse, error)
	ListDomains(ctx context.Context) ([]string, error)
}

type allowListService struct {
	c *client
}

// AllowListEntry represents an email on the allow list, optionally enriched with invite details.
type AllowListEntry struct {
	ID               string    `json:"id"`
	Email            string    `json:"email"`
	Note             string    `json:"note"`
	AddedBy          string    `json:"addedBy"`
	InviteID         string    `json:"inviteId,omitempty"`
	Created          time.Time `json:"created"`
	InviteCodePrefix string    `json:"inviteCodePrefix,omitempty"`
	InviteMaxUses    int       `json:"inviteMaxUses,omitempty"`
	InviteUseCount   int       `json:"inviteUseCount,omitempty"`
	InviteExpiresAt  time.Time `json:"inviteExpiresAt,omitempty"`
	InviteRevoked    bool      `json:"inviteRevoked,omitempty"`
}

// AllowListResponse is the response from listing allow list entries.
type AllowListResponse struct {
	Items      []AllowListEntry `json:"items"`
	TotalCount int              `json:"totalCount"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// AllowListAddRequest is used when adding an email to the allow list.
type AllowListAddRequest struct {
	Email string `json:"email"`
	Note  string `json:"note"`
}

// AllowListBulkAddRequest is the request for bulk importing emails.
type AllowListBulkAddRequest struct {
	Emails []AllowListAddRequest `json:"emails"`
}

// AllowListBulkAddResponse is the response from bulk importing emails.
type AllowListBulkAddResponse struct {
	Added   int `json:"added"`
	Skipped int `json:"skipped"`
	Total   int `json:"total"`
}

// AllowListDomainsResponse is the response from listing email domains.
type AllowListDomainsResponse struct {
	Domains []string `json:"domains"`
}

func (s *allowListService) List(ctx context.Context, cursor string) (*AllowListResponse, error) {
	path := "/api/v1/admin/allow-list"
	if cursor != "" {
		path += "?cursor=" + url.QueryEscape(cursor)
	}
	resp, err := s.c.transport.Get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[AllowListResponse](resp)
}

func (s *allowListService) Add(ctx context.Context, email, note string) (*AllowListEntry, error) {
	req := &AllowListAddRequest{Email: email, Note: note}
	resp, err := s.c.transport.Post(ctx, "/api/v1/admin/allow-list", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[AllowListEntry](resp)
}

func (s *allowListService) Remove(ctx context.Context, email string) error {
	resp, err := s.c.transport.Delete(ctx, "/api/v1/admin/allow-list/"+url.PathEscape(email), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

func (s *allowListService) BulkAdd(ctx context.Context, emails []AllowListAddRequest) (*AllowListBulkAddResponse, error) {
	req := &AllowListBulkAddRequest{Emails: emails}
	resp, err := s.c.transport.Post(ctx, "/api/v1/admin/allow-list/import", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[AllowListBulkAddResponse](resp)
}

func (s *allowListService) ListDomains(ctx context.Context) ([]string, error) {
	resp, err := s.c.transport.Get(ctx, "/api/v1/admin/allow-list/domains", nil)
	if err != nil {
		return nil, err
	}
	result, err := apiclient.DecodeResponse[AllowListDomainsResponse](resp)
	if err != nil {
		return nil, err
	}
	return result.Domains, nil
}
