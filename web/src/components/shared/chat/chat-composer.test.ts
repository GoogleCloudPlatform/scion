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
 * Tests for per-file attachment upload results in the composer (#1045).
 *
 * The server takes each file on its own merits, so a batch can come back part
 * stored and part refused. The composer has to keep what was stored and name
 * what was not — collapsing the answer into a single "upload failed" throws
 * away both halves of it.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

/* eslint-disable @typescript-eslint/no-explicit-any */

const apiFetch = vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })));

vi.mock('../../../client/api.js', () => ({ apiFetch }));

let ATTACHMENT_ACCEPT: string;

/** A composer with a file already chosen in its hidden input. */
function createComposer(): any {
  const el = document.createElement('scion-chat-composer') as any;
  el.conversationMode = true;
  el.projectId = 'proj-1';
  return el;
}

/** Drive the file picker's change handler with a set of files. */
async function selectFiles(el: any, names: string[]): Promise<void> {
  const files = names.map((name) => new File(['content'], name));
  await el.handleFileSelected({ target: { files } });
}

function respondWith(status: number, body: unknown): void {
  apiFetch.mockResolvedValue(new Response(JSON.stringify(body), { status }));
}

const STORED = {
  id: 'att-1',
  name: 'compose.yaml',
  mime: 'text/plain',
  size: 12,
  url: '/api/v1/chat/attachments/att-1',
};

beforeAll(async () => {
  const mod = await import('./chat-composer.js');
  ATTACHMENT_ACCEPT = (mod as any).ATTACHMENT_ACCEPT;
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
});

describe('composer — partial upload results', () => {
  it('keeps the stored files and records the refused ones', async () => {
    const el = createComposer();
    respondWith(201, {
      attachments: [STORED],
      failures: [{ name: 'bad.exe', error: 'files with a .exe extension are not accepted' }],
    });

    await selectFiles(el, ['compose.yaml', 'bad.exe']);

    expect(el.pendingFiles).toHaveLength(1);
    expect(el.pendingFiles[0].name).toBe('compose.yaml');
    expect(el.uploadFailures).toEqual([
      { name: 'bad.exe', error: 'files with a .exe extension are not accepted' },
    ]);
  });

  it('records failures from a batch where nothing was stored', async () => {
    const el = createComposer();
    respondWith(400, {
      attachments: [],
      failures: [{ name: 'a.exe', error: 'not accepted' }],
    });

    await selectFiles(el, ['a.exe']);

    expect(el.pendingFiles).toHaveLength(0);
    expect(el.uploadFailures).toHaveLength(1);
  });

  it('does not raise a composer-error when the failures are per file', async () => {
    const el = createComposer();
    respondWith(400, { attachments: [], failures: [{ name: 'a.exe', error: 'not accepted' }] });
    const errors: string[] = [];
    el.addEventListener('composer-error', (e: CustomEvent<{ message: string }>) =>
      errors.push(e.detail.message)
    );

    await selectFiles(el, ['a.exe']);

    expect(errors).toEqual([]);
  });

  it('still raises a composer-error when the whole request failed', async () => {
    const el = createComposer();
    respondWith(503, { message: 'Attachments not available' });
    const errors: string[] = [];
    el.addEventListener('composer-error', (e: CustomEvent<{ message: string }>) =>
      errors.push(e.detail.message)
    );

    await selectFiles(el, ['compose.yaml']);

    expect(errors).toEqual(['Attachments not available']);
  });

  it('clears earlier failures on the next successful upload', async () => {
    const el = createComposer();
    respondWith(400, { attachments: [], failures: [{ name: 'a.exe', error: 'not accepted' }] });
    await selectFiles(el, ['a.exe']);
    expect(el.uploadFailures).toHaveLength(1);

    respondWith(201, { attachments: [STORED], failures: [] });
    await selectFiles(el, ['compose.yaml']);

    expect(el.uploadFailures).toEqual([]);
  });
});

describe('composer — failure rendering', () => {
  async function mount(): Promise<any> {
    const el = createComposer();
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  it('names each refused file and its reason', async () => {
    const el = await mount();
    el.uploadFailures = [
      { name: 'bad.exe', error: 'files with a .exe extension are not accepted' },
      { name: 'huge.log', error: 'file exceeds the maximum size of 10485760 bytes' },
    ];
    await el.updateComplete;

    const rows = [...el.shadowRoot.querySelectorAll('.upload-failure')];
    expect(rows).toHaveLength(2);
    expect(rows[0]?.textContent).toContain('bad.exe');
    expect(rows[0]?.textContent).toContain('.exe extension are not accepted');
    expect(rows[1]?.textContent).toContain('huge.log');
  });

  it('shows nothing when every file was accepted', async () => {
    const el = await mount();

    expect(el.shadowRoot.querySelector('.upload-failures')).toBeNull();
  });

  it('dismisses the row that was clicked and keeps the rest', async () => {
    const el = await mount();
    el.uploadFailures = [
      { name: 'a.exe', error: 'not accepted' },
      { name: 'b.sh', error: 'not accepted' },
      { name: 'c.bat', error: 'not accepted' },
    ];
    await el.updateComplete;

    const dismissRows = [...el.shadowRoot.querySelectorAll('.dismiss-btn')];
    (dismissRows[1] as HTMLButtonElement).click();
    await el.updateComplete;

    expect(el.uploadFailures.map((f: { name: string }) => f.name)).toEqual(['a.exe', 'c.bat']);
    expect(el.shadowRoot.querySelectorAll('.upload-failure')).toHaveLength(2);
  });

  it('clears the surface once the last row is dismissed', async () => {
    const el = await mount();
    el.uploadFailures = [{ name: 'bad.exe', error: 'not accepted' }];
    await el.updateComplete;

    (el.shadowRoot.querySelector('.dismiss-btn') as HTMLButtonElement).click();
    await el.updateComplete;

    expect(el.uploadFailures).toEqual([]);
    expect(el.shadowRoot.querySelector('.upload-failures')).toBeNull();
  });
});

describe('composer — whole-request error messages', () => {
  it('reads the reason out of the hub error envelope', async () => {
    const el = createComposer();
    respondWith(503, {
      error: { code: 'SERVICE_UNAVAILABLE', message: 'Attachments not available' },
    });
    const errors: string[] = [];
    el.addEventListener('composer-error', (e: CustomEvent<{ message: string }>) =>
      errors.push(e.detail.message)
    );

    await selectFiles(el, ['compose.yaml']);

    expect(errors).toEqual(['Attachments not available']);
  });

  it('falls back to a generic message when the body carries no reason', async () => {
    const el = createComposer();
    respondWith(500, {});
    const errors: string[] = [];
    el.addEventListener('composer-error', (e: CustomEvent<{ message: string }>) =>
      errors.push(e.detail.message)
    );

    await selectFiles(el, ['compose.yaml']);

    expect(errors).toEqual(['Upload failed']);
  });
});

describe('composer — file picker filter', () => {
  it('ATTACHMENT_ACCEPT is empty so the picker offers all files', () => {
    expect(ATTACHMENT_ACCEPT).toBe('');
  });

  it('does not restrict the file input with an accept attribute', async () => {
    const el = createComposer();
    document.body.appendChild(el);
    await el.updateComplete;

    const input = el.shadowRoot.querySelector('input[type="file"]');
    expect(input).toBeTruthy();
    // No accept attribute — the server enforces the deny-list.
    expect(input?.hasAttribute('accept')).toBe(false);
  });
});
