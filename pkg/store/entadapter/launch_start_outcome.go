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

package entadapter

import (
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file holds the end-of-launch rules for start and restart launches
// (ptone/scion#4669, design wave 3 D-c). Create launches never reach them.

// isStartKindLaunch reports whether row's current launch is a start or
// restart launch.
func isStartKindLaunch(row *ent.Agent) bool {
	return row.LaunchKind == store.LaunchKindStart || row.LaunchKind == store.LaunchKindRestart
}

// startLaunchFailureEnd returns the phase a failed start-kind launch ends
// in (a failed report, a reaper timeout or a lost broker), and the resume
// memory (launch_resume_phase) the same write stores:
//
//   - a restart ends stopped, as the synchronous restart failure does;
//   - a start from stopped or error restores that phase, as the
//     synchronous rollback does;
//   - a start from suspended, created or provisioning ends error and
//     remembers that phase, so the failure is visible to phase-only
//     readers (a released CLI's wait ends on error) while a later plain
//     start or resume still behaves as it would have before the attempt
//     (store.EffectiveRestingPhase);
//   - any other prior (none recorded) ends error with no memory, as a
//     create launch does.
func startLaunchFailureEnd(row *ent.Agent) (phase state.Phase, resume string) {
	if row.LaunchKind == store.LaunchKindRestart {
		return state.PhaseStopped, ""
	}
	switch prior := state.Phase(row.LaunchPriorPhase); prior {
	case state.PhaseStopped, state.PhaseError:
		return prior, ""
	case state.PhaseSuspended, state.PhaseCreated, state.PhaseProvisioning:
		return state.PhaseError, string(prior)
	}
	return state.PhaseError, ""
}

// failureEndPhase is the phase a failure end of row's current launch
// writes: startLaunchFailureEnd's for a start-kind launch, error otherwise.
func failureEndPhase(row *ent.Agent) state.Phase {
	if !isStartKindLaunch(row) {
		return state.PhaseError
	}
	phase, _ := startLaunchFailureEnd(row)
	return phase
}

// withStartFailureEnd adds to u, a failure end of row's current launch
// that writes phase error for a create launch, the start-kind rule
// instead: the phase from startLaunchFailureEnd and the resume memory. For
// a create launch it returns u unchanged.
func withStartFailureEnd(u *ent.AgentUpdateOne, row *ent.Agent) *ent.AgentUpdateOne {
	if !isStartKindLaunch(row) {
		return u
	}
	phase, resume := startLaunchFailureEnd(row)
	return u.SetPhase(string(phase)).SetLaunchResumePhase(resume)
}

// withStartEndClearsMemory adds to u, a non-failure end of row's current
// launch (succeeded, running_observed, stopped during launch, or a
// wind-down end), the explicit clear of the resume memory for a start-kind
// launch. The ent hook also clears it whenever such a write sets a phase
// other than starting or error, but these ends do not all write a phase,
// so the end clears it itself. For a create launch it returns u unchanged.
func withStartEndClearsMemory(u *ent.AgentUpdateOne, row *ent.Agent) *ent.AgentUpdateOne {
	if !isStartKindLaunch(row) {
		return u
	}
	return u.SetLaunchResumePhase("")
}
