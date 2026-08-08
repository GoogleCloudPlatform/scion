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
	"time"
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
}

// FederationCacheConfig holds cache tuning parameters for federation JWKS fetching.
type FederationCacheConfig struct {
	RefreshInterval  time.Duration `json:"refresh_interval,omitempty" yaml:"refresh_interval,omitempty" koanf:"refresh_interval"`
	DebounceInterval time.Duration `json:"debounce_interval,omitempty" yaml:"debounce_interval,omitempty" koanf:"debounce_interval"`
}

// allowedAlgorithms is the set of algorithms permitted for federation token validation.
var allowedAlgorithms = map[string]bool{
	"RS256": true,
	"ES256": true,
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
			}

			if seen[issuer.IssuerURL] {
				errs = append(errs, fmt.Errorf("trusted_issuers[%d]: duplicate issuer_url %q", i, issuer.IssuerURL))
			}
			seen[issuer.IssuerURL] = true
		}

		// Rule 4: Empty ExpectedAudience is allowed (resolved later).
	}

	return errs
}
