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
 * V4: the Configure tab of the Brokers area (broker UI Option A, slice A1),
 * mounted on its own: the Identity PATCH payload for labels and the GCP
 * fields, the refusal text, the capacity PUT (custom value, inherit, 409),
 * the effective-limit source, and the instance-file name line.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

import type { BrokerSettingsResponse, RuntimeBroker } from '../../shared/types.js';
import { FLAT_K8S, LEGACY, jsonResponse, settle } from './__fixtures__/broker-area.js';

type Cfg = HTMLElement & {
  broker: RuntimeBroker | null;
  settings: BrokerSettingsResponse | null;
  updateComplete: Promise<boolean>;
  name: string;
  labelsText: string;
  saEmail: string;
  saProject: string;
  capacityMode: 'default' | 'custom';
  capacityValue: string;
};

function settings(over: Partial<BrokerSettingsResponse> = {}): BrokerSettingsResponse {
  return {
    brokerId: 'b',
    settings: {},
    effective: {
      maxAgents: {
        value: 30,
        source: 'not_enforced',
        count: 2,
        inherited: { value: 30, source: 'hub_default' },
      },
    },
    revision: 3,
    _capabilities: { update: true },
    ...over,
  } as BrokerSettingsResponse;
}

let fetchMock: ReturnType<typeof vi.fn>;
let patchStatus = 200;
let putStatus = 200;
const mounted: HTMLElement[] = [];

async function mount(broker: RuntimeBroker, s: BrokerSettingsResponse | null = settings()) {
  const el = document.createElement('scion-broker-area-configure') as Cfg;
  el.broker = broker;
  el.settings = s;
  document.body.appendChild(el);
  mounted.push(el);
  await el.updateComplete;
  return el;
}

function bodyOf(method: string): unknown {
  const call = fetchMock.mock.calls.find(([, init]) => (init as RequestInit)?.method === method);
  expect(call, method).toBeDefined();
  return JSON.parse((call![1] as RequestInit).body as string);
}

function click(el: Cfg, sel: string): void {
  (el.shadowRoot!.querySelector(sel) as HTMLElement).click();
}

describe('scion-broker-area-configure', () => {
  beforeAll(async () => {
    await import('./broker-area-configure.js');
  }, 60_000);

  beforeEach(() => {
    patchStatus = 200;
    putStatus = 200;
    fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method === 'PATCH') {
        if (patchStatus !== 200) {
          return jsonResponse(
            { error: { code: 'conflict', message: 'the name "dup" is already used' } },
            patchStatus
          );
        }
        return jsonResponse(LEGACY);
      }
      if (init?.method === 'PUT') {
        if (putStatus !== 200) return jsonResponse({ error: { message: 'stale' } }, putStatus);
        return jsonResponse(settings({ revision: 4 }));
      }
      return jsonResponse(LEGACY);
    });
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    while (mounted.length) mounted.pop()!.remove();
    vi.unstubAllGlobals();
  });

  it('sends labels and the GCP fields that changed, and nothing else', async () => {
    const el = await mount({ ...LEGACY, _capabilities: { actions: ['read', 'update'] } });
    el.labelsText = 'team=a\nzone = b';
    el.saEmail = 'sa@p.iam.gserviceaccount.com';
    el.saProject = 'proj-1';
    await el.updateComplete;
    click(el, 'sl-button.save-identity');
    await settle(el);
    expect(bodyOf('PATCH')).toEqual({
      labels: { team: 'a', zone: 'b' },
      gcpHostServiceAccountEmail: 'sa@p.iam.gserviceaccount.com',
      gcpHostProjectId: 'proj-1',
    });
  });

  it('shows the hub refusal text', async () => {
    patchStatus = 409;
    const el = await mount({ ...LEGACY, _capabilities: { actions: ['read', 'update'] } });
    el.name = 'dup';
    await el.updateComplete;
    click(el, 'sl-button.save-identity');
    await settle(el);
    expect(el.shadowRoot!.querySelector('.form-error')?.textContent).toContain(
      'the name "dup" is already used'
    );
  });

  it('capacity: a custom value, then the inherited limit (null), with the revision', async () => {
    const el = await mount({ ...LEGACY, _capabilities: { actions: ['read', 'update'] } });
    el.capacityMode = 'custom';
    el.capacityValue = '7';
    await el.updateComplete;
    click(el, 'sl-button.save-capacity');
    await settle(el);
    expect(bodyOf('PUT')).toEqual({ settings: { maxAgents: 7 }, expectedRevision: 3 });

    fetchMock.mockClear();
    el.capacityMode = 'default';
    await el.updateComplete;
    click(el, 'sl-button.save-capacity');
    await settle(el);
    expect(bodyOf('PUT')).toEqual({ settings: { maxAgents: null }, expectedRevision: 3 });
  });

  it('capacity: a 409 says the settings changed', async () => {
    putStatus = 409;
    const el = await mount({ ...LEGACY, _capabilities: { actions: ['read', 'update'] } });
    el.capacityMode = 'custom';
    el.capacityValue = '5';
    await el.updateComplete;
    click(el, 'sl-button.save-capacity');
    await settle(el);
    expect(el.shadowRoot!.querySelector('.capacity-config .form-error')?.textContent).toContain(
      'changed by someone else'
    );
  });

  it('shows the effective limit with its source', async () => {
    const el = await mount({ ...LEGACY, _capabilities: { actions: ['read', 'update'] } });
    const text = el.shadowRoot!.querySelector('.effective-limit')?.textContent ?? '';
    expect(text.replace(/\s+/g, ' ')).toContain(
      'Effective limit: 30 (from not enforced — the quota switch is off)'
    );
  });

  it('shows the instance-file name only when it differs from the row name', async () => {
    const differ = await mount({ ...FLAT_K8S, _capabilities: { actions: ['read', 'update'] } });
    expect(differ.shadowRoot!.querySelector('.instance-file-name')).not.toBeNull();
    const p = FLAT_K8S.runtimeTargetPresentation!;
    const same = await mount({
      ...FLAT_K8S,
      runtimeTargetPresentation: { ...p, name: FLAT_K8S.name },
      _capabilities: { actions: ['read', 'update'] },
    });
    expect(same.shadowRoot!.querySelector('.instance-file-name')).toBeNull();
  });
});
