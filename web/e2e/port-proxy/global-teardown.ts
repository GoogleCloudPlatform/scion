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
 * Global teardown of the port proxy journeys: stops the helper, the fixture
 * app and the hub, then removes the state dir.
 */

import { stopHub } from '../harness/hub.js';
import { hubStateFile, readSuiteState, removeStateDir } from './state.js';

/** Sends SIGTERM to a process group started by global setup. */
function stopGroup(pid: number, what: string): void {
  if (!pid) return;
  try {
    process.kill(-pid, 'SIGTERM');
  } catch (err: unknown) {
    if ((err as NodeJS.ErrnoException).code !== 'ESRCH') {
      console.warn(`[teardown] could not stop ${what}: ${String((err as Error).message)}`);
    }
  }
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

/**
 * Bound of the wait for the helper and the fixture server to exit. The
 * helper's broker shutdown takes up to 10s and writes under the state dir.
 */
const EXIT_WAIT_MS = 15_000;

/** Waits until none of pids runs, or the bound passes; then SIGKILLs the groups left. */
async function waitExited(pids: number[]): Promise<void> {
  const deadline = Date.now() + EXIT_WAIT_MS;
  let left = pids.filter((pid) => pid && alive(pid));
  while (left.length > 0 && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 200));
    left = left.filter(alive);
  }
  for (const pid of left) {
    console.warn(`[teardown] PID ${pid} did not exit within ${EXIT_WAIT_MS}ms; killing its group`);
    try {
      process.kill(-pid, 'SIGKILL');
    } catch {
      // Already gone.
    }
  }
}

async function globalTeardown(): Promise<void> {
  const suite = readSuiteState();
  if (suite) {
    stopGroup(suite.helperPid, 'the fake agent helper');
    stopGroup(suite.fixturePid, 'the fixture server');
    // Before the state dir goes: the helper writes under it while it stops.
    await waitExited([suite.helperPid, suite.fixturePid]);
  }
  stopHub(hubStateFile());
  removeStateDir();
}

export default globalTeardown;
