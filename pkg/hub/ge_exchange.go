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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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

// GEExchangeService handles the credential exchange flow.
type GEExchangeService struct {
	config           GEGoogleExchangeConfig
	validator        GoogleCredentialValidator
	userTokenService *UserTokenService
	resolver         *GoogleIdentityResolver
	logger           *slog.Logger
	nowFunc          func() time.Time
}

// NewGEExchangeService creates a new GE exchange service. resolver is shared
// with the external-bearer auth path (server.go's New constructs one
// GoogleIdentityResolver and passes the same instance to both), so the
// exchange endpoint and external-bearer authentication reach identical
// resolution decisions — including the admin_emails-aware role for newly
// provisioned users — during the soak between the two mechanisms.
func NewGEExchangeService(
	config GEGoogleExchangeConfig,
	validator GoogleCredentialValidator,
	userTokenService *UserTokenService,
	resolver *GoogleIdentityResolver,
	logger *slog.Logger,
) *GEExchangeService {
	if config.TokenTTL == 0 {
		config.TokenTTL = DefaultGETokenTTL
	}
	if config.TokenTTL > MaxGETokenTTL {
		config.TokenTTL = MaxGETokenTTL
	}
	return &GEExchangeService{
		config:           config,
		validator:        validator,
		userTokenService: userTokenService,
		resolver:         resolver,
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
		// Must be checked before ErrGoogleFieldDisagreement: a
		// service-account ID token with a disagreeing azp is wrapped with
		// both (google_credential_validator.go's SA azp/sub check), and this
		// case must win so the exchange keeps its SA rejection contract
		// (403) for that shape too, instead of the generic 401 the
		// disagreement case below would otherwise give it.
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

	if identity == nil {
		// A GoogleCredentialValidator that returns (nil, nil) is a contract
		// violation, not a verification failure — treat it as a generic
		// internal error rather than reaching the nil pointer dereference on
		// identity.IsServiceAccount below.
		s.logger.Error("GE exchange: validator returned no identity",
			"credential_type", credType)
		return nil, http.StatusInternalServerError, fmt.Errorf("credential validation failed")
	}

	// Step 1.5: reject service-account credentials for user exchange. The
	// validator only classifies IsServiceAccount and each caller decides
	// whether to admit it; the exchange endpoint rejects SA credentials
	// outright.
	if identity.IsServiceAccount {
		s.logger.Warn("GE exchange: rejecting service account credential",
			"credential_type", credType,
			"sub", identity.Subject,
			"email", identity.Email)
		return nil, http.StatusForbidden, fmt.Errorf("service account credentials not accepted for user exchange")
	}

	// Step 2: Resolve to a local Hub user via external identity binding.
	user, err := s.resolver.Resolve(ctx, identity, ResolvePolicy{})
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
			s.recordGEExchange(GEExchangeOutcomeRateLimited)
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited,
				fmt.Sprintf("rate limit exceeded; retry in %ds", retryAfter), nil)
			return
		}
	}

	if s.geExchangeService == nil {
		s.recordGEExchange(GEExchangeOutcomeNotConfigured)
		writeError(w, http.StatusUnauthorized, "not_configured",
			"GE Google exchange is not configured", nil)
		return
	}

	// Body size limit — before JSON decode/validation.
	r.Body = http.MaxBytesReader(w, r.Body, geExchangeMaxBodyBytes)

	var req ExchangeRequest
	if err := readJSON(r, &req); err != nil {
		s.recordGEExchange(GEExchangeOutcomeInvalidRequest)
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
		outcome := GEExchangeOutcomeExchangeFailed
		switch statusCode {
		case http.StatusBadRequest:
			code, outcome = "bad_request", GEExchangeOutcomeBadRequest
		case http.StatusUnauthorized:
			code, outcome = "invalid_credential", GEExchangeOutcomeInvalidCredential
		case http.StatusForbidden:
			code, outcome = "forbidden", GEExchangeOutcomeForbidden
		case http.StatusTooManyRequests:
			code, outcome = "rate_limited", GEExchangeOutcomeRateLimited
		}
		s.recordGEExchange(outcome)
		writeError(w, statusCode, code, err.Error(), nil)
		return
	}

	s.recordGEExchange(GEExchangeOutcomeOK)
	writeJSON(w, statusCode, resp)
}

// recordGEExchange records one exchange-endpoint outcome, nil-safe against
// s.geExchangeMetrics never having been wired — true only for a Server not
// built through New() (most hand-built `&Server{}` tests in this package);
// New() always wires the in-process default (the snapshot recorder),
// optionally replaced later by SetGEExchangeMetrics with an OTel-backed one.
// It also enforces the closed label set at this boundary: an invalid outcome
// is dropped (with a warning naming only the label and its type, never the
// value) rather than reaching any recorder.
func (s *Server) recordGEExchange(outcome GEExchangeOutcome) {
	if !outcome.valid() {
		slog.Warn("ge exchange: dropping metric record: invalid label", "label", "outcome", "type", fmt.Sprintf("%T", outcome))
		return
	}
	if s.geExchangeMetrics == nil {
		return
	}
	s.geExchangeMetrics.RecordGEExchangeRequest(outcome)
}

// isMaxBytesError checks whether an error is an *http.MaxBytesError (body
// exceeded the limit set by MaxBytesReader).
func isMaxBytesError(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.Is(err, http.ErrBodyReadAfterClose) || errors.As(err, &maxBytesErr)
}
