/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * The launch step and code labels (design wave 3 §3 E-e, ptone/scion#4670).
 * The vocabulary is read from the Go sources, so a step or code added there
 * without a label here fails this test.
 */

import { readFileSync } from 'fs';
import { resolve } from 'path';
import { describe, it, expect } from 'vitest';

import {
  LAUNCH_CODE_LABELS,
  LAUNCH_STEP_LABELS,
  launchCodeLabel,
  launchFailedTitle,
  launchKindTitle,
  launchStepLabel,
} from './agent-launch-labels.js';

const REPO_ROOT = resolve(__dirname, '../../..');

function goSource(path: string): string {
  return readFileSync(resolve(REPO_ROOT, path), 'utf-8');
}

/**
 * The string values of the Go constants whose names start with prefix, in a
 * const block or a one-line const, typed or not. A constant of that prefix
 * defined as an alias of another identifier fails the test unless it is in
 * aliases (its value is checked separately).
 */
function goStringConstants(src: string, prefix: string, aliases: string[] = []): string[] {
  const lit = new RegExp(`^\\s*(?:const\\s+)?(${prefix}\\w*)(?:\\s+\\w+)?\\s*=\\s*"([^"]+)"`, 'gm');
  const alias = new RegExp(
    `^\\s*(?:const\\s+)?(${prefix}\\w*)(?:\\s+\\w+)?\\s*=\\s*[A-Za-z]`,
    'gm'
  );
  const unknownAliases = Array.from(src.matchAll(alias), (m) => m[1]).filter(
    (name) => !aliases.includes(name)
  );
  expect(unknownAliases, `${prefix} constants defined as aliases`).toEqual([]);
  return Array.from(src.matchAll(lit), (m) => m[2]);
}

describe('launch label vocabulary', () => {
  const launchCodes = goSource('pkg/api/launch_codes.go');

  it('labels every launch step in pkg/api', () => {
    const steps = goStringConstants(launchCodes, 'LaunchStep');
    // Guards the parser: launch_codes.go has 18 steps today.
    expect(steps.length).toBeGreaterThanOrEqual(18);
    expect(steps.filter((s) => !Object.hasOwn(LAUNCH_STEP_LABELS, s))).toEqual([]);
  });

  it('labels every launch code in pkg/api and the broker codes that reach launch.error', () => {
    // LaunchCodeHarnessConfigUnusable aliases the broker code checked below.
    const codes = goStringConstants(launchCodes, 'LaunchCode', ['LaunchCodeHarnessConfigUnusable']);
    expect(codes.length).toBeGreaterThanOrEqual(19);
    const brokerCodes = goStringConstants(
      goSource('pkg/api/broker_error_codes.go'),
      'BrokerErrCode(?:SkillResolution|HarnessConfigUnusable|StorageMigrationRequired)'
    );
    expect([...brokerCodes].sort()).toEqual([
      'harness_config_unusable',
      'skill_resolution_failed',
      'storage_migration_required',
    ]);
    expect([...codes, ...brokerCodes].filter((c) => !Object.hasOwn(LAUNCH_CODE_LABELS, c))).toEqual(
      []
    );
  });

  it("uses the broker's report text for each typed runtime failure", () => {
    const src = goSource('pkg/runtimebroker/run_launch.go');
    const block = src.slice(src.indexOf('var launchFailureText'));
    const entries = Array.from(
      block.slice(0, block.indexOf('\n}')).matchAll(/api\.LaunchCode(\w+):\s*"([^"]+)"/g),
      (m) => [m[1], m[2]] as const
    );
    expect(entries.length).toBeGreaterThanOrEqual(7);
    const codeOf = (name: string): string =>
      launchCodes.match(new RegExp(`^\\s*LaunchCode${name}\\s*=\\s*"([^"]+)"`, 'm'))![1];
    for (const [name, text] of entries) {
      // The broker's launch_timeout text is about the pod only.
      if (name === 'LaunchTimeout') continue;
      expect(launchCodeLabel(codeOf(name)), name).toBe(text);
    }
  });
});

describe('launch labels', () => {
  it('labels the steps and codes the design names', () => {
    expect(launchStepLabel('container_create')).toBe(
      'preparing the container (image pull, volume mounts)'
    );
    expect(launchStepLabel('awaiting_capacity')).toBe('waiting for node capacity');
    expect(launchCodeLabel('image_pull_failed')).toBe('the agent image could not be pulled');
    expect(launchCodeLabel('launch_timeout')).toBe('the launch timed out');
  });

  it('shows a step or code without a label with "_" as a space', () => {
    expect(launchStepLabel('warming_up_node')).toBe('warming up node');
    expect(launchCodeLabel('quota_exceeded')).toBe('quota exceeded');
    // Object prototype keys are not labels.
    expect(launchStepLabel('constructor')).toBe('constructor');
  });

  // The hub always sends these, but a missing value must not throw.
  it('shows nothing for a missing step or code', () => {
    expect(launchStepLabel(undefined)).toBe('');
    expect(launchStepLabel(null)).toBe('');
    expect(launchCodeLabel(undefined)).toBe('');
    expect(launchCodeLabel(null)).toBe('');
  });

  it('titles a launch without a kind as a create', () => {
    expect(launchKindTitle(undefined)).toBe('Launching');
    expect(launchFailedTitle(null)).toBe('Agent create did not complete');
  });

  it.each([
    ['create', 'Launching', 'Agent create did not complete'],
    ['start', 'Starting', 'Agent start did not complete'],
    ['restart', 'Restarting', 'Agent restart did not complete'],
    ['', 'Launching', 'Agent create did not complete'],
  ])('titles a %s launch', (kind, inFlight, failed) => {
    expect(launchKindTitle(kind)).toBe(inFlight);
    expect(launchFailedTitle(kind)).toBe(failed);
  });
});
