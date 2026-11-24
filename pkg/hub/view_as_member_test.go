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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/eventstreams"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func enableViewAsMemberExperiment(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	if enabled {
		fakeStore.seed("experiments", json.RawMessage(`{"overrides":{"hub.view_as_member":true}}`))
	} else {
		fakeStore.seed("experiments", json.RawMessage(`{"overrides":{"hub.view_as_member":false}}`))
	}
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

func TestViewAsMember_ExperimentRegistered(t *testing.T) {
	exp, ok := experiments.Default().Lookup(experiments.ViewAsMember)
	require.True(t, ok, "hub.view_as_member must be registered")
	assert.Equal(t, "hub.view_as_member", exp.Name)
	assert.False(t, exp.Default, "hub.view_as_member must default to off")
	assert.Equal(t, experiments.StageAlpha, exp.Stage)
	assert.True(t, exp.HasLayer(experiments.LayerServer))
	assert.True(t, exp.HasLayer(experiments.LayerWeb))
	assert.Equal(t, testAdminPolicyDeny, testAdminExperimentPolicy[experiments.ViewAsMember])
}

// TestViewAsMember_ContextSetterCensus enforces design §4.2.1 and D4:
//   - only UnifiedAuthMiddleware (auth.go) and webSessionContextFromCookie
//     (view_as_member.go) may call contextWithViewAsMember;
//   - only RecordViewAsMemberTransition (view_as_member.go) may call
//     withoutViewAsMember;
//   - no file outside view_as_member.go may construct a viewAsMemberKey{}
//     composite literal;
//   - only unclampedUserForMaintenance (view_as_member.go) may call
//     (*AuthenticatedUser).WithoutViewAsMember;
//   - only adminModeMiddleware (admin_mode.go) and deferDuringMaintenance
//     (agent_pending_config_authz.go) may call unclampedUserForMaintenance.
func TestViewAsMember_ContextSetterCensus(t *testing.T) {
	allowedSetters := map[string]bool{
		"auth.go:UnifiedAuthMiddleware":                 true,
		"view_as_member.go:webSessionContextFromCookie": true,
	}
	allowedShadow := map[string]bool{
		"view_as_member.go:RecordViewAsMemberTransition": true,
	}
	allowedWithout := map[string]bool{
		"view_as_member.go:unclampedUserForMaintenance": true,
	}
	allowedUnclamp := map[string]bool{
		"admin_mode.go:adminModeMiddleware":                    true,
		"agent_pending_config_authz.go:deferDuringMaintenance": true,
	}
	foundSetters := map[string]bool{}
	foundShadow := map[string]bool{}
	foundWithout := map[string]bool{}
	foundUnclamp := map[string]bool{}
	var badKeyLiterals []string
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, _ *token.FileSet) {
		key := file + ":" + fn.Name.Name
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if lit, ok := n.(*ast.CompositeLit); ok {
				if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "viewAsMemberKey" && file != "view_as_member.go" {
					badKeyLiterals = append(badKeyLiterals, key)
				}
				return true
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.Ident:
				switch f.Name {
				case "contextWithViewAsMember":
					foundSetters[key] = true
				case "withoutViewAsMember":
					foundShadow[key] = true
				case "unclampedUserForMaintenance":
					foundUnclamp[key] = true
				}
			case *ast.SelectorExpr:
				if f.Sel.Name == "WithoutViewAsMember" {
					foundWithout[key] = true
				}
			}
			return true
		})
	})
	assert.Equal(t, allowedSetters, foundSetters, "contextWithViewAsMember must only be called from its two sanctioned entry points")
	assert.Equal(t, allowedShadow, foundShadow, "withoutViewAsMember must only be called from RecordViewAsMemberTransition")
	assert.Empty(t, badKeyLiterals, "viewAsMemberKey{} composite literals are only permitted in view_as_member.go")
	assert.Equal(t, allowedWithout, foundWithout, "WithoutViewAsMember must only be called from unclampedUserForMaintenance")
	assert.Equal(t, allowedUnclamp, foundUnclamp, "unclampedUserForMaintenance must only be called from D4 RealByDesign maintenance gates")
}

func TestViewAsMember_TokenMintAndCarrierIntegrity(t *testing.T) {
	svc, err := NewUserTokenService(UserTokenConfig{SigningKey: []byte("test-signing-key-32-bytes-long!!")})
	require.NoError(t, err)

	// Mint refuses ViewAsMember on non-web client types.
	for _, ct := range []ClientType{ClientTypeCLI, ClientTypeAPI} {
		_, _, _, err := svc.MintTokenPair(UserTokenSubject{
			UserID:       "admin-1",
			Email:        "admin@example.com",
			Role:         store.UserRoleAdmin,
			ClientType:   ct,
			ViewAsMember: true,
		})
		require.Error(t, err, "MintTokenPair must refuse vam on %s", ct)

		_, _, err = svc.MintAccessToken(UserTokenSubject{
			UserID:       "admin-1",
			Email:        "admin@example.com",
			Role:         store.UserRoleAdmin,
			ClientType:   ct,
			ViewAsMember: true,
		})
		require.Error(t, err, "MintAccessToken must refuse vam on %s", ct)

		_, _, err = svc.MintAccessTokenWithTTL(UserTokenSubject{
			UserID:       "admin-1",
			Email:        "admin@example.com",
			Role:         store.UserRoleAdmin,
			ClientType:   ct,
			ViewAsMember: true,
		}, 5*time.Minute)
		require.Error(t, err, "MintAccessTokenWithTTL must refuse vam on %s", ct)
	}

	// Mint succeeds on ClientTypeWeb and preserves stored Role alongside vam=true.
	access, refresh, _, err := svc.MintTokenPair(UserTokenSubject{
		UserID:            "admin-1",
		Email:             "admin@example.com",
		DisplayName:       "Admin",
		Role:              store.UserRoleAdmin,
		SessionGeneration: 2,
		ClientType:        ClientTypeWeb,
		ViewAsMember:      true,
	})
	require.NoError(t, err)

	accessClaims, err := svc.ValidateUserToken(access)
	require.NoError(t, err)
	assert.True(t, accessClaims.ViewAsMember)
	assert.Equal(t, store.UserRoleAdmin, accessClaims.Role)
	assert.Equal(t, ClientTypeWeb, accessClaims.ClientType)

	refreshClaims, err := svc.ValidateRefreshToken(refresh)
	require.NoError(t, err)
	assert.True(t, refreshClaims.ViewAsMember)
	assert.Equal(t, store.UserRoleAdmin, refreshClaims.Role)

	// W8: refreshHubTokens preserves vam from the refresh token.
	ws := NewWebServer(WebServerConfig{Host: "127.0.0.1"})
	ws.SetUserTokenService(svc)
	newAccess, newRefresh, _, err := ws.refreshHubTokens(context.Background(), refresh)
	require.NoError(t, err)
	newAccessClaims, err := svc.ValidateUserToken(newAccess)
	require.NoError(t, err)
	assert.True(t, newAccessClaims.ViewAsMember)
	newRefreshClaims, err := svc.ValidateRefreshToken(newRefresh)
	require.NoError(t, err)
	assert.True(t, newRefreshClaims.ViewAsMember)

	// UnifiedAuthMiddleware refuses a signed non-web token carrying vam=true with 401.
	now := time.Now()
	forgedCLI, err := svc.signClaims(UserTokenClaims{
		Claims: jwt.Claims{
			Issuer:    UserTokenIssuer,
			Subject:   "admin-1",
			Audience:  jwt.Audience{UserTokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			Expiry:    jwt.NewNumericDate(now.Add(15 * time.Minute)),
			NotBefore: jwt.NewNumericDate(now),
			ID:        "forged-jti",
		},
		UserID:       "admin-1",
		Email:        "admin@example.com",
		Role:         store.UserRoleAdmin,
		TokenType:    TokenTypeCLI,
		ClientType:   ClientTypeCLI,
		ViewAsMember: true,
	}, now)
	require.NoError(t, err)

	mw := UnifiedAuthMiddleware(AuthConfig{UserTokenSvc: svc})
	nextCalled := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+forgedCLI)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, nextCalled)

	// Bridge consistency check: cookie vam=true heals a stale vam=false access token,
	// and cookie-overflow per-request mint (W7) carries vam=true.
	staleAccess, staleRefresh, _, err := svc.MintTokenPair(UserTokenSubject{
		UserID:       "admin-1",
		Email:        "admin@example.com",
		DisplayName:  "Admin",
		Role:         store.UserRoleAdmin,
		ClientType:   ClientTypeWeb,
		ViewAsMember: false,
	})
	require.NoError(t, err)

	makeCookie := func(withTokens bool, vam bool) []*http.Cookie {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		sess, _ := ws.sessionStore.New(r, webSessionName)
		sess.Values[sessKeyUserID] = "admin-1"
		sess.Values[sessKeyUserEmail] = "admin@example.com"
		sess.Values[sessKeyUserName] = "Admin"
		sess.Values[sessKeyUserRole] = store.UserRoleAdmin
		if vam {
			sess.Values[sessKeyViewAsMember] = true
		}
		if withTokens {
			sess.Values[sessKeyHubAccessToken] = staleAccess
			sess.Values[sessKeyHubRefreshToken] = staleRefresh
			sess.Values[sessKeyHubTokenExpiry] = time.Now().Add(10 * time.Minute).UnixMilli()
		}
		require.NoError(t, sess.Save(r, w))
		return w.Result().Cookies()
	}

	var forwardedAuth string
	bridge := ws.sessionToBearerMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))

	// Case 1: mismatch (cookie vam=true, token vam=false) heals to vam=true.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	for _, c := range makeCookie(true, true) {
		req.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	bridge.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, strings.HasPrefix(forwardedAuth, "Bearer "))
	healedClaims, err := svc.ValidateUserToken(strings.TrimPrefix(forwardedAuth, "Bearer "))
	require.NoError(t, err)
	assert.True(t, healedClaims.ViewAsMember, "bridge must heal mismatched token to cookie vam=true")

	// Case 2: cookie overflow (no token in cookie) mints per-request token with vam=true.
	forwardedAuth = ""
	req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	for _, c := range makeCookie(false, true) {
		req.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	bridge.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, strings.HasPrefix(forwardedAuth, "Bearer "))
	overflowClaims, err := svc.ValidateUserToken(strings.TrimPrefix(forwardedAuth, "Bearer "))
	require.NoError(t, err)
	assert.True(t, overflowClaims.ViewAsMember, "cookie-overflow mint must carry vam=true")
}

// TestViewAsMember_ClampAndNoEscalation tests binding clamping, D2 synthesis,
// other-principal isolation, and Invariant 1 (authority(with vam) ⊆ authority(without vam))
// across all built-in system roles.
func TestViewAsMember_ClampAndNoEscalation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a super-admin who has ONLY super-admin (no hub-member binding)
	// plus project-member on project A.
	adminA := &store.User{
		ID:          api.NewUUID(),
		Email:       "admin-a@example.com",
		DisplayName: "Admin A",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, adminA))
	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminA.ID,
		RoleDefinitionID: superAdminRD.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	// Second admin to verify other-principal reads are unaffected.
	adminB := &store.User{
		ID:          api.NewUUID(),
		Email:       "admin-b@example.com",
		DisplayName: "Admin B",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, adminB))
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminB.ID,
		RoleDefinitionID: superAdminRD.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	// Two projects: adminA is project-member of projA only.
	projA := &store.Project{ID: api.NewUUID(), Name: "Project A", Slug: "vam-proj-a", OwnerID: adminA.ID}
	projB := &store.Project{ID: api.NewUUID(), Name: "Project B", Slug: "vam-proj-b", OwnerID: adminB.ID}
	require.NoError(t, s.CreateProject(ctx, projA))
	require.NoError(t, s.CreateProject(ctx, projB))
	projMemberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminA.ID,
		RoleDefinitionID: projMemberRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projA.ID,
	})
	require.NoError(t, err)

	vamCtx := contextWithViewAsMember(ctx, adminA.ID)

	// Unclamped: adminA is system admin and sees ScopeSetAll.
	assert.True(t, srv.authzService.IsSystemAdmin(ctx, adminA.ID))
	scopesUnclamped, err := srv.authzService.ResolveListScopes(ctx, NewAuthenticatedUser(adminA.ID, adminA.Email, adminA.DisplayName, store.UserRoleAdmin, "web"), "project.list")
	require.NoError(t, err)
	assert.True(t, scopesUnclamped.Scopes.IsAll(), "unclamped admin should get ScopeSetAll")

	// Clamped: adminA is NOT system/hub admin; other adminB IS still system admin.
	assert.False(t, srv.authzService.IsSystemAdmin(vamCtx, adminA.ID))
	assert.False(t, srv.authzService.IsHubAdmin(vamCtx, adminA.ID))
	assert.True(t, srv.authzService.IsSystemAdmin(vamCtx, adminB.ID), "other principal reads must not be clamped")

	// Clamped ResolveListScopes returns only projA (not ScopeSetAll, not projB).
	vamUser := NewAuthenticatedUser(adminA.ID, adminA.Email, adminA.DisplayName, store.UserRoleAdmin, "web").WithViewAsMember()
	scopesClamped, err := srv.authzService.ResolveListScopes(vamCtx, vamUser, "project.list")
	require.NoError(t, err)
	assert.False(t, scopesClamped.Scopes.IsAll(), "clamped admin must not get ScopeSetAll")
	assert.True(t, scopesClamped.Scopes.Contains(projA.ID), "clamped admin keeps own project")
	assert.False(t, scopesClamped.Scopes.Contains(projB.ID), "clamped admin must not see other project")

	// D2 synthesis: adminA (who had no hub-member binding) gets synthesized hub-member permissions
	// like project.create, while admin permissions like hub.admin_mode.read are denied.
	clampedPerms, err := srv.authzService.getEffectivePermissions(vamCtx, store.RoleBindingPrincipalUser, adminA.ID, store.RoleScopeSystem, "")
	require.NoError(t, err)
	assert.Contains(t, clampedPerms, "project.create", "D2 must synthesize hub-member for super-admin without hub-member binding")
	assert.NotContains(t, clampedPerms, "hub.admin_mode.read", "admin permission must be dropped")

	// Invariant 1 (No escalation): for every built-in system role,
	// effectivePermissions(with vam) ⊆ effectivePermissions(without vam).
	builtinSystemRoles := []string{
		store.SystemRoleSuperAdmin,
		store.SystemRoleHubAdmin,
		store.SystemRoleGlobalCatalogAuthor,
		store.SystemRoleHubMember,
		store.SystemRoleHubViewer,
	}
	for _, roleName := range builtinSystemRoles {
		t.Run("no_escalation_"+roleName, func(t *testing.T) {
			uid := api.NewUUID()
			roleStr := store.UserRoleMember
			if roleName == store.SystemRoleSuperAdmin {
				roleStr = store.UserRoleAdmin
			}
			require.NoError(t, s.CreateUser(ctx, &store.User{
				ID:          uid,
				Email:       "vam-role-" + roleName + "@example.com",
				DisplayName: roleName,
				Role:        roleStr,
				Status:      store.UserStatusActive,
			}))
			rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
			require.NoError(t, err)
			_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
				ID:               api.NewUUID(),
				PrincipalType:    store.RoleBindingPrincipalUser,
				PrincipalID:      uid,
				RoleDefinitionID: rd.ID,
				ScopeType:        store.RoleScopeSystem,
				CreatedBy:        store.SystemReconcileCreatedBy,
			})
			require.NoError(t, err)

			withoutVam, err := srv.authzService.getEffectivePermissions(ctx, store.RoleBindingPrincipalUser, uid, store.RoleScopeSystem, "")
			require.NoError(t, err)
			withVam, err := srv.authzService.getEffectivePermissions(contextWithViewAsMember(ctx, uid), store.RoleBindingPrincipalUser, uid, store.RoleScopeSystem, "")
			require.NoError(t, err)

			for _, p := range withVam {
				assert.True(t, slices.Contains(withoutVam, p),
					"role %s: vam granted permission %q not held without vam", roleName, p)
			}
		})
	}
	// Reference permissions package to ensure registry is reachable.
	assert.NotEmpty(t, permissions.Registry)
}

// TestViewAsMember_EndToEnd_WebToHub exercises the full vertical slice across
// WebServer -> scion_sess -> sessionToBearerMiddleware -> Hub Server:
//   - Toggle endpoint gates (experiment off -> 404 enter / 200 exit, member -> 403,
//     CSRF/Content-Type/method checks, idempotent re-enter/re-exit without duplicate audit)
//   - Transition mutation audit records (enter, exit, sign_out)
//   - Enter -> legacy requireAdmin route 403, meta.Permission route 403,
//     admin-status isAdmin=false/viewAsMember=true, project list scoped to member projects,
//     /auth/me & /api/v1/auth/me role="member" + viewAsMember=true
//   - Concurrent CLI JWT, UAT, and second browser session unaffected
//   - Exit -> admin routes 200 and admin-status restored
func TestViewAsMember_EndToEnd_WebToHub(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ws := NewWebServer(WebServerConfig{
		Host:          "127.0.0.1",
		SessionSecret: "test-session-secret-32-bytes-ok!",
	})
	ws.SetStore(s)
	ws.SetUserTokenService(srv.GetUserTokenService())
	ws.SetAuthzService(srv.GetAuthzService())
	ws.SetViewAsMemberHub(srv)
	ws.MountHubAPI(srv.Handler(), nil)

	ts := httptest.NewServer(ws.Handler())
	t.Cleanup(ts.Close)

	// Seed users: an admin and an ordinary member.
	adminUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "e2e-admin@example.com",
		DisplayName: "E2E Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}
	memberUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "e2e-member@example.com",
		DisplayName: "E2E Member",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, adminUser))
	require.NoError(t, s.CreateUser(ctx, memberUser))

	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminUser.ID,
		RoleDefinitionID: superAdminRD.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
	hubMemberRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      memberUser.ID,
		RoleDefinitionID: hubMemberRD.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	require.NoError(t, err)

	// Two projects: admin owns P1 (and is project-owner), member owns P2.
	proj1 := &store.Project{ID: api.NewUUID(), Name: "Admin Own Project", Slug: "e2e-proj-1", OwnerID: adminUser.ID}
	proj2 := &store.Project{ID: api.NewUUID(), Name: "Other Project", Slug: "e2e-proj-2", OwnerID: memberUser.ID}
	require.NoError(t, s.CreateProject(ctx, proj1))
	require.NoError(t, s.CreateProject(ctx, proj2))
	projOwnerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminUser.ID,
		RoleDefinitionID: projOwnerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj1.ID,
	})
	require.NoError(t, err)

	// Helper to build an authenticated browser client with a scion_sess cookie.
	newBrowserClient := func(u *store.User) *http.Client {
		t.Helper()
		access, refresh, expiresIn, err := srv.GetUserTokenService().MintTokenPair(UserTokenSubject{
			UserID:            u.ID,
			Email:             u.Email,
			DisplayName:       u.DisplayName,
			Role:              u.Role,
			SessionGeneration: u.SessionGeneration,
			ClientType:        ClientTypeWeb,
		})
		require.NoError(t, err)

		r := httptest.NewRequest(http.MethodGet, ts.URL+"/", nil)
		w := httptest.NewRecorder()
		sess, _ := ws.sessionStore.New(r, webSessionName)
		sess.Values[sessKeyUserID] = u.ID
		sess.Values[sessKeyUserEmail] = u.Email
		sess.Values[sessKeyUserName] = u.DisplayName
		sess.Values[sessKeyUserRole] = u.Role
		sess.Values[sessKeySessionGeneration] = u.SessionGeneration
		sess.Values[sessKeyHubAccessToken] = access
		sess.Values[sessKeyHubRefreshToken] = refresh
		sess.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
		require.NoError(t, sess.Save(r, w))

		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		uURL := mustParseURL(t, ts.URL)
		jar.SetCookies(uURL, w.Result().Cookies())
		return &http.Client{Jar: jar}
	}

	postToggle := func(client *http.Client, body string, headers map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/auth/view-as-member", bytes.NewBufferString(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		return resp
	}

	adminClient := newBrowserClient(adminUser)
	adminSecondSession := newBrowserClient(adminUser)
	memberClient := newBrowserClient(memberUser)

	// 1. Guard checks: unauthenticated -> 401, cross-site -> 403, experiment off -> 404.
	resp := postToggle(&http.Client{}, `{"enabled":true}`, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	resp = postToggle(adminClient, `{"enabled":true}`, map[string]string{"Sec-Fetch-Site": "cross-site"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// Experiment is off by default: enter -> 404.
	resp = postToggle(adminClient, `{"enabled":true}`, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()

	// Enable experiment.
	enableViewAsMemberExperiment(t, srv, true)

	// Member cannot enter -> 403.
	resp = postToggle(memberClient, `{"enabled":true}`, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// 2. Baseline (normal admin mode): admin routes 200, admin-status isAdmin=true, projects includes P1 and P2.
	getJSON := func(client *http.Client, path string, bearer string, dst any) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		require.NoError(t, err)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		r, err := client.Do(req)
		require.NoError(t, err)
		defer r.Body.Close()
		if dst != nil && r.StatusCode == http.StatusOK {
			require.NoError(t, json.NewDecoder(r.Body).Decode(dst))
		}
		return r.StatusCode
	}

	var statusBefore AdminStatusResponse
	assert.Equal(t, http.StatusOK, getJSON(adminClient, "/api/v1/auth/admin-status", "", &statusBefore))
	assert.True(t, statusBefore.IsAdmin)
	assert.True(t, statusBefore.IsSuperAdmin)
	assert.False(t, statusBefore.ViewAsMember)

	// Legacy requireAdmin route & meta.Permission route both return 200 before toggle.
	assert.Equal(t, http.StatusOK, getJSON(adminClient, "/api/v1/admin/delegation-adoption", "", nil))
	assert.Equal(t, http.StatusOK, getJSON(adminClient, "/api/v1/admin/maintenance", "", nil))

	var listBefore struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	assert.Equal(t, http.StatusOK, getJSON(adminClient, "/api/v1/projects", "", &listBefore))
	assert.Len(t, listBefore.Projects, 2, "normal admin sees both projects")

	// 3. Enter View-as-member mode.
	resp = postToggle(adminClient, `{"enabled":true}`, map[string]string{"Sec-Fetch-Site": "same-origin"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var toggleResp viewAsMemberResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&toggleResp))
	resp.Body.Close()
	assert.True(t, toggleResp.ViewAsMember)

	// Idempotent re-enter -> 200 with no second audit record.
	resp = postToggle(adminClient, `{"enabled":true}`, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	enterAudits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: viewAsMemberAuditEnter})
	require.NoError(t, err)
	require.Len(t, enterAudits, 1, "idempotent re-enter must not write a duplicate audit record")
	assert.Equal(t, adminUser.ID, enterAudits[0].TargetID)
	assert.Equal(t, adminUser.ID, enterAudits[0].ActorPrincipalID)
	assert.Equal(t, "user", enterAudits[0].ActorPrincipalKind)
	assert.Equal(t, "interactive", enterAudits[0].ActorCredentialType)
	assert.Equal(t, viewAsMemberSessionMode, enterAudits[0].SessionMode, "enter transition record must carry session_mode=view_as_member")
	assert.JSONEq(t, `{"view_as_member":true}`, enterAudits[0].AfterSummary)

	// 4. Verify clamped behavior in View-as-member mode.
	var webMe webSessionUser
	assert.Equal(t, http.StatusOK, getJSON(adminClient, "/auth/me", "", &webMe))
	assert.Equal(t, adminUser.ID, webMe.UserID)
	assert.Equal(t, store.UserRoleMember, webMe.Role)
	assert.True(t, webMe.ViewAsMember)

	var apiMe UserResponse
	assert.Equal(t, http.StatusOK, getJSON(adminClient, "/api/v1/auth/me", "", &apiMe))
	assert.Equal(t, adminUser.ID, apiMe.ID, "Invariant 2: principal stays caller's own ID")
	assert.Equal(t, store.UserRoleMember, apiMe.Role)
	assert.True(t, apiMe.ViewAsMember)

	var statusClamped AdminStatusResponse
	assert.Equal(t, http.StatusOK, getJSON(adminClient, "/api/v1/auth/admin-status", "", &statusClamped))
	assert.False(t, statusClamped.IsAdmin)
	assert.False(t, statusClamped.IsSuperAdmin)
	assert.True(t, statusClamped.ViewAsMember)

	// Both legacy requireAdmin route and meta.Permission route return 403!
	assert.Equal(t, http.StatusForbidden, getJSON(adminClient, "/api/v1/admin/delegation-adoption", "", nil),
		"legacy requireAdmin route must return 403 in view-as-member mode")
	assert.Equal(t, http.StatusForbidden, getJSON(adminClient, "/api/v1/admin/maintenance", "", nil),
		"meta.Permission RouteHubAdmin route must return 403 in view-as-member mode")

	// Project list returns ONLY proj1 (not proj2).
	var listClamped struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	assert.Equal(t, http.StatusOK, getJSON(adminClient, "/api/v1/projects", "", &listClamped))
	require.Len(t, listClamped.Projects, 1, "clamped admin sees only own project")
	assert.Equal(t, proj1.ID, listClamped.Projects[0].ID)

	// 5. Invariant 4: CLI JWT, UAT, and second browser session are unaffected!
	cliAccess, _, _, err := srv.GetUserTokenService().MintTokenPair(UserTokenSubject{
		UserID:            adminUser.ID,
		Email:             adminUser.Email,
		DisplayName:       adminUser.DisplayName,
		Role:              adminUser.Role,
		SessionGeneration: adminUser.SessionGeneration,
		ClientType:        ClientTypeCLI,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, getJSON(&http.Client{}, "/api/v1/admin/delegation-adoption", cliAccess, nil),
		"CLI JWT must be unaffected by browser view-as-member")
	assert.Equal(t, http.StatusOK, getJSON(&http.Client{}, "/api/v1/admin/maintenance", cliAccess, nil),
		"CLI JWT must be unaffected by browser view-as-member")

	// UAT for the same admin is unaffected.
	adminIdent := NewAuthenticatedUser(adminUser.ID, adminUser.Email, adminUser.DisplayName, adminUser.Role, string(ClientTypeWeb))
	uatCtx := contextWithCredentialContext(contextWithIdentity(ctx, adminIdent), credentialContextForIdentity(adminIdent))
	uatToken, _, err := srv.uatService.CreateToken(uatCtx, adminUser.ID, "e2e-uat", proj1.ID, []string{"project:read"}, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, getJSON(&http.Client{}, "/api/v1/projects/"+proj1.ID, uatToken, nil),
		"UAT must be unaffected by browser view-as-member")

	// Second browser session of the same admin is unaffected.
	var secondStatus AdminStatusResponse
	assert.Equal(t, http.StatusOK, getJSON(adminSecondSession, "/api/v1/auth/admin-status", "", &secondStatus))
	assert.True(t, secondStatus.IsAdmin)
	assert.False(t, secondStatus.ViewAsMember)
	assert.Equal(t, http.StatusOK, getJSON(adminSecondSession, "/api/v1/admin/maintenance", "", nil))

	// 6. Turning the experiment OFF while in the mode: session stays clamped, and EXIT still works (200)!
	enableViewAsMemberExperiment(t, srv, false)
	assert.Equal(t, http.StatusForbidden, getJSON(adminClient, "/api/v1/admin/maintenance", "", nil),
		"turning experiment off must not silently restore admin authority")

	resp = postToggle(adminClient, `{"enabled":false}`, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "exit must return 200 even with experiment off")
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&toggleResp))
	resp.Body.Close()
	assert.False(t, toggleResp.ViewAsMember)

	// Exit audit written.
	exitAudits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: viewAsMemberAuditExit})
	require.NoError(t, err)
	require.Len(t, exitAudits, 1)
	assert.Empty(t, exitAudits[0].SessionMode, "exit transition record must leave session_mode empty")
	assert.JSONEq(t, `{"view_as_member":false,"reason":"user"}`, exitAudits[0].AfterSummary)

	// 7. After exit, admin routes return 200 again!
	assert.Equal(t, http.StatusOK, getJSON(adminClient, "/api/v1/admin/delegation-adoption", "", nil))
	assert.Equal(t, http.StatusOK, getJSON(adminClient, "/api/v1/admin/maintenance", "", nil))
	var statusAfter AdminStatusResponse
	assert.Equal(t, http.StatusOK, getJSON(adminClient, "/api/v1/auth/admin-status", "", &statusAfter))
	assert.True(t, statusAfter.IsAdmin)
	assert.True(t, statusAfter.IsSuperAdmin)
	assert.False(t, statusAfter.ViewAsMember)

	// 8. Sign-out while in view-as-member writes exit audit with reason=sign_out.
	enableViewAsMemberExperiment(t, srv, true)
	resp = postToggle(adminClient, `{"enabled":true}`, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	logoutReq, err := http.NewRequest(http.MethodPost, ts.URL+"/auth/logout", nil)
	require.NoError(t, err)
	logoutResp, err := adminClient.Do(logoutReq)
	require.NoError(t, err)
	logoutResp.Body.Close()
	assert.Equal(t, http.StatusOK, logoutResp.StatusCode)

	exitAudits, _, err = s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: viewAsMemberAuditExit})
	require.NoError(t, err)
	require.Len(t, exitAudits, 2)
	reasons := []string{exitAudits[0].AfterSummary, exitAudits[1].AfterSummary}
	assert.Contains(t, reasons, `{"view_as_member":false,"reason":"sign_out"}`)
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// TestViewAsMember_AllRouteHubAdminReturn403 enforces design §6 invariant 3 and
// §11 item 4: with view-as-member on, every admin-only RouteHubAdmin route in
// routeMetadataTable returns 403 and never invokes the underlying handler,
// while the same super-admin without view-as-member is admitted (200).
// The handful of RouteHubAdmin routes whose routeGuard declares a read-only
// permission intentionally held by hub-member (currently only quota.read and
// role.read — handlers enforce quota.manage/role.manage on mutations) are
// pinned to that exact set, verified for exact parity with a real hub-member,
// and checked that write mutations return 403.
func TestViewAsMember_AllRouteHubAdminReturn403(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	adminUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "vam-all-admin-routes@example.com",
		DisplayName: "All Admin Routes Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, adminUser))
	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminUser.ID,
		RoleDefinitionID: superAdminRD.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	// Real hub-member control user to verify exact parity on every RouteHubAdmin route.
	memberUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "vam-all-admin-routes-member@example.com",
		DisplayName: "All Admin Routes Member",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, memberUser))
	hubMemberRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      memberUser.ID,
		RoleDefinitionID: hubMemberRD.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	require.NoError(t, err)

	normalToken, _, _, err := srv.GetUserTokenService().MintTokenPair(UserTokenSubject{
		UserID:            adminUser.ID,
		Email:             adminUser.Email,
		DisplayName:       adminUser.DisplayName,
		Role:              adminUser.Role,
		SessionGeneration: adminUser.SessionGeneration,
		ClientType:        ClientTypeWeb,
		ViewAsMember:      false,
	})
	require.NoError(t, err)

	vamToken, _, _, err := srv.GetUserTokenService().MintTokenPair(UserTokenSubject{
		UserID:            adminUser.ID,
		Email:             adminUser.Email,
		DisplayName:       adminUser.DisplayName,
		Role:              adminUser.Role,
		SessionGeneration: adminUser.SessionGeneration,
		ClientType:        ClientTypeWeb,
		ViewAsMember:      true,
	})
	require.NoError(t, err)

	memberToken, _, _, err := srv.GetUserTokenService().MintTokenPair(UserTokenSubject{
		UserID:            memberUser.ID,
		Email:             memberUser.Email,
		DisplayName:       memberUser.DisplayName,
		Role:              memberUser.Role,
		SessionGeneration: memberUser.SessionGeneration,
		ClientType:        ClientTypeWeb,
		ViewAsMember:      false,
	})
	require.NoError(t, err)

	authMW := UnifiedAuthMiddleware(srv.authConfig)

	// Pinned set of RouteHubAdmin route patterns whose routeGuard declares a
	// read-only permission intentionally held by hub-member (quota.read or
	// role.read; see hubMemberPermissionIDs in seed.go) and whose handlers
	// enforce manage permissions on writes. Keyed by route pattern (not
	// permission) so any new member-readable RouteHubAdmin route fails until
	// explicitly reviewed and decided.
	hubMemberRouteAdminReadRoutes := map[string]bool{
		"/api/v1/admin/entitlements/": true,
		"/api/v1/admin/limits":        true,
		"/api/v1/admin/limits/":       true,
		"/api/v1/admin/permissions":   true,
		"/api/v1/admin/roles":         true,
		"/api/v1/admin/roles/":        true,
		"/api/v1/admin/roles/export":  true,
		"/api/v1/admin/roles/import":  true,
		"/api/v1/admin/usage":         true,
		"/api/v1/admin/usage/":        true,
	}

	var patterns []string
	for pattern, meta := range routeMetadataTable {
		if meta.Classification == RouteHubAdmin {
			patterns = append(patterns, pattern)
		}
	}
	slices.Sort(patterns)
	require.NotEmpty(t, patterns, "routeMetadataTable must contain RouteHubAdmin routes")
	for pattern := range hubMemberRouteAdminReadRoutes {
		meta, ok := routeMetadataTable[pattern]
		assert.True(t, ok && meta.Classification == RouteHubAdmin,
			"stale hubMemberRouteAdminReadRoutes entry %q: not a RouteHubAdmin route", pattern)
	}

	deniedCount := 0
	for _, pattern := range patterns {
		meta := routeMetadataTable[pattern]
		method := http.MethodGet
		reqPath := pattern
		if parts := strings.SplitN(pattern, " ", 2); len(parts) == 2 {
			method = parts[0]
			reqPath = parts[1]
		}
		// Replace Go 1.22 path wildcards like {id} with concrete segments.
		reqPath = strings.ReplaceAll(reqPath, "{id}", "00000000-0000-0000-0000-000000000001")
		reqPath = strings.ReplaceAll(reqPath, "{userId}", adminUser.ID)
		reqPath = strings.ReplaceAll(reqPath, "{name}", "test-item")
		reqPath = strings.ReplaceAll(reqPath, "{key}", "test-key")
		reqPath = strings.ReplaceAll(reqPath, "{section}", "access")
		if strings.Contains(reqPath, "{") {
			// Fallback for any future wildcard name.
			var b strings.Builder
			inBrace := false
			for _, ch := range reqPath {
				switch ch {
				case '{':
					inBrace = true
					b.WriteString("sample")
				case '}':
					inBrace = false
				default:
					if !inBrace {
						b.WriteRune(ch)
					}
				}
			}
			reqPath = b.String()
		}

		memberReadRoute := hubMemberRouteAdminReadRoutes[pattern]
		if !memberReadRoute {
			deniedCount++
		}

		t.Run(meta.RouteID+"_"+pattern, func(t *testing.T) {
			called := false
			guarded := authMW(srv.routeGuard(meta, func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))

			// Control: normal super-admin is admitted.
			reqNormal := httptest.NewRequest(method, reqPath, nil)
			reqNormal.Header.Set("Authorization", "Bearer "+normalToken)
			recNormal := httptest.NewRecorder()
			guarded.ServeHTTP(recNormal, reqNormal)
			assert.Equal(t, http.StatusOK, recNormal.Code, "control: unclamped super-admin should pass %s", pattern)
			assert.True(t, called, "control: handler should run for unclamped super-admin on %s", pattern)

			// Real hub-member control.
			called = false
			reqMem := httptest.NewRequest(method, reqPath, nil)
			reqMem.Header.Set("Authorization", "Bearer "+memberToken)
			recMem := httptest.NewRecorder()
			guarded.ServeHTTP(recMem, reqMem)
			memberCode := recMem.Code
			memberCalled := called

			// View-as-member: must match real hub-member exactly, and must return 403
			// on every admin-only RouteHubAdmin route.
			called = false
			reqVAM := httptest.NewRequest(method, reqPath, nil)
			reqVAM.Header.Set("Authorization", "Bearer "+vamToken)
			recVAM := httptest.NewRecorder()
			guarded.ServeHTTP(recVAM, reqVAM)

			assert.Equal(t, memberCode, recVAM.Code, "view-as-member status must match real hub-member on %s", pattern)
			assert.Equal(t, memberCalled, called, "view-as-member handler reachability must match real hub-member on %s", pattern)
			if !memberReadRoute {
				assert.Equal(t, http.StatusForbidden, recVAM.Code, "view-as-member must return 403 on %s", pattern)
				assert.False(t, called, "handler must not run for view-as-member on %s", pattern)
			}
		})
	}
	assert.Greater(t, deniedCount, 70, "must exercise all admin-only RouteHubAdmin routes")

	// Verify write mutations on the hub-member read-guarded quota/role routes
	// are denied with 403 in view-as-member mode by the full handler stack.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"create_role", http.MethodPost, "/api/v1/admin/roles", `{"name":"vam-test","scopeType":"system","permissions":["user.read"]}`},
		{"import_roles", http.MethodPost, "/api/v1/admin/roles/import", `{"roles":[]}`},
		{"create_limit", http.MethodPost, "/api/v1/admin/limits", `{"name":"vam-limit","resourceType":"agent","limitValue":1}`},
	} {
		t.Run("mutation_"+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+vamToken)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusForbidden, rec.Code, "view-as-member must return 403 on %s %s", tc.method, tc.path)
		})
	}
}

// TestViewAsMember_MaintenanceModeAdmitsAndAllowsExit verifies design §4.9 / D4:
// when the hub is in maintenance mode, a stored-admin browser session in
// view-as-member mode stays admitted by adminModeWebMiddleware and
// adminModeMiddleware (never 503), still evaluates downstream authorization at
// member level (403 on admin routes), and can always exit via
// POST /auth/view-as-member. Also verifies deferDuringMaintenance uses real
// stored admin status.
func TestViewAsMember_MaintenanceModeAdmitsAndAllowsExit(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	enableViewAsMemberExperiment(t, srv, true)

	// Share a single MaintenanceState between Hub Server and WebServer.
	maint := srv.GetMaintenanceState()
	require.NotNil(t, maint)

	ws := NewWebServer(WebServerConfig{
		Host:          "127.0.0.1",
		SessionSecret: "test-maint-session-secret-32byte",
	})
	ws.SetStore(s)
	ws.SetUserTokenService(srv.GetUserTokenService())
	ws.SetAuthzService(srv.GetAuthzService())
	ws.SetViewAsMemberHub(srv)
	ws.SetMaintenanceState(maint)
	ws.MountHubAPI(srv.Handler(), nil)

	ts := httptest.NewServer(ws.Handler())
	t.Cleanup(ts.Close)

	adminUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "maint-vam-admin@example.com",
		DisplayName: "Maint Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}
	memberUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "maint-vam-member@example.com",
		DisplayName: "Maint Member",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, adminUser))
	require.NoError(t, s.CreateUser(ctx, memberUser))

	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminUser.ID,
		RoleDefinitionID: superAdminRD.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	newClient := func(u *store.User) *http.Client {
		t.Helper()
		access, refresh, expiresIn, err := srv.GetUserTokenService().MintTokenPair(UserTokenSubject{
			UserID:            u.ID,
			Email:             u.Email,
			DisplayName:       u.DisplayName,
			Role:              u.Role,
			SessionGeneration: u.SessionGeneration,
			ClientType:        ClientTypeWeb,
		})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		sess, err := ws.sessionStore.New(req, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyUserID] = u.ID
		sess.Values[sessKeyUserEmail] = u.Email
		sess.Values[sessKeyUserName] = u.DisplayName
		sess.Values[sessKeyUserRole] = u.Role
		sess.Values[sessKeySessionGeneration] = u.SessionGeneration
		sess.Values[sessKeyHubAccessToken] = access
		sess.Values[sessKeyHubRefreshToken] = refresh
		sess.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
		require.NoError(t, sess.Save(req, rec))
		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		jar.SetCookies(mustParseURL(t, ts.URL), rec.Result().Cookies())
		return &http.Client{Jar: jar}
	}

	adminClient := newClient(adminUser)
	memberClient := newClient(memberUser)

	postToggle := func(c *http.Client, enabled bool) *http.Response {
		t.Helper()
		body := `{"enabled":false}`
		if enabled {
			body = `{"enabled":true}`
		}
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/auth/view-as-member", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := c.Do(req)
		require.NoError(t, err)
		return resp
	}

	// Admin enters view-as-member, then hub enters maintenance mode.
	resp := postToggle(adminClient, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	maint.Set(true, "Planned maintenance")

	// Control: ordinary member is blocked with 503 on both web and Hub API.
	memWebResp, err := memberClient.Get(ts.URL + "/profile")
	require.NoError(t, err)
	memWebResp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, memWebResp.StatusCode, "member web request must get 503 in maintenance")

	memAPIResp, err := memberClient.Get(ts.URL + "/api/v1/auth/me")
	require.NoError(t, err)
	memAPIResp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, memAPIResp.StatusCode, "member API request must get 503 in maintenance")

	// Admin in view-as-member stays admitted on web pages (not 503).
	adminWebResp, err := adminClient.Get(ts.URL + "/profile")
	require.NoError(t, err)
	adminWebResp.Body.Close()
	assert.NotEqual(t, http.StatusServiceUnavailable, adminWebResp.StatusCode, "admin in view-as-member must not get 503 on web routes during maintenance")

	// Admin in view-as-member stays admitted on non-admin Hub API (200) and still sees member view.
	apiMeResp, err := adminClient.Get(ts.URL + "/api/v1/auth/me")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, apiMeResp.StatusCode, "admin in view-as-member must be admitted by adminModeMiddleware")
	var meBody UserResponse
	require.NoError(t, json.NewDecoder(apiMeResp.Body).Decode(&meBody))
	apiMeResp.Body.Close()
	assert.Equal(t, store.UserRoleMember, meBody.Role)
	assert.True(t, meBody.ViewAsMember)

	// Admin in view-as-member is admitted past adminModeMiddleware on admin API routes,
	// and denied 403 (not 503) by the route guard.
	adminRouteResp, err := adminClient.Get(ts.URL + "/api/v1/admin/delegation-adoption")
	require.NoError(t, err)
	adminRouteResp.Body.Close()
	assert.Equal(t, http.StatusForbidden, adminRouteResp.StatusCode, "admin route must return 403 (not 503) in maintenance + view-as-member")

	// Exit via POST /auth/view-as-member succeeds during maintenance and restores full admin access.
	exitResp := postToggle(adminClient, false)
	require.Equal(t, http.StatusOK, exitResp.StatusCode, "exit must succeed during maintenance mode")
	var toggleResp viewAsMemberResponse
	require.NoError(t, json.NewDecoder(exitResp.Body).Decode(&toggleResp))
	exitResp.Body.Close()
	assert.False(t, toggleResp.ViewAsMember)

	restoredResp, err := adminClient.Get(ts.URL + "/api/v1/admin/delegation-adoption")
	require.NoError(t, err)
	restoredResp.Body.Close()
	assert.Equal(t, http.StatusOK, restoredResp.StatusCode, "after exit during maintenance, admin route must return 200")

	// deferDuringMaintenance also uses real (stored) admin status (D4).
	vamAdminIdent := NewAuthenticatedUser(adminUser.ID, adminUser.Email, adminUser.DisplayName, store.UserRoleAdmin, string(ClientTypeWeb)).WithViewAsMember()
	got := srv.deferDuringMaintenance(pendingSaverIdentity{out: saverApply, identity: vamAdminIdent}, vamAdminIdent)
	assert.Equal(t, saverApply, got.out, "deferDuringMaintenance must admit a stored admin in view-as-member")

	memberIdent := NewAuthenticatedUser(memberUser.ID, memberUser.Email, memberUser.DisplayName, store.UserRoleMember, string(ClientTypeWeb))
	gotMem := srv.deferDuringMaintenance(pendingSaverIdentity{out: saverApply, identity: memberIdent}, memberIdent)
	assert.Equal(t, saverDeferred, gotMem.out, "deferDuringMaintenance must defer a real member during maintenance")
}

// TestViewAsMember_RecheckDelegationAdoptionAndRebuilders verifies the DB-row
// short-circuit in recheckDelegationAdoptionAdmin and the
// applyViewAsMemberFromContext helper used by request-path identity rebuilders.
func TestViewAsMember_RecheckDelegationAdoptionAndRebuilders(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	adminUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "recheck-vam-admin@example.com",
		DisplayName: "Recheck Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, adminUser))

	actor := delegationAdoptionActor{UserID: adminUser.ID, CredentialKind: string(CredentialKindInteractive)}

	// Unclamped: recheckDelegationAdoptionAdmin admits the stored admin.
	require.NoError(t, srv.recheckDelegationAdoptionAdmin(ctx, s, actor))

	// With view-as-member on for the same user: short-circuit denies even when
	// called directly without the preceding route guard.
	vamCtx := contextWithViewAsMember(ctx, adminUser.ID)
	err := srv.recheckDelegationAdoptionAdmin(vamCtx, s, actor)
	assert.ErrorIs(t, err, errAdoptionPermissionLost, "recheckDelegationAdoptionAdmin must deny when caller is in view-as-member")

	// With view-as-member on for a DIFFERENT user: unaffected.
	otherVamCtx := contextWithViewAsMember(ctx, api.NewUUID())
	assert.NoError(t, srv.recheckDelegationAdoptionAdmin(otherVamCtx, s, actor))

	// applyViewAsMemberFromContext clamps only when ctx matches the rebuilt user's ID.
	rebuilt := NewAuthenticatedUser(adminUser.ID, adminUser.Email, adminUser.DisplayName, store.UserRoleAdmin, string(ClientTypeWeb))
	clamped := applyViewAsMemberFromContext(vamCtx, rebuilt)
	assert.True(t, clamped.ViewAsMember())
	assert.Equal(t, store.UserRoleMember, clamped.Role())
	assert.False(t, IsUnscopedLocalPlatformAdmin(clamped))

	unclampedOther := applyViewAsMemberFromContext(otherVamCtx, rebuilt)
	assert.False(t, unclampedOther.ViewAsMember())
	assert.Equal(t, store.UserRoleAdmin, unclampedOther.Role())
	assert.True(t, IsUnscopedLocalPlatformAdmin(unclampedOther))
}

// TestViewAsMember_ViewerRoleNotEscalated verifies that while view-as-member is
// active, a stored "viewer" role stays "viewer" (never escalated to "member")
// on both AuthenticatedUser.Role() and webSessionUserFromCookie.
func TestViewAsMember_ViewerRoleNotEscalated(t *testing.T) {
	cases := []struct {
		stored string
		want   string
	}{
		{store.UserRoleAdmin, store.UserRoleMember},
		{store.UserRoleMember, store.UserRoleMember},
		{store.UserRoleViewer, store.UserRoleViewer},
	}

	ws := NewWebServer(WebServerConfig{
		Host:          "127.0.0.1",
		SessionSecret: "test-session-secret-32-bytes-ok!",
	})

	for _, tc := range cases {
		t.Run("stored_"+tc.stored, func(t *testing.T) {
			au := NewAuthenticatedUser("u-1", "u@example.com", "U", tc.stored, string(ClientTypeWeb))
			assert.Equal(t, tc.stored, au.Role())
			assert.Equal(t, tc.want, au.WithViewAsMember().Role())

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			sess, err := ws.sessionStore.New(r, webSessionName)
			require.NoError(t, err)
			sess.Values[sessKeyUserID] = "u-1"
			sess.Values[sessKeyUserEmail] = "u@example.com"
			sess.Values[sessKeyUserName] = "U"
			sess.Values[sessKeyUserRole] = tc.stored

			normal := ws.webSessionUserFromCookie(sess, "u-1", "")
			assert.Equal(t, tc.stored, normal.Role)
			assert.False(t, normal.ViewAsMember)

			sess.Values[sessKeyViewAsMember] = true
			clamped := ws.webSessionUserFromCookie(sess, "u-1", "")
			assert.Equal(t, tc.want, clamped.Role)
			assert.True(t, clamped.ViewAsMember)
		})
	}
}

type vamMockOAuthTransport struct {
	tokenJSON    string
	userinfoJSON string
}

func (m *vamMockOAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := m.userinfoJSON
	if strings.Contains(req.URL.String(), "token") {
		body = m.tokenJSON
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

type vamStaticAccessSettings struct {
	adminEmails []string
}

func (s *vamStaticAccessSettings) AdminEmails() []string          { return s.adminEmails }
func (s *vamStaticAccessSettings) AuthorizedDomains() []string    { return nil }
func (s *vamStaticAccessSettings) UserAccessMode() string         { return "open" }
func (s *vamStaticAccessSettings) DefaultUserRole() string        { return store.UserRoleMember }
func (s *vamStaticAccessSettings) CustomDefaultRoleEnabled() bool { return false }

type vamMockProxyAuth struct {
	user *ProxyUserInfo
}

func (m *vamMockProxyAuth) Authenticate(*http.Request) (*ProxyUserInfo, error) { return m.user, nil }
func (m *vamMockProxyAuth) Name() string                                       { return "mock-iap" }
func (m *vamMockProxyAuth) AssertionHeaders() []string                         { return nil }

// TestViewAsMember_TerminationAndW6 covers:
//   - (a) W2 OAuth callback, W4 dev sign-in, and test-login on a cookie carrying
//     vam=true clear sessKeyViewAsMember and mint tokens with vam=false.
//   - (b) Admin enters view-as-member, is then demoted (role->member, super-admin
//     binding removed), and POST /auth/view-as-member {"enabled":false} returns 200.
//   - (c) W6 proxy role-reconcile re-mint preserves sessKeyViewAsMember=true and
//     mints tokens with vam=true.
func TestViewAsMember_TerminationAndW6(t *testing.T) {
	const sessionSecret = "test-session-secret-32-bytes-ok!"

	decodeSavedSession := func(t *testing.T, ws *WebServer, cookies []*http.Cookie) (vam bool, accessToken, refreshToken, role string) {
		t.Helper()
		var sessCookie *http.Cookie
		for _, c := range cookies {
			if c.Name == webSessionName {
				sessCookie = c
			}
		}
		require.NotNil(t, sessCookie, "expected %s cookie in response", webSessionName)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(sessCookie)
		sess, err := ws.sessionStore.Get(req, webSessionName)
		require.NoError(t, err)
		vam = sessionViewAsMember(sess)
		accessToken, _ = sess.Values[sessKeyHubAccessToken].(string)
		refreshToken, _ = sess.Values[sessKeyHubRefreshToken].(string)
		role, _ = sess.Values[sessKeyUserRole].(string)
		return vam, accessToken, refreshToken, role
	}

	t.Run("W2_OAuthCallbackClearsVam", func(t *testing.T) {
		srv, s := testServer(t)
		adminEmail := "vam-oauth-admin@example.com"
		ws := NewWebServer(WebServerConfig{
			Host:          "127.0.0.1",
			SessionSecret: sessionSecret,
			BaseURL:       "http://localhost:8080",
		})
		ws.SetStore(s)
		ws.SetUserTokenService(srv.GetUserTokenService())
		ws.SetAccessSettingsProvider(&vamStaticAccessSettings{adminEmails: []string{adminEmail}})
		ws.oauthService = NewOAuthService(OAuthConfig{
			Web: OAuthClientConfig{
				Google: OAuthProviderConfig{ClientID: "cid", ClientSecret: "csec"},
			},
		}, nil)
		ws.oauthService.httpClient = &http.Client{
			Transport: &vamMockOAuthTransport{
				tokenJSON:    `{"access_token":"mock-tok","token_type":"Bearer","expires_in":3600}`,
				userinfoJSON: `{"id":"google-vam-1","email":"` + adminEmail + `","verified_email":true,"name":"OAuth Admin"}`,
			},
		}

		// Pre-seed cookie with vam=true and valid OAuth state.
		rSetup := httptest.NewRequest(http.MethodGet, "/auth/login/google", nil)
		wSetup := httptest.NewRecorder()
		sess, err := ws.sessionStore.New(rSetup, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyOAuthState] = "oauth-state-vam"
		sess.Values[sessKeyViewAsMember] = true
		require.NoError(t, sess.Save(rSetup, wSetup))

		req := httptest.NewRequest(http.MethodGet, "/auth/callback/google?code=ok&state=oauth-state-vam", nil)
		for _, c := range wSetup.Result().Cookies() {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())

		vam, access, refresh, role := decodeSavedSession(t, ws, rec.Result().Cookies())
		assert.False(t, vam, "OAuth callback login must clear sessKeyViewAsMember")
		assert.Equal(t, store.UserRoleAdmin, role)
		require.NotEmpty(t, access)
		require.NotEmpty(t, refresh)
		ac, err := srv.GetUserTokenService().ValidateUserToken(access)
		require.NoError(t, err)
		assert.False(t, ac.ViewAsMember, "OAuth minted access token must have vam=false")
		rc, err := srv.GetUserTokenService().ValidateRefreshToken(refresh)
		require.NoError(t, err)
		assert.False(t, rc.ViewAsMember, "OAuth minted refresh token must have vam=false")
	})

	t.Run("W4_DevSignInClearsVam", func(t *testing.T) {
		srv, s := testServer(t)
		const devTok = "scion_dev_test_token_12345"
		ws := NewWebServer(WebServerConfig{
			Host:          "127.0.0.1",
			SessionSecret: sessionSecret,
			DevAuthToken:  devTok,
		})
		ws.SetStore(s)
		ws.SetUserTokenService(srv.GetUserTokenService())

		// Mint a dev sign-in code via POST /auth/dev-launch.
		launchReq := httptest.NewRequest(http.MethodPost, devLaunchPath, nil)
		launchReq.Header.Set("Authorization", "Bearer "+devTok)
		launchRec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(launchRec, launchReq)
		require.Equal(t, http.StatusOK, launchRec.Code, launchRec.Body.String())
		var launchResp struct {
			SignInPath string `json:"signInPath"`
		}
		require.NoError(t, json.Unmarshal(launchRec.Body.Bytes(), &launchResp))
		require.NotEmpty(t, launchResp.SignInPath)

		// Pre-seed cookie with vam=true, then drive dev sign-in on that same cookie.
		rSetup := httptest.NewRequest(http.MethodGet, "/", nil)
		wSetup := httptest.NewRecorder()
		sess, err := ws.sessionStore.New(rSetup, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyViewAsMember] = true
		require.NoError(t, sess.Save(rSetup, wSetup))

		signInReq := httptest.NewRequest(http.MethodGet, launchResp.SignInPath, nil)
		for _, c := range wSetup.Result().Cookies() {
			signInReq.AddCookie(c)
		}
		signInRec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(signInRec, signInReq)
		require.Equal(t, http.StatusSeeOther, signInRec.Code, signInRec.Body.String())

		vam, access, refresh, role := decodeSavedSession(t, ws, signInRec.Result().Cookies())
		assert.False(t, vam, "dev sign-in must clear sessKeyViewAsMember")
		assert.Equal(t, store.UserRoleAdmin, role)
		require.NotEmpty(t, access)
		require.NotEmpty(t, refresh)
		ac, err := srv.GetUserTokenService().ValidateUserToken(access)
		require.NoError(t, err)
		assert.False(t, ac.ViewAsMember, "dev sign-in access token must have vam=false")
		rc, err := srv.GetUserTokenService().ValidateRefreshToken(refresh)
		require.NoError(t, err)
		assert.False(t, rc.ViewAsMember, "dev sign-in refresh token must have vam=false")

		// Also verify populateDevSession directly clears vam on a reused session
		// object (defense-in-depth independent of handleDevSignIn's fresh map).
		dirtyReq := httptest.NewRequest(http.MethodGet, "/", nil)
		dirtySess, err := ws.sessionStore.New(dirtyReq, webSessionName)
		require.NoError(t, err)
		dirtySess.Values[sessKeyViewAsMember] = true
		ws.populateDevSession(context.Background(), dirtySess)
		assert.False(t, sessionViewAsMember(dirtySess), "populateDevSession must clear sessKeyViewAsMember")
	})

	t.Run("TestLogin_ClearsVam", func(t *testing.T) {
		srv, s := testServer(t)
		ws := NewWebServer(WebServerConfig{
			Host:            "127.0.0.1",
			SessionSecret:   sessionSecret,
			EnableTestLogin: true,
		})
		ws.SetStore(s)
		ws.SetUserTokenService(srv.GetUserTokenService())

		testLoginTok, err := srv.GetUserTokenService().GenerateTestLoginToken("test-suite")
		require.NoError(t, err)

		// Pre-seed cookie with vam=true.
		rSetup := httptest.NewRequest(http.MethodGet, "/", nil)
		wSetup := httptest.NewRecorder()
		sess, err := ws.sessionStore.New(rSetup, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyViewAsMember] = true
		require.NoError(t, sess.Save(rSetup, wSetup))

		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login",
			bytes.NewBufferString(`{"email":"testlogin-vam@example.com","role":"admin"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testLoginTok)
		for _, c := range wSetup.Result().Cookies() {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		vam, access, refresh, role := decodeSavedSession(t, ws, rec.Result().Cookies())
		assert.False(t, vam, "test-login must clear sessKeyViewAsMember")
		assert.Equal(t, store.UserRoleAdmin, role)
		require.NotEmpty(t, access)
		require.NotEmpty(t, refresh)
		ac, err := srv.GetUserTokenService().ValidateUserToken(access)
		require.NoError(t, err)
		assert.False(t, ac.ViewAsMember, "test-login access token must have vam=false")
		rc, err := srv.GetUserTokenService().ValidateRefreshToken(refresh)
		require.NoError(t, err)
		assert.False(t, rc.ViewAsMember, "test-login refresh token must have vam=false")
	})

	t.Run("ExitAfterAdminDemotion", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		enableViewAsMemberExperiment(t, srv, true)

		ws := NewWebServer(WebServerConfig{
			Host:          "127.0.0.1",
			SessionSecret: sessionSecret,
		})
		ws.SetStore(s)
		ws.SetUserTokenService(srv.GetUserTokenService())
		ws.SetAuthzService(srv.GetAuthzService())
		ws.SetViewAsMemberHub(srv)

		admin := &store.User{
			ID:          api.NewUUID(),
			Email:       "demoted-admin@example.com",
			DisplayName: "Demoted Admin",
			Role:        store.UserRoleAdmin,
			Status:      store.UserStatusActive,
		}
		require.NoError(t, s.CreateUser(ctx, admin))
		superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		require.NoError(t, err)
		rbID := api.NewUUID()
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			ID:               rbID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      admin.ID,
			RoleDefinitionID: superAdminRD.ID,
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        store.SystemReconcileCreatedBy,
		})
		require.NoError(t, err)

		// Establish session cookie for admin.
		rSetup := httptest.NewRequest(http.MethodGet, "/", nil)
		wSetup := httptest.NewRecorder()
		sess, err := ws.sessionStore.New(rSetup, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyUserID] = admin.ID
		sess.Values[sessKeyUserEmail] = admin.Email
		sess.Values[sessKeyUserName] = admin.DisplayName
		sess.Values[sessKeyUserRole] = admin.Role
		sess.Values[sessKeySessionGeneration] = admin.SessionGeneration
		require.NoError(t, sess.Save(rSetup, wSetup))

		// Enter view-as-member.
		enterReq := httptest.NewRequest(http.MethodPost, "/auth/view-as-member", bytes.NewBufferString(`{"enabled":true}`))
		enterReq.Header.Set("Content-Type", "application/json")
		for _, c := range wSetup.Result().Cookies() {
			enterReq.AddCookie(c)
		}
		enterRec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(enterRec, enterReq)
		require.Equal(t, http.StatusOK, enterRec.Code, enterRec.Body.String())

		// Demote admin in the store: role -> member and remove super-admin binding.
		admin.Role = store.UserRoleMember
		require.NoError(t, s.UpdateUser(ctx, admin))
		require.NoError(t, s.DeleteRoleBinding(ctx, rbID))

		// Exit must still return 200 and clear sessKeyViewAsMember.
		exitReq := httptest.NewRequest(http.MethodPost, "/auth/view-as-member", bytes.NewBufferString(`{"enabled":false}`))
		exitReq.Header.Set("Content-Type", "application/json")
		for _, c := range enterRec.Result().Cookies() {
			exitReq.AddCookie(c)
		}
		exitRec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(exitRec, exitReq)
		require.Equal(t, http.StatusOK, exitRec.Code, exitRec.Body.String())

		var resp viewAsMemberResponse
		require.NoError(t, json.Unmarshal(exitRec.Body.Bytes(), &resp))
		assert.False(t, resp.ViewAsMember)
		vam, access, _, _ := decodeSavedSession(t, ws, exitRec.Result().Cookies())
		assert.False(t, vam, "exit after demotion must clear sessKeyViewAsMember")
		ac, err := srv.GetUserTokenService().ValidateUserToken(access)
		require.NoError(t, err)
		assert.False(t, ac.ViewAsMember)
	})

	t.Run("W6_ProxyRoleReconcilePreservesVam", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		u := &store.User{
			ID:          api.NewUUID(),
			Email:       "proxy-w6@example.com",
			DisplayName: "Proxy W6",
			Role:        store.UserRoleMember,
			Status:      store.UserStatusActive,
		}
		require.NoError(t, s.CreateUser(ctx, u))

		accessCfg := &vamStaticAccessSettings{adminEmails: []string{u.Email}}
		ws := NewWebServer(WebServerConfig{
			Host:               "127.0.0.1",
			SessionSecret:      sessionSecret,
			AuthMode:           "proxy",
			ProxyAuthenticator: &vamMockProxyAuth{user: &ProxyUserInfo{Subject: "sub-w6", Email: u.Email}},
		})
		ws.SetStore(s)
		ws.SetUserTokenService(srv.GetUserTokenService())
		ws.SetAccessSettingsProvider(accessCfg)

		// Pre-seed session cookie with role="member" (stale) and vam=true.
		// Because u.Email is in adminEmails, proxyAuthMiddleware reconciles
		// expectedRole="admin" != currentRole="member" and re-mints the token pair (W6).
		rSetup := httptest.NewRequest(http.MethodGet, "/", nil)
		wSetup := httptest.NewRecorder()
		sess, err := ws.sessionStore.New(rSetup, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyUserID] = u.ID
		sess.Values[sessKeyUserEmail] = u.Email
		sess.Values[sessKeyUserName] = u.DisplayName
		sess.Values[sessKeyUserRole] = store.UserRoleMember
		sess.Values[sessKeySessionGeneration] = u.SessionGeneration
		sess.Values[sessKeyViewAsMember] = true
		require.NoError(t, sess.Save(rSetup, wSetup))

		req := httptest.NewRequest(http.MethodGet, "/projects", nil)
		req.Header.Set("Accept", "text/html")
		for _, c := range wSetup.Result().Cookies() {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, req)

		vam, access, refresh, role := decodeSavedSession(t, ws, rec.Result().Cookies())
		assert.Equal(t, store.UserRoleAdmin, role, "role reconcile should update stored role to admin")
		assert.True(t, vam, "W6 role-reconcile must preserve sessKeyViewAsMember")
		require.NotEmpty(t, access)
		require.NotEmpty(t, refresh)
		ac, err := srv.GetUserTokenService().ValidateUserToken(access)
		require.NoError(t, err)
		assert.True(t, ac.ViewAsMember, "W6 re-minted access token must preserve vam=true")
		assert.Equal(t, store.UserRoleAdmin, ac.Role)
		rc, err := srv.GetUserTokenService().ValidateRefreshToken(refresh)
		require.NoError(t, err)
		assert.True(t, rc.ViewAsMember, "W6 re-minted refresh token must preserve vam=true")
	})
}

// TestViewAsMember_GroupBoundAdminClamped verifies that a user who obtains
// hub-level admin authority exclusively through a group binding (not a direct
// user binding — note super-admin is direct-user-only at the store layer, so
// hub-admin is the group-bindable system admin role) is fully clamped under
// view-as-member: IsSystemAdmin==false, IsHubAdmin==false, ResolveListScopes
// is not ScopeSetAll, and admin routes (/api/v1/admin/server-config/schema and
// /api/v1/admin/maintenance) return 403.
func TestViewAsMember_GroupBoundAdminClamped(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// User has stored role "member" and NO direct admin binding.
	groupAdmin := &store.User{
		ID:          api.NewUUID(),
		Email:       "group-admin@example.com",
		DisplayName: "Group HubAdmin",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, groupAdmin))

	// Create a group, add user to it, and bind hub-admin to the group.
	adminGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      "Platform Hub Admins",
		Slug:      "platform-hub-admins",
		GroupType: "explicit",
	}
	require.NoError(t, s.CreateGroup(ctx, adminGroup))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    adminGroup.ID,
		MemberType: store.RoleBindingPrincipalUser,
		MemberID:   groupAdmin.ID,
		Role:       "member",
		AddedAt:    time.Now().UTC(),
	}))
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      adminGroup.ID,
		RoleDefinitionID: hubAdminRD.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	require.NoError(t, err)

	// Unclamped: group-bound hub-admin has IsHubAdmin==true, ScopeSetAll, and 200 on hub-admin route.
	normalUser := NewAuthenticatedUser(groupAdmin.ID, groupAdmin.Email, groupAdmin.DisplayName, groupAdmin.Role, string(ClientTypeWeb))
	assert.True(t, srv.authzService.IsHubAdmin(ctx, groupAdmin.ID), "unclamped group hub-admin must be hub admin")
	scopesUnclamped, err := srv.authzService.ResolveListScopes(ctx, normalUser, "project.list")
	require.NoError(t, err)
	assert.True(t, scopesUnclamped.Scopes.IsAll(), "unclamped group hub-admin must get ScopeSetAll")

	normalTok, _, err := srv.GetUserTokenService().MintAccessToken(UserTokenSubject{
		UserID:            groupAdmin.ID,
		Email:             groupAdmin.Email,
		DisplayName:       groupAdmin.DisplayName,
		Role:              groupAdmin.Role,
		SessionGeneration: groupAdmin.SessionGeneration,
		ClientType:        ClientTypeWeb,
		ViewAsMember:      false,
	})
	require.NoError(t, err)
	reqNormal := httptest.NewRequest(http.MethodGet, "/api/v1/admin/server-config/schema", nil)
	reqNormal.Header.Set("Authorization", "Bearer "+normalTok)
	recNormal := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recNormal, reqNormal)
	assert.Equal(t, http.StatusOK, recNormal.Code, "unclamped group hub-admin should reach hub-admin route")

	// Clamped: IsSystemAdmin==false, IsHubAdmin==false, ResolveListScopes is not ScopeSetAll,
	// and both /api/v1/admin/server-config/schema and /api/v1/admin/maintenance return 403.
	vamCtx := contextWithViewAsMember(ctx, groupAdmin.ID)
	assert.False(t, srv.authzService.IsSystemAdmin(vamCtx, groupAdmin.ID), "clamped group admin must not be system admin")
	assert.False(t, srv.authzService.IsHubAdmin(vamCtx, groupAdmin.ID), "clamped group admin must not be hub admin")
	scopesClamped, err := srv.authzService.ResolveListScopes(vamCtx, normalUser.WithViewAsMember(), "project.list")
	require.NoError(t, err)
	assert.False(t, scopesClamped.Scopes.IsAll(), "clamped group admin must not get ScopeSetAll")

	vamTok, _, err := srv.GetUserTokenService().MintAccessToken(UserTokenSubject{
		UserID:            groupAdmin.ID,
		Email:             groupAdmin.Email,
		DisplayName:       groupAdmin.DisplayName,
		Role:              groupAdmin.Role,
		SessionGeneration: groupAdmin.SessionGeneration,
		ClientType:        ClientTypeWeb,
		ViewAsMember:      true,
	})
	require.NoError(t, err)
	for _, adminPath := range []string{"/api/v1/admin/server-config/schema", "/api/v1/admin/maintenance"} {
		reqVam := httptest.NewRequest(http.MethodGet, adminPath, nil)
		reqVam.Header.Set("Authorization", "Bearer "+vamTok)
		recVam := httptest.NewRecorder()
		srv.Handler().ServeHTTP(recVam, reqVam)
		assert.Equal(t, http.StatusForbidden, recVam.Code, "clamped group admin must get 403 on %s", adminPath)
	}
}

// TestViewAsMember_SSE_ConnectExpansionAndToggleReconnect verifies design §4.5
// and §11.5:
//   - Connect-time expansion in view-as-member mode: admin.* is denied (403)
//     and project.> expands only to member-visible projects.
//   - Toggling into or out of view-as-member mode with an open SSE stream
//     unconditionally sends `event: reconnect` with `{"reason":"session_mode"}`
//     and closes the stream (CloseAccessChanged), while another user's open
//     stream stays open.
func TestViewAsMember_SSE_ConnectExpansionAndToggleReconnect(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	enableViewAsMemberExperiment(t, srv, true)

	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	srv.SetEventPublisher(pub)

	ws := NewWebServer(WebServerConfig{
		Host:          "127.0.0.1",
		SessionSecret: "test-session-secret-32-bytes-ok!",
	})
	var sseLogMu sync.Mutex
	var sseLogBuf bytes.Buffer
	ws.log = slog.New(slog.NewJSONHandler(&vamLockedBuffer{mu: &sseLogMu, buf: &sseLogBuf}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ws.SetStore(s)
	ws.SetUserTokenService(srv.GetUserTokenService())
	ws.SetAuthzService(srv.GetAuthzService())
	ws.SetEventPublisher(pub)
	ws.SetViewAsMemberHub(srv)
	ws.MountHubAPI(srv.Handler(), nil)

	ts := httptest.NewServer(ws.Handler())
	t.Cleanup(ts.Close)

	adminUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "sse-admin@example.com",
		DisplayName: "SSE Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}
	otherUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "sse-other@example.com",
		DisplayName: "SSE Other",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, adminUser))
	require.NoError(t, s.CreateUser(ctx, otherUser))

	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminUser.ID,
		RoleDefinitionID: superAdminRD.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	proj1 := &store.Project{ID: api.NewUUID(), Name: "Admin Project", Slug: "sse-vam-proj-1", OwnerID: adminUser.ID}
	proj2 := &store.Project{ID: api.NewUUID(), Name: "Other Project", Slug: "sse-vam-proj-2", OwnerID: otherUser.ID}
	require.NoError(t, s.CreateProject(ctx, proj1))
	require.NoError(t, s.CreateProject(ctx, proj2))

	projOwnerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminUser.ID,
		RoleDefinitionID: projOwnerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj1.ID,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      otherUser.ID,
		RoleDefinitionID: projOwnerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj2.ID,
	})
	require.NoError(t, err)

	newBrowserClient := func(u *store.User) *http.Client {
		t.Helper()
		access, refresh, expiresIn, err := srv.GetUserTokenService().MintTokenPair(UserTokenSubject{
			UserID:            u.ID,
			Email:             u.Email,
			DisplayName:       u.DisplayName,
			Role:              u.Role,
			SessionGeneration: u.SessionGeneration,
			ClientType:        ClientTypeWeb,
		})
		require.NoError(t, err)

		r := httptest.NewRequest(http.MethodGet, ts.URL+"/", nil)
		w := httptest.NewRecorder()
		sess, _ := ws.sessionStore.New(r, webSessionName)
		sess.Values[sessKeyUserID] = u.ID
		sess.Values[sessKeyUserEmail] = u.Email
		sess.Values[sessKeyUserName] = u.DisplayName
		sess.Values[sessKeyUserRole] = u.Role
		sess.Values[sessKeySessionGeneration] = u.SessionGeneration
		sess.Values[sessKeyHubAccessToken] = access
		sess.Values[sessKeyHubRefreshToken] = refresh
		sess.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
		require.NoError(t, sess.Save(r, w))

		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		jar.SetCookies(mustParseURL(t, ts.URL), w.Result().Cookies())
		return &http.Client{Jar: jar}
	}

	postToggle := func(client *http.Client, enabled bool) {
		t.Helper()
		body := `{"enabled":false}`
		if enabled {
			body = `{"enabled":true}`
		}
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/auth/view-as-member", bytes.NewBufferString(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	adminClient := newBrowserClient(adminUser)
	otherClient := newBrowserClient(otherUser)

	// 1. Open SSE streams in normal mode: admin subscribes to admin.* and project.>,
	// other user subscribes to project.> (control stream that must stay open).
	openStream := func(client *http.Client, query string) (*http.Response, context.CancelFunc) {
		t.Helper()
		sctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(sctx, http.MethodGet, ts.URL+"/events?"+query, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		return resp, cancel
	}

	// Open admin stream in normal mode (admin.* is allowed).
	adminResp, adminCancel := openStream(adminClient, "sub=admin.*")
	defer adminCancel()
	require.Equal(t, http.StatusOK, adminResp.StatusCode)

	otherResp, otherCancel := openStream(otherClient, "sub=project.>")
	defer func() {
		otherCancel()
		otherResp.Body.Close()
	}()
	require.Equal(t, http.StatusOK, otherResp.StatusCode)

	// Toggle admin INTO view-as-member mode: admin's open stream must receive
	// `event: reconnect` with `{"reason":"session_mode"}` and close cleanly.
	adminStreamDone := make(chan string, 1)
	go func() {
		raw, _ := io.ReadAll(adminResp.Body)
		adminResp.Body.Close()
		adminStreamDone <- string(raw)
	}()

	postToggle(adminClient, true)

	select {
	case body := <-adminStreamDone:
		assert.Contains(t, body, "event: reconnect\ndata: {\"reason\":\"session_mode\"}\n\n",
			"open stream must receive session_mode reconnect event on toggle")
	case <-time.After(5 * time.Second):
		t.Fatal("open admin SSE stream did not close after entering view-as-member")
	}

	// Control stream for otherUser must still be open and receiving proj2 events.
	otherLineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(otherResp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, "control-alive") {
				otherLineCh <- line
				return
			}
		}
	}()
	pub.PublishRaw("project."+proj2.ID+".agent.updated", map[string]string{"marker": "control-alive"})
	select {
	case line := <-otherLineCh:
		assert.Contains(t, line, "control-alive")
	case <-time.After(5 * time.Second):
		t.Fatal("other user's control SSE stream should stay open when admin toggles")
	}

	// 2. Connect-time checks while in view-as-member mode:
	// 2a. admin.* is denied with 403.
	deniedResp, deniedCancel := openStream(adminClient, "sub=admin.*")
	deniedCancel()
	deniedBody, _ := io.ReadAll(deniedResp.Body)
	deniedResp.Body.Close()
	assert.Equal(t, http.StatusForbidden, deniedResp.StatusCode, "admin.* SSE must be denied in view-as-member")
	assert.Contains(t, string(deniedBody), "admin.*")

	// 2b. project.> expands ONLY to proj1 (member-visible), NOT proj2.
	vamResp, vamCancel := openStream(adminClient, "sub=project.>")
	defer vamCancel()
	require.Equal(t, http.StatusOK, vamResp.StatusCode)

	vamBodyCh := make(chan string, 1)
	go func() {
		raw, _ := io.ReadAll(vamResp.Body)
		vamResp.Body.Close()
		vamBodyCh <- string(raw)
	}()

	// Publish to proj2 (not visible to clamped admin) first, then proj1 (visible).
	pub.PublishRaw("project."+proj2.ID+".agent.updated", map[string]string{"marker": "proj2-secret"})
	pub.PublishRaw("project."+proj1.ID+".agent.updated", map[string]string{"marker": "proj1-visible"})

	// Wait briefly for events to be written before toggling exit.
	time.Sleep(100 * time.Millisecond)

	// 3. Toggle EXIT while vamResp is open: stream must receive session_mode reconnect and close.
	postToggle(adminClient, false)

	select {
	case body := <-vamBodyCh:
		assert.Contains(t, body, "proj1-visible", "project.> must include member-visible proj1")
		assert.NotContains(t, body, "proj2-secret", "project.> must NOT include non-member proj2 in view-as-member mode")
		assert.Contains(t, body, "event: reconnect\ndata: {\"reason\":\"session_mode\"}\n\n",
			"open stream must receive session_mode reconnect event on exit")
	case <-time.After(5 * time.Second):
		t.Fatal("open view-as-member SSE stream did not close after exiting view-as-member")
	}

	// Verify handleSSE actually recorded CloseAccessChanged on the closed streams.
	require.Eventually(t, func() bool {
		sseLogMu.Lock()
		defer sseLogMu.Unlock()
		return strings.Count(sseLogBuf.String(), `"reason":"`+string(eventstreams.CloseAccessChanged)+`"`) >= 2
	}, 5*time.Second, 10*time.Millisecond, "both toggled streams must close with CloseAccessChanged")
}

type vamLockedBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (b *vamLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// TestViewAsMember_AuditSessionModeMarking verifies design §4.6, §6.8, and §11.8:
//   - auditActorFromContext fills SessionMode from viewAsMemberUserFromContext,
//     and ApplyActor copies it under the same principal-match rule as credential fields.
//   - BuildDecisionAuditRecord fills SessionMode; mapDecisionEnvelope carries it on
//     EnvelopeV1.Credential and renders session_mode under schema_version 1.
//   - requestAuthAttrs adds session_mode=view_as_member when active.
//   - End-to-end: a mutation performed in view-as-member mode (creating a project)
//     records session_mode=view_as_member; one outside the mode leaves it empty.
func TestViewAsMember_AuditSessionModeMarking(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	enableViewAsMemberExperiment(t, srv, true)

	// --- Unit: auditActorFromContext & ApplyActor principal-match rule ---
	adminID := api.NewUUID()
	vamIdentity := NewAuthenticatedUser(adminID, "audit-admin@example.com", "Audit Admin", store.UserRoleAdmin, string(ClientTypeWeb)).WithViewAsMember()
	vamCtx := contextWithIdentity(ctx, vamIdentity)
	vamCtx = contextWithCredentialContext(vamCtx, credentialContextForIdentity(vamIdentity))
	vamCtx = contextWithViewAsMember(vamCtx, adminID)

	actor := auditActorFromContext(vamCtx)
	assert.Equal(t, viewAsMemberSessionMode, actor.SessionMode)

	// Unpreset record inherits SessionMode.
	rec1 := &store.MutationAuditRecord{}
	actor.ApplyActor(rec1)
	assert.Equal(t, viewAsMemberSessionMode, rec1.SessionMode)

	// Preset matching principal inherits SessionMode.
	rec2 := &store.MutationAuditRecord{ActorPrincipalKind: "user", ActorPrincipalID: adminID}
	actor.ApplyActor(rec2)
	assert.Equal(t, viewAsMemberSessionMode, rec2.SessionMode)

	// Preset DIFFERENT principal must NOT inherit SessionMode (same rule as credential fields).
	rec3 := &store.MutationAuditRecord{ActorPrincipalKind: "user", ActorPrincipalID: "other-user"}
	actor.ApplyActor(rec3)
	assert.Empty(t, rec3.SessionMode, "different preset principal must not inherit ambient session_mode")

	// Normal context produces empty SessionMode.
	normalIdentity := NewAuthenticatedUser(adminID, "audit-admin@example.com", "Audit Admin", store.UserRoleAdmin, string(ClientTypeWeb))
	normalCtx := contextWithCredentialContext(contextWithIdentity(ctx, normalIdentity), credentialContextForIdentity(normalIdentity))
	assert.Empty(t, auditActorFromContext(normalCtx).SessionMode)

	// --- Unit: BuildDecisionAuditRecord & auditevent.EnvelopeV1 Credential block ---
	decisionReq := AuthzRequest{
		Principal:  principalContextForIdentity(vamIdentity),
		Credential: credentialContextForIdentity(vamIdentity),
		Resource:   Resource{Type: "project", ID: "proj-1"},
		Action:     ActionRead,
		Permission: "project.read",
	}
	decision := Decision{
		Allowed:            false,
		Reason:             "denied in view-as-member",
		PrincipalKind:      PrincipalKindUser,
		PrincipalID:        adminID,
		principalDecorated: true,
		CredentialKind:     string(CredentialKindInteractive),
		PermissionID:       "project.read",
	}
	reqMetaCtx := logging.ContextWithRequestMeta(vamCtx, &logging.RequestMeta{RequestID: "req-vam-1"})
	decRec := BuildDecisionAuditRecord(reqMetaCtx, decisionReq, decision)
	assert.Equal(t, viewAsMemberSessionMode, decRec.SessionMode)

	env, disp := mapDecisionEnvelope(reqMetaCtx, decRec)
	require.Equal(t, decisionAuditEnqueued, disp)
	require.NotNil(t, env.Credential)
	assert.Equal(t, auditevent.SchemaVersion, env.SchemaVersion)
	assert.Equal(t, viewAsMemberSessionMode, env.Credential.SessionMode())

	rendered, err := auditevent.Render(env)
	require.NoError(t, err)
	assert.Contains(t, string(rendered), `"session_mode":"view_as_member"`)

	// Decision for another principal does not inherit caller's session_mode.
	otherDec := decision
	otherDec.PrincipalID = "another-user"
	otherDecRec := BuildDecisionAuditRecord(reqMetaCtx, decisionReq, otherDec)
	assert.Empty(t, otherDecRec.SessionMode)

	// --- Unit: requestAuthAttrs ---
	hasSessionModeAttr := func(attrs []slog.Attr) (string, bool) {
		for _, a := range attrs {
			if a.Key == "session_mode" {
				return a.Value.String(), true
			}
		}
		return "", false
	}
	gotMode, ok := hasSessionModeAttr(requestAuthAttrs(vamCtx))
	assert.True(t, ok, "requestAuthAttrs must include session_mode when view-as-member is active")
	assert.Equal(t, viewAsMemberSessionMode, gotMode)

	_, ok = hasSessionModeAttr(requestAuthAttrs(normalCtx))
	assert.False(t, ok, "requestAuthAttrs must omit session_mode in normal sessions")

	// Principal-mismatch check across all four sites (auditActorFromContext,
	// BuildDecisionAuditRecord, accessConstraintAuditCredential, requestAuthAttrs):
	// a ctx whose identity principal differs from the marker uid yields no session_mode.
	mismatchIdentity := NewAuthenticatedUser("different-user-id", "other@example.com", "Other", store.UserRoleMember, string(ClientTypeWeb))
	mismatchCtx := contextWithIdentity(ctx, mismatchIdentity)
	mismatchCtx = contextWithCredentialContext(mismatchCtx, credentialContextForIdentity(mismatchIdentity))
	mismatchCtx = contextWithViewAsMember(mismatchCtx, adminID)

	assert.Empty(t, auditActorFromContext(mismatchCtx).SessionMode,
		"auditActorFromContext must not set session_mode when principal differs from marker uid")
	mismatchDec := decision
	mismatchDec.PrincipalID = mismatchIdentity.ID()
	assert.Empty(t, BuildDecisionAuditRecord(mismatchCtx, decisionReq, mismatchDec).SessionMode,
		"BuildDecisionAuditRecord must not set session_mode when principal differs from marker uid")
	mismatchCred, err := accessConstraintAuditCredential(mismatchCtx)
	require.NoError(t, err)
	require.NotNil(t, mismatchCred)
	assert.Empty(t, mismatchCred.SessionMode(),
		"accessConstraintAuditCredential must not set session_mode when principal differs from marker uid")
	_, ok = hasSessionModeAttr(requestAuthAttrs(mismatchCtx))
	assert.False(t, ok, "requestAuthAttrs must omit session_mode when principal differs from marker uid")

	// Matching ctx on accessConstraintAuditCredential does populate session_mode.
	matchedCred, err := accessConstraintAuditCredential(vamCtx)
	require.NoError(t, err)
	require.NotNil(t, matchedCred)
	assert.Equal(t, viewAsMemberSessionMode, matchedCred.SessionMode())

	// --- End-to-end: mutation in view-as-member vs outside ---
	ws := NewWebServer(WebServerConfig{
		Host:          "127.0.0.1",
		SessionSecret: "test-session-secret-32-bytes-ok!",
	})
	ws.SetStore(s)
	ws.SetUserTokenService(srv.GetUserTokenService())
	ws.SetAuthzService(srv.GetAuthzService())
	ws.SetViewAsMemberHub(srv)
	ws.MountHubAPI(srv.Handler(), nil)

	ts := httptest.NewServer(ws.Handler())
	t.Cleanup(ts.Close)

	adminUser := &store.User{
		ID:          adminID,
		Email:       "audit-admin@example.com",
		DisplayName: "Audit Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, adminUser))
	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminUser.ID,
		RoleDefinitionID: superAdminRD.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	access, refresh, expiresIn, err := srv.GetUserTokenService().MintTokenPair(UserTokenSubject{
		UserID:            adminUser.ID,
		Email:             adminUser.Email,
		DisplayName:       adminUser.DisplayName,
		Role:              adminUser.Role,
		SessionGeneration: adminUser.SessionGeneration,
		ClientType:        ClientTypeWeb,
	})
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodGet, ts.URL+"/", nil)
	w := httptest.NewRecorder()
	sess, _ := ws.sessionStore.New(r, webSessionName)
	sess.Values[sessKeyUserID] = adminUser.ID
	sess.Values[sessKeyUserEmail] = adminUser.Email
	sess.Values[sessKeyUserName] = adminUser.DisplayName
	sess.Values[sessKeyUserRole] = adminUser.Role
	sess.Values[sessKeySessionGeneration] = adminUser.SessionGeneration
	sess.Values[sessKeyHubAccessToken] = access
	sess.Values[sessKeyHubRefreshToken] = refresh
	sess.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
	require.NoError(t, sess.Save(r, w))

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	jar.SetCookies(mustParseURL(t, ts.URL), w.Result().Cookies())
	client := &http.Client{Jar: jar}

	createProject := func(name string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/projects", bytes.NewBufferString(`{"name":`+strconvQuote(name)+`}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		var out struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		require.NotEmpty(t, out.ID)
		return out.ID
	}

	// Mutation OUTSIDE view-as-member mode.
	normalProjID := createProject("Normal Mode Project")
	normalAudits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "project_member_add",
		TargetID:     normalProjID,
	})
	require.NoError(t, err)
	require.Len(t, normalAudits, 1)
	assert.Empty(t, normalAudits[0].SessionMode, "mutation outside view-as-member must not carry session_mode")

	// Enter view-as-member mode, then perform the same mutation.
	toggleReq, err := http.NewRequest(http.MethodPost, ts.URL+"/auth/view-as-member", bytes.NewBufferString(`{"enabled":true}`))
	require.NoError(t, err)
	toggleReq.Header.Set("Content-Type", "application/json")
	toggleResp, err := client.Do(toggleReq)
	require.NoError(t, err)
	toggleResp.Body.Close()
	require.Equal(t, http.StatusOK, toggleResp.StatusCode)

	vamProjID := createProject("Member View Project")
	vamAudits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "project_member_add",
		TargetID:     vamProjID,
		SessionMode:  viewAsMemberSessionMode,
	})
	require.NoError(t, err)
	require.Len(t, vamAudits, 1, "mutation inside view-as-member must be queryable by session_mode=view_as_member")
	assert.Equal(t, viewAsMemberSessionMode, vamAudits[0].SessionMode)
	assert.Equal(t, adminUser.ID, vamAudits[0].ActorPrincipalID)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestViewAsMember_DevAuthTransitionAuditSessionMode is a regression test for
// R1: under dev-auth (DevAuthToken configured), devAuthMiddleware runs on
// /auth/view-as-member and /auth/logout and attaches the ambient view-as-member
// marker when the cookie has vam=true. Enter transition records must still
// record session_mode="view_as_member", while BOTH toggle-exit and sign-out
// exit transition records must leave session_mode empty (design §4.6, §11.8).
func TestViewAsMember_DevAuthTransitionAuditSessionMode(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	enableViewAsMemberExperiment(t, srv, true)

	ws := NewWebServer(WebServerConfig{
		Host:          "127.0.0.1",
		SessionSecret: "test-session-secret-32-bytes-ok!",
		DevAuthToken:  "dev-auth-regression-token",
	})
	ws.SetStore(s)
	ws.SetUserTokenService(srv.GetUserTokenService())
	ws.SetAuthzService(srv.GetAuthzService())
	ws.SetViewAsMemberHub(srv)
	ws.MountHubAPI(srv.Handler(), nil)

	ts := httptest.NewServer(ws.Handler())
	t.Cleanup(ts.Close)

	adminUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "devauth-vam-admin@example.com",
		DisplayName: "DevAuth Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, adminUser))
	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminUser.ID,
		RoleDefinitionID: superAdminRD.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	access, refresh, expiresIn, err := srv.GetUserTokenService().MintTokenPair(UserTokenSubject{
		UserID:            adminUser.ID,
		Email:             adminUser.Email,
		DisplayName:       adminUser.DisplayName,
		Role:              adminUser.Role,
		SessionGeneration: adminUser.SessionGeneration,
		ClientType:        ClientTypeWeb,
	})
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodGet, ts.URL+"/", nil)
	w := httptest.NewRecorder()
	sess, _ := ws.sessionStore.New(r, webSessionName)
	sess.Values[sessKeyUserID] = adminUser.ID
	sess.Values[sessKeyUserEmail] = adminUser.Email
	sess.Values[sessKeyUserName] = adminUser.DisplayName
	sess.Values[sessKeyUserRole] = adminUser.Role
	sess.Values[sessKeySessionGeneration] = adminUser.SessionGeneration
	sess.Values[sessKeyHubAccessToken] = access
	sess.Values[sessKeyHubRefreshToken] = refresh
	sess.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
	require.NoError(t, sess.Save(r, w))

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	jar.SetCookies(mustParseURL(t, ts.URL), w.Result().Cookies())
	client := &http.Client{Jar: jar}

	postToggle := func(enabled bool) {
		t.Helper()
		body := `{"enabled":false}`
		if enabled {
			body = `{"enabled":true}`
		}
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/auth/view-as-member", bytes.NewBufferString(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	// 1. Enter -> enter audit carries session_mode="view_as_member".
	postToggle(true)
	enterAudits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: viewAsMemberAuditEnter,
		TargetID:     adminUser.ID,
	})
	require.NoError(t, err)
	require.Len(t, enterAudits, 1)
	assert.Equal(t, viewAsMemberSessionMode, enterAudits[0].SessionMode,
		"enter transition record under dev-auth must carry session_mode=view_as_member")

	// 2. Toggle exit -> exit audit must leave session_mode empty despite devAuthMiddleware.
	postToggle(false)
	exitAudits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: viewAsMemberAuditExit,
		TargetID:     adminUser.ID,
	})
	require.NoError(t, err)
	require.Len(t, exitAudits, 1)
	assert.Empty(t, exitAudits[0].SessionMode,
		"toggle-exit transition record under dev-auth must leave session_mode empty")
	assert.JSONEq(t, `{"view_as_member":false,"reason":"user"}`, exitAudits[0].AfterSummary)

	// 3. Re-enter, then sign out via /auth/logout -> sign_out exit audit must also leave session_mode empty.
	postToggle(true)
	logoutReq, err := http.NewRequest(http.MethodPost, ts.URL+"/auth/logout", nil)
	require.NoError(t, err)
	logoutResp, err := client.Do(logoutReq)
	require.NoError(t, err)
	logoutResp.Body.Close()
	require.Equal(t, http.StatusOK, logoutResp.StatusCode)

	exitAudits, _, err = s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: viewAsMemberAuditExit,
		TargetID:     adminUser.ID,
	})
	require.NoError(t, err)
	require.Len(t, exitAudits, 2)
	for _, rec := range exitAudits {
		assert.Empty(t, rec.SessionMode,
			"all exit transition records under dev-auth (toggle and sign_out) must leave session_mode empty")
	}
	assert.JSONEq(t, `{"view_as_member":false,"reason":"sign_out"}`, exitAudits[1].AfterSummary)
}

type orderCheckViewAsMemberHub struct {
	onAudit   func()
	onPublish func()
}

func (h *orderCheckViewAsMemberHub) AuthorizeViewAsMemberChange(context.Context, string, bool) error {
	return nil
}

func (h *orderCheckViewAsMemberHub) RecordViewAsMemberTransition(context.Context, string, bool, string) {
	if h.onAudit != nil {
		h.onAudit()
	}
}

func (h *orderCheckViewAsMemberHub) PublishSessionModeChanged(context.Context, string) {
	if h.onPublish != nil {
		h.onPublish()
	}
}

// TestViewAsMember_PublishAfterResponseFlush pins the ordering invariant in
// handleViewAsMember: the transition audit runs synchronously before the HTTP
// response is written, and PublishSessionModeChanged runs only AFTER the HTTP
// response (Set-Cookie + JSON body) has been written and flushed so the
// toggling tab's SSE reconnect/reload cannot abort the in-flight POST and lose
// the cookie update.
func TestViewAsMember_PublishAfterResponseFlush(t *testing.T) {
	ws := NewWebServer(WebServerConfig{
		Host:          "127.0.0.1",
		SessionSecret: "test-session-secret-32-bytes-ok!",
	})

	// Seed a valid admin session cookie.
	seedReq := httptest.NewRequest(http.MethodGet, "/", nil)
	seedRec := httptest.NewRecorder()
	sess, err := ws.sessionStore.New(seedReq, webSessionName)
	require.NoError(t, err)
	sess.Values[sessKeyUserID] = "admin-order-1"
	sess.Values[sessKeyUserEmail] = "admin-order@example.com"
	sess.Values[sessKeyUserName] = "Order Admin"
	sess.Values[sessKeyUserRole] = store.UserRoleAdmin
	require.NoError(t, sess.Save(seedReq, seedRec))

	rec := httptest.NewRecorder()
	var (
		auditCalled       bool
		auditBodyLen      int
		auditFlushed      bool
		publishCalled     bool
		publishStatus     int
		publishFlushed    bool
		publishBody       string
		publishSetCookies []string
	)
	ws.SetViewAsMemberHub(&orderCheckViewAsMemberHub{
		onAudit: func() {
			auditCalled = true
			auditBodyLen = rec.Body.Len()
			auditFlushed = rec.Flushed
		},
		onPublish: func() {
			publishCalled = true
			publishStatus = rec.Code
			publishFlushed = rec.Flushed
			publishBody = rec.Body.String()
			publishSetCookies = append([]string(nil), rec.Header().Values("Set-Cookie")...)
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/auth/view-as-member", bytes.NewBufferString(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range seedRec.Result().Cookies() {
		req.AddCookie(c)
	}

	ws.handleViewAsMember(rec, req)

	require.True(t, auditCalled, "transition audit must run")
	assert.Equal(t, 0, auditBodyLen, "synchronous transition audit must run before response body is written")
	assert.False(t, auditFlushed, "synchronous transition audit must run before response is flushed")

	require.True(t, publishCalled, "PublishSessionModeChanged must run")
	assert.Equal(t, http.StatusOK, publishStatus, "response status must be written before publish")
	assert.True(t, publishFlushed, "response must be flushed before publish")
	assert.JSONEq(t, `{"viewAsMember":true}`, publishBody, "response JSON body must be written before publish")
	require.NotEmpty(t, publishSetCookies, "Set-Cookie header must be written before publish")
	assert.Contains(t, strings.Join(publishSetCookies, ";"), webSessionName+"=")
}

// TestViewAsMember_RecordTransitionShadowsAmbientMarker directly exercises
// withoutViewAsMember inside RecordViewAsMemberTransition: when ctx carries
// BOTH an authenticated identity for user U (with matching credential context)
// AND the view-as-member marker for U, an exit transition (enter=false) must
// still persist an empty SessionMode. Without the shadow, ApplyActor would
// match the principal and stamp SessionMode="view_as_member" onto the exit
// record.
func TestViewAsMember_RecordTransitionShadowsAmbientMarker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	uid := api.NewUUID()
	u := NewAuthenticatedUser(uid, "shadow-admin@example.com", "Shadow Admin", store.UserRoleAdmin, string(ClientTypeWeb)).WithViewAsMember()
	markedCtx := contextWithIdentity(ctx, u)
	markedCtx = contextWithCredentialContext(markedCtx, credentialContextForIdentity(u))
	markedCtx = contextWithViewAsMember(markedCtx, uid)

	srv.RecordViewAsMemberTransition(markedCtx, uid, false, viewAsMemberReasonUser)

	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: viewAsMemberAuditExit,
		TargetID:     uid,
	})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	assert.Equal(t, uid, audits[0].ActorPrincipalID)
	assert.Equal(t, string(CredentialKindInteractive), audits[0].ActorCredentialType)
	assert.Empty(t, audits[0].SessionMode,
		"RecordViewAsMemberTransition(enter=false) must shadow ambient marker so exit record has empty SessionMode")
}

// TestViewAsMember_LogoutPublishAfterResponseFlush pins the logout transition
// audit and publish-after-flush ordering for both browser (HTML redirect) and
// API (JSON) sign-out requests while view-as-member is active.
func TestViewAsMember_LogoutPublishAfterResponseFlush(t *testing.T) {
	cases := []struct {
		name           string
		accept         string
		wantStatus     int
		wantLocation   string
		wantBodySubstr string
	}{
		{
			name:         "browser_redirect",
			accept:       "text/html",
			wantStatus:   http.StatusFound,
			wantLocation: "/login",
		},
		{
			name:           "json_api",
			accept:         "application/json",
			wantStatus:     http.StatusOK,
			wantBodySubstr: `"success":true`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := NewWebServer(WebServerConfig{
				Host:          "127.0.0.1",
				SessionSecret: "test-session-secret-32-bytes-ok!",
			})

			seedReq := httptest.NewRequest(http.MethodGet, "/", nil)
			seedRec := httptest.NewRecorder()
			sess, err := ws.sessionStore.New(seedReq, webSessionName)
			require.NoError(t, err)
			sess.Values[sessKeyUserID] = "admin-logout-order"
			sess.Values[sessKeyUserEmail] = "admin-logout@example.com"
			sess.Values[sessKeyUserName] = "Logout Admin"
			sess.Values[sessKeyUserRole] = store.UserRoleAdmin
			sess.Values[sessKeyViewAsMember] = true
			require.NoError(t, sess.Save(seedReq, seedRec))

			rec := httptest.NewRecorder()
			var (
				auditCalled       bool
				auditFlushed      bool
				publishCalled     bool
				publishStatus     int
				publishFlushed    bool
				publishBody       string
				publishLocation   string
				publishSetCookies []string
			)
			ws.SetViewAsMemberHub(&orderCheckViewAsMemberHub{
				onAudit: func() {
					auditCalled = true
					auditFlushed = rec.Flushed
				},
				onPublish: func() {
					publishCalled = true
					publishStatus = rec.Code
					publishFlushed = rec.Flushed
					publishBody = rec.Body.String()
					publishLocation = rec.Header().Get("Location")
					publishSetCookies = append([]string(nil), rec.Header().Values("Set-Cookie")...)
				},
			})

			req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
			req.Header.Set("Accept", tc.accept)
			for _, c := range seedRec.Result().Cookies() {
				req.AddCookie(c)
			}

			ws.handleLogout(rec, req)

			require.True(t, auditCalled, "logout transition audit must run")
			assert.False(t, auditFlushed, "logout transition audit must run before response is flushed")

			require.True(t, publishCalled, "logout must call PublishSessionModeChanged when view-as-member was active")
			assert.Equal(t, tc.wantStatus, publishStatus, "response status must be written before publish")
			assert.True(t, publishFlushed, "response must be flushed before publish")
			if tc.wantLocation != "" {
				assert.Equal(t, tc.wantLocation, publishLocation)
			}
			if tc.wantBodySubstr != "" {
				assert.Contains(t, publishBody, tc.wantBodySubstr)
			}
			require.NotEmpty(t, publishSetCookies, "Set-Cookie clearing scion_sess must be written before publish")
			assert.Contains(t, strings.Join(publishSetCookies, ";"), webSessionName+"=")
		})
	}
}
