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
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Notification kind and resource fields (ptone/scion#5145).

const testArtifactID = "5f1c2d3e-0000-4000-8000-000000000001"

// newResourceNotification returns an unsaved notification about an
// artifact addressed to user subscriberID, with no subscription and no
// agent (the nil UUID in both).
func newResourceNotification(subscriberID string) *store.Notification {
	return &store.Notification{
		ID:             uuid.NewString(),
		SubscriptionID: store.CreatorFallbackSubscriptionID,
		AgentID:        store.CreatorFallbackSubscriptionID,
		ProjectID:      uuid.NewString(),
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   subscriberID,
		Status:         store.NotificationStatusArtifactReviewRequested,
		Message:        "A user asked you to review an artifact (Can review): scion://artifact/" + testArtifactID,
		Kind:           store.NotificationKindArtifactGrant,
		ResourceType:   store.NotificationResourceArtifact,
		ResourceID:     testArtifactID,
	}
}

func TestNotificationStore_ResourceFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	withKind := newResourceNotification("user-1")
	require.NoError(t, s.CreateNotification(ctx, withKind))
	plain := &store.Notification{
		ID:             uuid.NewString(),
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      uuid.NewString(),
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "user-1",
		Status:         "COMPLETED",
		Message:        "agent done",
	}
	require.NoError(t, s.CreateNotification(ctx, plain))

	got, err := s.GetNotification(ctx, withKind.ID)
	require.NoError(t, err)
	assert.Equal(t, store.NotificationKindArtifactGrant, got.Kind)
	assert.Equal(t, store.NotificationResourceArtifact, got.ResourceType)
	assert.Equal(t, testArtifactID, got.ResourceID)

	got, err = s.GetNotification(ctx, plain.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Kind, "a writer that sets no kind stores an empty kind")
	assert.Empty(t, got.ResourceType)
	assert.Empty(t, got.ResourceID)

	list, err := s.GetNotifications(ctx, store.SubscriberTypeUser, "user-1", false)
	require.NoError(t, err)
	require.Len(t, list, 2)
	byID := map[string]store.Notification{}
	for _, n := range list {
		byID[n.ID] = n
	}
	assert.Equal(t, testArtifactID, byID[withKind.ID].ResourceID)
	assert.Empty(t, byID[plain.ID].Kind)

	other, err := s.GetNotifications(ctx, store.SubscriberTypeUser, "user-2", false)
	require.NoError(t, err)
	assert.Empty(t, other, "a notification is listed for its subscriber only")
}

// TestNotificationStore_UserRowsSkippedBySweepAndExpiry pins that a user
// row with the nil subscription (which on an agent row is the HALTED
// creator fallback) is never claimed by the dispatch sweep and never
// expired: both read agent-subscriber rows only.
func TestNotificationStore_UserRowsSkippedBySweepAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	n := newResourceNotification("user-1")
	n.CreatedAt = time.Now().Add(-48 * time.Hour)
	require.NoError(t, s.CreateNotification(ctx, n))

	undispatched, err := s.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	for _, u := range undispatched {
		assert.NotEqual(t, n.ID, u.ID, "the sweep must not see a user row")
	}

	expired, err := s.ExpireStaleAgentNotifications(ctx, time.Now())
	require.NoError(t, err)
	assert.Zero(t, expired)

	got, err := s.GetNotification(ctx, n.ID)
	require.NoError(t, err)
	assert.False(t, got.Dispatched)
	assert.Empty(t, got.DispatchOutcome)
	assert.Empty(t, got.DispatchReason)
}

// TestNotificationStore_AcknowledgedResourceRowsArePurged pins that a
// notification with no agent and no subscription is not kept as history:
// the orphan purge deletes it once it is acknowledged, and keeps it until
// then.
func TestNotificationStore_AcknowledgedResourceRowsArePurged(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	acked := newResourceNotification("user-1")
	require.NoError(t, cs.CreateNotification(ctx, acked))
	require.NoError(t, cs.AcknowledgeNotification(ctx, acked.ID))
	unacked := newResourceNotification("user-1")
	require.NoError(t, cs.CreateNotification(ctx, unacked))

	purged, err := cs.PurgeOrphanedNotifications(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, purged)

	_, err = cs.GetNotification(ctx, acked.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "an acknowledged notification with no agent is purged")
	_, err = cs.GetNotification(ctx, unacked.ID)
	assert.NoError(t, err, "an unacknowledged notification is kept")
}

// TestAgentCascades_RefuseNilAgentID pins that the agent cascades, which
// delete notifications by agent id, refuse the nil UUID: notifications
// that are not about an agent carry it.
func TestAgentCascades_RefuseNilAgentID(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	n := newResourceNotification("user-1")
	require.NoError(t, cs.CreateNotification(ctx, n))

	err := cs.deleteAgentDependents(ctx, uuid.Nil.String())
	require.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = cs.GetNotification(ctx, n.ID)
	assert.NoError(t, err, "a nil-agent notification survives a refused cascade")

	assert.ErrorIs(t, refuseNilAgentIDs(uuid.New(), uuid.Nil), store.ErrInvalidInput)
	assert.NoError(t, refuseNilAgentIDs(uuid.New(), uuid.New()))
	assert.NoError(t, refuseNilAgentIDs())
}

// TestDeleteProject_RefusesNilAgentID pins the DeleteProject half of the
// nil-UUID guard (K23b): a project holding an agent whose id is the nil
// UUID is not deleted, because its notification cascade would match every
// notification that is not about an agent. Everything survives, since the
// guard runs before any delete in the transaction.
func TestDeleteProject_RefusesNilAgentID(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	project := &store.Project{ID: uuid.NewString(), Name: "nil-agent project", Slug: "nil-agent-project"}
	require.NoError(t, cs.CreateProject(ctx, project))
	nilAgent := &store.Agent{
		ID: uuid.Nil.String(), Slug: "nil-agent", Name: "nil agent", Template: "default",
		ProjectID: project.ID, Phase: "running",
	}
	require.NoError(t, cs.CreateAgent(ctx, nilAgent))
	n := newResourceNotification("user-1")
	require.NoError(t, cs.CreateNotification(ctx, n))

	err := cs.DeleteProject(ctx, project.ID)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	_, err = cs.GetProject(ctx, project.ID)
	assert.NoError(t, err, "the project survives")
	_, err = cs.GetAgent(ctx, uuid.Nil.String())
	assert.NoError(t, err, "the agent survives")
	_, err = cs.GetNotification(ctx, n.ID)
	assert.NoError(t, err, "the nil-agent notification survives")
}

// assertResourceFieldsUpgraded checks the legacy rows (see
// notification_dispatch_migration_test.go) after Migrate: they read empty
// kind and resource fields, and a new row stores them.
func assertResourceFieldsUpgraded(t *testing.T, cs *CompositeStore) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []string{legacyNotifDelivered, legacyNotifPending} {
		n, err := cs.GetNotification(ctx, id)
		require.NoError(t, err)
		assert.Empty(t, n.Kind, id)
		assert.Empty(t, n.ResourceType, id)
		assert.Empty(t, n.ResourceID, id)
	}
	fresh := newResourceNotification("user-1")
	require.NoError(t, cs.CreateNotification(ctx, fresh))
	got, err := cs.GetNotification(ctx, fresh.ID)
	require.NoError(t, err)
	assert.Equal(t, store.NotificationKindArtifactGrant, got.Kind)
	assert.Equal(t, testArtifactID, got.ResourceID)
}

var resourceColumns = []string{"kind", "resource_type", "resource_id"}

func TestNotificationResourceFields_UpgradeFromLegacySchema_SQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "legacy-notifications.db")
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	execAll(t, raw, legacyNotificationsSQLite)
	require.NoError(t, raw.Close())

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))

	cols := sqliteNotificationColumns(t, cs)
	for _, col := range resourceColumns {
		require.Contains(t, cols, col)
		assert.True(t, cols[col].notNull, col)
		assert.Contains(t, cols[col].dflt.String, "''", col)
	}

	assertResourceFieldsUpgraded(t, cs)
}

// Runs under -tags integration with SCION_TEST_POSTGRES_URL set; skips
// otherwise.
func TestNotificationResourceFields_UpgradeFromLegacySchema_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping the Postgres notifications upgrade case")
	}
	dsn := enttest.NewEmptySchemaURL(t)
	raw, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	execAll(t, raw, legacyNotificationsPostgres)

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 3, MaxIdleConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))

	for _, col := range resourceColumns {
		nullable, dflt := postgresNotificationColumn(t, raw, col)
		assert.Equal(t, "NO", nullable, col)
		assert.Contains(t, dflt.String, "''", col)
	}

	assertResourceFieldsUpgraded(t, cs)
}
