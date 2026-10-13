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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestFlatRow_StaleSweepMarksOfflineKeepsTarget: the heartbeat-timeout sweep
// (MarkStaleBrokersOffline, the guaranteed backstop that marks an unhosted
// Runtime Broker offline) treats a FLAT Runtime Broker row like any other:
// a flat row whose heartbeat is older than the threshold becomes offline,
// its connection columns are cleared and its project provider record goes
// offline, while its runtime target (ID, type, display name), its embedded
// label and its flat-ness are kept. A flat row with a fresh heartbeat is
// left online and connected (ptone/scion#3269, the Connection indicator).
//
// The name matches the TestFlatRow_ filter of make
// test-launch-store-postgres, so it also runs on Postgres.
func TestFlatRow_StaleSweepMarksOfflineKeepsTarget(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)

	staleTarget := dockerTarget()
	stale := flatBroker("flat-stale-sweep", store.BrokerStatusOnline, staleTarget)
	stale.Labels = embeddedLabels()
	fs.createBroker(t, stale)
	require.NoError(t, fs.brokers.ClaimRuntimeBrokerConnection(ctx, stale.ID, "hub-a", "session-stale"))
	// The claim stamps a fresh heartbeat; age it past the threshold, as an
	// unhosted Runtime Broker's row ages when no heartbeat arrives.
	require.NoError(t, fs.client.RuntimeBroker.UpdateOneID(uuid.MustParse(stale.ID)).
		SetLastHeartbeat(time.Now().Add(-20*time.Minute)).Exec(ctx))
	require.NoError(t, fs.brokers.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: fs.projectID, BrokerID: stale.ID, BrokerName: stale.Name, Status: store.BrokerStatusOnline,
	}))

	freshTarget := dockerTarget()
	fresh := flatBroker("flat-fresh-sweep", store.BrokerStatusOnline, freshTarget)
	fs.createBroker(t, fresh)
	require.NoError(t, fs.brokers.ClaimRuntimeBrokerConnection(ctx, fresh.ID, "hub-a", "session-fresh"))

	ids, err := fs.brokers.MarkStaleBrokersOffline(ctx, time.Now().Add(-5*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, []string{stale.ID}, ids, "only the stale flat row is swept")

	got, err := fs.brokers.GetRuntimeBroker(ctx, stale.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOffline, got.Status, "a stale flat row is marked offline")
	assert.Nil(t, got.ConnectedHubID, "connection columns cleared")
	assert.Nil(t, got.ConnectedSessionID, "connection columns cleared")
	assert.Nil(t, got.ConnectedAt, "connection columns cleared")
	require.NotNil(t, got.RuntimeTarget, "the runtime target is kept")
	assert.Equal(t, staleTarget.ID, got.RuntimeTarget.ID)
	assert.Equal(t, staleTarget.Type, got.RuntimeTarget.Type)
	assert.Equal(t, staleTarget.DisplayName, got.RuntimeTarget.DisplayName)
	assert.True(t, got.IsFlat(), "an offline flat row is still a flat row")
	assert.Equal(t, "embedded", got.Labels["scion.io/broker-role"], "the embedded label is kept")
	assert.Empty(t, got.Profiles)

	provider, err := fs.brokers.GetProjectProvider(ctx, fs.projectID, stale.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOffline, provider.Status, "the project provider record goes offline too")

	still, err := fs.brokers.GetRuntimeBroker(ctx, fresh.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOnline, still.Status, "a flat row with a fresh heartbeat stays online")
	require.NotNil(t, still.ConnectedSessionID)
	assert.Equal(t, "session-fresh", *still.ConnectedSessionID)
	require.NotNil(t, still.RuntimeTarget)
	assert.Equal(t, freshTarget.ID, still.RuntimeTarget.ID)
}
