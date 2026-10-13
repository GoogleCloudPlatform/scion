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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for workload-bound issuer delegations (ptone/scion#4240, phase X2).
// The Google signature check itself is GoogleCredentialValidator's (tested
// in google_credential_validator_test.go); here x2FakeGoogle stands in for
// it with the same refusal classes, so the arm's own checks (strict
// audience, pinned subject, allowlist, delegation, grantor, route
// confinement) run for real against a SQLite store.

const (
	x2HubID    = "x2-test-hub"
	x2Sub      = "100000000000000000077"
	x2OtherSub = "100000000000000000078"
	x2SAEmail  = "t-issuer@x2-proj.iam.gserviceaccount.com"
	x2GoodSig  = "Z29vZ2xlLXNpZ25hdHVyZQ"
)

// x2FakeGoogle is a GoogleCredentialValidator over unsigned test JWTs: the
// third segment must be x2GoodSig ("signed by Google"), the issuer must be
// Google, the audience must be allowed and the token unexpired. It counts
// calls.
type x2FakeGoogle struct {
	now   func() time.Time
	calls atomic.Int64
}

func (f *x2FakeGoogle) ValidateIDToken(_ context.Context, token string, allowed []string) (*ValidatedGoogleIdentity, error) {
	f.calls.Add(1)
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[2] != x2GoodSig {
		return nil, ErrGoogleInvalidCredential
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrGoogleInvalidCredential
	}
	var c struct {
		Iss   string          `json:"iss"`
		Aud   json.RawMessage `json:"aud"`
		Sub   string          `json:"sub"`
		Email string          `json:"email"`
		Exp   int64           `json:"exp"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrGoogleInvalidCredential
	}
	if c.Iss != googleIssuerHTTPS {
		return nil, ErrGoogleUntrustedIssuer
	}
	var auds []string
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		auds = []string{one}
	} else if json.Unmarshal(c.Aud, &auds) != nil {
		return nil, ErrGoogleInvalidCredential
	}
	matched := ""
	for _, a := range auds {
		for _, ok := range allowed {
			if a == ok && matched == "" {
				matched = a
			}
		}
	}
	if matched == "" {
		return nil, ErrGoogleUntrustedAudience
	}
	now := time.Now()
	if f.now != nil {
		now = f.now()
	}
	if !now.Before(time.Unix(c.Exp, 0)) {
		return nil, ErrGoogleExpiredCredential
	}
	return &ValidatedGoogleIdentity{
		Subject: c.Sub, Email: c.Email, EmailVerified: true, Issuer: googleCanonicalIssuer,
		Audience: matched, UpstreamExpiry: time.Unix(c.Exp, 0),
		IsServiceAccount: strings.HasSuffix(c.Email, ".gserviceaccount.com"),
	}, nil
}

func (f *x2FakeGoogle) ValidateAccessToken(context.Context, string, []string) (*ValidatedGoogleIdentity, error) {
	f.calls.Add(1)
	return nil, ErrGoogleInvalidCredential
}

// x2TokenOpts shape a test workload token.
type x2TokenOpts struct {
	aud   any // string or []string
	sub   string
	email string
	iss   string
	exp   time.Time
	iat   time.Time
	sig   string
}

func x2Token(t *testing.T, o x2TokenOpts) string {
	t.Helper()
	if o.aud == nil {
		o.aud = testIssuerAudience(x2HubID)
	}
	if o.sub == "" {
		o.sub = x2Sub
	}
	if o.email == "" {
		o.email = x2SAEmail
	}
	if o.iss == "" {
		o.iss = googleIssuerHTTPS
	}
	if o.iat.IsZero() {
		o.iat = time.Now().Add(-time.Minute).Truncate(time.Second)
	}
	if o.exp.IsZero() {
		o.exp = time.Now().Add(time.Hour)
	}
	if o.sig == "" {
		o.sig = x2GoodSig
	}
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	payload, err := json.Marshal(map[string]any{
		"iss": o.iss, "aud": o.aud, "sub": o.sub, "azp": o.sub, "email": o.email, "email_verified": true,
		"iat": o.iat.Unix(), "exp": o.exp.Unix(),
	})
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + o.sig
}

// x2Env is a test hub with the member tier on and a workload issuer
// allowlist.
type x2Env struct {
	srv    *Server
	s      store.Store
	google *x2FakeGoogle
}

// enableWorkloadIssuerForTest installs the workload issuer state New would
// build from cfg.TestIdentityWorkloadIssuers for hub id hubID.
func enableWorkloadIssuerForTest(srv *Server, st store.Store, v GoogleCredentialValidator, hubID string, subs ...string) {
	allow := map[string]string{}
	for _, sub := range subs {
		allow[sub] = config.WorkloadIssuerTierMember
	}
	wi := &workloadIssuerAuthenticator{
		audience:  testIssuerAudience(hubID),
		allowlist: allow,
		validator: v,
		store:     st,
		now:       func() time.Time { return srv.testIdentities.clock() },
	}
	srv.workloadIssuer = wi
	srv.authConfig.WorkloadIssuer = wi
}

func newX2Env(t *testing.T, enabled bool) *x2Env {
	t.Helper()
	srv, s := newTestIdentityServer(t, enabled)
	g := &x2FakeGoogle{now: func() time.Time { return srv.testIdentities.clock() }}
	enableWorkloadIssuerForTest(srv, s, g, x2HubID, x2Sub, x2OtherSub)
	return &x2Env{srv: srv, s: s, google: g}
}

// grantor returns a live human super-admin user (a human issuer).
func (e *x2Env) grantor(t *testing.T, name string) *store.User {
	t.Helper()
	id := hubConfigTokenUser(t, e.s, name, store.SystemRoleSuperAdmin)
	u, err := e.s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

// bindingGrantor returns a member holding test_identity.issue through a
// system role binding, and that binding's ID.
func (e *x2Env) bindingGrantor(t *testing.T, name string) (*store.User, string) {
	t.Helper()
	ctx := context.Background()
	u := &store.User{ID: tid(name), Email: tid(name) + "@test.com", DisplayName: name, Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, e.s.CreateUser(ctx, u))
	ensureHubMembership(ctx, e.s, u.ID)
	rd, err := e.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		ID: api.NewUUID(), Name: tid(name + "-role"), Description: "x2 issuer",
		ScopeType: store.RoleScopeSystem, Permissions: []string{permissionTestIdentityIssue},
	})
	require.NoError(t, err)
	b, err := e.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: u.ID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	return u, b.ID
}

// delegate creates a delegation as user's session and returns it.
func (e *x2Env) delegate(t *testing.T, user *store.User, body map[string]any) TestIdentityDelegationView {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	if _, ok := body["workloadSub"]; !ok {
		body["workloadSub"] = x2Sub
	}
	if _, ok := body["scopes"]; !ok {
		body["scopes"] = []string{"test_identity:issue"}
	}
	rec := doRequestAsUser(t, e.srv, user, http.MethodPost, "/api/v1/test-identity-delegations", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var v TestIdentityDelegationView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	return v
}

// workload sends a request with a workload token and delegation id.
func (e *x2Env) workload(t *testing.T, method, path, token, delegationID string, body any, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	if body != nil {
		var err error
		buf, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if delegationID != "" {
		req.Header.Set(testIssuerDelegationHeader, delegationID)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// mint issues a member through delegation d with a fresh valid token.
func (e *x2Env) mint(t *testing.T, delegationID string) *httptest.ResponseRecorder {
	t.Helper()
	return e.workload(t, http.MethodPost, "/api/v1/test-identities", x2Token(t, x2TokenOpts{}), delegationID, map[string]string{"role": "member"}, nil)
}

func (e *x2Env) liveFixtures(t *testing.T) int {
	t.Helper()
	n, err := e.s.CountLiveTestFixtureUsers(context.Background(), "", e.srv.testIdentities.clock())
	require.NoError(t, err)
	return n
}

func (e *x2Env) userCount(t *testing.T) int {
	t.Helper()
	res, err := e.s.ListUsers(context.Background(), store.UserFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	return len(res.Items)
}

// --- AC X2-1: gate -----------------------------------------------------------

func TestWorkloadDelegation_FlagOff(t *testing.T) {
	e := newX2Env(t, false)
	admin := e.grantor(t, "x2-off-admin")

	rec := doRequestAsUser(t, e.srv, admin, http.MethodPost, "/api/v1/test-identity-delegations",
		map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue"}})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, e.srv, admin, http.MethodGet, "/api/v1/test-identity-delegations", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, e.srv, admin, http.MethodDelete, "/api/v1/test-identity-delegations/"+generateID(), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// A workload token on the issuance routes: 401, whatever the delegation.
	d := &store.TestIdentityDelegation{GrantorID: admin.ID, WorkloadSub: x2Sub, Scope: testIdentityDelegationScope, MemberCap: 5,
		Created: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, e.s.CreateTestIdentityDelegation(context.Background(), d))
	rec = e.mint(t, d.ID)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	rec = e.workload(t, http.MethodGet, "/api/v1/test-identities", x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Zero(t, e.liveFixtures(t))
	assert.Zero(t, e.google.calls.Load(), "the token is not even verified while the tier is off")
}

// --- AC X2-2: delegation create authorization and validation -----------------

func TestTestIdentityDelegation_CreateAuthorization(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	grantor := e.grantor(t, "x2-create-grantor")

	// A human session holding test_identity.issue: 201, with an audit row.
	d := e.delegate(t, grantor, map[string]any{"label": "window-1", "memberCap": 3, "ttlSeconds": 3600})
	assert.Equal(t, grantor.ID, d.GrantorID)
	assert.Equal(t, x2Sub, d.WorkloadSub)
	assert.Equal(t, "test_identity:issue", d.Scope)
	assert.Equal(t, 3, d.MemberCap)
	assert.Equal(t, "window-1", d.Label)
	assert.True(t, d.Live)
	assert.WithinDuration(t, d.Created.Add(time.Hour), d.ExpiresAt, time.Second)
	audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityDelegationCreateMutation, TargetID: d.ID})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	assert.Equal(t, grantor.ID, audits[0].ActorPrincipalID)
	assert.Contains(t, audits[0].AfterSummary, `"workload_sub":"`+x2Sub+`"`)

	// Defaults: memberCap 5, 8 h.
	def := e.delegate(t, grantor, nil)
	assert.Equal(t, testIdentityPerIssuerCap, def.MemberCap)
	assert.WithinDuration(t, def.Created.Add(8*time.Hour), def.ExpiresAt, time.Second)

	countRows := func() int {
		rows, err := e.s.ListTestIdentityDelegations(ctx, "", 0)
		require.NoError(t, err)
		return len(rows)
	}
	before := countRows()
	valid := map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue"}}

	// A PAT carrying test_identity:issue: 403 (session only).
	_, pat := tiIssuer(t, e.srv, e.s, "x2-create-pat")
	rec := doRequestWithToken(t, e.srv, pat, http.MethodPost, "/api/v1/test-identity-delegations", valid)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A real agent, whose token authenticates: exactly 403 on every
	// delegation route, and no row.
	_, _, _, agentProject := setupCredentialTestServerOn(t, e.srv, e.s)
	agentID := tid("x2-agent")
	createCredTestAgent(t, e.s, agentID, agentProject.ID, tid("user-cred-test"))
	agentTok, err := e.srv.GenerateAgentToken(agentID, agentProject.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	agentRequest := func(method, path string, body any) *httptest.ResponseRecorder {
		var buf []byte
		if body != nil {
			buf, err = json.Marshal(body)
			require.NoError(t, err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Agent-Token", agentTok)
		rr := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rr, req)
		return rr
	}
	sanity := agentRequest(http.MethodGet, "/api/v1/agents/"+agentID, nil)
	require.NotEqual(t, http.StatusUnauthorized, sanity.Code, "the agent token must authenticate: %s", sanity.Body.String())
	require.NotEqual(t, http.StatusServiceUnavailable, sanity.Code, sanity.Body.String())
	rec = agentRequest(http.MethodPost, "/api/v1/test-identity-delegations", valid)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = agentRequest(http.MethodGet, "/api/v1/test-identity-delegations", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = agentRequest(http.MethodDelete, "/api/v1/test-identity-delegations/"+d.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	stillLive, err := e.s.GetTestIdentityDelegation(ctx, d.ID)
	require.NoError(t, err)
	assert.Nil(t, stillLive.RevokedAt, "the agent did not revoke the delegation")

	// A test identity (kind test_fixture) session: 403.
	fixture := tiIssue(t, e.srv, pat, map[string]string{"role": "member"})
	rec = doRequestWithToken(t, e.srv, fixture.AccessToken, http.MethodPost, "/api/v1/test-identity-delegations", valid)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A member session without test_identity.issue: 403.
	member := &store.User{ID: tid("x2-create-member"), Email: tid("x2-create-member") + "@test.com", DisplayName: "m", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, e.s.CreateUser(ctx, member))
	ensureHubMembership(ctx, e.s, member.ID)
	rec = doRequestAsUser(t, e.srv, member, http.MethodPost, "/api/v1/test-identity-delegations", valid)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A workload delegation cannot create a delegation (route-confined: 401).
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identity-delegations", x2Token(t, x2TokenOpts{}), d.ID, valid, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	// 422s.
	for name, tc := range map[string]struct {
		body   map[string]any
		reason string
	}{
		"sub not allow-listed": {map[string]any{"workloadSub": "999", "scopes": []string{"test_identity:issue"}}, testIdentityDelegationReasonSub},
		"two scopes":           {map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue", "test_identity:issue"}}, testIdentityDelegationReasonScope},
		"no scope":             {map[string]any{"workloadSub": x2Sub, "scopes": []string{}}, testIdentityDelegationReasonScope},
		"admin scope":          {map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue_admin"}}, testIdentityDelegationReasonAdminTier},
		"super-admin scope":    {map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue_super_admin"}}, testIdentityDelegationReasonAdminTier},
		"other scope":          {map[string]any{"workloadSub": x2Sub, "scopes": []string{"hub_health:read"}}, testIdentityDelegationReasonScope},
		"age above maximum":    {map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue"}, "ttlSeconds": int64(testIdentityDelegationMaxAge/time.Second) + 1}, testIdentityDelegationReasonTTL},
		"age below minimum":    {map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue"}, "ttlSeconds": 30}, testIdentityDelegationReasonTTL},
		"cap above 5":          {map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue"}, "memberCap": 6}, testIdentityDelegationReasonCap},
		"negative cap":         {map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue"}, "memberCap": -1}, testIdentityDelegationReasonCap},
		"control char label":   {map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue"}, "label": "a\nb"}, testIdentityDelegationReasonLabel},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doRequestAsUser(t, e.srv, grantor, http.MethodPost, "/api/v1/test-identity-delegations", tc.body)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.reason)
		})
	}
	// An unknown body field: 400.
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodPost, "/api/v1/test-identity-delegations",
		map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue"}, "grantorId": member.ID})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, before, countRows(), "no refused request created a delegation")

	// No workload issuer configured: 422.
	e.srv.workloadIssuer = nil
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodPost, "/api/v1/test-identity-delegations", valid)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), testIdentityDelegationReasonUnavailable)
}

func TestTestIdentityDelegation_ListAndRevoke(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	a, _ := e.bindingGrantor(t, "x2-list-a")
	b, _ := e.bindingGrantor(t, "x2-list-b")
	admin := e.grantor(t, "x2-list-admin")
	da := e.delegate(t, a, nil)
	db := e.delegate(t, b, nil)

	list := func(u *store.User) []TestIdentityDelegationView {
		rec := doRequestAsUser(t, e.srv, u, http.MethodGet, "/api/v1/test-identity-delegations", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListTestIdentityDelegationsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.Items
	}
	got := list(a)
	require.Len(t, got, 1)
	assert.Equal(t, da.ID, got[0].ID)
	assert.Len(t, list(admin), 2, "an unscoped admin session sees every delegation")

	// B cannot revoke A's delegation: 404, and it stays live.
	rec := doRequestAsUser(t, e.srv, b, http.MethodDelete, "/api/v1/test-identity-delegations/"+da.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	d, err := e.s.GetTestIdentityDelegation(ctx, da.ID)
	require.NoError(t, err)
	assert.Nil(t, d.RevokedAt)
	rec = doRequestAsUser(t, e.srv, a, http.MethodDelete, "/api/v1/test-identity-delegations/"+generateID(), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// The grantor revokes; a second revoke is 204 and writes no second audit.
	rec = doRequestAsUser(t, e.srv, a, http.MethodDelete, "/api/v1/test-identity-delegations/"+da.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, e.srv, a, http.MethodDelete, "/api/v1/test-identity-delegations/"+da.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityDelegationRevokeMutation, TargetID: da.ID})
	require.NoError(t, err)
	assert.Len(t, audits, 1)
	got = list(a)
	require.Len(t, got, 1)
	assert.False(t, got[0].Live)
	assert.Equal(t, a.ID, got[0].RevokedBy)

	// An admin session revokes another grantor's delegation.
	rec = doRequestAsUser(t, e.srv, admin, http.MethodDelete, "/api/v1/test-identity-delegations/"+db.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// A PAT cannot list.
	_, pat := tiIssuer(t, e.srv, e.s, "x2-list-pat")
	rec = doRequestWithToken(t, e.srv, pat, http.MethodGet, "/api/v1/test-identity-delegations", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// --- AC X2-3: issuance and its audit -----------------------------------------

func TestWorkloadDelegation_IssueAndAudit(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	grantor := e.grantor(t, "x2-issue-grantor")
	d := e.delegate(t, grantor, nil)
	usersBefore := e.userCount(t)

	iat := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	token := x2Token(t, x2TokenOpts{iat: iat})
	rec := e.workload(t, http.MethodPost, "/api/v1/test-identities", token, d.ID,
		map[string]string{"role": "viewer", "purpose": "x2 run"}, map[string]string{testIssuerRunLabelHeader: "run-42"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := tiDecode(t, rec)
	assert.Equal(t, grantor.ID, resp.Identity.IssuedBy)
	assert.Equal(t, d.ID, resp.Identity.IssuerDelegationID)
	assert.Equal(t, store.UserRoleViewer, resp.Identity.Role)

	row, err := e.s.GetUser(ctx, resp.Identity.ID)
	require.NoError(t, err)
	assert.True(t, row.IsTestFixture())
	require.NotNil(t, row.IssuerDelegationID)
	assert.Equal(t, d.ID, *row.IssuerDelegationID)

	audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityIssueMutation, TargetID: resp.Identity.ID})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	a := audits[0]
	assert.Equal(t, d.ID, a.ActorCredentialID, "C4: the audit records the delegation id as the credential")
	assert.Equal(t, string(CredentialKindWorkloadDelegation), a.ActorCredentialType)
	assert.Equal(t, grantor.ID, a.ActorPrincipalID)
	var summary struct {
		Workload testIdentityWorkloadAudit `json:"workload_delegation"`
	}
	require.NoError(t, json.Unmarshal([]byte(a.AfterSummary), &summary))
	assert.Equal(t, testIdentityWorkloadAudit{
		DelegationID: d.ID, Grantor: grantor.ID, WorkloadSub: x2Sub,
		TokenIAT: iat.UTC().Format(time.RFC3339), RunLabel: "run-42",
	}, summary.Workload)
	// No token substring: neither segment of the workload token nor the
	// issued access token.
	for _, seg := range append(strings.Split(token, "."), strings.Split(resp.AccessToken, ".")...) {
		if len(seg) >= 8 {
			assert.NotContains(t, a.AfterSummary, seg)
			assert.NotContains(t, a.BeforeSummary, seg)
		}
	}

	// Exactly one user row was created (the fixture); none for the workload.
	assert.Equal(t, usersBefore+1, e.userCount(t))
	_, err = e.s.GetUserByEmail(ctx, x2SAEmail)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// The issued fixture token works and is the fixture, not the grantor.
	me := tiAuthMe(t, e.srv, resp.AccessToken)
	require.Equal(t, http.StatusOK, me.Code, me.Body.String())
	assert.Contains(t, me.Body.String(), resp.Identity.Email)

	// An invalid run label is refused.
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities", x2Token(t, x2TokenOpts{}), d.ID,
		nil, map[string]string{testIssuerRunLabelHeader: strings.Repeat("r", testIssuerRunLabelMaxRunes+1)})
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}

// --- AC X2-4 / advisor (a): refusal matrix ------------------------------------

func TestWorkloadDelegation_RefusalMatrix(t *testing.T) {
	e := newX2Env(t, true)
	grantor := e.grantor(t, "x2-matrix-grantor")
	d := e.delegate(t, grantor, nil)
	other := e.delegate(t, grantor, map[string]any{"workloadSub": x2OtherSub})
	_ = other

	cases := map[string]struct {
		token        string
		delegationID string
		want         int
	}{
		"IAP audience":           {x2Token(t, x2TokenOpts{aud: "iap-client.apps.googleusercontent.com"}), d.ID, http.StatusUnauthorized},
		"another hub's audience": {x2Token(t, x2TokenOpts{aud: testIssuerAudience("other-hub")}), d.ID, http.StatusUnauthorized},
		"two audiences":          {x2Token(t, x2TokenOpts{aud: []string{testIssuerAudience(x2HubID), "extra"}}), d.ID, http.StatusUnauthorized},
		"wrong sub (allow-listed, other delegation's)": {x2Token(t, x2TokenOpts{sub: x2OtherSub}), d.ID, http.StatusUnauthorized},
		"unlisted sub":          {x2Token(t, x2TokenOpts{sub: "123"}), d.ID, http.StatusUnauthorized},
		"expired token":         {x2Token(t, x2TokenOpts{exp: time.Now().Add(-time.Minute)}), d.ID, http.StatusUnauthorized},
		"unsigned token":        {x2Token(t, x2TokenOpts{sig: "none"}), d.ID, http.StatusUnauthorized},
		"other issuer":          {x2Token(t, x2TokenOpts{iss: "https://issuer.example.com"}), d.ID, http.StatusUnauthorized},
		"not a service account": {x2Token(t, x2TokenOpts{email: "someone@example.com"}), d.ID, http.StatusUnauthorized},
		"missing delegation id": {x2Token(t, x2TokenOpts{}), "", http.StatusUnauthorized},
		"unknown delegation id": {x2Token(t, x2TokenOpts{}), generateID(), http.StatusUnauthorized},
		"malformed delegation":  {x2Token(t, x2TokenOpts{}), "not-a-uuid", http.StatusUnauthorized},
		"no bearer":             {"", d.ID, http.StatusUnauthorized},
		"opaque bearer":         {"scion_pat_notreal", d.ID, http.StatusUnauthorized},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			before := e.liveFixtures(t)
			rec := e.workload(t, http.MethodPost, "/api/v1/test-identities", tc.token, tc.delegationID, map[string]string{"role": "member"}, nil)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
			assert.Equal(t, before, e.liveFixtures(t), "no row created")
		})
	}

	// A refused request names no detail beyond the generic message.
	rec := e.workload(t, http.MethodPost, "/api/v1/test-identities", x2Token(t, x2TokenOpts{sub: x2OtherSub}), d.ID, nil, nil)
	assert.NotContains(t, rec.Body.String(), x2OtherSub)
}

// C3: a delegation revoked between two mints stops the second at once.
func TestWorkloadDelegation_RevokedBetweenMints(t *testing.T) {
	e := newX2Env(t, true)
	grantor := e.grantor(t, "x2-revoke-grantor")
	d := e.delegate(t, grantor, nil)
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)
	rec := doRequestAsUser(t, e.srv, grantor, http.MethodDelete, "/api/v1/test-identity-delegations/"+d.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	before := e.liveFixtures(t)
	rec = e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, before, e.liveFixtures(t))
}

// C3 inside the issuance transaction: a delegation revoked after the
// request authenticated mints nothing (recheckWorkloadDelegation).
func TestWorkloadDelegation_RevokedInsideTransaction(t *testing.T) {
	e := newX2Env(t, true)
	grantor := e.grantor(t, "x2-revoke-tx-grantor")
	d := e.delegate(t, grantor, nil)
	st := &workloadDelegationState{delegation: &store.TestIdentityDelegation{ID: d.ID, GrantorID: grantor.ID}, sub: x2Sub}
	ctx := context.Background()
	now := e.srv.testIdentities.clock()
	require.NoError(t, e.srv.recheckWorkloadDelegation(ctx, e.s, st, now))
	_, rerr := e.s.RevokeTestIdentityDelegation(ctx, d.ID, grantor.ID, now)
	require.NoError(t, rerr)
	assert.ErrorIs(t, e.srv.recheckWorkloadDelegation(ctx, e.s, st, now), errTestIdentityDelegationNotLive)
	assert.ErrorIs(t, e.srv.recheckWorkloadDelegation(ctx, e.s, &workloadDelegationState{
		delegation: &store.TestIdentityDelegation{ID: generateID(), GrantorID: grantor.ID}, sub: x2Sub}, now), errTestIdentityDelegationNotLive)
}

// C2: a delegation past its stored expiry, or past the hard maximum age
// from its creation, is refused (server-side age).
func TestWorkloadDelegation_PastItsAge(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	grantor := e.grantor(t, "x2-age-grantor")
	d := e.delegate(t, grantor, map[string]any{"ttlSeconds": 120})
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)

	e.srv.testIdentities.now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	e.google.now = time.Now // the workload token itself stays valid
	rec := e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	e.srv.testIdentities.now = time.Now

	// A row whose stored expiry is later than the maximum age allows (only
	// the store could write it) is still refused once older than the
	// maximum age.
	old := &store.TestIdentityDelegation{GrantorID: grantor.ID, WorkloadSub: x2Sub, Scope: testIdentityDelegationScope, MemberCap: 5,
		Created: time.Now().Add(-testIdentityDelegationMaxAge - time.Minute), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, e.s.CreateTestIdentityDelegation(ctx, old))
	rec = e.mint(t, old.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A scope other than test_identity:issue on a stored row is refused.
	odd := &store.TestIdentityDelegation{GrantorID: grantor.ID, WorkloadSub: x2Sub, Scope: "test_identity:issue_admin", MemberCap: 1,
		Created: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, e.s.CreateTestIdentityDelegation(ctx, odd))
	rec = e.mint(t, odd.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// C1 / advisor (a): the grantor's live bindings are read at every mint. A
// grantor who loses test_identity.issue stops minting at once; the
// delegation's frozen scope never grants beyond it (ceiling only).
func TestWorkloadDelegation_GrantorLosesPermissionBetweenMints(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	grantor, bindingID := e.bindingGrantor(t, "x2-c1-grantor")
	d := e.delegate(t, grantor, nil)
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)

	require.NoError(t, e.s.DeleteRoleBinding(ctx, bindingID))
	before := e.liveFixtures(t)
	rec := e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, before, e.liveFixtures(t))
}

// Ceiling only: the delegation's identity holds only test_identity.issue,
// however much authority the grantor has. A super-admin grantor's
// delegation cannot reach the issuance routes as an admin "sees all"
// caller, and the identity it builds is scoped.
func TestWorkloadDelegation_ScopeIsACeilingOnly(t *testing.T) {
	e := newX2Env(t, true)
	admin := e.grantor(t, "x2-ceiling-admin")
	other := e.grantor(t, "x2-ceiling-other")
	d := e.delegate(t, admin, nil)
	// An identity another issuer made: a super-admin session would see it.
	_, pat := tiIssuer(t, e.srv, e.s, "x2-ceiling-pat")
	foreign := tiIssue(t, e.srv, pat, nil)
	_ = other

	rec := e.workload(t, http.MethodGet, "/api/v1/test-identities", x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), foreign.Identity.ID, "a delegation never inherits the admin's see-all scope")
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/"+foreign.Identity.ID+"/token", x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+foreign.Identity.ID, x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// The arm's identity: hub boundary, the one scope, the delegation id.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/test-identities", nil)
	req.Header.Set("Authorization", "Bearer "+x2Token(t, x2TokenOpts{}))
	req.Header.Set(testIssuerDelegationHeader, d.ID)
	auth, refusal := e.srv.workloadIssuer.authenticate(req.Context(), req, true)
	require.Nil(t, refusal)
	scoped := auth.identity
	require.NotNil(t, scoped)
	assert.Equal(t, admin.ID, scoped.ID())
	assert.Equal(t, d.ID, scoped.CredentialID())
	assert.Equal(t, []string{"test_identity.issue"}, scoped.Ceiling().PermissionIDs)
	assert.Equal(t, BoundaryKindHub, scoped.Boundary().Kind)
	assert.False(t, IsUnscopedLocalPlatformAdmin(scoped))
	assert.Equal(t, CredentialKindWorkloadDelegation, auth.credential.Kind)
	assert.Equal(t, d.ID, auth.credential.ID)
	assert.Equal(t, d.ID, auth.state.delegation.ID)
}

// --- AC X2-5 / advisor (b): route confinement, no user row, no resolver -----

func TestWorkloadDelegation_RouteConfined(t *testing.T) {
	e := newX2Env(t, true)
	grantor := e.grantor(t, "x2-route-grantor")
	d := e.delegate(t, grantor, nil)
	usersBefore := e.userCount(t)

	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/auth/me"},
		{http.MethodGet, "/api/v1/projects"},
		{http.MethodPost, "/api/v1/projects"},
		{http.MethodGet, "/api/v1/test-identity-delegations"},
		{http.MethodPost, "/api/v1/test-identity-delegations"},
		{http.MethodDelete, "/api/v1/test-identity-delegations/" + d.ID},
		{http.MethodPut, "/api/v1/test-identities"},
		{http.MethodGet, "/api/v1/test-identities/" + generateID()},
		{http.MethodPost, "/api/v1/test-identities/a/b/token"},
		{http.MethodGet, "/api/v1/test-identitiesx"},
	} {
		for _, header := range []bool{true, false} {
			id := ""
			if header {
				id = d.ID
			}
			rec := e.workload(t, rt.method, rt.path, x2Token(t, x2TokenOpts{}), id, nil, nil)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s %s header=%v: %s", rt.method, rt.path, header, rec.Body.String())
		}
	}
	assert.Equal(t, usersBefore, e.userCount(t), "no user row was created for the workload")
	assert.Zero(t, e.google.calls.Load(), "off-route requests are refused before any verification")
}

func TestIsTestIdentityIssuanceRoute(t *testing.T) {
	for _, tc := range []struct {
		method, target string
		want           bool
	}{
		{http.MethodPost, "/api/v1/test-identities", true},
		{http.MethodGet, "/api/v1/test-identities", true},
		{http.MethodPost, "/api/v1/test-identities/abc/token", true},
		{http.MethodPost, "/api/v1/test-identities/abc/web-login-code", true},
		{http.MethodGet, "/api/v1/test-identities/abc/web-login-code", false},
		{http.MethodDelete, "/api/v1/test-identities/abc/web-login-code", false},
		{http.MethodPost, "/api/v1/test-identities//web-login-code", false},
		{http.MethodPost, "/api/v1/test-identities/a/b/web-login-code", false},
		{http.MethodPost, "/api/v1/test-identities/a%2Fb/web-login-code", false},
		{http.MethodDelete, "/api/v1/test-identities/abc", true},
		{http.MethodDelete, "/api/v1/test-identities", false},
		{http.MethodGet, "/api/v1/test-identities/abc", false},
		{http.MethodPost, "/api/v1/test-identities/abc", false},
		{http.MethodPost, "/api/v1/test-identities//token", false},
		{http.MethodDelete, "/api/v1/test-identities/../users/x", false},
		{http.MethodDelete, "/api/v1/test-identities/a%2Fb", false},
		{http.MethodPost, "/api/v1/test-identity-delegations", false},
		{http.MethodGet, "/api/v1/test-identities/", false},
	} {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		assert.Equal(t, tc.want, isTestIdentityIssuanceRoute(req), "%s %s", tc.method, tc.target)
	}
}

// Advisor (b): with Google user trust configured on the external-bearer
// path, a workload token (with or without the delegation header) never
// reaches the Google validator of that path or the resolver, and no user
// is provisioned. A token for the trusted external audience still works,
// which shows the trust is live in this setup.
func TestWorkloadDelegation_NeverReachesExternalBearerOrResolver(t *testing.T) {
	// The external-bearer validator would accept any token as this Google
	// user, so reaching it at all would provision a user.
	user := &ValidatedGoogleIdentity{Subject: "x2-google-user-sub", Email: "user@gmail.com", EmailVerified: true, Issuer: googleCanonicalIssuer,
		Audience: externalBearerTestAudience, UpstreamExpiry: time.Now().Add(time.Hour)}
	external := &countingGoogleValidator{fakeGoogleValidator: fakeGoogleValidator{idTokenResult: user, accessTokenErr: ErrGoogleInvalidCredential}}
	userStore := newFakeUserStore()
	resolver := NewGoogleIdentityResolver(userStore, newMemExtIDStore(), alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, external, resolver)
	cfg.TestIdentitiesEnabled = true
	workloadGoogle := &x2FakeGoogle{}
	cfg.WorkloadIssuer = &workloadIssuerAuthenticator{
		audience: testIssuerAudience(x2HubID), allowlist: map[string]string{x2Sub: config.WorkloadIssuerTierMember},
		validator: workloadGoogle, store: userStore,
	}

	serve := func(path, token, delegationID string) *httptest.ResponseRecorder {
		result := &probeResult{}
		h := UnifiedAuthMiddleware(cfg)(probeHandler(result))
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if delegationID != "" {
			req.Header.Set(testIssuerDelegationHeader, delegationID)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, id := range []string{"", generateID()} {
		rec := serve("/api/v1/auth/me", x2Token(t, x2TokenOpts{}), id)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	}
	assert.Zero(t, external.idTokenCalls.Load()+external.accessTokenCalls.Load(), "the external-bearer validator is never called")
	assert.Empty(t, userStore.users, "no user row is provisioned")

	// Control: a token for the trusted external audience is provisioned.
	rec := serve("/api/v1/auth/me", x2Token(t, x2TokenOpts{aud: externalBearerTestAudience}), "")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Positive(t, external.idTokenCalls.Load())
}

// --- AC X2-6: caps ------------------------------------------------------------

func x2ParallelMint(t *testing.T, e *x2Env, delegationIDs []string, n int) map[int]int {
	t.Helper()
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = e.mint(t, delegationIDs[i%len(delegationIDs)]).Code
		}(i)
	}
	wg.Wait()
	out := map[int]int{}
	for _, c := range codes {
		out[c]++
	}
	return out
}

func TestWorkloadDelegation_CapsPerDelegation(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	grantor := e.grantor(t, "x2-cap-grantor")
	d1 := e.delegate(t, grantor, nil)
	d2 := e.delegate(t, grantor, nil)
	d3 := e.delegate(t, grantor, map[string]any{"memberCap": 2})

	got := x2ParallelMint(t, e, []string{d1.ID}, 6)
	assert.Equal(t, map[int]int{http.StatusCreated: 5, http.StatusTooManyRequests: 1}, got)
	got = x2ParallelMint(t, e, []string{d2.ID}, 6)
	assert.Equal(t, map[int]int{http.StatusCreated: 5, http.StatusTooManyRequests: 1}, got, "a second delegation of the same grantor has its own 5")
	got = x2ParallelMint(t, e, []string{d3.ID}, 4)
	assert.Equal(t, map[int]int{http.StatusCreated: 2, http.StatusTooManyRequests: 2}, got, "the grantor-chosen member cap")
	for id, want := range map[string]int{d1.ID: 5, d2.ID: 5, d3.ID: 2} {
		n, err := e.s.CountLiveTestFixtureUsersByDelegation(ctx, id, time.Now())
		require.NoError(t, err)
		assert.Equal(t, want, n)
	}
	rec := e.mint(t, d3.ID)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Contains(t, rec.Body.String(), testIdentityReasonIssuerCap)
	assert.Contains(t, rec.Body.String(), `"limit":2`)
}

func TestWorkloadDelegation_HubCapAcrossDelegations(t *testing.T) {
	e := newX2Env(t, true)
	grantor := e.grantor(t, "x2-hubcap-grantor")
	for i := 0; i < testIdentityHubCap-2; i++ {
		tiStoreFixture(t, e.s, generateID(), time.Now().Add(time.Hour))
	}
	d1 := e.delegate(t, grantor, nil)
	d2 := e.delegate(t, grantor, nil)
	got := x2ParallelMint(t, e, []string{d1.ID, d2.ID}, 6)
	assert.Equal(t, map[int]int{http.StatusCreated: 2, http.StatusTooManyRequests: 4}, got)
	assert.Equal(t, testIdentityHubCap, e.liveFixtures(t))
}

func TestWorkloadDelegation_RateLimitKeyedPerDelegation(t *testing.T) {
	e := newX2Env(t, true)
	grantor := e.grantor(t, "x2-rate-grantor")
	d1 := e.delegate(t, grantor, nil)
	d2 := e.delegate(t, grantor, nil)
	e.srv.testIdentities.limiter = NewGCPTokenRateLimiter(0.0001, 1)
	assert.Equal(t, http.StatusCreated, e.mint(t, d1.ID).Code)
	rec := e.mint(t, d1.ID)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Contains(t, rec.Body.String(), testIdentityReasonRateLimited)
	assert.Equal(t, http.StatusCreated, e.mint(t, d2.ID).Code, "another delegation has its own bucket")
}

// --- AC X2-8: management scope ------------------------------------------------

func TestWorkloadDelegation_ManagementScopedToDelegation(t *testing.T) {
	e := newX2Env(t, true)
	grantor, _ := e.bindingGrantor(t, "x2-mgmt-grantor")
	admin := e.grantor(t, "x2-mgmt-admin")
	d1 := e.delegate(t, grantor, nil)
	d2 := e.delegate(t, grantor, nil)
	rec := e.mint(t, d1.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	i1 := tiDecode(t, rec).Identity

	tok := func() string { return x2Token(t, x2TokenOpts{}) }
	// D2 gets 404 on D1's identity for token and delete, and lists nothing.
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/"+i1.ID+"/token", tok(), d2.ID, nil, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+i1.ID, tok(), d2.ID, nil, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = e.workload(t, http.MethodGet, "/api/v1/test-identities", tok(), d2.ID, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list ListTestIdentitiesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Empty(t, list.Items)

	// D1 sees and re-issues its own.
	rec = e.workload(t, http.MethodGet, "/api/v1/test-identities", tok(), d1.ID, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Items, 1)
	assert.Equal(t, i1.ID, list.Items[0].ID)
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/"+i1.ID+"/token", tok(), d1.ID, nil, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	audits, _, err := e.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testIdentityTokenIssueMutation, TargetID: i1.ID})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	assert.Equal(t, d1.ID, audits[0].ActorCredentialID)
	assert.Contains(t, audits[0].AfterSummary, `"delegation_id":"`+d1.ID+`"`)

	// The grantor's own session and an admin session still manage it.
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodPost, "/api/v1/test-identities/"+i1.ID+"/token", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, e.srv, admin, http.MethodPost, "/api/v1/test-identities/"+i1.ID+"/token", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// D1 deletes its own.
	rec = e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+i1.ID, tok(), d1.ID, nil, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// A workload credential context without its state manages nothing.
	ctx := contextWithCredentialContext(context.Background(), CredentialContext{Kind: CredentialKindWorkloadDelegation, ID: d1.ID})
	issued := grantor.ID
	assert.False(t, canManageTestIdentity(ctx, NewAuthenticatedUser(grantor.ID, grantor.Email, "", "member", "api"),
		&store.User{IssuedBy: &issued, IssuerDelegationID: &d1.ID}))
}

// --- AC X2-10: a copied config on another hub ---------------------------------

func TestWorkloadDelegation_CopiedConfigOtherHubRefuses(t *testing.T) {
	e := newX2Env(t, true)
	grantor := e.grantor(t, "x2-copy-grantor")
	d := e.delegate(t, grantor, nil)
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)

	// Same allowlist and store, but this hub's id differs: its issuer
	// audience is computed from its own id, so a token minted for the
	// original hub's audience is refused.
	enableWorkloadIssuerForTest(e.srv, e.s, e.google, "x2-other-hub", x2Sub, x2OtherSub)
	before := e.liveFixtures(t)
	rec := e.mint(t, d.ID)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Equal(t, before, e.liveFixtures(t))
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities", x2Token(t, x2TokenOpts{aud: testIssuerAudience("x2-other-hub")}), d.ID, nil, nil)
	assert.Equal(t, http.StatusCreated, rec.Code, "the token for this hub's own audience works: %s", rec.Body.String())
}

// --- AC X2-11 (amended): allowlist ceiling at every mint -----------------------

func TestWorkloadDelegation_AllowlistShrunkAtRestart(t *testing.T) {
	e := newX2Env(t, true)
	grantor := e.grantor(t, "x2-shrink-grantor")
	d := e.delegate(t, grantor, nil)
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)

	// Restart with the subject no longer allow-listed: the next mint on the
	// existing delegation is refused.
	enableWorkloadIssuerForTest(e.srv, e.s, e.google, x2HubID, x2OtherSub)
	rec := e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	// ADV-X2-1: the list no longer shows it as live.
	listRec := doRequestAsUser(t, e.srv, grantor, http.MethodGet, "/api/v1/test-identity-delegations", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var listed ListTestIdentityDelegationsResponse
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listed))
	require.Len(t, listed.Items, 1)
	assert.Equal(t, d.ID, listed.Items[0].ID)
	assert.Nil(t, listed.Items[0].RevokedAt, "the row itself is unchanged")
	assert.False(t, listed.Items[0].Live, "a delegation whose subject left the allowlist is not live")
	// A delegation to a subject still allow-listed stays live.
	kept := e.delegate(t, grantor, map[string]any{"workloadSub": x2OtherSub})
	assert.True(t, kept.Live)
	// And no new delegation for it can be created.
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodPost, "/api/v1/test-identity-delegations",
		map[string]any{"workloadSub": x2Sub, "scopes": []string{"test_identity:issue"}})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	// subCovers: member entries cover the member scope only.
	wi := e.srv.workloadIssuer
	assert.True(t, wi.subCovers(x2OtherSub, testIdentityDelegationScope))
	assert.False(t, wi.subCovers(x2OtherSub, "test_identity:issue_admin"))
	assert.False(t, wi.subCovers(x2Sub, testIdentityDelegationScope))
}

// --- AC X2-13: grantor fail-closed ---------------------------------------------

// x2GetUserErrStore fails GetUser for one id.
type x2GetUserErrStore struct {
	store.Store
	id string
}

func (s x2GetUserErrStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.id {
		return nil, errors.New("x2: store unavailable")
	}
	return s.Store.GetUser(ctx, id)
}

func TestWorkloadDelegation_GrantorFailClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("deleted", func(t *testing.T) {
		e := newX2Env(t, true)
		g := e.grantor(t, "x2-fc-deleted")
		d := e.delegate(t, g, nil)
		require.NoError(t, e.s.DeleteUser(ctx, g.ID))
		assert.Equal(t, http.StatusForbidden, e.mint(t, d.ID).Code)
	})
	t.Run("suspended", func(t *testing.T) {
		e := newX2Env(t, true)
		g := e.grantor(t, "x2-fc-suspended")
		d := e.delegate(t, g, nil)
		g.Status = store.UserStatusSuspended
		require.NoError(t, e.s.UpdateUser(ctx, g))
		assert.Equal(t, http.StatusForbidden, e.mint(t, d.ID).Code)
	})
	t.Run("unreadable", func(t *testing.T) {
		e := newX2Env(t, true)
		g := e.grantor(t, "x2-fc-unreadable")
		d := e.delegate(t, g, nil)
		e.srv.workloadIssuer.store = x2GetUserErrStore{Store: e.s, id: g.ID}
		assert.Equal(t, http.StatusForbidden, e.mint(t, d.ID).Code)
	})
	t.Run("not human", func(t *testing.T) {
		e := newX2Env(t, true)
		fixture := tiStoreFixture(t, e.s, generateID(), time.Now().Add(time.Hour))
		d := &store.TestIdentityDelegation{GrantorID: fixture.ID, WorkloadSub: x2Sub, Scope: testIdentityDelegationScope, MemberCap: 5,
			Created: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
		require.NoError(t, e.s.CreateTestIdentityDelegation(ctx, d))
		assert.Equal(t, http.StatusForbidden, e.mint(t, d.ID).Code)
	})
}

// --- AC X2-7 + allowlist: startup validation ----------------------------------

func TestWorkloadDelegation_StartupValidation(t *testing.T) {
	member := []config.TestIdentityWorkloadIssuer{{Sub: x2Sub, MaxTier: "member"}}
	aud := testIssuerAudience(x2HubID)
	base := func() ServerConfig {
		return ServerConfig{HubID: x2HubID, EnableTestIdentities: true, TestIdentityWorkloadIssuers: member,
			TestMode: TestModeConfig{Level: TestModeMember, HubID: x2HubID, HubClass: config.HubClassTest, MarkerHubID: x2HubID, MarkerClass: config.HubClassTest}}
	}
	require.NoError(t, validateTestIdentityWorkloadIssuers(base()))
	require.NoError(t, validateTestIdentityWorkloadIssuers(ServerConfig{}), "no workload issuer: nothing to check")

	cfg := base()
	cfg.Federation.TrustedIssuers = []config.TrustedIssuerConfig{{IssuerURL: "https://accounts.google.com", IssuerType: "user", ExpectedAudience: aud}}
	assert.ErrorIs(t, validateTestIdentityWorkloadIssuers(cfg), errWorkloadIssuerAudienceConflict, "external-bearer expected_audience")

	cfg = base()
	cfg.ProxyAuth = &IAPAuthenticator{Audience: aud}
	assert.ErrorIs(t, validateTestIdentityWorkloadIssuers(cfg), errWorkloadIssuerAudienceConflict, "IAP audience")

	cfg = base()
	cfg.TransportAudience = aud
	assert.ErrorIs(t, validateTestIdentityWorkloadIssuers(cfg), errWorkloadIssuerAudienceConflict, "transport audience")

	cfg = base()
	cfg.ProxyAuth = &IAPAuthenticator{Audience: "iap-client"}
	cfg.Federation.TrustedIssuers = []config.TrustedIssuerConfig{{IssuerURL: "https://accounts.google.com", IssuerType: "user", ExpectedAudience: "client-id"}}
	assert.NoError(t, validateTestIdentityWorkloadIssuers(cfg), "distinct audiences pass")

	cfg = base()
	cfg.HubID = ""
	assert.ErrorIs(t, validateTestIdentityWorkloadIssuers(cfg), errWorkloadIssuerNoHubID)

	cfg = base()
	cfg.TestIdentityWorkloadIssuers = []config.TestIdentityWorkloadIssuer{{Sub: x2Sub, MaxTier: "admin"}}
	assert.ErrorIs(t, validateTestIdentityWorkloadIssuers(cfg), config.ErrWorkloadIssuerAdminTierUnsupported)

	// New refuses to build a Server from such a config.
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	_, err = New(cfg, s)
	assert.ErrorIs(t, err, config.ErrWorkloadIssuerAdminTierUnsupported)
}

// --- status: testIssuerAudience -------------------------------------------------

func TestWorkloadDelegation_StatusIssuerAudience(t *testing.T) {
	e := newX2Env(t, true)
	rec := doRequestNoAuth(t, e.srv, http.MethodGet, "/api/v1/test-infra/status", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var body TestInfraStatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "scion-test-issuer:"+x2HubID, body.TestIssuerAudience)

	off := newX2Env(t, false)
	rec = doRequestNoAuth(t, off.srv, http.MethodGet, "/api/v1/test-infra/status", nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Empty(t, body.TestIssuerAudience, "member tier off")

	e.srv.workloadIssuer = nil
	rec = doRequestNoAuth(t, e.srv, http.MethodGet, "/api/v1/test-infra/status", nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Empty(t, body.TestIssuerAudience, "no workload issuer configured")
}

// --- review N3: the in-transaction recheck, through the handlers ---------------

// x2RevokeOnGrantorRead revokes a delegation the first time the arm reads
// its grantor: after the arm loaded and checked the delegation, before the
// handler's issuance transaction. Only the arm reads this store.
type x2RevokeOnGrantorRead struct {
	store.Store
	grantorID    string
	delegationID string
	once         sync.Once
}

func (s *x2RevokeOnGrantorRead) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.grantorID {
		s.once.Do(func() {
			_, _ = s.RevokeTestIdentityDelegation(ctx, s.delegationID, id, time.Now())
		})
	}
	return s.Store.GetUser(ctx, id)
}

func TestWorkloadDelegation_RevokedAfterAuthMintsNothing(t *testing.T) {
	ctx := context.Background()
	auditCount := func(t *testing.T, e *x2Env, mutation string) int {
		audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutation})
		require.NoError(t, err)
		return len(audits)
	}

	t.Run("create", func(t *testing.T) {
		e := newX2Env(t, true)
		grantor := e.grantor(t, "x2-n3-create")
		d := e.delegate(t, grantor, nil)
		e.srv.workloadIssuer.store = &x2RevokeOnGrantorRead{Store: e.s, grantorID: grantor.ID, delegationID: d.ID}
		fixtures, audits := e.liveFixtures(t), auditCount(t, e, testIdentityIssueMutation)
		rec := e.mint(t, d.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, fixtures, e.liveFixtures(t), "no fixture row")
		assert.Equal(t, audits, auditCount(t, e, testIdentityIssueMutation), "no issuance audit")
		got, err := e.s.GetTestIdentityDelegation(ctx, d.ID)
		require.NoError(t, err)
		require.NotNil(t, got.RevokedAt, "the revocation happened after the arm accepted the request")
	})

	t.Run("token re-issue", func(t *testing.T) {
		e := newX2Env(t, true)
		grantor := e.grantor(t, "x2-n3-token")
		d := e.delegate(t, grantor, nil)
		rec := e.mint(t, d.ID)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		id := tiDecode(t, rec).Identity.ID
		e.srv.workloadIssuer.store = &x2RevokeOnGrantorRead{Store: e.s, grantorID: grantor.ID, delegationID: d.ID}
		audits := auditCount(t, e, testIdentityTokenIssueMutation)
		rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/"+id+"/token", x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "accessToken")
		assert.Equal(t, audits, auditCount(t, e, testIdentityTokenIssueMutation), "no re-issue audit")
	})

	// X3b (advisor): the web-login code mint rechecks the delegation in
	// its transaction too, so a delegation revoked after the arm accepted
	// the request mints no code.
	t.Run("web-login code", func(t *testing.T) {
		e := newX2Env(t, true)
		x2EnableWebSessions(t, e)
		grantor, _ := e.bindingGrantor(t, "x2-n3-webcode")
		d := e.delegate(t, grantor, nil)
		rec := e.mint(t, d.ID)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		id := tiDecode(t, rec).Identity.ID
		e.srv.workloadIssuer.store = &x2RevokeOnGrantorRead{Store: e.s, grantorID: grantor.ID, delegationID: d.ID}
		rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/"+id+"/web-login-code", x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		// The error envelope's error.code is fine; no web-login code field
		// may appear at the top level.
		var top map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &top))
		assert.NotContains(t, top, "code")
		assert.NotContains(t, top, "codeId")
		assert.Contains(t, top, "error")
		assert.Empty(t, x2WebCodeAudits(t, e, id), "no web-login code audit")
		got, err := e.s.GetTestIdentityDelegation(ctx, d.ID)
		require.NoError(t, err)
		require.NotNil(t, got.RevokedAt, "the revocation happened after the arm accepted the request")
	})
}

// N4: the in-transaction recheck uses the arm's definition of live, so a
// delegation whose subject left the allowlist, or which is past its hard
// maximum age, is refused there too.
func TestWorkloadDelegation_RecheckUsesTheArmDefinition(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	grantor := e.grantor(t, "x2-n4-grantor")
	d := e.delegate(t, grantor, nil)
	st := &workloadDelegationState{delegation: &store.TestIdentityDelegation{ID: d.ID, GrantorID: grantor.ID}, sub: x2Sub}
	now := e.srv.testIdentities.clock()
	require.NoError(t, e.srv.recheckWorkloadDelegation(ctx, e.s, st, now))

	// Grantor or subject mismatch.
	assert.ErrorIs(t, e.srv.recheckWorkloadDelegation(ctx, e.s, &workloadDelegationState{
		delegation: &store.TestIdentityDelegation{ID: d.ID, GrantorID: generateID()}, sub: x2Sub}, now), errTestIdentityDelegationNotLive)
	assert.ErrorIs(t, e.srv.recheckWorkloadDelegation(ctx, e.s, &workloadDelegationState{
		delegation: &store.TestIdentityDelegation{ID: d.ID, GrantorID: grantor.ID}, sub: x2OtherSub}, now), errTestIdentityDelegationNotLive)

	// Hard maximum age from creation, with a later stored expiry.
	old := &store.TestIdentityDelegation{GrantorID: grantor.ID, WorkloadSub: x2Sub, Scope: testIdentityDelegationScope, MemberCap: 5,
		Created: now.Add(-testIdentityDelegationMaxAge - time.Minute), ExpiresAt: now.Add(time.Hour)}
	require.NoError(t, e.s.CreateTestIdentityDelegation(ctx, old))
	assert.ErrorIs(t, e.srv.recheckWorkloadDelegation(ctx, e.s, &workloadDelegationState{
		delegation: &store.TestIdentityDelegation{ID: old.ID, GrantorID: grantor.ID}, sub: x2Sub}, now), errTestIdentityDelegationNotLive)

	// Subject no longer allow-listed.
	enableWorkloadIssuerForTest(e.srv, e.s, e.google, x2HubID, x2OtherSub)
	assert.ErrorIs(t, e.srv.recheckWorkloadDelegation(ctx, e.s, st, now), errTestIdentityDelegationNotLive)

	// No workload issuer at all: fail closed.
	e.srv.workloadIssuer = nil
	assert.ErrorIs(t, e.srv.recheckWorkloadDelegation(ctx, e.s, st, now), errTestIdentityDelegationNotLive)
}

// N5: concurrent revocations of one delegation write one revoke audit.
func TestTestIdentityDelegation_ConcurrentRevokeWritesOneAudit(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	grantor := e.grantor(t, "x2-n5-grantor")
	d := e.delegate(t, grantor, nil)
	const n = 6
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = doRequestAsUser(t, e.srv, grantor, http.MethodDelete, "/api/v1/test-identity-delegations/"+d.ID, nil).Code
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		assert.Contains(t, []int{http.StatusNoContent, http.StatusTooManyRequests}, c)
	}
	audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityDelegationRevokeMutation, TargetID: d.ID})
	require.NoError(t, err)
	assert.Len(t, audits, 1)
	got, err := e.s.GetTestIdentityDelegation(ctx, d.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RevokedAt)
}

// R2-N1: the list's live flag uses the arm's definition, so a stored row
// past its hard maximum age (even with a later stored expiry), or with a
// scope the hub does not accept, is not shown as live.
func TestTestIdentityDelegation_ListLiveUsesTheArmDefinition(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	grantor := e.grantor(t, "x2-r2n1-grantor")
	fresh := e.delegate(t, grantor, nil)
	now := time.Now()
	old := &store.TestIdentityDelegation{GrantorID: grantor.ID, WorkloadSub: x2Sub, Scope: testIdentityDelegationScope, MemberCap: 5,
		Created: now.Add(-testIdentityDelegationMaxAge - time.Minute), ExpiresAt: now.Add(time.Hour)}
	require.NoError(t, e.s.CreateTestIdentityDelegation(ctx, old))
	odd := &store.TestIdentityDelegation{GrantorID: grantor.ID, WorkloadSub: x2Sub, Scope: "test_identity:issue_admin", MemberCap: 1,
		Created: now, ExpiresAt: now.Add(time.Hour)}
	require.NoError(t, e.s.CreateTestIdentityDelegation(ctx, odd))

	rec := doRequestAsUser(t, e.srv, grantor, http.MethodGet, "/api/v1/test-identity-delegations", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListTestIdentityDelegationsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	live := map[string]bool{}
	for _, it := range resp.Items {
		live[it.ID] = it.Live
	}
	require.Len(t, live, 3)
	assert.True(t, live[fresh.ID], "a fresh delegation is live")
	assert.False(t, live[old.ID], "past the hard maximum age: not live, although its stored expiry is later")
	assert.False(t, live[odd.ID], "a scope the hub does not accept: not live")

	// No workload issuer configured: nothing is live.
	e.srv.workloadIssuer = nil
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodGet, "/api/v1/test-identity-delegations", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	for _, it := range resp.Items {
		assert.False(t, it.Live, it.ID)
	}
}

// Merge review R1: the Phase 3b purge (DELETE ...?purge=true) is scoped to
// the issuing delegation. A delegation purges only its own live identity;
// the leftovers of an already deleted identity (which record the issuing
// user, not the delegation) are never purged by a workload delegation
// request, its own or another; the grantor's session and an admin session
// still purge them.
func TestWorkloadDelegation_PurgeScopedToDelegation(t *testing.T) {
	e := newX2Env(t, true)
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	// The fixtures below create a group, as in the Phase 3b purge tests.
	fpAllowMembersToCreateGroups(t, e.s)
	ctx := context.Background()
	grantor, _ := e.bindingGrantor(t, "x2-purge-grantor")
	admin := e.grantor(t, "x2-purge-admin")
	d1 := e.delegate(t, grantor, nil)
	d2 := e.delegate(t, grantor, nil)
	tok := func() string { return x2Token(t, x2TokenOpts{}) }
	purgePath := func(id string) string { return "/api/v1/test-identities/" + id + "?purge=true" }
	issueWithGroup := func(name string) (TestIdentityView, string) {
		rec := e.mint(t, d1.ID)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		resp := tiDecode(t, rec)
		group := fpCreateGroup(t, e.srv, resp.AccessToken, CreateGroupRequest{Name: name})
		return resp.Identity, group
	}
	groupExists := func(id string) bool {
		_, err := e.s.GetGroup(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return false
		}
		require.NoError(t, err)
		return true
	}
	emptyReport := func(rec *httptest.ResponseRecorder, id string) {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, *emptyTestIdentityPurgeReport(id), fpPurgeReport(t, rec))
	}

	// Live identity: D2 gets the empty report and nothing is removed; D1
	// purges its own identity and its stamped group.
	live, liveGroup := issueWithGroup("x2-purge-live")
	emptyReport(e.workload(t, http.MethodDelete, purgePath(live.ID), tok(), d2.ID, nil, nil), live.ID)
	assert.True(t, groupExists(liveGroup))
	assert.True(t, fpUserExists(t, e.s, live.ID))
	rec := e.workload(t, http.MethodDelete, purgePath(live.ID), tok(), d1.ID, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := fpPurgeReport(t, rec)
	assert.True(t, r.IdentityDeleted)
	assert.Equal(t, 1, r.Deleted.Groups)
	assert.False(t, groupExists(liveGroup))

	// Deleted identity (D1 deletes it; its group is left): neither D2 nor
	// D1 purges the leftovers; the grantor's own session does.
	gone, goneGroup := issueWithGroup("x2-purge-gone")
	require.Equal(t, http.StatusNoContent, e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+gone.ID, tok(), d1.ID, nil, nil).Code)
	require.False(t, fpUserExists(t, e.s, gone.ID))
	emptyReport(e.workload(t, http.MethodDelete, purgePath(gone.ID), tok(), d2.ID, nil, nil), gone.ID)
	emptyReport(e.workload(t, http.MethodDelete, purgePath(gone.ID), tok(), d1.ID, nil, nil), gone.ID)
	assert.True(t, groupExists(goneGroup), "no workload delegation purges a deleted identity's leftovers")
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodDelete, purgePath(gone.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, fpPurgeReport(t, rec).Deleted.Groups)
	assert.False(t, groupExists(goneGroup))

	// An admin session also purges a deleted identity's leftovers.
	gone2, gone2Group := issueWithGroup("x2-purge-gone-admin")
	require.Equal(t, http.StatusNoContent, e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+gone2.ID, tok(), d1.ID, nil, nil).Code)
	emptyReport(e.workload(t, http.MethodDelete, purgePath(gone2.ID), tok(), d1.ID, nil, nil), gone2.ID)
	rec = doRequestAsUser(t, e.srv, admin, http.MethodDelete, purgePath(gone2.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, fpPurgeReport(t, rec).Deleted.Groups)
	assert.False(t, groupExists(gone2Group))
}

// rev-9: a workload delegation never reaches an admin-kind identity (A1),
// even one its own grantor issued. Token re-issue and delete answer 404
// (never the admin-kind 409 or a 200), nothing is minted and nothing is
// deleted; the grantor's own session does reach the admin-kind 409 branch.
func TestWorkloadDelegation_AdminKindIdentityUnreachable(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	grantor := e.grantor(t, "x2-adminkind-grantor")
	d := e.delegate(t, grantor, nil)

	rd, err := e.s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	exp := time.Now().Add(30 * time.Minute)
	issuer := grantor.ID
	issuance := generateID()
	admin := &store.User{
		ID:          generateID(),
		Email:       "test-admin-" + generateID()[:8] + "@" + store.TestHubAdminEmailDomain,
		DisplayName: "x2 admin kind",
		Role:        store.TestAdminRole(store.UserKindTestHubAdmin),
		Status:      store.UserStatusActive,
		Kind:        store.UserKindTestHubAdmin,
		ExpiresAt:   &exp,
		IssuedBy:    &issuer,
		IssuanceID:  &issuance,
	}
	_, err = e.s.CreateTestAdminUser(ctx, admin, rd.ID)
	require.NoError(t, err)

	tokenIssues := func() int {
		audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityTokenIssueMutation, TargetID: admin.ID})
		require.NoError(t, err)
		return len(audits)
	}
	before := tokenIssues()

	rec := e.workload(t, http.MethodPost, "/api/v1/test-identities/"+admin.ID+"/token", x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "accessToken")
	rec = e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+admin.ID, x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, before, tokenIssues(), "nothing minted")
	_, err = e.s.GetUser(ctx, admin.ID)
	require.NoError(t, err, "nothing deleted")

	// Positive control: the grantor's own session reaches the admin-kind
	// branch (the identity is its own), which refuses re-issue with 409.
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodPost, "/api/v1/test-identities/"+admin.ID+"/token", nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, before, tokenIssues())
}

// Upstream review (cost model): with no workload issuer configured,
// isWorkloadDelegationRequest does no work beyond one header lookup for a
// JWT-shaped bearer (it allocates nothing, so it neither reads nor decodes
// the token); with a workload issuer configured the same request's token
// payload is decoded (it allocates), and a token carrying the issuer
// audience is routed to the arm.
func TestIsWorkloadDelegationRequest_NoWorkWithoutWorkloadIssuer(t *testing.T) {
	token := x2Token(t, x2TokenOpts{aud: testIssuerAudience(x2HubID)})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	assert.False(t, isWorkloadDelegationRequest(req, nil))
	allocs := testing.AllocsPerRun(100, func() { _ = isWorkloadDelegationRequest(req, nil) })
	assert.Zero(t, allocs, "no workload issuer: the bearer is not read or decoded")

	wi := &workloadIssuerAuthenticator{audience: testIssuerAudience(x2HubID), allowlist: map[string]string{x2Sub: config.WorkloadIssuerTierMember}}
	assert.True(t, isWorkloadDelegationRequest(req, wi), "with a workload issuer, the issuer-audience token is routed to the arm")
	allocs = testing.AllocsPerRun(100, func() { _ = isWorkloadDelegationRequest(req, wi) })
	assert.Positive(t, allocs, "control: with a workload issuer the payload is decoded")
}

// Upstream review (label UTF-8): the delegation label comes from the JSON
// body, and encoding/json replaces invalid UTF-8 with U+FFFD while
// decoding, so a stored label is always valid UTF-8, even for a request
// whose label holds invalid bytes.
func TestTestIdentityDelegation_LabelIsAlwaysValidUTF8(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	grantor := e.grantor(t, "x2-utf8-grantor")
	body := []byte(`{"workloadSub":"` + x2Sub + `","scopes":["test_identity:issue"],"label":"ab` + "\xff" + `cd"}`)
	require.False(t, utf8.Valid(body), "the request body carries an invalid byte")
	token, _, _, err := e.srv.userTokenService.GenerateTokenPair(grantor.ID, grantor.Email, grantor.DisplayName, grantor.Role, ClientTypeWeb)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/test-identity-delegations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var v TestIdentityDelegationView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	assert.Equal(t, "ab�cd", v.Label)
	stored, err := e.s.GetTestIdentityDelegation(ctx, v.ID)
	require.NoError(t, err)
	assert.True(t, utf8.ValidString(stored.Label))
	assert.Equal(t, "ab�cd", stored.Label)
}

// x2EnableWebSessions gives e's server a hub-local web session key, so
// test identity web sessions are available.
func x2EnableWebSessions(t *testing.T, e *x2Env) {
	t.Helper()
	key := make([]byte, testIdentityWebKeyLen)
	_, err := rand.Read(key)
	require.NoError(t, err)
	e.srv.testIdentityWeb = newTestIdentityWebState(TestIdentityWebSessionKey{Key: key})
	webOn, _ := e.srv.testIdentityWebSessionGate()
	require.True(t, webOn, "web sessions are available in this setup")
}

// x2WebCodeAudits returns the web-login code audit records for identity id.
func x2WebCodeAudits(t *testing.T, e *x2Env, id string) []*store.MutationAuditRecord {
	t.Helper()
	audits, _, err := e.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testIdentityWebCodeIssueMutation, TargetID: id})
	require.NoError(t, err)
	return audits
}

// X3b (ptone/scion#4240): a workload delegation mints a web-login code for
// a member-tier identity it issued itself. The code is returned once, and
// the audit names the grantor as the actor and the delegation as the
// credential, with the delegation in the summary; it holds no code.
func TestWorkloadDelegation_WebLoginCodeForOwnIdentity(t *testing.T) {
	e := newX2Env(t, true)
	x2EnableWebSessions(t, e)

	grantor, _ := e.bindingGrantor(t, "x2-web-own-grantor")
	d1 := e.delegate(t, grantor, nil)
	rec := e.mint(t, d1.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	i1 := tiDecode(t, rec).Identity
	path := "/api/v1/test-identities/" + i1.ID + "/web-login-code"

	assert.True(t, isTestIdentityIssuanceRoute(httptest.NewRequest(http.MethodPost, path, nil)))
	rec = e.workload(t, http.MethodPost, path, x2Token(t, x2TokenOpts{}), d1.ID, nil, map[string]string{testIssuerRunLabelHeader: "x3b-run"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var resp TestIdentityWebCodeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Code)
	require.NotEmpty(t, resp.CodeID)

	audits := x2WebCodeAudits(t, e, i1.ID)
	require.Len(t, audits, 1)
	a := audits[0]
	assert.Equal(t, grantor.ID, a.ActorPrincipalID, "the grantor is the actor")
	assert.Equal(t, d1.ID, a.ActorCredentialID, "the delegation is the credential")
	assert.NotContains(t, a.AfterSummary, resp.Code)
	var summary struct {
		CredentialKind string                     `json:"credential_kind"`
		Workload       *testIdentityWorkloadAudit `json:"workload_delegation"`
	}
	require.NoError(t, json.Unmarshal([]byte(a.AfterSummary), &summary))
	require.NotNil(t, summary.Workload, "the summary records the workload delegation")
	assert.Equal(t, d1.ID, summary.Workload.DelegationID)
	assert.Equal(t, grantor.ID, summary.Workload.Grantor)
	assert.Equal(t, x2Sub, summary.Workload.WorkloadSub)
	assert.Equal(t, "x3b-run", summary.Workload.RunLabel)
	assert.Equal(t, string(CredentialKindWorkloadDelegation), summary.CredentialKind)

	// The web session gate still applies to a workload request.
	e.srv.testIdentityWeb = testIdentityWebState{}
	rec = e.workload(t, http.MethodPost, path, x2Token(t, x2TokenOpts{}), d1.ID, nil, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), testIdentityWebReasonUnavailable)
	assert.Len(t, x2WebCodeAudits(t, e, i1.ID), 1, "no code minted while web sessions are unavailable")
}

// X3b with WS2 (ptone/scion#4240): on a proxy auth mode hub, a workload
// delegation mints a web-login code for its own member identity only while
// the hub lists at least one driver principal. Without one it gets the 404
// unavailable answer with the no-driver-principals cause, and no code is
// minted.
func TestWorkloadDelegation_WebLoginCodeOnProxyHubFollowsDriverPrincipals(t *testing.T) {
	e := newX2Env(t, true)
	x2EnableWebSessions(t, e)

	grantor, _ := e.bindingGrantor(t, "x2-web-proxy-grantor")
	d1 := e.delegate(t, grantor, nil)
	rec := e.mint(t, d1.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	i1 := tiDecode(t, rec).Identity
	path := "/api/v1/test-identities/" + i1.ID + "/web-login-code"

	e.srv.config.AuthMode = "proxy"
	e.srv.config.ProxyAuth = &fakeProxyAuthenticator{}
	e.srv.proxyDrivers = newDriverPrincipalSet([]string{"web-driver@x2-proj.iam.gserviceaccount.com"})
	webOn, cause := e.srv.testIdentityWebSessionGate()
	require.True(t, webOn, "proxy auth mode with a driver principal: web sessions available (cause %q)", cause)

	rec = e.workload(t, http.MethodPost, path, x2Token(t, x2TokenOpts{}), d1.ID, nil, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp TestIdentityWebCodeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Code)
	require.Len(t, x2WebCodeAudits(t, e, i1.ID), 1, "one code minted with a driver principal configured")

	e.srv.proxyDrivers = nil
	webOn, cause = e.srv.testIdentityWebSessionGate()
	require.False(t, webOn)
	require.Equal(t, testIdentityWebCauseNoDriverPrincipals, cause)

	rec = e.workload(t, http.MethodPost, path, x2Token(t, x2TokenOpts{}), d1.ID, nil, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), testIdentityWebReasonUnavailable)
	assert.Contains(t, rec.Body.String(), testIdentityWebCauseNoDriverPrincipals)
	assert.NotContains(t, rec.Body.String(), `"code":"`, "no code in the answer")
	assert.Len(t, x2WebCodeAudits(t, e, i1.ID), 1, "no code minted without a driver principal")
}

// X3b (ptone/scion#4240): a workload delegation gets the same 404 as for
// an unknown identity on another delegation's identity, on the grantor's
// own (session-issued) identity and on an admin-kind identity; nothing is
// minted. The grantor's own session still mints a code for the identity.
func TestWorkloadDelegation_WebLoginCodeRefusedOutsideOwnMemberIdentities(t *testing.T) {
	e := newX2Env(t, true)
	x2EnableWebSessions(t, e)
	ctx := context.Background()

	grantor, _ := e.bindingGrantor(t, "x2-web-other-grantor")
	d1 := e.delegate(t, grantor, nil)
	d2 := e.delegate(t, grantor, nil)
	rec := e.mint(t, d1.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	i1 := tiDecode(t, rec).Identity

	// An identity the grantor issued with its own session, through no
	// delegation.
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodPost, "/api/v1/test-identities", map[string]string{"role": "member"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	own := tiDecode(t, rec).Identity

	// An admin-kind identity issued by the same grantor.
	rd, err := e.s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	exp := time.Now().Add(30 * time.Minute)
	issuer := grantor.ID
	issuance := generateID()
	admin := &store.User{
		ID:          generateID(),
		Email:       "test-admin-" + generateID()[:8] + "@" + store.TestHubAdminEmailDomain,
		DisplayName: "x3b admin kind",
		Role:        store.TestAdminRole(store.UserKindTestHubAdmin),
		Status:      store.UserStatusActive,
		Kind:        store.UserKindTestHubAdmin,
		ExpiresAt:   &exp,
		IssuedBy:    &issuer,
		IssuanceID:  &issuance,
	}
	_, err = e.s.CreateTestAdminUser(ctx, admin, rd.ID)
	require.NoError(t, err)

	unknown := e.workload(t, http.MethodPost, "/api/v1/test-identities/"+generateID()+"/web-login-code", x2Token(t, x2TokenOpts{}), d1.ID, nil, nil)
	require.Equal(t, http.StatusNotFound, unknown.Code, unknown.Body.String())

	for _, tc := range []struct {
		name, delegation, identity string
	}{
		{"another delegation's identity", d2.ID, i1.ID},
		{"the grantor's session-issued identity", d1.ID, own.ID},
		{"an admin-kind identity", d1.ID, admin.ID},
	} {
		path := "/api/v1/test-identities/" + tc.identity + "/web-login-code"
		rec := e.workload(t, http.MethodPost, path, x2Token(t, x2TokenOpts{}), tc.delegation, nil, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", tc.name, rec.Body.String())
		assert.JSONEq(t, unknown.Body.String(), rec.Body.String(), "%s: same answer as an unknown identity", tc.name)
		assert.Empty(t, x2WebCodeAudits(t, e, tc.identity), "%s: no code minted", tc.name)
	}

	// Positive control: the grantor's own session mints a code for the
	// delegation's identity.
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodPost, "/api/v1/test-identities/"+i1.ID+"/web-login-code", nil)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// The member-tier rule of workloadWebCodeAdmitted on its own: the store
// never holds an admin-kind row with a delegation id, so the handler test
// above cannot reach this branch with one.
func TestWorkloadWebCodeAdmitted(t *testing.T) {
	ctx := context.Background()
	did := "d-1"
	member := &store.User{ID: "u-member", Kind: store.UserKindTestFixture, IssuerDelegationID: &did}
	admin := &store.User{ID: "u-admin", Kind: store.UserKindTestHubAdmin, IssuerDelegationID: &did}
	superAdmin := &store.User{ID: "u-super", Kind: store.UserKindTestSuperAdmin, IssuerDelegationID: &did}
	withState := func(scope string) context.Context {
		c := contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindWorkloadDelegation, ID: did})
		return contextWithWorkloadDelegationState(c, &workloadDelegationState{delegation: &store.TestIdentityDelegation{ID: did, Scope: scope}})
	}

	assert.True(t, workloadWebCodeAdmitted(ctx, member), "not a workload request")
	assert.True(t, workloadWebCodeAdmitted(ctx, admin), "not a workload request: the kind rule is testIdentityWebKindAdmitted's")
	assert.True(t, workloadWebCodeAdmitted(withState(testIdentityDelegationScope), member))
	assert.False(t, workloadWebCodeAdmitted(withState(testIdentityDelegationScope), admin))
	assert.False(t, workloadWebCodeAdmitted(withState(testIdentityDelegationScope), superAdmin))
	assert.False(t, workloadWebCodeAdmitted(withState("test_identity:issue_admin"), member), "a scope other than the member scope")
	noState := contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindWorkloadDelegation, ID: did})
	assert.False(t, workloadWebCodeAdmitted(noState, member), "a workload credential without its state")
}
