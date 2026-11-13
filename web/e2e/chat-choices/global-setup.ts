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
 * Global setup for the real-hub chat choices E2E suite (ptone/scion#7776).
 *
 * Starts a throwaway test hub with:
 *   - co-located runtime broker (`runtimeBroker: true, testIdentities: true`)
 *     so containerless agents can be provisioned without Docker,
 *   - `testLogin: true` so multi-user browser sessions and deterministic
 *     `X-Scion-Agent-Token` signing keys are available, and
 *   - `experiments: ['web.chat_choices']` enabled via the admin API.
 */

import { startHub } from '../harness/hub.js';
import { createStateFile } from './state.js';

async function globalSetup(): Promise<void> {
  const state = await startHub({
    testLogin: true,
    testIdentities: true,
    runtimeBroker: true,
    build: process.env.E2E_PREBUILT !== '1',
    stateFile: createStateFile(),
    env: {
      SCION_IMAGE_REGISTRY: 'registry.invalid/scion-e2e',
    },
    experiments: ['web.chat_choices', 'web.project_decisions'],
  });
  console.info(`[setup] Chat-choices real hub ${String(state.hubId)} at ${state.baseURL}`);
}

export default globalSetup;
