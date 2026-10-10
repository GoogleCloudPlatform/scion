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

// This file proves the sink pin alone — with the source-side path/id
// validators bypassed — still stops the viewer from ever calling apiFetch
// with a malicious URL. It mocks `chat-file-links.js` so the component's
// `buildFileApiUrl`/`buildAttachmentApiUrl` imports resolve to the
// `*PinOnly` variants (construction + the pin, no source validation), the
// same seam `chat-file-links.test.ts` uses to test the pin directly. This is
// a cross-module import from the component's point of view, so it can be
// mocked here; it is a separate file (not a describe block added to
// chat-file-preview.test.ts) because a `vi.mock` factory applies to every
// test in its file, and every other test in chat-file-preview.test.ts
// depends on the real, fully-validating builders.

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

vi.mock('../code-editor.js', () => ({
  getLanguageFromPath: () => 'plaintext',
}));

const apiFetchMock = vi.fn();
vi.mock('../../../client/api.js', () => ({
  apiFetch: (path: string, options?: RequestInit) => apiFetchMock(path, options),
  extractApiError: async (res: Response, fallback: string) => {
    try {
      const body = (await res.json()) as { error?: string };
      return body?.error ?? fallback;
    } catch {
      return fallback;
    }
  },
}));

vi.mock('../../../utils/chat-file-links.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../../utils/chat-file-links.js')>();
  return {
    ...actual,
    // Source-side validation bypassed: only the sink pin can still stop a
    // malicious target from reaching apiFetch.
    buildFileApiUrl: actual.buildFileApiUrlPinOnly,
    buildAttachmentApiUrl: actual.buildAttachmentApiUrlPinOnly,
  };
});

await import('./chat-file-preview.js');
type ScionChatFilePreview = import('./chat-file-preview.js').ScionChatFilePreview;
type PreviewTarget = import('./chat-file-preview.js').PreviewTarget;

async function mount(): Promise<ScionChatFilePreview> {
  const el = document.createElement('scion-chat-file-preview') as ScionChatFilePreview;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

async function settle(el: ScionChatFilePreview): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await Promise.resolve();
    await el.updateComplete;
  }
}

function dialog(el: ScionChatFilePreview): Element | null {
  return el.shadowRoot?.querySelector('sl-dialog.file-preview-dialog') ?? null;
}

function hasDownloadButton(el: ScionChatFilePreview): boolean {
  return Array.from(dialog(el)?.querySelectorAll('sl-button') ?? []).some((b) =>
    b.textContent?.includes('Download')
  );
}

describe('scion-chat-file-preview, the pin alone, with source validation bypassed', () => {
  beforeEach(() => {
    apiFetchMock.mockReset();
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  const bypassCases: Array<{ name: string; target: PreviewTarget }> = [
    {
      name: 'a same-shape cross-project retargeting workspace path',
      target: {
        kind: 'path',
        projectId: 'proj-1',
        containerPath: '/workspace/secret.txt',
        location: {
          kind: 'workspace',
          filePath: '../../../OTHER-PROJECT/workspace/files/secret.txt',
        },
        name: 'secret.txt',
      },
    },
    {
      name: 'a raw .. shared-dir name',
      target: {
        kind: 'path',
        projectId: 'proj-1',
        containerPath: '/workspace/.scion-volumes/shared/notes.txt',
        location: { kind: 'shared-dir', dirName: '..', filePath: 'notes.txt' },
        name: 'notes.txt',
      },
    },
    {
      name: 'a raw .. attachment id',
      target: { kind: 'attachment', id: '..', name: 'notes.txt', mime: 'text/plain', size: 5 },
    },
  ];

  for (const { name, target } of bypassCases) {
    it(`shows an error with Retry/Close, never calls apiFetch, and offers no Download link, for ${name}`, async () => {
      const el = await mount();
      el.target = target;
      await settle(el);

      expect(apiFetchMock).not.toHaveBeenCalled();
      expect(dialog(el)).not.toBeNull();
      expect(dialog(el)?.querySelector('.file-preview-placeholder.error')).not.toBeNull();
      expect(dialog(el)?.textContent).toContain('Retry');
      expect(hasDownloadButton(el)).toBe(false);
      // The user sees a generic message, never the internal pin/builder
      // error text (see chat-file-preview.ts's load(), which maps a thrown
      // downloadUrlFor error to a fixed string before it reaches state).
      expect(dialog(el)?.textContent).not.toContain('pinBuiltUrl');
      expect(dialog(el)?.textContent).not.toContain('buildFileApiUrl');
      expect(dialog(el)?.textContent).not.toContain('buildAttachmentApiUrl');
    });
  }

  it('still loads a safe target normally when only the pin (not source validation) is active', async () => {
    apiFetchMock.mockResolvedValue({ ok: true, status: 200, text: () => Promise.resolve('hello') });
    const el = await mount();
    el.target = { kind: 'attachment', id: 'att-1', name: 'notes.txt', mime: 'text/plain', size: 5 };
    await settle(el);

    expect(apiFetchMock).toHaveBeenCalledWith('/api/v1/chat/attachments/att-1', expect.anything());
    expect(dialog(el)?.querySelector('.file-preview-placeholder.error')).toBeNull();
  });
});
