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
 * Global setup of the page policy suite: starts a throwaway hub (the
 * default one of the e2e harness) and records its state in a private state
 * file for the spec and the teardown (see state.ts). With E2E_PREBUILT=1
 * it runs the binary and web assets already built, as the test identity
 * session suite does.
 */

import { startHub } from '../harness/hub.js';
import { createStateFile } from './state.js';

async function globalSetup(): Promise<void> {
  const state = await startHub({
    build: process.env.E2E_PREBUILT !== '1',
    stateFile: createStateFile(),
  });
  console.info(`[setup] Hub at ${state.baseURL}`);
}

export default globalSetup;
