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
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// Wire type aliases shared with pkg/messages so pkg/hub and pkg/hubclient share
// one canonical JSON contract for command requests (ptone/scion#9278).
type (
	CommandRequestInput             = messages.CommandRequestInput
	CommandRequestSendResult        = messages.CommandRequestSendResult
	CommandRequestView              = messages.CommandRequestView
	CommandRequestRunView           = messages.CommandRequestRunView
	CommandRequestExecContainerView = messages.CommandRequestExecContainerView
	CommandRequestDetailResponse    = messages.CommandRequestDetailResponse
	ApproveCommandRequestBody       = messages.ApproveCommandRequestBody
	ApproveCommandRequestResponse   = messages.ApproveCommandRequestResponse
	AgentCommandRequestListResponse = messages.AgentCommandRequestListResponse
)

// ListAgentCommandRequestsOptions configures GET /api/v1/agent/command-requests.
type ListAgentCommandRequestsOptions struct {
	Key    string
	Cursor string
	Limit  int
}

// CommandRequestService provides operations on human-approved command requests.
type CommandRequestService interface {
	// Get retrieves a command request and its run view (GET /api/v1/command-requests/{id}).
	Get(ctx context.Context, id string) (*CommandRequestDetailResponse, error)
	// Approve approves a pending command request (POST /api/v1/command-requests/{id}/approve).
	Approve(ctx context.Context, id string, req *ApproveCommandRequestBody) (*ApproveCommandRequestResponse, error)
	// List lists command requests created by the calling agent (GET /api/v1/agent/command-requests).
	List(ctx context.Context, opts *ListAgentCommandRequestsOptions) (*AgentCommandRequestListResponse, error)
}

type commandRequestService struct {
	c *client
}

// Get implements CommandRequestService.
func (s *commandRequestService) Get(ctx context.Context, id string) (*CommandRequestDetailResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/command-requests/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[CommandRequestDetailResponse](resp)
}

// Approve implements CommandRequestService.
func (s *commandRequestService) Approve(ctx context.Context, id string, req *ApproveCommandRequestBody) (*ApproveCommandRequestResponse, error) {
	if req == nil {
		req = &ApproveCommandRequestBody{}
	}
	resp, err := s.c.post(ctx, "/api/v1/command-requests/"+url.PathEscape(id)+"/approve", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ApproveCommandRequestResponse](resp)
}

// List implements CommandRequestService.
func (s *commandRequestService) List(ctx context.Context, opts *ListAgentCommandRequestsOptions) (*AgentCommandRequestListResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Key != "" {
			query.Set("key", opts.Key)
		}
		if opts.Cursor != "" {
			query.Set("cursor", opts.Cursor)
		}
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
	}
	resp, err := s.c.getWithQuery(ctx, "/api/v1/agent/command-requests", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[AgentCommandRequestListResponse](resp)
}
