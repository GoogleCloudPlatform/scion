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
 * resolveInheritedPlaceholders (ptone/scion#3974): the create form's
 * inherited placeholders equal what the hub dispatches, one case per tier.
 *
 * The cases come from pkg/hub/testdata/agent-create-inherited-golden.json,
 * whose dispatched values TestCreateAgent_InheritedValuesGolden checks
 * against the hub's create path. So a placeholder here equals the value the
 * hub dispatches for an agent created with that field left unset, for the
 * sources the web reads.
 */

import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

import {
  FROM_HUB,
  FROM_PROJECT,
  FROM_TEMPLATE,
  PROJECT_OVERRIDE,
  resolveInheritedPlaceholders,
  type InheritedSources,
} from './agent-config-inherited.js';

interface GoldenCase {
  tier: 'template' | 'project' | 'hub';
  sources: InheritedSources;
  dispatched: Record<string, string>;
}

const GOLDEN_PATH = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  '../../../pkg/hub/testdata/agent-create-inherited-golden.json'
);
const golden = JSON.parse(readFileSync(GOLDEN_PATH, 'utf-8')) as { cases: GoldenCase[] };

/** Fields whose inherited value the broker resolves, outside the hub golden. */
const BROKER_RESOLVED = new Set(['config.image']);

function goldenCase(tier: GoldenCase['tier']): GoldenCase {
  const c = golden.cases.find((x) => x.tier === tier);
  if (!c) throw new Error(`golden has no ${tier} case`);
  return c;
}

/** The placeholder equals the dispatched value, for every key the hub dispatched. */
function expectMatchesDispatch(c: GoldenCase): void {
  const placeholders = resolveInheritedPlaceholders(c.sources);
  for (const [key, value] of Object.entries(c.dispatched)) {
    expect(placeholders[key]?.value, `${c.tier}: ${key}`).toBe(value);
  }
  // And no placeholder shows a value the hub did not dispatch.
  for (const [key, p] of Object.entries(placeholders)) {
    if (p.value === undefined || BROKER_RESOLVED.has(key)) continue;
    expect(c.dispatched, `${c.tier}: unexpected placeholder value for ${key}`).toHaveProperty(key);
  }
}

describe('placeholder equals the dispatched value', () => {
  it('template tier', () => {
    const c = goldenCase('template');
    expectMatchesDispatch(c);
    const p = resolveInheritedPlaceholders(c.sources);
    expect(p['config.model'].source).toBe(FROM_TEMPLATE);
    expect(p['config.telemetry'].source).toBe(FROM_TEMPLATE);
    expect(p.messageMode.source).toBe(FROM_TEMPLATE);
  });

  it('project tier', () => {
    const c = goldenCase('project');
    expectMatchesDispatch(c);
    const p = resolveInheritedPlaceholders(c.sources);
    expect(p['config.model'].source).toBe(FROM_PROJECT);
    expect(p['config.max_turns'].source).toBe(FROM_PROJECT);
    expect(p.autoExpose.source).toBe(FROM_PROJECT);
  });

  it('hub tier', () => {
    const c = goldenCase('hub');
    expectMatchesDispatch(c);
    const p = resolveInheritedPlaceholders(c.sources);
    expect(p['config.model'].source).toBe(FROM_HUB);
    expect(p.autoExpose.source).toContain(FROM_HUB);
  });
});

describe('resolveInheritedPlaceholders labels and gaps', () => {
  it('labels the project telemetry setting as an override', () => {
    const p = resolveInheritedPlaceholders({ projectSettings: { telemetryEnabled: true } });
    expect(p['config.telemetry']).toEqual({
      value: 'true',
      source: PROJECT_OVERRIDE,
      override: true,
    });
  });

  it('does not show the hub-wide public telemetry setting, which create does not apply', () => {
    const p = resolveInheritedPlaceholders({ hubSettings: { telemetryEnabled: true } });
    expect(p['config.telemetry']).toBeUndefined();
  });

  it('shows no value for fields the fetched sources do not determine', () => {
    const p = resolveInheritedPlaceholders({
      template: { config: { harness: 'claude' } },
      projectSettings: {},
      hubSettings: {},
    });
    expect(p).toEqual({});
  });

  it('takes the template image, top-level or in config', () => {
    expect(resolveInheritedPlaceholders({ template: { image: 'a:1' } })['config.image']).toEqual({
      value: 'a:1',
      source: FROM_TEMPLATE,
    });
    expect(
      resolveInheritedPlaceholders({ template: { image: 'a:1', config: { image: 'b:2' } } })[
        'config.image'
      ]?.value
    ).toBe('b:2');
  });

  it('maps project resources per sub-field', () => {
    const p = resolveInheritedPlaceholders({
      projectSettings: { defaultResources: { limits: { memory: '8Gi' } } },
    });
    expect(p['config.resources.limits.memory']).toEqual({ value: '8Gi', source: FROM_PROJECT });
    expect(p['config.resources.requests.cpu']).toBeUndefined();
  });
});
