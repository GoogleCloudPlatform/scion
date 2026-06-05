package entadapter

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newOfflineBroker returns an unclaimed broker (offline, no affinity) so tests
// can observe the claim transition.
func newOfflineBroker(t *testing.T, ps *ProjectStore) *store.RuntimeBroker {
	t.Helper()
	b := newBroker()
	b.Status = store.BrokerStatusOffline
	require.NoError(t, ps.CreateRuntimeBroker(context.Background(), b))
	return b
}

func TestClaimRuntimeBrokerConnection_SetsAffinityAndOnline(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	b := newOfflineBroker(t, ps)

	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hub-1", "sess-1"))

	got, err := ps.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ConnectedHubID)
	assert.Equal(t, "hub-1", *got.ConnectedHubID)
	require.NotNil(t, got.ConnectedSessionID)
	assert.Equal(t, "sess-1", *got.ConnectedSessionID)
	require.NotNil(t, got.ConnectedAt)
	assert.False(t, got.ConnectedAt.IsZero())
	// Claim bumps status->online + refreshes heartbeat in the same write.
	assert.Equal(t, store.BrokerStatusOnline, got.Status)
	assert.False(t, got.LastHeartbeat.IsZero())
}

func TestClaimRuntimeBrokerConnection_NewestWins(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	b := newOfflineBroker(t, ps)

	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hub-1", "sess-1"))
	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hub-2", "sess-2"))

	got, err := ps.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ConnectedHubID)
	assert.Equal(t, "hub-2", *got.ConnectedHubID)
	require.NotNil(t, got.ConnectedSessionID)
	assert.Equal(t, "sess-2", *got.ConnectedSessionID)
}

func TestReleaseRuntimeBrokerConnection_ClearsWhenOwner(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	b := newOfflineBroker(t, ps)

	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hub-1", "sess-1"))

	cleared, err := ps.ReleaseRuntimeBrokerConnection(ctx, b.ID, "hub-1", "sess-1")
	require.NoError(t, err)
	assert.True(t, cleared)

	got, err := ps.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Nil(t, got.ConnectedHubID)
	assert.Nil(t, got.ConnectedSessionID)
	assert.Nil(t, got.ConnectedAt)
	// Release must NOT change status — the caller decides offline based on cleared.
	assert.Equal(t, store.BrokerStatusOnline, got.Status)
}

func TestReleaseRuntimeBrokerConnection_NoOpWhenAffinityMoved(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	b := newOfflineBroker(t, ps)

	// Affinity currently owned by (hub-2, sess-2).
	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hub-2", "sess-2"))

	// A stale owner (hub-1, sess-1) tries to release: must be a no-op.
	cleared, err := ps.ReleaseRuntimeBrokerConnection(ctx, b.ID, "hub-1", "sess-1")
	require.NoError(t, err)
	assert.False(t, cleared)

	got, err := ps.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ConnectedHubID)
	assert.Equal(t, "hub-2", *got.ConnectedHubID)
	require.NotNil(t, got.ConnectedSessionID)
	assert.Equal(t, "sess-2", *got.ConnectedSessionID)
}

func TestReleaseRuntimeBrokerConnection_NoOpWhenUnclaimed(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	b := newOfflineBroker(t, ps)

	cleared, err := ps.ReleaseRuntimeBrokerConnection(ctx, b.ID, "hub-1", "sess-1")
	require.NoError(t, err)
	assert.False(t, cleared)
}

// ---------------------------------------------------------------------------
// ReleaseAndMarkBrokerOffline — atomic release + offline stamp
// ---------------------------------------------------------------------------

func TestReleaseAndMarkBrokerOffline_StampsOffline(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	b := newOfflineBroker(t, ps)

	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hub-1", "sess-1"))

	cleared, err := ps.ReleaseAndMarkBrokerOffline(ctx, b.ID, "hub-1", "sess-1")
	require.NoError(t, err)
	assert.True(t, cleared)

	got, err := ps.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Nil(t, got.ConnectedHubID)
	assert.Nil(t, got.ConnectedSessionID)
	assert.Nil(t, got.ConnectedAt)
	assert.Equal(t, store.BrokerStatusOffline, got.Status)
	assert.False(t, got.LastHeartbeat.IsZero())
}

func TestReleaseAndMarkBrokerOffline_NoopOnSessionMismatch(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	b := newOfflineBroker(t, ps)

	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hub-1", "sess-NEW"))

	// Stale session tries to release+offline: must be a no-op.
	cleared, err := ps.ReleaseAndMarkBrokerOffline(ctx, b.ID, "hub-1", "sess-OLD")
	require.NoError(t, err)
	assert.False(t, cleared)

	got, err := ps.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ConnectedHubID)
	assert.Equal(t, "hub-1", *got.ConnectedHubID)
	require.NotNil(t, got.ConnectedSessionID)
	assert.Equal(t, "sess-NEW", *got.ConnectedSessionID)
	assert.Equal(t, store.BrokerStatusOnline, got.Status, "status must remain online")
}

// TestReleaseAndMarkBrokerOffline_NoopAfterReclaim reproduces the exact race
// from issue #131: old session releases + stamps offline, but a new session
// has already re-claimed the broker. The stale release must be a no-op.
func TestReleaseAndMarkBrokerOffline_NoopAfterReclaim(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	b := newOfflineBroker(t, ps)

	// t0: session A claims.
	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hub-1", "sess-A"))
	// t1: session A disconnects, but before the callback runs, session B re-claims.
	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hub-1", "sess-B"))

	// t2: stale callback tries to release+offline for session A.
	cleared, err := ps.ReleaseAndMarkBrokerOffline(ctx, b.ID, "hub-1", "sess-A")
	require.NoError(t, err)
	assert.False(t, cleared, "stale session must not stamp offline")

	got, err := ps.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ConnectedSessionID)
	assert.Equal(t, "sess-B", *got.ConnectedSessionID, "new session must still own the broker")
	assert.Equal(t, store.BrokerStatusOnline, got.Status, "status must remain online")
}

func TestReleaseAndMarkBrokerOffline_NoopWhenUnclaimed(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	b := newOfflineBroker(t, ps)

	cleared, err := ps.ReleaseAndMarkBrokerOffline(ctx, b.ID, "hub-1", "sess-1")
	require.NoError(t, err)
	assert.False(t, cleared)
}

// ---------------------------------------------------------------------------
// Flap / cross-hub scenarios
// ---------------------------------------------------------------------------

// TestBrokerAffinity_FlapAtoB reproduces the design §9.4 disconnect race: a
// broker flaps from hub A to hub B; A's delayed onDisconnect must NOT clobber
// B's live ownership.
func TestBrokerAffinity_FlapAtoB(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()
	b := newOfflineBroker(t, ps)

	// t0: socket on hub A (session s1).
	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hubA", "s1"))
	// t2: broker re-dials, lands on hub B (session s2); B claims (newest wins).
	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hubB", "s2"))

	// t3: hub A's old socket finally errors -> delayed release for (hubA, s1).
	cleared, err := ps.ReleaseRuntimeBrokerConnection(ctx, b.ID, "hubA", "s1")
	require.NoError(t, err)
	assert.False(t, cleared, "stale owner release must be a no-op")

	// Affinity still names B, status still online (no false offline).
	got, err := ps.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ConnectedHubID)
	assert.Equal(t, "hubB", *got.ConnectedHubID)
	require.NotNil(t, got.ConnectedSessionID)
	assert.Equal(t, "s2", *got.ConnectedSessionID)
	assert.Equal(t, store.BrokerStatusOnline, got.Status)
}
