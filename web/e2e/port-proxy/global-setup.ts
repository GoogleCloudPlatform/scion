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
 *
 * The hub binary (make build), the web assets (npm run build) and the
 * helper binary must exist with E2E_PREBUILT=1; otherwise they are built
 * here. E2E_FAKEAGENT_BINARY overrides the helper path.
 */

import { execFileSync, spawn } from 'node:child_process';
import * as fs from 'node:fs';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';

import { startHub } from '../harness/hub.js';
import globalTeardown from './global-teardown.js';
import {
  createStateDir,
  hubStateFile,
  SINGLE_PROBE_TEXT,
  type SuiteAgent,
  TRANSPORTS,
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
    globalTeardown();
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
    startupExperiments: ['hub.conduit'],
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
}

export default globalSetup;
