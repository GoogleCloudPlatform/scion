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

package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

const (
	wwwAuthenticateHeader = `Bearer realm="scion-mcp"`
	delegatedAgentPrefix  = "scion_adt_"
	agentJWTIssuer        = "scion-hub"
	agentJWTAudience      = "scion-hub-api"
)

type requestAuthKey struct{}

// requestAuth holds the front-door-validated credential and resolved principal
// for the current MCP HTTP request.
type requestAuth struct {
	Credential Credential
	Principal  Principal
	UserAgent  string
}

func withRequestAuth(ctx context.Context, ra requestAuth) context.Context {
	return context.WithValue(ctx, requestAuthKey{}, ra)
}

func requestAuthFromContext(ctx context.Context) (requestAuth, bool) {
	ra, ok := ctx.Value(requestAuthKey{}).(requestAuth)
	return ra, ok
}

// frontDoor enforces the MCP HTTP front-door policies (§4.2, §4.5):
//  1. Shutdown refusal (503)
//  2. Per-request hub.mcp_server experiment check (404 when off)
//  3. Origin allowlist (403 on disallowed Origin; absent Origin is allowed)
//  4. Credential extraction (Authorization: Bearer xor X-Scion-Federation-Token;
//     401 + WWW-Authenticate for none/both; 401 unsupported_credential for
//     agent JWT, scion_adt_, and scion_dev_)
//  5. Principal resolution via GET /api/v1/auth/me (cached <= 60s)
func (s *Server) frontDoor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.shuttingDown.Load() {
			writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "MCP server is shutting down")
			return
		}

		enabled, err := s.backend.ExperimentEnabled(r.Context(), Credential{}, experiments.MCPServer)
		if err != nil {
			s.logger.Warn("MCP experiment check failed", "error", err)
			writeJSONError(w, http.StatusServiceUnavailable, "experiment_check_failed", "unable to check MCP experiment status")
			return
		}
		if !enabled {
			writeJSONError(w, http.StatusNotFound, "not_found", "MCP server experiment is not enabled")
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" {
			if !s.isOriginAllowed(origin) {
				writeJSONError(w, http.StatusForbidden, "origin_not_allowed", "Origin header is not in allowed_origins")
				return
			}
		}

		cred, errCode, errMsg := extractAndClassifyCredential(r)
		if errCode != "" {
			w.Header().Set("WWW-Authenticate", wwwAuthenticateHeader)
			writeJSONError(w, http.StatusUnauthorized, errCode, errMsg)
			return
		}

		ua := r.Header.Get("User-Agent")
		ctx := withTransportMeta(r.Context(), transportMeta{
			RemoteAddr: r.RemoteAddr,
			ClientName: ua,
		})

		principal, err := s.resolvePrincipal(ctx, cred)
		if err != nil {
			var apiErr *apiclient.APIError
			if errors.As(err, &apiErr) {
				if apiErr.StatusCode == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", wwwAuthenticateHeader)
					writeJSONError(w, http.StatusUnauthorized, "unauthorized", apiErr.Message)
					return
				}
				if apiErr.StatusCode == http.StatusForbidden {
					writeJSONError(w, http.StatusForbidden, "forbidden", apiErr.Message)
					return
				}
			}
			s.logger.Warn("MCP principal resolution failed", "credential_class", cred.Class(), "error", err)
			writeJSONError(w, http.StatusServiceUnavailable, "backend_unavailable", "unable to verify credential with Hub")
			return
		}

		ctx = withRequestAuth(ctx, requestAuth{
			Credential: cred,
			Principal:  principal,
			UserAgent:  ua,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) isOriginAllowed(origin string) bool {
	if len(s.allowedOrigins) == 0 {
		return false
	}
	_, ok := s.allowedOrigins[origin]
	return ok
}

func (s *Server) resolvePrincipal(ctx context.Context, cred Credential) (Principal, error) {
	key := cred.Hash()
	now := time.Now()
	if p, ok := s.principalCache.get(key, now); ok {
		return p, nil
	}
	p, err := s.backend.Me(ctx, cred)
	if err != nil {
		return Principal{}, err
	}
	s.principalCache.set(key, p, now)
	return p, nil
}

// extractAndClassifyCredential reads Authorization: Bearer and
// X-Scion-Federation-Token, enforces strict XOR, and refuses unsupported
// credential classes (agent JWT, scion_adt_, scion_dev_) without validating them.
func extractAndClassifyCredential(r *http.Request) (cred Credential, errCode, errMsg string) {
	var bearer string
	if authz := strings.TrimSpace(r.Header.Get("Authorization")); authz != "" {
		scheme, token, ok := strings.Cut(authz, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			return Credential{}, "unauthorized", "Authorization header must use Bearer scheme"
		}
		bearer = strings.TrimSpace(token)
	}

	fed := strings.TrimSpace(r.Header.Get(HeaderFederationToken))

	switch {
	case bearer == "" && fed == "":
		return Credential{}, "unauthorized", "missing credential: provide Authorization: Bearer or X-Scion-Federation-Token"
	case bearer != "" && fed != "":
		return Credential{}, "unauthorized", "ambiguous credentials: provide only one of Authorization: Bearer or X-Scion-Federation-Token"
	}

	if bearer != "" {
		if strings.HasPrefix(bearer, apiclient.DevTokenPrefix) {
			return Credential{}, "unsupported_credential", "dev token credentials are not accepted on the MCP endpoint"
		}
		if strings.HasPrefix(bearer, delegatedAgentPrefix) {
			return Credential{}, "unsupported_credential", "agent delegated credentials are not accepted on the MCP endpoint"
		}
		if isAgentJWT(bearer) {
			return Credential{}, "unsupported_credential", "agent JWT credentials are not accepted on the MCP endpoint"
		}
		return Credential{Bearer: bearer}, "", ""
	}

	return Credential{Federation: fed}, "", ""
}

// isAgentJWT performs an unverified structural peek on a compact JWS payload
// to identify Scion agent JWTs (iss="scion-hub", no user "type" claim, and
// aud="scion-hub-api" or agent claims "scopes"/"project_id"). User JWTs carry
// a non-empty "type" claim ("access", "cli", "refresh") and are not matched.
func isAgentJWT(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var peek struct {
		Issuer    string   `json:"iss"`
		Audience  any      `json:"aud"`
		TokenType string   `json:"type"`
		Scopes    []string `json:"scopes"`
		ProjectID string   `json:"project_id"`
	}
	if err := json.Unmarshal(payloadBytes, &peek); err != nil {
		return false
	}
	if peek.Issuer != agentJWTIssuer || peek.TokenType != "" {
		return false
	}
	if len(peek.Scopes) > 0 || peek.ProjectID != "" {
		return true
	}
	return audienceContains(peek.Audience, agentJWTAudience)
}

func audienceContains(aud any, target string) bool {
	switch v := aud.(type) {
	case string:
		return v == target
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == target {
				return true
			}
		}
	}
	return false
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": message,
	})
}
