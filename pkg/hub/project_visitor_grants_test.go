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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project visitor grants by email and first contact (ptone/scion#4603).

const (
	grantTestIssuer      = "https://idp-grant.example.com"
	grantTestOtherIssuer = "https://idp-grant-other.example.com"
	grantTestNoFlag      = "https://idp-grant-noflag.example.com"
	grantTestAudience    = "https://hub-grant.example.com"
)

// visitorGrantFixture is a test server with the visitor experiment on, a
// federation authenticator trusting three user issuers (two that allow
// visitor grants, the first limited to @corp.example emails, and one that
// does not), and a project with an owner, an admin and a member.
type visitorGrantFixture struct {
	*visitorFixture
	fedConfig config.FederationConfig
	projectID string
	owner     *store.User
	admin     *store.User
	member    *store.User
}

// newVisitorGrantFixture builds the fixture. Each install runs right after
// the server is constructed and before any setup, which is where
// installStoreFault must be called.
func newVisitorGrantFixture(t *testing.T, install ...func(srv *Server)) *visitorGrantFixture {
	t.Helper()
	srv, st := testServer(t)
	for _, in := range install {
		in(srv)
	}
	key, jwksSrv, kid := setupFederationTestServer(t)
	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{IssuerURL: grantTestIssuer, IssuerType: string(IssuerTypeUser), JWKSURL: jwksSrv.URL, ExpectedAudience: grantTestAudience,
				AllowVisitorGrants: true, AllowedEmails: []string{"*@corp.example"}},
			{IssuerURL: grantTestOtherIssuer, IssuerType: string(IssuerTypeUser), JWKSURL: jwksSrv.URL, ExpectedAudience: grantTestAudience,
				AllowVisitorGrants: true},
			{IssuerURL: grantTestNoFlag, IssuerType: string(IssuerTypeUser), JWKSURL: jwksSrv.URL, ExpectedAudience: grantTestAudience},
		},
	}
	auth, err := NewFederationAuthenticator(cfg, grantTestAudience,
		&http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	require.NoError(t, err)
	srv.federationAuth.Store(auth)
	visitorSetExperiment(t, srv, true)

	ctx := context.Background()
	ownerID := tid(t.Name() + "-owner")
	projectID := tid(t.Name() + "-project")
	createRS1Project(t, st, projectID, ownerID)
	adminID := tid(t.Name() + "-admin")
	createRS1UserWithRole(t, st, adminID, adminID+"@test.com", projectID, store.ProjectRoleAdmin)
	memberID := tid(t.Name() + "-member")
	createRS1UserWithRole(t, st, memberID, memberID+"@test.com", projectID, store.ProjectRoleMember)
	get := func(id string) *store.User {
		u, err := st.GetUser(ctx, id)
		require.NoError(t, err)
		return u
	}
	return &visitorGrantFixture{
		visitorFixture: &visitorFixture{srv: srv, st: st, auth: auth, key: key, kid: kid},
		fedConfig:      cfg,
		projectID:      projectID,
		owner:          get(ownerID),
		admin:          get(adminID),
		member:         get(memberID),
	}
}

// setIssuerAcceptsVisitors replaces the server's federation authenticator
// with one built from the fixture's configuration with issuer's
// allow_visitor_grants set to on, as a configuration reload does.
func (f *visitorGrantFixture) setIssuerAcceptsVisitors(t *testing.T, issuer string, on bool) {
	t.Helper()
	cfg := f.fedConfig
	cfg.TrustedIssuers = append([]config.TrustedIssuerConfig(nil), f.fedConfig.TrustedIssuers...)
	found := false
	for i := range cfg.TrustedIssuers {
		if cfg.TrustedIssuers[i].IssuerURL == issuer {
			cfg.TrustedIssuers[i].AllowVisitorGrants = on
			found = true
		}
	}
	require.True(t, found, "issuer %s is not in the fixture", issuer)
	auth, err := NewFederationAuthenticator(cfg, grantTestAudience,
		&http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	require.NoError(t, err)
	f.srv.federationAuth.Store(auth)
}

// middlewareResponse runs token through the server's auth middleware and
// returns the response; the next handler answers 200 when it runs.
func (f *visitorGrantFixture) middlewareResponse(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	h := UnifiedAuthMiddleware(f.srv.authConfig)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set(FederationTokenHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (f *visitorGrantFixture) visitorsPath() string {
	return "/api/v1/projects/" + f.projectID + "/visitors"
}

// grant posts a visitor grant as actor and returns the response.
func (f *visitorGrantFixture) grant(t *testing.T, actor *store.User, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, actor, http.MethodPost, f.visitorsPath(), body)
}

// grantOK grants (issuer, email) as the owner and returns the visitor.
func (f *visitorGrantFixture) grantOK(t *testing.T, issuer, email string) ProjectVisitor {
	t.Helper()
	rec := f.grant(t, f.owner, map[string]interface{}{"issuer": issuer, "email": email})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var v ProjectVisitor
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	return v
}

func (f *visitorGrantFixture) list(t *testing.T) []ProjectVisitor {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.visitorsPath(), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListProjectVisitorsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Visitors
}

// token signs an issuer token from issuer for subject with email; verified is
// the email_verified claim (nil leaves it out).
func (f *visitorGrantFixture) token(t *testing.T, issuer, subject, email string, verified *bool) string {
	t.Helper()
	claims := validFederationClaims(issuer, grantTestAudience)
	claims.Subject = subject
	claims.Email = email
	claims.EmailVerified = verified
	claims.Name = "Visitor " + subject
	claims.ProjectID, claims.AgentName, claims.RootUser, claims.Ancestry = "", "", "", nil
	return signFederationToken(t, f.key, f.kid, claims)
}

// visitorPermissions returns the permissions the request identity of
// token holds on the project, read from the store on each call, or nil
// when the request is not a visitor.
func (f *visitorGrantFixture) visitorPermissions(t *testing.T, token string) []string {
	t.Helper()
	identity, _, code := f.identityThroughMiddleware(t, token)
	require.Equal(t, http.StatusOK, code)
	v, ok := identity.(*VisitorIdentity)
	if !ok {
		return nil
	}
	perms, err := f.srv.authzService.getEffectivePermissions(context.Background(), v.Type(), v.ID(), store.RoleScopeProject, f.projectID)
	require.NoError(t, err)
	return perms
}

func (f *visitorGrantFixture) audits(t *testing.T, mutationType string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := f.st.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationType})
	require.NoError(t, err)
	return recs
}

func TestProjectVisitors_AddListRemove(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()

	v := f.grantOK(t, grantTestIssuer+"/", " Alice@Corp.Example ")
	assert.Equal(t, "alice@corp.example", v.Email, "email stored lower-cased")
	assert.Equal(t, grantTestIssuer, v.Issuer)
	assert.Equal(t, store.UserStatusPending, v.Status)
	assert.Equal(t, f.owner.ID, v.GrantedBy)
	assert.True(t, v.IssuerAccepted)

	// One transaction wrote a pending federated account and its binding.
	acct, err := f.st.GetFederatedUserByEmail(ctx, grantTestIssuer, "alice@corp.example")
	require.NoError(t, err)
	assert.Equal(t, v.ID, acct.ID)
	assert.Equal(t, store.UserStatusPending, acct.Status)
	bindings, err := f.st.ListVisitorRoleBindings(ctx, f.projectID)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	assert.Equal(t, acct.ID, bindings[0].PrincipalID)
	assert.Equal(t, v.BindingID, bindings[0].ID)

	grants := f.audits(t, mutationTypeProjectVisitorGrant)
	require.Len(t, grants, 1)
	assert.Equal(t, f.projectID, grants[0].TargetID)
	assert.Equal(t, f.owner.ID, grants[0].ActorPrincipalID)
	assert.Contains(t, grants[0].AfterSummary, `"email":"alice@corp.example"`)
	assert.Contains(t, grants[0].AfterSummary, `"issuer":"`+grantTestIssuer+`"`)

	listed := f.list(t)
	require.Len(t, listed, 1)
	assert.Equal(t, v.ID, listed[0].ID)
	assert.Equal(t, "alice@corp.example", listed[0].Email)
	assert.Equal(t, store.UserStatusPending, listed[0].Status)
	assert.True(t, listed[0].IssuerAccepted)

	// Re-adding the same visitor is a conflict, not a second grant.
	rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "email": "alice@corp.example"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Len(t, f.list(t), 1)

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+v.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, f.list(t))
	revokes := f.audits(t, mutationTypeProjectVisitorRevoke)
	require.Len(t, revokes, 1)
	assert.Contains(t, revokes[0].BeforeSummary, `"visitor_id":"`+v.ID+`"`)

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+v.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// An existing federated account for (issuer, email) is bound, not
// duplicated; the grant audit names its subject once linked.
func TestProjectVisitors_AddBindsExistingAccount(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	issuer, email := grantTestIssuer, "bob@corp.example"
	acct := &store.User{DisplayName: "Bob", Kind: store.UserKindFederated, Status: store.UserStatusActive, HomeIssuer: &issuer, FederatedEmail: &email}
	require.NoError(t, f.st.CreateFederatedUser(ctx, acct))
	_, err := f.st.LinkFederatedIdentity(ctx, acct.ID, issuer, "sub-bob", email)
	require.NoError(t, err)

	v := f.grantOK(t, grantTestIssuer, "Bob@corp.example")
	assert.Equal(t, acct.ID, v.ID)
	assert.Equal(t, store.UserStatusActive, v.Status)
	grants := f.audits(t, mutationTypeProjectVisitorGrant)
	require.Len(t, grants, 1)
	assert.Contains(t, grants[0].AfterSummary, `"subject":"sub-bob"`)
}

func TestProjectVisitors_GrantRefusals(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()

	for name, tc := range map[string]struct {
		body  map[string]interface{}
		field string
	}{
		"issuer not trusted":     {map[string]interface{}{"issuer": "https://unknown.example.com", "email": "carol@corp.example"}, "issuer"},
		"issuer flag off":        {map[string]interface{}{"issuer": grantTestNoFlag, "email": "carol@corp.example"}, "issuer"},
		"email not allowed":      {map[string]interface{}{"issuer": grantTestIssuer, "email": "carol@elsewhere.example"}, "email"},
		"missing issuer":         {map[string]interface{}{"email": "carol@corp.example"}, "issuer"},
		"invalid email":          {map[string]interface{}{"issuer": grantTestIssuer, "email": "carol"}, "email"},
		"synthetic email":        {map[string]interface{}{"issuer": grantTestIssuer, "email": "fed+x@corp.example.invalid"}, "email"},
		"expiry in the past":     {map[string]interface{}{"issuer": grantTestIssuer, "email": "carol@corp.example", "expiresAt": time.Now().Add(-time.Hour)}, "expiresAt"},
		"expiry before start":    {map[string]interface{}{"issuer": grantTestIssuer, "email": "carol@corp.example", "notBefore": time.Now().Add(48 * time.Hour), "expiresAt": time.Now().Add(24 * time.Hour)}, "expiresAt"},
		"expiry not a timestamp": {map[string]interface{}{"issuer": grantTestIssuer, "email": "carol@corp.example", "expiresAt": "30d"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.grant(t, f.owner, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			if tc.field != "" {
				assert.Equal(t, tc.field, resp.Error.Details["field"])
			}
		})
	}
	rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestNoFlag, "email": "carol@corp.example"})
	assert.Contains(t, rec.Body.String(), visitorIssuerNotAcceptedMessage)

	t.Run("member actor", func(t *testing.T) {
		rec := f.grant(t, f.member, map[string]interface{}{"issuer": grantTestIssuer, "email": "carol@corp.example"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		rec = doRequestAsUser(t, f.srv, f.member, http.MethodGet, f.visitorsPath(), nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("user access credential", func(t *testing.T) {
		key := mintScopedUAT(t, f.srv, f.owner.ID, f.projectID, []string{"project:read"})
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := doRequestWithUAT(t, f.srv, key, method, f.visitorsPath(),
				map[string]interface{}{"issuer": grantTestIssuer, "email": "carol@corp.example"})
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			reason, credential := sessionOnlyDetailsOf(rec)
			assert.Equal(t, "GOV_PENDING", reason)
			assert.Equal(t, sessionRequiredCredential, credential)
		}
	})

	t.Run("experiment off", func(t *testing.T) {
		visitorSetExperiment(t, f.srv, false)
		defer visitorSetExperiment(t, f.srv, true)
		rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "email": "carol@corp.example"})
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.visitorsPath(), nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	// No refusal above created an account or a grant.
	_, err := f.st.GetFederatedUserByEmail(ctx, grantTestIssuer, "carol@corp.example")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.st.GetFederatedUserByEmail(ctx, grantTestNoFlag, "carol@corp.example")
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Empty(t, f.list(t))
	assert.Empty(t, f.audits(t, mutationTypeProjectVisitorGrant))
}

// bindCustomProjectRole binds userID to a new project role named name,
// holding perms, on the fixture's project.
func (f *visitorGrantFixture) bindCustomProjectRole(t *testing.T, name, userID string, perms []string) {
	t.Helper()
	ctx := context.Background()
	rd, err := f.st.CreateRoleDefinition(ctx, &store.RoleDefinition{Name: name, ScopeType: store.RoleScopeProject, Permissions: perms})
	require.NoError(t, err)
	_, err = f.st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// listedVisitor returns the listed grant of visitor id, and whether it is
// listed.
func (f *visitorGrantFixture) listedVisitor(t *testing.T, id string) (ProjectVisitor, bool) {
	t.Helper()
	for _, v := range f.list(t) {
		if v.ID == id {
			return v, true
		}
	}
	return ProjectVisitor{}, false
}

// grantWithExpiry grants email of issuer as the owner, expiring at
// expiresAt.
func (f *visitorGrantFixture) grantWithExpiry(t *testing.T, issuer, email string, expiresAt time.Time) ProjectVisitor {
	t.Helper()
	rec := f.grant(t, f.owner, map[string]interface{}{"issuer": issuer, "email": email, "expiresAt": expiresAt})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var v ProjectVisitor
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	return v
}

// Project admins hold project.manage_visitors as owners do; the
// super-admin holds it through its system-scope role (every permission,
// role_binding.create included), with no special case. A hub admin holds
// it through the hub-admin role, so it may list, revoke and shorten the
// grants of any project, but granting or extending a window needs every
// permission of the project-visitor role on the project too, which
// hub-admin alone does not give.
func TestProjectVisitors_WhoMayGrant(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	rec := f.grant(t, f.admin, map[string]interface{}{"issuer": grantTestOtherIssuer, "email": "dan@any.example"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var dan ProjectVisitor
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dan))

	superID := tid(t.Name() + "-super")
	createTestUserWithRole(t, f.st, superID, superID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, f.st, superID)
	super, err := f.st.GetUser(ctx, superID)
	require.NoError(t, err)
	rec = f.grant(t, super, map[string]interface{}{"issuer": grantTestOtherIssuer, "email": "erin@any.example"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	expiry := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	fern := f.grantWithExpiry(t, grantTestOtherIssuer, "fern@any.example", expiry)
	assert.Len(t, f.list(t), 3)

	hubAdminID := tid(t.Name() + "-hubadmin")
	createTestUserWithRole(t, f.st, hubAdminID, hubAdminID+"@test.com", "member", store.SystemRoleHubAdmin)
	ensureHubMembership(ctx, f.st, hubAdminID)
	hubAdmin, err := f.st.GetUser(ctx, hubAdminID)
	require.NoError(t, err)

	// hub-admin alone: list, extend (refused), shorten, revoke, grant
	// (refused).
	rec = doRequestAsUser(t, f.srv, hubAdmin, http.MethodGet, f.visitorsPath(), nil)
	assert.Equal(t, http.StatusOK, rec.Code, "a hub admin may list: %s", rec.Body.String())

	rec = doRequestAsUser(t, f.srv, hubAdmin, http.MethodPatch, f.visitorsPath()+"/"+fern.ID,
		map[string]interface{}{"expiresAt": expiry.Add(24 * time.Hour)})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a hub admin may not extend: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), visitorGrantNonAmplificationMessage)
	listed, ok := f.listedVisitor(t, fern.ID)
	require.True(t, ok)
	require.NotNil(t, listed.ExpiresAt)
	assert.WithinDuration(t, expiry, *listed.ExpiresAt, time.Second, "the window is unchanged")
	assert.Empty(t, f.audits(t, mutationTypeProjectVisitorUpdate))

	shorter := expiry.Add(-12 * time.Hour)
	rec = doRequestAsUser(t, f.srv, hubAdmin, http.MethodPatch, f.visitorsPath()+"/"+fern.ID,
		map[string]interface{}{"expiresAt": shorter})
	assert.Equal(t, http.StatusOK, rec.Code, "a hub admin may shorten: %s", rec.Body.String())
	listed, ok = f.listedVisitor(t, fern.ID)
	require.True(t, ok)
	require.NotNil(t, listed.ExpiresAt)
	assert.WithinDuration(t, shorter, *listed.ExpiresAt, time.Second)
	assert.Len(t, f.audits(t, mutationTypeProjectVisitorUpdate), 1)

	rec = doRequestAsUser(t, f.srv, hubAdmin, http.MethodDelete, f.visitorsPath()+"/"+dan.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "a hub admin may revoke: %s", rec.Body.String())
	_, ok = f.listedVisitor(t, dan.ID)
	assert.False(t, ok, "the revoked grant is gone")
	assert.Len(t, f.audits(t, mutationTypeProjectVisitorRevoke), 1)

	rec = f.grant(t, hubAdmin, map[string]interface{}{"issuer": grantTestOtherIssuer, "email": "gil@any.example"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a hub admin may not grant: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), visitorGrantNonAmplificationMessage)
	assert.Len(t, f.list(t), 2)

	// With the project-visitor permissions on the project, the hub admin's
	// project.manage_visitors (from hub-admin) lets it grant and extend.
	f.bindCustomProjectRole(t, "visitor-permissions-only", hubAdminID, projectVisitorPermissionIDs())
	rec = f.grant(t, hubAdmin, map[string]interface{}{"issuer": grantTestOtherIssuer, "email": "gil@any.example"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, hubAdmin, http.MethodPatch, f.visitorsPath()+"/"+fern.ID, map[string]interface{}{})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	listed, ok = f.listedVisitor(t, fern.ID)
	require.True(t, ok)
	assert.Nil(t, listed.ExpiresAt, "the expiry is cleared")
	assert.Len(t, f.list(t), 3)
}

// Non-amplification: an actor that holds project.manage_visitors but not
// every permission of the project-visitor role may list grants and shorten
// their windows, but may neither grant nor extend a grant's window.
func TestProjectVisitors_GrantNeedsEveryVisitorPermission(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	expiry := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	existing := f.grantWithExpiry(t, grantTestOtherIssuer, "hana@any.example", expiry)

	partialID := tid(t.Name() + "-partial")
	require.NoError(t, f.st.CreateUser(ctx, &store.User{
		ID: partialID, Email: partialID + "@test.com", DisplayName: "Partial", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.st, partialID)
	f.bindCustomProjectRole(t, "visitor-manager-partial", partialID,
		[]string{"project.manage_visitors", "agent.list", "agent.read"})
	partial, err := f.st.GetUser(ctx, partialID)
	require.NoError(t, err)

	rec := doRequestAsUser(t, f.srv, partial, http.MethodGet, f.visitorsPath(), nil)
	require.Equal(t, http.StatusOK, rec.Code, "project.manage_visitors admits the actor: %s", rec.Body.String())

	rec = f.grant(t, partial, map[string]interface{}{"issuer": grantTestOtherIssuer, "email": "ines@any.example"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), visitorGrantNonAmplificationMessage)
	_, err = f.st.GetFederatedUserByEmail(ctx, grantTestOtherIssuer, "ines@any.example")
	assert.ErrorIs(t, err, store.ErrNotFound, "no pending account was created")
	assert.Len(t, f.audits(t, mutationTypeProjectVisitorGrant), 1, "only the owner's grant was audited")

	// Extending the window (a later expiry, or clearing it) is refused.
	for _, body := range []map[string]interface{}{
		{"expiresAt": expiry.Add(24 * time.Hour)},
		{},
	} {
		rec = doRequestAsUser(t, f.srv, partial, http.MethodPatch, f.visitorsPath()+"/"+existing.ID, body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "extension %v: %s", body, rec.Body.String())
		assert.Contains(t, rec.Body.String(), visitorGrantNonAmplificationMessage)
	}
	listed := f.list(t)
	require.Len(t, listed, 1)
	assert.Equal(t, existing.BindingID, listed[0].BindingID)
	require.NotNil(t, listed[0].ExpiresAt)
	assert.WithinDuration(t, expiry, *listed[0].ExpiresAt, time.Second, "the window is unchanged")
	assert.Empty(t, f.audits(t, mutationTypeProjectVisitorUpdate))

	// Shortening it is allowed.
	shorter := expiry.Add(-12 * time.Hour)
	rec = doRequestAsUser(t, f.srv, partial, http.MethodPatch, f.visitorsPath()+"/"+existing.ID,
		map[string]interface{}{"expiresAt": shorter})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	listed = f.list(t)
	require.Len(t, listed, 1)
	require.NotNil(t, listed[0].ExpiresAt)
	assert.WithinDuration(t, shorter, *listed[0].ExpiresAt, time.Second)
	assert.Len(t, f.audits(t, mutationTypeProjectVisitorUpdate), 1)
}

func TestVisitorWindowExtends(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	for _, tc := range []struct {
		name             string
		oldStart, oldEnd *time.Time
		newStart, newEnd *time.Time
		extends          bool
	}{
		{name: "open stays open"},
		{name: "same window", oldStart: at(time.Hour), oldEnd: at(2 * time.Hour), newStart: at(time.Hour), newEnd: at(2 * time.Hour)},
		{name: "expiry added", newEnd: at(time.Hour)},
		{name: "start added", newStart: at(time.Hour)},
		{name: "shorter expiry", oldEnd: at(2 * time.Hour), newEnd: at(time.Hour)},
		{name: "later expiry", oldEnd: at(time.Hour), newEnd: at(2 * time.Hour), extends: true},
		{name: "expiry cleared", oldEnd: at(time.Hour), extends: true},
		{name: "later start", oldStart: at(time.Hour), newStart: at(2 * time.Hour)},
		{name: "earlier start", oldStart: at(2 * time.Hour), newStart: at(time.Hour), extends: true},
		{name: "start cleared", oldStart: at(time.Hour), extends: true},
		{name: "start cleared, expiry shortened", oldStart: at(time.Hour), oldEnd: at(3 * time.Hour), newEnd: at(2 * time.Hour), extends: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.extends, visitorWindowExtends(tc.oldStart, tc.oldEnd, tc.newStart, tc.newEnd))
		})
	}
}

// A grant authorizes only inside its window: a start in the future or an
// expiry in the past does not authorize, and a window change takes effect
// on the next request.
func TestProjectVisitors_WindowHonoured(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "email": "fay@corp.example", "notBefore": future})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var v ProjectVisitor
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	assert.NotNil(t, v.NotBefore)

	token := f.token(t, grantTestIssuer, "sub-fay", "fay@corp.example", boolPtr(true))
	assert.NotContains(t, f.visitorPermissions(t, token), "agent.message", "a grant that has not started does not authorize")

	// Clearing the window (PATCH with no bounds) authorizes.
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, f.visitorsPath()+"/"+v.ID, map[string]interface{}{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, f.visitorPermissions(t, token), "agent.message")
	require.Len(t, f.audits(t, mutationTypeProjectVisitorUpdate), 1)

	// An expiry in the past is refused by the route, and once reached it
	// stops the grant authorizing.
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, f.visitorsPath()+"/"+v.ID,
		map[string]interface{}{"expiresAt": time.Now().Add(-time.Minute)})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	past := time.Now().Add(-time.Minute)
	_, err := f.st.UpdateVisitorRoleBindingWindow(ctx, v.BindingID, nil, &past)
	require.NoError(t, err)
	assert.NotContains(t, f.visitorPermissions(t, token), "agent.message", "an expired grant does not authorize")

	rec = doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.visitorsPath()+"/"+v.ID, map[string]interface{}{})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, f.visitorsPath()+"/"+f.member.ID, map[string]interface{}{})
	assert.Equal(t, http.StatusNotFound, rec.Code, "only a visitor of the project is addressable: %s", rec.Body.String())
}

// Revoking a grant takes effect on the visitor's next request: nothing
// caches the grant.
func TestProjectVisitors_RevokeRefusesNextRequest(t *testing.T) {
	f := newVisitorGrantFixture(t)
	v := f.grantOK(t, grantTestIssuer, "gus@corp.example")
	token := f.token(t, grantTestIssuer, "sub-gus", "gus@corp.example", boolPtr(true))
	assert.Contains(t, f.visitorPermissions(t, token), "agent.message")

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+v.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.NotContains(t, f.visitorPermissions(t, token), "agent.message")
}

func TestFederatedVisitor_FirstContactLinksPendingAccount(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	v := f.grantOK(t, grantTestIssuer, "hal@corp.example")

	identity, credential, code := f.identityThroughMiddleware(t, f.token(t, grantTestIssuer, "sub-hal", "Hal@Corp.Example", boolPtr(true)))
	require.Equal(t, http.StatusOK, code)
	visitor, ok := identity.(*VisitorIdentity)
	require.True(t, ok, "identity = %T, want *VisitorIdentity", identity)
	assert.Equal(t, v.ID, visitor.ID(), "the request acts as the granted account")
	assert.Equal(t, "hal@corp.example", visitor.Email())
	assert.Equal(t, CredentialKindFederation, credential.Kind)

	acct, err := f.st.GetUser(ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, acct.Status)
	bySubject, err := f.st.GetFederatedUserBySubject(ctx, grantTestIssuer, "sub-hal")
	require.NoError(t, err)
	assert.Equal(t, v.ID, bySubject.ID)
	linked := f.audits(t, mutationTypeVisitorLinked)
	require.Len(t, linked, 1)
	assert.Equal(t, v.ID, linked[0].TargetID)
	assert.Contains(t, linked[0].AfterSummary, `"subject":"sub-hal"`)
	assert.Equal(t, store.UserStatusActive, f.list(t)[0].Status)

	// After linking the subject is the key: a later email change at the
	// issuer keeps the same account and grant, and links nothing new.
	identity, _, code = f.identityThroughMiddleware(t, f.token(t, grantTestIssuer, "sub-hal", "hal.new@corp.example", boolPtr(true)))
	require.Equal(t, http.StatusOK, code)
	visitor, ok = identity.(*VisitorIdentity)
	require.True(t, ok, "identity = %T, want *VisitorIdentity", identity)
	assert.Equal(t, v.ID, visitor.ID())
	assert.Len(t, f.audits(t, mutationTypeVisitorLinked), 1)

	// A token with no email_verified claim links too (the issuer does not
	// provide one).
	w := f.grantOK(t, grantTestIssuer, "ivy@corp.example")
	identity, _, code = f.identityThroughMiddleware(t, f.token(t, grantTestIssuer, "sub-ivy", "ivy@corp.example", nil))
	require.Equal(t, http.StatusOK, code)
	visitor, ok = identity.(*VisitorIdentity)
	require.True(t, ok, "identity = %T, want *VisitorIdentity", identity)
	assert.Equal(t, w.ID, visitor.ID())
}

func TestFederatedVisitor_FirstContactRefusals(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	v := f.grantOK(t, grantTestIssuer, "jo@corp.example")
	// A pending account on the other issuer, which has no allowed_emails,
	// so a token without an email claim still authenticates there.
	pat := f.grantOK(t, grantTestOtherIssuer, "pat@any.example")
	// A pending account on the issuer without the flag (written directly:
	// the route refuses that issuer).
	noFlag, email := grantTestNoFlag, "jo@corp.example"
	require.NoError(t, f.st.CreateFederatedUser(ctx, &store.User{DisplayName: "Jo", Kind: store.UserKindFederated,
		Status: store.UserStatusPending, HomeIssuer: &noFlag, FederatedEmail: &email}))

	for name, token := range map[string]string{
		"email_verified false": f.token(t, grantTestIssuer, "sub-jo", "jo@corp.example", boolPtr(false)),
		"no email claim":       f.token(t, grantTestOtherIssuer, "sub-pat", "", boolPtr(true)),
		"other email":          f.token(t, grantTestIssuer, "sub-jo", "joe@corp.example", boolPtr(true)),
		"mismatched issuer":    f.token(t, grantTestOtherIssuer, "sub-jo", "jo@corp.example", boolPtr(true)),
		"issuer flag off":      f.token(t, grantTestNoFlag, "sub-jo", "jo@corp.example", boolPtr(true)),
	} {
		t.Run(name, func(t *testing.T) {
			identity, _, code := f.identityThroughMiddleware(t, token)
			require.Equal(t, http.StatusOK, code)
			_, ok := identity.(*FederatedUserIdentity)
			assert.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
		})
	}

	t.Run("experiment off", func(t *testing.T) {
		visitorSetExperiment(t, f.srv, false)
		defer visitorSetExperiment(t, f.srv, true)
		identity, _, code := f.identityThroughMiddleware(t, f.token(t, grantTestIssuer, "sub-jo", "jo@corp.example", boolPtr(true)))
		require.Equal(t, http.StatusOK, code)
		_, ok := identity.(*FederatedUserIdentity)
		assert.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
	})

	for _, id := range []string{v.ID, pat.ID} {
		acct, err := f.st.GetUser(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, store.UserStatusPending, acct.Status, "nothing linked the account")
	}
	for _, issuer := range []string{grantTestIssuer, grantTestOtherIssuer, grantTestNoFlag} {
		_, err := f.st.GetFederatedUserBySubject(ctx, issuer, "sub-jo")
		assert.ErrorIs(t, err, store.ErrNotFound, issuer)
	}
	_, err := f.st.GetFederatedUserBySubject(ctx, grantTestOtherIssuer, "sub-pat")
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Empty(t, f.audits(t, mutationTypeVisitorLinked))
}

// staleFirstContactStore passes every call through until stale is set.
// From then on it answers the first subject lookup with not found and the
// first email lookup with stale, the account as it was before a
// concurrent first contact linked it: the view of the request that loses
// the race.
type staleFirstContactStore struct {
	store.Store
	stale        atomic.Pointer[store.User]
	subjectCalls atomic.Int32
	emailCalls   atomic.Int32
}

func (s *staleFirstContactStore) GetFederatedUserBySubject(ctx context.Context, issuer, subject string) (*store.User, error) {
	if s.stale.Load() != nil && s.subjectCalls.Add(1) == 1 {
		return nil, store.ErrNotFound
	}
	return s.Store.GetFederatedUserBySubject(ctx, issuer, subject)
}

func (s *staleFirstContactStore) GetFederatedUserByEmail(ctx context.Context, issuer, email string) (*store.User, error) {
	if stale := s.stale.Load(); stale != nil && s.emailCalls.Add(1) == 1 {
		c := *stale
		return &c, nil
	}
	return s.Store.GetFederatedUserByEmail(ctx, issuer, email)
}

// Two first requests at once: the one that loses the race finds the
// account already linked (ErrAlreadyExists), reads it again by subject and
// continues as the visitor; the database holds one link.
func TestFederatedVisitor_ConcurrentFirstContact(t *testing.T) {
	var wrapped *staleFirstContactStore
	f := newVisitorGrantFixture(t, func(srv *Server) {
		wrapped, _ = installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *staleFirstContactStore {
			return &staleFirstContactStore{Store: inner}
		})
	})
	ctx := context.Background()
	v := f.grantOK(t, grantTestIssuer, "kim@corp.example")
	before, err := f.st.GetUser(ctx, v.ID)
	require.NoError(t, err)
	require.Equal(t, store.UserStatusPending, before.Status)

	// The winning request links first.
	_, err = f.st.LinkPendingFederatedUser(ctx, v.ID, grantTestIssuer, "sub-kim", "kim@corp.example")
	require.NoError(t, err)

	wrapped.stale.Store(before)
	identity, _, code := f.identityThroughMiddleware(t, f.token(t, grantTestIssuer, "sub-kim", "kim@corp.example", boolPtr(true)))
	require.Equal(t, http.StatusOK, code)
	visitor, ok := identity.(*VisitorIdentity)
	require.True(t, ok, "identity = %T, want *VisitorIdentity", identity)
	assert.Equal(t, v.ID, visitor.ID())
	assert.Equal(t, int32(2), wrapped.subjectCalls.Load(), "the losing request read the account again by subject")

	bindings, err := f.st.GetExternalIdentitiesByUserID(ctx, v.ID)
	require.NoError(t, err)
	assert.Len(t, bindings, 1)
	assert.Empty(t, f.audits(t, mutationTypeVisitorLinked), "the losing request wrote no link record")
}

// Status policy at the request path: a suspended account is refused,
// whatever its grants; a linked account still pending keeps the verified
// identity.
func TestFederatedVisitor_StatusPolicy(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	v := f.grantOK(t, grantTestIssuer, "lee@corp.example")
	token := f.token(t, grantTestIssuer, "sub-lee", "lee@corp.example", boolPtr(true))
	identity, _, code := f.identityThroughMiddleware(t, token)
	require.Equal(t, http.StatusOK, code)
	require.IsType(t, &VisitorIdentity{}, identity)

	acct, err := f.st.GetUser(ctx, v.ID)
	require.NoError(t, err)
	acct.Status = store.UserStatusSuspended
	require.NoError(t, f.st.UpdateUser(ctx, acct))

	identity, _, code = f.identityThroughMiddleware(t, token)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Nil(t, identity, "the handler never runs for a suspended account")

	// A suspended account receives no new grant either.
	rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "email": "lee@corp.example"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// A linked account that is still pending (linked by another path) is
	// not a visitor.
	issuer, email := grantTestIssuer, "mo@corp.example"
	pending := &store.User{DisplayName: "Mo", Kind: store.UserKindFederated, Status: store.UserStatusPending, HomeIssuer: &issuer, FederatedEmail: &email}
	require.NoError(t, f.st.CreateFederatedUser(ctx, pending))
	_, err = f.st.LinkFederatedIdentity(ctx, pending.ID, issuer, "sub-mo", email)
	require.NoError(t, err)
	identity, _, code = f.identityThroughMiddleware(t, f.token(t, grantTestIssuer, "sub-mo", "mo@corp.example", boolPtr(true)))
	require.Equal(t, http.StatusOK, code)
	assert.IsType(t, &FederatedUserIdentity{}, identity)
}

// The visitor routes never open a route to a visitor: a linked visitor is
// refused by the route guard before the handler.
func TestProjectVisitors_VisitorIsRefusedOnTheRoutes(t *testing.T) {
	f := newVisitorGrantFixture(t)
	f.grantOK(t, grantTestIssuer, "nia@corp.example")
	req := httptest.NewRequest(http.MethodGet, f.visitorsPath(), nil)
	req.Header.Set(FederationTokenHeader, f.token(t, grantTestIssuer, "sub-nia", "nia@corp.example", boolPtr(true)))
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), visitorRouteRefusalMessage)
}

// federatedGet sends GET path with the issuer token as the federation credential
// through the full server handler.
func (f *visitorGrantFixture) federatedGet(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(FederationTokenHeader, token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// A linked visitor reads its own profile on GET /api/v1/auth/me (a
// VisitorAllowed route): the federated account's ID and the email the
// issuer verified. The same credential is refused on a route without the
// flag.
func TestVisitorAllowed_AuthMeReturnsVisitorOwnProfile(t *testing.T) {
	f := newVisitorGrantFixture(t)
	v := f.grantOK(t, grantTestIssuer, "ada@corp.example")
	token := f.token(t, grantTestIssuer, "sub-ada", "ada@corp.example", boolPtr(true))

	rec := f.federatedGet(t, "/api/v1/auth/me", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var me UserResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	assert.Equal(t, v.ID, me.ID, "the visitor's own federated account")
	assert.Equal(t, "ada@corp.example", me.Email)
	assert.Equal(t, "viewer", me.Role)

	rec = f.federatedGet(t, "/api/v1/users/me/groups", token)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), visitorRouteRefusalMessage)
}

// With allow_visitor_grants off for the issuer, a linked visitor is
// refused on the VisitorAllowed route too, with the uniform refusal.
func TestVisitorAllowed_AuthMeRefusedWhileIssuerFlagOff(t *testing.T) {
	f := newVisitorGrantFixture(t)
	f.grantOK(t, grantTestIssuer, "bea@corp.example")
	token := f.token(t, grantTestIssuer, "sub-bea", "bea@corp.example", boolPtr(true))
	rec := f.federatedGet(t, "/api/v1/auth/me", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.setIssuerAcceptsVisitors(t, grantTestIssuer, false)
	rec = f.federatedGet(t, "/api/v1/auth/me", token)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeForbidden, resp.Error.Code)
	assert.Equal(t, visitorRouteRefusalMessage, resp.Error.Message, "the uniform refusal")
}

// An unlinked federated user (no grant, so no federated account) is not a
// visitor: it gets the same responses with the visitor experiment on as
// with it off, on the VisitorAllowed route and on a route without the flag.
func TestVisitorAllowed_UnlinkedFederatedUserUnchanged(t *testing.T) {
	f := newVisitorGrantFixture(t)
	token := f.token(t, grantTestOtherIssuer, "sub-cyd", "cyd@other.example", boolPtr(true))
	identity, _, code := f.identityThroughMiddleware(t, token)
	require.Equal(t, http.StatusOK, code)
	require.IsType(t, &FederatedUserIdentity{}, identity)

	for _, path := range []string{"/api/v1/auth/me", "/api/v1/users/me/groups"} {
		visitorSetExperiment(t, f.srv, true)
		on := f.federatedGet(t, path, token)
		visitorSetExperiment(t, f.srv, false)
		off := f.federatedGet(t, path, token)
		assert.Equal(t, off.Code, on.Code, "%s", path)
		assert.Equal(t, off.Body.String(), on.Body.String(), "%s", path)
		assert.NotContains(t, on.Body.String(), visitorRouteRefusalMessage, "%s", path)
	}

	rec := f.federatedGet(t, "/api/v1/auth/me", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var me UserResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	assert.Equal(t, grantTestOtherIssuer+":sub-cyd", me.ID, "the verified identity, not a local account")
}

// allow_visitor_grants is checked on every visitor request: turning it off
// refuses a linked, granted visitor with the uniform refusal and keeps the
// grant; turning it back on admits the visitor again with no grant change.
func TestFederatedVisitor_IssuerFlagCheckedOnEveryRequest(t *testing.T) {
	f := newVisitorGrantFixture(t)
	v := f.grantOK(t, grantTestIssuer, "uma@corp.example")
	token := f.token(t, grantTestIssuer, "sub-uma", "uma@corp.example", boolPtr(true))
	identity, _, code := f.identityThroughMiddleware(t, token)
	require.Equal(t, http.StatusOK, code)
	require.IsType(t, &VisitorIdentity{}, identity)

	f.setIssuerAcceptsVisitors(t, grantTestIssuer, false)
	rec := f.middlewareResponse(t, token)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeForbidden, resp.Error.Code)
	assert.Equal(t, visitorRouteRefusalMessage, resp.Error.Message, "the uniform refusal")

	listed := f.list(t)
	require.Len(t, listed, 1)
	assert.Equal(t, v.BindingID, listed[0].BindingID, "the grant is kept")
	assert.False(t, listed[0].IssuerAccepted)
	rec = f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "email": "vic@corp.example"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "no new grant for the issuer while it is off: %s", rec.Body.String())

	f.setIssuerAcceptsVisitors(t, grantTestIssuer, true)
	identity, _, code = f.identityThroughMiddleware(t, token)
	require.Equal(t, http.StatusOK, code)
	visitor, ok := identity.(*VisitorIdentity)
	require.True(t, ok, "identity = %T, want *VisitorIdentity", identity)
	assert.Equal(t, v.ID, visitor.ID())
	listed = f.list(t)
	require.Len(t, listed, 1)
	assert.Equal(t, v.BindingID, listed[0].BindingID, "no grant change in between")
	assert.True(t, listed[0].IssuerAccepted)
}

// errVisitorAuditFault is visitorAuditFaultStore's injected failure.
var errVisitorAuditFault = errors.New("injected visitor audit write fault")

// visitorAuditFaultStore fails the audit write of a visitor grant, revoke
// or first-contact link, inside transactions too, once its switch is
// armed.
type visitorAuditFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *visitorAuditFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&visitorAuditFaultStore{Store: tx, fault: s.fault})
	})
}

func (s *visitorAuditFaultStore) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if s.fault.Active() {
		switch r.MutationType {
		case mutationTypeProjectVisitorGrant, mutationTypeProjectVisitorRevoke, mutationTypeVisitorLinked:
			return errVisitorAuditFault
		}
	}
	return s.Store.CreateMutationAudit(ctx, r)
}

// A grant, a revoke and a first-contact link each write their audit record
// in the transaction of their change: when the audit write fails, nothing
// of the change remains.
func TestProjectVisitors_AuditFailureLeavesNoChange(t *testing.T) {
	var fault *storeFaultSwitch
	f := newVisitorGrantFixture(t, func(srv *Server) {
		_, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *visitorAuditFaultStore {
			return &visitorAuditFaultStore{Store: inner, fault: sw}
		})
	})
	ctx := context.Background()
	kept := f.grantOK(t, grantTestIssuer, "olga@corp.example")
	pending := f.grantOK(t, grantTestIssuer, "pia@corp.example")
	fault.Arm()

	t.Run("grant", func(t *testing.T) {
		rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "email": "quinn@corp.example"})
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		_, err := f.st.GetFederatedUserByEmail(ctx, grantTestIssuer, "quinn@corp.example")
		assert.ErrorIs(t, err, store.ErrNotFound, "no pending account remains")
		bindings, err := f.st.ListVisitorRoleBindings(ctx, f.projectID)
		require.NoError(t, err)
		assert.Len(t, bindings, 2, "no binding remains")
	})

	t.Run("first contact", func(t *testing.T) {
		rec := f.middlewareResponse(t, f.token(t, grantTestIssuer, "sub-pia", "pia@corp.example", boolPtr(true)))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		acct, err := f.st.GetUser(ctx, pending.ID)
		require.NoError(t, err)
		assert.Equal(t, store.UserStatusPending, acct.Status, "the account stays pending")
		_, err = f.st.GetFederatedUserBySubject(ctx, grantTestIssuer, "sub-pia")
		assert.ErrorIs(t, err, store.ErrNotFound, "the subject is not linked")
		links, err := f.st.GetExternalIdentitiesByUserID(ctx, pending.ID)
		require.NoError(t, err)
		assert.Empty(t, links)
	})

	t.Run("revoke", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+kept.ID, nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		bindings, err := f.st.ListVisitorRoleBindings(ctx, f.projectID)
		require.NoError(t, err)
		ids := make([]string, 0, len(bindings))
		for _, b := range bindings {
			ids = append(ids, b.ID)
		}
		assert.Contains(t, ids, kept.BindingID, "the binding remains")
	})
}
