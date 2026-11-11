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
 * Hub process manager for E2E tests.
 *
 * Builds the web frontend and the Go hub binary, then starts the hub on an
 * ephemeral port with an in-memory/temp-dir SQLite store, dev-auth enabled,
 * and the test-login endpoint available. Waits for the health endpoint before
 * returning.
 *
 * With `testIdentities: true` (and `testLogin: false`) it starts a throwaway
 * member-level test hub instead: hub-issued test identities on, the test
 * infrastructure marker written to the temp database first (it holds the
 * hub-local key for test identity web sessions), and a private HOME and
 * environment under the temp dir. See README.md ("Test identity browser
 * sessions").
 */

import * as crypto from 'node:crypto';
import { type ChildProcess, execFileSync, execSync, spawn } from 'node:child_process';
import * as fs from 'node:fs';
import * as net from 'node:net';
import * as os from 'node:os';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';

/** Well-known session secret for deterministic signing key derivation. */
export const E2E_SESSION_SECRET = 'e2e-test-secret';

/** Path to the built hub binary. */
const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const WORKSPACE_ROOT = path.resolve(__dirname, '..', '..', '..');
const WEB_DIR = path.resolve(WORKSPACE_ROOT, 'web');
const BUILD_DIR = path.resolve(WORKSPACE_ROOT, 'build');
const HUB_BINARY = path.resolve(BUILD_DIR, 'scion');

/** State file for sharing hub info between setup and teardown. */
const STATE_FILE = path.join(os.tmpdir(), 'scion-e2e-hub-state.json');

export interface HubState {
  pid: number;
  port: number;
  baseURL: string;
  dbDir: string;
  /**
   * Dev-auth token for API calls (super-admin). Empty after
   * restartHubWithoutDevAuth: that hub no longer accepts it.
   */
  devToken: string;
  /** False after restartHubWithoutDevAuth; unset (dev auth on) otherwise. */
  devAuth?: boolean;
  /** Hub id of a member-level test hub (testIdentities), else unset. */
  hubId?: string;
  /** Private HOME of a member-level test hub (under dbDir), else unset. */
  homeDir?: string;
  /** The `server start` flags, by name only (no values), in start order. */
  startFlags?: string[];
}

/** The flag names of a command line (`--name` or `--name=value` gives `--name`); values are dropped. */
export function flagNames(args: readonly string[]): string[] {
  return args.filter((a) => a.startsWith('--')).map((a) => a.split('=')[0]);
}

/** Options of startHub. The defaults start the hub the main suite uses. */
export interface StartHubOptions {
  /** Pass --enable-test-login (and the fixed --session-secret). Default true. */
  testLogin?: boolean;
  /**
   * Start a member-level test hub: --test-mode=member:<hub id> with a random
   * hub id, server.hub.class: test (SCION_SERVER_HUB_CLASS), the test
   * infrastructure marker written by
   * `scion server test-infra mark` into the temp database before start, and
   * a private HOME and SCION_*-free environment under the temp dir.
   * Default false.
   */
  testIdentities?: boolean;
  /** Build the web assets and the binary first. Default true. */
  build?: boolean;
  /** State file shared with stopHub. Default: the main suite's. */
  stateFile?: string;
  /**
   * Extra environment of the hub process, e.g. supported configuration
   * such as SCION_IMAGE_REGISTRY or SCION_SERVER_HUB_LAUNCHTIMEOUT.
   * Experiments are not environment settings: use experiments or
   * startupExperiments.
   * Applied over the harness environment (also over the SCION_*-free
   * environment of a member-level test hub). Keys the harness owns
   * (HARNESS_OWNED_ENV) are refused.
   */
  env?: Record<string, string>;
  /**
   * Run the co-located runtime broker in the same process
   * (--enable-runtime-broker on a free loopback port, --auto-provide), so a
   * hub-managed project created on this hub gets it as its provider.
   * Requires testIdentities (the broker writes under the private HOME).
   * The broker runs containers only where a container runtime works; it
   * registers either way. Default false.
   */
  runtimeBroker?: boolean;
  /** Hub binary to run. Default: build/scion. For the harness's own tests. */
  binary?: string;
  /**
   * Experiments the hub reads only at process start (such as hub.conduit,
   * whose relay starts only if it is on at startup). Each one is turned on
   * through the admin experiments API, and then the hub is restarted on
   * the same database, port, HOME and hub id. Default: none (one start).
   */
  startupExperiments?: string[];
  /**
   * Experiments to turn on for the run. Each one is set on through the
   * admin experiments API once the hub is healthy (after any
   * startupExperiments restart), with no restart, and then read back:
   * startHub throws, and kills the hub, unless every one is enabled. The
   * override is stored in the hub's database, so it survives
   * restartHubWithoutDevAuth. Default: none.
   */
  experiments?: string[];
}

/**
 * Environment keys the harness sets itself; StartHubOptions.env may not
 * set them.
 */
export const HARNESS_OWNED_ENV: readonly string[] = [
  'HOME',
  'SCION_CLI_MODE',
  'SCION_SERVER_HUB_HUBID',
  'SCION_SERVER_HUB_CLASS',
  'SCION_SERVER_RUNTIMEBROKER_HOST',
];

/**
 * Prefixes of environment keys StartHubOptions.env may not set either: the
 * test mode and test identity settings stay with the harness (the
 * --test-mode flag and the marker).
 */
export const HARNESS_OWNED_ENV_PREFIXES: readonly string[] = [
  'SCION_SERVER_HUB_TEST',
  'SCION_SERVER_TEST',
];

/** Whether StartHubOptions.env may not set key. */
export function isHarnessOwnedEnv(key: string): boolean {
  const upper = key.toUpperCase();
  return (
    HARNESS_OWNED_ENV.includes(upper) ||
    HARNESS_OWNED_ENV_PREFIXES.some((prefix) => upper.startsWith(prefix))
  );
}

/** Throws if extra sets a harness-owned key. */
function checkExtraEnv(extra: Record<string, string>): void {
  const owned = Object.keys(extra).filter(isHarnessOwnedEnv);
  if (owned.length > 0) {
    throw new Error(`startHub: env may not set harness-owned keys: ${owned.join(', ')}`);
  }
}

/** Acknowledgement flag of `scion server test-infra mark`. */
const TEST_INFRA_MARK_ACK = '--i-understand-this-hub-can-mint-super-admins';

/**
 * The hub prints the dev token file path, not the token value:
 * `export SCION_DEV_TOKEN=$(cat '<path>')`, with the path single-quoted
 * for a POSIX shell. Group 1 is the quoted body; see unquoteShellSingle.
 */
const DEV_TOKEN_FILE_PATTERN = /SCION_DEV_TOKEN=\$\(cat '((?:[^']|'\\'')*)'\)/;
/** Any dev token value, for redaction in logs. */
const DEV_TOKEN_ANY = /scion_dev_[0-9a-f]+/g;

/** Hub log lines are echoed with any dev token redacted. */
function redact(line: string): string {
  return line.replace(DEV_TOKEN_ANY, 'scion_dev_<redacted>');
}

/**
 * Returns the message of a hub log line. The hub logs JSON records (the
 * message is "msg", or "message" with GCP logging); other lines are used as
 * they are.
 */
function hubLogMessage(line: string): string {
  if (line.startsWith('{')) {
    try {
      const rec = JSON.parse(line) as { msg?: unknown; message?: unknown };
      const msg = rec.msg ?? rec.message;
      if (typeof msg === 'string') return msg;
    } catch {
      // Not JSON after all: fall through to the raw line.
    }
  }
  return line;
}

/** Undoes the hub's single quoting: each '\'' in the body is one quote. */
function unquoteShellSingle(body: string): string {
  return body.replace(/'\\''/g, "'");
}

/** Reads the dev token from the file the hub printed; throws if it is empty. */
function readDevTokenFile(file: string): string {
  if (!fs.existsSync(file)) {
    throw new Error(`Dev token file ${file}, printed by the hub, does not exist.`);
  }
  const token = fs.readFileSync(file, 'utf8').trim();
  if (!token) {
    throw new Error(`Dev token file ${file} is empty.`);
  }
  return token;
}

/** Path of the hub binary the harness builds and runs. */
export function hubBinaryPath(): string {
  return HUB_BINARY;
}

/**
 * The environment of a member-level test hub and of CLI calls against it:
 * the caller's environment without any SCION_* variable (inherited hub,
 * project or token settings must not reach the throwaway hub),
 * with a private HOME and human CLI mode.
 */
function isolatedEnv(homeDir: string, extra: Record<string, string> = {}): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(process.env)) {
    if (!key.startsWith('SCION_')) env[key] = value;
  }
  return { ...env, HOME: homeDir, SCION_CLI_MODE: 'human', ...extra };
}

/** Find an available port by briefly binding to port 0. */
async function findFreePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.listen(0, '127.0.0.1', () => {
      const addr = server.address();
      if (addr && typeof addr === 'object') {
        const port = addr.port;
        server.close(() => resolve(port));
      } else {
        server.close(() => reject(new Error('Could not determine port')));
      }
    });
    server.on('error', reject);
  });
}

/** Build web assets (npm run build). */
function buildWeb(): void {
  console.log('[hub] Building web assets...');
  execSync('npm run build', {
    cwd: WEB_DIR,
    stdio: 'inherit',
    timeout: 300_000,
  });
  console.log('[hub] Web assets built.');
}

/** Build the Go hub binary (make build). */
function buildHub(): void {
  console.log('[hub] Building Go hub binary...');
  execSync('make build', {
    cwd: WORKSPACE_ROOT,
    stdio: 'inherit',
    timeout: 600_000,
  });
  console.log('[hub] Hub binary built.');
}

/** Wait for the hub health endpoint to respond with a healthy status. */
async function waitForHealth(baseURL: string, timeoutMs = 30_000): Promise<void> {
  const start = Date.now();
  const healthURL = `${baseURL}/healthz`;
  console.log(`[hub] Waiting for health at ${healthURL}...`);

  // Last body seen, so a hub that answers but never goes healthy (e.g.
  // degraded because its co-located broker failed to register) times out
  // with the failing checks visible instead of no explanation.
  let lastBody = '';
  while (Date.now() - start < timeoutMs) {
    try {
      const res = await fetch(healthURL, { signal: AbortSignal.timeout(2000) });
      if (res.ok) {
        lastBody = await res.text();
        const body = JSON.parse(lastBody) as { status?: unknown };
        if (body.status === 'healthy') {
          console.log('[hub] Hub is healthy.');
          return;
        }
      }
    } catch {
      // Hub not ready yet — retry
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(
    `Hub did not become healthy within ${timeoutMs}ms` +
      (lastBody ? `; last /healthz response: ${lastBody}` : ' (no response)')
  );
}

/** How a hub started by startHub in this process was launched (by state file). */
interface HubLaunch {
  binary: string;
  args: string[];
  env: NodeJS.ProcessEnv;
  cwd: string;
  baseURL: string;
  child: ChildProcess;
}
const launches = new Map<string, HubLaunch>();

/** A started hub process and the dev token it printed. */
interface LaunchedHub {
  hubProcess: ChildProcess;
  devToken: string;
}

/**
 * Spawns the hub (its own process group), echoes its output with any dev
 * token redacted, waits for health and reads the dev token from the file
 * the hub printed.
 */
async function launchHub(
  binary: string,
  args: string[],
  env: NodeJS.ProcessEnv,
  cwd: string,
  baseURL: string,
  onSpawn?: (child: ChildProcess) => void,
  requireDevToken = true
): Promise<LaunchedHub> {
  const hubProcess = spawn(binary, args, {
    cwd,
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: true,
    env,
  });
  onSpawn?.(hubProcess);

  // Capture the dev token file path from hub startup logs; the token is
  // read from that file once the hub is healthy.
  let devTokenFile = '';
  const captureDevTokenFile = (line: string) => {
    const match = hubLogMessage(line).match(DEV_TOKEN_FILE_PATTERN);
    if (match) {
      devTokenFile = unquoteShellSingle(match[1]);
    }
  };

  // Log hub output for debugging and capture the dev token file path
  hubProcess.stdout?.on('data', (data: Buffer) => {
    const lines = data.toString().trim().split('\n');
    for (const line of lines) {
      console.log(`[hub:stdout] ${redact(line)}`);
      captureDevTokenFile(line);
    }
  });

  hubProcess.stderr?.on('data', (data: Buffer) => {
    const lines = data.toString().trim().split('\n');
    for (const line of lines) {
      console.log(`[hub:stderr] ${redact(line)}`);
      captureDevTokenFile(line);
    }
  });

  hubProcess.on('error', (err) => {
    console.error(`[hub] Process error: ${err.message}`);
  });

  hubProcess.on('exit', (code, signal) => {
    console.log(`[hub] Process exited: code=${code}, signal=${signal}`);
  });

  // Wait for health
  await waitForHealth(baseURL);

  // A hub started without --dev-auth prints no dev token.
  if (!requireDevToken) return { hubProcess, devToken: '' };

  if (!devTokenFile) {
    throw new Error(
      'Failed to capture the dev token file path from hub output. ' +
        'Ensure the hub is started with --dev-auth.'
    );
  }
  // A relative path is relative to the hub's working directory.
  const devToken = readDevTokenFile(path.resolve(cwd, devTokenFile));

  console.log(`[hub] Dev token captured.`);
  return { hubProcess, devToken };
}

/** Turns each named experiment on through the admin experiments API. */
async function setExperimentOverrides(
  baseURL: string,
  devToken: string,
  names: string[]
): Promise<void> {
  const url = `${baseURL}/api/v1/admin/experiments`;
  const headers = { Authorization: `Bearer ${devToken}`, 'Content-Type': 'application/json' };
  const read = await fetch(url, { headers, signal: AbortSignal.timeout(10_000) });
  if (read.status !== 200) {
    throw new Error(`startHub: GET /api/v1/admin/experiments answered ${read.status}`);
  }
  const { revision } = (await read.json()) as { revision: number };
  const overrides = Object.fromEntries(names.map((name) => [name, true]));
  const put = await fetch(url, {
    method: 'PUT',
    headers,
    body: JSON.stringify({ overrides, expected_revision: revision }),
    signal: AbortSignal.timeout(10_000),
  });
  if (put.status !== 200) {
    throw new Error(`startHub: PUT /api/v1/admin/experiments answered ${put.status}`);
  }
}

/**
 * Reads the admin experiments listing back and throws unless every named
 * experiment is enabled: the guard that a suite really runs with them on.
 */
async function assertExperimentsEnabled(
  baseURL: string,
  devToken: string,
  names: string[]
): Promise<void> {
  const url = `${baseURL}/api/v1/admin/experiments`;
  const read = await fetch(url, {
    headers: { Authorization: `Bearer ${devToken}` },
    signal: AbortSignal.timeout(10_000),
  });
  if (read.status !== 200) {
    throw new Error(`startHub: GET /api/v1/admin/experiments answered ${read.status}`);
  }
  const { experiments } = (await read.json()) as {
    experiments?: Array<{ name: string; enabled: boolean }>;
  };
  const off = names.filter(
    (name) => (experiments ?? []).find((e) => e.name === name)?.enabled !== true
  );
  if (off.length > 0) {
    throw new Error(`startHub: experiments not enabled after setting them on: ${off.join(', ')}`);
  }
}

/** SIGKILLs a hub started by launchHub (its process group); a gone process is fine. */
function killProcessGroup(child: ChildProcess): void {
  if (!child.pid || child.exitCode !== null || child.signalCode !== null) return;
  try {
    process.kill(-child.pid, 'SIGKILL');
  } catch (err: unknown) {
    if ((err as NodeJS.ErrnoException).code !== 'ESRCH') {
      console.warn(`[hub] Could not kill the hub (PID ${child.pid}): ${String(err)}`);
    }
  }
}

/** Stops a hub started by launchHub (its process group) and waits for its exit. */
async function stopProcessGroup(child: ChildProcess, timeoutMs = 30_000): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const exited = new Promise<void>((resolve) => child.once('exit', () => resolve()));
  try {
    process.kill(-child.pid!, 'SIGTERM');
  } catch (err: unknown) {
    if ((err as NodeJS.ErrnoException).code === 'ESRCH') return;
    throw err;
  }
  let timer: NodeJS.Timeout | undefined;
  const timedOut = new Promise<'timeout'>((resolve) => {
    timer = setTimeout(() => resolve('timeout'), timeoutMs);
  });
  const result = await Promise.race([exited, timedOut]);
  clearTimeout(timer);
  if (result === 'timeout') {
    throw new Error(`startHub: the hub (PID ${child.pid}) did not exit within ${timeoutMs}ms`);
  }
}

/**
 * Build assets and start the hub. Returns the hub state (port, PID, dev token, etc.).
 * The state is also written to a temp file for the teardown script.
 */
export async function startHub(options: StartHubOptions = {}): Promise<HubState> {
  const testLogin = options.testLogin ?? true;
  const testIdentities = options.testIdentities ?? false;
  const stateFile = options.stateFile ?? STATE_FILE;
  const binary = options.binary ?? HUB_BINARY;
  const extraEnv = options.env ?? {};
  checkExtraEnv(extraEnv);
  if (options.runtimeBroker && !testIdentities) {
    throw new Error('startHub: runtimeBroker needs testIdentities (a private HOME)');
  }

  // Build assets and binary
  if (options.build ?? true) {
    buildWeb();
    buildHub();
  }

  // Create a temp directory for the SQLite database
  const dbDir = fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-db-'));
  const dbPath = path.join(dbDir, 'e2e.db');

  // A member-level test hub gets its own HOME and hub id, and its database
  // is marked as test infrastructure before the hub starts (the hub reads
  // the marker, and the web session key in it, only at startup).
  let hubId: string | undefined;
  let homeDir: string | undefined;
  let env: NodeJS.ProcessEnv = {
    ...process.env,
    // The binary restricts commands by CLI mode; server requires human mode
    SCION_CLI_MODE: 'human',
  };
  if (testIdentities) {
    hubId = crypto.randomBytes(6).toString('hex');
    homeDir = path.join(dbDir, 'home');
    fs.mkdirSync(homeDir, { mode: 0o700 });
    // --test-mode=member requires the hub class "test" and the marker for
    // this hub id (written below); the hub id binding is in the flag value.
    env = isolatedEnv(homeDir, { SCION_SERVER_HUB_HUBID: hubId, SCION_SERVER_HUB_CLASS: 'test' });
    console.info(`[hub] Marking ${dbPath} as test infrastructure for hub ${hubId}...`);
    const markOut = execFileSync(
      binary,
      [
        'server',
        'test-infra',
        'mark',
        '--hub-id',
        hubId,
        '--db',
        dbPath,
        TEST_INFRA_MARK_ACK,
        '--operator',
        'e2e-harness',
      ],
      { cwd: homeDir, env, encoding: 'utf-8', timeout: 60_000 }
    );
    for (const line of markOut.trim().split('\n')) console.info(`[hub:mark] ${redact(line)}`);
  }

  // The hub process gets the caller's extra environment; the mark step
  // above does not need it. Harness-owned keys were refused above.
  env = { ...env, ...extraEnv };
  // The co-located broker listens on loopback, like the hub (it would
  // default to all interfaces in hosted mode).
  if (options.runtimeBroker) env.SCION_SERVER_RUNTIMEBROKER_HOST = '127.0.0.1';

  // Find a free port
  const port = await findFreePort();
  const baseURL = `http://127.0.0.1:${port}`;

  console.log(`[hub] Starting hub on port ${port}, db: ${dbPath}`);

  // Start the hub binary
  const webAssetsDir = path.join(WEB_DIR, 'dist', 'client');
  const args = [
    'server',
    'start',
    '--foreground',
    '--host',
    '127.0.0.1',
    '--web-port',
    String(port),
    '--enable-hub',
    '--enable-web',
    '--hosted',
    '--dev-auth',
  ];
  if (options.runtimeBroker) {
    // Two probes can return the same free port; the broker needs its own.
    let brokerPort = await findFreePort();
    for (let i = 0; i < 10 && brokerPort === port; i++) brokerPort = await findFreePort();
    if (brokerPort === port) throw new Error('startHub: no free port for the runtime broker');
    args.push(
      '--enable-runtime-broker',
      '--runtime-broker-port',
      String(brokerPort),
      '--auto-provide'
    );
  } else {
    args.push('--enable-runtime-broker=false');
  }
  if (testLogin) args.push('--enable-test-login', '--session-secret', E2E_SESSION_SECRET);
  if (testIdentities) args.push(`--test-mode=member:${hubId}`);
  args.push('--db', dbPath, '--web-assets-dir', webAssetsDir);
  const cwd = homeDir ?? WORKSPACE_ROOT;
  let { hubProcess, devToken } = await launchHub(binary, args, env, cwd, baseURL);

  // Startup-read experiments: set the overrides through the admin API,
  // then restart the hub on the same database, port, HOME and hub id, so
  // they apply from process start.
  const startupExperiments = options.startupExperiments ?? [];
  if (startupExperiments.length > 0) {
    // No state file exists yet, so stopHub could not find a hub left
    // running by a failure here: kill whichever process is current.
    let current: ChildProcess = hubProcess;
    try {
      await setExperimentOverrides(baseURL, devToken, startupExperiments);
      console.info(
        `[hub] Restarting the hub for startup experiments: ${startupExperiments.join(', ')}`
      );
      await stopProcessGroup(hubProcess);
      ({ hubProcess, devToken } = await launchHub(binary, args, env, cwd, baseURL, (child) => {
        current = child;
      }));
    } catch (err) {
      killProcessGroup(current);
      throw err;
    }
  }

  // Run-time experiments: set on through the admin API (no restart), then
  // read back, so a suite never runs with one silently off.
  const runExperiments = options.experiments ?? [];
  if (runExperiments.length > 0) {
    try {
      await setExperimentOverrides(baseURL, devToken, runExperiments);
      await assertExperimentsEnabled(baseURL, devToken, runExperiments);
      console.info(`[hub] Experiments on: ${runExperiments.join(', ')}`);
    } catch (err) {
      killProcessGroup(hubProcess);
      throw err;
    }
  }

  const state: HubState = {
    pid: hubProcess.pid!,
    port,
    baseURL,
    dbDir,
    devToken,
    ...(hubId ? { hubId } : {}),
    ...(homeDir ? { homeDir } : {}),
    startFlags: flagNames(args),
  };

  // Write state file for teardown. It holds the dev token: owner-only, and
  // created fresh (a stale file or a symlink at the path is removed first;
  // 'wx' never follows one).
  fs.rmSync(stateFile, { force: true });
  fs.writeFileSync(stateFile, JSON.stringify(state, null, 2), { mode: 0o600, flag: 'wx' });

  // Unref so the parent process can exit cleanly
  hubProcess.unref();

  launches.set(path.resolve(stateFile), { binary, args, env, cwd, baseURL, child: hubProcess });
  return state;
}

/**
 * Restarts a hub that startHub started in this process on the same
 * database, port, HOME and hub id, with the same arguments except
 * --dev-auth: requests without a credential are then no longer signed in
 * as the dev user, as on a hub in production. Run the setup that needs the
 * dev token (test identities, memberships, fixtures) before this; the dev
 * token is not accepted afterwards. Test identity web sessions and
 * web-login codes, agent and broker credentials keep working (they live in
 * the database). The state file is rewritten with the new PID, an empty
 * devToken and devAuth: false. If the restarted hub does not get healthy,
 * it is killed, the state file is rewritten the same way with the killed
 * process's PID, and the call throws.
 */
export async function restartHubWithoutDevAuth(stateFile: string = STATE_FILE): Promise<HubState> {
  const key = path.resolve(stateFile);
  const launch = launches.get(key);
  const state = readHubState(stateFile);
  if (!launch || !state) {
    throw new Error(
      'restartHubWithoutDevAuth: no hub started by startHub in this process for that state file'
    );
  }
  if (!launch.args.includes('--dev-auth')) {
    throw new Error('restartHubWithoutDevAuth: the hub already runs without dev auth');
  }
  const args = launch.args.filter((arg) => arg !== '--dev-auth');
  console.info('[hub] Restarting the hub without dev auth...');
  await stopProcessGroup(launch.child);
  let current: ChildProcess | undefined;
  let hubProcess: ChildProcess;
  try {
    ({ hubProcess } = await launchHub(
      launch.binary,
      args,
      launch.env,
      launch.cwd,
      launch.baseURL,
      (child) => {
        current = child;
      },
      false
    ));
  } catch (err) {
    if (current) killProcessGroup(current);
    launches.delete(key);
    // Rewritten so it names the last hub process (now stopped) and no
    // longer offers the dev token; stopHub still removes the temp dir.
    const failed: HubState = {
      ...state,
      pid: current?.pid ?? state.pid,
      devToken: '',
      devAuth: false,
    };
    fs.rmSync(stateFile, { force: true });
    fs.writeFileSync(stateFile, JSON.stringify(failed, null, 2), { mode: 0o600, flag: 'wx' });
    throw err;
  }
  const next: HubState = { ...state, pid: hubProcess.pid!, devToken: '', devAuth: false };
  fs.rmSync(stateFile, { force: true });
  fs.writeFileSync(stateFile, JSON.stringify(next, null, 2), { mode: 0o600, flag: 'wx' });
  hubProcess.unref();
  launches.set(key, { ...launch, args, child: hubProcess });
  return next;
}

/** Read the hub state from the temp file (used by teardown and tests). */
export function readHubState(stateFile: string = STATE_FILE): HubState | null {
  try {
    const raw = fs.readFileSync(stateFile, 'utf-8');
    return JSON.parse(raw) as HubState;
  } catch {
    return null;
  }
}

/**
 * Whether stopHub keeps the hub's temp dir (E2E_KEEP_HUB_STATE=1). Off by
 * default. The kept dir holds the database and the private HOME, which
 * contain hub credentials (dev token, session keys, test identities): do
 * not copy it into evidence or artifacts, and remove it by hand when done.
 */
export function keepHubState(): boolean {
  return process.env.E2E_KEEP_HUB_STATE === '1';
}

/** Stop the hub process and clean up temp files (see keepHubState). */
export function stopHub(stateFile: string = STATE_FILE): void {
  const state = readHubState(stateFile);
  if (!state) {
    console.log('[hub] No hub state found — nothing to stop.');
    return;
  }

  console.log(`[hub] Stopping hub (PID ${state.pid})...`);

  try {
    // Kill the process group (negative PID kills the group)
    process.kill(-state.pid, 'SIGTERM');
  } catch (err: unknown) {
    if ((err as NodeJS.ErrnoException).code !== 'ESRCH') {
      // eslint-disable-next-line @typescript-eslint/restrict-template-expressions -- template conversion of the caught value is kept so the logged text stays the same
      console.warn(`[hub] Error stopping hub: ${err}`);
    }
  }

  // Clean up temp database, unless E2E_KEEP_HUB_STATE=1 asks to keep it
  // (database, private HOME and hub files) for inspection after a run.
  if (keepHubState()) {
    console.info(
      `[hub] E2E_KEEP_HUB_STATE=1: kept the hub state at ${state.dbDir} ` +
        '(contains hub credentials; do not copy it into evidence or artifacts)'
    );
  } else {
    try {
      fs.rmSync(state.dbDir, { recursive: true, force: true });
    } catch {
      // Best-effort cleanup
    }
  }

  // Clean up state file
  try {
    fs.unlinkSync(stateFile);
  } catch {
    // Best-effort cleanup
  }

  console.log('[hub] Hub stopped and cleaned up.');
}

/**
 * Runs the scion CLI against a member-level test hub (started with
 * `testIdentities: true`) as the dev-auth admin, in the hub's private HOME
 * and environment, and returns its stdout. --global is added (there is no
 * local project). A failure throws with the exit
 * status and stderr (the CLI never prints a token or web-login code).
 */
export function runScionCLI(state: HubState, args: string[]): string {
  if (!state.homeDir) {
    throw new Error('runScionCLI needs a hub started with testIdentities: true');
  }
  const env = isolatedEnv(state.homeDir, {
    SCION_HUB_ENDPOINT: state.baseURL,
    SCION_HUB_TOKEN: state.devToken,
    SCION_DEV_TOKEN: state.devToken,
  });
  try {
    return execFileSync(HUB_BINARY, ['--global', ...args], {
      cwd: state.homeDir,
      env,
      encoding: 'utf-8',
      timeout: 60_000,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
  } catch (err) {
    const e = err as { status?: number; stderr?: string };
    // eslint-disable-next-line preserve-caught-error -- the exec error carries raw stderr and argv; only the redacted text is kept
    throw new Error(
      `scion ${args.slice(0, 3).join(' ')} failed (exit ${String(e.status)}): ${redact(String(e.stderr ?? '')).trim()}`
    );
  }
}
