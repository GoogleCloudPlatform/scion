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
 * Global setup of the async create journey.
 *
 * Remote target (E2E_TARGET_URL): nothing to start.
 *
 * Local target: starts a throwaway member-level test hub with the
 * co-located runtime broker and server.hub.async_agent_launch on, and
 * records whether agents can actually start here:
 * - E2E_LAUNCH_CAPABLE=1 or 0 decides it explicitly;
 * - otherwise a working `docker info` and E2E_IMAGE_REGISTRY (the registry
 *   the broker pulls agent images from) are both needed.
 * Without a usable runtime the broker still registers; launches then fail
 * at the broker, and the spec skips the checks that need a real launch.
 */

import { execFileSync } from 'node:child_process';

import { startHub } from '../harness/hub.js';
import {
  createStateFile,
  hubStateFilePath,
  isRemoteTarget,
  writeSuiteState,
  type LocalSuiteState,
} from './state.js';

/** Placeholder registry when none is given; nothing is pulled from it. */
const PLACEHOLDER_REGISTRY = 'registry.invalid/scion-e2e';

function dockerWorks(): boolean {
  try {
    execFileSync('docker', ['info'], { stdio: 'ignore', timeout: 20_000 });
    return true;
  } catch {
    return false;
  }
}

function launchCapability(registry: string | undefined): { capable: boolean; reason: string } {
  const explicit = process.env.E2E_LAUNCH_CAPABLE;
  if (explicit === '1' || explicit === '0') {
    return { capable: explicit === '1', reason: `E2E_LAUNCH_CAPABLE=${explicit}` };
  }
  if (!dockerWorks()) {
    return { capable: false, reason: 'no working container runtime (docker info failed)' };
  }
  if (!registry) {
    return { capable: false, reason: 'E2E_IMAGE_REGISTRY is not set' };
  }
  return { capable: true, reason: 'docker info succeeded and E2E_IMAGE_REGISTRY is set' };
}

async function globalSetup(): Promise<void> {
  if (isRemoteTarget()) {
    console.info('[setup] Remote target: no local hub.');
    return;
  }
  const registry = process.env.E2E_IMAGE_REGISTRY || undefined;
  const { capable, reason } = launchCapability(registry);
  const suiteStateFile = createStateFile();
  const hubStateFile = hubStateFilePath(suiteStateFile);
  const imageRegistry = registry ?? PLACEHOLDER_REGISTRY;

  const state = await startHub({
    testLogin: false,
    testIdentities: true,
    runtimeBroker: true,
    build: process.env.E2E_PREBUILT !== '1',
    stateFile: hubStateFile,
    env: {
      SCION_SERVER_HUB_ASYNCAGENTLAUNCH: 'true',
      SCION_IMAGE_REGISTRY: imageRegistry,
    },
  });

  const suiteState: LocalSuiteState = {
    hubStateFile,
    launchCapable: capable,
    launchCapableReason: reason,
    imageRegistry,
  };
  writeSuiteState(suiteStateFile, suiteState);
  console.info(
    `[setup] Async create hub ${String(state.hubId)} at ${state.baseURL}; ` +
      `launch capable: ${String(capable)} (${reason})`
  );
}

export default globalSetup;
