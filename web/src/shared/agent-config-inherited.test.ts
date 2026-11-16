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
  inheritedAgentRole,
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
    // The template's agentConfig model beats the hub default (ptone/scion#4222).
    expect(p['config.model'].source).toBe(FROM_TEMPLATE);
    expect(p.messageMode.source).toBe(FROM_TEMPLATE);
    // Template telemetry, env and image are environment keys: no placeholder.
    expect(p['config.telemetry']).toBeUndefined();
    expect(p.autoExpose).toBeUndefined();
    expect(p['config.image']).toBeUndefined();
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
      template: { agentConfig: {} },
      projectSettings: {},
      hubSettings: {},
    });
    expect(p).toEqual({});
  });

  it('reads template job keys from agentConfig only, never the row config', () => {
    const p = resolveInheritedPlaceholders({
      template: {
        config: { model: 'row-model', messageMode: 'branch', image: 'row:1' },
      } as unknown as InheritedSources['template'],
    });
    expect(p).toEqual({});
  });

  it('ranks the template model between the project and the hub', () => {
    const template = { agentConfig: { model: 'template-model' } };
    expect(
      resolveInheritedPlaceholders({ template, hubSettings: { defaultModel: 'hub-model' } })[
        'config.model'
      ]
    ).toEqual({ value: 'template-model', source: FROM_TEMPLATE });
    expect(
      resolveInheritedPlaceholders({
        template,
        projectSettings: { defaultModel: 'project-model' },
        hubSettings: { defaultModel: 'hub-model' },
      })['config.model']
    ).toEqual({ value: 'project-model', source: FROM_PROJECT });
  });

  it('maps project resources per sub-field', () => {
    const p = resolveInheritedPlaceholders({
      projectSettings: { defaultResources: { limits: { memory: '8Gi' } } },
    });
    expect(p['config.resources.limits.memory']).toEqual({ value: '8Gi', source: FROM_PROJECT });
    expect(p['config.resources.requests.cpu']).toBeUndefined();
  });
});

describe('inheritedAgentRole caps the project default at the project maximum', () => {
  it.each([
    ['full', 'baseline', 'baseline'],
    ['readonly', 'baseline', 'readonly'],
    ['baseline', undefined, 'baseline'],
    ['full', 'full', 'full'],
    [undefined, 'baseline', undefined],
    [undefined, 'none', 'none'],
    ['bogus', undefined, undefined],
  ])('default %s, max %s -> %s', (def, max, want) => {
    expect(inheritedAgentRole(def, max)).toBe(want);
  });

  it('shows the capped role as the placeholder', () => {
    const p = resolveInheritedPlaceholders({
      projectSettings: { defaultAgentRole: 'full', maxAgentRole: 'readonly' },
    });
    expect(p.agentRole).toEqual({ value: 'readonly', source: FROM_PROJECT });
  });
});

// A project count limit of 0 is Unlimited, not unset (ptone/scion#3925):
// the placeholder reads "Unlimited" with the project source, a limit keeps
// its number, and null or absent gives no project placeholder.
describe('project count limits', () => {
  it('shows a project 0 as Unlimited, from project settings', () => {
    const p = resolveInheritedPlaceholders({
      projectSettings: { defaultMaxTurns: 0, defaultMaxModelCalls: 0 },
    });
    expect(p['config.max_turns']).toEqual({ value: 'Unlimited', source: FROM_PROJECT });
    expect(p['config.max_model_calls']).toEqual({ value: 'Unlimited', source: FROM_PROJECT });
  });

  it('keeps a project limit as its number', () => {
    const p = resolveInheritedPlaceholders({ projectSettings: { defaultMaxTurns: 40 } });
    expect(p['config.max_turns']).toEqual({ value: '40', source: FROM_PROJECT });
  });

  it('gives no project placeholder for null or absent', () => {
    const p = resolveInheritedPlaceholders({
      projectSettings: { defaultMaxTurns: null },
    });
    expect(p['config.max_turns']).toBeUndefined();
    expect(p['config.max_model_calls']).toBeUndefined();
  });
});

describe('resolveInheritedGCPIdentity ladder (ptone/scion#8185)', () => {
  const accounts = [
    { id: 'sa-broker', email: 'broker-sa@example.iam.gserviceaccount.com', displayName: '' },
    { id: 'sa-profile', email: 'profile-sa@example.iam.gserviceaccount.com', displayName: '' },
    {
      id: 'sa-project',
      email: 'project-sa@example.iam.gserviceaccount.com',
      displayName: 'Proj SA',
    },
    { id: 'sa-hub', email: 'hub-sa@example.iam.gserviceaccount.com', displayName: '' },
  ];

  it('resolves per-broker default for a flat broker when hub.flat_runtime_brokers is on, beating project block/assign and hub defaults', async () => {
    const { resolveInheritedGCPIdentity } = await import('./agent-config-inherited.js');
    for (const projectMode of ['block', 'passthrough', 'assign']) {
      const Sources = {
        flatBrokersEnabled: true,
        broker: { id: 'flat-cr-1', name: 'Cloud Run US', flat: true },
        gcpServiceAccounts: accounts,
        projectSettings: {
          defaultGCPIdentityMode: projectMode,
          defaultGCPIdentityServiceAccountID: 'sa-project',
          defaultGCPIdentityServiceAccountIDByBroker: { 'flat-cr-1': 'sa-broker' },
          defaultGCPIdentityServiceAccountIDByProfile: { default: 'sa-profile' },
        },
        hubGCPIdentity: { mode: 'assign', serviceAccountId: 'sa-hub' },
      };
      const resolved = resolveInheritedGCPIdentity(Sources);
      expect(resolved).toEqual({
        rung: 'broker',
        mode: 'assign',
        serviceAccountId: 'sa-broker',
        value: 'Assign broker-sa@example.iam.gserviceaccount.com',
        source: 'inherited from project default for Runtime Broker "Cloud Run US"',
      });
      const placeholders = resolveInheritedPlaceholders(Sources);
      expect(placeholders.gcp_identity).toEqual({
        value: 'Assign broker-sa@example.iam.gserviceaccount.com',
        source: 'inherited from project default for Runtime Broker "Cloud Run US"',
      });
    }
  });

  it('ignores per-broker default when hub.flat_runtime_brokers is off', async () => {
    const { resolveInheritedGCPIdentity } = await import('./agent-config-inherited.js');
    const sources = {
      flatBrokersEnabled: false,
      broker: { id: 'flat-cr-1', name: 'Cloud Run US', flat: true },
      gcpServiceAccounts: accounts,
      projectSettings: {
        defaultGCPIdentityMode: 'block',
        defaultGCPIdentityServiceAccountIDByBroker: { 'flat-cr-1': 'sa-broker' },
      },
    };
    expect(resolveInheritedGCPIdentity(sources)).toEqual({
      rung: 'project',
      mode: 'block',
      value: 'Block',
      source: FROM_PROJECT,
    });
    expect(resolveInheritedPlaceholders(sources).gcp_identity).toBeUndefined();
  });

  it('ignores per-broker default on a legacy profile-based broker and uses per-profile default instead', async () => {
    const { resolveInheritedGCPIdentity } = await import('./agent-config-inherited.js');
    const sources = {
      flatBrokersEnabled: true,
      broker: { id: 'legacy-1', name: 'Legacy Broker', flat: false },
      profile: 'k8s-prod',
      gcpServiceAccounts: accounts,
      projectSettings: {
        defaultGCPIdentityMode: 'block',
        defaultGCPIdentityServiceAccountIDByBroker: { 'legacy-1': 'sa-broker' },
        defaultGCPIdentityServiceAccountIDByProfile: { 'k8s-prod': 'sa-profile' },
      },
    };
    expect(resolveInheritedGCPIdentity(sources)).toEqual({
      rung: 'profile',
      mode: 'assign',
      serviceAccountId: 'sa-profile',
      value: 'Assign profile-sa@example.iam.gserviceaccount.com',
      source: 'inherited from project default for profile "k8s-prod"',
    });
  });

  it('falls through to project default then hub default when flat broker has no per-broker entry', async () => {
    const { resolveInheritedGCPIdentity } = await import('./agent-config-inherited.js');
    expect(
      resolveInheritedGCPIdentity({
        flatBrokersEnabled: true,
        broker: { id: 'flat-other', name: 'Other Flat', flat: true },
        gcpServiceAccounts: accounts,
        projectSettings: {
          defaultGCPIdentityMode: 'assign',
          defaultGCPIdentityServiceAccountID: 'sa-project',
          defaultGCPIdentityServiceAccountIDByBroker: { 'flat-cr-1': 'sa-broker' },
        },
        hubGCPIdentity: { mode: 'block' },
      })
    ).toEqual({
      rung: 'project',
      mode: 'assign',
      serviceAccountId: 'sa-project',
      value: 'Assign Proj SA (project-sa@example.iam.gserviceaccount.com)',
      source: FROM_PROJECT,
    });

    expect(
      resolveInheritedGCPIdentity({
        flatBrokersEnabled: true,
        broker: { id: 'flat-other', name: 'Other Flat', flat: true },
        gcpServiceAccounts: accounts,
        projectSettings: {
          defaultGCPIdentityServiceAccountIDByBroker: { 'flat-cr-1': 'sa-broker' },
        },
        hubGCPIdentity: { mode: 'assign', serviceAccountId: 'sa-hub' },
      })
    ).toEqual({
      rung: 'hub',
      mode: 'assign',
      serviceAccountId: 'sa-hub',
      value: 'Assign hub-sa@example.iam.gserviceaccount.com',
      source: FROM_HUB,
    });
  });
});
