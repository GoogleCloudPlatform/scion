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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// providerCapacityView mirrors the fields of ProjectProviderView this test
// class cares about, decoded from the GET .../providers response body.
type providerCapacityView struct {
	BrokerID   string `json:"brokerId"`
	AgentLimit *int64 `json:"agentLimit"`
	AgentCount *int64 `json:"agentCount"`
}

type providerCapacityListResponse struct {
	Providers []providerCapacityView `json:"providers"`
}

// listProviderCapacity issues GET .../providers as the dev user and decodes
// the response into providerCapacityView records, keyed by broker ID for
// convenient lookup.
func listProviderCapacity(t *testing.T, srv *Server, projectID string) map[string]providerCapacityView {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/providers", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp providerCapacityListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	byBroker := make(map[string]providerCapacityView, len(resp.Providers))
	for _, p := range resp.Providers {
		byBroker[p.BrokerID] = p
	}
	return byBroker
}

// TestListProjectProviders_AgentLimitDefaultNoBindings covers the common
// path with no entitlement bindings at all: agentLimit must reflect the
// max_agents_per_broker limit definition's default value (seeded at 12, see
// seedLimitDefinitions), and agentCount must reflect the one agent created
// on the broker (ptone/scion#2161).
func TestListProjectProviders_AgentLimitDefaultNoBindings(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	brokerID := project.DefaultRuntimeBrokerID

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "capacity-default-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	byBroker := listProviderCapacity(t, srv, project.ID)
	view, ok := byBroker[brokerID]
	require.True(t, ok, "response must include the project's provider")

	require.NotNil(t, view.AgentLimit, "agentLimit must be set from the limit definition default")
	assert.EqualValues(t, 12, *view.AgentLimit)
	require.NotNil(t, view.AgentCount)
	assert.EqualValues(t, 1, *view.AgentCount)

	// Sanity: the default-value path really did resolve with zero bindings.
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	bindings, err := s.ListEntitlementBindingsForSubject(context.Background(), store.EntitlementSubjectSystemDefault, "")
	require.NoError(t, err)
	for _, b := range bindings {
		assert.NotEqual(t, def.ID, b.LimitDefinitionID, "this test must exercise the no-bindings default path")
	}
}

// TestListProjectProviders_AgentLimitBrokerScopedOverride covers a
// broker-scoped entitlement binding overriding the limit definition's
// default value: agentLimit must reflect the override, not the default
// (ptone/scion#2161).
func TestListProjectProviders_AgentLimitBrokerScopedOverride(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	brokerID := project.DefaultRuntimeBrokerID
	ctx := context.Background()

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	require.EqualValues(t, 12, def.DefaultValue, "override must differ from the default to prove precedence")

	seedBinding(t, s, def.ID, store.EntitlementSubjectSystemDefault, "", store.QuotaScopeBroker, brokerID, 3)

	byBroker := listProviderCapacity(t, srv, project.ID)
	view, ok := byBroker[brokerID]
	require.True(t, ok)
	require.NotNil(t, view.AgentLimit)
	assert.EqualValues(t, 3, *view.AgentLimit, "broker-scoped override must win over the limit definition default")
}

// TestListProjectProviders_AgentLimitUnsetWhenUnlimited covers the
// unlimited case: when the effective limit resolves to <= 0, agentLimit
// must be left unset (nil), distinguishing it from an actual limit of zero,
// while agentCount is still reported (ptone/scion#2161).
func TestListProjectProviders_AgentLimitUnsetWhenUnlimited(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{createPhase: string(state.PhaseRunning)})
	brokerID := project.DefaultRuntimeBrokerID

	setBrokerAgentCeiling(t, s, 0) // 0 means unlimited, see ResolveEffectiveLimit.

	byBroker := listProviderCapacity(t, srv, project.ID)
	view, ok := byBroker[brokerID]
	require.True(t, ok)
	assert.Nil(t, view.AgentLimit, "unlimited must leave agentLimit unset")
	require.NotNil(t, view.AgentCount, "agentCount must still be reported when unlimited")
	assert.EqualValues(t, 0, *view.AgentCount)
}

// TestListProjectProviders_AgentCountReflectsActiveReservations proves
// agentCount tracks active max_agents_per_broker reservations exactly as
// the quota gate does: it must drop when a counted agent transitions to a
// non-counted phase, using the same stop path exercised in
// TestBrokerQuota_StopFreesSlot (ptone/scion#2161, ptone/scion#1963).
func TestListProjectProviders_AgentCountReflectsActiveReservations(t *testing.T) {
	disp := &quotaLifecycleDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetDispatcher(disp)
	brokerID := project.DefaultRuntimeBrokerID

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "capacity-count-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	byBroker := listProviderCapacity(t, srv, project.ID)
	require.NotNil(t, byBroker[brokerID].AgentCount)
	assert.EqualValues(t, 1, *byBroker[brokerID].AgentCount, "running agent must be counted")

	recStop := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+created.Agent.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, recStop.Code, recStop.Body.String())
	// Confirm against the reservation table directly, the same way
	// broker_quota_test.go does, before re-checking the providers view.
	require.EqualValues(t, 0, brokerReservationCount(t, s, brokerID))

	byBroker = listProviderCapacity(t, srv, project.ID)
	require.NotNil(t, byBroker[brokerID].AgentCount)
	assert.EqualValues(t, 0, *byBroker[brokerID].AgentCount, "stopped agent must no longer be counted")
}

// TestListProjectProviders_OwnerSeesCapacityFields confirms a caller who
// could already read the providers list (the project owner) sees the new
// fields — this change grants no new read access, it only adds fields to an
// existing, already-authorized response (ptone/scion#2161).
func TestListProjectProviders_OwnerSeesCapacityFields(t *testing.T) {
	f := providersAuthzSetup(t)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.path(), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp providerCapacityListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Providers, 1)
	view := resp.Providers[0]
	assert.Equal(t, f.linked.ID, view.BrokerID)
	require.NotNil(t, view.AgentLimit, "owner must see the resolved agent limit")
	assert.EqualValues(t, 12, *view.AgentLimit)
	require.NotNil(t, view.AgentCount, "owner must see the resolved agent count")
	assert.EqualValues(t, 0, *view.AgentCount)
}
