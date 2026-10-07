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
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
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
	// GoogleValidator verifies Google ID tokens / access tokens for the
	// external-bearer path (auth_external_bearer.go). nil disables that path
	// even when FederationAuth trusts accounts.google.com.
	GoogleValidator GoogleCredentialValidator
	// GoogleResolver resolves a validated Google identity to a Hub user for
	// the external-bearer path, sharing decisions with GEExchangeService.
	GoogleResolver *GoogleIdentityResolver
	// ExternalBearerLimiter rate-limits the external-bearer path per client
	// IP, consulted only on a Google-credential-cache miss. nil disables
	// rate limiting for that path (see authenticateExternalBearer).
	ExternalBearerLimiter *externalBearerRateLimiter
	// ExternalBearerMetrics records the outcome of every external-bearer
	// authentication attempt (the external_bearer counter; see
	// external_bearer_metrics.go for the closed label set and the real
	// exported metric names). UnifiedAuthMiddleware captures a copy of this
	// cfg each time applyMiddleware runs (Start(), Handler()).
	// cmd/server_foreground.go calls SetExternalBearerMetrics before either,
	// so a plain field would work today; the *atomic.Pointer (the
	// FederationAuth shape above) makes a setter call after the handler is
	// already built still take effect, race-free, so correctness does not
	// depend on that ordering. A nil pointer, or one currently holding a nil
	// interface, disables recording; it never changes the external-bearer
	// path's authentication outcome.
	ExternalBearerMetrics *atomic.Pointer[ExternalBearerMetricsRecorder]
	// CredentialStore handles agent credential validation (Phase 1H).
	// When non-nil, agent tokens are validated against persistent credential state.
	CredentialStore store.AgentCredentialStore
	// HoldStore enables the per-request agent hold check (ptone/scion#3433):
	// a token whose agent has an active hold is refused exactly like a
	// revoked credential, whether or not the token has a credential row.
	HoldStore store.AgentHoldStore
	// UserStore enables per-request user-status checks (e.g. suspension
	// enforcement) for self-contained credentials like JWTs that do not
	// themselves hit the database.
	UserStore store.UserStore
	// Debug enables verbose logging
	Debug bool
	// Logger is the subsystem logger for auth middleware (defaults to slog.Default())
	Logger *slog.Logger
	// PlatformAuthSA is the hub's configured platform/transport auth service
	// account email. UnifiedAuthMiddleware's tokenTypeUser and tokenTypeUAT
	// arms use it to reject an otherwise-valid user JWT or PAT issued for
	// that identity — see isReservedPlatformIdentity's invariant comment.
	// Wired from the same server-config value as Server.platformAuthSA (see
	// server.go's New) so the two cannot diverge. Empty when no transport
	// service account is configured, which leaves the check inert.
	PlatformAuthSA string
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

const constraintAuditPathPrefix = "/api/v1/admin/access-constraints/"

// isConstraintAuditAuthFailureRoute matches only the canonical live-constraint
// audit subresource. Query parameters are intentionally irrelevant, while an
// encoded path is rejected even when net/url decodes it to the same URL.Path.
func isConstraintAuditAuthFailureRoute(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path {
		return false
	}
	if !strings.HasPrefix(r.URL.Path, constraintAuditPathPrefix) || !strings.HasSuffix(r.URL.Path, "/audit") {
		return false
	}

	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, constraintAuditPathPrefix), "/audit")
	return id != "" && id != "." && id != ".." && !strings.Contains(id, "/")
}

// constraintAuditAuthFailureWriter preserves authentication's fail-closed
// control flow while making credential rejection responses indistinguishable
// from the endpoint's absent-resource response. It never forwards the rejected
// body. Infrastructure failures remain unchanged.
type constraintAuditAuthFailureWriter struct {
	http.ResponseWriter
	normalized bool
}

func (w *constraintAuditAuthFailureWriter) WriteHeader(statusCode int) {
	if w.normalized {
		return
	}
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		w.normalized = true
		NotFound(w.ResponseWriter, "Access Constraint")
		return
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *constraintAuditAuthFailureWriter) Write(body []byte) (int, error) {
	if w.normalized {
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}

func normalizeConstraintAuditAuthFailures(w http.ResponseWriter, r *http.Request) http.ResponseWriter {
	if isConstraintAuditAuthFailureRoute(r) {
		return &constraintAuditAuthFailureWriter{ResponseWriter: w}
	}
	return w
}

// serveAfterAuth unwraps the response normalizer before entering downstream
// middleware or a handler, so only authentication failures are rewritten.
func serveAfterAuth(w http.ResponseWriter, next http.Handler, r *http.Request) {
	if normalizer, ok := w.(*constraintAuditAuthFailureWriter); ok {
		next.ServeHTTP(normalizer.ResponseWriter, r)
		return
	}
	next.ServeHTTP(w, r)
}

func handlerAfterAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveAfterAuth(w, next, r)
	})
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
			w = normalizeConstraintAuditAuthFailures(w, r)
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
				serveAfterAuth(w, next, r)
				return
			}

			// Step 1: Try agent token (X-Scion-Agent-Token header or agent JWT)
			if token := extractAgentToken(r); token != "" {
				if cfg.AgentTokenSvc != nil {
					if claims, err := cfg.AgentTokenSvc.ValidateAgentToken(token); err == nil {
						// Step 1a: Agent-token authentication requires a successful
						// credential-status evaluation (Phase 1H). See
						// evaluateAgentCredentialStatus for the possible outcomes.
						if cfg.CredentialStore != nil && claims.ID != "" {
							cred, isLegacy, credErr := evaluateAgentCredentialStatus(ctx, cfg.CredentialStore, claims.ID)
							switch {
							case errors.Is(credErr, errAgentCredentialRevoked):
								writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
									"token has been revoked", nil)
								return
							case credErr != nil:
								// Any store error other than "not found" must not fall
								// back to authenticating the request: it means the
								// credential's status could not actually be determined,
								// so treat it as a retryable failure.
								log.Error("Agent credential status lookup failed",
									"agent_id", claims.Subject, "error", credErr)
								writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
									"unable to verify credential status", nil)
								return
							case isLegacy:
								// Legacy compatibility path: tokens issued before the
								// credential table existed have no credential record.
								// Retained pending a product decision; do not close or
								// widen this branch.
								log.Warn("Agent token not found in credential store (legacy/pre-table token)",
									"agent_id", claims.Subject, "jti_hash", hashJTI(claims.ID)[:8])
								ctx = context.WithValue(ctx, legacyTokenContextKey{}, true)
							default:
								// Credential found and active.
								ctx = context.WithValue(ctx, agentCredentialIDContextKey{}, cred.ID)
								// Update last_seen_at (fire-and-forget)
								go func() {
									_ = cfg.CredentialStore.UpdateAgentCredentialLastSeen(
										context.Background(), cred.ID, time.Now())
								}()
							}
						}

						// Step 1b: a held agent's token is refused on every
						// request, including tokens on the legacy path above
						// (ptone/scion#3433). Same response as a revoked
						// credential; a lookup fault is the same 503.
						if cfg.HoldStore != nil {
							held, holdErr := cfg.HoldStore.HasActiveAgentHold(ctx, claims.Subject)
							if holdErr != nil {
								log.Error("Agent hold lookup failed",
									"agent_id", claims.Subject, "error", holdErr)
								writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
									"unable to verify credential status", nil)
								return
							}
							if held {
								writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
									"token has been revoked", nil)
								return
							}
						}

						ctx = context.WithValue(ctx, agentContextKey{}, claims)
						ctx = withStandingMemo(ctx)
						identity := &agentIdentityWrapper{claims}
						ctx = contextWithIdentity(ctx, identity)
						ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
						ctx = contextWithAuthType(ctx, AuthTypeAgent)
						if cfg.Debug {
							log.Debug("Agent authenticated", "subject", claims.Subject)
						}
						serveAfterAuth(w, next, r.WithContext(ctx))
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
				serveAfterAuth(w, next, r.WithContext(ctx))
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
				// Broker HMAC and on-behalf-of authentication run downstream.
				// Keep the exact-route normalizer attached until that delegated
				// authentication succeeds inside the broker middleware.
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
						serveAfterAuth(w, next, r.WithContext(ctx))
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
						serveAfterAuth(w, next, r.WithContext(ctx))
						return
					}
				}

				// Step 3c: Skill file capability URL (#1792). A credential-less
				// GET of /api/v1/skills/{id}/files/{path} carrying exp/sig
				// parameters is passed through WITHOUT an identity. Only the
				// request shape is checked here; handleSkillFileRead verifies
				// the HMAC signature unconditionally and rejects the request
				// if it does not validate, and every other handler sees an
				// anonymous request exactly as it would for a public route.
				if isSignedSkillFileRequest(r) {
					ctx = contextWithAuthType(ctx, AuthTypeSignedURL)
					serveAfterAuth(w, next, r.WithContext(ctx))
					return
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
					logUATRejection(log, ctx, err)
					if errors.Is(err, ErrUserSuspended) {
						writeError(w, http.StatusForbidden, "user_suspended",
							"access denied: user account is suspended", nil)
						return
					}
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"invalid access token", nil)
					return
				}
				// See isReservedPlatformIdentity: every credential-acceptance
				// point checks this too, including a PAT issued under a
				// reserved-identity user row.
				if isReservedPlatformIdentity(scopedUser.Email(), cfg.PlatformAuthSA) {
					logCredentialRejected(log, ctx, "reserved_identity", true, scopedUser.CredentialID())
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"invalid access token", nil)
					return
				}
				ctx = context.WithValue(ctx, userContextKey{}, scopedUser)
				ctx = contextWithIdentity(ctx, scopedUser)
				ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(scopedUser))
				ctx = contextWithAuthType(ctx, AuthTypeUAT)
				if cfg.Debug {
					boundary := scopedUser.Boundary()
					log.Debug("UAT authenticated", "email", scopedUser.Email(), "boundary_kind", string(boundary.Kind), "project_id", boundary.ProjectID)
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
						serveAfterAuth(w, next, r.WithContext(ctx))
						return
					}
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"user authentication is not enabled", nil)
					return
				}
				claims, err := cfg.UserTokenSvc.ValidateUserToken(token)
				if err != nil {
					// Not a Hub-issued user JWT. It may be a Google ID token
					// forwarded verbatim by a trusted external caller.
					if serveExternalBearer(w, r, handlerAfterAuth(next), ctx, token, cfg, log) {
						return
					}
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"invalid access token: "+err.Error(), nil)
					return
				}
				// PRIMARY choke: see isReservedPlatformIdentity's invariant
				// comment. This check applies to every self-contained user
				// JWT that reaches this point, independent of which mint
				// site issued it or how long ago — it is what revokes an
				// already-issued, unexpired access token for the reserved
				// identity. Agent, federation, and broker credentials never
				// reach this arm (see UnifiedAuthMiddleware's earlier steps),
				// so this cannot deny an agent or broker token.
				if isReservedPlatformIdentity(claims.Email, cfg.PlatformAuthSA) {
					writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
						"invalid access token", nil)
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
					// NOTE: We intentionally check only for UserStatusSuspended rather
					// than u.Status != UserStatusActive. The third status, UserStatusInvited,
					// represents users in the onboarding flow who do not hold JWT tokens —
					// they authenticate via the OAuth/invitation path, not the JWT path.
					// Suspended is the only non-active state reachable with a cached JWT.
					if uErr == nil && u.Status == store.UserStatusSuspended {
						log.Warn("JWT auth rejected: user is suspended",
							"user_id", claims.UserID, "email", claims.Email)
						writeError(w, http.StatusForbidden, "user_suspended",
							"access denied: user account is suspended", nil)
						return
					}
					// A token whose subject has no user record (for example
					// the account was deleted after the token was issued) is
					// rejected here, like a suspended user. The token is
					// self-contained, so authentication must not succeed for
					// an identity that has no store record.
					if errors.Is(uErr, store.ErrNotFound) {
						log.Warn("JWT auth rejected: no user record for this token",
							"user_id", claims.UserID)
						writeError(w, http.StatusUnauthorized, ErrCodeUserNotFound,
							"invalid access token: no user record for this token", nil)
						return
					}
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
				// Opaque (non-JWT) bearer tokens land here — detectTokenType
				// routes every 3-segment token to tokenTypeUser, so this arm
				// never sees a JWT. This is the external-bearer path's
				// access-token hook site (auth_external_bearer.go):
				// serveExternalBearer returns true whenever it has fully
				// handled the request (Google trust configured, whether
				// validation succeeds or fails), so this arm returns and the
				// "unrecognized token format" rejection below never runs. It
				// returns false only when the token is not applicable to
				// this path at all (e.g. no Google trust configured), in
				// which case that rejection runs as usual. The ID-token hook
				// site is reached only from the tokenTypeUser case above.
				if serveExternalBearer(w, r, handlerAfterAuth(next), ctx, token, cfg, log) {
					return
				}
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"unrecognized token format", nil)
				return
			}

			serveAfterAuth(w, next, r.WithContext(ctx))
		})
	}
}

// logUATRejection logs a single "credential rejected" line for a UAT that
// failed ValidateToken (plan §3.1(3)). It classifies the reason from the
// returned error's *UATRejection, when present. Any other error shape means
// ValidateToken itself could not complete — a store or database failure, not
// a client-presented bad credential — so it is logged distinctly (reason
// "lookup_error", at Error level), never folded into the client-facing
// "invalid" reason: an operator must be able to tell an outage from a wave
// of bad tokens.
func logUATRejection(log *slog.Logger, ctx context.Context, err error) {
	var rej *UATRejection
	if errors.As(err, &rej) {
		logCredentialRejected(log, ctx, rej.Reason, rej.Found, rej.TokenID)
		return
	}
	logCredentialRejectedAtLevel(log, ctx, slog.LevelError, "lookup_error", false, "")
}

// logCredentialRejected logs the standard "credential rejected" warning line.
// The token ID is included only when found is true, i.e. the presented value
// matched a server-verified stored record — this marks the record as
// rejected, never as an authenticated principal (rulings, plan correction
// (b)). The presented token string itself is never logged (ruling Q3).
func logCredentialRejected(log *slog.Logger, ctx context.Context, reason string, found bool, tokenID string) {
	logCredentialRejectedAtLevel(log, ctx, slog.LevelWarn, reason, found, tokenID)
}

// logCredentialRejectedAtLevel is logCredentialRejected's implementation,
// parameterized on level so an internal lookup failure (logUATRejection's
// fallback) can be distinguished from an ordinary client-side rejection.
func logCredentialRejectedAtLevel(log *slog.Logger, ctx context.Context, level slog.Level, reason string, found bool, tokenID string) {
	attrs := []any{
		slog.String("auth_type", AuthTypeUAT),
		slog.String("reason", reason),
	}
	if found && tokenID != "" {
		attrs = append(attrs, slog.String("credential.id", tokenID))
	}
	if reqID := logging.RequestIDFromContext(ctx); reqID != "" {
		attrs = append(attrs, slog.String(logging.AttrRequestID, reqID))
	}
	log.Log(ctx, level, "credential rejected", attrs...)
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
	case "/api/v1/auth/integrations/google/exchange": // GE Google credential exchange (pre-auth; handler validates Google credential)
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

// isReservedPlatformIdentity reports whether email is the hub's configured
// platform/transport auth service account. A reserved-platform-identity
// credential is never minted, re-minted, accepted as valid at validation, or
// reported valid — and the validation choke (below) also revokes an
// already-issued, unexpired token, not just new ones.
//
// Invariant: every path that provisions a user, or mints, re-mints, or
// validates a hub token, checks this. That covers, today:
//   - Provisioning: Server.provisionUser (API proxy/IAP, OAuth, and session
//     login), GoogleIdentityResolver.Resolve (GE exchange and the
//     external-bearer path), WebServer.proxyAuthMiddleware's fresh-identity
//     branch, and the web OAuth callback.
//   - Re-minting from an existing credential: Server.handleAuthRefresh,
//     WebServer.proxyAuthMiddleware's existing-session branch, and
//     WebServer.sessionToBearerMiddleware (both its cookie-overflow mint and
//     its refresh branches).
//   - Validation (the choke that also revokes an already-issued token):
//     UnifiedAuthMiddleware's tokenTypeUser arm (the primary choke — every
//     self-contained hub-issued user JWT passes through it) and its
//     tokenTypeUAT arm (PATs), and Server.handleAuthValidate.
//
// Intentionally NOT checked, because none of them can authenticate as this
// identity or are gated some other way: devAuthMiddleware (mints a token
// only for the fixed dev-user identity, never an external one),
// handlers_test_login (gated behind --enable-test-login, never enabled in
// production), and the a2a-bridge's synthetic service token (server.go,
// an internal token that never carries an external identity's email).
//
// Stating the covered and excluded sites here makes the guard set auditable
// by checking this list against the code, rather than by a reachability
// argument for each new call site.
//
// Comparison trims surrounding whitespace and is case-insensitive on both
// sides. Returns false whenever platformAuthSA is empty, so the check is
// inert on hubs that do not configure a transport service account (the
// common case).
func isReservedPlatformIdentity(email, platformAuthSA string) bool {
	platformAuthSA = strings.TrimSpace(platformAuthSA)
	if platformAuthSA == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(email), platformAuthSA)
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
