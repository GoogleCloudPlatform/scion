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
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Admin-kind test identity web sessions (ptone/scion#4240, phase WS4).
// Every identity, user and project here is synthetic, in an in-memory
// store. The harness is WS1's (tiWebEnv) plus the A1 admin helpers.

// --- Harness ----------------------------------------------------------------

type ws4Env struct {
	*tiWebEnv
	sink *taSinkBuffer
}

// newWS4Env builds a WS1 environment (member level on, a hub-local web
// session key) with the test hub-admin kind on, the test super-admin kind
// on when superAdmin is set, the admin web session switch set to adminWeb,
// and a captured test admin audit sink. Test-only: production code
// captures these once, in New.
func newWS4Env(t *testing.T, superAdmin, adminWeb bool) *ws4Env {
	t.Helper()
	e := newTIWebEnv(t)
	e.srv.testAdminGate = testAdminGate{hubAdmin: true, superAdmin: superAdmin, hubID: taHubID, hubClass: config.HubClassTest}
	e.srv.testAdminWeb = testAdminWebState{on: adminWeb}
	sink := &taSinkBuffer{}
	e.srv.testAdminAudit = newTestAdminAuditSink(sink)
	e.ws.events = NewChannelEventPublisher()
	return &ws4Env{tiWebEnv: e, sink: sink}
}

// issuer creates a human super-admin user (a live admin issuer).
func (e *ws4Env) issuer(name string) *store.User {
	e.t.Helper()
	return taUser(e.t, e.st, name+"-"+generateID()[:6], store.SystemRoleSuperAdmin)
}

// issueAdmin issues an admin-kind identity of adminRole as the session of
// iss, requiring 201.
func (e *ws4Env) issueAdmin(iss *store.User, adminRole string) TestAdminIdentityTokenResponse {
	e.t.Helper()
	rec := doRequestAsUser(e.t, e.srv, iss, http.MethodPost, taPath, taBody(adminRole))
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	return taDecode(e.t, rec)
}

func ws4WebCodePath(id string) string {
	return "/api/v1/test-identities/" + id + "/web-login-code"
}

// mintAsRec calls the mint route as the session of iss.
func (e *ws4Env) mintAsRec(iss *store.User, id string) *httptest.ResponseRecorder {
	e.t.Helper()
	return doRequestAsUser(e.t, e.srv, iss, http.MethodPost, ws4WebCodePath(id), nil)
}

// mintAs mints a code as the session of iss, requiring 201.
func (e *ws4Env) mintAs(iss *store.User, id string) TestIdentityWebCodeResponse {
	e.t.Helper()
	rec := e.mintAsRec(iss, id)
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp TestIdentityWebCodeResponse
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// adminLogin mints a code for id as iss and redeems it, requiring 204, and
// returns the session cookies.
func (e *ws4Env) adminLogin(iss *store.User, id string) []*http.Cookie {
	e.t.Helper()
	rec := e.redeem(e.mintAs(iss, id).Code)
	require.Equal(e.t, http.StatusNoContent, rec.Code, rec.Body.String())
	return tiWebApplyCookies(nil, rec)
}

// sse opens the event stream with jar. Only refusals are asserted here: a
// stream that is accepted would not return.
func (e *ws4Env) sse(jar []*http.Cookie) *httptest.ResponseRecorder {
	return e.do(tiReq{path: "/events?sub=notification.ti", cookies: jar})
}

func (e *ws4Env) classify(jar []*http.Cookie) tiSessionResult {
	return e.ws.classifyTestIdentitySession(tiWebRequest(jar), tiWebSession(e.t, e.ws, jar), nil)
}

// mintedToken returns the access token the web server forwarded on the
// last API request.
func (e *ws4Env) mintedToken() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.TrimPrefix(e.lastAuth, "Bearer ")
}

// assertSessionRefusedEverywhere requires every session-gated route to
// refuse jar: the page (static "session ended", no login redirect), the
// web /auth/me, the API through sessionToBearer, and the event stream.
func (e *ws4Env) assertSessionRefusedEverywhere(jar []*http.Cookie, msg string) {
	e.t.Helper()
	rec := e.page(jar)
	assert.Equal(e.t, http.StatusUnauthorized, rec.Code, "page: %s", msg)
	assert.Contains(e.t, rec.Body.String(), "Test identity session ended", "page: %s", msg)
	assert.Empty(e.t, rec.Header().Get("Location"), "no login redirect: %s", msg)
	assert.Equal(e.t, http.StatusUnauthorized, e.webMe(jar).Code, "/auth/me: %s", msg)
	assert.Equal(e.t, http.StatusUnauthorized, e.apiMe(jar).Code, "API: %s", msg)
	rec = e.sse(jar)
	assert.Equal(e.t, http.StatusUnauthorized, rec.Code, "SSE: %s", msg)
	assert.NotContains(e.t, rec.Header().Get("Content-Type"), "text/event-stream", "SSE: %s", msg)
}

// ws4RawUser writes a users row directly in the database, so a test can
// choose any kind and email (including combinations no issuance path
// writes), and returns the row as the store reads it.
func ws4RawUser(t *testing.T, s store.Store, kind, email, role string, created, expires time.Time) *store.User {
	t.Helper()
	ctx := context.Background()
	raw, ok := s.(interface {
		DB() *sql.DB
		Dialect() string
	})
	require.True(t, ok, "the test store exposes its database")
	id := generateID()
	cols := "id, email, display_name, role, status, created, session_generation, kind, expires_at"
	vals := "?, ?, 'ws4 raw', ?, 'active', ?, 0, ?, ?"
	args := []any{id, email, role, created.UTC(), kind, expires.UTC()}
	if kind == store.UserKindFederated {
		cols += ", home_issuer, federated_email"
		vals += ", ?, ?"
		args = append(args, "https://issuer.example.com", "fed-"+id[:8]+"@example.com")
	}
	q := "INSERT INTO users (" + cols + ") VALUES (" + vals + ")"
	_, err := raw.DB().ExecContext(ctx, taRebind(raw.Dialect(), q), args...)
	require.NoError(t, err)
	u, err := s.GetUser(ctx, id)
	require.NoError(t, err)
	return u
}

// --- B1: without the setting the admin kinds are refused ----------------------

// With the member level and the admin tier on but the admin web session
// setting off, an admin-kind identity gets no code (404), a stored code
// for it does not redeem (401), and a hand-made marked session for it is
// cleared. Member-tier web sessions are unchanged. Turning the setting on
// admits the same identity, so the refusals come from the setting.
func TestAdminWebSessions_RefusedWithoutSetting(t *testing.T) {
	e := newWS4Env(t, true, false)
	iss := e.issuer("ws4-b1-iss")
	adm := e.issueAdmin(iss, testAdminRoleHubAdmin)
	row := e.row(adm.Identity.ID)

	rec := e.mintAsRec(iss, adm.Identity.ID)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// A code row written straight to the store does not redeem.
	code, hash, err := newTestIdentityWebCode()
	require.NoError(t, err)
	now := e.clock.Now()
	require.NoError(t, e.st.ReplaceTestIdentityLoginCode(context.Background(), &store.TestIdentityLoginCode{
		UserID: row.ID, CodeHash: hash, IssuedBy: iss.ID, ExpiresAt: now.Add(testIdentityWebCodeTTL), Created: now,
	}))
	rec = e.redeem(code)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Nil(t, tiWebSetCookie(rec, testIdentitySessionCookie), "no marker cookie")
	notAdmitted := 0
	for _, a := range tiWebAudits(t, e.st, testIdentityWebSessionRefuseMutation) {
		if strings.Contains(a.AfterSummary, "identity_not_admitted") && strings.Contains(a.AfterSummary, row.ID) {
			notAdmitted++
		}
	}
	assert.Equal(t, 1, notAdmitted, "one refusal audit row for the admin-kind code")

	// A hand-made marked session is cleared on every consumer.
	jar := e.craftMarkedSession(row, generateID(), *row.ExpiresAt, row.SessionGeneration)
	assert.Equal(t, "kind_not_admitted", e.classify(jar).reason)
	apiRec := e.apiMe(jar)
	assert.Equal(t, http.StatusUnauthorized, apiRec.Code)
	assert.True(t, tiWebCleared(apiRec, webSessionName))
	e.assertSessionRefusedEverywhere(jar, "setting off")

	// Member web sessions are unchanged.
	fx := e.issue()
	mjar := e.login(fx.Identity.ID)
	assert.Equal(t, http.StatusOK, e.apiMe(mjar).Code)

	// The setting admits the same identity.
	e.srv.testAdminWeb = testAdminWebState{on: true}
	ajar := e.adminLogin(iss, adm.Identity.ID)
	assert.Equal(t, http.StatusOK, e.apiMe(ajar).Code)
	assert.Equal(t, http.StatusOK, e.page(ajar).Code)
}

// --- B3: each kind at its own level ------------------------------------------

// test_hub_admin is admitted at admin level and above, test_super_admin
// only at super-admin; below its level a kind gets no code and its live
// session is refused. Member-tier sessions are unaffected throughout.
func TestAdminWebSessions_KindAdmittedOnlyAtItsLevel(t *testing.T) {
	e := newWS4Env(t, true, true)
	issA := e.issuer("ws4-b3-hubadmin-iss")
	issS := e.issuer("ws4-b3-super-iss")
	hubAdm := e.issueAdmin(issA, testAdminRoleHubAdmin)
	superAdm := e.issueAdmin(issS, testAdminRoleSuperAdmin)

	// super-admin level: both kinds.
	hubJar := e.adminLogin(issA, hubAdm.Identity.ID)
	superJar := e.adminLogin(issS, superAdm.Identity.ID)
	assert.Equal(t, http.StatusOK, e.apiMe(hubJar).Code)
	assert.Equal(t, http.StatusOK, e.apiMe(superJar).Code)
	valid := e.classify(superJar)
	require.Equal(t, tiSessionValid, valid.state, valid.reason)
	assert.Equal(t, store.UserKindTestSuperAdmin, valid.row.Kind)

	// admin level: the super-admin kind is refused, the hub-admin kind is
	// not.
	e.srv.testAdminGate = testAdminGate{hubAdmin: true, hubID: taHubID, hubClass: config.HubClassTest}
	assert.Equal(t, http.StatusNotFound, e.mintAsRec(issS, superAdm.Identity.ID).Code)
	assert.Equal(t, "kind_not_admitted", e.classify(superJar).reason)
	e.assertSessionRefusedEverywhere(superJar, "super-admin kind at admin level")
	assert.Equal(t, http.StatusOK, e.apiMe(hubJar).Code)
	hubJar2 := e.adminLogin(issA, hubAdm.Identity.ID)
	assert.Equal(t, http.StatusOK, e.apiMe(hubJar2).Code)

	// member level: no admin kind.
	e.srv.testAdminGate = testAdminGate{}
	assert.Equal(t, http.StatusNotFound, e.mintAsRec(issA, hubAdm.Identity.ID).Code)
	assert.Equal(t, "kind_not_admitted", e.classify(hubJar2).reason)
	e.assertSessionRefusedEverywhere(hubJar2, "hub-admin kind at member level")

	fx := e.issue()
	mjar := e.login(fx.Identity.ID)
	assert.Equal(t, http.StatusOK, e.apiMe(mjar).Code)

	// The gate itself, per kind and level.
	for _, tc := range []struct {
		gate       testAdminGate
		web        bool
		hub, super bool
	}{
		{testAdminGate{hubAdmin: true, superAdmin: true}, true, true, true},
		{testAdminGate{hubAdmin: true}, true, true, false},
		{testAdminGate{}, true, false, false},
		{testAdminGate{hubAdmin: true, superAdmin: true}, false, false, false},
	} {
		e.srv.testAdminGate = tc.gate
		e.srv.testAdminWeb = testAdminWebState{on: tc.web}
		okHub, _ := e.srv.testAdminWebSessionGate(testAdminKindHubAdmin)
		okSuper, _ := e.srv.testAdminWebSessionGate(testAdminKindSuperAdmin)
		assert.Equal(t, tc.hub, okHub, "%+v web=%v", tc.gate, tc.web)
		assert.Equal(t, tc.super, okSuper, "%+v web=%v", tc.gate, tc.web)
		assert.Equal(t, tc.hub, e.srv.testInfraGates().TestAdminWebSessions, "status %+v web=%v", tc.gate, tc.web)
	}
}

// The code for an admin-kind identity takes what issuing one takes: the
// kind's capability on the request (an access token needs exactly the
// kind's scope), a live super-admin issuer, and the identity's issuer (or
// an unscoped platform admin session). A member-tier identity still needs
// test_identity.issue. Each refusal is pinned to the check that makes it.
func TestAdminWebSessions_MintNeedsTheKindsIssuerChecks(t *testing.T) {
	e := newWS4Env(t, false, true)
	iss := e.issuer("ws4-mint-iss")
	adm := e.issueAdmin(iss, testAdminRoleHubAdmin)
	path := ws4WebCodePath(adm.Identity.ID)

	// The issuer's token scoped to exactly the admin capability: 201.
	adminKey, _ := taPAT(t, e.srv, iss.ID, uatScopeForPermission(permissionTestIdentityIssueAdmin))
	rec := doRequestWithToken(t, e.srv, adminKey, http.MethodPost, path, nil)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// The issuer's token scoped to the member capability: 403 from the
	// kind's capability check (testAdminWebCodeIssuer).
	memberKey, _ := taPAT(t, e.srv, iss.ID, "test_identity:issue")
	rec = doRequestWithToken(t, e.srv, memberKey, http.MethodPost, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	msg, reason := ws4ErrorBody(t, rec)
	assert.Equal(t, "requires test_identity.issue_admin permission", msg)
	assert.Empty(t, reason)

	// Another issuer's member token: 404 (not its identity).
	rec = doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, path, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// A member-tier identity issued by the same issuer: its session mints
	// a code, the admin-scoped token gets 404 (it does not hold
	// test_identity.issue, which a member-tier identity needs).
	rec = doRequestAsUser(t, e.srv, iss, http.MethodPost, "/api/v1/test-identities", map[string]string{"role": "member", "purpose": "ws4 member"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	member := tiDecode(t, rec)
	rec = doRequestWithToken(t, e.srv, adminKey, http.MethodPost, ws4WebCodePath(member.Identity.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusCreated, e.mintAsRec(iss, member.Identity.ID).Code)

	// The admin identity's own bearer: 403 (a test identity never issues).
	rec = doRequestWithToken(t, e.srv, adm.AccessToken, http.MethodPost, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// An issuer that still holds both capabilities through its super-admin
	// binding but is no longer a live super-admin (its stored role is not
	// admin): 403 from canIssueTestAdmin.
	row := e.row(iss.ID)
	row.Role = store.UserRoleMember
	require.NoError(t, e.st.UpdateUser(context.Background(), row))
	rec = e.mintAsRec(iss, adm.Identity.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	msg, reason = ws4ErrorBody(t, rec)
	assert.Equal(t, "requires test_identity.issue_admin permission", msg)
	assert.Equal(t, testAdminReasonNotLiveAdmin, reason)
}

// ws4ErrorBody decodes an API error body into its message and
// details.reason.
func ws4ErrorBody(t *testing.T, rec *httptest.ResponseRecorder) (message, reason string) {
	t.Helper()
	var body struct {
		Error struct {
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	reason, _ = body.Error.Details["reason"].(string)
	return body.Error.Message, reason
}

// --- B4: revoke-all ends a live admin browser session ------------------------

// revoke-all bumps every admin-kind row's session generation before it
// deletes anything, so a live admin web session is refused on its next
// page, API and event stream request even while the row itself is still
// there (here: the delete is rolled back because the audit sink is down).
// Once the sink is back, revoke-all deletes the row and the session stays
// refused.
func TestAdminWebSessions_RevokeAllEndsLiveSession(t *testing.T) {
	e := newWS4Env(t, false, true)
	iss := e.issuer("ws4-b4-iss")
	adm := e.issueAdmin(iss, testAdminRoleHubAdmin)
	jar := e.adminLogin(iss, adm.Identity.ID)
	require.Equal(t, http.StatusOK, e.page(jar).Code)
	require.Equal(t, http.StatusOK, e.apiMe(jar).Code)
	before := e.row(adm.Identity.ID).SessionGeneration

	// The row delete lags: the sink write fails, so the delete transaction
	// rolls back and the row stays, active.
	e.sink.mu.Lock()
	e.sink.err = errors.New("sink unavailable")
	e.sink.mu.Unlock()
	rec := doRequestAsUser(t, e.srv, iss, http.MethodPost, taRevokeAllPath, nil)
	require.Contains(t, []int{http.StatusServiceUnavailable, http.StatusInternalServerError}, rec.Code, rec.Body.String())
	row := e.row(adm.Identity.ID)
	assert.Equal(t, store.UserStatusActive, row.Status, "the row is still there and active")
	assert.Equal(t, before+1, row.SessionGeneration, "revoke-all bumped the session generation first")

	assert.Equal(t, "session_generation_mismatch", e.classify(jar).reason)
	e.assertSessionRefusedEverywhere(jar, "after revoke-all with the row still present")

	// The sink is back: revoke-all deletes the row.
	e.sink.mu.Lock()
	e.sink.err = nil
	e.sink.mu.Unlock()
	resp := taRevokeAll(t, e.srv, iss)
	require.Equal(t, 1, resp.Count)
	assert.Equal(t, "deleted", resp.Revoked[0].Outcome)
	_, err := e.st.GetUser(context.Background(), adm.Identity.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	e.assertSessionRefusedEverywhere(jar, "after revoke-all deleted the row")
}

// --- B5: session-originated admin requests are in the audit sink ---------------

// A request from an admin web session writes the same chained request and
// response lines as a request with the identity's own token (user, kind,
// issuance), marked credential "web_session" instead of "bearer"; a
// policy refusal is marked the same way, and so are mutation audit labels.
func TestAdminWebSessions_SessionRequestsInAuditSink(t *testing.T) {
	e := newWS4Env(t, true, true)
	iss := e.issuer("ws4-b5-iss")
	adm := e.issueAdmin(iss, testAdminRoleSuperAdmin)
	jar := e.adminLogin(iss, adm.Identity.ID)

	startReq := len(tsaSinkLines(e.sink, testAdminAuditEventRequest))
	startResp := len(tsaSinkLines(e.sink, testAdminAuditEventResponse))
	rec := e.do(tiReq{path: "/api/v1/users/" + iss.ID, cookies: jar})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	webTok := e.mintedToken()
	rec = doRequestWithToken(t, e.srv, adm.AccessToken, http.MethodGet, "/api/v1/users/"+iss.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	reqLines := tsaSinkLines(e.sink, testAdminAuditEventRequest)[startReq:]
	respLines := tsaSinkLines(e.sink, testAdminAuditEventResponse)[startResp:]
	require.Len(t, reqLines, 2)
	require.Len(t, respLines, 2)
	for i, want := range []string{testAdminCredentialWebSession, testAdminCredentialBearer} {
		for _, l := range []map[string]any{reqLines[i], respLines[i]} {
			assert.Equal(t, adm.Identity.ID, l["user_id"])
			assert.Equal(t, store.UserKindTestSuperAdmin, l["kind"])
			assert.Equal(t, adm.Identity.IssuanceID, l["issuance_id"])
			assert.Equal(t, http.MethodGet, l["method"])
			assert.Equal(t, want, l["credential"])
		}
		assert.Equal(t, "allowed", respLines[i]["decision"])
	}

	// The marker rests on the signed client type claim: the web session's
	// token is client type web, the identity's own token api.
	webClaims, err := e.srv.userTokenService.ValidateUserToken(webTok)
	require.NoError(t, err)
	assert.Equal(t, ClientTypeWeb, webClaims.ClientType)
	apiClaims, err := e.srv.userTokenService.ValidateUserToken(adm.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, ClientTypeAPI, apiClaims.ClientType)

	// A policy refusal from the session is marked too.
	startDenial := len(tsaSinkLines(e.sink, testAdminAuditEventPolicyDenial))
	rec = e.do(tiReq{path: "/api/v1/admin/server-config", cookies: jar})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	denials := tsaSinkLines(e.sink, testAdminAuditEventPolicyDenial)[startDenial:]
	require.NotEmpty(t, denials)
	for _, d := range denials {
		assert.Equal(t, adm.Identity.ID, d["user_id"])
		assert.Equal(t, testAdminCredentialWebSession, d["credential"])
	}

	n, err := verifyTestAdminAuditChain(strings.NewReader(e.sink.String()))
	require.NoError(t, err)
	assert.Equal(t, len(tsaSinkLines(e.sink, "")), n)

	// Mutation audit labels carry the credential as well.
	row := e.row(adm.Identity.ID)
	for _, cred := range []string{testAdminCredentialWebSession, testAdminCredentialBearer} {
		actor := auditActorFromContext(contextWithTestAdminActor(context.Background(), row, cred))
		var labels map[string]string
		require.NoError(t, json.Unmarshal([]byte(actor.CredentialLabels), &labels), actor.CredentialLabels)
		assert.Equal(t, cred, labels["credential"])
		assert.Equal(t, store.UserKindTestSuperAdmin, labels["kind"])
		assert.Equal(t, adm.Identity.IssuanceID, labels["issuance_id"])
	}
	assert.Equal(t, testAdminCredentialWebSession, testAdminCredentialFor(ClientTypeWeb))
	assert.Equal(t, testAdminCredentialBearer, testAdminCredentialFor(ClientTypeAPI))
}

// --- B6: the kind's maximum lifetime -------------------------------------------

// An admin web session never outlives the kind's maximum identity
// lifetime (60 minutes for test_hub_admin, 30 for test_super_admin), the
// cap is re-checked on every consumer, and an expired session is refused
// everywhere.
func TestAdminWebSessions_LifetimeCappedAtKindMax(t *testing.T) {
	// The cap itself.
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	far := now.Add(2 * time.Hour)
	hubRow := &store.User{Kind: store.UserKindTestHubAdmin, Created: now, ExpiresAt: &far}
	superRow := &store.User{Kind: store.UserKindTestSuperAdmin, Created: now, ExpiresAt: &far}
	fixtureRow := &store.User{Kind: store.UserKindTestFixture, Created: now, ExpiresAt: &far}
	assert.Equal(t, now.Add(testHubAdminMaxLifetime), testAdminWebSessionExpiry(hubRow, now))
	assert.Equal(t, now.Add(testSuperAdminMaxLifetime), testAdminWebSessionExpiry(superRow, now))
	assert.Equal(t, far, testAdminWebSessionExpiry(fixtureRow, now), "member tier: the identity's expiry")
	oldRow := &store.User{Kind: store.UserKindTestHubAdmin, Created: now.Add(-50 * time.Minute), ExpiresAt: &far}
	assert.Equal(t, now.Add(10*time.Minute), testAdminWebSessionExpiry(oldRow, now), "capped from the identity's creation")
	near := now.Add(5 * time.Minute)
	nearRow := &store.User{Kind: store.UserKindTestHubAdmin, Created: now, ExpiresAt: &near}
	assert.Equal(t, near, testAdminWebSessionExpiry(nearRow, now), "never past the identity's expiry")

	assert.Equal(t, "", testIdentityWebSessionExpiryReason(hubRow, now.Add(testHubAdminMaxLifetime)))
	assert.Equal(t, "session_expiry_beyond_kind_cap", testIdentityWebSessionExpiryReason(hubRow, now.Add(testHubAdminMaxLifetime+time.Millisecond)))
	assert.Equal(t, "session_expiry_beyond_kind_cap", testIdentityWebSessionExpiryReason(superRow, now.Add(testSuperAdminMaxLifetime+time.Millisecond)))
	assert.Equal(t, "session_expiry_beyond_identity", testIdentityWebSessionExpiryReason(hubRow, far.Add(time.Millisecond)))
	assert.Equal(t, "", testIdentityWebSessionExpiryReason(fixtureRow, far))
	assert.Equal(t, "session_expiry_beyond_identity", testIdentityWebSessionExpiryReason(fixtureRow, far.Add(time.Millisecond)))
	assert.Equal(t, time.Duration(0), testAdminKindMaxLifetime(store.UserKindTestFixture))

	e := newWS4Env(t, false, true)
	iss := e.issuer("ws4-b6-iss")
	adm := e.issueAdmin(iss, testAdminRoleHubAdmin)
	row := e.row(adm.Identity.ID)

	// Redeem: the session expiry and the cookie lifetime stay within the
	// identity's expiry and the kind's maximum.
	rec := e.redeem(e.mintAs(iss, adm.Identity.ID).Code)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	jar := tiWebApplyCookies(nil, rec)
	tiExp, ok := e.sessionValues(jar)[sessKeyTIExpiry].(int64)
	require.True(t, ok, "the session carries its expiry")
	assert.LessOrEqual(t, tiExp, row.ExpiresAt.UnixMilli())
	assert.LessOrEqual(t, tiExp, row.Created.Add(testHubAdminMaxLifetime).UnixMilli())
	assert.LessOrEqual(t, tiExp, e.clock.Now().Add(testHubAdminMaxLifetime).UnixMilli())
	sess := tiWebSetCookie(rec, webSessionName)
	require.NotNil(t, sess)
	assert.LessOrEqual(t, sess.MaxAge, int(testHubAdminMaxLifetime/time.Second))
	assert.Equal(t, http.StatusOK, e.apiMe(jar).Code)

	// A row whose expiry lies beyond the kind's maximum (written directly;
	// no issuance path writes one): a session within the cap works, one
	// past it is refused on every consumer.
	created := e.clock.Now().Add(-10 * time.Minute)
	raw := ws4RawUser(t, e.st, store.UserKindTestHubAdmin, "test-admin-"+generateID()[:8]+"@"+store.TestHubAdminEmailDomain,
		store.UserRoleMember, created, e.clock.Now().Add(2*time.Hour))
	within := e.craftMarkedSession(raw, generateID(), raw.Created.Add(50*time.Minute), raw.SessionGeneration)
	assert.Equal(t, http.StatusOK, e.apiMe(within).Code)
	beyond := e.craftMarkedSession(raw, generateID(), raw.Created.Add(90*time.Minute), raw.SessionGeneration)
	assert.Equal(t, "session_expiry_beyond_kind_cap", e.classify(beyond).reason)
	e.assertSessionRefusedEverywhere(beyond, "session past the kind's maximum lifetime")

	// Expiry passes (C5): the identity is live, the session is not.
	short := e.craftMarkedSession(raw, generateID(), e.clock.Now().Add(time.Minute), raw.SessionGeneration)
	require.Equal(t, http.StatusOK, e.apiMe(short).Code)
	e.clock.Advance(2 * time.Minute)
	assert.Equal(t, "session_expired", e.classify(short).reason)
	e.assertSessionRefusedEverywhere(short, "expired admin session")
}

// --- B7: the classifier decides by kind -----------------------------------------

// Every kind other than human and federated is a test identity session
// for the classifier, decided from the row's kind alone: here each row has
// an ordinary email outside every reserved domain, so the WS1 check (kind
// test_fixture or a reserved-domain email) would have missed every kind but
// test_fixture, including a kind this binary does not know. An unmarked
// session for such a row is invalid. Human and federated rows stay
// ordinary accounts for the classifier (federated accounts belong to the
// federation code), but a marked session (a scion_ti cookie and marker
// values) for a federated or unknown-kind row is still refused.
func TestAdminWebSessions_ClassifierKeysOnKind(t *testing.T) {
	e := newWS4Env(t, true, true)
	now := e.clock.Now()
	for _, tc := range []struct {
		kind     string
		testKind bool
	}{
		{store.UserKindHuman, false},
		{store.UserKindTestFixture, true},
		{store.UserKindTestHubAdmin, true},
		{store.UserKindTestSuperAdmin, true},
		{store.UserKindFederated, false},
		{"future_kind", true},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			email := "ws4-" + generateID()[:8] + "@example.com"
			row := ws4RawUser(t, e.st, tc.kind, email, store.UserRoleMember, now, now.Add(30*time.Minute))
			require.False(t, isReservedTestIdentityEmail(row.Email), "an ordinary email")
			assert.Equal(t, tc.testKind, testIdentityRowByKind(row))
			assert.Equal(t, tc.kind == store.UserKindHuman, row.IsHumanKind())

			res := e.classify(loginSession(t, e.ws, row.ID, row.Email, row.Role))
			if tc.kind == store.UserKindFederated {
				marked := e.craftMarkedSession(row, generateID(), now.Add(10*time.Minute), row.SessionGeneration)
				assert.Equal(t, "kind_not_admitted", e.classify(marked).reason)
			}
			if !tc.testKind {
				assert.Equal(t, tiSessionNotFixture, res.state)
				return
			}
			assert.Equal(t, tiSessionInvalid, res.state, "an unmarked session for a %s row", tc.kind)
			assert.Equal(t, "marker_missing_or_invalid", res.reason)
			if tc.kind != store.UserKindTestFixture {
				assert.False(t, row.IsTestFixture() || isReservedTestIdentityEmail(row.Email), "outside the WS1 check")
			}
			if tc.kind == "future_kind" {
				marked := e.craftMarkedSession(row, generateID(), now.Add(10*time.Minute), row.SessionGeneration)
				assert.Equal(t, "kind_not_admitted", e.classify(marked).reason)
			}
		})
	}
}

// --- B8: the session token has the authority of the kind's own bearer ----------

// The token the web server mints per request for an admin web session
// (client type web) and the identity's own issued token (client type api)
// reach identical decisions over the whole permission registry, the
// session-only gate and every permission route guard, and carry the same
// admin-kind actor apart from the credential marker.
func TestAdminWebSessions_DecideEquivalentToKindBearer(t *testing.T) {
	for _, adminRole := range []string{testAdminRoleHubAdmin, testAdminRoleSuperAdmin} {
		t.Run(adminRole, func(t *testing.T) {
			e := newWS4Env(t, true, true)
			iss := e.issuer("ws4-b8-iss")
			adm := e.issueAdmin(iss, adminRole)
			jar := e.adminLogin(iss, adm.Identity.ID)
			require.Equal(t, http.StatusOK, e.apiMe(jar).Code)
			webTok := e.mintedToken()
			require.NotEmpty(t, webTok)
			apiTok := adm.AccessToken

			ctxFor := func(tok string) context.Context {
				var got context.Context
				h := UnifiedAuthMiddleware(e.srv.authConfig)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Context() }))
				req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
				req.Header.Set("Authorization", "Bearer "+tok)
				h.ServeHTTP(httptest.NewRecorder(), req)
				require.NotNil(t, got, "token must authenticate")
				return got
			}
			webCtx, apiCtx := ctxFor(webTok), ctxFor(apiTok)
			webID := GetIdentityFromContext(webCtx).(UserIdentity)
			apiID := GetIdentityFromContext(apiCtx).(UserIdentity)
			assert.Equal(t, adm.Identity.ID, webID.ID())
			assert.Equal(t, GetCredentialContextFromContext(apiCtx), GetCredentialContextFromContext(webCtx))
			assert.Equal(t, CredentialKindInteractive, GetCredentialContextFromContext(webCtx).Kind)
			pw, pa := principalContextForIdentity(webID), principalContextForIdentity(apiID)
			assert.Equal(t, pa.Kind, pw.Kind)
			assert.Equal(t, pa.ID, pw.ID)
			assert.Equal(t, sessionCredentialAllowed(apiCtx), sessionCredentialAllowed(webCtx), "session-only gate")

			taWeb, okWeb := testAdminActorFromContext(webCtx)
			taAPI, okAPI := testAdminActorFromContext(apiCtx)
			require.True(t, okWeb)
			require.True(t, okAPI)
			assert.Equal(t, testAdminCredentialWebSession, taWeb.Credential)
			assert.Equal(t, testAdminCredentialBearer, taAPI.Credential)
			taWeb.Credential, taAPI.Credential = "", ""
			assert.Equal(t, taAPI, taWeb, "the same admin-kind actor apart from the credential")

			n := 0
			for _, p := range permissions.Registry {
				for _, res := range []Resource{{Type: p.Resource}, {Type: p.Resource, ID: "ws4-target"}, {Type: "project", ID: "ws4-project"}} {
					req := func(id UserIdentity) AuthzRequest {
						return AuthzRequest{
							Principal:      principalContextForIdentity(id),
							Credential:     credentialContextForIdentity(id),
							Resource:       res,
							Action:         Action(p.Action),
							Permission:     p.ID,
							TargetEvidence: hubCollectionEvidence(p.ID),
						}
					}
					dw := e.srv.authzService.Decide(webCtx, req(webID))
					da := e.srv.authzService.Decide(apiCtx, req(apiID))
					assert.Equal(t, da, dw, "%s on %+v", p.ID, res)
					n++
				}
			}
			assert.Greater(t, n, 100, "the whole registry is covered")

			routes := 0
			for pattern, meta := range routeMetadataTable {
				if meta.Permission == "" {
					continue
				}
				rw := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(webCtx)
				ra := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(apiCtx)
				dw := e.srv.routePermissionDecision(rw, meta)
				da := e.srv.routePermissionDecision(ra, meta)
				assert.Equal(t, da.outcome, dw.outcome, pattern)
				assert.Equal(t, da.reason, dw.reason, pattern)
				routes++
			}
			assert.Greater(t, routes, 10)
		})
	}
}

// --- B2: startup refusals and the never-writable setting ------------------------

// New refuses the setting below an admin level, with another hub id, and
// without the test admin audit sink; at an admin level it starts, logs the
// resolved state, writes it in the gate-start line, and reports it in
// test-infra status (true only while web sessions are available).
func TestAdminWebSessions_NewRefusesMisconfiguration(t *testing.T) {
	t.Run("below admin level", func(t *testing.T) {
		tiaCaptureAudit(t)
		cfg := tiaGateServerConfig()
		cfg.TestAdminGate = TestAdminGateConfig{}
		cfg.TestMode.Level = TestModeMember
		cfg.TestAdminWebSessions = TestAdminWebSessionsConfig{HubID: tiaHubID}
		_, _, err := tiaNewServer(t, cfg)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTestAdminWebSessionsBelowAdmin)
		assert.Contains(t, err.Error(), config.TestIdentityAdminWebSessionsKey)
	})
	t.Run("another hub id", func(t *testing.T) {
		tiaCaptureAudit(t)
		cfg := tiaGateServerConfig()
		cfg.TestAdminWebSessions = TestAdminWebSessionsConfig{HubID: "another-hub"}
		_, _, err := tiaNewServer(t, cfg)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTestAdminWebSessionsHubIDMismatch)
	})
	t.Run("no audit sink", func(t *testing.T) {
		prev := testAdminAuditOutput
		testAdminAuditOutput = nil
		t.Cleanup(func() { testAdminAuditOutput = prev })
		cfg := tiaGateServerConfig()
		cfg.TestAdminWebSessions = TestAdminWebSessionsConfig{HubID: tiaHubID}
		_, _, err := tiaNewServer(t, cfg)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTestAdminWebSessionsRequiresAuditSink)
	})
	t.Run("admin level without a web session key", func(t *testing.T) {
		buf := tiaCaptureAudit(t)
		logs := tiWebCaptureLogs(t)
		cfg := tiaGateServerConfig()
		cfg.TestAdminWebSessions = TestAdminWebSessionsConfig{HubID: tiaHubID}
		srv, _, err := tiaNewServer(t, cfg)
		require.NoError(t, err)
		assert.True(t, srv.testAdminWeb.on)
		assert.False(t, srv.testAdminWebSessionsAvailable(), "member web sessions are unavailable without the key")
		assert.Contains(t, logs.String(), "Admin test identity web sessions are configured but unavailable")
		assert.Equal(t, true, ws4GateStartLine(t, buf.String())["admin_web_sessions"])
		st := ws4Status(t, srv)
		assert.False(t, st.TestAdminWebSessions)
		assert.True(t, st.TestHubAdmin)
	})
	t.Run("admin level with a web session key", func(t *testing.T) {
		tiaCaptureAudit(t)
		logs := tiWebCaptureLogs(t)
		cfg := tiaGateServerConfig()
		cfg.TestAdminWebSessions = TestAdminWebSessionsConfig{HubID: tiaHubID}
		cfg.TestIdentityWebSessionKey = TestIdentityWebSessionKey{Key: tiWebKey(t)}
		srv, _, err := tiaNewServer(t, cfg)
		require.NoError(t, err)
		assert.True(t, srv.testAdminWebSessionsAvailable())
		assert.Contains(t, logs.String(), "Admin test identity web sessions are ENABLED")
		assert.True(t, ws4Status(t, srv).TestAdminWebSessions)
	})
	t.Run("unset", func(t *testing.T) {
		buf := tiaCaptureAudit(t)
		srv, _, err := tiaNewServer(t, tiaGateServerConfig())
		require.NoError(t, err)
		assert.False(t, srv.testAdminWeb.on)
		assert.Equal(t, false, ws4GateStartLine(t, buf.String())["admin_web_sessions"])
		assert.False(t, ws4Status(t, srv).TestAdminWebSessions)
	})
}

// The setting is never writable through the settings API, in file, DB and
// workstation mode (server.hub.test_identities and every key under it are
// in neverAPIWritableSettingsKeys).
func TestAdminWebSessions_SettingNeverAPIWritable(t *testing.T) {
	bodies := map[string]string{
		"nested":      `{"server":{"hub":{"test_identities":{"admin_web_sessions":"hub-x"}}}}`,
		"flat dotted": `{"server.hub.test_identities.admin_web_sessions":"hub-x"}`,
		"case folded": `{"Server":{"Hub":{"Test_Identities":{"Admin_Web_Sessions":"hub-x"}}}}`,
	}
	for name, body := range bodies {
		t.Run("file/"+name, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv, body)
			assert.Equal(t, []string{config.TestIdentitiesKey}, tiaErrorKeys(t, rr))
			_, err := os.Stat(settingsPath)
			assert.True(t, os.IsNotExist(err), "a refused PUT writes no settings file")
		})
		for _, workstation := range []bool{false, true} {
			t.Run(map[bool]string{false: "db", true: "workstation"}[workstation]+"/"+name, func(t *testing.T) {
				settingsPath := workstationHome(t)
				srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
				before := hubSettingRevisions(t, st)
				rr := putServerConfig(t, srv, body)
				assert.Equal(t, []string{config.TestIdentitiesKey}, tiaErrorKeys(t, rr))
				data, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.NotContains(t, string(data), "admin_web_sessions")
				assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
			})
		}
	}
	assert.True(t, strings.HasPrefix(config.TestIdentityAdminWebSessionsKey, config.TestIdentitiesKey+"."))
}

// ws4Status reads GET /api/v1/test-infra/status, requiring 200.
func ws4Status(t *testing.T, srv *Server) TestInfraStatusResponse {
	t.Helper()
	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/test-infra/status", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var st TestInfraStatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st), rec.Body.String())
	return st
}

// ws4GateStartLine returns the decoded test_admin_gate_start line of the
// test admin audit sink output out, requiring exactly one.
func ws4GateStartLine(t *testing.T, out string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, testAdminAuditTag+" ") {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(l, testAdminAuditTag+" ")), &m), l)
		if m["event"] == testAdminAuditEventGateStart {
			found = append(found, m)
		}
	}
	require.Len(t, found, 1, "one gate-start line")
	return found[0]
}

// ws4BumpFailStore fails every session generation bump.
type ws4BumpFailStore struct{ store.Store }

func (ws4BumpFailStore) IncrementSessionGeneration(context.Context, string) error {
	return errors.New("session generation bump unavailable")
}

// revoke-all fails closed on the session generation bump: when a bump
// fails it answers 503 and deletes or suspends nothing, so it never
// reports a revocation that may have left a live session behind. Once the
// bump works again, revoke-all ends the session and deletes the row.
func TestAdminWebSessions_RevokeAllFailsClosedOnBumpError(t *testing.T) {
	e := newWS4Env(t, false, true)
	iss := e.issuer("ws4-b4b-iss")
	adm := e.issueAdmin(iss, testAdminRoleHubAdmin)
	jar := e.adminLogin(iss, adm.Identity.ID)
	require.Equal(t, http.StatusOK, e.apiMe(jar).Code)
	before := e.row(adm.Identity.ID)

	orig := e.srv.store
	e.srv.store = ws4BumpFailStore{orig}
	rec := doRequestAsUser(t, e.srv, iss, http.MethodPost, taRevokeAllPath, nil)
	e.srv.store = orig
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	after := e.row(adm.Identity.ID)
	assert.Equal(t, store.UserStatusActive, after.Status, "nothing was suspended")
	assert.Equal(t, before.SessionGeneration, after.SessionGeneration)
	assert.Empty(t, tiWebAudits(t, e.st, testAdminRevokeMutation), "no revoke audit record")
	assert.NotContains(t, e.sink.String(), `"event":"`+testAdminAuditEventRevoke+`"`, "no revoke sink line")

	resp := taRevokeAll(t, e.srv, iss)
	require.Equal(t, 1, resp.Count)
	assert.Equal(t, "deleted", resp.Revoked[0].Outcome)
	e.assertSessionRefusedEverywhere(jar, "after revoke-all")
}

// ws4DecisionRecorder records the permission of every decision audit
// record.
type ws4DecisionRecorder struct {
	mu    sync.Mutex
	perms []string
}

func (r *ws4DecisionRecorder) EmitDecisionAudit(_ context.Context, rec *store.DecisionAuditRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.perms = append(r.perms, rec.PermissionID)
}

func (r *ws4DecisionRecorder) count(perm string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.perms {
		if p == perm {
			n++
		}
	}
	return n
}

// A routine member-tier mint decides only test_identity.issue: the admin
// capabilities are decided (and audited) only for an admin-kind code.
func TestAdminWebSessions_MemberMintDecidesOnlyTheMemberCapability(t *testing.T) {
	e := newWS4Env(t, false, true)
	fx := e.issue()
	rec := &ws4DecisionRecorder{}
	e.srv.authzService.SetDecisionAuditEmitter(rec)
	t.Cleanup(func() { e.srv.authzService.SetDecisionAuditEmitter(nil) })

	e.mint(fx.Identity.ID)
	assert.Positive(t, rec.count(permissionTestIdentityIssue), "the member capability is decided")
	assert.Zero(t, rec.count(permissionTestIdentityIssueAdmin), "no admin capability decision for a member-tier code")
	assert.Zero(t, rec.count(permissionTestIdentityIssueSuperAdmin), "no super-admin capability decision for a member-tier code")

	// An admin-kind code by a super-admin session decides the kind's
	// capability once more, in the issuer checks.
	iss := e.issuer("ws4-f6-iss")
	adm := e.issueAdmin(iss, testAdminRoleHubAdmin)
	beforeAdmin := rec.count(permissionTestIdentityIssueAdmin)
	e.mintAs(iss, adm.Identity.ID)
	assert.Greater(t, rec.count(permissionTestIdentityIssueAdmin), beforeAdmin)
	assert.Zero(t, rec.count(permissionTestIdentityIssueSuperAdmin))
}
