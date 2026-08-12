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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxDiscoveryResponseSize limits the OIDC discovery response body to 1 MB
// to prevent DoS from oversized responses.
const maxDiscoveryResponseSize = 1 << 20 // 1 MB

// OIDCDiscoveryDoc holds the endpoints extracted from an OIDC
// .well-known/openid-configuration response.
type OIDCDiscoveryDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// discoverOIDCEndpoints fetches and parses the OIDC discovery document
// from {issuerURL}/.well-known/openid-configuration.
// Returns an error if any required endpoint (authorization, token, userinfo) is missing.
func discoverOIDCEndpoints(issuerURL string, client *http.Client) (*OIDCDiscoveryDoc, error) {
	discoveryURL := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
	resp, err := client.Get(discoveryURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: failed to fetch %s: %w", discoveryURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery: %s returned status %d", discoveryURL, resp.StatusCode)
	}

	var doc OIDCDiscoveryDoc
	limitedBody := io.LimitReader(resp.Body, maxDiscoveryResponseSize)
	if err := json.NewDecoder(limitedBody).Decode(&doc); err != nil {
		return nil, fmt.Errorf("oidc discovery: failed to parse response from %s: %w", discoveryURL, err)
	}

	if doc.AuthorizationEndpoint == "" {
		return nil, fmt.Errorf("oidc discovery: no authorization_endpoint in response from %s", discoveryURL)
	}
	if doc.TokenEndpoint == "" {
		return nil, fmt.Errorf("oidc discovery: no token_endpoint in response from %s", discoveryURL)
	}
	if doc.UserinfoEndpoint == "" {
		return nil, fmt.Errorf("oidc discovery: no userinfo_endpoint in response from %s", discoveryURL)
	}

	// Validate that all discovered endpoints use HTTPS (allow http for localhost/127.0.0.1).
	for name, ep := range map[string]string{
		"authorization_endpoint": doc.AuthorizationEndpoint,
		"token_endpoint":         doc.TokenEndpoint,
		"userinfo_endpoint":      doc.UserinfoEndpoint,
	} {
		if err := validateOIDCEndpointScheme(ep, name); err != nil {
			return nil, err
		}
	}

	return &doc, nil
}

// validateOIDCEndpointScheme ensures an OIDC endpoint URL uses the https scheme.
// http is permitted only for localhost (127.0.0.1) to support local development.
func validateOIDCEndpointScheme(endpoint, fieldName string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("oidc discovery: invalid %s URL %q: %w", fieldName, endpoint, err)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	// Allow http for localhost / 127.0.0.1 (local dev/testing).
	host := parsed.Hostname()
	if parsed.Scheme == "http" && (host == "localhost" || host == "127.0.0.1") {
		return nil
	}
	return fmt.Errorf("oidc discovery: %s %q must use HTTPS scheme", fieldName, endpoint)
}

// discoverJWKSURL fetches the OIDC discovery document from the issuer and
// extracts the jwks_uri field. Returns an error if the discovery document
// cannot be fetched or does not contain a jwks_uri.
// When requireHTTPS is true, the discovered jwks_uri must use the HTTPS scheme.
func discoverJWKSURL(issuerURL string, client *http.Client, requireHTTPS bool) (string, error) {
	discoveryURL := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
	resp, err := client.Get(discoveryURL)
	if err != nil {
		return "", fmt.Errorf("oidc discovery: failed to fetch %s: %w", discoveryURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc discovery: %s returned status %d", discoveryURL, resp.StatusCode)
	}

	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	limitedBody := io.LimitReader(resp.Body, maxDiscoveryResponseSize)
	if err := json.NewDecoder(limitedBody).Decode(&doc); err != nil {
		return "", fmt.Errorf("oidc discovery: failed to parse response from %s: %w", discoveryURL, err)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("oidc discovery: no jwks_uri in response from %s", discoveryURL)
	}

	if requireHTTPS {
		parsed, err := url.Parse(doc.JWKSURI)
		if err != nil {
			return "", fmt.Errorf("oidc discovery: invalid jwks_uri %q: %w", doc.JWKSURI, err)
		}
		if parsed.Scheme != "https" {
			return "", fmt.Errorf("oidc discovery: jwks_uri %q must use HTTPS", doc.JWKSURI)
		}
	}

	return doc.JWKSURI, nil
}
