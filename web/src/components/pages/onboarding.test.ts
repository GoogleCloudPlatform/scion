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
import { BROKER_RESTART_HINT, registryPrefill } from './onboarding.js';
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
  harnessConfigs: Array<{ slug: string }>;
  harnessLoadFailed: boolean;
  loadHarnessConfigs(): Promise<void>;
  handleSaveRegistryAndNext(): Promise<void>;
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

describe('onboarding registry step: pre-fill', () => {
  it('never pre-fills the server node env, and needs no save for it', () => {
    const p = registryPrefill({
      imageRegistry: 'node.example/scion',
      imageRegistrySource: 'node_env',
      imageRegistrySaveable: 'file.example/scion',
    });
    expect(p.effective).toBe('node.example/scion');
    expect(p.input).toBe('file.example/scion');
    expect(p.fromNodeEnv).toBe(true);
  });

  it('pre-fills the saveable value for a saved or file registry', () => {
    const p = registryPrefill({
      imageRegistry: 'db.example/scion',
      imageRegistrySource: 'hub_db',
      imageRegistrySaveable: 'db.example/scion',
    });
    expect(p.input).toBe('db.example/scion');
    expect(p.fromNodeEnv).toBe(false);
  });

  it('suggests the default when nothing is saveable', () => {
    const p = registryPrefill({
      imageRegistry: 'node.example/scion',
      imageRegistrySource: 'node_env',
    });
    expect(p.input).toBe('ghcr.io/homebrew-scion');
    expect(registryPrefill(null).input).toBe('ghcr.io/homebrew-scion');
  });

  it('falls back to imageRegistry for a hub that does not report the source', () => {
    const p = registryPrefill({ imageRegistry: 'old.example/scion' });
    expect(p.input).toBe('old.example/scion');
    expect(p.fromNodeEnv).toBe(false);
  });
});

describe('onboarding harness step: harness config list', () => {
  const listPath = '/api/v1/harness-configs?scope=global&status=active';
  const configs = [
    {
      id: 'hc-1',
      name: 'claude',
      slug: 'claude',
      harness: 'claude',
      status: 'active',
      scope: 'global',
    },
    {
      id: 'hc-2',
      name: 'codex',
      slug: 'codex',
      harness: 'codex',
      status: 'active',
      scope: 'global',
    },
  ];

  function listCalls(): number {
    return vi.mocked(apiFetch).mock.calls.filter((c) => c[0] === listPath).length;
  }

  afterEach(() => {
    sessionStorage.clear();
  });

  it('a resume that lands on the harness step lists the harness configs', async () => {
    // A reload after the restart the registry step asks for: onboarding was
    // started and the status reports harnesses seeded.
    sessionStorage.setItem('onboardingStarted', 'true');
    sessionStorage.setItem(
      'onboardingStatus',
      JSON.stringify({ identitySet: true, runtimeOK: true, harnessesSeeded: true })
    );
    vi.mocked(apiFetch).mockImplementation((path) =>
      Promise.resolve(
        path === listPath ? jsonResponse({ harnessConfigs: configs }) : jsonResponse({}, 404)
      )
    );
    const el = createPage();
    document.body.appendChild(el);
    try {
      await vi.waitFor(() => {
        expect(el.shadowRoot?.querySelector('.harness-list')).not.toBeNull();
      });
      await el.updateComplete;

      expect(el.currentStep).toBe(4);
      expect(listCalls()).toBe(1);
      expect(el.harnessConfigs.map((hc) => hc.slug)).toEqual(['claude', 'codex']);
      expect(el.error).toBeNull();
      const listed = el.shadowRoot?.querySelector('.harness-list')?.textContent ?? '';
      expect(listed).toContain('claude');
      expect(listed).toContain('codex');
    } finally {
      el.remove();
    }
  });

  it('a failed list request shows an actionable error and a retry, not an empty list', async () => {
    vi.mocked(apiFetch).mockImplementation((path) =>
      Promise.resolve(
        path === listPath
          ? jsonResponse({ error: { message: 'harness configs unavailable' } }, 500)
          : jsonResponse({}, 404)
      )
    );
    const el = createPage();
    document.body.appendChild(el);
    try {
      await vi.waitFor(() => {
        expect(el.shadowRoot?.querySelector('.loading-state')).toBeNull();
      });
      await el.loadHarnessConfigs();
      el.currentStep = 4;
      await el.updateComplete;

      expect(el.harnessConfigs).toEqual([]);
      expect(el.harnessLoadFailed).toBe(true);
      expect(el.error).toBe(
        'harness configs unavailable (HTTP 500). Retry, or check the server log.'
      );
      expect(el.shadowRoot?.querySelector('.error-banner')?.textContent).toContain(
        'harness configs unavailable'
      );
      const retry = el.shadowRoot?.querySelector<HTMLElement>('.harness-load-retry');
      expect(retry).not.toBeNull();

      // The retry loads the list again; a success clears the error.
      vi.mocked(apiFetch).mockImplementation((path) =>
        Promise.resolve(
          path === listPath ? jsonResponse({ harnessConfigs: configs }) : jsonResponse({}, 404)
        )
      );
      retry!.click();
      await vi.waitFor(() => {
        expect(el.harnessConfigs.map((hc) => hc.slug)).toEqual(['claude', 'codex']);
      });
      expect(el.error).toBeNull();
      expect(el.harnessLoadFailed).toBe(false);
    } finally {
      el.remove();
    }
  });

  it('a failed list request without a message shows the fallback error', async () => {
    const el = createPage();
    respond(new Response('bad gateway', { status: 502 }));

    await el.loadHarnessConfigs();

    expect(el.error).toBe(
      'Failed to load harness configurations (HTTP 502). Retry, or check the server log.'
    );
    expect(el.harnessLoadFailed).toBe(true);
  });

  it('saving the registry and going on loads the list once', async () => {
    vi.mocked(apiFetch).mockImplementation((path) => {
      if (path === listPath) return Promise.resolve(jsonResponse({ harnessConfigs: configs }));
      if (path === '/api/v1/system/registry') return Promise.resolve(jsonResponse({ ok: true }));
      if (path === '/api/v1/system/status') {
        return Promise.resolve(jsonResponse({ complete: false, embeddedBrokerID: 'b-1' }));
      }
      return Promise.resolve(jsonResponse({}, 404));
    });
    const el = createPage();
    el.currentStep = 3;
    el.registryInput = 'ghcr.io/example';

    await el.handleSaveRegistryAndNext();
    await vi.waitFor(() => {
      expect(el.harnessConfigs.map((hc) => hc.slug)).toEqual(['claude', 'codex']);
    });

    expect(el.currentStep).toBe(4);
    expect(listCalls()).toBe(1);
  });
});
