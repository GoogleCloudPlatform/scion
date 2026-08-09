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
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// FederationTokenHeader is the HTTP header carrying federation OIDC identity tokens.
const FederationTokenHeader = "X-Scion-Federation-Token"

// DefaultFederationScopes is the scope set granted to federated agents when
// no per-issuer default_scopes are configured.
var DefaultFederationScopes = []AgentTokenScope{
	ScopeAgentStatusUpdate,
	ScopeAgentLogAppend,
}

// FederationAuthenticator validates OIDC identity tokens from trusted external issuers.
type FederationAuthenticator struct {
	issuers    map[string]*issuerEntry
	algorithms []jose.SignatureAlgorithm
	log        *slog.Logger
}

// issuerEntry holds the configuration and JWKS cache for a single trusted issuer.
type issuerEntry struct {
	config config.TrustedIssuerConfig
	cache  *jwksCache
}

// federationClaims is the claims shape for inbound federation OIDC identity tokens.
// Separate from OIDCIdentityTokenClaims (outbound) to maintain trust boundary separation.
type federationClaims struct {
	jwt.Claims
	ProjectID     string   `json:"project_id,omitempty"`
	AgentName     string   `json:"agent_name,omitempty"`
	Ancestry      []string `json:"ancestry,omitempty"`
	RootUser      string   `json:"root_user,omitempty"`
	Email         string   `json:"email,omitempty"`
	EmailVerified bool     `json:"email_verified,omitempty"`
	Name          string   `json:"name,omitempty"`
}

// NewFederationAuthenticator creates a FederationAuthenticator from the given config.
// oidcIssuerURL is this hub's own OIDC issuer URL, used as the default expected audience.
// httpClient is used for JWKS endpoint fetches. The caller should configure
// CheckRedirect to return http.ErrUseLastResponse to prevent open-redirect attacks.
// mode is the server mode (e.g. "workstation", "dev", "hosted"); non-dev/workstation
// modes reject HTTP issuer URLs for security.
func NewFederationAuthenticator(cfg config.FederationConfig, oidcIssuerURL string,
	httpClient *http.Client, mode string, log *slog.Logger) (*FederationAuthenticator, error) {

	// Validate config.
	if errs := cfg.Validate(); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return nil, fmt.Errorf("federation config validation failed: %s", strings.Join(msgs, "; "))
	}

	issuers := make(map[string]*issuerEntry, len(cfg.TrustedIssuers))

	for _, issuer := range cfg.TrustedIssuers {
		// Normalize issuer URL by trimming trailing slashes for consistent map lookup.
		normalizedIssuer := strings.TrimRight(issuer.IssuerURL, "/")

		// Fix 2: In hosted mode (not workstation, not dev), reject HTTP issuer URLs.
		if mode != "workstation" && mode != "dev" {
			u, err := url.Parse(normalizedIssuer)
			if err != nil {
				return nil, fmt.Errorf("invalid issuer_url %q: %v", issuer.IssuerURL, err)
			}
			if u.Scheme == "http" {
				return nil, fmt.Errorf("issuer_url %q uses http, which is not allowed in %q mode (HTTPS required)", issuer.IssuerURL, mode)
			}
		}

		// Resolve JWKS URL with 3-tier resolution:
		// 1. Explicit jwks_url config (always wins)
		// 2. Non-hub: OIDC discovery ({iss}/.well-known/openid-configuration -> jwks_uri)
		// 3. Hub: derive {iss}/.well-known/jwks.json (existing convention)
		jwksURL := issuer.JWKSURL
		if jwksURL == "" {
			issuerType := IssuerType(issuer.IssuerType)
			if issuerType == "" {
				issuerType = IssuerTypeHub
			}
			if issuerType != IssuerTypeHub {
				requireHTTPS := mode != "dev" && mode != "workstation"
				discovered, err := discoverJWKSURL(issuer.IssuerURL, httpClient, requireHTTPS)
				if err != nil {
					return nil, fmt.Errorf("federation: issuer %q: jwks_url not configured and OIDC discovery failed: %w", issuer.IssuerURL, err)
				}
				jwksURL = discovered
			} else {
				// Hub convention: derive from issuer URL
				jwksURL = strings.TrimRight(issuer.IssuerURL, "/") + "/.well-known/jwks.json"
			}
		}

		// Resolve expected audience: fall back to this hub's OIDC issuer URL.
		expectedAud := issuer.ExpectedAudience
		if expectedAud == "" {
			expectedAud = oidcIssuerURL
		}
		if expectedAud == "" {
			return nil, fmt.Errorf("issuer %q: expected_audience is empty and no oidcIssuerURL provided", issuer.IssuerURL)
		}

		// Store the resolved audience back into the config copy for later use.
		resolvedCfg := issuer
		resolvedCfg.ExpectedAudience = expectedAud

		// Create a jwksCache with configurable intervals.
		cache := &jwksCache{
			url:              jwksURL,
			client:           httpClient,
			refreshInterval:  cfg.Cache.RefreshInterval,
			debounceInterval: cfg.Cache.DebounceInterval,
		}

		issuers[normalizedIssuer] = &issuerEntry{
			config: resolvedCfg,
			cache:  cache,
		}
	}

	// Build algorithms list: default to RS256 if none configured.
	var algorithms []jose.SignatureAlgorithm
	if len(cfg.Algorithms) == 0 {
		algorithms = []jose.SignatureAlgorithm{jose.RS256}
	} else {
		algorithms = make([]jose.SignatureAlgorithm, len(cfg.Algorithms))
		for i, alg := range cfg.Algorithms {
			algorithms[i] = jose.SignatureAlgorithm(alg)
		}
	}

	return &FederationAuthenticator{
		issuers:    issuers,
		algorithms: algorithms,
		log:        log,
	}, nil
}

// Authenticate validates a federation OIDC identity token and returns the
// authenticated FederatedIdentity on success.
func (a *FederationAuthenticator) Authenticate(tokenString string) (FederatedIdentity, error) {
	// 1. Parse JWT with algorithm pinning — rejects wrong algorithms at parse time.
	tok, err := jwt.ParseSigned(tokenString, a.algorithms)
	if err != nil {
		return nil, fmt.Errorf("federation: failed to parse JWT: %w", err)
	}

	// 2. Extract kid from JWT header.
	if len(tok.Headers) == 0 {
		return nil, fmt.Errorf("federation: JWT has no headers")
	}
	kid := tok.Headers[0].KeyID
	if kid == "" {
		return nil, fmt.Errorf("federation: JWT has no kid")
	}

	// 3. Extract unverified issuer to look up the correct key set.
	var unverified federationClaims
	if err := tok.UnsafeClaimsWithoutVerification(&unverified); err != nil {
		return nil, fmt.Errorf("federation: failed to peek at claims: %w", err)
	}

	// 4. Look up issuer (normalize trailing slashes for consistent matching).
	normalizedIssuer := strings.TrimRight(unverified.Issuer, "/")
	entry, ok := a.issuers[normalizedIssuer]
	if !ok {
		return nil, fmt.Errorf("federation: untrusted issuer %q", unverified.Issuer)
	}

	// 5. Fetch public key via JWKS cache.
	key, err := entry.cache.GetKey(kid)
	if err != nil {
		return nil, fmt.Errorf("federation: JWKS key lookup failed for kid %q: %w", kid, err)
	}

	// 6. Verify signature and extract verified claims.
	var claims federationClaims
	if err := tok.Claims(key, &claims); err != nil {
		return nil, fmt.Errorf("federation: JWT signature verification failed: %w", err)
	}

	// 7. Validate standard claims (iss, aud, exp, nbf with clock skew).
	expectedAud := entry.config.ExpectedAudience
	now := time.Now()

	// Normalize issuer for comparison (handles trailing slash differences).
	claims.Issuer = strings.TrimRight(claims.Issuer, "/")

	expected := jwt.Expected{
		Issuer:      strings.TrimRight(entry.config.IssuerURL, "/"),
		AnyAudience: jwt.Audience{expectedAud},
		Time:        now,
	}
	// Apply clock skew tolerance.
	if err := claims.ValidateWithLeeway(expected, iapClockSkew); err != nil {
		return nil, fmt.Errorf("federation: claims validation failed: %w", err)
	}

	// 8. Extract identity based on issuer type.
	issuerType := IssuerType(entry.config.IssuerType)
	if issuerType == "" {
		issuerType = IssuerTypeHub
	}

	// Build scopes.
	scopes := DefaultFederationScopes
	if len(entry.config.DefaultScopes) > 0 {
		scopes = make([]AgentTokenScope, len(entry.config.DefaultScopes))
		for i, s := range entry.config.DefaultScopes {
			scopes[i] = AgentTokenScope(s)
		}
	} else if issuerType != IssuerTypeHub {
		// Non-hub issuers default to empty scopes (zero-trust).
		scopes = nil
	}

	var identity FederatedIdentity
	switch issuerType {
	case IssuerTypeHub:
		identity, err = extractHubClaims(&claims, entry.config, scopes)
	case IssuerTypeServiceAccount:
		identity, err = extractServiceAccountClaims(&claims, entry.config, scopes)
	case IssuerTypeUser:
		identity, err = extractUserClaims(&claims, entry.config, scopes)
	default:
		return nil, fmt.Errorf("federation: unknown issuer type %q", issuerType)
	}
	if err != nil {
		return nil, err
	}

	// 9. Apply issuer constraints based on type.
	switch issuerType {
	case IssuerTypeHub:
		if len(entry.config.AllowedProjects) > 0 {
			if !contains(entry.config.AllowedProjects, claims.ProjectID) {
				return nil, fmt.Errorf("federation: project %q not in allowed_projects", claims.ProjectID)
			}
		}
		if len(entry.config.AllowedRootUsers) > 0 {
			if !contains(entry.config.AllowedRootUsers, claims.RootUser) {
				return nil, fmt.Errorf("federation: root_user %q not in allowed_root_users", claims.RootUser)
			}
		}
	case IssuerTypeServiceAccount, IssuerTypeUser:
		if len(entry.config.AllowedEmails) > 0 {
			var email string
			if sid, ok := identity.(*FederatedServiceIdentity); ok {
				email = sid.Email()
			} else if uid, ok := identity.(*FederatedUserIdentity); ok {
				email = uid.Email()
			}
			if !matchesAllowedEmails(entry.config.AllowedEmails, email) {
				return nil, fmt.Errorf("federation: email %q not in allowed_emails", email)
			}
		}
	}

	a.log.Debug("federation token validated",
		"issuer", identity.IssuerURL(),
		"type", identity.Type(),
		"id", identity.ID(),
	)

	return identity, nil
}

// extractHubClaims extracts Scion hub agent claims. This is the existing
// extraction logic moved into a function.
func extractHubClaims(claims *federationClaims, issuerCfg config.TrustedIssuerConfig,
	scopes []AgentTokenScope) (FederatedIdentity, error) {
	if claims.Subject == "" {
		return nil, fmt.Errorf("federation: empty sub claim")
	}
	return NewFederatedAgentIdentity(
		claims.Issuer, claims.Subject, claims.ProjectID,
		claims.AgentName, claims.RootUser, claims.Ancestry, scopes,
	), nil
}

// extractServiceAccountClaims extracts GCP service account claims.
func extractServiceAccountClaims(claims *federationClaims,
	issuerCfg config.TrustedIssuerConfig, scopes []AgentTokenScope) (FederatedIdentity, error) {
	if claims.Subject == "" {
		return nil, fmt.Errorf("federation: service account token missing sub claim")
	}
	if claims.Email == "" {
		return nil, fmt.Errorf("federation: service account token missing email claim")
	}
	return NewFederatedServiceIdentity(
		claims.Issuer, claims.Subject, claims.Email, scopes,
	), nil
}

// extractUserClaims extracts Firebase/Google user claims.
func extractUserClaims(claims *federationClaims,
	issuerCfg config.TrustedIssuerConfig, scopes []AgentTokenScope) (FederatedIdentity, error) {
	if claims.Subject == "" {
		return nil, fmt.Errorf("federation: user token missing sub claim")
	}
	role := issuerCfg.DefaultRole
	if role == "" {
		role = "viewer"
	}
	return NewFederatedUserIdentity(
		claims.Issuer, claims.Subject, claims.Email, claims.Name, role, scopes,
	), nil
}

// matchesAllowedEmails checks if an email matches any pattern in the allowed list.
// Supports exact match and leading-wildcard suffix match (e.g. "*@example.com").
// Note: a bare "*" pattern matches all emails (the suffix after "*" is empty,
// and strings.HasSuffix always returns true for an empty suffix). Use an empty
// AllowedEmails slice instead to express "accept all emails" in configuration.
func matchesAllowedEmails(patterns []string, email string) bool {
	email = strings.ToLower(email)
	for _, pattern := range patterns {
		pattern = strings.ToLower(pattern)
		if strings.HasPrefix(pattern, "*") {
			// Leading-wildcard: match suffix.
			suffix := pattern[1:] // strip the *
			if strings.HasSuffix(email, suffix) {
				return true
			}
		} else {
			// Exact match.
			if pattern == email {
				return true
			}
		}
	}
	return false
}

// contains checks if a string is present in a slice.
func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
