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
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/require"
)

// cpmSetup creates two projects (A and B) with agents, owners, and hub-level
// operational settings that enable cross-project messaging.
func cpmSetup(t *testing.T) (srv *Server, s store.Store, projectA, projectB string, ownerA, ownerB *store.User, agentA, agentB *store.Agent) {
	t.Helper()
	srv, s = testServer(t)
	projectA, projectB, ownerA, ownerB, agentA, agentB = cpmSetupOn(t, srv, s)
	return srv, s, projectA, projectB, ownerA, ownerB, agentA, agentB
}

// cpmSetupWithFault is cpmSetup with a switch-gated store wrapper (see
// installStoreFault) installed before the fixture's audited setup
// (seedProjectCreatorMembership emits mutation audits whose goroutines read
// srv.store). Tests call fault.Arm() where they used to assign srv.store,
// which would race those goroutines (ptone/scion#3184). It returns only
// what the fault tests use.
func cpmSetupWithFault[W store.Store](t *testing.T, wrap func(inner store.Store, fault *storeFaultSwitch) W) (srv *Server, projectB string, wrapped W, fault *storeFaultSwitch) {
	t.Helper()
	var s store.Store
	srv, s, wrapped, fault = testServerWithStoreFault(t, wrap)
	_, projectB, _, _, _, _ = cpmSetupOn(t, srv, s)
	return srv, projectB, wrapped, fault
}

// enableCPM sets up OperationalSettings with cross_project_messaging_enabled=true.
func enableCPM(t *testing.T, srv *Server, s store.Store) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fileK := koanf.New(".")
	envK := koanf.New(".")
	ops := NewOperationalSettings(fakeStore, fileK, envK)
	// Seed the messaging section with cross-project enabled.
	doc := []byte(`{"conversation_envelope_switch":true,"cross_project_messaging_enabled":true}`)
	rev, err := ops.Update(context.Background(), "messaging", doc, "test", 0, "managed")
	require.NoError(t, err, "failed to seed messaging opsettings")
	require.Greater(t, rev, int64(0), "expected positive revision")
	// Verify the value is readable.
	require.True(t, ops.CrossProjectMessagingEnabled(), "cross-project should be enabled after Update")
	srv.SetOperationalSettings(ops)
}

func cpmAgentIdentity(agentID, projectID string, ancestry []string) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Ancestry:  ancestry,
	}}
}

func cpmSetupOn(t *testing.T, srv *Server, s store.Store) (projectA, projectB string, ownerA, ownerB *store.User, agentA, agentB *store.Agent) {
	t.Helper()
	ctx := context.Background()

	// Create owners
	ownerA = &store.User{
		ID:          tid("cpm-owner-a"),
		Email:       "owner-a@test.com",
		DisplayName: "Owner A",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerA))
	ensureHubMembership(ctx, s, ownerA.ID)

	ownerB = &store.User{
		ID:          tid("cpm-owner-b"),
		Email:       "owner-b@test.com",
		DisplayName: "Owner B",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerB))
	ensureHubMembership(ctx, s, ownerB.ID)

	// Create project A
	projectA = tid("cpm-project-a")
	pA := &store.Project{
		ID:        projectA,
		Name:      "project-a",
		Slug:      "project-a",
		OwnerID:   ownerA.ID,
		CreatedBy: ownerA.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, pA))
	srv.seedProjectCreatorMembership(ctx, pA)
	msgAuthzAddProjectMember(t, s, ownerA.ID, projectA, "project-a", store.GroupMemberRoleOwner)
	// Set inbound policy to "any" (CreateProject doesn't persist this field; default revision is 1)
	_, err := s.UpdateProjectMessagingPolicy(ctx, projectA, store.CrossProjectInboundAny, 1)
	require.NoError(t, err, "failed to set project A inbound policy")

	// Create project B
	projectB = tid("cpm-project-b")
	pB := &store.Project{
		ID:        projectB,
		Name:      "project-b",
		Slug:      "project-b",
		OwnerID:   ownerB.ID,
		CreatedBy: ownerB.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, pB))
	srv.seedProjectCreatorMembership(ctx, pB)
	msgAuthzAddProjectMember(t, s, ownerB.ID, projectB, "project-b", store.GroupMemberRoleOwner)
	// Set inbound policy to "any" (default revision is 1)
	_, err = s.UpdateProjectMessagingPolicy(ctx, projectB, store.CrossProjectInboundAny, 1)
	require.NoError(t, err, "failed to set project B inbound policy")

	// Create hub-mode agent in project A
	agentA = &store.Agent{
		ID:          tid("cpm-agent-a"),
		Name:        "agent-alpha",
		Slug:        "agent-alpha",
		ProjectID:   projectA,
		MessageMode: store.MessageModeHub,
		Ancestry:    []string{ownerA.ID},
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	// Create project-mode agent in project B
	agentB = &store.Agent{
		ID:          tid("cpm-agent-b"),
		Name:        "agent-beta",
		Slug:        "agent-beta",
		ProjectID:   projectB,
		MessageMode: store.MessageModeProject,
		Ancestry:    []string{ownerB.ID},
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Enable cross-project messaging via OperationalSettings.
	enableCPM(t, srv, s)

	return projectA, projectB, ownerA, ownerB, agentA, agentB
}
