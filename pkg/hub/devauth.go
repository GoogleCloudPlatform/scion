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
	"fmt"
	"log/slog"
	"net/http"
	osuser "os/user"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// DevUserID is the well-known UUID for the development pseudo-user.
// Deterministic so that references in the database remain stable across restarts.
const DevUserID = "be67fbc9-c869-5d43-b15d-c28ca3e8d355"

// DevUserConfig holds optional identity overrides for the development user.
type DevUserConfig struct {
	Username    string
	DisplayName string
	Email       string
}

// DevUser represents the pseudo-user for development authentication.
type DevUser struct {
	id          string
	username    string
	displayName string
	email       string
}

// NewDevUser creates a DevUser with the stable UUID, applying config overrides
// and falling back to the current OS user for unset fields.
func NewDevUser(cfg DevUserConfig) *DevUser {
	u := &DevUser{id: DevUserID}

	u.username = cfg.Username
	u.displayName = cfg.DisplayName
	u.email = cfg.Email

	if u.username == "" || u.displayName == "" {
		if osUser, err := osuser.Current(); err == nil {
			if u.username == "" {
				u.username = osUser.Username
			}
			if u.displayName == "" {
				u.displayName = osUser.Name
			}
		}
	}

	if u.displayName == "" {
		u.displayName = "Development User"
	}
	if u.username == "" {
		u.username = "dev"
	}
	if u.email == "" {
		u.email = fmt.Sprintf("%s@localhost", u.username)
	}

	return u
}

// ID returns the user ID.
func (u *DevUser) ID() string { return u.id }

// Type returns the identity type ("dev").
func (u *DevUser) Type() string { return "dev" }

// localAncestryProvenance reports that a dev user is a local user: the root
// of its own ancestry chain.
func (u *DevUser) localAncestryProvenance() ancestryProvenance {
	return ancestryProvenanceLocalUser
}

// Username returns the user's login name.
func (u *DevUser) Username() string { return u.username }

// Email returns the user email.
func (u *DevUser) Email() string { return u.email }

// DisplayName returns the user display name.
func (u *DevUser) DisplayName() string { return u.displayName }

// Role returns the user role.
func (u *DevUser) Role() string { return "admin" }

// isTrustedLocalDevUser reports whether identity is the concrete, trusted
// local development identity constructed by NewDevUser (DevAuthMiddleware /
// UnifiedAuthMiddleware's dev-token arm; also seed.go's seeding path). This
// is the single predicate ptone/scion#2342's dev_local attribution and B.3's
// fire-time reconstruction both call — do not reimplement the type
// assertion elsewhere.
//
// The check is deliberately narrow and exact, per the issue's security
// contract:
//   - identity must type-assert to the concrete *DevUser (not merely
//     satisfy Identity, and not a distinct type that embeds or wraps
//     *DevUser — Go type assertions do not see through embedding to an
//     outer type, so a wrapper fails this assertion even though it promotes
//     DevUser's methods).
//   - a nil identity, or a non-nil Identity holding a nil *DevUser, is
//     rejected explicitly rather than by relying on a nil ID() call (which
//     would panic here, since DevUser.ID() does not guard against a nil
//     receiver).
//   - the identity's ID must equal the well-known DevUserID. NewDevUser
//     always sets this id itself; nothing here reads it from a request.
//
// It is never derived from identity.Type() == "dev" alone: that string is
// self-reported by any Identity implementation and proves nothing about
// which concrete type produced it.
func isTrustedLocalDevUser(identity Identity) bool {
	du, ok := identity.(*DevUser)
	if !ok || du == nil {
		return false
	}
	return du.ID() == DevUserID
}

// devLocalAuthorityEnabled reports whether THIS server currently accepts
// the recognized local dev user as an authority source at all (B.3 R6,
// ptone/scion#2342): dev-token authentication is enabled and the well-known
// DevUserID row has been seeded.
//
// Both of those are gated by the single ServerConfig.DevAuthToken != ""
// condition, so that one bit is exactly what this reports:
//   - New's "Build unified auth configuration" block sets
//     AuthConfig.DevAuthEnabled from cfg.DevAuthToken != "", and
//     srv.authConfig (assigned exactly once, in New) is the only AuthConfig
//     UnifiedAuthMiddleware is ever called with. UnifiedAuthMiddleware's
//     tokenTypeDev arm checks DevAuthEnabled before accepting a dev token,
//     and the tokenTypeUser fallback's dev-token arm checks the same field.
//   - New seeds the DevUserID row (seedDevUser) only when cfg.DevAuthToken
//     != "", so a server with dev-auth off never seeds it during that
//     startup (a row seeded some other way, e.g. directly in a test, does
//     not change this bit — see setDevLocalAuthorityEnabled).
//
// A nil receiver (a zero-value or never-constructed AuthzService) reports
// false: fail closed.
//
// Invariant: for any single running server, isTrustedLocalDevUser(id) ==
// true implies devLocalAuthorityEnabled() == true — the concrete *DevUser
// this server's request pipeline can produce only ever comes from
// NewDevUser, itself only reachable through the same dev-token code paths
// this bit tracks. This method does not change isTrustedLocalDevUser's
// semantics, and it does not change E.2b's attribution (initiatorCredentialKindFor
// still requires the identity assertion regardless of this flag); it exists
// so B.3's fire-time authority decision can additionally confirm this
// server currently admits dev_local at all before trusting a previously
// stored dev_local row (a server later reconfigured with dev-auth off must
// not honor an old dev_local row's authority). Enforced, on both sides of
// dev-auth on/off and through the real UnifiedAuthMiddleware wiring (not a
// re-derivation of cfg.DevAuthToken != ""), by
// TestAuthzService_DevLocalAuthorityEnabled.
func (a *AuthzService) devLocalAuthorityEnabled() bool {
	if a == nil {
		return false
	}
	return a.devLocalEnabled
}

// setDevLocalAuthorityEnabled sets the bit devLocalAuthorityEnabled reports.
// Called exactly once, at server construction (server.go, immediately after
// NewAuthzService), from the same cfg.DevAuthToken != "" condition that
// governs dev-token acceptance and DevUserID seeding — never from a
// request. A setter (rather than a NewAuthzService parameter) keeps
// NewAuthzService(store, logger)'s signature unchanged for its many
// existing callers.
func (a *AuthzService) setDevLocalAuthorityEnabled(enabled bool) {
	a.devLocalEnabled = enabled
}

// userContextKey is the key for storing the user in the request context.
type userContextKey struct{}

// DevAuthMiddleware creates middleware that validates development tokens.
// If the token is valid, it adds a DevUser to the request context.
// Use DevAuthMiddlewareWithDebug for verbose logging of auth failures.
func DevAuthMiddleware(validToken string, userCfg DevUserConfig) func(http.Handler) http.Handler {
	return DevAuthMiddlewareWithDebug(validToken, userCfg, false)
}

// DevAuthMiddlewareWithDebug creates middleware with optional debug logging.
func DevAuthMiddlewareWithDebug(validToken string, userCfg DevUserConfig, debug bool) func(http.Handler) http.Handler {
	devUser := NewDevUser(userCfg)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w = normalizeConstraintAuditAuthFailures(w, r)
			// Skip auth for health endpoints
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				serveAfterAuth(w, next, r)
				return
			}

			// Check if already authenticated by agent token middleware
			if GetAgentFromContext(r.Context()) != nil {
				if debug {
					slog.Debug("Auth success: agent token already validated")
				}
				serveAfterAuth(w, next, r)
				return
			}

			// Check for X-Scion-Agent-Token header - if present, skip dev auth
			// (the agent token middleware will have validated it or rejected it)
			if r.Header.Get("X-Scion-Agent-Token") != "" {
				// Agent token was present but not validated - reject
				if debug {
					slog.Debug("Auth failed: X-Scion-Agent-Token present but not validated")
				}
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"invalid agent token", nil)
				return
			}

			// Extract token from Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				if debug {
					slog.Debug("Auth failed: missing Authorization header",
						"method", r.Method,
						"path", r.URL.Path,
					)
				}
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"missing authorization header", nil)
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
				if debug {
					slog.Debug("Auth failed: invalid Authorization header format (expected 'Bearer <token>')")
				}
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"invalid authorization header format", nil)
				return
			}

			token := parts[1]

			// Validate token (constant-time comparison)
			if !apiclient.ValidateDevToken(token, validToken) {
				if debug {
					// Log token prefix for debugging (safe: only shows first chars)
					tokenPrefix := token
					if len(tokenPrefix) > 20 {
						tokenPrefix = tokenPrefix[:20] + "..."
					}
					expectedPrefix := validToken
					if len(expectedPrefix) > 20 {
						expectedPrefix = expectedPrefix[:20] + "..."
					}
					slog.Debug("Auth failed: token mismatch",
						"provided_prefix", tokenPrefix,
						"expected_prefix", expectedPrefix,
					)
				}
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"invalid token", nil)
				return
			}

			if debug {
				slog.Debug("Auth success: dev-user authenticated")
			}

			// Add dev user context
			ctx := context.WithValue(r.Context(), userContextKey{}, devUser)
			serveAfterAuth(w, next, r.WithContext(ctx))
		})
	}
}

// GetUserFromContext retrieves the user from the request context.
func GetUserFromContext(ctx context.Context) *DevUser {
	if user, ok := ctx.Value(userContextKey{}).(*DevUser); ok {
		return user
	}
	return nil
}
