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
 * Global setup of the agent-owned artifact journey (R1-B, spec section 1a).
 *
 * 1. Refuses a remote target, and runs the credential guard: agents reach
 *    running through the shipped harnesses' no-auth fallback.
 * 2. Starts a throwaway member-level test hub (class test, --test-mode
 *    member, marked database, pinned hub id, private HOME) with the
 *    co-located runtime broker and the hub.async_agent_launch experiment on
 *    (set through the admin experiments API and read back), as the
 *    async create journey does.
 * 3. With the hub's own issuer credential (the dev token, before the
 *    restart): turns hub.artifacts on through the admin experiments API,
 *    then issues the five member test identities and mints each one's
 *    single-use web-login code into a 0600 file in the private state dir.
 *    This is the recorded deviation: a local issuer, no workload
 *    delegation.
 * 4. Restarts the hub without development sign-in, as in production.
 * 5. Reads P2 back: the pre-restart dev token, presented as a bearer, must
 *    now be refused. The token is held in memory only and never written.
 *
 * The spec redeems the codes (they live 5 minutes) in its beforeAll.
 * Launch capability: E2E_LAUNCH_CAPABLE=1|0 decides it; otherwise a working
 * `docker info` and E2E_IMAGE_REGISTRY are both needed. Without it the
 * agent rows are declared skips (the b02 gate tier).
 */

import { execFileSync } from 'node:child_process';
import * as fs from 'node:fs';
import * as path from 'node:path';

import { assertNoHarnessCredentialEnv } from '../async-create/credential-guard.js';
import { readHubState, restartHubWithoutDevAuth, runScionCLI, startHub } from '../harness/hub.js';
import globalTeardown from './global-teardown.js';
import {
  SUITE_DEFAULT_HARNESS,
  IDENTITY_KEYS,
  createStateFile,
  hubStateFilePath,
  refuseRemoteTarget,
  writeSuiteState,
  type IdentityKey,
  type IssuedIdentity,
  type StatusRecord,
} from './state.js';

const EXPERIMENT = 'hub.artifacts';

/** Placeholder registry when none is given; nothing is pulled from it. */
const PLACEHOLDER_REGISTRY = 'registry.invalid/scion-e2e';

/** Bound of one admin or probe request in setup. */
const REQUEST_TIMEOUT_MS = 30_000;

function dockerWorks(): boolean {
  try {
    execFileSync('docker', ['info'], { stdio: 'ignore', timeout: 20_000 });
    return true;
  } catch {
    return false;
  }
}

/** As the async create journey's global setup decides it. */
function launchCapability(registry: string | undefined): { capable: boolean; reason: string } {
  const explicit = process.env.E2E_LAUNCH_CAPABLE;
  if (explicit === '1' || explicit === '0') {
    return { capable: explicit === '1', reason: `E2E_LAUNCH_CAPABLE=${explicit}` };
  }
  if (!dockerWorks()) {
    return { capable: false, reason: 'no working container runtime (docker info failed)' };
  }
  if (!registry) return { capable: false, reason: 'E2E_IMAGE_REGISTRY is not set' };
  return { capable: true, reason: 'docker info succeeded and E2E_IMAGE_REGISTRY is set' };
}

/** The error code of a hub error body, or null. */
function errorCodeOf(text: string): string | null {
  try {
    const b = JSON.parse(text) as { error?: { code?: unknown } | string; code?: unknown };
    if (b.error && typeof b.error === 'object' && typeof b.error.code === 'string') {
      return b.error.code;
    }
    return typeof b.code === 'string' ? b.code : null;
  } catch {
    return null;
  }
}

/** A request with a bearer token; returns status and body text. */
async function bearerRequest(
  baseURL: string,
  token: string,
  method: string,
  apiPath: string,
  body?: unknown
): Promise<{ status: number; text: string }> {
  const res = await fetch(`${baseURL}${apiPath}`, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    redirect: 'manual',
  });
  return { status: res.status, text: await res.text() };
}

/** Turns hub.artifacts on through the admin experiments API and reads it back. */
async function enableArtifacts(
  baseURL: string,
  devToken: string
): Promise<{ enabled: boolean | null; override: boolean | null }> {
  type Listing = {
    revision: number;
    experiments: Array<{ name: string; enabled: boolean; override: boolean | null }>;
  };
  const read = async (): Promise<Listing> => {
    const res = await bearerRequest(baseURL, devToken, 'GET', '/api/v1/admin/experiments');
    if (res.status !== 200) throw new Error(`GET /api/v1/admin/experiments answered ${res.status}`);
    return JSON.parse(res.text) as Listing;
  };
  const before = await read();
  const put = await bearerRequest(baseURL, devToken, 'PUT', '/api/v1/admin/experiments', {
    overrides: { [EXPERIMENT]: true },
    expected_revision: before.revision,
  });
  if (put.status !== 200) throw new Error(`PUT /api/v1/admin/experiments answered ${put.status}`);
  const entry = (await read()).experiments.find((e) => e.name === EXPERIMENT);
  return { enabled: entry?.enabled ?? null, override: entry?.override ?? null };
}

/** Probes a route with the old dev token after the restart; records status and code only. */
async function probeOldToken(
  baseURL: string,
  token: string,
  apiPath: string
): Promise<StatusRecord> {
  try {
    const res = await bearerRequest(baseURL, token, 'GET', apiPath);
    return { status: res.status, code: errorCodeOf(res.text) };
  } catch (err) {
    return { status: null, code: null, error: err instanceof Error ? err.message : String(err) };
  }
}

async function globalSetup(): Promise<void> {
  refuseRemoteTarget();
  assertNoHarnessCredentialEnv();
  const suiteStateFile = createStateFile();
  try {
    await setUp(suiteStateFile);
  } catch (err) {
    // Stop whatever did start before reporting the failure.
    globalTeardown();
    throw err;
  }
}

async function setUp(suiteStateFile: string): Promise<void> {
  const dir = path.dirname(suiteStateFile);
  const registry = process.env.E2E_IMAGE_REGISTRY || undefined;
  const { capable, reason } = launchCapability(registry);
  const imageRegistry = registry ?? PLACEHOLDER_REGISTRY;
  const harnessConfig = process.env.E2E_HARNESS_CONFIG || SUITE_DEFAULT_HARNESS;
  const agentImage =
    process.env.E2E_AGENT_IMAGE || `${imageRegistry}/scion-${harnessConfig}:latest`;
  console.info(`[setup] Harness config: ${harnessConfig} (no-auth drop-to-shell)`);
  console.info(`[setup] Agent image (expected): ${agentImage}`);
  console.info(`[setup] Launch capable: ${String(capable)} (${reason})`);

  const hubStateFile = hubStateFilePath(suiteStateFile);
  const hub = await startHub({
    testLogin: false,
    testIdentities: true,
    runtimeBroker: true,
    build: process.env.E2E_PREBUILT !== '1',
    stateFile: hubStateFile,
    env: {
      SCION_IMAGE_REGISTRY: imageRegistry,
    },
    // Set explicitly, whatever the registry default, and read back by
    // startHub, so the journey always runs with async create on.
    experiments: ['hub.async_agent_launch'],
  });
  if (!hub.hubId) throw new Error('startHub returned no hub id (testIdentities is required)');
  // The only copy of the issuer credential this suite keeps; it lives in
  // this function's memory and is dropped with it.
  const devToken = hub.devToken;

  const experimentAfterEnable = await enableArtifacts(hub.baseURL, devToken);
  if (experimentAfterEnable.enabled !== true) {
    throw new Error(`${EXPERIMENT} is not on after enabling it`);
  }

  // Identities last, so their 5-minute web-login codes are as fresh as
  // possible when the spec redeems them.
  const identities = {} as Record<IdentityKey, IssuedIdentity>;
  for (const key of IDENTITY_KEYS) {
    const tokenFile = path.join(dir, `${key}.token`);
    const issued = JSON.parse(
      runScionCLI(hub, [
        'hub',
        'test-identity',
        'issue',
        '--role',
        'member',
        '--purpose',
        `e2e agent-owned artifact (${key})`,
        '--out',
        tokenFile,
        '--json',
      ])
    ) as { identity: { id: string; email: string; displayName?: string; expiresAt?: string } };
    // The browser path needs no API token.
    fs.rmSync(tokenFile, { force: true });
    const codeFile = path.join(dir, `${key}.code`);
    runScionCLI(hub, ['hub', 'test-identity', 'web-code', issued.identity.id, '--out', codeFile]);
    identities[key] = {
      id: issued.identity.id,
      email: issued.identity.email,
      displayName: issued.identity.displayName ?? '',
      codeFile,
      codeMintedAt: Date.now(),
      expiresAt: issued.identity.expiresAt ?? null,
    };
  }

  // Production-like sign-in from here on.
  await restartHubWithoutDevAuth(hubStateFile);
  const restartedAt = Date.now();
  const p2 = {
    restartedAt,
    devAuthAfterRestart: readHubState(hubStateFile)?.devAuth ?? null,
    oldDevTokenAuthMe: await probeOldToken(hub.baseURL, devToken, '/api/v1/auth/me'),
    oldDevTokenProjects: await probeOldToken(hub.baseURL, devToken, '/api/v1/projects'),
  };
  console.info(
    `[setup] Restarted without dev auth; the old dev token answers ` +
      `${String(p2.oldDevTokenAuthMe.status)} on /api/v1/auth/me`
  );

  writeSuiteState(suiteStateFile, {
    hubStateFile,
    baseURL: hub.baseURL,
    hubId: hub.hubId,
    startFlags: hub.startFlags ?? [],
    launchCapable: capable,
    launchCapableReason: reason,
    imageRegistry,
    harnessConfig,
    agentImage,
    pinnedAgentImage: process.env.E2E_AGENT_IMAGE || null,
    experimentAfterEnable,
    identities,
    p2,
  });
  console.info(`[setup] Agent-owned artifact hub ${hub.hubId} at ${hub.baseURL}`);
}

export default globalSetup;
