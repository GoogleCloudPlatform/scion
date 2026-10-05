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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The passthrough gate counts the embedded broker as owned by an unscoped
// local platform administrator. These tests drive POST /api/v1/agents through
// the real authentication middleware so each credential reaches the gate in
// its production form. The host service account is registered and the actAs
// checker allows it, so the broker-ownership check is the only check that can
// refuse a request; a refusal there carries the "broker ownership" message.

const ptEmbeddedHostSA = "broker-host@my-project.iam.gserviceaccount.com"

// ptEmbeddedFixture is a hub with an embedded broker that has no recorded
// owner, serving one project, with the actAs checker allowing the host SA.
type ptEmbeddedFixture struct {
	srv     *Server
	store   store.Store
	project *store.Project
	broker  *store.RuntimeBroker
}

// newPTEmbeddedFixture builds the fixture. projectOwner owns the project and
// may create agents in it. brokerCreatedBy is the broker's recorded owner;
// pass "" for the single-node registration shape.
func newPTEmbeddedFixture(t *testing.T, name string, projectOwner *store.User, brokerCreatedBy string) ptEmbeddedFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, projectOwner))
	ensureHubMembership(ctx, s, projectOwner.ID)

	project := &store.Project{
		ID:        tid("project-pt-adm-" + name),
		Name:      "PT Admin " + name,
		Slug:      "pt-adm-" + name,
		OwnerID:   projectOwner.ID,
		CreatedBy: projectOwner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	broker := &store.RuntimeBroker{
		ID:                         tid("broker-pt-adm-" + name),
		Name:                       "PT Admin Broker " + name,
		Slug:                       "pt-adm-broker-" + name,
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  brokerCreatedBy,
		AutoProvide:                true,
		GCPHostServiceAccountEmail: ptEmbeddedHostSA,
		GCPHostProjectID:           "my-project",
		Labels:                     map[string]string{"scion.io/broker-role": "embedded"},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	srv.SetDispatcher(&createAgentDispatcher{createPhase: string(state.PhaseRunning)})
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(ptEmbeddedHostSA))

	return ptEmbeddedFixture{srv: srv, store: s, project: project, broker: broker}
}

// createReq is a passthrough agent-create request against the fixture project.
func (f ptEmbeddedFixture) createReq(agentName string) CreateAgentRequest {
	return CreateAgentRequest{
		Name:        agentName,
		ProjectID:   f.project.ID,
		Task:        "test",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: "passthrough"},
	}
}

// TestPassthroughEmbeddedBroker_UnscopedAdminPasses pins that an unscoped
// local platform administrator passes the broker-ownership check for the
// embedded broker, over both an interactive session and the dev credential.
func TestPassthroughEmbeddedBroker_UnscopedAdminPasses(t *testing.T) {
	t.Run("session", func(t *testing.T) {
		admin := ptUser(tid("user-pt-adm-session"), "pt-adm-session@test.com", store.UserRoleAdmin)
		f := newPTEmbeddedFixture(t, "session", admin, "")

		rec := doRequestAsUser(t, f.srv, admin, http.MethodPost, "/api/v1/agents", f.createReq("pt-adm-session"))

		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	})

	t.Run("dev credential", func(t *testing.T) {
		owner := ptUser(tid("user-pt-adm-dev-owner"), "pt-adm-dev-owner@test.com", store.UserRoleMember)
		f := newPTEmbeddedFixture(t, "dev", owner, "")

		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", f.createReq("pt-adm-dev"))

		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	})
}

// TestPassthroughEmbeddedBroker_AdminArmRequiresUnscopedAdmin pins that a
// user access token held by an administrator does not count as owning the
// embedded broker, even with a hub boundary and the agent:create selector:
// it is refused at the broker-ownership check.
func TestPassthroughEmbeddedBroker_AdminArmRequiresUnscopedAdmin(t *testing.T) {
	adminID := tid("user-pt-adm-uat")
	owner := ptUser(tid("user-pt-adm-uat-owner"), "pt-adm-uat-owner@test.com", store.UserRoleMember)
	f := newPTEmbeddedFixture(t, "uat", owner, "")
	createTestUserWithRole(t, f.store, adminID, adminID+"@test.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)

	key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID:   adminID,
		Name:     "pt-adm-uat",
		Boundary: TokenBoundary{Kind: BoundaryKindHub},
		Scopes:   []string{"agent:create"},
	})
	require.NoError(t, err)

	// role=none satisfies the token's delegation ceiling, so the request
	// reaches the passthrough gate.
	req := f.createReq("pt-adm-uat")
	req.AgentRole = string(AgentRoleNone)
	rec := doRequestWithUAT(t, f.srv, key, http.MethodPost, "/api/v1/agents", req)

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "broker ownership")
}

// TestPassthroughEmbeddedBroker_NonAdminRequiresOwnership pins that a
// non-administrator session passes the broker-ownership check for the
// embedded broker only when the user is its recorded owner.
func TestPassthroughEmbeddedBroker_NonAdminRequiresOwnership(t *testing.T) {
	t.Run("not owner", func(t *testing.T) {
		member := ptUser(tid("user-pt-adm-member"), "pt-adm-member@test.com", store.UserRoleMember)
		f := newPTEmbeddedFixture(t, "member", member, "")

		rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/agents", f.createReq("pt-adm-member"))

		assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), "broker ownership")
	})

	t.Run("owner", func(t *testing.T) {
		member := ptUser(tid("user-pt-adm-brokerowner"), "pt-adm-brokerowner@test.com", store.UserRoleMember)
		f := newPTEmbeddedFixture(t, "brokerowner", member, member.ID)

		rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/agents", f.createReq("pt-adm-brokerowner"))

		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	})
}
