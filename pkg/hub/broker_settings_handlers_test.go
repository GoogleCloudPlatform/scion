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

// newBrokerSettingsTestBroker creates an online runtime broker directly in
// the store, owned by createdBy (may be "" for no owner), for broker
// settings API tests.
func newBrokerSettingsTestBroker(t *testing.T, s store.Store, slug, createdBy string) *store.RuntimeBroker {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:        tid("broker-settings-" + slug),
		Name:      "Broker " + slug,
		Slug:      slug,
		Status:    store.BrokerStatusOnline,
		CreatedBy: createdBy,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	return broker
}

func settingsPath(brokerID string) string {
	return "/api/v1/runtime-brokers/" + brokerID + "/settings"
}

// =============================================================================
// GET: missing broker -> 404
// =============================================================================

func TestBrokerSettings_Get_MissingBroker404(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, settingsPath(tid("no-such-broker")), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// =============================================================================
// GET: no settings row -> settings={}, revision=0, effective reflects the
// hub-wide default (design.md §5.4).
// =============================================================================

func TestBrokerSettings_Get_NoRowDefaults(t *testing.T) {
	srv, s := testServer(t)
	broker := newBrokerSettingsTestBroker(t, s, "no-row", "")

	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodGet, settingsPath(broker.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, broker.ID, resp.BrokerID)
	assert.Nil(t, resp.Settings.MaxAgents, "no settings row means an empty document")
	assert.Equal(t, int64(0), resp.Revision)
	require.NotNil(t, resp.Effective.MaxAgents.Value)
	assert.EqualValues(t, def.DefaultValue, *resp.Effective.MaxAgents.Value)
	assert.Equal(t, BrokerLimitSourceHubDefault, resp.Effective.MaxAgents.Source)
	assert.True(t, resp.Capabilities.Update, "the dev/admin caller must be able to write")
}

// =============================================================================
// PUT: unknown key -> 400
// =============================================================================

func TestBrokerSettings_Put_UnknownKey400(t *testing.T) {
	srv, s := testServer(t)
	broker := newBrokerSettingsTestBroker(t, s, "unknown-key", "")

	rec := doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"bogusKey": 1},
		"expectedRevision": 0,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// =============================================================================
// PUT: negative value -> 400
// =============================================================================

func TestBrokerSettings_Put_NegativeValue400(t *testing.T) {
	srv, s := testServer(t)
	broker := newBrokerSettingsTestBroker(t, s, "negative-value", "")

	rec := doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": -1},
		"expectedRevision": 0,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// =============================================================================
// PUT: broker owner without quota.update -> 403; GET still succeeds (via
// broker ownership) but _capabilities.update is false (design.md §5.3,
// AC-P2-3).
// =============================================================================

func TestBrokerSettings_Put_OwnerWithoutQuotaUpdate403(t *testing.T) {
	srv, s := testServer(t)
	owner := newPlainUser(t, s, "broker-owner")
	broker := newBrokerSettingsTestBroker(t, s, "owner-no-quota", owner.ID)

	getRec := doRequestAsUser(t, srv, owner, http.MethodGet, settingsPath(broker.ID), nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var getResp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getResp))
	assert.False(t, getResp.Capabilities.Update, "a broker owner without quota.update must not see update capability")

	putRec := doRequestAsUser(t, srv, owner, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 5},
		"expectedRevision": 0,
	})
	assert.Equal(t, http.StatusForbidden, putRec.Code, putRec.Body.String())
}

// =============================================================================
// PUT: hub-admin -> 200
// =============================================================================

func TestBrokerSettings_Put_HubAdmin200(t *testing.T) {
	srv, s := testServer(t)
	admin := newSuperAdminUser(t, s, "broker-settings-admin")
	broker := newBrokerSettingsTestBroker(t, s, "hub-admin", "")

	rec := doRequestAsUser(t, srv, admin, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 7},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Settings.MaxAgents)
	assert.EqualValues(t, 7, *resp.Settings.MaxAgents)
	assert.Equal(t, int64(1), resp.Revision)
	assert.Equal(t, BrokerLimitSourceBroker, resp.Effective.MaxAgents.Source)
	require.NotNil(t, resp.Effective.MaxAgents.Value)
	assert.EqualValues(t, 7, *resp.Effective.MaxAgents.Value)
}

// A non-admin user explicitly granted quota.update (P2-D3's "hub admins" is
// enforced by permission, not by the built-in admin role specifically) must
// also be able to write.
func TestBrokerSettings_Put_GrantedQuotaUpdate200(t *testing.T) {
	srv, s := testServer(t)
	user := newPlainUser(t, s, "quota-granted-user")
	grantUserActionOnResource(t, s, user.ID, "quota", "hub", ActionUpdate)
	broker := newBrokerSettingsTestBroker(t, s, "granted-quota", user.ID)

	rec := doRequestAsUser(t, srv, user, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 2},
		"expectedRevision": 0,
	})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// =============================================================================
// PUT: stale revision -> 409, body contains the current record
// =============================================================================

func TestBrokerSettings_Put_StaleRevision409(t *testing.T) {
	srv, s := testServer(t)
	broker := newBrokerSettingsTestBroker(t, s, "stale-revision", "")

	first := doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 4},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())

	// Reuse expectedRevision 0 (create-only) again — the row now exists, so
	// this is a stale/incorrect revision from the caller's point of view.
	second := doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 9},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusConflict, second.Code, second.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeRevisionConflict, body["error"])
	current, ok := body["current"].(map[string]interface{})
	require.True(t, ok, "409 body must include the current record: %s", second.Body.String())
	assert.EqualValues(t, 1, current["revision"])
}

// =============================================================================
// Precedence (design.md §5.2): broker setting > entitlement binding > hub
// default; broker=0 means unlimited.
// =============================================================================

func TestEffectiveBrokerLimit_Precedence(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newBrokerSettingsTestBroker(t, s, "precedence", "")

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	setBrokerAgentCeiling(t, s, 16)
	seedBinding(t, s, def.ID, store.EntitlementSubjectSystemDefault, "", store.QuotaScopeSystem, "", 30)

	// Refresh the definition pointer post-update (DefaultValue changed).
	def, err = s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	// Unset: existing entitlement-engine behaviour applies unchanged — the
	// system-scoped binding (30) beats the hub default (16).
	value, source, err := srv.effectiveBrokerLimit(ctx, broker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 30, value)
	assert.Equal(t, BrokerLimitSourceEntitlement, source)

	// broker=3: the per-broker override beats both the system-scoped
	// binding (30) and the hub default (16).
	rec := doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 3},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	value, source, err = srv.effectiveBrokerLimit(ctx, broker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 3, value)
	assert.Equal(t, BrokerLimitSourceBroker, source)

	// broker=0: unlimited, still sourced from the broker's own setting.
	rec = doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 0},
		"expectedRevision": 1,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	value, source, err = srv.effectiveBrokerLimit(ctx, broker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 0, value)
	assert.Equal(t, BrokerLimitSourceBroker, source)
}

// =============================================================================
// Heartbeat and re-registration must not touch broker settings
// (AC-P2-4).
// =============================================================================

func TestBrokerSettings_HeartbeatLeavesSettingsUnchanged(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newBrokerSettingsTestBroker(t, s, "heartbeat", "")

	putRec := doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 5},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	before, err := s.GetBrokerSettings(ctx, broker.ID)
	require.NoError(t, err)

	// Exercise the same store call the heartbeat handler makes
	// (handleBrokerHeartbeat, pkg/hub/handlers_runtime_brokers.go) directly,
	// rather than through HTTP: heartbeats authenticate as the broker's own
	// HMAC identity in production, which is out of scope to simulate here.
	// The property under test — that a runtime_brokers write never touches
	// the separate broker_settings row — is a store-layer property either
	// way (design.md §5.1: settings live in their own table specifically so
	// heartbeats never contend with them).
	require.NoError(t, s.UpdateRuntimeBrokerHeartbeat(ctx, broker.ID, string(store.BrokerStatusOnline)))

	after, err := s.GetBrokerSettings(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, before.Revision, after.Revision)
	require.NotNil(t, after.Settings.MaxAgents)
	assert.EqualValues(t, 5, *after.Settings.MaxAgents)
}

// =============================================================================
// End to end through HTTP (design.md §5.7): set maxAgents=1 -> the 2nd
// create on that broker returns 429; clear it -> the hub default applies;
// another broker is unaffected. The providers endpoint and settings GET
// agree with what Reserve enforces (AC-P2-10).
// =============================================================================

func TestBrokerSettings_EndToEndEnforcement(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	brokerID := project.DefaultRuntimeBrokerID

	otherBroker := newTestBroker(t, s, "unaffected")
	otherProject := addProjectOnBroker(t, s, "unaffected-project", otherBroker)

	// Set maxAgents=1 on the primary broker via PUT.
	putRec := doRequest(t, srv, http.MethodPut, settingsPath(brokerID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 1},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	// First create succeeds (at the cap).
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "settings-e2e-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())

	// Second create on the same broker is rejected.
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "settings-e2e-2", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusTooManyRequests, rec2.Code, rec2.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeQuotaExceeded, errResp.Error.Code)

	// The other broker, on a different project, is unaffected.
	rec3 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "settings-e2e-other-broker", ProjectID: otherProject.ID,
	})
	require.Equal(t, http.StatusCreated, rec3.Code, rec3.Body.String())

	// Providers endpoint and settings GET agree with Reserve: the primary
	// broker reports limit=1 and count=1 from both read paths.
	providersRec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/providers", nil)
	require.Equal(t, http.StatusOK, providersRec.Code, providersRec.Body.String())
	var providersResp struct {
		Providers []providerCapacityView `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(providersRec.Body.Bytes(), &providersResp))
	require.Len(t, providersResp.Providers, 1)
	require.NotNil(t, providersResp.Providers[0].AgentLimit)
	assert.EqualValues(t, 1, *providersResp.Providers[0].AgentLimit)
	require.NotNil(t, providersResp.Providers[0].AgentCount)
	assert.EqualValues(t, 1, *providersResp.Providers[0].AgentCount)

	settingsRec := doRequest(t, srv, http.MethodGet, settingsPath(brokerID), nil)
	require.Equal(t, http.StatusOK, settingsRec.Code, settingsRec.Body.String())
	var settingsResp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(settingsRec.Body.Bytes(), &settingsResp))
	require.NotNil(t, settingsResp.Effective.MaxAgents.Value)
	assert.EqualValues(t, 1, *settingsResp.Effective.MaxAgents.Value)
	assert.Equal(t, BrokerLimitSourceBroker, settingsResp.Effective.MaxAgents.Source)

	// Clear the override — the hub default applies again.
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	clearRec := doRequest(t, srv, http.MethodPut, settingsPath(brokerID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": nil},
		"expectedRevision": 1,
	})
	require.Equal(t, http.StatusOK, clearRec.Code, clearRec.Body.String())

	// Now the hub default (>1) allows another agent on the primary broker.
	rec4 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "settings-e2e-after-clear", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec4.Code, rec4.Body.String())

	afterClear := doRequest(t, srv, http.MethodGet, settingsPath(brokerID), nil)
	require.Equal(t, http.StatusOK, afterClear.Code, afterClear.Body.String())
	var afterClearResp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(afterClear.Body.Bytes(), &afterClearResp))
	require.NotNil(t, afterClearResp.Effective.MaxAgents.Value)
	assert.EqualValues(t, def.DefaultValue, *afterClearResp.Effective.MaxAgents.Value)
	assert.Equal(t, BrokerLimitSourceHubDefault, afterClearResp.Effective.MaxAgents.Source)
}
