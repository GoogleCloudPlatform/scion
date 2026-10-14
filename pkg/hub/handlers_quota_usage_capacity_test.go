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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkCapacityTestBroker creates a runtime broker for the usage-capacity
// tests below.
func mkCapacityTestBroker(t *testing.T, s store.Store, name string) *store.RuntimeBroker {
	t.Helper()
	b := &store.RuntimeBroker{
		ID:      uuid.New().String(),
		Name:    name,
		Slug:    uuid.New().String(),
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), b))
	return b
}

// reservationByScopeID finds a decoded usage reservation by its ScopeID
// (the broker ID, for max_agents_per_broker rows).
func reservationByScopeID(reservations []usageReservationView, scopeID string) *usageReservationView {
	for i := range reservations {
		if reservations[i].ScopeID == scopeID {
			return &reservations[i]
		}
	}
	return nil
}

// TestGetUsageByLimit_MaxAgentsPerBroker_PerBrokerSourceAndLimit is the
// central P2.2 test (ptone/scion#2061, ptone/scion#2177 "and/or the admin
// Quotas page"): the admin usage detail for max_agents_per_broker must show
// each broker's actual effective limit and precedence source — broker
// override, entitlement binding, or hub default — never the raw binding or
// the hub-wide default value substituted for a broker that has its own
// override (AC-P2-10). It also proves the underlying listing bug is fixed:
// before this change, getUsageByLimit only queried store.QuotaScopeSystem
// reservations, so a broker-scoped limit like max_agents_per_broker always
// came back with zero reservations regardless of real broker usage.
func TestGetUsageByLimit_MaxAgentsPerBroker_PerBrokerSourceAndLimit(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	brokerOverride := mkCapacityTestBroker(t, s, "usage-override-broker")
	brokerDefault := mkCapacityTestBroker(t, s, "usage-default-broker")
	brokerEntitlement := mkCapacityTestBroker(t, s, "usage-entitlement-broker")

	const overrideValue = 3
	const bindingValue = 7
	require.NotEqualValues(t, overrideValue, def.DefaultValue)
	require.NotEqualValues(t, bindingValue, def.DefaultValue)

	maxAgents := int64(overrideValue)
	_, err = s.PutBrokerSettings(ctx, brokerOverride.ID, store.BrokerSettings{MaxAgents: &maxAgents}, 0, "test")
	require.NoError(t, err)

	seedBinding(t, s, def.ID, store.EntitlementSubjectSystemDefault, "", store.QuotaScopeBroker, brokerEntitlement.ID, bindingValue)

	for _, b := range []*store.RuntimeBroker{brokerOverride, brokerDefault, brokerEntitlement} {
		_, err := s.CreateUsageReservation(ctx, &store.UsageReservation{
			LimitDefinitionID: def.ID,
			SubjectID:         b.ID,
			ScopeType:         store.QuotaScopeBroker,
			ScopeID:           b.ID,
			ResourceID:        "agent-on-" + b.ID,
			Reserved:          1,
		})
		require.NoError(t, err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageByLimitResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 3, resp.TotalActive, "reservations across every broker must be listed, not just system-scoped ones")
	require.Len(t, resp.Reservations, 3)

	override := reservationByScopeID(resp.Reservations, brokerOverride.ID)
	require.NotNil(t, override)
	require.NotNil(t, override.BrokerAgentLimit)
	assert.EqualValues(t, overrideValue, *override.BrokerAgentLimit)
	assert.Equal(t, BrokerLimitSourceBroker, override.BrokerAgentLimitSource)

	defaultView := reservationByScopeID(resp.Reservations, brokerDefault.ID)
	require.NotNil(t, defaultView)
	require.NotNil(t, defaultView.BrokerAgentLimit)
	assert.EqualValues(t, def.DefaultValue, *defaultView.BrokerAgentLimit)
	assert.Equal(t, BrokerLimitSourceHubDefault, defaultView.BrokerAgentLimitSource)

	entitlement := reservationByScopeID(resp.Reservations, brokerEntitlement.ID)
	require.NotNil(t, entitlement)
	require.NotNil(t, entitlement.BrokerAgentLimit)
	assert.EqualValues(t, bindingValue, *entitlement.BrokerAgentLimit)
	assert.Equal(t, BrokerLimitSourceEntitlement, entitlement.BrokerAgentLimitSource)
}

// TestGetUsageByLimit_NonBrokerLimit_Unaffected proves the broker-scoped
// enumeration in getUsageByLimit is opt-in by limit name: a system-scoped
// limit must keep using the original system-scope query and must never
// carry BrokerAgentLimit/BrokerAgentLimitSource.
func TestGetUsageByLimit_NonBrokerLimit_Unaffected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	def := seedLimit(t, s, "usage_capacity_non_broker_limit", 5)
	_, err := s.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: def.ID,
		SubjectID:         "some-user",
		ScopeType:         store.QuotaScopeSystem,
		ScopeID:           "",
		ResourceID:        "agent-1",
		Reserved:          1,
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageByLimitResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Reservations, 1)
	assert.Nil(t, resp.Reservations[0].BrokerAgentLimit)
	assert.Empty(t, resp.Reservations[0].BrokerAgentLimitSource)

	raw := struct {
		Reservations []map[string]any `json:"reservations"`
	}{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Len(t, raw.Reservations, 1)
	_, hasLimit := raw.Reservations[0]["brokerAgentLimit"]
	assert.False(t, hasLimit, "brokerAgentLimit key must be absent from the JSON for a non-broker limit")
	_, hasSource := raw.Reservations[0]["brokerAgentLimitSource"]
	assert.False(t, hasSource, "brokerAgentLimitSource key must be absent from the JSON for a non-broker limit")
}

// TestGetUsageByLimit_MaxAgentsPerBroker_NoActiveReservations proves the
// broker-scoped enumeration returns an empty, not nil, list and a 200 when
// no broker currently holds a reservation — mirroring the original
// no-reservations behaviour for other limits.
func TestGetUsageByLimit_MaxAgentsPerBroker_NoActiveReservations(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	mkCapacityTestBroker(t, s, "usage-idle-broker")

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageByLimitResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.TotalActive)
	assert.Empty(t, resp.Reservations)
}

// TestGetUsageSummary_MaxAgentsPerBroker_SumsAcrossBrokers proves the admin
// usage summary (GET /api/v1/admin/usage) reports the real active count for
// max_agents_per_broker by summing reservations across every runtime broker,
// the same way getUsageByLimit and ReconcileStaleBrokerQuotaReservations
// enumerate brokers (ptone/scion#2061 P2.2). Before this fix, the summary's
// activeCount for this limit always queried store.QuotaScopeSystem — a scope
// max_agents_per_broker reservations are never stored at — so the admin
// quotas page always showed 0 active agents for it regardless of real usage.
func TestGetUsageSummary_MaxAgentsPerBroker_SumsAcrossBrokers(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	brokerA := mkCapacityTestBroker(t, s, "summary-broker-a")
	brokerB := mkCapacityTestBroker(t, s, "summary-broker-b")

	// Two reservations on brokerA, one on brokerB: the summary must report 3
	// total, not 0 (the pre-fix behaviour) and not just one broker's count.
	for _, res := range []*store.UsageReservation{
		{LimitDefinitionID: def.ID, SubjectID: brokerA.ID, ScopeType: store.QuotaScopeBroker, ScopeID: brokerA.ID, ResourceID: "agent-a1", Reserved: 1},
		{LimitDefinitionID: def.ID, SubjectID: brokerA.ID, ScopeType: store.QuotaScopeBroker, ScopeID: brokerA.ID, ResourceID: "agent-a2", Reserved: 1},
		{LimitDefinitionID: def.ID, SubjectID: brokerB.ID, ScopeType: store.QuotaScopeBroker, ScopeID: brokerB.ID, ResourceID: "agent-b1", Reserved: 1},
	} {
		_, err := s.CreateUsageReservation(ctx, res)
		require.NoError(t, err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	var found *usageSummaryEntry
	for i := range resp.Items {
		if resp.Items[i].LimitDefinition.ID == def.ID {
			found = &resp.Items[i]
			break
		}
	}
	require.NotNil(t, found, "summary must include the max_agents_per_broker limit")
	assert.Equal(t, 3, found.ActiveCount, "activeCount must sum reservations across every broker, not just query system scope")
}

// TestGetUsageSummary_NonBrokerLimit_Unaffected proves the broker-scoped
// enumeration in getUsageSummary is opt-in by limit name: a system-scoped
// limit must keep using the original system-scope query.
func TestGetUsageSummary_NonBrokerLimit_Unaffected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	def := seedLimit(t, s, "usage_summary_non_broker_limit", 5)
	_, err := s.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: def.ID,
		SubjectID:         "some-user",
		ScopeType:         store.QuotaScopeSystem,
		ScopeID:           "",
		ResourceID:        "agent-1",
		Reserved:          1,
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	var found *usageSummaryEntry
	for i := range resp.Items {
		if resp.Items[i].LimitDefinition.ID == def.ID {
			found = &resp.Items[i]
			break
		}
	}
	require.NotNil(t, found)
	assert.Equal(t, 1, found.ActiveCount)
}
