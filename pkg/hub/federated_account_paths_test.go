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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Federated accounts (ptone/scion#4603): every email-keyed path family and
// every general grant path leaves a federated account alone. Each test uses
// the account's synthetic address and its real (federated) address.

const fedPathsIssuer = "https://hub-a.example.com"

// newFedPathsAccount creates an active federated account for alice@corp.com.
func newFedPathsAccount(t *testing.T, st store.Store) *store.User {
	t.Helper()
	issuer, email := fedPathsIssuer, "alice@corp.com"
	u := &store.User{
		DisplayName: "Alice (visitor)", Kind: store.UserKindFederated, Status: store.UserStatusActive,
		HomeIssuer: &issuer, FederatedEmail: &email,
	}
	require.NoError(t, st.CreateFederatedUser(context.Background(), u))
	return u
}

// fedAddresses are the two addresses a caller could try for acct.
func fedAddresses(acct *store.User) []string {
	return []string{acct.Email, *acct.FederatedEmail}
}

// assertFedAccountUngranted asserts acct holds no role binding and no group membership.
func assertFedAccountUngranted(t *testing.T, st store.Store, acct *store.User) {
	t.Helper()
	ctx := context.Background()
	bindings, err := st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, acct.ID)
	require.NoError(t, err)
	assert.Empty(t, bindings, "a federated account holds no role binding")
	groups, err := st.GetEffectiveGroups(ctx, acct.ID)
	require.NoError(t, err)
	assert.Empty(t, groups, "a federated account is in no group")
}

// Sign-in: find-or-create by email never lands on the account. The real
// address provisions a separate local user; the synthetic one is refused.
func TestFederatedAccount_SignInNeverResolvesAccount(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	acct := newFedPathsAccount(t, st)

	u, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: acct.Email, DisplayName: "x"})
	assert.Error(t, err, "the synthetic address never signs in")
	assert.Nil(t, u)

	u, err = srv.provisionUser(ctx, &ExternalUserInfo{Email: *acct.FederatedEmail, DisplayName: "Alice"})
	require.NoError(t, err)
	assert.NotEqual(t, acct.ID, u.ID, "the real address provisions a separate local user")
	assert.True(t, u.IsHumanKind())

	got, err := st.GetUser(ctx, acct.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, got.Status, "the federated account is untouched")
	assertFedAccountUngranted(t, st, acct)
}

// A hub-issued user token for the account is refused by the JWT row block,
// so no session or user token ever authenticates it.
func TestFederatedAccount_HubUserTokenRefused(t *testing.T) {
	srv, st := testServer(t)
	acct := newFedPathsAccount(t, st)

	rec := doRequestAsUser(t, srv, acct, http.MethodGet, "/api/v1/projects", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}

// Project member add, by email and by ID.
func TestFederatedAccount_ProjectMemberAddRefused(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()
	acct := newFedPathsAccount(t, st)

	for _, addr := range fedAddresses(acct) {
		_, err := srv.resolveMemberPrincipal(ctx, store.RoleBindingPrincipalUser, addr)
		assert.ErrorIs(t, err, store.ErrNotFound, "resolveMemberPrincipal(%q)", addr)
	}

	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	for _, principal := range append(fedAddresses(acct), acct.ID) {
		rec := doRequestAsUser(t, srv, owner, http.MethodPost,
			"/api/v1/projects/"+project.ID+"/members",
			addProjectMemberRequest{RoleDefinitionID: memberRoleDef.ID, PrincipalType: "user", PrincipalID: principal})
		assert.GreaterOrEqual(t, rec.Code, 400, "principal %q: %s", principal, rec.Body.String())
		assert.Less(t, rec.Code, 500, "principal %q: %s", principal, rec.Body.String())
	}

	decision := srv.membershipService.refuseTestAdminMemberGrantee(ctx, store.RoleBindingPrincipalUser, acct.ID)
	require.NotNil(t, decision)
	assert.Equal(t, federatedGranteeReasonRefused, decision.Reason)
	assertFedAccountUngranted(t, st, acct)
}

// Role binding create, by email and by ID (the dev user is a super-admin).
func TestFederatedAccount_RoleBindingRefused(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	acct := newFedPathsAccount(t, st)
	project := &store.Project{ID: tid("fed-rb-project"), Name: "Fed RB", Slug: "fed-rb"}
	require.NoError(t, st.CreateProject(ctx, project))
	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	for _, principal := range append(fedAddresses(acct), acct.ID) {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
			RoleDefinitionID: memberRoleDef.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: principal,
			ScopeType: store.RoleScopeProject, ScopeID: project.ID,
		})
		assert.GreaterOrEqual(t, rec.Code, 400, "principal %q: %s", principal, rec.Body.String())
		assert.Less(t, rec.Code, 500, "principal %q: %s", principal, rec.Body.String())
	}

	d := srv.authzService.CanDelegate(ctx, NewDevUser(DevUserConfig{Username: "dev", Email: "dev@localhost"}), GrantDescriptor{
		Type: GrantTypeRoleBinding, ScopeType: store.RoleScopeProject, ScopeID: project.ID,
		GranteeType: store.RoleBindingPrincipalUser, GranteeID: acct.ID,
	})
	assert.False(t, d.Allowed)
	assert.Equal(t, federatedGranteeReasonRefused, d.Reason)
	assertFedAccountUngranted(t, st, acct)
}

// Group member add, by email and by ID; and the hub-members auto-join.
func TestFederatedAccount_GroupMemberAddRefused(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	acct := newFedPathsAccount(t, st)
	group := &store.Group{ID: uuid.NewString(), Name: "fed-group", Slug: "fed-group"}
	require.NoError(t, st.CreateGroup(ctx, group))

	for _, principal := range append(fedAddresses(acct), acct.ID) {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+group.ID+"/members", AddGroupMemberRequest{
			MemberType: store.GroupMemberTypeUser, MemberID: principal, Role: store.GroupMemberRoleMember,
		})
		assert.GreaterOrEqual(t, rec.Code, 400, "principal %q: %s", principal, rec.Body.String())
		assert.Less(t, rec.Code, 500, "principal %q: %s", principal, rec.Body.String())
	}

	assert.ErrorIs(t, syncHubRoleGrants(ctx, st, acct.ID, store.UserRoleMember, store.SystemReconcileCreatedBy),
		store.ErrFederatedAccountRefused, "never auto-joined to hub-members")
	assertFedAccountUngranted(t, st, acct)
}

// Message to a user by email.
func TestFederatedAccount_MessageByEmailNeverResolvesAccount(t *testing.T) {
	srv, st := testServer(t)
	acct := newFedPathsAccount(t, st)
	agent := &store.Agent{ID: tid("fed-msg-agent"), ProjectID: tid("fed-msg-project")}

	for _, addr := range fedAddresses(acct) {
		rec := httptest.NewRecorder()
		_, err := srv.resolveOutboundRouting(context.Background(), rec, &OutboundMessageRequest{Recipient: "user:" + addr, Msg: "hi"}, agent)
		assert.ErrorIs(t, err, store.ErrNotFound, "recipient %q", addr)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "recipient %q", addr)
		assert.Contains(t, rec.Body.String(), ErrCodeAddrUnknown, "recipient %q", addr)
	}
}

// Broker inbound sender (and broker on-behalf-of): the email lookup never
// finds the account, and the shared refusal rejects its synthetic address
// and the row itself.
func TestFederatedAccount_BrokerInboundSenderNeverResolvesAccount(t *testing.T) {
	_, st := testServer(t)
	acct := newFedPathsAccount(t, st)

	assert.True(t, emailResolvedPrincipalRefused(acct.Email, nil), "synthetic address refused before lookup")
	assert.True(t, emailResolvedPrincipalRefused(*acct.FederatedEmail, acct), "the row itself is refused")
	assert.False(t, emailResolvedPrincipalRefused(*acct.FederatedEmail, nil), "the real address alone is an ordinary email")
	for _, addr := range fedAddresses(acct) {
		_, err := st.GetUserByEmail(context.Background(), addr)
		assert.ErrorIs(t, err, store.ErrNotFound, "GetUserByEmail(%q)", addr)
	}
}

// Invite: inviting the real address creates a separate invited user; the
// synthetic address is refused. The account is never modified.
func TestFederatedAccount_InviteNeverResolvesAccount(t *testing.T) {
	_, st := testServer(t)
	ctx := context.Background()
	acct := newFedPathsAccount(t, st)

	u, outcome, err := createPendingUserTx(ctx, st, PendingUserSpec{Email: *acct.FederatedEmail, InvitedBy: tid("fed-inviter")})
	require.NoError(t, err)
	assert.Equal(t, PendingCreated, outcome)
	assert.NotEqual(t, acct.ID, u.ID)
	assert.Equal(t, store.UserStatusInvited, u.Status)

	_, _, err = createPendingUserTx(ctx, st, PendingUserSpec{Email: acct.Email, InvitedBy: tid("fed-inviter")})
	assert.Error(t, err, "the synthetic address is never invited")

	got, err := st.GetUser(ctx, acct.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, got.Status)
	assert.Equal(t, acct.Email, got.Email)
}
