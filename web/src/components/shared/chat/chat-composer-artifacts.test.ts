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
 * Tests for "Attach artifact" in the composer and its picker
 * (ptone/scion#3224): the paperclip menu, pending artifact chips, the send
 * payload, and the picker's list, search, filters and limit.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import '@shoelace-style/shoelace/dist/components/textarea/textarea.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

const apiFetch = vi.fn();
vi.mock('../../../client/api.js', () => ({
  apiFetch: (...args: unknown[]) => apiFetch(...args),
  extractApiError: (_res: Response, fallback: string) => Promise.resolve(fallback),
}));
vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

await import('./chat-composer.js');
await import('./artifact-picker.js');

const A = '5f1c2d3e-0000-4000-8000-0000000000aa';
const B = '5f1c2d3e-0000-4000-8000-0000000000bb';

function item(id: string, title: string, scopeRef = 'proj-1', currentSeq = 1) {
  return {
    id,
    ref: `scion://artifact/${id}`,
    scopeKind: 'project',
    scopeRef,
    ownerKind: 'user',
    ownerRef: 'u1',
    title,
    currentSeq,
    createdAt: '2026-10-07T10:00:00Z',
    updatedAt: '2026-10-07T10:00:00Z',
    reviewPending: false,
  };
}

function listResponse(items: unknown[], nextCursor?: string) {
  return new Response(JSON.stringify({ artifacts: items, ...(nextCursor ? { nextCursor } : {}) }), {
    status: 200,
  });
}

async function settle(el: any): Promise<void> {
  for (let i = 0; i < 8; i++) {
    await Promise.resolve();
    await el.updateComplete;
  }
}

describe('composer — attach artifact', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
    window.__SCION_FEATURES__ = { 'hub.artifacts': true };
    apiFetch.mockReset();
    apiFetch.mockImplementation(() => Promise.resolve(listResponse([])));
  });
  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
  });

  async function mount(): Promise<any> {
    const el = document.createElement('scion-chat-composer') as any;
    el.conversationMode = true;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  it('turns the paperclip into a menu with Upload file and Attach artifact', async () => {
    const el = await mount();
    const items = [...el.shadowRoot.querySelectorAll('.attach-menu sl-menu-item')].map(
      (i: Element) => ({
        value: i.getAttribute('value'),
        icon: i.querySelector('sl-icon')?.getAttribute('name'),
        label: i.textContent?.trim(),
      })
    );
    expect(items).toEqual([
      { value: 'file', icon: 'upload', label: 'Upload file' },
      { value: 'artifact', icon: 'file-earmark-richtext', label: 'Attach artifact…' },
    ]);
  });

  it('keeps the plain paperclip while the experiment is off', async () => {
    window.__SCION_FEATURES__ = { 'hub.artifacts': false };
    const el = await mount();
    expect(el.shadowRoot.querySelector('.attach-menu')).toBeNull();
    expect(el.shadowRoot.querySelector('sl-icon-button.attach-btn')).toBeTruthy();
  });

  it('opens the picker from the menu', async () => {
    const el = await mount();
    el.handleAttachMenuSelect(
      new CustomEvent('sl-select', { detail: { item: { value: 'artifact' } } })
    );
    await settle(el);
    const picker = el.shadowRoot.querySelector('scion-artifact-picker');
    expect(picker.open).toBe(true);
    expect(picker.projectId).toBe('proj-1');
    expect(picker.remaining).toBe(10);
  });

  it('shows picked artifacts as removable chips and sends their refs', async () => {
    const el = await mount();
    el.handleArtifactsPicked(
      new CustomEvent('artifact-picker-select', {
        detail: {
          artifacts: [
            { id: A, ref: `scion://artifact/${A}`, title: 'Design notes', version: 3 },
            { id: B, ref: `scion://artifact/${B}`, title: 'Funnel', version: 2 },
          ],
        },
      })
    );
    await el.updateComplete;
    const chips = [...el.shadowRoot.querySelectorAll('.pending-artifact')];
    expect(chips.map((c: Element) => c.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      'Design notes · v3',
      'Funnel · v2',
    ]);

    (chips[1].querySelector('.remove-artifact') as HTMLElement).click();
    await el.updateComplete;
    expect(el.shadowRoot.querySelectorAll('.pending-artifact')).toHaveLength(1);

    // Artifacts alone are sendable; the refs ride in the send detail.
    const sent: any[] = [];
    el.addEventListener('chat-send', (e: CustomEvent) => sent.push(e.detail));
    el.handleSend();
    expect(sent).toHaveLength(1);
    expect(sent[0].text).toBe('');
    expect(sent[0].artifactRefs).toEqual([`scion://artifact/${A}`]);
    await el.updateComplete;
    expect(el.shadowRoot.querySelectorAll('.pending-artifact')).toHaveLength(0);

    // A failed send gives them back.
    sent[0].onError('boom');
    await el.updateComplete;
    expect(el.shadowRoot.querySelectorAll('.pending-artifact')).toHaveLength(1);
  });

  it('sends no artifactRefs field when nothing was picked', async () => {
    const el = await mount();
    el.text = 'hello';
    const sent: any[] = [];
    el.addEventListener('chat-send', (e: CustomEvent) => sent.push(e.detail));
    el.handleSend();
    expect(sent[0]).not.toHaveProperty('artifactRefs');
  });
});

function lastListCall(): string | undefined {
  return apiFetch.mock.calls
    .map((c) => c[0] as string)
    .filter((u) => u.startsWith('/api/v1/artifacts'))
    .at(-1);
}

describe('scion-artifact-picker', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
    apiFetch.mockReset();
  });
  afterEach(() => {
    vi.useRealTimers();
    document.body.innerHTML = '';
  });

  async function openPicker(props: Record<string, unknown> = {}): Promise<any> {
    const el = document.createElement('scion-artifact-picker') as any;
    el.projectId = 'proj-1';
    Object.assign(el, props);
    document.body.appendChild(el);
    el.open = true;
    await settle(el);
    return el;
  }

  function titles(el: any): string[] {
    return [...el.shadowRoot.querySelectorAll('tbody td.title')].map(
      (t: Element) => t.textContent ?? ''
    );
  }

  it('lists readable artifacts with mine=1 and attaches the picked ones', async () => {
    apiFetch.mockImplementation((url: string) =>
      Promise.resolve(
        url.startsWith('/api/v1/artifacts')
          ? listResponse([item(A, 'Design notes', 'proj-1', 3), item(B, 'Brand guide', 'other')])
          : new Response('{}', { status: 404 })
      )
    );
    const el = await openPicker();
    expect(apiFetch.mock.calls[0][0]).toBe('/api/v1/artifacts?mine=1&limit=25');
    expect(titles(el)).toEqual(['Design notes', 'Brand guide']);

    const picked: any[] = [];
    el.addEventListener('artifact-picker-select', (e: CustomEvent) => picked.push(e.detail));
    (el.shadowRoot.querySelector('tbody tr') as HTMLElement).click();
    await el.updateComplete;
    expect(el.shadowRoot.querySelector('.footer .count').textContent).toBe('1 selected');
    el.attach();
    expect(picked[0].artifacts).toEqual([
      { id: A, ref: `scion://artifact/${A}`, title: 'Design notes', version: 3 },
    ]);
  });

  it('searches through the hub and filters to owned or this project', async () => {
    apiFetch.mockImplementation(() =>
      Promise.resolve(
        listResponse([item(A, 'Design notes', 'proj-1'), item(B, 'Brand guide', 'other')])
      )
    );
    const el = await openPicker();
    vi.useFakeTimers();
    el.onSearch({ target: { value: 'brand' } });
    vi.advanceTimersByTime(300);
    vi.useRealTimers();
    await settle(el);
    expect(lastListCall()).toBe('/api/v1/artifacts?mine=1&q=brand&limit=25');

    el.setFilter('owned');
    await settle(el);
    expect(lastListCall()).toBe('/api/v1/artifacts?mine=1&q=brand&owner=me&limit=25');

    el.setFilter('project');
    await settle(el);
    expect(titles(el)).toEqual(['Design notes']);
  });

  it('stops at the remaining limit and keeps already attached ones fixed', async () => {
    apiFetch.mockImplementation(() =>
      Promise.resolve(listResponse([item(A, 'One'), item(B, 'Two')]))
    );
    const el = await openPicker({ remaining: 1, attachedIds: [] });
    el.toggle(item(A, 'One'));
    el.toggle(item(B, 'Two'));
    expect([...el.selected.keys()]).toEqual([A]);

    const fixed = await openPicker({ attachedIds: [B] });
    const boxes = [...fixed.shadowRoot.querySelectorAll('sl-checkbox')] as any[];
    expect(boxes[1].hasAttribute('checked')).toBe(true);
    expect(boxes[1].hasAttribute('disabled')).toBe(true);
  });

  it('shows an empty state with no command to run', async () => {
    apiFetch.mockImplementation(() => Promise.resolve(listResponse([])));
    const el = await openPicker();
    const empty = el.shadowRoot.querySelector('.placeholder.empty');
    expect(empty.textContent).toContain('No artifacts yet');
    expect(empty.textContent).not.toContain('scion ');
  });
});
