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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateAgent_ProjectMemberCannotAutoLinkBroker: a project member holds
// agent.create but not project.update. Naming a broker that is not yet a
// provider of the project must be denied, and must leave no trace: no
// provider row, no default broker.
func TestCreateAgent_ProjectMemberCannotAutoLinkBroker(t *testing.T) {
	f := brokerLinkAuthzSetup(t)

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost,
		"/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "member-named-broker-agent", RuntimeBrokerID: f.unlinked.ID})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Empty(t, f.providerIDs(t, f.proj.ID), "no provider row may be created on denial")
	assert.Empty(t, f.defaultBroker(t, f.proj.ID), "no default broker may be set on denial")

	_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "member-named-broker-agent")
	assert.Equal(t, store.ErrNotFound, err, "no agent may have been created")
}

// TestCreateAgent_OwnerAutoLinksBrokerWithDefault: the project owner holds
// project.update, so naming a not-yet-linked broker still succeeds — the
// gate must not break the legitimate flow it was modeled on.
func TestCreateAgent_OwnerAutoLinksBrokerWithDefault(t *testing.T) {
	f := brokerLinkAuthzSetup(t)

	rec := createAgentAsOwner(t, f.bypassAgentsFixture, CreateAgentRequest{
		Name:            "owner-named-broker-agent",
		RuntimeBrokerID: f.unlinked.ID,
	})

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	assert.ElementsMatch(t, []string{f.unlinked.ID}, f.providerIDs(t, f.proj.ID))
	assert.Equal(t, f.unlinked.ID, f.defaultBroker(t, f.proj.ID),
		"linking to a project with no default broker still sets it")
}

// TestCreateAgent_AgentCallerCannotAutoLinkBroker: an agent identity with
// ScopeAgentCreate can create agents in its own project but never holds
// project.update. Naming a not-yet-linked broker must be denied and leave no
// trace, matching the member case above.
func TestCreateAgent_AgentCallerCannotAutoLinkBroker(t *testing.T) {
	f := brokerLinkAuthzSetup(t)

	rec := f.asAgent(t, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "agent-named-broker-agent", RuntimeBrokerID: f.unlinked.ID},
		ScopeAgentCreate)

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Empty(t, f.providerIDs(t, f.proj.ID), "no provider row may be created on denial")
	assert.Empty(t, f.defaultBroker(t, f.proj.ID), "no default broker may be set on denial")
}

// TestCreateAgent_MemberNamingExistingProviderUnaffected is the regression
// check: naming a broker that is ALREADY a provider does not touch the new
// gate at all (Case 1's existing-provider branch returns before it), so a
// low-privilege member creating an agent against an already-linked broker
// keeps working exactly as before.
func TestCreateAgent_MemberNamingExistingProviderUnaffected(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	require.NoError(t, f.store.AddProjectProvider(context.Background(), &store.ProjectProvider{
		ProjectID:  f.proj.ID,
		BrokerID:   f.broker.ID,
		BrokerName: f.broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost,
		"/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "member-named-existing-provider-agent", RuntimeBrokerID: f.broker.ID})

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}
