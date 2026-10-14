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
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// presentationInterval is the default flat heartbeat interval the
// conformance cases step the clock by.
const presentationInterval = 30 * time.Second

// presentationFixture is one store with a flat and a legacy Runtime Broker
// row and a fixed clock origin.
type presentationFixture struct {
	s      store.Store
	flat   *store.RuntimeBroker
	legacy *store.RuntimeBroker
	t0     time.Time
}

func newPresentationFixture(t *testing.T, factory Factory) *presentationFixture {
	t.Helper()
	ctx := context.Background()
	s := factory(t)
	flatID := uuid.NewString()
	flat := &store.RuntimeBroker{
		ID: flatID, Name: "flat-" + flatID[:8], Slug: "flat-" + flatID[:8], Status: store.BrokerStatusOnline,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-" + flatID, Type: "kubernetes", DisplayName: "Cluster A"},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, flat))
	legacyID := uuid.NewString()
	legacy := &store.RuntimeBroker{
		ID: legacyID, Name: "legacy-" + legacyID[:8], Slug: "legacy-" + legacyID[:8], Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, legacy))
	// A whole second keeps every backend's timestamp precision exact.
	t0 := time.Now().UTC().Truncate(time.Second)
	return &presentationFixture{s: s, flat: flat, legacy: legacy, t0: t0}
}

func (f *presentationFixture) at(n int) time.Time {
	return f.t0.Add(time.Duration(n) * presentationInterval)
}

func (f *presentationFixture) set(t *testing.T, p *api.RuntimeTargetPresentation, hash string, epoch, seq int64, now time.Time) store.PresentationOutcome {
	t.Helper()
	out, err := f.s.SetRuntimeBrokerPresentation(context.Background(), f.flat.ID, f.flat.RuntimeTarget.ID, p, hash, epoch, seq, now)
	require.NoError(t, err)
	return out
}

func (f *presentationFixture) row(t *testing.T) *store.RuntimeBroker {
	t.Helper()
	b, err := f.s.GetRuntimeBroker(context.Background(), f.flat.ID)
	require.NoError(t, err)
	return b
}

// presentationFor returns a small presentation whose namespace tells
// snapshots apart.
func presentationFor(namespace string) *api.RuntimeTargetPresentation {
	evict := false
	return &api.RuntimeTargetPresentation{
		SchemaVersion: 1, Key: "k8s-1", Name: "k8s one",
		Target: &api.PresentedTarget{Type: "kubernetes", DisplayName: "Cluster A", Namespace: namespace, SafeToEvict: &evict},
	}
}

func requireInt64(t *testing.T, want int64, got *int64, what string) {
	t.Helper()
	require.NotNil(t, got, what)
	assert.Equal(t, want, *got, what)
}

// RuntimeBrokerPresentationConformance checks SetRuntimeBrokerPresentation
// (FC4-a presentation transport, ptone/scion#3282) on the backend the factory
// selects: SQLite by default, Postgres under -tags integration with
// SCION_TEST_POSTGRES_URL set (enttest.NewClient), so both backends give
// the same outcomes for the same conditional update.
func RuntimeBrokerPresentationConformance(t *testing.T, factory Factory) {
	t.Run("WriteConfirmAndObservedRefresh", func(t *testing.T) {
		f := newPresentationFixture(t, factory)
		p := presentationFor("agents")
		require.Equal(t, store.PresentationWritten, f.set(t, p, "h1", 100, 1, f.at(0)))
		got := f.row(t)
		assert.Equal(t, p, got.RuntimeTargetPresentation)
		assert.Equal(t, "h1", got.RuntimeTargetPresentationHash)
		require.NotNil(t, got.RuntimeTargetPresentationObservedAt)
		assert.True(t, f.at(0).Equal(*got.RuntimeTargetPresentationObservedAt), "observed at %v", got.RuntimeTargetPresentationObservedAt)
		requireInt64(t, 100, got.PresentationEpoch, "epoch")
		requireInt64(t, 1, got.PresentationSeq, "seq")

		// Same hash: confirmed. The fence advances; observed-at stays
		// until it is older than the refresh period.
		require.Equal(t, store.PresentationConfirmed, f.set(t, p, "h1", 100, 2, f.at(1)))
		got = f.row(t)
		requireInt64(t, 2, got.PresentationSeq, "seq after a confirm")
		require.NotNil(t, got.PresentationFencedAt)
		assert.True(t, f.at(1).Equal(*got.PresentationFencedAt), "a confirm sets fenced_at")
		assert.True(t, f.at(0).Equal(*got.RuntimeTargetPresentationObservedAt), "observed-at not refreshed within the period")

		later := f.t0.Add(store.PresentationObservedRefresh + time.Second)
		require.Equal(t, store.PresentationConfirmed, f.set(t, p, "h1", 100, 3, later))
		got = f.row(t)
		assert.True(t, later.Equal(*got.RuntimeTargetPresentationObservedAt), "observed-at refreshed after the period")
		assert.Equal(t, p, got.RuntimeTargetPresentation)

		// A new hash: written.
		p2 := presentationFor("agents-2")
		require.Equal(t, store.PresentationWritten, f.set(t, p2, "h2", 100, 4, later.Add(presentationInterval)))
		got = f.row(t)
		assert.Equal(t, p2, got.RuntimeTargetPresentation)
		assert.Equal(t, "h2", got.RuntimeTargetPresentationHash)
	})

	t.Run("LowerSeqAndLowerEpochAreStale", func(t *testing.T) {
		f := newPresentationFixture(t, factory)
		require.Equal(t, store.PresentationWritten, f.set(t, presentationFor("new"), "hb", 200, 5, f.at(0)))
		before := f.row(t)
		// A delayed retry of the same process (lower seq), a replay (same
		// seq) and an older process (lower epoch, any seq) write nothing.
		assert.Equal(t, store.PresentationStale, f.set(t, presentationFor("old"), "ha", 200, 4, f.at(1)))
		assert.Equal(t, store.PresentationStale, f.set(t, presentationFor("old"), "ha", 200, 5, f.at(1)))
		assert.Equal(t, store.PresentationStale, f.set(t, presentationFor("old"), "ha", 199, 1000, f.at(1)))
		after := f.row(t)
		assert.Equal(t, before.RuntimeTargetPresentation, after.RuntimeTargetPresentation)
		assert.Equal(t, "hb", after.RuntimeTargetPresentationHash)
		requireInt64(t, 200, after.PresentationEpoch, "epoch")
		requireInt64(t, 5, after.PresentationSeq, "seq")
		assert.True(t, f.at(0).Equal(*after.PresentationFencedAt), "a stale request leaves fenced_at")
	})

	t.Run("TwoWritersEndOnTheNewerEpoch", func(t *testing.T) {
		// Process A (epoch 1, an older process that has beaten for long,
		// so its sequence is far ahead) and process B (epoch 2, just
		// started) of one instance, with different snapshots, interleaved
		// in every order of their two beats each (each process's own beats
		// in order): the row ends on B's snapshot. The epoch, not the
		// sequence, decides between processes.
		type beat struct {
			proc  string
			epoch int64
			seq   int64
		}
		a1, a2, b1, b2 := beat{"A", 1, 101}, beat{"A", 1, 102}, beat{"B", 2, 1}, beat{"B", 2, 2}
		orders := [][]beat{
			{a1, a2, b1, b2}, {a1, b1, a2, b2}, {a1, b1, b2, a2},
			{b1, a1, a2, b2}, {b1, a1, b2, a2}, {b1, b2, a1, a2},
		}
		snap := map[string]*api.RuntimeTargetPresentation{"A": presentationFor("ns-a"), "B": presentationFor("ns-b")}
		hash := map[string]string{"A": "hash-a", "B": "hash-b"}
		for _, order := range orders {
			f := newPresentationFixture(t, factory)
			for i, b := range order {
				f.set(t, snap[b.proc], hash[b.proc], b.epoch, b.seq, f.at(i))
			}
			got := f.row(t)
			assert.Equal(t, snap["B"], got.RuntimeTargetPresentation, "order %v", order)
			assert.Equal(t, "hash-b", got.RuntimeTargetPresentationHash, "order %v", order)
			requireInt64(t, 2, got.PresentationEpoch, "epoch")
		}
	})

	t.Run("SteadyConfirmsKeepTheFenceFresh", func(t *testing.T) {
		// B writes once, then confirms an unchanged presentation every
		// interval. A lower-epoch write arriving 11 intervals in (more
		// than PresentationFenceRecovery after B's only hash write, but one
		// interval after B's last confirm) is still stale.
		f := newPresentationFixture(t, factory)
		pb := presentationFor("ns-b")
		require.Equal(t, store.PresentationWritten, f.set(t, pb, "hash-b", 2, 1, f.at(0)))
		for i := 1; i <= 10; i++ {
			require.Equal(t, store.PresentationConfirmed, f.set(t, pb, "hash-b", 2, int64(i+1), f.at(i)))
		}
		require.Greater(t, f.at(11).Sub(f.at(0)), store.PresentationFenceRecovery)
		assert.Equal(t, store.PresentationStale, f.set(t, presentationFor("ns-a"), "hash-a", 1, 1, f.at(11)))
		got := f.row(t)
		assert.Equal(t, pb, got.RuntimeTargetPresentation)
		requireInt64(t, 2, got.PresentationEpoch, "epoch")
	})

	t.Run("SilentFenceRecovers", func(t *testing.T) {
		// A stored epoch from a clock that ran ahead: once nothing has
		// advanced the fence for PresentationFenceRecovery, a lower epoch
		// is accepted (it is then the only live writer).
		f := newPresentationFixture(t, factory)
		require.Equal(t, store.PresentationWritten, f.set(t, presentationFor("ahead"), "hash-ahead", 9_000, 1, f.at(0)))
		within := f.t0.Add(store.PresentationFenceRecovery - time.Second)
		assert.Equal(t, store.PresentationStale, f.set(t, presentationFor("now"), "hash-now", 10, 1, within))
		after := f.t0.Add(store.PresentationFenceRecovery + time.Second)
		assert.Equal(t, store.PresentationWritten, f.set(t, presentationFor("now"), "hash-now", 10, 2, after))
		got := f.row(t)
		assert.Equal(t, "hash-now", got.RuntimeTargetPresentationHash)
		requireInt64(t, 10, got.PresentationEpoch, "epoch")
	})

	t.Run("RefusesLegacyRowsAndOtherTargets", func(t *testing.T) {
		f := newPresentationFixture(t, factory)
		ctx := context.Background()
		_, err := f.s.SetRuntimeBrokerPresentation(ctx, f.legacy.ID, "rt-any", presentationFor("x"), "h", 1, 1, f.at(0))
		assert.ErrorIs(t, err, store.ErrRuntimeBrokerNotFlat)
		legacy, err := f.s.GetRuntimeBroker(ctx, f.legacy.ID)
		require.NoError(t, err)
		assert.Nil(t, legacy.RuntimeTargetPresentation)
		assert.Nil(t, legacy.PresentationEpoch)

		_, err = f.s.SetRuntimeBrokerPresentation(ctx, f.flat.ID, "rt-other", presentationFor("x"), "h", 1, 1, f.at(0))
		assert.ErrorIs(t, err, store.ErrRuntimeTargetChanged)
		assert.Nil(t, f.row(t).RuntimeTargetPresentation)

		otherType := presentationFor("x")
		otherType.Target.Type = "docker"
		_, err = f.s.SetRuntimeBrokerPresentation(ctx, f.flat.ID, f.flat.RuntimeTarget.ID, otherType, "h", 1, 1, f.at(0))
		assert.ErrorIs(t, err, store.ErrRuntimeTargetChanged, "the reported type must equal the stored type")
		noTarget := presentationFor("x")
		noTarget.Target = nil
		_, err = f.s.SetRuntimeBrokerPresentation(ctx, f.flat.ID, f.flat.RuntimeTarget.ID, noTarget, "h", 1, 1, f.at(0))
		assert.ErrorIs(t, err, store.ErrRuntimeTargetChanged, "a presentation without a target is refused")
		assert.Nil(t, f.row(t).RuntimeTargetPresentation)

		_, err = f.s.SetRuntimeBrokerPresentation(ctx, uuid.NewString(), "rt-any", presentationFor("x"), "h", 1, 1, f.at(0))
		assert.ErrorIs(t, err, store.ErrNotFound)

		_, err = f.s.SetRuntimeBrokerPresentation(ctx, f.flat.ID, f.flat.RuntimeTarget.ID, nil, "h", 1, 1, f.at(0))
		assert.Error(t, err, "a nil presentation is refused")
		assert.Nil(t, f.row(t).PresentationEpoch)

		// An empty reported type is refused even against a flat row whose
		// stored type is empty too.
		untypedID := uuid.NewString()
		untyped := &store.RuntimeBroker{ID: untypedID, Name: "untyped-" + untypedID[:8], Slug: "untyped-" + untypedID[:8],
			RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-" + untypedID}}
		require.NoError(t, f.s.CreateRuntimeBroker(ctx, untyped))
		_, err = f.s.SetRuntimeBrokerPresentation(ctx, untypedID, "rt-"+untypedID, noTarget, "h", 1, 1, f.at(0))
		assert.ErrorIs(t, err, store.ErrRuntimeTargetChanged, "an empty reported type is never accepted")
		got, err := f.s.GetRuntimeBroker(ctx, untypedID)
		require.NoError(t, err)
		assert.Nil(t, got.RuntimeTargetPresentation)
	})

	t.Run("StaleCopyUpdateRuntimeBrokerKeepsPresentation", func(t *testing.T) {
		// A full-row write-back of a copy read BEFORE a presentation write
		// leaves all six presentation columns as that write set them.
		f := newPresentationFixture(t, factory)
		ctx := context.Background()
		stale := f.row(t)
		require.Nil(t, stale.RuntimeTargetPresentation)
		p := presentationFor("agents")
		require.Equal(t, store.PresentationWritten, f.set(t, p, "h1", 7, 3, f.at(0)))
		want := f.row(t)

		stale.Name = stale.Name + "-renamed"
		require.NoError(t, f.s.UpdateRuntimeBroker(ctx, stale))
		got := f.row(t)
		assert.Equal(t, stale.Name, got.Name, "the full-row write applied")
		assert.Equal(t, want.RuntimeTargetPresentation, got.RuntimeTargetPresentation)
		assert.Equal(t, want.RuntimeTargetPresentationHash, got.RuntimeTargetPresentationHash)
		assert.Equal(t, want.RuntimeTargetPresentationObservedAt, got.RuntimeTargetPresentationObservedAt)
		assert.Equal(t, want.PresentationEpoch, got.PresentationEpoch)
		assert.Equal(t, want.PresentationSeq, got.PresentationSeq)
		assert.Equal(t, want.PresentationFencedAt, got.PresentationFencedAt)
		require.NotNil(t, got.RuntimeTargetPresentation)

		// CreateRuntimeBroker does not write them either.
		id := uuid.NewString()
		withPresentation := &store.RuntimeBroker{
			ID: id, Name: "flat-" + id[:8], Slug: "flat-" + id[:8],
			RuntimeTarget:                 &api.RuntimeTargetDescriptor{ID: "rt-" + id, Type: "docker"},
			RuntimeTargetPresentation:     p,
			RuntimeTargetPresentationHash: "h1",
		}
		require.NoError(t, f.s.CreateRuntimeBroker(ctx, withPresentation))
		created, err := f.s.GetRuntimeBroker(ctx, id)
		require.NoError(t, err)
		assert.Nil(t, created.RuntimeTargetPresentation)
		assert.Empty(t, created.RuntimeTargetPresentationHash)
	})
}
