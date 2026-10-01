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

package entadapter

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func historyTestConstraint(name string) *store.AccessConstraint {
	return &store.AccessConstraint{
		Name:               name,
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          "system",
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "history store test",
	}
}

func historyTestEntry(constraintID, eventID string, occurredAt time.Time) *store.AccessConstraintHistory {
	after := int64(1)
	return &store.AccessConstraintHistory{
		EventID:           eventID,
		ConstraintID:      constraintID,
		OccurredAt:        occurredAt,
		Operation:         "create",
		ActorKind:         "user",
		ActorID:           "actor-1",
		CorrelationID:     "correlation-1",
		AfterRevision:     &after,
		Classification:    "tighten",
		PreviewID:         "preview-1",
		DraftHash:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ImpactCountsJSON:  `{"agents":1,"users":2,"projects":3}`,
		ChangedFieldsJSON: `["maximum_permissions"]`,
	}
}

func TestConstraintHistory_PrunesDeterministicallyPerConstraint(t *testing.T) {
	ctx := context.Background()
	composite := NewCompositeStore(enttest.NewClient(t))
	first, err := composite.CreateAccessConstraint(ctx, historyTestConstraint("history-first"))
	require.NoError(t, err)
	second, err := composite.CreateAccessConstraint(ctx, historyTestConstraint("history-second"))
	require.NoError(t, err)

	occurredAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, composite.WithTx(ctx, func(tx store.Store) error {
		for i := 0; i < 1001; i++ {
			if err := tx.AppendConstraintHistoryTx(ctx, historyTestEntry(first.ID, fmt.Sprintf("event-%04d", i), occurredAt)); err != nil {
				return err
			}
		}
		if err := tx.AppendConstraintHistoryTx(ctx, historyTestEntry(second.ID, "other-constraint-event", occurredAt.Add(-time.Hour))); err != nil {
			return err
		}
		return tx.PruneConstraintHistoryTx(ctx, first.ID)
	}))

	firstRows, err := composite.ListConstraintHistory(ctx, first.ID)
	require.NoError(t, err)
	require.Len(t, firstRows, 1000)
	assert.Equal(t, "event-1000", firstRows[0].EventID)
	assert.Equal(t, "user", firstRows[0].ActorKind)
	assert.Equal(t, "actor-1", firstRows[0].ActorID)
	assert.Equal(t, "correlation-1", firstRows[0].CorrelationID)
	assert.Equal(t, int64(1), *firstRows[0].AfterRevision)
	assert.Equal(t, "tighten", firstRows[0].Classification)
	assert.Equal(t, "preview-1", firstRows[0].PreviewID)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", firstRows[0].DraftHash)
	assert.JSONEq(t, `{"agents":1,"users":2,"projects":3}`, firstRows[0].ImpactCountsJSON)
	assert.JSONEq(t, `["maximum_permissions"]`, firstRows[0].ChangedFieldsJSON)
	assert.Equal(t, "event-0001", firstRows[len(firstRows)-1].EventID)
	secondRows, err := composite.ListConstraintHistory(ctx, second.ID)
	require.NoError(t, err)
	require.Len(t, secondRows, 1)
	assert.Equal(t, "other-constraint-event", secondRows[0].EventID)
}

func TestConstraintHistory_CascadesWithLiveConstraint(t *testing.T) {
	ctx := context.Background()
	composite := NewCompositeStore(enttest.NewClient(t))
	constraint, err := composite.CreateAccessConstraint(ctx, historyTestConstraint("history-cascade"))
	require.NoError(t, err)
	require.NoError(t, composite.AppendConstraintHistoryTx(ctx, historyTestEntry(constraint.ID, "cascade-event", time.Now().UTC())))
	require.NoError(t, composite.DeleteAccessConstraint(ctx, constraint.ID))
	rows, err := composite.ListConstraintHistory(ctx, constraint.ID)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestConstraintHistory_InsertFailureRollsBackConstraintCreate(t *testing.T) {
	ctx := context.Background()
	composite := NewCompositeStore(enttest.NewClient(t))
	existing, err := composite.CreateAccessConstraint(ctx, historyTestConstraint("history-existing"))
	require.NoError(t, err)
	require.NoError(t, composite.AppendConstraintHistoryTx(ctx, historyTestEntry(existing.ID, "duplicate-event", time.Now().UTC())))

	var attemptedID string
	err = composite.WithTx(ctx, func(tx store.Store) error {
		created, createErr := tx.CreateAccessConstraint(ctx, historyTestConstraint("history-rolled-back"))
		if createErr != nil {
			return createErr
		}
		attemptedID = created.ID
		return tx.AppendConstraintHistoryTx(ctx, historyTestEntry(created.ID, "duplicate-event", time.Now().UTC()))
	})
	require.Error(t, err)
	_, err = composite.GetAccessConstraint(ctx, attemptedID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestConstraintHistory_SurvivesRestartAndSecondStoreInstance(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "history.db")
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(ctx, client))
	firstStore := NewCompositeStore(client)
	constraint, err := firstStore.CreateAccessConstraint(ctx, historyTestConstraint("history-restart"))
	require.NoError(t, err)
	require.NoError(t, firstStore.AppendConstraintHistoryTx(ctx, historyTestEntry(constraint.ID, "restart-event", time.Now().UTC())))

	replicaStore := NewCompositeStore(client)
	rows, err := replicaStore.ListConstraintHistory(ctx, constraint.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NoError(t, client.Close())

	reopened, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	restartedStore := NewCompositeStore(reopened)
	rows, err = restartedStore.ListConstraintHistory(ctx, constraint.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "restart-event", rows[0].EventID)
}
