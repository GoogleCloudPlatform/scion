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

import { describe, expect, it, vi } from 'vitest';
import { withDownloadParam, triggerFileDownload } from './download-url.js';
import { buildAttachmentDownloadUrl } from './chat-file-links.js';

describe('withDownloadParam', () => {
  it('appends ?download=1 to a bare path', () => {
    expect(withDownloadParam('/api/v1/chat/attachments/att-1')).toBe(
      '/api/v1/chat/attachments/att-1?download=1'
    );
  });

  it('preserves existing query parameters and is idempotent', () => {
    expect(withDownloadParam('/api/v1/artifacts/a1/files/readme.md?foo=bar')).toBe(
      '/api/v1/artifacts/a1/files/readme.md?foo=bar&download=1'
    );
    expect(withDownloadParam('/api/v1/artifacts/a1/files/readme.md?download=1')).toBe(
      '/api/v1/artifacts/a1/files/readme.md?download=1'
    );
  });

  it('preserves hash fragments', () => {
    expect(withDownloadParam('/api/v1/chat/attachments/att-1#frag')).toBe(
      '/api/v1/chat/attachments/att-1?download=1#frag'
    );
    expect(withDownloadParam('/api/v1/chat/attachments/att-1?x=1#frag')).toBe(
      '/api/v1/chat/attachments/att-1?x=1&download=1#frag'
    );
  });
});

describe('buildAttachmentDownloadUrl', () => {
  it('builds a validated forced-attachment download URL', () => {
    expect(buildAttachmentDownloadUrl('att-123')).toBe(
      '/api/v1/chat/attachments/att-123?download=1'
    );
  });

  it('rejects unsafe attachment ids', () => {
    expect(() => buildAttachmentDownloadUrl('../secret')).toThrow(/unsafe attachment id/);
    expect(() => buildAttachmentDownloadUrl('a/b')).toThrow(/unsafe attachment id/);
  });
});

describe('triggerFileDownload', () => {
  it('creates a hidden anchor with href and download, clicks it, and removes it', () => {
    let capturedHref: string | null = null;
    let capturedDownload: string | null = null;
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
      this: HTMLAnchorElement
    ) {
      capturedHref = this.getAttribute('href');
      capturedDownload = this.getAttribute('download');
    });

    try {
      triggerFileDownload('/api/v1/chat/attachments/att-1?download=1', 'notes.md');
      expect(clickSpy).toHaveBeenCalledTimes(1);
      expect(capturedHref).toBe('/api/v1/chat/attachments/att-1?download=1');
      expect(capturedDownload).toBe('notes.md');
      expect(document.body.querySelector('a')).toBeNull();
    } finally {
      clickSpy.mockRestore();
    }
  });
});
