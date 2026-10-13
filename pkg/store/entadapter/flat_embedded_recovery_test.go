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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func embeddedLabels() map[string]string { return map[string]string{"scion.io/broker-role": "embedded"} }

// TestFindEmbeddedBroker_SkipsFlatRows: the legacy broker-ID recovery never
// selects a flat Runtime Broker row, even one labelled as the embedded
// Runtime Broker; a legacy embedded row next to it is still recovered.
func TestFindEmbeddedBroker_SkipsFlatRows(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)

	flat := flatBroker("flat-embedded", store.BrokerStatusOnline, dockerTarget())
	flat.Labels = embeddedLabels()
	fs.createBroker(t, flat)

	got, err := fs.brokers.FindEmbeddedBroker(ctx)
	require.NoError(t, err)
	assert.Nil(t, got, "a flat embedded row is never recovered as the legacy identity")

	legacy := flatBroker("legacy-embedded", store.BrokerStatusOffline, nil)
	legacy.Labels = embeddedLabels()
	fs.createBroker(t, legacy)

	got, err = fs.brokers.FindEmbeddedBroker(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, legacy.ID, got.ID, "the legacy embedded row is recovered; the flat row is not a candidate")
}

// TestFlatRow_EmbeddedLabelAndTargetSurviveEveryWriter: a flat Runtime
// Broker row labelled as the embedded Runtime Broker keeps both its
// scion.io/broker-role=embedded label and its runtime target columns
// (runtime_target_id, runtime_target_type, runtime_target_display_name)
// across the legacy broker-ID recovery read and every Runtime Broker row
// writer: the full-row UpdateRuntimeBroker (with a stale model carrying no
// target and leftover profiles) and the status, heartbeat, connection,
// staleness and created-by writers. Only SetRuntimeBrokerTarget changes the
// display name, and nothing changes the target ID or type.
//
// This test runs on SQLite and on Postgres (make test-launch-store-postgres)
// and must never be dropped, skipped or weakened. Every new Runtime Broker
// row writer must be added to it.
func TestFlatRow_EmbeddedLabelAndTargetSurviveEveryWriter(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	target := dockerTarget()
	flat := flatBroker("flat-embedded-writers", store.BrokerStatusOnline, target)
	flat.Labels = embeddedLabels()
	created := fs.createBroker(t, flat)

	wantDisplay := target.DisplayName
	check := func(t *testing.T, step string) {
		t.Helper()
		got, err := fs.brokers.GetRuntimeBroker(ctx, created.ID)
		require.NoError(t, err, step)
		require.NotNil(t, got.RuntimeTarget, "%s: runtime target columns cleared", step)
		assert.Equal(t, target.ID, got.RuntimeTarget.ID, "%s: runtime_target_id", step)
		assert.Equal(t, target.Type, got.RuntimeTarget.Type, "%s: runtime_target_type", step)
		assert.Equal(t, wantDisplay, got.RuntimeTarget.DisplayName, "%s: runtime_target_display_name", step)
		assert.Equal(t, "embedded", got.Labels["scion.io/broker-role"], "%s: embedded label", step)
		assert.Empty(t, got.Profiles, "%s: profiles never persist on a flat row", step)
	}
	check(t, "create")

	// Legacy broker-ID recovery (read-only; never returns the flat row).
	rec, err := fs.brokers.FindEmbeddedBroker(ctx)
	require.NoError(t, err)
	assert.Nil(t, rec)
	check(t, "FindEmbeddedBroker")

	// Full-row write from a stale model with no target and leftover profiles.
	stale := *created
	stale.RuntimeTarget = nil
	stale.Profiles = []store.BrokerProfile{{Name: "local", Type: "docker"}}
	stale.DefaultProfile = "local"
	stale.Status = store.BrokerStatusOffline
	require.NoError(t, fs.brokers.UpdateRuntimeBroker(ctx, &stale))
	check(t, "UpdateRuntimeBroker (stale model)")

	require.NoError(t, fs.brokers.UpdateRuntimeBrokerHeartbeat(ctx, created.ID, store.BrokerStatusOnline))
	check(t, "UpdateRuntimeBrokerHeartbeat")
	require.NoError(t, fs.brokers.ClaimRuntimeBrokerConnection(ctx, created.ID, "hub-a", "session-1"))
	check(t, "ClaimRuntimeBrokerConnection")
	_, err = fs.brokers.ReleaseRuntimeBrokerConnection(ctx, created.ID, "hub-a", "session-1")
	require.NoError(t, err)
	check(t, "ReleaseRuntimeBrokerConnection")
	require.NoError(t, fs.brokers.ClaimRuntimeBrokerConnection(ctx, created.ID, "hub-a", "session-2"))
	_, err = fs.brokers.ReleaseAndMarkBrokerOffline(ctx, created.ID, "hub-a", "session-2")
	require.NoError(t, err)
	check(t, "ReleaseAndMarkBrokerOffline")
	require.NoError(t, fs.brokers.MarkBrokerOffline(ctx, created.ID))
	check(t, "MarkBrokerOffline")
	_, err = fs.brokers.ReapStaleBrokerAffinity(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	check(t, "ReapStaleBrokerAffinity")
	_, err = fs.brokers.MarkStaleBrokersOffline(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	check(t, "MarkStaleBrokersOffline")
	_, err = fs.brokers.SetRuntimeBrokerCreatedByIfEmpty(ctx, created.ID, "someone")
	require.NoError(t, err)
	check(t, "SetRuntimeBrokerCreatedByIfEmpty")

	// The only target writer: display name only, same ID and type.
	_, err = fs.brokers.SetRuntimeBrokerTarget(ctx, created.ID, api.RuntimeTargetDescriptor{ID: target.ID, Type: target.Type, DisplayName: "Renamed Docker"})
	require.NoError(t, err)
	wantDisplay = "Renamed Docker"
	check(t, "SetRuntimeBrokerTarget (display name)")
	_, err = fs.brokers.SetRuntimeBrokerTarget(ctx, created.ID, api.RuntimeTargetDescriptor{ID: "other", Type: target.Type})
	assert.ErrorIs(t, err, store.ErrRuntimeTargetChanged)
	check(t, "SetRuntimeBrokerTarget (different target refused)")
}
