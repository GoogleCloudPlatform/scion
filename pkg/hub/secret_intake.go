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
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
)

// ============================================================================
// Secret Intake — Data Types
// ============================================================================

const (
	// Defaults.
	defaultIntakeTTL = 15 * time.Minute
	maxIntakeTTL     = 1 * time.Hour
)

// SecretIntake holds the state for a secret intake link.
type SecretIntake struct {
	ID             string     `json:"id"`
	Key            string     `json:"key"`
	Scope          string     `json:"scope"`
	ScopeID        string     `json:"scopeId"`
	SecretType     string     `json:"type"`
	Target         string     `json:"target"`
	Description    string     `json:"description"`
	UserID         string     `json:"userId"`
	Channel        string     `json:"channel,omitempty"`
	ChannelContext string     `json:"channelContext,omitempty"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	CreatedAt      time.Time  `json:"createdAt"`
	Consumed       bool       `json:"consumed"`
	ConsumedAt     *time.Time `json:"consumedAt,omitempty"`
}

// IntakeTokenClaims are the JWT claims embedded in the intake URL fragment.
type IntakeTokenClaims struct {
	jwt.Claims
	Key         string `json:"key"`
	Scope       string `json:"scope"`
	ScopeID     string `json:"scope_id"`
	SecretType  string `json:"type"`
	Target      string `json:"target,omitempty"`
	Description string `json:"description,omitempty"`
}

// ============================================================================
// Secret Intake Service (in-memory, short-lived)
// ============================================================================

// SecretIntakeService manages secret intake links. Records are kept
// in memory because they are short-lived (max 1 hour). This mirrors the
// TelegramLinkService pattern in the same package.
type SecretIntakeService struct {
	mu      sync.Mutex
	intakes map[string]*SecretIntake // intake ID -> intake

	closeOnce sync.Once
	done      chan struct{}
}

// NewSecretIntakeService creates a new service and starts a background
// goroutine to clean up expired intakes.
func NewSecretIntakeService() *SecretIntakeService {
	s := &SecretIntakeService{
		intakes: make(map[string]*SecretIntake),
		done:    make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

// Close stops the background cleanup goroutine.
func (s *SecretIntakeService) Close() {
	s.closeOnce.Do(func() { close(s.done) })
}

// CreateIntake registers a new intake and returns its ID.
func (s *SecretIntakeService) CreateIntake(intake *SecretIntake) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intakes[intake.ID] = intake
}

// GetIntake returns a copy of an intake by ID, or nil if not found.
// The returned struct is safe to read without holding any lock.
func (s *SecretIntakeService) GetIntake(id string) *SecretIntake {
	s.mu.Lock()
	defer s.mu.Unlock()
	intake, ok := s.intakes[id]
	if !ok {
		return nil
	}
	cp := *intake
	return &cp
}

// ConsumeIntake atomically marks an intake as consumed if it is still valid.
// Returns the intake copy and an error code (empty on success).
// Error codes: "not_found", "expired", "already_consumed".
func (s *SecretIntakeService) ConsumeIntake(id string) (*SecretIntake, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	intake, ok := s.intakes[id]
	if !ok {
		return nil, "not_found"
	}

	if time.Now().After(intake.ExpiresAt) {
		cp := *intake
		return &cp, "expired"
	}

	if intake.Consumed {
		cp := *intake
		return &cp, "already_consumed"
	}

	now := time.Now()
	intake.Consumed = true
	intake.ConsumedAt = &now

	cp := *intake
	return &cp, ""
}

func (s *SecretIntakeService) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			now := time.Now()

			s.mu.Lock()
			for id, intake := range s.intakes {
				// Remove intakes older than 1 hour past expiry.
				if now.After(intake.ExpiresAt.Add(1 * time.Hour)) {
					delete(s.intakes, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

// ============================================================================
// Handlers
// ============================================================================

// handleSecretIntake routes POST /api/v1/secret-intake.
func (s *Server) handleSecretIntake(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	s.handleCreateIntake(w, r)
}

// handleSecretIntakeByID routes /api/v1/secret-intake/{id}/{action}.
func (s *Server) handleSecretIntakeByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/secret-intake")

	if id == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "intake ID is required", nil)
		return
	}

	switch action {
	case "store":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		s.handleStoreViaIntake(w, r, id)
	default:
		NotFound(w, "intake action")
	}
}

// handleCreateIntake creates a new secret intake link.
// POST /api/v1/secret-intake
// Authorization: Bearer <user-or-agent-token>
func (s *Server) handleCreateIntake(w http.ResponseWriter, r *http.Request) {
	// Require user or agent authentication.
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return
	}

	// Determine the user ID.
	var userID string
	if user := GetUserIdentityFromContext(r.Context()); user != nil {
		userID = user.ID()
	} else if agent := GetAgentIdentityFromContext(r.Context()); agent != nil {
		// For agent-created intakes, use the agent's origin user ID.
		userID = agent.OriginUserID()
		if userID == "" {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Agent must have an origin user to create intake links", nil)
			return
		}
	} else {
		Unauthorized(w)
		return
	}

	var req struct {
		Key            string `json:"key"`
		Scope          string `json:"scope"`
		ScopeID        string `json:"scope_id"`
		SecretType     string `json:"type"`
		Target         string `json:"target"`
		Description    string `json:"description"`
		TTLSeconds     int    `json:"ttl_seconds"`
		Channel        string `json:"channel"`
		ChannelContext string `json:"channel_context"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	// Validate required fields.
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, "key is required", nil)
		return
	}
	if req.Scope == "" {
		req.Scope = "user"
	}
	if req.Scope == "project" && req.ScopeID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, "scope_id is required for project scope", nil)
		return
	}
	if req.SecretType == "" {
		req.SecretType = "environment"
	}

	// Compute TTL.
	ttl := defaultIntakeTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
		if ttl > maxIntakeTTL {
			ttl = maxIntakeTTL
		}
	}

	now := time.Now()
	intakeID := uuid.NewString()
	expiresAt := now.Add(ttl)

	// Create intake record.
	intake := &SecretIntake{
		ID:             intakeID,
		Key:            req.Key,
		Scope:          req.Scope,
		ScopeID:        req.ScopeID,
		SecretType:     req.SecretType,
		Target:         req.Target,
		Description:    req.Description,
		UserID:         userID,
		Channel:        req.Channel,
		ChannelContext: req.ChannelContext,
		ExpiresAt:      expiresAt,
		CreatedAt:      now,
	}

	if s.secretIntakeService == nil {
		InternalError(w)
		return
	}
	s.secretIntakeService.CreateIntake(intake)

	// Generate JWT for the URL fragment.
	token, err := s.generateIntakeToken(intake)
	if err != nil {
		slog.Error("Failed to generate intake token", "error", err, "intakeID", intakeID)
		InternalError(w)
		return
	}

	// Build the intake URL.
	baseURL := s.config.HubEndpoint
	if baseURL == "" {
		baseURL = fmt.Sprintf("http://localhost:%d", s.config.Port)
	}
	url := baseURL + "/intake#" + token

	slog.Info("Secret intake link created",
		"intake_id", intakeID,
		"key", req.Key,
		"scope", req.Scope,
		"user_id", userID,
		"expires_at", expiresAt.Format(time.RFC3339),
	)

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"url":        url,
		"expires_at": expiresAt.Format(time.RFC3339),
		"intake_id":  intakeID,
	})
}

// handleStoreViaIntake stores a secret value via an authenticated intake link.
// POST /api/v1/secret-intake/{id}/store
// Authorization: Bearer <user-token> (user must be logged in)
func (s *Server) handleStoreViaIntake(w http.ResponseWriter, r *http.Request, intakeID string) {
	// Require authenticated user.
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	if s.secretIntakeService == nil {
		InternalError(w)
		return
	}

	// Limit request body to 128 KiB to prevent memory exhaustion.
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)

	var req struct {
		Token string `json:"token"`
		Value string `json:"value"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}
	if req.Token == "" || req.Value == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, "token and value are required", nil)
		return
	}

	// Validate the JWT.
	claims, err := s.validateIntakeToken(req.Token)
	if err != nil {
		slog.Debug("Intake token validation failed", "error", err, "intakeID", intakeID)
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid or expired token", nil)
		return
	}

	// Verify the JWT's jti matches the intake ID in the URL.
	if claims.ID != intakeID {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "token does not match intake", nil)
		return
	}

	// Atomically consume the intake.
	intake, errCode := s.secretIntakeService.ConsumeIntake(intakeID)
	if errCode != "" {
		switch errCode {
		case "not_found":
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "intake not found", nil)
		case "expired":
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "intake expired", nil)
		case "already_consumed":
			writeError(w, http.StatusGone, "already_consumed", "intake already used", nil)
		}
		return
	}

	// Store the secret via the secret backend.
	backend := s.GetSecretBackend()
	if backend == nil {
		slog.Error("No secret backend configured for intake store", "intake_id", intakeID)
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"secret backend not available", nil)
		return
	}

	scopeID := intake.ScopeID
	if intake.Scope == "user" && scopeID == "" {
		scopeID = intake.UserID
	}

	input := &secret.SetSecretInput{
		Name:        intake.Key,
		Value:       req.Value,
		SecretType:  intake.SecretType,
		Target:      intake.Target,
		Scope:       intake.Scope,
		ScopeID:     scopeID,
		Description: intake.Description,
		CreatedBy:   user.ID(),
	}

	if _, _, err := backend.Set(r.Context(), input); err != nil {
		slog.Error("Failed to store secret via intake", "error", err, "intake_id", intakeID)
		InternalError(w)
		return
	}

	slog.Info("Secret stored via intake",
		"intake_id", intakeID,
		"key", intake.Key,
		"scope", intake.Scope,
		"scope_id", scopeID,
		"user_id", user.ID(),
		"channel", intake.Channel,
	)

	// TODO: Send notification to originating chat channel when channel
	// notification infrastructure is wired up. The intake record carries
	// Channel and ChannelContext for this purpose.

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "stored",
		"key":    intake.Key,
	})
}

// ============================================================================
// JWT helpers (reuse the Hub's user signing key)
// ============================================================================

// generateIntakeToken creates a signed JWT for a secret intake link.
func (s *Server) generateIntakeToken(intake *SecretIntake) (string, error) {
	svc := s.GetUserTokenService()
	if svc == nil {
		return "", fmt.Errorf("user token service not initialized")
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: svc.config.SigningKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create signer: %w", err)
	}

	claims := IntakeTokenClaims{
		Claims: jwt.Claims{
			Issuer:   UserTokenIssuer,
			Subject:  "secret-intake",
			IssuedAt: jwt.NewNumericDate(intake.CreatedAt),
			Expiry:   jwt.NewNumericDate(intake.ExpiresAt),
			ID:       intake.ID,
		},
		Key:         intake.Key,
		Scope:       intake.Scope,
		ScopeID:     intake.ScopeID,
		SecretType:  intake.SecretType,
		Target:      intake.Target,
		Description: intake.Description,
	}

	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", fmt.Errorf("failed to sign intake token: %w", err)
	}
	return token, nil
}

// validateIntakeToken validates a JWT intake token and returns its claims.
func (s *Server) validateIntakeToken(tokenString string) (*IntakeTokenClaims, error) {
	svc := s.GetUserTokenService()
	if svc == nil {
		return nil, fmt.Errorf("user token service not initialized")
	}

	parsed, err := jwt.ParseSigned(tokenString, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	var claims IntakeTokenClaims
	if err := parsed.Claims(svc.config.SigningKey, &claims); err != nil {
		return nil, fmt.Errorf("failed to verify token: %w", err)
	}

	// Validate standard claims (issuer, expiry).
	expected := jwt.Expected{
		Issuer: UserTokenIssuer,
		Time:   time.Now(),
	}
	if err := claims.Claims.Validate(expected); err != nil {
		return nil, fmt.Errorf("token validation failed: %w", err)
	}

	// Verify subject is "secret-intake".
	if claims.Subject != "secret-intake" {
		return nil, fmt.Errorf("invalid token subject: %s", claims.Subject)
	}

	return &claims, nil
}
