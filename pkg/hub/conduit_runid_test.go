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
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setRunID sets the agent row's run id to runID ("" clears it, as a
// revert to a run-less row or a broker reporting an unlabelled entry does)
// and refreshes f.launched.
func (f *relayFixture) setRunID(t *testing.T, runID string) {
	t.Helper()
	ctx := context.Background()
	if runID == "" {
		ok, err := f.store.CompareAndSwapAgentRunID(ctx, f.launched.ID, f.launched.RunID, "")
		require.NoError(t, err)
		require.True(t, ok)
	} else {
		_, err := f.store.SetAgentRunID(ctx, f.launched.ID, runID)
		require.NoError(t, err)
	}
	a, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	f.launched = a
}

func (f *relayFixture) agentSessions(t *testing.T) []registry.SessionRecord {
	t.Helper()
	ps, err := f.regStore.ListPrincipalSessions(context.Background(), registry.PrincipalAgent, f.launched.ID)
	require.NoError(t, err)
	out := make([]registry.SessionRecord, 0, len(ps.Sessions))
	for _, s := range ps.Sessions {
		out = append(out, s.Session)
	}
	return out
}

// TestConduitAdmission_RunIDCompare pins the admission compare against
// agents.run_id, including the empty-id rules: a row without a run id
// admits only a Hello without a launch id (as gen-N), and a Hello without
// a launch id is refused while a session admitted with the current run id
// is live.
func TestConduitAdmission_RunIDCompare(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rowRunID     string // "" clears the row's run id; "current" keeps the fixture's
		connectFirst bool   // a session with the current run id is live first
		presented    string // "current" presents the row's run id
		wantReason   string // "" means admitted
		wantSource   string
	}{
		{name: "row empty, Hello empty: generation fallback", rowRunID: "", presented: "", wantSource: relay.IncarnationSourceGeneration},
		{name: "row empty, Hello with launch id: refused", rowRunID: "", presented: "some-run", wantReason: relay.ReasonSupersededIncarnation},
		{name: "row set, Hello empty, nothing connected: generation fallback", rowRunID: "current", presented: "", wantSource: relay.IncarnationSourceGeneration},
		{name: "row and Hello differ: refused", rowRunID: "current", presented: "other-run", wantReason: relay.ReasonSupersededIncarnation},
		{name: "row and Hello match: admitted", rowRunID: "current", presented: "current", wantSource: relay.IncarnationSourceLaunchID},
		{name: "Hello empty while the current run is connected: refused", rowRunID: "current", connectFirst: true, presented: "", wantReason: relay.ReasonLegacyHelloSuperseded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelayFixture(t, nil)
			tok := f.agentToken(t, f.launched)
			if tc.connectFirst {
				_, _, err := f.dial(t, tok, f.launched.RunID)
				require.NoError(t, err)
			}
			if tc.rowRunID == "" {
				f.setRunID(t, "")
			}
			presented := tc.presented
			if presented == "current" {
				presented = f.launched.RunID
			}
			_, wel, err := f.dial(t, tok, presented)
			if tc.wantReason != "" {
				require.Error(t, err)
				var ce *conduit.CloseError
				require.ErrorAs(t, err, &ce)
				assert.Equal(t, relay.CloseSupersededIncarnation, ce.Code)
				assert.Equal(t, tc.wantReason, ce.Reason)
				return
			}
			require.NoError(t, err)
			want := presented
			if tc.wantSource == relay.IncarnationSourceGeneration {
				want = "gen-" + strconv.Itoa(f.launched.Generation)
			}
			assert.Equal(t, want, wel.GetEndpointIncarnation())
			var found bool
			for _, s := range f.agentSessions(t) {
				if s.SessionID == wel.GetSessionId() {
					found = true
					assert.Equal(t, tc.wantSource, s.Capabilities.IncarnationSource)
				}
			}
			assert.True(t, found, "admitted session has a row")
		})
	}
}

// TestConduitAdmission_ZombieRefused: after a restart mints a new run id,
// a container of the superseded run that reconnects after its successor
// is refused 4409, and the successor's session stays the agent's route.
func TestConduitAdmission_ZombieRefused(t *testing.T) {
	f := newRelayFixture(t, nil)
	tok := f.agentToken(t, f.launched)
	oldRun := f.launched.RunID
	_, _, err := f.dial(t, tok, oldRun)
	require.NoError(t, err)

	newRun := uuid.NewString()
	f.setRunID(t, newRun)
	_, succ, err := f.dial(t, tok, newRun)
	require.NoError(t, err)

	_, _, err = f.dial(t, tok, oldRun)
	require.Error(t, err)
	assert.True(t, relay.IsSupersededIncarnation(err), "zombie: %v", err)

	// Routing asks only for the current run's incarnation (then the
	// generation fallback), so the superseded run's session is never picked.
	for _, inc := range relay.RouteAgentIncarnations(agentIncarnationFacts(f.launched)) {
		assert.NotEqual(t, oldRun, inc.Value)
	}
	var routable []string
	for _, s := range f.agentSessions(t) {
		if s.EndpointIncarnation == newRun && s.ConnectionEpoch == succ.GetConnectionEpoch() {
			routable = append(routable, s.SessionID)
		}
	}
	assert.Equal(t, []string{succ.GetSessionId()}, routable, "the successor's session is current")
}

// TestConduitAdmission_ReusedContainerAdoptedRunID: a start that finds the
// agent's container still running keeps it. The container carries its own
// run id, so while the row holds the newly minted id the container is
// refused, and once the hub adopts the run id the broker reported it is
// admitted again.
func TestConduitAdmission_ReusedContainerAdoptedRunID(t *testing.T) {
	f := newRelayFixture(t, nil)
	tok := f.agentToken(t, f.launched)
	containerRun := f.launched.RunID
	ctx := context.Background()

	d := NewHTTPAgentDispatcherWithClient(f.store, nil, false, slog.Default())
	agent := *f.launched
	minted, _, _, err := d.beginRun(ctx, &agent)
	require.NoError(t, err)
	require.NotEqual(t, containerRun, minted)

	_, _, err = f.dial(t, tok, containerRun)
	require.Error(t, err, "in-flux window: the row holds the minted id")
	assert.True(t, relay.IsSupersededIncarnation(err), "%v", err)

	d.adoptBrokerRunID(ctx, &agent, minted, &RemoteAgentResponse{Agent: &RemoteAgentInfo{RunID: containerRun}})
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	require.Equal(t, containerRun, got.RunID, "the broker's run id is adopted")

	_, wel, err := f.dial(t, tok, containerRun)
	require.NoError(t, err)
	assert.Equal(t, containerRun, wel.GetEndpointIncarnation())
}

// TestConduitAdmission_RuntimeLocalRestart: a runtime-local restart of the
// same container (a restart policy) is not a new launch. The container
// redials with the same run id and is admitted with a higher connection
// epoch, which fences its earlier connection.
func TestConduitAdmission_RuntimeLocalRestart(t *testing.T) {
	f := newRelayFixture(t, nil)
	tok := f.agentToken(t, f.launched)
	_, first, err := f.dial(t, tok, f.launched.RunID)
	require.NoError(t, err)

	_, second, err := f.dial(t, tok, f.launched.RunID)
	require.NoError(t, err)
	assert.Equal(t, f.launched.RunID, second.GetEndpointIncarnation())
	assert.Greater(t, second.GetConnectionEpoch(), first.GetConnectionEpoch())

	ps, err := f.regStore.ListPrincipalSessions(context.Background(), registry.PrincipalAgent, f.launched.ID)
	require.NoError(t, err)
	var current []string
	for _, s := range ps.Sessions {
		if registry.EpochCurrent(ps, s.Session) {
			current = append(current, s.Session.SessionID)
		}
	}
	assert.Equal(t, []string{second.GetSessionId()}, current, "only the redial is epoch-current; the first session is fenced")
	assert.NotEqual(t, first.GetSessionId(), second.GetSessionId())
}
