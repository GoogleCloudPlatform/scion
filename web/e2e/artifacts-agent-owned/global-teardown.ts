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
 * Global teardown of the agent-owned artifact journey: stops the local hub
 * (its temp dir, database and private HOME go with it, which also ends the
 * test identities and the experiment override) and removes the state dir.
 */

import * as fs from 'node:fs';

import { stopHub } from '../harness/hub.js';
import { hubStateFilePath, removeStateDir } from './state.js';

function globalTeardown(): void {
  const file = process.env.E2E_AO_STATE_FILE;
  if (file) {
    const hubStateFile = hubStateFilePath(file);
    if (fs.existsSync(hubStateFile)) stopHub(hubStateFile);
  }
  removeStateDir();
}

export default globalTeardown;
