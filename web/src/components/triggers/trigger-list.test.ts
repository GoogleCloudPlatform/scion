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

import { describe, it, expect, afterEach, vi } from 'vitest';
import './trigger-list.js';
import type { ScionTriggerList } from './trigger-list.js';
import type { TriggerDefinition } from './types.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const execTrigger: TriggerDefinition = {
  id: 'tr-exec',
  projectId: 'proj-1',
  name: 'exec-rule',
  source: { kind: 'webhook', id: 'ep-1' },
  match: { types: ['webhook.received'], rules: [{ dialect: 'cel', expr: 'true' }] },
  action: {
    type: 'exec',
    command: ['bash', '-c', 'SECRET_EXEC_COMMAND'],
    commandRedacted: true,
    argc: 3,
    commandSha256: 'deadbeef1234567890',
  },
  subject: { mode: 'new' },
  status: 'active',
  authorizationRevision: 2,
  fireCount: 5,
  errorCount: 1,
  lastFireStatus: 'fired',
  lastFireAt: '2026-11-20T10:05:00Z',
  createdAt: '2026-11-20T10:00:00Z',
  updatedAt: '2026-11-20T10:00:00Z',
};

describe('scion-trigger-list', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('renders exec rule with redacted summary (never unredacted argv) and expands details', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse({ triggers: [execTrigger], totalCount: 1 }))
    );

    const el = document.createElement('scion-trigger-list') as ScionTriggerList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    const summary = el.shadowRoot?.querySelector('[data-trigger-action-summary="tr-exec"]');
    expect(summary?.textContent).toContain('3 args');
    expect(summary?.textContent).toContain('redacted');
    expect(el.shadowRoot?.textContent).not.toContain('SECRET_EXEC_COMMAND');

    (el.shadowRoot?.querySelector('[data-expand-trigger="tr-exec"]') as HTMLElement).click();
    await el.updateComplete;

    const detail = el.shadowRoot?.querySelector('[data-trigger-detail="tr-exec"]');
    expect(detail).toBeTruthy();
    expect(detail?.textContent).toContain('Rev 2');
  });

  it('previews render against latest event and highlights untrusted variables', async () => {
    const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
      if (url.includes('/trigger-events?')) {
        return Promise.resolve(jsonResponse({ events: [{ id: 'ev-latest' }], totalCount: 1 }));
      }
      if (init?.method === 'POST' && url.includes('/triggers/tr-exec/render?eventId=ev-latest')) {
        return Promise.resolve(
          jsonResponse({
            slots: {
              message: {
                rendered: 'Deploy PR: untrusted-title',
                used: [
                  { path: 'event.type', class: 'trusted' },
                  { path: 'event.attributes.title', class: 'untrusted' },
                ],
              },
            },
            errors: [],
            notes: ['Preview is authority-free'],
          })
        );
      }
      return Promise.resolve(jsonResponse({ triggers: [execTrigger], totalCount: 1 }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-trigger-list') as ScionTriggerList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-preview-trigger="tr-exec"]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 30));
    await el.updateComplete;

    const slot = el.shadowRoot?.querySelector('[data-preview-slot="message"]');
    expect(slot?.textContent).toContain('Deploy PR: untrusted-title');
    const untrustedBadge = slot?.querySelector('[data-var-class="untrusted"]');
    expect(untrustedBadge).toBeTruthy();
    expect(untrustedBadge?.getAttribute('variant')).toBe('warning');
  });

  it('surfaces per-row 400 invalid_rule compile errors onto where-editor', async () => {
    const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && url.endsWith('/triggers')) {
        return Promise.resolve(
          jsonResponse(
            {
              error: {
                code: 'invalid_rule',
                message: 'invalid match rule [element 1]',
                details: {
                  field: 'match',
                  elements: [
                    {
                      element: 1,
                      category: 'syntax',
                      message: 'missing operand',
                      line: 1,
                      column: 6,
                    },
                  ],
                },
              },
            },
            400
          )
        );
      }
      return Promise.resolve(jsonResponse({ triggers: [], totalCount: 0 }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-trigger-list') as ScionTriggerList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    el.openCreateDialog('webhook');
    await el.updateComplete;

    const nameInput = el.shadowRoot?.querySelector('[data-trigger-name-input]') as HTMLInputElement;
    nameInput.value = 'bad-rule';
    nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));

    const srcInput = el.shadowRoot?.querySelector('[data-trigger-source-id]') as HTMLInputElement;
    srcInput.value = 'ep-1';
    srcInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    const whereEl = el.shadowRoot?.querySelector('scion-trigger-where-editor') as HTMLElement & {
      rowErrors: Record<number, { message: string; line?: number; column?: number }>;
    };
    expect(whereEl?.rowErrors[0]?.message).toBe('missing operand');
    expect(whereEl?.rowErrors[0]?.line).toBe(1);
    expect(whereEl?.rowErrors[0]?.column).toBe(6);
  });

  it('sends only changed fields on edit PATCH so rename/description never re-attributes authority (R1, O2)', async () => {
    const existing: TriggerDefinition = {
      id: 'tr-agent',
      projectId: 'proj-1',
      name: 'agent-rule',
      description: 'orig desc',
      source: { kind: 'webhook', id: 'ep-1' },
      match: {
        types: ['webhook.received'],
        rules: [{ dialect: 'cel', expr: 'event.attributes.env == "prod"' }],
      },
      action: { type: 'message', agentId: 'ag-1', message: 'ping' },
      subject: { mode: 'new' },
      maxFiresPerMinute: 30,
      status: 'active',
      authorizationRevision: 4,
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
          return Promise.resolve(jsonResponse(existing));
        }
        return Promise.resolve(jsonResponse({ triggers: [existing], totalCount: 1 }));
      })
    );

    const el = document.createElement('scion-trigger-list') as ScionTriggerList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    // 1. Rename-only edit: must send ONLY { name, expectedRevision }.
    (el.shadowRoot?.querySelector('[data-edit-trigger="tr-agent"]') as HTMLElement).click();
    await el.updateComplete;
    const nameInput = el.shadowRoot?.querySelector('[data-trigger-name-input]') as HTMLInputElement;
    nameInput.value = 'agent-rule-renamed';
    nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));

    expect(patchBodies[0]).toEqual({
      name: 'agent-rule-renamed',
      expectedRevision: 4,
    });

    // 2. Description-only edit: must NOT send source, match, action, or subject.
    (el.shadowRoot?.querySelector('[data-edit-trigger="tr-agent"]') as HTMLElement).click();
    await el.updateComplete;
    const descInput = el.shadowRoot?.querySelector(
      '[data-trigger-description-input]'
    ) as HTMLInputElement;
    descInput.value = 'updated desc';
    descInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));

    expect(patchBodies[1]).toEqual({
      description: 'updated desc',
      expectedRevision: 4,
    });

    // 3. Edit one CEL row + clear maxFiresPerMinute: sends match and maxFiresPerMinute: 0, NOT action/source/subject.
    (el.shadowRoot?.querySelector('[data-edit-trigger="tr-agent"]') as HTMLElement).click();
    await el.updateComplete;
    const whereEditor = el.shadowRoot?.querySelector('scion-trigger-where-editor');
    whereEditor?.dispatchEvent(
      new CustomEvent('rules-changed', {
        detail: { rules: [{ dialect: 'cel', expr: 'event.attributes.env == "staging"' }] },
        bubbles: true,
        composed: true,
      })
    );
    const fpmInput = el.shadowRoot?.querySelector('[data-trigger-max-fpm]') as HTMLInputElement;
    fpmInput.value = '';
    fpmInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));

    expect(Object.keys(patchBodies[2]).sort()).toEqual([
      'expectedRevision',
      'match',
      'maxFiresPerMinute',
    ]);
    expect(patchBodies[2]['maxFiresPerMinute']).toBe(0);
    expect(patchBodies[2]['action']).toBeUndefined();
    expect(patchBodies[2]['source']).toBeUndefined();
  });

  it('omits untrustedVars on create and edit so hub derives them (ptone/scion#9776)', async () => {
    const existing: TriggerDefinition = {
      id: 'tr-msg-untrusted',
      projectId: 'proj-1',
      name: 'ci-alert',
      source: { kind: 'webhook', id: 'ep-1' },
      match: { types: ['webhook.received'], rules: [] },
      action: {
        type: 'message',
        agentId: 'ag-1',
        message: 'Old ${event.payload.old}',
        untrustedVars: ['event.payload.old'],
      },
      subject: { mode: 'new' },
      status: 'active',
      authorizationRevision: 1,
      fireCount: 0,
      errorCount: 0,
      createdAt: '2026-11-28T05:00:00Z',
      updatedAt: '2026-11-28T05:00:00Z',
    };

    const requests: Array<{ method: string; body: Record<string, unknown> }> = [];
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((_url: string, init?: RequestInit) => {
        if (init?.method === 'POST' || init?.method === 'PATCH') {
          const rawBody = typeof init.body === 'string' ? init.body : '';
          requests.push({
            method: init.method,
            body: JSON.parse(rawBody) as Record<string, unknown>,
          });
          return Promise.resolve(jsonResponse(existing, init.method === 'POST' ? 201 : 200));
        }
        return Promise.resolve(jsonResponse({ triggers: [existing], totalCount: 1 }));
      })
    );

    const el = document.createElement('scion-trigger-list') as ScionTriggerList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    // 1. Create multi-action trigger with message (${event.payload.commit}) + dispatch_agent:
    //    must send actions[] (not scalar action) and omit untrustedVars on every action.
    el.openCreateDialog('webhook');
    await el.updateComplete;
    const nameInput = el.shadowRoot?.querySelector('[data-trigger-name-input]') as HTMLInputElement;
    nameInput.value = 'web-msg-commit';
    nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    const srcInput = el.shadowRoot?.querySelector('[data-trigger-source-id]') as HTMLInputElement;
    srcInput.value = 'ep-1';
    srcInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    const actionEditor = el.shadowRoot?.querySelector('scion-trigger-action-editor');
    actionEditor?.dispatchEvent(
      new CustomEvent('actions-changed', {
        detail: {
          actions: [
            {
              type: 'message',
              agentId: 'ag-1',
              message: 'CI at ${event.payload.commit}',
              offline: 'skip',
            },
            {
              type: 'dispatch_agent',
              template: 'developer',
              task: 'Investigate ${event.attributes.repo}',
            },
          ],
        },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));

    expect(requests[0]?.method).toBe('POST');
    expect(requests[0]?.body['action']).toBeUndefined();
    const createdActions = requests[0]?.body['actions'] as Array<Record<string, unknown>>;
    expect(createdActions).toHaveLength(2);
    expect(createdActions[0]['message']).toBe('CI at ${event.payload.commit}');
    expect('untrustedVars' in createdActions[0]).toBe(false);
    expect(createdActions[1]['task']).toBe('Investigate ${event.attributes.repo}');
    expect('untrustedVars' in createdActions[1]).toBe(false);

    // 2. Edit legacy single-action message trigger with new placeholder:
    //    round-trips via actions[] and omits stored untrustedVars so hub re-derives.
    (el.shadowRoot?.querySelector('[data-edit-trigger="tr-msg-untrusted"]') as HTMLElement).click();
    await el.updateComplete;
    const editActionEditor = el.shadowRoot?.querySelector('scion-trigger-action-editor');
    editActionEditor?.dispatchEvent(
      new CustomEvent('actions-changed', {
        detail: {
          actions: [
            {
              type: 'message',
              agentId: 'ag-1',
              message: 'CI at ${event.payload.commit}',
              untrustedVars: ['event.payload.old'],
            },
          ],
        },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));

    expect(requests[1]?.method).toBe('PATCH');
    expect(requests[1]?.body['action']).toBeUndefined();
    const patchedActions = requests[1]?.body['actions'] as Array<Record<string, unknown>>;
    expect(patchedActions).toHaveLength(1);
    expect(patchedActions[0]['message']).toBe('CI at ${event.payload.commit}');
    expect('untrustedVars' in patchedActions[0]).toBe(false);
  });

  it('uses canonical deep equality for actions, rules, form, durations, and untrimmed descriptions on rename (O-A, Nit)', async () => {
    const existing: TriggerDefinition = {
      id: 'tr-canon',
      projectId: 'proj-1',
      name: 'canon-rule',
      description: '  has surrounding spaces  ',
      source: { kind: 'webhook', id: 'ep-1' },
      match: {
        types: ['webhook.received'],
        rules: [{ dialect: 'cel', expr: 'true' }],
        form: { b: 2, a: 1 },
      },
      action: { type: 'message', agentId: 'ag-1', message: 'hi' },
      subject: { mode: 'reuse', expr: 'event.subject', coalesce_window: '5m0s' },
      status: 'active',
      authorizationRevision: 7,
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
          return Promise.resolve(jsonResponse(existing));
        }
        return Promise.resolve(jsonResponse({ triggers: [existing], totalCount: 1 }));
      })
    );

    const el = document.createElement('scion-trigger-list') as ScionTriggerList;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-edit-trigger="tr-canon"]') as HTMLElement).click();
    await el.updateComplete;

    // Re-emit action with reordered keys and empty/false omitempty defaults (e.g. same-agent re-pick).
    const actionEditor = el.shadowRoot?.querySelector('scion-trigger-action-editor');
    actionEditor?.dispatchEvent(
      new CustomEvent('actions-changed', {
        detail: {
          actions: [
            {
              message: 'hi',
              agentId: 'ag-1',
              agentName: '',
              wake: false,
              interrupt: false,
              type: 'message',
            },
          ],
        },
        bubbles: true,
        composed: true,
      })
    );

    // Re-emit rules with reordered keys, form with reordered keys, and equivalent duration '5m' vs '5m0s'.
    const internal = el as unknown as {
      formRules: Array<{ expr: string; dialect: string }>;
      formMatchForm: Record<string, unknown>;
      formCoalesceWindow: string;
    };
    internal.formRules = [{ expr: 'true', dialect: 'cel' }];
    internal.formMatchForm = { a: 1, b: 2, ignored: undefined };
    internal.formCoalesceWindow = '5m';

    // Rename only (leave untrimmed stored description untouched).
    const nameInput = el.shadowRoot?.querySelector('[data-trigger-name-input]') as HTMLInputElement;
    nameInput.value = 'canon-rule-renamed';
    nameInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-submit-trigger]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 20));

    expect(patchBodies[0]).toEqual({
      name: 'canon-rule-renamed',
      expectedRevision: 7,
    });
  });
});
