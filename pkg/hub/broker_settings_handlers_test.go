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
	"strings"
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

	// With no override in play, Inherited must agree with Effective (review
	// round 3, F1): both resolve to the hub-wide default.
	require.NotNil(t, resp.Effective.MaxAgents.Inherited.Value)
	assert.EqualValues(t, def.DefaultValue, *resp.Effective.MaxAgents.Inherited.Value)
	assert.Equal(t, BrokerLimitSourceHubDefault, resp.Effective.MaxAgents.Inherited.Source)
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
// PUT: a broker owner cannot clear an admin-set cap by omission (review
// round 1, F1 — critical authz bypass). PUT is a full replace (design.md
// §5.4: absent/null = unset), so the permission check must run over the keys
// that *change*, not just the keys present in the request. Each variant here
// must be forbidden and must leave the stored value untouched.
// =============================================================================

func TestBrokerSettings_Put_OwnerCannotClearViaOmission(t *testing.T) {
	srv, s := testServer(t)
	owner := newPlainUser(t, s, "clear-omission-owner")
	broker := newBrokerSettingsTestBroker(t, s, "clear-omission", owner.ID)

	// An admin sets the cap first.
	adminRec := doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 3},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, adminRec.Code, adminRec.Body.String())

	assertUnchanged := func(t *testing.T) {
		t.Helper()
		rec, err := s.GetBrokerSettings(context.Background(), broker.ID)
		require.NoError(t, err)
		require.NotNil(t, rec.Settings.MaxAgents)
		assert.EqualValues(t, 3, *rec.Settings.MaxAgents, "the admin-set cap must survive a denied owner PUT")
		assert.EqualValues(t, 1, rec.Revision, "a denied PUT must not bump the revision")
	}

	t.Run("empty settings object", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, owner, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
			"settings":         map[string]interface{}{},
			"expectedRevision": 1,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertUnchanged(t)
	})

	t.Run("settings key omitted entirely", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, owner, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
			"expectedRevision": 1,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertUnchanged(t)
	})

	t.Run("explicit null", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, owner, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
			"settings":         map[string]interface{}{"maxAgents": nil},
			"expectedRevision": 1,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertUnchanged(t)
	})

	// Re-sending the identical current value is a no-op for the stored
	// value, so it needs no permission: this is the "either allowed or 403,
	// pick one and test it" case from review round 1. It must also be a
	// true no-op on an *existing* row (review round 3, F4/C1 coverage): no
	// revision bump and no updatedBy rewrite, which would otherwise
	// misattribute the admin-set value to the owner who merely re-sent it.
	t.Run("identical value is allowed without permission", func(t *testing.T) {
		before, err := s.GetBrokerSettings(context.Background(), broker.ID)
		require.NoError(t, err)

		rec := doRequestAsUser(t, srv, owner, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
			"settings":         map[string]interface{}{"maxAgents": 3},
			"expectedRevision": 1,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err := s.GetBrokerSettings(context.Background(), broker.ID)
		require.NoError(t, err)
		require.NotNil(t, got.Settings.MaxAgents)
		assert.EqualValues(t, 3, *got.Settings.MaxAgents)
		assert.Equal(t, before.Revision, got.Revision, "a no-op PUT must not bump the revision")
		assert.Equal(t, before.UpdatedBy, got.UpdatedBy,
			"a no-op PUT must not overwrite updatedBy with the re-sending caller's identity")
	})
}

// =============================================================================
// PUT: expectedRevision must match the freshly-read current revision before
// authorization runs, not just at the eventual CAS (review round 2, R1). A
// caller declaring a revision that doesn't match what the handler just read
// gets 409 immediately, so the diff-based permission decision is always made
// against the exact document the write would replace.
// =============================================================================

// TestBrokerSettings_Put_ExpectedRevisionMismatchIsConflictNotBypass is R1's
// simple case: an owner declares a revision one ahead of the true current
// revision (as if speculating about a write that hasn't happened yet). This
// must be a plain 409, not 200 (silently accepted) or 403 (which would imply
// the handler evaluated permission against the wrong document).
func TestBrokerSettings_Put_ExpectedRevisionMismatchIsConflictNotBypass(t *testing.T) {
	srv, s := testServer(t)
	owner := newPlainUser(t, s, "revision-mismatch-owner")
	broker := newBrokerSettingsTestBroker(t, s, "revision-mismatch", owner.ID)

	adminRec := doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 1},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, adminRec.Code, adminRec.Body.String())

	rec := doRequestAsUser(t, srv, owner, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{},
		"expectedRevision": 2, // true current revision is 1
	})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	got, err := s.GetBrokerSettings(context.Background(), broker.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Settings.MaxAgents)
	assert.EqualValues(t, 1, *got.Settings.MaxAgents, "a revision-mismatched request must never apply")
	assert.EqualValues(t, 1, got.Revision)
}

// racingBrokerSettingsPutStore triggers racer exactly once, the first time
// PutBrokerSettings is called, before delegating to the real store. Used to
// simulate a concurrent admin write landing between the handler's
// GetBrokerSettings snapshot read and its own PutBrokerSettings call — the
// narrow window R1's fix leaves deliberately open, guarded instead by the
// store's own CAS.
type racingBrokerSettingsPutStore struct {
	store.Store
	fault *storeFaultSwitch // nil: always active
	racer func()
	fired bool
}

func (r *racingBrokerSettingsPutStore) PutBrokerSettings(ctx context.Context, brokerID string, settings store.BrokerSettings, expectedRevision int64, updatedBy string) (*store.BrokerSettingsRecord, error) {
	if r.fault.Active() && !r.fired {
		r.fired = true
		r.racer()
	}
	return r.Store.PutBrokerSettings(ctx, brokerID, settings, expectedRevision, updatedBy)
}

// TestBrokerSettings_Put_ConcurrentWriteBetweenReadAndCASIsConflictNotBypass
// is R1's deterministic race test for the narrow window the fix
// deliberately leaves open: two *authorized* writers (C1 means an
// unauthorized, no-op request never reaches the store at all, so it can no
// longer race here — see TestBrokerSettings_Put_NoOpOnFreshBrokerCreatesNoRow
// and the permission-diff tests instead). Between the handler's own
// snapshot read and its PutBrokerSettings call, a concurrent admin write
// changes the value and bumps the revision. The first writer's now-stale
// CAS must fail with a plain 409 — never silently overwrite the concurrent
// write.
func TestBrokerSettings_Put_ConcurrentWriteBetweenReadAndCASIsConflictNotBypass(t *testing.T) {
	// Installed before the first PUT, whose mutation audit goroutine reads
	// srv.store (ptone/scion#3184); armed for the second PUT.
	srv, s, racing, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *racingBrokerSettingsPutStore {
		return &racingBrokerSettingsPutStore{Store: inner, fault: f}
	})
	broker := newBrokerSettingsTestBroker(t, s, "race", "")

	adminRec := doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 1},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, adminRec.Code, adminRec.Body.String())

	racing.racer = func() {
		_, err := s.PutBrokerSettings(context.Background(), broker.ID,
			store.BrokerSettings{MaxAgents: int64ptrForTest(5)}, 1, "admin-concurrent")
		require.NoError(t, err)
	}
	fault.Arm()

	// This request is itself authorized (dev/admin token) and really does
	// change the value (1 -> 10), so it reaches the actual PutBrokerSettings
	// call — where the injected concurrent write has already landed.
	rec := doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 10},
		"expectedRevision": 1, // stale by the time the CAS actually runs
	})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	got, err := s.GetBrokerSettings(context.Background(), broker.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Settings.MaxAgents)
	assert.EqualValues(t, 5, *got.Settings.MaxAgents, "the concurrent write must survive; the stale write must never apply")
}

func int64ptrForTest(v int64) *int64 { return &v }

// =============================================================================
// PUT: a no-op write (nothing changes) skips the store write entirely
// (review round 2, C1) — no revision bump, no updatedBy rewrite, no audit
// event, and critically no empty row created for a broker that never had
// one, which would otherwise misattribute a settings row to whoever merely
// re-sent an unset/empty document.
// =============================================================================

func TestBrokerSettings_Put_NoOpOnFreshBrokerCreatesNoRow(t *testing.T) {
	srv, s := testServer(t)
	owner := newPlainUser(t, s, "noop-owner")
	broker := newBrokerSettingsTestBroker(t, s, "noop-fresh", owner.ID)

	rec := doRequestAsUser(t, srv, owner, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, int64(0), resp.Revision, "a no-op PUT must not create a row")

	_, err := s.GetBrokerSettings(context.Background(), broker.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "a no-op PUT on a fresh broker must not create an empty row")
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
// PUT/GET must key settings off the canonical broker ID GetRuntimeBroker
// resolves to, not the raw path segment (review round 1, F2): GetRuntimeBroker's
// UUID parsing accepts uppercase/braced forms, and a naive handler that keys
// off the raw segment would write/read a different row than the one Reserve
// and the providers listing use.
// =============================================================================

func TestBrokerSettings_Put_NonCanonicalBrokerIDUsesCanonicalKey(t *testing.T) {
	srv, s := testServer(t)
	broker := newBrokerSettingsTestBroker(t, s, "non-canonical", "")
	uppercasePath := settingsPath(strings.ToUpper(broker.ID))

	putRec := doRequest(t, srv, http.MethodPut, uppercasePath, map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 1},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	// The row must be stored under the canonical (lowercase) ID: a GET on
	// the canonical path must see it...
	getRec := doRequest(t, srv, http.MethodGet, settingsPath(broker.ID), nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var resp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Settings.MaxAgents, "the setting must be readable from the canonical broker ID")
	assert.EqualValues(t, 1, *resp.Settings.MaxAgents)

	// ...and the store itself must have exactly one row, keyed by the
	// canonical ID.
	rec, err := s.GetBrokerSettings(context.Background(), broker.ID)
	require.NoError(t, err)
	require.NotNil(t, rec.Settings.MaxAgents)
	assert.EqualValues(t, 1, *rec.Settings.MaxAgents)

	// effectiveBrokerLimit (and therefore Reserve) must see the same row.
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	value, source, err := srv.effectiveBrokerLimit(context.Background(), broker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 1, value)
	assert.Equal(t, BrokerLimitSourceBroker, source)
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
// AC-P2-8: clearing the override falls back to the global default, with
// source hub_default, on both the settings GET and the providers response
// (review round 2, R4 — this leg was lost when the end-to-end test below
// grew a system-scoped binding for F6/AC-P2-2, which makes *its* "clear"
// leg fall back to that binding instead). No binding here, so this is the
// plain hub_default path.
// =============================================================================

func TestBrokerSettings_ClearOverrideFallsBackToHubDefault(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	brokerID := project.DefaultRuntimeBrokerID

	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	setRec := doRequest(t, srv, http.MethodPut, settingsPath(brokerID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 1},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, setRec.Code, setRec.Body.String())

	// While the override is active, Inherited must still report what
	// clearing it would produce — the hub-wide default, not the override
	// itself (review round 3, F1; design.md §5.6, R2).
	var setResp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(setRec.Body.Bytes(), &setResp))
	require.NotNil(t, setResp.Effective.MaxAgents.Value)
	assert.EqualValues(t, 1, *setResp.Effective.MaxAgents.Value)
	assert.Equal(t, BrokerLimitSourceBroker, setResp.Effective.MaxAgents.Source)
	require.NotNil(t, setResp.Effective.MaxAgents.Inherited.Value)
	assert.EqualValues(t, def.DefaultValue, *setResp.Effective.MaxAgents.Inherited.Value)
	assert.Equal(t, BrokerLimitSourceHubDefault, setResp.Effective.MaxAgents.Inherited.Source)

	clearRec := doRequest(t, srv, http.MethodPut, settingsPath(brokerID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": nil},
		"expectedRevision": 1,
	})
	require.Equal(t, http.StatusOK, clearRec.Code, clearRec.Body.String())

	settingsRec := doRequest(t, srv, http.MethodGet, settingsPath(brokerID), nil)
	require.Equal(t, http.StatusOK, settingsRec.Code, settingsRec.Body.String())
	var settingsResp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(settingsRec.Body.Bytes(), &settingsResp))
	require.NotNil(t, settingsResp.Effective.MaxAgents.Value)
	assert.EqualValues(t, def.DefaultValue, *settingsResp.Effective.MaxAgents.Value)
	assert.Equal(t, BrokerLimitSourceHubDefault, settingsResp.Effective.MaxAgents.Source)
	require.NotNil(t, settingsResp.Effective.MaxAgents.Inherited.Value)
	assert.EqualValues(t, def.DefaultValue, *settingsResp.Effective.MaxAgents.Inherited.Value)
	assert.Equal(t, BrokerLimitSourceHubDefault, settingsResp.Effective.MaxAgents.Inherited.Source)

	providersRec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/providers", nil)
	require.Equal(t, http.StatusOK, providersRec.Code, providersRec.Body.String())
	var providersResp struct {
		Providers []providerCapacityView `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(providersRec.Body.Bytes(), &providersResp))
	require.Len(t, providersResp.Providers, 1)
	require.NotNil(t, providersResp.Providers[0].AgentLimit)
	assert.EqualValues(t, def.DefaultValue, *providersResp.Providers[0].AgentLimit)
	assert.Equal(t, BrokerLimitSourceHubDefault, providersResp.Providers[0].AgentLimitSource)

	// The hub default (which the seeded default comfortably exceeds 1 of)
	// admits another agent on the now-uncapped broker.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "clear-fallback-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// =============================================================================
// End to end through HTTP (design.md §5.7): set maxAgents=1 -> the 2nd
// create on that broker returns 429 even below a system-scoped entitlement
// binding (AC-P2-2); clear it -> the entitlement binding applies (see
// TestBrokerSettings_ClearOverrideFallsBackToHubDefault for the plain
// hub_default fallback, with no binding in play); another broker is
// unaffected. The providers endpoint and settings GET agree with what
// Reserve enforces (AC-P2-10).
// =============================================================================

func TestBrokerSettings_EndToEndEnforcement(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	brokerID := project.DefaultRuntimeBrokerID

	otherBroker := newTestBroker(t, s, "unaffected")
	otherProject := addProjectOnBroker(t, s, "unaffected-project", otherBroker)

	// AC-P2-2 / review round 1 F6: a system-scoped entitlement binding of 30
	// would, on its own, let this broker run up to 30 agents (most generous
	// wins over the hub default). The per-broker override must still win at
	// Reserve, not just on the read side.
	limitDef, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	seedBinding(t, s, limitDef.ID, store.EntitlementSubjectSystemDefault, "", store.QuotaScopeSystem, "", 30)

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
	assert.Equal(t, BrokerLimitSourceBroker, providersResp.Providers[0].AgentLimitSource,
		"the providers response must report the broker override, not the 30-value system binding, as the source")

	settingsRec := doRequest(t, srv, http.MethodGet, settingsPath(brokerID), nil)
	require.Equal(t, http.StatusOK, settingsRec.Code, settingsRec.Body.String())
	var settingsResp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(settingsRec.Body.Bytes(), &settingsResp))
	require.NotNil(t, settingsResp.Effective.MaxAgents.Value)
	assert.EqualValues(t, 1, *settingsResp.Effective.MaxAgents.Value)
	assert.Equal(t, BrokerLimitSourceBroker, settingsResp.Effective.MaxAgents.Source)
	require.NotNil(t, settingsResp.Effective.MaxAgents.Count,
		"the settings response must include the live count (review round 1, F3), not leave the detail page to compute its own")
	assert.EqualValues(t, 1, *settingsResp.Effective.MaxAgents.Count)
	// Inherited must report the entitlement binding (30) — what clearing the
	// override would fall back to — not the hub-wide default, since a
	// matching system-scoped binding beats it (review round 3, F1).
	require.NotNil(t, settingsResp.Effective.MaxAgents.Inherited.Value)
	assert.EqualValues(t, 30, *settingsResp.Effective.MaxAgents.Inherited.Value)
	assert.Equal(t, BrokerLimitSourceEntitlement, settingsResp.Effective.MaxAgents.Inherited.Source)

	// Clear the override — the entitlement engine applies again. A
	// system-scoped binding (30) is in effect, so the fallback is that
	// binding, not the hub-wide default (existing ResolveEffectiveLimit
	// "most generous wins" behaviour, unaffected by broker settings).
	clearRec := doRequest(t, srv, http.MethodPut, settingsPath(brokerID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": nil},
		"expectedRevision": 1,
	})
	require.Equal(t, http.StatusOK, clearRec.Code, clearRec.Body.String())

	// Now the entitlement binding (30, well above 1) allows another agent on
	// the primary broker.
	rec4 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "settings-e2e-after-clear", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec4.Code, rec4.Body.String())

	afterClear := doRequest(t, srv, http.MethodGet, settingsPath(brokerID), nil)
	require.Equal(t, http.StatusOK, afterClear.Code, afterClear.Body.String())
	var afterClearResp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(afterClear.Body.Bytes(), &afterClearResp))
	require.NotNil(t, afterClearResp.Effective.MaxAgents.Value)
	assert.EqualValues(t, 30, *afterClearResp.Effective.MaxAgents.Value)
	assert.Equal(t, BrokerLimitSourceEntitlement, afterClearResp.Effective.MaxAgents.Source)

	afterClearProvidersRec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/providers", nil)
	require.Equal(t, http.StatusOK, afterClearProvidersRec.Code, afterClearProvidersRec.Body.String())
	var afterClearProvidersResp struct {
		Providers []providerCapacityView `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(afterClearProvidersRec.Body.Bytes(), &afterClearProvidersResp))
	require.Len(t, afterClearProvidersResp.Providers, 1)
	assert.Equal(t, BrokerLimitSourceEntitlement, afterClearProvidersResp.Providers[0].AgentLimitSource)
}

// =============================================================================
// Amendment A1 (design.md, 2026-09-30): with the P1b enforcement switch off,
// every read path reports the RESOLVED value with source "not_enforced" —
// not "unlimited", not the real precedence step. AC-P2-6.
// =============================================================================

// TestEffectiveBrokerLimit_NotEnforced_KeepsValueChangesSourceOnly covers
// effectiveBrokerLimit directly across all four precedence outcomes (broker
// override, entitlement binding, hub default, and a resolved-zero/unlimited
// cap): with the switch off, the value is unchanged but the source becomes
// not_enforced in every case — including brokerCapacity itself reporting
// Limit == nil for the zero/unlimited case, exactly as it would if the
// source were "unlimited" — and flipping the switch back on restores the
// real source with no other change (AC-P2-6's "switch on: sources
// unchanged" leg). inheritedBrokerLimit (the "clear the override" preview)
// must keep reporting its real step throughout — Amendment A1 explicitly
// carves it out.
func TestEffectiveBrokerLimit_NotEnforced_KeepsValueChangesSourceOnly(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newBrokerSettingsTestBroker(t, s, "not-enforced-precedence", "")

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	setBrokerAgentCeiling(t, s, 16)
	// Scoped to broker specifically (not system-wide), so it doesn't also
	// apply to hubDefaultBroker/entitlementBroker below — this test only
	// needs one clean example of each precedence step, unlike
	// TestEffectiveBrokerLimit_Precedence, which deliberately uses a
	// system-scoped binding to prove a broker override beats even that
	// (AC-P2-2).
	seedBinding(t, s, def.ID, store.EntitlementSubjectSystemDefault, "", store.QuotaScopeBroker, broker.ID, 30)
	def, err = s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	// A broker with neither a binding nor an override resolves through to
	// the hub_default step on its own.
	hubDefaultBroker := newBrokerSettingsTestBroker(t, s, "not-enforced-hubdefault", "")

	// A separate broker carries only the entitlement binding (30), with no
	// override, so it stays on the entitlement step throughout — `broker`
	// above cannot be reused for this, since it is given a maxAgents
	// override later on and would no longer exercise the entitlement step
	// once the switch-off section runs.
	entitlementBroker := newBrokerSettingsTestBroker(t, s, "not-enforced-entitlement", "")
	seedBinding(t, s, def.ID, store.EntitlementSubjectSystemDefault, "", store.QuotaScopeBroker, entitlementBroker.ID, 30)

	// A broker overridden to 0 resolves to a value of 0 (unlimited) via the
	// broker-override step — the resolved-zero/unlimited case.
	zeroBroker := newBrokerSettingsTestBroker(t, s, "not-enforced-zero", "")
	zeroRec := doRequest(t, srv, http.MethodPut, settingsPath(zeroBroker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 0},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, zeroRec.Code, zeroRec.Body.String())

	// --- Switch on (default): every step reports its real source. ---
	require.Nil(t, srv.config.EnforceBrokerQuotas, "switch must default to unset (enforced)")

	value, source, err := srv.effectiveBrokerLimit(ctx, hubDefaultBroker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 16, value)
	assert.Equal(t, BrokerLimitSourceHubDefault, source)

	value, source, err = srv.effectiveBrokerLimit(ctx, entitlementBroker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 30, value)
	assert.Equal(t, BrokerLimitSourceEntitlement, source)

	value, source, err = srv.effectiveBrokerLimit(ctx, zeroBroker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 0, value)
	assert.Equal(t, BrokerLimitSourceBroker, source)

	value, source, err = srv.effectiveBrokerLimit(ctx, broker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 30, value)
	assert.Equal(t, BrokerLimitSourceEntitlement, source)

	rec := doRequest(t, srv, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 3},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	value, source, err = srv.effectiveBrokerLimit(ctx, broker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 3, value)
	assert.Equal(t, BrokerLimitSourceBroker, source)

	// The inherited preview (what clearing the override would produce) must
	// report the real entitlement step regardless of the switch.
	inheritedValue, inheritedSource, err := srv.inheritedBrokerLimit(ctx, broker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 30, inheritedValue)
	assert.Equal(t, BrokerLimitSourceEntitlement, inheritedSource)

	// --- Switch off: same values, source becomes not_enforced everywhere. ---
	srv.config.EnforceBrokerQuotas = boolPtr(false)

	value, source, err = srv.effectiveBrokerLimit(ctx, hubDefaultBroker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 16, value, "hub_default value must be unchanged while off")
	assert.Equal(t, BrokerLimitSourceNotEnforced, source)

	value, source, err = srv.effectiveBrokerLimit(ctx, entitlementBroker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 30, value, "entitlement value must be unchanged while off")
	assert.Equal(t, BrokerLimitSourceNotEnforced, source)

	value, source, err = srv.effectiveBrokerLimit(ctx, broker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 3, value, "broker-override value must be unchanged while off")
	assert.Equal(t, BrokerLimitSourceNotEnforced, source)

	value, source, err = srv.effectiveBrokerLimit(ctx, zeroBroker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 0, value, "a resolved-zero (unlimited) cap must still be reported as not_enforced, not silently promoted to plain unlimited")
	assert.Equal(t, BrokerLimitSourceNotEnforced, source)

	// brokerCapacity must still treat the resolved value of 0 as unlimited
	// (Limit == nil) while reporting the not_enforced source — the same
	// convention it uses when the real step is "unlimited".
	zeroCapacity := srv.brokerCapacity(ctx, zeroBroker.ID, def)
	assert.Nil(t, zeroCapacity.Limit, "a resolved value of 0 must still mean unlimited (nil Limit) in the read model")
	assert.Equal(t, BrokerLimitSourceNotEnforced, zeroCapacity.Source)

	// The inherited preview is unaffected by the switch (Amendment A1: "keeps
	// reporting its real step, not not_enforced").
	inheritedValue, inheritedSource, err = srv.inheritedBrokerLimit(ctx, broker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 30, inheritedValue)
	assert.Equal(t, BrokerLimitSourceEntitlement, inheritedSource)

	// --- Switch back on: real sources return, no rebuild needed. ---
	srv.config.EnforceBrokerQuotas = boolPtr(true)
	value, source, err = srv.effectiveBrokerLimit(ctx, broker.ID, def)
	require.NoError(t, err)
	assert.EqualValues(t, 3, value)
	assert.Equal(t, BrokerLimitSourceBroker, source)
}

// TestEffectiveBrokerLimit_NotEnforced_NilLimitDefStaysUnlimited pins
// Amendment A1's explicit carve-out: when limitDef or the quota service is
// nil, the result stays "unlimited" regardless of the switch — the switch
// only relabels a resolved value, it does not manufacture one.
func TestEffectiveBrokerLimit_NotEnforced_NilLimitDefStaysUnlimited(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newBrokerSettingsTestBroker(t, s, "not-enforced-nil-limitdef", "")

	srv.config.EnforceBrokerQuotas = boolPtr(false)

	value, source, err := srv.effectiveBrokerLimit(ctx, broker.ID, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 0, value)
	assert.Equal(t, BrokerLimitSourceUnlimited, source)
}

// TestBrokerSettings_Get_NotEnforced covers the settings GET surface (design
// Amendment A1): with the switch off, Effective.MaxAgents.Source is
// not_enforced, Value/Count are retained (not dropped), and Inherited still
// reports its real step.
func TestBrokerSettings_Get_NotEnforced(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	brokerID := project.DefaultRuntimeBrokerID

	putRec := doRequest(t, srv, http.MethodPut, settingsPath(brokerID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 1},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "not-enforced-get-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	srv.config.EnforceBrokerQuotas = boolPtr(false)

	getRec := doRequest(t, srv, http.MethodGet, settingsPath(brokerID), nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var resp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &resp))

	require.NotNil(t, resp.Effective.MaxAgents.Value)
	assert.EqualValues(t, 1, *resp.Effective.MaxAgents.Value, "the resolved value is kept, not dropped")
	assert.Equal(t, BrokerLimitSourceNotEnforced, resp.Effective.MaxAgents.Source)
	require.NotNil(t, resp.Effective.MaxAgents.Count)
	assert.EqualValues(t, 1, *resp.Effective.MaxAgents.Count, "usage is still counted while off")

	// Inherited (what clearing the override would produce) is unaffected.
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	require.NotNil(t, resp.Effective.MaxAgents.Inherited.Value)
	assert.EqualValues(t, def.DefaultValue, *resp.Effective.MaxAgents.Inherited.Value)
	assert.Equal(t, BrokerLimitSourceHubDefault, resp.Effective.MaxAgents.Inherited.Source)
}

// TestListProjectProviders_NotEnforced covers the providers-listing surface
// (design Amendment A1): agentLimit is retained and agentLimitSource is
// not_enforced with the switch off.
func TestListProjectProviders_NotEnforced(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)
	brokerID := project.DefaultRuntimeBrokerID

	putRec := doRequest(t, srv, http.MethodPut, settingsPath(brokerID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 30},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	srv.config.EnforceBrokerQuotas = boolPtr(false)

	providersRec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/providers", nil)
	require.Equal(t, http.StatusOK, providersRec.Code, providersRec.Body.String())
	var providersResp struct {
		Providers []providerCapacityView `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(providersRec.Body.Bytes(), &providersResp))
	require.Len(t, providersResp.Providers, 1)
	require.NotNil(t, providersResp.Providers[0].AgentLimit)
	assert.EqualValues(t, 30, *providersResp.Providers[0].AgentLimit, "agentLimit is retained (informational), not dropped")
	assert.Equal(t, BrokerLimitSourceNotEnforced, providersResp.Providers[0].AgentLimitSource)
}

// TestBrokerQuotaSwitch_OffAllowsOverCapWithBrokerSettingOverride is
// AC-P2-6's Reserve leg, specifically for a per-broker settings override
// (design.md §5.2) rather than the hub-wide default P1b's own tests cover
// (TestBrokerQuotaSwitch_OffAllowsOverCapAndCounts uses setBrokerAgentCeiling,
// the hub_default step): with the switch off, a broker at its per-broker cap
// can still Reserve (created=true) and the count keeps incrementing.
// Reserve/limitOverride are unchanged by Amendment A1 — this only confirms
// that fact holds when the effective limit comes from a broker override.
func TestBrokerQuotaSwitch_OffAllowsOverCapWithBrokerSettingOverride(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	brokerID := project.DefaultRuntimeBrokerID

	putRec := doRequest(t, srv, http.MethodPut, settingsPath(brokerID), map[string]interface{}{
		"settings":         map[string]interface{}{"maxAgents": 1},
		"expectedRevision": 0,
	})
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	srv.config.EnforceBrokerQuotas = boolPtr(false)

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "not-enforced-override-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
	assert.EqualValues(t, 1, brokerReservationCount(t, s, brokerID))

	// At the per-broker cap (1), but enforcement is off: the create must
	// still succeed, and still create a reservation.
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "not-enforced-override-2", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
	assert.EqualValues(t, 2, brokerReservationCount(t, s, brokerID),
		"a reservation row must exist for the over-cap create even though the cap came from a broker override, not the hub default")
}
