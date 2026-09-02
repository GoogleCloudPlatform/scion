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
 * Helpers for loading E2E environment state inside test specs.
 *
 * The global-setup writes an env JSON file to /tmp; specs import this
 * module to read it.
 */

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import type { E2EEnv } from './global-setup.js';

const E2E_ENV_FILE = path.join(os.tmpdir(), 'scion-e2e-env.json');

let cachedEnv: E2EEnv | null = null;

/**
 * Load the E2E environment written by global-setup.
 * Throws if global-setup has not run.
 */
export function getE2EEnv(): E2EEnv {
  if (cachedEnv) return cachedEnv;

  if (!fs.existsSync(E2E_ENV_FILE)) {
    throw new Error(
      `E2E env file not found at ${E2E_ENV_FILE}. ` +
        'Did global-setup run? Use "npm run test:e2e" to run the full suite.',
    );
  }

  cachedEnv = JSON.parse(fs.readFileSync(E2E_ENV_FILE, 'utf-8')) as E2EEnv;
  return cachedEnv;
}
