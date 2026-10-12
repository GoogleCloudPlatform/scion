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

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

/**
 * Environment variable naming this suite's hub state file. Global setup
 * creates the file in a private temp dir and sets it; Playwright passes the
 * environment of global setup on to the workers and the teardown.
 */
const STATE_ENV = 'E2E_TI_HUB_STATE_FILE';

/** Creates the private state dir and publishes the state file path. */
export function createStateFile(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-ti-state-'));
  fs.chmodSync(dir, 0o700);
  const file = path.join(dir, 'hub-state.json');
  process.env[STATE_ENV] = file;
  return file;
}

/** The state file path published by global setup. */
export function stateFile(): string {
  const file = process.env[STATE_ENV];
  if (!file) {
    throw new Error(`${STATE_ENV} is not set; did global-setup run?`);
  }
  return file;
}

/** Removes the private state dir. */
export function removeStateDir(): void {
  const file = process.env[STATE_ENV];
  if (file) fs.rmSync(path.dirname(file), { recursive: true, force: true });
}
