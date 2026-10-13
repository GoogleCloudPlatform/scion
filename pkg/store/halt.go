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

package store

import "github.com/GoogleCloudPlatform/scion/pkg/agent/state"

// AgentStatusResult is what UpdateAgentStatus reports back to its caller
// through AgentStatusUpdate.Result. It is filled only on success.
type AgentStatusResult struct {
	// Halted is true when this write moved the agent into phase halted.
	// A halt is written at most once per run, inside the update's
	// transaction, so the caller that sees Halted is the only one that
	// should notify about it.
	Halted bool
	// HaltedFrom is the phase the write would have stored without the halt
	// rule, when that differs from the phase the row had before the write
	// (stopped or error); otherwise "". Callers use it to keep firing the
	// stopped/error notifications and hooks that write would have fired.
	HaltedFrom string
}

// IsHaltEvidence reports whether an exit reason shows that the platform,
// not the user or the agent, ended the run: a disruption (preempted,
// evicted), an OOM kill, or a container that went missing.
func IsHaltEvidence(reason string) bool {
	switch state.ExitReason(reason) {
	case state.ExitReasonPreempted, state.ExitReasonEvicted:
		return true
	case state.ExitReasonOOMKilled, state.ExitReasonContainerMissing:
		return true
	}
	return false
}

// RuntimeHaltEligible reports whether an agent runtime name (the runtime's
// Name()) is one whose end-of-run evidence the halt rule trusts: docker,
// podman, container (Apple) and kubernetes. Empty, Cloud Run and other
// runtimes never halt; their agents end stopped or error as before.
func RuntimeHaltEligible(runtime string) bool {
	switch runtime {
	case "docker", "podman", "container", "kubernetes":
		return true
	}
	return false
}

// IsTerminalPhase reports whether phase is one in which the agent has no
// running container: stopped, error or halted.
func IsTerminalPhase(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseStopped, state.PhaseError, state.PhaseHalted:
		return true
	}
	return false
}
