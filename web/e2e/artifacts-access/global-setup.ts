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
 * Global setup of the artifact access journey (R1-A).
 *
 * Remote target (E2E_TARGET_URL): nothing to start.
 *
 * Local target: starts a throwaway member-level test hub (test identities
 * on, no test-login). R1-A needs no runtime broker, no container runtime
 * and no credentials. hub.artifacts is left at its default (off): the
 * spec checks the experiment-off rows first and then enables it through
 * the admin experiments API.
 */

import { startHub } from '../harness/hub.js';
import { createStateFile, isRemoteTarget } from './state.js';

async function globalSetup(): Promise<void> {
  if (isRemoteTarget()) {
    console.info('[setup] Remote target: no local hub.');
    return;
  }
  const state = await startHub({
    testLogin: false,
    testIdentities: true,
    build: process.env.E2E_PREBUILT !== '1',
    stateFile: createStateFile(),
  });
  console.info(`[setup] Artifact access hub ${String(state.hubId)} at ${state.baseURL}`);
}

export default globalSetup;
