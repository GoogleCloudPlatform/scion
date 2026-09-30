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

package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

const (
	// UATRandomBytes is the number of random bytes in a UAT.
	UATRandomBytes = 32
	// UATPrefixLength is the length of the visible prefix for identification.
	UATPrefixLength = 12
)

var (
	ErrInvalidUAT        = errors.New("invalid access token")
	ErrUATExpired        = errors.New("access token expired")
	ErrUATRevoked        = errors.New("access token revoked")
	ErrInvalidUATFormat  = errors.New("invalid token format")
	ErrUATLimitExceeded  = errors.New("token limit exceeded")
	ErrInvalidUATScope   = errors.New("invalid token scope")
	ErrUATExpiryTooLong  = errors.New("token expiry exceeds maximum (1 year)")
	ErrUATExpiryPast     = errors.New("token expiry must be in the future")
	ErrUATNameRequired   = errors.New("token name is required")
	ErrUATProjectIDEmpty = errors.New("project ID is required")
	ErrUATScopeEmpty     = errors.New("at least one scope is required")

	// ErrUATScopeViolation is returned when the issuer does not hold all
	// requested scopes in the target project.
	ErrUATScopeViolation = errors.New("requested scopes exceed issuer authority")

	// ErrUATProjectForbidden is returned when the issuer has no authority in
	// the target project OR the project does not exist (oracle resistance).
	ErrUATProjectForbidden = errors.New("forbidden")

	// ErrUATCredentialDenied is returned when a non-session credential
	// (UAT, agent JWT, broker token) attempts a token-management operation.
	ErrUATCredentialDenied = errors.New("access tokens cannot manage other access tokens")
)

// UATRejection reports that a presented UAT failed ValidateToken, and
// classifies why. Reason is one of "invalid", "revoked", "expired", or
// "user_suspended" — see auth.go's UAT branch, which logs exactly one of
// these per rejection (plan §3.1(3)).
//
// Found and TokenID answer the rulings' plan correction (b): a rejection log
// may identify a server-verified matched credential record, but must mark it
// as a rejection, not an authenticated principal. Found is true, and TokenID
// is set, only when the presented value hashed to a stored token row
// (revoked, expired, or the row's user suspended) — never for a value that
// matched nothing (invalid format or unknown hash), so an unrecognized
// bearer value never yields an asserted identity, not even a rejected one.
type UATRejection struct {
	err     error
	Reason  string
	Found   bool
	TokenID string
}

func (e *UATRejection) Error() string { return e.err.Error() }

// Unwrap preserves errors.Is/errors.As compatibility with the pre-existing
// sentinel errors (ErrInvalidUAT, ErrUATRevoked, ErrUATExpired,
// ErrUserSuspended) for the one production caller (auth.go) that already
// branches on ErrUserSuspended via errors.Is.
func (e *UATRejection) Unwrap() error { return e.err }

// ---------------------------------------------------------------------------
// UserAccessTokenService — RS4 bounded domain service
//
// All UAT mutations (create, revoke, delete) flow through this service.
// HTTP handlers validate transport input and delegate; they never directly
// perform authorization, audit, or store mutations for tokens.
//
// The service implements:
//   - A1: Credential caveat — only session/dev credentials admitted
//   - A2: Issuer ceiling — token scopes ⊆ issuer's target-project authority
//   - Oracle-resistant target project authorization
//   - Atomic mutation and audit within store.WithTx
//   - Concurrency-safe per-user token cap
//   - A5: Single operation ID for revoke and delete
//   - Stable typed denial codes
// ---------------------------------------------------------------------------

// UserAccessTokenService handles UAT generation, validation, and management.
type UserAccessTokenService struct {
	store    store.Store
	tokens   store.UserAccessTokenStore
	users    store.UserStore
	projects store.ProjectStore
	authz    *AuthzService
	logger   *slog.Logger
	nowFunc  func() time.Time
}

// NewUserAccessTokenService creates a new UAT service.
func NewUserAccessTokenService(s store.Store, authz *AuthzService, logger *slog.Logger) *UserAccessTokenService {
	return &UserAccessTokenService{
		store:    s,
		tokens:   s,
		users:    s,
		projects: s,
		authz:    authz,
		logger:   logger,
		nowFunc:  time.Now,
	}
}

// createAuditRecord writes a mutation audit record synchronously within the
// caller's context (and transaction, if any). Unlike the fire-and-forget
// emitMutationAudit, this returns an error so the caller can roll back.
func (s *UserAccessTokenService) createAuditRecord(ctx context.Context, txStore store.Store, record *store.MutationAuditRecord) error {
	// E.2a: consolidated actor/credential-snapshot/correlation helper (plan
	// §3.3), replacing this function's own copy of the extraction logic.
	// ApplyActor only fills fields the caller has not already set explicitly.
	auditActorFromContext(ctx).ApplyActor(record)
	if record.Timestamp.IsZero() {
		record.Timestamp = s.nowFunc()
	}
	return txStore.CreateMutationAudit(ctx, record)
}

// enforceSessionCredential enforces the A1 credential caveat and actor/user
// binding at the service boundary:
//
//  1. Only interactive or dev credentials may call token-management methods.
//  2. The caller-supplied userID must match the authenticated identity in the
//     context. This prevents a valid session for user A from operating on
//     user B's tokens via direct service method calls.
//
// This mirrors ProjectDeletionService.Delete's credential ceiling and ensures
// the invariant holds even if a caller bypasses the HTTP handler.
func (s *UserAccessTokenService) enforceSessionCredential(ctx context.Context, userID string) error {
	credential := GetCredentialContextFromContext(ctx)
	switch credential.Kind {
	case CredentialKindInteractive, CredentialKindDev:
		// ok
	default:
		// Fail closed: empty, unknown, UAT, agent_jwt, federation, broker.
		return ErrUATCredentialDenied
	}

	// Actor/user binding: the context identity must match the target userID.
	identity := GetIdentityFromContext(ctx)
	if identity == nil || identity.ID() != userID {
		return ErrUATProjectForbidden
	}
	return nil
}

// TokenMetadata carries optional, bounded, issuer-supplied descriptive
// fields for a new token. Metadata is immutable after issuance: there is no
// update path. Use TokenMetadata{} for no metadata.
type TokenMetadata struct {
	Purpose string
	Labels  map[string]string
}

// CreateTokenParams collects the inputs for minting a token in one struct,
// so a future field (e.g. a hub-vs-project boundary kind) can be added
// without growing a positional argument list.
type CreateTokenParams struct {
	UserID    string
	Name      string
	ProjectID string
	Scopes    []string
	ExpiresAt *time.Time
	Metadata  TokenMetadata
}

// CreateToken generates a new user access token with issuer ceiling,
// target-project authorization, atomic audit, and concurrency-safe cap.
// Returns the plaintext token (shown only once) and the stored metadata.
//
// Deprecated: prefer CreateTokenWithParams. Retained as a thin wrapper over
// it so existing positional call sites keep compiling during incremental
// migration to CreateTokenParams.
func (s *UserAccessTokenService) CreateToken(ctx context.Context, userID, name, projectID string, scopes []string, expiresAt *time.Time) (string, *store.UserAccessToken, error) {
	return s.CreateTokenWithParams(ctx, CreateTokenParams{
		UserID: userID, Name: name, ProjectID: projectID, Scopes: scopes, ExpiresAt: expiresAt,
	})
}

// CreateTokenWithParams is CreateToken's implementation, taking
// CreateTokenParams directly.
func (s *UserAccessTokenService) CreateTokenWithParams(ctx context.Context, params CreateTokenParams) (string, *store.UserAccessToken, error) {
	// A1: Credential caveat at service boundary.
	if err := s.enforceSessionCredential(ctx, params.UserID); err != nil {
		return "", nil, err
	}

	// --- Input validation (typed errors) ---
	if params.Name == "" {
		return "", nil, ErrUATNameRequired
	}
	if params.ProjectID == "" {
		return "", nil, ErrUATProjectIDEmpty
	}
	// Bounded validation of name/purpose/labels at issuance. Metadata is
	// immutable afterward, so this is the only place it is checked.
	if err := ValidateCredentialMetadata(params.Name, params.Metadata.Purpose, params.Metadata.Labels); err != nil {
		return "", nil, err
	}

	// Expand and validate scopes against the registry.
	expanded := expandScopes(params.Scopes)
	for _, scope := range expanded {
		if !store.UATValidScopes[scope] {
			return "", nil, fmt.Errorf("%w: %s", ErrInvalidUATScope, scope)
		}
	}
	if len(expanded) == 0 {
		return "", nil, ErrUATScopeEmpty
	}

	// Validate / default expiry.
	now := s.nowFunc()
	expiresAt := params.ExpiresAt
	if expiresAt == nil {
		defaultExpiry := now.Add(store.UATDefaultExpiry)
		expiresAt = &defaultExpiry
	}
	if expiresAt.Before(now) {
		return "", nil, ErrUATExpiryPast
	}
	if expiresAt.After(now.Add(store.UATMaxExpiry)) {
		return "", nil, ErrUATExpiryTooLong
	}

	// --- Issuer ceiling at mint (target-project only) with oracle resistance ---
	// Resolve only project-scoped permissions for the target project. System/hub
	// authority must not enlarge the token.
	//
	// getProjectScopedPermissions filters to ScopeType==project && ScopeID==projectID
	// while retaining group-expanded principals (transitive membership), activation
	// window filtering (future/expired bindings excluded), and AccessConstraint
	// reduction. If the result is empty, the user has no project-level authority —
	// this covers both non-membership and nonexistent projects with the same error
	// (oracle resistance, G10).
	//
	// Note: authorization runs outside WithTx. The TOCTOU window is acceptable
	// because (1) use-time enforcement narrows every request to the intersection
	// of token scopes and the user's current permissions, and (2) token minting
	// only reads authority state, it does not mutate it. See O1 documentation in
	// rs4_credential_test.go.
	actorPerms, err := s.authz.getProjectScopedPermissions(ctx, store.RoleBindingPrincipalUser, params.UserID, params.ProjectID)
	if err != nil {
		s.logger.Warn("RS4: failed to resolve project-scoped permissions",
			"user_id", params.UserID, "project_id", params.ProjectID, "error", err)
		return "", nil, ErrUATProjectForbidden
	}
	if len(actorPerms) == 0 {
		return "", nil, ErrUATProjectForbidden
	}

	// Resolve the requested scopes to a CeilingVersionV1 ceiling and verify
	// the issuer holds every resulting permission in the target project;
	// that same ceiling is persisted below and is what runtime authorization
	// and delegation enforce. Fail closed if any valid scope does not
	// resolve.
	ceiling, ceilingOK := permissions.BuildCeilingFromSelectors(expanded)
	if !ceilingOK {
		s.logger.Error("RS4: scope-to-permission mapping gap — some valid scope has no resolvable selector",
			"expanded_count", len(expanded))
		return "", nil, ErrUATScopeViolation
	}
	actorPermSet := make(map[string]bool, len(actorPerms))
	for _, p := range actorPerms {
		actorPermSet[p] = true
	}
	for _, permID := range ceiling.PermissionIDs {
		if !actorPermSet[permID] {
			return "", nil, ErrUATScopeViolation
		}
	}

	// --- Atomic mint: token insert + audit in one transaction ---
	var fullKey string
	var token *store.UserAccessToken

	txErr := s.store.WithTx(ctx, func(tx store.Store) error {
		// B5/G7: Concurrency-safe token cap inside the transaction.
		if lockErr := tx.LockUserForTokens(ctx, params.UserID); lockErr != nil {
			return fmt.Errorf("failed to acquire token lock: %w", lockErr)
		}

		count, countErr := tx.CountUserAccessTokens(ctx, params.UserID)
		if countErr != nil {
			return fmt.Errorf("failed to check token count: %w", countErr)
		}
		if count >= store.UATMaxPerUser {
			return ErrUATLimitExceeded
		}

		// Generate random token.
		randomBytes := make([]byte, UATRandomBytes)
		if _, randErr := rand.Read(randomBytes); randErr != nil {
			return fmt.Errorf("failed to generate random bytes: %w", randErr)
		}

		keyBody := base64.RawURLEncoding.EncodeToString(randomBytes)
		fullKey = store.UATPrefix + keyBody
		prefix := store.UATPrefix + keyBody[:UATPrefixLength]
		hash := sha256.Sum256([]byte(fullKey))
		hashStr := hex.EncodeToString(hash[:])

		token = &store.UserAccessToken{
			ID:                   uuid.New().String(),
			UserID:               params.UserID,
			Name:                 params.Name,
			Prefix:               prefix,
			KeyHash:              hashStr,
			ProjectID:            params.ProjectID,
			Scopes:               expanded,
			CeilingVersion:       ceiling.Version,
			CeilingPermissionIDs: ceiling.PermissionIDs,
			ExpiresAt:            expiresAt,
			Created:              now,
		}
		// Descriptive credential metadata: set only when supplied.
		trimmedPurpose := strings.TrimSpace(params.Metadata.Purpose)
		if trimmedPurpose != "" {
			token.Purpose = &trimmedPurpose
		}
		if len(params.Metadata.Labels) > 0 {
			token.Labels = params.Metadata.Labels
		}

		if createErr := tx.CreateUserAccessToken(ctx, token); createErr != nil {
			return fmt.Errorf("failed to create token: %w", createErr)
		}

		// B3/G3: Atomic audit — commit or roll back with the token.
		scopesJSON, _ := json.Marshal(expanded)
		afterSummary := fmt.Sprintf(`{"token_id":%q,"scopes":%s,"project_id":%q}`,
			token.ID, string(scopesJSON), params.ProjectID)
		// Record that purpose/label metadata was set and which label keys
		// were used, without recording label or purpose values in audit
		// (values are issuer-supplied and unbounded-trust text).
		if trimmedPurpose != "" || len(params.Metadata.Labels) > 0 {
			afterSummary = appendCredentialMetadataAuditFields(afterSummary, trimmedPurpose != "", params.Metadata.Labels)
		}

		return s.createAuditRecord(ctx, tx, &store.MutationAuditRecord{
			MutationType: "credential_create",
			TargetType:   "user_access_token",
			TargetID:     token.ID,
			AfterSummary: afterSummary,
		})
	})

	if txErr != nil {
		return "", nil, txErr
	}

	return fullKey, token, nil
}

// ValidateToken validates a UAT and returns the scoped user identity.
func (s *UserAccessTokenService) ValidateToken(ctx context.Context, key string) (*ScopedUserIdentity, error) {
	if !strings.HasPrefix(key, store.UATPrefix) {
		return nil, &UATRejection{err: ErrInvalidUATFormat, Reason: "invalid"}
	}

	hash := sha256.Sum256([]byte(key))
	hashStr := hex.EncodeToString(hash[:])

	token, err := s.tokens.GetUserAccessTokenByHash(ctx, hashStr)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// No stored token matched: an unrecognized presented value never
			// yields an asserted identity, not even a rejected one (rulings,
			// plan correction (b)).
			return nil, &UATRejection{err: ErrInvalidUAT, Reason: "invalid"}
		}
		return nil, fmt.Errorf("failed to look up token: %w", err)
	}

	if token.Revoked {
		return nil, &UATRejection{err: ErrUATRevoked, Reason: "revoked", Found: true, TokenID: token.ID}
	}

	if token.ExpiresAt != nil && time.Now().After(*token.ExpiresAt) {
		return nil, &UATRejection{err: ErrUATExpired, Reason: "expired", Found: true, TokenID: token.ID}
	}

	// Update last used (async)
	go func() {
		_ = s.tokens.UpdateUserAccessTokenLastUsed(context.Background(), token.ID)
	}()

	user, err := s.users.GetUser(ctx, token.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("token user not found")
		}
		return nil, fmt.Errorf("failed to look up user: %w", err)
	}

	if user.Status == store.UserStatusSuspended {
		return nil, &UATRejection{err: ErrUserSuspended, Reason: "user_suspended", Found: true, TokenID: token.ID}
	}

	// Derive the descriptive credential decoration from the
	// server-validated token row this function already loaded. This is the
	// single trustworthy derivation point — no header, query parameter, or
	// body field ever contributes to it.
	//
	// D.1 has not yet persisted a boundary column on the UAT row, so this
	// builds TokenBoundary inline from the token's stored project ID (every
	// UAT is project-scoped today). This is the one call site that changes
	// when D.1 lands.
	decoration := &CredentialDecoration{
		Kind:      CredentialKindUAT,
		TokenID:   token.ID,
		TokenName: token.Name,
		Boundary:  decorationBoundaryFromToken(TokenBoundary{Kind: BoundaryKindProject, ProjectID: token.ProjectID}),
	}
	if token.Purpose != nil {
		decoration.Purpose = *token.Purpose
	}
	if len(token.Labels) > 0 {
		labels := make(map[string]string, len(token.Labels))
		for k, v := range token.Labels {
			labels[k] = v
		}
		decoration.Labels = labels
	}

	return NewScopedUserIdentityWithCeilingAndDecoration(
		NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, string(ClientTypeAPI)),
		token.ProjectID,
		token.Scopes,
		token.ID,
		token.NormalizedCeiling(),
		decoration,
	), nil
}

// ListTokens returns all tokens for a user.
func (s *UserAccessTokenService) ListTokens(ctx context.Context, userID string) ([]store.UserAccessToken, error) {
	// A1: Credential caveat at service boundary.
	if err := s.enforceSessionCredential(ctx, userID); err != nil {
		return nil, err
	}
	return s.tokens.ListUserAccessTokens(ctx, userID)
}

// GetToken retrieves a single token by ID, verifying ownership.
func (s *UserAccessTokenService) GetToken(ctx context.Context, userID, tokenID string) (*store.UserAccessToken, error) {
	// A1: Credential caveat at service boundary.
	if err := s.enforceSessionCredential(ctx, userID); err != nil {
		return nil, err
	}
	token, err := s.tokens.GetUserAccessToken(ctx, tokenID)
	if err != nil {
		return nil, err
	}
	if token.UserID != userID {
		return nil, store.ErrNotFound
	}
	return token, nil
}

// RevokeToken soft-revokes a token, verifying ownership.
// Mutation and audit are atomic within a transaction (B3/G4).
// The operation ID is credential.token.revoke with action:revoke (A5).
func (s *UserAccessTokenService) RevokeToken(ctx context.Context, userID, tokenID string) error {
	// A1: Credential caveat at service boundary.
	if err := s.enforceSessionCredential(ctx, userID); err != nil {
		return err
	}
	return s.store.WithTx(ctx, func(tx store.Store) error {
		token, err := tx.GetUserAccessToken(ctx, tokenID)
		if err != nil {
			return err
		}
		if token.UserID != userID {
			return store.ErrNotFound
		}

		if err := tx.RevokeUserAccessToken(ctx, tokenID); err != nil {
			return err
		}

		// B3/G4: Atomic audit with before-state.
		beforeSummary := fmt.Sprintf(`{"token_id":%q,"action":"revoke"}`, tokenID)
		return s.createAuditRecord(ctx, tx, &store.MutationAuditRecord{
			MutationType:  "credential_revoke",
			TargetType:    "user_access_token",
			TargetID:      tokenID,
			BeforeSummary: beforeSummary,
		})
	})
}

// DeleteToken permanently deletes a token, verifying ownership.
// Mutation and audit are atomic within a transaction (B3/G4).
// The operation ID is credential.token.revoke with action:delete (A5).
func (s *UserAccessTokenService) DeleteToken(ctx context.Context, userID, tokenID string) error {
	// A1: Credential caveat at service boundary.
	if err := s.enforceSessionCredential(ctx, userID); err != nil {
		return err
	}
	return s.store.WithTx(ctx, func(tx store.Store) error {
		token, err := tx.GetUserAccessToken(ctx, tokenID)
		if err != nil {
			return err
		}
		if token.UserID != userID {
			return store.ErrNotFound
		}

		if err := tx.DeleteUserAccessToken(ctx, tokenID); err != nil {
			return err
		}

		// B3/G4: Atomic audit with before-state.
		beforeSummary := fmt.Sprintf(`{"token_id":%q,"action":"delete"}`, tokenID)
		return s.createAuditRecord(ctx, tx, &store.MutationAuditRecord{
			MutationType:  "credential_revoke",
			TargetType:    "user_access_token",
			TargetID:      tokenID,
			BeforeSummary: beforeSummary,
		})
	})
}

// expandScopes expands convenience aliases like agent:manage, skill:manage, etc.
func expandScopes(scopes []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, scope := range scopes {
		if resource, ok := permissions.UATManageAliases[scope]; ok {
			for _, s := range permissions.UATManageScopesFor(resource) {
				if !seen[s] {
					seen[s] = true
					result = append(result, s)
				}
			}
		} else if !seen[scope] {
			seen[scope] = true
			result = append(result, scope)
		}
	}
	return result
}

// IsUAT returns true if the token appears to be a user access token.
func IsUAT(token string) bool {
	return strings.HasPrefix(token, store.UATPrefix)
}
