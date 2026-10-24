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

package hub

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// TestLifecycleEvents_HeldEditFoldAtStart (WP1 upmerge-2 r1 O2): with
// hub.agent_edit_held on and the lifecycle observer installed, a held model
// edit of a running agent is applied by the next start
// (foldPendingConfigAtStart -> store.UpdateAgentConfig). Over the whole
// flow (held save, stop, start): exactly one metadata_changed, carrying the
// applied model; no configured; no resynced.
func TestLifecycleEvents_HeldEditFoldAtStart(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setHeldEditsExperiment(t, srv, true)
	ls, ok := s.(lifecycleEventStore)
	require.True(t, ok)
	srv.config.HubIDPinned = true
	sink := telemetryemit.NewMemorySink()
	srv.SetLifecycleEventsSink(sink, telemetryemit.NewMemoryCounters())
	st, err := srv.installLifecycleObserver()
	require.NoError(t, err)
	require.Equal(t, telemetryemit.ObserverStateEnabled, st)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseRunning)

	resp := patchPending(t, srv, agent.ID, map[string]interface{}{
		"config":       map[string]interface{}{"model": "new-model"},
		"stateVersion": agent.StateVersion,
	})
	require.Equal(t, []string{"config.model"}, resp.Disposition.Held)
	pendingLifecycleWarnings(t, srv, agent.ID, "stop")
	pendingLifecycleWarnings(t, srv, agent.ID, "start")

	after := pendingReloadAgent(t, s, agent.ID)
	require.Nil(t, after.PendingConfig, "the start applied the held edit")
	require.Equal(t, "new-model", after.AppliedConfig.Model)

	evs := lcEvents(t, ls, sink, agent.ID)
	var changed []*tc.LifecycleEvent
	for _, e := range evs {
		if e.Transition == tc.TransitionMetadataChanged {
			changed = append(changed, e)
		}
	}
	require.Len(t, changed, 1, "exactly one metadata_changed: %s", lcSeqFull(evs))
	require.NotNil(t, changed[0].Metadata)
	assert.Equal(t, "new-model", changed[0].Metadata.RequestedModel)
	assert.Zero(t, countOf(evs, tc.TransitionConfigured), "never configured: %s", lcSeqFull(evs))
	assert.Zero(t, countOf(evs, tc.TransitionResynced), "no resynced: %s", lcSeqFull(evs))
}
