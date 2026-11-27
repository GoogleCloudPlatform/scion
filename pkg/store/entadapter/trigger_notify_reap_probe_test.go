//go:build !no_sqlite

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// P7a review probe: a notify row (action_type=notify, action_index=-1) left in
// "firing" by a hub crash mid-notification is reaped by ReapExpiredTriggerFires,
// which must still mark it failed without touching the trigger's error_count or
// last_fire_* (notify rows must be excluded from counters).
func TestP7aProbe_ReapedNotifyRowCountsAsTriggerError_DefectRepro(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("dialect=%s", cs.client.Driver().Dialect())

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projID, Name: "p7a-reap", Slug: "p7a-reap"}))
	trID := uuid.NewString()
	require.NoError(t, cs.CreateTrigger(ctx, &store.Trigger{
		ID: trID, ProjectID: projID, Name: "reap-trig",
		SourceKind: store.TriggerSourceKindWebhook, SourceID: uuid.NewString(),
		MatchTypes: []string{"webhook.received"},
		Actions:    []store.TriggerAction{{Type: store.TriggerActionMessage, Payload: `{"agentId":"` + uuid.NewString() + `","message":"hi"}`}},
		Status:     store.TriggerStatusActive,
	}))
	before, err := cs.GetTrigger(ctx, trID)
	require.NoError(t, err)

	past := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	notify := &store.TriggerFire{
		ID: uuid.NewString(), ProjectID: projID, TriggerID: trID,
		EventID: uuid.NewString(), Subject: "notify:x",
		ActionType: store.TriggerActionNotify, ActionIndex: store.TriggerFireActionIndexNotify,
		Status: store.TriggerFireStatusFiring, LeaseUntil: past, StartedAt: past, CreatedAt: past,
	}
	claimed, _, err := cs.ClaimTriggerFire(ctx, notify)
	require.NoError(t, err)
	require.True(t, claimed)

	n, err := cs.ReapExpiredTriggerFires(ctx, time.Now().UTC(), 100)
	require.NoError(t, err)
	t.Logf("reaped=%d", n)
	require.Equal(t, 1, n, "expired notify row must still be reaped")

	reapedFire, err := cs.GetTriggerFire(ctx, notify.ID)
	require.NoError(t, err)
	require.Equal(t, store.TriggerFireStatusFailed, reapedFire.Status, "reaped notify row must be marked failed")

	after, err := cs.GetTrigger(ctx, trID)
	require.NoError(t, err)
	t.Logf("errorCount before=%d after=%d lastFireStatus=%q lastFireError=%q",
		before.ErrorCount, after.ErrorCount, after.LastFireStatus, after.LastFireError)
	// Correct behaviour: notify rows never touch trigger counters.
	require.Equal(t, before.ErrorCount, after.ErrorCount, "reaped notify row must not count as a trigger error")
	require.Equal(t, before.LastFireStatus, after.LastFireStatus, "reaped notify row must not overwrite lastFireStatus")
	require.Equal(t, before.LastFireError, after.LastFireError, "reaped notify row must not overwrite lastFireError")
}
