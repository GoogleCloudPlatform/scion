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
	"context"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// legacyResumedPhase is a non-standard phase value that a resuming agent's
// broker may report instead of the canonical state.PhaseRunning; pkg/agent/run.go
// no longer writes it. "resumed" was never a valid state.Phase, so it is not
// part of the state package's enum — but a broker running an older build
// during a rolling upgrade can still surface it for a short window after a
// resume. Treat it as transitional, equivalent to starting/running, so
// readiness polling does not fail a healthy resume just because of version
// skew between the hub and an individual broker. Safe to remove once all
// brokers run a build containing this fix.
const legacyResumedPhase = "resumed"

// waitForAgentReady polls the agent store until the agent accepts input
// after a resume (state.AgentState.AcceptsInput), or until timeout.
//
// An agent that is still initializing when the timeout ends (the container
// is up and reported running/initializing, but its harness has not shown it
// reads input yet) is treated as ready: the message goes into the broker's
// message buffer, which waits for harness readiness before pasting. This
// keeps a slow harness start from turning into a rejected message. Only an
// agent that never reported any activity times out, as before.
func (s *Server) waitForAgentReady(ctx context.Context, agentID string, timeout time.Duration) error {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	initializing := false
	for {
		select {
		case <-ctx.Done():
			if initializing && parent.Err() == nil {
				s.messageLog.Info("wake: agent still initializing at readiness timeout; delivering into broker buffer",
					"agent_id", agentID, "timeout", timeout)
				return nil
			}
			return fmt.Errorf("timed out waiting for agent to become ready")
		case <-ticker.C:
			agent, err := s.store.GetAgent(ctx, agentID)
			if err != nil {
				if ctx.Err() != nil {
					continue // the timeout case above decides
				}
				return fmt.Errorf("failed to check agent status: %w", err)
			}

			phase := state.Phase(agent.Phase)
			if phase != state.PhaseStarting && phase != state.PhaseRunning && agent.Phase != legacyResumedPhase {
				return fmt.Errorf("agent entered unexpected phase %q while waiting for readiness", agent.Phase)
			}

			activity := state.Activity(agent.Activity)
			if (state.AgentState{Phase: phase, Activity: activity}).AcceptsInput() {
				return nil
			}
			// Brokers that report a transitional phase (starting, or the
			// legacy "resumed") with a real activity counted as ready
			// before the initializing activity existed; keep that.
			if activity != "" && activity != state.ActivityInitializing {
				return nil
			}
			initializing = activity == state.ActivityInitializing
		}
	}
}
