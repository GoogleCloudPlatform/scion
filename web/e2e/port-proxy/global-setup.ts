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
 * Global setup of the port proxy journeys.
 *
 * 1. Starts a throwaway member-level test hub (../harness/hub.ts) with
 *    hub.conduit on from process start (startupExperiments), so agents can
 *    serve ports over a conduit session as well as the legacy tunnel.
 * 2. Starts the fixture app server (serve-fixture.mjs) on a loopback port.
 * 3. Starts the fake agent helper (hack/e2e/fakeagent): a standalone
 *    Runtime Broker joined to the hub that runs the real agent-side
 *    clients in process. It creates a project and one agent per transport,
 *    each registering the fixture port, and writes a ready file once each
 *    port answers through the hub.
 * 4. Issues the member test identity and makes it project-admin of the
 *    fixture project.
 * 5. Restarts the hub without development sign-in, as in production (the
 *    dev token is not accepted afterwards); the spec waits for the agents
 *    to reconnect.
 * 6. Redeems the member's web-login code once and saves the session as a
 *    storage state (0600, state dir) that every browser context loads.
 *
 * The page view suite (playwright.view.config.ts, E2E_PP_VIEW=1) also turns
 * hub.port_view on from process start, serves the port from both agents
 * (conduit and legacy tunnel), makes the member project-owner, and adds a peer
 * (project-admin) and an outsider (no project role), each signed in the
 * same way.
 *
 * The hub binary (make build), the web assets (npm run build) and the
 * helper binary must exist with E2E_PREBUILT=1; otherwise they are built
 * here. E2E_FAKEAGENT_BINARY overrides the helper path.
 */

import { execFileSync, spawn } from 'node:child_process';
import * as fs from 'node:fs';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';

import { request } from '@playwright/test';

import { type HubState, restartHubWithoutDevAuth, runScionCLI, startHub } from '../harness/hub.js';
import globalTeardown from './global-teardown.js';
import {
  createStateDir,
  hubStateFile,
  SINGLE_PROBE_TEXT,
  type SuiteAgent,
  TRANSPORTS,
  viewMode,
  writeSuiteState,
} from './state.js';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const WORKSPACE_ROOT = path.resolve(HERE, '..', '..', '..');
const DEFAULT_HELPER = path.join(WORKSPACE_ROOT, 'build', 'e2e-fakeagent');

const READY_TIMEOUT_MS = 120_000;

function helperBinary(): string {
  return process.env.E2E_FAKEAGENT_BINARY || DEFAULT_HELPER;
}

function buildHelper(out: string): void {
  console.info('[setup] Building the fake agent helper...');
  execFileSync('go', ['build', '-buildvcs=false', '-o', out, './hack/e2e/fakeagent'], {
    cwd: WORKSPACE_ROOT,
    stdio: 'inherit',
    timeout: 600_000,
  });
}

/** The environment of a helper process: no inherited SCION_* settings, a private HOME. */
function helperEnv(home: string): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(process.env)) {
    if (!key.startsWith('SCION_')) env[key] = value;
  }
  return { ...env, HOME: home };
}

/** Starts a detached process (its own group) logging to logFile. */
function startDetached(
  cmd: string,
  args: string[],
  env: NodeJS.ProcessEnv,
  logFile: string
): number {
  const log = fs.openSync(logFile, 'a', 0o600);
  const child = spawn(cmd, args, { detached: true, stdio: ['ignore', log, log], env });
  fs.closeSync(log);
  child.unref();
  if (!child.pid) throw new Error(`could not start ${path.basename(cmd)}`);
  return child.pid;
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

function tail(file: string, lines = 30): string {
  try {
    return fs.readFileSync(file, 'utf-8').trimEnd().split('\n').slice(-lines).join('\n');
  } catch {
    return '(no log)';
  }
}

/** Waits for a JSON ready file written by pid; fails early if pid exits. */
async function waitReady<T>(file: string, pid: number, logFile: string, what: string): Promise<T> {
  const deadline = Date.now() + READY_TIMEOUT_MS;
  while (Date.now() < deadline) {
    if (fs.existsSync(file)) return JSON.parse(fs.readFileSync(file, 'utf-8')) as T;
    if (!alive(pid)) throw new Error(`${what} exited before it was ready:\n${tail(logFile)}`);
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error(`${what} was not ready within ${READY_TIMEOUT_MS}ms:\n${tail(logFile)}`);
}

async function globalSetup(): Promise<void> {
  const dir = createStateDir();
  try {
    await setUp(dir);
  } catch (err) {
    // Stop whatever did start before reporting the failure.
    await globalTeardown();
    throw err;
  }
}

async function setUp(dir: string): Promise<void> {
  const build = process.env.E2E_PREBUILT !== '1';
  const helper = helperBinary();
  if (build && !process.env.E2E_FAKEAGENT_BINARY) buildHelper(helper);
  if (!fs.existsSync(helper)) throw new Error(`fake agent helper ${helper} does not exist`);

  const hub = await startHub({
    testLogin: false,
    testIdentities: true,
    build,
    stateFile: hubStateFile(),
    startupExperiments: viewMode() ? ['hub.conduit', 'hub.port_view'] : ['hub.conduit'],
  });
  console.info(`[setup] Port proxy hub ${String(hub.hubId)} at ${hub.baseURL}`);

  // Fixture app.
  const fixtureLog = path.join(dir, 'fixture.log');
  const fixtureReady = path.join(dir, 'fixture-ready.json');
  const fixturePid = startDetached(
    process.execPath,
    [path.join(HERE, 'serve-fixture.mjs'), fixtureReady],
    process.env,
    fixtureLog
  );
  // Recorded at once, so the teardown stops it even if it never gets ready.
  writeSuiteState({ fixturePid, helperPid: 0, port: 0, brokerId: '', projectId: '', agents: [] });
  const { port } = await waitReady<{ port: number }>(
    fixtureReady,
    fixturePid,
    fixtureLog,
    'fixture server'
  );

  // Fake agent helper. The dev token goes in a 0600 file, never on argv.
  const tokenFile = path.join(dir, 'dev-token');
  fs.writeFileSync(tokenFile, hub.devToken, { mode: 0o600 });
  const helperHome = path.join(dir, 'helper-home');
  fs.mkdirSync(helperHome, { mode: 0o700 });
  const helperLog = path.join(dir, 'fakeagent.log');
  const helperReady = path.join(dir, 'fakeagent-ready.json');
  const helperPid = startDetached(
    helper,
    [
      '-hub',
      hub.baseURL,
      '-dev-token-file',
      tokenFile,
      '-port',
      String(port),
      '-state-dir',
      helperHome,
      '-ready-file',
      helperReady,
      '-transports',
      TRANSPORTS.join(','),
      '-probe-path',
      'single.html',
      '-probe-text',
      SINGLE_PROBE_TEXT,
    ],
    helperEnv(helperHome),
    helperLog
  );
  // Recorded before the wait, so the teardown stops both even if it fails.
  writeSuiteState({ fixturePid, helperPid, port, brokerId: '', projectId: '', agents: [] });
  const ready = await waitReady<{ brokerId: string; projectId: string; agents: SuiteAgent[] }>(
    helperReady,
    helperPid,
    helperLog,
    'fake agent helper'
  );
  writeSuiteState({ fixturePid, helperPid, port, ...ready });
  console.info(
    `[setup] Agents ready on port ${port}: ${ready.agents.map((a) => `${a.name} (${a.transport})`).join(', ')}`
  );

  // The identities, their web-login codes and their project roles need the
  // dev token, so they are created before the restart below.
  const view = viewMode();
  const member = await createIdentity(
    hub,
    dir,
    ready.projectId,
    'member',
    view ? 'project-owner' : 'project-admin'
  );
  const peer = view
    ? await createIdentity(hub, dir, ready.projectId, 'peer', 'project-admin')
    : undefined;
  const outsider = view
    ? await createIdentity(hub, dir, ready.projectId, 'outsider', null)
    : undefined;

  // The journeys run against a hub without development sign-in, as in
  // production: a request without credentials is not signed in. The
  // agents reconnect on their own; the spec waits for them.
  await restartHubWithoutDevAuth(hubStateFile());

  // Redeem the single-use web-login code once, here, and keep the session
  // as a storage state, so a retried beforeAll reuses the session instead
  // of a code that is already spent.
  const signedIn = async (who: {
    name: string;
    email: string;
    codeFile: string;
  }): Promise<{ email: string; storageFile: string }> => ({
    email: who.email,
    storageFile: await redeemToStorageState(hub.baseURL, who.codeFile, dir, who.name),
  });
  writeSuiteState({
    fixturePid,
    helperPid,
    port,
    ...ready,
    member: await signedIn(member),
    ...(peer ? { peer: await signedIn(peer) } : {}),
    ...(outsider ? { outsider: await signedIn(outsider) } : {}),
  });
}

/**
 * Redeems a web-login code (POST /auth/test-identity/redeem) in a fresh
 * request context and saves the session cookie as a Playwright storage
 * state in a 0600 file under the state dir. The code file is removed after
 * a successful redeem. A failure throws with the status only.
 */
async function redeemToStorageState(
  baseURL: string,
  codeFile: string,
  dir: string,
  name: string
): Promise<string> {
  const code = fs.readFileSync(codeFile, 'utf-8').trim();
  const ctx = await request.newContext({ baseURL });
  try {
    const res = await ctx.post('/auth/test-identity/redeem', {
      data: { code },
      headers: { 'Content-Type': 'application/json' },
      maxRedirects: 0,
    });
    const status = res.status();
    await res.dispose();
    if (status !== 204) throw new Error(`test identity redeem answered ${status}, want 204`);
    fs.rmSync(codeFile, { force: true });
    const storageFile = path.join(dir, `${name}-storage.json`);
    const state = await ctx.storageState();
    fs.writeFileSync(storageFile, JSON.stringify(state), { mode: 0o600, flag: 'wx' });
    return storageFile;
  } finally {
    await ctx.dispose();
  }
}

/** Bound of one admin API call in setup. */
const ADMIN_REQUEST_TIMEOUT_MS = 30_000;

/** An admin API call with the dev token (setup only); it fails after ADMIN_REQUEST_TIMEOUT_MS. */
async function adminJSON(
  hub: HubState,
  method: string,
  apiPath: string,
  body?: unknown
): Promise<unknown> {
  const res = await fetch(`${hub.baseURL}${apiPath}`, {
    method,
    headers: { Authorization: `Bearer ${hub.devToken}`, 'Content-Type': 'application/json' },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    signal: AbortSignal.timeout(ADMIN_REQUEST_TIMEOUT_MS),
  });
  if (!res.ok) throw new Error(`${method} ${apiPath} answered ${res.status}`);
  return (await res.json()) as unknown;
}

/**
 * Issues a member-level test identity, writes its web-login code to a 0600
 * file in the state dir (the API token is removed unused) and gives it
 * projectRole in the fixture project (project-admin and project-owner open
 * its agents' exposed ports), or no project role when projectRole is null.
 */
async function createIdentity(
  hub: HubState,
  dir: string,
  projectId: string,
  name: string,
  projectRole: string | null
): Promise<{ name: string; email: string; codeFile: string }> {
  const tokenFile = path.join(dir, `${name}.token`);
  const issued = JSON.parse(
    runScionCLI(hub, [
      'hub',
      'test-identity',
      'issue',
      '--role',
      'member',
      '--purpose',
      'e2e port proxy',
      '--out',
      tokenFile,
      '--json',
    ])
  ) as { identity: { id: string; email: string } };
  fs.rmSync(tokenFile, { force: true });
  const codeFile = path.join(dir, `${name}.code`);
  runScionCLI(hub, ['hub', 'test-identity', 'web-code', issued.identity.id, '--out', codeFile]);
  if (projectRole === null) return { name, email: issued.identity.email, codeFile };

  const roles = (await adminJSON(
    hub,
    'GET',
    `/api/v1/projects/${projectId}/members/assignable-roles`
  )) as { items?: Array<{ id: string; name: string }> };
  const role = roles.items?.find((r) => r.name === projectRole);
  if (!role) throw new Error(`no assignable ${projectRole} role`);
  await adminJSON(hub, 'POST', `/api/v1/projects/${projectId}/members`, {
    roleDefinitionId: role.id,
    principalType: 'user',
    principalId: issued.identity.email,
  });
  return { name, email: issued.identity.email, codeFile };
}

export default globalSetup;
