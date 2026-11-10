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
 * Tests for the pending secrets in the user-scope secret list: one merged
 * list with state badges, the details of a pending secret, and the
 * Approve, Reject and Reveal requests.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

import { showToast } from '../../utils/toast.js';
import type { ScionSecretList } from './secret-list.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

const FAKE_VALUE = 'test-value-1';
const FAKE_VALUE_B64 = btoa(FAKE_VALUE);

function approvedSecret(key: string, extra: Record<string, unknown> = {}): unknown {
  return {
    id: `s-${key}`,
    key,
    type: 'environment',
    scope: 'user',
    scopeId: 'u-1',
    injectionMode: 'always',
    version: 3,
    created: '2026-11-01T10:00:00Z',
    updated: '2026-11-02T10:00:00Z',
    createdBy: 'u-1',
    updatedBy: 'agent:agent-0',
    ...extra,
  };
}

function pendingSecret(key: string, extra: Record<string, unknown> = {}): unknown {
  return {
    id: `p-${key}`,
    key,
    type: 'file',
    target: '/home/agent/.config/tool.json',
    scope: 'user',
    allowProgeny: true,
    valueLen: 12,
    agentId: 'agent-1',
    agentSlug: 'helper',
    projectId: 'proj-1',
    projectName: 'alpha',
    created: '2026-11-09T10:00:00Z',
    updated: '2026-11-09T11:00:00Z',
    expiresAt: '2026-11-16T10:00:00Z',
    replacesExisting: false,
    state: 'pending',
    overridesProjectSecrets: [
      { projectId: 'proj-2', projectName: 'beta' },
      { projectId: 'proj-3', projectName: 'gamma' },
    ],
    ...extra,
  };
}

const LIST = {
  scope: 'user',
  scopeId: 'u-1',
  secrets: [approvedSecret('A_APPROVED'), approvedSecret('TOKEN')],
  items: [
    { state: 'approved', key: 'A_APPROVED', secret: approvedSecret('A_APPROVED') },
    { state: 'approved', key: 'TOKEN', secret: approvedSecret('TOKEN') },
    {
      state: 'pending',
      key: 'TOKEN',
      pending: pendingSecret('TOKEN', {
        type: 'environment',
        target: 'TOKEN',
        allowProgeny: false,
        replacesExisting: true,
        overridesProjectSecrets: [],
      }),
    },
    { state: 'pending', key: 'Z_NEW', pending: pendingSecret('Z_NEW') },
  ],
};

interface Call {
  url: string;
  init: RequestInit | undefined;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

type Responder = (url: string, init?: RequestInit) => Response | undefined;

async function mount(respond: Responder = () => undefined): Promise<{ el: any; calls: Call[] }> {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init?: RequestInit) => {
      calls.push({ url, init });
      const r = respond(url, init);
      if (r) return Promise.resolve(r);
      if (url.startsWith('/api/v1/secrets')) return Promise.resolve(jsonResponse(LIST));
      return Promise.resolve(jsonResponse({}));
    })
  );
  const el = document.createElement('scion-secret-list') as InstanceType<typeof ScionSecretList>;
  el.setAttribute('scope', 'user');
  el.setAttribute('apiBasePath', '/api/v1');
  document.body.appendChild(el);
  await settle(el);
  return { el, calls };
}

async function settle(el: any): Promise<void> {
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 20));
  await el.updateComplete;
}

function rowKeys(el: any): string[] {
  return Array.from((el.shadowRoot as ShadowRoot).querySelectorAll('tbody td.key-cell')).map((td) =>
    (td.textContent ?? '').trim()
  );
}

function entry(el: any, key: string): any {
  return (el.entries as any[]).find((e) => e.key === key);
}

describe('scion-secret-list — pending user secrets', () => {
  beforeAll(async () => {
    await import('./secret-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    vi.mocked(showToast).mockClear();
  });

  it('requests the merged list with pending secrets', async () => {
    const { calls } = await mount();
    expect(calls[0]?.url).toBe('/api/v1/secrets?scope=user&includePending=true');
  });

  it('shows one entry per key, pending first, with state badges', async () => {
    const { el } = await mount();
    expect(rowKeys(el)).toEqual(['TOKEN', 'Z_NEW', 'A_APPROVED']);
    const badges = Array.from((el.shadowRoot as ShadowRoot).querySelectorAll('.state-badge')).map(
      (b) => (b.textContent ?? '').trim()
    );
    expect(badges).toEqual(['Approved, replacement pending', 'Pending', 'Approved']);
    expect((el.shadowRoot as ShadowRoot).textContent).toContain('expires');
  });

  it('shows the details of a pending secret, never its value', async () => {
    const { el } = await mount();
    const details = Array.from(
      (el.shadowRoot as ShadowRoot).querySelectorAll('.pending-details')
    ).map((d) => (d.textContent ?? '').replace(/\s+/g, ' '));
    expect(details).toHaveLength(2);
    const [token, fresh] = details as [string, string];

    expect(fresh).toContain('Name: Z_NEW');
    expect(fresh).toContain('Type: file');
    expect(fresh).toContain('Target: /home/agent/.config/tool.json');
    expect(fresh).toContain('Value length: 12 bytes');
    expect(fresh).toContain('Written by agent helper (agent-1) in project alpha (proj-1)');
    expect(fresh).toContain('replaced at');
    expect(fresh).toContain('expires at');
    expect(fresh).toContain('Delivered to your agents in all projects and to agents they create');
    expect(fresh).toContain('Overrides project secret Z_NEW in projects beta, gamma');
    expect(fresh).not.toContain('Replaces your existing secret');

    expect(token).toContain('Delivered to your agents in all projects');
    expect(token).not.toContain('agents they create');
    expect(token).toContain('Replaces your existing secret (last set by agent:agent-0,');
    expect(token).not.toContain('Overrides project secret');

    expect((el.shadowRoot as ShadowRoot).textContent).not.toContain(FAKE_VALUE);
  });

  it('approves with a same-origin JSON POST and refreshes', async () => {
    const { el, calls } = await mount((url) =>
      url.endsWith('/approve')
        ? jsonResponse({ id: 'p-Z_NEW', key: 'Z_NEW', scope: 'user', state: 'approved' })
        : undefined
    );
    el.openApproveDialog(entry(el, 'Z_NEW'));
    await settle(el);
    await el.handleApprove();
    await settle(el);

    const post = calls.find((c) => c.url.endsWith('/approve'));
    expect(post?.url).toBe('/api/v1/pending-secrets/p-Z_NEW/approve');
    expect(post?.init?.method).toBe('POST');
    expect(post?.init?.credentials).toBe('include');
    expect((post?.init?.headers as Record<string, string>)['Content-Type']).toBe(
      'application/json'
    );
    expect(JSON.parse(post?.init?.body as string)).toEqual({});
    expect(calls.filter((c) => c.url.startsWith('/api/v1/secrets')).length).toBe(2);
    expect(el.approveEntry).toBeNull();
  });

  it('sends allowProgeny false when the user unticks progeny access', async () => {
    const { el, calls } = await mount((url) =>
      url.endsWith('/approve') ? jsonResponse({ state: 'approved' }) : undefined
    );
    el.openApproveDialog(entry(el, 'Z_NEW'));
    await settle(el);
    const box = (el.shadowRoot as ShadowRoot).querySelector('sl-checkbox.approve-progeny') as any;
    expect(box).not.toBeNull();
    box.checked = false;
    box.dispatchEvent(new Event('sl-change'));
    await settle(el);
    expect(el.approveAllowProgeny).toBe(false);

    // Ticking it again is not possible: the box is disabled and stays off.
    expect(box.hasAttribute('disabled')).toBe(true);
    box.checked = true;
    box.dispatchEvent(new Event('sl-change'));
    await settle(el);
    expect(el.approveAllowProgeny).toBe(false);

    await el.handleApprove();
    const post = calls.find((c) => c.url.endsWith('/approve'));
    expect(JSON.parse(post?.init?.body as string)).toEqual({ allowProgeny: false });
  });

  it('offers no progeny box for a secret saved without progeny access', async () => {
    const { el } = await mount();
    el.openApproveDialog(entry(el, 'TOKEN'));
    await settle(el);
    expect((el.shadowRoot as ShadowRoot).querySelector('sl-checkbox.approve-progeny')).toBeNull();
  });

  it('explains a 409 when the existing secret changed', async () => {
    const { el } = await mount((url) =>
      url.endsWith('/approve')
        ? jsonResponse(
            { error: { code: 'secret_pending_existing_changed', message: 'conflict' } },
            409
          )
        : undefined
    );
    el.openApproveDialog(entry(el, 'TOKEN'));
    await el.handleApprove();
    await settle(el);
    expect(el.approveError).toBe(
      'Your existing secret changed since the agent saved this one. ' +
        'Reject this pending secret and have the agent save it again.'
    );
    expect(el.approveEntry).not.toBeNull();
  });

  it('refreshes on a 404 from approve', async () => {
    const { el, calls } = await mount((url) =>
      url.endsWith('/approve')
        ? jsonResponse({ error: { code: 'not_found', message: 'gone' } }, 404)
        : undefined
    );
    el.openApproveDialog(entry(el, 'Z_NEW'));
    await el.handleApprove();
    await settle(el);
    expect(el.approveEntry).toBeNull();
    expect(showToast).toHaveBeenCalledWith(expect.stringContaining('expired'), 'warning');
    expect(calls.filter((c) => c.url.startsWith('/api/v1/secrets')).length).toBe(2);
  });

  it('rejects with a POST without a body', async () => {
    const { el, calls } = await mount((url) =>
      url.endsWith('/reject') ? jsonResponse({ state: 'rejected' }) : undefined
    );
    await el.handleReject(entry(el, 'Z_NEW').pending, new MouseEvent('click', { altKey: true }));
    const post = calls.find((c) => c.url.endsWith('/reject'));
    expect(post?.url).toBe('/api/v1/pending-secrets/p-Z_NEW/reject');
    expect(post?.init?.method).toBe('POST');
    expect(post?.init?.body).toBeUndefined();
    expect(calls.filter((c) => c.url.startsWith('/api/v1/secrets')).length).toBe(2);
  });

  it('reveals the decoded value on request and clears it when closed', async () => {
    const log = vi.spyOn(console, 'log');
    const info = vi.spyOn(console, 'info');
    const { el, calls } = await mount((url) =>
      url.endsWith('/reveal')
        ? jsonResponse({
            id: 'p-Z_NEW',
            key: 'Z_NEW',
            type: 'file',
            target: '/x',
            value: FAKE_VALUE_B64,
            encoding: 'base64',
          })
        : undefined
    );
    expect(calls.some((c) => c.url.endsWith('/reveal'))).toBe(false);

    await el.handleReveal(entry(el, 'Z_NEW').pending);
    await settle(el);
    const post = calls.find((c) => c.url.endsWith('/reveal'));
    expect(post?.url).toBe('/api/v1/pending-secrets/p-Z_NEW/reveal');
    expect(post?.init?.method).toBe('POST');
    expect(post?.init?.body).toBeUndefined();

    const pre = (el.shadowRoot as ShadowRoot).querySelector('.reveal-value');
    expect(pre?.textContent).toBe(FAKE_VALUE);

    el.closeReveal();
    await settle(el);
    expect((el.shadowRoot as ShadowRoot).querySelector('.reveal-value')).toBeNull();
    expect((el.shadowRoot as ShadowRoot).textContent).not.toContain(FAKE_VALUE);
    expect(el.revealText).toBeNull();
    for (const spy of [log, info]) {
      for (const args of spy.mock.calls) {
        expect(JSON.stringify(args)).not.toContain(FAKE_VALUE);
      }
    }
  });

  it('shows a value that is not text base64-encoded', async () => {
    const bin = btoa('\xff\xfe\x00');
    const { el } = await mount((url) =>
      url.endsWith('/reveal') ? jsonResponse({ value: bin, encoding: 'base64' }) : undefined
    );
    await el.handleReveal(entry(el, 'Z_NEW').pending);
    await settle(el);
    expect((el.shadowRoot as ShadowRoot).querySelector('.reveal-value')?.textContent).toBe(bin);
    expect((el.shadowRoot as ShadowRoot).textContent).toContain('not text');
  });

  it('refreshes on a 404 from reveal', async () => {
    const { el, calls } = await mount((url) =>
      url.endsWith('/reveal') ? jsonResponse({ error: { code: 'not_found' } }, 404) : undefined
    );
    await el.handleReveal(entry(el, 'Z_NEW').pending);
    await settle(el);
    expect(el.revealPending).toBeNull();
    expect(calls.filter((c) => c.url.startsWith('/api/v1/secrets')).length).toBe(2);
  });

  it('lists approved secrets alone when the response has no items', async () => {
    const { el } = await mount((url) =>
      url.startsWith('/api/v1/secrets')
        ? jsonResponse({ secrets: [approvedSecret('ONLY')] })
        : undefined
    );
    expect(rowKeys(el)).toEqual(['ONLY']);
  });
});
