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
import '@shoelace-style/shoelace/dist/components/textarea/textarea.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

const apiFetch = vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })));
const showToast = vi.fn();

vi.mock('../../../client/api.js', () => ({ apiFetch }));
vi.mock('../../../utils/toast.js', () => ({ showToast }));

let ATTACHMENT_ACCEPT: string;
let PASTE_TO_ATTACHMENT_THRESHOLD: number;

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
  PASTE_TO_ATTACHMENT_THRESHOLD = (mod as any).PASTE_TO_ATTACHMENT_THRESHOLD;
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

// ── Paste-to-attachment auto-conversion ──────────────────────────────────

/** Build a minimal paste event with the given plain text on the clipboard. */
function makePasteEvent(text: string, imageFiles?: File[]): ClipboardEvent {
  const items: DataTransferItem[] = [];
  if (imageFiles) {
    for (const f of imageFiles) {
      items.push({ kind: 'file', type: f.type, getAsFile: () => f, getAsString: () => {} } as any);
    }
  }
  if (text) {
    items.push({
      kind: 'string',
      type: 'text/plain',
      getAsFile: () => null,
      getAsString: (cb: (s: string) => void) => cb(text),
    } as any);
  }

  const clipboardData = {
    items,
    getData: (format: string) => (format === 'text/plain' ? text : ''),
  } as any;

  const event = new Event('paste', { bubbles: true, cancelable: true }) as any;
  event.clipboardData = clipboardData;
  // Add a working preventDefault that sets defaultPrevented.
  let prevented = false;
  event.preventDefault = () => {
    prevented = true;
  };
  Object.defineProperty(event, 'defaultPrevented', { get: () => prevented });
  return event as ClipboardEvent;
}

describe('composer — paste-to-attachment', () => {
  it('lets short pastes fall through to the textarea', () => {
    const el = createComposer();
    const shortText = 'a'.repeat(PASTE_TO_ATTACHMENT_THRESHOLD - 1);
    const event = makePasteEvent(shortText);

    el.handlePaste(event);

    expect(event.defaultPrevented).toBe(false);
    expect(apiFetch).not.toHaveBeenCalled();
    expect(showToast).not.toHaveBeenCalled();
  });

  it('converts pastes over the threshold into a text attachment upload', async () => {
    const el = createComposer();
    respondWith(201, {
      attachments: [
        {
          id: 'att-paste',
          name: 'pasted-text.txt',
          mime: 'text/plain',
          size: 1501,
          url: '/api/v1/chat/attachments/att-paste',
        },
      ],
      failures: [],
    });
    const longText = 'x'.repeat(PASTE_TO_ATTACHMENT_THRESHOLD + 1);
    const event = makePasteEvent(longText);

    el.handlePaste(event);

    expect(event.defaultPrevented).toBe(true);
    // uploadFiles is async — wait for the microtask to flush.
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalledTimes(1));

    // The uploaded FormData should contain a File with the pasted text.
    const call = apiFetch.mock.calls[0];
    expect(call[0]).toBe('/api/v1/chat/attachments');
    const body = call[1].body as FormData;
    const uploaded = body.get('files') as File;
    expect(uploaded).toBeInstanceOf(File);
    expect(uploaded.type).toBe('text/plain');
    expect(uploaded.name).toMatch(/^pasted-text-.*\.txt$/);
    const content = await uploaded.text();
    expect(content).toBe(longText);

    // Toast was shown.
    expect(showToast).toHaveBeenCalledWith('Large paste converted to text attachment', 'primary');
  });

  it('gives image pastes precedence over text conversion', async () => {
    const el = createComposer();
    respondWith(201, { attachments: [], failures: [] });
    const imageFile = new File(['img'], 'screenshot.png', { type: 'image/png' });
    const longText = 'x'.repeat(PASTE_TO_ATTACHMENT_THRESHOLD + 500);
    const event = makePasteEvent(longText, [imageFile]);

    el.handlePaste(event);

    expect(event.defaultPrevented).toBe(true);
    // Wait for the image upload to complete so it doesn't leak into later tests.
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalledTimes(1));
    // The upload was for the image, not the text — no toast.
    expect(showToast).not.toHaveBeenCalled();
  });

  it('skips paste-to-attachment in edit mode', () => {
    const el = createComposer();
    el.editMessage = { messageId: 'msg-1', content: 'original' };
    const longText = 'y'.repeat(PASTE_TO_ATTACHMENT_THRESHOLD + 100);
    const event = makePasteEvent(longText);

    el.handlePaste(event);

    expect(event.defaultPrevented).toBe(false);
    expect(apiFetch).not.toHaveBeenCalled();
    expect(showToast).not.toHaveBeenCalled();
  });

  it('does not convert pastes at exactly the threshold', () => {
    const el = createComposer();
    const exactText = 'a'.repeat(PASTE_TO_ATTACHMENT_THRESHOLD);
    const event = makePasteEvent(exactText);

    el.handlePaste(event);

    expect(event.defaultPrevented).toBe(false);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('lets large paste fall through when at attachment limit', () => {
    const el = createComposer();
    // Simulate 10 pending files
    el.pendingFiles = Array.from({ length: 10 }, (_, i) => ({
      id: `att-${i}`,
      name: `file-${i}.txt`,
      mime: 'text/plain',
      size: 100,
      url: `/att/${i}`,
    }));
    const longText = 'z'.repeat(PASTE_TO_ATTACHMENT_THRESHOLD + 1);
    const event = makePasteEvent(longText);

    el.handlePaste(event);

    expect(event.defaultPrevented).toBe(false);
    expect(apiFetch).not.toHaveBeenCalled();
    expect(showToast).not.toHaveBeenCalled();
  });
});

// ── Caret-end focus hardening (reply target set before sl-textarea upgrades) ──

/**
 * `focusTextareaCaretEnd()` (triggered by a `replyTo` change) reaches into
 * `<sl-textarea>`'s shadow DOM for the native `<textarea>`. If that lookup
 * runs before `<sl-textarea>` has finished its own first render, the shadow
 * DOM is still empty and the lookup returns null, silently dropping focus.
 * No code path sets `replyTo` that early today (see chat-thread.test.ts's
 * reply-focus suite for the real flow), but the composer hardens against it:
 * retry once after the child's own `updateComplete`, and fall back to
 * focusing the `<sl-textarea>` host if the inner textarea still can't be
 * found.
 */
describe('composer — caret-end focus hardening when sl-textarea is not yet upgraded', () => {
  async function mountBare(): Promise<any> {
    const el = document.createElement('scion-chat-composer') as any;
    document.body.appendChild(el);
    await el.updateComplete;
    const slTextarea = el.shadowRoot.querySelector('sl-textarea');
    await slTextarea.updateComplete;
    return { el, slTextarea };
  }

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('retries after the child upgrades instead of dropping focus, keeping the caret at the end', async () => {
    const { el, slTextarea } = await mountBare();
    const textarea = slTextarea.shadowRoot.querySelector('textarea') as HTMLTextAreaElement;

    // Simulate an in-progress draft the user already typed, caret left at
    // the start — a real input event so `el.text` matches the DOM value and
    // Lit's `live()` binding won't itself reset the caret on the next render.
    textarea.value = 'draft so far';
    textarea.selectionStart = textarea.selectionEnd = 0;
    textarea.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
    await el.updateComplete;

    const realGetTextareaElement = el.getTextareaElement.bind(el);
    let calls = 0;
    // First lookup simulates <sl-textarea> not having rendered its shadow
    // DOM yet; later lookups behave normally.
    el.getTextareaElement = (): HTMLTextAreaElement | null => {
      calls++;
      return calls === 1 ? null : realGetTextareaElement();
    };

    el.replyTo = { messageId: 'm1', senderName: 'Ann', content: 'hi' };
    await el.updateComplete;

    await vi.waitFor(() => {
      expect(slTextarea.shadowRoot?.activeElement).toBe(textarea);
      expect(textarea.selectionStart).toBe('draft so far'.length);
      expect(textarea.selectionEnd).toBe('draft so far'.length);
    });
    // Confirms the retry branch, not just the first-try success path, ran.
    expect(calls).toBeGreaterThanOrEqual(2);
  });

  it('falls back to focusing the sl-textarea host if the inner textarea never appears', async () => {
    const { el, slTextarea } = await mountBare();
    el.getTextareaElement = (): null => null;

    el.replyTo = { messageId: 'm1', senderName: 'Ann', content: 'hi' };
    await el.updateComplete;

    await vi.waitFor(() => {
      expect(el.shadowRoot.activeElement).toBe(slTextarea);
    });
  });
});

describe('isComposing', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
  });

  function stubTouchPrimary(touch: boolean): void {
    vi.stubGlobal(
      'matchMedia',
      vi.fn((query: string) => ({
        matches: touch && query.includes('pointer: coarse'),
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
      }))
    );
  }

  async function mountFocused(): Promise<any> {
    const el = createComposer();
    document.body.appendChild(el);
    await el.updateComplete;
    Object.defineProperty(el.shadowRoot, 'activeElement', {
      configurable: true,
      get: () => el.shadowRoot.querySelector('sl-textarea'),
    });
    return el;
  }

  it('is true with draft text on any device', async () => {
    stubTouchPrimary(false);
    const el = createComposer();
    document.body.appendChild(el);
    await el.updateComplete;
    expect(el.isComposing).toBe(false);
    el.text = '   ';
    expect(el.isComposing).toBe(false);
    el.text = 'half a thought';
    expect(el.isComposing).toBe(true);
  });

  it('on desktop, focus alone is not composing (it stays after every send)', async () => {
    stubTouchPrimary(false);
    const el = await mountFocused();
    expect(el.isComposing).toBe(false);
  });

  it('on touch, focus means the keyboard is up, so it counts', async () => {
    stubTouchPrimary(true);
    const el = await mountFocused();
    expect(el.isComposing).toBe(true);
  });
});
