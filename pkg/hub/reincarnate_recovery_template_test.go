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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReincarnationRecoveryTask_KeyPhrases pins the recovery addendum
// (ptone/scion#5511): it names the failed target generation, where it
// failed, that the agent remains at the generation before it, and that the
// handoff that follows was prepared for that attempt and is delivered for
// recovery. It is not the generation N+1 preamble.
func TestReincarnationRecoveryTask_KeyPhrases(t *testing.T) {
	const handoff = "## Immediate active work\nfinish the parser"
	task := reincarnationRecoveryTask(3, handoff, store.AgentReincarnationStateProvisioning)

	assert.True(t, strings.HasPrefix(task, "[SCION RECOVERY HANDOFF] "), task)
	assert.Contains(t, task, "A reincarnation to generation 3 failed while provisioning the new configuration.")
	assert.Contains(t, task, "You remain generation 2.")
	assert.Contains(t, task, "The handoff below was prepared for that attempt and is delivered to you for recovery.")
	assert.True(t, strings.HasSuffix(task, "\n---\n"+handoff), "the requester's handoff follows the addendum verbatim")
	assert.NotContains(t, task, "[SCION REINCARNATION]", "the recovery Task is not the new generation's preamble")
	assert.NotContains(t, task, "You are generation 3", "the agent did not become generation 3")
}

// TestReincarnationRecoveryReason_StepDerived: the reason comes from the
// failed step only, one per step, never from error text.
func TestReincarnationRecoveryReason_StepDerived(t *testing.T) {
	for state, want := range map[string]string{
		store.AgentReincarnationStatePending:      "before it started",
		store.AgentReincarnationStateStopping:     "while stopping the agent",
		store.AgentReincarnationStateProvisioning: "while provisioning the new configuration",
		store.AgentReincarnationStateStarting:     "while starting the new generation",
		"":                                        "before it completed",
	} {
		assert.Equal(t, want, reincarnationRecoveryReason(state), "state %q", state)
	}
}

// TestReincarnationRecoveryConfig_CopyAndEmptyHandoff: the recovery config is
// a copy of previous with only the Task replaced, previous itself is not
// changed, and with no handoff previous is returned unchanged (nothing new
// to carry forward).
func TestReincarnationRecoveryConfig_CopyAndEmptyHandoff(t *testing.T) {
	previous := &store.AgentAppliedConfig{Image: "old-image:v1", Task: "generation 1 task"}

	got := reincarnationRecoveryConfig(previous, 2, "new handoff", store.AgentReincarnationStateStopping)
	require.NotNil(t, got)
	assert.NotSame(t, previous, got)
	assert.Equal(t, reincarnationRecoveryTask(2, "new handoff", store.AgentReincarnationStateStopping), got.Task)
	assert.Equal(t, "old-image:v1", got.Image)
	assert.Equal(t, "generation 1 task", previous.Task, "previous (the audit copy) is not changed")

	assert.Same(t, previous, reincarnationRecoveryConfig(previous, 2, "", store.AgentReincarnationStateStopping))
	assert.Nil(t, reincarnationRecoveryConfig(nil, 2, "new handoff", store.AgentReincarnationStateStopping))
}
