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

package bridge

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// FederationTokenHeader is the HTTP header used to pass federation OIDC
// identity tokens from the bridge to the hub.
// IMPORTANT: must match pkg/hub/federation_auth.go:FederationTokenHeader.
// See TestFederationTokenHeaderMatchesHub in federation_test.go.
const FederationTokenHeader = "X-Scion-Federation-Token"

// flexibleAudience handles the RFC 7519 aud claim which may be either a
// single string or an array of strings.
type flexibleAudience []string

func (a *flexibleAudience) UnmarshalJSON(data []byte) error {
	// Try string first.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*a = []string{s}
		return nil
	}
	// Fall back to array.
	var arr []string
	if err := json.Unmarshal(data, &arr); err != nil {
		return err
	}
	*a = arr
	return nil
}

// federationJWTClaims holds the JWT claims we extract from a federation
// token WITHOUT signature verification. The hub validates the token;
// the bridge only decodes it for local bookkeeping (task isolation, logging).
//
// Mirrors the hub's federationClaims (pkg/hub/federation_auth.go).
type federationJWTClaims struct {
	Issuer    string           `json:"iss"`
	Subject   string           `json:"sub"`
	Audience  flexibleAudience `json:"aud"`
	ProjectID string           `json:"project_id,omitempty"`
	AgentName string           `json:"agent_name,omitempty"`
	Ancestry  []string         `json:"ancestry,omitempty"`
	RootUser  string           `json:"root_user,omitempty"`
}

// decodeFederationToken parses a JWT WITHOUT signature verification and
// extracts caller identity fields for bridge-local bookkeeping (task store
// isolation, logging, sender formatting). The hub validates the token via
// its FederationAuthenticator when the bridge passes it in the
// X-Scion-Federation-Token header.
func decodeFederationToken(tokenString string) (*CallerIdentity, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("federation: token is not a valid JWT (expected 3 parts, got %d)", len(parts))
	}

	// Decode the payload (second part), adding padding if needed.
	payloadStr := parts[1]
	if l := len(payloadStr) % 4; l > 0 {
		payloadStr += strings.Repeat("=", 4-l)
	}
	payload, err := base64.URLEncoding.DecodeString(payloadStr)
	if err != nil {
		return nil, fmt.Errorf("federation: failed to decode JWT payload: %w", err)
	}

	var claims federationJWTClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("federation: failed to parse JWT claims: %w", err)
	}

	if claims.Subject == "" {
		return nil, fmt.Errorf("federation: JWT missing sub claim")
	}

	return &CallerIdentity{
		TokenType: "federation",
		RawToken:  tokenString,
		AgentID:   claims.Subject,
		IssuerURL: claims.Issuer,
		ProjectID: claims.ProjectID,
		Ancestry:  claims.Ancestry,
	}, nil
}

// federationHeaderTransport wraps an http.RoundTripper and injects the
// X-Scion-Federation-Token header on every request.
type federationHeaderTransport struct {
	base  http.RoundTripper
	token string
}

func (t *federationHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set(FederationTokenHeader, t.token)
	return t.base.RoundTrip(clone)
}
