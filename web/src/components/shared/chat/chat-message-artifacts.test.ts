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
 * Tests for artifact references on <scion-chat-message> (ptone/scion#3224,
 * ptone/scion#3225): chips (readable and unavailable), their order, the
 * linkified scion://artifact/ URLs, and both opening the in-place preview.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import type { MessageArtifactRef } from '../../../client/artifacts.js';

vi.mock('../../../utils/markdown.js', () => ({
  getMarkdownRenderer: () =>
    Promise.resolve({
      render: (markdown: string) =>
        `<p>${markdown
          .replace(/```([\s\S]*?)```/g, '</p><pre><code>$1</code></pre><p>')
          .replace(/`([^`]+)`/g, '<code>$1</code>')}</p>`,
    }),
}));

vi.mock('../code-editor.js', () => ({
  getLanguageFromPath: () => 'plaintext',
}));

const apiFetchMock = vi.fn();
vi.mock('../../../client/api.js', () => ({
  apiFetch: (path: string, options?: RequestInit) => apiFetchMock(path, options),
  extractApiError: () => Promise.resolve('error'),
}));

await import('./chat-message.js');
type ScionChatMessage = import('./chat-message.js').ScionChatMessage;

const A = '5f1c2d3e-0000-4000-8000-0000000000aa';
const B = '5f1c2d3e-0000-4000-8000-0000000000bb';
const C = '5f1c2d3e-0000-4000-8000-0000000000cc';

function readable(id: string, title: string, version: number, owner: string): MessageArtifactRef {
  return {
    ref: `scion://artifact/${id}`,
    id,
    available: true,
    title,
    version,
    ownerName: owner,
    ownerKind: 'agent',
    ownerRef: 'x',
  };
}

async function mount(body: string, refs: MessageArtifactRef[] = []): Promise<ScionChatMessage> {
  const el = document.createElement('scion-chat-message') as ScionChatMessage;
  el.body = body;
  el.artifactRefs = refs;
  document.body.appendChild(el);
  await el.updateComplete;
  await Promise.resolve();
  await el.updateComplete;
  return el;
}

function chips(el: ScionChatMessage): HTMLButtonElement[] {
  return Array.from(el.shadowRoot?.querySelectorAll<HTMLButtonElement>('.artifact-chip') ?? []);
}

function previewTarget(el: ScionChatMessage): unknown {
  const preview = el.shadowRoot?.querySelector('scion-chat-file-preview') as
    | (HTMLElement & { target: unknown })
    | null;
  return preview?.target ?? null;
}

describe('scion-chat-message artifact references', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
    window.__SCION_FEATURES__ = { 'hub.artifacts': true };
    // The preview opens and starts loading; keep it pending.
    apiFetchMock.mockReset();
    apiFetchMock.mockImplementation(() => new Promise(() => {}));
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
  });

  it('renders a readable chip as title · vN · owner and an unavailable one without a title', async () => {
    const el = await mount('see these', [
      readable(A, 'Design notes', 3, 'docs-writer'),
      { ref: `scion://artifact/${B}`, id: B, available: false },
    ]);
    const [first, second] = chips(el);
    expect(first.textContent?.replace(/\s+/g, ' ').trim()).toBe('Design notes · v3 · docs-writer');
    expect(first.querySelector('sl-icon')?.getAttribute('name')).toBe('file-earmark-richtext');
    expect(second.classList.contains('unavailable')).toBe(true);
    expect(second.textContent?.trim()).toBe('Artifact unavailable');
    expect(second.querySelector('sl-icon')?.getAttribute('name')).toBe('lock');
  });

  it('orders chips by where each artifact appears in the body, not by id', async () => {
    const el = await mount(`first scion://artifact/${C} then scion://artifact/${A}@2`, [
      readable(A, 'Alpha', 2, 'o'),
      readable(B, 'Not in body', 1, 'o'),
      readable(C, 'Gamma', 1, 'o'),
    ]);
    expect(chips(el).map((c) => c.querySelector('.artifact-title')?.textContent)).toEqual([
      'Gamma',
      'Alpha',
      'Not in body',
    ]);
  });

  it('opens the in-place preview from a chip, readable or not', async () => {
    const el = await mount('x', [
      { ...readable(A, 'Design notes', 3, 'o'), seq: 3 },
      { ref: `scion://artifact/${B}`, id: B, available: false },
    ]);
    chips(el)[0].click();
    await el.updateComplete;
    expect(previewTarget(el)).toEqual({ kind: 'artifact', id: A, seq: 3, name: 'Design notes' });

    chips(el)[1].click();
    await el.updateComplete;
    expect(previewTarget(el)).toEqual({ kind: 'artifact', id: B, seq: 0, name: 'Artifact' });
  });

  it('linkifies scion://artifact/ URLs and opens the preview on click', async () => {
    const el = await mount(`read scion://artifact/${A.toUpperCase()}@2 please`);
    const link = el.shadowRoot?.querySelector<HTMLAnchorElement>('.md-content a.artifact-link');
    expect(link).toBeTruthy();
    expect(link?.dataset.artifactId).toBe(A);
    expect(link?.dataset.artifactSeq).toBe('2');
    // The version suffix is not restyled as an @mention.
    expect(link?.querySelector('.mention')).toBeNull();
    expect(link?.textContent).toBe(`scion://artifact/${A.toUpperCase()}@2`);

    link?.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await el.updateComplete;
    expect(previewTarget(el)).toEqual({ kind: 'artifact', id: A, seq: 2, name: 'Artifact' });
  });

  it('keeps a middle-click on an artifact link from opening a tab', async () => {
    const el = await mount(`read scion://artifact/${A}`);
    const link = el.shadowRoot?.querySelector<HTMLAnchorElement>('a.artifact-link');
    const aux = new MouseEvent('auxclick', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 1,
    });
    link?.dispatchEvent(aux);
    expect(aux.defaultPrevented).toBe(true);
    expect(previewTarget(el)).toBeNull();
  });

  it('leaves references inside code and malformed references as text', async () => {
    const el = await mount(`\`scion://artifact/${A}\` and scion://artifact/not-a-uuid`);
    expect(el.shadowRoot?.querySelectorAll('a.artifact-link')).toHaveLength(0);
  });

  // Bodies below stand for the sanitised HTML the markdown renderer hands
  // the linkify pass (the mock renderer passes HTML through unchanged).
  it('does not link a reference inside a tag attribute', async () => {
    const el = await mount(
      `<a href="https://e.com" title="scion://artifact/${A}">x</a> <img alt="scion://artifact/${A}" src="https://e.com/i.png">`
    );
    expect(el.shadowRoot?.querySelectorAll('a.artifact-link')).toHaveLength(0);
    const md = el.shadowRoot?.querySelector('.md-content');
    expect(md?.querySelector('a[href="https://e.com"]')?.getAttribute('title')).toBe(
      `scion://artifact/${A}`
    );
    expect(md?.querySelector('img')?.getAttribute('alt')).toBe(`scion://artifact/${A}`);
  });

  it('does not nest a link inside an existing link', async () => {
    const el = await mount(`<a href="https://e.com">scion://artifact/${A}</a>`);
    expect(el.shadowRoot?.querySelectorAll('a.artifact-link')).toHaveLength(0);
    expect(el.shadowRoot?.querySelector('.md-content a')?.textContent).toBe(
      `scion://artifact/${A}`
    );
  });

  it('keeps links out of attribute values that contain ">"', async () => {
    // Attribute values that contain ">" must not produce links or
    // script-bearing markup.
    const el = await mount(
      `<span title="x> scion://artifact/${A}">t</span> <span data-x='y> scion://artifact/${B}@2'>u</span>`
    );
    const md = el.shadowRoot?.querySelector('.md-content') as HTMLElement;
    expect(md.querySelectorAll('script')).toHaveLength(0);
    for (const node of [md, ...Array.from(md.querySelectorAll('*'))]) {
      for (const attr of Array.from(node.attributes)) {
        expect(attr.name.toLowerCase().startsWith('on'), `${node.tagName} ${attr.name}`).toBe(
          false
        );
        expect(
          /javascript:/i.test(attr.value),
          `${node.tagName} ${attr.name}="${attr.value}"`
        ).toBe(false);
      }
    }
    for (const a of Array.from(md.querySelectorAll('a'))) {
      expect(a.className).toBe('entity-link artifact-link');
      expect(a.getAttribute('data-artifact-id')).toMatch(
        /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
      );
    }
  });

  it('shows no chips and no links while the experiment is off', async () => {
    window.__SCION_FEATURES__ = { 'hub.artifacts': false };
    const el = await mount(`scion://artifact/${A}`, [readable(A, 'Alpha', 1, 'o')]);
    expect(chips(el)).toHaveLength(0);
    expect(el.shadowRoot?.querySelectorAll('a.artifact-link')).toHaveLength(0);
  });
});
