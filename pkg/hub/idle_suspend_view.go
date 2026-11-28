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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// computeAgentIdleSuspendView computes the earliest instant at which agent
// becomes eligible for the idle auto-suspend sweep, mirroring the predicates of
// ListIdleSuspendCandidates, autoSuspendAgent, and ClaimIdleSuspend.
//
// It returns nil when idle auto-suspend is disabled, when the agent is not
// currently eligible, or when any sweep/claim guard blocks suspension (e.g.,
// never-suspend policy, active live work, stale heartbeat, non-resumable
// harness, active delete/reincarnation/claim).
func computeAgentIdleSuspendView(
	agent *store.Agent,
	now time.Time,
	enabled bool,
	after time.Duration,
	neverSuspendActive bool,
	harnessSupportsResume bool,
) *time.Time {
	if agent == nil || !enabled || after <= 0 {
		return nil
	}
	if !agent.DeletedAt.IsZero() || agent.DeletionHoldsRow(now) {
		return nil
	}
	if reincarnationInFlight(agent) {
		return nil
	}
	if agent.StartClaimID != "" {
		return nil
	}
	if agent.IdleSuspendClaimUntil != nil {
		if now.Before(*agent.IdleSuspendClaimUntil) {
			return nil
		}
		if backoff := idleSuspendBackoff(); backoff > 0 && now.Before(agent.IdleSuspendClaimUntil.Add(backoff)) {
			return nil
		}
	}
	if agent.Phase != string(state.PhaseRunning) {
		return nil
	}
	if neverSuspendActive {
		return nil
	}
	if agent.RunIntent != "" && agent.RunIntent != store.RunIntentRunning {
		return nil
	}
	if !state.Activity(agent.Activity).IsSuspendEligible() {
		return nil
	}
	if agent.ActivitySince == nil {
		return nil
	}
	if agent.LastSeen.IsZero() || agent.LastSeen.Before(now.Add(-agentHeartbeatRecency)) {
		return nil
	}
	if agent.HasRecentLiveWork(now) {
		return nil
	}
	if !harnessSupportsResume {
		return nil
	}

	refTime := *agent.ActivitySince
	if agent.LastDeliveryAt != nil && agent.LastDeliveryAt.After(refTime) {
		refTime = *agent.LastDeliveryAt
	}
	t := refTime.Add(after).UTC()
	return &t
}
