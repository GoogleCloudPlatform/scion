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
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// TokenService handles user access token operations.
type TokenService interface {
	// Create creates a new user access token.
	Create(ctx context.Context, req *CreateTokenRequest) (*CreateTokenResponse, error)

	// List returns all tokens for the authenticated user.
	List(ctx context.Context) (*ListTokensResponse, error)

	// Get returns details for a specific token.
	Get(ctx context.Context, id string) (*TokenInfo, error)

	// Revoke soft-revokes a token (it still exists but cannot be used).
	Revoke(ctx context.Context, id string) error

	// Delete permanently removes a token.
	Delete(ctx context.Context, id string) error

	// ListScopes returns every published token selector. With opts.ProjectID
	// set (project boundary) or opts.Boundary set to BoundaryHub, each entry
	// additionally reports whether the authenticated user may currently
	// select it for a token of that boundary: this answers only "may you
	// select this restriction," never a target list, and it is computed
	// fresh for the caller's current authority.
	ListScopes(ctx context.Context, opts *ListScopesOptions) (*ScopesResponse, error)
}

// tokenService is the implementation of TokenService.
type tokenService struct {
	c *client
}

// Token boundary kinds, as sent in TokenBoundary.Kind and accepted by
// ListScopesOptions.Boundary.
const (
	// BoundaryProject is a token that works only in one project.
	BoundaryProject = "project"
	// BoundaryHub is a token that is not tied to a project: it reaches any
	// project the user can currently access, and is the only boundary that
	// can select hub-level scopes.
	BoundaryHub = "hub"
)

// CreateTokenRequest is the request for creating a user access token.
// A project token sets both ProjectID and Boundary
// {Kind: BoundaryProject, ProjectID: id}, naming the same project; a hub
// token sets only Boundary {Kind: BoundaryHub}.
type CreateTokenRequest struct {
	Name string `json:"name"`
	// ProjectID is the project-boundary shorthand. With a project
	// Boundary it must name the same project; leave it empty for a hub
	// Boundary.
	ProjectID string `json:"projectId,omitempty"`
	// Boundary is the explicit boundary. Nil means the project boundary of
	// ProjectID; the hub rejects a request that names neither.
	Boundary  *TokenBoundary `json:"boundary,omitempty"`
	Scopes    []string       `json:"scopes"`
	ExpiresAt *time.Time     `json:"expiresAt,omitempty"`

	// E.1 descriptive credential metadata: optional, bounded, immutable
	// after issuance (there is no update endpoint).
	Purpose string            `json:"purpose,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// CreateTokenResponse is the response from creating a user access token.
type CreateTokenResponse struct {
	Token       string     `json:"token"` // Full token value, shown only once
	AccessToken *TokenInfo `json:"accessToken"`
}

// TokenInfo represents token metadata (without the actual token value).
type TokenInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	// ProjectID is set only for a project-boundary token; the hub omits it
	// for a hub-boundary token. Use BoundaryKind to tell the kinds apart.
	ProjectID string `json:"projectId,omitempty"`
	// Boundary is the boundary the token was issued under, and is
	// authoritative when present; nil from a hub that does not report it.
	Boundary  *TokenBoundary `json:"boundary,omitempty"`
	Scopes    []string       `json:"scopes"`
	Revoked   bool           `json:"revoked"`
	ExpiresAt *time.Time     `json:"expiresAt,omitempty"`
	LastUsed  *time.Time     `json:"lastUsed,omitempty"`
	Created   time.Time      `json:"created"`

	// E.1 descriptive credential metadata: empty for tokens created before
	// E.1 or without metadata supplied at issuance.
	Purpose string            `json:"purpose,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// BoundaryKind returns the token's boundary kind: Boundary.Kind when the
// hub reports a boundary, otherwise BoundaryProject when the token carries
// a project ID, otherwise "" (unknown). An unknown boundary is never
// reported as BoundaryHub.
func (t TokenInfo) BoundaryKind() string {
	if t.Boundary != nil && t.Boundary.Kind != "" {
		return t.Boundary.Kind
	}
	if t.ProjectID != "" {
		return BoundaryProject
	}
	return ""
}

// ListTokensResponse is the response from listing user access tokens.
type ListTokensResponse struct {
	Items []TokenInfo `json:"items"`
}

// ListScopesOptions selects which boundary, if any, to compute eligibility
// for. Leaving both fields empty returns the unchanged scope catalog, with
// no eligibility on any entry.
type ListScopesOptions struct {
	// ProjectID requests project-boundary eligibility for this project.
	ProjectID string
	// Boundary is BoundaryProject (needs ProjectID) or BoundaryHub (must
	// not set ProjectID). A hub that cannot compute hub-boundary
	// eligibility answers BoundaryHub with a 400 APIError whose Code is
	// "unsupported_boundary"; callers then fall back to the catalog
	// filtered by ScopeInfo.AllowedBoundaries.
	Boundary string
}

// TokenBoundary is the wire shape shared with the token create/response
// boundary field: {"kind":"project","projectId":"..."} or {"kind":"hub"}
// (ProjectID omitted for hub).
type TokenBoundary struct {
	Kind      string `json:"kind"`
	ProjectID string `json:"projectId,omitempty"`
}

// ScopeEligibility answers, for one selector and one requested boundary,
// only "may the authenticated user select this restriction" -- never a
// capability and never a target list.
type ScopeEligibility struct {
	Boundary TokenBoundary `json:"boundary"`
	Eligible bool          `json:"eligible"`
	// Reason is a stable machine code (e.g. "flat_role_insufficient",
	// "no_relationship_candidacy", "boundary_not_allowed",
	// "unknown_selector", "project_access_required", and for a hub
	// boundary "authority_required": the account has no current access
	// that includes the permission), present only when !Eligible.
	// "project_access_required" appears here only when at least one other
	// selector in the same response was eligible; when EVERY evaluated
	// selector lacks project access, the whole request fails with the
	// request-level error instead (see ListScopes).
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
	// IneligibleMembers is set only on a ScopeAliasInfo entry: the subset
	// of ExpandsTo that is not eligible.
	IneligibleMembers []string `json:"ineligibleMembers,omitempty"`
}

// ScopeInfo is a single UAT scope from GET /api/v1/auth/scopes.
type ScopeInfo struct {
	ID          string `json:"id"`
	Resource    string `json:"resource"`
	Action      string `json:"action"`
	Description string `json:"description"`

	PermissionID           string   `json:"permissionId,omitempty"`
	AllowedBoundaries      []string `json:"allowedBoundaries,omitempty"`
	EligibilityKind        string   `json:"eligibilityKind,omitempty"`
	Relationships          []string `json:"relationships,omitempty"`
	RequiresExistingTarget bool     `json:"requiresExistingTarget,omitempty"`

	// Eligibility is present only when the request named a boundary.
	Eligibility *ScopeEligibility `json:"eligibility,omitempty"`
}

// ScopeAliasInfo is a convenience alias (e.g. "agent:manage") that expands
// to multiple scopes.
type ScopeAliasInfo struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	ExpandsTo   []string `json:"expands_to"`

	// Eligibility is present only when the request named a boundary. An
	// alias is eligible only when every member selector is.
	Eligibility *ScopeEligibility `json:"eligibility,omitempty"`
}

// ScopesResponse is the response from GET /api/v1/auth/scopes.
type ScopesResponse struct {
	Scopes  []ScopeInfo      `json:"scopes"`
	Aliases []ScopeAliasInfo `json:"aliases"`
}

// Create creates a new user access token.
func (s *tokenService) Create(ctx context.Context, req *CreateTokenRequest) (*CreateTokenResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/auth/tokens", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[CreateTokenResponse](resp)
}

// List returns all tokens for the authenticated user.
func (s *tokenService) List(ctx context.Context) (*ListTokensResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/auth/tokens", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListTokensResponse](resp)
}

// Get returns details for a specific token.
func (s *tokenService) Get(ctx context.Context, id string) (*TokenInfo, error) {
	resp, err := s.c.get(ctx, "/api/v1/auth/tokens/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[TokenInfo](resp)
}

// Revoke soft-revokes a token.
func (s *tokenService) Revoke(ctx context.Context, id string) error {
	resp, err := s.c.post(ctx, "/api/v1/auth/tokens/"+url.PathEscape(id)+"/revoke", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Delete permanently removes a token.
func (s *tokenService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.delete(ctx, "/api/v1/auth/tokens/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// ListScopes returns every published token selector, and with
// opts.ProjectID or opts.Boundary set, mint eligibility per selector for
// that boundary. A nil opts (or one with both fields empty) is the
// catalog-only call.
func (s *tokenService) ListScopes(ctx context.Context, opts *ListScopesOptions) (*ScopesResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.ProjectID != "" {
			query.Set("projectId", opts.ProjectID)
		}
		if opts.Boundary != "" {
			query.Set("boundary", opts.Boundary)
		}
	}
	resp, err := s.c.getWithQuery(ctx, "/api/v1/auth/scopes", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ScopesResponse](resp)
}

// AsScopeViolation reports whether err is the scope_violation error POST
// /api/v1/auth/tokens returns when mint-time eligibility (CanMintSelector)
// denies a requested selector, returning the denied selector and the
// machine-readable reason. Denials for both boundaries are typed this way;
// a hub-boundary denial carries the reason "authority_required".
// It returns ok=false for every other error,
// including the detail-free, oracle-resistant project-forbidden error mint
// also returns for a project the caller cannot access.
func AsScopeViolation(err error) (selector, reason string, ok bool) {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "scope_violation" {
		return "", "", false
	}
	selector, _ = apiErr.Details["selector"].(string)
	reason, _ = apiErr.Details["reason"].(string)
	return selector, reason, true
}
