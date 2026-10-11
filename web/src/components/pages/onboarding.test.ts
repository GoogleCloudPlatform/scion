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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { apiFetch } from '../../client/api.js';
import { BROKER_RESTART_HINT } from './onboarding.js';
import {
  MEMBERSHIP_CHANGED_EVENT,
  type MembershipChangedDetail,
} from '../../utils/membership-events.js';

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return { ...actual, apiFetch: vi.fn() };
});

/** The workspace step's state and handlers, reached past their private modifiers. */
interface OnboardingPage extends HTMLElement {
  currentStep: number;
  error: string | null;
  wsProjectName: string;
  wsEmbeddedBrokerID: string;
  wsPathValidation: { resolved: string; exists: boolean; isDir: boolean } | null;
  handleWsHubCreate(): Promise<void>;
  handleWsLinkedCreate(): Promise<void>;
  gcloudADCAvailable: boolean;
  autoInjectGcloudADC: boolean;
  selectedHarnesses: Set<string>;
  handleHarnessesNext(): Promise<void>;
  registryInput: string;
  brokerRestartNeeded: boolean;
  handleSaveRegistry(): Promise<void>;
  updateComplete: Promise<boolean>;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function respond(...responses: Response[]): void {
  const queue = [...responses];
  vi.mocked(apiFetch).mockImplementation(() => Promise.resolve(queue.shift()!));
}

let heard: MembershipChangedDetail[] = [];
const onMembership = (e: Event): void => {
  heard.push((e as CustomEvent<MembershipChangedDetail>).detail);
};

beforeAll(async () => {
  await import('./onboarding.js');
}, 60_000);

afterEach(() => {
  window.removeEventListener(MEMBERSHIP_CHANGED_EVENT, onMembership);
  vi.mocked(apiFetch).mockReset();
});

function createPage(): OnboardingPage {
  heard = [];
  window.addEventListener(MEMBERSHIP_CHANGED_EVENT, onMembership);
  const el = document.createElement('scion-page-onboarding') as unknown as OnboardingPage;
  el.wsProjectName = 'Notes';
  el.wsEmbeddedBrokerID = 'broker-1';
  el.wsPathValidation = { resolved: '/home/u/notes', exists: true, isDir: true };
  return el;
}

describe('onboarding workspace step: membership-changed after project creation', () => {
  it('creating a hub project announces a membership change for it', async () => {
    const el = createPage();
    respond(jsonResponse({ project: { id: 'p-new' } }, 201));

    await el.handleWsHubCreate();

    expect(heard).toEqual([{ kind: 'project', id: 'p-new' }]);
    expect(el.currentStep).toBe(6);
  });

  it('a hub project create that finds an existing project announces nothing', async () => {
    const el = createPage();
    respond(jsonResponse({ project: { id: 'p-old' } }, 200));

    await el.handleWsHubCreate();

    expect(heard).toEqual([]);
    expect(el.currentStep).toBe(6);
  });

  it('a failed hub project create announces nothing', async () => {
    const el = createPage();
    respond(jsonResponse({ error: { message: 'nope' } }, 500));

    await el.handleWsHubCreate();

    expect(heard).toEqual([]);
    expect(el.currentStep).toBe(0);
  });

  it('creating a linked project announces a membership change, even when linking fails', async () => {
    const el = createPage();
    respond(
      jsonResponse({ project: { id: 'p-linked' } }, 201),
      jsonResponse({ error: { message: 'link failed' } }, 500)
    );

    await el.handleWsLinkedCreate();

    expect(heard).toEqual([{ kind: 'project', id: 'p-linked' }]);
    expect(el.error).toBeTruthy();
  });

  it('a linked project create that finds an existing project announces nothing', async () => {
    const el = createPage();
    respond(
      jsonResponse({ project: { id: 'p-old' } }, 200),
      jsonResponse({ error: { message: 'link failed' } }, 500)
    );

    await el.handleWsLinkedCreate();

    expect(heard).toEqual([]);
  });

  it('a failed linked project create announces nothing', async () => {
    const el = createPage();
    respond(jsonResponse({ error: { message: 'nope' } }, 500));

    await el.handleWsLinkedCreate();

    expect(heard).toEqual([]);
  });
});

describe('onboarding harness step: gcloud ADC preference', () => {
  function createHarnessPage(): OnboardingPage {
    const el = createPage();
    el.currentStep = 4;
    el.selectedHarnesses = new Set(['claude']);
    el.gcloudADCAvailable = true;
    el.autoInjectGcloudADC = true;
    return el;
  }

  it('saves the preference through the workstation-settings PATCH, not server-config', async () => {
    const el = createHarnessPage();
    respond(jsonResponse({}), jsonResponse({ auto_inject_gcloud_adc: true }));

    await el.handleHarnessesNext();

    const calls = vi.mocked(apiFetch).mock.calls;
    expect(calls).toHaveLength(2);
    expect(calls[1][0]).toBe('/api/v1/system/workstation-settings');
    expect(calls[1][1]?.method).toBe('PATCH');
    expect(JSON.parse(calls[1][1]?.body as string)).toEqual({ auto_inject_gcloud_adc: true });
    expect(calls.some((c) => c[0] === '/api/v1/admin/server-config')).toBe(false);
    expect(el.error).toBeNull();
    expect(el.currentStep).toBe(5);
  });

  it('a failed save surfaces an error and stays on the step', async () => {
    const el = createHarnessPage();
    respond(jsonResponse({}), jsonResponse({ error: { message: 'write failed' } }, 500));

    await el.handleHarnessesNext();

    expect(el.error).toBeTruthy();
    expect(el.currentStep).toBe(4);
  });
});

describe('onboarding registry step: broker restart hint', () => {
  /**
   * Connects the page, lets its own initialize() finish (its status and
   * identity reads answer 404), and returns the text of the rendered
   * warning banners.
   */
  async function renderedWarnings(el: OnboardingPage): Promise<string[]> {
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({}, 404));
    document.body.appendChild(el);
    try {
      await vi.waitFor(() => {
        expect(el.shadowRoot?.querySelector('.loading-state')).toBeNull();
        expect(el.shadowRoot?.querySelector('.wizard')).not.toBeNull();
      });
      await el.updateComplete;
      return Array.from(el.shadowRoot?.querySelectorAll('.warning-banner') ?? []).map(
        (b) => b.textContent ?? ''
      );
    } finally {
      el.remove();
    }
  }

  function createRegistryPage(): OnboardingPage {
    const el = createPage();
    el.currentStep = 3;
    el.wsEmbeddedBrokerID = '';
    el.registryInput = 'ghcr.io/example';
    return el;
  }

  it('shows the restart hint when the server reports no embedded broker after the save', async () => {
    const el = createRegistryPage();
    respond(jsonResponse({ ok: true }), jsonResponse({ complete: false, embeddedBrokerID: '' }));

    await el.handleSaveRegistry();

    const calls = vi.mocked(apiFetch).mock.calls;
    expect(calls[0][0]).toBe('/api/v1/system/registry');
    expect(calls[1][0]).toBe('/api/v1/system/status');
    expect(el.brokerRestartNeeded).toBe(true);
    expect(el.error).toBeNull();

    expect(await renderedWarnings(el)).toContainEqual(expect.stringContaining(BROKER_RESTART_HINT));
  });

  it('no hint when the embedded broker is running', async () => {
    const el = createRegistryPage();
    respond(jsonResponse({ ok: true }), jsonResponse({ complete: false, embeddedBrokerID: 'b-1' }));

    await el.handleSaveRegistry();

    expect(el.brokerRestartNeeded).toBe(false);
    expect(el.wsEmbeddedBrokerID).toBe('b-1');

    expect(await renderedWarnings(el)).not.toContainEqual(
      expect.stringContaining(BROKER_RESTART_HINT)
    );
  });

  it('a failed save does not check the broker', async () => {
    const el = createRegistryPage();
    respond(jsonResponse({ error: { message: 'nope' } }, 500));

    await el.handleSaveRegistry();

    expect(vi.mocked(apiFetch).mock.calls).toHaveLength(1);
    expect(el.brokerRestartNeeded).toBe(false);
    expect(el.error).toBeTruthy();
  });

  it('the workspace step names the restart when no embedded broker is known', async () => {
    const el = createPage();
    el.wsEmbeddedBrokerID = '';

    await el.handleWsLinkedCreate();

    expect(el.error).toBe(`No embedded broker available. ${BROKER_RESTART_HINT}`);
    expect(vi.mocked(apiFetch)).not.toHaveBeenCalled();
  });
});
