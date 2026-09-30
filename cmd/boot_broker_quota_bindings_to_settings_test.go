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
// proves the real historical "user-subject hack" shape — a user binding
// whose subjectId IS the broker ID (the shape effectiveBrokerLimit's
// subjectID=brokerID resolution actually matched, pkg/hub/broker_capacity.go)
// — is migrated into a broker setting with the migration attribution.
func TestBrokerQuotaBindingsToSettingsMigration_UserHackBindingBecomesSetting(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	broker := createTestRuntimeBroker(t, ctx, s, "broker-1", nil)
	limitDef := seedMaxAgentsPerBrokerLimit(t, ctx, s, 100)
	// The real hack: subjectId equals the broker ID, which is what made
	// matchesScope/resolveEffectiveLimitWithSource pick this row up for
	// subjectID=brokerID lookups.
	binding := seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectUser, broker.ID, broker.ID, 5)

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

// TestBrokerQuotaBindingsToSettingsMigration_NeverEnforcedSubjectStillMigrated
// proves the migration deliberately takes a broker-scoped binding the
// entitlement engine never enforced — a system_default row with a
// non-empty subject (the exact ptone/scion#2063 item-3 silent no-op) — and
// migrates it anyway, per design §5.5's "any subject" instruction. This is
// a wider net than replicating the engine's own resolution would be.
func TestBrokerQuotaBindingsToSettingsMigration_NeverEnforcedSubjectStillMigrated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	broker := createTestRuntimeBroker(t, ctx, s, "broker-1", nil)
	limitDef := seedMaxAgentsPerBrokerLimit(t, ctx, s, 100)
	// system_default with a non-empty subject: the engine's system-default
	// collection only ever looks up subjectID="" (pkg/hub/quota.go), so this
	// row was never enforced pre-migration.
	seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectSystemDefault, "legacy-non-empty-subject", broker.ID, 7)

	runBrokerQuotaBindingsToSettingsMigration(ctx, s)

	rec, err := s.GetBrokerSettings(ctx, broker.ID)
	require.NoError(t, err)
	require.NotNil(t, rec.Settings.MaxAgents, "a binding the engine never enforced must still be migrated per design §5.5")
	assert.Equal(t, int64(7), *rec.Settings.MaxAgents)
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

// TestBrokerQuotaBindingsToSettingsMigration_NegativeSelection proves the
// migration only ever acts on scopeType=broker bindings for
// max_agents_per_broker: a system-scoped binding on the same limit is not
// migrated (no settings row appears for the empty scope ID), a
// project-scoped binding whose scopeId happens to equal a broker's ID is not
// migrated either (scopeType, not just a non-empty scopeId, gates
// selection), a broker-scoped binding on a DIFFERENT limit is invisible to
// this migration entirely (it is filtered out before any broker is ever
// looked at, since bindings are listed by this limit's ID), and a broker
// with no binding of its own gets no settings row just because some other
// broker did.
//
// The brokers_scanned=1/missing_broker=0 log assertions are load-bearing,
// not decorative: if the scopeType check were ever dropped from the
// grouping filter, the system-scoped binding's empty scopeId would land in
// its own "broker" bucket, GetRuntimeBroker("") would return ErrNotFound,
// and the pass would still complete "successfully" — just with
// brokers_scanned=2 and missing_broker=1 instead of 1/0. Asserting only
// GetBrokerSettings(ctx, "") returns ErrNotFound cannot tell that apart from
// the correct behaviour, because both paths leave no settings row at "".
// (Verified by mutation: see the round-2 review disposition in the project
// log for how this was confirmed against the real filter.)
func TestBrokerQuotaBindingsToSettingsMigration_NegativeSelection(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	targetBroker := createTestRuntimeBroker(t, ctx, s, "broker-target", nil)
	untouchedBroker := createTestRuntimeBroker(t, ctx, s, "broker-untouched", nil)

	limitDef := seedMaxAgentsPerBrokerLimit(t, ctx, s, 100)
	seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectUser, targetBroker.ID, targetBroker.ID, 5)

	// A system-scoped binding on the SAME limit must not produce a settings
	// row keyed by the empty scope ID, and must not affect targetBroker.
	_, err := s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: limitDef.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		SubjectID:         "",
		ScopeType:         store.QuotaScopeSystem,
		ScopeID:           "",
		Value:             999,
		CreatedBy:         "test",
	})
	require.NoError(t, err)

	// A project-scoped binding whose scopeId equals targetBroker's ID must
	// also be excluded: the filter is on scopeType=broker, not merely on a
	// non-empty scopeId. If the scopeType check were dropped, this row would
	// wrongly join targetBroker's bucket and (being the largest value) would
	// change its migrated result from 5 to 777.
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: limitDef.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         "some-user",
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           targetBroker.ID,
		Value:             777,
		CreatedBy:         "test",
	})
	require.NoError(t, err)

	// A broker-scoped binding on a DIFFERENT limit must be invisible to this
	// migration: it is filtered out by the ListEntitlementBindings(limitDef.ID)
	// call before any grouping happens, so it can't touch any broker's
	// max_agents_per_broker setting.
	otherLimit, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "some_other_broker_scoped_limit",
		ResourceType: "test",
		Unit:         "count",
		DefaultValue: 0,
	})
	require.NoError(t, err)
	seedBrokerBinding(t, ctx, s, otherLimit.ID, store.EntitlementSubjectUser, untouchedBroker.ID, untouchedBroker.ID, 42)

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerQuotaBindingsToSettingsMigration(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "brokers_scanned=1", "only targetBroker's real broker-scoped binding should ever form a group")
	assert.Contains(t, logOutput, "missing_broker=0", "neither the system- nor project-scoped binding should be mistaken for a broker-scoped one with a missing broker")

	rec, err := s.GetBrokerSettings(ctx, targetBroker.ID)
	require.NoError(t, err)
	require.NotNil(t, rec.Settings.MaxAgents)
	assert.Equal(t, int64(5), *rec.Settings.MaxAgents, "neither the system-scoped nor the project-scoped binding's value may leak into the broker-scoped result")

	_, err = s.GetBrokerSettings(ctx, untouchedBroker.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "a broker with no max_agents_per_broker binding of its own must get no settings row")

	_, err = s.GetBrokerSettings(ctx, "")
	assert.ErrorIs(t, err, store.ErrNotFound, "a system-scoped binding (empty scope ID) must never produce a settings row")
}

// TestBrokerQuotaBindingsToSettingsMigration_NilMaxAgentsCASUpdate proves the
// migration correctly performs a CAS update (not a create) when a broker
// already has a broker_settings row whose maxAgents field is nil — the
// shape left behind when P2.1 shipped and an admin set, then cleared, a
// cap (or wrote some other future settings key first). The migration must
// use the existing row's revision, not 0, or the write would fail with
// ErrRevisionConflict.
func TestBrokerQuotaBindingsToSettingsMigration_NilMaxAgentsCASUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	broker := createTestRuntimeBroker(t, ctx, s, "broker-1", nil)
	limitDef := seedMaxAgentsPerBrokerLimit(t, ctx, s, 100)
	seedBrokerBinding(t, ctx, s, limitDef.ID, store.EntitlementSubjectUser, broker.ID, broker.ID, 9)

	// Pre-create a settings row with maxAgents unset (revision 1), the shape
	// an admin leaves behind by setting then clearing the cap.
	created, err := s.PutBrokerSettings(ctx, broker.ID, store.BrokerSettings{}, 0, "admin@example.com")
	require.NoError(t, err)
	require.Nil(t, created.Settings.MaxAgents)
	require.EqualValues(t, 1, created.Revision)

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerQuotaBindingsToSettingsMigration(ctx, s)

	assert.Contains(t, buf.String(), "migrated=1", "a nil maxAgents on an existing row must still count as migrated, not already-set")

	rec, err := s.GetBrokerSettings(ctx, broker.ID)
	require.NoError(t, err)
	require.NotNil(t, rec.Settings.MaxAgents)
	assert.Equal(t, int64(9), *rec.Settings.MaxAgents)
	assert.EqualValues(t, 2, rec.Revision, "the write must be a CAS update (revision 1 -> 2), not a failed create")
	assert.Equal(t, migrationUpdatedBy, rec.UpdatedBy)
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
