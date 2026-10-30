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
 * Suite state of the port proxy journeys, shared by global setup, the spec
 * and the teardown.
 *
 * Global setup creates a private 0700 temp dir, publishes its path in
 * E2E_PP_STATE_DIR and writes there: the hub state (hub.json, the shared
 * harness's), the suite state (suite.json: the fixture server, the fake
 * agent helper and what it created) and the helpers' logs.
 */

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

const STATE_ENV = 'E2E_PP_STATE_DIR';

/** The transports the fake agent helper serves a port over, one agent each. */
export const TRANSPORTS = ['conduit', 'legacy'] as const;
export type Transport = (typeof TRANSPORTS)[number];

/**
 * Set by playwright.view.config.ts: the page view journeys (3 and 4, with
 * hub.port_view on). Global setup then starts the hub with the experiment
 * on, serves the port from the conduit agent only and creates the extra
 * identities those journeys use.
 */
export const VIEW_ENV = 'E2E_PP_VIEW';

/** Whether this run is the page view suite (playwright.view.config.ts). */
export function viewMode(): boolean {
  return process.env[VIEW_ENV] === '1';
}

/** A signed-in test identity: its email and its session as a storage state file (0600). */
export interface SuiteIdentity {
  email: string;
  storageFile: string;
}

/** Text single.html writes to #probe; the helper's readiness probe looks for it too. */
export const SINGLE_PROBE_TEXT = 'single-ok';

/** One agent the helper created (its ready file). */
export interface SuiteAgent {
  transport: Transport;
  id: string;
  name: string;
}

/** The suite state global setup writes. */
export interface SuiteState {
  /** PID of the fixture app server (its own process group). */
  fixturePid: number;
  /** PID of the fake agent helper (its own process group). */
  helperPid: number;
  /** Loopback port of the fixture app, registered by every agent. */
  port: number;
  brokerId: string;
  projectId: string;
  agents: SuiteAgent[];
  /**
   * The member test identity the journeys sign in as: project-admin of
   * the fixture project. storageFile is its signed-in session as a
   * Playwright storage state (0600, under the state dir). Unset until
   * setup has redeemed it.
   */
  member?: SuiteIdentity;
  /**
   * Page view suite only. peer: project-admin of the fixture project, a
   * second user who may open its ports. outsider: a signed-in user with no
   * role in the project. In that suite the member is project-owner, so it
   * can remove the peer's role.
   */
  peer?: SuiteIdentity;
  outsider?: SuiteIdentity;
}

/** Creates the private state dir and publishes its path. */
export function createStateDir(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-pp-'));
  fs.chmodSync(dir, 0o700);
  process.env[STATE_ENV] = dir;
  return dir;
}

/** The state dir published by global setup. */
export function stateDir(): string {
  const dir = process.env[STATE_ENV];
  if (!dir) throw new Error(`${STATE_ENV} is not set; did global-setup run?`);
  return dir;
}

/** The shared harness's hub state file. */
export function hubStateFile(): string {
  return path.join(stateDir(), 'hub.json');
}

function suiteStateFile(): string {
  return path.join(stateDir(), 'suite.json');
}

export function writeSuiteState(state: SuiteState): void {
  fs.writeFileSync(suiteStateFile(), JSON.stringify(state, null, 2), { mode: 0o600 });
}

/** The suite state, or null when global setup did not get that far. */
export function readSuiteState(): SuiteState | null {
  try {
    return JSON.parse(fs.readFileSync(suiteStateFile(), 'utf-8')) as SuiteState;
  } catch {
    return null;
  }
}

/** Removes the private state dir (unless E2E_KEEP_HUB_STATE=1). */
export function removeStateDir(): void {
  const dir = process.env[STATE_ENV];
  if (!dir) return;
  if (process.env.E2E_KEEP_HUB_STATE === '1') {
    console.info(`[teardown] E2E_KEEP_HUB_STATE=1: kept ${dir} (contains hub credentials)`);
    return;
  }
  fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3, retryDelay: 200 });
}
