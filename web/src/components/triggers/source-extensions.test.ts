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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { html } from 'lit';
import {
  clearSourceExtensionsForTests,
  ensureExtensionLoaded,
  getEventColumns,
  getEventDetail,
  getFormAuthoring,
  getRegisteredSourceCards,
  registerEventColumns,
  registerEventDetail,
  registerFormAuthoring,
  registerSourceCard,
} from './source-extensions.js';
import { resetServerFlagStateForTests, setServerFlags } from '../../utils/feature-flags.js';
import '../pages/project-triggers.js';
import './trigger-list.js';
import './trigger-event-list.js';
import type { ScionPageProjectTriggers } from '../pages/project-triggers.js';
import type { ScionTriggerList } from './trigger-list.js';
import type { ScionTriggerEventList } from './trigger-event-list.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('triggers source-extensions registry (trivial test provider)', () => {
  beforeEach(() => {
    clearSourceExtensionsForTests();
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
  });

  afterEach(() => {
    clearSourceExtensionsForTests();
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('registers, gates with isEnabled, lazy-loads once, and unregisters cleanly', async () => {
    let enabled = false;
    const loadSpy = vi.fn().mockResolvedValue(undefined);

    const unregister = registerSourceCard('demo-source', {
      isEnabled: () => enabled,
      load: loadSpy,
      render: ({ projectId }) => html`<div data-demo-card>${projectId}</div>`,
    });

    expect(getRegisteredSourceCards()).toHaveLength(0);

    enabled = true;
    const cards = getRegisteredSourceCards();
    expect(cards).toHaveLength(1);
    expect(cards[0].sourceKind).toBe('demo-source');

    await ensureExtensionLoaded(cards[0].entry);
    await ensureExtensionLoaded(cards[0].entry);
    expect(loadSpy).toHaveBeenCalledTimes(1);

    unregister();
    expect(getRegisteredSourceCards()).toHaveLength(0);
  });

  it('renders a registered source card in the Sources header above the tabs', async () => {
    setServerFlags({ 'hub.triggers': true });

    registerSourceCard('demo-source', {
      render: ({ projectId, onNavigateToCreate }) => html`
        <div data-demo-source-card>
          Demo source for ${projectId}
          <button
            type="button"
            data-demo-new-rule
            @click=${() =>
              onNavigateToCreate?.({ sourceKind: 'demo-source', presetId: 'preset-1' })}
          >
            New Rule
          </button>
        </div>
      `,
    });

    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/webhook-endpoints'))
          return Promise.resolve(jsonResponse({ endpoints: [], totalCount: 0 }));
        if (url.includes('/triggers'))
          return Promise.resolve(jsonResponse({ triggers: [], totalCount: 0 }));
        if (url.includes('/trigger-events'))
          return Promise.resolve(jsonResponse({ events: [], totalCount: 0 }));
        if (url.includes('/trigger-fires'))
          return Promise.resolve(jsonResponse({ fires: [], totalCount: 0 }));
        if (url.includes('/agents')) return Promise.resolve(jsonResponse({ agents: [] }));
        return Promise.resolve(jsonResponse({ id: 'proj-1', name: 'Demo Project' }));
      })
    );

    const el = document.createElement('scion-page-project-triggers') as ScionPageProjectTriggers;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    const header = el.shadowRoot?.querySelector('[data-sources-header]');
    expect(header).toBeTruthy();
    expect(header?.textContent).toContain('Demo source for proj-1');
  });

  it('mounts registered form-authoring pane as default, populates fields, and drops form on CEL edit', async () => {
    registerFormAuthoring('demo-source', {
      label: 'Demo Form',
      render: ({ presetId, onChange }) => html`
        <div data-demo-form-pane>
          Preset: ${presetId ?? 'none'}
          <button
            type="button"
            data-demo-apply-form
            @click=${() =>
              onChange({
                types: ['demo.opened'],
                rules: [{ dialect: 'cel', expr: 'event.attributes.kind == "bug"' }],
                form: { preset: presetId ?? 'default', label: 'bug' },
                suggestedActions: [{ type: 'message', agentId: 'ag-1', message: 'hi' }],
              })}
          >
            Apply Form
          </button>
        </div>
      `,
    });

    expect(getFormAuthoring('demo-source')).toBeDefined();

    const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && url.endsWith('/triggers')) {
        return Promise.resolve(jsonResponse({ id: 'tr-new' }, 201));
      }
      return Promise.resolve(jsonResponse({ triggers: [], totalCount: 0 }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-trigger-list') as ScionTriggerList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    el.openCreateDialog('demo-source', 'preset-bug');
    await el.updateComplete;

    const formPane = el.shadowRoot?.querySelector('[data-demo-form-pane]');
    expect(formPane).toBeTruthy();
    expect(formPane?.textContent).toContain('Preset: preset-bug');

    const applyBtn = el.shadowRoot?.querySelector('[data-demo-apply-form]') as HTMLButtonElement;
    applyBtn.click();
    await el.updateComplete;

    // Switch to CEL pane and edit a rule -> form must be dropped.
    const modeGroup = el.shadowRoot?.querySelector('[data-authoring-mode]') as HTMLElement & {
      value: string;
    };
    modeGroup.value = 'cel';
    modeGroup.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await el.updateComplete;

    const whereEditor = el.shadowRoot?.querySelector('scion-trigger-where-editor');
    expect(whereEditor).toBeTruthy();
    whereEditor?.dispatchEvent(
      new CustomEvent('rules-changed', {
        detail: { rules: [{ dialect: 'cel', expr: 'event.attributes.kind == "edited"' }] },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;

    // Fill name & sourceId and submit to verify form: null is sent after CEL edit.
    const nameInput = el.shadowRoot?.querySelector('[data-trigger-name-input]') as HTMLInputElement;
    nameInput.value = 'demo-rule';
    nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));

    const srcInput = el.shadowRoot?.querySelector('[data-trigger-source-id]') as HTMLInputElement;
    srcInput.value = 'src-1';
    srcInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;

    const submitBtn = el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement;
    submitBtn.click();
    await new Promise((r) => setTimeout(r, 20));

    const postCall = fetchMock.mock.calls.find(
      ([url, init]) =>
        typeof url === 'string' && url.endsWith('/triggers') && init?.method === 'POST'
    );
    expect(postCall).toBeDefined();
    const sentBody = JSON.parse(String(postCall?.[1]?.body)) as { match: { form: unknown } };
    expect(sentBody.match.form).toBeNull();
  });

  it('renders registered event columns and event detail section in trigger-event-list', async () => {
    registerEventColumns('demo-source', {
      columns: () => [
        {
          id: 'demo-actor',
          header: 'Demo Actor',
          render: (ev) => ev.attributes?.['actor'] ?? '—',
        },
      ],
    });
    registerEventDetail('demo-source', {
      render: (ev) => html`<div data-demo-detail-section>Extra for ${ev.id}</div>`,
    });

    expect(getEventColumns('demo-source')).toHaveLength(1);
    expect(getEventDetail('demo-source')).toBeDefined();

    const sampleEvent = {
      id: 'ev-1',
      projectId: 'proj-1',
      source: { kind: 'demo-source', id: 'src-1' },
      type: 'demo.opened',
      subject: 'demo:1',
      dedupKey: 'd1',
      occurredAt: '2026-11-20T10:00:00Z',
      receivedAt: '2026-11-20T10:00:00Z',
      attributes: { actor: 'alice' },
      payloadBytes: 12,
      status: 'processed',
      matchCount: 1,
      fireCount: 1,
      createdAt: '2026-11-20T10:00:00Z',
    };

    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/trigger-events/ev-1')) {
          return Promise.resolve(
            jsonResponse({ ...sampleEvent, headers: {}, payload: { ok: true } })
          );
        }
        if (url.includes('/trigger-fires')) {
          return Promise.resolve(jsonResponse({ fires: [], totalCount: 0 }));
        }
        return Promise.resolve(jsonResponse({ events: [sampleEvent], totalCount: 1 }));
      })
    );

    const el = document.createElement('scion-trigger-event-list') as ScionTriggerEventList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-ext-col-header="demo-actor"]')?.textContent).toBe(
      'Demo Actor'
    );
    expect(el.shadowRoot?.querySelector('[data-ext-col-cell="demo-actor"]')?.textContent).toBe(
      'alice'
    );

    await el.openEventDetail('ev-1');
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-demo-detail-section]')?.textContent).toContain(
      'Extra for ev-1'
    );
  });
});
