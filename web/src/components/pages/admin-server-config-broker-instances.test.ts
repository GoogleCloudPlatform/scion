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
import type { ScionPageAdminServerConfig } from './admin-server-config.js';
import {
  FLAT_BROKERS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import {
  registrySchemaResponse,
  unknownPayloadKeys,
  valueAtPath,
} from './__fixtures__/settings-registry.js';

vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

function makeConfig(overrides: Record<string, unknown> = {}) {
  return {
    schema_version: '1',
    scion_version: '0.1.0-test',
    scion_commit: 'abc123',
    scion_build_time: '2026-01-01T00:00:00Z',
    active_profile: 'default',
    default_template: 'gemini',
    image_registry: 'ghcr.io/test',
    server: {
      mode: 'standalone',
      log_level: 'info',
      hub: {
        port: 8080,
        host: '0.0.0.0',
        admin_emails: ['admin@example.com'],
        public_url: 'https://hub.example.com',
      },
      broker: {
        enabled: true,
        broker_name: 'workstation-host',
        auto_provide: true,
        instances: [
          {
            key: 'local-docker',
            name: 'Workstation Docker',
            runtime_target: {
              type: 'docker',
              display_name: 'Local Docker',
            },
          },
          {
            key: 'cluster-a-agents',
            name: 'Cluster A Agents',
            agent_defaults: {
              default_harness_config: 'gemini',
              image_registry: 'us-docker.pkg.dev/acme/agents',
            },
            runtime_target: {
              type: 'kubernetes',
              display_name: 'Cluster A',
              context: 'gke_acme_us-central1_cluster-a',
              namespace: 'agents',
              kubeconfig: '/etc/scion/cluster-a.kubeconfig',
              gke: true,
              priority_class_name: 'agents-default',
              safe_to_evict: false,
              shared_dir_storage_class: 'standard-rwx',
              shared_dir_size: '20Gi',
              kubernetes_block_service_account: 'agent-block',
              kubernetes_service_account_mappings: {
                'runner@acme.iam.gserviceaccount.com': 'agent-runner',
              },
              home_storage_backend: 'legacy-removed',
              node_selector: {
                'scion.dev/pool': 'agents',
              },
              tolerations: [
                {
                  key: 'scion.dev/pool',
                  operator: 'Equal',
                  value: 'agents',
                  effect: 'NoSchedule',
                },
              ],
              runtime_class_name: 'gvisor',
            },
          },
        ],
      },
      database: { driver: 'sqlite', url: '********' },
      auth: { dev_mode: false, user_access_mode: 'open', authorized_domains: [] },
      storage: { provider: 'local', local_path: '/data' },
      secrets: { backend: 'local' },
    },
    telemetry: {
      enabled: false,
      cloud: { enabled: false },
      hub: { enabled: false },
    },
    ...overrides,
  };
}

function createFetchHandler(
  configResponse: Record<string, unknown>,
  capturedPuts: Record<string, unknown>[]
) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    if (
      init?.method === 'PUT' &&
      path.includes('/api/v1/admin/server-config') &&
      !path.includes('/schema')
    ) {
      const reqBody = JSON.parse(init.body as string) as Record<string, unknown>;
      capturedPuts.push(reqBody);
      return Promise.resolve(
        new Response(JSON.stringify({ reload: { file_applied: ['server.broker.instances'] } }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/api/v1/admin/server-config/schema')) {
      return Promise.resolve(
        new Response(JSON.stringify(registrySchemaResponse()), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/api/v1/admin/server-config')) {
      return Promise.resolve(
        new Response(JSON.stringify(configResponse), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/api/v1/admin/messaging')) {
      return Promise.resolve(
        new Response(JSON.stringify({ cross_project_messaging_enabled: false, revision: 1 }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/api/v1/admin/maintenance/update-status')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            current: {
              version: '0.1.0-test',
              channel: 'nightly',
              commit: 'abc',
              isGitBuild: false,
            },
            mode: { effective: 'release', repoConfigured: false },
            channel: { effective: 'nightly', configured: 'nightly', source: 'config' },
            pin: 'latest',
            installer: { strategy: 'symlink', supported: true },
            updatePolicy: 'notify',
            pendingUpdate: null,
            lastUpdate: null,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    if (path.includes('/api/v1/github-app')) {
      return Promise.resolve(
        new Response(JSON.stringify({}), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    return Promise.resolve(
      new Response(JSON.stringify([]), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

async function mountConfig(
  configResponse: Record<string, unknown>,
  capturedPuts: Record<string, unknown>[] = []
): Promise<ScionPageAdminServerConfig> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(configResponse, capturedPuts)));
  const el = document.createElement('scion-page-admin-server-config') as ScionPageAdminServerConfig;
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 150));
  await el.updateComplete;
  return el;
}

function inputEvent(target: Element, value: string): void {
  (target as HTMLInputElement).value = value;
  target.dispatchEvent(new CustomEvent('sl-input', { bubbles: true, composed: true }));
}

function changeEvent(target: Element, value: string): void {
  (target as HTMLSelectElement).value = value;
  target.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
}

async function clickSave(el: ScionPageAdminServerConfig): Promise<void> {
  const saveBtn = saveButton(el);
  saveBtn.click();
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 120));
  await el.updateComplete;
}

function saveButton(el: ScionPageAdminServerConfig): HTMLElement {
  const buttons = Array.from(el.shadowRoot?.querySelectorAll('sl-button[variant="primary"]') ?? []);
  const btn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload') as
    | HTMLElement
    | undefined;
  expect(btn, 'Save & Reload button').toBeDefined();
  return btn!;
}

describe('scion-page-admin-server-config — server.broker.instances (ptone/scion#3283)', () => {
  let element: ScionPageAdminServerConfig | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler(makeConfig(), [])));
    await import('./admin-server-config.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    vi.restoreAllMocks();
  });

  function makeConfigWithoutInstances(
    overrides: Record<string, unknown> = {}
  ): Record<string, unknown> {
    const cfg = makeConfig(overrides);
    delete (cfg.server.broker as Record<string, unknown>).instances;
    return cfg;
  }

  it('hides server.broker.instances when FLAT_BROKERS_FLAG is off and no instances are stored', async () => {
    setFeatureFlag(FLAT_BROKERS_FLAG, false);
    element = await mountConfig(
      makeConfigWithoutInstances({ settings_tier: 'db', layer0_editable: true })
    );

    expect(element.shadowRoot?.querySelector('[data-broker-instances-section]')).toBeNull();
  });

  it('renders server.broker.instances when FLAT_BROKERS_FLAG is off if stored instances exist', async () => {
    setFeatureFlag(FLAT_BROKERS_FLAG, false);
    element = await mountConfig(makeConfig({ settings_tier: 'db', layer0_editable: true }));

    const section = element.shadowRoot?.querySelector('[data-broker-instances-section]');
    expect(section).not.toBeNull();
    expect(section?.querySelectorAll('[data-broker-instance-card]').length).toBe(2);
  });

  it('renders server.broker.instances when FLAT_BROKERS_FLAG is on even with no stored instances', async () => {
    setFeatureFlag(FLAT_BROKERS_FLAG, true);
    element = await mountConfig(
      makeConfigWithoutInstances({ settings_tier: 'db', layer0_editable: true })
    );

    const section = element.shadowRoot?.querySelector('[data-broker-instances-section]');
    expect(section).not.toBeNull();
    expect(section?.textContent).toContain('No flat Runtime Broker instances configured.');
    const addBtn = section?.querySelector('[data-add-broker-instance]') as HTMLElement | null;
    expect(addBtn).not.toBeNull();
    addBtn!.click();
    await element.updateComplete;
    expect(element.shadowRoot?.querySelectorAll('[data-broker-instance-card]').length).toBe(1);
  });

  it('renders server.broker.instances read-only in hosted DB mode and never sends it on save', async () => {
    const puts: Record<string, unknown>[] = [];
    element = await mountConfig(makeConfig({ settings_tier: 'db', layer0_editable: false }), puts);

    const cards = Array.from(
      element.shadowRoot?.querySelectorAll('[data-broker-instance-card]') ?? []
    );
    expect(cards.length).toBe(2);

    const section = element.shadowRoot?.querySelector('[data-broker-instances-section]');
    expect(section).toBeTruthy();
    expect(section?.querySelector('.read-only-badge')?.textContent).toContain(
      'Managed via deployment configuration'
    );
    expect(section?.querySelector('[data-add-broker-instance]')).toBeNull();
    expect(section?.querySelector('[data-remove-broker-instance]')).toBeNull();

    const inputs = Array.from(section?.querySelectorAll('sl-input, sl-select') ?? []);
    expect(inputs.length).toBeGreaterThan(0);
    for (const inp of inputs) {
      expect(inp.hasAttribute('disabled')).toBe(true);
    }

    // Mark a Layer-1 setting dirty and save; server.broker.instances must not be sent.
    (element as unknown as { defaultMaxTurns: number }).defaultMaxTurns = 42;
    await element.updateComplete;
    await clickSave(element);

    expect(puts.length).toBe(1);
    const body = { ...puts[0] };
    delete body.expected_revisions;
    expect(unknownPayloadKeys(body, 'layer1')).toEqual([]);
    expect(valueAtPath(puts[0], ['server', 'broker']).present).toBe(false);
  });

  it('omits server.broker.instances when untouched in both workstation DB mode and file mode', async () => {
    for (const mode of [
      { settings_tier: 'db', layer0_editable: true },
      { settings_tier: 'file' },
    ]) {
      const puts: Record<string, unknown>[] = [];
      element = await mountConfig(makeConfig(mode), puts);

      // Edit a sibling server.broker field without touching instances.
      (element as unknown as { brokerNickname: string }).brokerNickname = 'My Host';
      await element.updateComplete;
      await clickSave(element);

      expect(puts.length).toBe(1);
      expect(valueAtPath(puts[0], ['server', 'broker', 'broker_nickname']).value).toBe('My Host');
      expect(valueAtPath(puts[0], ['server', 'broker', 'instances']).present).toBe(false);

      element.remove();
      element = null;
    }
  });

  it('edits instance fields & Kubernetes placement while preserving unrendered subfields', async () => {
    const puts: Record<string, unknown>[] = [];
    element = await mountConfig(makeConfig({ settings_tier: 'db', layer0_editable: true }), puts);

    const cards = Array.from(
      element.shadowRoot?.querySelectorAll('[data-broker-instance-card]') ?? []
    );
    expect(cards.length).toBe(2);

    // Card 0 is docker: Kubernetes fields and placement must be hidden.
    const dockerCard = cards[0]!;
    expect(dockerCard.querySelector('[data-instance-field="context"]')).toBeNull();
    expect(dockerCard.querySelector('[data-instance-field="namespace"]')).toBeNull();
    expect(dockerCard.querySelector('.k8s-placement')).toBeNull();

    // Card 1 is kubernetes: context, namespace, and placement are rendered.
    const k8sCard = cards[1]!;
    const displayNameInput = k8sCard.querySelector(
      '[data-instance-field="display_name"]'
    ) as HTMLElement;
    expect(displayNameInput).toBeTruthy();
    inputEvent(displayNameInput, 'Cluster A Prod');

    const nsInput = k8sCard.querySelector('[data-instance-field="namespace"]') as HTMLElement;
    inputEvent(nsInput, 'agents-prod');

    // Update runtime_class_name in placement section
    const rcInput = k8sCard.querySelector('.k8s-runtime_class_name') as HTMLElement;
    expect(rcInput).toBeTruthy();
    inputEvent(rcInput, 'kata');

    // Add a second toleration with Exists (which disables value)
    const tolSection = k8sCard.querySelector('[data-k8s-field="tolerations"]')!;
    const addTolBtn = tolSection.querySelector('.k8s-add') as HTMLElement;
    addTolBtn.click();
    await element.updateComplete;

    const tolRows = Array.from(tolSection.querySelectorAll('.k8s-row'));
    expect(tolRows.length).toBe(2);
    const secondOp = tolRows[1]!.querySelector('.tol-operator') as HTMLElement;
    changeEvent(secondOp, 'Exists');
    await element.updateComplete;
    const secondVal = tolRows[1]!.querySelector('.tol-value') as HTMLElement;
    expect(secondVal.hasAttribute('disabled')).toBe(true);

    await clickSave(element);

    expect(puts.length).toBe(1);
    const body = { ...puts[0] };
    delete body.expected_revisions;
    expect(unknownPayloadKeys(body, 'settings')).toEqual([]);

    const sent = valueAtPath(puts[0], ['server', 'broker', 'instances']).value as Array<
      Record<string, unknown>
    >;
    expect(Array.isArray(sent)).toBe(true);
    expect(sent.length).toBe(2);

    const sentK8s = sent[1]!;
    expect(sentK8s.key).toBe('cluster-a-agents');
    expect(sentK8s.name).toBe('Cluster A Agents');
    // Preserved agent_defaults:
    expect(sentK8s.agent_defaults).toEqual({
      default_harness_config: 'gemini',
      image_registry: 'us-docker.pkg.dev/acme/agents',
    });

    const rt = sentK8s.runtime_target as Record<string, unknown>;
    expect(rt.type).toBe('kubernetes');
    expect(rt.display_name).toBe('Cluster A Prod');
    expect(rt.context).toBe('gke_acme_us-central1_cluster-a');
    expect(rt.namespace).toBe('agents-prod');
    expect(rt.runtime_class_name).toBe('kata');
    expect(rt.node_selector).toEqual({ 'scion.dev/pool': 'agents' });
    expect(rt.tolerations).toEqual([
      { key: 'scion.dev/pool', operator: 'Equal', value: 'agents', effect: 'NoSchedule' },
      { operator: 'Exists' },
    ]);
    // Preserved unrendered Kubernetes subfields:
    expect(rt.kubeconfig).toBe('/etc/scion/cluster-a.kubeconfig');
    expect(rt.gke).toBe(true);
    expect(rt.priority_class_name).toBe('agents-default');
    expect(rt.safe_to_evict).toBe(false);
    expect(rt.shared_dir_storage_class).toBe('standard-rwx');
    expect(rt.shared_dir_size).toBe('20Gi');
    expect(rt.kubernetes_block_service_account).toBe('agent-block');
    expect(rt.kubernetes_service_account_mappings).toEqual({
      'runner@acme.iam.gserviceaccount.com': 'agent-runner',
    });
    // Removed legacy storage override stripped:
    expect(rt.home_storage_backend).toBeUndefined();
  });

  it('locks key and runtime_target.type on saved instances while keeping them editable on newly added instances', async () => {
    const puts: Record<string, unknown>[] = [];
    element = await mountConfig(makeConfig({ settings_tier: 'file' }), puts);

    const savedCards = Array.from(
      element.shadowRoot?.querySelectorAll('[data-broker-instance-card]') ?? []
    );
    expect(savedCards.length).toBe(2);

    for (const card of savedCards) {
      const keyInput = card.querySelector('[data-instance-field="key"]')!;
      const typeSelect = card.querySelector('[data-instance-field="type"]')!;
      const nameInput = card.querySelector('[data-instance-field="name"]')!;
      expect(keyInput.hasAttribute('disabled')).toBe(true);
      expect(typeSelect.hasAttribute('disabled')).toBe(true);
      expect(nameInput.hasAttribute('disabled')).toBe(false);
    }

    // Programmatic events on locked fields must also be ignored as no-ops:
    inputEvent(savedCards[0]!.querySelector('[data-instance-field="key"]')!, 'hijacked-key');
    changeEvent(savedCards[1]!.querySelector('[data-instance-field="type"]')!, 'docker');
    await element.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 30));
    await element.updateComplete;

    // Newly added instance has editable key and runtime_target.type:
    const section = element.shadowRoot?.querySelector('[data-broker-instances-section]');
    expect(section).toBeTruthy();
    const addBtn = section!.querySelector('[data-add-broker-instance]') as HTMLElement;
    addBtn.click();
    await element.updateComplete;

    const allCards = Array.from(
      element.shadowRoot?.querySelectorAll('[data-broker-instance-card]') ?? []
    );
    expect(allCards.length).toBe(3);

    const newCard = allCards[2]!;
    const newKeyInput = newCard.querySelector('[data-instance-field="key"]')!;
    const newTypeSelect = newCard.querySelector('[data-instance-field="type"]')!;
    expect(newKeyInput.hasAttribute('disabled')).toBe(false);
    expect(newTypeSelect.hasAttribute('disabled')).toBe(false);

    inputEvent(newKeyInput, 'cluster-b-agents');
    inputEvent(newCard.querySelector('[data-instance-field="name"]')!, 'Cluster B Agents');
    changeEvent(newTypeSelect, 'kubernetes');
    await element.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 30));
    await element.updateComplete;

    await clickSave(element);
    expect(puts.length).toBe(1);

    const sent = valueAtPath(puts[0], ['server', 'broker', 'instances']).value as Array<
      Record<string, unknown>
    >;
    expect(sent.length).toBe(3);
    // Saved instances kept their original key and runtime_target.type untouched:
    expect(sent[0]!.key).toBe('local-docker');
    expect((sent[1]!.runtime_target as Record<string, unknown>).type).toBe('kubernetes');
    // Newly added instance saved with custom key and kubernetes type:
    expect(sent[2]!).toEqual({
      key: 'cluster-b-agents',
      name: 'Cluster B Agents',
      runtime_target: { type: 'kubernetes' },
    });
  });

  it('strips Kubernetes-only runtime_target fields when switching a newly added instance to docker', async () => {
    const puts: Record<string, unknown>[] = [];
    element = await mountConfig(makeConfig({ settings_tier: 'file' }), puts);

    const section = element.shadowRoot?.querySelector('[data-broker-instances-section]');
    expect(section).toBeTruthy();
    const addBtn = section!.querySelector('[data-add-broker-instance]') as HTMLElement;
    addBtn.click();
    await element.updateComplete;

    let cards = Array.from(
      element.shadowRoot?.querySelectorAll('[data-broker-instance-card]') ?? []
    );
    let addedCard = cards[2]!;
    inputEvent(addedCard.querySelector('[data-instance-field="key"]')!, 'converted-docker');
    inputEvent(addedCard.querySelector('[data-instance-field="name"]')!, 'Converted Docker');
    inputEvent(addedCard.querySelector('[data-instance-field="display_name"]')!, 'Converted');

    const typeSelect = addedCard.querySelector('[data-instance-field="type"]') as HTMLElement;
    changeEvent(typeSelect, 'kubernetes');
    await element.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 30));
    await element.updateComplete;

    cards = Array.from(element.shadowRoot?.querySelectorAll('[data-broker-instance-card]') ?? []);
    addedCard = cards[2]!;
    inputEvent(addedCard.querySelector('[data-instance-field="context"]')!, 'gke_ctx');
    inputEvent(addedCard.querySelector('[data-instance-field="namespace"]')!, 'agents');
    inputEvent(addedCard.querySelector('.k8s-runtime_class_name')!, 'gvisor');
    await element.updateComplete;

    // Now switch the unsaved instance back to docker: K8s-only fields are stripped
    changeEvent(addedCard.querySelector('[data-instance-field="type"]')!, 'docker');
    await element.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 30));
    await element.updateComplete;

    await clickSave(element);
    expect(puts.length).toBe(1);

    const sent = valueAtPath(puts[0], ['server', 'broker', 'instances']).value as Array<
      Record<string, unknown>
    >;
    const converted = sent[2]!;
    expect(converted.runtime_target).toEqual({
      type: 'docker',
      display_name: 'Converted',
    });
  });

  it('supports adding an instance and sends null when removing all loaded instances', async () => {
    const puts: Record<string, unknown>[] = [];
    element = await mountConfig(makeConfig({ settings_tier: 'db', layer0_editable: true }), puts);

    const section = element.shadowRoot?.querySelector('[data-broker-instances-section]');
    expect(section).toBeTruthy();
    const addBtn = section!.querySelector('[data-add-broker-instance]') as HTMLElement;
    addBtn.click();
    await element.updateComplete;

    let cards = Array.from(
      element.shadowRoot?.querySelectorAll('[data-broker-instance-card]') ?? []
    );
    expect(cards.length).toBe(3);

    const newCard = cards[2]!;
    inputEvent(newCard.querySelector('[data-instance-field="key"]')!, 'edge-docker');
    inputEvent(newCard.querySelector('[data-instance-field="name"]')!, 'Edge Docker');
    await element.updateComplete;

    expect(saveButton(element).hasAttribute('disabled')).toBe(false);

    // Now remove all 3 instances and save: must send server.broker.instances = null
    while (true) {
      const rem = element.shadowRoot?.querySelector(
        '[data-remove-broker-instance]'
      ) as HTMLElement | null;
      if (!rem) break;
      rem.click();
      await element.updateComplete;
    }

    cards = Array.from(element.shadowRoot?.querySelectorAll('[data-broker-instance-card]') ?? []);
    expect(cards.length).toBe(0);

    await clickSave(element);
    expect(puts.length).toBe(1);
    const instLeaf = valueAtPath(puts[0], ['server', 'broker', 'instances']);
    expect(instLeaf.present).toBe(true);
    expect(instLeaf.value).toBeNull();
  });

  it('blocks Save on invalid instance key, duplicate key, empty name, or invalid Kubernetes placement', async () => {
    element = await mountConfig(makeConfig({ settings_tier: 'file' }));

    const section = element.shadowRoot?.querySelector('[data-broker-instances-section]');
    expect(section).toBeTruthy();
    (section!.querySelector('[data-add-broker-instance]') as HTMLElement).click();
    await element.updateComplete;

    const cards = Array.from(
      element.shadowRoot?.querySelectorAll('[data-broker-instance-card]') ?? []
    );
    const k8sCard = cards[1]!;
    const newCard = cards[2]!;
    inputEvent(newCard.querySelector('[data-instance-field="name"]')!, 'New Instance');

    // Invalid key on newly added instance
    const keyInput = newCard.querySelector('[data-instance-field="key"]')!;
    inputEvent(keyInput, 'Bad_Key');
    await element.updateComplete;
    expect(saveButton(element).hasAttribute('disabled')).toBe(true);

    // Duplicate key colliding with a loaded instance
    inputEvent(keyInput, 'local-docker');
    await element.updateComplete;
    expect(saveButton(element).hasAttribute('disabled')).toBe(true);

    // Valid key on new instance, empty name on loaded instance
    inputEvent(keyInput, 'new-docker');
    const nameInput = k8sCard.querySelector('[data-instance-field="name"]')!;
    inputEvent(nameInput, '');
    await element.updateComplete;
    expect(saveButton(element).hasAttribute('disabled')).toBe(true);

    // Restore name, set invalid RuntimeClass
    inputEvent(nameInput, 'Cluster A Agents');
    const rcInput = k8sCard.querySelector('.k8s-runtime_class_name')!;
    inputEvent(rcInput, 'Bad_Runtime_Class');
    await element.updateComplete;
    expect(saveButton(element).hasAttribute('disabled')).toBe(true);

    // Fix RuntimeClass -> Save re-enabled
    inputEvent(rcInput, 'gvisor');
    await element.updateComplete;
    expect(saveButton(element).hasAttribute('disabled')).toBe(false);
  });
});
