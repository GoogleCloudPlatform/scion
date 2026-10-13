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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// legacyScopedIdentity builds a *ScopedUserIdentity the way an unbackfilled
// row is normalized: NewScopedUserIdentity derives the ceiling from scopes
// via the frozen legacy snapshot, exactly what
// store.UserAccessToken.NormalizedCeiling does for such a row. Tests that
// need the production load path instead (ValidateToken against a real
// stored row) use seedLegacyTokenInStore.
func legacyScopedIdentity(base UserIdentity, projectID string, scopes []string) *ScopedUserIdentity {
	return NewScopedUserIdentity(base, projectID, scopes)
}

// seedLegacyTokenInStore inserts a user_access_tokens row directly through
// the real store, the way a row exists before its ceiling has ever been
// computed: CeilingVersion/CeilingPermissionIDs are left at their zero
// values. It returns the plaintext key (for ValidateToken) and the row ID.
func seedLegacyTokenInStore(t *testing.T, s store.Store, userID, projectID string, scopes []string) (plaintext, tokenID string) {
	t.Helper()
	randomBytes := make([]byte, UATRandomBytes)
	_, err := rand.Read(randomBytes)
	require.NoError(t, err)
	keyBody := base64.RawURLEncoding.EncodeToString(randomBytes)
	fullKey := store.UATPrefix + keyBody
	prefix := store.UATPrefix + keyBody[:UATPrefixLength]
	hash := sha256.Sum256([]byte(fullKey))
	hashStr := hex.EncodeToString(hash[:])

	future := time.Now().Add(90 * 24 * time.Hour)
	token := &store.UserAccessToken{
		ID: uuid.New().String(), UserID: userID, Name: "legacy", Prefix: prefix, KeyHash: hashStr,
		ProjectID: projectID, Scopes: scopes, ExpiresAt: &future, Created: time.Now(),
	}
	require.NoError(t, s.CreateUserAccessToken(context.Background(), token))
	return fullKey, token.ID
}

// uatTestSetup creates a test server with seeded role definitions, a non-admin
// user, a project, and an authz service. The user has NO role bindings by
// default — callers add only what each test needs.
func uatTestSetup(t *testing.T) (authz *AuthzService, s store.Store, userID, projectID string) {
	t.Helper()
	_, s = testServer(t)
	ctx := context.Background()

	seedRoleDefinitions(ctx, s)

	projectID = tid("uat-project-1")
	project := &store.Project{
		ID:      projectID,
		Name:    "UAT Test Project",
		Slug:    "uat-test-project",
		OwnerID: tid("project-owner"),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	userID = tid("uat-test-user")
	user := &store.User{
		ID:          userID,
		Email:       "uat-user@test.com",
		DisplayName: "UAT User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))

	authz = NewAuthzService(s, nil)
	return authz, s, userID, projectID
}

// grantPermissionViaRoleBinding creates a custom role definition with a single
// permission and binds it to the user at the given scope. Returns the role
// binding ID for cleanup if needed.
func grantPermissionViaRoleBinding(t *testing.T, s store.Store, userID, permissionID, scopeType, scopeID string) string {
	t.Helper()
	ctx := context.Background()

	rdName := "test-role-" + permissionID + "-" + userID
	rd := &store.RoleDefinition{
		Name:        rdName,
		Description: "Test role granting " + permissionID,
		ScopeType:   scopeType,
		Permissions: []string{permissionID},
		System:      false,
	}
	created, err := s.CreateRoleDefinition(ctx, rd)
	require.NoError(t, err, "creating role definition for %s", permissionID)

	rb := &store.RoleBinding{
		RoleDefinitionID: created.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        scopeType,
		ScopeID:          scopeID,
		CreatedBy:        "test",
	}
	binding, err := s.CreateRoleBinding(ctx, rb)
	require.NoError(t, err, "creating role binding for %s", permissionID)
	return binding.ID
}

// decideAsUAT builds an AuthzRequest that mirrors how real handlers invoke
// authz: a ScopedUserIdentity (UAT) with the Permission field set so the
// role-binding evaluation path fires.
func decideAsUAT(ctx context.Context, authz *AuthzService, userID, projectID string, scopes []string, resource Resource, action Action, permissionID string) Decision {
	scoped := makeScopedIdentity(userID, projectID, scopes)
	return authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(scoped),
		Resource:   resource,
		Action:     action,
		Permission: permissionID,
	})
}

// uatpMember creates a project-member user, matching production shape: a
// project-scoped member role binding AND the seeded hub-members group
// (seed.go:462-480), which every logged-in user actually holds. Tests in
// this file deliberately do not use a minimal fixture that omits hub
// membership: the seeded hub-member system role interacts with the
// live-project-access gate (see
// TestProjectUAT_HubMembershipAloneDoesNotGrantProjectAccess and the
// seeded-role regressions below), and a fixture without that binding would
// hide the interaction instead of exposing it.
func uatpMember(t *testing.T, s store.Store, projectID, userID string) {
	t.Helper()
	createTestUserWithProjectRole(t, s, userID, userID+"@test.com", projectID, store.ProjectRoleMember)
	ensureHubMembership(context.Background(), s, userID)
}

// uatpAgent creates an agent directly via the store with Hub-recorded
// OwnerID/Ancestry, rather than through the HTTP handler.
func uatpAgent(t *testing.T, s store.Store, projectID, ownerID, idSuffix string, ancestry ...string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:        tid("uatp-agent-" + idSuffix),
		Slug:      "uatp-agent-" + idSuffix,
		Name:      "UATP Agent " + idSuffix,
		ProjectID: projectID,
		OwnerID:   ownerID,
		Phase:     string(state.PhaseStopped),
		Ancestry:  ancestry,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

// uatpExposePort registers an exposed port directly via the store, instead
// of the HTTP registration path (which scoped UATs cannot use --
// authorizePortRegistration always denies ScopedUserIdentity, by design).
func uatpExposePort(t *testing.T, s store.Store, agent *store.Agent, port int) {
	t.Helper()
	ports := append([]store.ExposedPort(nil), agent.ExposedPorts...)
	ports = append(ports, store.ExposedPort{
		Port: port, Label: "web", Host: "127.0.0.1", Mode: "rw",
		ExposedAt: time.Now().UTC(), ExposedBy: "agent",
	})
	require.NoError(t, s.UpdateAgentExposedPorts(context.Background(), agent.ID, ports))
	agent.ExposedPorts = ports
}

// requireAuthorizedPTY is assertAuthorizedPTY's require-semantics twin, for
// sanity preconditions a test cannot usefully continue past.
func requireAuthorizedPTY(t *testing.T, rec *httptest.ResponseRecorder, msgAndArgs ...interface{}) {
	t.Helper()
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, msgAndArgs...)
}

// uatpDeleteProjectBinding removes userID's direct project-scoped role
// binding(s) in projectID, modeling an admin removing the member.
func uatpDeleteProjectBinding(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
	}
}

func makeScopedIdentity(userID, projectID string, scopes []string) *ScopedUserIdentity {
	base := NewAuthenticatedUser(userID, "uat-user@test.com", "UAT User", store.UserRoleMember, "api")
	return NewScopedUserIdentity(base, projectID, scopes)
}
