package hub

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/ptone/scion-agent/pkg/apiclient"
)

// AuthConfig holds authentication configuration.
type AuthConfig struct {
	// Mode is the authentication mode: "production", "development", "testing"
	Mode string
	// DevAuthEnabled enables development token authentication
	DevAuthEnabled bool
	// DevAuthToken is the valid development token
	DevAuthToken string
	// AgentTokenSvc handles agent JWT validation
	AgentTokenSvc *AgentTokenService
	// UserTokenSvc handles user JWT validation
	UserTokenSvc *UserTokenService
	// APIKeySvc handles API key validation
	APIKeySvc *APIKeyService
	// TrustedProxies is a list of trusted proxy IPs/CIDRs
	TrustedProxies []string
	// Debug enables verbose logging
	Debug bool
}

// tokenType represents the type of authentication token.
type tokenType int

const (
	tokenTypeUnknown tokenType = iota
	tokenTypeDev
	tokenTypeUser
	tokenTypeAPIKey
	tokenTypeAgent
)

// UnifiedAuthMiddleware creates middleware that handles all authentication types.
// It processes tokens in priority order:
// 1. Agent tokens (X-Scion-Agent-Token or agent JWT in Bearer)
// 2. Development tokens (scion_dev_* prefix)
// 3. API keys (sk_live_* or sk_test_* prefix)
// 4. User JWTs
// 5. Trusted proxy headers
func UnifiedAuthMiddleware(cfg AuthConfig) func(http.Handler) http.Handler {
	// Parse trusted proxy CIDRs
	trustedNets := parseTrustedProxies(cfg.TrustedProxies)

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
				log.Printf("[Auth] %s %s - hasAuth=%v authPrefix=%q", r.Method, r.URL.Path, hasAuth, authPrefix)
			}

			// Skip auth for unauthenticated endpoints (health checks, CLI OAuth)
			if isUnauthenticatedEndpoint(r.URL.Path) {
				if cfg.Debug {
					log.Printf("[Auth] Skipping auth for unauthenticated endpoint: %s", r.URL.Path)
				}
				next.ServeHTTP(w, r)
				return
			}

			// Step 1: Try agent token (X-Scion-Agent-Token header or agent JWT)
			if token := extractAgentToken(r); token != "" {
				if cfg.AgentTokenSvc != nil {
					if claims, err := cfg.AgentTokenSvc.ValidateAgentToken(token); err == nil {
						ctx = context.WithValue(ctx, agentContextKey{}, claims)
						ctx = contextWithIdentity(ctx, &agentIdentityWrapper{claims})
						if cfg.Debug {
							log.Printf("[Auth] Agent authenticated: %s", claims.Subject)
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

			// Step 2: Extract bearer token
			token := extractBearerToken(r)
			if token == "" {
				// Check for trusted proxy headers
				if len(trustedNets) > 0 && isTrustedProxy(r, trustedNets) {
					if user := extractProxyUser(r); user != nil {
						ctx = context.WithValue(ctx, userContextKey{}, user)
						ctx = contextWithIdentity(ctx, user)
						if cfg.Debug {
							log.Printf("[Auth] Proxy user: %s", user.Email())
						}
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				}

				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"missing authorization header", nil)
				return
			}

			// Step 3: Detect token type and validate
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
				devUser := &DevUser{id: "dev-user"}
				ctx = context.WithValue(ctx, userContextKey{}, devUser)
				ctx = contextWithIdentity(ctx, devUser)
				if cfg.Debug {
					log.Printf("[Auth] Dev user authenticated")
				}

			case tokenTypeAPIKey:
				if cfg.APIKeySvc == nil {
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"API key authentication is not enabled", nil)
					return
				}
				user, err := cfg.APIKeySvc.ValidateAPIKey(ctx, token)
				if err != nil {
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"invalid API key", nil)
					return
				}
				ctx = context.WithValue(ctx, userContextKey{}, user)
				ctx = contextWithIdentity(ctx, user)
				if cfg.Debug {
					log.Printf("[Auth] API key authenticated: %s", user.Email())
				}

			case tokenTypeUser:
				if cfg.UserTokenSvc == nil {
					// Fall back to dev auth if user tokens not configured
					if cfg.DevAuthEnabled && apiclient.ValidateDevToken(token, cfg.DevAuthToken) {
						devUser := &DevUser{id: "dev-user"}
						ctx = context.WithValue(ctx, userContextKey{}, devUser)
						ctx = contextWithIdentity(ctx, devUser)
						if cfg.Debug {
							log.Printf("[Auth] Dev user authenticated (fallback)")
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
				user := NewAuthenticatedUser(
					claims.UserID,
					claims.Email,
					claims.DisplayName,
					claims.Role,
					string(claims.ClientType),
				)
				ctx = context.WithValue(ctx, userContextKey{}, user)
				ctx = contextWithIdentity(ctx, user)
				if cfg.Debug {
					log.Printf("[Auth] User authenticated: %s", user.Email())
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
	case strings.HasPrefix(token, "sk_live_"), strings.HasPrefix(token, "sk_test_"):
		return tokenTypeAPIKey
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
	return path == "/healthz" || path == "/readyz"
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
	case "/api/v1/auth/cli/authorize": // CLI OAuth authorization URL
		return true
	case "/api/v1/auth/cli/token": // CLI OAuth token exchange
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
