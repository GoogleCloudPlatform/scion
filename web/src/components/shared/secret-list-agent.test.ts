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
import type { ScionSecretList } from './secret-list.js';
import type { Secret } from '../../shared/types.js';

interface RecordedCall {
  url: string;
  method: string;
  body?: Record<string, unknown>;
}

const confirmSpy = vi.fn(() => Promise.resolve(true));
vi.mock('./confirm-dialog.js', () => ({
  showConfirm: (...args: unknown[]) => confirmSpy(...args),
}));

const toastSpy = vi.fn();
vi.mock('../../utils/toast.js', () => ({
  showToast: (...args: unknown[]) => toastSpy(...args),
}));

const sampleAgentSecret: Secret = {
  id: 'sec-1',
  key: 'API_KEY',
  type: 'environment',
  target: 'API_KEY',
  scope: 'agent',
  scopeId: 'agent-uuid-1',
  description: 'Agent API key',
  injectionMode: 'as_needed',
  version: 2,
  created: '2026-11-20T00:00:00Z',
  updated: '2026-11-21T00:00:00Z',
};

function installFetch(
  secrets: Secret[] = [sampleAgentSecret],
  override?: (url: string, init?: RequestInit) => Response | null
) {
  const calls: RecordedCall[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      const method = init?.method ?? 'GET';
      const body =
        typeof init?.body === 'string'
          ? (JSON.parse(init.body) as Record<string, unknown>)
          : undefined;
      calls.push({ url, method, body });

      const custom = override?.(url, init);
      if (custom) return Promise.resolve(custom);

      if (method === 'DELETE') {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (method === 'PUT' || method === 'PATCH') {
        return Promise.resolve(
          new Response(JSON.stringify({ secret: sampleAgentSecret, created: method === 'PUT' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify({ secrets }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    })
  );
  return calls;
}

async function mountList(attrs: {
  scope: 'agent' | 'user' | 'project' | 'hub';
  scopeId?: string;
  apiBasePath?: string;
  compact?: boolean;
  readonly?: boolean;
}): Promise<ScionSecretList> {
  const el = document.createElement('scion-secret-list') as ScionSecretList;
  el.scope = attrs.scope;
  if (attrs.scopeId !== undefined) el.scopeId = attrs.scopeId;
  if (attrs.apiBasePath !== undefined) el.apiBasePath = attrs.apiBasePath;
  if (attrs.compact) el.compact = true;
  if (attrs.readonly) el.readonly = true;
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 30));
  await el.updateComplete;
  return el;
}

describe('scion-secret-list — agent scope and existing scopes', () => {
  beforeAll(async () => {
    await import('./secret-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    confirmSpy.mockClear();
    toastSpy.mockClear();
  });

  it('sends scope=agent&scopeId= on list, set, update, patch, and delete', async () => {
    const calls = installFetch();
    const el = await mountList({
      scope: 'agent',
      scopeId: 'agent-uuid-1',
      compact: true,
    });

    // 1. List
    expect(calls[0]).toMatchObject({
      method: 'GET',
      url: '/api/v1/secrets?scope=agent&scopeId=agent-uuid-1',
    });

    // 2. Create (PUT)
    const priv = el as unknown as {
      dialogMode: 'create' | 'update' | 'edit-settings';
      dialogKey: string;
      dialogValue: string;
      dialogType: string;
      dialogInjectionMode: string;
      handleSave: (e: Event) => Promise<void>;
      handleDelete: (s: Secret, e?: MouseEvent) => Promise<void>;
    };
    priv.dialogMode = 'create';
    priv.dialogKey = 'NEW_SECRET';
    priv.dialogValue = 'top-secret';
    priv.dialogType = 'environment';
    priv.dialogInjectionMode = 'as_needed';
    await priv.handleSave(new Event('submit'));

    const putCall = calls.find((c) => c.method === 'PUT' && c.url.includes('NEW_SECRET'));
    expect(putCall).toBeDefined();
    expect(putCall!.url).toBe('/api/v1/secrets/NEW_SECRET?scope=agent&scopeId=agent-uuid-1');
    expect(putCall!.body).toMatchObject({
      scope: 'agent',
      scopeId: 'agent-uuid-1',
      type: 'environment',
      injectionMode: 'as_needed',
    });
    expect(putCall!.body!.allowProgeny).toBeUndefined();

    // 3. Edit settings (PATCH)
    priv.dialogMode = 'edit-settings';
    priv.dialogKey = 'API_KEY';
    priv.dialogInjectionMode = 'always';
    await priv.handleSave(new Event('submit'));

    const patchCall = calls.find((c) => c.method === 'PATCH');
    expect(patchCall).toBeDefined();
    expect(patchCall!.url).toBe('/api/v1/secrets/API_KEY?scope=agent&scopeId=agent-uuid-1');
    expect(patchCall!.body!.allowProgeny).toBeUndefined();

    // 4. Update value (PUT)
    priv.dialogMode = 'update';
    priv.dialogKey = 'API_KEY';
    priv.dialogValue = 'rotated-secret';
    await priv.handleSave(new Event('submit'));

    const updateCall = calls.find((c) => c.method === 'PUT' && c.url.includes('API_KEY'));
    expect(updateCall).toBeDefined();
    expect(updateCall!.url).toBe('/api/v1/secrets/API_KEY?scope=agent&scopeId=agent-uuid-1');
    expect(updateCall!.body).toMatchObject({
      scope: 'agent',
      scopeId: 'agent-uuid-1',
    });
    expect(updateCall!.body!.allowProgeny).toBeUndefined();

    // 5. Delete (DELETE)
    await priv.handleDelete(sampleAgentSecret);
    const deleteCall = calls.find((c) => c.method === 'DELETE');
    expect(deleteCall).toBeDefined();
    expect(deleteCall!.url).toBe('/api/v1/secrets/API_KEY?scope=agent&scopeId=agent-uuid-1');
  });

  it('shows no reveal, no pending state, no progeny control, and accurate agent-scope copy', async () => {
    installFetch();
    const el = await mountList({
      scope: 'agent',
      scopeId: 'agent-uuid-1',
      compact: true,
    });

    const text = el.shadowRoot!.textContent ?? '';
    expect(text).toContain('readable only by this agent');
    expect(text).not.toContain('all agents on this hub');
    expect(text).not.toContain('agents in this project');

    const headers = Array.from(el.shadowRoot!.querySelectorAll('thead th')).map((th) =>
      th.textContent?.trim()
    );
    expect(headers).not.toContain('State');
    expect(headers).not.toContain('Progeny');
    expect(text).not.toContain('Reveal');
    expect(text).not.toContain('Allow agent progeny');
    expect(el.shadowRoot!.querySelector('sl-switch')).toBeNull();
  });

  it('hides edit/create/delete affordances when readonly is true', async () => {
    installFetch();
    const el = await mountList({
      scope: 'agent',
      scopeId: 'agent-uuid-1',
      compact: true,
      readonly: true,
    });

    const buttons = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).map((b) =>
      b.textContent?.trim()
    );
    expect(buttons).not.toContain('Add Secret');
    expect(el.shadowRoot!.querySelectorAll('sl-icon-button')).toHaveLength(0);
  });

  it('surfaces 403 errors cleanly on write when not marked readonly', async () => {
    installFetch([sampleAgentSecret], (_url, init) => {
      if (init?.method === 'PUT') {
        return new Response(
          JSON.stringify({ error: { message: 'Forbidden: agent.update required' } }),
          { status: 403, headers: { 'Content-Type': 'application/json' } }
        );
      }
      return null;
    });
    const el = await mountList({
      scope: 'agent',
      scopeId: 'agent-uuid-1',
      compact: true,
    });

    const priv = el as unknown as {
      dialogMode: 'create';
      dialogKey: string;
      dialogValue: string;
      dialogError: string | null;
      handleSave: (e: Event) => Promise<void>;
    };
    priv.dialogMode = 'create';
    priv.dialogKey = 'DENIED_KEY';
    priv.dialogValue = 'secret';
    await priv.handleSave(new Event('submit'));
    await el.updateComplete;

    expect(priv.dialogError).toContain('Forbidden');
  });

  it('keeps user, project, and hub scope URLs and copy unchanged', async () => {
    // User scope
    const userCalls = installFetch([]);
    const userEl = await mountList({ scope: 'user' });
    expect(userCalls[0].url).toBe('/api/v1/secrets?scope=user&includePending=true');
    userEl.remove();

    // Project scope
    const projectCalls = installFetch([]);
    const projEl = await mountList({
      scope: 'project',
      scopeId: 'proj-1',
      apiBasePath: '/api/v1/projects/proj-1',
      compact: true,
    });
    expect(projectCalls[0].url).toBe('/api/v1/projects/proj-1/secrets');
    expect(projEl.shadowRoot!.textContent).toContain('agents in this project');
    projEl.remove();

    // Hub scope
    const hubCalls = installFetch([]);
    const hubEl = await mountList({ scope: 'hub', compact: true });
    expect(hubCalls[0].url).toBe('/api/v1/secrets?scope=hub');
    expect(hubEl.shadowRoot!.textContent).toContain('all agents on this hub');
    hubEl.remove();
  });
});
