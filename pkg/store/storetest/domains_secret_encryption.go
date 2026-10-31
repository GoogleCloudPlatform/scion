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

package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Secret encryption conformance (ptone/scion#1530 §6): the payload scan,
// the ciphertext-only compare-and-swap, the policy and migration records,
// and the policy fence on every payload-changing write. The values are
// shaped envelopes over fixed filler bytes; the store never decrypts, so no
// key is involved.
//
// Each catalogued store mutation is called through exactly one wrapper
// below, so the authorization catalog classifies one call site per symbol.

func encCreate(ctx context.Context, s store.SecretStore, sec *store.Secret) error {
	return s.CreateSecret(ctx, sec)
}

func encUpdate(ctx context.Context, s store.SecretStore, sec *store.Secret) error {
	return s.UpdateSecret(ctx, sec)
}

func encUpsert(ctx context.Context, s store.SecretStore, sec *store.Secret) (bool, error) {
	return s.UpsertSecret(ctx, sec)
}

func encValueIfVersion(ctx context.Context, s store.SecretStore, key, scope, scopeID string, version int, value string) (bool, error) {
	return s.UpdateSecretValueIfVersion(ctx, key, scope, scopeID, version, value)
}

func encDelete(ctx context.Context, s store.SecretStore, key, scope, scopeID string) error {
	return s.DeleteSecret(ctx, key, scope, scopeID)
}

func encScan(ctx context.Context, s store.SecretStore, afterID string, limit int) ([]store.SecretPayload, error) {
	return s.ScanSecretPayloads(ctx, afterID, limit)
}

func encReplace(ctx context.Context, s store.SecretStore, id string, version int, expected, replacement string) (bool, error) {
	return s.ReplaceSecretCiphertext(ctx, id, version, expected, replacement)
}

func encSetPolicy(ctx context.Context, s store.SecretEncryptionStore, expected int64, next store.EncryptionPolicy) (bool, error) {
	return s.UpdateEncryptionPolicy(ctx, expected, next)
}

func encCreateMigration(ctx context.Context, s store.SecretEncryptionStore, m *store.EncryptionMigration) error {
	return s.CreateEncryptionMigration(ctx, m)
}

func encUpdateMigration(ctx context.Context, s store.SecretEncryptionStore, m *store.EncryptionMigration, expected store.EncryptionMigrationState) (bool, error) {
	return s.UpdateEncryptionMigration(ctx, m, expected)
}

// fakeSealed is filler standing in for nonce||ciphertext||tag.
func fakeSealed(fill byte) []byte {
	return bytes.Repeat([]byte{fill}, secretenvelope.NonceSize+secretenvelope.TagSize+8)
}

func v1Value(fill byte) string { return secretenvelope.FormatV1Value(fakeSealed(fill)) }
func v2Value(kid string, fill byte) string {
	return secretenvelope.FormatV2Value(kid, fakeSealed(fill))
}

func encSecret(key, scope, scopeID, value, ref string) *store.Secret {
	return &store.Secret{
		ID:             uuid.NewString(),
		Key:            key,
		EncryptedValue: value,
		SecretRef:      ref,
		SecretType:     store.SecretTypeEnvironment,
		Target:         key,
		Scope:          scope,
		ScopeID:        scopeID,
		Description:    "conformance " + key,
		InjectionMode:  store.InjectionModeAsNeeded,
		CreatedBy:      "conformance-creator",
		UpdatedBy:      "conformance-updater",
	}
}

func fencePolicy(gen int64, writers ...string) store.EncryptionPolicy {
	return store.EncryptionPolicy{
		Generation:       gen,
		MinReaderVersion: store.EncryptionPolicyMinReaderVersion,
		AllowedWriteKIDs: writers,
		RequiredReadKIDs: append([]string{"k1"}, writers...),
		State:            store.EncryptionPolicyPrepared,
	}
}

// SecretEncryptionConformance runs every secret encryption contract on the
// backend factory selects.
func SecretEncryptionConformance(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ScanSecretPayloads", func(t *testing.T) { scanConformance(t, factory(t)) })
	t.Run("ReplaceSecretCiphertext", func(t *testing.T) { replaceConformance(t, factory(t)) })
	t.Run("EncryptionPolicy", func(t *testing.T) { policyConformance(t, factory(t)) })
	t.Run("EncryptionMigration", func(t *testing.T) { migrationConformance(t, factory(t)) })
	t.Run("PolicyFence", func(t *testing.T) { fenceConformance(t, factory(t)) })
	t.Run("ReferenceOnlyPayload", func(t *testing.T) { refOnlyConformance(t, factory(t)) })
	t.Run("TransitionAfterFencedWrite", func(t *testing.T) { transitionInTxConformance(t, factory(t)) })
	t.Run("PolicyCreationAfterFencedWrite", func(t *testing.T) { policyCreationAfterWriteConformance(t, factory(t)) })
}

func scanConformance(t *testing.T, s store.Store) {
	ctx := context.Background()
	got, err := encScan(ctx, s, "", 10)
	require.NoError(t, err)
	assert.Empty(t, got)

	type want struct{ key, scope, scopeID, value, ref string }
	rows := []want{
		{"ENC_HUB", store.ScopeHub, "hub-a", v1Value(1), ""},
		{"ENC_HUB_OLD", store.ScopeHub, "hub", v2Value("k1", 2), ""},
		{"ENC_HUB_EMPTY_SCOPE", store.ScopeHub, "", v2Value("k2", 3), ""},
		{"ENC_USER", store.ScopeUser, uuid.NewString(), "fake-plain", ""},
		{"ENC_PROJECT", store.ScopeProject, uuid.NewString(), v2Value("k2", 4), ""},
		{"ENC_BROKER", store.ScopeRuntimeBroker, uuid.NewString(), "", "gcpsm:projects/fake/secrets/x"},
		{"ENC_REF_BACKUP", store.ScopeHub, "hub-a", v1Value(5), "gcpsm:projects/fake/secrets/y"},
	}
	byID := map[string]want{}
	for _, r := range rows {
		sec := encSecret(r.key, r.scope, r.scopeID, r.value, r.ref)
		require.NoError(t, encCreate(ctx, s, sec))
		byID[sec.ID] = r
	}

	var all []store.SecretPayload
	after := ""
	for page := 0; ; page++ {
		require.Less(t, page, 10, "scan does not terminate")
		got, err := encScan(ctx, s, after, 3)
		require.NoError(t, err)
		require.LessOrEqual(t, len(got), 3)
		if len(got) == 0 {
			break
		}
		all = append(all, got...)
		after = got[len(got)-1].ID
	}
	require.Len(t, all, len(rows))
	ids := make([]string, len(all))
	for i, p := range all {
		ids[i] = p.ID
		w, ok := byID[p.ID]
		require.True(t, ok, "scan returned an unknown row")
		assert.Equal(t, w.key, p.Key)
		assert.Equal(t, w.scope, p.Scope)
		assert.Equal(t, w.scopeID, p.ScopeID)
		assert.Equal(t, 1, p.Version)
		assert.True(t, w.value == p.EncryptedValue, "payload of %s differs", w.key)
		assert.Equal(t, w.ref, p.SecretRef)
	}
	assert.True(t, sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }), "scan is not in ascending ID order")

	one, err := encScan(ctx, s, "", store.MaxSecretPayloadScan)
	require.NoError(t, err)
	assert.Len(t, one, len(rows))
	for _, bad := range []int{0, -1, store.MaxSecretPayloadScan + 1} {
		_, err := encScan(ctx, s, "", bad)
		assert.ErrorIs(t, err, store.ErrInvalidInput, "limit %d", bad)
	}
	_, err = encScan(ctx, s, "not-a-uuid", 10)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	last, err := encScan(ctx, s, ids[len(ids)-1], 10)
	require.NoError(t, err)
	assert.Empty(t, last)
}

func replaceConformance(t *testing.T, s store.Store) {
	ctx := context.Background()
	old := v1Value(7)
	sec := encSecret("ENC_REPLACE", store.ScopeHub, "hub-a", old, "gcpsm:projects/fake/secrets/r")
	require.NoError(t, encCreate(ctx, s, sec))
	before, err := s.GetSecret(ctx, sec.Key, sec.Scope, sec.ScopeID)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond) // a rewritten timestamp would differ

	t.Run("version mismatch", func(t *testing.T) {
		applied, err := encReplace(ctx, s, before.ID, before.Version+1, old, v2Value("k2", 8))
		require.NoError(t, err)
		assert.False(t, applied)
	})
	t.Run("ciphertext mismatch is exact bytes", func(t *testing.T) {
		// Same length, differs only in letter case: no collation may match.
		folded := strings.ToLower(old[:len(old)-6]) + old[len(old)-6:]
		if folded == old {
			folded = strings.ToUpper(old[:len(old)-6]) + old[len(old)-6:]
		}
		require.NotEqual(t, old, folded)
		applied, err := encReplace(ctx, s, before.ID, before.Version, folded, v2Value("k2", 8))
		require.NoError(t, err)
		assert.False(t, applied)
		applied, err = encReplace(ctx, s, before.ID, before.Version, old+" ", v2Value("k2", 8))
		require.NoError(t, err)
		assert.False(t, applied)
	})
	t.Run("empty replacement is refused", func(t *testing.T) {
		applied, err := encReplace(ctx, s, before.ID, before.Version, old, "")
		assert.ErrorIs(t, err, store.ErrInvalidInput)
		assert.False(t, applied)
	})
	t.Run("unknown and invalid id", func(t *testing.T) {
		applied, err := encReplace(ctx, s, uuid.NewString(), 1, old, v2Value("k2", 8))
		require.NoError(t, err)
		assert.False(t, applied)
		_, err = encReplace(ctx, s, "not-a-uuid", 1, old, v2Value("k2", 8))
		assert.ErrorIs(t, err, store.ErrInvalidInput)
	})
	t.Run("unchanged after misses", func(t *testing.T) {
		cur, err := s.GetSecret(ctx, sec.Key, sec.Scope, sec.ScopeID)
		require.NoError(t, err)
		assert.True(t, cur.EncryptedValue == old)
		assert.Equal(t, before.Version, cur.Version)
	})
	t.Run("applied preserves everything but the payload", func(t *testing.T) {
		repl := v2Value("k2", 9)
		applied, err := encReplace(ctx, s, before.ID, before.Version, old, repl)
		require.NoError(t, err)
		require.True(t, applied)
		cur, err := s.GetSecret(ctx, sec.Key, sec.Scope, sec.ScopeID)
		require.NoError(t, err)
		assert.True(t, cur.EncryptedValue == repl)
		assert.Equal(t, before.ID, cur.ID)
		assert.Equal(t, before.Version, cur.Version)
		assert.Equal(t, before.SecretRef, cur.SecretRef)
		assert.Equal(t, before.SecretType, cur.SecretType)
		assert.Equal(t, before.Target, cur.Target)
		assert.Equal(t, before.Description, cur.Description)
		assert.Equal(t, before.InjectionMode, cur.InjectionMode)
		assert.Equal(t, before.AllowProgeny, cur.AllowProgeny)
		assert.Equal(t, before.CreatedBy, cur.CreatedBy)
		assert.Equal(t, before.UpdatedBy, cur.UpdatedBy)
		assert.True(t, before.Created.Equal(cur.Created), "created changed")
		assert.True(t, before.Updated.Equal(cur.Updated), "updated changed")
		// The old expected ciphertext no longer matches.
		applied, err = encReplace(ctx, s, before.ID, before.Version, old, v2Value("k2", 10))
		require.NoError(t, err)
		assert.False(t, applied)
	})
	t.Run("delete and recreate is a different row", func(t *testing.T) {
		cur, err := s.GetSecret(ctx, sec.Key, sec.Scope, sec.ScopeID)
		require.NoError(t, err)
		require.NoError(t, encDelete(ctx, s, sec.Key, sec.Scope, sec.ScopeID))
		again := encSecret(sec.Key, sec.Scope, sec.ScopeID, cur.EncryptedValue, "")
		require.NoError(t, encCreate(ctx, s, again))
		applied, err := encReplace(ctx, s, cur.ID, 1, cur.EncryptedValue, v2Value("k2", 11))
		require.NoError(t, err)
		assert.False(t, applied, "a recreated row must not match the old ID")
		got, err := s.GetSecret(ctx, sec.Key, sec.Scope, sec.ScopeID)
		require.NoError(t, err)
		assert.True(t, got.EncryptedValue == cur.EncryptedValue)
	})
	t.Run("value changed at the same key", func(t *testing.T) {
		cur, err := s.GetSecret(ctx, sec.Key, sec.Scope, sec.ScopeID)
		require.NoError(t, err)
		applied, err := encValueIfVersion(ctx, s, sec.Key, sec.Scope, sec.ScopeID, cur.Version, v2Value("k2", 12))
		require.NoError(t, err)
		require.True(t, applied)
		applied, err = encReplace(ctx, s, cur.ID, cur.Version, cur.EncryptedValue, v2Value("k2", 13))
		require.NoError(t, err)
		assert.False(t, applied, "a concurrent value write must win")
	})
}

func policyConformance(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, err := s.GetEncryptionPolicy(ctx)
	require.ErrorIs(t, err, store.ErrNotFound)

	migID := uuid.NewString()
	p1 := fencePolicy(1, "k2")
	p1.MigrationID = migID
	start := time.Now().Add(-time.Second)
	applied, err := encSetPolicy(ctx, s, 0, p1)
	require.NoError(t, err)
	require.True(t, applied)
	got, err := s.GetEncryptionPolicy(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.Generation)
	assert.Equal(t, store.EncryptionPolicyMinReaderVersion, got.MinReaderVersion)
	assert.Equal(t, []string{"k2"}, got.AllowedWriteKIDs)
	assert.Equal(t, []string{"k1", "k2"}, got.RequiredReadKIDs)
	assert.Equal(t, migID, got.MigrationID)
	assert.Equal(t, store.EncryptionPolicyPrepared, got.State)
	assert.True(t, got.UpdatedAt.After(start), "updated_at is set by the store")
	assert.Equal(t, time.UTC, got.UpdatedAt.Location())

	// A second creator and a stale generation do not apply.
	applied, err = encSetPolicy(ctx, s, 0, fencePolicy(1, "k3"))
	require.NoError(t, err)
	assert.False(t, applied)
	applied, err = encSetPolicy(ctx, s, 5, fencePolicy(6, "k3"))
	require.NoError(t, err)
	assert.False(t, applied)

	// The next generation must be expected+1, and the policy must be valid.
	_, err = encSetPolicy(ctx, s, 1, fencePolicy(3, "k3"))
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	bad := map[string]store.EncryptionPolicy{
		"no writers":       fencePolicy(2),
		"invalid writer":   fencePolicy(2, "bad kid"),
		"duplicate writer": fencePolicy(2, "k2", "k2"),
		"min reader 1":     func() store.EncryptionPolicy { p := fencePolicy(2, "k2"); p.MinReaderVersion = 1; return p }(),
		"unknown state":    func() store.EncryptionPolicy { p := fencePolicy(2, "k2"); p.State = "done"; return p }(),
		"invalid reader": func() store.EncryptionPolicy {
			p := fencePolicy(2, "k2")
			p.RequiredReadKIDs = []string{"a:b"}
			return p
		}(),
		"invalid migration":   func() store.EncryptionPolicy { p := fencePolicy(2, "k2"); p.MigrationID = "nope"; return p }(),
		"generation not next": fencePolicy(9, "k2"),
	}
	for name, p := range bad {
		_, err := encSetPolicy(ctx, s, 1, p)
		assert.ErrorIs(t, err, store.ErrInvalidInput, name)
	}
	got, err = s.GetEncryptionPolicy(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.Generation, "a refused transition changed the policy")

	p2 := fencePolicy(2, "k3")
	p2.State = store.EncryptionPolicyVerified
	applied, err = encSetPolicy(ctx, s, 1, p2)
	require.NoError(t, err)
	require.True(t, applied)
	got, err = s.GetEncryptionPolicy(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Generation)
	assert.Equal(t, []string{"k3"}, got.AllowedWriteKIDs)
	assert.Equal(t, "", got.MigrationID, "a transition without a migration clears it")
	assert.Equal(t, store.EncryptionPolicyVerified, got.State)
}

func migrationConformance(t *testing.T, s store.Store) {
	ctx := context.Background()
	m := &store.EncryptionMigration{
		ID: uuid.NewString(), SourceGeneration: 1, TargetKID: "k2",
		State: store.EncryptionMigrationRunning, PlaintextReadAdmitted: true,
	}
	require.NoError(t, encCreateMigration(ctx, s, m))
	assert.False(t, m.Created.IsZero())
	got, err := s.GetEncryptionMigration(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.SourceGeneration)
	assert.Equal(t, "k2", got.TargetKID)
	assert.Equal(t, store.EncryptionMigrationRunning, got.State)
	assert.True(t, got.PlaintextReadAdmitted)
	assert.Equal(t, "", got.LastScannedID)

	assert.ErrorIs(t, encCreateMigration(ctx, s, &store.EncryptionMigration{ID: m.ID, TargetKID: "k2", State: store.EncryptionMigrationRunning}), store.ErrAlreadyExists)
	_, err = s.GetEncryptionMigration(ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
	for name, bad := range map[string]*store.EncryptionMigration{
		"no id":          {TargetKID: "k2", State: store.EncryptionMigrationRunning},
		"invalid kid":    {ID: uuid.NewString(), TargetKID: "", State: store.EncryptionMigrationRunning},
		"unknown state":  {ID: uuid.NewString(), TargetKID: "k2", State: "paused"},
		"negative count": {ID: uuid.NewString(), TargetKID: "k2", State: store.EncryptionMigrationRunning, Scanned: -1},
		"invalid uuid":   {ID: "nope", TargetKID: "k2", State: store.EncryptionMigrationRunning},
	} {
		assert.ErrorIs(t, encCreateMigration(ctx, s, bad), store.ErrInvalidInput, name)
	}

	// Progress update while running.
	upd := *got
	upd.LastScannedID = uuid.NewString()
	upd.Scanned, upd.Rewritten, upd.Failures = 10, 8, 1
	applied, err := encUpdateMigration(ctx, s, &upd, store.EncryptionMigrationRunning)
	require.NoError(t, err)
	require.True(t, applied)
	got, err = s.GetEncryptionMigration(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, upd.LastScannedID, got.LastScannedID)
	assert.Equal(t, int64(10), got.Scanned)
	assert.Equal(t, int64(8), got.Rewritten)
	assert.Equal(t, int64(1), got.Failures)

	// Wrong expected state does not apply.
	stale := *got
	stale.State = store.EncryptionMigrationInterrupted
	applied, err = encUpdateMigration(ctx, s, &stale, store.EncryptionMigrationInterrupted)
	require.NoError(t, err)
	assert.False(t, applied)

	// Verified clears plaintext admission even when asked to keep it.
	ver := *got
	ver.State = store.EncryptionMigrationVerified
	ver.PlaintextReadAdmitted = true
	applied, err = encUpdateMigration(ctx, s, &ver, store.EncryptionMigrationRunning)
	require.NoError(t, err)
	require.True(t, applied)
	got, err = s.GetEncryptionMigration(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, store.EncryptionMigrationVerified, got.State)
	assert.False(t, got.PlaintextReadAdmitted)

	// A verified job is terminal: no resume, no re-admission.
	resume := *got
	resume.State = store.EncryptionMigrationRunning
	_, err = encUpdateMigration(ctx, s, &resume, store.EncryptionMigrationVerified)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	readmit := *got
	readmit.PlaintextReadAdmitted = true
	_, err = encUpdateMigration(ctx, s, &readmit, store.EncryptionMigrationVerified)
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	// Created verified never stores admission.
	v := &store.EncryptionMigration{ID: uuid.NewString(), TargetKID: "k2", State: store.EncryptionMigrationVerified, PlaintextReadAdmitted: true}
	require.NoError(t, encCreateMigration(ctx, s, v))
	got, err = s.GetEncryptionMigration(ctx, v.ID)
	require.NoError(t, err)
	assert.False(t, got.PlaintextReadAdmitted)
}

// fenceCase is one candidate payload for the fence table.
type fenceCase struct {
	name    string
	value   string
	ref     string
	allowed bool
}

func fenceCases() []fenceCase {
	const ref = "gcpsm:projects/fake/secrets/z"
	return []fenceCase{
		{name: "plaintext", value: "fake-plain"},
		{name: "empty without ref", value: ""},
		{name: "v1", value: v1Value(21)},
		{name: "v2 stale writer", value: v2Value("k1", 22)},
		{name: "v2 unknown writer", value: v2Value("k9", 23)},
		{name: "malformed v2", value: "enc:v2:k2:@@@@"},
		{name: "unknown version", value: "enc:v3:" + v1Value(24)[7:]},
		{name: "v2 allowed writer", value: v2Value("k2", 25), allowed: true},
		{name: "v2 allowed writer with ref", value: v2Value("k2", 26), ref: ref, allowed: true},
		{name: "empty reference-only", value: "", ref: ref, allowed: true},
	}
}

func fenceConformance(t *testing.T, s store.Store) {
	ctx := context.Background()
	const scope, scopeID = store.ScopeHub, "fence-hub"

	// Before any policy: every payload is accepted (legacy behaviour).
	for i, c := range fenceCases() {
		sec := encSecret("PRE_POLICY_"+string(rune('A'+i)), scope, scopeID, c.value, c.ref)
		require.NoError(t, encCreate(ctx, s, sec), c.name)
	}
	// Seed rows the post-policy writes target, then enable the policy.
	legacyRow := encSecret("FENCE_LEGACY_ROW", scope, scopeID, v1Value(30), "")
	require.NoError(t, encCreate(ctx, s, legacyRow))
	refRow := encSecret("FENCE_REF_ROW", scope, scopeID, v1Value(31), "gcpsm:projects/fake/secrets/z")
	require.NoError(t, encCreate(ctx, s, refRow))
	applied, err := encSetPolicy(ctx, s, 0, fencePolicy(1, "k2"))
	require.NoError(t, err)
	require.True(t, applied)

	snapshot := func(t *testing.T, key string) *store.Secret {
		t.Helper()
		got, err := s.GetSecret(ctx, key, scope, scopeID)
		require.NoError(t, err)
		return got
	}
	check := func(t *testing.T, c fenceCase, err error, key string, before *store.Secret) {
		t.Helper()
		if c.allowed {
			require.NoError(t, err)
			return
		}
		require.ErrorIs(t, err, store.ErrEncryptionPolicyMismatch)
		if before == nil {
			_, gerr := s.GetSecret(ctx, key, scope, scopeID)
			assert.ErrorIs(t, gerr, store.ErrNotFound, "a refused create wrote a row")
			return
		}
		after := snapshot(t, key)
		assert.True(t, after.EncryptedValue == before.EncryptedValue, "a refused write changed the payload")
		assert.Equal(t, before.Version, after.Version, "a refused write changed the version")
		assert.Equal(t, before.SecretRef, after.SecretRef)
	}

	for i, c := range fenceCases() {
		c := c
		suffix := string(rune('A' + i))
		t.Run("CreateSecret/"+c.name, func(t *testing.T) {
			key := "FENCE_CREATE_" + suffix
			check(t, c, encCreate(ctx, s, encSecret(key, scope, scopeID, c.value, c.ref)), key, nil)
		})
		t.Run("UpsertSecret new/"+c.name, func(t *testing.T) {
			key := "FENCE_UPSERT_NEW_" + suffix
			_, err := encUpsert(ctx, s, encSecret(key, scope, scopeID, c.value, c.ref))
			check(t, c, err, key, nil)
		})
		t.Run("UpdateSecret/"+c.name, func(t *testing.T) {
			key := "FENCE_UPDATE_" + suffix
			seedFenceRow(t, ctx, s, key, scope, scopeID)
			before := snapshot(t, key)
			upd := *before
			upd.EncryptedValue, upd.SecretRef = c.value, c.ref
			check(t, c, encUpdate(ctx, s, &upd), key, before)
		})
		t.Run("UpsertSecret existing/"+c.name, func(t *testing.T) {
			key := "FENCE_UPSERT_OLD_" + suffix
			seedFenceRow(t, ctx, s, key, scope, scopeID)
			before := snapshot(t, key)
			_, err := encUpsert(ctx, s, encSecret(key, scope, scopeID, c.value, c.ref))
			check(t, c, err, key, before)
		})
		t.Run("UpdateSecretValueIfVersion/"+c.name, func(t *testing.T) {
			key := "FENCE_CAS_" + suffix
			seedFenceRowRef(t, ctx, s, key, scope, scopeID, c.ref)
			before := snapshot(t, key)
			applied, err := encValueIfVersion(ctx, s, key, scope, scopeID, before.Version, c.value)
			check(t, c, err, key, before)
			assert.Equal(t, c.allowed, applied)
		})
		t.Run("ReplaceSecretCiphertext/"+c.name, func(t *testing.T) {
			if c.value == "" {
				// An empty replacement is refused before the fence
				// (ErrInvalidInput; see the ReplaceSecretCiphertext
				// conformance).
				return
			}
			key := "FENCE_REPLACE_" + suffix
			seedFenceRowRef(t, ctx, s, key, scope, scopeID, c.ref)
			before := snapshot(t, key)
			applied, err := encReplace(ctx, s, before.ID, before.Version, before.EncryptedValue, c.value)
			check(t, c, err, key, before)
			assert.Equal(t, c.allowed, applied)
		})
		t.Run("WithTx CreateSecret/"+c.name, func(t *testing.T) {
			key := "FENCE_TX_" + suffix
			err := s.WithTx(ctx, func(tx store.Store) error {
				return encCreate(ctx, tx, encSecret(key, scope, scopeID, c.value, c.ref))
			})
			check(t, c, err, key, nil)
		})
	}

	t.Run("unfenced writes", func(t *testing.T) {
		before := snapshot(t, legacyRow.Key)
		// Metadata through UpdateSecret with the unchanged payload.
		upd := *before
		upd.Description = "metadata only"
		require.NoError(t, encUpdate(ctx, s, &upd))
		// Upsert re-writing the stored payload unchanged.
		same := encSecret(legacyRow.Key, scope, scopeID, before.EncryptedValue, "")
		same.Description = "upsert metadata only"
		_, err := encUpsert(ctx, s, same)
		require.NoError(t, err)
		desc := "meta update"
		_, err = s.UpdateSecretMeta(ctx, legacyRow.Key, scope, scopeID, &store.SecretMetaUpdate{Description: &desc})
		require.NoError(t, err)
		cur := snapshot(t, legacyRow.Key)
		applied, err := s.UpdateSecretRefIfMatches(ctx, legacyRow.Key, scope, scopeID, "", cur.Version, "gcpsm:projects/fake/secrets/n")
		require.NoError(t, err)
		assert.True(t, applied)
		cur = snapshot(t, legacyRow.Key)
		assert.True(t, cur.EncryptedValue == before.EncryptedValue, "unfenced writes must keep the old payload")
		// Replacing with the identical bytes is not a payload change.
		applied, err = encReplace(ctx, s, cur.ID, cur.Version, cur.EncryptedValue, cur.EncryptedValue)
		require.NoError(t, err)
		assert.True(t, applied)
		require.NoError(t, encDelete(ctx, s, legacyRow.Key, scope, scopeID))
		require.NoError(t, encDelete(ctx, s, refRow.Key, scope, scopeID))
	})

	t.Run("a policy transition moves the fence", func(t *testing.T) {
		applied, err := encSetPolicy(ctx, s, 1, fencePolicy(2, "k3"))
		require.NoError(t, err)
		require.True(t, applied)
		err = encCreate(ctx, s, encSecret("FENCE_AFTER_TRANSITION_OLD", scope, scopeID, v2Value("k2", 40), ""))
		assert.ErrorIs(t, err, store.ErrEncryptionPolicyMismatch)
		assert.NoError(t, encCreate(ctx, s, encSecret("FENCE_AFTER_TRANSITION_NEW", scope, scopeID, v2Value("k3", 41), "")))
	})

	t.Run("mismatch is not another store error", func(t *testing.T) {
		err := encCreate(ctx, s, encSecret("FENCE_ERR_KIND", scope, scopeID, "fake-plain", ""))
		require.ErrorIs(t, err, store.ErrEncryptionPolicyMismatch)
		for _, other := range []error{store.ErrNotFound, store.ErrAlreadyExists, store.ErrInvalidInput} {
			assert.False(t, errors.Is(err, other))
		}
		assert.Equal(t, "encryption_policy_mismatch", err.Error())
	})
}

// seedFenceRow creates a row whose payload passes the fence: an allowed v2
// value without a ref.
func seedFenceRow(t *testing.T, ctx context.Context, s store.Store, key, scope, scopeID string) {
	t.Helper()
	seedFenceRowRef(t, ctx, s, key, scope, scopeID, "")
}

func seedFenceRowRef(t *testing.T, ctx context.Context, s store.Store, key, scope, scopeID, ref string) {
	t.Helper()
	require.NoError(t, encCreate(ctx, s, encSecret(key, scope, scopeID, v2Value("k2", 50), ref)))
}

// refOnlyConformance: under a policy no write may leave a row with neither
// a value nor a SecretRef, including writes that do not change the value
// (the reference-only compare-and-swap, and UpdateSecret or UpsertSecret
// re-writing an empty value). Before any policy such writes are allowed.
func refOnlyConformance(t *testing.T, s store.Store) {
	ctx := context.Background()
	const scope, scopeID = store.ScopeHub, "ref-only-hub"
	const ref1, ref2 = "gcpsm:projects/fake/secrets/r1", "gcpsm:projects/fake/secrets/r2"
	get := func(t *testing.T, key string) *store.Secret {
		t.Helper()
		got, err := s.GetSecret(ctx, key, scope, scopeID)
		require.NoError(t, err)
		return got
	}
	unchanged := func(t *testing.T, before *store.Secret) {
		t.Helper()
		after := get(t, before.Key)
		assert.True(t, after.EncryptedValue == before.EncryptedValue, "a refused write changed the payload")
		assert.Equal(t, before.SecretRef, after.SecretRef, "a refused write changed the ref")
		assert.Equal(t, before.Version, after.Version, "a refused write changed the version")
	}

	// No policy: clearing the ref of a reference-only row is legacy
	// behaviour and still applies.
	require.NoError(t, encCreate(ctx, s, encSecret("REF_PRE_POLICY", scope, scopeID, "", ref1)))
	pre := get(t, "REF_PRE_POLICY")
	applied, err := s.UpdateSecretRefIfMatches(ctx, pre.Key, scope, scopeID, ref1, pre.Version, "")
	require.NoError(t, err)
	assert.True(t, applied)
	assert.Equal(t, "", get(t, pre.Key).SecretRef)

	// Seed the rows the policy-mode writes target, then enable the policy.
	require.NoError(t, encCreate(ctx, s, encSecret("REF_ONLY", scope, scopeID, "", ref1)))
	require.NoError(t, encCreate(ctx, s, encSecret("REF_VALUE", scope, scopeID, v2Value("k2", 60), ref1)))
	applied, err = encSetPolicy(ctx, s, 0, fencePolicy(1, "k2"))
	require.NoError(t, err)
	require.True(t, applied)

	t.Run("ref CAS clearing the ref of an empty row is refused", func(t *testing.T) {
		before := get(t, "REF_ONLY")
		applied, err := s.UpdateSecretRefIfMatches(ctx, before.Key, scope, scopeID, ref1, before.Version, "")
		require.ErrorIs(t, err, store.ErrEncryptionPolicyMismatch)
		assert.False(t, applied)
		unchanged(t, before)
	})
	t.Run("ref CAS with a failing compare is not applied, not refused", func(t *testing.T) {
		before := get(t, "REF_ONLY")
		applied, err := s.UpdateSecretRefIfMatches(ctx, before.Key, scope, scopeID, ref2, before.Version, "")
		require.NoError(t, err)
		assert.False(t, applied)
		applied, err = s.UpdateSecretRefIfMatches(ctx, before.Key, scope, scopeID, ref1, before.Version+1, "")
		require.NoError(t, err)
		assert.False(t, applied)
		applied, err = s.UpdateSecretRefIfMatches(ctx, "REF_MISSING", scope, scopeID, ref1, 1, "")
		require.NoError(t, err)
		assert.False(t, applied)
		unchanged(t, before)
	})
	t.Run("ref-only rotation to a non-empty ref applies", func(t *testing.T) {
		before := get(t, "REF_ONLY")
		applied, err := s.UpdateSecretRefIfMatches(ctx, before.Key, scope, scopeID, ref1, before.Version, ref2)
		require.NoError(t, err)
		assert.True(t, applied)
		after := get(t, before.Key)
		assert.Equal(t, ref2, after.SecretRef)
		assert.Equal(t, "", after.EncryptedValue)
		assert.Equal(t, before.Version+1, after.Version)
	})
	t.Run("ref CAS clearing the ref of a row with a value applies", func(t *testing.T) {
		before := get(t, "REF_VALUE")
		applied, err := s.UpdateSecretRefIfMatches(ctx, before.Key, scope, scopeID, ref1, before.Version, "")
		require.NoError(t, err)
		assert.True(t, applied)
		after := get(t, before.Key)
		assert.Equal(t, "", after.SecretRef)
		assert.True(t, after.EncryptedValue == before.EncryptedValue)
	})
	t.Run("UpdateSecret clearing the ref of an empty row is refused", func(t *testing.T) {
		before := get(t, "REF_ONLY")
		upd := *before
		upd.SecretRef = ""
		require.ErrorIs(t, encUpdate(ctx, s, &upd), store.ErrEncryptionPolicyMismatch)
		unchanged(t, before)
	})
	t.Run("UpsertSecret clearing the ref of an empty row is refused", func(t *testing.T) {
		before := get(t, "REF_ONLY")
		_, err := encUpsert(ctx, s, encSecret(before.Key, scope, scopeID, "", ""))
		require.ErrorIs(t, err, store.ErrEncryptionPolicyMismatch)
		unchanged(t, before)
	})
	t.Run("metadata writes keeping the ref apply", func(t *testing.T) {
		before := get(t, "REF_ONLY")
		upd := *before
		upd.Description = "ref-only metadata"
		require.NoError(t, encUpdate(ctx, s, &upd))
		same := encSecret(before.Key, scope, scopeID, "", before.SecretRef)
		same.Description = "ref-only upsert metadata"
		_, err := encUpsert(ctx, s, same)
		require.NoError(t, err)
		after := get(t, before.Key)
		assert.Equal(t, before.SecretRef, after.SecretRef)
		assert.Equal(t, before.Version+2, after.Version)
	})
	t.Run("an empty row without a ref takes no metadata write but can regain a ref", func(t *testing.T) {
		// REF_PRE_POLICY lost its ref before the policy existed.
		before := get(t, "REF_PRE_POLICY")
		require.Equal(t, "", before.SecretRef)
		upd := *before
		upd.Description = "metadata on an empty row"
		require.ErrorIs(t, encUpdate(ctx, s, &upd), store.ErrEncryptionPolicyMismatch)
		unchanged(t, before)
		applied, err := s.UpdateSecretRefIfMatches(ctx, before.Key, scope, scopeID, "", before.Version, ref1)
		require.NoError(t, err)
		assert.True(t, applied)
		assert.Equal(t, ref1, get(t, before.Key).SecretRef)
	})
}

// transitionInTxConformance: a policy or migration transition inside a
// WithTx transaction that has already run a fenced secret write is refused
// with ErrPolicyTransitionAfterFencedWrite, and the transaction rolls back.
// A transition before any fenced write, or outside a caller-supplied
// transaction, runs.
//
// Every refusal subtest is self-contained: the fixtures are created before
// the subtests, each subtest derives its transition from the current state
// (so the transition would apply if it were not refused), uses its own keys
// and values, and compares against a snapshot taken at its start. With the
// refusal removed, each one therefore fails at its require.ErrorIs (the
// transition returns nil), not at a setup step.
func transitionInTxConformance(t *testing.T, s store.Store) {
	ctx := context.Background()
	const scope, scopeID = store.ScopeHub, "transition-tx-hub"
	const valueKey, refKey = "TX_VALUE_ROW", "TX_REF_ROW"
	const ref = "gcpsm:projects/fake/secrets/t"

	applied, err := encSetPolicy(ctx, s, 0, fencePolicy(1, "k2"))
	require.NoError(t, err)
	require.True(t, applied)
	require.NoError(t, encCreate(ctx, s, encSecret(valueKey, scope, scopeID, v2Value("k2", 70), "")))
	require.NoError(t, encCreate(ctx, s, encSecret(refKey, scope, scopeID, "", ref)))
	mig := &store.EncryptionMigration{ID: uuid.NewString(), TargetKID: "k3", State: store.EncryptionMigrationRunning}
	require.NoError(t, encCreateMigration(ctx, s, mig))

	// otherState is the migration state a transition moves to from cur.
	otherState := func(cur store.EncryptionMigrationState) store.EncryptionMigrationState {
		if cur == store.EncryptionMigrationRunning {
			return store.EncryptionMigrationInterrupted
		}
		return store.EncryptionMigrationRunning
	}

	// Each write takes the subtest number n for its own keys and values.
	writes := map[string]func(tx store.Store, n int) error{
		"CreateSecret": func(tx store.Store, n int) error {
			return encCreate(ctx, tx, encSecret(fmt.Sprintf("TX_W_CREATE_%02d", n), scope, scopeID, v2Value("k2", byte(n)), ""))
		},
		"UpsertSecret": func(tx store.Store, n int) error {
			_, err := encUpsert(ctx, tx, encSecret(fmt.Sprintf("TX_W_UPSERT_%02d", n), scope, scopeID, v2Value("k2", byte(n)), ""))
			return err
		},
		"UpdateSecret": func(tx store.Store, n int) error {
			cur, err := tx.GetSecret(ctx, valueKey, scope, scopeID)
			if err != nil {
				return err
			}
			cur.EncryptedValue = v2Value("k2", byte(100+n))
			return encUpdate(ctx, tx, cur)
		},
		"UpdateSecretValueIfVersion": func(tx store.Store, n int) error {
			cur, err := tx.GetSecret(ctx, valueKey, scope, scopeID)
			if err != nil {
				return err
			}
			_, err = encValueIfVersion(ctx, tx, cur.Key, scope, scopeID, cur.Version, v2Value("k2", byte(130+n)))
			return err
		},
		"ReplaceSecretCiphertext": func(tx store.Store, n int) error {
			cur, err := tx.GetSecret(ctx, valueKey, scope, scopeID)
			if err != nil {
				return err
			}
			_, err = encReplace(ctx, tx, cur.ID, cur.Version, cur.EncryptedValue, v2Value("k2", byte(160+n)))
			return err
		},
		"UpdateSecretRefIfMatches": func(tx store.Store, n int) error {
			cur, err := tx.GetSecret(ctx, refKey, scope, scopeID)
			if err != nil {
				return err
			}
			_, err = tx.UpdateSecretRefIfMatches(ctx, cur.Key, scope, scopeID, cur.SecretRef, cur.Version, fmt.Sprintf("%s-%02d", ref, n))
			return err
		},
		"a refused CreateSecret": func(tx store.Store, n int) error {
			err := encCreate(ctx, tx, encSecret(fmt.Sprintf("TX_W_REFUSED_%02d", n), scope, scopeID, "fake-plain", ""))
			if !errors.Is(err, store.ErrEncryptionPolicyMismatch) {
				return err
			}
			return nil
		},
	}
	transitions := map[string]func(tx store.Store) error{
		"UpdateEncryptionPolicy": func(tx store.Store) error {
			cur, err := tx.GetEncryptionPolicy(ctx)
			if err != nil {
				return err
			}
			_, err = encSetPolicy(ctx, tx, cur.Generation, fencePolicy(cur.Generation+1, "k2"))
			return err
		},
		"CreateEncryptionMigration": func(tx store.Store) error {
			return encCreateMigration(ctx, tx, &store.EncryptionMigration{ID: uuid.NewString(), TargetKID: "k3", State: store.EncryptionMigrationRunning})
		},
		"UpdateEncryptionMigration": func(tx store.Store) error {
			cur, err := tx.GetEncryptionMigration(ctx, mig.ID)
			if err != nil {
				return err
			}
			next := *cur
			next.State = otherState(cur.State)
			_, err = encUpdateMigration(ctx, tx, &next, cur.State)
			return err
		},
	}

	n := 0
	for _, tn := range sortedKeys(transitions) {
		for _, wn := range sortedKeys(writes) {
			n++
			tn, wn, n := tn, wn, n
			t.Run(tn+" after "+wn, func(t *testing.T) {
				policy, err := s.GetEncryptionPolicy(ctx)
				require.NoError(t, err)
				m, err := s.GetEncryptionMigration(ctx, mig.ID)
				require.NoError(t, err)
				var rows []*store.Secret
				for _, key := range []string{valueKey, refKey} {
					row, err := s.GetSecret(ctx, key, scope, scopeID)
					require.NoError(t, err)
					rows = append(rows, row)
				}

				err = s.WithTx(ctx, func(tx store.Store) error {
					if err := writes[wn](tx, n); err != nil {
						return err
					}
					return transitions[tn](tx)
				})
				require.ErrorIs(t, err, store.ErrPolicyTransitionAfterFencedWrite)

				// Nothing committed: the policy, the migration and the rows
				// are as at the start, and no new key exists.
				after, err := s.GetEncryptionPolicy(ctx)
				require.NoError(t, err)
				assert.Equal(t, policy.Generation, after.Generation)
				am, err := s.GetEncryptionMigration(ctx, mig.ID)
				require.NoError(t, err)
				assert.Equal(t, m.State, am.State)
				for _, before := range rows {
					got, err := s.GetSecret(ctx, before.Key, scope, scopeID)
					require.NoError(t, err)
					assert.Equal(t, before.Version, got.Version, before.Key)
					assert.True(t, got.EncryptedValue == before.EncryptedValue, before.Key)
					assert.Equal(t, before.SecretRef, got.SecretRef, before.Key)
				}
				for _, key := range []string{fmt.Sprintf("TX_W_CREATE_%02d", n), fmt.Sprintf("TX_W_UPSERT_%02d", n)} {
					_, err := s.GetSecret(ctx, key, scope, scopeID)
					assert.ErrorIs(t, err, store.ErrNotFound, "the refused transaction committed %s", key)
				}
			})
		}
	}

	t.Run("a transition before fenced writes runs", func(t *testing.T) {
		cur, err := s.GetEncryptionPolicy(ctx)
		require.NoError(t, err)
		err = s.WithTx(ctx, func(tx store.Store) error {
			applied, err := encSetPolicy(ctx, tx, cur.Generation, fencePolicy(cur.Generation+1, "k3"))
			if err != nil {
				return err
			}
			if !applied {
				return errors.New("the transition did not apply")
			}
			// The write in the same transaction sees the new policy.
			if err := encCreate(ctx, tx, encSecret("TX_AFTER_PLAIN", scope, scopeID, v2Value("k2", 200), "")); !errors.Is(err, store.ErrEncryptionPolicyMismatch) {
				return fmt.Errorf("a k2 write after the transition to k3 was not fenced by it: %v", err)
			}
			return encCreate(ctx, tx, encSecret("TX_AFTER_TRANSITION", scope, scopeID, v2Value("k3", 201), ""))
		})
		require.NoError(t, err)
		got, err := s.GetEncryptionPolicy(ctx)
		require.NoError(t, err)
		assert.Equal(t, cur.Generation+1, got.Generation)
		_, err = s.GetSecret(ctx, "TX_AFTER_TRANSITION", scope, scopeID)
		require.NoError(t, err)
	})

	t.Run("transitions outside a caller transaction run", func(t *testing.T) {
		cur, err := s.GetEncryptionPolicy(ctx)
		require.NoError(t, err)
		applied, err := encSetPolicy(ctx, s, cur.Generation, fencePolicy(cur.Generation+1, "k3"))
		require.NoError(t, err)
		assert.True(t, applied)
		m, err := s.GetEncryptionMigration(ctx, mig.ID)
		require.NoError(t, err)
		next := *m
		next.State = otherState(m.State)
		applied, err = encUpdateMigration(ctx, s, &next, m.State)
		require.NoError(t, err)
		assert.True(t, applied)
	})
}

// policyCreationAfterWriteConformance: creating the policy (no policy yet)
// inside a WithTx transaction after a fenced write is refused, and neither
// the policy nor the write commits. It runs on a store of its own, so with
// the refusal removed it fails at its require.ErrorIs.
func policyCreationAfterWriteConformance(t *testing.T, s store.Store) {
	ctx := context.Background()
	const scope, scopeID = store.ScopeHub, "transition-tx-hub"
	err := s.WithTx(ctx, func(tx store.Store) error {
		if err := encCreate(ctx, tx, encSecret("TX_BEFORE_CREATE", scope, scopeID, v1Value(70), "")); err != nil {
			return err
		}
		_, err := encSetPolicy(ctx, tx, 0, fencePolicy(1, "k2"))
		return err
	})
	require.ErrorIs(t, err, store.ErrPolicyTransitionAfterFencedWrite)
	_, err = s.GetEncryptionPolicy(ctx)
	assert.ErrorIs(t, err, store.ErrNotFound, "the refused transition created the policy")
	_, err = s.GetSecret(ctx, "TX_BEFORE_CREATE", scope, scopeID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the refused transaction committed its write")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
