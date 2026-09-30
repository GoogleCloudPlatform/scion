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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// brokerByID finds a decoded broker list entry by ID, or nil if absent.
func brokerByID(brokers []RuntimeBrokerWithCapabilities, id string) *RuntimeBrokerWithCapabilities {
	for i := range brokers {
		if brokers[i].ID == id {
			return &brokers[i]
		}
	}
	return nil
}

// listRuntimeBrokersCapacity issues GET /api/v1/runtime-brokers and decodes
// the response.
func listRuntimeBrokersCapacity(t *testing.T, srv *Server) []RuntimeBrokerWithCapabilities {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ListRuntimeBrokersWithCapsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Brokers
}

// TestListRuntimeBrokers_AgentLimitFromSettingsOverride covers the "7 / 30"
// brokers-list column (design.md §5.6, §5.9): a broker with its own
// settings.maxAgents override must report that value with source "broker",
// through the same brokerCapacity/effectiveBrokerLimit read model Reserve
// enforces (AC-P2-10) — never the raw binding or the hub default.
func TestListRuntimeBrokers_AgentLimitFromSettingsOverride(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      uuid.New().String(),
		Name:    "capacity-override-broker",
		Slug:    "capacity-override-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	const overrideValue = 3
	require.NotEqualValues(t, overrideValue, def.DefaultValue, "override must differ from the default to prove precedence")

	maxAgents := int64(overrideValue)
	_, err = s.PutBrokerSettings(ctx, broker.ID, store.BrokerSettings{MaxAgents: &maxAgents}, 0, "test")
	require.NoError(t, err)

	brokers := listRuntimeBrokersCapacity(t, srv)
	view := brokerByID(brokers, broker.ID)
	require.NotNil(t, view, "response must include the broker")
	require.NotNil(t, view.AgentLimit)
	assert.EqualValues(t, overrideValue, *view.AgentLimit)
	assert.Equal(t, BrokerLimitSourceBroker, view.AgentLimitSource)
}

// TestListRuntimeBrokers_AgentLimitHubDefault covers the no-override,
// no-binding path: the brokers list must report the limit definition's
// default value with source "hub_default".
func TestListRuntimeBrokers_AgentLimitHubDefault(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      uuid.New().String(),
		Name:    "capacity-default-broker",
		Slug:    "capacity-default-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	brokers := listRuntimeBrokersCapacity(t, srv)
	view := brokerByID(brokers, broker.ID)
	require.NotNil(t, view)
	require.NotNil(t, view.AgentLimit)
	assert.EqualValues(t, def.DefaultValue, *view.AgentLimit)
	assert.Equal(t, BrokerLimitSourceHubDefault, view.AgentLimitSource)
}

// TestListRuntimeBrokers_AgentLimitFromEntitlementBinding covers a
// broker-scoped entitlement binding (no settings override): the brokers list
// must report the binding's value with source "entitlement", matching the
// providers listing's semantics exactly (ptone/scion#2161).
func TestListRuntimeBrokers_AgentLimitFromEntitlementBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      uuid.New().String(),
		Name:    "capacity-entitlement-broker",
		Slug:    "capacity-entitlement-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	const bindingValue = 7
	require.NotEqualValues(t, bindingValue, def.DefaultValue)
	seedBinding(t, s, def.ID, store.EntitlementSubjectSystemDefault, "", store.QuotaScopeBroker, broker.ID, bindingValue)

	brokers := listRuntimeBrokersCapacity(t, srv)
	view := brokerByID(brokers, broker.ID)
	require.NotNil(t, view)
	require.NotNil(t, view.AgentLimit)
	assert.EqualValues(t, bindingValue, *view.AgentLimit)
	assert.Equal(t, BrokerLimitSourceEntitlement, view.AgentLimitSource)
}

// TestListRuntimeBrokers_CapacityFieldsAgreeWithSettingsGET pins AC-P2-10 at
// the brokers-list layer: the effective limit and source it reports for a
// broker must agree with what the broker settings GET reports for the same
// broker, since both come from the one shared brokerCapacity read model.
func TestListRuntimeBrokers_CapacityFieldsAgreeWithSettingsGET(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      uuid.New().String(),
		Name:    "capacity-agreement-broker",
		Slug:    "capacity-agreement-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	maxAgents := int64(5)
	_, err := s.PutBrokerSettings(ctx, broker.ID, store.BrokerSettings{MaxAgents: &maxAgents}, 0, "test")
	require.NoError(t, err)

	brokers := listRuntimeBrokersCapacity(t, srv)
	listView := brokerByID(brokers, broker.ID)
	require.NotNil(t, listView)
	require.NotNil(t, listView.AgentLimit)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/"+broker.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var settingsResp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &settingsResp))

	require.NotNil(t, settingsResp.Effective.MaxAgents.Value)
	assert.EqualValues(t, *settingsResp.Effective.MaxAgents.Value, *listView.AgentLimit)
	assert.Equal(t, settingsResp.Effective.MaxAgents.Source, listView.AgentLimitSource)
}

// TestListRuntimeBrokers_DeniedUserSeesNoCapacityFields proves the capacity
// fields grant no new read access (ptone/scion#2061 P2.2, design.md §5.6):
// a caller without broker.read for a broker doesn't see that broker's row at
// all, exactly as before this change, so there is nothing to leak.
func TestListRuntimeBrokers_DeniedUserSeesNoCapacityFields(t *testing.T) {
	f := brokerAuthSetup(t)

	maxAgents := int64(9)
	_, err := f.store.PutBrokerSettings(context.Background(), f.broker.ID, store.BrokerSettings{MaxAgents: &maxAgents}, 0, "test")
	require.NoError(t, err)

	rec := doRequestAsUser(t, f.srv, f.deniedUser, http.MethodGet, "/api/v1/runtime-brokers", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ListRuntimeBrokersWithCapsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, brokerByID(resp.Brokers, f.broker.ID), "a denied user must not see the broker row, capacity fields included")
}

// TestListRuntimeBrokers_OneLimitDefinitionLookupPerListing pins that
// listRuntimeBrokers looks up the max_agents_per_broker limit definition
// once and reuses it for every broker in the page (design.md §5.9,
// AC-P2-10), the same convention listProjectProviders uses.
func TestListRuntimeBrokers_OneLimitDefinitionLookupPerListing(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		broker := &store.RuntimeBroker{
			ID:      uuid.New().String(),
			Name:    "counting-broker",
			Slug:    uuid.New().String(),
			Status:  store.BrokerStatusOnline,
			Created: time.Now(),
			Updated: time.Now(),
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	}

	counting := &countingLimitDefLookupStore{Store: s}
	srv.store = counting

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	assert.Equal(t, 1, counting.calls, "GetLimitDefinitionByName must be called exactly once per listing, not once per broker")
}

// countingLimitDefLookupStore wraps a store.Store to count
// GetLimitDefinitionByName calls, for asserting the "one lookup per listing"
// property cheaply without instrumenting production code.
type countingLimitDefLookupStore struct {
	store.Store
	calls int
}

func (c *countingLimitDefLookupStore) GetLimitDefinitionByName(ctx context.Context, name string) (*store.LimitDefinition, error) {
	c.calls++
	return c.Store.GetLimitDefinitionByName(ctx, name)
}

// TestListRuntimeBrokers_AgentCountAgreesWithReserve pins AC-P2-10 against
// the actual enforcement path, not just a manually-inserted reservation row:
// the brokers list's agentCount/agentLimit must match what
// checkAndReserveBrokerQuota itself counts and enforces (review round 1,
// F2), including the unlimited shape (maxAgents=0): agentLimit absent,
// agentCount still present, source still "broker" (the override — not the
// entitlement engine or the hub default — is what produced "unlimited").
func TestListRuntimeBrokers_AgentCountAgreesWithReserve(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker, project := newQuotaTestBrokerAndProject(t, s, "capacity-list-agree")

	maxAgents := int64(2)
	rec1, err := s.PutBrokerSettings(ctx, broker.ID, store.BrokerSettings{MaxAgents: &maxAgents}, 0, "test")
	require.NoError(t, err)

	agent1 := newQuotaTestAgent(t, s, broker, project, "capacity-list-agree-1", state.PhaseRunning)
	created, err := srv.checkAndReserveBrokerQuota(ctx, agent1)
	require.NoError(t, err)
	assert.True(t, created)

	agent2 := newQuotaTestAgent(t, s, broker, project, "capacity-list-agree-2", state.PhaseRunning)
	created, err = srv.checkAndReserveBrokerQuota(ctx, agent2)
	require.NoError(t, err)
	assert.True(t, created)

	brokers := listRuntimeBrokersCapacity(t, srv)
	view := brokerByID(brokers, broker.ID)
	require.NotNil(t, view)
	require.NotNil(t, view.AgentLimit)
	assert.EqualValues(t, 2, *view.AgentLimit)
	require.NotNil(t, view.AgentCount)
	assert.EqualValues(t, 2, *view.AgentCount, "agentCount must agree with what Reserve actually admitted")
	assert.Equal(t, BrokerLimitSourceBroker, view.AgentLimitSource)

	// A third reservation must be rejected at the cap — the same cap the
	// list just reported.
	agent3 := newQuotaTestAgent(t, s, broker, project, "capacity-list-agree-3", state.PhaseRunning)
	_, err = srv.checkAndReserveBrokerQuota(ctx, agent3)
	assert.ErrorIs(t, err, store.ErrQuotaExceeded)

	// Clear the cap to unlimited (maxAgents=0).
	unlimited := int64(0)
	_, err = s.PutBrokerSettings(ctx, broker.ID, store.BrokerSettings{MaxAgents: &unlimited}, rec1.Revision, "test")
	require.NoError(t, err)

	created, err = srv.checkAndReserveBrokerQuota(ctx, agent3)
	require.NoError(t, err)
	assert.False(t, created, "unlimited resolves before creating a reservation (quota.go)")

	brokers = listRuntimeBrokersCapacity(t, srv)
	view = brokerByID(brokers, broker.ID)
	require.NotNil(t, view)
	assert.Nil(t, view.AgentLimit, "unlimited must leave agentLimit unset")
	require.NotNil(t, view.AgentCount, "agentCount must still be reported when unlimited")
	assert.EqualValues(t, 2, *view.AgentCount,
		"unlimited Reserve returns before creating a reservation, so the count doesn't advance synchronously for agent3")
	assert.Equal(t, BrokerLimitSourceBroker, view.AgentLimitSource,
		"the override (now resolving to unlimited) is still what produced the result, not the entitlement engine or hub default")
}
