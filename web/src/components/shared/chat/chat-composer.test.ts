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
  it('offers the developer formats the server now accepts', async () => {
    for (const ext of ['.json', '.yaml', '.go', '.py', '.tsx', '.jsx', '.env', '.sql', '.md']) {
      expect(ATTACHMENT_ACCEPT).toContain(ext);
    }
    expect(ATTACHMENT_ACCEPT).toContain('image/png');
    expect(ATTACHMENT_ACCEPT).toContain('application/pdf');
  });

  it('does not offer the blocked extensions', async () => {
    for (const ext of ['.exe', '.bat', '.sh', '.ps1', '.jar']) {
      expect(ATTACHMENT_ACCEPT).not.toContain(ext);
    }
    // .js is blocked; .jsx is not, and contains no ".js," token.
    expect(ATTACHMENT_ACCEPT.split(',')).not.toContain('.js');
  });

  it('puts the filter on the file input', async () => {
    const el = createComposer();
    document.body.appendChild(el);
    await el.updateComplete;

    const input = el.shadowRoot.querySelector('input[type="file"]');
    expect(input?.getAttribute('accept')).toBe(ATTACHMENT_ACCEPT);
  });
});

describe('composer — uploadFiles refactor', () => {
  it('uploadFiles uploads files and populates pendingFiles', async () => {
    const el = createComposer();
    respondWith(201, { attachments: [STORED], failures: [] });

    await el.uploadFiles([new File(['content'], 'compose.yaml')]);

    expect(el.pendingFiles).toHaveLength(1);
    expect(el.pendingFiles[0].name).toBe('compose.yaml');
  });

  it('uploadFiles enforces max 10 attachments', async () => {
    const el = createComposer();
    // Pre-fill 9 pending files
    el.pendingFiles = Array.from({ length: 9 }, (_, i) => ({
      ...STORED,
      id: `att-${i}`,
      name: `file-${i}.txt`,
    }));

    const errors: string[] = [];
    el.addEventListener('composer-error', (e: CustomEvent<{ message: string }>) =>
      errors.push(e.detail.message)
    );

    // Try to add 2 more (9 + 2 = 11 > 10)
    await el.uploadFiles([new File(['a'], 'a.txt'), new File(['b'], 'b.txt')]);

    expect(errors).toEqual(['Maximum 10 attachments per message']);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('uploadFiles does nothing with an empty array', async () => {
    const el = createComposer();
    await el.uploadFiles([]);
    expect(apiFetch).not.toHaveBeenCalled();
  });
});

describe('composer — paste-to-upload', () => {
  it('extracts image files from clipboard and uploads them', async () => {
    const el = createComposer();
    respondWith(201, {
      attachments: [{ ...STORED, name: 'image.png', mime: 'image/png' }],
      failures: [],
    });

    const imageFile = new File(['pixels'], 'image.png', { type: 'image/png' });
    const item = {
      type: 'image/png',
      getAsFile: () => imageFile,
    };

    const pasteEvent = new ClipboardEvent('paste', { cancelable: true });
    Object.defineProperty(pasteEvent, 'clipboardData', {
      value: { items: [item] },
    });

    // Call the paste handler directly
    (el as any).handlePaste(pasteEvent);

    // The event should be prevented (image paste overrides text paste)
    expect(pasteEvent.defaultPrevented).toBe(true);

    // Wait for the async uploadFiles call to resolve
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
  });

  it('ignores paste events with no image items', () => {
    const el = createComposer();

    const textItem = {
      type: 'text/plain',
      getAsFile: () => null,
    };

    const pasteEvent = new ClipboardEvent('paste', { cancelable: true });
    Object.defineProperty(pasteEvent, 'clipboardData', {
      value: { items: [textItem] },
    });

    (el as any).handlePaste(pasteEvent);

    // Text paste should not be prevented
    expect(pasteEvent.defaultPrevented).toBe(false);
    expect(apiFetch).not.toHaveBeenCalled();
  });
});

describe('composer — drag-and-drop', () => {
  it('sets dragOver on dragenter and clears on dragleave', async () => {
    const el = createComposer();
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.dragOver).toBe(false);

    // Simulate dragenter
    (el as any).handleDragEnter(new DragEvent('dragenter', { cancelable: true }));
    expect(el.dragOver).toBe(true);

    // Simulate dragleave with relatedTarget outside the wrapper
    const leaveEvent = new DragEvent('dragleave');
    Object.defineProperty(leaveEvent, 'currentTarget', { value: document.createElement('div') });
    Object.defineProperty(leaveEvent, 'relatedTarget', { value: null });
    (el as any).handleDragLeave(leaveEvent);
    expect(el.dragOver).toBe(false);

    document.body.removeChild(el);
  });

  it('uploads files on drop', async () => {
    const el = createComposer();
    respondWith(201, { attachments: [STORED], failures: [] });

    const file = new File(['data'], 'test.txt');
    const dt = new DataTransfer();
    dt.items.add(file);

    const dropEvent = new DragEvent('drop', { cancelable: true });
    Object.defineProperty(dropEvent, 'dataTransfer', { value: dt });

    (el as any).handleDrop(dropEvent);

    expect(dropEvent.defaultPrevented).toBe(true);
    expect(el.dragOver).toBe(false);
  });

  it('renders the drop zone overlay when dragOver is true', async () => {
    const el = createComposer();
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.shadowRoot.querySelector('.drop-zone-overlay')).toBeNull();

    el.dragOver = true;
    await el.updateComplete;

    const overlay = el.shadowRoot.querySelector('.drop-zone-overlay');
    expect(overlay).not.toBeNull();
    expect(overlay?.textContent).toContain('Drop files here');

    document.body.removeChild(el);
  });
});

describe('composer — draft persistence', () => {
  afterEach(() => {
    localStorage.clear();
  });

  it('saves a draft to localStorage on input (debounced)', async () => {
    const el = createComposer();
    el.conversationKey = 'conv-123';
    document.body.appendChild(el);
    await el.updateComplete;

    // Simulate input
    el.text = 'hello world';
    (el as any).saveDraft();

    // Before debounce fires, localStorage should be empty
    expect(localStorage.getItem('scion-chat-draft-conv-123')).toBeNull();

    // Wait for debounce (500ms + buffer)
    await new Promise((r) => setTimeout(r, 600));

    expect(localStorage.getItem('scion-chat-draft-conv-123')).toBe('hello world');

    document.body.removeChild(el);
  });

  it('restores a draft from localStorage on connect', async () => {
    localStorage.setItem('scion-chat-draft-conv-456', 'restored text');

    const el = createComposer();
    el.conversationKey = 'conv-456';
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.text).toBe('restored text');

    document.body.removeChild(el);
  });

  it('restores a draft when conversationKey changes', async () => {
    localStorage.setItem('scion-chat-draft-conv-A', 'draft A');
    localStorage.setItem('scion-chat-draft-conv-B', 'draft B');

    const el = createComposer();
    el.conversationKey = 'conv-A';
    document.body.appendChild(el);
    await el.updateComplete;
    expect(el.text).toBe('draft A');

    el.conversationKey = 'conv-B';
    await el.updateComplete;
    expect(el.text).toBe('draft B');

    document.body.removeChild(el);
  });

  it('clears the draft from localStorage on clearDraft', () => {
    localStorage.setItem('scion-chat-draft-conv-789', 'some text');

    const el = createComposer();
    el.conversationKey = 'conv-789';
    (el as any).clearDraft();

    expect(localStorage.getItem('scion-chat-draft-conv-789')).toBeNull();
  });

  it('does not save draft when conversationKey is empty', async () => {
    const el = createComposer();
    el.conversationKey = '';
    el.text = 'orphan draft';
    (el as any).saveDraft();

    await new Promise((r) => setTimeout(r, 600));

    // No key should have been written
    expect(localStorage.length).toBe(0);
  });
});
