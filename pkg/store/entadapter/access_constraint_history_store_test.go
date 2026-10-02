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
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/accessconstraint"
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
		return nil
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
	require.NoError(t, composite.WithTx(ctx, func(tx store.Store) error {
		return tx.AppendConstraintHistoryTx(ctx, historyTestEntry(constraint.ID, "cascade-event", time.Now().UTC()))
	}))
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
	require.NoError(t, composite.WithTx(ctx, func(tx store.Store) error {
		return tx.AppendConstraintHistoryTx(ctx, historyTestEntry(existing.ID, "duplicate-event", time.Now().UTC()))
	}))

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

func TestConstraintHistory_AppendRequiresTransaction(t *testing.T) {
	ctx := context.Background()
	composite := NewCompositeStore(enttest.NewClient(t))
	constraint, err := composite.CreateAccessConstraint(ctx, historyTestConstraint("history-requires-tx"))
	require.NoError(t, err)

	err = composite.AppendConstraintHistoryTx(ctx, historyTestEntry(constraint.ID, "outside-tx", time.Now().UTC()))
	require.Error(t, err)
	rows, listErr := composite.ListConstraintHistory(ctx, constraint.ID)
	require.NoError(t, listErr)
	assert.Empty(t, rows)
}

func TestConstraintHistory_PruneDeleteFailureRollsBackCreateAndHistory(t *testing.T) {
	ctx := context.Background()
	composite := NewCompositeStore(enttest.NewClient(t))
	sentinel := errors.New("injected history prune delete failure")
	composite.client.AccessConstraintHistory.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
			if mutation.Op() == ent.OpDelete {
				return nil, sentinel
			}
			return next.Mutate(ctx, mutation)
		})
	})

	var attemptedID string
	err := composite.WithTx(ctx, func(tx store.Store) error {
		created, createErr := tx.CreateAccessConstraint(ctx, historyTestConstraint("history-prune-rolled-back"))
		if createErr != nil {
			return createErr
		}
		attemptedID = created.ID
		occurredAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		for i := 0; i < 1001; i++ {
			if appendErr := tx.AppendConstraintHistoryTx(ctx, historyTestEntry(created.ID, fmt.Sprintf("rollback-event-%04d", i), occurredAt)); appendErr != nil {
				return appendErr
			}
		}
		return nil
	})
	require.ErrorIs(t, err, sentinel)
	_, err = composite.GetAccessConstraint(ctx, attemptedID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	rows, listErr := composite.ListConstraintHistory(ctx, attemptedID)
	require.NoError(t, listErr)
	assert.Empty(t, rows)
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
	require.NoError(t, firstStore.WithTx(ctx, func(tx store.Store) error {
		return tx.AppendConstraintHistoryTx(ctx, historyTestEntry(constraint.ID, "restart-event", time.Now().UTC()))
	}))

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

func TestConstraintHistory_ConcurrentCapPostgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("SCION_TEST_POSTGRES_URL is required for the PostgreSQL cap race")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := enttest.NewSchemaURL(t)
	clientA, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientA.Close() })
	clientB, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientB.Close() })
	storeA := NewCompositeStore(clientA)
	storeB := NewCompositeStore(clientB)

	constraint, err := storeA.CreateAccessConstraint(ctx, historyTestConstraint("history-postgres-race"))
	require.NoError(t, err)
	occurredAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, storeA.WithTx(ctx, func(tx store.Store) error {
		for i := 0; i < 999; i++ {
			if err := tx.AppendConstraintHistoryTx(ctx, historyTestEntry(constraint.ID, fmt.Sprintf("event-%04d", i), occurredAt)); err != nil {
				return err
			}
		}
		return nil
	}))

	constraintID, err := parseUUID(constraint.ID)
	require.NoError(t, err)
	holderReady := make(chan int, 1)
	holderDone := make(chan error, 1)
	releaseHolder := make(chan struct{})
	holderReleased := false
	defer func() {
		if !holderReleased {
			close(releaseHolder)
		}
	}()
	go func() {
		holderDone <- storeA.WithTx(ctx, func(tx store.Store) error {
			txStore := tx.(*CompositeStore)
			pid, pidErr := constraintHistoryPostgresBackendPID(ctx, txStore)
			if pidErr != nil {
				return pidErr
			}
			_, lockErr := txStore.client.AccessConstraint.Query().
				Where(
					accessconstraint.IDEQ(constraintID),
					func(selector *entsql.Selector) { selector.For(entsql.LockNoKeyUpdate) },
				).
				Only(ctx)
			if lockErr != nil {
				return fmt.Errorf("acquire holder row lock: %w", lockErr)
			}
			entry := historyTestEntry(constraint.ID, "event-0999", occurredAt)
			builder := txStore.client.AccessConstraintHistory.Create().
				SetID(entry.EventID).
				SetConstraintID(constraintID).
				SetOccurredAt(entry.OccurredAt).
				SetOperation(entry.Operation)
			setOptionalHistoryFields(builder, entry)
			if _, saveErr := builder.Save(ctx); saveErr != nil {
				return fmt.Errorf("insert holder history row: %w", saveErr)
			}
			holderReady <- pid
			select {
			case <-releaseHolder:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	var holderPID int
	select {
	case holderPID = <-holderReady:
	case holderErr := <-holderDone:
		require.NoError(t, holderErr)
		t.Fatal("holder transaction completed before publishing its lock")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	writerReady := make(chan int, 1)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- storeB.WithTx(ctx, func(tx store.Store) error {
			txStore := tx.(*CompositeStore)
			pid, pidErr := constraintHistoryPostgresBackendPID(ctx, txStore)
			if pidErr != nil {
				return pidErr
			}
			writerReady <- pid
			return tx.AppendConstraintHistoryTx(ctx, historyTestEntry(constraint.ID, "event-1000", occurredAt))
		})
	}()

	var writerPID int
	select {
	case writerPID = <-writerReady:
	case writerErr := <-writerDone:
		require.NoError(t, writerErr)
		t.Fatal("writer transaction completed before publishing its backend PID")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	observer := storeA.DB()
	require.NotNil(t, observer)
	for {
		var blockedOnHolder bool
		err := observer.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE pid = $1
				  AND wait_event_type = 'Lock'
				  AND $2::integer = ANY(pg_blocking_pids(pid))
				  AND POSITION('FOR UPDATE' IN UPPER(query)) > 0
			)`, writerPID, holderPID).Scan(&blockedOnHolder)
		require.NoError(t, err)
		if blockedOnHolder {
			break
		}
		select {
		case writerErr := <-writerDone:
			require.NoError(t, writerErr)
			t.Fatal("writer completed before waiting on the holder's row lock")
		case <-ctx.Done():
			t.Fatal("writer row-lock wait was not observed before the test deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}

	close(releaseHolder)
	holderReleased = true
	require.NoError(t, <-holderDone)
	require.NoError(t, <-writerDone)

	rows, err := storeA.ListConstraintHistory(ctx, constraint.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1000)
	assert.Equal(t, "event-1000", rows[0].EventID)
	assert.Equal(t, "event-0001", rows[len(rows)-1].EventID)
	for _, row := range rows {
		assert.NotEqual(t, "event-0000", row.EventID)
	}
}

func constraintHistoryPostgresBackendPID(ctx context.Context, txStore *CompositeStore) (int, error) {
	rows := &entsql.Rows{}
	if err := txStore.client.Driver().Query(ctx, "SELECT pg_backend_pid()", []any{}, rows); err != nil {
		return 0, fmt.Errorf("query PostgreSQL backend PID: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return 0, fmt.Errorf("query PostgreSQL backend PID: no row returned")
	}
	var pid int
	if err := rows.Scan(&pid); err != nil {
		return 0, fmt.Errorf("scan PostgreSQL backend PID: %w", err)
	}
	return pid, rows.Err()
}
