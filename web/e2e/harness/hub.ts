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
import { execFileSync, execSync, spawn } from 'node:child_process';
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
  /** Dev-auth token for API calls (super-admin). */
  devToken: string;
  /** Hub id of a member-level test hub (testIdentities), else unset. */
  hubId?: string;
  /** Private HOME of a member-level test hub (under dbDir), else unset. */
  homeDir?: string;
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

/**
 * Build assets and start the hub. Returns the hub state (port, PID, dev token, etc.).
 * The state is also written to a temp file for the teardown script.
 */
export async function startHub(options: StartHubOptions = {}): Promise<HubState> {
  const testLogin = options.testLogin ?? true;
  const testIdentities = options.testIdentities ?? false;
  const stateFile = options.stateFile ?? STATE_FILE;

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
      HUB_BINARY,
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
    '--enable-runtime-broker=false',
    '--hosted',
    '--dev-auth',
  ];
  if (testLogin) args.push('--enable-test-login', '--session-secret', E2E_SESSION_SECRET);
  if (testIdentities) args.push(`--test-mode=member:${hubId}`);
  args.push('--db', dbPath, '--web-assets-dir', webAssetsDir);
  const hubProcess = spawn(HUB_BINARY, args, {
    cwd: homeDir ?? WORKSPACE_ROOT,
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: true,
    env,
  });

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

  if (!devTokenFile) {
    throw new Error(
      'Failed to capture the dev token file path from hub output. ' +
        'Ensure the hub is started with --dev-auth.'
    );
  }
  // A relative path is relative to the hub's working directory.
  const devToken = readDevTokenFile(path.resolve(homeDir ?? WORKSPACE_ROOT, devTokenFile));

  console.log(`[hub] Dev token captured.`);

  const state: HubState = {
    pid: hubProcess.pid!,
    port,
    baseURL,
    dbDir,
    devToken,
    ...(hubId ? { hubId } : {}),
    ...(homeDir ? { homeDir } : {}),
  };

  // Write state file for teardown. It holds the dev token: owner-only, and
  // created fresh (a stale file or a symlink at the path is removed first;
  // 'wx' never follows one).
  fs.rmSync(stateFile, { force: true });
  fs.writeFileSync(stateFile, JSON.stringify(state, null, 2), { mode: 0o600, flag: 'wx' });

  // Unref so the parent process can exit cleanly
  hubProcess.unref();

  return state;
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

/** Stop the hub process and clean up temp files. */
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

  // Clean up temp database
  try {
    fs.rmSync(state.dbDir, { recursive: true, force: true });
  } catch {
    // Best-effort cleanup
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
