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

// AgentDelegationService manages agent delegation grants
// (.design/agent-delegation.md §7, §18.6). Grant issuance, list, read,
// revoke, credential revoke and revoke-all take a user session; the hub
// refuses an access token, an agent token on the user routes, and a
// delegated credential. Exchange takes the bound agent's own agent token.
// No method returns a credential except Exchange, which returns it exactly
// as the hub sends it, once.
type AgentDelegationService interface {
	// Create issues a grant for agentID. It is sent once, never retried.
	Create(ctx context.Context, agentID string, req *CreateAgentDelegationRequest) (*AgentDelegationGrant, error)
	// List lists the agent's grants that the caller may see.
	List(ctx context.Context, agentID string) (*ListAgentDelegationsResponse, error)
	// ListMine lists the grants the caller issued.
	ListMine(ctx context.Context) (*ListAgentDelegationsResponse, error)
	// Get reads one grant of the agent.
	Get(ctx context.Context, agentID, grantID string) (*AgentDelegationGrant, error)
	// Revoke revokes a grant and every credential exchanged from it.
	Revoke(ctx context.Context, agentID, grantID string) error
	// RevokeCredential revokes one delegated credential of a grant.
	RevokeCredential(ctx context.Context, agentID, grantID, credentialID string) error
	// RevokeAll revokes every unrevoked grant of the agent the caller may
	// revoke this way, and reports the ones it skipped. It is sent once.
	RevokeAll(ctx context.Context, agentID string) (*RevokeAllAgentDelegationsResponse, error)
	// RevokeAllMine revokes every unrevoked grant the caller issued. It is
	// sent once.
	RevokeAllMine(ctx context.Context) (*RevokeAllAgentDelegationsResponse, error)
	// Exchange exchanges a grant for a short-lived delegated credential,
	// with the bound agent's own agent token. It is sent once, never
	// retried.
	Exchange(ctx context.Context, agentID, grantID string, req *ExchangeAgentDelegationRequest) (*DelegatedAgentCredential, error)
}

// AgentDelegationBoundary bounds a grant to a project or to the hub. Kind is
// "project" (ProjectID set) or "hub".
type AgentDelegationBoundary struct {
	Kind      string `json:"kind"`
	ProjectID string `json:"projectId,omitempty"`
}

// CreateAgentDelegationRequest is the body of Create.
type CreateAgentDelegationRequest struct {
	Boundary AgentDelegationBoundary `json:"boundary"`
	// Permissions are token selectors, for example "agent:read".
	Permissions []string `json:"permissions"`
	// ExpiresAt is the grant expiry (nil: the hub default, 7 days; at most
	// 30 days).
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Name, Purpose and Labels are descriptive, issuer-supplied text.
	Name    string            `json:"name"`
	Purpose string            `json:"purpose,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	// MaxCredentialTTLSeconds caps each delegated credential (nil: the hub
	// default, 15 minutes; at most 3600).
	MaxCredentialTTLSeconds *int `json:"maxCredentialTtlSeconds,omitempty"`
}

// AgentDelegatedCredentialSummary names one active delegated credential of
// a grant. It is never the credential.
type AgentDelegatedCredentialSummary struct {
	ID        string    `json:"id"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// AgentDelegationGrant is grant metadata as the hub returns it. It carries
// no credential, hash or prefix. Name, Purpose and Labels are
// issuer-supplied text (UserSuppliedFields), never an identity.
type AgentDelegationGrant struct {
	ID                      string                            `json:"id"`
	AgentID                 string                            `json:"agentId"`
	IssuerUserID            string                            `json:"issuerUserId"`
	Boundary                AgentDelegationBoundary           `json:"boundary"`
	Permissions             []string                          `json:"permissions"`
	CeilingVersion          int                               `json:"ceilingVersion"`
	Name                    string                            `json:"name"`
	Purpose                 string                            `json:"purpose,omitempty"`
	Labels                  map[string]string                 `json:"labels,omitempty"`
	ExpiresAt               time.Time                         `json:"expiresAt"`
	MaxCredentialTTLSeconds int                               `json:"maxCredentialTtlSeconds"`
	Created                 time.Time                         `json:"created"`
	LastExchangedAt         *time.Time                        `json:"lastExchangedAt,omitempty"`
	Status                  string                            `json:"status"`
	UserSuppliedFields      []string                          `json:"userSuppliedFields"`
	RevokedAt               *time.Time                        `json:"revokedAt,omitempty"`
	RevokedBy               string                            `json:"revokedBy,omitempty"`
	RevokeReason            string                            `json:"revokeReason,omitempty"`
	ActiveCredentialCount   int                               `json:"activeCredentialCount"`
	ActiveCredentials       []AgentDelegatedCredentialSummary `json:"activeCredentials"`
}

// ListAgentDelegationsResponse is the response of List and ListMine. The
// lists are not paginated.
type ListAgentDelegationsResponse struct {
	Delegations []AgentDelegationGrant `json:"delegations"`
}

// AgentDelegationRevokeAllSkip is a grant RevokeAll left unrevoked: the
// caller may read it but holds no revoke-all authority over it.
type AgentDelegationRevokeAllSkip struct {
	GrantID string `json:"grantId"`
	AgentID string `json:"agentId"`
	Reason  string `json:"reason"`
}

// RevokeAllAgentDelegationsResponse is the response of RevokeAll and
// RevokeAllMine.
type RevokeAllAgentDelegationsResponse struct {
	Revoked []string                       `json:"revoked"`
	Skipped []AgentDelegationRevokeAllSkip `json:"skipped"`
}

// ExchangeAgentDelegationRequest is the body of Exchange.
type ExchangeAgentDelegationRequest struct {
	// Audience is the hub's delegation audience ("scion-hub:<hub id>").
	Audience string `json:"audience"`
	// Permissions narrows the credential to a subset of the grant (empty:
	// the whole grant), as canonical permission IDs or selectors.
	Permissions []string `json:"permissions,omitempty"`
	// TTLSeconds asks for a shorter lifetime (nil: the grant's maximum).
	TTLSeconds *int `json:"ttlSeconds,omitempty"`
}

// DelegatedAgentCredential is the response of Exchange, the only response
// that carries a delegated credential. String and GoString leave the token
// out, so formatting the value never prints it.
type DelegatedAgentCredential struct {
	Token        string    `json:"token"`
	ExpiresAt    time.Time `json:"expiresAt"`
	GrantID      string    `json:"grantId"`
	CredentialID string    `json:"credentialId"`
	Audience     string    `json:"audience"`
	Permissions  []string  `json:"permissions"`
}

// String describes the credential without its token.
func (c DelegatedAgentCredential) String() string {
	return fmt.Sprintf("DelegatedAgentCredential{credential=%s grant=%s expires=%s}", c.CredentialID, c.GrantID, c.ExpiresAt.UTC().Format(time.RFC3339))
}

// GoString describes the credential without its token.
func (c DelegatedAgentCredential) GoString() string { return c.String() }

type agentDelegationService struct {
	c *client
}

func agentDelegationsPath(agentID string) string {
	return "/api/v1/agents/" + url.PathEscape(agentID) + "/delegations"
}

func agentDelegationPath(agentID, grantID string) string {
	return agentDelegationsPath(agentID) + "/" + url.PathEscape(grantID)
}

func requireIDs(ids ...[2]string) error {
	for _, id := range ids {
		if id[1] == "" {
			return fmt.Errorf("%s is required", id[0])
		}
	}
	return nil
}

func (s *agentDelegationService) Create(ctx context.Context, agentID string, req *CreateAgentDelegationRequest) (*AgentDelegationGrant, error) {
	if err := requireIDs([2]string{"agent id", agentID}); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("a delegation request is required")
	}
	resp, err := s.c.postNoRetry(ctx, agentDelegationsPath(agentID), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[AgentDelegationGrant](resp)
}

func (s *agentDelegationService) List(ctx context.Context, agentID string) (*ListAgentDelegationsResponse, error) {
	if err := requireIDs([2]string{"agent id", agentID}); err != nil {
		return nil, err
	}
	resp, err := s.c.get(ctx, agentDelegationsPath(agentID), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListAgentDelegationsResponse](resp)
}

func (s *agentDelegationService) ListMine(ctx context.Context) (*ListAgentDelegationsResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/users/me/delegations", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListAgentDelegationsResponse](resp)
}

func (s *agentDelegationService) Get(ctx context.Context, agentID, grantID string) (*AgentDelegationGrant, error) {
	if err := requireIDs([2]string{"agent id", agentID}, [2]string{"grant id", grantID}); err != nil {
		return nil, err
	}
	resp, err := s.c.get(ctx, agentDelegationPath(agentID, grantID), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[AgentDelegationGrant](resp)
}

func (s *agentDelegationService) Revoke(ctx context.Context, agentID, grantID string) error {
	if err := requireIDs([2]string{"agent id", agentID}, [2]string{"grant id", grantID}); err != nil {
		return err
	}
	resp, err := s.c.delete(ctx, agentDelegationPath(agentID, grantID), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

func (s *agentDelegationService) RevokeCredential(ctx context.Context, agentID, grantID, credentialID string) error {
	if err := requireIDs([2]string{"agent id", agentID}, [2]string{"grant id", grantID}, [2]string{"credential id", credentialID}); err != nil {
		return err
	}
	resp, err := s.c.delete(ctx, agentDelegationPath(agentID, grantID)+"/credentials/"+url.PathEscape(credentialID), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

func (s *agentDelegationService) RevokeAll(ctx context.Context, agentID string) (*RevokeAllAgentDelegationsResponse, error) {
	if err := requireIDs([2]string{"agent id", agentID}); err != nil {
		return nil, err
	}
	resp, err := s.c.postNoRetry(ctx, agentDelegationsPath(agentID)+"/revoke-all", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[RevokeAllAgentDelegationsResponse](resp)
}

func (s *agentDelegationService) RevokeAllMine(ctx context.Context) (*RevokeAllAgentDelegationsResponse, error) {
	resp, err := s.c.postNoRetry(ctx, "/api/v1/users/me/delegations/revoke-all", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[RevokeAllAgentDelegationsResponse](resp)
}

func (s *agentDelegationService) Exchange(ctx context.Context, agentID, grantID string, req *ExchangeAgentDelegationRequest) (*DelegatedAgentCredential, error) {
	if err := requireIDs([2]string{"agent id", agentID}, [2]string{"grant id", grantID}); err != nil {
		return nil, err
	}
	if req == nil || req.Audience == "" {
		return nil, fmt.Errorf("an exchange request with an audience is required")
	}
	resp, err := s.c.postNoRetry(ctx, agentDelegationPath(agentID, grantID)+"/exchange", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[DelegatedAgentCredential](resp)
}
