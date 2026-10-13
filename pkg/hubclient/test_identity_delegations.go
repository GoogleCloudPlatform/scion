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

// TestIdentityDelegationService manages workload issuer delegations
// (/api/v1/test-identity-delegations, ptone/scion#4240). Every route takes
// a human session credential only: an access token, an agent or a
// workload credential is refused by the hub.
type TestIdentityDelegationService interface {
	// Create creates a delegation. It is sent once, never retried.
	Create(ctx context.Context, req *CreateTestIdentityDelegationRequest) (*TestIdentityDelegation, error)
	// List lists the caller's delegations (every delegation for an admin).
	List(ctx context.Context) (*ListTestIdentityDelegationsResponse, error)
	// Revoke revokes a delegation.
	Revoke(ctx context.Context, id string) error
}

// CreateTestIdentityDelegationRequest is the body of Create.
type CreateTestIdentityDelegationRequest struct {
	// WorkloadSub is the service account unique id the delegation is
	// pinned to; it must be in the hub's startup allowlist.
	WorkloadSub string `json:"workloadSub"`
	// Scopes holds exactly one scope, "test_identity:issue".
	Scopes []string `json:"scopes"`
	// TTLSeconds is the delegation lifetime (0: the hub default).
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`
	// MemberCap is the number of live identities the delegation may hold
	// (0: the hub default, 5).
	MemberCap int `json:"memberCap,omitempty"`
	// Label is a short label for the test window.
	Label string `json:"label,omitempty"`
}

// TestIdentityDelegation is a delegation as the hub returns it. No field
// is a secret: the id names the delegation but is not a credential.
type TestIdentityDelegation struct {
	ID          string     `json:"id"`
	GrantorID   string     `json:"grantorId"`
	WorkloadSub string     `json:"workloadSub"`
	Scope       string     `json:"scope"`
	MemberCap   int        `json:"memberCap"`
	Label       string     `json:"label,omitempty"`
	Created     time.Time  `json:"created"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
	RevokedBy   string     `json:"revokedBy,omitempty"`
	Live        bool       `json:"live"`
}

// ListTestIdentityDelegationsResponse is the response of List.
type ListTestIdentityDelegationsResponse struct {
	Items     []TestIdentityDelegation `json:"items"`
	Truncated bool                     `json:"truncated"`
}

type testIdentityDelegationService struct {
	c *client
}

func (s *testIdentityDelegationService) Create(ctx context.Context, req *CreateTestIdentityDelegationRequest) (*TestIdentityDelegation, error) {
	if req == nil {
		return nil, fmt.Errorf("a delegation request is required")
	}
	resp, err := s.c.postNoRetry(ctx, "/api/v1/test-identity-delegations", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[TestIdentityDelegation](resp)
}

func (s *testIdentityDelegationService) List(ctx context.Context) (*ListTestIdentityDelegationsResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/test-identity-delegations", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListTestIdentityDelegationsResponse](resp)
}

func (s *testIdentityDelegationService) Revoke(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("delegation id is required")
	}
	resp, err := s.c.delete(ctx, "/api/v1/test-identity-delegations/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
