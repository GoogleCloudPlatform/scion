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

// waitForAgentReady polls the agent store until the agent's Activity field
// indicates the harness has initialized after a resume, or until timeout.
func (s *Server) waitForAgentReady(ctx context.Context, agentID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for agent to become ready")
		case <-ticker.C:
			agent, err := s.store.GetAgent(ctx, agentID)
			if err != nil {
				return fmt.Errorf("failed to check agent status: %w", err)
			}

			phase := state.Phase(agent.Phase)
			if phase != state.PhaseStarting && phase != state.PhaseRunning {
				return fmt.Errorf("agent entered unexpected phase %q while waiting for readiness", agent.Phase)
			}

			if agent.Activity != "" {
				return nil // Harness has reported its first status
			}
		}
	}
}
