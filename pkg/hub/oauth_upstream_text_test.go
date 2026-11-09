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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oauthServiceAnswering returns an OAuthService whose every upstream call is
// answered with status and body.
func oauthServiceAnswering(status int, body string) *OAuthService {
	return &OAuthService{httpClient: &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return httpJSONResponse(status, body), nil
		}),
	}}
}

// Every OAuth upstream failure carries the upstream body in its error only
// as upstreamText renders it: capped, with control and line-break runes
// replaced. The handlers log that error and answer the client fixed text.
func TestOAuthUpstreamErrorBodyRendered(t *testing.T) {
	ctx := context.Background()
	cfg := OAuthProviderConfig{ClientID: "client-id", ClientSecret: "client-secret"}
	tests := []struct {
		name   string
		status int
		call   func(s *OAuthService) error
	}{
		{name: "token exchange", status: http.StatusBadRequest, call: func(s *OAuthService) error {
			_, err := s.exchangeCodeForToken(ctx, "https://idp.example/token", "client-id", "", "code", "https://hub.example/cb")
			return err
		}},
		{name: "GitHub token exchange", status: http.StatusBadRequest, call: func(s *OAuthService) error {
			_, err := s.exchangeGitHubCodeForTokenWithConfig(ctx, cfg, "code", "https://hub.example/cb")
			return err
		}},
		{name: "Google userinfo", status: http.StatusInternalServerError, call: func(s *OAuthService) error {
			_, err := s.getGoogleUserInfo(ctx, "token")
			return err
		}},
		{name: "GitHub userinfo", status: http.StatusInternalServerError, call: func(s *OAuthService) error {
			_, err := s.getGitHubUserInfo(ctx, "token")
			return err
		}},
		{name: "GitHub emails", status: http.StatusInternalServerError, call: func(s *OAuthService) error {
			_, err := s.getGitHubEmails(ctx, "token")
			return err
		}},
		{name: "Google device code", status: http.StatusInternalServerError, call: func(s *OAuthService) error {
			_, err := s.requestGoogleDeviceCode(ctx, cfg)
			return err
		}},
		{name: "GitHub device code", status: http.StatusInternalServerError, call: func(s *OAuthService) error {
			_, err := s.requestGitHubDeviceCode(ctx, cfg)
			return err
		}},
		{name: "Google device token poll", status: http.StatusInternalServerError, call: func(s *OAuthService) error {
			_, err := s.pollGoogleDeviceToken(ctx, cfg, "device-code")
			return err
		}},
		{name: "GitHub device token poll", status: http.StatusInternalServerError, call: func(s *OAuthService) error {
			_, err := s.pollGitHubDeviceToken(ctx, cfg, "device-code")
			return err
		}},
		{name: "OIDC userinfo", status: http.StatusInternalServerError, call: func(s *OAuthService) error {
			_, err := s.getOIDCUserInfo(ctx, "token", "https://idp.example/userinfo")
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(oauthServiceAnswering(tc.status, upstreamMarkerBody))
			if err == nil {
				t.Fatal("expected an error")
			}
			assertUpstreamTextRendered(t, err.Error())
		})
	}
}

// The body-level error field of a device token poll answer is rendered too.
func TestOAuthDeviceTokenErrorFieldRendered(t *testing.T) {
	ctx := context.Background()
	cfg := OAuthProviderConfig{ClientID: "client-id"}
	body, err := json.Marshal(map[string]string{"error": upstreamMarkerBody})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		status int
		call   func(s *OAuthService) error
	}{
		{name: "Google", status: http.StatusBadRequest, call: func(s *OAuthService) error {
			_, err := s.pollGoogleDeviceToken(ctx, cfg, "device-code")
			return err
		}},
		{name: "GitHub", status: http.StatusOK, call: func(s *OAuthService) error {
			_, err := s.pollGitHubDeviceToken(ctx, cfg, "device-code")
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(oauthServiceAnswering(tc.status, string(body)))
			if err == nil {
				t.Fatal("expected an error")
			}
			assertUpstreamTextRendered(t, err.Error())
		})
	}
}

// GitHub answers a failed code exchange with 200 and the error in the body.
// The error names GitHub's error fields, rendered; before, the body had
// already been consumed by the decoder and the error carried nothing.
func TestOAuthGitHubNoAccessTokenNamesError(t *testing.T) {
	body, err := json.Marshal(map[string]string{
		"error":             "bad_verification_code",
		"error_description": upstreamMarkerBody,
		"error_uri":         "https://docs.github.example/oauth",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := oauthServiceAnswering(http.StatusOK, string(body))
	_, err = s.exchangeGitHubCodeForTokenWithConfig(context.Background(), OAuthProviderConfig{ClientID: "client-id"}, "code", "https://hub.example/cb")
	if err == nil {
		t.Fatal("expected an error")
	}
	assertUpstreamTextRendered(t, err.Error())
	if want := "bad_verification_code"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name GitHub's error %q", err.Error(), want)
	}
}

// A successful GitHub code exchange still returns the token: reading the
// body once before decoding does not change the success path.
func TestOAuthGitHubExchangeSuccess(t *testing.T) {
	s := oauthServiceAnswering(http.StatusOK, `{"access_token":"gho_test","token_type":"bearer","scope":"read:user"}`)
	tok, err := s.exchangeGitHubCodeForTokenWithConfig(context.Background(), OAuthProviderConfig{ClientID: "client-id"}, "code", "https://hub.example/cb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "gho_test" {
		t.Errorf("access token = %q, want %q", tok.AccessToken, "gho_test")
	}
}

// The device flow handler answers a failed upstream device code request with
// fixed text and the same status and code as before; the upstream body
// reaches only the server log, rendered, in one record.
//
// Must not run in parallel: it swaps the default logger.
func TestCLIDeviceAuthorize_UpstreamBodyLoggedNotReturned(t *testing.T) {
	srv, _ := testServer(t)
	srv.oauthService = NewOAuthService(OAuthConfig{
		Device: OAuthClientConfig{GitHub: OAuthProviderConfig{ClientID: "device-gh-id", ClientSecret: "device-gh-secret"}},
	}, nil)
	srv.oauthService.httpClient = oauthServiceAnswering(http.StatusInternalServerError, upstreamMarkerBody).httpClient
	logs := captureUpstreamLog(t)

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/cli/device", CLIDeviceAuthorizeRequest{Provider: "github"})

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"oauth_error"`)
	assert.Contains(t, rec.Body.String(), "failed to request device code")
	assert.NotContains(t, rec.Body.String(), upstreamMarker)
	assertLoggedOnceRendered(t, logs, "Device code request failed")
	assert.Equal(t, 1, strings.Count(logs.String(), upstreamMarker), "the marker reaches exactly one log record")
}
