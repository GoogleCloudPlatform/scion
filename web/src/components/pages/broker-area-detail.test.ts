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
 * V3 and V4: the Brokers area detail page (broker UI Option A, slice A1).
 * The Health tab appears only with hub.health.read and a readable summary;
 * Configure only with the broker's update or delete capability; Unregister
 * only with delete. After a PATCH, Configure re-reads the broker and the
 * page takes that record: the new name shows in the header and the form,
 * hostedByThisServer (carried by GET) stays, and cleared labels and GCP
 * fields stay cleared. The Default-for-project marker comes from isDefault
 * on the broker's providers listing, with no per-project reads.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/admin-status.js', () => ({
  loadAdminStatus: vi.fn(),
  clearAdminStatus: vi.fn(),
}));
vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

import type { RuntimeBroker } from '../../shared/types.js';
import { loadAdminStatus } from '../../client/admin-status.js';
import {
  FakeEventSource,
  FLAT_K8S,
  LAYOUT_MISMATCH,
  LEGACY,
  adminStatus,
  healthSummary,
  jsonResponse,
  nfsStorage,
  settle,
  type HealthSummaryItem,
} from './__fixtures__/broker-area.js';

type Page = HTMLElement & { updateComplete: Promise<boolean> };

const SETTINGS = {
  brokerId: FLAT_K8S.id,
  settings: {},
  effective: {
    maxAgents: {
      value: 30,
      source: 'hub_default',
      count: 2,
      inherited: { value: 30, source: 'hub_default' },
    },
  },
  revision: 0,
  _capabilities: { update: true },
};

let broker: RuntimeBroker;
let summaryStatus = 200;
let summaryItems: HealthSummaryItem[] = [];
let summaryTruncated = false;
/** The summary answers 200 with a body that is not JSON. */
let summaryBadJson = false;
let patchResponse: Partial<RuntimeBroker> = {};
/** What GET returns after a PATCH (the stored record); null = unchanged. */
let afterPatch: RuntimeBroker | null = null;
/** The providers listing GET /runtime-brokers/{id}/projects returns. */
let providerProjects: Record<string, unknown>[] = [];
let fetchMock: ReturnType<typeof vi.fn>;
const mounted: HTMLElement[] = [];

async function mount(b: RuntimeBroker): Promise<Page> {
  broker = b;
  window.history.replaceState(null, '', `/brokers/${b.id}`);
  const el = document.createElement('scion-page-broker-area-detail') as Page;
  document.body.appendChild(el);
  mounted.push(el);
  await settle(el);
  return el;
}

function tabs(el: Page): string[] {
  return Array.from(el.shadowRoot!.querySelectorAll('sl-tab')).map(
    (t) => t.getAttribute('panel') ?? ''
  );
}

function configure(el: Page): (HTMLElement & { updateComplete: Promise<boolean> }) | null {
  return el.shadowRoot!.querySelector('scion-broker-area-configure');
}

describe('scion-page-broker-area-detail', () => {
  beforeAll(async () => {
    await import('./broker-area-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    summaryStatus = 200;
    summaryItems = [];
    summaryTruncated = false;
    summaryBadJson = false;
    patchResponse = {};
    afterPatch = null;
    providerProjects = [];
    fetchMock = vi.fn((url: string, init?: RequestInit) => {
      const id = broker.id;
      if (url === `/api/v1/runtime-brokers/${id}` && init?.method === 'PATCH') {
        if (afterPatch) broker = afterPatch;
        return jsonResponse(patchResponse);
      }
      if (url === `/api/v1/runtime-brokers/${id}`) return jsonResponse(broker);
      if (url === `/api/v1/runtime-brokers/${id}/projects`) {
        return jsonResponse({ projects: providerProjects });
      }
      if (url === `/api/v1/runtime-brokers/${id}/settings`) return jsonResponse(SETTINGS);
      if (url.startsWith('/api/v1/agents')) return jsonResponse({ agents: [] });
      if (url === '/api/v1/admin/health/summary') {
        if (summaryStatus !== 200) {
          return jsonResponse({ error: { code: 'forbidden', message: 'no' } }, summaryStatus);
        }
        if (summaryBadJson) {
          return new Response('not json', {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          });
        }
        return jsonResponse(healthSummary(summaryItems, summaryTruncated));
      }
      return jsonResponse({}, 404);
    });
    vi.stubGlobal('fetch', fetchMock);
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(false));
  });

  afterEach(() => {
    while (mounted.length) mounted.pop()!.remove();
    vi.unstubAllGlobals();
  });

  it('Health tab: absent without hub.health.read, and no summary request', async () => {
    const el = await mount(FLAT_K8S);
    expect(tabs(el)).not.toContain('health');
    expect(fetchMock.mock.calls.some(([u]) => u === '/api/v1/admin/health/summary')).toBe(false);
  });

  it('Health tab: absent on a 403 from the summary', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    summaryStatus = 403;
    const el = await mount(FLAT_K8S);
    expect(tabs(el)).not.toContain('health');
  });

  it('Health tab: present with the permission and a 200', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    summaryItems = [{ id: FLAT_K8S.id }];
    const el = await mount(FLAT_K8S);
    expect(tabs(el)).toContain('health');
    const health = el.shadowRoot!.querySelector('scion-broker-area-health') as Page;
    await health.updateComplete;
    expect(health.shadowRoot!.querySelector('.health-checks')?.textContent).toContain('runtime');
  });

  it('Health tab: says "not in the summary" when the row is missing', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    const el = await mount(FLAT_K8S);
    const health = el.shadowRoot!.querySelector('scion-broker-area-health') as Page;
    await health.updateComplete;
    expect(health.shadowRoot!.querySelector('.not-in-summary')).not.toBeNull();
    expect(health.shadowRoot!.textContent).not.toContain('healthy');
  });

  it('Project storage: the Overview tab shows the reported layout', async () => {
    const el = await mount({ ...FLAT_K8S, workspaceStorage: nfsStorage() });
    const card = el.shadowRoot!.querySelector(
      'sl-tab-panel[name="overview"] scion-broker-storage-card'
    ) as Page;
    expect(card).not.toBeNull();
    await card.updateComplete;
    expect(card.shadowRoot!.querySelector('dl.storage-rows')?.textContent).toContain(
      'filer.example.internal'
    );
    // Without hub.health.read the comparison is not known: no alert.
    expect(card.shadowRoot!.querySelector('.storage-mismatch-alert')).toBeNull();
    expect(card.shadowRoot!.querySelector('.storage-compare-note')).not.toBeNull();
  });

  it('Project storage: the hub-reported differences reach the card and the Health tab', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    summaryItems = [{ id: FLAT_K8S.id, layout_mismatch: LAYOUT_MISMATCH }];
    const el = await mount({ ...FLAT_K8S, workspaceStorage: nfsStorage() });
    const card = el.shadowRoot!.querySelector('scion-broker-storage-card') as Page;
    await card.updateComplete;
    const cardLines = Array.from(card.shadowRoot!.querySelectorAll('.storage-mismatch-alert li'));
    expect(cardLines.map((li) => li.textContent!.trim())).toEqual(LAYOUT_MISMATCH);
    const health = el.shadowRoot!.querySelector('scion-broker-area-health') as Page;
    await health.updateComplete;
    const healthLines = Array.from(
      health.shadowRoot!.querySelectorAll('.storage-layout-mismatch li')
    );
    expect(healthLines.map((li) => li.textContent!.trim())).toEqual(LAYOUT_MISMATCH);
  });

  it('Project storage: no differences listed when the summary reports none', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    summaryItems = [{ id: FLAT_K8S.id }];
    const el = await mount({ ...FLAT_K8S, workspaceStorage: nfsStorage() });
    const card = el.shadowRoot!.querySelector('scion-broker-storage-card') as Page;
    await card.updateComplete;
    expect(card.shadowRoot!.querySelector('.storage-mismatch-alert')).toBeNull();
    expect(card.shadowRoot!.querySelector('.storage-compare-note')).toBeNull();
    const health = el.shadowRoot!.querySelector('scion-broker-area-health') as Page;
    await health.updateComplete;
    expect(health.shadowRoot!.querySelector('.storage-layout-mismatch')).toBeNull();
  });

  it('Project storage: comparison not available when a truncated summary has no row for the broker', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    summaryItems = [{ id: 'other-broker', layout_mismatch: LAYOUT_MISMATCH }];
    summaryTruncated = true;
    const el = await mount({ ...FLAT_K8S, workspaceStorage: nfsStorage() });
    const card = el.shadowRoot!.querySelector('scion-broker-storage-card') as Page;
    await card.updateComplete;
    expect(card.shadowRoot!.querySelector('.storage-mismatch-alert')).toBeNull();
    expect(card.shadowRoot!.querySelector('.storage-compare-note')).not.toBeNull();
  });

  it('Project storage: comparison not available when the summary has no row for the broker', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    summaryItems = [{ id: 'other-broker', layout_mismatch: LAYOUT_MISMATCH }];
    const el = await mount({ ...FLAT_K8S, workspaceStorage: nfsStorage() });
    const card = el.shadowRoot!.querySelector('scion-broker-storage-card') as Page;
    await card.updateComplete;
    expect(card.shadowRoot!.querySelector('.storage-mismatch-alert')).toBeNull();
    expect(card.shadowRoot!.querySelector('.storage-compare-note')).not.toBeNull();
  });

  it('Project storage: comparison not available when the summary row has no workspace storage', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    summaryItems = [{ id: FLAT_K8S.id, no_workspace_storage: true }];
    const el = await mount({ ...FLAT_K8S });
    const card = el.shadowRoot!.querySelector('scion-broker-storage-card') as Page;
    await card.updateComplete;
    expect(card.shadowRoot!.querySelector('.storage-none')).not.toBeNull();
    expect(card.shadowRoot!.querySelector('.storage-mismatch-alert')).toBeNull();
    expect(card.shadowRoot!.querySelector('.storage-compare-note')).not.toBeNull();
  });

  it('Project storage: comparison not available when the broker reports storage but its summary row has none', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    summaryItems = [{ id: FLAT_K8S.id, no_workspace_storage: true }];
    const el = await mount({ ...FLAT_K8S, workspaceStorage: nfsStorage() });
    const card = el.shadowRoot!.querySelector('scion-broker-storage-card') as Page;
    await card.updateComplete;
    expect(card.shadowRoot!.querySelector('.storage-none')).toBeNull();
    expect(card.shadowRoot!.querySelector('.storage-mismatch-alert')).toBeNull();
    expect(card.shadowRoot!.querySelector('.storage-compare-note')).not.toBeNull();
  });

  it('Project storage: comparison not available when the summary request fails', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    summaryStatus = 500;
    const el = await mount({ ...FLAT_K8S, workspaceStorage: nfsStorage() });
    expect(fetchMock.mock.calls.some(([u]) => u === '/api/v1/admin/health/summary')).toBe(true);
    expect(tabs(el)).not.toContain('health');
    const card = el.shadowRoot!.querySelector('scion-broker-storage-card') as Page;
    await card.updateComplete;
    expect(card.shadowRoot!.querySelector('.storage-mismatch-alert')).toBeNull();
    expect(card.shadowRoot!.querySelector('.storage-compare-note')).not.toBeNull();
  });

  it('Project storage: comparison not available when the summary body is not JSON', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    summaryBadJson = true;
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      const el = await mount({ ...FLAT_K8S, workspaceStorage: nfsStorage() });
      expect(fetchMock.mock.calls.some(([u]) => u === '/api/v1/admin/health/summary')).toBe(true);
      expect(errorSpy).toHaveBeenCalledWith('Failed to load broker health:', expect.anything());
      expect(tabs(el)).not.toContain('health');
      const card = el.shadowRoot!.querySelector('scion-broker-storage-card') as Page;
      await card.updateComplete;
      expect(card.shadowRoot!.querySelector('.storage-mismatch-alert')).toBeNull();
      expect(card.shadowRoot!.querySelector('.storage-compare-note')).not.toBeNull();
    } finally {
      errorSpy.mockRestore();
    }
  });

  it('Default for project: taken from isDefault in the providers listing, with no project reads', async () => {
    providerProjects = [
      { projectId: 'p1', projectName: 'One', agentCount: 0, isDefault: false },
      { projectId: 'p2', projectName: 'Two', agentCount: 0, isDefault: true },
      { projectId: 'p3', projectName: 'Three', agentCount: 0 },
    ];
    const el = await mount(FLAT_K8S);
    const agents = el.shadowRoot!.querySelector('scion-broker-area-agents') as HTMLElement & {
      defaultFor: ReadonlySet<string>;
    };
    expect(agents).not.toBeNull();
    expect([...agents.defaultFor]).toEqual(['p2']);
    expect(
      fetchMock.mock.calls.some(([u]) => typeof u === 'string' && u.startsWith('/api/v1/projects/'))
    ).toBe(false);
  });

  it('Configure: absent without update or delete capability', async () => {
    const el = await mount({ ...FLAT_K8S, _capabilities: { actions: ['read'] } });
    expect(tabs(el)).not.toContain('configure');
    expect(configure(el)).toBeNull();
  });

  it('Unregister: shown with delete for a non-admin owner', async () => {
    const el = await mount({ ...LEGACY, _capabilities: { actions: ['read', 'delete'] } });
    expect(tabs(el)).toContain('configure');
    const cfg = configure(el)!;
    await cfg.updateComplete;
    expect(cfg.shadowRoot!.querySelector('sl-button.unregister')).not.toBeNull();
  });

  it('Unregister: hidden without delete, even for an admin', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    const el = await mount({ ...LEGACY, _capabilities: { actions: ['read', 'update'] } });
    const cfg = configure(el)!;
    await cfg.updateComplete;
    expect(cfg.shadowRoot!.querySelector('.identity-section')).not.toBeNull();
    expect(cfg.shadowRoot!.querySelector('sl-button.unregister')).toBeNull();
  });

  it('Configure PATCH: only changed fields; new name shows; marker kept', async () => {
    const el = await mount({ ...FLAT_K8S, _capabilities: { actions: ['read', 'update'] } });
    const cfg = configure(el)! as Page & { name: string };
    await cfg.updateComplete;
    // The PATCH response is the plain record (no hostedByThisServer); the
    // re-read GET carries it.
    const { hostedByThisServer: _omit, ...plain } = FLAT_K8S;
    patchResponse = { ...plain, name: 'renamed' };
    afterPatch = { ...FLAT_K8S, name: 'renamed', _capabilities: { actions: ['read', 'update'] } };
    cfg.name = 'renamed';
    await cfg.updateComplete;
    (cfg.shadowRoot!.querySelector('sl-button.save-identity') as HTMLElement).click();
    await settle(el);

    const patchCall = fetchMock.mock.calls.find(([, init]) => init?.method === 'PATCH');
    expect(patchCall).toBeDefined();
    expect(JSON.parse(String(patchCall![1].body))).toEqual({ name: 'renamed' });
    const header = el.shadowRoot!.querySelector('scion-detail-header');
    expect(header?.getAttribute('heading')).toBe('renamed');
    const marker = el.shadowRoot!.querySelector('scion-detail-header sl-icon.hosted-marker');
    expect(marker).not.toBeNull();
    const cfgAfter = configure(el)!;
    await cfgAfter.updateComplete;
    const nameInput = cfgAfter.shadowRoot!.querySelector('sl-input[name="name"]') as HTMLElement & {
      value: string;
    };
    expect(nameInput.value).toBe('renamed');
  });

  it('Configure PATCH: cleared labels and SA email stay cleared after the save', async () => {
    const start = {
      ...LEGACY,
      labels: { team: 'a' },
      gcp_host_service_account_email: 'sa@p.iam.gserviceaccount.com',
      _capabilities: { actions: ['read', 'update'] },
    } as RuntimeBroker;
    const el = await mount(start);
    const cfg = configure(el)! as Page & { labelsText: string; saEmail: string };
    await cfg.updateComplete;
    expect(cfg.labelsText).toBe('team=a');
    // The hub omits empty labels and SA fields from both PATCH and GET.
    patchResponse = { ...LEGACY };
    afterPatch = { ...LEGACY, _capabilities: { actions: ['read', 'update'] } } as RuntimeBroker;
    cfg.labelsText = '';
    cfg.saEmail = '';
    await cfg.updateComplete;
    (cfg.shadowRoot!.querySelector('sl-button.save-identity') as HTMLElement).click();
    await settle(el);

    const patchCall = fetchMock.mock.calls.find(([, init]) => init?.method === 'PATCH');
    expect(JSON.parse(String(patchCall![1].body))).toEqual({
      labels: {},
      gcpHostServiceAccountEmail: '',
    });
    const after = configure(el)! as Page & { labelsText: string; saEmail: string };
    await after.updateComplete;
    expect(after.labelsText).toBe('');
    expect(after.saEmail).toBe('');
  });

  it('Configure: the registration hint on flat rows only, and the instance key', async () => {
    const flat = await mount({ ...FLAT_K8S, _capabilities: { actions: ['read', 'update'] } });
    const cfg = configure(flat)!;
    await cfg.updateComplete;
    const hints = cfg.shadowRoot!.querySelectorAll('.registration-hint');
    expect(hints).toHaveLength(3);
    expect(hints[0].textContent?.trim()).toBe('Set by the broker on registration.');
    expect(cfg.shadowRoot!.querySelector('.instance-config')?.textContent).toContain(
      'server.broker.instances[k8s-nfs-1]'
    );
    expect(cfg.shadowRoot!.querySelector('.instance-file-name')?.textContent).toContain(
      'Name in instance file: k8s nfs one'
    );
    flat.remove();
    mounted.length = 0;

    const legacy = await mount({ ...LEGACY, _capabilities: { actions: ['read', 'update'] } });
    const lcfg = configure(legacy)!;
    await lcfg.updateComplete;
    expect(lcfg.shadowRoot!.querySelectorAll('.registration-hint')).toHaveLength(0);
    expect(lcfg.shadowRoot!.querySelector('.instance-config')).toBeNull();
  });
});
