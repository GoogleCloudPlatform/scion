// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The consumers below admit an agent whose edge carries recorded authority
// (see the tests that seed with seedRecordedDelegationEdge). Each test here
// seeds the same chain with an edge that carries no provenance -- the
// unrecorded state of an edge written before provenance recording -- and
// asserts the consumer denies with DenyCause ceiling_unrecorded.

func assertUnrecordedDeny(t *testing.T, d Decision) {
	t.Helper()
	require.False(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, "reason %q", d.Reason)
	assert.Equal(t, DenyCauseCeilingUnrecorded, d.DenyCause, "reason %q", d.Reason)
}

// gcp_service_account.assign: the SA-assign gate's Layer 1 CheckAccess.
func TestUnrecordedEdgeDeniesSAAssign(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID, userID := tid("unrec-assign-proj"), tid("unrec-assign-user")
	createDCProject(t, s, projectID, "unrec-assign-project")
	createDCUser(t, s, userID, "unrec-assign-user@example.com", projectID, store.ProjectRoleOwner)
	sa := scaCreateSA(t, s, projectID)

	recorded, unrecorded := tid("unrec-assign-recorded"), tid("unrec-assign-unrecorded")
	createDCAgent(t, s, recorded, projectID, userID, AgentRoleFull)
	createDCAgent(t, s, unrecorded, projectID, userID, AgentRoleFull)
	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, userID, store.DelegationPrincipalAgent, recorded,
		store.RoleScopeProject, projectID, string(AgentRoleFull))
	createDCEdge(t, s, store.DelegationPrincipalUser, userID, store.DelegationPrincipalAgent, unrecorded,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	check := func(agentID string) Decision {
		agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
		return srv.authzService.CheckAccess(contextWithIdentity(ctx, agent), agent, gcpServiceAccountResource(sa), ActionAssign)
	}
	control := check(recorded)
	require.True(t, control.Allowed, "recorded edge: reason %q", control.Reason)
	assertUnrecordedDeny(t, check(unrecorded))
}

// gcp_service_account.use: Decide for the exact SA token scope.
func TestUnrecordedEdgeDeniesGCPServiceAccountUse(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("unrec-use-sa")
	agentID, projectID := tid("unrec-use-agent"), tid("unrec-use-project")
	setBackfillCompleted(t, s)
	adminID := tid("unrec-use-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	createDCEdge(t, s, store.DelegationPrincipalUser, adminID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, store.ProjectRoleOwner)

	agent := newFullAgentIdentity(agentID, projectID, nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})
	assertUnrecordedDeny(t, authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	}))
}

// secret.use and project.secret_read: Decide, and the runtime project-secret
// fetch, which reports the item as not found.
func TestUnrecordedEdgeDeniesSecretUseAndProjectSecretRead(t *testing.T) {
	f := newMaterialFixture(t, "unrec-secret")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "UNRECORDED_KEY", "v", "", "", f.ProjectID)

	delegator := tid("unrec-secret-owner")
	// A super-admin delegator holds both permissions (allPermissionIDs), as in
	// the recorded-edge consumer tests, so the deny comes from the hop alone.
	createTestUserWithRole(t, f.Store, delegator, "unrec-secret-owner@test.com", "member", store.SystemRoleSuperAdmin)
	agentID := tid("unrec-secret-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "unrec-secret-agent", Name: "unrec", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{delegator},
		Created: time.Now(), Updated: time.Now(),
	}))
	createDCEdge(t, f.Store, store.DelegationPrincipalUser, delegator, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	ident := newFullAgentIdentity(agentID, f.ProjectID, []string{delegator}, []AgentTokenScope{ScopeProjectSecretRead})
	// Resource and action as the existing consumer tests decide them.
	for _, row := range []struct {
		perm   string
		action Action
	}{
		{"secret.use", ActionUse},
		{permissionProjectSecretRead, Action("secret_read")},
	} {
		t.Run(row.perm, func(t *testing.T) {
			assertUnrecordedDeny(t, f.Server.authzService.Decide(ctx, AuthzRequest{
				Principal:  principalContextForIdentity(ident),
				Credential: credentialContextForIdentity(ident),
				Resource:   Resource{Type: "project", ID: f.ProjectID},
				Action:     row.action,
				Permission: row.perm,
			}))
		})
	}

	token, err := f.Server.agentTokenService.GenerateAgentToken(agentID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{delegator})
	require.NoError(t, err)
	assertProjectDenied(t, f, agentID, token, "UNRECORDED_KEY")
}
