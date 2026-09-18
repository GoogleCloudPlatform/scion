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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// GE Google credential exchange — POST /api/v1/auth/integrations/google/exchange
//
// This endpoint validates a Google end-user credential (ID token or access
// token) and returns a short-lived Hub user access token. It is the Hub-side
// implementation of the auth-exchange-contract.
// ---------------------------------------------------------------------------

// GEGoogleExchangeConfig holds the trust configuration for GE Google exchange.
type GEGoogleExchangeConfig struct {
	// Enabled controls whether the exchange endpoint is active.
	Enabled bool
	// AllowedClientIDs is the non-empty set of Google OAuth client IDs
	// accepted as credential audiences. A dedicated GE web client is
	// recommended; the existing Hub web client may be listed intentionally.
	AllowedClientIDs []string
	// TokenTTL is the configured lifetime for minted Hub access tokens.
	// Capped at min(this, upstream expiry) per contract-review point 4.
	// Default: 5 minutes.
	TokenTTL time.Duration
}

// IsValid returns true if the config has required fields populated.
func (c *GEGoogleExchangeConfig) IsValid() bool {
	return c.Enabled && len(c.AllowedClientIDs) > 0
}

// DefaultGETokenTTL is the default Hub access token lifetime for GE exchange.
// Aligned with the bridge cache default (~60s) per the auth-exchange contract:
// a shorter token lifetime bounds the revocation window when a Google credential
// is revoked upstream. The effective TTL is min(this, upstream remaining).
const DefaultGETokenTTL = 60 * time.Second

// MaxGETokenTTL is the maximum allowed Hub access token lifetime.
// Matches the bridge cache maximum (300s). Longer values widen the revocation
// window without proportional benefit since the bridge re-validates anyway.
const MaxGETokenTTL = 5 * time.Minute

// ExternalIdentityBinding is an alias for the store model type.
// The durable store implementation lives in pkg/store/entadapter backed by
// the ExternalIdentity ent schema with a unique composite index on
// (provider, issuer, subject) for conflict-safe concurrent binding.
type ExternalIdentityBinding = store.ExternalIdentityBinding

// ExternalIdentityStore is an alias for the store interface.
type ExternalIdentityStore = store.ExternalIdentityStore

// ---------------------------------------------------------------------------
// GEExchangeService — the core exchange logic.
// ---------------------------------------------------------------------------

// UserAuthChecker checks whether a user email is authorized to access the Hub
// per the configured domain restrictions, invite-only mode, and admin list.
// Returns true if the user is authorized.
type UserAuthChecker func(ctx context.Context, email string) bool

// GEExchangeService handles the credential exchange flow.
type GEExchangeService struct {
	config           GEGoogleExchangeConfig
	validator        GoogleCredentialValidator
	userTokenService *UserTokenService
	extIDStore       ExternalIdentityStore
	userStore        store.Store
	authChecker      UserAuthChecker
	logger           *slog.Logger
	nowFunc          func() time.Time
}

// NewGEExchangeService creates a new GE exchange service.
func NewGEExchangeService(
	config GEGoogleExchangeConfig,
	validator GoogleCredentialValidator,
	userTokenService *UserTokenService,
	extIDStore ExternalIdentityStore,
	userStore store.Store,
	authChecker UserAuthChecker,
	logger *slog.Logger,
) *GEExchangeService {
	if config.TokenTTL == 0 {
		config.TokenTTL = DefaultGETokenTTL
	}
	if config.TokenTTL > MaxGETokenTTL {
		config.TokenTTL = MaxGETokenTTL
	}
	if authChecker == nil {
		// Fail closed: if no auth checker is provided, reject all provisioning.
		authChecker = func(ctx context.Context, email string) bool { return false }
	}
	return &GEExchangeService{
		config:           config,
		validator:        validator,
		userTokenService: userTokenService,
		extIDStore:       extIDStore,
		userStore:        userStore,
		authChecker:      authChecker,
		logger:           logger,
		nowFunc:          time.Now,
	}
}

// ExchangeRequest is the request body for the exchange endpoint.
type ExchangeRequest struct {
	Credential     string `json:"credential"`
	CredentialType string `json:"credentialType"` // "id_token" or "access_token"
}

// ExchangeResponse is the successful response from the exchange endpoint.
type ExchangeResponse struct {
	AccessToken       string        `json:"accessToken"`
	TokenType         string        `json:"tokenType"` // always "Bearer"
	ExpiresAt         string        `json:"expiresAt"`
	UpstreamExpiresAt string        `json:"upstreamExpiresAt"`
	User              *UserResponse `json:"user"`
}

// Exchange performs the full credential exchange flow:
// 1. Validate the Google credential
// 2. Resolve to a local Hub user (via external identity binding)
// 3. Mint a short-lived Hub access token
func (s *GEExchangeService) Exchange(ctx context.Context, req *ExchangeRequest) (*ExchangeResponse, int, error) {
	if !s.config.IsValid() {
		return nil, http.StatusUnauthorized, ErrGENotConfigured
	}

	// Parse and validate credential type — comes from bridge config, not heuristic.
	credType := GoogleCredentialType(req.CredentialType)
	switch credType {
	case GoogleCredentialIDToken, GoogleCredentialAccessToken:
		// valid
	default:
		return nil, http.StatusBadRequest, fmt.Errorf("%w: %q", ErrGEUnsupportedCredType, req.CredentialType)
	}

	if req.Credential == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("credential is required")
	}

	// Step 1: Validate the Google credential.
	var identity *ValidatedGoogleIdentity
	var err error
	switch credType {
	case GoogleCredentialIDToken:
		identity, err = s.validator.ValidateIDToken(ctx, req.Credential, s.config.AllowedClientIDs)
	case GoogleCredentialAccessToken:
		identity, err = s.validator.ValidateAccessToken(ctx, req.Credential, s.config.AllowedClientIDs)
	}
	if err != nil {
		s.logger.Warn("GE exchange: credential validation failed",
			"credential_type", credType,
			"error", err)
		// Map specific errors to HTTP status codes.
		switch {
		case errors.Is(err, ErrGoogleExpiredCredential),
			errors.Is(err, ErrGENoRemainingLifetime):
			return nil, http.StatusUnauthorized, fmt.Errorf("credential expired or has no remaining lifetime")
		case errors.Is(err, ErrGoogleUntrustedAudience),
			errors.Is(err, ErrGoogleUntrustedIssuer):
			return nil, http.StatusUnauthorized, fmt.Errorf("credential not trusted")
		case errors.Is(err, ErrGoogleServiceAccount):
			return nil, http.StatusForbidden, fmt.Errorf("service account credentials not accepted for user exchange")
		case errors.Is(err, ErrGoogleUnverifiedEmail):
			return nil, http.StatusForbidden, fmt.Errorf("email not verified")
		case errors.Is(err, ErrGENotConfigured):
			return nil, http.StatusUnauthorized, fmt.Errorf("exchange not configured")
		case errors.Is(err, ErrGoogleFieldDisagreement):
			return nil, http.StatusUnauthorized, fmt.Errorf("credential metadata inconsistent")
		default:
			return nil, http.StatusUnauthorized, fmt.Errorf("credential validation failed")
		}
	}

	// Step 2: Resolve to a local Hub user via external identity binding.
	user, err := s.resolveLocalUser(ctx, identity)
	if err != nil {
		s.logger.Warn("GE exchange: user resolution failed",
			"sub", identity.Subject,
			"email", identity.Email,
			"error", err)
		switch {
		case errors.Is(err, ErrAccessDenied):
			return nil, http.StatusForbidden, fmt.Errorf("user not authorized")
		case errors.Is(err, ErrUserSuspended):
			return nil, http.StatusForbidden, fmt.Errorf("user account suspended")
		case errors.Is(err, errBindingConflict):
			return nil, http.StatusForbidden, fmt.Errorf("identity binding conflict")
		case errors.Is(err, errAmbiguousLinkage):
			return nil, http.StatusForbidden, fmt.Errorf("ambiguous identity linkage")
		case errors.Is(err, errNonAuthoritativeEmail):
			return nil, http.StatusForbidden, fmt.Errorf("automatic account linkage not available for this email domain")
		default:
			return nil, http.StatusInternalServerError, fmt.Errorf("user resolution failed")
		}
	}

	// Step 3: Mint a short-lived Hub access token.
	// Cap the token TTL at min(configured TTL, upstream expiry) per contract-review point 4.
	tokenTTL := s.config.TokenTTL
	upstreamRemaining := time.Until(identity.UpstreamExpiry)
	if upstreamRemaining < tokenTTL {
		tokenTTL = upstreamRemaining
	}

	// Reject if no remaining usable lifetime after capping.
	if tokenTTL <= 0 {
		return nil, http.StatusUnauthorized, ErrGENoRemainingLifetime
	}

	now := s.nowFunc()
	hubExpiry := now.Add(tokenTTL)

	accessToken, _, err := s.userTokenService.GenerateAccessTokenWithTTL(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeAPI, tokenTTL,
	)
	if err != nil {
		s.logger.Error("GE exchange: token generation failed", "error", err)
		return nil, http.StatusInternalServerError, fmt.Errorf("token generation failed")
	}

	return &ExchangeResponse{
		AccessToken:       accessToken,
		TokenType:         "Bearer",
		ExpiresAt:         hubExpiry.UTC().Format(time.RFC3339),
		UpstreamExpiresAt: identity.UpstreamExpiry.UTC().Format(time.RFC3339),
		User: &UserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
			Role:        user.Role,
		},
	}, http.StatusOK, nil
}

// ---------------------------------------------------------------------------
// User resolution — external identity binding with local user provisioning.
// ---------------------------------------------------------------------------

var (
	errBindingConflict       = errors.New("external identity binding conflicts with existing user")
	errAmbiguousLinkage      = errors.New("ambiguous email-to-user linkage")
	errNonAuthoritativeEmail = errors.New("email domain not authoritative for automatic linkage")
)

// resolveLocalUser resolves a validated Google identity to a local Hub user
// via the external identity binding system. The flow is:
//
// 1. Look up existing binding by (provider, canonical issuer, sub).
// 2. If found: verify the bound user exists and is not suspended, update
//    email if changed. Return the user.
// 3. If not found: attempt first-time bootstrap via email, guarded by
//    authoritative email domain requirement.
// 4. Create atomic binding and return user.
func (s *GEExchangeService) resolveLocalUser(ctx context.Context, identity *ValidatedGoogleIdentity) (*store.User, error) {
	canonicalIssuer := canonicalizeGoogleIssuer(identity.Issuer)

	// Step 1: Look up existing binding.
	binding, err := s.extIDStore.GetExternalIdentity(ctx, "google", canonicalIssuer, identity.Subject)
	if err == nil {
		// Binding exists — verify the bound user.
		user, err := s.userStore.GetUser(ctx, binding.UserID)
		if err != nil {
			s.logger.Error("GE exchange: bound user not found",
				"binding_id", binding.ID,
				"user_id", binding.UserID,
				"sub", identity.Subject)
			return nil, fmt.Errorf("bound user not found: %w", err)
		}

		if user.Status == "suspended" {
			return nil, ErrUserSuspended
		}

		// Update email if it changed (informational, does not relink).
		normalizedEmail := strings.ToLower(identity.Email)
		if strings.ToLower(binding.Email) != normalizedEmail {
			s.logger.Info("GE exchange: updating binding email",
				"old", binding.Email, "new", normalizedEmail,
				"sub", identity.Subject, "user_id", user.ID)
			_ = s.extIDStore.UpdateExternalIdentityEmail(ctx, binding.ID, normalizedEmail)
			// Also update the user's profile email if it matches the old binding email.
			if strings.ToLower(user.Email) == strings.ToLower(binding.Email) {
				user.Email = normalizedEmail
				_ = s.userStore.UpdateUser(ctx, user)
			}
		}

		// Update display name / avatar if missing.
		updated := false
		if identity.DisplayName != "" && user.DisplayName == "" {
			user.DisplayName = identity.DisplayName
			updated = true
		}
		if identity.AvatarURL != "" && user.AvatarURL == "" {
			user.AvatarURL = identity.AvatarURL
			updated = true
		}
		if updated {
			_ = s.userStore.UpdateUser(ctx, user)
		}

		return user, nil
	}

	// Step 2: No existing binding — attempt first-time bootstrap.
	// Per contract-review point 1: automatic bootstrap only for authoritative
	// email domains (Gmail or verified Workspace hd).
	if !isAuthoritativeEmailDomain(identity.Email, identity.HostedDomain) {
		s.logger.Warn("GE exchange: non-authoritative email, cannot auto-link",
			"email", identity.Email,
			"hd", identity.HostedDomain,
			"sub", identity.Subject)
		return nil, errNonAuthoritativeEmail
	}

	// Look up existing user by email.
	normalizedEmail := strings.ToLower(identity.Email)
	existingUser, err := s.userStore.GetUserByEmail(ctx, normalizedEmail)
	if err == nil {
		// Found a user by email. Verify no conflicting binding exists.
		existingBindings, _ := s.extIDStore.GetExternalIdentitiesByUserID(ctx, existingUser.ID)
		for _, eb := range existingBindings {
			if eb.Provider == "google" && eb.Issuer == canonicalIssuer && eb.Subject != identity.Subject {
				// Another Google subject is already bound to this user.
				s.logger.Error("GE exchange: conflicting Google binding",
					"existing_sub", eb.Subject,
					"new_sub", identity.Subject,
					"user_id", existingUser.ID)
				return nil, errBindingConflict
			}
		}

		if existingUser.Status == "suspended" {
			return nil, ErrUserSuspended
		}

		// Create the binding atomically. If a concurrent exchange already
		// created it (unique constraint violation), fall back to the winner's
		// binding — this is the conflict-safe race-resolution path.
		now := time.Now()
		if err := s.extIDStore.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
			ID:        uuid.New().String(),
			Provider:  "google",
			Issuer:    canonicalIssuer,
			Subject:   identity.Subject,
			UserID:    existingUser.ID,
			Email:     normalizedEmail,
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			return s.resolveAfterConflict(ctx, canonicalIssuer, identity, existingUser.ID, err)
		}

		s.logger.Info("GE exchange: created new binding for existing user",
			"sub", identity.Subject,
			"email", normalizedEmail,
			"user_id", existingUser.ID)
		return existingUser, nil
	}

	// No existing user by email — provision a new user through the normal path.
	// This requires the same authorization checks as regular login.
	//
	// provisionNewUser may return an existing user instead of a newly created
	// one when a concurrent exchange wins the unique-email race. The
	// provisioned flag distinguishes the two cases for orphan cleanup below.
	user, provisioned, err := s.provisionNewUser(ctx, identity)
	if err != nil {
		return nil, err
	}

	// Create the binding. If a concurrent exchange already created it
	// (unique constraint violation), resolve via the winning binding and
	// clean up the orphaned user only if we actually provisioned a NEW user
	// that differs from the winner's user.
	now := time.Now()
	if err := s.extIDStore.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
		ID:        uuid.New().String(),
		Provider:  "google",
		Issuer:    canonicalIssuer,
		Subject:   identity.Subject,
		UserID:    user.ID,
		Email:     normalizedEmail,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		winner, resolveErr := s.resolveAfterConflict(ctx, canonicalIssuer, identity, "", err)
		// Orphan cleanup: only delete if we provisioned a new user AND the
		// winner resolved to a different user. When all concurrent exchanges
		// converge on the same user (via email-collision resolution), the
		// user is NOT an orphan even if we lose the binding race.
		if provisioned && resolveErr == nil && winner != nil && winner.ID != user.ID {
			if delErr := s.userStore.DeleteUser(ctx, user.ID); delErr != nil {
				s.logger.Warn("GE exchange: failed to clean up orphaned user after conflict",
					"user_id", user.ID, "error", delErr)
			}
		}
		return winner, resolveErr
	}

	return user, nil
}

// resolveAfterConflict handles the case where CreateExternalIdentity failed
// due to a unique constraint violation (race between concurrent exchanges).
// It looks up the winning binding and resolves to the winner's user.
//
// If expectedUserID is non-empty (linking to an existing user), the winner's
// binding must point to the same user — otherwise it fails closed with
// errBindingConflict to prevent silently adopting a mismatched user.
func (s *GEExchangeService) resolveAfterConflict(ctx context.Context, canonicalIssuer string, identity *ValidatedGoogleIdentity, expectedUserID string, createErr error) (*store.User, error) {
	// Retry by looking up the binding the winner created.
	winner, err := s.extIDStore.GetExternalIdentity(ctx, "google", canonicalIssuer, identity.Subject)
	if err != nil {
		// Binding still not found: this was a genuine error, not a race.
		s.logger.Error("GE exchange: binding creation failed and no winning binding found",
			"create_error", createErr, "lookup_error", err, "sub", identity.Subject)
		return nil, fmt.Errorf("failed to create identity binding: %w", createErr)
	}

	// If we expected a specific user (existing-user linkage path), validate
	// the winner bound to the same user. Fail closed otherwise.
	if expectedUserID != "" && winner.UserID != expectedUserID {
		s.logger.Error("GE exchange: conflict resolution mismatch — winner bound to different user",
			"expected_user_id", expectedUserID, "winner_user_id", winner.UserID,
			"sub", identity.Subject)
		return nil, errBindingConflict
	}

	// Found the winner's binding — resolve to the winner's user.
	user, err := s.userStore.GetUser(ctx, winner.UserID)
	if err != nil {
		return nil, fmt.Errorf("bound user not found after conflict resolution: %w", err)
	}
	if user.Status == "suspended" {
		return nil, ErrUserSuspended
	}

	s.logger.Info("GE exchange: resolved to existing binding after race",
		"sub", identity.Subject, "user_id", user.ID)
	return user, nil
}

// provisionNewUser creates a new user via the normal Hub provisioning path.
// Enforces the same domain/invite/allow-registration policy as the normal
// Hub login flow via the injected authChecker.
//
// Returns (user, true, nil) when a new user was created, or
// (winner, false, nil) when CreateUser lost a unique-email race and the
// winning user was found. The caller uses the provisioned flag to decide
// whether orphan cleanup is appropriate.
//
// When CreateUser fails with a unique-email constraint violation (concurrent
// exchange race), re-queries by normalized email. If the winning user is found
// and passes suspension checks, returns the winner with provisioned=false;
// otherwise returns the original create error to fail closed.
func (s *GEExchangeService) provisionNewUser(ctx context.Context, identity *ValidatedGoogleIdentity) (user *store.User, provisioned bool, err error) {
	normalizedEmail := strings.ToLower(identity.Email)

	// Enforce Hub registration policy (domain, invite-only, allow-list).
	// Fail closed: authChecker defaults to rejecting all if not provided.
	if !s.authChecker(ctx, normalizedEmail) {
		s.logger.Warn("GE exchange: user not authorized for auto-provisioning",
			"email", normalizedEmail, "sub", identity.Subject)
		return nil, false, fmt.Errorf("%w: user not authorized for auto-provisioning", ErrAccessDenied)
	}

	newUser := &store.User{
		ID:          uuid.New().String(),
		Email:       normalizedEmail,
		DisplayName: identity.DisplayName,
		AvatarURL:   identity.AvatarURL,
		Role:        "member",
		Status:      store.UserStatusActive,
		Created:     time.Now(),
		LastLogin:   time.Now(),
	}

	if createErr := s.userStore.CreateUser(ctx, newUser); createErr != nil {
		// Unique-email collision: another concurrent exchange won the race
		// and created the user first. Re-query by email to find the winner.
		if errors.Is(createErr, store.ErrAlreadyExists) {
			winner, lookupErr := s.userStore.GetUserByEmail(ctx, normalizedEmail)
			if lookupErr != nil {
				// No winner found — return the original create error (fail closed).
				s.logger.Error("GE exchange: user creation conflict but no winner found",
					"email", normalizedEmail, "create_error", createErr, "lookup_error", lookupErr)
				return nil, false, fmt.Errorf("create user: %w", createErr)
			}
			if winner.Status == "suspended" {
				return nil, false, ErrUserSuspended
			}
			s.logger.Info("GE exchange: resolved to existing user after email collision",
				"email", normalizedEmail, "winner_user_id", winner.ID,
				"sub", identity.Subject)
			return winner, false, nil
		}
		return nil, false, fmt.Errorf("create user: %w", createErr)
	}

	s.logger.Info("GE exchange: provisioned new user",
		"email", normalizedEmail,
		"user_id", newUser.ID,
		"sub", identity.Subject)

	return newUser, true, nil
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

// handleGEGoogleExchange handles POST /api/v1/auth/integrations/google/exchange.
//
// Endpoint-specific defenses (applied before credential validation):
//   - Body size: http.MaxBytesReader caps the body at geExchangeMaxBodyBytes
//     (8 KB). Oversize requests return 413 without invoking the Google
//     validator, user store, or binding store.
//   - Rate limit: per-client-IP token bucket (geExchangeRateLimiter) enforced
//     before credential validation or outbound Google requests. Returns 429
//     with Retry-After header. Client IP extraction uses safe trusted-proxy
//     semantics via geExchangeClientIP.
func (s *Server) handleGEGoogleExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Rate limit — before any credential validation or outbound calls.
	if s.geExchangeRateLimiter != nil {
		trustedNets := parseTrustedProxies(s.config.TrustedProxies)
		clientIP := geExchangeClientIP(r, trustedNets)
		allowed, retryAfter := s.geExchangeRateLimiter.Allow(clientIP)
		if !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited,
				fmt.Sprintf("rate limit exceeded; retry in %ds", retryAfter), nil)
			return
		}
	}

	if s.geExchangeService == nil {
		writeError(w, http.StatusUnauthorized, "not_configured",
			"GE Google exchange is not configured", nil)
		return
	}

	// Body size limit — before JSON decode/validation.
	r.Body = http.MaxBytesReader(w, r.Body, geExchangeMaxBodyBytes)

	var req ExchangeRequest
	if err := readJSON(r, &req); err != nil {
		if isMaxBytesError(err) {
			writeError(w, http.StatusRequestEntityTooLarge, ErrCodeInvalidRequest,
				"request body too large", nil)
			return
		}
		BadRequest(w, "invalid request body")
		return
	}

	resp, statusCode, err := s.geExchangeService.Exchange(r.Context(), &req)
	if err != nil {
		code := "exchange_failed"
		switch statusCode {
		case http.StatusBadRequest:
			code = "bad_request"
		case http.StatusUnauthorized:
			code = "invalid_credential"
		case http.StatusForbidden:
			code = "forbidden"
		case http.StatusTooManyRequests:
			code = "rate_limited"
		}
		writeError(w, statusCode, code, err.Error(), nil)
		return
	}

	writeJSON(w, statusCode, resp)
}

// isMaxBytesError checks whether an error is an *http.MaxBytesError (body
// exceeded the limit set by MaxBytesReader).
func isMaxBytesError(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.Is(err, http.ErrBodyReadAfterClose) || errors.As(err, &maxBytesErr)
}
