/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Display labels for async launch steps and failure codes (design wave 3
 * §3 E-e, ptone/scion#4670). The keys are the wire values in
 * pkg/api/launch_codes.go, plus the broker codes skill_resolution_failed,
 * harness_config_unusable and storage_migration_required, which also reach
 * launch.error. The CLI
 * (cmd/launch_wait.go) uses the same wording, so change both together.
 *
 * Labels are lower case with no final period, so they read inside a
 * sentence: "Step: <step label>" and "<code>: <code label>". A key without a
 * label (a step or code newer than this file) is shown with "_" as a space.
 */

/** Labels for launch steps (api.LaunchStep*). */
export const LAUNCH_STEP_LABELS: Readonly<Record<string, string>> = {
  claim: 'claiming the launch',
  pre_clean: 'removing a leftover from an earlier agent',
  secrets: 'creating secrets',
  pod_create: 'creating the pod',
  scheduling: 'waiting to be scheduled',
  awaiting_capacity: 'waiting for node capacity',
  init_containers: 'running init containers',
  container_create: 'preparing the container (image pull, volume mounts)',
  image_pull: 'pulling the image',
  container_start: 'starting the container',
  home_sync: 'copying the agent home',
  workspace_sync: 'copying the workspace',
  startup_gate: 'starting the agent',
  launching: 'launching',
  sandbox_layout: 'preparing the sandbox',
  workspace_provision: 'provisioning the workspace',
  instance_start: 'starting the instance',
  instance_create: 'creating the instance',
};

/**
 * Labels for launch failure codes (api.LaunchCode*). The texts of the typed
 * runtime failures are the broker's report texts (launchFailureText in
 * pkg/runtimebroker/run_launch.go).
 */
export const LAUNCH_CODE_LABELS: Readonly<Record<string, string>> = {
  launch_timeout: 'the launch timed out',
  broker_lost: 'the runtime broker stopped responding',
  hub_unreachable: 'the runtime broker could not reach the hub',
  name_in_use: 'another agent or run holds the name',
  template_not_found: 'the template or harness config was not found',
  runtime_error: 'the runtime could not start the agent',
  launch_stopped: 'the launch was stopped',
  agent_error: 'the agent failed to start',
  harness_config_policy: 'the harness config is not allowed by policy',
  harness_config_unusable: 'the harness config cannot be used',
  agent_state_unavailable: "the agent's state on the broker is unavailable",
  agent_state_conflict: "the agent's state on the broker cannot be used",
  skill_resolution_failed: 'a required skill could not be resolved',
  storage_migration_required: "the agent's storage settings must be migrated first",
  image_pull_failed: 'the agent image could not be pulled',
  invalid_image: 'the agent image reference is not valid',
  container_config: 'the agent container could not be configured',
  crash_loop: 'the agent container keeps crashing on start',
  init_container_failed: 'an init container of the agent pod failed',
  unschedulable: 'the agent pod cannot be scheduled',
  pod_disrupted: 'the agent pod was preempted, evicted or deleted while starting',
  broker_restarted: 'the runtime broker restarted during the launch',
  dispatch_failed: 'the runtime broker did not accept the launch',
};

function fallbackLabel(key: string): string {
  return key.replace(/_/g, ' ');
}

/** The display text for a launch step; "" when there is none. */
export function launchStepLabel(step: string | null | undefined): string {
  if (!step) return '';
  return Object.hasOwn(LAUNCH_STEP_LABELS, step) ? LAUNCH_STEP_LABELS[step] : fallbackLabel(step);
}

/** The display text for a launch failure code; "" when there is none. */
export function launchCodeLabel(code: string | null | undefined): string {
  if (!code) return '';
  return Object.hasOwn(LAUNCH_CODE_LABELS, code) ? LAUNCH_CODE_LABELS[code] : fallbackLabel(code);
}

/**
 * The launch banner title for a launch in flight, by launch kind. A missing
 * kind reads as a create, the only kind before start launches.
 */
export function launchKindTitle(kind: string | null | undefined): string {
  switch (kind) {
    case 'start':
      // A resume is a start launch too: the launch view does not say
      // which phase the agent started from.
      return 'Starting';
    case 'restart':
      return 'Restarting';
    default:
      return 'Launching';
  }
}

/** The failed-launch banner title, by launch kind. */
export function launchFailedTitle(kind: string | null | undefined): string {
  switch (kind) {
    case 'start':
      return 'Agent start did not complete';
    case 'restart':
      return 'Agent restart did not complete';
    default:
      return 'Agent create did not complete';
  }
}
