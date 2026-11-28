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

import { describe, it, expect, vi, afterEach } from 'vitest';
import {
  DEFAULT_UPLOAD_LIMITS,
  MAX_LEGACY_WORKSPACE_FILE_SIZE,
  MAX_WORKSPACE_REQUEST_BODY,
  fileTooLargeMessage,
  formatUploadSize,
  getUploadLimits,
  legacyWorkspaceMaxFileSize,
  validateWorkspaceUploadFiles,
} from './upload-limits.js';
import { invalidatePublicSettings } from './server-feature-flags.js';

describe('upload-limits', () => {
  afterEach(() => {
    invalidatePublicSettings();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('formats byte sizes in binary units', () => {
    expect(formatUploadSize(900)).toBe('900 B');
    expect(formatUploadSize(1024)).toBe('1 KiB');
    expect(formatUploadSize(10 * 1024 * 1024)).toBe('10 MiB');
    expect(formatUploadSize(50 * 1024 * 1024)).toBe('50 MiB');
    expect(formatUploadSize(1.5 * 1024 * 1024 * 1024)).toBe('1.5 GiB');
  });

  it('falls back to hub defaults when public settings omit uploads', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response(JSON.stringify({}), { status: 200 })))
    );
    expect(await getUploadLimits()).toEqual(DEFAULT_UPLOAD_LIMITS);
  });

  it('reads numeric limits from public settings', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              uploads: {
                maxAttachmentSize: 32 << 20,
                maxWorkspaceFileSize: 80 << 20,
              },
            }),
            { status: 200 }
          )
        )
      )
    );
    expect(await getUploadLimits()).toEqual({
      maxAttachmentSize: 32 << 20,
      maxWorkspaceFileSize: 80 << 20,
    });
  });

  it('validates per-file and total request limits', () => {
    expect(validateWorkspaceUploadFiles([{ name: 'a.bin', size: 60 << 20 }], 50 << 20)).toBe(
      fileTooLargeMessage('a.bin', 50 << 20)
    );

    expect(
      validateWorkspaceUploadFiles(
        [
          { name: 'a.bin', size: 60 << 20 },
          { name: 'b.bin', size: 50 << 20 },
        ],
        80 << 20,
        MAX_WORKSPACE_REQUEST_BODY
      )
    ).toContain('100 MiB');

    expect(
      validateWorkspaceUploadFiles(
        [
          { name: 'a.bin', size: 40 << 20 },
          { name: 'b.bin', size: 40 << 20 },
        ],
        50 << 20
      )
    ).toBeNull();
  });

  it('clamps legacy single-request workspace limit to 100 MiB', () => {
    expect(legacyWorkspaceMaxFileSize(1 << 30)).toBe(MAX_LEGACY_WORKSPACE_FILE_SIZE);
    expect(legacyWorkspaceMaxFileSize(80 << 20)).toBe(80 << 20);
  });
});
