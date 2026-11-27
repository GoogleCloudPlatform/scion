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
 * Global setup for the real-hub Inbox Dashboard E2E suite (ptone/scion#8962).
 *
 * Starts a throwaway test hub with:
 *   - co-located runtime broker (`runtimeBroker: true, testIdentities: true`)
 *     so containerless test agents can be provisioned without Docker,
 *   - `testLogin: true` for user browser sessions and deterministic agent JWTs,
 *   - `experiments: ['web.inbox_dashboard']` enabled via the admin API.
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
    experiments: [
      'web.inbox_dashboard',
      'hub.artifacts',
      'hub.artifacts_review_requests',
      'hub.artifacts_review_drafts',
    ],
  });
  console.info(`[setup] Inbox real hub ${String(state.hubId)} at ${state.baseURL}`);
}

export default globalSetup;
