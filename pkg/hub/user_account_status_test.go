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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nonActiveUserStatuses is every defined user status other than active
// that a local users row can hold. The account-status rule refuses each of
// them. store.UserStatusPending is held only by federated account rows (the
// store refuses it on any other row); TestCheckUserAccount_RequiresActiveStatus
// covers it, and every check below goes through checkUserAccount.
var nonActiveUserStatuses = []string{
	store.UserStatusSuspended,
	store.UserStatusInvited,
}

// statusReader is a userRowReader that returns one row, store.ErrNotFound
// (row nil, err nil) or a fixed error.
type statusReader struct {
	row *store.User
	err error
}

func (r statusReader) GetUser(_ context.Context, id string) (*store.User, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.row == nil || r.row.ID != id {
		return nil, store.ErrNotFound
	}
	return r.row, nil
}

// nilRowReader returns neither a row nor an error.
type nilRowReader struct{}

func (nilRowReader) GetUser(context.Context, string) (*store.User, error) { return nil, nil }

// TestCheckUserAccount_RequiresActiveStatus pins the shared account-status
// rule: only an existing active row passes for a local or development
// user; every other status (defined or added later), a missing row and a
// lookup fault are refused, each with its own classification.
func TestCheckUserAccount_RequiresActiveStatus(t *testing.T) {
	ctx := context.Background()
	row := func(status string) statusReader {
		return statusReader{row: &store.User{ID: DevUserID, Status: status}}
	}
	for _, kind := range []PrincipalKind{PrincipalKindDev, PrincipalKindUser} {
		p := PrincipalContext{Kind: kind, ID: DevUserID}
		t.Run(string(kind), func(t *testing.T) {
			u, err := checkUserAccount(ctx, row(store.UserStatusActive), p)
			require.NoError(t, err)
			assert.Equal(t, accountAllowed, classifyUserAccount(u, err))

			for _, status := range append(append([]string{}, nonActiveUserStatuses...), store.UserStatusPending, "status-added-later") {
				u, err := checkUserAccount(ctx, row(status), p)
				require.ErrorIs(t, err, ErrProjectAccessDenied, status)
				want := accountNotActive
				if status == store.UserStatusSuspended {
					want = accountSuspended
				}
				assert.Equal(t, want, classifyUserAccount(u, err), status)
			}

			u, err = checkUserAccount(ctx, statusReader{}, p)
			assert.Equal(t, accountMissing, classifyUserAccount(u, err), "missing row")

			u, err = checkUserAccount(ctx, statusReader{err: errors.New("db down")}, p)
			assert.Equal(t, accountLookupFault, classifyUserAccount(u, err), "lookup fault")

			u, err = checkUserAccount(ctx, nil, p)
			assert.Equal(t, accountLookupFault, classifyUserAccount(u, err), "no reader")

			u, err = checkUserAccount(ctx, nilRowReader{}, p)
			assert.Equal(t, accountLookupFault, classifyUserAccount(u, err), "neither a row nor an error")
		})
	}

	// A principal that is not a user is not checked.
	u, err := checkUserAccount(ctx, statusReader{err: errors.New("unused")}, PrincipalContext{Kind: PrincipalKindAgent, ID: "a"})
	require.NoError(t, err)
	assert.Nil(t, u)
}

// requireAccountRefusal asserts UnifiedAuthMiddleware's answer for an
// account in status (writeAccountRefusal).
func requireAccountRefusal(t *testing.T, rec *httptest.ResponseRecorder, status string) {
	t.Helper()
	wantCode, wantErr := http.StatusUnauthorized, `"`+ErrCodeUnauthorized+`"`
	if status == store.UserStatusSuspended {
		wantCode, wantErr = http.StatusForbidden, `"user_suspended"`
	}
	if rec.Code != wantCode || !strings.Contains(rec.Body.String(), wantErr) {
		t.Fatalf("account-status refusal: status %s got HTTP %d, want %d %s: %s", status, rec.Code, wantCode, wantErr, rec.Body.String())
	}
}

// TestDevToken_AccountStatusApplies_AdminRoute: a development token is
// refused on an administrator-role route while the development user's
// account is not active, and served again once it is active.
func TestDevToken_AccountStatusApplies_AdminRoute(t *testing.T) {
	srv, s := testServer(t)
	path := "/api/v1/users/" + DevUserID + "/revoke-sessions"

	rec := doRequest(t, srv, http.MethodPost, path, map[string]interface{}{})
	require.Less(t, rec.Code, 300, "active development user is served: %s", rec.Body.String())

	for _, status := range nonActiveUserStatuses {
		t.Run(status, func(t *testing.T) {
			setUserStatus(t, s, DevUserID, status)
			defer setUserStatus(t, s, DevUserID, store.UserStatusActive)
			requireAccountRefusal(t, doRequest(t, srv, http.MethodPost, path, map[string]interface{}{}), status)
		})
	}

	rec = doRequest(t, srv, http.MethodPost, path, map[string]interface{}{})
	require.Less(t, rec.Code, 300, "reactivated development user is served: %s", rec.Body.String())
}

// TestDevToken_AccountStatusApplies_SelfScopedRoute: a development token is
// refused on a route that serves the caller's own records while the
// development user's account is not active.
func TestDevToken_AccountStatusApplies_SelfScopedRoute(t *testing.T) {
	srv, s := testServer(t)
	path := "/api/v1/notifications"

	rec := doRequest(t, srv, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, status := range nonActiveUserStatuses {
		t.Run(status, func(t *testing.T) {
			setUserStatus(t, s, DevUserID, status)
			defer setUserStatus(t, s, DevUserID, store.UserStatusActive)
			requireAccountRefusal(t, doRequest(t, srv, http.MethodGet, path, nil), status)
		})
	}

	rec = doRequest(t, srv, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// oneUserStore is a store.UserStore holding at most one users row.
type oneUserStore struct {
	store.UserStore
	row *store.User
}

func (s oneUserStore) GetUser(_ context.Context, id string) (*store.User, error) {
	if s.row == nil || s.row.ID != id {
		return nil, store.ErrNotFound
	}
	return s.row, nil
}

// TestDevToken_AccountStatusApplies_BothTokenArms: UnifiedAuthMiddleware
// applies the account-status rule to the development user in both arms
// that accept a development token: the prefixed token and a JWT-shaped
// development token when no user token service is configured. An active
// row is served; a suspended row and a missing row are refused with the
// hub user token arm's answers.
func TestDevToken_AccountStatusApplies_BothTokenArms(t *testing.T) {
	const jwtShaped = "dev-header.dev-claims.dev-signature"
	for _, arm := range []struct{ name, token string }{
		{"prefixed", testDevToken},
		{"jwt_shaped", jwtShaped},
	} {
		for _, tc := range []struct {
			state    string
			row      *store.User
			wantCode int
			wantErr  string
		}{
			{"active", &store.User{ID: DevUserID, Status: store.UserStatusActive}, http.StatusOK, ""},
			{"suspended", &store.User{ID: DevUserID, Status: store.UserStatusSuspended}, http.StatusForbidden, `"user_suspended"`},
			{"missing", nil, http.StatusUnauthorized, `"` + ErrCodeUserNotFound + `"`},
		} {
			t.Run(arm.name+"_"+tc.state, func(t *testing.T) {
				mw := UnifiedAuthMiddleware(AuthConfig{
					Mode:           "development",
					DevAuthEnabled: true,
					DevAuthToken:   arm.token,
					UserStore:      oneUserStore{row: tc.row},
				})
				var served Identity
				h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					served = GetIdentityFromContext(r.Context())
				}))
				req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
				req.Header.Set("Authorization", "Bearer "+arm.token)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if tc.wantErr == "" {
					if served == nil || rec.Code != tc.wantCode {
						t.Fatalf("middleware account-status: %s %s: served=%v HTTP %d, want served", arm.name, tc.state, served != nil, rec.Code)
					}
					assert.IsType(t, &DevUser{}, served)
					return
				}
				if served != nil || rec.Code != tc.wantCode || !strings.Contains(rec.Body.String(), tc.wantErr) {
					t.Fatalf("middleware account-status: %s %s: served=%v HTTP %d, want refused with %d %s: %s",
						arm.name, tc.state, served != nil, rec.Code, tc.wantCode, tc.wantErr, rec.Body.String())
				}
			})
		}
	}
}

// TestDevToken_AccountRefusalMatchesUserToken: for the same account state,
// the development token and a hub user token get the same status code and
// body.
func TestDevToken_AccountRefusalMatchesUserToken(t *testing.T) {
	srv, s := testServer(t)
	dev, err := s.GetUser(context.Background(), DevUserID)
	require.NoError(t, err)
	session, _, _, err := srv.userTokenService.GenerateTokenPair(dev.ID, dev.Email, dev.DisplayName, dev.Role, ClientTypeWeb)
	require.NoError(t, err)

	for _, status := range nonActiveUserStatuses {
		t.Run(status, func(t *testing.T) {
			setUserStatus(t, s, DevUserID, status)
			defer setUserStatus(t, s, DevUserID, store.UserStatusActive)
			devRec := doRequest(t, srv, http.MethodGet, "/api/v1/notifications", nil)
			userRec := doBearerRequest(t, srv, http.MethodGet, "/api/v1/notifications", session)
			requireAccountRefusal(t, devRec, status)
			if userRec.Code != devRec.Code || userRec.Body.String() != devRec.Body.String() {
				t.Fatalf("refusal mismatch: status %s: user token HTTP %d %s, development token HTTP %d %s",
					status, userRec.Code, userRec.Body.String(), devRec.Code, devRec.Body.String())
			}
		})
	}
}

// doBearerRequest serves one request with the given bearer token.
func doBearerRequest(t *testing.T, srv *Server, method, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestConduitRenewal_DevUserAccountStatusApplies: a live-connection renewal
// for the development user is refused as an inactive principal while its
// account is not active.
func TestConduitRenewal_DevUserAccountStatusApplies(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ident, err := srv.conduitRenewalIdentity(ctx, &grant.RecheckClaims{}, DevUserID)
	require.NoError(t, err)
	assert.IsType(t, &DevUser{}, ident)

	for _, status := range nonActiveUserStatuses {
		t.Run(status, func(t *testing.T) {
			setUserStatus(t, s, DevUserID, status)
			defer setUserStatus(t, s, DevUserID, store.UserStatusActive)
			_, err := srv.conduitRenewalIdentity(ctx, &grant.RecheckClaims{}, DevUserID)
			if !errors.Is(err, errConduitPrincipalInactive) {
				t.Fatalf("renewal account-status: status %s: err %v, want an inactive principal", status, err)
			}
		})
	}
}

// TestRelayAdmission_DevUserAccountStatusApplies: a relay-held development
// session is denied on its re-check while the development user's account
// is not active.
func TestRelayAdmission_DevUserAccountStatusApplies(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	claims := conduitAdmissionClaims{ID: DevUserID, AuthType: AuthTypeDevToken}

	ident, verdict, reason := srv.relayUserIdentity(ctx, claims)
	require.Equal(t, conduitAuthzAllowed, verdict, reason)
	assert.IsType(t, &DevUser{}, ident)

	for _, status := range nonActiveUserStatuses {
		t.Run(status, func(t *testing.T) {
			setUserStatus(t, s, DevUserID, status)
			defer setUserStatus(t, s, DevUserID, store.UserStatusActive)
			ident, verdict, reason := srv.relayUserIdentity(ctx, claims)
			if verdict != conduitAuthzDenied || ident != nil {
				t.Fatalf("relay account-status: status %s: verdict %v (%q), identity %T, want denied", status, verdict, reason, ident)
			}
		})
	}
}

// TestConduitUserStatus_DevUserAccountStatusApplies: the per-stream and
// tunnel user-status rule denies the development user while its account is
// not active.
func TestConduitUserStatus_DevUserAccountStatusApplies(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	dev := NewDevUser(srv.authConfig.DevUserCfg)

	verdict, reason := srv.conduitUserStatus(ctx, dev, dev.ID())
	require.Equal(t, conduitAuthzAllowed, verdict, reason)

	for _, status := range nonActiveUserStatuses {
		t.Run(status, func(t *testing.T) {
			setUserStatus(t, s, DevUserID, status)
			defer setUserStatus(t, s, DevUserID, store.UserStatusActive)
			verdict, reason := srv.conduitUserStatus(ctx, dev, dev.ID())
			if verdict != conduitAuthzDenied {
				t.Fatalf("conduit user status: status %s: verdict %v (%q), want denied", status, verdict, reason)
			}
		})
	}
}

// TestUserToken_RequiresActiveAccount: a hub user token is accepted only
// while its user's account is active; every defined non-active status is
// refused.
func TestUserToken_RequiresActiveAccount(t *testing.T) {
	srv, s := testServer(t)
	userID := tid("uta-user")
	createTestUserWithRole(t, s, userID, userID+"@test.com", "member", store.SystemRoleSuperAdmin)
	ensureHubMembership(context.Background(), s, userID)
	u, err := s.GetUser(context.Background(), userID)
	require.NoError(t, err)
	session, _, _, err := srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
	require.NoError(t, err)
	path := "/api/v1/notifications"

	rec := doBearerRequest(t, srv, http.MethodGet, path, session)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, status := range nonActiveUserStatuses {
		t.Run(status, func(t *testing.T) {
			setUserStatus(t, s, userID, status)
			defer setUserStatus(t, s, userID, store.UserStatusActive)
			requireAccountRefusal(t, doBearerRequest(t, srv, http.MethodGet, path, session), status)
		})
	}
}

// TestUserAccessToken_RequiresActiveAccount: a user access token validates,
// at authentication and on the live re-check, only while its user's
// account is active; every defined non-active status is refused.
func TestUserAccessToken_RequiresActiveAccount(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	userID := tid("uat-active-user")
	createTestUserWithRole(t, s, userID, userID+"@test.com", "member", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, userID)
	key, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "uat-active-" + tid("tok"), Boundary: hubBoundary(), Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)
	user := NewAuthenticatedUser(userID, userID+"@test.com", "UAT", "member", "web")
	recheck := contextWithCredentialContext(contextWithIdentity(ctx, user), CredentialContext{Kind: CredentialKindUAT, ID: token.ID})

	_, err = srv.uatService.ValidateToken(ctx, key)
	require.NoError(t, err)
	require.True(t, srv.bearerStillValid(recheck))

	for _, status := range nonActiveUserStatuses {
		t.Run(status, func(t *testing.T) {
			setUserStatus(t, s, userID, status)
			defer setUserStatus(t, s, userID, store.UserStatusActive)

			_, err := srv.uatService.ValidateToken(ctx, key)
			require.Error(t, err)
			var rej *UATRejection
			require.True(t, errors.As(err, &rej), "a refusal of a matched token: %v", err)
			assert.True(t, rej.Found)
			if status == store.UserStatusSuspended {
				assert.ErrorIs(t, err, ErrUserSuspended)
				assert.Equal(t, "user_suspended", rej.Reason)
			} else {
				assert.ErrorIs(t, err, ErrInvalidUAT)
				assert.Equal(t, "user_not_active", rej.Reason)
			}
			assert.False(t, srv.bearerStillValid(recheck), "live re-check")
			requireAccountRefusal(t, doBearerRequest(t, srv, http.MethodGet, "/api/v1/notifications", key), status)
		})
	}
}
