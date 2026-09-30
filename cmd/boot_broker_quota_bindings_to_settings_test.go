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

package cmd

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedMaxAgentsPerBrokerLimit creates the max_agents_per_broker limit
// definition in the store, mirroring pkg/hub/seed.go's shape closely enough
// for this migration's purposes.
func seedMaxAgentsPerBrokerLimit(t *testing.T, ctx context.Context, s store.Store, defaultValue int64) *store.LimitDefinition {
	t.Helper()
	def, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         store.LimitMaxAgentsPerBroker,
		ResourceType: "agent",
		Unit:         "count",
		Description:  "test limit: " + store.LimitMaxAgentsPerBroker,
		DefaultValue: defaultValue,
		System:       true,
	})
	require.NoError(t, err)
	return def
}

// seedBrokerBinding creates a broker-scoped entitlement binding on limitDefID
// for brokerID, with the given subject shape (either the "user-subject hack"
// or a system_default row with a non-empty subject — both are ptone/scion#2063
// item 2/3 workarounds this migration must treat identically).
func seedBrokerBinding(t *testing.T, ctx context.Context, s store.Store, limitDefID, subjectType, subjectID, brokerID string, value int64) *store.EntitlementBinding {
	t.Helper()
	b, err := s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: limitDefID,
		SubjectType:       subjectType,
		SubjectID:         subjectID,
		ScopeType:         store.QuotaScopeBroker,
		ScopeID:           brokerID,
		Value:             value,
		CreatedBy:         "test",
	})
	require.NoError(t, err)
	return b
}

// TestBrokerQuotaBindingsToSettingsMigration_UserHackBindingBecomesSetting
// proves the historical "user-subject hack" shape (subjectType=user,
// scopeType=broker) is migrated into a broker setting with the migration
// attribution.
func TestBrokerQuotaBindingsToSettingsMigration_UserHackBindingBecomesSetting(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	broker := createTestRuntimeBroker(t, ctx, s, "broker-1", nil)
	limitDef := seedMaxAgentsPerBrokerLimit(t, ctx, s, 100)
	binding := seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectUser, "user-a", broker.ID, 5)

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerQuotaBindingsToSettingsMigration(ctx, s)

	assert.Contains(t, buf.String(), "migrated=1")

	rec, err := s.GetBrokerSettings(ctx, broker.ID)
	require.NoError(t, err)
	require.NotNil(t, rec.Settings.MaxAgents)
	assert.Equal(t, int64(5), *rec.Settings.MaxAgents)
	assert.Equal(t, migrationUpdatedBy, rec.UpdatedBy)

	// The binding must remain in place (nothing destructive), now shadowed.
	got, err := s.GetEntitlementBinding(ctx, binding.ID)
	require.NoError(t, err)
	assert.Equal(t, binding.ID, got.ID)
}

// TestBrokerQuotaBindingsToSettingsMigration_TwoBindingsGiveMax proves that
// when a broker has more than one matching binding (e.g. both hack shapes at
// once), the migrated value is the maximum, matching the entitlement
// engine's "most generous wins" merge rule.
func TestBrokerQuotaBindingsToSettingsMigration_TwoBindingsGiveMax(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	broker := createTestRuntimeBroker(t, ctx, s, "broker-1", nil)
	limitDef := seedMaxAgentsPerBrokerLimit(t, ctx, s, 100)
	seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectUser, "user-a", broker.ID, 5)
	seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectSystemDefault, "legacy-subject", broker.ID, 12)

	runBrokerQuotaBindingsToSettingsMigration(ctx, s)

	rec, err := s.GetBrokerSettings(ctx, broker.ID)
	require.NoError(t, err)
	require.NotNil(t, rec.Settings.MaxAgents)
	assert.Equal(t, int64(12), *rec.Settings.MaxAgents)
}

// TestBrokerQuotaBindingsToSettingsMigration_AnyZeroGivesZero proves that if
// any matching binding grants unlimited (value 0), the migrated setting is 0
// (unlimited), even when another binding has a larger finite value — 0 is
// the most generous possible grant.
func TestBrokerQuotaBindingsToSettingsMigration_AnyZeroGivesZero(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	broker := createTestRuntimeBroker(t, ctx, s, "broker-1", nil)
	limitDef := seedMaxAgentsPerBrokerLimit(t, ctx, s, 100)
	seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectUser, "user-a", broker.ID, 30)
	seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectUser, "user-b", broker.ID, 0)

	runBrokerQuotaBindingsToSettingsMigration(ctx, s)

	rec, err := s.GetBrokerSettings(ctx, broker.ID)
	require.NoError(t, err)
	require.NotNil(t, rec.Settings.MaxAgents)
	assert.Equal(t, int64(0), *rec.Settings.MaxAgents)
}

// TestBrokerQuotaBindingsToSettingsMigration_ExistingSettingUntouched proves
// a broker that already has a maxAgents setting is left alone, even though
// it also has a shadowed binding that would suggest a different value.
func TestBrokerQuotaBindingsToSettingsMigration_ExistingSettingUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	broker := createTestRuntimeBroker(t, ctx, s, "broker-1", nil)
	limitDef := seedMaxAgentsPerBrokerLimit(t, ctx, s, 100)
	seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectUser, "user-a", broker.ID, 5)

	existing := int64(42)
	_, err := s.PutBrokerSettings(ctx, broker.ID, store.BrokerSettings{MaxAgents: &existing}, 0, "admin@example.com")
	require.NoError(t, err)

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerQuotaBindingsToSettingsMigration(ctx, s)

	assert.Contains(t, buf.String(), "already_set=1")
	assert.Contains(t, buf.String(), "migrated=0")

	rec, err := s.GetBrokerSettings(ctx, broker.ID)
	require.NoError(t, err)
	require.NotNil(t, rec.Settings.MaxAgents)
	assert.Equal(t, int64(42), *rec.Settings.MaxAgents, "existing setting must not be overwritten")
	assert.Equal(t, "admin@example.com", rec.UpdatedBy)
}

// TestBrokerQuotaBindingsToSettingsMigration_Idempotent confirms a second
// boot is a no-op: the migration marker short-circuits the pass entirely.
func TestBrokerQuotaBindingsToSettingsMigration_Idempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	broker := createTestRuntimeBroker(t, ctx, s, "broker-1", nil)
	limitDef := seedMaxAgentsPerBrokerLimit(t, ctx, s, 100)
	seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectUser, "user-a", broker.ID, 5)

	runBrokerQuotaBindingsToSettingsMigration(ctx, s)

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerQuotaBindingsToSettingsMigration(ctx, s)

	assert.Contains(t, buf.String(), "already complete, skipping")

	done, err := IsMigrationComplete(ctx, s, MigrationBrokerQuotaBindingsToSettings)
	require.NoError(t, err)
	assert.True(t, done)
}

// TestBrokerQuotaBindingsToSettingsMigration_MissingBrokerSkipped proves a
// binding whose broker no longer exists is skipped (no settings row is
// created for a nonexistent broker), logged, and still lets the pass
// complete (a missing broker is a permanent, deterministic outcome, not a
// run-level failure).
func TestBrokerQuotaBindingsToSettingsMigration_MissingBrokerSkipped(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	limitDef := seedMaxAgentsPerBrokerLimit(t, ctx, s, 100)
	seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectUser, "user-a", "nonexistent-broker", 5)

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerQuotaBindingsToSettingsMigration(ctx, s)

	assert.Contains(t, buf.String(), "missing_broker=1")
	assert.Contains(t, buf.String(), "broker no longer exists")

	_, err := s.GetBrokerSettings(ctx, "nonexistent-broker")
	assert.ErrorIs(t, err, store.ErrNotFound)

	done, err := IsMigrationComplete(ctx, s, MigrationBrokerQuotaBindingsToSettings)
	require.NoError(t, err)
	assert.True(t, done, "a missing broker is a permanent outcome and must not block completion")
}

// TestBrokerQuotaBindingsToSettingsMigration_NoLimitDefined confirms the
// migration completes cleanly (and does not panic or error) on a hub that
// has never seeded the max_agents_per_broker limit at all.
func TestBrokerQuotaBindingsToSettingsMigration_NoLimitDefined(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	runBrokerQuotaBindingsToSettingsMigration(ctx, s)

	done, err := IsMigrationComplete(ctx, s, MigrationBrokerQuotaBindingsToSettings)
	require.NoError(t, err)
	assert.True(t, done)
}
