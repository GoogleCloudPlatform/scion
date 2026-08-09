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
	"strings"
	"testing"
)

func TestFederationConfig_Validate(t *testing.T) {
	tests := []struct {
		name      string
		config    FederationConfig
		wantErrs  int      // expected number of errors (0 = valid)
		wantSubst []string // substrings expected in error messages
	}{
		{
			name: "disabled with no issuers is valid",
			config: FederationConfig{
				Enabled: false,
			},
			wantErrs: 0,
		},
		{
			name: "enabled with no issuers is invalid",
			config: FederationConfig{
				Enabled: true,
			},
			wantErrs:  1,
			wantSubst: []string{"no trusted_issuers"},
		},
		{
			name: "enabled with valid issuer is valid",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "https://hub.example.com"},
				},
			},
			wantErrs: 0,
		},
		{
			name: "enabled with http issuer is valid",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "http://localhost:9810"},
				},
			},
			wantErrs: 0,
		},
		{
			name: "duplicate issuer URLs are rejected",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "https://hub-a.example.com"},
					{IssuerURL: "https://hub-a.example.com"},
				},
			},
			wantErrs:  1,
			wantSubst: []string{"duplicate issuer_url"},
		},
		{
			name: "HS256 algorithm is rejected",
			config: FederationConfig{
				Enabled:    true,
				Algorithms: []string{"HS256"},
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "https://hub.example.com"},
				},
			},
			wantErrs:  1,
			wantSubst: []string{"unsupported algorithm", "HS256"},
		},
		{
			name: "none algorithm is rejected",
			config: FederationConfig{
				Enabled:    true,
				Algorithms: []string{"none"},
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "https://hub.example.com"},
				},
			},
			wantErrs:  1,
			wantSubst: []string{"unsupported algorithm", "none"},
		},
		{
			name: "RS256 algorithm is valid",
			config: FederationConfig{
				Enabled:    true,
				Algorithms: []string{"RS256"},
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "https://hub.example.com"},
				},
			},
			wantErrs: 0,
		},
		{
			name: "RS256 and ES256 algorithms are valid",
			config: FederationConfig{
				Enabled:    true,
				Algorithms: []string{"RS256", "ES256"},
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "https://hub.example.com"},
				},
			},
			wantErrs: 0,
		},
		{
			name: "empty algorithms is valid",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "https://hub.example.com"},
				},
			},
			wantErrs: 0,
		},
		{
			name: "issuer URL with no scheme is rejected",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "hub.example.com"},
				},
			},
			wantErrs:  1,
			wantSubst: []string{"must use https or http scheme"},
		},
		{
			name: "issuer URL with non-http scheme is rejected",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "ftp://hub.example.com"},
				},
			},
			wantErrs:  1,
			wantSubst: []string{"must use https or http scheme"},
		},
		{
			name: "issuer URL with empty host is rejected",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "https://"},
				},
			},
			wantErrs:  1,
			wantSubst: []string{"has no host"},
		},
		{
			name: "empty issuer URL is rejected",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: ""},
				},
			},
			wantErrs:  1,
			wantSubst: []string{"issuer_url is required"},
		},
		{
			name: "empty expected_audience is valid",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{
						IssuerURL:        "https://hub.example.com",
						ExpectedAudience: "",
					},
				},
			},
			wantErrs: 0,
		},
		{
			name: "multiple errors are collected",
			config: FederationConfig{
				Enabled:    true,
				Algorithms: []string{"HS256", "none"},
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "ftp://bad.example.com"},
					{IssuerURL: "https://good.example.com"},
					{IssuerURL: "https://good.example.com"},
				},
			},
			wantErrs: 4, // 2 bad algorithms + 1 bad scheme + 1 duplicate
		},
		// --- issuer_type validation ---
		{
			name: "issuer_type hub is valid",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "https://hub.example.com", IssuerType: "hub"},
				},
			},
			wantErrs: 0,
		},
		{
			name: "issuer_type service_account is valid with jwks_url",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{
						IssuerURL:  "https://accounts.google.com",
						IssuerType: "service_account",
						JWKSURL:    "https://www.googleapis.com/oauth2/v3/certs",
					},
				},
			},
			wantErrs: 0,
		},
		{
			name: "issuer_type user is valid with jwks_url",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{
						IssuerURL:  "https://securetoken.google.com/my-project",
						IssuerType: "user",
						JWKSURL:    "https://www.googleapis.com/service_accounts/v1/jwk/securetoken@system.gserviceaccount.com",
					},
				},
			},
			wantErrs: 0,
		},
		{
			name: "unknown issuer_type is rejected",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "https://hub.example.com", IssuerType: "unknown", JWKSURL: "https://example.com/jwks"},
				},
			},
			wantErrs:  1,
			wantSubst: []string{"unknown issuer_type"},
		},
		{
			name: "empty issuer_type defaults to hub (valid)",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{IssuerURL: "https://hub.example.com", IssuerType: ""},
				},
			},
			wantErrs: 0,
		},
		{
			name: "non-hub issuer without jwks_url is valid (OIDC discovery available)",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{
						IssuerURL:  "https://accounts.google.com",
						IssuerType: "service_account",
						// JWKSURL omitted — OIDC discovery can resolve it at runtime
					},
				},
			},
			wantErrs: 0,
		},
		{
			name: "allowed_projects on non-hub issuer produces warning error",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{
						IssuerURL:       "https://accounts.google.com",
						IssuerType:      "service_account",
						JWKSURL:         "https://www.googleapis.com/oauth2/v3/certs",
						AllowedProjects: []string{"project-1"},
					},
				},
			},
			wantErrs:  1,
			wantSubst: []string{"allowed_projects is not applicable"},
		},
		{
			name: "allowed_root_users on non-hub issuer produces warning error",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{
						IssuerURL:        "https://securetoken.google.com/proj",
						IssuerType:       "user",
						JWKSURL:          "https://www.googleapis.com/keys",
						AllowedRootUsers: []string{"user:alice"},
					},
				},
			},
			wantErrs:  1,
			wantSubst: []string{"allowed_root_users is not applicable"},
		},
		{
			name: "hub issuer with allowed_projects is still valid",
			config: FederationConfig{
				Enabled: true,
				TrustedIssuers: []TrustedIssuerConfig{
					{
						IssuerURL:       "https://hub.example.com",
						IssuerType:      "hub",
						AllowedProjects: []string{"project-1"},
					},
				},
			},
			wantErrs: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := tt.config.Validate()
			if len(errs) != tt.wantErrs {
				t.Errorf("Validate() returned %d errors, want %d: %v", len(errs), tt.wantErrs, errs)
				return
			}
			if len(tt.wantSubst) > 0 && len(errs) > 0 {
				combined := ""
				for _, e := range errs {
					combined += e.Error() + " "
				}
				for _, sub := range tt.wantSubst {
					if !strings.Contains(combined, sub) {
						t.Errorf("expected error messages to contain %q, got: %s", sub, combined)
					}
				}
			}
		})
	}
}
