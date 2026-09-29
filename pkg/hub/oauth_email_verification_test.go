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

// githubUserInfoRoundTrip routes GET /user to userBody and GET /user/emails
// to emailsBody (with the given status, defaulting to 200), so
// getGitHubUserInfo can be tested without an end-to-end harness.
func githubUserInfoRoundTrip(userBody, emailsBody string, emailsStatus int) roundTripFunc {
	if emailsStatus == 0 {
		emailsStatus = http.StatusOK
	}
	return func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case githubUserURL:
			return httpJSONResponse(http.StatusOK, userBody), nil
		case githubEmailURL:
			return httpJSONResponse(emailsStatus, emailsBody), nil
		default:
			return httpJSONResponse(http.StatusNotFound, `{"error":"not found"}`), nil
		}
	}
}

// TestOAuthService_GetGitHubUserInfo_ProfileEmailVerified_Selected covers
// the common case: the profile's public email is verified in the caller's
// email list and is used as-is.
func TestOAuthService_GetGitHubUserInfo_ProfileEmailVerified_Selected(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: githubUserInfoRoundTrip(
			`{"id":1,"login":"octocat","name":"Test User","email":"public@example.com","avatar_url":"https://example.com/a.png"}`,
			`[
				{"email":"public@example.com","primary":true,"verified":true},
				{"email":"other@example.com","primary":false,"verified":true}
			]`, 0),
	}}

	info, err := svc.getGitHubUserInfo(context.Background(), "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Email != "public@example.com" {
		t.Errorf("email = %q, want %q", info.Email, "public@example.com")
	}
}

// TestOAuthService_GetGitHubUserInfo_ProfileEmailUnverified_FallsBackToVerified
// is the R1 regression: the profile's public email, always present and
// always taken as-is before this fix, is listed unverified. It must never be
// used; a different verified address on the account is selected instead.
func TestOAuthService_GetGitHubUserInfo_ProfileEmailUnverified_FallsBackToVerified(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: githubUserInfoRoundTrip(
			`{"id":1,"login":"octocat","name":"Test User","email":"public@example.com","avatar_url":""}`,
			`[
				{"email":"public@example.com","primary":true,"verified":false},
				{"email":"other@example.com","primary":false,"verified":true}
			]`, 0),
	}}

	info, err := svc.getGitHubUserInfo(context.Background(), "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Email != "other@example.com" {
		t.Errorf("email = %q, want %q (the unverified profile email must not be used)", info.Email, "other@example.com")
	}
}

// TestOAuthService_GetGitHubUserInfo_ProfileEmailUnverifiedAndNoOtherVerified_Rejected
// covers the same regression when there is no other verified address to
// fall back to: the login must be rejected, not silently admitted on the
// unverified profile email.
func TestOAuthService_GetGitHubUserInfo_ProfileEmailUnverifiedAndNoOtherVerified_Rejected(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: githubUserInfoRoundTrip(
			`{"id":1,"login":"octocat","name":"Test User","email":"public@example.com","avatar_url":""}`,
			`[{"email":"public@example.com","primary":true,"verified":false}]`, 0),
	}}

	if info, err := svc.getGitHubUserInfo(context.Background(), "token"); err == nil {
		t.Fatalf("expected an error, got info=%+v", info)
	}
}

// TestOAuthService_GetGitHubUserInfo_ProfileEmailNotInList_NotUsed covers a
// stale profile email that no longer appears in the caller's email list at
// all: it must not be used, even though it is non-empty.
func TestOAuthService_GetGitHubUserInfo_ProfileEmailNotInList_NotUsed(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: githubUserInfoRoundTrip(
			`{"id":1,"login":"octocat","name":"Test User","email":"stale@example.com","avatar_url":""}`,
			`[{"email":"current@example.com","primary":true,"verified":true}]`, 0),
	}}

	info, err := svc.getGitHubUserInfo(context.Background(), "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Email != "current@example.com" {
		t.Errorf("email = %q, want %q (the stale profile email is not in the list and must not be used)", info.Email, "current@example.com")
	}
}

// TestOAuthService_GetGitHubUserInfo_EmailsEndpointEmpty_Rejected covers the
// emails endpoint returning nothing at all: rejected, even though a profile
// email is present — there is no unverified fallback.
func TestOAuthService_GetGitHubUserInfo_EmailsEndpointEmpty_Rejected(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: githubUserInfoRoundTrip(
			`{"id":1,"login":"octocat","name":"Test User","email":"public@example.com","avatar_url":""}`,
			`[]`, 0),
	}}

	if _, err := svc.getGitHubUserInfo(context.Background(), "token"); err == nil {
		t.Fatal("expected an error when the emails endpoint lists nothing, even though a profile email is present")
	}
}

// TestOAuthService_GetGitHubUserInfo_EmailsEndpointForbidden_Rejected covers
// the emails endpoint being unreachable (e.g. the user:email scope was not
// actually granted): rejected, not a silent fallback to the profile email.
func TestOAuthService_GetGitHubUserInfo_EmailsEndpointForbidden_Rejected(t *testing.T) {
	svc := &OAuthService{httpClient: &http.Client{
		Transport: githubUserInfoRoundTrip(
			`{"id":1,"login":"octocat","name":"Test User","email":"public@example.com","avatar_url":""}`,
			`{"message":"Forbidden"}`, http.StatusForbidden),
	}}

	if _, err := svc.getGitHubUserInfo(context.Background(), "token"); err == nil {
		t.Fatal("expected an error when the emails endpoint is forbidden, even though a profile email is present")
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

func TestRequireVerifiedEmail_WhitespaceOnlyEmail_Rejected(t *testing.T) {
	if email, err := requireVerifiedEmail(hubclient.OAuthProviderGoogle, "   ", true); err == nil {
		t.Fatalf("expected an error for a whitespace-only email, got email=%q", email)
	}
}

func TestRequireVerifiedEmail_TrimsSurroundingWhitespace(t *testing.T) {
	email, err := requireVerifiedEmail(hubclient.OAuthProviderGoogle, "  user@example.com  ", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if email != "user@example.com" {
		t.Errorf("email = %q, want trimmed %q", email, "user@example.com")
	}
}
