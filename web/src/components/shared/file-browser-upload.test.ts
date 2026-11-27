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
import { setFeatureFlag, resetServerFlagStateForTests } from '../../utils/feature-flags.js';
import {
  _setUploadTestConfig,
  UploadCancelledError,
  UploadHttpError,
} from '../../client/chunked-upload.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import type { FileBrowserDataSource, FileListResult, ScionFileBrowser } from './file-browser.js';

let WorkspaceDataSourceCtor: typeof import('./file-browser.js').WorkspaceFileBrowserDataSource;

beforeAll(async () => {
  const mod = await import('./file-browser.js');
  WorkspaceDataSourceCtor = mod.WorkspaceFileBrowserDataSource;
});

beforeEach(() => {
  resetServerFlagStateForTests();
  delete window.__SCION_FEATURES__;
  _setUploadTestConfig({ retryDelayMs: 0, maxBackoffMs: 0 });
});

afterEach(() => {
  document.body.innerHTML = '';
  delete window.__SCION_FEATURES__;
  resetServerFlagStateForTests();
  _setUploadTestConfig(null);
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function makeFileList(files: File[]): FileList {
  const list = Object.assign([...files], {
    item: (i: number) => files[i] ?? null,
  });
  return list as unknown as FileList;
}

describe('WorkspaceFileBrowserDataSource upload routing', () => {
  it('uses legacy single multipart POST when hub.large_uploads is off', async () => {
    setFeatureFlag('hub.large_uploads', false);
    const uploadUrls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const url = requestUrl(input);
        if (url === '/api/v1/settings/public') {
          return Promise.resolve(
            new Response(JSON.stringify({}), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        uploadUrls.push(url);
        return Promise.resolve(
          new Response(JSON.stringify({ files: [] }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );

    const ds = new WorkspaceDataSourceCtor('proj-1');
    // Even a >8 MiB file uses the legacy POST /workspace/files when flag is off.
    const big = new File([new Uint8Array(10)], 'big.bin');
    Object.defineProperty(big, 'size', { value: 9 * 1024 * 1024 });
    await ds.uploadFiles(makeFileList([big]));

    expect(uploadUrls).toEqual(['/api/v1/projects/proj-1/workspace/files']);
  });

  it('uses chunked upload session when hub.large_uploads is on and file exceeds chunkBytes', async () => {
    setFeatureFlag('hub.large_uploads', true);
    const calls: Array<{ method: string; url: string }> = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request, init?: RequestInit) => {
        const url = requestUrl(input);
        const method = (init?.method ?? 'GET').toUpperCase();
        calls.push({ method, url });
        if (url.endsWith('/workspace/uploads') && method === 'POST') {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                id: 'upl_abc',
                transport: 'hub_chunked',
                chunkBytes: 8 * 1024 * 1024,
                maxBytes: 1 << 30,
                received: 0,
                expiresAt: new Date(Date.now() + 3600_000).toISOString(),
              }),
              { status: 201, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        if (method === 'PUT') {
          const parsed = new URL(url, 'http://hub.test');
          const offset = Number(parsed.searchParams.get('offset') ?? '0');
          const bodyObj = init?.body as { size?: unknown } | null | undefined;
          const size = typeof bodyObj?.size === 'number' ? bodyObj.size : 0;
          return Promise.resolve(
            new Response(
              JSON.stringify({
                received: offset + size,
                expiresAt: new Date(Date.now() + 3600_000).toISOString(),
              }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify({ path: 'big.bin', size: 9 * 1024 * 1024 }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );

    const ds = new WorkspaceDataSourceCtor('proj-1');
    const fakeLarge = new File([new Uint8Array(10)], 'big.bin');
    Object.defineProperty(fakeLarge, 'size', { value: 9 * 1024 * 1024 });
    fakeLarge.slice = (start?: number, end?: number) => {
      const len = (end ?? 9 * 1024 * 1024) - (start ?? 0);
      return { size: len } as Blob;
    };

    const progress: Array<[number, number]> = [];
    await ds.uploadFiles(makeFileList([fakeLarge]), {
      onProgress: (sent, total) => progress.push([sent, total]),
    });

    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      'POST /api/v1/projects/proj-1/workspace/uploads',
      'PUT /api/v1/projects/proj-1/workspace/uploads/upl_abc?offset=0',
      'PUT /api/v1/projects/proj-1/workspace/uploads/upl_abc?offset=8388608',
      'POST /api/v1/projects/proj-1/workspace/uploads/upl_abc/complete',
    ]);
    expect(progress.at(-1)).toEqual([9 * 1024 * 1024, 9 * 1024 * 1024]);
  });
});

describe('ScionFileBrowser upload UI', () => {
  function mountBrowser(dataSource: FileBrowserDataSource): ScionFileBrowser {
    const el = document.createElement('scion-file-browser') as ScionFileBrowser;
    el.editable = true;
    el.dataSource = dataSource;
    document.body.appendChild(el);
    return el;
  }

  function makeSource(
    uploadImpl: FileBrowserDataSource['uploadFiles'],
    files: FileListResult['files'] = []
  ): FileBrowserDataSource {
    return {
      listFiles: vi.fn(() =>
        Promise.resolve({
          files,
          totalSize: files.reduce((s, f) => s + f.size, 0),
          totalCount: files.length,
        })
      ),
      deleteFile: vi.fn(),
      uploadFiles: vi.fn(uploadImpl),
      getDownloadUrl: (p) => `/download/${p}`,
      getPreviewUrl: (p) => `/preview/${p}`,
    };
  }

  it('shows progress percentage and cancel button, and aborts without error on cancel', async () => {
    let reportProgress: ((sent: number, total: number) => void) | undefined;
    let receivedSignal: AbortSignal | undefined;
    const source = makeSource((_files, opts) => {
      reportProgress = opts?.onProgress;
      receivedSignal = opts?.signal;
      return new Promise<void>((_resolve, reject) => {
        opts?.signal?.addEventListener('abort', () => reject(new UploadCancelledError()));
      });
    });

    const el = mountBrowser(source);
    await el.updateComplete;

    const input = el.shadowRoot!.querySelector('#file-browser-input') as HTMLInputElement;
    Object.defineProperty(input, 'files', {
      value: makeFileList([new File(['abc'], 'a.bin')]),
      configurable: true,
    });
    input.dispatchEvent(new Event('change'));

    await vi.waitFor(() => expect(reportProgress).toBeDefined());
    reportProgress!(50, 100);
    await el.updateComplete;

    const status = el.shadowRoot!.querySelector('.upload-status');
    expect(status?.textContent?.trim()).toBe('Uploading 50%');

    const cancelBtn = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find(
      (b) => b.textContent?.trim() === 'Cancel'
    ) as HTMLElement;
    expect(cancelBtn).toBeTruthy();
    cancelBtn.click();

    await vi.waitFor(() => expect(receivedSignal?.aborted).toBe(true));
    await el.updateComplete;

    expect(el.shadowRoot!.querySelector('.upload-status')).toBeNull();
    expect(el.shadowRoot!.querySelector('.error-state')).toBeNull();
  });

  it('shows "Large uploads were disabled." when server returns experiment_disabled', async () => {
    const source = makeSource(() =>
      Promise.reject(
        new UploadHttpError('Large uploads were disabled.', 404, 'experiment_disabled')
      )
    );

    const el = mountBrowser(source);
    await el.updateComplete;

    const input = el.shadowRoot!.querySelector('#file-browser-input') as HTMLInputElement;
    Object.defineProperty(input, 'files', {
      value: makeFileList([new File(['abc'], 'a.bin')]),
      configurable: true,
    });
    input.dispatchEvent(new Event('change'));

    await vi.waitFor(() => {
      const errEl = el.shadowRoot!.querySelector('.error-state');
      expect(errEl?.textContent?.trim()).toBe('Large uploads were disabled.');
    });
  });

  it('hides .scion-upload-*.partial files even when showDotFiles is toggled on', async () => {
    const source = makeSource(
      () => Promise.resolve(),
      [
        { path: 'readme.md', size: 12, modTime: '2026-01-01T00:00:00Z', mode: '-rw-r--r--' },
        { path: '.env', size: 10, modTime: '2026-01-01T00:00:00Z', mode: '-rw-r--r--' },
        {
          path: '.scion-upload-upl_123.partial',
          size: 1024,
          modTime: '2026-01-01T00:00:00Z',
          mode: '-rw-r--r--',
        },
        {
          path: 'sub/.scion-upload-upl_456.partial',
          size: 1024,
          modTime: '2026-01-01T00:00:00Z',
          mode: '-rw-r--r--',
        },
      ]
    );

    const el = mountBrowser(source);
    await vi.waitFor(() => {
      expect(el.shadowRoot!.textContent).toContain('readme.md');
    });

    // Enable showDotFiles.
    const checkbox = el.shadowRoot!.querySelector('.dot-files-toggle') as HTMLElement & {
      checked: boolean;
    };
    checkbox.checked = true;
    checkbox.dispatchEvent(new CustomEvent('sl-change', { bubbles: true }));
    await el.updateComplete;

    const text = el.shadowRoot!.textContent ?? '';
    expect(text).toContain('.env');
    expect(text).not.toContain('.scion-upload-upl_123.partial');
    expect(text).not.toContain('.scion-upload-upl_456.partial');
  });
});
