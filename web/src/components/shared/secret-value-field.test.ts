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

import { afterEach, describe, expect, it, vi } from 'vitest';
import './secret-value-field.js';
import type {
  ScionSecretValueField,
  SecretValueChangeDetail,
  SecretValueErrorDetail,
} from './secret-value-field.js';

async function mountField(
  props: Partial<ScionSecretValueField> = {}
): Promise<ScionSecretValueField> {
  const el = document.createElement('scion-secret-value-field') as ScionSecretValueField;
  el.secretKey = 'GITHUB_TOKEN';
  el.rowId = 'row-1';
  Object.assign(el, props);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function dispatchPaste(target: EventTarget, text: string): ClipboardEvent {
  const pasteEvent = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
  Object.defineProperty(pasteEvent, 'clipboardData', {
    value: { getData: (t: string) => (t === 'text/plain' ? text : '') },
  });
  target.dispatchEvent(pasteEvent);
  return pasteEvent;
}

function dispatchDrop(target: EventTarget, text: string): DragEvent {
  const dropEvent = new Event('drop', { bubbles: true, cancelable: true }) as DragEvent;
  Object.defineProperty(dropEvent, 'dataTransfer', {
    value: { getData: (t: string) => (t === 'text/plain' ? text : '') },
  });
  target.dispatchEvent(dropEvent);
  return dropEvent;
}

describe('scion-secret-value-field', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('renders native password input while masked, toggles Show/Hide with aria-pressed, and focuses textarea on Show', async () => {
    const el = await mountField({ secretKey: 'API_KEY', label: 'Value' });
    const shadow = el.shadowRoot!;

    expect(shadow.querySelector('.field-label')?.textContent).toBe('Value');
    const input = shadow.querySelector('input[data-key="API_KEY"]') as HTMLInputElement;
    expect(input).not.toBeNull();
    expect(input.type).toBe('password');
    expect(input.autocomplete).toBe('off');
    expect(input.getAttribute('data-lpignore')).toBe('true');
    expect(input.getAttribute('data-1p-ignore')).toBe('true');
    expect(shadow.querySelector('textarea')).toBeNull();
    expect(shadow.querySelector('input[type="file"]')).toBeNull();

    const changes: string[] = [];
    el.addEventListener('secret-value-change', (e) => {
      changes.push((e as CustomEvent<SecretValueChangeDetail>).detail.value);
    });

    input.value = 's3cr3t-value';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    expect(el.value).toBe('s3cr3t-value');
    expect(changes).toEqual(['s3cr3t-value']);

    const toggleBtn = shadow.querySelector(
      'button[data-toggle-visibility="API_KEY"]'
    ) as HTMLButtonElement;
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');

    toggleBtn.click();
    await el.updateComplete;

    const ta = shadow.querySelector('textarea[data-key="API_KEY"]') as HTMLTextAreaElement;
    expect(ta).not.toBeNull();
    expect(ta.value).toBe('s3cr3t-value');
    expect(shadow.activeElement).toBe(ta);
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('true');

    toggleBtn.click();
    await el.updateComplete;
    expect(shadow.querySelector('textarea')).toBeNull();
    const maskedAgain = shadow.querySelector('input[data-key="API_KEY"]') as HTMLInputElement;
    expect(maskedAgain.type).toBe('password');
    expect(maskedAgain.value).toBe('s3cr3t-value');
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');
  });

  it('captures multi-line paste verbatim while masked, hides plaintext from DOM, and preserves across Show/Hide', async () => {
    const multiline = '-----BEGIN PRIVATE KEY-----\nline-two-secret\n-----END PRIVATE KEY-----';
    const el = await mountField({ secretKey: 'DEPLOY_KEY', allowFileLoad: true });
    const shadow = el.shadowRoot!;
    const input = shadow.querySelector('input[data-key="DEPLOY_KEY"]') as HTMLInputElement;

    const ev = dispatchPaste(input, multiline);
    await el.updateComplete;

    expect(ev.defaultPrevented).toBe(true);
    expect(el.value).toBe(multiline);
    expect(input.value).toBe('');
    expect(shadow.querySelector('textarea')).toBeNull();
    expect(shadow.textContent).not.toContain('line-two-secret');
    expect(shadow.innerHTML).not.toContain('line-two-secret');

    const badge = shadow.querySelector('[data-testid="file-value-masked"]');
    expect(badge).not.toBeNull();
    expect(badge?.getAttribute('aria-label')).toBe('Secret value for DEPLOY_KEY entered, hidden');

    const toggleBtn = shadow.querySelector('.toggle-visibility') as HTMLButtonElement;
    toggleBtn.click();
    await el.updateComplete;
    const ta = shadow.querySelector('textarea[data-key="DEPLOY_KEY"]') as HTMLTextAreaElement;
    expect(ta.value).toBe(multiline);

    toggleBtn.click();
    await el.updateComplete;
    expect(shadow.querySelector('textarea')).toBeNull();
    expect((shadow.querySelector('input[data-key="DEPLOY_KEY"]') as HTMLInputElement).value).toBe(
      ''
    );
    expect(el.value).toBe(multiline);
  });

  it('composes type-then-paste at selection and captures beforeinput/drop multi-line text', async () => {
    const el = await mountField({ secretKey: 'TOKEN' });
    const input = el.shadowRoot!.querySelector('input[data-key="TOKEN"]') as HTMLInputElement;

    // 1. Single-line paste on non-captured field does not preventDefault
    input.value = 'abc';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    const singlePaste = dispatchPaste(input, 'def');
    expect(singlePaste.defaultPrevented).toBe(false);

    // 2. Multi-line paste with caret in the middle composes at selection
    input.setSelectionRange(1, 1);
    const multiPaste = dispatchPaste(input, 'X\nY');
    await el.updateComplete;
    expect(multiPaste.defaultPrevented).toBe(true);
    expect(el.value).toBe('aX\nYbc');

    // 3. beforeinput insertText with \r\n replaces captured value verbatim
    const bi = new InputEvent('beforeinput', {
      bubbles: true,
      cancelable: true,
      inputType: 'insertText',
      data: 'Y1\r\nY2',
    });
    input.dispatchEvent(bi);
    await el.updateComplete;
    expect(bi.defaultPrevented).toBe(true);
    expect(el.value).toBe('Y1\r\nY2');

    // 4. beforeinput insertReplacementText replaces captured value verbatim
    const rep = new InputEvent('beforeinput', {
      bubbles: true,
      cancelable: true,
      inputType: 'insertReplacementText',
      data: 'R1\nR2',
    });
    input.dispatchEvent(rep);
    await el.updateComplete;
    expect(rep.defaultPrevented).toBe(true);
    expect(el.value).toBe('R1\nR2');

    // 5. drop event with multi-line text replaces captured value verbatim
    const dropEv = dispatchDrop(input, 'D1\nD2\nD3');
    await el.updateComplete;
    expect(dropEv.defaultPrevented).toBe(true);
    expect(el.value).toBe('D1\nD2\nD3');
  });

  it('clears a captured value on Backspace, Delete, and delete* beforeinput', async () => {
    const el = await mountField({ secretKey: 'TOKEN', allowFileLoad: true });
    const shadow = el.shadowRoot!;
    const input = shadow.querySelector('input[data-key="TOKEN"]') as HTMLInputElement;

    dispatchPaste(input, 'line1\nline2');
    await el.updateComplete;
    expect(shadow.querySelector('[data-testid="file-value-masked"]')).not.toBeNull();

    const delBi = new InputEvent('beforeinput', {
      bubbles: true,
      cancelable: true,
      inputType: 'deleteContentBackward',
    });
    input.dispatchEvent(delBi);
    await el.updateComplete;
    expect(delBi.defaultPrevented).toBe(true);
    expect(el.value).toBe('');
    expect(shadow.querySelector('[data-testid="file-value-masked"]')).toBeNull();

    dispatchPaste(input, 'line1\nline2');
    await el.updateComplete;
    const bs = new KeyboardEvent('keydown', { key: 'Backspace', bubbles: true, cancelable: true });
    input.dispatchEvent(bs);
    await el.updateComplete;
    expect(bs.defaultPrevented).toBe(true);
    expect(el.value).toBe('');

    dispatchPaste(input, 'line1\nline2');
    await el.updateComplete;
    const delKey = new KeyboardEvent('keydown', { key: 'Delete', bubbles: true, cancelable: true });
    input.dispatchEvent(delKey);
    await el.updateComplete;
    expect(delKey.defaultPrevented).toBe(true);
    expect(el.value).toBe('');
  });

  it('preserves CRLF when editing revealed all-CRLF value and guards mixed/bare-CR as readonly', async () => {
    const el = await mountField({ secretKey: 'CRLF_KEY', allowFileLoad: true });
    const shadow = el.shadowRoot!;
    const input = shadow.querySelector('input[data-key="CRLF_KEY"]') as HTMLInputElement;

    dispatchPaste(input, 'AAA\r\nBBB\r\nCCC');
    await el.updateComplete;

    const toggleBtn = shadow.querySelector('.toggle-visibility') as HTMLButtonElement;
    toggleBtn.click();
    await el.updateComplete;

    const ta = shadow.querySelector('textarea[data-key="CRLF_KEY"]') as HTMLTextAreaElement;
    expect(ta.readOnly).toBe(false);
    expect(shadow.querySelector('[data-testid="mixed-line-endings-note"]')).toBeNull();

    ta.value = 'AAA\nBBB\nCCCX';
    ta.dispatchEvent(new Event('input', { bubbles: true }));
    await el.updateComplete;
    expect(el.value).toBe('AAA\r\nBBB\r\nCCCX');

    // Paste LF into CRLF revealed textarea -> splices raw and becomes mixed readonly
    ta.setSelectionRange(ta.value.length, ta.value.length);
    const lfPaste = dispatchPaste(ta, '\nL1\nL2');
    await el.updateComplete;
    expect(lfPaste.defaultPrevented).toBe(true);
    expect(ta.readOnly).toBe(true);
    expect(
      shadow.querySelector('[data-testid="mixed-line-endings-note"]')?.textContent?.trim()
    ).toBe('This value has mixed line endings; paste or load to replace it');
    expect(el.value).toBe('AAA\r\nBBB\r\nCCCX\nL1\nL2');

    // Synthetic input on mixed readonly textarea does not alter stored value
    ta.value = 'corrupted';
    ta.dispatchEvent(new Event('input', { bubbles: true }));
    await el.updateComplete;
    expect(el.value).toBe('AAA\r\nBBB\r\nCCCX\nL1\nL2');
  });

  it('blocks undo/redo on both masked and revealed controls', async () => {
    const el = await mountField({ secretKey: 'UNDO_KEY' });
    const shadow = el.shadowRoot!;
    const input = shadow.querySelector('input[data-key="UNDO_KEY"]') as HTMLInputElement;

    const maskedUndoBi = new InputEvent('beforeinput', {
      inputType: 'historyUndo',
      bubbles: true,
      cancelable: true,
    });
    input.dispatchEvent(maskedUndoBi);
    expect(maskedUndoBi.defaultPrevented).toBe(true);

    const maskedUndoKd = new KeyboardEvent('keydown', {
      key: 'z',
      ctrlKey: true,
      bubbles: true,
      cancelable: true,
    });
    input.dispatchEvent(maskedUndoKd);
    expect(maskedUndoKd.defaultPrevented).toBe(true);

    (shadow.querySelector('.toggle-visibility') as HTMLButtonElement).click();
    await el.updateComplete;
    const ta = shadow.querySelector('textarea[data-key="UNDO_KEY"]') as HTMLTextAreaElement;

    const revRedoBi = new InputEvent('beforeinput', {
      inputType: 'historyRedo',
      bubbles: true,
      cancelable: true,
    });
    ta.dispatchEvent(revRedoBi);
    expect(revRedoBi.defaultPrevented).toBe(true);

    const revRedoKd = new KeyboardEvent('keydown', {
      key: 'y',
      metaKey: true,
      bubbles: true,
      cancelable: true,
    });
    ta.dispatchEvent(revRedoKd);
    expect(revRedoKd.defaultPrevented).toBe(true);
  });

  it('loads UTF-8 files (preserving BOM), rejects invalid/oversized/NUL/empty files, and cancels in-flight reads on clear', async () => {
    const el = await mountField({ secretKey: 'CERT_KEY', allowFileLoad: true, showError: true });
    const shadow = el.shadowRoot!;
    const fileInput = shadow.querySelector(
      'input[type="file"][data-file-input="CERT_KEY"]'
    ) as HTMLInputElement;
    expect(fileInput.getAttribute('tabindex')).toBe('-1');

    const errors: string[] = [];
    el.addEventListener('secret-value-error', (e) => {
      errors.push((e as CustomEvent<SecretValueErrorDetail>).detail.message);
    });

    const triggerFile = async (file: File): Promise<void> => {
      Object.defineProperty(fileInput, 'files', { value: [file], configurable: true });
      fileInput.dispatchEvent(new Event('change'));
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
    };

    // 1. Oversized > 64 KiB
    await triggerFile(new File([new Uint8Array(64 * 1024 + 1)], 'big.pem'));
    expect(shadow.querySelector('[data-testid="secret-value-error"]')?.textContent).toContain(
      '64 KiB'
    );
    expect(fileInput.value).toBe('');
    expect(el.value).toBe('');

    // 2. Invalid UTF-8
    await triggerFile(new File([new Uint8Array([0xff, 0xfe, 0x00, 0x01])], 'bin.der'));
    expect(shadow.querySelector('[data-testid="secret-value-error"]')?.textContent).toContain(
      'UTF-8'
    );
    expect(el.value).toBe('');

    // 3. NUL byte
    await triggerFile(new File(['hello\u0000world'], 'nul.bin', { type: 'text/plain' }));
    expect(shadow.querySelector('[data-testid="secret-value-error"]')?.textContent).toContain(
      'NUL'
    );
    expect(el.value).toBe('');

    // 4. Empty file
    await triggerFile(new File([''], 'empty.txt', { type: 'text/plain' }));
    expect(shadow.querySelector('[data-testid="secret-value-error"]')?.textContent).toContain(
      'empty'
    );
    expect(el.value).toBe('');

    // 5. Valid UTF-8 file with leading BOM; auto-hides if Show was active
    (shadow.querySelector('.toggle-visibility') as HTMLButtonElement).click();
    await el.updateComplete;
    expect(shadow.querySelector('textarea')).not.toBeNull();

    const bomBytes = new Uint8Array([0xef, 0xbb, 0xbf, 0x61, 0x62, 0x63, 0x0a]);
    await triggerFile(new File([bomBytes], 'bom.txt', { type: 'text/plain' }));
    expect(shadow.querySelector('[data-testid="secret-value-error"]')).toBeNull();
    expect(shadow.querySelector('textarea')).toBeNull();
    expect(shadow.querySelector('[data-testid="file-value-masked"]')).not.toBeNull();
    expect(el.value).toBe('\uFEFFabc\n');
    expect(errors[errors.length - 1]).toBe('');

    // 6. Selecting a failing or unreadable file clears the previous value immediately and reports a friendly error
    const unreadable = new File(['abc'], 'unreadable.pem', { type: 'text/plain' });
    vi.spyOn(unreadable, 'arrayBuffer').mockRejectedValue(new DOMException('NotReadableError'));
    await triggerFile(unreadable);
    expect(el.value).toBe('');
    expect(shadow.querySelector('[data-testid="file-value-masked"]')).toBeNull();
    expect(shadow.querySelector('[data-testid="secret-value-error"]')?.textContent?.trim()).toBe(
      'Could not read the file for CERT_KEY.'
    );

    // 7. Async file read cancelled by clear()
    let resolveBuf: ((buf: ArrayBuffer) => void) | null = null;
    const slowFile = new File(['delayed'], 'slow.pem', { type: 'text/plain' });
    vi.spyOn(slowFile, 'arrayBuffer').mockImplementation(
      () =>
        new Promise<ArrayBuffer>((resolve) => {
          resolveBuf = resolve;
        })
    );
    Object.defineProperty(fileInput, 'files', { value: [slowFile], configurable: true });
    fileInput.dispatchEvent(new Event('change'));
    await new Promise((r) => setTimeout(r, 0));

    el.clear();
    resolveBuf!(new TextEncoder().encode('delayed').buffer);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;
    expect(el.value).toBe('');
    expect(shadow.querySelector('[data-testid="file-value-masked"]')).toBeNull();
  });

  it('discards a pending file A read when superseded by a rejected file B or a typed edit', async () => {
    // 1. Pending file A -> select oversize file B (rejected) -> resolve A => no value, B error kept
    const el1 = await mountField({ secretKey: 'RACE_FILE', allowFileLoad: true, showError: true });
    const shadow1 = el1.shadowRoot!;
    const fileInput1 = shadow1.querySelector(
      'input[type="file"][data-file-input="RACE_FILE"]'
    ) as HTMLInputElement;

    let resolveA1!: (buf: ArrayBuffer) => void;
    const fileA1 = new File(['stale-file-a'], 'a.pem', { type: 'text/plain' });
    fileA1.arrayBuffer = (): Promise<ArrayBuffer> =>
      new Promise<ArrayBuffer>((res) => {
        resolveA1 = res;
      });
    Object.defineProperty(fileInput1, 'files', { value: [fileA1], configurable: true });
    fileInput1.dispatchEvent(new Event('change'));
    await el1.updateComplete;

    const oversizeB = new File([new Uint8Array(65 * 1024)], 'b.pem', { type: 'text/plain' });
    Object.defineProperty(fileInput1, 'files', { value: [oversizeB], configurable: true });
    fileInput1.dispatchEvent(new Event('change'));
    await el1.updateComplete;
    expect(shadow1.querySelector('[data-testid="secret-value-error"]')?.textContent).toContain(
      'exceeds the 64 KiB limit'
    );

    resolveA1(new TextEncoder().encode('stale-file-a').buffer);
    await new Promise((r) => setTimeout(r, 10));
    await el1.updateComplete;

    expect(el1.value).toBe('');
    expect(shadow1.querySelector('[data-testid="file-value-masked"]')).toBeNull();
    expect(shadow1.querySelector('[data-testid="secret-value-error"]')?.textContent).toContain(
      'exceeds the 64 KiB limit'
    );
    el1.remove();

    // 2. Pending file A -> user types a value -> resolve A => typed value kept
    const el2 = await mountField({ secretKey: 'RACE_FILE', allowFileLoad: true, showError: true });
    const shadow2 = el2.shadowRoot!;
    const fileInput2 = shadow2.querySelector(
      'input[type="file"][data-file-input="RACE_FILE"]'
    ) as HTMLInputElement;

    let resolveA2!: (buf: ArrayBuffer) => void;
    const fileA2 = new File(['stale-file-a'], 'a.pem', { type: 'text/plain' });
    fileA2.arrayBuffer = (): Promise<ArrayBuffer> =>
      new Promise<ArrayBuffer>((res) => {
        resolveA2 = res;
      });
    Object.defineProperty(fileInput2, 'files', { value: [fileA2], configurable: true });
    fileInput2.dispatchEvent(new Event('change'));
    await el2.updateComplete;

    const input2 = shadow2.querySelector('input[data-key="RACE_FILE"]') as HTMLInputElement;
    input2.value = 'typed-secret';
    input2.dispatchEvent(new Event('input'));
    await el2.updateComplete;

    resolveA2(new TextEncoder().encode('stale-file-a').buffer);
    await new Promise((r) => setTimeout(r, 10));
    await el2.updateComplete;

    expect(el2.value).toBe('typed-secret');
    expect(shadow2.querySelector('[data-testid="file-value-masked"]')).toBeNull();
    expect(shadow2.querySelector('[data-testid="secret-value-error"]')).toBeNull();
    el2.remove();
  });

  it('clears value on pagehide, clearToken increment, and disconnect', async () => {
    const el = await mountField({ secretKey: 'EPHEMERAL' });
    const input = el.shadowRoot!.querySelector('input[data-key="EPHEMERAL"]') as HTMLInputElement;
    input.value = 'secret-1';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    expect(el.value).toBe('secret-1');

    window.dispatchEvent(new Event('pagehide'));
    await el.updateComplete;
    expect(el.value).toBe('');
    expect(input.value).toBe('');

    input.value = 'secret-2';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    el.clearToken += 1;
    await el.updateComplete;
    expect(el.value).toBe('');

    input.value = 'secret-3';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    el.remove();
    expect(el.value).toBe('');
  });
});
