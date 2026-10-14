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

// Project visitor rules: visitor grants by email domain (ptone/scion#4603).
// The fixture's grantTestIssuer admits only *@corp.example emails;
// grantTestOtherIssuer admits any email.

func (f *visitorGrantFixture) rulesPath() string {
	return f.visitorsPath() + "/rules"
}

// addRule posts a visitor rule as actor and returns the response.
func (f *visitorGrantFixture) addRule(t *testing.T, actor *store.User, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, actor, http.MethodPost, f.rulesPath(), body)
}

// addRuleOK adds a rule for (issuer, domain) as the owner and returns it.
func (f *visitorGrantFixture) addRuleOK(t *testing.T, issuer, domain string) ProjectVisitorRuleView {
	t.Helper()
	rec := f.addRule(t, f.owner, map[string]interface{}{"issuer": issuer, "domain": domain})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var v ProjectVisitorRuleView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	return v
}

func (f *visitorGrantFixture) listRules(t *testing.T) []ProjectVisitorRuleView {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.rulesPath(), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListProjectVisitorRulesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Rules
}

// visitorThrough runs an issuer token through the auth middleware and returns the
// visitor identity, or nil when the request is not a visitor.
func (f *visitorGrantFixture) visitorThrough(t *testing.T, token string) *VisitorIdentity {
	t.Helper()
	identity, _, code := f.identityThroughMiddleware(t, token)
	require.Equal(t, http.StatusOK, code)
	v, _ := identity.(*VisitorIdentity)
	return v
}

// ruleBindings returns the project's visitor bindings created by ruleID.
func (f *visitorGrantFixture) ruleBindings(t *testing.T, projectID, ruleID string) []*store.RoleBinding {
	t.Helper()
	all, err := f.st.ListVisitorRoleBindings(context.Background(), projectID)
	require.NoError(t, err)
	var out []*store.RoleBinding
	for _, b := range all {
		if b.SourceRuleID != nil && *b.SourceRuleID == ruleID {
			out = append(out, b)
		}
	}
	return out
}

func TestProjectVisitorRules_AddListRemove(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()

	rule := f.addRuleOK(t, grantTestIssuer+"/", " Corp.Example ")
	assert.Equal(t, "corp.example", rule.Domain, "domain stored lower-cased")
	assert.Equal(t, grantTestIssuer, rule.Issuer)
	assert.Equal(t, f.owner.ID, rule.CreatedBy)
	assert.True(t, rule.IssuerAccepted)
	adds := f.audits(t, mutationTypeProjectVisitorRuleAdd)
	require.Len(t, adds, 1)
	assert.Equal(t, f.projectID, adds[0].TargetID)
	assert.Equal(t, f.owner.ID, adds[0].ActorPrincipalID)
	assert.Contains(t, adds[0].AfterSummary, `"rule_id":"`+rule.ID+`"`)
	assert.Contains(t, adds[0].AfterSummary, `"domain":"corp.example"`)

	listed := f.listRules(t)
	require.Len(t, listed, 1)
	assert.Equal(t, rule.ID, listed[0].ID)
	assert.Empty(t, f.list(t), "a rule is not a visitor until someone arrives")

	// The same (issuer, domain) again is a conflict.
	rec := f.addRule(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "domain": "CORP.example"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// A matching verified request makes its user a visitor through the rule.
	token := f.token(t, grantTestIssuer, "sub-ann", "ann@corp.example", boolPtr(true))
	v := f.visitorThrough(t, token)
	require.NotNil(t, v, "the matching user is a visitor")
	assert.Contains(t, f.visitorPermissions(t, token), "agent.message")
	visitors := f.list(t)
	require.Len(t, visitors, 1)
	assert.Equal(t, v.ID(), visitors[0].ID)
	assert.Equal(t, "ann@corp.example", visitors[0].Email)
	assert.Equal(t, projectVisitorSourceRule, visitors[0].Source)
	assert.Equal(t, rule.ID, visitors[0].SourceRuleID)
	assert.Equal(t, f.owner.ID, visitors[0].GrantedBy, "the rule's author granted it")

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.rulesPath()+"/"+rule.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, f.listRules(t))
	assert.Empty(t, f.list(t), "removing the rule removed its visitor")
	removes := f.audits(t, mutationTypeProjectVisitorRuleRemove)
	require.Len(t, removes, 1)
	assert.Contains(t, removes[0].BeforeSummary, `"rule_id":"`+rule.ID+`"`)
	assert.Contains(t, removes[0].BeforeSummary, `"removed_visitor_ids":["`+v.ID()+`"]`)
	assert.NotContains(t, f.visitorPermissions(t, token), "agent.message", "the next request is refused")
	_, err := f.st.GetProjectVisitorRule(ctx, rule.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.rulesPath()+"/"+rule.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// The method and path shapes of the rule routes.
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPut, f.rulesPath(), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.rulesPath()+"/"+rule.ID, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.rulesPath()+"/"+rule.ID+"/x", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

func TestProjectVisitorRules_AddRefusals(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()

	for name, tc := range map[string]struct {
		body  map[string]interface{}
		field string
	}{
		"issuer not trusted":       {map[string]interface{}{"issuer": "https://unknown.example.com", "domain": "corp.example"}, "issuer"},
		"issuer flag off":          {map[string]interface{}{"issuer": grantTestNoFlag, "domain": "corp.example"}, "issuer"},
		"domain not allowed":       {map[string]interface{}{"issuer": grantTestIssuer, "domain": "elsewhere.example"}, "domain"},
		"subdomain not allowed":    {map[string]interface{}{"issuer": grantTestIssuer, "domain": "sub.corp.example"}, "domain"},
		"missing issuer":           {map[string]interface{}{"domain": "corp.example"}, "issuer"},
		"missing domain":           {map[string]interface{}{"issuer": grantTestIssuer}, "domain"},
		"wildcard domain":          {map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "*.any.example"}, "domain"},
		"email instead of domain":  {map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "ann@any.example"}, "domain"},
		"at-sign prefixed domain":  {map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "@any.example"}, "domain"},
		"expiry in the past":       {map[string]interface{}{"issuer": grantTestIssuer, "domain": "corp.example", "expiresAt": time.Now().Add(-time.Hour)}, "expiresAt"},
		"expiry not a timestamp":   {map[string]interface{}{"issuer": grantTestIssuer, "domain": "corp.example", "expiresAt": "30d"}, ""},
		"expiry before its start":  {map[string]interface{}{"issuer": grantTestIssuer, "domain": "corp.example", "notBefore": time.Now().Add(48 * time.Hour), "expiresAt": time.Now().Add(24 * time.Hour)}, "expiresAt"},
		"leading dot in domain":    {map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": ".any.example"}, "domain"},
		"trailing dot in domain":   {map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "any.example."}, "domain"},
		"space inside the domain":  {map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "any example"}, "domain"},
		"domain with a url scheme": {map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "https://any.example"}, "domain"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.addRule(t, f.owner, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			if tc.field != "" {
				assert.Equal(t, tc.field, resp.Error.Details["field"])
			}
		})
	}
	rec := f.addRule(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "domain": "elsewhere.example"})
	assert.Contains(t, rec.Body.String(), visitorDomainNotAcceptedMessage)

	t.Run("member actor", func(t *testing.T) {
		rec := f.addRule(t, f.member, map[string]interface{}{"issuer": grantTestIssuer, "domain": "corp.example"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		rec = doRequestAsUser(t, f.srv, f.member, http.MethodGet, f.rulesPath(), nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("user access credential", func(t *testing.T) {
		key := mintScopedUAT(t, f.srv, f.owner.ID, f.projectID, []string{"project:read"})
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := doRequestWithUAT(t, f.srv, key, method, f.rulesPath(),
				map[string]interface{}{"issuer": grantTestIssuer, "domain": "corp.example"})
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			reason, credential := sessionOnlyDetailsOf(rec)
			assert.Equal(t, "GOV_PENDING", reason)
			assert.Equal(t, sessionRequiredCredential, credential)
		}
	})

	t.Run("experiment off", func(t *testing.T) {
		visitorSetExperiment(t, f.srv, false)
		defer visitorSetExperiment(t, f.srv, true)
		rec := f.addRule(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "domain": "corp.example"})
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.rulesPath(), nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	rules, err := f.st.ListProjectVisitorRules(ctx, f.projectID)
	require.NoError(t, err)
	assert.Empty(t, rules, "no refusal above created a rule")
	assert.Empty(t, f.audits(t, mutationTypeProjectVisitorRuleAdd))
}

func TestVisitorDomainAllowedByIssuer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		emails  []string
		domains []string
		domain  string
		want    bool
	}{
		{name: "no restriction", domain: "corp.example", want: true},
		{name: "domain listed", domains: []string{"Corp.Example"}, domain: "corp.example", want: true},
		{name: "domain not listed", domains: []string{"corp.example"}, domain: "other.example"},
		{name: "subdomain of a listed domain", domains: []string{"corp.example"}, domain: "sub.corp.example"},
		{name: "wildcard email pattern", emails: []string{"*@corp.example"}, domain: "corp.example", want: true},
		{name: "bare wildcard", emails: []string{"*"}, domain: "corp.example", want: true},
		{name: "exact address only", emails: []string{"ann@corp.example"}, domain: "corp.example"},
		{name: "wildcard of another domain", emails: []string{"*@other.example"}, domain: "corp.example"},
		{name: "pattern with a leading space, as the per-email check reads it", emails: []string{" *@corp.example"}, domain: "corp.example"},
		{name: "both set, both pass", emails: []string{"*@corp.example"}, domains: []string{"corp.example"}, domain: "corp.example", want: true},
		{name: "both set, domains fail", emails: []string{"*@corp.example"}, domains: []string{"other.example"}, domain: "corp.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := config.TrustedIssuerConfig{AllowedEmails: tc.emails, AllowedDomains: tc.domains}
			assert.Equal(t, tc.want, visitorDomainAllowedByIssuer(c, tc.domain))
		})
	}
}

// Non-amplification on rules, as on grants: an actor holding
// project.manage_visitors but not every project-visitor permission (here a
// hub admin) may list rules, shorten their windows and remove them, but
// may neither add a rule nor extend one.
func TestProjectVisitorRules_AddNeedsEveryVisitorPermission(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	expiry := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	rec := f.addRule(t, f.owner, map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "any.example", "expiresAt": expiry})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var rule ProjectVisitorRuleView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rule))

	hubAdminID := tid(t.Name() + "-hubadmin")
	createTestUserWithRole(t, f.st, hubAdminID, hubAdminID+"@test.com", "member", store.SystemRoleHubAdmin)
	ensureHubMembership(ctx, f.st, hubAdminID)
	hubAdmin, err := f.st.GetUser(ctx, hubAdminID)
	require.NoError(t, err)

	rec = doRequestAsUser(t, f.srv, hubAdmin, http.MethodGet, f.rulesPath(), nil)
	assert.Equal(t, http.StatusOK, rec.Code, "a hub admin may list rules: %s", rec.Body.String())

	rec = f.addRule(t, hubAdmin, map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "more.example"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), visitorGrantNonAmplificationMessage)

	for _, body := range []map[string]interface{}{
		{"expiresAt": expiry.Add(24 * time.Hour)},
		{},
	} {
		rec = doRequestAsUser(t, f.srv, hubAdmin, http.MethodPatch, f.rulesPath()+"/"+rule.ID, body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "extension %v: %s", body, rec.Body.String())
		assert.Contains(t, rec.Body.String(), visitorGrantNonAmplificationMessage)
	}
	listed := f.listRules(t)
	require.Len(t, listed, 1)
	require.NotNil(t, listed[0].ExpiresAt)
	assert.WithinDuration(t, expiry, *listed[0].ExpiresAt, time.Second, "the window is unchanged")
	assert.Empty(t, f.audits(t, mutationTypeProjectVisitorRuleUpdate))

	shorter := expiry.Add(-12 * time.Hour)
	rec = doRequestAsUser(t, f.srv, hubAdmin, http.MethodPatch, f.rulesPath()+"/"+rule.ID, map[string]interface{}{"expiresAt": shorter})
	require.Equal(t, http.StatusOK, rec.Code, "a hub admin may shorten: %s", rec.Body.String())
	assert.Len(t, f.audits(t, mutationTypeProjectVisitorRuleUpdate), 1)

	rec = doRequestAsUser(t, f.srv, hubAdmin, http.MethodDelete, f.rulesPath()+"/"+rule.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "a hub admin may remove: %s", rec.Body.String())

	// With every project-visitor permission on the project, it may add.
	f.bindCustomProjectRole(t, "rule-visitor-permissions", hubAdminID, projectVisitorPermissionIDs())
	rec = f.addRule(t, hubAdmin, map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "more.example"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// Materialization: the first verified request whose email's domain
// matches an active rule creates and links an account and binds it, in one
// transaction, audited with the rule's ID; repeats change nothing. The
// email_verified rule and the issuer's email filters apply as for a grant
// by email, and the domain matches exactly.
func TestFederatedVisitor_RuleMaterialization(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")

	token := f.token(t, grantTestOtherIssuer, "sub-bea", "Bea@ANY.example", boolPtr(true))
	v := f.visitorThrough(t, token)
	require.NotNil(t, v, "a case-insensitive domain match makes a visitor")
	acct, err := f.st.GetUser(ctx, v.ID())
	require.NoError(t, err)
	assert.Equal(t, store.UserKindFederated, acct.Kind)
	assert.Equal(t, store.UserStatusActive, acct.Status)
	require.NotNil(t, acct.FederatedEmail)
	assert.Equal(t, "bea@any.example", *acct.FederatedEmail)
	bySubject, err := f.st.GetFederatedUserBySubject(ctx, grantTestOtherIssuer, "sub-bea")
	require.NoError(t, err)
	assert.Equal(t, v.ID(), bySubject.ID)
	bindings := f.ruleBindings(t, f.projectID, rule.ID)
	require.Len(t, bindings, 1)
	assert.Equal(t, v.ID(), bindings[0].PrincipalID)

	linked := f.audits(t, mutationTypeVisitorLinked)
	require.Len(t, linked, 2, "the link and the rule binding")
	var sawRule, sawLink bool
	for _, rec := range linked {
		assert.Equal(t, v.ID(), rec.TargetID)
		var after map[string]string
		require.NoError(t, json.Unmarshal([]byte(rec.AfterSummary), &after))
		assert.Equal(t, "sub-bea", after["subject"])
		if after["rule_id"] == rule.ID {
			sawRule = true
			assert.Equal(t, f.projectID, after["project_id"])
			assert.Equal(t, bindings[0].ID, after["binding_id"])
		} else {
			sawLink = true
			assert.Equal(t, projectVisitorSourceRule, after["source"], "the account was created for a rule")
		}
	}
	assert.True(t, sawRule, "a visitor_linked record names the rule")
	assert.True(t, sawLink, "a visitor_linked record links the account")

	// Idempotent: repeats keep one account, one binding and the audits.
	for i := 0; i < 2; i++ {
		again := f.visitorThrough(t, token)
		require.NotNil(t, again)
		assert.Equal(t, v.ID(), again.ID())
	}
	assert.Len(t, f.ruleBindings(t, f.projectID, rule.ID), 1)
	assert.Len(t, f.audits(t, mutationTypeVisitorLinked), 2)

	// An issuer token with no email_verified claim matches too.
	assert.NotNil(t, f.visitorThrough(t, f.token(t, grantTestOtherIssuer, "sub-cy", "cy@any.example", nil)))

	for name, tok := range map[string]string{
		"email_verified false":    f.token(t, grantTestOtherIssuer, "sub-di", "di@any.example", boolPtr(false)),
		"no email claim":          f.token(t, grantTestOtherIssuer, "sub-ed", "", boolPtr(true)),
		"subdomain":               f.token(t, grantTestOtherIssuer, "sub-fi", "fi@sub.any.example", boolPtr(true)),
		"parent domain":           f.token(t, grantTestOtherIssuer, "sub-gu", "gu@example", boolPtr(true)),
		"domain as a prefix":      f.token(t, grantTestOtherIssuer, "sub-ha", "ha@any.example.org", boolPtr(true)),
		"domain as a suffix":      f.token(t, grantTestOtherIssuer, "sub-io", "io@many.example", boolPtr(true)),
		"issuer without the flag": f.token(t, grantTestNoFlag, "sub-ku", "ku@any.example", boolPtr(true)),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Nil(t, f.visitorThrough(t, tok), "no visitor")
		})
	}
	for _, sub := range []string{"sub-di", "sub-ed", "sub-fi", "sub-gu", "sub-ha", "sub-io", "sub-ku"} {
		for _, issuer := range []string{grantTestOtherIssuer, grantTestIssuer, grantTestNoFlag} {
			_, err := f.st.GetFederatedUserBySubject(ctx, issuer, sub)
			assert.ErrorIs(t, err, store.ErrNotFound, "%s at %s", sub, issuer)
		}
	}
	assert.Len(t, f.ruleBindings(t, f.projectID, rule.ID), 2, "only bea and cy were bound")
}

// A rule added after a visitor's first contact binds that visitor on their
// next request; a rule on another project binds there too.
func TestFederatedVisitor_RuleAddedLaterAppliesOnNextRequest(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	f.grantOK(t, grantTestOtherIssuer, "mo@any.example")
	token := f.token(t, grantTestOtherIssuer, "sub-mo", "mo@any.example", boolPtr(true))
	v := f.visitorThrough(t, token)
	require.NotNil(t, v)

	otherProject := tid(t.Name() + "-other")
	createRS1Project(t, f.st, otherProject, tid(t.Name()+"-other-owner"))
	rule := &store.ProjectVisitorRule{ProjectID: otherProject, Issuer: grantTestOtherIssuer, Domain: "any.example", CreatedBy: f.owner.ID}
	require.NoError(t, f.st.CreateProjectVisitorRule(ctx, rule))

	require.NotNil(t, f.visitorThrough(t, token))
	bindings := f.ruleBindings(t, otherProject, rule.ID)
	require.Len(t, bindings, 1)
	assert.Equal(t, v.ID(), bindings[0].PrincipalID)
	perms, err := f.srv.authzService.getEffectivePermissions(ctx, v.Type(), v.ID(), store.RoleScopeProject, otherProject)
	require.NoError(t, err)
	assert.Contains(t, perms, "agent.message")
}

// First contact of a pending account granted by email also applies the
// rules that match, in the same transaction; on the project where the
// email grant exists the rule adds nothing.
func TestFederatedVisitor_FirstContactAppliesRules(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	granted := f.grantOK(t, grantTestOtherIssuer, "nia@any.example")
	sameProject := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
	otherProject := tid(t.Name() + "-other")
	createRS1Project(t, f.st, otherProject, tid(t.Name()+"-other-owner"))
	rule := &store.ProjectVisitorRule{ProjectID: otherProject, Issuer: grantTestOtherIssuer, Domain: "any.example", CreatedBy: f.owner.ID}
	require.NoError(t, f.st.CreateProjectVisitorRule(ctx, rule))

	v := f.visitorThrough(t, f.token(t, grantTestOtherIssuer, "sub-nia", "nia@any.example", boolPtr(true)))
	require.NotNil(t, v)
	assert.Equal(t, granted.ID, v.ID(), "the granted account is linked, not a new one")
	assert.Empty(t, f.ruleBindings(t, f.projectID, sameProject.ID), "the email grant stands; the rule adds nothing")
	listed, ok := f.listedVisitor(t, granted.ID)
	require.True(t, ok)
	assert.Equal(t, projectVisitorSourceEmail, listed.Source)
	assert.Len(t, f.ruleBindings(t, otherProject, rule.ID), 1)
	assert.Len(t, f.audits(t, mutationTypeVisitorLinked), 2, "the link and the other project's rule binding")
}

// Revoking one rule visitor removes their binding and records an exclusion,
// so the rule does not bind them again; the rule's other visitors keep
// access. A rule visitor's window follows the rule's, so changing it on
// the visitor is a conflict.
func TestProjectVisitorRules_RevokeOneVisitorExcludes(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
	olaToken := f.token(t, grantTestOtherIssuer, "sub-ola", "ola@any.example", boolPtr(true))
	pimToken := f.token(t, grantTestOtherIssuer, "sub-pim", "pim@any.example", boolPtr(true))
	ola := f.visitorThrough(t, olaToken)
	pim := f.visitorThrough(t, pimToken)
	require.NotNil(t, ola)
	require.NotNil(t, pim)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, f.visitorsPath()+"/"+pim.ID(),
		map[string]interface{}{"expiresAt": time.Now().Add(time.Hour)})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+ola.ID(), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	revokes := f.audits(t, mutationTypeProjectVisitorRevoke)
	require.Len(t, revokes, 1)
	assert.Contains(t, revokes[0].BeforeSummary, `"source_rule_id":"`+rule.ID+`"`)
	excluded, err := f.st.IsProjectVisitorExcluded(ctx, rule.ID, ola.ID())
	require.NoError(t, err)
	assert.True(t, excluded)

	// Ola's next requests do not bring the binding back; Pim keeps access.
	for i := 0; i < 2; i++ {
		assert.NotContains(t, f.visitorPermissions(t, olaToken), "agent.message")
	}
	bindings := f.ruleBindings(t, f.projectID, rule.ID)
	require.Len(t, bindings, 1)
	assert.Equal(t, pim.ID(), bindings[0].PrincipalID)
	assert.Contains(t, f.visitorPermissions(t, pimToken), "agent.message")

	// An email grant still works for an excluded person.
	f.grantOK(t, grantTestOtherIssuer, "ola@any.example")
	assert.Contains(t, f.visitorPermissions(t, olaToken), "agent.message")
}

// Removing a rule deletes only its bindings: a grant by email to a person
// who also matches the rule, and another rule's visitors, keep access.
func TestProjectVisitorRules_RemoveDeletesOnlyItsBindings(t *testing.T) {
	f := newVisitorGrantFixture(t)
	rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
	other := f.addRuleOK(t, grantTestOtherIssuer, "other.example")
	f.grantOK(t, grantTestOtherIssuer, "qi@any.example")
	qiToken := f.token(t, grantTestOtherIssuer, "sub-qi", "qi@any.example", boolPtr(true))
	raToken := f.token(t, grantTestOtherIssuer, "sub-ra", "ra@any.example", boolPtr(true))
	suToken := f.token(t, grantTestOtherIssuer, "sub-su", "su@other.example", boolPtr(true))
	for _, tok := range []string{qiToken, raToken, suToken} {
		require.NotNil(t, f.visitorThrough(t, tok))
	}
	require.Len(t, f.list(t), 3)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.rulesPath()+"/"+rule.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Contains(t, f.visitorPermissions(t, qiToken), "agent.message", "the email grant stays")
	assert.NotContains(t, f.visitorPermissions(t, raToken), "agent.message", "the rule's visitor is removed")
	assert.Contains(t, f.visitorPermissions(t, suToken), "agent.message", "another rule's visitor stays")
	assert.Len(t, f.ruleBindings(t, f.projectID, other.ID), 1)
	visitors := f.list(t)
	require.Len(t, visitors, 2)
}

// A rule binds only inside its window, and its visitors' bindings follow
// window changes.
func TestProjectVisitorRules_WindowHonoured(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	rec := f.addRule(t, f.owner, map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "any.example", "notBefore": future})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var rule ProjectVisitorRuleView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rule))

	token := f.token(t, grantTestOtherIssuer, "sub-tia", "tia@any.example", boolPtr(true))
	assert.Nil(t, f.visitorThrough(t, token), "a rule that has not started binds no one")
	assert.Empty(t, f.ruleBindings(t, f.projectID, rule.ID))

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, f.rulesPath()+"/"+rule.ID, map[string]interface{}{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, f.visitorPermissions(t, token), "agent.message")

	expiry := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, f.rulesPath()+"/"+rule.ID, map[string]interface{}{"expiresAt": expiry})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	bindings := f.ruleBindings(t, f.projectID, rule.ID)
	require.Len(t, bindings, 1)
	require.NotNil(t, bindings[0].ExpiresAt)
	assert.WithinDuration(t, expiry, *bindings[0].ExpiresAt, time.Second, "the binding follows the rule's window")

	// Once the rule's expiry passes, its visitor's binding stops
	// authorizing with it, and the expired rule binds no one new.
	past := time.Now().Add(-time.Minute)
	_, err := f.st.UpdateProjectVisitorRuleWindow(ctx, rule.ID, nil, &past)
	require.NoError(t, err)
	assert.NotContains(t, f.visitorPermissions(t, token), "agent.message")
	assert.Nil(t, f.visitorThrough(t, f.token(t, grantTestOtherIssuer, "sub-uma", "uma@any.example", boolPtr(true))))

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, f.rulesPath()+"/"+rule.ID,
		map[string]interface{}{"expiresAt": time.Now().Add(-time.Minute)})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, f.rulesPath()+"/"+f.member.ID, map[string]interface{}{})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// The live gate: while the issuer's allow_visitor_grants is off, a rule
// binds no one and its linked visitors are refused; turning it back on
// restores them with no change to the rule.
func TestProjectVisitorRules_IssuerFlagOff(t *testing.T) {
	f := newVisitorGrantFixture(t)
	rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
	token := f.token(t, grantTestOtherIssuer, "sub-val", "val@any.example", boolPtr(true))
	require.NotNil(t, f.visitorThrough(t, token))

	f.setIssuerAcceptsVisitors(t, grantTestOtherIssuer, false)
	rec := f.middlewareResponse(t, token)
	assert.Equal(t, http.StatusForbidden, rec.Code, "a linked rule visitor is refused: %s", rec.Body.String())
	rec = f.middlewareResponse(t, f.token(t, grantTestOtherIssuer, "sub-wen", "wen@any.example", boolPtr(true)))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err := f.st.GetFederatedUserBySubject(context.Background(), grantTestOtherIssuer, "sub-wen")
	assert.ErrorIs(t, err, store.ErrNotFound, "no account is created while the flag is off")
	listed := f.listRules(t)
	require.Len(t, listed, 1)
	assert.False(t, listed[0].IssuerAccepted)
	rec = f.addRule(t, f.owner, map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "more.example"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "no new rule while the flag is off: %s", rec.Body.String())

	f.setIssuerAcceptsVisitors(t, grantTestOtherIssuer, true)
	assert.Contains(t, f.visitorPermissions(t, token), "agent.message")
	assert.Len(t, f.ruleBindings(t, f.projectID, rule.ID), 1)
}

// ruleAuditFaultStore fails the audit record of a rule add, update or
// removal, and of a rule binding on the request path, inside transactions
// too, once its switch is armed.
type ruleAuditFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

var errRuleAuditFault = errors.New("injected rule audit failure")

func (s *ruleAuditFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&ruleAuditFaultStore{Store: tx, fault: s.fault})
	})
}

func (s *ruleAuditFaultStore) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if s.fault.Active() {
		switch r.MutationType {
		case mutationTypeProjectVisitorRuleAdd, mutationTypeProjectVisitorRuleUpdate,
			mutationTypeProjectVisitorRuleRemove, mutationTypeVisitorLinked:
			return errRuleAuditFault
		}
	}
	return s.Store.CreateMutationAudit(ctx, r)
}

// Each rule change, and each rule binding on the request path, writes its
// audit record in the transaction of the change: when the audit write
// fails, nothing of the change remains. In particular a failed removal
// keeps the rule and every binding it created.
func TestProjectVisitorRules_AuditFailureLeavesNoChange(t *testing.T) {
	var fault *storeFaultSwitch
	f := newVisitorGrantFixture(t, func(srv *Server) {
		_, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *ruleAuditFaultStore {
			return &ruleAuditFaultStore{Store: inner, fault: sw}
		})
	})
	ctx := context.Background()
	rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
	for _, sub := range []string{"sub-xi", "sub-yu"} {
		require.NotNil(t, f.visitorThrough(t, f.token(t, grantTestOtherIssuer, sub, sub[4:]+"@any.example", boolPtr(true))))
	}
	require.Len(t, f.ruleBindings(t, f.projectID, rule.ID), 2)
	fault.Arm()

	t.Run("remove", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.rulesPath()+"/"+rule.ID, nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		_, err := f.st.GetProjectVisitorRule(ctx, rule.ID)
		assert.NoError(t, err, "the rule remains")
		assert.Len(t, f.ruleBindings(t, f.projectID, rule.ID), 2, "every binding remains")
	})

	t.Run("update", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, f.rulesPath()+"/"+rule.ID,
			map[string]interface{}{"expiresAt": time.Now().Add(time.Hour)})
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		for _, b := range f.ruleBindings(t, f.projectID, rule.ID) {
			assert.Nil(t, b.ExpiresAt, "no binding's window changed")
		}
		got, err := f.st.GetProjectVisitorRule(ctx, rule.ID)
		require.NoError(t, err)
		assert.Nil(t, got.ExpiresAt, "the rule's window is unchanged")
	})

	t.Run("add", func(t *testing.T) {
		rec := f.addRule(t, f.owner, map[string]interface{}{"issuer": grantTestOtherIssuer, "domain": "more.example"})
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		rules, err := f.st.ListProjectVisitorRules(ctx, f.projectID)
		require.NoError(t, err)
		assert.Len(t, rules, 1, "no rule was added")
	})

	t.Run("first contact through a rule", func(t *testing.T) {
		rec := f.middlewareResponse(t, f.token(t, grantTestOtherIssuer, "sub-zed", "zed@any.example", boolPtr(true)))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		_, err := f.st.GetFederatedUserByEmail(ctx, grantTestOtherIssuer, "zed@any.example")
		assert.ErrorIs(t, err, store.ErrNotFound, "no account remains")
		_, err = f.st.GetFederatedUserBySubject(ctx, grantTestOtherIssuer, "sub-zed")
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.Len(t, f.ruleBindings(t, f.projectID, rule.ID), 2, "no binding remains")
	})
}

// staleVisitorRulesStore answers ListActiveProjectVisitorRules with the
// rules it holds once they are set: the view of a request that read the
// rules just before an owner changed or removed one.
type staleVisitorRulesStore struct {
	store.Store
	rules atomic.Pointer[[]*store.ProjectVisitorRule]
}

func (s *staleVisitorRulesStore) ListActiveProjectVisitorRules(ctx context.Context, issuer, domain string, at time.Time) ([]*store.ProjectVisitorRule, error) {
	if rules := s.rules.Load(); rules != nil {
		return *rules, nil
	}
	return s.Store.ListActiveProjectVisitorRules(ctx, issuer, domain, at)
}

// newStaleRulesFixture is newVisitorGrantFixture with its server's store
// wrapped in a staleVisitorRulesStore.
func newStaleRulesFixture(t *testing.T) (*visitorGrantFixture, *staleVisitorRulesStore) {
	t.Helper()
	var wrapped *staleVisitorRulesStore
	f := newVisitorGrantFixture(t, func(srv *Server) {
		wrapped, _ = installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *staleVisitorRulesStore {
			return &staleVisitorRulesStore{Store: inner}
		})
	})
	return f, wrapped
}

// A request that read a rule before the owner changed it binds by the
// rule as it is when the binding is written: a shortened rule gives the
// binding its new window, and a removed rule binds no one and leaves no
// account or binding behind.
func TestFederatedVisitor_RuleChangedAfterReadBindsCurrentRule(t *testing.T) {
	t.Run("window shortened", func(t *testing.T) {
		f, stale := newStaleRulesFixture(t)
		ctx := context.Background()
		long := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
		rule := &store.ProjectVisitorRule{ProjectID: f.projectID, Issuer: grantTestOtherIssuer, Domain: "any.example", ExpiresAt: &long, CreatedBy: f.owner.ID}
		require.NoError(t, f.st.CreateProjectVisitorRule(ctx, rule))
		read, err := f.st.ListActiveProjectVisitorRules(ctx, grantTestOtherIssuer, "any.example", time.Now())
		require.NoError(t, err)
		require.Len(t, read, 1)
		short := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
		_, err = f.st.UpdateProjectVisitorRuleWindow(ctx, rule.ID, nil, &short)
		require.NoError(t, err)
		stale.rules.Store(&read)

		require.NotNil(t, f.visitorThrough(t, f.token(t, grantTestOtherIssuer, "sub-ari", "ari@any.example", boolPtr(true))))
		bindings := f.ruleBindings(t, f.projectID, rule.ID)
		require.Len(t, bindings, 1)
		require.NotNil(t, bindings[0].ExpiresAt)
		assert.True(t, bindings[0].ExpiresAt.Equal(short), "the binding ends with the rule as changed (%v), not as read (%v)", short, long)
	})

	t.Run("rule removed before first contact", func(t *testing.T) {
		f, stale := newStaleRulesFixture(t)
		ctx := context.Background()
		rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
		read, err := f.st.ListActiveProjectVisitorRules(ctx, grantTestOtherIssuer, "any.example", time.Now())
		require.NoError(t, err)
		require.Len(t, read, 1)
		_, err = f.st.DeleteProjectVisitorRule(ctx, rule.ID)
		require.NoError(t, err)
		stale.rules.Store(&read)

		assert.Nil(t, f.visitorThrough(t, f.token(t, grantTestOtherIssuer, "sub-bex", "bex@any.example", boolPtr(true))))
		_, err = f.st.GetFederatedUserBySubject(ctx, grantTestOtherIssuer, "sub-bex")
		assert.ErrorIs(t, err, store.ErrNotFound, "no account is linked")
		_, err = f.st.GetFederatedUserByEmail(ctx, grantTestOtherIssuer, "bex@any.example")
		assert.ErrorIs(t, err, store.ErrNotFound, "no account is created")
		all, err := f.st.ListVisitorRoleBindings(ctx, f.projectID)
		require.NoError(t, err)
		assert.Empty(t, all, "no binding names the removed rule")
		assert.Empty(t, f.audits(t, mutationTypeVisitorLinked))
	})

	t.Run("rule removed before a linked visitor's request", func(t *testing.T) {
		f, stale := newStaleRulesFixture(t)
		ctx := context.Background()
		f.grantOK(t, grantTestOtherIssuer, "cy@any.example")
		token := f.token(t, grantTestOtherIssuer, "sub-cy", "cy@any.example", boolPtr(true))
		require.NotNil(t, f.visitorThrough(t, token))
		otherProject := tid(t.Name() + "-other")
		createRS1Project(t, f.st, otherProject, tid(t.Name()+"-other-owner"))
		rule := &store.ProjectVisitorRule{ProjectID: otherProject, Issuer: grantTestOtherIssuer, Domain: "any.example", CreatedBy: f.owner.ID}
		require.NoError(t, f.st.CreateProjectVisitorRule(ctx, rule))
		read, err := f.st.ListActiveProjectVisitorRules(ctx, grantTestOtherIssuer, "any.example", time.Now())
		require.NoError(t, err)
		require.Len(t, read, 1)
		_, err = f.st.DeleteProjectVisitorRule(ctx, rule.ID)
		require.NoError(t, err)
		stale.rules.Store(&read)

		require.NotNil(t, f.visitorThrough(t, token), "the visitor keeps its grant by email")
		there, err := f.st.ListVisitorRoleBindings(ctx, otherProject)
		require.NoError(t, err)
		assert.Empty(t, there, "no binding names the removed rule")
	})
}

// ruleGoneExclusionStore answers CreateProjectVisitorExclusion with
// ErrNotFound, inside transactions too, once its switch is armed: the
// view of a revoke whose binding's rule no longer exists.
type ruleGoneExclusionStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *ruleGoneExclusionStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&ruleGoneExclusionStore{Store: tx, fault: s.fault})
	})
}

func (s *ruleGoneExclusionStore) CreateProjectVisitorExclusion(ctx context.Context, ruleID, userID, createdBy string) error {
	if s.fault.Active() {
		return store.ErrNotFound
	}
	return s.Store.CreateProjectVisitorExclusion(ctx, ruleID, userID, createdBy)
}

// Revoking a rule visitor whose rule no longer exists still removes the
// binding, with no exclusion, rather than answering 404.
func TestProjectVisitorRules_RevokeWhenRuleGone(t *testing.T) {
	var fault *storeFaultSwitch
	f := newVisitorGrantFixture(t, func(srv *Server) {
		_, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *ruleGoneExclusionStore {
			return &ruleGoneExclusionStore{Store: inner, fault: sw}
		})
	})
	ctx := context.Background()
	rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
	v := f.visitorThrough(t, f.token(t, grantTestOtherIssuer, "sub-dov", "dov@any.example", boolPtr(true)))
	require.NotNil(t, v)
	require.Len(t, f.ruleBindings(t, f.projectID, rule.ID), 1)
	fault.Arm()

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+v.ID(), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, f.ruleBindings(t, f.projectID, rule.ID), "the binding is removed")
	assert.Len(t, f.audits(t, mutationTypeProjectVisitorRevoke), 1)
	excluded, err := f.st.IsProjectVisitorExcluded(ctx, rule.ID, v.ID())
	require.NoError(t, err)
	assert.False(t, excluded)
}

// Deleting a project deletes its visitor rules, the bindings they created
// and their exclusions; a matching visitor's next request creates nothing
// at the deleted project's ID.
func TestProjectVisitorRules_ProjectDeletionRemovesRules(t *testing.T) {
	f := newVisitorGrantFixture(t)
	ctx := context.Background()
	rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
	token := f.token(t, grantTestOtherIssuer, "sub-eda", "eda@any.example", boolPtr(true))
	eda := f.visitorThrough(t, token)
	require.NotNil(t, eda)
	fay := f.visitorThrough(t, f.token(t, grantTestOtherIssuer, "sub-fay", "fay@any.example", boolPtr(true)))
	require.NotNil(t, fay)
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+fay.ID(), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	actor := NewAuthenticatedUser(f.owner.ID, f.owner.Email, "Owner", "member", "web")
	result, decision := f.srv.deletionService.Delete(setTestIdentity(ctx, actor), ProjectDeleteRequest{ProjectID: f.projectID, Actor: actor})
	require.Nil(t, decision, "the owner may delete the project")
	require.NotNil(t, result)
	assert.Equal(t, 1, result.CascadeSummary.ProjectVisitorRules)

	rules, err := f.st.ListProjectVisitorRules(ctx, f.projectID)
	require.NoError(t, err)
	assert.Empty(t, rules)
	_, err = f.st.GetProjectVisitorRule(ctx, rule.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	excluded, err := f.st.IsProjectVisitorExcluded(ctx, rule.ID, fay.ID())
	require.NoError(t, err)
	assert.False(t, excluded, "the rule's exclusions are deleted")

	for i := 0; i < 2; i++ {
		f.visitorThrough(t, token)
	}
	f.visitorThrough(t, f.token(t, grantTestOtherIssuer, "sub-gil", "gil@any.example", boolPtr(true)))
	left, err := f.st.ListVisitorRoleBindings(ctx, f.projectID)
	require.NoError(t, err)
	assert.Empty(t, left, "no binding at the deleted project's ID")
	_, err = f.st.GetFederatedUserBySubject(ctx, grantTestOtherIssuer, "sub-gil")
	assert.ErrorIs(t, err, store.ErrNotFound, "no account is created for the deleted project's rule")
}

// An inactive grant by email (expired, or not started yet) does not keep a
// person out of an active rule of the same project: the rule binding
// replaces it in one transaction, and the visitor_linked record names the
// replaced binding. An active grant by email is kept, and the rule adds
// nothing on that project.
func TestFederatedVisitor_RuleReplacesInactiveEmailGrant(t *testing.T) {
	ruleLinkSummary := func(t *testing.T, f *visitorGrantFixture, ruleID string) map[string]string {
		t.Helper()
		for _, rec := range f.audits(t, mutationTypeVisitorLinked) {
			var after map[string]string
			require.NoError(t, json.Unmarshal([]byte(rec.AfterSummary), &after))
			if after["rule_id"] == ruleID {
				return after
			}
		}
		return nil
	}

	t.Run("expired grant of a linked visitor", func(t *testing.T) {
		f := newVisitorGrantFixture(t)
		ctx := context.Background()
		granted := f.grantWithExpiry(t, grantTestOtherIssuer, "hal@any.example", time.Now().Add(time.Hour))
		token := f.token(t, grantTestOtherIssuer, "sub-hal", "hal@any.example", boolPtr(true))
		require.NotNil(t, f.visitorThrough(t, token))
		past := time.Now().Add(-time.Minute)
		_, err := f.st.UpdateVisitorRoleBindingWindow(ctx, granted.BindingID, nil, &past)
		require.NoError(t, err)
		require.NotContains(t, f.visitorPermissions(t, token), "agent.message", "the grant has expired")

		rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
		assert.Contains(t, f.visitorPermissions(t, token), "agent.message", "the rule gives access")
		bindings := f.ruleBindings(t, f.projectID, rule.ID)
		require.Len(t, bindings, 1)
		assert.Equal(t, granted.ID, bindings[0].PrincipalID)
		_, err = f.st.GetRoleBinding(ctx, granted.BindingID)
		assert.ErrorIs(t, err, store.ErrNotFound, "the expired grant is replaced")
		listed, ok := f.listedVisitor(t, granted.ID)
		require.True(t, ok)
		assert.Equal(t, projectVisitorSourceRule, listed.Source)
		after := ruleLinkSummary(t, f, rule.ID)
		require.NotNil(t, after, "a visitor_linked record names the rule")
		assert.Equal(t, granted.BindingID, after["replaced_binding_id"])
		assert.Equal(t, bindings[0].ID, after["binding_id"])
	})

	t.Run("grant not started at first contact", func(t *testing.T) {
		f := newVisitorGrantFixture(t)
		ctx := context.Background()
		rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestOtherIssuer, "email": "ida@any.example",
			"notBefore": time.Now().Add(24 * time.Hour)})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var granted ProjectVisitor
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &granted))
		rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")

		token := f.token(t, grantTestOtherIssuer, "sub-ida", "ida@any.example", boolPtr(true))
		v := f.visitorThrough(t, token)
		require.NotNil(t, v)
		assert.Equal(t, granted.ID, v.ID(), "the granted account is linked")
		assert.Contains(t, f.visitorPermissions(t, token), "agent.message", "the rule gives access now")
		require.Len(t, f.ruleBindings(t, f.projectID, rule.ID), 1)
		_, err := f.st.GetRoleBinding(ctx, granted.BindingID)
		assert.ErrorIs(t, err, store.ErrNotFound, "the grant that had not started is replaced")
		after := ruleLinkSummary(t, f, rule.ID)
		require.NotNil(t, after)
		assert.Equal(t, granted.BindingID, after["replaced_binding_id"])
	})

	t.Run("active grant is kept", func(t *testing.T) {
		f := newVisitorGrantFixture(t)
		ctx := context.Background()
		expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		granted := f.grantWithExpiry(t, grantTestOtherIssuer, "jo@any.example", expiry)
		rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
		token := f.token(t, grantTestOtherIssuer, "sub-jo", "jo@any.example", boolPtr(true))
		for i := 0; i < 2; i++ {
			require.NotNil(t, f.visitorThrough(t, token))
		}
		assert.Empty(t, f.ruleBindings(t, f.projectID, rule.ID), "the rule adds nothing")
		got, err := f.st.GetRoleBinding(ctx, granted.BindingID)
		require.NoError(t, err, "the active grant is kept")
		require.NotNil(t, got.ExpiresAt)
		assert.WithinDuration(t, expiry, *got.ExpiresAt, time.Second)
		assert.Nil(t, ruleLinkSummary(t, f, rule.ID))
	})
}

// A grant by email to a person who is a visitor through a rule answers 409
// with a message saying so; once the rule visitor is removed, the grant
// by email succeeds.
func TestProjectVisitors_GrantToRuleVisitorConflicts(t *testing.T) {
	f := newVisitorGrantFixture(t)
	rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
	v := f.visitorThrough(t, f.token(t, grantTestOtherIssuer, "sub-kai", "kai@any.example", boolPtr(true)))
	require.NotNil(t, v)

	rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestOtherIssuer, "email": "kai@any.example"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), visitorGrantedByRuleMessage)
	assert.Len(t, f.ruleBindings(t, f.projectID, rule.ID), 1, "the rule binding is unchanged")
	assert.Empty(t, f.audits(t, mutationTypeProjectVisitorGrant))

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+v.ID(), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	granted := f.grantOK(t, grantTestOtherIssuer, "kai@any.example")
	assert.Equal(t, v.ID(), granted.ID)
	assert.Equal(t, projectVisitorSourceEmail, granted.Source)

	rec = f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestOtherIssuer, "email": "kai@any.example"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), visitorGrantedByRuleMessage, "a repeat grant by email keeps its own message")
}

// staleRuleFirstContactStore, once armed, answers the first subject lookup
// and the first email lookup with not found: the view of a first request
// through a rule that loses the race to a concurrent first request of the
// same person, which created and linked the account first.
type staleRuleFirstContactStore struct {
	store.Store
	armed        atomic.Bool
	subjectCalls atomic.Int32
	emailCalls   atomic.Int32
}

func (s *staleRuleFirstContactStore) GetFederatedUserBySubject(ctx context.Context, issuer, subject string) (*store.User, error) {
	if s.armed.Load() && s.subjectCalls.Add(1) == 1 {
		return nil, store.ErrNotFound
	}
	return s.Store.GetFederatedUserBySubject(ctx, issuer, subject)
}

func (s *staleRuleFirstContactStore) GetFederatedUserByEmail(ctx context.Context, issuer, email string) (*store.User, error) {
	if s.armed.Load() && s.emailCalls.Add(1) == 1 {
		return nil, store.ErrNotFound
	}
	return s.Store.GetFederatedUserByEmail(ctx, issuer, email)
}

// Two first requests at once through a rule: the one that loses the race
// cannot create the account (ErrAlreadyExists), reads it again by subject
// and continues as that visitor; one account and one rule binding remain.
func TestFederatedVisitor_ConcurrentFirstContactThroughRule(t *testing.T) {
	var wrapped *staleRuleFirstContactStore
	f := newVisitorGrantFixture(t, func(srv *Server) {
		wrapped, _ = installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *staleRuleFirstContactStore {
			return &staleRuleFirstContactStore{Store: inner}
		})
	})
	ctx := context.Background()
	rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
	token := f.token(t, grantTestOtherIssuer, "sub-lia", "lia@any.example", boolPtr(true))

	// The winning request creates, links and binds first.
	winner := f.visitorThrough(t, token)
	require.NotNil(t, winner)
	require.Len(t, f.audits(t, mutationTypeVisitorLinked), 2)

	wrapped.armed.Store(true)
	loser := f.visitorThrough(t, token)
	require.NotNil(t, loser, "the losing request continues as the visitor")
	assert.Equal(t, winner.ID(), loser.ID())
	assert.Equal(t, int32(2), wrapped.subjectCalls.Load(), "the losing request read the account again by subject")
	assert.Equal(t, int32(1), wrapped.emailCalls.Load())

	acct, err := f.st.GetFederatedUserByEmail(ctx, grantTestOtherIssuer, "lia@any.example")
	require.NoError(t, err)
	assert.Equal(t, winner.ID(), acct.ID, "one account")
	identities, err := f.st.GetExternalIdentitiesByUserID(ctx, winner.ID())
	require.NoError(t, err)
	assert.Len(t, identities, 1, "one link")
	bindings := f.ruleBindings(t, f.projectID, rule.ID)
	require.Len(t, bindings, 1, "one rule binding")
	assert.Equal(t, winner.ID(), bindings[0].PrincipalID)
	assert.Len(t, f.audits(t, mutationTypeVisitorLinked), 2, "the losing request wrote no record")
}

// visitorRevokeOrderStore records, in order, the exclusions and binding
// deletes made through it, inside transactions too, and fails every
// binding delete while failDelete is set.
type visitorRevokeOrderStore struct {
	store.Store
	calls      *[]string
	failDelete *atomic.Bool
}

var errRevokeDeleteFault = errors.New("injected visitor binding delete failure")

func (s *visitorRevokeOrderStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&visitorRevokeOrderStore{Store: tx, calls: s.calls, failDelete: s.failDelete})
	})
}

func (s *visitorRevokeOrderStore) CreateProjectVisitorExclusion(ctx context.Context, ruleID, userID, createdBy string) error {
	*s.calls = append(*s.calls, "exclusion")
	return s.Store.CreateProjectVisitorExclusion(ctx, ruleID, userID, createdBy)
}

func (s *visitorRevokeOrderStore) DeleteRoleBinding(ctx context.Context, id string) error {
	*s.calls = append(*s.calls, "delete")
	if s.failDelete.Load() {
		return errRevokeDeleteFault
	}
	return s.Store.DeleteRoleBinding(ctx, id)
}

// Revoking a rule visitor records the exclusion before it deletes the
// binding, in one transaction: the revoke reaches the rule before the
// binding, as rule changes do. When the delete then fails, neither the
// exclusion nor any other change remains.
func TestProjectVisitorRules_RevokeExcludesBeforeDelete(t *testing.T) {
	var calls []string
	failDelete := &atomic.Bool{}
	f := newVisitorGrantFixture(t, func(srv *Server) {
		srv.membershipService.store = &visitorRevokeOrderStore{Store: srv.membershipService.store, calls: &calls, failDelete: failDelete}
	})
	ctx := context.Background()
	rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
	ola := f.visitorThrough(t, f.token(t, grantTestOtherIssuer, "sub-ola", "ola@any.example", boolPtr(true)))
	pim := f.visitorThrough(t, f.token(t, grantTestOtherIssuer, "sub-pim", "pim@any.example", boolPtr(true)))
	require.NotNil(t, ola)
	require.NotNil(t, pim)

	t.Run("exclusion first", func(t *testing.T) {
		calls = nil
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+ola.ID(), nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"exclusion", "delete"}, calls)
		excluded, err := f.st.IsProjectVisitorExcluded(ctx, rule.ID, ola.ID())
		require.NoError(t, err)
		assert.True(t, excluded)
	})

	t.Run("failed delete leaves no change", func(t *testing.T) {
		calls = nil
		failDelete.Store(true)
		t.Cleanup(func() { failDelete.Store(false) })
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+pim.ID(), nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"exclusion", "delete"}, calls)
		excluded, err := f.st.IsProjectVisitorExcluded(ctx, rule.ID, pim.ID())
		require.NoError(t, err)
		assert.False(t, excluded, "the exclusion is rolled back with the delete")
		bindings := f.ruleBindings(t, f.projectID, rule.ID)
		require.Len(t, bindings, 1, "the binding remains")
		assert.Equal(t, pim.ID(), bindings[0].PrincipalID)
		assert.Len(t, f.audits(t, mutationTypeProjectVisitorRevoke), 1, "only the first revoke is audited")
	})
}
