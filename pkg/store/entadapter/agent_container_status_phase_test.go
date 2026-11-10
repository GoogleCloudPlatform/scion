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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The store clears a container status that a phase change outlives: a
// write that moves the phase between live and at rest without a container
// status of its own (ptone/scion#3607). A write that carries one sets it,
// a phase change within live or within at rest keeps it, and stop_queued
// is never cleared here.
func TestUpdateAgentStatus_PhaseChangeClearsOutlivedContainerStatus(t *testing.T) {
	cases := []struct {
		name       string
		fromPhase  string
		fromStatus string
		update     store.AgentStatusUpdate
		wantPhase  string
		wantStatus string
	}{
		{"live to at rest clears", string(state.PhaseRunning), "Up 5 minutes",
			store.AgentStatusUpdate{Phase: string(state.PhaseStopped)}, string(state.PhaseStopped), ""},
		{"at rest to live clears", string(state.PhaseStopped), "exited (0)",
			store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}, string(state.PhaseStarting), ""},
		{"an explicit status is stored", string(state.PhaseRunning), "Up 5 minutes",
			store.AgentStatusUpdate{Phase: string(state.PhaseStopped), ContainerStatus: "stopped"}, string(state.PhaseStopped), "stopped"},
		{"within live keeps it", string(state.PhaseStarting), "Up 1 second",
			store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}, string(state.PhaseRunning), "Up 1 second"},
		{"within at rest keeps it", string(state.PhaseStopped), "exited (1)",
			store.AgentStatusUpdate{Phase: string(state.PhaseError)}, string(state.PhaseError), "exited (1)"},
		{"stop_queued is kept", string(state.PhaseStopped), store.ContainerStatusStopQueued,
			store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}, string(state.PhaseStarting), store.ContainerStatusStopQueued},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, projectID := newTestAgentStore(t)
			a := makeAgent(projectID, "cs-clear")
			a.Phase, a.Activity, a.ContainerStatus = tc.fromPhase, "", tc.fromStatus
			require.NoError(t, s.CreateAgent(ctx, a))

			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, tc.update))

			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPhase, got.Phase)
			assert.Equal(t, tc.wantStatus, got.ContainerStatus)
		})
	}
}

func TestContainerStatusOutlived(t *testing.T) {
	cases := []struct {
		name                       string
		current, next, currentStat string
		want                       bool
	}{
		{"live to at rest", "running", "stopped", "Up 5 minutes", true},
		{"at rest to live", "stopped", "starting", "exited (0)", true},
		{"live to suspended", "running", "suspended", "running", true},
		{"halted to provisioning", "halted", "provisioning", "exited (137)", true},
		{"within live", "starting", "running", "Up 1 second", false},
		{"within at rest", "stopped", "error", "exited (1)", false},
		{"no phase in the write", "running", "", "Up 5 minutes", false},
		{"same phase", "running", "running", "Up 5 minutes", false},
		{"no stored status", "running", "stopped", "", false},
		{"stop_queued", "stopped", "starting", store.ContainerStatusStopQueued, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, containerStatusOutlived(tc.current, tc.next, tc.currentStat))
		})
	}
}
