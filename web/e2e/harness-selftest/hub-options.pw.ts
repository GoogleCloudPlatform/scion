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
 * - runtimeBroker without testIdentities is refused.
 */

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

import { expect, test } from '@playwright/test';

import { isHarnessOwnedEnv, keepHubState, startHub, stopHub } from '../harness/hub.js';

interface Invocation {
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
  'SCION_SERVER_HUB_ASYNCAGENTLAUNCH',
  'SCION_IMAGE_REGISTRY',
];

const STAND_IN = `#!/usr/bin/env node
import * as fs from 'node:fs';
import * as http from 'node:http';
const argv = process.argv.slice(2);
const env = {};
for (const key of ${JSON.stringify(RECORDED_ENV)}) env[key] = process.env[key];
fs.appendFileSync(process.env.HARNESS_SELFTEST_LOG, JSON.stringify({ argv, env }) + '\\n');
if (argv[0] === 'server' && argv[1] === 'test-infra') {
  console.log('marked');
  process.exit(0);
}
if (argv[0] === 'server' && argv[1] === 'start') {
  const port = Number(argv[argv.indexOf('--web-port') + 1]);
  const tokenFile = process.env.HARNESS_SELFTEST_LOG + '.dev-token';
  fs.writeFileSync(tokenFile, 'scion_dev_0123456789abcdef\\n', { mode: 0o600 });
  http
    .createServer((req, res) => {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ status: 'healthy' }));
    })
    .listen(port, '127.0.0.1', () => console.log("export SCION_DEV_TOKEN=$(cat '" + tokenFile + "')"));
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
      SCION_SERVER_HUB_ASYNCAGENTLAUNCH: 'true',
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
  expect(start.env.SCION_SERVER_HUB_ASYNCAGENTLAUNCH).toBe('true');
  expect(start.env.SCION_IMAGE_REGISTRY).toBe('registry.invalid/e2e');
  expect(mark.env.SCION_SERVER_HUB_ASYNCAGENTLAUNCH).toBeUndefined();

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
        env: { SCION_SERVER_HUB_ASYNCAGENTLAUNCH: 'true', [key]: 'x' },
      })
    ).rejects.toThrow(`startHub: env may not set harness-owned keys: ${key}`);
    expect(invocations(), 'nothing ran').toEqual([]);
  });
}

test('ordinary settings are not harness-owned', () => {
  for (const key of ['SCION_SERVER_HUB_ASYNCAGENTLAUNCH', 'SCION_IMAGE_REGISTRY', 'PATH']) {
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
