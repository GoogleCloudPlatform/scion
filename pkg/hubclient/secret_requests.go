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
// one canonical JSON contract for secret requests (ptone/scion#8747).
type (
	SecretRequestInput             = messages.SecretRequestInput
	SecretRequestSendResult        = messages.SecretRequestSendResult
	SecretRequestReplaced          = messages.SecretRequestReplaced
	SecretRequestReplacedBy        = messages.SecretRequestReplacedBy
	SecretRequestFulfilled         = messages.SecretRequestFulfilled
	SecretRequestWithdrawn         = messages.SecretRequestWithdrawn
	SecretRequestDismissed         = messages.SecretRequestDismissed
	SecretRequestExpired           = messages.SecretRequestExpired
	SecretRequestView              = messages.SecretRequestView
	WithdrawSecretRequestsRequest  = messages.WithdrawSecretRequestsRequest
	WithdrawSecretRequestsResponse = messages.WithdrawSecretRequestsResponse
	AgentSecretRequestListResponse = messages.AgentSecretRequestListResponse
)

// ListAgentSecretRequestsOptions configures GET /api/v1/agent/secret-requests.
type ListAgentSecretRequestsOptions struct {
	State  string // pending (default), fulfilled, withdrawn, dismissed, expired, or all
	Cursor string
	Limit  int
}

// ListRequests returns one page of secret requests belonging to the calling agent.
func (s *secretService) ListRequests(ctx context.Context, opts *ListAgentSecretRequestsOptions) (*AgentSecretRequestListResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.State != "" {
			query.Set("state", opts.State)
		}
		if opts.Cursor != "" {
			query.Set("cursor", opts.Cursor)
		}
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
	}
	resp, err := s.c.getWithQuery(ctx, "/api/v1/agent/secret-requests", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[AgentSecretRequestListResponse](resp)
}

// WithdrawRequests withdraws pending secret requests belonging to the calling agent.
func (s *secretService) WithdrawRequests(ctx context.Context, req *WithdrawSecretRequestsRequest) (*WithdrawSecretRequestsResponse, error) {
	if req == nil {
		req = &WithdrawSecretRequestsRequest{}
	}
	resp, err := s.c.post(ctx, "/api/v1/agent/secret-requests/withdraw", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[WithdrawSecretRequestsResponse](resp)
}
