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
  getRegisteredSourceKinds,
  registerEventColumns,
  registerEventDetail,
  registerFormAuthoring,
  registerSourceCard,
  registerSourceKind,
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

  it('evicts failed load() promises so extensions are retryable (O4)', async () => {
    let attempts = 0;
    const entry = {
      load: vi.fn().mockImplementation(() => {
        attempts++;
        if (attempts === 1) return Promise.reject(new Error('transient load failure'));
        return Promise.resolve();
      }),
      render: () => html`<div>ok</div>`,
    };

    await expect(ensureExtensionLoaded(entry)).rejects.toThrow('transient load failure');
    await expect(ensureExtensionLoaded(entry)).resolves.toBeUndefined();
    expect(entry.load).toHaveBeenCalledTimes(2);
    // Subsequent call is cached after success.
    await expect(ensureExtensionLoaded(entry)).resolves.toBeUndefined();
    expect(entry.load).toHaveBeenCalledTimes(2);
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

  it('supports registerSourceKind(sourceIdMode: none), form subject/valid, save create & edit with empty source.id, and dropping form on CEL edit (R3, GAR 1-3)', async () => {
    registerSourceKind('demo-source', {
      label: 'Demo App',
      sourceIdMode: 'none',
      defaultTypes: ['demo.opened'],
    });
    expect(getRegisteredSourceKinds()).toHaveLength(1);

    registerFormAuthoring('demo-source', {
      label: 'Demo Form',
      render: ({ presetId, onChange }) => html`
        <div data-demo-form-pane>
          Preset: ${presetId ?? 'none'}
          <button
            type="button"
            data-demo-invalid-form
            @click=${() =>
              onChange({
                types: [],
                rules: [],
                form: { preset: presetId ?? 'default' },
                valid: false,
                errors: ['Select at least one event'],
              })}
          >
            Make Invalid
          </button>
          <button
            type="button"
            data-demo-apply-form
            @click=${() =>
              onChange({
                types: ['demo.opened'],
                rules: [{ dialect: 'cel', expr: 'event.attributes.kind == "bug"' }],
                form: { preset: presetId ?? 'default', label: 'bug' },
                subject: { mode: 'reuse', expr: 'event.subject', coalesceWindow: '45s' },
                suggestedActions: [{ type: 'message', agentId: 'ag-1', message: 'hi' }],
                valid: true,
              })}
          >
            Apply Form
          </button>
        </div>
      `,
    });

    expect(getFormAuthoring('demo-source')).toBeDefined();

    const existingDemoTrigger = {
      id: 'tr-demo-existing',
      projectId: 'proj-1',
      name: 'existing-demo',
      source: { kind: 'demo-source', id: '' },
      match: {
        types: ['demo.opened'],
        rules: [{ dialect: 'cel', expr: 'true' }],
        form: { preset: 'p1' },
      },
      action: { type: 'message', agentId: 'ag-1', message: 'hi' },
      subject: { mode: 'new' },
      status: 'active',
      authorizationRevision: 1,
      fireCount: 0,
      errorCount: 0,
      createdAt: '2026-11-20T10:00:00Z',
      updatedAt: '2026-11-20T10:00:00Z',
    };

    const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && url.endsWith('/triggers')) {
        return Promise.resolve(jsonResponse({ id: 'tr-new' }, 201));
      }
      if (init?.method === 'PATCH' && url.includes('/triggers/tr-demo-existing')) {
        return Promise.resolve(jsonResponse(existingDemoTrigger));
      }
      return Promise.resolve(jsonResponse({ triggers: [existingDemoTrigger], totalCount: 1 }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-trigger-list') as ScionTriggerList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    el.openCreateDialog('demo-source', 'preset-bug');
    await el.updateComplete;

    // Source ID input must be hidden for sourceIdMode === 'none'
    expect(el.shadowRoot?.querySelector('[data-trigger-source-id]')).toBeNull();

    const formPane = el.shadowRoot?.querySelector('[data-demo-form-pane]');
    expect(formPane).toBeTruthy();
    expect(formPane?.textContent).toContain('Preset: preset-bug');

    // Invalid form disables Save button and shows errors (GAR 2)
    (el.shadowRoot?.querySelector('[data-demo-invalid-form]') as HTMLButtonElement).click();
    await el.updateComplete;
    const submitBtn = el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLButtonElement;
    expect(submitBtn.hasAttribute('disabled')).toBe(true);
    expect(el.shadowRoot?.querySelector('[data-form-authoring-errors]')?.textContent).toContain(
      'Select at least one event'
    );

    // Valid form re-enables Save and applies subject (GAR 1, 2)
    (el.shadowRoot?.querySelector('[data-demo-apply-form]') as HTMLButtonElement).click();
    await el.updateComplete;
    expect(submitBtn.hasAttribute('disabled')).toBe(false);

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

    // Fill name ONLY (no sourceId!) and submit -> POST succeeds with source.id === '' and subject from form.
    const nameInput = el.shadowRoot?.querySelector('[data-trigger-name-input]') as HTMLInputElement;
    nameInput.value = 'demo-rule';
    nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;

    submitBtn.click();
    await new Promise((r) => setTimeout(r, 20));

    const postCall = fetchMock.mock.calls.find(
      ([url, init]) =>
        typeof url === 'string' && url.endsWith('/triggers') && init?.method === 'POST'
    );
    expect(postCall).toBeDefined();
    const sentBody = JSON.parse(String(postCall?.[1]?.body)) as {
      source: { kind: string; id: string };
      match: { form: unknown };
      subject: { mode: string; expr?: string; coalesce_window?: string };
    };
    expect(sentBody.source).toEqual({ kind: 'demo-source', id: '' });
    expect(sentBody.match.form).toBeNull();
    expect(sentBody.subject).toEqual({
      mode: 'reuse',
      dialect: 'cel',
      expr: 'event.subject',
      coalesce_window: '45s',
    });

    // Also verify EDIT on existing demo-source rule with empty source.id succeeds and applies form subject!
    (el.shadowRoot?.querySelector('[data-edit-trigger="tr-demo-existing"]') as HTMLElement).click();
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-demo-apply-form]') as HTMLButtonElement).click();
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));

    const patchCall = fetchMock.mock.calls.find(
      ([url, init]) =>
        typeof url === 'string' &&
        url.includes('/triggers/tr-demo-existing') &&
        init?.method === 'PATCH'
    );
    expect(patchCall).toBeDefined();
    const patchBody = JSON.parse(String(patchCall?.[1]?.body)) as {
      subject?: { mode: string; coalesce_window?: string };
    };
    expect(patchBody.subject?.mode).toBe('reuse');
    expect(patchBody.subject?.coalesce_window).toBe('45s');
  });

  it('ignores invalid form output when switching to CEL (O-C)', async () => {
    registerSourceKind('demo-source', {
      label: 'Demo App',
      sourceIdMode: 'none',
    });

    registerFormAuthoring('demo-source', {
      label: 'Demo Form',
      render: ({ onChange }) => html`
        <button
          type="button"
          data-emit-invalid-garbage
          @click=${() =>
            onChange({
              types: ['invalid.type'],
              rules: [{ dialect: 'cel', expr: 'invalid_expr' }],
              form: { broken: true },
              subject: { mode: 'skip', expr: 'bad' },
              valid: false,
              errors: ['Broken form state'],
            })}
        >
          Emit Invalid
        </button>
      `,
    });

    const existingDemoTrigger = {
      id: 'tr-oc',
      projectId: 'proj-1',
      name: 'oc-rule',
      source: { kind: 'demo-source', id: '' },
      match: {
        types: ['demo.opened'],
        rules: [{ dialect: 'cel', expr: 'true' }],
        form: { preset: 'original' },
      },
      action: { type: 'message', agentId: 'ag-1', message: 'hi' },
      subject: { mode: 'reuse', expr: 'event.subject', coalesce_window: '5m0s' },
      status: 'active',
      authorizationRevision: 3,
      fireCount: 0,
      errorCount: 0,
      createdAt: '2026-11-20T10:00:00Z',
      updatedAt: '2026-11-20T10:00:00Z',
    };

    const patchBodies: Array<Record<string, unknown>> = [];
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((_url: string, init?: RequestInit) => {
        if (init?.method === 'PATCH') {
          const rawBody = typeof init.body === 'string' ? init.body : '';
          patchBodies.push(JSON.parse(rawBody) as Record<string, unknown>);
          return Promise.resolve(jsonResponse(existingDemoTrigger));
        }
        return Promise.resolve(jsonResponse({ triggers: [existingDemoTrigger], totalCount: 1 }));
      })
    );

    const el = document.createElement('scion-trigger-list') as ScionTriggerList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-edit-trigger="tr-oc"]') as HTMLElement).click();
    await el.updateComplete;

    // O-C: emit invalid output, switch to CEL pane, rename, and save.
    (el.shadowRoot?.querySelector('[data-emit-invalid-garbage]') as HTMLButtonElement).click();
    await el.updateComplete;

    const modeGroup = el.shadowRoot?.querySelector('[data-authoring-mode]') as HTMLElement & {
      value: string;
    };
    modeGroup.value = 'cel';
    modeGroup.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await el.updateComplete;

    const nameInput = el.shadowRoot?.querySelector('[data-trigger-name-input]') as HTMLInputElement;
    nameInput.value = 'oc-rule-renamed';
    nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));

    // Must NOT contain invalid types/rules/form/subject — only the rename!
    expect(patchBodies[0]).toEqual({
      name: 'oc-rule-renamed',
      expectedRevision: 3,
    });
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

  it('disables form pane and preserves system_one rule when stored trigger has match.form plus non-admitted dialect (J16c)', async () => {
    registerSourceKind('demo-source', {
      label: 'Demo App',
      sourceIdMode: 'none',
    });

    const formRenderSpy = vi.fn(
      ({ onChange }) => html`
        <div data-demo-form-pane>
          <button
            type="button"
            data-demo-overwrite
            @click=${() =>
              onChange({
                types: ['demo.opened'],
                rules: [{ dialect: 'cel', expr: 'form_only == true' }],
                form: { preset: 'overwritten' },
                valid: true,
              })}
          >
            Overwrite
          </button>
        </div>
      `
    );
    registerFormAuthoring('demo-source', {
      label: 'Demo Form',
      render: formRenderSpy,
    });

    const systemOneRule = {
      dialect: 'system_one',
      expr: 'require_approval("sec-team")',
    };
    const existingTrigger = {
      id: 'tr-form-s1',
      projectId: 'proj-1',
      name: 'form-plus-s1',
      source: { kind: 'demo-source', id: '' },
      match: {
        types: ['demo.opened'],
        rules: [{ dialect: 'cel', expr: 'event.attributes.kind == "pr"' }, { ...systemOneRule }],
        form: { preset: 'pr-review' },
      },
      action: { type: 'message', agentId: 'ag-1', message: 'hi' },
      subject: { mode: 'new' },
      status: 'active',
      authorizationRevision: 2,
      fireCount: 0,
      errorCount: 0,
      createdAt: '2026-11-28T10:00:00Z',
      updatedAt: '2026-11-28T10:00:00Z',
    };

    const patchBodies: Array<Record<string, unknown>> = [];
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((_url: string, init?: RequestInit) => {
        if (init?.method === 'PATCH') {
          const rawBody = typeof init.body === 'string' ? init.body : '';
          patchBodies.push(JSON.parse(rawBody) as Record<string, unknown>);
          return Promise.resolve(jsonResponse(existingTrigger));
        }
        return Promise.resolve(jsonResponse({ triggers: [existingTrigger], totalCount: 1 }));
      })
    );

    const el = document.createElement('scion-trigger-list') as ScionTriggerList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-edit-trigger="tr-form-s1"]') as HTMLElement).click();
    await el.updateComplete;

    // Must open in the rule-list pane, never rendering the form pane.
    expect(el.shadowRoot?.querySelector('[data-demo-form-pane]')).toBeNull();
    expect(formRenderSpy).not.toHaveBeenCalled();
    expect(el.shadowRoot?.querySelector('scion-trigger-where-editor')).toBeTruthy();

    // Form mode radio button is disabled and notice is displayed.
    const formRadio = el.shadowRoot?.querySelector(
      '[data-authoring-mode] sl-radio-button[value="form"]'
    );
    expect(formRadio?.hasAttribute('disabled')).toBe(true);
    const disabledNotice = el.shadowRoot?.querySelector('[data-form-pane-disabled-notice]');
    expect(disabledNotice).toBeTruthy();
    expect(disabledNotice?.textContent?.replace(/\s+/g, ' ')).toContain(
      'this rule contains elements the form cannot represent'
    );

    // Attempting to switch authoring mode to 'form' is blocked.
    const modeGroup = el.shadowRoot?.querySelector('[data-authoring-mode]') as HTMLElement & {
      value: string;
    };
    modeGroup.value = 'form';
    modeGroup.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-demo-form-pane]')).toBeNull();
    expect(formRenderSpy).not.toHaveBeenCalled();

    // Defense-in-depth: even a direct form authoring output cannot overwrite non-cel rules.
    const internal = el as unknown as {
      handleFormAuthoringOutput: (out: {
        types: string[];
        rules: Array<{ dialect: string; expr: string }>;
        form: unknown;
        valid: boolean;
      }) => void;
    };
    internal.handleFormAuthoringOutput({
      types: ['demo.closed'],
      rules: [{ dialect: 'cel', expr: 'form_only == true' }],
      form: { preset: 'overwritten' },
      valid: true,
    });
    await el.updateComplete;

    // 1. Saving a rename omits match completely (preserving S and match.form).
    const nameInput = el.shadowRoot?.querySelector('[data-trigger-name-input]') as HTMLInputElement;
    nameInput.value = 'form-plus-s1-renamed';
    nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));

    expect(patchBodies[0]).toEqual({
      name: 'form-plus-s1-renamed',
      expectedRevision: 2,
    });

    // 2. Changing event types without editing any where row preserves BOTH rules (incl. S) and match.form.
    (el.shadowRoot?.querySelector('[data-edit-trigger="tr-form-s1"]') as HTMLElement).click();
    await el.updateComplete;
    const typeInput = el.shadowRoot?.querySelector('.chip-input-row sl-input') as HTMLInputElement;
    typeInput.value = 'demo.labeled';
    typeInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    (el.shadowRoot?.querySelector('.chip-input-row sl-button') as HTMLElement).click();
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));

    expect(patchBodies[1]?.['match']).toEqual({
      types: ['demo.opened', 'demo.labeled'],
      rules: [{ dialect: 'cel', expr: 'event.attributes.kind == "pr"' }, systemOneRule],
      form: { preset: 'pr-review' },
    });

    // 3. Editing CEL row 0 preserves S byte-identical in position 2 and drops match.form (S3 §9.2).
    (el.shadowRoot?.querySelector('[data-edit-trigger="tr-form-s1"]') as HTMLElement).click();
    await el.updateComplete;
    const whereEditor = el.shadowRoot?.querySelector(
      'scion-trigger-where-editor'
    ) as HTMLElement & { updateComplete: Promise<boolean> };
    await whereEditor.updateComplete;
    const expr0 = whereEditor.shadowRoot?.querySelector(
      '[data-where-expr="0"]'
    ) as HTMLTextAreaElement;
    expr0.value = 'event.attributes.kind == "issue"';
    expr0.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));

    const matchPatch = patchBodies[2]?.['match'] as {
      rules: Array<{ dialect: string; expr: string }>;
      form: unknown;
    };
    expect(matchPatch.rules).toEqual([
      { dialect: 'cel', expr: 'event.attributes.kind == "issue"' },
      systemOneRule,
    ]);
    expect(matchPatch.form).toBeNull();
  });
});
