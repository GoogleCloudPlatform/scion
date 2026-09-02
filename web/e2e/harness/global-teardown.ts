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
 * Playwright global teardown.
 *
 * Stops the hub process and cleans up temporary files.
 */

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import { stopHub } from './hub.js';

const E2E_ENV_FILE = path.join(os.tmpdir(), 'scion-e2e-env.json');
const AUTH_DIR = path.join(os.tmpdir(), 'scion-e2e-auth');

async function globalTeardown(): Promise<void> {
  console.log('\n=== E2E Global Teardown ===\n');

  // Stop the hub
  stopHub();

  // Clean up auth state files
  try {
    fs.rmSync(AUTH_DIR, { recursive: true, force: true });
  } catch {
    // Best-effort cleanup
  }

  // Clean up env file
  try {
    fs.unlinkSync(E2E_ENV_FILE);
  } catch {
    // Already cleaned up or never created
  }

  console.log('\n=== E2E Global Teardown Complete ===\n');
}

export default globalTeardown;
