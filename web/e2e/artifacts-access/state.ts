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
 * hub (no broker, no runtime) and publishes its state file path in
 * E2E_AA_HUB_STATE_FILE (a private 0700 temp dir).
 *
 * Remote target: E2E_TARGET_URL names an existing non-IAP hub; global setup
 * starts nothing. The runner issues the five test identities out of band
 * and passes one web-login code file and email per identity; see
 * remoteTargetFromEnv().
 */

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

import type { HubState } from '../harness/hub.js';

const STATE_ENV = 'E2E_AA_HUB_STATE_FILE';

/** The five identities of the journey (spec section 1). */
export const IDENTITY_KEYS = ['owner', 'member', 'padmin', 'grantee', 'outsider'] as const;
export type IdentityKey = (typeof IDENTITY_KEYS)[number];

/** The target the spec runs against. */
export interface JourneyTarget {
  mode: 'local' | 'remote';
  baseURL: string;
  /** Local only: the throwaway hub (CLI and admin API as the dev-auth admin). */
  hub?: HubState;
  /** Remote only: web-login code file and email per identity. */
  identities?: Record<IdentityKey, { codeFile: string; email: string }>;
  /**
   * Remote only: an admin token (read from E2E_ADMIN_TOKEN_FILE) for the
   * experiment toggle, with E2E_ARTIFACTS_TOGGLE_EXPERIMENT=1. Without it
   * the experiment-off rows are not run and the experiment must be on.
   */
  adminToken?: string;
}

/** Creates the private state dir and publishes the hub state file path. */
export function createStateFile(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-aa-state-'));
  fs.chmodSync(dir, 0o700);
  const file = path.join(dir, 'hub-state.json');
  process.env[STATE_ENV] = file;
  return file;
}

/** The hub state file path published by global setup. */
export function stateFile(): string {
  const file = process.env[STATE_ENV];
  if (!file) throw new Error(`${STATE_ENV} is not set; did global-setup run?`);
  return file;
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
 * - E2E_R1_<KEY>_CODE_FILE and E2E_R1_<KEY>_EMAIL for each identity key
 *   (OWNER, MEMBER, PADMIN, GRANTEE, OUTSIDER): a member test identity's
 *   web-login code file and email. The redeem removes each code file.
 * - E2E_ARTIFACTS_TOGGLE_EXPERIMENT=1 with E2E_ADMIN_TOKEN_FILE: toggle
 *   hub.artifacts (experiment-off rows); otherwise they are not run.
 */
export function remoteTargetFromEnv(env: NodeJS.ProcessEnv = process.env): JourneyTarget {
  const missing: string[] = [];
  const identities = {} as Record<IdentityKey, { codeFile: string; email: string }>;
  for (const key of IDENTITY_KEYS) {
    const upper = key.toUpperCase();
    const codeFile = env[`E2E_R1_${upper}_CODE_FILE`] ?? '';
    const email = env[`E2E_R1_${upper}_EMAIL`] ?? '';
    if (!codeFile) missing.push(`E2E_R1_${upper}_CODE_FILE`);
    if (!email) missing.push(`E2E_R1_${upper}_EMAIL`);
    identities[key] = { codeFile, email };
  }
  if (missing.length > 0) {
    throw new Error(`E2E_TARGET_URL is set; also set ${missing.join(', ')}`);
  }
  let adminToken: string | undefined;
  if (env.E2E_ARTIFACTS_TOGGLE_EXPERIMENT === '1') {
    const file = env.E2E_ADMIN_TOKEN_FILE ?? '';
    if (!file) {
      throw new Error('E2E_ARTIFACTS_TOGGLE_EXPERIMENT=1 needs E2E_ADMIN_TOKEN_FILE');
    }
    adminToken = fs.readFileSync(file, 'utf-8').trim();
  }
  return {
    mode: 'remote',
    baseURL: new URL(env.E2E_TARGET_URL ?? '').origin,
    identities,
    ...(adminToken ? { adminToken } : {}),
  };
}
