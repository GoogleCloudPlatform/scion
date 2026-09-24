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

package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// FederationConfig holds configuration for hub-hub federation authentication.
type FederationConfig struct {
	Enabled        bool                  `json:"enabled" yaml:"enabled" koanf:"enabled"`
	TrustedIssuers []TrustedIssuerConfig `json:"trusted_issuers,omitempty" yaml:"trusted_issuers,omitempty" koanf:"trusted_issuers"`
	Algorithms     []string              `json:"algorithms,omitempty" yaml:"algorithms,omitempty" koanf:"algorithms"`
	Cache          FederationCacheConfig `json:"cache,omitempty" yaml:"cache,omitempty" koanf:"cache"`
}

// TrustedIssuerConfig holds configuration for a single trusted OIDC issuer.
// The issuer is not assumed to be a Scion Hub — any OIDC-compliant issuer
// with a JWKS endpoint should work.
type TrustedIssuerConfig struct {
	IssuerURL        string   `json:"issuer_url" yaml:"issuer_url" koanf:"issuer_url"`
	JWKSURL          string   `json:"jwks_url,omitempty" yaml:"jwks_url,omitempty" koanf:"jwks_url"`
	ExpectedAudience string   `json:"expected_audience,omitempty" yaml:"expected_audience,omitempty" koanf:"expected_audience"`
	AllowedProjects  []string `json:"allowed_projects,omitempty" yaml:"allowed_projects,omitempty" koanf:"allowed_projects"`
	AllowedRootUsers []string `json:"allowed_root_users,omitempty" yaml:"allowed_root_users,omitempty" koanf:"allowed_root_users"`
	// DefaultScopes holds scope strings applied to federated agents from this issuer.
	// These are string representations of AgentTokenScope (defined in pkg/hub/agenttoken.go);
	// string is used here to avoid a circular import between pkg/config and pkg/hub.
	DefaultScopes []string `json:"default_scopes,omitempty" yaml:"default_scopes,omitempty" koanf:"default_scopes"`

	// IssuerType controls claims extraction and identity construction.
	// Default: "hub". Options: "hub", "service_account", "user".
	// Uses string (not the hub IssuerType type) to avoid circular imports.
	IssuerType string `json:"issuer_type,omitempty" yaml:"issuer_type,omitempty" koanf:"issuer_type"`

	// DefaultRole sets the role for federated user identities (issuer_type: user).
	// Ignored for other issuer types. Default: "viewer".
	DefaultRole string `json:"default_role,omitempty" yaml:"default_role,omitempty" koanf:"default_role"`

	// AllowedEmails restricts accepted tokens to specific email claims.
	// Supports leading-wildcard suffix matching (e.g. "*@example.com").
	// If empty, all emails accepted.
	AllowedEmails []string `json:"allowed_emails,omitempty" yaml:"allowed_emails,omitempty" koanf:"allowed_emails"`

	// AllowedGCPProjects admits SERVICE-ACCOUNT principals (Google issuer
	// only) whose GCP project ID — parsed from the SA email — is listed
	// (exact, case-insensitive). Empty means no service accounts are
	// admitted. This is deliberately a distinct field from AllowedProjects:
	// that field already means the Scion project ID matched against a
	// federated hub agent token's project_id claim (issuer_type: hub,
	// pkg/hub/federation_auth.go), and reusing it here would give one config
	// key two unrelated meanings depending on issuer_type — misreadable, and
	// the misreading fails open (see Validate's rule below).
	AllowedGCPProjects []string `json:"allowed_gcp_projects,omitempty" yaml:"allowed_gcp_projects,omitempty" koanf:"allowed_gcp_projects"`

	// AllowedDomains constrains USER principals (Google issuer only) to these
	// email domains (exact, case-insensitive, no wildcards, no subdomain
	// matching). Empty means no issuer-level domain constraint — the Hub
	// sign-in policy (authorized_domains, user_access_mode, admin_emails)
	// still applies on top of this, for every user regardless of whether
	// this field is set. Never consulted for a service-account principal:
	// those are admitted by AllowedGCPProjects instead.
	AllowedDomains []string `json:"allowed_domains,omitempty" yaml:"allowed_domains,omitempty" koanf:"allowed_domains"`
}

// FederationCacheConfig holds cache tuning parameters for federation JWKS fetching.
type FederationCacheConfig struct {
	RefreshInterval  time.Duration `json:"refresh_interval,omitempty" yaml:"refresh_interval,omitempty" koanf:"refresh_interval"`
	DebounceInterval time.Duration `json:"debounce_interval,omitempty" yaml:"debounce_interval,omitempty" koanf:"debounce_interval"`
}

// allowedIssuerTypes is the set of valid issuer_type values for TrustedIssuerConfig.
var allowedIssuerTypes = map[string]bool{
	"":                true, // empty defaults to "hub"
	"hub":             true,
	"service_account": true,
	"user":            true,
}

// allowedAlgorithms is the set of algorithms permitted for federation token validation.
var allowedAlgorithms = map[string]bool{
	"RS256": true,
	"ES256": true,
}

// googleIssuerURLs are the two issuer URL forms Google uses for its OIDC
// issuer. Duplicated from pkg/hub's googleIssuerHTTPS/googleIssuerBare
// constants (not imported, to avoid a circular import between pkg/config and
// pkg/hub — the same reason IssuerType is a plain string on this struct).
var googleIssuerURLs = map[string]bool{
	"https://accounts.google.com": true,
	"accounts.google.com":         true,
}

// isGoogleIssuerURL reports whether issuerURL is one of Google's OIDC issuer
// forms. Used to gate AllowedGCPProjects (valid only for a Google issuer) and
// to improve the existing AllowedProjects error message when an operator
// most likely meant the Google-specific field instead.
func isGoogleIssuerURL(issuerURL string) bool {
	return googleIssuerURLs[strings.TrimRight(issuerURL, "/")]
}

// isActiveGoogleUserIssuer reports whether issuer is configured as the
// Google issuer, issuer_type "user", with a non-empty expected_audience —
// the exact shape the external-bearer path's googleTrust gate requires
// (pkg/hub/auth_external_bearer.go) for the path to be reachable at all.
// Any Google-issuer-scoped field that only takes effect through that gate
// (AllowedGCPProjects, AllowedDomains) does nothing on any other shape, so
// Validate rejects setting it there outright instead of silently accepting a
// config no request path will ever enforce.
func isActiveGoogleUserIssuer(issuer TrustedIssuerConfig) bool {
	return isGoogleIssuerURL(issuer.IssuerURL) && issuer.IssuerType == "user" && issuer.ExpectedAudience != ""
}

// appendGoogleUserOnlyFieldError appends a validation error for a
// Google-issuer-scoped field (named by fieldName) that is set on an issuer
// which is not isActiveGoogleUserIssuer. Shared by AllowedGCPProjects and
// AllowedDomains, which are validated identically: not applicable to any
// non-Google issuer, and unenforceable on a Google issuer that isn't an
// active user issuer.
func appendGoogleUserOnlyFieldError(errs []error, i int, issuer TrustedIssuerConfig, fieldName string) []error {
	if !isGoogleIssuerURL(issuer.IssuerURL) {
		return append(errs, fmt.Errorf("trusted_issuers[%d]: %s is only applicable to the Google issuer (%q)", i, fieldName, issuer.IssuerURL))
	}
	return append(errs, fmt.Errorf("trusted_issuers[%d]: %s requires issuer_type \"user\" and a non-empty expected_audience on the Google issuer; nothing would enforce it otherwise", i, fieldName))
}

// invalidDomainEntryReason reports why domain can never match domainOf's
// parsed email domain (pkg/hub/auth_external_bearer.go: exact,
// case-insensitive comparison, no wildcards, no subdomain matching), or ""
// if the shape is fine. This does not check whether the domain is real or
// reachable, only whether it is a shape that could ever compare equal to
// something domainOf returns — an email address, a leading-wildcard pattern,
// whitespace, or a leading/trailing dot never can, and each is a plausible
// operator mistake worth catching at config-validation time instead of a
// silent, permanent lockout.
func invalidDomainEntryReason(domain string) string {
	switch {
	case domain == "":
		return "must not be empty"
	case strings.ContainsAny(domain, "@*"):
		return `must be a bare domain, not an email address or wildcard pattern (no "@" or "*")`
	case strings.IndexFunc(domain, unicode.IsSpace) >= 0:
		return "must not contain whitespace"
	case strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, "."):
		return "must not have a leading or trailing dot"
	default:
		return ""
	}
}

// Validate checks FederationConfig for configuration errors.
// It returns a slice of all errors found (not just the first).
func (c *FederationConfig) Validate() []error {
	var errs []error

	if !c.Enabled {
		return nil
	}

	// Rule 1: When enabled, at least one trusted issuer is required.
	if len(c.TrustedIssuers) == 0 {
		errs = append(errs, fmt.Errorf("federation is enabled but no trusted_issuers are configured"))
	}

	// Rule 3: Validate algorithms (if specified).
	for _, alg := range c.Algorithms {
		if !allowedAlgorithms[alg] {
			errs = append(errs, fmt.Errorf("unsupported algorithm %q: only RS256 and ES256 are allowed", alg))
		}
	}

	// Rule 5: Check for duplicate IssuerURLs.
	seen := make(map[string]bool)
	for i, issuer := range c.TrustedIssuers {
		// Rule 2: Validate IssuerURL is a valid URL with http(s) scheme.
		if issuer.IssuerURL == "" {
			errs = append(errs, fmt.Errorf("trusted_issuers[%d]: issuer_url is required", i))
		} else {
			u, err := url.Parse(issuer.IssuerURL)
			if err != nil {
				errs = append(errs, fmt.Errorf("trusted_issuers[%d]: invalid issuer_url %q: %v", i, issuer.IssuerURL, err))
			} else if u.Scheme != "https" && u.Scheme != "http" {
				errs = append(errs, fmt.Errorf("trusted_issuers[%d]: issuer_url %q must use https or http scheme", i, issuer.IssuerURL))
			} else if u.Host == "" {
				errs = append(errs, fmt.Errorf("trusted_issuers[%d]: issuer_url %q has no host", i, issuer.IssuerURL))
			}

			if seen[issuer.IssuerURL] {
				errs = append(errs, fmt.Errorf("trusted_issuers[%d]: duplicate issuer_url %q", i, issuer.IssuerURL))
			}
			seen[issuer.IssuerURL] = true
		}

		// Rule 4: Empty ExpectedAudience is allowed (resolved later).

		// Rule 6: Validate issuer_type is a known value.
		if !allowedIssuerTypes[issuer.IssuerType] {
			errs = append(errs, fmt.Errorf("trusted_issuers[%d]: unknown issuer_type %q (must be \"hub\", \"service_account\", or \"user\")", i, issuer.IssuerType))
		}
		if issuer.IssuerType == "user" && issuer.DefaultRole == "admin" {
			errs = append(errs, fmt.Errorf("trusted_issuers[%d]: default_role \"admin\" is not allowed for federated users", i))
		}

		// Rule 7: Non-hub issuers may omit jwks_url if OIDC discovery is available.
		// The authenticator will attempt discovery at startup and fail if neither works.
		isNonHub := issuer.IssuerType != "" && issuer.IssuerType != "hub"

		// Rule 8: Warn if hub-specific fields are set on non-hub issuers.
		// AllowedProjects here is the hub-federation Scion-project allowlist
		// (matched against a federated agent token's project_id claim,
		// pkg/hub/federation_auth.go) — unrelated to, and unchanged by,
		// AllowedGCPProjects below. It stays an error on every non-hub
		// issuer, including a Google one: a Google issuer wants
		// allowed_gcp_projects instead, so the message says so when that's
		// the likely mistake.
		if isNonHub && len(issuer.AllowedProjects) > 0 {
			msg := fmt.Sprintf("trusted_issuers[%d]: allowed_projects is not applicable for issuer_type %q", i, issuer.IssuerType)
			if isGoogleIssuerURL(issuer.IssuerURL) {
				msg += " (use allowed_gcp_projects for a Google issuer's service-account project allowlist)"
			}
			errs = append(errs, fmt.Errorf("%s", msg))
		}
		if isNonHub && len(issuer.AllowedRootUsers) > 0 {
			errs = append(errs, fmt.Errorf("trusted_issuers[%d]: allowed_root_users is not applicable for issuer_type %q", i, issuer.IssuerType))
		}

		// Rule 9: AllowedGCPProjects (service-account admission by GCP
		// project, design §4.1/§4.4) only does anything on an ACTIVE Google
		// user issuer (isActiveGoogleUserIssuer) — the one shape
		// googleTrust actually reaches. Set anywhere else, it must not be
		// silently ignored: that would let an operator believe they've
		// scoped service-account admission when nothing enforces it — a
		// Google issuer with the wrong issuer_type or no expected_audience
		// is exactly as unenforced as a non-Google issuer. This is a hard
		// error rather than a startup warning: both fields are new, so no
		// existing config can break.
		if len(issuer.AllowedGCPProjects) > 0 && !isActiveGoogleUserIssuer(issuer) {
			errs = appendGoogleUserOnlyFieldError(errs, i, issuer, "allowed_gcp_projects")
		}

		// Rule 10: AllowedDomains (user-principal email-domain constraint,
		// design §4.1/§4.4) is validated identically to AllowedGCPProjects
		// above — it only does anything on an ACTIVE Google user issuer.
		if len(issuer.AllowedDomains) > 0 && !isActiveGoogleUserIssuer(issuer) {
			errs = appendGoogleUserOnlyFieldError(errs, i, issuer, "allowed_domains")
		}

		// Rule 11: each allowed_domains entry must be a shape that
		// domainOf's parsed email domain (pkg/hub/auth_external_bearer.go)
		// could ever equal. An email address, a leading-wildcard pattern (the
		// allowed_emails idiom, which does not apply here), whitespace, or a
		// leading/trailing dot can never match, so an entry in one of those
		// shapes silently locks out every user in the domain the operator
		// meant to allow. Checked regardless of isActiveGoogleUserIssuer: a
		// malformed entry is a mistake in every position, not just the
		// active one.
		for j, domain := range issuer.AllowedDomains {
			if reason := invalidDomainEntryReason(domain); reason != "" {
				errs = append(errs, fmt.Errorf("trusted_issuers[%d]: allowed_domains[%d] %q: %s", i, j, domain, reason))
			}
		}
	}

	return errs
}
