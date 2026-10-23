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

// User access token tests for POST /api/v1/users (user.admin.provision).
// They run on a hub without dev authentication, on which provisioning is
// available, and reuse the invitation token fixture and route helpers of
// invite_allow_list_token_test.go.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newProvisionTokenFixture is the invitation token fixture on a hub
// without dev authentication.
func newProvisionTokenFixture(t *testing.T, prefix string) *inviteTokenFixture {
	t.Helper()
	srv, s := testServerNoDevAuth(t)
	return newInviteTokenFixtureOn(t, prefix, srv, s)
}

// provisionRoute is POST /api/v1/users for a fresh email.
func (f *inviteTokenFixture) provisionRoute() itfRouteCase {
	return itfRouteCase{"provision", http.MethodPost, http.StatusCreated, func(t *testing.T) (string, interface{}, func() bool) {
		email := itfUniqueName("ptok") + "@example.com"
		return provisionPath, map[string]interface{}{"email": email}, func() bool { return f.userExists(t, email) }
	}}
}

// ptokRequireReason sends a provisioning request for a fresh email with
// the token and requires 403 forbidden with details.reason reason.
func ptokRequireReason(t *testing.T, f *inviteTokenFixture, key, reason, who string) {
	t.Helper()
	path, body, _ := f.provisionRoute().prep(t)
	rec := doRequestWithToken(t, f.srv, key, http.MethodPost, path, body)
	require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", who, rec.Body.String())
	code, details := provisionErr(t, rec)
	assert.Equal(t, ErrCodeForbidden, code, who)
	assert.Equal(t, reason, details["reason"], who)
}

// ptokRequireStructuredDenial sends a provisioning request for a fresh
// email through send and requires the structured user/invite denial.
func ptokRequireStructuredDenial(t *testing.T, f *inviteTokenFixture, send func(path string, body interface{}) *httptest.ResponseRecorder, who string) {
	t.Helper()
	path, body, _ := f.provisionRoute().prep(t)
	rec := send(path, body)
	require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", who, rec.Body.String())
	assertStructuredDenial(t, parseErrorResponse(t, rec.Body.Bytes()), "user", "invite")
}

// ptokStoredTokenDB returns the store's database for raw row edits.
func ptokStoredTokenDB(t *testing.T, s store.Store) *sql.DB {
	t.Helper()
	db, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	return db.DB()
}

// ptokSetStoredCeiling sets the stored ceiling columns of the token row
// for key: version, and the raw ceiling_permission_ids column value.
func ptokSetStoredCeiling(t *testing.T, s store.Store, key string, version permissions.CeilingVersion, rawIDs string) {
	t.Helper()
	hash := sha256.Sum256([]byte(key))
	res, err := ptokStoredTokenDB(t, s).ExecContext(context.Background(),
		"UPDATE user_access_tokens SET ceiling_version = ?, ceiling_permission_ids = ? WHERE key_hash = ?",
		int32(version), rawIDs, hex.EncodeToString(hash[:]))
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}

// TestProvisionToken_RequiresInviteSelectorAndHubBoundary pins POST
// /api/v1/users for user access tokens. The handler decides user.invite
// at the hub user collection, so a token is admitted only with user:invite,
// on the hub boundary, and with the holder's live user.invite authority,
// checked at each request:
//   - a token without user:invite, a token on a project boundary, and a
//     hub token whose stored ceiling is empty, of an unknown version or
//     malformed (the holder holds user.invite) are refused with
//     details.reason credential_insufficient;
//   - a token whose holder lost user.invite gets the structured user/invite
//     denial;
//   - refused requests write nothing, and are refused before the body is
//     read, so a body naming a role still gets 403;
//   - an admitted token naming any role gets 422, as an admitted session
//     does.
//
// Sessions are decided on the same permission: a hub admin's session is
// admitted and a hub member's gets the structured denial.
func TestProvisionToken_RequiresInviteSelectorAndHubBoundary(t *testing.T) {
	f := newProvisionTokenFixture(t, "ptok")
	insufficient := string(authzop.DenialCredentialInsufficient)

	// The selector is hub-only and needs live user.invite authority.
	assert.True(t, f.mintFails(f.admin.ID, projectBoundary(f.project), "user:invite"), "user:invite must not mint on a project boundary")
	assert.True(t, f.mintFails(f.member.ID, hubBoundary(), "user:invite"), "a holder without user.invite must not mint user:invite")

	admitted := f.mint(t, hubBoundary(), "user:invite")
	// A hub token of the same holder with another hub selector it holds.
	unrelated := f.mint(t, hubBoundary(), "hub_allow_list:read")
	projectToken := f.mint(t, projectBoundary(f.project), "project:read")
	// A stored project-boundary row carrying the hub-only selector:
	// refused at request time for the boundary.
	legacy := uatpInsertLegacyToken(t, f.s, f.admin.ID, f.project, []string{"user:invite"})
	// Stored hub-boundary rows of the hub admin whose ceiling does not
	// allow user.invite: an explicit empty list, an unknown version naming
	// user.invite, and a column that is not a JSON list.
	emptyCeiling := uatpInsertHubToken(t, f.s, f.admin.ID, []string{"user:invite"})
	ptokSetStoredCeiling(t, f.s, emptyCeiling, permissions.CeilingVersionV1, "[]")
	unknownVersion := uatpInsertHubToken(t, f.s, f.admin.ID, []string{"user:invite"})
	ptokSetStoredCeiling(t, f.s, unknownVersion, permissions.CeilingVersion(99), `["user.invite"]`)
	malformedCeiling := uatpInsertHubToken(t, f.s, f.admin.ID, []string{"user:invite"})
	ptokSetStoredCeiling(t, f.s, malformedCeiling, permissions.CeilingVersionV1, `{"user.invite"`)
	ceilingRows := map[string]string{
		"hub row with an empty ceiling":           emptyCeiling,
		"hub row with an unknown ceiling version": unknownVersion,
		"hub row with a malformed ceiling":        malformedCeiling,
	}
	// A token minted while its holder held user.invite, used after the
	// holder lost it: refused at request time.
	former, demote := f.formerAdminToken(t, "ptok-former", "user:invite")
	path, body, _ := f.provisionRoute().prep(t)
	require.Equal(t, http.StatusCreated, doRequestWithToken(t, f.srv, former, http.MethodPost, path, body).Code,
		"the token is admitted while its holder holds user.invite")
	demote()

	rc := f.provisionRoute()
	itfExpectRefused(t, rc, f.withToken(t, former, rc.method), "hub token whose holder lost user.invite")
	itfExpectRefused(t, rc, f.withToken(t, unrelated, rc.method), "hub token without user:invite")
	itfExpectRefused(t, rc, f.withToken(t, projectToken, rc.method), "project token")
	itfExpectRefused(t, rc, f.withToken(t, legacy, rc.method), "project-boundary row with user:invite")
	for who, key := range ceilingRows {
		itfExpectRefused(t, rc, f.withToken(t, key, rc.method), who)
	}
	itfExpectRefused(t, rc, f.asUser(t, f.member, rc.method), "hub member session")
	itfExpectAdmitted(t, rc, f.withToken(t, admitted, rc.method), "hub token with user:invite")
	itfExpectAdmitted(t, rc, f.asUser(t, f.admin, rc.method), "hub admin session")

	// The refusal of each token names its cause.
	ptokRequireReason(t, f, unrelated, insufficient, "hub token without user:invite")
	ptokRequireReason(t, f, projectToken, insufficient, "project token")
	ptokRequireReason(t, f, legacy, insufficient, "project-boundary row with user:invite")
	for who, key := range ceilingRows {
		ptokRequireReason(t, f, key, insufficient, who)
	}
	ptokRequireStructuredDenial(t, f, func(p string, b interface{}) *httptest.ResponseRecorder {
		return doRequestWithToken(t, f.srv, former, http.MethodPost, p, b)
	}, "hub token whose holder lost user.invite")
	ptokRequireStructuredDenial(t, f, func(p string, b interface{}) *httptest.ResponseRecorder {
		return doRequestAsUser(t, f.srv, f.member, http.MethodPost, p, b)
	}, "hub member session")

	// Admission runs before the body is read; an admitted token gets the
	// body rules of an admitted session.
	roleBody := map[string]interface{}{"email": itfUniqueName("ptok-role") + "@example.com", "role": store.UserRoleAdmin}
	refusedBeforeBody := map[string]string{
		"hub token without user:invite":           unrelated,
		"project token":                           projectToken,
		"project-boundary row with user:invite":   legacy,
		"hub token whose holder lost user.invite": former,
	}
	for who, key := range refusedBeforeBody {
		assert.Equal(t, http.StatusForbidden, doRequestWithToken(t, f.srv, key, http.MethodPost, provisionPath, roleBody).Code, who)
	}
	for role, reason := range map[string]string{
		store.UserRoleAdmin:  provisionReasonPrivilegedRole,
		store.UserRoleMember: provisionReasonRoleNotSupported,
	} {
		roleBody["role"] = role
		rec := doRequestWithToken(t, f.srv, admitted, http.MethodPost, provisionPath, roleBody)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "role %s: %s", role, rec.Body.String())
		_, details := provisionErr(t, rec)
		assert.Equal(t, reason, details["reason"], "role %s", role)
	}
	assert.False(t, f.userExists(t, roleBody["email"].(string)), "a refused request creates no record")
}

// TestProvisionToken_DetailViewNeedsReadSelector pins the detail authority
// of POST /api/v1/users for user access tokens: user.read is checked on the
// hub-level user record, so a token whose holder holds user.read gets the
// detailed replay view and the distinguishing collision reason only when
// it also carries user:read. A token with user:invite alone gets the
// minimal view {email, status} and the undifferentiated user_exists.
func TestProvisionToken_DetailViewNeedsReadSelector(t *testing.T) {
	f := newProvisionTokenFixture(t, "ptdv")
	pending := seedUser(t, f.s, "ptdv-pending@example.com", store.UserStatusInvited, "", strPtr("n"))
	replay := map[string]interface{}{"email": pending.Email, "note": "n"}
	collide := map[string]interface{}{"email": pending.Email, "note": "other"}

	inviteOnly := f.mint(t, hubBoundary(), "user:invite")
	withRead := f.mint(t, hubBoundary(), "user:invite", "user:read")

	rec := doRequestWithToken(t, f.srv, inviteOnly, http.MethodPost, provisionPath, replay)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"email", "status"}, provisionUserKeys(t, rec), "minimal view without user:read")
	assert.NotContains(t, rec.Body.String(), pending.ID)
	rec = doRequestWithToken(t, f.srv, inviteOnly, http.MethodPost, provisionPath, collide)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	_, details := provisionErr(t, rec)
	assert.Equal(t, provisionReasonUserExists, details["reason"])
	assert.NotContains(t, details, "userId")

	rec = doRequestWithToken(t, f.srv, withRead, http.MethodPost, provisionPath, replay)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeProvisionResponse(t, rec)
	assert.Equal(t, pending.ID, resp.User.ID, "detailed view with user:read")
	rec = doRequestWithToken(t, f.srv, withRead, http.MethodPost, provisionPath, collide)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	_, details = provisionErr(t, rec)
	assert.Equal(t, provisionReasonPendingUserExists, details["reason"])
	assert.Equal(t, pending.ID, details["userId"])

	// The holder's session holds user.read: the detailed view.
	rec = doRequestAsUser(t, f.srv, f.admin, http.MethodPost, provisionPath, replay)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, pending.ID, decodeProvisionResponse(t, rec).User.ID)
	assert.Empty(t, provisionAudits(t, f.s, ""), "replays and collisions write no audit")
}

// ptokSetTokenExpiry sets a stored token's expiry directly, written as an
// RFC 3339 timestamp.
func ptokSetTokenExpiry(t *testing.T, s store.Store, tokenID string, at time.Time) {
	t.Helper()
	res, err := ptokStoredTokenDB(t, s).ExecContext(context.Background(),
		"UPDATE user_access_tokens SET expires_at = ? WHERE id = ?", at.UTC().Format(time.RFC3339Nano), tokenID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}

// TestProvisionToken_RecordOutlivesTokenAndHolderAuthority pins the
// lifetime of a record a user access token provisions. The record's
// lifetime is independent of the token and of its holder: after the token
// is revoked or expires, the holder is suspended, or the holder loses
// user.invite, the token is refused, and the record stays invited and
// admits a sign-in under invite_only like any invited record. A revoked or
// expired token gets 401 unauthorized, also when its holder is suspended;
// a suspended holder's token gets 403 user_suspended; a token
// whose holder lost user.invite gets 403 forbidden. The
// user_provision audit names the holder and the token (ID, kind and hub
// boundary), and a record the token created can be withdrawn through the
// allow list.
func TestProvisionToken_RecordOutlivesTokenAndHolderAuthority(t *testing.T) {
	revoke := func(t *testing.T, f *inviteTokenFixture, holderID, tokenID string) {
		require.NoError(t, f.srv.uatService.RevokeToken(rs4MintContext(holderID), holderID, tokenID))
	}
	suspend := func(t *testing.T, f *inviteTokenFixture, holderID string) {
		u, err := f.s.GetUser(context.Background(), holderID)
		require.NoError(t, err)
		u.Status = store.UserStatusSuspended
		require.NoError(t, f.s.UpdateUser(context.Background(), u))
	}
	cases := []struct {
		name string
		// change applies the change to the token or its holder.
		change func(t *testing.T, f *inviteTokenFixture, holderID, tokenID string)
		// refused and code are the status and error code of the token's
		// next request; rejection is the token validation error, or nil
		// when the token still validates.
		refused   int
		code      string
		rejection error
	}{
		{"token revoked", revoke, http.StatusUnauthorized, ErrCodeUnauthorized, ErrUATRevoked},
		{"token expired", func(t *testing.T, f *inviteTokenFixture, _, tokenID string) {
			ptokSetTokenExpiry(t, f.s, tokenID, time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC))
		}, http.StatusUnauthorized, ErrCodeUnauthorized, ErrUATExpired},
		{"holder suspended", func(t *testing.T, f *inviteTokenFixture, holderID, _ string) {
			suspend(t, f, holderID)
		}, http.StatusForbidden, "user_suspended", ErrUserSuspended},
		// A revoked token is refused as revoked whatever its holder's
		// status: 401, not the suspended holder's 403.
		{"token revoked and holder suspended", func(t *testing.T, f *inviteTokenFixture, holderID, tokenID string) {
			revoke(t, f, holderID, tokenID)
			suspend(t, f, holderID)
		}, http.StatusUnauthorized, ErrCodeUnauthorized, ErrUATRevoked},
		{"holder lost user.invite", nil, http.StatusForbidden, ErrCodeForbidden, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProvisionTokenFixture(t, "ptlife")
			ctx := context.Background()
			setAccessConfig(f.srv, "invite_only", nil)

			holderID := hubConfigTokenUser(t, f.s, "ptlife-holder", store.SystemRoleHubAdmin)
			key, tok, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(holderID), CreateTokenParams{
				UserID: holderID, Name: "ptlife-" + tid("tok"), Boundary: hubBoundary(), Scopes: []string{"user:invite"},
			})
			require.NoError(t, err)

			signIn := itfUniqueName("ptlife-in") + "@gmail.com"
			withdraw := itfUniqueName("ptlife-out") + "@gmail.com"
			var ids []string
			for _, email := range []string{signIn, withdraw} {
				rec := doRequestWithToken(t, f.srv, key, http.MethodPost, provisionPath, map[string]interface{}{"email": email})
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				ids = append(ids, decodeProvisionResponse(t, rec).User.ID)
			}
			for _, id := range ids {
				audits := provisionAudits(t, f.s, id)
				require.Len(t, audits, 1)
				a := audits[0]
				assert.Equal(t, holderID, a.ActorPrincipalID, "the audit names the token's holder")
				assert.Equal(t, tok.ID, a.ActorCredentialID, "the audit names the token")
				assert.Equal(t, string(CredentialKindUAT), a.ActorCredentialType)
				assert.EqualValues(t, BoundaryKindHub, a.CredentialBoundaryKind)
			}

			if tc.change != nil {
				tc.change(t, f, holderID, tok.ID)
			} else {
				f.removeHubAdminBinding(t, holderID)
			}
			rec := doRequestWithToken(t, f.srv, key, http.MethodPost, provisionPath,
				map[string]interface{}{"email": itfUniqueName("ptlife-late") + "@gmail.com"})
			require.Equal(t, tc.refused, rec.Code, "the token is refused after the change: %s", rec.Body.String())
			code, _ := provisionErr(t, rec)
			assert.Equal(t, tc.code, code)
			_, verr := f.srv.uatService.ValidateToken(ctx, key)
			if tc.rejection == nil {
				assert.NoError(t, verr, "the token still validates; the decision refuses it")
			} else {
				assert.ErrorIs(t, verr, tc.rejection)
			}

			for _, email := range []string{signIn, withdraw} {
				u, err := f.s.GetUserByEmail(ctx, email)
				require.NoError(t, err)
				assert.Equal(t, store.UserStatusInvited, u.Status, "the record stays invited")
			}
			got := newSignInPaths(t, f.srv, f.s).apiLogin(signIn)
			require.True(t, got.Admitted, "the record admits a sign-in under invite_only: %s", got.ErrText)
			assert.Equal(t, store.UserStatusActive, got.Status)

			rec = doRequestAsUser(t, f.srv, f.admin, http.MethodDelete, "/api/v1/admin/allow-list/"+withdraw, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.False(t, f.userExists(t, withdraw), "the record is withdrawn")
		})
	}
}
