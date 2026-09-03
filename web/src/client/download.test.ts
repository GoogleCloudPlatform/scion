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
 * Tests for the shared downloadJsonFile helper.
 *
 * Covers:
 *   - Blob content matches JSON-serialised input
 *   - Correct MIME type on Blob
 *   - Anchor element receives the expected filename
 *   - Anchor is appended, clicked, and removed from the DOM
 *   - Object URL is revoked after a delay
 *   - Errors from Blob/URL APIs propagate
 */

import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { downloadJsonFile } from './download.js';

describe('downloadJsonFile', () => {
  let createObjectURLSpy: ReturnType<typeof vi.spyOn>;
  let revokeObjectURLSpy: ReturnType<typeof vi.spyOn>;
  let appendChildSpy: ReturnType<typeof vi.spyOn>;
  let removeChildSpy: ReturnType<typeof vi.spyOn>;
  let clickSpy: ReturnType<typeof vi.fn>;
  let capturedAnchor: HTMLAnchorElement | null = null;

  beforeEach(() => {
    createObjectURLSpy = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    revokeObjectURLSpy = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    appendChildSpy = vi.spyOn(document.body, 'appendChild').mockImplementation((node) => node);
    removeChildSpy = vi.spyOn(document.body, 'removeChild').mockImplementation((node) => node);

    clickSpy = vi.fn();
    const originalCreateElement = document.createElement.bind(document);
    vi.spyOn(document, 'createElement').mockImplementation((tag: string) => {
      if (tag === 'a') {
        const anchor = originalCreateElement('a');
        anchor.click = clickSpy;
        capturedAnchor = anchor;
        return anchor;
      }
      return originalCreateElement(tag);
    });
  });

  afterEach(() => {
    capturedAnchor = null;
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('creates a Blob with correct JSON content and MIME type', () => {
    const data = { version: '1', roles: [{ name: 'test' }] };
    downloadJsonFile(data, 'export.json');

    expect(createObjectURLSpy).toHaveBeenCalledOnce();
    const blob = createObjectURLSpy.mock.calls[0][0] as Blob;
    expect(blob).toBeInstanceOf(Blob);
    expect(blob.type).toBe('application/json');
  });

  it('serialises data as pretty-printed JSON', async () => {
    const data = { version: '1', roles: [{ name: 'test' }] };
    downloadJsonFile(data, 'export.json');

    const blob = createObjectURLSpy.mock.calls[0][0] as Blob;
    const text = await blob.text();
    expect(text).toBe(JSON.stringify(data, null, 2));
  });

  it('sets the download filename on the anchor element', () => {
    downloadJsonFile({ hello: 'world' }, 'my-file.json');

    expect(capturedAnchor).not.toBeNull();
    expect(capturedAnchor!.download).toBe('my-file.json');
  });

  it('sets the blob URL as the anchor href', () => {
    downloadJsonFile({ hello: 'world' }, 'file.json');

    expect(capturedAnchor).not.toBeNull();
    expect(capturedAnchor!.href).toContain('blob:mock-url');
  });

  it('appends anchor to body, clicks it, then removes it', () => {
    downloadJsonFile({}, 'file.json');

    expect(appendChildSpy).toHaveBeenCalledOnce();
    expect(clickSpy).toHaveBeenCalledOnce();
    expect(removeChildSpy).toHaveBeenCalledOnce();

    // appendChild must happen before click
    const appendOrder = appendChildSpy.mock.invocationCallOrder[0];
    const clickOrder = clickSpy.mock.invocationCallOrder[0];
    const removeOrder = removeChildSpy.mock.invocationCallOrder[0];
    expect(appendOrder).toBeLessThan(clickOrder);
    expect(clickOrder).toBeLessThan(removeOrder);
  });

  it('revokes the object URL after a delay', () => {
    vi.useFakeTimers();
    downloadJsonFile({}, 'file.json');

    // URL should not be revoked synchronously
    expect(revokeObjectURLSpy).not.toHaveBeenCalled();

    // Advance timer to trigger the revocation
    vi.advanceTimersByTime(200);
    expect(revokeObjectURLSpy).toHaveBeenCalledOnce();
    expect(revokeObjectURLSpy).toHaveBeenCalledWith('blob:mock-url');
  });

  it('propagates errors from createObjectURL', () => {
    createObjectURLSpy.mockImplementation(() => {
      throw new Error('Blob URL creation failed');
    });

    expect(() => downloadJsonFile({}, 'file.json')).toThrow('Blob URL creation failed');
  });
});
