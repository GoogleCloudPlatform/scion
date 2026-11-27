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
 * Tests for <scion-attachment-save-dialog> (ptone/scion#4598, #4599):
 * project picker, default title, new artifact vs keyed version append,
 * result state with the new artifact chip and Post into thread action,
 * and empty publishable-projects state.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { requestUrl } from '../../../client/__fixtures__/request-url.js';
import type {
  AttachmentArtifactPostDetail,
  SaveAttachmentTarget,
  ScionAttachmentSaveDialog,
} from './attachment-save-dialog.js';

const GLOBAL = { id: 'g-1', name: 'Global', slug: 'global' };
const ALPHA = { id: 'p-alpha', name: 'Alpha', slug: 'alpha' };
const BETA = { id: 'p-beta', name: 'Beta', slug: 'beta' };

const ATT: SaveAttachmentTarget = {
  id: '5f1c2d3e-0000-4000-8000-000000000011',
  name: 'design-notes.md',
  mime: 'text/markdown',
  size: 1024,
};

const ART_ID = '5f1c2d3e-0000-4000-8000-000000000099';

function mockHub(opts: {
  projects?: Array<{ id: string; name: string; slug: string }>;
  publishable?: Set<string>;
  seq?: number;
}) {
  const projects = opts.projects ?? [GLOBAL, ALPHA, BETA];
  const publishable = opts.publishable ?? new Set([GLOBAL.id, ALPHA.id]);
  const seq = opts.seq ?? 1;
  const posted: Array<Record<string, unknown>> = [];

  const json = (body: unknown, status = 200): Response =>
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });

  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = init?.method ?? 'GET';
      if (url.startsWith('/api/v1/projects?')) {
        const params = new URLSearchParams(url.split('?')[1] ?? '');
        const slug = params.get('slug');
        if (slug) {
          const found = projects.filter((p) => p.slug === slug);
          return Promise.resolve(json({ projects: found }));
        }
        return Promise.resolve(json({ projects }));
      }
      if (method === 'GET' && url.startsWith('/api/v1/artifacts?')) {
        const params = new URLSearchParams(url.split('?')[1] ?? '');
        const scope = params.get('scope') ?? '';
        return Promise.resolve(json({ artifacts: [], canCreate: publishable.has(scope) }));
      }
      if (method === 'POST' && url === '/api/v1/artifacts') {
        const body = JSON.parse(typeof init?.body === 'string' ? init.body : '{}') as Record<
          string,
          unknown
        >;
        posted.push(body);
        const scopeRef = typeof body.scopeRef === 'string' ? body.scopeRef : ALPHA.id;
        const key = typeof body.key === 'string' ? body.key : undefined;
        const title = typeof body.title === 'string' ? body.title : ATT.name;
        return Promise.resolve(
          json(
            {
              artifact: {
                id: ART_ID,
                ref: `scion://artifact/${ART_ID}`,
                scopeKind: 'project',
                scopeRef,
                ownerKind: 'user',
                ownerRef: 'u1',
                key,
                title,
                currentSeq: seq,
                createdAt: '2026-11-20T00:00:00Z',
                updatedAt: '2026-11-20T00:00:00Z',
              },
              version: {
                seq,
                ref: `scion://artifact/${ART_ID}@${seq}`,
                kind: 'publish',
                entryPath: ATT.name,
                totalBytes: ATT.size,
                fileCount: 1,
                createdAt: '2026-11-20T00:00:00Z',
                state: 'ready',
                files: [],
                sourceAttachmentId: ATT.id,
                sourceMessageId: 'msg-1',
              },
            },
            201
          )
        );
      }
      return Promise.resolve(json({}, 404));
    })
  );
  return { posted };
}

describe('scion-attachment-save-dialog (#4598, #4599)', () => {
  beforeAll(async () => {
    await import('./attachment-save-dialog.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('defaults title to attachment filename, preselects defaultProjectId, creates a new artifact, and offers Post into thread', async () => {
    const { posted } = mockHub({ publishable: new Set([GLOBAL.id, ALPHA.id]), seq: 1 });
    const el = document.createElement('scion-attachment-save-dialog') as ScionAttachmentSaveDialog;
    el.attachment = ATT;
    el.defaultProjectId = ALPHA.id;
    el.open = true;
    document.body.appendChild(el);

    await vi.waitFor(() => {
      const select = el.shadowRoot?.querySelector<HTMLSelectElement>('.project-select');
      expect(select?.value).toBe(ALPHA.id);
    });

    const titleInput = el.shadowRoot?.querySelector<HTMLInputElement>('.title-input');
    expect(titleInput?.value).toBe(ATT.name);

    const noteInput = el.shadowRoot?.querySelector<HTMLInputElement>('.note-input');
    if (noteInput) {
      noteInput.value = 'from chat';
      noteInput.dispatchEvent(new CustomEvent('sl-input'));
    }
    await el.updateComplete;

    const submit = el.shadowRoot?.querySelector<HTMLElement>('.submit-btn');
    expect(submit?.hasAttribute('disabled')).toBe(false);
    submit?.click();

    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('.result-artifact-chip')).not.toBeNull();
    });

    expect(posted).toEqual([
      {
        fromAttachment: ATT.id,
        scopeRef: ALPHA.id,
        title: ATT.name,
        note: 'from chat',
      },
    ]);

    const chip = el.shadowRoot?.querySelector<HTMLAnchorElement>('.result-artifact-chip');
    expect(chip?.textContent).toContain(ATT.name);
    expect(chip?.textContent).toContain('v1');

    const postEvents: AttachmentArtifactPostDetail[] = [];
    el.addEventListener('attachment-artifact-post', (e: Event) => {
      postEvents.push((e as CustomEvent<AttachmentArtifactPostDetail>).detail);
    });
    el.shadowRoot?.querySelector<HTMLElement>('.post-to-thread-btn')?.click();

    expect(postEvents).toHaveLength(1);
    expect(postEvents[0].ref).toBe(`scion://artifact/${ART_ID}`);
    expect(el.open).toBe(false);
  });

  it('supports appending the next version of an existing keyed artifact', async () => {
    const { posted } = mockHub({ publishable: new Set([ALPHA.id]), seq: 2 });
    const el = document.createElement('scion-attachment-save-dialog') as ScionAttachmentSaveDialog;
    el.attachment = ATT;
    el.defaultProjectId = ALPHA.id;
    el.open = true;
    document.body.appendChild(el);

    await vi.waitFor(() => {
      const select = el.shadowRoot?.querySelector<HTMLSelectElement>('.project-select');
      expect(select?.value).toBe(ALPHA.id);
    });

    // Switch mode to 'version'.
    const radio = el.shadowRoot?.querySelector<HTMLInputElement>('.mode-group');
    if (radio) {
      radio.value = 'version';
      radio.dispatchEvent(new CustomEvent('sl-change'));
    }
    await el.updateComplete;

    // Submit is disabled until a key is entered.
    expect(el.shadowRoot?.querySelector('.submit-btn')?.hasAttribute('disabled')).toBe(true);

    const keyInput = el.shadowRoot?.querySelector<HTMLInputElement>('.key-input');
    if (keyInput) {
      keyInput.value = 'design-spec';
      keyInput.dispatchEvent(new CustomEvent('sl-input'));
    }
    await el.updateComplete;

    const submit = el.shadowRoot?.querySelector<HTMLElement>('.submit-btn');
    expect(submit?.hasAttribute('disabled')).toBe(false);
    submit?.click();

    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('.result-artifact-chip')).not.toBeNull();
    });

    expect(posted).toEqual([
      {
        fromAttachment: ATT.id,
        scopeRef: ALPHA.id,
        key: 'design-spec',
      },
    ]);
    expect(el.shadowRoot?.querySelector('.result-intro')?.textContent).toContain(
      'Published version v2'
    );
  });

  it('disables submit and explains when caller has no publishable projects', async () => {
    mockHub({ publishable: new Set() });
    const el = document.createElement('scion-attachment-save-dialog') as ScionAttachmentSaveDialog;
    el.attachment = ATT;
    el.open = true;
    document.body.appendChild(el);

    await vi.waitFor(() => {
      const select = el.shadowRoot?.querySelector('.project-select');
      expect(select?.getAttribute('help-text')).toContain("can't publish");
    });

    expect(el.shadowRoot?.querySelector('.submit-btn')?.hasAttribute('disabled')).toBe(true);
  });
});
