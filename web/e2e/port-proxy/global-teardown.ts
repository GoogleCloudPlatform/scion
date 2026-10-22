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

/** Global teardown of the port proxy journeys: stops the helper, the fixture app and the hub. */

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

function globalTeardown(): void {
  const suite = readSuiteState();
  if (suite) {
    stopGroup(suite.helperPid, 'the fake agent helper');
    stopGroup(suite.fixturePid, 'the fixture server');
  }
  stopHub(hubStateFile());
  removeStateDir();
}

export default globalTeardown;
