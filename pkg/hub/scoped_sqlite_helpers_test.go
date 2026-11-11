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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// bindableFederatedAdmin is a federated identity whose ID is a local UUID so a
// test can grant an ordinary policy and reach a direct bypass branch. Production
// federation IDs intentionally include the issuer and are not bindable locally.
type bindableFederatedAdmin struct {
	UserIdentity
	issuerURL string
}

func newBindableFederatedAdmin(id string) *bindableFederatedAdmin {
	return &bindableFederatedAdmin{
		UserIdentity: NewAuthenticatedUser(id, "admin@example.com", "Admin", "admin", "api"),
		issuerURL:    "https://issuer.example",
	}
}

// setupScopedAdminTest creates a test server with a hub-admin user and a regular
// member user. The hub-admin has Role=member but holds a system-scoped hub-admin
// role binding, giving them permissions defined in hubAdminPermissionIDs().
func setupScopedAdminTest(t *testing.T) (*Server, store.Store, *store.User, *store.User) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a hub-admin user (NOT super-admin — role is "member")
	hubAdmin := &store.User{
		ID:          tid("user-hub-admin-test"),
		Email:       "hubadmin@test.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, hubAdmin))
	ensureHubMembership(ctx, s, hubAdmin.ID)

	// Create system-scoped hub-admin role binding
	hubAdminRoleDef, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      hubAdmin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "system",
	})
	require.NoError(t, err)

	// Create a regular member user for comparison
	member := &store.User{
		ID:          tid("user-member-test"),
		Email:       "member@test.com",
		DisplayName: "Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)

	return srv, s, hubAdmin, member
}

func (f *bindableFederatedAdmin) Type() string      { return "federated_user" }
func (f *bindableFederatedAdmin) IssuerURL() string { return f.issuerURL }

// authzClassification opts this fake into principalContextForIdentity /
// credentialContextForIdentity classification as a federated user: those
// functions key on concrete type, and this fake is a distinct Go type from
// the production FederatedUserIdentity. This keeps these tests exercising
// IsUnscopedLocalPlatformAdmin's FederatedIdentity-specific denial rather
// than an unrelated "unrecognized identity" denial that would happen to
// carry the same HTTP status.
func (f *bindableFederatedAdmin) authzClassification() (PrincipalKind, CredentialKind) {
	return PrincipalKindFederatedUser, CredentialKindFederation
}
