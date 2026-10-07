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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	// AgentTokenIssuer is the issuer claim for agent tokens.
	AgentTokenIssuer = "scion-hub"
	// AgentTokenAudience is the audience claim for agent tokens.
	AgentTokenAudience = "scion-hub-api"
	// DefaultAgentTokenDuration is the default validity duration for agent tokens.
	// Tokens are refreshed by sciontool 2 hours before expiry.
	DefaultAgentTokenDuration = 10 * time.Hour
)

// AgentTokenScope represents the authorized scopes for an agent.
type AgentTokenScope string

const (
	// ScopeAgentStatusUpdate allows the agent to update its own status.
	ScopeAgentStatusUpdate AgentTokenScope = "agent:status:update"
	// ScopeAgentLogAppend allows the agent to append logs.
	ScopeAgentLogAppend AgentTokenScope = "agent:log:append"
	// ScopeProjectSecretRead allows the agent to read project secrets.
	ScopeProjectSecretRead AgentTokenScope = "project:secret:read"
	// ScopeAgentCreate allows the agent to create sub-agents within the same project.
	ScopeAgentCreate AgentTokenScope = "project:agent:create"
	// ScopeAgentSAAssign allows the agent to assign a GCP service account to
	// an agent within the same project. Split from ScopeAgentCreate
	// (ptone/scion#2339) so the two permissions are granted and checked
	// independently. A JWT minted with the combined ScopeAgentCreate before
	// the split keeps authorizing gcp_service_account.assign until it next
	// refreshes onto the split scopes — see effectiveAgentScopes (authz.go)
	// and CurrentAgentScopeSchema.
	ScopeAgentSAAssign AgentTokenScope = "project:agent:sa_assign"
	// ScopeAgentLifecycle allows the agent to start/stop/restart agents within the same project.
	ScopeAgentLifecycle AgentTokenScope = "project:agent:lifecycle"
	// ScopeAgentNotify allows the agent to create notification subscriptions within the same project.
	ScopeAgentNotify AgentTokenScope = "project:agent:notify"
	// ScopeAgentTokenRefresh allows the agent to refresh its own token before expiry.
	ScopeAgentTokenRefresh AgentTokenScope = "agent:token:refresh"
	// ScopeAgentPortForward allows the agent to register ports and hold port-forward tunnels.
	ScopeAgentPortForward AgentTokenScope = "agent:port:forward"
	// ScopeIdentityToken grants the ability to request OIDC identity tokens.
	ScopeIdentityToken AgentTokenScope = "agent:identity:token"
	// ScopeProjectRead grants read access to project resources (agents, templates,
	// skills, harness configs, projects). Enforced by checkAgentReadScope().
	ScopeProjectRead AgentTokenScope = "project:read"
	// ScopeProjectTemplateWrite allows the agent to create and update templates
	// within its own project. Deliberately excludes deletion: publishing a
	// template is recoverable, removing one another agent depends on is not.
	//
	// Granted only to agent-role-full, whose holders can already create agents.
	// An agent that can spawn progeny can already determine what those agents
	// do; being able to publish the template it spawns them from is the same
	// authority expressed once instead of per-agent.
	ScopeProjectTemplateWrite AgentTokenScope = "project:template:write"
	// ScopeProjectArtifactRead allows the agent to use the artifact service
	// to read artifacts (artifact.read). It is a ceiling-optional role scope
	// (ceilingOptionalRoleScopes): readonly, baseline and full carry it, but
	// a mint drops it when the source ceiling lacks artifact.read, so tokens
	// issued under ceilings that predate artifacts do not gain it.
	// The hub's artifacts.Host does not serve an agent whose token lacks it,
	// whatever artifact grants exist.
	ScopeProjectArtifactRead AgentTokenScope = "project:artifact:read"
	// ScopeProjectArtifactWrite allows the agent to publish artifacts, and
	// new versions of them, homed in its own project (artifact.create,
	// artifact.update). Deliberately excludes deletion and grant
	// management. Minted for the baseline and full agent roles
	// (ScopesForRole).
	ScopeProjectArtifactWrite AgentTokenScope = "project:artifact:write"
	// ScopeAgentSetMessageMode allows the agent to change message mode
	// for agents within the same project.
	ScopeAgentSetMessageMode AgentTokenScope = "project:agent:set_message_mode"
	// ScopeGCPTokenPrefix is the prefix for GCP token scopes.
	// Full scope format: "project:gcp:token:<sa-id>"
	ScopeGCPTokenPrefix = "project:gcp:token:"
)

// CurrentAgentScopeSchema is stamped onto every agent JWT this hub mints or
// refreshes (AgentTokenClaims.ScopeSchema), so a verified token's wire form
// records which scope vocabulary it was minted under. It increments only
// when a permission moves off a shared scope onto its own (ptone/scion#2339
// is schema 1: gcp_service_account.assign gets project:agent:sa_assign
// instead of sharing project:agent:create).
//
// Authorization code never reads ScopeSchema; ValidateAgentToken alone reads
// it, once, to set AgentTokenClaims.legacyScopeSchema, the unexported field
// authz.go's effectiveAgentScopes actually keys on. ValidateAgentToken sets
// legacyScopeSchema only when a verified token's wire form carries no
// scope_schema claim (ScopeSchema reads as its Go zero value, 0, because
// SignAgentToken — the only agent-JWT signer — has always stamped a
// nonzero value since this field existed, so a verified 0 can only mean
// "minted before the field existed"; any other value, including one from a
// schema this package does not yet know about, is left as not legacy). Every
// in-process AgentTokenClaims built directly as a Go literal — every
// stored-record identity, scheduled-dispatch creator identity, and synthetic
// secret-resolution identity among them — leaves legacyScopeSchema at its
// own zero value (false) and so is NEVER read as legacy, regardless of what
// its Scopes list contains: only a value that came out of ValidateAgentToken
// can be legacy.
//
// legacyScopeSchema's effect may be deleted once no pre-split token can
// still validate. ValidateAgentToken rejects a token once its exp claim
// (its mint time plus the AgentTokenConfig.TokenDuration configured at
// mint) is more than jwt.DefaultLeeway (one minute) in the past, so the
// window closes one minute plus the TokenDuration in effect when the last
// pre-split token was minted after the last hub instance running pre-split
// code stops minting.
const CurrentAgentScopeSchema = 1

// AgentTokenClaims represents the custom claims in an agent JWT.
type AgentTokenClaims struct {
	jwt.Claims
	ProjectID string            `json:"project_id,omitempty"`
	Scopes    []AgentTokenScope `json:"scopes,omitempty"`
	Ancestry  []string          `json:"ancestry,omitempty"` // [root_user, ..., parent_agent]
	// ScopeSchema records which scope vocabulary Scopes was minted under.
	// See CurrentAgentScopeSchema. Metadata only; see legacyScopeSchema for
	// the field that actually gates compatibility behavior.
	ScopeSchema int `json:"scope_schema,omitempty"`
	// RunID is the agent run the token was issued for (agents.run_id at
	// issue). Empty for a token issued without a run.
	RunID string `json:"run_id,omitempty"`
	// legacyScopeSchema is set only by ValidateAgentToken, and only when the
	// verified token's wire form carries no scope_schema claim. It is
	// deliberately unexported (so it is never part of the wire format and
	// can never be set by unmarshaling untrusted input) and deliberately
	// never set by any direct construction of this struct — see
	// CurrentAgentScopeSchema's doc comment for why that is the safe
	// default. Read via authz.go's effectiveAgentScopes /
	// isLegacyPreSplitAgentJWT, never directly.
	legacyScopeSchema bool
}

// AgentTokenConfig holds configuration for agent token generation.
type AgentTokenConfig struct {
	// SigningKey is the secret key used for HS256 signing.
	// In production, use RS256 with a proper key pair.
	SigningKey []byte
	// TokenDuration is how long tokens remain valid.
	TokenDuration time.Duration
}

// AgentTokenService handles agent token generation and validation.
type AgentTokenService struct {
	config AgentTokenConfig
	signer jose.Signer
}

// hashJTI returns the SHA-256 hex hash of a JWT ID (JTI).
func hashJTI(jti string) string {
	h := sha256.Sum256([]byte(jti))
	return hex.EncodeToString(h[:])
}

// errAgentCredentialRevoked is returned by evaluateAgentCredentialStatus when
// the looked-up credential has been revoked.
var errAgentCredentialRevoked = errors.New("agent credential has been revoked")

// errAgentCredentialMissing is returned by evaluateAgentCredentialStatus when
// the credential store reports success but returns no credential record.
// Callers treat it like any other store failure (retryable, not
// authenticated), since the credential's status could not be determined.
var errAgentCredentialMissing = errors.New("credential store returned no credential and no error")

// evaluateAgentCredentialStatus performs the credential-status lookup that
// agent-token authentication requires: given the JTI carried by a validated
// agent token, it looks up the corresponding credential record in credStore
// and classifies the result. The result is one of four outcomes:
//
//   - Found and active: the credential is returned with isLegacy=false and a
//     nil error.
//   - Found and revoked: a nil credential and errAgentCredentialRevoked (test
//     with errors.Is).
//   - Not found (store.ErrNotFound): a nil credential, isLegacy=true, and a
//     nil error. This is the legacy compatibility path for tokens issued
//     before the credential table existed; it is retained pending a product
//     decision and callers must not close or widen it here.
//   - Any other store error: returned verbatim with isLegacy=false and a nil
//     credential. Callers must treat that as a retryable failure — never as
//     successful authentication — since a store error means the credential's
//     status could not actually be determined.
//
// A nil credential with a nil store error is classified as a store failure
// and reported as errAgentCredentialMissing, so it follows the same
// retryable path as any other store error.
func evaluateAgentCredentialStatus(ctx context.Context, credStore store.AgentCredentialStore, jti string) (cred *store.AgentCredential, isLegacy bool, err error) {
	cred, err = credStore.GetAgentCredentialByJTIHash(ctx, hashJTI(jti))
	switch {
	case err == nil:
		if cred == nil {
			return nil, false, errAgentCredentialMissing
		}
		if cred.RevokedAt != nil {
			return nil, false, errAgentCredentialRevoked
		}
		return cred, false, nil
	case errors.Is(err, store.ErrNotFound):
		return nil, true, nil
	default:
		return nil, false, err
	}
}

// NewAgentTokenService creates a new agent token service.
// If signingKey is empty, a random key is generated (suitable for development).
func NewAgentTokenService(config AgentTokenConfig) (*AgentTokenService, error) {
	if len(config.SigningKey) == 0 {
		// Generate a random key for development/testing
		config.SigningKey = make([]byte, 32)
		if _, err := rand.Read(config.SigningKey); err != nil {
			return nil, fmt.Errorf("failed to generate signing key: %w", err)
		}
	}

	if config.TokenDuration == 0 {
		config.TokenDuration = DefaultAgentTokenDuration
	}

	// Create signer using HS256 (symmetric)
	// In production, consider RS256 (asymmetric) for better security
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: config.SigningKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %w", err)
	}

	return &AgentTokenService{
		config: config,
		signer: signer,
	}, nil
}

// AgentTokenGrant is what an agent token is authorized to carry: the
// subject, project, scopes and ancestry. It is produced before the token's
// run is known and signed once it is (SignAgentToken).
type AgentTokenGrant struct {
	AgentID   string
	ProjectID string
	Scopes    []AgentTokenScope
	Ancestry  []string
}

// SignAgentToken signs a token for grant, bound to runID, and returns it
// with the credential row describing it. It has no side effects: the
// caller records the credential, and must not hand out the token unless
// that record succeeded.
func (s *AgentTokenService) SignAgentToken(grant AgentTokenGrant, runID string) (string, *store.AgentCredential, error) {
	now := time.Now()

	scopes := grant.Scopes
	// Default to status update scope if none provided
	if len(scopes) == 0 {
		scopes = []AgentTokenScope{ScopeAgentStatusUpdate}
	}

	jti := generateTokenID()
	expiry := now.Add(s.config.TokenDuration)
	claims := AgentTokenClaims{
		Claims: jwt.Claims{
			Issuer:    AgentTokenIssuer,
			Subject:   grant.AgentID,
			Audience:  jwt.Audience{AgentTokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			Expiry:    jwt.NewNumericDate(expiry),
			NotBefore: jwt.NewNumericDate(now),
			ID:        jti,
		},
		ProjectID:   grant.ProjectID,
		Scopes:      scopes,
		Ancestry:    grant.Ancestry,
		ScopeSchema: CurrentAgentScopeSchema,
		RunID:       runID,
	}

	token, err := jwt.Signed(s.signer).Claims(claims).Serialize()
	if err != nil {
		return "", nil, fmt.Errorf("failed to sign token: %w", err)
	}
	cred := &store.AgentCredential{
		AgentID:      grant.AgentID,
		ProjectID:    grant.ProjectID,
		TokenJTIHash: hashJTI(jti),
		RunID:        runID,
		IssuedAt:     now,
		ExpiresAt:    expiry,
	}
	return token, cred, nil
}

// ValidateAgentToken validates a JWT and returns the claims if valid.
func (s *AgentTokenService) ValidateAgentToken(tokenString string) (*AgentTokenClaims, error) {
	token, err := jwt.ParseSigned(tokenString, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	var claims AgentTokenClaims
	if err := token.Claims(s.config.SigningKey, &claims); err != nil {
		return nil, fmt.Errorf("failed to verify token: %w", err)
	}

	// Validate standard claims
	expected := jwt.Expected{
		Issuer:      AgentTokenIssuer,
		AnyAudience: jwt.Audience{AgentTokenAudience},
		Time:        time.Now(),
	}

	if err := claims.Validate(expected); err != nil {
		return nil, fmt.Errorf("token validation failed: %w", err)
	}

	// A verified token whose wire form carried no scope_schema claim (reads
	// as the Go zero value, 0) predates CurrentAgentScopeSchema entirely:
	// SignAgentToken has always stamped a nonzero schema since the field
	// existed, and this method only returns claims that passed HS256
	// verification against this hub's own signing key, so no other signer's
	// output reaches this line. This is the one place legacyScopeSchema is
	// ever set — see CurrentAgentScopeSchema's doc comment.
	if claims.ScopeSchema == 0 {
		claims.legacyScopeSchema = true
	}

	return &claims, nil
}

// HasScope checks if the claims include the specified scope.
func (c *AgentTokenClaims) HasScope(scope AgentTokenScope) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// HasScopePrefix checks if the claims include any scope that starts with the given prefix.
func (c *AgentTokenClaims) HasScopePrefix(prefix string) bool {
	for _, s := range c.Scopes {
		if strings.HasPrefix(string(s), prefix) {
			return true
		}
	}
	return false
}

// GCPTokenScopeForSA returns the full GCP token scope string for a given service account ID.
func GCPTokenScopeForSA(saID string) AgentTokenScope {
	return AgentTokenScope(ScopeGCPTokenPrefix + saID)
}

// generateTokenID generates a unique token ID.
func generateTokenID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// agentContextKey is the key for storing agent claims in the request context.
type agentContextKey struct{}

// GetAgentFromContext retrieves the agent claims from the request context.
func GetAgentFromContext(ctx context.Context) *AgentTokenClaims {
	if claims, ok := ctx.Value(agentContextKey{}).(*AgentTokenClaims); ok {
		return claims
	}
	return nil
}

// AgentAuthMiddleware creates middleware that validates agent tokens.
// It looks for tokens in the Authorization header (Bearer) or X-Scion-Agent-Token header.
// If valid, it adds the agent claims to the request context.
func (s *AgentTokenService) AgentAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to extract token from headers
		token := extractAgentToken(r)
		if token == "" {
			// No agent token found, continue to next middleware (may have other auth)
			next.ServeHTTP(w, r)
			return
		}

		// Validate the token
		claims, err := s.ValidateAgentToken(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
				"invalid agent token: "+err.Error(), nil)
			return
		}

		// Add claims to context
		ctx := context.WithValue(r.Context(), agentContextKey{}, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractAgentToken extracts the agent token from the request.
// It checks both the Authorization header and X-Scion-Agent-Token header.
func extractAgentToken(r *http.Request) string {
	// Check X-Scion-Agent-Token header first (takes precedence)
	if token := r.Header.Get("X-Scion-Agent-Token"); token != "" {
		return token
	}

	// Check Authorization header for Bearer token
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return ""
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}

	return parts[1]
}

// RequireAgentScope returns a middleware that requires the agent to have a specific scope.
// It must be used after AgentAuthMiddleware.
func RequireAgentScope(scope AgentTokenScope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := GetAgentFromContext(r.Context())
			if claims == nil {
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"agent authentication required", nil)
				return
			}

			if !claims.HasScope(scope) {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					fmt.Sprintf("missing required scope: %s", scope), nil)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequireAgentSelfAccess returns a middleware that ensures the agent can only access its own resources.
// It extracts the agent ID from the URL path and compares it with the token's subject.
func RequireAgentSelfAccess(pathPrefix string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := GetAgentFromContext(r.Context())
			if claims == nil {
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"agent authentication required", nil)
				return
			}

			// Extract agent ID from path
			agentID, _ := extractAction(r, pathPrefix)
			if agentID == "" {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
					"agent ID required in path", nil)
				return
			}

			// Verify the agent is accessing its own resource
			if agentID != claims.Subject {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"agents can only access their own resources", nil)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// checkAgentReadScope verifies that agent callers have ScopeProjectRead.
// Returns true if the request should proceed, false if a 403 was written.
// User callers are not affected — the check only applies when the caller
// is an agent (i.e., GetAgentIdentityFromContext returns non-nil).
//
// Backward compatibility note: legacy agents created before the role system
// may not have ScopeProjectRead in their JWT. Token refresh re-derives scopes
// from the agent's stored role (via agentRoleAndScopes → ScopesForRole),
// which includes ScopeProjectRead for baseline and above. Token refresh is
// automatic and frequent, so active legacy agents will acquire the scope
// on their next refresh cycle.
func checkAgentReadScope(w http.ResponseWriter, r *http.Request) bool {
	if agentIdent := GetAgentIdentityFromContext(r.Context()); agentIdent != nil {
		if !agentIdent.HasScope(ScopeProjectRead) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Missing required scope: project:read", nil)
			return false
		}
	}
	return true
}
