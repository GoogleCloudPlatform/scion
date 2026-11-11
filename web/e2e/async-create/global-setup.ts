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
 * co-located runtime broker and the hub.async_agent_launch experiment on
 * (set through the admin experiments API and read back), and
 * records whether agents can actually start here:
 * - E2E_LAUNCH_CAPABLE=1 or 0 decides it explicitly;
 * - otherwise a working `docker info` and E2E_IMAGE_REGISTRY (the registry
 *   the broker pulls agent images from) are both needed.
 * Without a usable runtime the broker still registers; launches then fail
 * at the broker, and the spec skips the checks that need a real launch.
 *
 * The journey runs without credentials: agents reach running through the
 * shipped harnesses' no-auth fallback (drop-to-shell). The credential guard
 * refuses to start while a harness credential variable is set. The
 * harness (E2E_HARNESS_CONFIG, default the form's default) decides the
 * agent image, <registry>/scion-<harness>:latest; the bad image ref is
 * a tag that does not exist in that same repo (E2E_BAD_IMAGE_REPO or
 * E2E_BAD_IMAGE override it). Both refs are printed.
 */

import { execFileSync } from 'node:child_process';
import * as crypto from 'node:crypto';

import { startHub } from '../harness/hub.js';
import { assertNoHarnessCredentialEnv } from './credential-guard.js';
import {
  FORM_DEFAULT_HARNESS,
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
  assertNoHarnessCredentialEnv();
  const registry = process.env.E2E_IMAGE_REGISTRY || undefined;
  const { capable, reason } = launchCapability(registry);
  const suiteStateFile = createStateFile();
  const hubStateFile = hubStateFilePath(suiteStateFile);
  const imageRegistry = registry ?? PLACEHOLDER_REGISTRY;
  const harnessConfig = process.env.E2E_HARNESS_CONFIG || FORM_DEFAULT_HARNESS;
  // The image A2 expects: the declared ref (E2E_AGENT_IMAGE), else the
  // harness default on this registry.
  const agentImage =
    process.env.E2E_AGENT_IMAGE || `${imageRegistry}/scion-${harnessConfig}:latest`;
  const badRepo = process.env.E2E_BAD_IMAGE_REPO || `scion-${harnessConfig}`;
  const badImage =
    process.env.E2E_BAD_IMAGE ||
    `${imageRegistry}/${badRepo}:e2e-no-such-tag-${crypto.randomBytes(3).toString('hex')}`;
  console.info(`[setup] Harness config: ${harnessConfig} (no-auth drop-to-shell)`);
  console.info(`[setup] Agent image (expected): ${agentImage}`);
  console.info(`[setup] Bad image fixture: ${badImage}`);

  const state = await startHub({
    testLogin: false,
    testIdentities: true,
    runtimeBroker: true,
    build: process.env.E2E_PREBUILT !== '1',
    stateFile: hubStateFile,
    env: {
      SCION_IMAGE_REGISTRY: imageRegistry,
    },
    // Set explicitly, whatever the registry default, and read back by
    // startHub, so the journey always runs with async create on.
    experiments: ['hub.async_agent_launch'],
  });

  const suiteState: LocalSuiteState = {
    hubStateFile,
    launchCapable: capable,
    launchCapableReason: reason,
    imageRegistry,
    harnessConfig,
    agentImage,
    badImage,
  };
  writeSuiteState(suiteStateFile, suiteState);
  console.info(
    `[setup] Async create hub ${String(state.hubId)} at ${state.baseURL}; ` +
      `launch capable: ${String(capable)} (${reason})`
  );
}

export default globalSetup;
