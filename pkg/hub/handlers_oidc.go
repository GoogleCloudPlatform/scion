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
	"encoding/json"
	"net/http"
)

// oidcDiscoveryDocument represents the OpenID Connect Provider Metadata
// returned by the /.well-known/openid-configuration endpoint.
type oidcDiscoveryDocument struct {
	Issuer                            string   `json:"issuer"`
	JWKSURI                           string   `json:"jwks_uri"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	SubjectTypesSupported             []string `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported  []string `json:"id_token_signing_alg_values_supported"`
	ClaimsSupported                   []string `json:"claims_supported"`
	ScopesSupported                   []string `json:"scopes_supported"`
}

// handleOIDCDiscovery serves the OIDC Provider Metadata at
// GET /.well-known/openid-configuration.
func (s *Server) handleOIDCDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	issuer := s.oidcIssuerURL

	doc := oidcDiscoveryDocument{
		Issuer:                           issuer,
		JWKSURI:                          issuer + "/.well-known/jwks.json",
		ResponseTypesSupported:           []string{"id_token"},
		SubjectTypesSupported:            []string{"public"},
		IDTokenSigningAlgValuesSupported: []string{"RS256"},
		ClaimsSupported: []string{
			"iss", "sub", "aud", "iat", "exp", "nbf", "jti",
			"project_id", "agent_name", "ancestry", "root_user",
		},
		ScopesSupported: []string{"openid"},
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if err := json.NewEncoder(w).Encode(doc); err != nil {
		http.Error(w, "failed to encode discovery document", http.StatusInternalServerError)
	}
}

// handleJWKS serves the JSON Web Key Set at GET /.well-known/jwks.json.
func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	jwks := s.oidcKeyManager.JWKS()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if err := json.NewEncoder(w).Encode(jwks); err != nil {
		http.Error(w, "failed to encode JWKS", http.StatusInternalServerError)
	}
}
