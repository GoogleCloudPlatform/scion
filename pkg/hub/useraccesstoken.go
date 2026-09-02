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
	if record.ActorPrincipalKind == "" || record.ActorPrincipalID == "" {
		identity := GetIdentityFromContext(ctx)
		if identity != nil {
			record.ActorPrincipalKind = identity.Type()
			record.ActorPrincipalID = identity.ID()
			credential := GetCredentialContextFromContext(ctx)
			if credential.Kind != "" {
				record.ActorCredentialID = credential.ID
				record.ActorCredentialType = string(credential.Kind)
			}
		}
	}
	if record.Timestamp.IsZero() {
		record.Timestamp = s.nowFunc()
	}
	return txStore.CreateMutationAudit(ctx, record)
}

// scopeToPermissionIDs converts UAT scope strings (resource:action) to
// permission IDs using the production permissions.Registry.
func scopeToPermissionIDs(scopes []string) []string {
	scopeSet := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		scopeSet[s] = true
	}
	var ids []string
	for _, p := range permissions.Registry {
		scopeKey := p.Resource + ":" + p.Action
		if scopeSet[scopeKey] {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// enforceSessionCredential enforces the A1 credential caveat at the service
// boundary: only interactive or dev credentials may call token-management
// methods. This mirrors ProjectDeletionService.Delete's credential ceiling
// and ensures the invariant holds even if a caller bypasses the HTTP handler.
func (s *UserAccessTokenService) enforceSessionCredential(ctx context.Context) error {
	credential := GetCredentialContextFromContext(ctx)
	switch credential.Kind {
	case CredentialKindInteractive, CredentialKindDev:
		return nil
	default:
		// Fail closed: empty, unknown, UAT, agent_jwt, federation, broker.
		return ErrUATCredentialDenied
	}
}

// CreateToken generates a new user access token with issuer ceiling,
// target-project authorization, atomic audit, and concurrency-safe cap.
// Returns the plaintext token (shown only once) and the stored metadata.
func (s *UserAccessTokenService) CreateToken(ctx context.Context, userID, name, projectID string, scopes []string, expiresAt *time.Time) (string, *store.UserAccessToken, error) {
	// A1: Credential caveat at service boundary.
	if err := s.enforceSessionCredential(ctx); err != nil {
		return "", nil, err
	}

	// --- Input validation (typed errors) ---
	if name == "" {
		return "", nil, ErrUATNameRequired
	}
	if projectID == "" {
		return "", nil, ErrUATProjectIDEmpty
	}

	// Expand and validate scopes against the registry.
	expanded := expandScopes(scopes)
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

	// --- B1/A2/B2: Issuer ceiling at mint (target-project only) with oracle resistance ---
	// Resolve only project-scoped permissions for the target project. System/hub
	// authority must not enlarge the token (frozen decision A2).
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
	actorPerms, err := s.authz.getProjectScopedPermissions(ctx, store.RoleBindingPrincipalUser, userID, projectID)
	if err != nil {
		s.logger.Warn("RS4: failed to resolve project-scoped permissions",
			"user_id", userID, "project_id", projectID, "error", err)
		return "", nil, ErrUATProjectForbidden
	}
	if len(actorPerms) == 0 {
		return "", nil, ErrUATProjectForbidden
	}

	// Convert requested scopes to permission IDs and verify the issuer holds
	// each one in the target project. Fail closed if any valid scope does not
	// map to exactly one registered permission (F1 defense).
	requiredPermIDs := scopeToPermissionIDs(expanded)
	if len(requiredPermIDs) < len(expanded) {
		s.logger.Error("RS4: scope-to-permission mapping gap — some valid scopes have no registered permission",
			"expanded_count", len(expanded), "mapped_count", len(requiredPermIDs))
		return "", nil, ErrUATScopeViolation
	}
	actorPermSet := make(map[string]bool, len(actorPerms))
	for _, p := range actorPerms {
		actorPermSet[p] = true
	}
	for _, permID := range requiredPermIDs {
		if !actorPermSet[permID] {
			return "", nil, ErrUATScopeViolation
		}
	}

	// --- Atomic mint: token insert + audit in one transaction ---
	var fullKey string
	var token *store.UserAccessToken

	txErr := s.store.WithTx(ctx, func(tx store.Store) error {
		// B5/G7: Concurrency-safe token cap inside the transaction.
		if lockErr := tx.LockUserForTokens(ctx, userID); lockErr != nil {
			return fmt.Errorf("failed to acquire token lock: %w", lockErr)
		}

		count, countErr := tx.CountUserAccessTokens(ctx, userID)
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
			ID:        uuid.New().String(),
			UserID:    userID,
			Name:      name,
			Prefix:    prefix,
			KeyHash:   hashStr,
			ProjectID: projectID,
			Scopes:    expanded,
			ExpiresAt: expiresAt,
			Created:   now,
		}

		if createErr := tx.CreateUserAccessToken(ctx, token); createErr != nil {
			return fmt.Errorf("failed to create token: %w", createErr)
		}

		// B3/G3: Atomic audit — commit or roll back with the token.
		scopesJSON, _ := json.Marshal(expanded)
		afterSummary := fmt.Sprintf(`{"token_id":%q,"scopes":%s,"project_id":%q}`,
			token.ID, string(scopesJSON), projectID)

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
		return nil, ErrInvalidUATFormat
	}

	hash := sha256.Sum256([]byte(key))
	hashStr := hex.EncodeToString(hash[:])

	token, err := s.tokens.GetUserAccessTokenByHash(ctx, hashStr)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidUAT
		}
		return nil, fmt.Errorf("failed to look up token: %w", err)
	}

	if token.Revoked {
		return nil, ErrUATRevoked
	}

	if token.ExpiresAt != nil && time.Now().After(*token.ExpiresAt) {
		return nil, ErrUATExpired
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
		return nil, ErrUserSuspended
	}

	return NewScopedUserIdentityWithCredentialID(
		NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, string(ClientTypeAPI)),
		token.ProjectID,
		token.Scopes,
		token.ID,
	), nil
}

// ListTokens returns all tokens for a user.
func (s *UserAccessTokenService) ListTokens(ctx context.Context, userID string) ([]store.UserAccessToken, error) {
	// A1: Credential caveat at service boundary.
	if err := s.enforceSessionCredential(ctx); err != nil {
		return nil, err
	}
	return s.tokens.ListUserAccessTokens(ctx, userID)
}

// GetToken retrieves a single token by ID, verifying ownership.
func (s *UserAccessTokenService) GetToken(ctx context.Context, userID, tokenID string) (*store.UserAccessToken, error) {
	// A1: Credential caveat at service boundary.
	if err := s.enforceSessionCredential(ctx); err != nil {
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
	if err := s.enforceSessionCredential(ctx); err != nil {
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
	if err := s.enforceSessionCredential(ctx); err != nil {
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
