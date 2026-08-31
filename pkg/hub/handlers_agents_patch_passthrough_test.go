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

// Regression tests for PATCH passthrough parity: the PATCH /api/v1/agents/{id}
// path must enforce the same broker-owner/admin restriction for passthrough
// mode as the create path. Without this, "create without passthrough, then
// PATCH it in" bypasses the create-path gate.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// patchPassthroughFixture sets up a world with a broker owned by one user
// and an agent owned by a different user in a project that uses that broker.
type patchPassthroughFixture struct {
	srv         *Server
	store       store.Store
	brokerOwner *store.User
	nonOwner    *store.User
	adminUser   *store.User
	project     *store.Project
	broker      *store.RuntimeBroker
	agent       *store.Agent
}

func setupPatchPassthroughFixture(t *testing.T) *patchPassthroughFixture {
	t.Helper()
	srv, s := bypassAgentsServer(t)
	ctx := context.Background()
	f := &patchPassthroughFixture{srv: srv, store: s}

	// Create the broker owner
	f.brokerOwner = &store.User{
		ID:          tid("pp-broker-owner"),
		Email:       "pp-broker-owner@test.com",
		DisplayName: "PP Broker Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.brokerOwner))
	ensureHubMembership(ctx, s, f.brokerOwner.ID)

	// Create a non-owner user who owns the agent but NOT the broker
	f.nonOwner = &store.User{
		ID:          tid("pp-non-owner"),
		Email:       "pp-non-owner@test.com",
		DisplayName: "PP Non Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.nonOwner))
	ensureHubMembership(ctx, s, f.nonOwner.ID)

	// Create an admin user
	f.adminUser = &store.User{
		ID:          tid("pp-admin"),
		Email:       "pp-admin@test.com",
		DisplayName: "PP Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.adminUser))
	ensureHubMembership(ctx, s, f.adminUser.ID)
	// Under CO1 the AK1 kernel requires an explicit super-admin role binding.
	grantSuperAdminRole(t, s, f.adminUser.ID)

	// Create a project owned by the non-owner
	f.project = &store.Project{
		ID:        tid("pp-project"),
		Name:      "PP Test Project",
		Slug:      "pp-test-project",
		OwnerID:   f.nonOwner.ID,
		CreatedBy: f.nonOwner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.project))
	srv.createProjectMembersGroup(ctx, f.project)

	// Create a broker owned by the broker owner, with host SA (P8).
	f.broker = &store.RuntimeBroker{
		ID:                         tid("pp-broker"),
		Name:                       "PP Test Broker",
		Slug:                       "pp-test-broker",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  f.brokerOwner.ID,
		GCPHostServiceAccountEmail: "host@test.iam.gserviceaccount.com",
		GCPHostProjectID:           "test",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, f.broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  f.project.ID,
		BrokerID:   f.broker.ID,
		BrokerName: f.broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	f.project.DefaultRuntimeBrokerID = f.broker.ID
	require.NoError(t, s.UpdateProject(ctx, f.project))

	// Create an agent in 'created' phase, owned by non-owner, using this broker.
	// The agent is in created phase so GCP identity can be PATCHed.
	f.agent = &store.Agent{
		ID:              tid("pp-agent"),
		Slug:            "pp-test-agent",
		Name:            "PP Test Agent",
		ProjectID:       f.project.ID,
		Phase:           string(state.PhaseCreated),
		CreatedBy:       f.nonOwner.ID,
		OwnerID:         f.nonOwner.ID,
		RuntimeBrokerID: f.broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, f.agent))

	return f
}

// patchPassthrough sends a PATCH request to switch the agent to passthrough mode.
func patchPassthrough(t *testing.T, f *patchPassthroughFixture, user *store.User, agentID string) (int, string) {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodPatch, "/api/v1/agents/"+agentID,
		map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode": store.GCPMetadataModePassthrough,
			},
		})
	return rec.Code, rec.Body.String()
}

// TestPatchPassthrough_NonOwnerDenied verifies that a user who owns the agent
// but NOT the broker cannot PATCH the agent to passthrough mode.
func TestPatchPassthrough_NonOwnerDenied(t *testing.T) {
	f := setupPatchPassthroughFixture(t)

	code, body := patchPassthrough(t, f, f.nonOwner, f.agent.ID)
	require.Equal(t, http.StatusForbidden, code,
		"non-broker-owner must be denied passthrough via PATCH; got: %s", body)

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(body), &errResp))
	assert.Contains(t, errResp.Error.Message, "broker ownership",
		"error message should mention broker ownership")

	// Verify the agent was NOT modified
	got, err := f.store.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	if got.AppliedConfig != nil && got.AppliedConfig.GCPIdentity != nil {
		assert.NotEqual(t, store.GCPMetadataModePassthrough, got.AppliedConfig.GCPIdentity.MetadataMode,
			"rejected passthrough must not have been persisted")
	}
}

// TestPatchPassthrough_BrokerOwnerAllowed verifies that the broker owner
// can PATCH an agent to passthrough mode.
func TestPatchPassthrough_BrokerOwnerAllowed(t *testing.T) {
	f := setupPatchPassthroughFixture(t)

	// The broker owner also needs update rights on the agent. Make the broker
	// owner a project member so the authz layer allows the PATCH.
	ctx := context.Background()
	addProjectMember(t, f.srv, f.store, f.project, f.brokerOwner)

	// Also transfer agent ownership to broker owner so they have update rights,
	// or create a new agent owned by the broker owner.
	agent := &store.Agent{
		ID:              tid("pp-agent-broker-owned"),
		Slug:            "pp-agent-broker-owned",
		Name:            "PP Agent Broker Owned",
		ProjectID:       f.project.ID,
		Phase:           string(state.PhaseCreated),
		CreatedBy:       f.brokerOwner.ID,
		OwnerID:         f.brokerOwner.ID,
		RuntimeBrokerID: f.broker.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, agent))

	code, body := patchPassthrough(t, f, f.brokerOwner, agent.ID)
	require.Equal(t, http.StatusOK, code,
		"broker owner must be allowed to PATCH passthrough; got: %s", body)

	got, err := f.store.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, got.AppliedConfig.GCPIdentity.MetadataMode)
}

// TestPatchPassthrough_AdminAllowed verifies that an admin (who is not the
// broker owner) can PATCH an agent to passthrough mode.
func TestPatchPassthrough_AdminAllowed(t *testing.T) {
	f := setupPatchPassthroughFixture(t)

	ctx := context.Background()
	// Create an agent owned by admin so they have update rights
	agent := &store.Agent{
		ID:              tid("pp-agent-admin-owned"),
		Slug:            "pp-agent-admin-owned",
		Name:            "PP Agent Admin Owned",
		ProjectID:       f.project.ID,
		Phase:           string(state.PhaseCreated),
		CreatedBy:       f.adminUser.ID,
		OwnerID:         f.adminUser.ID,
		RuntimeBrokerID: f.broker.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, agent))

	code, body := patchPassthrough(t, f, f.adminUser, agent.ID)
	require.Equal(t, http.StatusOK, code,
		"admin must be allowed to PATCH passthrough even without broker ownership; got: %s", body)

	got, err := f.store.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, got.AppliedConfig.GCPIdentity.MetadataMode)
}

// TestPatchPassthrough_AgentCallerDenied verifies that a non-user caller
// (agent) attempting to PATCH passthrough is denied with 403.
func TestPatchPassthrough_AgentCallerDenied(t *testing.T) {
	// Use the bypass agents fixture for agent-authenticated requests
	bf := bypassAgentsSetup(t)

	ctx := context.Background()
	// Create an agent in created phase in the bypass fixture's project,
	// with a broker assigned, that the agent caller (bf.caller) can update
	// via ancestry.
	agent := &store.Agent{
		ID:              tid("pp-agent-for-agent-caller"),
		Slug:            "pp-agent-for-agent-caller",
		Name:            "PP Agent For Agent Caller",
		ProjectID:       bf.proj.ID,
		Phase:           string(state.PhaseCreated),
		CreatedBy:       bf.caller.ID,
		OwnerID:         bf.caller.ID,
		Ancestry:        []string{bf.owner.ID, bf.caller.ID},
		RuntimeBrokerID: bf.broker.ID,
	}
	require.NoError(t, bf.store.CreateAgent(ctx, agent))

	// Agent caller trying to PATCH passthrough
	rec := bf.asAgent(t, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode": store.GCPMetadataModePassthrough,
			},
		})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"agent (non-user) caller must be denied passthrough PATCH; got: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	// Under CO1 the AK1 kernel rejects the agent caller at the authorization
	// layer (missing agent.update permission) before the passthrough gate runs,
	// so the error message is "Insufficient permissions" rather than
	// "broker ownership".
	assert.Contains(t, errResp.Error.Message, "Insufficient permissions",
		"error message should indicate insufficient permissions")
}

// TestPatchPassthrough_AutoProvideBrokerNonOwnerDenied verifies that
// dispatch permission from an AutoProvide broker does not mask the
// broker-ownership restriction for passthrough.
func TestPatchPassthrough_AutoProvideBrokerNonOwnerDenied(t *testing.T) {
	srv, s := bypassAgentsServer(t)
	ctx := context.Background()

	brokerOwner := &store.User{
		ID:          tid("pp-ap-broker-owner"),
		Email:       "pp-ap-broker-owner@test.com",
		DisplayName: "PP AP Broker Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, brokerOwner))
	ensureHubMembership(ctx, s, brokerOwner.ID)

	nonOwner := &store.User{
		ID:          tid("pp-ap-non-owner"),
		Email:       "pp-ap-non-owner@test.com",
		DisplayName: "PP AP Non Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, nonOwner))
	ensureHubMembership(ctx, s, nonOwner.ID)

	project := &store.Project{
		ID:        tid("pp-ap-project"),
		Name:      "PP AP Project",
		Slug:      "pp-ap-project",
		OwnerID:   nonOwner.ID,
		CreatedBy: nonOwner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	// AutoProvide broker: any user can dispatch to it, but that does NOT
	// grant passthrough. The broker is owned by brokerOwner.
	broker := &store.RuntimeBroker{
		ID:          tid("pp-ap-broker"),
		Name:        "PP AP Broker",
		Slug:        "pp-ap-broker",
		Status:      store.BrokerStatusOnline,
		CreatedBy:   brokerOwner.ID,
		AutoProvide: true,
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

	agent := &store.Agent{
		ID:              tid("pp-ap-agent"),
		Slug:            "pp-ap-agent",
		Name:            "PP AP Agent",
		ProjectID:       project.ID,
		Phase:           string(state.PhaseCreated),
		CreatedBy:       nonOwner.ID,
		OwnerID:         nonOwner.ID,
		RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Non-owner of an AutoProvide broker should still be denied passthrough
	rec := doRequestAsUser(t, srv, nonOwner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode": store.GCPMetadataModePassthrough,
			},
		})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"AutoProvide dispatch permission must not grant passthrough; got: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Contains(t, errResp.Error.Message, "broker ownership",
		"error message should mention broker ownership")
}

// TestPatchPassthrough_NoBrokerValidationError verifies that attempting to
// PATCH passthrough on an agent with no runtime broker returns a validation error.
func TestPatchPassthrough_NoBrokerValidationError(t *testing.T) {
	srv, s := bypassAgentsServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("pp-no-broker-user"),
		Email:       "pp-no-broker@test.com",
		DisplayName: "PP No Broker User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("pp-no-broker-project"),
		Name:      "PP No Broker Project",
		Slug:      "pp-no-broker-project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	// Agent with NO runtime broker
	agent := &store.Agent{
		ID:              tid("pp-no-broker-agent"),
		Slug:            "pp-no-broker-agent",
		Name:            "PP No Broker Agent",
		ProjectID:       project.ID,
		Phase:           string(state.PhaseCreated),
		CreatedBy:       user.ID,
		OwnerID:         user.ID,
		RuntimeBrokerID: "", // No broker
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	rec := doRequestAsUser(t, srv, user, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode": store.GCPMetadataModePassthrough,
			},
		})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"passthrough with no broker should return validation error; got: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Contains(t, errResp.Error.Message, "runtime broker",
		"error message should mention runtime broker requirement")
}

// addProjectMember adds a user to a project's members group.
func addProjectMember(t *testing.T, srv *Server, s store.Store, project *store.Project, user *store.User) {
	t.Helper()
	ctx := context.Background()
	membersSlug := "project:" + project.Slug + ":members"
	group, err := s.GetGroupBySlug(ctx, membersSlug)
	require.NoError(t, err, "project members group should exist")
	err = s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    group.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   user.ID,
		Role:       "member",
		AddedAt:    time.Now(),
	})
	if err != nil {
		// Ignore if already a member
		t.Logf("AddGroupMember: %v (may already be a member)", err)
	}
}
