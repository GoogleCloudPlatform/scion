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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
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
			name: "device google configured",
			config: OAuthConfig{
				Device: OAuthClientConfig{
					Google: OAuthProviderConfig{
						ClientID:     "device-google-client-id",
						ClientSecret: "device-google-secret",
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
		Device: OAuthClientConfig{
			GitHub: OAuthProviderConfig{
				ClientID:     "device-github-id",
				ClientSecret: "device-github-secret",
			},
		},
	}

	tests := []struct {
		provider string
		expected bool
	}{
		{"google", true}, // configured in web
		{"github", true}, // configured in device (cli missing secret)
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

	service := NewOAuthService(config, nil)

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

	service := NewOAuthService(config, nil)

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
		name             string
		config           OAuthConfig
		webConfigured    bool
		cliConfigured    bool
		deviceConfigured bool
		webGoogleID      string
		cliGoogleID      string
		deviceGoogleID   string
	}{
		{
			name:             "empty config",
			config:           OAuthConfig{},
			webConfigured:    false,
			cliConfigured:    false,
			deviceConfigured: false,
			webGoogleID:      "",
			cliGoogleID:      "",
			deviceGoogleID:   "",
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
			webConfigured:    true,
			cliConfigured:    false,
			deviceConfigured: false,
			webGoogleID:      "web-google-id",
			cliGoogleID:      "",
			deviceGoogleID:   "",
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
			webConfigured:    false,
			cliConfigured:    true,
			deviceConfigured: false,
			webGoogleID:      "",
			cliGoogleID:      "cli-google-id",
			deviceGoogleID:   "",
		},
		{
			name: "device-specific config only",
			config: OAuthConfig{
				Device: OAuthClientConfig{
					Google: OAuthProviderConfig{
						ClientID:     "device-google-id",
						ClientSecret: "device-secret",
					},
				},
			},
			webConfigured:    false,
			cliConfigured:    false,
			deviceConfigured: true,
			webGoogleID:      "",
			cliGoogleID:      "",
			deviceGoogleID:   "device-google-id",
		},
		{
			name: "separate web, cli, and device configs",
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
				Device: OAuthClientConfig{
					Google: OAuthProviderConfig{
						ClientID:     "device-google-id",
						ClientSecret: "device-secret",
					},
				},
			},
			webConfigured:    true,
			cliConfigured:    true,
			deviceConfigured: true,
			webGoogleID:      "web-google-id",
			cliGoogleID:      "cli-google-id",
			deviceGoogleID:   "device-google-id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			webCfg := tc.config.Web
			cliCfg := tc.config.CLI
			deviceCfg := tc.config.Device

			if webCfg.IsConfigured() != tc.webConfigured {
				t.Errorf("Web.IsConfigured() = %v, want %v", webCfg.IsConfigured(), tc.webConfigured)
			}
			if cliCfg.IsConfigured() != tc.cliConfigured {
				t.Errorf("CLI.IsConfigured() = %v, want %v", cliCfg.IsConfigured(), tc.cliConfigured)
			}
			if deviceCfg.IsConfigured() != tc.deviceConfigured {
				t.Errorf("Device.IsConfigured() = %v, want %v", deviceCfg.IsConfigured(), tc.deviceConfigured)
			}
			if webCfg.Google.ClientID != tc.webGoogleID {
				t.Errorf("Web.Google.ClientID = %q, want %q", webCfg.Google.ClientID, tc.webGoogleID)
			}
			if cliCfg.Google.ClientID != tc.cliGoogleID {
				t.Errorf("CLI.Google.ClientID = %q, want %q", cliCfg.Google.ClientID, tc.cliGoogleID)
			}
			if deviceCfg.Google.ClientID != tc.deviceGoogleID {
				t.Errorf("Device.Google.ClientID = %q, want %q", deviceCfg.Google.ClientID, tc.deviceGoogleID)
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
		Device: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "device-google-id",
				ClientSecret: "device-secret",
			},
		},
	}

	service := NewOAuthService(config, nil)

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

	t.Run("device client uses device config", func(t *testing.T) {
		url, err := service.GetAuthorizationURLForClient(OAuthClientTypeDevice, "google", "http://localhost:18271/callback", "test-state")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(url, "client_id=device-google-id") {
			t.Errorf("URL should contain device client ID: %s", url)
		}
	})
}

func TestOAuthService_DefaultProviderForClient(t *testing.T) {
	t.Run("returns google when both providers configured", func(t *testing.T) {
		service := NewOAuthService(OAuthConfig{
			CLI: OAuthClientConfig{
				Google: OAuthProviderConfig{ClientID: "g-id", ClientSecret: "g-secret"},
				GitHub: OAuthProviderConfig{ClientID: "gh-id", ClientSecret: "gh-secret"},
			},
		}, nil)
		if got := service.DefaultProviderForClient(OAuthClientTypeCLI); got != "google" {
			t.Errorf("DefaultProviderForClient(CLI) = %q, want %q", got, "google")
		}
	})

	t.Run("returns github when only github configured", func(t *testing.T) {
		service := NewOAuthService(OAuthConfig{
			CLI: OAuthClientConfig{
				GitHub: OAuthProviderConfig{ClientID: "gh-id", ClientSecret: "gh-secret"},
			},
		}, nil)
		if got := service.DefaultProviderForClient(OAuthClientTypeCLI); got != "github" {
			t.Errorf("DefaultProviderForClient(CLI) = %q, want %q", got, "github")
		}
	})

	t.Run("returns google when no providers configured", func(t *testing.T) {
		service := NewOAuthService(OAuthConfig{}, nil)
		if got := service.DefaultProviderForClient(OAuthClientTypeCLI); got != "google" {
			t.Errorf("DefaultProviderForClient(CLI) = %q, want %q", got, "google")
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
		Device: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "device-google-id",
				ClientSecret: "device-secret",
			},
		},
	}

	service := NewOAuthService(config, nil)

	tests := []struct {
		clientType OAuthClientType
		provider   string
		expected   bool
	}{
		{OAuthClientTypeWeb, "google", true},
		{OAuthClientTypeWeb, "github", false},
		{OAuthClientTypeCLI, "google", false},
		{OAuthClientTypeCLI, "github", true},
		{OAuthClientTypeDevice, "google", true},
		{OAuthClientTypeDevice, "github", false},
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

// --- OIDC Login Tests ---

func TestOAuthService_IsProviderConfiguredForClient_OIDC(t *testing.T) {
	t.Run("oidc configured for web", func(t *testing.T) {
		oidcCfg := &config.OIDCLoginConfig{
			Enabled:   true,
			IssuerURL: "https://idp.example.com",
			ClientID:  "client-id",
		}
		service := NewOAuthService(OAuthConfig{}, oidcCfg)

		if !service.IsProviderConfiguredForClient(OAuthClientTypeWeb, hubclient.OAuthProviderOIDC) {
			t.Error("expected OIDC configured for web")
		}
	})

	t.Run("oidc not configured for cli", func(t *testing.T) {
		oidcCfg := &config.OIDCLoginConfig{
			Enabled:   true,
			IssuerURL: "https://idp.example.com",
			ClientID:  "client-id",
		}
		service := NewOAuthService(OAuthConfig{}, oidcCfg)

		if service.IsProviderConfiguredForClient(OAuthClientTypeCLI, hubclient.OAuthProviderOIDC) {
			t.Error("expected OIDC NOT configured for CLI")
		}
	})

	t.Run("oidc not configured for device", func(t *testing.T) {
		oidcCfg := &config.OIDCLoginConfig{
			Enabled:   true,
			IssuerURL: "https://idp.example.com",
			ClientID:  "client-id",
		}
		service := NewOAuthService(OAuthConfig{}, oidcCfg)

		if service.IsProviderConfiguredForClient(OAuthClientTypeDevice, hubclient.OAuthProviderOIDC) {
			t.Error("expected OIDC NOT configured for device")
		}
	})

	t.Run("oidc disabled", func(t *testing.T) {
		oidcCfg := &config.OIDCLoginConfig{
			Enabled:   false,
			IssuerURL: "https://idp.example.com",
			ClientID:  "client-id",
		}
		service := NewOAuthService(OAuthConfig{}, oidcCfg)

		if service.IsProviderConfiguredForClient(OAuthClientTypeWeb, hubclient.OAuthProviderOIDC) {
			t.Error("expected OIDC NOT configured when disabled")
		}
	})

	t.Run("oidc nil config", func(t *testing.T) {
		service := NewOAuthService(OAuthConfig{}, nil)

		if service.IsProviderConfiguredForClient(OAuthClientTypeWeb, hubclient.OAuthProviderOIDC) {
			t.Error("expected OIDC NOT configured when config is nil")
		}
	})

	t.Run("oidc missing client id", func(t *testing.T) {
		oidcCfg := &config.OIDCLoginConfig{
			Enabled:   true,
			IssuerURL: "https://idp.example.com",
			// ClientID is empty
		}
		service := NewOAuthService(OAuthConfig{}, oidcCfg)

		if service.IsProviderConfiguredForClient(OAuthClientTypeWeb, hubclient.OAuthProviderOIDC) {
			t.Error("expected OIDC NOT configured when client ID is empty")
		}
	})
}

func TestOAuthService_OIDCDisplayName(t *testing.T) {
	t.Run("returns configured display name", func(t *testing.T) {
		oidcCfg := &config.OIDCLoginConfig{
			DisplayName: "Corporate SSO",
		}
		service := NewOAuthService(OAuthConfig{}, oidcCfg)
		if got := service.OIDCDisplayName(); got != "Corporate SSO" {
			t.Errorf("OIDCDisplayName() = %q, want %q", got, "Corporate SSO")
		}
	})

	t.Run("returns SSO when display name empty", func(t *testing.T) {
		oidcCfg := &config.OIDCLoginConfig{}
		service := NewOAuthService(OAuthConfig{}, oidcCfg)
		if got := service.OIDCDisplayName(); got != "SSO" {
			t.Errorf("OIDCDisplayName() = %q, want %q", got, "SSO")
		}
	})

	t.Run("returns SSO when config nil", func(t *testing.T) {
		service := NewOAuthService(OAuthConfig{}, nil)
		if got := service.OIDCDisplayName(); got != "SSO" {
			t.Errorf("OIDCDisplayName() = %q, want %q", got, "SSO")
		}
	})
}

func TestOAuthService_OIDCScopes(t *testing.T) {
	t.Run("returns default scopes", func(t *testing.T) {
		oidcCfg := &config.OIDCLoginConfig{Enabled: true}
		service := NewOAuthService(OAuthConfig{}, oidcCfg)
		if got := service.oidcScopes(); got != "openid email profile" {
			t.Errorf("oidcScopes() = %q, want %q", got, "openid email profile")
		}
	})

	t.Run("returns configured scopes", func(t *testing.T) {
		oidcCfg := &config.OIDCLoginConfig{
			Enabled: true,
			Scopes:  []string{"openid", "email", "custom"},
		}
		service := NewOAuthService(OAuthConfig{}, oidcCfg)
		if got := service.oidcScopes(); got != "openid email custom" {
			t.Errorf("oidcScopes() = %q, want %q", got, "openid email custom")
		}
	})
}

func TestOIDCDiscoveryCache_Get(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(OIDCDiscoveryDoc{
			Issuer:                "https://example.com",
			AuthorizationEndpoint: "https://example.com/auth",
			TokenEndpoint:         "https://example.com/token",
			UserinfoEndpoint:      "https://example.com/userinfo",
			JWKSURI:               "https://example.com/keys",
		})
	}))
	t.Cleanup(srv.Close)

	cache := &oidcDiscoveryCache{ttl: 1 * time.Hour}
	client := &http.Client{Timeout: 5 * time.Second}

	// First call should fetch
	doc, err := cache.get(srv.URL, client)
	if err != nil {
		t.Fatalf("first get() failed: %v", err)
	}
	if doc.AuthorizationEndpoint != "https://example.com/auth" {
		t.Errorf("unexpected auth endpoint: %s", doc.AuthorizationEndpoint)
	}
	if callCount != 1 {
		t.Errorf("expected 1 fetch, got %d", callCount)
	}

	// Second call should use cache
	doc2, err := cache.get(srv.URL, client)
	if err != nil {
		t.Fatalf("second get() failed: %v", err)
	}
	if doc2.AuthorizationEndpoint != "https://example.com/auth" {
		t.Errorf("unexpected auth endpoint: %s", doc2.AuthorizationEndpoint)
	}
	if callCount != 1 {
		t.Errorf("expected still 1 fetch (cached), got %d", callCount)
	}
}

func TestOIDCDiscoveryCache_StaleOnError(t *testing.T) {
	firstCall := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if firstCall {
			firstCall = false
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(OIDCDiscoveryDoc{
				Issuer:                "https://example.com",
				AuthorizationEndpoint: "https://example.com/auth",
				TokenEndpoint:         "https://example.com/token",
				UserinfoEndpoint:      "https://example.com/userinfo",
			})
			return
		}
		// Subsequent calls fail
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cache := &oidcDiscoveryCache{ttl: 1 * time.Millisecond} // Expire quickly
	client := &http.Client{Timeout: 5 * time.Second}

	// First call populates cache
	doc, err := cache.get(srv.URL, client)
	if err != nil {
		t.Fatalf("first get() failed: %v", err)
	}

	// Wait for TTL to expire
	time.Sleep(5 * time.Millisecond)

	// Second call should return stale doc (server returns 500)
	doc2, err := cache.get(srv.URL, client)
	if err != nil {
		t.Fatalf("expected stale cache on error, got error: %v", err)
	}
	if doc2.AuthorizationEndpoint != doc.AuthorizationEndpoint {
		t.Errorf("expected stale doc, got different: %s", doc2.AuthorizationEndpoint)
	}
}

// newTestOIDCServer creates a test server that serves OIDC discovery, token, and userinfo endpoints.
func newTestOIDCServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			// Use the test server's own URL as the base for endpoints
			base := "http://" + r.Host
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer":                 base,
				"authorization_endpoint": base + "/auth",
				"token_endpoint":         base + "/token",
				"userinfo_endpoint":      base + "/userinfo",
				"jwks_uri":               base + "/keys",
			})

		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "test-access-token",
				"token_type":   "Bearer",
				"expires_in":   3600,
			})

		case "/userinfo":
			auth := r.Header.Get("Authorization")
			if auth != "Bearer test-access-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"sub":                "user-123",
				"email":              "user@example.com",
				"email_verified":     true,
				"name":               "Test User",
				"preferred_username": "testuser",
				"picture":            "https://example.com/photo.jpg",
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOAuthService_GetOIDCAuthURL(t *testing.T) {
	srv := newTestOIDCServer(t)

	oidcCfg := &config.OIDCLoginConfig{
		Enabled:   true,
		IssuerURL: srv.URL,
		ClientID:  "test-client-id",
	}
	service := NewOAuthService(OAuthConfig{}, oidcCfg)

	authURL, err := service.getOIDCAuthURL("https://hub.example.com/auth/callback/oidc", "test-state")
	if err != nil {
		t.Fatalf("getOIDCAuthURL() failed: %v", err)
	}

	if !strings.Contains(authURL, "/auth?") {
		t.Errorf("expected auth URL to contain /auth?, got: %s", authURL)
	}
	if !strings.Contains(authURL, "client_id=test-client-id") {
		t.Errorf("expected client_id in URL, got: %s", authURL)
	}
	if !strings.Contains(authURL, "response_type=code") {
		t.Errorf("expected response_type=code in URL, got: %s", authURL)
	}
	if !strings.Contains(authURL, "state=test-state") {
		t.Errorf("expected state in URL, got: %s", authURL)
	}
	if !strings.Contains(authURL, "scope=openid+email+profile") {
		t.Errorf("expected default scopes in URL, got: %s", authURL)
	}
}

func TestOAuthService_GetOIDCAuthURL_EndpointWithQueryParams(t *testing.T) {
	// Simulate an OIDC provider whose authorization_endpoint already has query params
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		base := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 base,
			"authorization_endpoint": base + "/auth?tenant=abc",
			"token_endpoint":         base + "/token",
			"userinfo_endpoint":      base + "/userinfo",
		})
	}))
	t.Cleanup(srv.Close)

	oidcCfg := &config.OIDCLoginConfig{
		Enabled:   true,
		IssuerURL: srv.URL,
		ClientID:  "test-client",
	}
	service := NewOAuthService(OAuthConfig{}, oidcCfg)

	authURL, err := service.getOIDCAuthURL("https://hub.example.com/callback", "s1")
	if err != nil {
		t.Fatalf("getOIDCAuthURL() failed: %v", err)
	}

	// The URL should properly merge params, not produce a double-? URL
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("auth URL is not parseable: %v", err)
	}
	q := parsed.Query()
	if q.Get("tenant") != "abc" {
		t.Errorf("expected existing query param 'tenant=abc' preserved, got: %s", authURL)
	}
	if q.Get("client_id") != "test-client" {
		t.Errorf("expected client_id in merged URL, got: %s", authURL)
	}
	if strings.Count(authURL, "?") != 1 {
		t.Errorf("expected exactly one '?' in URL, got: %s", authURL)
	}
}

func TestOAuthService_ExchangeOIDCCode(t *testing.T) {
	srv := newTestOIDCServer(t)

	oidcCfg := &config.OIDCLoginConfig{
		Enabled:      true,
		IssuerURL:    srv.URL,
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
	}
	service := NewOAuthService(OAuthConfig{}, oidcCfg)

	userInfo, err := service.exchangeOIDCCode(context.Background(), "test-code", "https://hub.example.com/auth/callback/oidc")
	if err != nil {
		t.Fatalf("exchangeOIDCCode() failed: %v", err)
	}

	if userInfo.ID != "user-123" {
		t.Errorf("expected ID %q, got %q", "user-123", userInfo.ID)
	}
	if userInfo.Email != "user@example.com" {
		t.Errorf("expected Email %q, got %q", "user@example.com", userInfo.Email)
	}
	if userInfo.DisplayName != "Test User" {
		t.Errorf("expected DisplayName %q, got %q", "Test User", userInfo.DisplayName)
	}
	if userInfo.AvatarURL != "https://example.com/photo.jpg" {
		t.Errorf("expected AvatarURL %q, got %q", "https://example.com/photo.jpg", userInfo.AvatarURL)
	}
	if userInfo.Provider != "oidc" {
		t.Errorf("expected Provider %q, got %q", "oidc", userInfo.Provider)
	}
}

func TestOAuthService_GetOIDCUserInfo_MissingEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sub":  "user-123",
			"name": "Test User",
			// email is missing
		})
	}))
	t.Cleanup(srv.Close)

	oidcCfg := &config.OIDCLoginConfig{Enabled: true}
	service := NewOAuthService(OAuthConfig{}, oidcCfg)

	_, err := service.getOIDCUserInfo(context.Background(), "test-token", srv.URL)
	if err == nil {
		t.Fatal("expected error for missing email")
	}
	if !strings.Contains(err.Error(), "email claim") {
		t.Errorf("expected 'email claim' in error, got: %v", err)
	}
}

func TestOAuthService_GetOIDCUserInfo_UnverifiedEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sub":            "user-789",
			"email":          "unverified@example.com",
			"email_verified": false,
			"name":           "Test User",
		})
	}))
	t.Cleanup(srv.Close)

	oidcCfg := &config.OIDCLoginConfig{Enabled: true}
	service := NewOAuthService(OAuthConfig{}, oidcCfg)

	_, err := service.getOIDCUserInfo(context.Background(), "test-token", srv.URL)
	if err == nil {
		t.Fatal("expected error for unverified email")
	}
	if !strings.Contains(err.Error(), "unverified email") {
		t.Errorf("expected 'unverified email' in error, got: %v", err)
	}
}

func TestOAuthService_GetOIDCUserInfo_FallbackToPreferredUsername(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sub":                "user-456",
			"email":              "user@example.com",
			"email_verified":     true,
			"preferred_username": "jsmith",
			// name is missing — should fall back to preferred_username
		})
	}))
	t.Cleanup(srv.Close)

	oidcCfg := &config.OIDCLoginConfig{Enabled: true}
	service := NewOAuthService(OAuthConfig{}, oidcCfg)

	userInfo, err := service.getOIDCUserInfo(context.Background(), "test-token", srv.URL)
	if err != nil {
		t.Fatalf("getOIDCUserInfo() failed: %v", err)
	}
	if userInfo.DisplayName != "jsmith" {
		t.Errorf("expected DisplayName %q, got %q", "jsmith", userInfo.DisplayName)
	}
}

func TestOAuthService_ConfiguredProvidersForClient_OIDC(t *testing.T) {
	oidcCfg := &config.OIDCLoginConfig{
		Enabled:   true,
		IssuerURL: "https://idp.example.com",
		ClientID:  "client-id",
	}
	oauthCfg := OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{ClientID: "g-id", ClientSecret: "g-secret"},
		},
	}
	service := NewOAuthService(oauthCfg, oidcCfg)

	webProviders := service.ConfiguredProvidersForClient(OAuthClientTypeWeb)
	hasOIDC := false
	for _, p := range webProviders {
		if p == hubclient.OAuthProviderOIDC {
			hasOIDC = true
		}
	}
	if !hasOIDC {
		t.Errorf("expected OIDC in web providers, got: %v", webProviders)
	}

	cliProviders := service.ConfiguredProvidersForClient(OAuthClientTypeCLI)
	for _, p := range cliProviders {
		if p == hubclient.OAuthProviderOIDC {
			t.Errorf("did not expect OIDC in CLI providers, got: %v", cliProviders)
		}
	}
}

// --- Tests for G-1: sub claim validation and email fallback for display name ---

func TestOAuthService_GetOIDCUserInfo_MissingSub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"email":          "user@example.com",
			"email_verified": true,
			"name":           "Test User",
			// sub is missing
		})
	}))
	t.Cleanup(srv.Close)

	oidcCfg := &config.OIDCLoginConfig{Enabled: true}
	service := NewOAuthService(OAuthConfig{}, oidcCfg)

	_, err := service.getOIDCUserInfo(context.Background(), "test-token", srv.URL)
	if err == nil {
		t.Fatal("expected error for missing sub claim")
	}
	if !strings.Contains(err.Error(), "sub") {
		t.Errorf("expected 'sub' in error, got: %v", err)
	}
}

func TestOAuthService_GetOIDCUserInfo_FallbackToEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sub":            "user-789",
			"email":          "fallback@example.com",
			"email_verified": true,
			// name and preferred_username are missing — should fall back to email
		})
	}))
	t.Cleanup(srv.Close)

	oidcCfg := &config.OIDCLoginConfig{Enabled: true}
	service := NewOAuthService(OAuthConfig{}, oidcCfg)

	userInfo, err := service.getOIDCUserInfo(context.Background(), "test-token", srv.URL)
	if err != nil {
		t.Fatalf("getOIDCUserInfo() failed: %v", err)
	}
	if userInfo.DisplayName != "fallback@example.com" {
		t.Errorf("expected DisplayName %q, got %q", "fallback@example.com", userInfo.DisplayName)
	}
}

// --- Tests for G-3: validateOIDCLoginConfig ---

func TestValidateOIDCLoginConfig(t *testing.T) {
	tests := []struct {
		name      string
		cfg       config.OIDCLoginConfig
		wantError string // empty means no error expected
	}{
		{
			name: "valid config",
			cfg: config.OIDCLoginConfig{
				Enabled:      true,
				IssuerURL:    "https://idp.example.com",
				ClientID:     "client-id",
				ClientSecret: "client-secret",
			},
			wantError: "",
		},
		{
			name: "valid localhost config",
			cfg: config.OIDCLoginConfig{
				Enabled:      true,
				IssuerURL:    "http://localhost:8080",
				ClientID:     "client-id",
				ClientSecret: "client-secret",
			},
			wantError: "",
		},
		{
			name: "valid 127.0.0.1 config",
			cfg: config.OIDCLoginConfig{
				Enabled:      true,
				IssuerURL:    "http://127.0.0.1:9090",
				ClientID:     "client-id",
				ClientSecret: "client-secret",
			},
			wantError: "",
		},
		{
			name: "missing issuer URL",
			cfg: config.OIDCLoginConfig{
				Enabled:      true,
				ClientID:     "client-id",
				ClientSecret: "client-secret",
			},
			wantError: "issuerUrl is required",
		},
		{
			name: "http scheme rejected",
			cfg: config.OIDCLoginConfig{
				Enabled:      true,
				IssuerURL:    "http://idp.example.com",
				ClientID:     "client-id",
				ClientSecret: "client-secret",
			},
			wantError: "must use https",
		},
		{
			name: "issuer URL with query params",
			cfg: config.OIDCLoginConfig{
				Enabled:      true,
				IssuerURL:    "https://idp.example.com?foo=bar",
				ClientID:     "client-id",
				ClientSecret: "client-secret",
			},
			wantError: "query parameters",
		},
		{
			name: "issuer URL with fragment",
			cfg: config.OIDCLoginConfig{
				Enabled:      true,
				IssuerURL:    "https://idp.example.com#section",
				ClientID:     "client-id",
				ClientSecret: "client-secret",
			},
			wantError: "fragment",
		},
		{
			name: "missing client ID",
			cfg: config.OIDCLoginConfig{
				Enabled:      true,
				IssuerURL:    "https://idp.example.com",
				ClientSecret: "client-secret",
			},
			wantError: "clientId is required",
		},
		{
			name: "public client (no client secret)",
			cfg: config.OIDCLoginConfig{
				Enabled:   true,
				IssuerURL: "https://idp.example.com",
				ClientID:  "client-id",
			},
			wantError: "", // public OIDC clients legitimately have no secret
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOIDCLoginConfig(&tc.cfg)
			if tc.wantError == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantError)
				}
				if !strings.Contains(err.Error(), tc.wantError) {
					t.Errorf("expected error containing %q, got: %v", tc.wantError, err)
				}
			}
		})
	}
}
