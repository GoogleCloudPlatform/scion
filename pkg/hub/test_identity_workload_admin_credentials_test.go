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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Admin-kind credentials through an admin-scope workload delegation
// (ptone/scion#4240, phase X4b): token re-issue and web-login codes for
// the admin-kind identities the same delegation issued. The harness is
// WS4's web environment (a web server in front of the hub, a hub-local web
// session key, the admin web session switch) with X4's workload issuer on
// top. Every identity here is synthetic.

// x4bEnv is an X4 hub inside a WS4 web environment.
type x4bEnv struct {
	*x4Env
	web *ws4Env
}

// newX4bEnv builds the environment: the hub-admin kind on (super-admin
// too when superAdmin), the admin web session switch set to adminWeb, and
// the X4 workload issuer allowlist.
func newX4bEnv(t *testing.T, superAdmin, adminWeb bool) *x4bEnv {
	t.Helper()
	w := newWS4Env(t, superAdmin, adminWeb)
	return &x4bEnv{x4Env: setupX4(t, w.srv, w.st, w.sink, superAdmin), web: w}
}

// adminWorkloadToken returns a fresh workload token of the admin subject, valid
// for an hour from the hub's clock.
func (e *x4bEnv) adminWorkloadToken(t *testing.T) string {
	t.Helper()
	now := e.srv.testIdentities.clock()
	return x2Token(t, x2TokenOpts{sub: x4AdminSub, iat: now.Add(-time.Minute), exp: now.Add(time.Hour)})
}

// memberWorkloadToken returns a fresh workload token of the member subject.
func (e *x4bEnv) memberWorkloadToken(t *testing.T) string {
	t.Helper()
	now := e.srv.testIdentities.clock()
	return x2Token(t, x2TokenOpts{iat: now.Add(-time.Minute), exp: now.Add(time.Hour)})
}

// reissue calls the token route for id through the admin-subject
// delegation delegationID.
func (e *x4bEnv) reissue(t *testing.T, delegationID, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return e.workload(t, http.MethodPost, "/api/v1/test-identities/"+id+"/token", e.adminWorkloadToken(t), delegationID, body, nil)
}

// webCode calls the web-login code route for id through the
// admin-subject delegation delegationID.
func (e *x4bEnv) webCode(t *testing.T, delegationID, id string) *httptest.ResponseRecorder {
	t.Helper()
	return e.workload(t, http.MethodPost, ws4WebCodePath(id), e.adminWorkloadToken(t), delegationID, nil, nil)
}

// issueThrough issues an admin kind of role through delegationID,
// requiring 201.
func (e *x4bEnv) issueThrough(t *testing.T, delegationID, role string) TestAdminIdentityTokenResponse {
	t.Helper()
	rec := e.workload(t, http.MethodPost, taPath, e.adminWorkloadToken(t), delegationID, taBody(role), nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return taDecode(t, rec)
}

func (e *x4bEnv) row(t *testing.T, id string) *store.User {
	t.Helper()
	u, err := e.s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

// audits returns the mutation audit records of type mt for id.
func (e *x4bEnv) audits(t *testing.T, mt, id string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := e.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mt, TargetID: id})
	require.NoError(t, err)
	return recs
}

// sinkLinesFor returns the sink lines of event for identity id.
func (e *x4bEnv) sinkLinesFor(event, id string) []map[string]any {
	var out []map[string]any
	for _, l := range tsaSinkLines(e.sink, event) {
		if l["test_identity_id"] == id {
			out = append(out, l)
		}
	}
	return out
}

// x4bError is the decoded error envelope of a response.
type x4bError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func x4bErrorOf(t *testing.T, rec *httptest.ResponseRecorder) x4bError {
	t.Helper()
	var body struct {
		Error x4bError `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error
}

// x4bWorkloadSummary is the workload delegation part of an audit summary.
type x4bWorkloadSummary struct {
	Kind               string `json:"kind"`
	IssuerCredentialID string `json:"issuer_credential_id"`
	TokenTTLSeconds    int64  `json:"token_ttl_seconds"`
	Workload           *struct {
		DelegationID string `json:"delegation_id"`
		Grantor      string `json:"grantor"`
		WorkloadSub  string `json:"workload_sub"`
	} `json:"workload_delegation"`
}

var x4bKinds = []struct {
	scope, role, kind string
	spec              testAdminKindSpec
}{
	{testIdentityDelegationAdminScope, testAdminRoleHubAdmin, store.UserKindTestHubAdmin, testAdminKindSpecs[testAdminRoleHubAdmin]},
	{testIdentityDelegationSuperAdminScope, testAdminRoleSuperAdmin, store.UserKindTestSuperAdmin, testAdminKindSpecs[testAdminRoleSuperAdmin]},
}

// --- C2: admin token re-issue ---------------------------------------------------

// An admin-scope delegation re-issues the token of an admin-kind identity
// it issued: one token, at most the kind's token lifetime, never past the
// identity's expiry (which the re-issue does not move), with a mutation
// audit record and a sink line that carry the delegation id. A lifetime
// above the kind's maximum is refused, and once the identity has expired
// nothing more is minted.
func TestWorkloadAdminCredentials_TokenReissueOwnIdentity(t *testing.T) {
	for _, tc := range x4bKinds {
		t.Run(tc.role, func(t *testing.T) {
			e := newX4bEnv(t, true, false)
			grantor := taUser(t, e.s, "x4b-c2-"+tc.role, store.SystemRoleSuperAdmin)
			d := e.delegateScope(t, grantor, tc.scope, nil)
			issued := e.issueThrough(t, d.ID, tc.role)
			id := issued.Identity.ID
			before := e.row(t, id)
			require.NotNil(t, before.ExpiresAt)

			rec := e.reissue(t, d.ID, id, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			resp := taDecode(t, rec)
			assert.Equal(t, id, resp.Identity.ID)
			assert.Equal(t, tc.kind, resp.Identity.Kind)
			assert.Equal(t, tc.role, resp.Identity.AdminRole)
			assert.Equal(t, d.ID, resp.Identity.IssuerDelegationID)
			require.NotEmpty(t, resp.AccessToken)
			assert.NotEqual(t, issued.AccessToken, resp.AccessToken, "a new token")
			assert.LessOrEqual(t, taJWTLifetime(t, resp.AccessToken), tc.spec.defaultTokenTTL+time.Second, "the kind's default token lifetime")
			assert.Equal(t, http.StatusOK, doRequestWithToken(t, e.srv, resp.AccessToken, http.MethodGet, "/api/v1/auth/me", nil).Code, "the re-issued token works")

			after := e.row(t, id)
			require.NotNil(t, after.ExpiresAt)
			assert.True(t, before.ExpiresAt.Equal(*after.ExpiresAt), "re-issue never moves the identity's expiry")

			lines := e.sinkLinesFor(testAdminAuditEventTokenIssue, id)
			require.Len(t, lines, 1, "one test_admin_token_issue sink line")
			assert.Equal(t, d.ID, lines[0]["issuer_delegation_id"])
			assert.Equal(t, d.ID, lines[0]["issuer_credential_id"])
			assert.Equal(t, x4AdminSub, lines[0]["workload_sub"])
			assert.Equal(t, grantor.ID, lines[0]["issuer_id"])
			assert.Equal(t, tc.kind, lines[0]["kind"])
			assert.NotContains(t, lines[0], "access_token")

			audits := e.audits(t, testIdentityTokenIssueMutation, id)
			require.Len(t, audits, 1)
			var summary x4bWorkloadSummary
			require.NoError(t, json.Unmarshal([]byte(audits[0].AfterSummary), &summary))
			assert.Equal(t, tc.kind, summary.Kind)
			assert.Equal(t, d.ID, summary.IssuerCredentialID)
			require.NotNil(t, summary.Workload)
			assert.Equal(t, d.ID, summary.Workload.DelegationID)
			assert.Equal(t, grantor.ID, summary.Workload.Grantor)
			assert.Equal(t, x4AdminSub, summary.Workload.WorkloadSub)
			assert.NotContains(t, audits[0].AfterSummary, resp.AccessToken)

			// A token lifetime above the kind's maximum is refused.
			rec = e.reissue(t, d.ID, id, map[string]any{"tokenTtlSeconds": int64(tc.spec.maxTokenTTL/time.Second) + 1})
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, "tokenTtlSeconds", x4bErrorOf(t, rec).Details["field"])

			// Thirty seconds before the identity expires, the kind's
			// maximum token lifetime is cut to what is left.
			e.web.clock.Advance(after.ExpiresAt.Sub(e.srv.testIdentities.clock()) - 30*time.Second)
			rec = e.reissue(t, d.ID, id, map[string]any{"tokenTtlSeconds": int64(tc.spec.maxTokenTTL / time.Second)})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			late := taDecode(t, rec)
			assert.False(t, late.TokenExpiresAt.After(*after.ExpiresAt), "the token never outlives the identity")
			assert.LessOrEqual(t, late.ExpiresIn, int64(30))
			assert.True(t, after.ExpiresAt.Equal(*e.row(t, id).ExpiresAt))

			// Past its expiry: 409, nothing minted.
			e.web.clock.Advance(31 * time.Second)
			n := len(e.audits(t, testIdentityTokenIssueMutation, id))
			rec = e.reissue(t, d.ID, id, nil)
			assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Equal(t, testIdentityReasonExpired, taReason(t, rec))
			assert.NotContains(t, x4JSONKeys(t, rec.Body.Bytes()), "accessToken")
			assert.Len(t, e.audits(t, testIdentityTokenIssueMutation, id), n)
		})
	}
}

// The credential deadline: the identity's expiry, and for an admin kind
// never past its creation plus the kind's maximum lifetime; a row without
// an expiry, or an admin row without a creation time, gets none.
func TestWorkloadAdminCredentials_CredentialDeadline(t *testing.T) {
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	far := now.Add(3 * time.Hour)
	near := now.Add(10 * time.Minute)
	for _, tc := range []struct {
		name string
		row  *store.User
		want time.Time
		live bool
	}{
		{"hub-admin past its kind's maximum", &store.User{Kind: store.UserKindTestHubAdmin, Created: now, ExpiresAt: &far}, now.Add(testHubAdminMaxLifetime), true},
		{"super-admin past its kind's maximum", &store.User{Kind: store.UserKindTestSuperAdmin, Created: now, ExpiresAt: &far}, now.Add(testSuperAdminMaxLifetime), true},
		{"admin kind within its maximum", &store.User{Kind: store.UserKindTestHubAdmin, Created: now, ExpiresAt: &near}, near, true},
		{"member tier", &store.User{Kind: store.UserKindTestFixture, Created: now, ExpiresAt: &far}, far, true},
		{"no expiry", &store.User{Kind: store.UserKindTestHubAdmin, Created: now}, time.Time{}, false},
		{"admin kind without a creation time", &store.User{Kind: store.UserKindTestHubAdmin, ExpiresAt: &near}, time.Time{}, false},
	} {
		got, live := testIdentityCredentialDeadline(tc.row)
		assert.Equal(t, tc.live, live, tc.name)
		assert.True(t, tc.want.Equal(got), "%s: got %s, want %s", tc.name, got, tc.want)
	}
}

// Token re-issue is refused, with the member path's 404, for an identity
// the delegation did not issue: another delegation's (of the same grantor
// and kind), a human-issued one, a member-tier one, and an admin identity
// of the other kind. A member-scope delegation re-issues no admin-kind
// token. Nothing is minted for any of them.
func TestWorkloadAdminCredentials_TokenReissueRefusals(t *testing.T) {
	e := newX4bEnv(t, true, false)
	grantor := taUser(t, e.s, "x4b-c2r-grantor", store.SystemRoleSuperAdmin)
	d1 := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, map[string]any{"adminCap": 1})
	d2 := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, map[string]any{"adminCap": 1})
	ds := e.delegateScope(t, grantor, testIdentityDelegationSuperAdminScope, nil)
	md := e.delegateScope(t, grantor, testIdentityDelegationScope, map[string]any{"workloadSub": x2Sub})

	own := e.issueThrough(t, d1.ID, testAdminRoleHubAdmin).Identity.ID
	other := e.issueThrough(t, d2.ID, testAdminRoleHubAdmin).Identity.ID
	super := e.issueThrough(t, ds.ID, testAdminRoleSuperAdmin).Identity.ID
	session := taIssue(t, e.srv, taUser(t, e.s, "x4b-c2r-session", store.SystemRoleSuperAdmin)).Identity.ID
	rec := e.mint(t, md.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	member := tiDecode(t, rec).Identity.ID

	// The member path's 404 shape: an unknown id through the member
	// delegation.
	missing := e.workload(t, http.MethodPost, "/api/v1/test-identities/"+generateID()+"/token", e.memberWorkloadToken(t), md.ID, nil, nil)
	require.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())
	want := x4bErrorOf(t, missing)

	tokenAudits := func() int {
		recs, _, err := e.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testIdentityTokenIssueMutation})
		require.NoError(t, err)
		return len(recs)
	}
	audits, lines := tokenAudits(), len(tsaSinkLines(e.sink, testAdminAuditEventTokenIssue))

	for name, call := range map[string]func() *httptest.ResponseRecorder{
		"another delegation's identity": func() *httptest.ResponseRecorder { return e.reissue(t, d1.ID, other, nil) },
		"a human-issued identity":       func() *httptest.ResponseRecorder { return e.reissue(t, d1.ID, session, nil) },
		"a member-tier identity":        func() *httptest.ResponseRecorder { return e.reissue(t, d1.ID, member, nil) },
		"the other admin kind":          func() *httptest.ResponseRecorder { return e.reissue(t, d1.ID, super, nil) },
		"the hub-admin through the super-admin delegation": func() *httptest.ResponseRecorder {
			return e.reissue(t, ds.ID, own, nil)
		},
		"an unknown id": func() *httptest.ResponseRecorder { return e.reissue(t, d1.ID, generateID(), nil) },
		"a member-scope delegation on its grantor's admin identity": func() *httptest.ResponseRecorder {
			return e.workload(t, http.MethodPost, "/api/v1/test-identities/"+own+"/token", e.memberWorkloadToken(t), md.ID, nil, nil)
		},
	} {
		got := call()
		assert.Equal(t, http.StatusNotFound, got.Code, "%s: %s", name, got.Body.String())
		assert.Equal(t, want, x4bErrorOf(t, got), "%s: the member path's 404", name)
		assert.NotContains(t, x4JSONKeys(t, got.Body.Bytes()), "accessToken", name)
	}
	assert.Equal(t, audits, tokenAudits(), "no token audit record")
	assert.Len(t, tsaSinkLines(e.sink, testAdminAuditEventTokenIssue), lines, "no token sink line")

	// Positive control: the same delegation re-issues its own identity.
	rec = e.reissue(t, d1.ID, own, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A human caller still gets no admin-kind token: the grantor's session
	// on its own delegation-issued identity answers 409, and a token
	// scoped to exactly the admin capability (now admitted by the route
	// guard) gets the same 409.
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodPost, "/api/v1/test-identities/"+own+"/token", nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, testAdminReasonAdminKindTokens, taReason(t, rec))
	pat, _ := taPAT(t, e.srv, grantor.ID, uatScopeTestIdentityIssueAdmin)
	rec = doRequestWithToken(t, e.srv, pat, http.MethodPost, "/api/v1/test-identities/"+own+"/token", nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	// That token reaches no member-tier identity either (it lacks
	// test_identity.issue).
	rec = doRequestWithToken(t, e.srv, pat, http.MethodPost, "/api/v1/test-identities/"+member+"/token", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// A grantor demoted before the request gets 403 with no token minted;
// the outer checks refuse it before the handler's transaction.
func TestWorkloadAdminCredentials_TokenReissueDemotedGrantor(t *testing.T) {
	for _, tc := range x4bKinds {
		t.Run(tc.role, func(t *testing.T) {
			e := newX4bEnv(t, true, true)
			grantor := taUser(t, e.s, "x4b-demoted-"+tc.role, store.SystemRoleSuperAdmin)
			d := e.delegateScope(t, grantor, tc.scope, nil)
			id := e.issueThrough(t, d.ID, tc.role).Identity.ID
			taDeleteSystemRole(t, e.s, grantor.ID, store.SystemRoleSuperAdmin)

			rec := e.reissue(t, d.ID, id, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.NotContains(t, x4JSONKeys(t, rec.Body.Bytes()), "accessToken")
			rec = e.webCode(t, d.ID, id)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.NotContains(t, x4JSONKeys(t, rec.Body.Bytes()), "code")
			assert.Empty(t, e.audits(t, testIdentityTokenIssueMutation, id), "no token minted")
			assert.Empty(t, e.audits(t, testIdentityWebCodeIssueMutation, id), "no code minted")
			assert.Empty(t, e.sinkLinesFor(testAdminAuditEventTokenIssue, id))
			assert.Empty(t, e.sinkLinesFor(testAdminAuditEventWebCodeIssue, id))
		})
	}
}

// --- C2/C3: the in-transaction re-checks -----------------------------------------

// Both admin credential mints re-check, inside their transaction under the
// issuance lock, the delegation (C2, C3), the grantor (C1) and the
// identity row. A change made inside the transaction, after every check
// outside it passed, mints nothing: a demoted grantor answers 403
// issuer_not_live_super_admin, a revoked delegation 403, and a suspended
// identity 404. The unarmed hook mints, so each refusal comes from the
// in-transaction check.
func TestWorkloadAdminCredentials_InTransactionRechecks(t *testing.T) {
	changes := []struct {
		name   string
		status int
		reason string
		change func(t *testing.T, grantor *store.User, delegationID, identityID string) func(ctx context.Context, tx store.Store) error
	}{
		{
			name: "grantor demoted", status: http.StatusForbidden, reason: testAdminReasonNotLiveAdmin,
			change: func(t *testing.T, grantor *store.User, _, _ string) func(ctx context.Context, tx store.Store) error {
				return func(ctx context.Context, tx store.Store) error {
					rd, err := tx.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
					if err != nil {
						return err
					}
					bindings, err := tx.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, grantor.ID)
					if err != nil {
						return err
					}
					n := 0
					for _, b := range bindings {
						if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
							if err := tx.DeleteRoleBinding(ctx, b.ID); err != nil {
								return err
							}
							n++
						}
					}
					assert.Positive(t, n, "the grantor's super-admin binding was removed")
					return nil
				}
			},
		},
		{
			name: "delegation revoked", status: http.StatusForbidden,
			change: func(t *testing.T, grantor *store.User, delegationID, _ string) func(ctx context.Context, tx store.Store) error {
				return func(ctx context.Context, tx store.Store) error {
					changed, err := tx.RevokeTestIdentityDelegation(ctx, delegationID, grantor.ID, time.Now())
					assert.True(t, changed, "the delegation was revoked")
					return err
				}
			},
		},
		{
			name: "identity suspended", status: http.StatusNotFound,
			change: func(t *testing.T, _ *store.User, _, identityID string) func(ctx context.Context, tx store.Store) error {
				return func(ctx context.Context, tx store.Store) error {
					row, err := tx.GetUser(ctx, identityID)
					if err != nil {
						return err
					}
					row.Status = store.UserStatusSuspended
					return tx.UpdateUser(ctx, row)
				}
			},
		},
	}
	routes := []struct {
		name  string
		call  func(e *x4bEnv, t *testing.T, delegationID, id string) *httptest.ResponseRecorder
		ok    int
		audit string
		event string
		key   string
	}{
		{"token", func(e *x4bEnv, t *testing.T, d, id string) *httptest.ResponseRecorder {
			return e.reissue(t, d, id, nil)
		},
			http.StatusOK, testIdentityTokenIssueMutation, testAdminAuditEventTokenIssue, "accessToken"},
		{"web-code", func(e *x4bEnv, t *testing.T, d, id string) *httptest.ResponseRecorder { return e.webCode(t, d, id) },
			http.StatusCreated, testIdentityWebCodeIssueMutation, testAdminAuditEventWebCodeIssue, "code"},
	}
	for _, route := range routes {
		for _, tc := range changes {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				w := newWS4Env(t, false, true)
				hook := &x4LockHook{}
				installStoreFault(t, w.srv, func(inner store.Store, _ *storeFaultSwitch) *x4LockHookStore {
					return &x4LockHookStore{Store: inner, hook: hook}
				})
				e := &x4bEnv{x4Env: setupX4(t, w.srv, w.st, w.sink, false), web: w}
				grantor := taUser(t, e.s, "x4b-tx-grantor", store.SystemRoleSuperAdmin)
				d := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, nil)
				id := e.issueThrough(t, d.ID, testAdminRoleHubAdmin).Identity.ID

				// Positive control: the unarmed hook changes nothing.
				rec := route.call(e, t, d.ID, id)
				require.Equal(t, route.ok, rec.Code, rec.Body.String())
				audits, lines := len(e.audits(t, route.audit, id)), len(e.sinkLinesFor(route.event, id))

				hook.fn = tc.change(t, grantor, d.ID, id)
				hook.armed.Store(true)
				rec = route.call(e, t, d.ID, id)
				assert.False(t, hook.armed.Load(), "the hook ran inside the mint transaction")
				assert.Equal(t, tc.status, rec.Code, rec.Body.String())
				assert.NotContains(t, x4JSONKeys(t, rec.Body.Bytes()), route.key)
				if tc.reason != "" {
					assert.Equal(t, tc.reason, taReason(t, rec))
				}
				assert.Len(t, e.audits(t, route.audit, id), audits, "no audit record")
				assert.Len(t, e.sinkLinesFor(route.event, id), lines, "no sink line")
				assert.Equal(t, store.UserStatusActive, e.row(t, id).Status, "the change rolled back with the refusal")
			})
		}
	}
}

// --- C3: admin web-login code ---------------------------------------------------

// With the admin web session setting on, an admin-scope delegation mints a
// web-login code for an admin-kind identity it issued, at the matching
// level. The code redeems into a session capped at the identity's expiry
// and the kind's maximum lifetime; the audit record and the sink line
// carry the delegation id; the code never appears in either.
func TestWorkloadAdminCredentials_WebCodeOwnIdentity(t *testing.T) {
	for _, tc := range x4bKinds {
		t.Run(tc.role, func(t *testing.T) {
			e := newX4bEnv(t, true, true)
			grantor := taUser(t, e.s, "x4b-c3-"+tc.role, store.SystemRoleSuperAdmin)
			d := e.delegateScope(t, grantor, tc.scope, map[string]any{})
			id := e.issueThrough(t, d.ID, tc.role).Identity.ID
			row := e.row(t, id)

			rec := e.webCode(t, d.ID, id)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var resp TestIdentityWebCodeResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotEmpty(t, resp.Code)
			assert.False(t, resp.ExpiresAt.After(*row.ExpiresAt), "the code never outlives the identity")
			assert.False(t, resp.ExpiresAt.After(row.Created.Add(tc.spec.maxLifetime)))

			audits := e.audits(t, testIdentityWebCodeIssueMutation, id)
			require.Len(t, audits, 1)
			var summary struct {
				CodeID   string `json:"code_id"`
				Workload *struct {
					DelegationID string `json:"delegation_id"`
					Grantor      string `json:"grantor"`
					WorkloadSub  string `json:"workload_sub"`
				} `json:"workload_delegation"`
			}
			require.NoError(t, json.Unmarshal([]byte(audits[0].AfterSummary), &summary))
			assert.Equal(t, resp.CodeID, summary.CodeID)
			require.NotNil(t, summary.Workload)
			assert.Equal(t, d.ID, summary.Workload.DelegationID)
			assert.Equal(t, grantor.ID, summary.Workload.Grantor)
			assert.Equal(t, x4AdminSub, summary.Workload.WorkloadSub)
			assert.NotContains(t, audits[0].AfterSummary, resp.Code)

			lines := e.sinkLinesFor(testAdminAuditEventWebCodeIssue, id)
			require.Len(t, lines, 1)
			assert.Equal(t, d.ID, lines[0]["issuer_delegation_id"])
			assert.Equal(t, d.ID, lines[0]["issuer_credential_id"])
			assert.Equal(t, x4AdminSub, lines[0]["workload_sub"])
			assert.Equal(t, grantor.ID, lines[0]["issuer_id"])
			assert.Equal(t, resp.CodeID, lines[0]["code_id"])
			assert.NotContains(t, e.sink.String(), resp.Code)

			// The code redeems; the session is capped at the identity's
			// expiry and the kind's maximum lifetime.
			red := e.web.redeem(resp.Code)
			require.Equal(t, http.StatusNoContent, red.Code, red.Body.String())
			jar := tiWebApplyCookies(nil, red)
			tiExp, ok := e.web.sessionValues(jar)[sessKeyTIExpiry].(int64)
			require.True(t, ok, "the session carries its expiry")
			assert.LessOrEqual(t, tiExp, row.ExpiresAt.UnixMilli())
			assert.LessOrEqual(t, tiExp, row.Created.Add(tc.spec.maxLifetime).UnixMilli())
			sess := tiWebSetCookie(red, webSessionName)
			require.NotNil(t, sess)
			assert.LessOrEqual(t, sess.MaxAge, int(tc.spec.maxLifetime/time.Second))
			assert.Equal(t, http.StatusOK, e.web.apiMe(jar).Code, "the session works")
		})
	}
}

// The admin web-login code through a delegation is refused: while the
// admin web session setting is off (the same 404 WS4 gives the grantor's
// own session), for another delegation's identity, a human-issued one and
// the other kind, below the kind's level, and for a member-scope
// delegation. Each refusal mints no code. Turning the setting on admits
// the same request, so that refusal comes from the setting.
func TestWorkloadAdminCredentials_WebCodeRefusals(t *testing.T) {
	e := newX4bEnv(t, true, false)
	grantor := taUser(t, e.s, "x4b-c3r-grantor", store.SystemRoleSuperAdmin)
	d1 := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, map[string]any{"adminCap": 1})
	d2 := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, map[string]any{"adminCap": 1})
	ds := e.delegateScope(t, grantor, testIdentityDelegationSuperAdminScope, nil)
	md := e.delegateScope(t, grantor, testIdentityDelegationScope, map[string]any{"workloadSub": x2Sub})
	own := e.issueThrough(t, d1.ID, testAdminRoleHubAdmin).Identity.ID
	other := e.issueThrough(t, d2.ID, testAdminRoleHubAdmin).Identity.ID
	super := e.issueThrough(t, ds.ID, testAdminRoleSuperAdmin).Identity.ID
	session := taIssue(t, e.srv, taUser(t, e.s, "x4b-c3r-session", store.SystemRoleSuperAdmin)).Identity.ID

	codes := func() int {
		recs, _, err := e.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testIdentityWebCodeIssueMutation})
		require.NoError(t, err)
		return len(recs)
	}
	before := codes()

	// Setting off: the delegation gets the 404 the grantor's own session
	// gets from WS4.
	off := e.webCode(t, d1.ID, own)
	assert.Equal(t, http.StatusNotFound, off.Code, off.Body.String())
	ws4 := e.web.mintAsRec(grantor, own)
	require.Equal(t, http.StatusNotFound, ws4.Code, ws4.Body.String())
	assert.Equal(t, x4bErrorOf(t, ws4), x4bErrorOf(t, off), "the same refusal as WS4")
	assert.Equal(t, before, codes())

	e.srv.testAdminWeb = testAdminWebState{on: true}
	want := x4bErrorOf(t, e.webCode(t, d1.ID, generateID()))
	for name, call := range map[string]func() *httptest.ResponseRecorder{
		"another delegation's identity":                    func() *httptest.ResponseRecorder { return e.webCode(t, d1.ID, other) },
		"a human-issued identity":                          func() *httptest.ResponseRecorder { return e.webCode(t, d1.ID, session) },
		"the other admin kind":                             func() *httptest.ResponseRecorder { return e.webCode(t, d1.ID, super) },
		"the hub-admin through the super-admin delegation": func() *httptest.ResponseRecorder { return e.webCode(t, ds.ID, own) },
		"a member-scope delegation on its grantor's admin identity": func() *httptest.ResponseRecorder {
			return e.workload(t, http.MethodPost, ws4WebCodePath(own), e.memberWorkloadToken(t), md.ID, nil, nil)
		},
	} {
		got := call()
		assert.Equal(t, http.StatusNotFound, got.Code, "%s: %s", name, got.Body.String())
		assert.Equal(t, want, x4bErrorOf(t, got), "%s: the not-found answer", name)
		assert.NotContains(t, x4JSONKeys(t, got.Body.Bytes()), "code", name)
	}
	assert.Equal(t, before, codes(), "no code for a refused request")

	// Positive control with the setting on.
	rec := e.webCode(t, d1.ID, own)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.webCode(t, ds.ID, super)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	minted := codes()

	// Below the level: at --test-mode=admin the super-admin scope is not
	// available (403 from the auth arm) and the super-admin kind holds no
	// web session; the hub-admin kind still does.
	e.srv.testAdminGate = testAdminGate{hubAdmin: true, hubID: taHubID, hubClass: e.srv.testAdminGate.hubClass}
	rec = e.webCode(t, ds.ID, super)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = e.webCode(t, d1.ID, super)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, minted, codes(), "no code below the level")
	rec = e.webCode(t, d1.ID, own)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// --- C4: everything else stays refused --------------------------------------------

// An admin-scope delegation lists, deletes, purges and issues members
// nothing, for either admin scope and for its own identity; a member-scope
// delegation reaches no admin issuance. Nothing changes.
func TestWorkloadAdminCredentials_StillRefused(t *testing.T) {
	e := newX4bEnv(t, true, true)
	grantor := taUser(t, e.s, "x4b-c4-grantor", store.SystemRoleSuperAdmin)
	md := e.delegateScope(t, grantor, testIdentityDelegationScope, map[string]any{"workloadSub": x2Sub})
	rec := e.mint(t, md.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	member := tiDecode(t, rec).Identity.ID
	for _, tc := range x4bKinds {
		d := e.delegateScope(t, grantor, tc.scope, nil)
		own := e.issueThrough(t, d.ID, tc.role).Identity.ID
		fixtures, admins := e.liveFixtures(t), e.liveAdmins(t, "")
		for name, call := range map[string]func() *httptest.ResponseRecorder{
			"member issue": func() *httptest.ResponseRecorder {
				return e.workload(t, http.MethodPost, "/api/v1/test-identities", e.adminWorkloadToken(t), d.ID, map[string]string{"role": "member"}, nil)
			},
			"list": func() *httptest.ResponseRecorder {
				return e.workload(t, http.MethodGet, "/api/v1/test-identities", e.adminWorkloadToken(t), d.ID, nil, nil)
			},
			"delete own": func() *httptest.ResponseRecorder {
				return e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+own, e.adminWorkloadToken(t), d.ID, nil, nil)
			},
			"purge own": func() *httptest.ResponseRecorder {
				return e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+own+"?purge=true", e.adminWorkloadToken(t), d.ID, nil, nil)
			},
			"delete a member": func() *httptest.ResponseRecorder {
				return e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+member, e.adminWorkloadToken(t), d.ID, nil, nil)
			},
			// The issuer cleanup and provenance routes (Phase 3c) refuse an
			// admin-scope delegation too (testIdentityCaller).
			"cleanup": func() *httptest.ResponseRecorder {
				return e.workload(t, http.MethodPost, "/api/v1/test-identities/cleanup", e.adminWorkloadToken(t), d.ID, nil, nil)
			},
			"provenance": func() *httptest.ResponseRecorder {
				return e.workload(t, http.MethodGet, "/api/v1/test-identities/provenance", e.adminWorkloadToken(t), d.ID, nil, nil)
			},
		} {
			got := call()
			assert.Equal(t, http.StatusForbidden, got.Code, "%s %s: %s", tc.role, name, got.Body.String())
			keys := x4JSONKeys(t, got.Body.Bytes())
			assert.NotContains(t, keys, "accessToken", name)
			assert.NotContains(t, keys, "items", name)
			assert.NotContains(t, keys, "report", name)
		}
		assert.Equal(t, fixtures, e.liveFixtures(t), "%s: nothing issued", tc.role)
		assert.Equal(t, admins, e.liveAdmins(t, ""), "%s: nothing deleted", tc.role)
		_, err := e.s.GetUser(context.Background(), own)
		require.NoError(t, err, "%s: its identity is still there", tc.role)

		// A member-scope delegation reaches no admin issuance.
		got := e.workload(t, http.MethodPost, taPath, e.memberWorkloadToken(t), md.ID, taBody(tc.role), nil)
		assert.Equal(t, http.StatusForbidden, got.Code, got.Body.String())
		assert.Equal(t, admins, e.liveAdmins(t, ""))
	}
	_, err := e.s.GetUser(context.Background(), member)
	require.NoError(t, err, "the member identity is still there")
}

// testIdentityCallerAdmitting on its own: an admin-scope delegation is
// refused unless the handler opts in and lists the delegation's own
// permission, and then only that permission is decided; a member-scope
// delegation is admitted as before, whatever the flag.
func TestWorkloadAdminCredentials_CallerAdmission(t *testing.T) {
	e := newX4bEnv(t, true, false)
	ctx := context.Background()
	grantor := taUser(t, e.s, "x4b-caller-grantor", store.SystemRoleSuperAdmin)
	caller := func(d TestIdentityDelegationView, sub string, admit bool, perms ...string) (bool, map[string]bool, int) {
		dl, err := e.s.GetTestIdentityDelegation(ctx, d.ID)
		require.NoError(t, err)
		base := NewAuthenticatedUser(grantor.ID, grantor.Email, grantor.DisplayName, grantor.Role, string(ClientTypeAPI))
		ceiling, ok := permissions.BuildCeilingFromSelectors([]string{dl.Scope})
		require.True(t, ok)
		ident := NewScopedUserIdentityWithBoundary(base, TokenBoundary{Kind: BoundaryKindHub}, []string{dl.Scope}, dl.ID, ceiling)
		cred := credentialContextForIdentity(ident)
		cred.Kind = CredentialKindWorkloadDelegation
		cred.Type = string(CredentialKindWorkloadDelegation)
		c := context.WithValue(context.Background(), userContextKey{}, ident)
		c = contextWithIdentity(c, ident)
		c = contextWithCredentialContext(c, cred)
		c = contextWithWorkloadDelegationState(c, &workloadDelegationState{delegation: dl, sub: sub})
		r := httptest.NewRequest(http.MethodPost, "/api/v1/test-identities/x/token", nil).WithContext(c)
		w := httptest.NewRecorder()
		_, allowed, admitted := e.srv.testIdentityCallerAdmitting(w, r, admit, perms...)
		return admitted, allowed, w.Code
	}
	all := []string{permissionTestIdentityIssue, permissionTestIdentityIssueAdmin, permissionTestIdentityIssueSuperAdmin}
	hd := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, nil)
	sd := e.delegateScope(t, grantor, testIdentityDelegationSuperAdminScope, nil)
	md := e.delegateScope(t, grantor, testIdentityDelegationScope, map[string]any{"workloadSub": x2Sub})

	ok, _, code := caller(hd, x4AdminSub, false, all...)
	assert.False(t, ok, "not opted in")
	assert.Equal(t, http.StatusForbidden, code)
	ok, _, code = caller(hd, x4AdminSub, true, permissionTestIdentityIssue)
	assert.False(t, ok, "its permission is not in the list")
	assert.Equal(t, http.StatusForbidden, code)
	ok, _, _ = caller(sd, x4AdminSub, true, permissionTestIdentityIssue, permissionTestIdentityIssueAdmin)
	assert.False(t, ok, "the super-admin scope's permission is not in the list")

	ok, allowed, _ := caller(hd, x4AdminSub, true, all...)
	require.True(t, ok)
	assert.Equal(t, map[string]bool{permissionTestIdentityIssueAdmin: true}, allowed, "only its own permission is decided")
	ok, allowed, _ = caller(sd, x4AdminSub, true, all...)
	require.True(t, ok)
	assert.Equal(t, map[string]bool{permissionTestIdentityIssueSuperAdmin: true}, allowed)

	for _, admit := range []bool{false, true} {
		ok, allowed, _ = caller(md, x2Sub, admit, all...)
		require.True(t, ok, "member scope, admit=%v", admit)
		assert.Equal(t, map[string]bool{permissionTestIdentityIssue: true}, allowed)
	}
}
