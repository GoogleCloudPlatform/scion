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
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AuthConfig holds authentication configuration.
type AuthConfig struct {
	// Mode is the authentication mode: "production", "development", "testing"
	Mode string
	// DevAuthEnabled enables development token authentication
	DevAuthEnabled bool
	// DevAuthToken is the valid development token
	DevAuthToken string
	// DevUserCfg holds identity overrides for the development user
	DevUserCfg DevUserConfig
	// AgentTokenSvc handles agent JWT validation
	AgentTokenSvc *AgentTokenService
	// UserTokenSvc handles user JWT validation
	UserTokenSvc *UserTokenService
	// UATSvc handles user access token validation
	UATSvc *UserAccessTokenService
	// BrokerAuthSvc is the broker HMAC authentication service.
	//
	// UnifiedAuthMiddleware does not validate broker signatures itself:
	// BrokerAuthMiddleware, which runs later in the chain, does. This field tells
	// UnifiedAuthMiddleware whether that downstream validator is actually
	// installed and active (it is only installed when the service is non-nil, see
	// Server.applyMiddleware, and it no-ops when the service is disabled).
	//
	// When it is not, a request carrying X-Scion-Broker-ID must be rejected rather
	// than passed through, because nothing further down the chain will ever
	// establish an identity for it. See issue #591.
	BrokerAuthSvc *BrokerAuthService
	// TrustedProxies is a list of trusted proxy IPs/CIDRs
	TrustedProxies []string
	// ProxyAuthenticator is the configured proxy authenticator (for proxy auth mode).
	// When set, it replaces the legacy IP-only extractProxyUser path.
	ProxyAuthenticator ProxyAuthenticator
	// ProxyUserProvisioner is a function that provisions a user from a verified
	// proxy identity. It runs provisionUser and returns the stored user.
	// Required when ProxyAuthenticator is set.
	ProxyUserProvisioner func(ctx context.Context, info *ProxyUserInfo) (UserIdentity, error)
	// AuthMode is the exclusive human auth mode: "oauth", "proxy", "dev".
	AuthMode string
	// FederationAuth points to the server's atomic.Pointer for the
	// FederationAuthenticator. nil when federation was never configured.
	// The middleware loads from this pointer on each request to see
	// hot-reloaded authenticators.
	FederationAuth *atomic.Pointer[FederationAuthenticator]
	// CredentialStore handles agent credential validation (Phase 1H).
	// When non-nil, agent tokens are validated against persistent credential state.
	CredentialStore store.AgentCredentialStore
	// UserStore enables per-request user-status checks (e.g. suspension
	// enforcement) for self-contained credentials like JWTs that do not
	// themselves hit the database.
	UserStore store.UserStore
	// Debug enables verbose logging
	Debug bool
	// Logger is the subsystem logger for auth middleware (defaults to slog.Default())
	Logger *slog.Logger
}

// tokenType represents the type of authentication token.
type tokenType int

const (
	tokenTypeUnknown tokenType = iota
	tokenTypeDev
	tokenTypeUser
	tokenTypeUAT
	tokenTypeAgent
)

// brokerAuthActive reports whether BrokerAuthMiddleware will actually validate
// broker HMAC signatures for this service. It mirrors that middleware's own skip
// conditions exactly (nil service, or service disabled) so that the two cannot
// drift: if this returns false, nothing downstream authenticates a broker request.
func brokerAuthActive(svc *BrokerAuthService) bool {
	return svc != nil && svc.config.Enabled
}

// UnifiedAuthMiddleware creates middleware that handles all authentication types.
// It processes tokens in priority order:
// 1. Agent tokens (X-Scion-Agent-Token or agent JWT in Bearer)
// 2. Broker HMAC auth (X-Scion-Broker-ID header) - deferred to BrokerAuthMiddleware
// when that middleware is installed and enabled; rejected outright when it is not
// 3. Development tokens (scion_dev_* prefix)
// 4. User access tokens (scion_pat_* prefix)
// 5. User JWTs
// 6. Trusted proxy headers
func UnifiedAuthMiddleware(cfg AuthConfig) func(http.Handler) http.Handler {
	// Parse trusted proxy CIDRs
	trustedNets := parseTrustedProxies(cfg.TrustedProxies)
	devUser := NewDevUser(cfg.DevUserCfg)
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			if cfg.Debug {
				authHeader := r.Header.Get("Authorization")
				hasAuth := authHeader != ""
				authPrefix := ""
				if len(authHeader) > 20 {
					authPrefix = authHeader[:20] + "..."
				} else if hasAuth {
					authPrefix = authHeader
				}
				log.Debug("Auth check",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Bool("has_auth", hasAuth),
					slog.String("auth_prefix", authPrefix),
				)
			}

			// Skip auth for unauthenticated endpoints (health checks, CLI OAuth)
			if isUnauthenticatedEndpoint(r.URL.Path) {
				if cfg.Debug {
					log.Debug("Skipping auth for unauthenticated endpoint", "path", r.URL.Path)
				}
				next.ServeHTTP(w, r)
				return
			}

			// Step 1: Try agent token (X-Scion-Agent-Token header or agent JWT)
			if token := extractAgentToken(r); token != "" {
				if cfg.AgentTokenSvc != nil {
					if claims, err := cfg.AgentTokenSvc.ValidateAgentToken(token); err == nil {
						// Step 1a: Validate against persistent credential state (Phase 1H)
						if cfg.CredentialStore != nil && claims.ID != "" {
							jtiHash := hashJTI(claims.ID)
							cred, credErr := cfg.CredentialStore.GetAgentCredentialByJTIHash(ctx, jtiHash)
							if credErr == nil {
								// Credential found — check revocation status
								if cred.RevokedAt != nil {
									writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
										"token has been revoked", nil)
									return
								}
								// Store credential ID in context for downstream use
								ctx = context.WithValue(ctx, agentCredentialIDContextKey{}, cred.ID)
								// Update last_seen_at (fire-and-forget)
								go func() {
									_ = cfg.CredentialStore.UpdateAgentCredentialLastSeen(
										context.Background(), cred.ID, time.Now())
								}()
							} else if errors.Is(credErr, store.ErrNotFound) {
								// Compatibility window: accept pre-table tokens with a warning
								log.Warn("Agent token not found in credential store (legacy/pre-table token)",
									"agent_id", claims.Subject, "jti_hash", jtiHash[:8])
								ctx = context.WithValue(ctx, legacyTokenContextKey{}, true)
							} else {
								// Store error — log and accept (fail open for availability)
								log.Warn("Credential store lookup failed, accepting token",
									"agent_id", claims.Subject, "error", credErr)
							}
						}

						ctx = context.WithValue(ctx, agentContextKey{}, claims)
						identity := &agentIdentityWrapper{claims}
						ctx = contextWithIdentity(ctx, identity)
						ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
						ctx = contextWithAuthType(ctx, AuthTypeAgent)
						if cfg.Debug {
							log.Debug("Agent authenticated", "subject", claims.Subject)
						}
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					} else if r.Header.Get("X-Scion-Agent-Token") != "" {
						// Agent token header was present but invalid
						writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
							"invalid agent token: "+err.Error(), nil)
						return
					}
				}
				// Bearer token wasn't an agent token, continue to user auth
			}

			// Step 1.5: Federation OIDC token (X-Scion-Federation-Token header)
			if federationToken := r.Header.Get(FederationTokenHeader); federationToken != "" {
				var fedAuth *FederationAuthenticator
				if cfg.FederationAuth != nil {
					fedAuth = cfg.FederationAuth.Load()
				}
				if fedAuth == nil {
					// Header present but federation not enabled — reject, don't silently ignore
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"federation authentication is not configured", nil)
					return
				}
				identity, err := fedAuth.Authenticate(federationToken)
				if err != nil {
					if cfg.Debug {
						log.Debug("Federation token validation failed", "error", err)
					}
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"invalid federation token", nil)
					return
				}
				ctx = contextWithIdentity(ctx, identity)
				ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
				ctx = contextWithAuthType(ctx, AuthTypeFederation)
				if cfg.Debug {
					log.Debug("Federated identity authenticated",
						"issuer", identity.IssuerURL(),
						"type", identity.Type(),
						"id", identity.ID())
				}
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Step 2: Check for broker HMAC authentication (X-Scion-Broker-ID header)
			// If present, defer to BrokerAuthMiddleware, which runs later in the
			// chain and validates the HMAC signature.
			//
			// This branch sets an auth-type label only — it never establishes an
			// identity. Deferring is therefore only safe when BrokerAuthMiddleware
			// is actually installed and enabled; otherwise the request would reach
			// the handlers with no identity and no signature check at all. Fail
			// closed instead of passing it through (#591, design §8.1).
			if brokerID := r.Header.Get("X-Scion-Broker-ID"); brokerID != "" {
				if !brokerAuthActive(cfg.BrokerAuthSvc) {
					log.Warn("Rejecting broker-authenticated request: broker authentication is not available",
						slog.String("broker_id", brokerID),
						slog.String("path", r.URL.Path),
					)
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"broker authentication is not enabled", nil)
					return
				}
				if cfg.Debug {
					log.Debug("Broker auth headers present, deferring to BrokerAuthMiddleware", "brokerID", brokerID)
				}
				ctx = contextWithAuthType(ctx, AuthTypeBroker)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Step 3: Extract bearer token
			token := extractBearerToken(r)
			if token == "" {
				// Step 3a: Try proxy authenticator (new verified-assertion path)
				if cfg.ProxyAuthenticator != nil {
					proxyUser, proxyErr := cfg.ProxyAuthenticator.Authenticate(r)
					if proxyErr != nil {
						// Assertion present but invalid — reject
						if cfg.Debug {
							log.Debug("Proxy auth rejected", "provider", cfg.ProxyAuthenticator.Name(), "error", proxyErr)
						}
						writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
							"invalid proxy assertion: "+proxyErr.Error(), nil)
						return
					}
					if proxyUser != nil {
						// Verified proxy identity — provision the user
						identity, err := cfg.ProxyUserProvisioner(ctx, proxyUser)
						if err != nil {
							if cfg.Debug {
								log.Debug("Proxy user provisioning failed", "email", proxyUser.Email, "error", err)
							}
							if errors.Is(err, ErrAccessDenied) {
								writeError(w, http.StatusForbidden, ErrCodeForbidden,
									"access denied: email not authorized", nil)
							} else if errors.Is(err, ErrUserSuspended) {
								writeError(w, http.StatusForbidden, "user_suspended",
									"access denied: user account is suspended", nil)
							} else {
								writeError(w, http.StatusInternalServerError, "internal_error",
									"user provisioning failed", nil)
							}
							return
						}
						ctx = context.WithValue(ctx, userContextKey{}, identity)
						ctx = contextWithIdentity(ctx, identity)
						ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
						ctx = contextWithAuthType(ctx, AuthTypeProxy)
						if cfg.Debug {
							log.Debug("Proxy user authenticated", "provider", cfg.ProxyAuthenticator.Name(), "email", proxyUser.Email)
						}
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
					// (nil, nil) = no assertion present, fall through
				}

				// Step 3b: Legacy trusted proxy headers (backward compat when no ProxyAuthenticator)
				if cfg.ProxyAuthenticator == nil && len(trustedNets) > 0 && isTrustedProxy(r, trustedNets) {
					if user := extractProxyUser(r); user != nil {
						ctx = context.WithValue(ctx, userContextKey{}, user)
						ctx = contextWithIdentity(ctx, user)
						ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(user))
						ctx = contextWithAuthType(ctx, AuthTypeProxy)
						if cfg.Debug {
							log.Debug("Proxy user authenticated (legacy)", "email", user.Email())
						}
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				}

				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"missing authorization header", nil)
				return
			}

			// Step 4: Detect token type and validate
			switch detectTokenType(token) {
			case tokenTypeDev:
				if !cfg.DevAuthEnabled {
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"development authentication is not enabled", nil)
					return
				}
				if !apiclient.ValidateDevToken(token, cfg.DevAuthToken) {
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"invalid development token", nil)
					return
				}
				ctx = context.WithValue(ctx, userContextKey{}, devUser)
				ctx = contextWithIdentity(ctx, devUser)
				ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(devUser))
				ctx = contextWithAuthType(ctx, AuthTypeDevToken)
				if cfg.Debug {
					log.Debug("Dev user authenticated")
				}

			case tokenTypeUAT:
				if cfg.UATSvc == nil {
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"user access token authentication is not enabled", nil)
					return
				}
				scopedUser, err := cfg.UATSvc.ValidateToken(ctx, token)
				if err != nil {
					if errors.Is(err, ErrUserSuspended) {
						writeError(w, http.StatusForbidden, "user_suspended",
							"access denied: user account is suspended", nil)
						return
					}
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"invalid access token", nil)
					return
				}
				ctx = context.WithValue(ctx, userContextKey{}, scopedUser)
				ctx = contextWithIdentity(ctx, scopedUser)
				ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(scopedUser))
				ctx = contextWithAuthType(ctx, AuthTypeUAT)
				if cfg.Debug {
					log.Debug("UAT authenticated", "email", scopedUser.Email(), "project_id", scopedUser.ScopedProjectID())
				}

			case tokenTypeUser:
				if cfg.UserTokenSvc == nil {
					// Fall back to dev auth if user tokens not configured
					if cfg.DevAuthEnabled && apiclient.ValidateDevToken(token, cfg.DevAuthToken) {
						ctx = context.WithValue(ctx, userContextKey{}, devUser)
						ctx = contextWithIdentity(ctx, devUser)
						ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(devUser))
						ctx = contextWithAuthType(ctx, AuthTypeDevToken)
						if cfg.Debug {
							log.Debug("Dev user authenticated (fallback)")
						}
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"user authentication is not enabled", nil)
					return
				}
				claims, err := cfg.UserTokenSvc.ValidateUserToken(token)
				if err != nil {
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"invalid access token: "+err.Error(), nil)
					return
				}
				// JWT tokens are self-contained; check current user status
				// from the store to enforce suspension between token refreshes.
				if cfg.UserStore != nil {
					u, uErr := cfg.UserStore.GetUser(ctx, claims.UserID)
					if uErr != nil && !errors.Is(uErr, store.ErrNotFound) {
						log.Error("JWT auth: user store lookup failed",
							"user_id", claims.UserID, "error", uErr)
						writeError(w, http.StatusServiceUnavailable, "store_error",
							"unable to verify user status", nil)
						return
					}
					if uErr == nil && u.Status == store.UserStatusSuspended {
						log.Warn("JWT auth rejected: user is suspended",
							"user_id", claims.UserID, "email", claims.Email)
						writeError(w, http.StatusForbidden, "user_suspended",
							"access denied: user account is suspended", nil)
						return
					}
					// ErrNotFound (deleted user) falls through — downstream
					// handlers will fail closed when the identity has no
					// matching store record.
				}
				user := NewAuthenticatedUser(
					claims.UserID,
					claims.Email,
					claims.DisplayName,
					claims.Role,
					string(claims.ClientType),
				)
				ctx = context.WithValue(ctx, userContextKey{}, user)
				ctx = contextWithIdentity(ctx, user)
				ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(user))
				ctx = contextWithAuthType(ctx, AuthTypeJWT)
				if cfg.Debug {
					log.Debug("User authenticated", "email", user.Email())
				}

			default:
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"unrecognized token format", nil)
				return
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// detectTokenType identifies the type of token.
func detectTokenType(token string) tokenType {
	switch {
	case strings.HasPrefix(token, apiclient.DevTokenPrefix):
		return tokenTypeDev
	case strings.HasPrefix(token, "scion_pat_"):
		return tokenTypeUAT
	case looksLikeJWT(token):
		// Could be user or agent JWT - need to inspect claims
		// For now, assume user token (agent tokens use X-Scion-Agent-Token)
		return tokenTypeUser
	default:
		return tokenTypeUnknown
	}
}

// looksLikeJWT checks if a token appears to be a JWT.
func looksLikeJWT(token string) bool {
	parts := strings.Split(token, ".")
	return len(parts) == 3
}

// extractBearerToken extracts the bearer token from the Authorization header.
func extractBearerToken(r *http.Request) string {
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

// isHealthEndpoint returns true if the path is a health check endpoint.
func isHealthEndpoint(path string) bool {
	return path == "/healthz" || path == "/health" || path == "/readyz"
}

// isUnauthenticatedEndpoint returns true if the path does not require authentication.
// This includes health endpoints and OAuth/login endpoints.
func isUnauthenticatedEndpoint(path string) bool {
	if isHealthEndpoint(path) {
		return true
	}
	// OAuth/login/token endpoints - these are pre-authentication or authentication-management endpoints
	switch path {
	case "/api/v1/auth/login": // Web frontend OAuth token exchange
		return true
	case "/api/v1/auth/token": // OAuth code exchange (unified)
		return true
	case "/api/v1/auth/refresh": // Token refresh
		return true
	case "/api/v1/auth/validate": // Token validation
		return true
	case "/api/v1/auth/logout": // Logout
		return true
	case "/api/v1/auth/providers": // OAuth provider discovery for CLI login
		return true
	case "/api/v1/auth/cli/authorize": // CLI OAuth authorization URL
		return true
	case "/api/v1/auth/cli/token": // CLI OAuth token exchange
		return true
	case "/api/v1/auth/cli/device": // CLI device flow initiation
		return true
	case "/api/v1/auth/cli/device/token": // CLI device flow token polling
		return true
	case "/api/v1/auth/test-login": // Test-login for integration testing (gated by --enable-test-login)
		return true
	case "/api/v1/brokers/join": // Broker registration bootstrap (uses join token)
		return true
	case "/api/v1/webhooks/github": // GitHub App webhook (uses webhook signature verification)
		return true
	case "/github-app/setup": // GitHub App post-installation callback (browser redirect)
		return true
	case "/.well-known/openid-configuration": // OIDC discovery document (public metadata)
		return true
	case "/.well-known/jwks.json": // OIDC JSON Web Key Set (public keys)
		return true
	case "/api/v1/settings/public": // Public settings (no auth required)
		return true
	}
	return false
}

// parseTrustedProxies parses a list of IP addresses and CIDR ranges.
func parseTrustedProxies(proxies []string) []*net.IPNet {
	var nets []*net.IPNet
	for _, p := range proxies {
		// Try parsing as CIDR
		_, ipNet, err := net.ParseCIDR(p)
		if err == nil {
			nets = append(nets, ipNet)
			continue
		}
		// Try parsing as single IP
		ip := net.ParseIP(p)
		if ip != nil {
			// Convert to /32 or /128 CIDR
			var mask net.IPMask
			if ip.To4() != nil {
				mask = net.CIDRMask(32, 32)
			} else {
				mask = net.CIDRMask(128, 128)
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: mask})
		}
	}
	return nets
}

// isTrustedProxy checks if the request originates from a trusted proxy.
func isTrustedProxy(r *http.Request, trustedNets []*net.IPNet) bool {
	// Get client IP
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	for _, n := range trustedNets {
		if n.Contains(ip) {
			return true
		}
	}

	return false
}

// extractProxyUser extracts user information from trusted proxy headers.
func extractProxyUser(r *http.Request) UserIdentity {
	userID := r.Header.Get("X-Forwarded-User-Id")
	email := r.Header.Get("X-Forwarded-User-Email")
	name := r.Header.Get("X-Forwarded-User-Name")
	role := r.Header.Get("X-Forwarded-User-Role")

	// At minimum, we need user ID and email
	if userID == "" || email == "" {
		return nil
	}

	if role == "" {
		role = "member"
	}

	return NewAuthenticatedUser(userID, email, name, role, string(ClientTypeWeb))
}

// agentCredentialIDContextKey stores the credential ID for a validated agent token.
type agentCredentialIDContextKey struct{}

// legacyTokenContextKey marks that the agent token is a legacy (pre-table) token.
type legacyTokenContextKey struct{}

// GetAgentCredentialIDFromContext returns the credential ID if the agent token
// was found in the credential store, or "" if it's a legacy token.
func GetAgentCredentialIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(agentCredentialIDContextKey{}).(string)
	return id
}

// IsLegacyTokenFromContext returns true if the agent token was not found in the
// credential store (compatibility window for pre-table tokens).
func IsLegacyTokenFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(legacyTokenContextKey{}).(bool)
	return v
}

// RequireAuth is middleware that ensures a request is authenticated.
// It returns 401 if no identity is present in the context.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if GetIdentityFromContext(r.Context()) == nil {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
				"authentication required", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireUserAuth is middleware that ensures a request is from an authenticated user.
// It returns 401 if no user identity is present in the context.
func RequireUserAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if GetUserIdentityFromContext(r.Context()) == nil {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
				"user authentication required", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireRole is middleware that ensures the authenticated user has the required role.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	roleSet := make(map[string]bool)
	for _, r := range roles {
		roleSet[r] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := GetUserIdentityFromContext(r.Context())
			if user == nil {
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"authentication required", nil)
				return
			}

			if !roleSet[user.Role()] {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"insufficient permissions", nil)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ---- Proxy user resolution cache ----

const proxyUserCacheTTL = 60 * time.Second

// proxyUserCacheEntry holds a cached provisioned user identity.
type proxyUserCacheEntry struct {
	identity  UserIdentity
	expiresAt time.Time
}

// ProxyUserCache is a short-TTL cache keyed by verified email wrapping the
// provisionUser store lookup. The JWT signature verification still runs every
// request; only the store round-trip is cached.
type ProxyUserCache struct {
	mu    sync.RWMutex
	cache map[string]*proxyUserCacheEntry
}

// NewProxyUserCache creates a new proxy user resolution cache.
func NewProxyUserCache() *ProxyUserCache {
	return &ProxyUserCache{
		cache: make(map[string]*proxyUserCacheEntry),
	}
}

// Get returns a cached user identity if present and not expired.
func (c *ProxyUserCache) Get(email string) (UserIdentity, bool) {
	c.mu.RLock()
	entry, ok := c.cache[email]
	if !ok {
		c.mu.RUnlock()
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		c.mu.RUnlock()
		c.mu.Lock()
		if entry, ok = c.cache[email]; ok && time.Now().After(entry.expiresAt) {
			delete(c.cache, email)
		}
		c.mu.Unlock()
		return nil, false
	}
	defer c.mu.RUnlock()
	return entry.identity, true
}

// Set stores a user identity in the cache.
func (c *ProxyUserCache) Set(email string, identity UserIdentity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[email] = &proxyUserCacheEntry{
		identity:  identity,
		expiresAt: time.Now().Add(proxyUserCacheTTL),
	}
}

// MakeProxyUserProvisioner creates the ProxyUserProvisioner function that
// wraps provisionUser with a short-TTL cache. It converts the stored user
// to the canonical UserIdentity (real UUID/role from the store).
func MakeProxyUserProvisioner(server *Server) func(ctx context.Context, info *ProxyUserInfo) (UserIdentity, error) {
	cache := NewProxyUserCache()

	return func(ctx context.Context, info *ProxyUserInfo) (UserIdentity, error) {
		// Check cache first (keyed by verified email)
		if identity, ok := cache.Get(info.Email); ok {
			return identity, nil
		}

		// Provision: authorize + find-or-create + hub membership
		user, err := server.provisionUser(ctx, &ExternalUserInfo{
			Email:       info.Email,
			DisplayName: info.DisplayName,
		})
		if err != nil {
			return nil, err
		}

		// Build canonical identity from stored user
		identity := NewAuthenticatedUser(
			user.ID,
			user.Email,
			user.DisplayName,
			user.Role,
			string(ClientTypeWeb),
		)

		cache.Set(info.Email, identity)
		return identity, nil
	}
}
