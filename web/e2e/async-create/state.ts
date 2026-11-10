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

/**
 * Suite state shared by global setup, the spec and the teardown, and the
 * target the journey runs against.
 *
 * Local target (default): global setup starts a throwaway member-level test
 * hub with the co-located runtime broker and the hub.async_agent_launch
 * experiment on (startHub's experiments option sets it through the admin
 * experiments API and reads it back), and writes this suite's state to a
 * private file named by E2E_AC_STATE_FILE.
 *
 * Remote target: E2E_TARGET_URL names an existing non-IAP hub. Global
 * setup starts nothing. The runner issues the test identity out of band
 * and passes a web-login code file; see remoteTargetFromEnv().
 */

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

import type { HubState } from '../harness/hub.js';

const STATE_ENV = 'E2E_AC_STATE_FILE';

/** What the local global setup records for the spec. */
export interface LocalSuiteState {
  hubStateFile: string;
  /** Whether agents can start here: a container runtime and an image registry. */
  launchCapable: boolean;
  /** Why launchCapable is what it is (shown in skips and the report). */
  launchCapableReason: string;
  /** The image registry the hub's broker uses. */
  imageRegistry: string;
  /** Harness config the form picks (E2E_HARNESS_CONFIG, else the form's default). */
  harnessConfig: string;
  /** The image that harness resolves to on this registry (A2 asserts it exactly). */
  agentImage: string;
  /** Bad image ref the failure checks type into the form: a tag that does not exist. */
  badImage: string;
}

/** The create form's default harness config (agent-create.ts). */
export const FORM_DEFAULT_HARNESS = 'antigravity';

/** The target the spec runs against. */
export interface JourneyTarget {
  mode: 'local' | 'remote';
  baseURL: string;
  /** Local only: the throwaway hub (CLI access as the dev-auth admin). */
  hub?: HubState;
  launchCapable: boolean;
  /** Why launchCapable is what it is (shown in skips and the evidence). */
  launchCapableReason: string;
  /**
   * Whether the target is known to have no usable container runtime, so
   * the no-runtime failure panel check applies (local: !launchCapable;
   * remote: only with E2E_LAUNCH_CAPABLE=0).
   */
  noRuntimeConfirmed: boolean;
  /** Bad image ref typed into the form (local: generated per run; remote: E2E_BAD_IMAGE). */
  badImage?: string;
  /**
   * The declared image ref A2 asserts appliedConfig.image is exactly:
   * E2E_AGENT_IMAGE, else (local only) the harness default on the registry.
   */
  agentImage?: string;
  /** Remote only: an existing project to create agents in (kept). */
  projectId?: string;
  /** Remote only: the file holding the test identity's web-login code. */
  codeFile?: string;
  /** Remote only: the test identity's email. */
  identityEmail?: string;
  /** Harness config to pick in the form; the form's default when unset (remote). */
  harnessConfig?: string;
}

/** Creates the private state dir and publishes the state file path. */
export function createStateFile(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-ac-state-'));
  fs.chmodSync(dir, 0o700);
  const file = path.join(dir, 'suite-state.json');
  process.env[STATE_ENV] = file;
  return file;
}

/** Path of the hub state file next to the suite state file. */
export function hubStateFilePath(suiteStateFile: string): string {
  return path.join(path.dirname(suiteStateFile), 'hub-state.json');
}

export function writeSuiteState(file: string, state: LocalSuiteState): void {
  fs.writeFileSync(file, JSON.stringify(state, null, 2), { mode: 0o600, flag: 'wx' });
}

export function readSuiteState(): LocalSuiteState | null {
  const file = process.env[STATE_ENV];
  if (!file || !fs.existsSync(file)) return null;
  return JSON.parse(fs.readFileSync(file, 'utf-8')) as LocalSuiteState;
}

/** Removes the private state dir. */
export function removeStateDir(): void {
  const file = process.env[STATE_ENV];
  if (file) fs.rmSync(path.dirname(file), { recursive: true, force: true });
}

/** Whether the journey targets an existing hub (E2E_TARGET_URL). */
export function isRemoteTarget(): boolean {
  return !!process.env.E2E_TARGET_URL;
}

/**
 * The remote target from the environment:
 * - E2E_TARGET_URL: hub origin (non-IAP).
 * - E2E_TEST_IDENTITY_CODE_FILE: web-login code file of a member test
 *   identity (`scion hub test-identity web-code <id> --out <file>`).
 * - E2E_TEST_IDENTITY_EMAIL: that identity's email.
 * - E2E_LAUNCH_CAPABLE=1: the hub's broker can start containers.
 * - E2E_BAD_IMAGE: an image ref whose tag does not exist; the failure checks
 *   type it into the Container image field and A4 asserts it exactly.
 * - E2E_TARGET_PROJECT_ID: optional project with a provider broker; a
 *   hub-managed project is created (and deleted) when unset.
 * - E2E_HARNESS_CONFIG: optional harness config to pick in the form.
 * - E2E_AGENT_IMAGE: the image that harness config resolves to on the
 *   target; A2 requires appliedConfig.image to equal it exactly.
 */
export function remoteTargetFromEnv(): JourneyTarget {
  const env = process.env;
  const targetURL = env.E2E_TARGET_URL ?? '';
  const codeFile = env.E2E_TEST_IDENTITY_CODE_FILE ?? '';
  const identityEmail = env.E2E_TEST_IDENTITY_EMAIL ?? '';
  const missing = [
    ['E2E_TEST_IDENTITY_CODE_FILE', codeFile],
    ['E2E_TEST_IDENTITY_EMAIL', identityEmail],
  ]
    .filter(([, value]) => !value)
    .map(([key]) => key);
  if (missing.length > 0) {
    throw new Error(`E2E_TARGET_URL is set; also set ${missing.join(', ')}`);
  }
  const capable = env.E2E_LAUNCH_CAPABLE;
  const launchCapable = capable === '1';
  return {
    mode: 'remote',
    baseURL: new URL(targetURL).origin,
    launchCapable,
    launchCapableReason: launchCapable
      ? 'E2E_LAUNCH_CAPABLE=1'
      : capable === '0'
        ? 'E2E_LAUNCH_CAPABLE=0'
        : 'remote target without E2E_LAUNCH_CAPABLE=1',
    // Only an explicit E2E_LAUNCH_CAPABLE=0 says the remote hub cannot run
    // agents; without it the no-runtime check would be mislabelled.
    noRuntimeConfirmed: capable === '0',
    codeFile,
    identityEmail,
    ...(env.E2E_TARGET_PROJECT_ID ? { projectId: env.E2E_TARGET_PROJECT_ID } : {}),
    ...(env.E2E_HARNESS_CONFIG ? { harnessConfig: env.E2E_HARNESS_CONFIG } : {}),
    // The bad image ref typed into the form's Container image field.
    ...(env.E2E_BAD_IMAGE ? { badImage: env.E2E_BAD_IMAGE } : {}),
    // The image A2 expects in appliedConfig.image.
    ...(env.E2E_AGENT_IMAGE ? { agentImage: env.E2E_AGENT_IMAGE } : {}),
  };
}
