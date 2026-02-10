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
	"log/slog"
	"net/http"
	"strings"

	"github.com/ptone/scion-agent/pkg/apiclient"
)

// DevUser represents the pseudo-user for development authentication.
type DevUser struct {
	id string
}

// ID returns the user ID.
func (u *DevUser) ID() string { return u.id }

// Type returns the identity type ("dev").
func (u *DevUser) Type() string { return "dev" }

// Email returns the user email.
func (u *DevUser) Email() string { return "dev@localhost" }

// DisplayName returns the user display name.
func (u *DevUser) DisplayName() string { return "Development User" }

// Role returns the user role.
func (u *DevUser) Role() string { return "admin" }

// userContextKey is the key for storing the user in the request context.
type userContextKey struct{}

// DevAuthMiddleware creates middleware that validates development tokens.
// If the token is valid, it adds a DevUser to the request context.
// Use DevAuthMiddlewareWithDebug for verbose logging of auth failures.
func DevAuthMiddleware(validToken string) func(http.Handler) http.Handler {
	return DevAuthMiddlewareWithDebug(validToken, false)
}

// DevAuthMiddlewareWithDebug creates middleware with optional debug logging.
func DevAuthMiddlewareWithDebug(validToken string, debug bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth for health endpoints
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				next.ServeHTTP(w, r)
				return
			}

			// Check if already authenticated by agent token middleware
			if GetAgentFromContext(r.Context()) != nil {
				if debug {
					slog.Debug("Auth success: agent token already validated")
				}
				next.ServeHTTP(w, r)
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
			ctx := context.WithValue(r.Context(), userContextKey{}, &DevUser{id: "dev-user"})
			next.ServeHTTP(w, r.WithContext(ctx))
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
