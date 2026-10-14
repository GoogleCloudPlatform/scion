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

package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A co-located flat instance publishes its presentation from the internal
// heartbeat loop through the Hub's one writer: after one tick its row
// carries PresentInstance(entry).API(), with the hash the Hub computes for
// that presentation (the same helper the HTTP heartbeat path uses).
func TestFlatColocatedPresenter_PublishesPresentationOnTick(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	srv := flatTestHub(t, s, true)
	inst := flatTestInstance()
	prep, err := prepareFlatInstance(ctx, srv, runtime.NewDockerRuntime(), inst, nil, t.TempDir(),
		hub.EmbeddedFlatRegistrationOptions{Endpoint: "http://localhost:9800"}, fakeDockerProbe("daemon-1", nil))
	require.NoError(t, err)

	presenter := newFlatColocatedPresenter(srv, prep.identity.RuntimeBrokerID, prep.identity.RuntimeTarget.ID, inst, 1_000)
	require.NotNil(t, presenter)
	res, err := presenter.publish(ctx, time.Now())
	require.NoError(t, err)
	assert.Equal(t, hub.PresentationResult{Outcome: store.PresentationWritten}, res)

	want := config.PresentInstance(inst).API()
	row, err := s.GetRuntimeBroker(ctx, prep.identity.RuntimeBrokerID)
	require.NoError(t, err)
	require.NotNil(t, row.RuntimeTargetPresentation)
	assert.Equal(t, want, *row.RuntimeTargetPresentation)
	wantHash, err := hub.RuntimeTargetPresentationHash(&want)
	require.NoError(t, err)
	assert.Equal(t, wantHash, row.RuntimeTargetPresentationHash)
	require.NotNil(t, row.PresentationSeq)
	assert.Equal(t, int64(1), *row.PresentationSeq)

	// The next tick confirms the unchanged presentation with the next seq.
	res, err = presenter.publish(ctx, time.Now())
	require.NoError(t, err)
	assert.Equal(t, store.PresentationConfirmed, res.Outcome)
	row, err = s.GetRuntimeBroker(ctx, prep.identity.RuntimeBrokerID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), *row.PresentationSeq)
}

// With the experiment off, the co-located loop publishes nothing.
func TestFlatColocatedPresenter_ExperimentOffWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	on := flatTestHub(t, s, true)
	inst := flatTestInstance()
	prep, err := prepareFlatInstance(ctx, on, runtime.NewDockerRuntime(), inst, nil, t.TempDir(),
		hub.EmbeddedFlatRegistrationOptions{Endpoint: "http://localhost:9800"}, fakeDockerProbe("daemon-1", nil))
	require.NoError(t, err)

	off := flatTestHub(t, s, false)
	res, err := newFlatColocatedPresenter(off, prep.identity.RuntimeBrokerID, prep.identity.RuntimeTarget.ID, inst, 1_000).publish(ctx, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "experiment_off", res.Ignored)
	row, err := s.GetRuntimeBroker(ctx, prep.identity.RuntimeBrokerID)
	require.NoError(t, err)
	assert.Nil(t, row.RuntimeTargetPresentation)
	assert.Nil(t, row.PresentationEpoch)

	assert.Nil(t, newFlatColocatedPresenter(nil, "b", "t", inst, 1), "no Hub, no presenter")
}
