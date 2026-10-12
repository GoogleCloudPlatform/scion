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

// Async launch vocabulary shared by the runtimes, the runtime broker, the
// store and the clients (design wave 3 §3 E-a, ptone/scion#4670).
//
// A launch step names where a launch is: the runtime reports it through
// StartOptions.Checkpoint (before a resource-creating call) or
// StartOptions.Progress (a non-blocking update), and the hub records the
// latest one as the launch's step. A launch code names why a launch failed.
//
// The values are wire strings: they are stored and shown to clients, so they
// must never change. Clients render a label keyed on the step or code alone.
const (
	// LaunchStepClaim: the broker is claiming the launch from the hub.
	LaunchStepClaim = "claim"
	// LaunchStepPreClean: deleting a stale resource left by an earlier agent
	// of the same name.
	LaunchStepPreClean = "pre_clean"
	// LaunchStepSecrets: creating the agent's secrets.
	LaunchStepSecrets = "secrets"
	// LaunchStepPodCreate: creating the agent's pod.
	LaunchStepPodCreate = "pod_create"
	// LaunchStepScheduling: the pod is waiting to be scheduled.
	LaunchStepScheduling = "scheduling"
	// LaunchStepAwaitingCapacity: the pod is unschedulable on GKE, where the
	// cluster provisions node capacity and the launch keeps waiting.
	LaunchStepAwaitingCapacity = "awaiting_capacity"
	// LaunchStepInitContainers: the pod's init containers are running.
	LaunchStepInitContainers = "init_containers"
	// LaunchStepContainerCreate: the main container is being created, which
	// covers both the image pull and the volume mounts (pod status cannot
	// tell them apart).
	LaunchStepContainerCreate = "container_create"
	// LaunchStepImagePull is reserved for a reader of Kubernetes Events that
	// can see an image pull in progress. No runtime reports it yet.
	LaunchStepImagePull = "image_pull"
	// LaunchStepContainerStart: the main container is running and the
	// runtime is waiting for it to accept commands.
	LaunchStepContainerStart = "container_start"
	// LaunchStepHomeSync: copying the agent home into the container.
	LaunchStepHomeSync = "home_sync"
	// LaunchStepWorkspaceSync: copying the workspace into the container.
	LaunchStepWorkspaceSync = "workspace_sync"
	// LaunchStepStartupGate: releasing the container's startup gate so the
	// agent starts.
	LaunchStepStartupGate = "startup_gate"
	// LaunchStepLaunching: the generic step of runtimes that report no
	// finer steps (container runtimes).
	LaunchStepLaunching = "launching"
)

// Launch failure codes.
const (
	LaunchCodeLaunchTimeout         = "launch_timeout"
	LaunchCodeBrokerLost            = "broker_lost"
	LaunchCodeHubUnreachable        = "hub_unreachable"
	LaunchCodeNameInUse             = "name_in_use"
	LaunchCodeTemplateNotFound      = "template_not_found"
	LaunchCodeRuntimeError          = "runtime_error"
	LaunchCodeLaunchStopped         = "launch_stopped"
	LaunchCodeAgentError            = "agent_error"
	LaunchCodeHarnessConfigPolicy   = "harness_config_policy"
	LaunchCodeHarnessConfigUnusable = BrokerErrCodeHarnessConfigUnusable
	LaunchCodeAgentStateUnavailable = "agent_state_unavailable"
	LaunchCodeAgentStateConflict    = "agent_state_conflict"

	// LaunchCodeImagePullFailed: the agent image could not be pulled
	// (Kubernetes ImagePullBackOff or ErrImagePull).
	LaunchCodeImagePullFailed = "image_pull_failed"
	// LaunchCodeInvalidImage: the image reference is not valid
	// (Kubernetes InvalidImageName).
	LaunchCodeInvalidImage = "invalid_image"
	// LaunchCodeContainerConfig: the container could not be configured,
	// e.g. a missing secret or volume (Kubernetes CreateContainerConfigError).
	LaunchCodeContainerConfig = "container_config"
	// LaunchCodeCrashLoop: the main container keeps crashing on start
	// (Kubernetes CrashLoopBackOff).
	LaunchCodeCrashLoop = "crash_loop"
	// LaunchCodeInitContainerFailed: an init container exited non-zero or
	// could not start.
	LaunchCodeInitContainerFailed = "init_container_failed"
	// LaunchCodeUnschedulable: the pod cannot be scheduled and the cluster
	// will not add capacity for it (not GKE).
	LaunchCodeUnschedulable = "unschedulable"
	// LaunchCodePodDisrupted: the pod was preempted, evicted or deleted
	// while the launch waited for it to become ready.
	LaunchCodePodDisrupted = "pod_disrupted"
	// LaunchCodeBrokerRestarted: the broker restarted while the launch was
	// in flight.
	LaunchCodeBrokerRestarted = "broker_restarted"
)
