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

// Test identities with admin roles on test-designated hubs
// (ptone/scion#4240, admin tier phase A4): issuance, revoke-all and the
// residue report. The hub answers 404 to every route unless it started
// with --test-mode=admin or above.

// IssueTestAdminIdentityRequest is the request for POST
// /api/v1/test-identities/admin.
type IssueTestAdminIdentityRequest struct {
	// AdminRole is "hub-admin" or "super-admin".
	AdminRole string `json:"adminRole"`
	// Purpose is required by the hub, 8 to 200 characters.
	Purpose string `json:"purpose"`
	// LifetimeSeconds is the identity lifetime; 0 means the kind's
	// default.
	LifetimeSeconds int64 `json:"lifetimeSeconds,omitempty"`
	// TokenTTLSeconds is the access token lifetime; 0 means the kind's
	// default. The token never outlives the identity.
	TokenTTLSeconds int64 `json:"tokenTtlSeconds,omitempty"`
}

// TestAdminIdentity is a test identity with an admin role as the hub
// reports it. It never carries a token.
type TestAdminIdentity struct {
	TestIdentity
	// Kind is "test_hub_admin" or "test_super_admin".
	Kind string `json:"kind"`
	// AdminRole is "hub-admin" or "super-admin".
	AdminRole string `json:"adminRole"`
	// IssuanceID identifies the issuance in audit records and the residue
	// report.
	IssuanceID string `json:"issuanceId"`
}

// TestAdminIdentityTokenResponse is the response of IssueAdmin.
// AccessToken is a secret: write it only where the caller asked for it,
// never to logs, standard output or standard error.
type TestAdminIdentityTokenResponse struct {
	Identity       TestAdminIdentity `json:"identity"`
	AccessToken    string            `json:"accessToken"`
	TokenType      string            `json:"tokenType"`
	ExpiresIn      int64             `json:"expiresIn"`
	TokenExpiresAt time.Time         `json:"tokenExpiresAt"`
}

// RevokedTestAdminIdentity is one entry of the revoke-all response.
type RevokedTestAdminIdentity struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Outcome is "deleted", or "suspended" when the identity owns agents
	// or is a project's last owner.
	Outcome string `json:"outcome"`
}

// RevokeAllTestAdminIdentitiesResponse is the response of RevokeAllAdmin.
type RevokeAllTestAdminIdentitiesResponse struct {
	Revoked []RevokedTestAdminIdentity `json:"revoked"`
	Count   int                        `json:"count"`
}

// TestAdminResidueGrant is a grant row stamped with the identity's
// issuance: a role binding, group membership, custom role, invite or join
// token.
type TestAdminResidueGrant struct {
	Kind                 string     `json:"kind"`
	ID                   string     `json:"id"`
	RoleDefinitionID     string     `json:"roleDefinitionId,omitempty"`
	PrincipalType        string     `json:"principalType,omitempty"`
	PrincipalID          string     `json:"principalId,omitempty"`
	ScopeType            string     `json:"scopeType,omitempty"`
	ScopeID              string     `json:"scopeId,omitempty"`
	GroupID              string     `json:"groupId,omitempty"`
	Name                 string     `json:"name,omitempty"`
	ExpiresAt            *time.Time `json:"expiresAt,omitempty"`
	BoundOutsideIssuance bool       `json:"boundOutsideIssuance,omitempty"`
}

// TestAdminResidueResources lists the IDs of resources stamped with the
// identity as their provenance root.
type TestAdminResidueResources struct {
	Agents    []string `json:"agents"`
	Schedules []string `json:"schedules"`
	Groups    []string `json:"groups"`
	Projects  []string `json:"projects"`
}

// TestAdminResidueChange is one mutation the identity (or an agent rooted
// at it) made to a row without its stamp. The hub reports these and never
// reverts them.
type TestAdminResidueChange struct {
	MutationType string    `json:"mutationType"`
	TargetType   string    `json:"targetType"`
	TargetID     string    `json:"targetId"`
	ActorKind    string    `json:"actorKind"`
	ActorID      string    `json:"actorId"`
	Timestamp    time.Time `json:"timestamp"`
}

// TestAdminResidueReport is the response of AdminResidue. It holds IDs and
// audit facts only, no secrets.
type TestAdminResidueReport struct {
	IdentityID       string                    `json:"identityId"`
	IssuanceID       string                    `json:"issuanceId"`
	StampedGrants    []TestAdminResidueGrant   `json:"stampedGrants"`
	StampedResources TestAdminResidueResources `json:"stampedResources"`
	UnstampedChanges []TestAdminResidueChange  `json:"unstampedChanges"`
	// KeptRoles lists custom roles stamped with the issuance that a role
	// binding without the stamp names; the purge keeps them.
	KeptRoles []TestAdminResidueGrant `json:"keptRoles"`
	// Truncated is true when a list hit its bound.
	Truncated bool `json:"truncated,omitempty"`
	// Limit states what the report can see.
	Limit string `json:"limit"`
}

// IssueAdmin creates a test identity with an admin role. It is sent once,
// never retried, since a retry could create a second identity.
func (s *testIdentityService) IssueAdmin(ctx context.Context, req *IssueTestAdminIdentityRequest) (*TestAdminIdentityTokenResponse, error) {
	if req == nil || req.AdminRole == "" {
		return nil, fmt.Errorf("admin role is required")
	}
	resp, err := s.c.postNoRetry(ctx, "/api/v1/test-identities/admin", req, nil)
	if err != nil {
		return nil, err
	}
	out, err := apiclient.DecodeRequired[TestAdminIdentityTokenResponse](resp)
	if err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("hub returned no access token")
	}
	return out, nil
}

// RevokeAllAdmin revokes every test identity with an admin role. It is
// sent once, never retried, and carries no body.
func (s *testIdentityService) RevokeAllAdmin(ctx context.Context) (*RevokeAllTestAdminIdentitiesResponse, error) {
	resp, err := s.c.postNoRetry(ctx, "/api/v1/test-identities/admin/revoke-all", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[RevokeAllTestAdminIdentitiesResponse](resp)
}

// AdminResidue returns the residue report of one test identity with an
// admin role.
func (s *testIdentityService) AdminResidue(ctx context.Context, id string) (*TestAdminResidueReport, error) {
	if id == "" {
		return nil, fmt.Errorf("test identity id is required")
	}
	resp, err := s.c.get(ctx, "/api/v1/test-identities/admin/"+url.PathEscape(id)+"/residue", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[TestAdminResidueReport](resp)
}
