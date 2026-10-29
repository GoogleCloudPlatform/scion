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

package cmd

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Labels for the launch steps and failure codes a Hub reports on an agent's
// launch (design wave 3 §3 E-e, ptone/scion#4670). The texts are shared with
// the web agent page: keep the two tables identical. Labels are lower case
// with no final period so they can be embedded: "provisioning (waiting to
// be scheduled)" and "did not start (image_pull_failed: the agent image
// could not be pulled)". The code texts match the broker's fixed failure
// texts (pkg/runtimebroker/run_launch.go), which start the agent's message.

// launchStepLabels maps a launch step to its label.
var launchStepLabels = map[string]string{
	api.LaunchStepClaim:              "claiming the launch",
	api.LaunchStepPreClean:           "removing a leftover from an earlier agent",
	api.LaunchStepSecrets:            "creating secrets",
	api.LaunchStepPodCreate:          "creating the pod",
	api.LaunchStepScheduling:         "waiting to be scheduled",
	api.LaunchStepAwaitingCapacity:   "waiting for node capacity",
	api.LaunchStepInitContainers:     "running init containers",
	api.LaunchStepContainerCreate:    "preparing the container (image pull, volume mounts)",
	api.LaunchStepImagePull:          "pulling the image",
	api.LaunchStepContainerStart:     "starting the container",
	api.LaunchStepHomeSync:           "copying the agent home",
	api.LaunchStepWorkspaceSync:      "copying the workspace",
	api.LaunchStepStartupGate:        "starting the agent",
	api.LaunchStepLaunching:          "launching",
	api.LaunchStepSandboxLayout:      "preparing the sandbox",
	api.LaunchStepWorkspaceProvision: "provisioning the workspace",
	api.LaunchStepInstanceStart:      "starting the instance",
	api.LaunchStepInstanceCreate:     "creating the instance",
}

// launchCodeLabels maps a launch failure code to its label.
var launchCodeLabels = map[string]string{
	api.LaunchCodeLaunchTimeout:               "the launch timed out",
	api.LaunchCodeBrokerLost:                  "the runtime broker stopped responding",
	api.LaunchCodeHubUnreachable:              "the runtime broker could not reach the hub",
	api.LaunchCodeNameInUse:                   "another agent or run holds the name",
	api.LaunchCodeTemplateNotFound:            "the template or harness config was not found",
	api.LaunchCodeRuntimeError:                "the runtime could not start the agent",
	api.LaunchCodeLaunchStopped:               "the launch was stopped",
	api.LaunchCodeAgentError:                  "the agent failed to start",
	api.LaunchCodeHarnessConfigPolicy:         "the harness config is not allowed by policy",
	api.LaunchCodeHarnessConfigUnusable:       "the harness config cannot be used",
	api.LaunchCodeAgentStateUnavailable:       "the agent's state on the broker is unavailable",
	api.LaunchCodeAgentStateConflict:          "the agent's state on the broker cannot be used",
	api.BrokerErrCodeSkillResolution:          "a required skill could not be resolved",
	api.BrokerErrCodeStorageMigrationRequired: "the agent's storage settings must be migrated first",
	api.LaunchCodeImagePullFailed:             "the agent image could not be pulled",
	api.LaunchCodeInvalidImage:                "the agent image reference is not valid",
	api.LaunchCodeContainerConfig:             "the agent container could not be configured",
	api.LaunchCodeCrashLoop:                   "the agent container keeps crashing on start",
	api.LaunchCodeInitContainerFailed:         "an init container of the agent pod failed",
	api.LaunchCodeUnschedulable:               "the agent pod cannot be scheduled",
	api.LaunchCodePodDisrupted:                "the agent pod was preempted, evicted or deleted while starting",
	api.LaunchCodeBrokerRestarted:             "the runtime broker restarted during the launch",
	api.LaunchCodeDispatchFailed:              "the runtime broker did not accept the launch",
}

// launchStepLabel returns the label for a launch step. A step this CLI does
// not know (a newer Hub or runtime) is shown with "_" as a space.
func launchStepLabel(step string) string {
	return launchLabel(launchStepLabels, step)
}

// launchCodeLabel returns the label for a launch failure code, with the same
// fallback as launchStepLabel.
func launchCodeLabel(code string) string {
	return launchLabel(launchCodeLabels, code)
}

func launchLabel(labels map[string]string, key string) string {
	if l, ok := labels[key]; ok {
		return l
	}
	return strings.ReplaceAll(key, "_", " ")
}
