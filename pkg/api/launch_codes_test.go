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

package api

import "testing"

// TestLaunchVocabularyValues pins the wire values of the launch steps and
// codes: they are stored by the hub and shown to clients, and the existing
// ones must match the strings the broker and store already use.
func TestLaunchVocabularyValues(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{LaunchStepClaim, "claim"},
		{LaunchStepPreClean, "pre_clean"},
		{LaunchStepSecrets, "secrets"},
		{LaunchStepPodCreate, "pod_create"},
		{LaunchStepScheduling, "scheduling"},
		{LaunchStepAwaitingCapacity, "awaiting_capacity"},
		{LaunchStepInitContainers, "init_containers"},
		{LaunchStepContainerCreate, "container_create"},
		{LaunchStepImagePull, "image_pull"},
		{LaunchStepContainerStart, "container_start"},
		{LaunchStepHomeSync, "home_sync"},
		{LaunchStepWorkspaceSync, "workspace_sync"},
		{LaunchStepStartupGate, "startup_gate"},
		{LaunchStepLaunching, "launching"},

		{LaunchCodeLaunchTimeout, "launch_timeout"},
		{LaunchCodeBrokerLost, "broker_lost"},
		{LaunchCodeHubUnreachable, "hub_unreachable"},
		{LaunchCodeNameInUse, "name_in_use"},
		{LaunchCodeTemplateNotFound, "template_not_found"},
		{LaunchCodeRuntimeError, "runtime_error"},
		{LaunchCodeLaunchStopped, "launch_stopped"},
		{LaunchCodeAgentError, "agent_error"},
		{LaunchCodeHarnessConfigPolicy, "harness_config_policy"},
		{LaunchCodeHarnessConfigUnusable, "harness_config_unusable"},
		{LaunchCodeAgentStateUnavailable, "agent_state_unavailable"},
		{LaunchCodeAgentStateConflict, "agent_state_conflict"},
		{LaunchCodeImagePullFailed, "image_pull_failed"},
		{LaunchCodeInvalidImage, "invalid_image"},
		{LaunchCodeContainerConfig, "container_config"},
		{LaunchCodeCrashLoop, "crash_loop"},
		{LaunchCodeInitContainerFailed, "init_container_failed"},
		{LaunchCodeUnschedulable, "unschedulable"},
		{LaunchCodeBrokerRestarted, "broker_restarted"},
		{LaunchCodePodDisrupted, "pod_disrupted"},
	} {
		if c.got != c.want {
			t.Errorf("constant value %q, want %q", c.got, c.want)
		}
	}
}
