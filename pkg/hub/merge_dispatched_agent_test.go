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

package hub

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestMergeDispatchedAgent_ExitFields pins mergeDispatchedAgent's exit-field
// copy, used by the version-conflict retry in the create-resume (stopped/error)
// path: a clear performed on the in-memory src agent before dispatch must
// survive the retry's merge onto a freshly re-read dst, but only while dst
// is non-terminal and src reports running — mirroring the other
// running-phase fields the merge already carries (Activity, ContainerStatus,
// RuntimeState).
func TestMergeDispatchedAgent_ExitFields(t *testing.T) {
	t.Run("running dst and running src: src's clear is carried through", func(t *testing.T) {
		ec := 137
		dst := &store.Agent{Phase: string(state.PhaseRunning), ExitReason: "preempted", ExitCode: &ec}
		src := &store.Agent{Phase: string(state.PhaseRunning), ExitReason: "", ExitCode: nil}

		mergeDispatchedAgent(dst, src)

		if dst.ExitReason != "" {
			t.Errorf("expected ExitReason cleared, got %q", dst.ExitReason)
		}
		if dst.ExitCode != nil {
			t.Errorf("expected ExitCode cleared, got %v", *dst.ExitCode)
		}
	})

	t.Run("terminal dst: its phase and reason are both kept", func(t *testing.T) {
		dst := &store.Agent{Phase: string(state.PhaseStopped), ExitReason: "crashed"}
		src := &store.Agent{Phase: string(state.PhaseRunning), ExitReason: "", ExitCode: nil}

		mergeDispatchedAgent(dst, src)

		if dst.Phase != string(state.PhaseStopped) {
			t.Errorf("expected phase to stay stopped, got %q", dst.Phase)
		}
		if dst.ExitReason != "crashed" {
			t.Errorf("expected ExitReason to stay crashed, got %q", dst.ExitReason)
		}
	})

	t.Run("non-terminal dst but src not running: dst's reason is kept", func(t *testing.T) {
		dst := &store.Agent{Phase: string(state.PhaseRunning), ExitReason: "evicted"}
		src := &store.Agent{Phase: string(state.PhaseProvisioning)}

		mergeDispatchedAgent(dst, src)

		if dst.ExitReason != "evicted" {
			t.Errorf("expected ExitReason to stay evicted, got %q", dst.ExitReason)
		}
	})
}
