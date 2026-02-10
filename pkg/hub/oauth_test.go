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
	"strings"
	"testing"
)

func TestOAuthConfig_IsConfigured(t *testing.T) {
	tests := []struct {
		name     string
		config   OAuthConfig
		expected bool
	}{
		{
			name:     "empty config",
			config:   OAuthConfig{},
			expected: false,
		},
		{
			name: "web google configured",
			config: OAuthConfig{
				Web: OAuthClientConfig{
					Google: OAuthProviderConfig{
						ClientID:     "google-client-id",
						ClientSecret: "google-secret",
					},
				},
			},
			expected: true,
		},
		{
			name: "cli github configured",
			config: OAuthConfig{
				CLI: OAuthClientConfig{
					GitHub: OAuthProviderConfig{
						ClientID:     "github-client-id",
						ClientSecret: "github-secret",
					},
				},
			},
			expected: true,
		},
		{
			name: "both web and cli configured",
			config: OAuthConfig{
				Web: OAuthClientConfig{
					Google: OAuthProviderConfig{
						ClientID:     "web-google-client-id",
						ClientSecret: "web-google-secret",
					},
				},
				CLI: OAuthClientConfig{
					GitHub: OAuthProviderConfig{
						ClientID:     "cli-github-client-id",
						ClientSecret: "cli-github-secret",
					},
				},
			},
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.config.IsConfigured(); got != tc.expected {
				t.Errorf("IsConfigured() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestOAuthConfig_IsProviderConfigured(t *testing.T) {
	config := OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "google-client-id",
				ClientSecret: "google-secret",
			},
		},
		CLI: OAuthClientConfig{
			GitHub: OAuthProviderConfig{
				ClientID: "github-client-id",
				// Missing secret
			},
		},
	}

	tests := []struct {
		provider string
		expected bool
	}{
		{"google", true},  // configured in web
		{"github", false}, // missing secret in cli
		{"unknown", false},
	}

	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			if got := config.IsProviderConfigured(tc.provider); got != tc.expected {
				t.Errorf("IsProviderConfigured(%s) = %v, want %v", tc.provider, got, tc.expected)
			}
		})
	}
}

func TestOAuthService_GetAuthorizationURL(t *testing.T) {
	config := OAuthConfig{
		CLI: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "google-client-id",
				ClientSecret: "google-secret",
			},
			GitHub: OAuthProviderConfig{
				ClientID:     "github-client-id",
				ClientSecret: "github-secret",
			},
		},
	}

	service := NewOAuthService(config)

	t.Run("google authorization URL", func(t *testing.T) {
		url, err := service.GetAuthorizationURL("google", "http://localhost:18271/callback", "test-state")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.HasPrefix(url, "https://accounts.google.com/o/oauth2/v2/auth") {
			t.Errorf("unexpected URL prefix: %s", url)
		}
		if !strings.Contains(url, "client_id=google-client-id") {
			t.Errorf("URL missing client_id: %s", url)
		}
		if !strings.Contains(url, "state=test-state") {
			t.Errorf("URL missing state: %s", url)
		}
		if !strings.Contains(url, "redirect_uri=http") {
			t.Errorf("URL missing redirect_uri: %s", url)
		}
	})

	t.Run("github authorization URL", func(t *testing.T) {
		url, err := service.GetAuthorizationURL("github", "http://localhost:18271/callback", "test-state")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.HasPrefix(url, "https://github.com/login/oauth/authorize") {
			t.Errorf("unexpected URL prefix: %s", url)
		}
		if !strings.Contains(url, "client_id=github-client-id") {
			t.Errorf("URL missing client_id: %s", url)
		}
		if !strings.Contains(url, "state=test-state") {
			t.Errorf("URL missing state: %s", url)
		}
	})

	t.Run("unsupported provider", func(t *testing.T) {
		_, err := service.GetAuthorizationURL("unknown", "http://localhost:18271/callback", "test-state")
		if err == nil {
			t.Error("expected error for unsupported provider")
		}
	})
}

func TestOAuthService_NotConfigured(t *testing.T) {
	config := OAuthConfig{} // Empty config

	service := NewOAuthService(config)

	t.Run("google not configured", func(t *testing.T) {
		_, err := service.GetAuthorizationURL("google", "http://localhost:18271/callback", "test-state")
		if err == nil {
			t.Error("expected error when google is not configured")
		}
	})

	t.Run("github not configured", func(t *testing.T) {
		_, err := service.GetAuthorizationURL("github", "http://localhost:18271/callback", "test-state")
		if err == nil {
			t.Error("expected error when github is not configured")
		}
	})
}

func TestOAuthConfig_ClientTypeConfigs(t *testing.T) {
	tests := []struct {
		name          string
		config        OAuthConfig
		webConfigured bool
		cliConfigured bool
		webGoogleID   string
		cliGoogleID   string
	}{
		{
			name:          "empty config",
			config:        OAuthConfig{},
			webConfigured: false,
			cliConfigured: false,
			webGoogleID:   "",
			cliGoogleID:   "",
		},
		{
			name: "web-specific config only",
			config: OAuthConfig{
				Web: OAuthClientConfig{
					Google: OAuthProviderConfig{
						ClientID:     "web-google-id",
						ClientSecret: "web-secret",
					},
				},
			},
			webConfigured: true,
			cliConfigured: false,
			webGoogleID:   "web-google-id",
			cliGoogleID:   "",
		},
		{
			name: "cli-specific config only",
			config: OAuthConfig{
				CLI: OAuthClientConfig{
					Google: OAuthProviderConfig{
						ClientID:     "cli-google-id",
						ClientSecret: "cli-secret",
					},
				},
			},
			webConfigured: false,
			cliConfigured: true,
			webGoogleID:   "",
			cliGoogleID:   "cli-google-id",
		},
		{
			name: "separate web and cli configs",
			config: OAuthConfig{
				Web: OAuthClientConfig{
					Google: OAuthProviderConfig{
						ClientID:     "web-google-id",
						ClientSecret: "web-secret",
					},
				},
				CLI: OAuthClientConfig{
					Google: OAuthProviderConfig{
						ClientID:     "cli-google-id",
						ClientSecret: "cli-secret",
					},
				},
			},
			webConfigured: true,
			cliConfigured: true,
			webGoogleID:   "web-google-id",
			cliGoogleID:   "cli-google-id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			webCfg := tc.config.Web
			cliCfg := tc.config.CLI

			if webCfg.IsConfigured() != tc.webConfigured {
				t.Errorf("Web.IsConfigured() = %v, want %v", webCfg.IsConfigured(), tc.webConfigured)
			}
			if cliCfg.IsConfigured() != tc.cliConfigured {
				t.Errorf("CLI.IsConfigured() = %v, want %v", cliCfg.IsConfigured(), tc.cliConfigured)
			}
			if webCfg.Google.ClientID != tc.webGoogleID {
				t.Errorf("Web.Google.ClientID = %q, want %q", webCfg.Google.ClientID, tc.webGoogleID)
			}
			if cliCfg.Google.ClientID != tc.cliGoogleID {
				t.Errorf("CLI.Google.ClientID = %q, want %q", cliCfg.Google.ClientID, tc.cliGoogleID)
			}
		})
	}
}

func TestOAuthService_GetAuthorizationURLForClient(t *testing.T) {
	config := OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "web-google-id",
				ClientSecret: "web-secret",
			},
		},
		CLI: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "cli-google-id",
				ClientSecret: "cli-secret",
			},
		},
	}

	service := NewOAuthService(config)

	t.Run("web client uses web config", func(t *testing.T) {
		url, err := service.GetAuthorizationURLForClient(OAuthClientTypeWeb, "google", "http://example.com/callback", "test-state")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(url, "client_id=web-google-id") {
			t.Errorf("URL should contain web client ID: %s", url)
		}
	})

	t.Run("cli client uses cli config", func(t *testing.T) {
		url, err := service.GetAuthorizationURLForClient(OAuthClientTypeCLI, "google", "http://localhost:18271/callback", "test-state")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(url, "client_id=cli-google-id") {
			t.Errorf("URL should contain CLI client ID: %s", url)
		}
	})
}

func TestOAuthService_IsProviderConfiguredForClient(t *testing.T) {
	config := OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "web-google-id",
				ClientSecret: "web-secret",
			},
		},
		CLI: OAuthClientConfig{
			GitHub: OAuthProviderConfig{
				ClientID:     "cli-github-id",
				ClientSecret: "cli-secret",
			},
		},
	}

	service := NewOAuthService(config)

	tests := []struct {
		clientType OAuthClientType
		provider   string
		expected   bool
	}{
		{OAuthClientTypeWeb, "google", true},
		{OAuthClientTypeWeb, "github", false},
		{OAuthClientTypeCLI, "google", false},
		{OAuthClientTypeCLI, "github", true},
	}

	for _, tc := range tests {
		name := string(tc.clientType) + "_" + tc.provider
		t.Run(name, func(t *testing.T) {
			got := service.IsProviderConfiguredForClient(tc.clientType, tc.provider)
			if got != tc.expected {
				t.Errorf("IsProviderConfiguredForClient(%s, %s) = %v, want %v", tc.clientType, tc.provider, got, tc.expected)
			}
		})
	}
}
