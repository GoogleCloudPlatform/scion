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
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// providerCapacityView mirrors the fields of projectProviderView this test
// class cares about, decoded from the GET .../providers response body.
type providerCapacityView struct {
	BrokerID         string `json:"brokerId"`
	AgentLimit       *int64 `json:"agentLimit"`
	AgentCount       *int64 `json:"agentCount"`
	AgentLimitSource string `json:"agentLimitSource"`
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

// listProviderCapacityRaw issues the same GET as listProviderCapacity but
// decodes the response into raw JSON objects instead of providerCapacityView.
// A *int64 field can't distinguish an omitted key from an explicit null, so
// tests that need to assert a key's absence from the wire format — not just
// that the decoded pointer is nil — use this instead.
func listProviderCapacityRaw(t *testing.T, srv *Server, projectID string) []map[string]any {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/providers", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp struct {
		Providers []map[string]any `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Providers
}

// TestListProjectProviders_AgentLimitDefaultNoBindings covers the common
// path with no entitlement bindings at all: agentLimit must reflect the
// max_agents_per_broker limit definition's default value (see
// seedLimitDefinitions), and agentCount must reflect the one agent created
// on the broker (ptone/scion#2161). The expected limit is read from the
// store rather than hard-coded, since the seeded default value is not this
// test's concern and is subject to change independently (e.g. other work
// changing it from 12 to 100).
func TestListProjectProviders_AgentLimitDefaultNoBindings(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	brokerID := project.DefaultRuntimeBrokerID

	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "capacity-default-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	byBroker := listProviderCapacity(t, srv, project.ID)
	view, ok := byBroker[brokerID]
	require.True(t, ok, "response must include the project's provider")

	require.NotNil(t, view.AgentLimit, "agentLimit must be set from the limit definition default")
	assert.EqualValues(t, def.DefaultValue, *view.AgentLimit)
	require.NotNil(t, view.AgentCount)
	assert.EqualValues(t, 1, *view.AgentCount)

	// Sanity: the default-value path really did resolve with zero bindings.
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
	const overrideValue = 3
	require.NotEqualValues(t, overrideValue, def.DefaultValue, "override must differ from the default to prove precedence")

	seedBinding(t, s, def.ID, store.EntitlementSubjectSystemDefault, "", store.QuotaScopeBroker, brokerID, overrideValue)

	byBroker := listProviderCapacity(t, srv, project.ID)
	view, ok := byBroker[brokerID]
	require.True(t, ok)
	require.NotNil(t, view.AgentLimit)
	assert.EqualValues(t, overrideValue, *view.AgentLimit, "broker-scoped override must win over the limit definition default")
}

// TestListProjectProviders_AgentLimitUnsetWhenUnlimited covers the unlimited
// case: when the effective limit resolves to <= 0, agentLimit must be left
// unset (nil) — it is never 0, since a non-positive effective limit means
// unlimited — while agentCount is still reported (ptone/scion#2161). It also
// pins the documented lag on an unlimited broker: because QuotaService.Reserve
// returns before creating a reservation when the effective limit is <= 0
// (quota.go), a running agent is not reflected in agentCount until the
// periodic broker-quota-reconcile job (ReconcileStaleBrokerQuotaReservations)
// backfills it. This exercises both halves — before and after that backfill —
// rather than only the no-agents-at-all case.
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

	raw := listProviderCapacityRaw(t, srv, project.ID)
	require.Len(t, raw, 1)
	_, hasLimit := raw[0]["agentLimit"]
	assert.False(t, hasLimit, "agentLimit key must be absent from the JSON when unlimited")
	_, hasCount := raw[0]["agentCount"]
	assert.True(t, hasCount, "agentCount key must be present in the JSON")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "capacity-unlimited-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	byBroker = listProviderCapacity(t, srv, project.ID)
	view, ok = byBroker[brokerID]
	require.True(t, ok)
	assert.Nil(t, view.AgentLimit, "still unlimited")
	require.NotNil(t, view.AgentCount)
	assert.EqualValues(t, 0, *view.AgentCount,
		"an unlimited broker takes no synchronous reservation, so a running agent doesn't move the count yet")

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	byBroker = listProviderCapacity(t, srv, project.ID)
	view, ok = byBroker[brokerID]
	require.True(t, ok)
	require.NotNil(t, view.AgentCount)
	assert.EqualValues(t, 1, *view.AgentCount,
		"once the periodic reconcile backfills the reservation, the count catches up")
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

	def, err := f.store.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.path(), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp providerCapacityListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Providers, 1)
	view := resp.Providers[0]
	assert.Equal(t, f.linked.ID, view.BrokerID)
	require.NotNil(t, view.AgentLimit, "owner must see the resolved agent limit")
	assert.EqualValues(t, def.DefaultValue, *view.AgentLimit)
	require.NotNil(t, view.AgentCount, "owner must see the resolved agent count")
	assert.EqualValues(t, 0, *view.AgentCount)
}

// failCountReservationsStore wraps a store.Store so CountActiveReservations
// fails only for one scopeID, letting a test simulate a per-provider
// resolution failure without breaking every provider in the listing.
type failCountReservationsStore struct {
	store.Store
	failScopeID string
	err         error
}

func (f *failCountReservationsStore) CountActiveReservations(ctx context.Context, limitDefinitionID, subjectID, scopeType, scopeID string) (int64, error) {
	if scopeID == f.failScopeID {
		return 0, f.err
	}
	return f.Store.CountActiveReservations(ctx, limitDefinitionID, subjectID, scopeType, scopeID)
}

// TestListProjectProviders_ResolutionFailureLeavesOnlyThatProviderUnset
// pins the "one provider's capacity resolution fails, the rest of the
// listing still succeeds" contract (ptone/scion#2161).
// Two providers are linked to the project; the store is made to fail
// CountActiveReservations for only one broker's scopeID. The listing must
// still return 200, the failing provider's AgentLimit and AgentCount must
// both be nil, and the healthy provider's fields must both still be set.
func TestListProjectProviders_ResolutionFailureLeavesOnlyThatProviderUnset(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("project-capacity-partial"),
		Name: "Capacity Partial Project",
		Slug: "capacity-partial-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	healthyBroker := &store.RuntimeBroker{
		ID:     tid("broker-capacity-healthy"),
		Name:   "Healthy Broker",
		Slug:   "healthy-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, healthyBroker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   healthyBroker.ID,
		BrokerName: healthyBroker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	failingBroker := &store.RuntimeBroker{
		ID:     tid("broker-capacity-failing"),
		Name:   "Failing Broker",
		Slug:   "failing-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, failingBroker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   failingBroker.ID,
		BrokerName: failingBroker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	project.DefaultRuntimeBrokerID = healthyBroker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	srv.store = &failCountReservationsStore{Store: s, failScopeID: failingBroker.ID, err: errors.New("boom")}

	byBroker := listProviderCapacity(t, srv, project.ID)
	require.Len(t, byBroker, 2, "the listing must include both providers despite one failing")

	failing, ok := byBroker[failingBroker.ID]
	require.True(t, ok)
	assert.Nil(t, failing.AgentLimit, "resolution failure must leave agentLimit unset")
	assert.Nil(t, failing.AgentCount, "resolution failure must leave agentCount unset")

	healthy, ok := byBroker[healthyBroker.ID]
	require.True(t, ok)
	require.NotNil(t, healthy.AgentLimit, "the healthy provider must still resolve its limit")
	require.NotNil(t, healthy.AgentCount, "the healthy provider must still resolve its count")
	assert.EqualValues(t, 0, *healthy.AgentCount)
}

// failGetLimitDefinitionStore wraps a store.Store so GetLimitDefinitionByName
// always fails with a non-store.ErrNotFound error, simulating a store
// failure while looking up the shared max_agents_per_broker limit
// definition — as opposed to the "no such definition" case, which
// lookupAgentLimitDefinition treats as "no limit enforced".
type failGetLimitDefinitionStore struct {
	store.Store
	err error
}

func (f *failGetLimitDefinitionStore) GetLimitDefinitionByName(ctx context.Context, name string) (*store.LimitDefinition, error) {
	return nil, f.err
}

// TestListProjectProviders_LimitDefinitionLookupFailureLeavesAllUnset pins
// the "one lookup per listing" behaviour from hoisting
// lookupAgentLimitDefinition out of the per-provider loop: when that single
// lookup fails with something other than store.ErrNotFound, every
// provider's AgentLimit and AgentCount must be left unset, and the listing
// must still return 200 rather than fail the whole request (ptone/scion#2161).
func TestListProjectProviders_LimitDefinitionLookupFailureLeavesAllUnset(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	brokerID := project.DefaultRuntimeBrokerID

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "capacity-limitdef-failure-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	srv.store = &failGetLimitDefinitionStore{Store: s, err: errors.New("boom")}

	byBroker := listProviderCapacity(t, srv, project.ID)
	view, ok := byBroker[brokerID]
	require.True(t, ok)
	assert.Nil(t, view.AgentLimit, "limit definition lookup failure must leave agentLimit unset")
	assert.Nil(t, view.AgentCount, "limit definition lookup failure must leave agentCount unset")

	raw := listProviderCapacityRaw(t, srv, project.ID)
	require.Len(t, raw, 1)
	_, hasLimit := raw[0]["agentLimit"]
	assert.False(t, hasLimit, "agentLimit key must be absent from the JSON on lookup failure")
	_, hasCount := raw[0]["agentCount"]
	assert.False(t, hasCount, "agentCount key must be absent from the JSON on lookup failure")
}

// failResolveEffectiveLimitStore wraps a store.Store so
// ListEntitlementBindingsForSubject always fails, which makes
// QuotaService.ResolveEffectiveLimit fail (it is the first store call
// ResolveEffectiveLimit makes) without touching GetLimitDefinitionByName or
// CountActiveReservations.
type failResolveEffectiveLimitStore struct {
	store.Store
	err error
}

func (f *failResolveEffectiveLimitStore) ListEntitlementBindingsForSubject(ctx context.Context, subjectType, subjectID string) ([]*store.EntitlementBinding, error) {
	return nil, f.err
}

// TestListProjectProviders_ResolveEffectiveLimitFailureLeavesUnset covers
// the other resolveBrokerCapacity failure branch alongside
// TestListProjectProviders_ResolutionFailureLeavesOnlyThatProviderUnset:
// when ResolveEffectiveLimit itself fails, the provider's AgentLimit and
// AgentCount must both be left unset and the listing must still return 200
// (ptone/scion#2161).
func TestListProjectProviders_ResolveEffectiveLimitFailureLeavesUnset(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	brokerID := project.DefaultRuntimeBrokerID

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "capacity-resolvelimit-failure-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	failingStore := &failResolveEffectiveLimitStore{Store: s, err: errors.New("boom")}
	srv.quotaService = &QuotaService{store: failingStore, logger: slog.Default()}

	byBroker := listProviderCapacity(t, srv, project.ID)
	view, ok := byBroker[brokerID]
	require.True(t, ok)
	assert.Nil(t, view.AgentLimit, "ResolveEffectiveLimit failure must leave agentLimit unset")
	assert.Nil(t, view.AgentCount, "ResolveEffectiveLimit failure must leave agentCount unset")
}
