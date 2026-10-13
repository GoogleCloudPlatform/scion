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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project visitor rules (ptone/scion#4603), store half.

// createVisitorRule adds a rule for (issuer, domain) on the env's project.
func (e *visitorEnv) createVisitorRule(t *testing.T, issuer, domain string) *store.ProjectVisitorRule {
	t.Helper()
	rule := &store.ProjectVisitorRule{ProjectID: e.projectID, Issuer: issuer, Domain: domain, CreatedBy: "owner-1"}
	require.NoError(t, e.cs.CreateProjectVisitorRule(context.Background(), rule))
	return rule
}

// ruleBinding is a visitor binding of userID on the env's project created
// by rule.
func (e *visitorEnv) ruleBinding(userID string, rule *store.ProjectVisitorRule) *store.RoleBinding {
	rb := e.visitorBinding(userID)
	id := rule.ID
	rb.SourceRuleID = &id
	return rb
}

func TestCreateProjectVisitorRule_NormalizesAndRefuses(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()

	rule := env.createVisitorRule(t, testFedIssuer+"/", " Corp.COM ")
	assert.NotEmpty(t, rule.ID)
	assert.Equal(t, testFedIssuer, rule.Issuer)
	assert.Equal(t, "corp.com", rule.Domain)
	assert.Equal(t, "owner-1", rule.CreatedBy)
	assert.False(t, rule.CreatedAt.IsZero())

	got, err := env.cs.GetProjectVisitorRule(ctx, rule.ID)
	require.NoError(t, err)
	assert.Equal(t, rule.Domain, got.Domain)

	// One rule per (project, issuer, domain), compared after normalization.
	dup := &store.ProjectVisitorRule{ProjectID: env.projectID, Issuer: testFedIssuer, Domain: "CORP.com"}
	assert.ErrorIs(t, env.cs.CreateProjectVisitorRule(ctx, dup), store.ErrAlreadyExists)

	expires := time.Now().Add(time.Hour)
	for name, r := range map[string]*store.ProjectVisitorRule{
		"wildcard":        {ProjectID: env.projectID, Issuer: testFedIssuer, Domain: "*.corp.com"},
		"email":           {ProjectID: env.projectID, Issuer: testFedIssuer, Domain: "alice@corp.com"},
		"leading dot":     {ProjectID: env.projectID, Issuer: testFedIssuer, Domain: ".corp.com"},
		"empty domain":    {ProjectID: env.projectID, Issuer: testFedIssuer},
		"no issuer":       {ProjectID: env.projectID, Domain: "corp.com"},
		"no project":      {Issuer: testFedIssuer, Domain: "corp.com"},
		"expiry at start": {ProjectID: env.projectID, Issuer: testFedIssuer, Domain: "other.com", NotBefore: &expires, ExpiresAt: &expires},
	} {
		assert.ErrorIs(t, env.cs.CreateProjectVisitorRule(ctx, r), store.ErrInvalidInput, name)
	}
}

func TestValidVisitorRuleDomain(t *testing.T) {
	for _, d := range []string{"corp.com", "a.b.corp.example", "localhost", "xn--bcher-kva.example", "a-b.example"} {
		assert.True(t, store.ValidVisitorRuleDomain(d), d)
	}
	for _, d := range []string{"", "*", "*.corp.com", "@corp.com", "a@corp.com", ".corp.com", "corp.com.", "-corp.com", "corp-.com", "co rp.com", "Corp.com"} {
		assert.False(t, store.ValidVisitorRuleDomain(d), d)
	}
}

// The active-rule lookup matches the issuer and the domain exactly: no
// subdomain, no parent domain, no other issuer; and only rules whose
// window contains the time.
func TestListActiveProjectVisitorRules_ExactMatchAndWindow(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	now := time.Now()
	match := env.createVisitorRule(t, testFedIssuer, "corp.com")
	env.createVisitorRule(t, testFedIssuer, "sub.corp.com")
	env.createVisitorRule(t, "https://hub-b.example.com", "corp.com")
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	require.NoError(t, env.cs.CreateProjectVisitorRule(ctx, &store.ProjectVisitorRule{ProjectID: uuid.NewString(), Issuer: testFedIssuer, Domain: "corp.com", ExpiresAt: &past}))
	require.NoError(t, env.cs.CreateProjectVisitorRule(ctx, &store.ProjectVisitorRule{ProjectID: uuid.NewString(), Issuer: testFedIssuer, Domain: "corp.com", NotBefore: &future}))
	otherProject := &store.ProjectVisitorRule{ProjectID: uuid.NewString(), Issuer: testFedIssuer, Domain: "corp.com", NotBefore: &past, ExpiresAt: &future}
	require.NoError(t, env.cs.CreateProjectVisitorRule(ctx, otherProject))

	got, err := env.cs.ListActiveProjectVisitorRules(ctx, testFedIssuer+"/", "CORP.com", now)
	require.NoError(t, err)
	ids := make([]string, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	assert.ElementsMatch(t, []string{match.ID, otherProject.ID}, ids)

	for _, domain := range []string{"com", "x.corp.com", "corp.co"} {
		none, err := env.cs.ListActiveProjectVisitorRules(ctx, testFedIssuer, domain, now)
		require.NoError(t, err)
		assert.Empty(t, none, domain)
	}

	listed, err := env.cs.ListProjectVisitorRules(ctx, env.projectID)
	require.NoError(t, err)
	assert.Len(t, listed, 3, "the project's rules, whatever their issuer or window")
}

// A rule-created binding records its rule; the rule must be of the
// binding's project and of the account's home issuer, and only the
// visitor path may set it.
func TestCreateVisitorRoleBinding_SourceRule(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	rule := env.createVisitorRule(t, testFedIssuer, "corp.com")
	alice := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)

	rb, err := env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(alice.ID, rule))
	require.NoError(t, err)
	require.NotNil(t, rb.SourceRuleID)
	assert.Equal(t, rule.ID, *rb.SourceRuleID)
	listed, err := env.cs.ListVisitorRoleBindings(ctx, env.projectID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.NotNil(t, listed[0].SourceRuleID)
	assert.Equal(t, rule.ID, *listed[0].SourceRuleID)

	bob := createFederatedUser(t, env.cs, "https://hub-b.example.com", "bob@corp.com", store.UserStatusActive)
	_, err = env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(bob.ID, rule))
	assert.ErrorIs(t, err, store.ErrVisitorBindingRefused, "the rule is of another issuer")

	carol := createFederatedUser(t, env.cs, testFedIssuer, "carol@corp.com", store.UserStatusActive)
	otherProject := env.ruleBinding(carol.ID, rule)
	otherProject.ScopeID = uuid.NewString()
	_, err = env.cs.CreateVisitorRoleBinding(ctx, otherProject)
	assert.ErrorIs(t, err, store.ErrVisitorBindingRefused, "the rule is of another project")

	missing := uuid.NewString()
	unknown := env.visitorBinding(carol.ID)
	unknown.SourceRuleID = &missing
	_, err = env.cs.CreateVisitorRoleBinding(ctx, unknown)
	assert.ErrorIs(t, err, store.ErrVisitorBindingRefused, "the rule does not exist")

	human := env.humanUser(t)
	general := &store.RoleBinding{
		RoleDefinitionID: env.member.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: human.ID,
		ScopeType: store.RoleScopeProject, ScopeID: env.projectID, CreatedBy: "test", SourceRuleID: &rule.ID,
	}
	_, err = env.cs.CreateRoleBinding(ctx, general)
	assert.ErrorIs(t, err, store.ErrInvalidInput, "the general path never records a source rule")
}

// Removing a rule deletes the bindings it created and its exclusions, and
// nothing else: grants by email, and other rules' bindings, stay.
func TestDeleteProjectVisitorRule_DeletesOnlyItsBindings(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	rule := env.createVisitorRule(t, testFedIssuer, "corp.com")
	other := env.createVisitorRule(t, testFedIssuer, "corp.example")
	alice := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)
	bob := createFederatedUser(t, env.cs, testFedIssuer, "bob@corp.com", store.UserStatusActive)
	carol := createFederatedUser(t, env.cs, testFedIssuer, "carol@corp.com", store.UserStatusActive)
	dan := createFederatedUser(t, env.cs, testFedIssuer, "dan@corp.example", store.UserStatusActive)

	a, err := env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(alice.ID, rule))
	require.NoError(t, err)
	b, err := env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(bob.ID, rule))
	require.NoError(t, err)
	byEmail, err := env.cs.CreateVisitorRoleBinding(ctx, env.visitorBinding(carol.ID))
	require.NoError(t, err)
	otherRule, err := env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(dan.ID, other))
	require.NoError(t, err)
	require.NoError(t, env.cs.CreateProjectVisitorExclusion(ctx, rule.ID, carol.ID, "owner-1"))
	require.NoError(t, env.cs.CreateProjectVisitorExclusion(ctx, other.ID, carol.ID, "owner-1"))

	deleted, err := env.cs.DeleteProjectVisitorRule(ctx, rule.ID)
	require.NoError(t, err)
	ids := make([]string, 0, len(deleted))
	for _, rb := range deleted {
		ids = append(ids, rb.ID)
	}
	assert.ElementsMatch(t, []string{a.ID, b.ID}, ids)

	remaining, err := env.cs.ListVisitorRoleBindings(ctx, env.projectID)
	require.NoError(t, err)
	ids = ids[:0]
	for _, rb := range remaining {
		ids = append(ids, rb.ID)
	}
	assert.ElementsMatch(t, []string{byEmail.ID, otherRule.ID}, ids)

	_, err = env.cs.GetProjectVisitorRule(ctx, rule.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	excluded, err := env.cs.IsProjectVisitorExcluded(ctx, rule.ID, carol.ID)
	require.NoError(t, err)
	assert.False(t, excluded, "the rule's exclusions are deleted with it")
	excluded, err = env.cs.IsProjectVisitorExcluded(ctx, other.ID, carol.ID)
	require.NoError(t, err)
	assert.True(t, excluded, "another rule's exclusions stay")

	_, err = env.cs.DeleteProjectVisitorRule(ctx, rule.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// A window change applies to the rule and to every binding it created, and
// to nothing else.
func TestUpdateProjectVisitorRuleWindow_AppliesToItsBindings(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	rule := env.createVisitorRule(t, testFedIssuer, "corp.com")
	alice := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)
	carol := createFederatedUser(t, env.cs, testFedIssuer, "carol@corp.com", store.UserStatusActive)
	a, err := env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(alice.ID, rule))
	require.NoError(t, err)
	byEmail, err := env.cs.CreateVisitorRoleBinding(ctx, env.visitorBinding(carol.ID))
	require.NoError(t, err)

	notBefore := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := notBefore.Add(48 * time.Hour)
	updated, err := env.cs.UpdateProjectVisitorRuleWindow(ctx, rule.ID, &notBefore, &expiresAt)
	require.NoError(t, err)
	require.NotNil(t, updated.ExpiresAt)
	assert.True(t, updated.ExpiresAt.Equal(expiresAt))

	got, err := env.cs.GetRoleBinding(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.NotBefore)
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, got.NotBefore.Equal(notBefore))
	assert.True(t, got.ExpiresAt.Equal(expiresAt))
	untouched, err := env.cs.GetRoleBinding(ctx, byEmail.ID)
	require.NoError(t, err)
	assert.Nil(t, untouched.ExpiresAt, "a grant by email is not changed by a rule")

	_, err = env.cs.UpdateProjectVisitorRuleWindow(ctx, rule.ID, nil, nil)
	require.NoError(t, err)
	got, err = env.cs.GetRoleBinding(ctx, a.ID)
	require.NoError(t, err)
	assert.Nil(t, got.NotBefore)
	assert.Nil(t, got.ExpiresAt)

	_, err = env.cs.UpdateProjectVisitorRuleWindow(ctx, uuid.NewString(), nil, nil)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestProjectVisitorExclusion_Idempotent(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	rule := env.createVisitorRule(t, testFedIssuer, "corp.com")
	alice := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)

	excluded, err := env.cs.IsProjectVisitorExcluded(ctx, rule.ID, alice.ID)
	require.NoError(t, err)
	assert.False(t, excluded)
	require.NoError(t, env.cs.CreateProjectVisitorExclusion(ctx, rule.ID, alice.ID, "owner-1"))
	require.NoError(t, env.cs.CreateProjectVisitorExclusion(ctx, rule.ID, alice.ID, "owner-2"), "a repeat is not an error")
	excluded, err = env.cs.IsProjectVisitorExcluded(ctx, rule.ID, alice.ID)
	require.NoError(t, err)
	assert.True(t, excluded)

	// Inside a transaction too (the revoke route records it in one).
	require.NoError(t, env.cs.WithTx(ctx, func(tx store.Store) error {
		return tx.CreateProjectVisitorExclusion(ctx, rule.ID, alice.ID, "owner-3")
	}))

	assert.ErrorIs(t, env.cs.CreateProjectVisitorExclusion(ctx, uuid.NewString(), alice.ID, "owner-1"), store.ErrNotFound)
	assert.ErrorIs(t, env.cs.CreateProjectVisitorExclusion(ctx, rule.ID, " ", "owner-1"), store.ErrInvalidInput)
}

// A rule binding takes the rule's window as stored when the binding is
// written, whatever window the caller passes, and the store refuses a
// rule that is not active then: a binding never outlives, or starts
// before, its rule.
func TestCreateVisitorRoleBinding_SourceRuleWindow(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Hour)
	end := now.Add(2 * time.Hour)
	rule := &store.ProjectVisitorRule{ProjectID: env.projectID, Issuer: testFedIssuer, Domain: "corp.com",
		NotBefore: &start, ExpiresAt: &end, CreatedBy: "owner-1"}
	require.NoError(t, env.cs.CreateProjectVisitorRule(ctx, rule))

	alice := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)
	stale := env.ruleBinding(alice.ID, rule)
	longer := now.Add(30 * 24 * time.Hour)
	stale.NotBefore = nil
	stale.ExpiresAt = &longer
	rb, err := env.cs.CreateVisitorRoleBinding(ctx, stale)
	require.NoError(t, err)
	require.NotNil(t, rb.NotBefore)
	require.NotNil(t, rb.ExpiresAt)
	assert.True(t, rb.NotBefore.Equal(start), "the binding starts with the rule")
	assert.True(t, rb.ExpiresAt.Equal(end), "the binding ends with the rule, not the caller's expiry")
	got, err := env.cs.GetRoleBinding(ctx, rb.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, got.ExpiresAt.Equal(end))

	// Inside a transaction too, after a window change in it.
	shorter := now.Add(time.Hour)
	bob := createFederatedUser(t, env.cs, testFedIssuer, "bob@corp.com", store.UserStatusActive)
	require.NoError(t, env.cs.WithTx(ctx, func(tx store.Store) error {
		if _, err := tx.UpdateProjectVisitorRuleWindow(ctx, rule.ID, nil, &shorter); err != nil {
			return err
		}
		b := env.ruleBinding(bob.ID, rule)
		b.ExpiresAt = &end
		rb, err := tx.CreateVisitorRoleBinding(ctx, b)
		if err != nil {
			return err
		}
		assert.Nil(t, rb.NotBefore)
		require.NotNil(t, rb.ExpiresAt)
		assert.True(t, rb.ExpiresAt.Equal(shorter), "the binding takes the window as changed in the transaction")
		return nil
	}))

	// A rule that has ended, or has not started, binds no one.
	carol := createFederatedUser(t, env.cs, testFedIssuer, "carol@corp.com", store.UserStatusActive)
	past := now.Add(-time.Minute)
	_, err = env.cs.UpdateProjectVisitorRuleWindow(ctx, rule.ID, nil, &past)
	require.NoError(t, err)
	_, err = env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(carol.ID, rule))
	assert.ErrorIs(t, err, store.ErrVisitorBindingRefused, "the rule has ended")
	future := now.Add(time.Hour)
	_, err = env.cs.UpdateProjectVisitorRuleWindow(ctx, rule.ID, &future, nil)
	require.NoError(t, err)
	_, err = env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(carol.ID, rule))
	assert.ErrorIs(t, err, store.ErrVisitorBindingRefused, "the rule has not started")
	_, err = env.cs.UpdateProjectVisitorRuleWindow(ctx, rule.ID, nil, nil)
	require.NoError(t, err)
	_, err = env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(carol.ID, rule))
	assert.NoError(t, err, "an unbounded rule binds")
}

// Deleting a project's visitor rules deletes the bindings they created
// and their exclusions, and nothing of another project or a grant by
// email.
func TestDeleteProjectVisitorRulesByProject(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	rule := env.createVisitorRule(t, testFedIssuer, "corp.com")
	second := env.createVisitorRule(t, testFedIssuer, "corp.example")
	otherProject := uuid.NewString()
	elsewhere := &store.ProjectVisitorRule{ProjectID: otherProject, Issuer: testFedIssuer, Domain: "corp.com", CreatedBy: "owner-1"}
	require.NoError(t, env.cs.CreateProjectVisitorRule(ctx, elsewhere))

	alice := createFederatedUser(t, env.cs, testFedIssuer, "alice@corp.com", store.UserStatusActive)
	dan := createFederatedUser(t, env.cs, testFedIssuer, "dan@corp.example", store.UserStatusActive)
	carol := createFederatedUser(t, env.cs, testFedIssuer, "carol@corp.com", store.UserStatusActive)
	_, err := env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(alice.ID, rule))
	require.NoError(t, err)
	_, err = env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(dan.ID, second))
	require.NoError(t, err)
	byEmail, err := env.cs.CreateVisitorRoleBinding(ctx, env.visitorBinding(carol.ID))
	require.NoError(t, err)
	there := env.ruleBinding(alice.ID, elsewhere)
	there.ScopeID = otherProject
	thereRB, err := env.cs.CreateVisitorRoleBinding(ctx, there)
	require.NoError(t, err)
	require.NoError(t, env.cs.CreateProjectVisitorExclusion(ctx, rule.ID, carol.ID, "owner-1"))
	require.NoError(t, env.cs.CreateProjectVisitorExclusion(ctx, elsewhere.ID, carol.ID, "owner-1"))

	n, err := env.cs.DeleteProjectVisitorRulesByProject(ctx, env.projectID)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	rules, err := env.cs.ListProjectVisitorRules(ctx, env.projectID)
	require.NoError(t, err)
	assert.Empty(t, rules)
	remaining, err := env.cs.ListVisitorRoleBindings(ctx, env.projectID)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "only the grant by email remains")
	assert.Equal(t, byEmail.ID, remaining[0].ID)
	excluded, err := env.cs.IsProjectVisitorExcluded(ctx, rule.ID, carol.ID)
	require.NoError(t, err)
	assert.False(t, excluded, "the rules' exclusions are deleted with them")

	_, err = env.cs.GetProjectVisitorRule(ctx, elsewhere.ID)
	assert.NoError(t, err, "another project's rule stays")
	_, err = env.cs.GetRoleBinding(ctx, thereRB.ID)
	assert.NoError(t, err, "another project's rule binding stays")
	excluded, err = env.cs.IsProjectVisitorExcluded(ctx, elsewhere.ID, carol.ID)
	require.NoError(t, err)
	assert.True(t, excluded, "another project's exclusion stays")

	n, err = env.cs.DeleteProjectVisitorRulesByProject(ctx, env.projectID)
	require.NoError(t, err)
	assert.Zero(t, n, "a project with no rules")
	require.NoError(t, env.cs.WithTx(ctx, func(tx store.Store) error {
		_, err := tx.DeleteProjectVisitorRulesByProject(ctx, otherProject)
		return err
	}), "inside a transaction too")
	_, err = env.cs.GetProjectVisitorRule(ctx, elsewhere.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// DeleteInactiveVisitorGrant deletes only a visitor grant by email whose
// window does not contain the given time.
func TestDeleteInactiveVisitorGrant(t *testing.T) {
	env := newVisitorEnv(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	grant := func(email string, notBefore, expiresAt *time.Time) *store.RoleBinding {
		u := createFederatedUser(t, env.cs, testFedIssuer, email, store.UserStatusActive)
		b := env.visitorBinding(u.ID)
		b.NotBefore = notBefore
		b.ExpiresAt = expiresAt
		rb, err := env.cs.CreateVisitorRoleBinding(ctx, b)
		require.NoError(t, err)
		return rb
	}
	expired := grant("expired@corp.com", nil, &past)
	notStarted := grant("later@corp.com", &future, nil)
	active := grant("active@corp.com", &past, &future)
	unbounded := grant("always@corp.com", nil, nil)
	rule := env.createVisitorRule(t, testFedIssuer, "corp.com")
	ruleUser := createFederatedUser(t, env.cs, testFedIssuer, "rule@corp.com", store.UserStatusActive)
	byRule, err := env.cs.CreateVisitorRoleBinding(ctx, env.ruleBinding(ruleUser.ID, rule))
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		id   string
		want bool
	}{
		"expired grant":     {expired.ID, true},
		"not started grant": {notStarted.ID, true},
		"active grant":      {active.ID, false},
		"unbounded grant":   {unbounded.ID, false},
		"rule binding":      {byRule.ID, false},
		"missing binding":   {uuid.NewString(), false},
		"not an id":         {"not-a-uuid", false},
	} {
		t.Run(name, func(t *testing.T) {
			removed, err := env.cs.DeleteInactiveVisitorGrant(ctx, tc.id, now)
			require.NoError(t, err)
			assert.Equal(t, tc.want, removed)
		})
	}
	remaining, err := env.cs.ListVisitorRoleBindings(ctx, env.projectID)
	require.NoError(t, err)
	ids := make([]string, 0, len(remaining))
	for _, rb := range remaining {
		ids = append(ids, rb.ID)
	}
	assert.ElementsMatch(t, []string{active.ID, unbounded.ID, byRule.ID}, ids)

	// A binding of a role other than the project-visitor role is left alone.
	human := env.humanUser(t)
	member, err := env.cs.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.member.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: human.ID,
		ScopeType: store.RoleScopeProject, ScopeID: env.projectID, CreatedBy: "test", ExpiresAt: &past,
	})
	require.NoError(t, err)
	removed, err := env.cs.DeleteInactiveVisitorGrant(ctx, member.ID, now)
	require.NoError(t, err)
	assert.False(t, removed)
	_, err = env.cs.GetRoleBinding(ctx, member.ID)
	assert.NoError(t, err)
}
