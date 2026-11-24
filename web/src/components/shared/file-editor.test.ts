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

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

const showConfirmMock = vi.fn<(message: string, options?: unknown) => Promise<boolean>>();

vi.mock('./confirm-dialog.js', () => ({
  showConfirm: (...args: [message: string, options?: unknown]) => showConfirmMock(...args),
}));

import type { FileEditorDataSource, ScionFileEditor } from './file-editor.js';

function makeDataSource(content = 'initial'): FileEditorDataSource {
  return {
    getFileContent: vi.fn((path: string) =>
      Promise.resolve({
        path,
        content,
        size: content.length,
        modTime: '2026-01-01T00:00:00Z',
        encoding: 'utf-8',
      })
    ),
    saveFileContent: vi.fn(() => Promise.resolve({ modTime: '2026-01-01T00:01:00Z' })),
  };
}

describe('scion-file-editor requestClose', () => {
  let el: ScionFileEditor | null = null;

  beforeAll(async () => {
    await import('./file-editor.js');
  }, 60_000);

  beforeEach(() => {
    showConfirmMock.mockReset();
  });

  afterEach(() => {
    el?.remove();
    el = null;
  });

  async function mountEditor(content = 'initial'): Promise<ScionFileEditor> {
    const editor = document.createElement('scion-file-editor');
    editor.filePath = 'notes.md';
    editor.dataSource = makeDataSource(content);
    document.body.appendChild(editor);
    await editor.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await editor.updateComplete;
    el = editor;
    return editor;
  }

  function editContent(editor: ScionFileEditor, nextContent: string): void {
    const codeEditor = editor.shadowRoot!.querySelector('scion-code-editor');
    expect(codeEditor).not.toBeNull();
    codeEditor!.dispatchEvent(
      new CustomEvent('content-changed', {
        detail: { content: nextContent },
        bubbles: true,
        composed: true,
      })
    );
  }

  it('closes without prompting when clean, dispatches editor-closed, and resolves to true', async () => {
    const editor = await mountEditor('hello');
    const closed = vi.fn();
    editor.addEventListener('editor-closed', closed);

    expect(editor.dirty).toBe(false);
    const result = await editor.requestClose();

    expect(result).toBe(true);
    expect(showConfirmMock).not.toHaveBeenCalled();
    expect(closed).toHaveBeenCalledTimes(1);
    expect((closed.mock.calls[0][0] as CustomEvent).bubbles).toBe(true);
    expect((closed.mock.calls[0][0] as CustomEvent).composed).toBe(true);
  });

  it('prompts when dirty and cancels without dispatching editor-closed when declined', async () => {
    const editor = await mountEditor('hello');
    const closed = vi.fn();
    editor.addEventListener('editor-closed', closed);

    editContent(editor, 'hello edited');
    await editor.updateComplete;
    expect(editor.dirty).toBe(true);

    showConfirmMock.mockResolvedValueOnce(false);
    const result = await editor.requestClose();

    expect(result).toBe(false);
    expect(showConfirmMock).toHaveBeenCalledTimes(1);
    expect(showConfirmMock).toHaveBeenCalledWith('You have unsaved changes. Close anyway?');
    expect(closed).not.toHaveBeenCalled();
  });

  it('prompts when dirty and dispatches editor-closed when confirmed', async () => {
    const editor = await mountEditor('hello');
    const closed = vi.fn();
    editor.addEventListener('editor-closed', closed);

    editContent(editor, 'hello edited');
    await editor.updateComplete;

    showConfirmMock.mockResolvedValueOnce(true);
    const result = await editor.requestClose();

    expect(result).toBe(true);
    expect(showConfirmMock).toHaveBeenCalledTimes(1);
    expect(closed).toHaveBeenCalledTimes(1);
  });

  it('toolbar Close button delegates to requestClose and respects dirty confirmation', async () => {
    const editor = await mountEditor('hello');
    const closed = vi.fn();
    editor.addEventListener('editor-closed', closed);

    editContent(editor, 'dirty');
    await editor.updateComplete;

    const closeBtn = Array.from(editor.shadowRoot!.querySelectorAll('sl-button')).find(
      (b) => b.textContent?.trim() === 'Close'
    );
    expect(closeBtn).toBeTruthy();

    showConfirmMock.mockResolvedValueOnce(false);
    closeBtn!.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await new Promise((r) => setTimeout(r, 0));

    expect(showConfirmMock).toHaveBeenCalledTimes(1);
    expect(closed).not.toHaveBeenCalled();
  });

  it('coalesces concurrent requestClose calls while a confirm dialog is pending', async () => {
    const editor = await mountEditor('hello');
    const closed = vi.fn();
    editor.addEventListener('editor-closed', closed);

    editContent(editor, 'dirty');
    await editor.updateComplete;

    let resolveConfirm!: (value: boolean) => void;
    showConfirmMock.mockReturnValueOnce(
      new Promise<boolean>((resolve) => {
        resolveConfirm = resolve;
      })
    );

    const p1 = editor.requestClose();
    const p2 = editor.requestClose();
    expect(showConfirmMock).toHaveBeenCalledTimes(1);

    resolveConfirm(true);
    expect(await Promise.all([p1, p2])).toEqual([true, true]);
    expect(closed).toHaveBeenCalledTimes(1);
  });
});
