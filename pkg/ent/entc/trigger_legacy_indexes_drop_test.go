// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

//go:build !no_sqlite

package entc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAutoMigrate_DropsLegacyTriggerUniqueIndexes(t *testing.T) {
	ctx := context.Background()
	client, err := OpenSQLite(":memory:", PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	require.NoError(t, AutoMigrate(ctx, client))

	// Simulate an upgraded pre-P9 database that still has the tighter legacy
	// unique indexes on (event_id, trigger_id) and (trigger_id, subject).
	drv := client.Driver()
	require.NoError(t, drv.Exec(ctx,
		`CREATE UNIQUE INDEX triggerfire_event_id_trigger_id ON trigger_fires(event_id, trigger_id)`,
		[]any{}, nil))
	require.NoError(t, drv.Exec(ctx,
		`CREATE UNIQUE INDEX triggersubjectclaim_trigger_id_subject ON trigger_subject_claims(trigger_id, subject)`,
		[]any{}, nil))

	// Re-running AutoMigrate must idempotently drop both legacy unique indexes.
	require.NoError(t, AutoMigrate(ctx, client))

	now := time.Now().UTC()
	pid := uuid.New()
	tid := uuid.New()
	eid := uuid.New()

	// Multiple action_index rows for the same (event_id, trigger_id) must now succeed.
	for idx := 0; idx < 2; idx++ {
		_, err := client.TriggerFire.Create().
			SetID(uuid.New()).
			SetProjectID(pid).
			SetTriggerID(tid).
			SetEventID(eid).
			SetSubject("pr-42").
			SetActionType("message").
			SetActionIndex(idx).
			SetStatus("fired").
			SetLeaseUntil(now.Add(time.Minute)).
			SetStartedAt(now).
			Save(ctx)
		require.NoError(t, err, "expected trigger_fires insert for action_index=%d to succeed", idx)
	}

	// Multiple action_index claims for the same (trigger_id, subject) must now succeed.
	for idx := 0; idx < 2; idx++ {
		_, err := client.TriggerSubjectClaim.Create().
			SetID(uuid.New()).
			SetTriggerID(tid).
			SetActionIndex(idx).
			SetSubject("pr-42").
			SetState("active").
			SetEventID(eid).
			SetFireID(uuid.New()).
			SetClaimedAt(now).
			Save(ctx)
		require.NoError(t, err, "expected trigger_subject_claims insert for action_index=%d to succeed", idx)
	}
}
