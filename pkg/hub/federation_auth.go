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
	ProjectID string   `json:"project_id,omitempty"`
	AgentName string   `json:"agent_name,omitempty"`
	Ancestry  []string `json:"ancestry,omitempty"`
	RootUser  string   `json:"root_user,omitempty"`
}

// NewFederationAuthenticator creates a FederationAuthenticator from the given config.
// oidcIssuerURL is this hub's own OIDC issuer URL, used as the default expected audience.
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
		// Fix 2: In hosted mode (not workstation, not dev), reject HTTP issuer URLs.
		if mode != "workstation" && mode != "dev" {
			u, err := url.Parse(issuer.IssuerURL)
			if err != nil {
				return nil, fmt.Errorf("invalid issuer_url %q: %v", issuer.IssuerURL, err)
			}
			if u.Scheme == "http" {
				return nil, fmt.Errorf("issuer_url %q uses http, which is not allowed in %q mode (HTTPS required)", issuer.IssuerURL, mode)
			}
		}

		// Resolve JWKS URL: derive from issuer URL if not explicitly set.
		jwksURL := issuer.JWKSURL
		if jwksURL == "" {
			jwksURL = strings.TrimRight(issuer.IssuerURL, "/") + "/.well-known/jwks.json"
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

		issuers[issuer.IssuerURL] = &issuerEntry{
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
// authenticated FederatedAgentIdentity on success.
func (a *FederationAuthenticator) Authenticate(tokenString string) (*FederatedAgentIdentity, error) {
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

	// 4. Look up issuer.
	entry, ok := a.issuers[unverified.Issuer]
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

	expected := jwt.Expected{
		Issuer:      unverified.Issuer,
		AnyAudience: jwt.Audience{expectedAud},
		Time:        now,
	}
	// Apply clock skew tolerance.
	if err := claims.ValidateWithLeeway(expected, iapClockSkew); err != nil {
		return nil, fmt.Errorf("federation: claims validation failed: %w", err)
	}

	// 8. Validate federation-specific constraints.
	// Sub (agent ID) must be non-empty.
	if claims.Subject == "" {
		return nil, fmt.Errorf("federation: empty sub claim")
	}

	// If allowed_projects is non-empty, project_id must be in the list.
	if len(entry.config.AllowedProjects) > 0 {
		if !contains(entry.config.AllowedProjects, claims.ProjectID) {
			return nil, fmt.Errorf("federation: project %q not in allowed_projects", claims.ProjectID)
		}
	}

	// If allowed_root_users is non-empty, root_user must be in the list.
	if len(entry.config.AllowedRootUsers) > 0 {
		if !contains(entry.config.AllowedRootUsers, claims.RootUser) {
			return nil, fmt.Errorf("federation: root_user %q not in allowed_root_users", claims.RootUser)
		}
	}

	// 9. Build scopes.
	scopes := DefaultFederationScopes
	if len(entry.config.DefaultScopes) > 0 {
		scopes = make([]AgentTokenScope, len(entry.config.DefaultScopes))
		for i, s := range entry.config.DefaultScopes {
			scopes[i] = AgentTokenScope(s)
		}
	}

	// 10. Construct and return FederatedAgentIdentity.
	identity := NewFederatedAgentIdentity(
		claims.Issuer,
		claims.Subject,
		claims.ProjectID,
		claims.AgentName,
		claims.RootUser,
		claims.Ancestry,
		scopes,
	)

	a.log.Info("federation token validated",
		"issuer", claims.Issuer,
		"subject", claims.Subject,
		"project_id", claims.ProjectID,
		"agent_name", claims.AgentName,
	)

	return identity, nil
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
