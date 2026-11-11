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
 * Self-test of startHub's options (../harness/hub.ts) with a stand-in hub
 * binary. The stand-in is a Node script that records each invocation (argv
 * and the environment keys the checks read) as one JSON line, answers
 * `server test-infra mark`, and for `server start` serves a healthy
 * /healthz, writes a dev token file and prints its path, like the real
 * hub.
 *
 * Checked:
 * - with testIdentities and runtimeBroker, the test infrastructure marker
 *   is written to the same database before the server starts, and the
 *   server runs the co-located broker on loopback with --auto-provide
 *   next to --test-mode=member:<hub id> and the hub class "test";
 * - StartHubOptions.env reaches the hub process;
 * - env may not set a harness-owned key (hub id, hub class, HOME, broker
 *   host, test mode and test identity settings): startHub throws and runs
 *   nothing;
 * - runtimeBroker without testIdentities is refused;
 * - startupExperiments turns each experiment on through the admin
 *   experiments API and restarts the server on the same database, port,
 *   HOME and hub id; without it there is one start and no API call; a
 *   failure before the restart completes leaves no hub running;
 * - experiments turns each experiment on through the admin experiments
 *   API with no restart and reads the listing back; if one is not enabled
 *   afterwards, or the PUT fails, startHub throws and no hub is left
 *   running;
 * - restartHubWithoutDevAuth restarts the server with the same arguments
 *   except --dev-auth and rewrites the state file (new PID, no dev token);
 *   if the restarted server never gets healthy it is killed and the state
 *   file names it, without the dev token.
 */

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

import { expect, test } from '@playwright/test';

import {
  isHarnessOwnedEnv,
  keepHubState,
  readHubState,
  restartHubWithoutDevAuth,
  startHub,
  stopHub,
} from '../harness/hub.js';

interface Invocation {
  pid: number;
  argv: string[];
  env: Record<string, string | undefined>;
}

/** Environment keys the stand-in records. */
const RECORDED_ENV = [
  'HOME',
  'SCION_CLI_MODE',
  'SCION_SERVER_HUB_HUBID',
  'SCION_SERVER_HUB_CLASS',
  'SCION_SERVER_RUNTIMEBROKER_HOST',
  'SCION_SERVER_HUB_LAUNCHTIMEOUT',
  'SCION_IMAGE_REGISTRY',
];

const STAND_IN = `#!/usr/bin/env node
import * as fs from 'node:fs';
import * as http from 'node:http';
const argv = process.argv.slice(2);
const env = {};
for (const key of ${JSON.stringify(RECORDED_ENV)}) env[key] = process.env[key];
fs.appendFileSync(process.env.HARNESS_SELFTEST_LOG, JSON.stringify({ pid: process.pid, argv, env }) + '\\n');
if (argv[0] === 'server' && argv[1] === 'test-infra') {
  console.log('marked');
  process.exit(0);
}
if (argv[0] === 'server' && argv[1] === 'start') {
  const port = Number(argv[argv.indexOf('--web-port') + 1]);
  const tokenFile = process.env.HARNESS_SELFTEST_LOG + '.dev-token';
  fs.writeFileSync(tokenFile, 'scion_dev_0123456789abcdef\\n', { mode: 0o600 });
  // Experiments a PUT set; a GET lists them, enabled unless
  // HARNESS_SELFTEST_EXPERIMENTS_OFF=1 (an override the hub does not apply).
  const overrides = {};
  http
    .createServer((req, res) => {
      if (req.url === '/api/v1/admin/experiments') {
        let body = '';
        req.on('data', (chunk) => (body += chunk));
        req.on('end', () => {
          const record = { method: req.method, auth: req.headers.authorization, body };
          fs.appendFileSync(process.env.HARNESS_SELFTEST_LOG + '.requests', JSON.stringify(record) + '\\n');
          const fail = req.method === 'PUT' && process.env.HARNESS_SELFTEST_FAIL_PUT === '1';
          if (req.method === 'PUT' && !fail) Object.assign(overrides, JSON.parse(body).overrides);
          const off = process.env.HARNESS_SELFTEST_EXPERIMENTS_OFF === '1';
          const experiments = Object.keys(overrides).map((name) => ({
            name,
            enabled: !off,
            override: true,
          }));
          res.writeHead(fail ? 500 : 200, { 'Content-Type': 'application/json' });
          res.end(JSON.stringify({ revision: 7, experiments }));
        });
        return;
      }
      // HARNESS_SELFTEST_UNHEALTHY_WITHOUT_DEV_AUTH=1: a start without
      // --dev-auth never reports healthy.
      const unhealthy =
        process.env.HARNESS_SELFTEST_UNHEALTHY_WITHOUT_DEV_AUTH === '1' && !argv.includes('--dev-auth');
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ status: unhealthy ? 'degraded' : 'healthy' }));
    })
    .listen(port, '127.0.0.1', () => {
      console.log("export SCION_DEV_TOKEN=$(cat '" + tokenFile + "')");
    });
  process.on('SIGTERM', () => process.exit(0));
} else {
  process.exit(2);
}
`;

let dir = '';
let binary = '';
let logFile = '';
let stateFile = '';

test.beforeEach(() => {
  dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'scion-harness-selftest-')));
  binary = path.join(dir, 'scion-stand-in.mjs');
  fs.writeFileSync(binary, STAND_IN, { mode: 0o755 });
  logFile = path.join(dir, 'invocations.jsonl');
  stateFile = path.join(dir, 'hub-state.json');
  // isolatedEnv keeps non-SCION_* variables, so the stand-in sees this.
  process.env.HARNESS_SELFTEST_LOG = logFile;
});

test.afterEach(() => {
  stopHub(stateFile);
  delete process.env.HARNESS_SELFTEST_LOG;
  fs.rmSync(dir, { recursive: true, force: true });
});

function invocations(): Invocation[] {
  if (!fs.existsSync(logFile)) return [];
  return fs
    .readFileSync(logFile, 'utf-8')
    .trim()
    .split('\n')
    .map((line) => JSON.parse(line) as Invocation);
}

interface ExperimentsRequest {
  method: string;
  auth?: string;
  body: string;
}

/** The admin experiments requests the stand-in answered. */
function experimentsRequests(): ExperimentsRequest[] {
  const file = `${logFile}.requests`;
  if (!fs.existsSync(file)) return [];
  return fs
    .readFileSync(file, 'utf-8')
    .trim()
    .split('\n')
    .map((line) => JSON.parse(line) as ExperimentsRequest);
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

function flagValue(argv: string[], flag: string): string | undefined {
  const i = argv.indexOf(flag);
  return i >= 0 ? argv[i + 1] : undefined;
}

test('runtimeBroker with testIdentities marks the database before the server starts', async () => {
  const state = await startHub({
    testLogin: false,
    testIdentities: true,
    runtimeBroker: true,
    build: false,
    binary,
    stateFile,
    env: {
      SCION_SERVER_HUB_LAUNCHTIMEOUT: '15m',
      SCION_IMAGE_REGISTRY: 'registry.invalid/e2e',
    },
  });

  const calls = invocations();
  expect(calls.map((c) => c.argv.slice(0, 3).join(' '))).toEqual([
    'server test-infra mark',
    'server start --foreground',
  ]);
  const [mark, start] = calls;

  // The marker goes into the database the server then opens, for its hub id.
  expect(flagValue(mark.argv, '--db')).toBeTruthy();
  expect(flagValue(start.argv, '--db')).toBe(flagValue(mark.argv, '--db'));
  expect(flagValue(mark.argv, '--hub-id')).toBe(state.hubId);
  expect(start.env.SCION_SERVER_HUB_HUBID).toBe(state.hubId);

  // Test identities stay on, with the co-located broker on loopback.
  expect(start.argv).toContain(`--test-mode=member:${String(state.hubId)}`);
  expect(start.env.SCION_SERVER_HUB_CLASS).toBe('test');
  expect(start.argv).not.toContain('--enable-test-login');
  expect(start.argv).toContain('--enable-runtime-broker');
  expect(start.argv).not.toContain('--enable-runtime-broker=false');
  expect(start.argv).toContain('--auto-provide');
  expect(Number(flagValue(start.argv, '--runtime-broker-port'))).toBeGreaterThan(0);
  expect(flagValue(start.argv, '--runtime-broker-port')).not.toBe(String(state.port));
  expect(start.env.SCION_SERVER_RUNTIMEBROKER_HOST).toBe('127.0.0.1');

  // The caller's settings reach the server, not the mark step.
  expect(start.env.SCION_SERVER_HUB_LAUNCHTIMEOUT).toBe('15m');
  expect(start.env.SCION_IMAGE_REGISTRY).toBe('registry.invalid/e2e');
  expect(mark.env.SCION_SERVER_HUB_LAUNCHTIMEOUT).toBeUndefined();

  // Private HOME under the temp dir, human CLI mode.
  expect(start.env.HOME).toBe(state.homeDir);
  expect(start.env.SCION_CLI_MODE).toBe('human');
});

test('without runtimeBroker the server runs no broker', async () => {
  await startHub({ testLogin: false, testIdentities: true, build: false, binary, stateFile });
  const start = invocations()[1];
  expect(start.argv).toContain('--enable-runtime-broker=false');
  expect(start.argv).not.toContain('--auto-provide');
  expect(start.env.SCION_SERVER_RUNTIMEBROKER_HOST).toBeUndefined();
});

for (const key of [
  'HOME',
  'SCION_CLI_MODE',
  'SCION_SERVER_HUB_HUBID',
  'SCION_SERVER_HUB_CLASS',
  'SCION_SERVER_RUNTIMEBROKER_HOST',
  'SCION_SERVER_HUB_TESTIDENTITIES',
  'SCION_SERVER_HUB_TESTMODE',
  'SCION_SERVER_TESTMODE',
  'scion_server_hub_class',
]) {
  test(`env may not set the harness-owned key ${key}`, async () => {
    expect(isHarnessOwnedEnv(key)).toBe(true);
    await expect(
      startHub({
        testLogin: false,
        testIdentities: true,
        runtimeBroker: true,
        build: false,
        binary,
        stateFile,
        env: { SCION_SERVER_HUB_LAUNCHTIMEOUT: '15m', [key]: 'x' },
      })
    ).rejects.toThrow(`startHub: env may not set harness-owned keys: ${key}`);
    expect(invocations(), 'nothing ran').toEqual([]);
  });
}

test('ordinary settings are not harness-owned', () => {
  for (const key of ['SCION_SERVER_HUB_LAUNCHTIMEOUT', 'SCION_IMAGE_REGISTRY', 'PATH']) {
    expect(isHarnessOwnedEnv(key)).toBe(false);
  }
});

test('runtimeBroker needs testIdentities', async () => {
  await expect(startHub({ runtimeBroker: true, build: false, binary, stateFile })).rejects.toThrow(
    'startHub: runtimeBroker needs testIdentities'
  );
  expect(invocations(), 'nothing ran').toEqual([]);
});

test('E2E_KEEP_HUB_STATE=1 keeps the hub temp dir at stop; unset removes it', async () => {
  const previous = process.env.E2E_KEEP_HUB_STATE;
  try {
    process.env.E2E_KEEP_HUB_STATE = '1';
    expect(keepHubState()).toBe(true);
    const kept = await startHub({
      testLogin: false,
      testIdentities: true,
      build: false,
      binary,
      stateFile,
    });
    try {
      stopHub(stateFile);
      expect(fs.existsSync(kept.dbDir), 'kept with E2E_KEEP_HUB_STATE=1').toBe(true);
    } finally {
      fs.rmSync(kept.dbDir, { recursive: true, force: true });
    }

    delete process.env.E2E_KEEP_HUB_STATE;
    expect(keepHubState()).toBe(false);
    const removed = await startHub({
      testLogin: false,
      testIdentities: true,
      build: false,
      binary,
      stateFile,
    });
    stopHub(stateFile);
    expect(fs.existsSync(removed.dbDir), 'removed by default').toBe(false);
  } finally {
    if (previous === undefined) delete process.env.E2E_KEEP_HUB_STATE;
    else process.env.E2E_KEEP_HUB_STATE = previous;
  }
});

test('startupExperiments sets the overrides and restarts on the same database', async () => {
  const state = await startHub({
    testLogin: false,
    testIdentities: true,
    build: false,
    binary,
    stateFile,
    startupExperiments: ['hub.conduit', 'hub.example'],
  });

  const calls = invocations();
  expect(calls.map((c) => c.argv.slice(0, 3).join(' '))).toEqual([
    'server test-infra mark',
    'server start --foreground',
    'server start --foreground',
  ]);
  const [, first, second] = calls;
  // Same arguments (database, port, test mode with the hub id) and
  // environment (HOME, hub id) for the restart.
  expect(second.argv).toEqual(first.argv);
  expect(second.env).toEqual(first.env);
  expect(flagValue(second.argv, '--web-port')).toBe(String(state.port));
  expect(second.env.HOME).toBe(state.homeDir);

  // The overrides were set through the admin API, as the dev-auth admin,
  // before the restart; the first process is gone and the state names
  // the second.
  const requests = experimentsRequests();
  expect(requests.map((r) => r.method)).toEqual(['GET', 'PUT']);
  for (const r of requests) expect(r.auth).toBe(`Bearer ${state.devToken}`);
  expect(JSON.parse(requests[1].body)).toEqual({
    overrides: { 'hub.conduit': true, 'hub.example': true },
    expected_revision: 7,
  });
  expect(alive(first.pid), 'the first server stopped').toBe(false);
  expect(state.pid).toBe(second.pid);
});

test('without startupExperiments the server starts once and no experiment is set', async () => {
  await startHub({ testLogin: false, testIdentities: true, build: false, binary, stateFile });
  expect(invocations().map((c) => c.argv.slice(0, 2).join(' '))).toEqual([
    'server test-infra',
    'server start',
  ]);
  expect(experimentsRequests()).toEqual([]);
});

test('a failed startupExperiments restart leaves no hub running', async () => {
  process.env.HARNESS_SELFTEST_FAIL_PUT = '1';
  try {
    await expect(
      startHub({
        testLogin: false,
        testIdentities: true,
        build: false,
        binary,
        stateFile,
        startupExperiments: ['hub.conduit'],
      })
    ).rejects.toThrow('startHub: PUT /api/v1/admin/experiments answered 500');
  } finally {
    delete process.env.HARNESS_SELFTEST_FAIL_PUT;
  }
  const starts = invocations().filter((c) => c.argv[1] === 'start');
  expect(starts).toHaveLength(1);
  await expect.poll(() => alive(starts[0].pid), { message: 'the hub was stopped' }).toBe(false);
  expect(fs.existsSync(stateFile), 'no state file').toBe(false);
});

test('experiments sets the overrides without a restart and reads them back', async () => {
  const state = await startHub({
    testLogin: false,
    testIdentities: true,
    build: false,
    binary,
    stateFile,
    experiments: ['hub.async_agent_launch'],
  });

  const starts = invocations().filter((c) => c.argv[1] === 'start');
  expect(starts, 'one start, no restart').toHaveLength(1);
  expect(state.pid).toBe(starts[0].pid);
  const requests = experimentsRequests();
  expect(requests.map((r) => r.method)).toEqual(['GET', 'PUT', 'GET']);
  for (const r of requests) expect(r.auth).toBe(`Bearer ${state.devToken}`);
  expect(JSON.parse(requests[1].body)).toEqual({
    overrides: { 'hub.async_agent_launch': true },
    expected_revision: 7,
  });
});

for (const [name, envKey, message] of [
  [
    'an experiment that is not enabled after the PUT',
    'HARNESS_SELFTEST_EXPERIMENTS_OFF',
    'startHub: experiments not enabled after setting them on: hub.async_agent_launch',
  ],
  [
    'a failed experiments PUT',
    'HARNESS_SELFTEST_FAIL_PUT',
    'startHub: PUT /api/v1/admin/experiments answered 500',
  ],
] as const) {
  test(`${name} fails startHub and leaves no hub running`, async () => {
    process.env[envKey] = '1';
    try {
      await expect(
        startHub({
          testLogin: false,
          testIdentities: true,
          build: false,
          binary,
          stateFile,
          experiments: ['hub.async_agent_launch'],
        })
      ).rejects.toThrow(message);
    } finally {
      delete process.env[envKey];
    }
    const starts = invocations().filter((c) => c.argv[1] === 'start');
    expect(starts).toHaveLength(1);
    await expect.poll(() => alive(starts[0].pid), { message: 'the hub was stopped' }).toBe(false);
    expect(fs.existsSync(stateFile), 'no state file').toBe(false);
  });
}

test('restartHubWithoutDevAuth restarts without --dev-auth and rewrites the state', async () => {
  const before = await startHub({
    testLogin: false,
    testIdentities: true,
    build: false,
    binary,
    stateFile,
  });
  const after = await restartHubWithoutDevAuth(stateFile);

  const starts = invocations().filter((c) => c.argv[1] === 'start');
  expect(starts).toHaveLength(2);
  const [first, second] = starts;
  expect(first.argv).toContain('--dev-auth');
  expect(second.argv).toEqual(first.argv.filter((arg) => arg !== '--dev-auth'));
  expect(second.env).toEqual(first.env);
  await expect.poll(() => alive(first.pid), { message: 'the first server stopped' }).toBe(false);

  expect(after.pid).toBe(second.pid);
  expect(after.devToken).toBe('');
  expect(after.devAuth).toBe(false);
  expect({ ...after, pid: before.pid, devToken: before.devToken, devAuth: undefined }).toEqual({
    ...before,
    devAuth: undefined,
  });
  expect(readHubState(stateFile)).toEqual(after);
  await expect(restartHubWithoutDevAuth(stateFile)).rejects.toThrow(
    'restartHubWithoutDevAuth: the hub already runs without dev auth'
  );
});

test('restartHubWithoutDevAuth refuses a hub it did not start', async () => {
  await expect(restartHubWithoutDevAuth(path.join(dir, 'other-state.json'))).rejects.toThrow(
    'restartHubWithoutDevAuth: no hub started by startHub in this process'
  );
  expect(invocations(), 'nothing ran').toEqual([]);
});

test('a failed restartHubWithoutDevAuth kills the new hub and rewrites the state', async () => {
  test.setTimeout(90_000); // the health wait gives up after 30s
  // Set before startHub: the restart reuses the first start's environment.
  // The first start has --dev-auth, so it is healthy.
  process.env.HARNESS_SELFTEST_UNHEALTHY_WITHOUT_DEV_AUTH = '1';
  let before;
  try {
    before = await startHub({
      testLogin: false,
      testIdentities: true,
      build: false,
      binary,
      stateFile,
    });
    await expect(restartHubWithoutDevAuth(stateFile)).rejects.toThrow('did not become healthy');
  } finally {
    delete process.env.HARNESS_SELFTEST_UNHEALTHY_WITHOUT_DEV_AUTH;
  }
  const starts = invocations().filter((c) => c.argv[1] === 'start');
  expect(starts).toHaveLength(2);
  await expect
    .poll(() => alive(starts[0].pid), { message: 'the first server stopped' })
    .toBe(false);
  await expect
    .poll(() => alive(starts[1].pid), { message: 'the failed server was killed' })
    .toBe(false);
  const state = readHubState(stateFile);
  expect(state?.pid).toBe(starts[1].pid);
  expect(state?.devToken).toBe('');
  expect(state?.devAuth).toBe(false);
  expect(state?.dbDir).toBe(before.dbDir);
});
