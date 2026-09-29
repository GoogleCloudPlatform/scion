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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// ---------------------------------------------------------------------------
// Regression coverage: Google and GitHub web-login userinfo must require a
// provider-verified email before that email is usable for account
// association, matching OIDC's existing behavior. roundTripFunc and
// httpJSONResponse are defined in handlers_auth_test.go (same package).
// ---------------------------------------------------------------------------

func TestOAuthService_GetGoogleUserInfo_VerifiedEmail_Accepted(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return httpJSONResponse(http.StatusOK, `{
				"id":"google-user-1",
				"email":"user@example.com",
				"verified_email":true,
				"name":"Test User"
			}`), nil
		}),
	}}

	info, err := svc.getGoogleUserInfo(context.Background(), "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Email != "user@example.com" {
		t.Errorf("email = %q, want %q", info.Email, "user@example.com")
	}
}

func TestOAuthService_GetGoogleUserInfo_UnverifiedEmail_Rejected(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return httpJSONResponse(http.StatusOK, `{
				"id":"google-user-1",
				"email":"user@example.com",
				"verified_email":false,
				"name":"Test User"
			}`), nil
		}),
	}}

	info, err := svc.getGoogleUserInfo(context.Background(), "token")
	if err == nil {
		t.Fatalf("expected an error for an unverified email, got info=%+v", info)
	}
}

func TestOAuthService_GetGoogleUserInfo_MissingEmail_Rejected(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return httpJSONResponse(http.StatusOK, `{
				"id":"google-user-1",
				"email":"",
				"verified_email":true,
				"name":"Test User"
			}`), nil
		}),
	}}

	if _, err := svc.getGoogleUserInfo(context.Background(), "token"); err == nil {
		t.Fatal("expected an error when no email is present")
	}
}

func TestOAuthService_GetGitHubPrimaryEmail_PrimaryVerified_Preferred(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return httpJSONResponse(http.StatusOK, `[
				{"email":"secondary@example.com","primary":false,"verified":true},
				{"email":"primary@example.com","primary":true,"verified":true}
			]`), nil
		}),
	}}

	email, err := svc.getGitHubPrimaryEmail(context.Background(), "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if email != "primary@example.com" {
		t.Errorf("email = %q, want %q", email, "primary@example.com")
	}
}

func TestOAuthService_GetGitHubPrimaryEmail_AnyVerified_FallbackWhenNoPrimary(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return httpJSONResponse(http.StatusOK, `[
				{"email":"unverified@example.com","primary":true,"verified":false},
				{"email":"verified@example.com","primary":false,"verified":true}
			]`), nil
		}),
	}}

	email, err := svc.getGitHubPrimaryEmail(context.Background(), "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if email != "verified@example.com" {
		t.Errorf("email = %q, want %q", email, "verified@example.com")
	}
}

func TestOAuthService_GetGitHubPrimaryEmail_NoVerifiedEmail_Rejected(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return httpJSONResponse(http.StatusOK, `[
				{"email":"unverified@example.com","primary":true,"verified":false}
			]`), nil
		}),
	}}

	email, err := svc.getGitHubPrimaryEmail(context.Background(), "token")
	if err == nil {
		t.Fatalf("expected an error when no email is verified, got email=%q", email)
	}
}

func TestOAuthService_GetGitHubPrimaryEmail_EmptyList_Rejected(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return httpJSONResponse(http.StatusOK, `[]`), nil
		}),
	}}

	if _, err := svc.getGitHubPrimaryEmail(context.Background(), "token"); err == nil {
		t.Fatal("expected an error when the provider lists no email at all")
	}
}

// TestRequireVerifiedEmail_ConsistentAcrossProviders is a characterization
// test: the shared invariant behaves identically regardless of which
// provider is asking, given the same (email, verified) evidence.
func TestRequireVerifiedEmail_ConsistentAcrossProviders(t *testing.T) {
	providers := []string{
		hubclient.OAuthProviderGoogle,
		hubclient.OAuthProviderGitHub,
		hubclient.OAuthProviderOIDC,
	}
	cases := []struct {
		name     string
		email    string
		verified bool
		wantErr  bool
	}{
		{name: "verified email accepted", email: "user@example.com", verified: true, wantErr: false},
		{name: "unverified email rejected", email: "user@example.com", verified: false, wantErr: true},
		{name: "missing email rejected even if marked verified", email: "", verified: true, wantErr: true},
		{name: "missing unverified email rejected", email: "", verified: false, wantErr: true},
	}

	for _, provider := range providers {
		for _, c := range cases {
			t.Run(provider+"/"+c.name, func(t *testing.T) {
				email, err := requireVerifiedEmail(provider, c.email, c.verified)
				if c.wantErr {
					if err == nil {
						t.Fatalf("expected an error, got email=%q", email)
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if email != c.email {
					t.Fatalf("email = %q, want %q", email, c.email)
				}
			})
		}
	}
}
