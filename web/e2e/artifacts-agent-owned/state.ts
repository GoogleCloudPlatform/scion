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
 * Suite state shared by global setup, the spec and the teardown.
 *
 * The journey runs only against a local throwaway hub (spec section 1a):
 * global setup starts it with the co-located runtime broker, does the
 * setup that needs the hub's issuer credential, restarts it without
 * development sign-in, reads that back, and writes this suite's state to a
 * private 0700 dir named by E2E_AO_STATE_FILE. The target-hub mode of the
 * spec (section 1b) is not built: E2E_TARGET_URL is refused.
 */

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

const STATE_ENV = 'E2E_AO_STATE_FILE';

/** The five identities of the journey (spec section 2). */
export const IDENTITY_KEYS = ['powner', 'aowner', 'padmin', 'member', 'outsider'] as const;
export type IdentityKey = (typeof IDENTITY_KEYS)[number];

/** The deviation recorded on every row (grader's ruling on Q-L1). */
export const DEVIATION = 'member test-mode; local issuer, no workload delegation';

/**
 * The harness config the journey picks in the create form: gemini-cli is the
 * shipped harness whose no-auth fallback runs no login command, so the
 * agent's pane is a plain `exec bash` (pkg/runtime/common.go
 * buildNoAuthCmdLine); the others (antigravity, the form's default, included)
 * first run their interactive login, which would take the typed keys.
 */
export const SUITE_DEFAULT_HARNESS = 'gemini-cli';

/** One identity issued by the local issuer before the restart. */
export interface IssuedIdentity {
  id: string;
  email: string;
  displayName: string;
  /** The single-use web-login code file (0600, state dir); removed by the redeem. */
  codeFile: string;
  /** When its web-login code was minted (ms since the epoch); it lives 5 minutes. */
  codeMintedAt: number;
  expiresAt: string | null;
}

/** One HTTP answer as recorded: status and hub error code, no body. */
export interface StatusRecord {
  status: number | null;
  code: string | null;
  error?: string;
}

/** What the local global setup records for the spec. */
export interface LocalSuiteState {
  hubStateFile: string;
  baseURL: string;
  hubId: string;
  /**
   * The hub's startup flag names at first start (from the harness). The
   * restart drops --dev-auth only; p2.devAuthAfterRestart records that.
   */
  startFlags: string[];
  /** Whether agents can start here: a container runtime and an image registry. */
  launchCapable: boolean;
  launchCapableReason: string;
  imageRegistry: string;
  harnessConfig: string;
  agentImage: string;
  /**
   * E2E_AGENT_IMAGE when set (for example a digest-pinned ref): typed into the
   * create form's Container image field, and P7 requires appliedConfig.image
   * to equal it. Null: the harness default image, recorded only.
   */
  pinnedAgentImage: string | null;
  /** hub.artifacts as the admin experiments API reported it after enabling, before the restart. */
  experimentAfterEnable: { enabled: boolean | null; override: boolean | null };
  identities: Record<IdentityKey, IssuedIdentity>;
  /** P2 read back after the restart, by global setup (the only holder of the old dev token). */
  p2: {
    restartedAt: number;
    devAuthAfterRestart: boolean | null;
    /** The pre-restart dev token presented as a bearer (value never recorded). */
    oldDevTokenAuthMe: StatusRecord;
    oldDevTokenProjects: StatusRecord;
  };
}

/** Creates the private state dir and publishes the state file path. */
export function createStateFile(): string {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-ao-state-')));
  fs.chmodSync(dir, 0o700);
  const file = path.join(dir, 'suite-state.json');
  process.env[STATE_ENV] = file;
  return file;
}

/** The private state dir (global setup must have run). */
export function stateDir(): string {
  const file = process.env[STATE_ENV];
  if (!file) throw new Error(`${STATE_ENV} is not set; did global-setup run?`);
  return path.dirname(file);
}

/** Path of the hub state file in the state dir. */
export function hubStateFilePath(suiteStateFile: string): string {
  return path.join(path.dirname(suiteStateFile), 'hub-state.json');
}

export function writeSuiteState(file: string, state: LocalSuiteState): void {
  fs.rmSync(file, { force: true });
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

/** Refuses a remote target: this suite builds only the local mode (spec section 1). */
export function refuseRemoteTarget(env: NodeJS.ProcessEnv = process.env): void {
  if (env.E2E_TARGET_URL) {
    throw new Error(
      'E2E_TARGET_URL is set, but the agent-owned artifact journey runs only on a local ' +
        'throwaway hub (spec section 1a); unset it'
    );
  }
}
