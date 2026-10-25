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
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// TestLifecycleAttribution_HubSoftFinishRestoreE2E (WP1 attribution design
// v2.8, section 10.4): the hub's delete engine with the lifecycle observer
// installed. A plain delete of run-a is refused because the broker holds
// run-p, one of the row's previous runs; the engine deletes run-p and the
// soft finish records it as run_id. The restore (restoreAgentTx), a
// failed start's mint and the dispatcher's revert back to run-p follow.
// No event ever names run-p: the deleted and restored name run-a, the
// before run, and the revert to the quarantined run-p restores the held
// attribution (the undo), so the final hard delete names run-a too.
func TestLifecycleAttribution_HubSoftFinishRestoreE2E(t *testing.T) {
	srv, s := testServer(t)
	ls, ok := s.(lifecycleEventStore)
	require.True(t, ok)
	srv.config.HubIDPinned = true
	srv.config.SoftDeleteRetention = time.Hour
	sink, counters := telemetryemit.NewMemorySink(), telemetryemit.NewMemoryCounters()
	srv.SetLifecycleEventsSink(sink, counters)
	st, err := srv.installLifecycleObserver()
	require.NoError(t, err)
	require.Equal(t, telemetryemit.ObserverStateEnabled, st)

	client := &runMismatchDeleteClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, answer: answerHeld(t, "run-p", answerOK)}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	agent := setupBrokerAgentInPhase(t, s, "attr-e2e", state.PhaseRunning)
	ctx := context.Background()
	_, err = s.SetAgentRunID(ctx, agent.ID, "run-p", nil)
	require.NoError(t, err)
	_, err = s.SetAgentRunID(ctx, agent.ID, "run-a", nil)
	require.NoError(t, err)
	f := &runMismatchFixture{srv: srv, store: s, client: client, agent: mustGetAgent(t, s, agent.ID)}

	r := f.del(t, "")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	got := mustGetAgent(t, s, agent.ID)
	require.False(t, got.DeletedAt.IsZero(), "soft-deleted")
	require.Equal(t, "run-p", got.RunID, "the soft finish recorded the broker-held run")
	assert.Equal(t, []string{"run-a", "run-p", "run-a"}, client.deleted(), "the recorded run, the reported run, then the recorded run as a previous run")

	require.NoError(t, srv.restoreAgentTx(ctx, got, AuditActor{}))
	_, err = s.SetAgentRunID(ctx, agent.ID, "run-x", nil)
	require.NoError(t, err)
	ok, err = s.RevertAgentRunID(ctx, agent.ID, "run-x", "run-p")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, s.DeleteAgent(ctx, agent.ID))

	evs := lcEvents(t, ls, sink, agent.ID)
	var seq []string
	for _, e := range evs {
		assert.NotEqual(t, "run-p", e.LaunchID, "%s@%d names the broker-held run", e.Transition, e.TransitionRevision)
		assert.NotEqual(t, "run-p", e.AdoptedFromLaunchID)
		if e.Transition == tc.TransitionDeleted || e.Transition == tc.TransitionRestored {
			seq = append(seq, e.Transition+"["+e.LaunchID+"]")
		}
	}
	assert.Equal(t, []string{"deleted[run-a]", "restored[run-a]", "deleted[run-a]"}, seq)
}
