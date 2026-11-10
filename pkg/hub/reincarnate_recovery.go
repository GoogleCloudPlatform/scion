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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reincarnationRecoveryTemplate is the addendum put in front of the
// requester's handoff when a reincarnation fails (ptone/scion#5511). Its
// arguments are the target generation, the step-derived reason, the
// generation the agent stays at, and the handoff. It is not the
// generation N+1 preamble: the agent stays at generation N, so "you are
// generation N+1" would be false.
const reincarnationRecoveryTemplate = "[SCION RECOVERY HANDOFF] A reincarnation to generation %d failed %s. " +
	"You remain generation %d.\n" +
	"The handoff below was prepared for that attempt and is delivered to you for recovery.\n" +
	"---\n%s"

// reincarnationRecoveryReason describes, from the step a reincarnation
// failed out of, where it failed. It is built from the step only, never
// from error text, so it carries no paths or broker detail.
func reincarnationRecoveryReason(fromState string) string {
	switch fromState {
	case store.AgentReincarnationStatePending:
		return "before it started"
	case store.AgentReincarnationStateStopping:
		return "while stopping the agent"
	case store.AgentReincarnationStateProvisioning:
		return "while provisioning the new configuration"
	case store.AgentReincarnationStateStarting:
		return "while starting the new generation"
	default:
		return "before it completed"
	}
}

// reincarnationRecoveryTask is the Task a failed reincarnation leaves on
// the agent: the recovery addendum plus the handoff the requester
// supplied for the failed attempt.
func reincarnationRecoveryTask(toGeneration int, handoff, fromState string) string {
	return fmt.Sprintf(reincarnationRecoveryTemplate, toGeneration, reincarnationRecoveryReason(fromState), toGeneration-1, handoff)
}

// reincarnationRecoveryConfig is the config every failure path restores or
// re-renders in place of the outgoing generation's config (ptone/scion#5511):
// a shallow copy of previous whose Task is the recovery Task, so the next
// fresh start delivers the new handoff, marked as a recovery handoff,
// rather than the last generation's preamble and handoff. previous itself
// (the record's PreviousAppliedConfig, the audit copy) is never changed.
// With no handoff there is nothing new to carry, so previous is returned
// unchanged; a nil previous stays nil.
func reincarnationRecoveryConfig(previous *store.AgentAppliedConfig, toGeneration int, handoff, fromState string) *store.AgentAppliedConfig {
	if previous == nil || handoff == "" {
		return previous
	}
	cfg := *previous
	cfg.Task = reincarnationRecoveryTask(toGeneration, handoff, fromState)
	return &cfg
}
