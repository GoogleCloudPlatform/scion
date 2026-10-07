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

// Integration tests for POST /api/v1/users (user.admin.provision), against
// the real SQLite-backed hub. Design: .design/admin-user-provisioning.md.
// The design's §16.4 row-to-test index maps each §8 outcome row to a
// subtest here; the row number is in each subtest name ("row07_...").

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const provisionPath = "/api/v1/users"

// provisionFixture is a hub with one caller of each authority class.
type provisionFixture struct {
	srv *Server
	s   store.Store
	// superAdmin holds every permission.
	superAdmin *store.User
	// hubAdmin holds user.invite and user.read (detail authority).
	hubAdmin *store.User
	// inviter holds user.invite only, through a custom system role, and is
	// in no group: no detail authority.
	inviter *store.User
	// member is a hub member: user.read through the seeded hub-member
	// grants, no user.invite.
	member *store.User
	// viewer holds the hub-viewer grants: user.read, no user.invite.
	viewer *store.User
}

func newProvisionFixture(t *testing.T) *provisionFixture {
	t.Helper()
	srv, s := testServerNoDevAuth(t)
	return newProvisionFixtureOn(t, srv, s)
}

// testServerNoDevAuth is testServer with dev auth disabled. Provisioning is
// refused on a hub in dev-auth mode (row 4a), so the provisioning tests run
// on a hub that only has sign-in sessions; callers authenticate with
// session tokens (doRequestAsUser, provisionAs).
func testServerNoDevAuth(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	require.NoError(t, s.Migrate(context.Background()))
	_ = s.DeleteHubSetting(context.Background(), "migration_delegation_edge_backfill_v1")
	cfg := testServerConfig()
	cfg.DevAuthToken = "" // dev-auth off
	srv, err := New(cfg, s)
	require.NoError(t, err)
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		_ = s.Close()
	})
	waitUserScopedDataSweep(t, srv)
	require.False(t, srv.authConfig.DevAuthEnabled)
	return srv, s
}

func newProvisionFixtureOn(t *testing.T, srv *Server, s store.Store) *provisionFixture {
	t.Helper()
	ctx := context.Background()
	f := &provisionFixture{srv: srv, s: s}

	createTestUserWithRole(t, s, tid("prov-super"), "prov-super@example.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	createTestUserWithRole(t, s, tid("prov-hubadmin"), "prov-hubadmin@example.com", store.UserRoleMember, store.SystemRoleHubAdmin)

	inviterID := tid("prov-inviter")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: inviterID, Email: "prov-inviter@example.com", DisplayName: "Inviter", Role: store.UserRoleMember, Status: store.UserStatusActive}))
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "prov-invite-only", ScopeType: store.RoleScopeSystem, Permissions: []string{"user.invite"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: inviterID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)

	f.member = hubMemberUser(t, s, "prov-member")

	viewerID := tid("prov-viewer")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: viewerID, Email: "prov-viewer@example.com", DisplayName: "Viewer", Role: store.UserRoleViewer, Status: store.UserStatusActive}))
	require.NoError(t, syncHubRoleGrants(ctx, s, viewerID, store.UserRoleViewer, "test"))

	for _, p := range []struct {
		id  string
		dst **store.User
	}{{tid("prov-super"), &f.superAdmin}, {tid("prov-hubadmin"), &f.hubAdmin}, {inviterID, &f.inviter}, {viewerID, &f.viewer}} {
		u, err := s.GetUser(ctx, p.id)
		require.NoError(t, err)
		*p.dst = u
	}
	return f
}

// provisionRaw sends a raw body as user (nil user: no credentials).
func provisionRaw(t *testing.T, srv *Server, user *store.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, provisionPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != nil {
		token, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// provisionAs sends body (marshalled to JSON) as user.
func provisionAs(t *testing.T, srv *Server, user *store.User, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return provisionRaw(t, srv, user, string(raw))
}

func decodeProvisionResponse(t *testing.T, rec *httptest.ResponseRecorder) ProvisionUserResponse {
	t.Helper()
	var resp ProvisionUserResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp
}

// provisionUserKeys returns the sorted keys of the response "user" object.
func provisionUserKeys(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var raw struct {
		User map[string]json.RawMessage `json:"user"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	keys := make([]string, 0, len(raw.User))
	for k := range raw.User {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func provisionErr(t *testing.T, rec *httptest.ResponseRecorder) (code string, details map[string]interface{}) {
	t.Helper()
	resp := parseErrorResponse(t, rec.Body.Bytes())
	return resp.Error.Code, resp.Error.Details
}

func provisionAudits(t *testing.T, s store.Store, userID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "user", MutationType: provisionMutationType})
	require.NoError(t, err)
	var out []*store.MutationAuditRecord
	for _, r := range recs {
		if userID == "" || r.TargetID == userID {
			out = append(out, r)
		}
	}
	return out
}

func userByEmail(t *testing.T, s store.Store, email string) *store.User {
	t.Helper()
	u, err := s.GetUserByEmail(context.Background(), email)
	require.NoError(t, err, email)
	return u
}

func seedUser(t *testing.T, s store.Store, email, status, displayName string, note *string) *store.User {
	t.Helper()
	u := &store.User{ID: tid("seed-" + email), Email: email, DisplayName: displayName, Role: store.UserRoleMember, Status: status, InviteNote: note}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

// TestHandleProvisionUser covers the §8 outcome rows of POST /api/v1/users
// through the real handler chain and store.
func TestHandleProvisionUser(t *testing.T) {
	ctx := context.Background()

	t.Run("row13_super_admin_session_creates_invited_record", func(t *testing.T) {
		f := newProvisionFixture(t)
		rec := provisionAs(t, f.srv, f.superAdmin, map[string]interface{}{"email": "  New.Person@Example.com ", "displayName": " New Person ", "note": "Workshop"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		resp := decodeProvisionResponse(t, rec)
		assert.True(t, resp.Created)
		assert.Empty(t, resp.Warnings)
		assert.Equal(t, []string{"created", "displayName", "email", "id", "inviteNote", "invitedBy", "status"}, provisionUserKeys(t, rec),
			"detailed view; no role, no credential fields")
		assert.NotContains(t, rec.Body.String(), `"role"`)
		assert.Equal(t, "new.person@example.com", resp.User.Email)
		assert.Equal(t, "New Person", resp.User.DisplayName)
		assert.Equal(t, store.UserStatusInvited, resp.User.Status)
		assert.Equal(t, f.superAdmin.ID, resp.User.InvitedBy)
		require.NotNil(t, resp.User.InviteNote)
		assert.Equal(t, "Workshop", *resp.User.InviteNote)
		assert.Equal(t, "/api/v1/users/"+resp.User.ID, rec.Header().Get("Location"))

		u := userByEmail(t, f.s, "new.person@example.com")
		assert.Equal(t, resp.User.ID, u.ID)
		assert.Equal(t, store.UserStatusInvited, u.Status)
		assert.Equal(t, store.UserRoleMember, u.Role, "placeholder role, exactly as invite")
		assert.Equal(t, "New Person", u.DisplayName)
		require.NotNil(t, u.InvitedBy)
		assert.Equal(t, f.superAdmin.ID, *u.InvitedBy)

		bindings, err := f.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID)
		require.NoError(t, err)
		assert.Empty(t, bindings, "provisioning writes no binding")
		group, err := f.s.GetGroupBySlug(ctx, "hub-members")
		require.NoError(t, err)
		_, err = f.s.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, u.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "provisioning writes no group membership")

		audits := provisionAudits(t, f.s, u.ID)
		require.Len(t, audits, 1)
		a := audits[0]
		assert.Equal(t, f.superAdmin.ID, a.ActorPrincipalID)
		assert.Equal(t, string(CredentialKindInteractive), a.ActorCredentialType)
		assert.JSONEq(t, `{"email":"new.person@example.com","status":"invited","displayName":"New Person"}`, a.AfterSummary)
	})

	t.Run("row13_hub_admin_session", func(t *testing.T) {
		f := newProvisionFixture(t)
		rec := provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "by-hubadmin@example.com"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		u := userByEmail(t, f.s, "by-hubadmin@example.com")
		audits := provisionAudits(t, f.s, u.ID)
		require.Len(t, audits, 1)
		assert.Equal(t, string(CredentialKindInteractive), audits[0].ActorCredentialType)

		// The inviter holds only user.invite (no user.read) and can still
		// provision a new email: user.read selects the view, it never
		// admits.
		rec = provisionAs(t, f.srv, f.inviter, map[string]interface{}{"email": "by-inviter@example.com"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"created", "email", "id", "invitedBy", "status"}, provisionUserKeys(t, rec),
			"a created record always gets the detailed view")
	})

	t.Run("row14_identical_replay", func(t *testing.T) {
		f := newProvisionFixture(t)
		body := map[string]interface{}{"email": "replay@example.com", "displayName": "Bob", "note": "n1"}
		rec := provisionAs(t, f.srv, f.hubAdmin, body)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		id := decodeProvisionResponse(t, rec).User.ID

		rec = provisionAs(t, f.srv, f.hubAdmin, body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := decodeProvisionResponse(t, rec)
		assert.False(t, resp.Created)
		assert.Equal(t, id, resp.User.ID, "detailed view with detail authority")
		assert.Empty(t, rec.Header().Get("Location"))

		// Case variant and surrounding whitespace in displayName replay.
		rec = provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "Replay@EXAMPLE.com", "displayName": "  Bob ", "note": "n1"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Len(t, provisionAudits(t, f.s, id), 1, "replays write no audit")

		// note "" and an absent note are the same input.
		rec = provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "nonote@example.com", "note": ""})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Nil(t, userByEmail(t, f.s, "nonote@example.com").InviteNote, `note "" is stored as NULL`)
		rec = provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "nonote@example.com"})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		rec = provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "nonote@example.com", "note": nil, "displayName": nil})
		assert.Equal(t, http.StatusOK, rec.Code, "explicit null counts as absent: %s", rec.Body.String())
	})

	t.Run("row14_replay_of_invite_created_record", func(t *testing.T) {
		f := newProvisionFixture(t)
		rec := doRequestAsUser(t, f.srv, f.superAdmin, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{Email: "invited@example.com", Note: "from invite"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var inv UserInviteResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &inv))

		rec = provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "invited@example.com", "note": "from invite"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := decodeProvisionResponse(t, rec)
		assert.False(t, resp.Created)
		assert.Equal(t, inv.ID, resp.User.ID)
	})

	t.Run("row14_minimal_view_without_detail_authority", func(t *testing.T) {
		f := newProvisionFixture(t)
		seedUser(t, f.s, "pending@example.com", store.UserStatusInvited, "", strPtr("n"))
		rec := provisionAs(t, f.srv, f.inviter, map[string]interface{}{"email": "pending@example.com", "note": "n"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"email", "status"}, provisionUserKeys(t, rec), "minimal view has exactly email and status")
		assert.NotContains(t, rec.Body.String(), tid("seed-pending@example.com"))
	})

	t.Run("rows15_16_17_collisions_with_detail_authority", func(t *testing.T) {
		f := newProvisionFixture(t)
		active := seedUser(t, f.s, "active@example.com", store.UserStatusActive, "Active", nil)
		suspended := seedUser(t, f.s, "suspended@example.com", store.UserStatusSuspended, "Susp", nil)
		pending := seedUser(t, f.s, "pending@example.com", store.UserStatusInvited, "Bob", strPtr("note"))

		// Row 16: active, any case variant; never userId.
		rec := provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "ACTIVE@example.com"})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		code, details := provisionErr(t, rec)
		assert.Equal(t, ErrCodeConflict, code)
		assert.Equal(t, provisionReasonUserExists, details["reason"])
		assert.NotContains(t, details, "userId")
		assert.NotContains(t, rec.Body.String(), active.ID)

		// Row 17: suspended stays suspended.
		rec = provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "suspended@example.com"})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		_, details = provisionErr(t, rec)
		assert.Equal(t, provisionReasonSuspendedUserExists, details["reason"])
		assert.NotContains(t, details, "userId")
		assert.Equal(t, store.UserStatusSuspended, userByEmail(t, f.s, suspended.Email).Status)

		// Row 15: different displayName, different note, note differing
		// only by whitespace (notes are not trimmed).
		for _, body := range []map[string]interface{}{
			{"email": "pending@example.com", "displayName": "Robert", "note": "note"},
			{"email": "pending@example.com", "displayName": "Bob", "note": "other"},
			{"email": "pending@example.com", "displayName": "Bob", "note": " note "},
			{"email": "pending@example.com", "displayName": "Bob"},
		} {
			rec = provisionAs(t, f.srv, f.hubAdmin, body)
			require.Equal(t, http.StatusConflict, rec.Code, "%v: %s", body, rec.Body.String())
			_, details = provisionErr(t, rec)
			assert.Equal(t, provisionReasonPendingUserExists, details["reason"])
			assert.Equal(t, pending.ID, details["userId"])
		}
		got := userByEmail(t, f.s, pending.Email)
		assert.Equal(t, "Bob", got.DisplayName, "record unchanged")
		require.NotNil(t, got.InviteNote)
		assert.Equal(t, "note", *got.InviteNote)
		assert.Empty(t, provisionAudits(t, f.s, ""), "collisions write no audit")
	})

	t.Run("rows15_16_17_collisions_without_detail_authority", func(t *testing.T) {
		f := newProvisionFixture(t)
		active := seedUser(t, f.s, "active@example.com", store.UserStatusActive, "Active", nil)
		suspended := seedUser(t, f.s, "suspended@example.com", store.UserStatusSuspended, "", nil)
		pending := seedUser(t, f.s, "pending@example.com", store.UserStatusInvited, "Bob", nil)
		for _, email := range []string{active.Email, suspended.Email, pending.Email} {
			rec := provisionAs(t, f.srv, f.inviter, map[string]interface{}{"email": email, "displayName": "Someone Else"})
			require.Equal(t, http.StatusConflict, rec.Code, "%s: %s", email, rec.Body.String())
			code, details := provisionErr(t, rec)
			assert.Equal(t, ErrCodeConflict, code)
			assert.Equal(t, provisionReasonUserExists, details["reason"], email)
			assert.NotContains(t, details, "userId", email)
			for _, id := range []string{active.ID, suspended.ID, pending.ID} {
				assert.NotContains(t, rec.Body.String(), id)
			}
		}
	})

	t.Run("row01_unauthenticated", func(t *testing.T) {
		f := newProvisionFixture(t)
		rec := provisionAs(t, f.srv, nil, map[string]interface{}{"email": "x@example.com"})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	})

	t.Run("row03_suspended_session_caller", func(t *testing.T) {
		f := newProvisionFixture(t)
		token, _, _, err := f.srv.userTokenService.GenerateTokenPair(f.hubAdmin.ID, f.hubAdmin.Email, f.hubAdmin.DisplayName, f.hubAdmin.Role, ClientTypeWeb)
		require.NoError(t, err)
		u := userByEmail(t, f.s, f.hubAdmin.Email)
		u.Status = store.UserStatusSuspended
		require.NoError(t, f.s.UpdateUser(ctx, u))

		req := httptest.NewRequest(http.MethodPost, provisionPath, strings.NewReader(`{"email":"x@example.com"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		code, _ := provisionErr(t, rec)
		assert.Equal(t, "user_suspended", code)
		_, err = f.s.GetUserByEmail(ctx, "x@example.com")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("row04_row05_non_session_credentials", func(t *testing.T) {
		f := newProvisionFixture(t)
		user := NewAuthenticatedUser(f.superAdmin.ID, f.superAdmin.Email, "Super", store.UserRoleAdmin, string(ClientTypeAPI))
		call := func(identity Identity, kind CredentialKind) *httptest.ResponseRecorder {
			c := contextWithCredentialContext(contextWithIdentity(context.Background(), identity), CredentialContext{Kind: kind})
			rec := httptest.NewRecorder()
			f.srv.handleProvisionUser(rec, requestWithContext(c, http.MethodPost, provisionPath, map[string]interface{}{"email": "nope@example.com"}))
			return rec
		}

		// Row 5: user identities presented with a non-session credential
		// are refused by the session-only gate with its reason.
		for _, kind := range []CredentialKind{CredentialKindUAT, CredentialKindAgentJWT, CredentialKindBroker, CredentialKindFederation, CredentialKindHubDelivery, ""} {
			rec := call(user, kind)
			requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "credential kind "+string(kind))
		}
		// Row 4: non-user principals get 403 without a session-only reason.
		for name, identity := range map[string]Identity{
			"federated service": NewFederatedServiceIdentity("https://issuer.example", "svc", "svc@example.com", nil),
			"federated agent":   NewFederatedAgentIdentity("https://issuer.example", "agent-1", "proj-1", "agent", "root@example.com", nil, nil),
			"broker":            NewBrokerIdentity("broker-1"),
		} {
			rec := call(identity, CredentialKindInteractive)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
			reason, _ := sessionOnlyDetailsOf(rec)
			assert.Empty(t, reason, name)
		}
		_, err := f.s.GetUserByEmail(ctx, "nope@example.com")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("row04a_dev_auth_mode_refused", func(t *testing.T) {
		// Dev auth is single-user local mode: on a hub running with dev
		// auth, provisioning is refused for every caller, before any body
		// check: the dev credential, the session the web dev auto-login
		// mints for the dev user, and any other admin's session.
		srv, s := testServer(t)
		require.True(t, srv.authConfig.DevAuthEnabled)
		f := newProvisionFixtureOn(t, srv, s)
		devUser := getDevUser(t, srv, s)

		callers := map[string]func(body string) *httptest.ResponseRecorder{
			"dev credential": func(b string) *httptest.ResponseRecorder {
				return doRequestRaw(t, srv, http.MethodPost, provisionPath, []byte(b), "application/json")
			},
			"web session of the dev user": func(b string) *httptest.ResponseRecorder { return provisionRaw(t, srv, devUser, b) },
			"session of another super-admin": func(b string) *httptest.ResponseRecorder {
				return provisionRaw(t, srv, f.superAdmin, b)
			},
		}
		for name, send := range callers {
			for _, body := range []string{
				`{"email":"by-dev@example.com"}`,
				`{"email":"by-dev@example.com","role":"admin"}`,
				`{not json`,
			} {
				rec := send(body)
				require.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", name, body, rec.Body.String())
				code, details := provisionErr(t, rec)
				assert.Equal(t, ErrCodeForbidden, code, name)
				assert.Equal(t, provisionReasonDevAuthNotSupported, details["reason"], name)
			}
		}
		_, err := s.GetUserByEmail(ctx, "by-dev@example.com")
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.Empty(t, provisionAudits(t, s, ""))

		// A caller without user.invite still gets 403; tokens still get the
		// session-only refusal first (row 5).
		rec := provisionAs(t, srv, f.member, map[string]interface{}{"email": "x@example.com"})
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("row05_real_hub_token_with_user_invite_refused", func(t *testing.T) {
		m := newBearerMatrixFixture(t)
		key := m.mint(t, hubBoundary(), []string{"user:invite"})
		rec := doRequestWithUAT(t, m.srv, key, http.MethodPost, provisionPath, map[string]interface{}{"email": "via-token@example.com"})
		requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "hub token with user:invite")
		_, err := m.store.GetUserByEmail(ctx, "via-token@example.com")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("row08_callers_without_user_invite", func(t *testing.T) {
		f := newProvisionFixture(t)
		// member: seeded hub-member catalog grants only; viewer: hub-viewer.
		for name, u := range map[string]*store.User{"hub member": f.member, "viewer": f.viewer} {
			rec := provisionAs(t, f.srv, u, map[string]interface{}{"email": "x@example.com"})
			require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
			assertStructuredDenial(t, parseErrorResponse(t, rec.Body.Bytes()), "user", "invite")
		}
	})

	t.Run("row08_ordering_authorization_before_decoding", func(t *testing.T) {
		f := newProvisionFixture(t)
		for _, body := range []string{
			`{"email":"x@example.com","role":"admin"}`,
			`{"email":"x@example.com","role":"member"}`,
			`{"email":"x@example.com","status":"active"}`,
			`{not json`,
			`[]`,
			`{"email":"not-an-email"}`,
			`{"email":"x@example.com","displayName":"` + strings.Repeat("a", 200) + `"}`,
		} {
			rec := provisionRaw(t, f.srv, f.member, body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", body, rec.Body.String())
		}
	})

	t.Run("row08_replay_is_reauthorized", func(t *testing.T) {
		f := newProvisionFixture(t)
		body := map[string]interface{}{"email": "reauth@example.com"}
		rec := provisionAs(t, f.srv, f.inviter, body)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		bindings, err := f.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.inviter.ID)
		require.NoError(t, err)
		require.Len(t, bindings, 1)
		require.NoError(t, f.s.DeleteRoleBinding(ctx, bindings[0].ID))

		rec = provisionAs(t, f.srv, f.inviter, body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "a replay after losing user.invite is denied: %s", rec.Body.String())
	})

	t.Run("row09_malformed_and_wrong_types", func(t *testing.T) {
		f := newProvisionFixture(t)
		for _, body := range []string{
			``, `{not json`, `[]`, `"x"`, `null`, `5`,
			`{"email":5}`, `{"email":"x@example.com","note":{}}`, `{"email":"x@example.com","displayName":true}`,
			`{"email":"x@example.com"} {"email":"y@example.com"}`,
			`{"email":"x@example.com"}}`,
			`{"email":"x@example.com"}]`,
			`{"email":"x@example.com"} x`,
			`{"email":"x@example.com","email":"y@example.com"}`,
			`{"email":"x@example.com","role":"admin","role":null}`,
		} {
			rec := provisionRaw(t, f.srv, f.hubAdmin, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%q: %s", body, rec.Body.String())
			code, _ := provisionErr(t, rec)
			assert.Equal(t, ErrCodeInvalidRequest, code, body)
		}
		_, err := f.s.GetUserByEmail(ctx, "x@example.com")
		assert.ErrorIs(t, err, store.ErrNotFound, "no malformed body creates a record")
		// Surrounding whitespace is still accepted.
		rec := provisionRaw(t, f.srv, f.hubAdmin, " \n {\"email\":\"ws@example.com\"} \n ")
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})

	t.Run("row09_unknown_fields", func(t *testing.T) {
		f := newProvisionFixture(t)
		for _, field := range []string{"status", "avatarUrl", "preferences", "id", "groups", "provider", "subject"} {
			rec := provisionRaw(t, f.srv, f.hubAdmin, `{"email":"x@example.com","`+field+`":"v"}`)
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", field, rec.Body.String())
			resp := parseErrorResponse(t, rec.Body.Bytes())
			assert.Equal(t, ErrCodeInvalidRequest, resp.Error.Code)
			assert.Equal(t, `unknown field "`+field+`"; allowed fields are email, displayName, note`, resp.Error.Message)
		}
		_, err := f.s.GetUserByEmail(ctx, "x@example.com")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("row10_invalid_emails_match_invite", func(t *testing.T) {
		f := newProvisionFixture(t)
		for _, email := range []string{"", "   ", "not-an-email", "a@", "@b.com", "a@@b.com"} {
			inv := doRequestAsUser(t, f.srv, f.superAdmin, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{Email: email})
			prov := provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": email})
			require.Equal(t, http.StatusBadRequest, inv.Code, "invite %q", email)
			require.Equal(t, http.StatusBadRequest, prov.Code, "provision %q: %s", email, prov.Body.String())
			invResp := parseErrorResponse(t, inv.Body.Bytes())
			provResp := parseErrorResponse(t, prov.Body.Bytes())
			assert.Equal(t, invResp.Error.Code, provResp.Error.Code, email)
			assert.Equal(t, invResp.Error.Message, provResp.Error.Message, email)
			assert.Equal(t, "email", provResp.Error.Details["field"])
		}
		// A missing email is the same as an empty one.
		rec := provisionRaw(t, f.srv, f.hubAdmin, `{"displayName":"x"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("row10_email_parity_display_name_form", func(t *testing.T) {
		f := newProvisionFixture(t)
		rec := doRequestAsUser(t, f.srv, f.superAdmin, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{Email: "Bob <bob@x.com>"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		rec = provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "Carol <carol@x.com>"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, "bob <bob@x.com>", userByEmail(t, f.s, "bob <bob@x.com>").Email)
		assert.Equal(t, "carol <carol@x.com>", userByEmail(t, f.s, "carol <carol@x.com>").Email)
	})

	t.Run("row11_display_name_and_note_bounds", func(t *testing.T) {
		f := newProvisionFixture(t)
		bad := []struct {
			body  map[string]interface{}
			field string
		}{
			{map[string]interface{}{"email": "a@example.com", "displayName": strings.Repeat("é", 129)}, "displayName"},
			{map[string]interface{}{"email": "a@example.com", "displayName": "Bob\x07"}, "displayName"},
			{map[string]interface{}{"email": "a@example.com", "displayName": "Bo\nb"}, "displayName"},
			{map[string]interface{}{"email": "a@example.com", "note": strings.Repeat("n", 501)}, "note"},
			{map[string]interface{}{"email": "a@example.com", "note": "tab\there"}, "note"},
			{map[string]interface{}{"email": "a@example.com", "note": "nul\x00here"}, "note"},
			{map[string]interface{}{"email": "a@example.com", "displayName": "Bo\rb"}, "displayName"},
		}
		for _, tc := range bad {
			rec := provisionAs(t, f.srv, f.hubAdmin, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "%v: %s", tc.body, rec.Body.String())
			code, details := provisionErr(t, rec)
			assert.Equal(t, ErrCodeValidationError, code)
			assert.Equal(t, tc.field, details["field"])
		}
		good := []map[string]interface{}{
			{"email": "b@example.com", "displayName": strings.Repeat("é", 128), "note": strings.Repeat("n", 500)},
			{"email": "c@example.com", "displayName": "   ", "note": "line1\nline2"},
			{"email": "d@example.com", "note": "line1\r\nline2\r\n"},
		}
		for _, body := range good {
			rec := provisionAs(t, f.srv, f.hubAdmin, body)
			require.Equal(t, http.StatusCreated, rec.Code, "%v: %s", body, rec.Body.String())
		}
		assert.Equal(t, "", userByEmail(t, f.s, "c@example.com").DisplayName, "blank displayName is stored empty")
		crlf := userByEmail(t, f.s, "d@example.com").InviteNote
		require.NotNil(t, crlf)
		assert.Equal(t, "line1\r\nline2\r\n", *crlf, "CRLF line breaks are accepted and stored untrimmed")
	})

	t.Run("row12_any_role_is_refused_for_authorized_callers", func(t *testing.T) {
		f := newProvisionFixture(t)
		roles := map[string]string{
			`"admin"`:  provisionReasonPrivilegedRole,
			`"member"`: provisionReasonRoleNotSupported,
			`"viewer"`: provisionReasonRoleNotSupported,
			`5`:        provisionReasonRoleNotSupported,
			`true`:     provisionReasonRoleNotSupported,
			`{}`:       provisionReasonRoleNotSupported,
			`[]`:       provisionReasonRoleNotSupported,
		}
		send := map[string]func(body string) *httptest.ResponseRecorder{
			"session super-admin": func(b string) *httptest.ResponseRecorder { return provisionRaw(t, f.srv, f.superAdmin, b) },
			"session inviter":     func(b string) *httptest.ResponseRecorder { return provisionRaw(t, f.srv, f.inviter, b) },
		}
		for caller, fn := range send {
			for role, reason := range roles {
				rec := fn(`{"email":"r@example.com","role":` + role + `}`)
				require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "%s role=%s: %s", caller, role, rec.Body.String())
				code, details := provisionErr(t, rec)
				assert.Equal(t, ErrCodeUnprocessable, code)
				assert.Equal(t, reason, details["reason"], "%s role=%s", caller, role)
			}
		}
		// An explicit null role is absent.
		rec := provisionRaw(t, f.srv, f.superAdmin, `{"email":"r@example.com","role":null}`)
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})

	t.Run("row13_warnings", func(t *testing.T) {
		f := newProvisionFixture(t)
		f.srv.mu.Lock()
		f.srv.config.AuthorizedDomains = []string{"corp.example"}
		f.srv.config.AdminEmails = []string{"boss@outside.example"}
		f.srv.mu.Unlock()
		f.srv.platformAuthSA = "transport@corp.example"

		rec := provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "outsider@other.example"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, []string{provisionWarningDomainUnauthorized}, decodeProvisionResponse(t, rec).Warnings)

		rec = provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "boss@outside.example"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Empty(t, decodeProvisionResponse(t, rec).Warnings, "admin_emails are not outside the domains")

		rec = provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "Transport@corp.example"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, []string{provisionWarningReservedIdentity}, decodeProvisionResponse(t, rec).Warnings)
		// The warning predicts the sign-in outcome: the API login decision
		// point refuses the reserved identity, and the record stays invited.
		_, err := f.srv.provisionUser(ctx, &ExternalUserInfo{Email: "transport@corp.example"})
		assert.Error(t, err, "sign-in of a reserved identity is refused")
		assert.Equal(t, store.UserStatusInvited, userByEmail(t, f.s, "transport@corp.example").Status)
		// Likewise for the out-of-domain record.
		_, err = f.srv.provisionUser(ctx, &ExternalUserInfo{Email: "outsider@other.example"})
		assert.ErrorIs(t, err, ErrAccessDenied, "sign-in outside authorized_domains is refused")

		f.srv.mu.Lock()
		f.srv.config.AuthorizedDomains = nil
		f.srv.config.UserAccessMode = "domain_restricted"
		f.srv.mu.Unlock()
		rec = provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "anyone@corp.example"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, []string{provisionWarningAccessModeBlocked}, decodeProvisionResponse(t, rec).Warnings)
	})

	t.Run("row18_concurrent_identical_requests", func(t *testing.T) {
		f := newProvisionFixture(t)
		const n = 8
		codes := make([]int, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				codes[i] = provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "race@example.com", "displayName": "Race"}).Code
			}(i)
		}
		wg.Wait()
		created := 0
		for _, c := range codes {
			assert.Contains(t, []int{http.StatusCreated, http.StatusOK}, c, "codes: %v", codes)
			if c == http.StatusCreated {
				created++
			}
		}
		assert.Equal(t, 1, created, "codes: %v", codes)
		u := userByEmail(t, f.s, "race@example.com")
		assert.Len(t, provisionAudits(t, f.s, u.ID), 1)
	})

	t.Run("row18_lost_race_is_reclassified", func(t *testing.T) {
		// The winner of the unique-index race is committed first; the
		// losing request re-reads and classifies it as row 14, 15, 16 or
		// 17, including the detail-authority variant.
		cases := []struct {
			name        string
			status      string
			displayName string
			detail      bool // caller is the hub-admin (detail authority) or the inviter
			wantCode    int
			wantReason  string
			wantUserID  bool
		}{
			{"identical pending, hub-admin", store.UserStatusInvited, "Winner", true, http.StatusOK, "", false},
			{"identical pending, inviter", store.UserStatusInvited, "Winner", false, http.StatusOK, "", false},
			{"different pending, hub-admin", store.UserStatusInvited, "Other", true, http.StatusConflict, provisionReasonPendingUserExists, true},
			{"different pending, inviter", store.UserStatusInvited, "Other", false, http.StatusConflict, provisionReasonUserExists, false},
			{"active, hub-admin", store.UserStatusActive, "Winner", true, http.StatusConflict, provisionReasonUserExists, false},
			{"active, inviter", store.UserStatusActive, "Winner", false, http.StatusConflict, provisionReasonUserExists, false},
			{"suspended, hub-admin", store.UserStatusSuspended, "Winner", true, http.StatusConflict, provisionReasonSuspendedUserExists, false},
			{"suspended, inviter", store.UserStatusSuspended, "Winner", false, http.StatusConflict, provisionReasonUserExists, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				srv, s := testServerNoDevAuth(t)
				race := &provisionRaceStore{Store: s}
				installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *provisionRaceStore {
					race.Store = inner
					return race
				})
				f := newProvisionFixtureOn(t, srv, s)
				caller := f.inviter
				if tc.detail {
					caller = f.hubAdmin
				}
				winnerBy := f.hubAdmin.ID
				winnerID := tid("race-winner-" + tc.name)
				race.winner = &store.User{ID: winnerID, Email: "racer@example.com", DisplayName: tc.displayName,
					Status: tc.status, Role: store.UserRoleMember, InvitedBy: &winnerBy}

				rec := provisionAs(t, f.srv, caller, map[string]interface{}{"email": "racer@example.com", "displayName": "Winner"})
				require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
				if tc.wantCode == http.StatusOK {
					resp := decodeProvisionResponse(t, rec)
					assert.False(t, resp.Created)
					if tc.detail {
						assert.Equal(t, winnerID, resp.User.ID, "detailed view with detail authority")
						assert.Equal(t, "Winner", resp.User.DisplayName)
					} else {
						assert.Equal(t, []string{"email", "status"}, provisionUserKeys(t, rec))
					}
				} else {
					_, details := provisionErr(t, rec)
					assert.Equal(t, tc.wantReason, details["reason"])
					if tc.wantUserID {
						assert.Equal(t, winnerID, details["userId"])
					} else {
						assert.NotContains(t, details, "userId")
						assert.NotContains(t, rec.Body.String(), winnerID)
					}
				}
				assert.Empty(t, provisionAudits(t, s, ""), "the losing request writes no audit")
				got := userByEmail(t, s, "racer@example.com")
				assert.Equal(t, winnerID, got.ID)
				assert.Equal(t, tc.status, got.Status, "the winner's record is never modified")
				assert.Equal(t, tc.displayName, got.DisplayName)
			})
		}
	})

	t.Run("row19_create_failure_rolls_back", func(t *testing.T) {
		srv, s := testServerNoDevAuth(t)
		fault := &provisionFaultStore{Store: s, createErr: errors.New("injected create fault")}
		installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *provisionFaultStore {
			fault.Store = inner
			return fault
		})
		f := newProvisionFixtureOn(t, srv, s)
		rec := provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "fail@example.com"})
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		code, _ := provisionErr(t, rec)
		assert.Equal(t, ErrCodeInternalError, code)
		_, err := s.GetUserByEmail(ctx, "fail@example.com")
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.Empty(t, provisionAudits(t, s, ""))
	})

	t.Run("row20_audit_failure_rolls_back_user_row", func(t *testing.T) {
		srv, s := testServerNoDevAuth(t)
		fault := &provisionFaultStore{Store: s, auditErr: errors.New("injected audit fault")}
		installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *provisionFaultStore {
			fault.Store = inner
			return fault
		})
		f := newProvisionFixtureOn(t, srv, s)
		rec := provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "noaudit@example.com"})
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		_, err := s.GetUserByEmail(ctx, "noaudit@example.com")
		assert.ErrorIs(t, err, store.ErrNotFound, "no user row without its audit")
	})

	t.Run("row21_post_commit_runs_after_client_cancel", func(t *testing.T) {
		f := newProvisionFixture(t)
		logger := &ctxRecordingInviteAuditLogger{}
		f.srv.auditLogger = logger
		// A request whose context is already cancelled when the
		// post-commit side effects run (the client disconnected after the
		// commit): the side effects still see a live context.
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodPost, provisionPath, nil).WithContext(ctx)
		cancel()
		u := &store.User{ID: tid("pc-user"), Email: "pc@example.com"}
		actor := NewAuthenticatedUser(f.hubAdmin.ID, f.hubAdmin.Email, "Hub Admin", store.UserRoleMember, string(ClientTypeWeb))
		f.srv.provisionPostCommit(req, actor, u)
		require.Len(t, logger.ctxErrs, 1)
		assert.NoError(t, logger.ctxErrs[0], "the invite audit event runs on a context the client cannot cancel")
	})

	t.Run("row21_post_commit_failure_keeps_201", func(t *testing.T) {
		f := newProvisionFixture(t)
		logger := &failingInviteAuditLogger{}
		f.srv.auditLogger = logger
		spy := &provisionEventSpy{}
		f.srv.SetEventPublisher(spy)

		rec := provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "post@example.com"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		u := userByEmail(t, f.s, "post@example.com")
		assert.Len(t, provisionAudits(t, f.s, u.ID), 1, "the mutation audit is committed")
		require.Len(t, logger.attempts, 1, "the invite audit event was attempted")
		assert.Equal(t, InviteAuditUserProvisioned, logger.attempts[0].EventType)
		assert.Equal(t, map[string]string{"user_id": u.ID}, logger.attempts[0].Details)
		assert.Equal(t, f.hubAdmin.ID, logger.attempts[0].ActorID)
		assert.Equal(t, []string{"provisioned:post@example.com"}, spy.actions())
	})
}

// TestHandleProvisionUser_NoRegression pins the interaction of provisioned
// records with existing user operations.
func TestHandleProvisionUser_NoRegression(t *testing.T) {
	ctx := context.Background()
	f := newProvisionFixture(t)
	rec := provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": "prov@example.com", "displayName": "Prov"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeProvisionResponse(t, rec).User.ID

	// Bulk invite skips an already provisioned email.
	rec = doRequestAsUser(t, f.srv, f.superAdmin, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{Emails: []UserInviteRequest{{Email: "prov@example.com"}, {Email: "fresh@example.com"}}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var bulk UserInviteBulkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &bulk))
	assert.Equal(t, 1, bulk.Invited)
	assert.Equal(t, 1, bulk.Skipped)

	// Single invite of a provisioned email: the undifferentiated 409.
	rec = doRequestAsUser(t, f.srv, f.superAdmin, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{Email: "prov@example.com"})
	assert.Equal(t, http.StatusConflict, rec.Code)

	// PATCH role on the provisioned (invited) record is refused.
	rec = doRequestAsUser(t, f.srv, f.superAdmin, http.MethodPatch, "/api/v1/users/"+id, map[string]interface{}{"role": "viewer"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// DELETE of the provisioned record works.
	rec = doRequestAsUser(t, f.srv, f.superAdmin, http.MethodDelete, "/api/v1/users/"+id, nil)
	assert.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code, rec.Body.String())
	_, err := f.s.GetUser(ctx, id)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestAdminUserInvite_CharacterizationOnSharedCore pins that the single
// invite response, audit event and event publication are unchanged after
// the move onto createPendingUserTx.
func TestAdminUserInvite_CharacterizationOnSharedCore(t *testing.T) {
	srv, s := testServer(t)
	logger := &recordingAuditLogger{}
	srv.auditLogger = logger
	spy := &provisionEventSpy{}
	srv.SetEventPublisher(spy)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", map[string]string{"email": " Alice@Example.COM ", "note": "Workshop"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"created", "email", "id", "invitedBy", "status"}, keys, "invite response shape unchanged")
	assert.Equal(t, "alice@example.com", raw["email"])
	assert.Equal(t, "invited", raw["status"])

	u := userByEmail(t, s, "alice@example.com")
	assert.Equal(t, store.UserRoleMember, u.Role)
	assert.Equal(t, "", u.DisplayName)
	require.NotNil(t, u.InviteNote)
	assert.Equal(t, "Workshop", *u.InviteNote)

	logger.mu.Lock()
	require.Len(t, logger.invite, 1)
	ev := logger.invite[0]
	logger.mu.Unlock()
	assert.Equal(t, InviteAuditUserInvited, ev.EventType)
	assert.Equal(t, "alice@example.com", ev.Email)
	assert.Empty(t, ev.InviteID)
	assert.Nil(t, ev.Details)
	assert.Equal(t, []string{"invited:alice@example.com"}, spy.actions())

	// Every existing state is one undifferentiated 409, with no details.
	seedUser(t, s, "susp@example.com", store.UserStatusSuspended, "", nil)
	for _, email := range []string{"alice@example.com", "susp@example.com"} {
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", map[string]string{"email": email, "note": "Workshop"})
		require.Equal(t, http.StatusConflict, rec.Code)
		resp := parseErrorResponse(t, rec.Body.Bytes())
		assert.Equal(t, "user already exists", resp.Error.Message)
		assert.Empty(t, resp.Error.Details)
	}
	// The mutation audit table is not written by invite.
	assert.Empty(t, provisionAudits(t, s, ""))

	// Bulk invite (only its email rule moved to NormalizeInviteEmail):
	// response shape, counts, the user_invited_bulk event and the
	// bulk_invited publication are unchanged. Invalid emails are skipped
	// silently, as before.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", map[string]interface{}{"emails": []map[string]string{
		{"email": " Bob@Example.com "}, {"email": "alice@example.com"}, {"email": "not-an-email"}, {"email": "carol@example.com", "note": "n"},
	}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	raw = map[string]interface{}{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	keys = keys[:0]
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"errors", "invited", "skipped", "total"}, keys, "bulk response shape unchanged")
	assert.Equal(t, float64(2), raw["invited"])
	assert.Equal(t, float64(1), raw["skipped"])
	assert.Equal(t, float64(3), raw["total"])
	assert.Equal(t, store.UserStatusInvited, userByEmail(t, s, "bob@example.com").Status)
	logger.mu.Lock()
	require.Len(t, logger.invite, 2)
	bulkEv := logger.invite[1]
	logger.mu.Unlock()
	assert.Equal(t, InviteAuditUserInvitedBulk, bulkEv.EventType)
	assert.Equal(t, 2, bulkEv.Count)
	assert.Equal(t, map[string]string{"skipped": "1"}, bulkEv.Details)
	assert.Equal(t, []string{"invited:alice@example.com", "bulk_invited:"}, spy.actions())
}

// TestCreatePendingUserTx_Classification pins the shared core's outcome
// classification and that it never modifies an existing record.
func TestCreatePendingUserTx_Classification(t *testing.T) {
	ctx := context.Background()
	_, s := testServer(t)
	seedUser(t, s, "active@example.com", store.UserStatusActive, "A", nil)
	seedUser(t, s, "susp@example.com", store.UserStatusSuspended, "S", nil)
	seedUser(t, s, "pend@example.com", store.UserStatusInvited, "P", strPtr("n"))

	cases := []struct {
		spec PendingUserSpec
		want PendingOutcome
	}{
		{PendingUserSpec{Email: "ACTIVE@example.com"}, PendingExistingActive},
		{PendingUserSpec{Email: "susp@example.com"}, PendingExistingSuspended},
		{PendingUserSpec{Email: "pend@example.com", DisplayName: "P", Note: strPtr("n")}, PendingExistingIdentical},
		{PendingUserSpec{Email: "pend@example.com", DisplayName: "P"}, PendingExistingDifferent},
		{PendingUserSpec{Email: "pend@example.com", DisplayName: "Q", Note: strPtr("n")}, PendingExistingDifferent},
		{PendingUserSpec{Email: "new@example.com", Note: strPtr(""), InvitedBy: "actor"}, PendingCreated},
	}
	for _, tc := range cases {
		u, got, err := createPendingUserTx(ctx, s, tc.spec)
		require.NoError(t, err, tc.spec.Email)
		assert.Equal(t, tc.want, got, tc.spec.Email)
		require.NotNil(t, u)
	}
	created := userByEmail(t, s, "new@example.com")
	assert.Nil(t, created.InviteNote, `a "" note is stored as NULL`)
	assert.Equal(t, store.UserStatusInvited, created.Status)
	assert.Equal(t, "P", userByEmail(t, s, "pend@example.com").DisplayName)

	_, _, err := createPendingUserTx(ctx, s, PendingUserSpec{Email: "bad"})
	assert.Error(t, err)
}

// TestNormalizeInviteEmail pins the shared email rule.
func TestNormalizeInviteEmail(t *testing.T) {
	ok := map[string]string{
		"a@b.com":           "a@b.com",
		"  A@B.Com ":        "a@b.com",
		"Bob <bob@x.com>":   "bob <bob@x.com>",
		"first.last@ex.org": "first.last@ex.org",
	}
	for in, want := range ok {
		got, err := NormalizeInviteEmail(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got)
	}
	for _, in := range []string{"", "  ", "nope", "a@", "@b"} {
		_, err := NormalizeInviteEmail(in)
		assert.Error(t, err, in)
	}
}

// --- test doubles ---

// provisionFaultStore injects create or audit failures, including inside
// WithTx.
type provisionFaultStore struct {
	store.Store
	createErr error
	auditErr  error
}

func (s *provisionFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		c := *s
		c.Store = tx
		return fn(&c)
	})
}

func (s *provisionFaultStore) CreateUser(ctx context.Context, u *store.User) error {
	if s.createErr != nil && u.Status == store.UserStatusInvited {
		return s.createErr
	}
	return s.Store.CreateUser(ctx, u)
}

func (s *provisionFaultStore) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if s.auditErr != nil && r.MutationType == provisionMutationType {
		return s.auditErr
	}
	return s.Store.CreateMutationAudit(ctx, r)
}

// provisionRaceStore simulates losing the unique-index race once: the
// winner's identical record is committed before the transaction starts,
// and the first lookup inside the transaction misses it (as if it ran
// before the winner committed). The insert then fails on the unique email
// index with store.ErrAlreadyExists.
type provisionRaceStore struct {
	store.Store
	winner *store.User
}

func (s *provisionRaceStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	winner := s.winner
	s.winner = nil
	if winner != nil {
		if err := s.CreateUser(ctx, winner); err != nil {
			return err
		}
	}
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&provisionRaceTx{Store: tx, missEmail: winnerEmail(winner)})
	})
}

func winnerEmail(u *store.User) string {
	if u == nil {
		return ""
	}
	return u.Email
}

type provisionRaceTx struct {
	store.Store
	missEmail string
}

func (t *provisionRaceTx) GetUserByEmail(ctx context.Context, email string) (*store.User, error) {
	if t.missEmail != "" && strings.EqualFold(email, t.missEmail) {
		t.missEmail = ""
		return nil, store.ErrNotFound
	}
	return t.Store.GetUserByEmail(ctx, email)
}

// failingInviteAuditLogger records invite audit attempts and fails each.
type failingInviteAuditLogger struct {
	recordingAuditLogger
	attempts []*InviteAuditEvent
}

func (l *failingInviteAuditLogger) LogInviteAuditEvent(_ context.Context, event *InviteAuditEvent) error {
	l.attempts = append(l.attempts, event)
	return errors.New("injected invite audit failure")
}

// ctxRecordingInviteAuditLogger records the context error seen by each
// invite audit event.
type ctxRecordingInviteAuditLogger struct {
	recordingAuditLogger
	ctxErrs []error
}

func (l *ctxRecordingInviteAuditLogger) LogInviteAuditEvent(ctx context.Context, _ *InviteAuditEvent) error {
	l.ctxErrs = append(l.ctxErrs, ctx.Err())
	return nil
}

// provisionEventSpy records allow-list change events.
type provisionEventSpy struct {
	noopEventPublisher
	mu  sync.Mutex
	got []string
}

func (e *provisionEventSpy) PublishAllowListChanged(_ context.Context, action, email string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, action+":"+email)
}

func (e *provisionEventSpy) actions() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.got...)
}
