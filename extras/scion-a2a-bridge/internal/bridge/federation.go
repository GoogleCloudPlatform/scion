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
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
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
	Email     string           `json:"email,omitempty"`
	// TokenKind is the hub-set credential kind: "user" or "agent". Empty on
	// credentials from issuers that predate the claim.
	TokenKind string `json:"scion_token_kind,omitempty"`
}

// Federation credential kinds (the scion_token_kind claim).
const (
	federationKindAgent = "agent"
	federationKindUser  = "user"
)

// hasAgentClaims reports whether the claims carry any agent claim.
func (c *federationJWTClaims) hasAgentClaims() bool {
	return c.ProjectID != "" || c.AgentName != "" || len(c.Ancestry) > 0 || c.RootUser != ""
}

// classifyFederationClaims returns the kind of caller a federation
// credential names: federationKindAgent or federationKindUser.
//
//   - scion_token_kind "agent": an agent.
//   - scion_token_kind "user": a user; refused when it also carries an agent
//     claim (it would claim both kinds) or has no email.
//   - no scion_token_kind (issuers that predate the claim): an agent when it
//     carries an agent claim, a user when it carries an email and no agent
//     claim, otherwise an agent (as before the claim existed).
//   - any other scion_token_kind: refused.
func classifyFederationClaims(c *federationJWTClaims) (string, error) {
	switch c.TokenKind {
	case federationKindAgent:
		return federationKindAgent, nil
	case federationKindUser:
		if c.hasAgentClaims() {
			return "", fmt.Errorf("federation: user credential carries agent claims")
		}
		if c.Email == "" {
			return "", fmt.Errorf("federation: user credential has no email claim")
		}
		return federationKindUser, nil
	case "":
		if !c.hasAgentClaims() && c.Email != "" {
			return federationKindUser, nil
		}
		return federationKindAgent, nil
	default:
		return "", fmt.Errorf("federation: unknown credential kind %q", c.TokenKind)
	}
}

// decodeFederationToken parses a JWT WITHOUT signature verification and
// extracts caller identity fields for bridge-local bookkeeping (task store
// isolation, logging, sender formatting). The hub validates the token via
// its FederationAuthenticator when the bridge passes it in the
// X-Scion-Federation-Token header.
//
// An agent credential yields an agent caller (AgentID = sub), exactly as
// before credential kinds existed. A user credential yields a federated
// user caller (Subject = sub, Email from the credential) whose UserID is
// still empty: the caller is usable only after Bridge.identifyFederatedUser
// has asked the hub which account the credential belongs to. When the
// credential belongs to no project visitor account (for example, an
// unlinked identity issuer user), the hub refuses it and the bridge passes
// the refusal on.
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

	kind, err := classifyFederationClaims(&claims)
	if err != nil {
		return nil, err
	}

	if kind == federationKindUser {
		return &CallerIdentity{
			TokenType: "federation",
			RawToken:  tokenString,
			Email:     claims.Email,
			IssuerURL: claims.Issuer,
			Subject:   claims.Subject,
		}, nil
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

// federationHeaderTransport wraps an http.RoundTripper for clients that talk
// only to the configured hub. It sends a request only when its origin (the
// scheme and the host and port) is the hub endpoint's, and then adds the
// X-Scion-Federation-Token header. Any other request, such as the target of
// a redirect the hub answers with, is refused with errFederationOtherOrigin
// before the wrapped RoundTripper (and any credential it adds) sees it.
type federationHeaderTransport struct {
	base      http.RoundTripper
	token     string
	hubScheme string // lower-cased scheme of the hub endpoint
	hubHost   string // canonical host:port of the hub endpoint
}

// errFederationOtherOrigin is returned for a request whose origin is not the
// hub endpoint's. Its text never includes the request URL.
var errFederationOtherOrigin = errors.New("federation transport: request outside the configured hub origin refused")

// newFederationHeaderTransport returns a federationHeaderTransport that sends
// requests only to the scheme, host and port of hubEndpoint.
func newFederationHeaderTransport(base http.RoundTripper, token, hubEndpoint string) (*federationHeaderTransport, error) {
	// The errors never include the endpoint text, which may carry user
	// information: a url.Parse error quotes its input, and an endpoint
	// without a host may hold user information outside u.User.
	u, err := url.Parse(hubEndpoint)
	if err != nil {
		return nil, errors.New("federation transport: hub endpoint is not a valid URL")
	}
	if u.Host == "" {
		return nil, errors.New("federation transport: hub endpoint has no host")
	}
	return &federationHeaderTransport{
		base:      base,
		token:     token,
		hubScheme: strings.ToLower(u.Scheme),
		hubHost:   canonicalHostPort(u),
	}, nil
}

// canonicalHostPort returns u's lower-cased host joined with its port, using
// the scheme's default port when u has none.
func canonicalHostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// isHubOrigin reports whether u has the hub endpoint's scheme, host and port.
func (t *federationHeaderTransport) isHubOrigin(u *url.URL) bool {
	return strings.ToLower(u.Scheme) == t.hubScheme && canonicalHostPort(u) == t.hubHost
}

func (t *federationHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.isHubOrigin(req.URL) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errFederationOtherOrigin
	}
	clone := req.Clone(req.Context())
	clone.Header.Set(FederationTokenHeader, t.token)
	return t.base.RoundTrip(clone)
}
