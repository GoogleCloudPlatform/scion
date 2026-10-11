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

// A denied sign-in reports why it was denied: unauthorized_domain when the
// email domain is not allowed, invite_only when the hub is invite-only and
// the email has no invite or active account (ptone/scion#3330).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckSignInDenial_Reasons(t *testing.T) {
	st := newInviteFlowStore()
	st.users["u1"] = &store.User{ID: "u1", Email: "invited@allowed.com", Status: store.UserStatusInvited}

	tests := []struct {
		name    string
		email   string
		domains []string
		admins  []string
		mode    string
		want    string
	}{
		{"open mode allows", "anyone@other.com", nil, nil, "open", ""},
		{"admin always allowed", "boss@other.com", []string{"allowed.com"}, []string{"boss@other.com"}, "invite_only", ""},
		{"domain_restricted outside domain", "user@other.com", []string{"allowed.com"}, nil, "domain_restricted", signInDeniedDomain},
		{"domain_restricted inside domain", "user@allowed.com", []string{"allowed.com"}, nil, "domain_restricted", ""},
		{"domain_restricted without domains", "user@allowed.com", nil, nil, "domain_restricted", signInDeniedDomain},
		{"invite_only without invite", "uninvited@allowed.com", nil, nil, "invite_only", signInDeniedInviteOnly},
		{"invite_only with invite", "invited@allowed.com", nil, nil, "invite_only", ""},
		{"invite_only domain checked first", "user@other.com", []string{"allowed.com"}, nil, "invite_only", signInDeniedDomain},
		{"invite_only inside domain without invite", "uninvited@allowed.com", []string{"allowed.com"}, nil, "invite_only", signInDeniedInviteOnly},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkSignInDenial(context.Background(), tc.email, tc.domains, tc.admins, tc.mode, st)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want == "",
				checkUserAuthorized(context.Background(), tc.email, tc.domains, tc.admins, tc.mode, st),
				"checkUserAuthorized must agree with checkSignInDenial")
		})
	}
}

func TestOAuthCallback_DenialReasonCode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		domains []string
		email   string
		want    string
	}{
		{"invite_only without invite", "invite_only", nil, "uninvited@other.example", "/login?error=invite_only"},
		{"domain_restricted outside domain", "domain_restricted", []string{"allowed.example"}, "user@other.example", "/login?error=unauthorized_domain"},
		{"invite_only outside domain", "invite_only", []string{"allowed.example"}, "user@other.example", "/login?error=unauthorized_domain"},
		{"invite_only inside domain without invite", "invite_only", []string{"allowed.example"}, "user@allowed.example", "/login?error=invite_only"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newLiveModeStore()
			srv := newLiveModeServer(st, tc.mode)
			srv.config.AuthorizedDomains = tc.domains

			ws := newTestWebServer(t, WebServerConfig{
				SessionSecret: "test-session-secret-for-denial-reason-1234567890",
				BaseURL:       "http://localhost:8080",
			})
			ws.oauthService = NewOAuthService(OAuthConfig{
				Web: OAuthClientConfig{
					Google: OAuthProviderConfig{ClientID: "test-client-id", ClientSecret: "test-client-secret"},
				},
			}, nil)
			ws.store = st
			ws.SetAccessSettingsProvider(srv)

			assert.Equal(t, tc.want, oauthCallbackLogin(t, ws, tc.email))
			_, err := st.GetUserByEmail(context.Background(), tc.email)
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestProvisionUser_DenialReason(t *testing.T) {
	ctx := context.Background()

	inviteSrv := newLiveModeServer(newInviteFlowStore(), "invite_only")
	_, err := inviteSrv.provisionUser(ctx, &ExternalUserInfo{Email: "uninvited@other.example"})
	require.ErrorIs(t, err, ErrAccessDenied)
	require.ErrorIs(t, err, ErrInviteRequired)

	domainSrv := newLiveModeServer(newInviteFlowStore(), "domain_restricted")
	domainSrv.config.AuthorizedDomains = []string{"allowed.example"}
	_, err = domainSrv.provisionUser(ctx, &ExternalUserInfo{Email: "user@other.example"})
	require.ErrorIs(t, err, ErrAccessDenied)
	require.NotErrorIs(t, err, ErrInviteRequired)
}

func TestWriteSignInDenied_Code(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"invite required", ErrInviteRequired, "invite_only"},
		{"domain denied", ErrAccessDenied, "unauthorized_domain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeSignInDenied(rec, tc.err)
			require.Equal(t, http.StatusForbidden, rec.Code)
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tc.want, resp.Error.Code)
		})
	}
}
