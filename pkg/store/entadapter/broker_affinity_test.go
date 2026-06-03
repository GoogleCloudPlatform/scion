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
