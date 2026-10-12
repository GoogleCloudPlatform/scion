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
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// runBootAdoption runs the boot adoption migration over the store's current
// rows, as on the first boot of a hub whose database holds unrecorded edges.
// Test servers run Migrate on an empty database first, which writes an empty
// snapshot and the marker; both are cleared so the snapshot covers the rows
// the test seeded.
func runBootAdoption(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, section := range []string{delegationadoption.MarkerSection, delegationadoption.CohortSection} {
		if err := s.DeleteHubSetting(ctx, section); err != nil && !errors.Is(err, store.ErrNotFound) {
			require.NoError(t, err)
		}
	}
	a, ok := s.(provenanceAdopter)
	require.True(t, ok, "store %T has no boot adoption", s)
	require.NoError(t, a.AdoptLegacyDelegationProvenance(ctx))
}

// adoptOnly writes the planned adoption of agentID's hop alone, skipping the
// top-down check, to build partially adopted chains.
func adoptOnly(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.WithTx(ctx, func(tx store.Store) error {
		plan, err := delegationadoption.Build(ctx, tx, delegationadoption.Scope{AgentIDs: []string{agentID}})
		if err != nil {
			return err
		}
		h := plan.Hop(agentID)
		require.NotNil(t, h)
		require.Equal(t, delegationadoption.OutcomeAdopt, h.Outcome, "reason %q", h.Reason)
		res, err := delegationadoption.ApplyPlannedAdopt(ctx, tx, h, uuid.NewString(), delegationadoption.Actor{})
		if err != nil {
			return err
		}
		require.Equal(t, store.DelegationAdoptionAdopted, res.Status, "reason %q", res.Reason)
		return nil
	}))
}

func compatIDs(t *testing.T, role AgentRole, sa bool) []string {
	t.Helper()
	ids, ok := permissions.CompatibilityCeiling(permissions.CompatibilityPolicyV1, string(role), sa)
	require.True(t, ok)
	return ids
}

func requireCreated(t *testing.T, code int, body string) {
	t.Helper()
	require.Contains(t, []int{http.StatusCreated, http.StatusOK, http.StatusAccepted}, code, body)
}

func setupDelegationCeilingTest(t *testing.T) (*AuthzService, store.Store) {
	t.Helper()
	return authzTestSetup(t)
}

// createDCUser creates a user and gives them a project-scoped role binding.
// The user is created with role="member" (NOT "admin") so that delegation
// ceiling tests exercise the actual ceiling logic rather than being
// short-circuited by the super-admin bypass in checkUserHoldsPermission.
func createDCUser(t *testing.T, s store.Store, userID, email, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()

	// Create user if not exists
	if _, err := s.GetUser(ctx, userID); err != nil {
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: userID, Email: email, DisplayName: email, Role: "member", Status: "active",
		}))
	}

	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err, "role definition %q not found", roleName)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create role binding: %v", err)
	}
}

// assertNotSystemAdmin verifies that the test user is NOT a system admin,
// ensuring delegation ceiling tests exercise the actual ceiling logic.
func assertNotSystemAdmin(t *testing.T, authz *AuthzService, ctx context.Context, userID string) {
	t.Helper()
	require.False(t, authz.IsSystemAdmin(ctx, userID),
		"test user %s must NOT be a system admin — ceiling tests would be vacuous", userID)
}

func createDCProject(t *testing.T, s store.Store, projectID, slug string) {
	t.Helper()
	ctx := context.Background()
	_ = s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Slug: slug,
		Name: slug,
	})
}

func createDCAgent(t *testing.T, s store.Store, agentID, projectID, ownerID string, role AgentRole) {
	t.Helper()
	ctx := context.Background()

	appliedConfig := &store.AgentAppliedConfig{
		AgentRole: string(role),
	}

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:            agentID,
		Slug:          "slug-" + agentID[:8],
		Name:          "name-" + agentID[:8],
		ProjectID:     projectID,
		Phase:         "running",
		CreatedBy:     ownerID,
		OwnerID:       ownerID,
		AppliedConfig: appliedConfig,
		Ancestry:      []string{ownerID},
	}))
}

func createDCEdge(t *testing.T, s store.Store, delegatorType, delegatorID, delegateType, delegateID, scopeType, scopeID, role string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  delegateType,
		DelegateID:    delegateID,
		ScopeType:     scopeType,
		ScopeID:       scopeID,
		Role:          role,
		Active:        true,
	}))
}

// seedRecordedDelegationEdge records an active edge carrying recorded
// authority: a principal effect ceiling with session provenance, version 1.
// This is the edge an interactive session create writes. createDCEdge, by
// contrast, writes an edge without provenance (it reads as unrecorded, the
// state of edges that predate provenance recording).
func seedRecordedDelegationEdge(t *testing.T, s store.Store, delegatorType, delegatorID, delegateType, delegateID, scopeType, scopeID, role string) {
	t.Helper()
	require.NoError(t, s.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  delegateType,
		DelegateID:    delegateID,
		ScopeType:     scopeType,
		ScopeID:       scopeID,
		Role:          role,
		Active:        true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    1,
			SourcePrincipalKind:  delegatorType,
			SourcePrincipalID:    delegatorID,
			SourceCredentialKind: store.SourceCredentialSession,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))
}

func dcAgentIdentity(agentID, projectID string, role AgentRole) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Scopes:    ScopesForRole(role),
	}}
}

// provenanceAdopter is the boot adoption entry point of the ent store.
type provenanceAdopter interface {
	AdoptLegacyDelegationProvenance(ctx context.Context) error
}
