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
 */

import { execSync, spawn, type ChildProcess } from 'node:child_process';
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
    timeout: 120_000,
  });
  console.log('[hub] Web assets built.');
}

/** Build the Go hub binary (make build). */
function buildHub(): void {
  console.log('[hub] Building Go hub binary...');
  execSync('make build', {
    cwd: WORKSPACE_ROOT,
    stdio: 'inherit',
    timeout: 180_000,
  });
  console.log('[hub] Hub binary built.');
}

/** Wait for the hub health endpoint to respond with a healthy status. */
async function waitForHealth(
  baseURL: string,
  timeoutMs = 30_000,
): Promise<void> {
  const start = Date.now();
  const healthURL = `${baseURL}/healthz`;
  console.log(`[hub] Waiting for health at ${healthURL}...`);

  while (Date.now() - start < timeoutMs) {
    try {
      const res = await fetch(healthURL, { signal: AbortSignal.timeout(2000) });
      if (res.ok) {
        const body = await res.json();
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
  throw new Error(`Hub did not become healthy within ${timeoutMs}ms`);
}

/**
 * Build assets and start the hub. Returns the hub state (port, PID, dev token, etc.).
 * The state is also written to a temp file for the teardown script.
 */
export async function startHub(): Promise<HubState> {
  // Build assets and binary
  buildWeb();
  buildHub();

  // Create a temp directory for the SQLite database
  const dbDir = fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-db-'));
  const dbPath = path.join(dbDir, 'e2e.db');

  // Find a free port
  const port = await findFreePort();
  const baseURL = `http://127.0.0.1:${port}`;

  console.log(`[hub] Starting hub on port ${port}, db: ${dbPath}`);

  // Start the hub binary
  const webAssetsDir = path.join(WEB_DIR, 'dist', 'client');
  const hubProcess = spawn(
    HUB_BINARY,
    [
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
      '--enable-test-login',
      '--session-secret',
      E2E_SESSION_SECRET,
      '--db',
      dbPath,
      '--web-assets-dir',
      webAssetsDir,
    ],
    {
      cwd: WORKSPACE_ROOT,
      stdio: ['ignore', 'pipe', 'pipe'],
      detached: true,
      env: {
        ...process.env,
        // The binary restricts commands by CLI mode; server requires human mode
        SCION_CLI_MODE: 'human',
      },
    },
  );

  // Capture the dev token from hub startup logs
  let devToken = '';
  // Token is scion_dev_ followed by hex; stop before any trailing quote/brace.
  const devTokenPattern = /SCION_DEV_TOKEN=(scion_dev_[0-9a-f]+)/;

  // Log hub output for debugging and capture dev token
  hubProcess.stdout?.on('data', (data: Buffer) => {
    const lines = data.toString().trim().split('\n');
    for (const line of lines) {
      console.log(`[hub:stdout] ${line}`);
      const match = line.match(devTokenPattern);
      if (match) {
        devToken = match[1];
      }
    }
  });

  hubProcess.stderr?.on('data', (data: Buffer) => {
    const lines = data.toString().trim().split('\n');
    for (const line of lines) {
      console.log(`[hub:stderr] ${line}`);
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

  if (!devToken) {
    throw new Error(
      'Failed to capture dev token from hub output. ' +
        'Ensure the hub is started with --dev-auth.',
    );
  }

  console.log(`[hub] Dev token captured.`);

  const state: HubState = {
    pid: hubProcess.pid!,
    port,
    baseURL,
    dbDir,
    devToken,
  };

  // Write state file for teardown
  fs.writeFileSync(STATE_FILE, JSON.stringify(state, null, 2));

  // Unref so the parent process can exit cleanly
  hubProcess.unref();

  return state;
}

/** Read the hub state from the temp file (used by teardown and tests). */
export function readHubState(): HubState | null {
  try {
    const raw = fs.readFileSync(STATE_FILE, 'utf-8');
    return JSON.parse(raw) as HubState;
  } catch {
    return null;
  }
}

/** Stop the hub process and clean up temp files. */
export function stopHub(): void {
  const state = readHubState();
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
    fs.unlinkSync(STATE_FILE);
  } catch {
    // Best-effort cleanup
  }

  console.log('[hub] Hub stopped and cleaned up.');
}
