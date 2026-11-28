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
import { _setUploadTestConfig } from '../../client/chunked-upload.js';
import { invalidatePublicSettings } from '../../client/server-feature-flags.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import type { ScionTerminalPane } from './terminal-pane.js';

type TerminalPaneInternal = ScionTerminalPane & {
  projectId: string;
  loading: boolean;
  ownedSession: { state: { generation: number }; setFrontmost(front: boolean): void } | null;
  uploadEnabled: boolean;
  uploadTargetDir: string;
  uploadBasePath: string;
  isUploading: boolean;
  uploadStatus: string;
  _visible: boolean;
  _focused: boolean;
  _handleFileDrop(files: FileList): Promise<void>;
  sendData(data: string): void;
};

let paneCtorRegistered = false;

beforeAll(async () => {
  if (!paneCtorRegistered) {
    await import('./terminal-pane.js');
    paneCtorRegistered = true;
  }
});

beforeEach(() => {
  invalidatePublicSettings();
  resetServerFlagStateForTests();
  delete window.__SCION_FEATURES__;
  _setUploadTestConfig({ retryDelayMs: 0, maxBackoffMs: 0, abortWindowMs: 50 });
});

afterEach(() => {
  document.body.innerHTML = '';
  delete window.__SCION_FEATURES__;
  invalidatePublicSettings();
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

function makeFakeLargeFile(name: string, size: number): File {
  const f = new File([new Uint8Array(10)], name);
  Object.defineProperty(f, 'size', { value: size });
  f.slice = (start?: number, end?: number) => {
    const len = (end ?? size) - (start ?? 0);
    return { size: len } as Blob;
  };
  return f;
}

function mountPane(): { pane: TerminalPaneInternal; sent: string[] } {
  const pane = document.createElement('scion-terminal-pane') as TerminalPaneInternal;
  pane.projectId = 'proj-1';
  pane.loading = false;
  pane.ownedSession = { state: { generation: 1 }, setFrontmost: () => {} };
  pane.uploadEnabled = true;
  pane.uploadTargetDir = 'scratchpad';
  pane.uploadBasePath = '/scion-volumes/scratchpad';
  pane._visible = true;
  pane._focused = true;
  const sent: string[] = [];
  pane.sendData = (data: string) => {
    sent.push(data);
  };
  document.body.appendChild(pane);
  return { pane, sent };
}

describe('ScionTerminalPane drag-drop upload routing', () => {
  it('clamps legacy drop upload to 100 MiB when hub.large_uploads is off', async () => {
    setFeatureFlag('hub.large_uploads', false);
    const uploadUrls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const url = requestUrl(input);
        if (url === '/api/v1/settings/public') {
          return Promise.resolve(
            new Response(JSON.stringify({ uploads: { maxWorkspaceFileSize: 1 << 30 } }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        uploadUrls.push(url);
        return Promise.resolve(new Response(JSON.stringify({ files: [] }), { status: 200 }));
      })
    );

    const { pane, sent } = mountPane();
    await pane.updateComplete;

    const huge = makeFakeLargeFile('huge.bin', 200 * 1024 * 1024);
    await pane._handleFileDrop(makeFileList([huge]));

    expect(uploadUrls).toEqual([]);
    expect(sent).toEqual([]);
    expect(pane.uploadStatus).toContain('exceeds the maximum size of 100 MiB');
  });

  it('uses chunked shared-dir upload when hub.large_uploads is on, shows progress, and injects path', async () => {
    setFeatureFlag('hub.large_uploads', true);
    const calls: Array<{ method: string; url: string }> = [];
    const seenStatuses: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request, init?: RequestInit) => {
        const url = requestUrl(input);
        const method = (init?.method ?? 'GET').toUpperCase();
        calls.push({ method, url });
        if (url.endsWith('/shared-dirs/scratchpad/uploads') && method === 'POST') {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                id: 'upl_term1',
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
          new Response(JSON.stringify({ path: 'model.bin', size: 9 * 1024 * 1024 }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );

    const { pane, sent } = mountPane();
    await pane.updateComplete;

    const origUpdate = pane.requestUpdate.bind(pane);
    pane.requestUpdate = ((...args: Parameters<typeof pane.requestUpdate>) => {
      if (pane.uploadStatus) seenStatuses.push(pane.uploadStatus);
      return origUpdate(...args);
    }) as typeof pane.requestUpdate;

    const big = makeFakeLargeFile('model.bin', 9 * 1024 * 1024);
    await pane._handleFileDrop(makeFileList([big]));

    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      'POST /api/v1/projects/proj-1/shared-dirs/scratchpad/uploads',
      'PUT /api/v1/projects/proj-1/shared-dirs/scratchpad/uploads/upl_term1?offset=0',
      'PUT /api/v1/projects/proj-1/shared-dirs/scratchpad/uploads/upl_term1?offset=8388608',
      'POST /api/v1/projects/proj-1/shared-dirs/scratchpad/uploads/upl_term1/complete',
    ]);
    expect(seenStatuses.some((s) => /^Uploading \d+%$/.test(s))).toBe(true);
    expect(pane.isUploading).toBe(false);
    expect(pane.uploadStatus).toBe('');
    expect(sent).toHaveLength(1);
    expect(sent[0]).toMatch(
      /^\/scion-volumes\/scratchpad\/\.attachments\/_web\/[^/]+\/model\.bin $/
    );
  });

  it('cancels an in-flight chunked upload via Cancel button without error or path injection', async () => {
    setFeatureFlag('hub.large_uploads', true);
    let deleteCalled = false;
    let resolvePutStarted!: () => void;
    const putStarted = new Promise<void>((r) => {
      resolvePutStarted = r;
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request, init?: RequestInit) => {
        const url = requestUrl(input);
        const method = (init?.method ?? 'GET').toUpperCase();
        if (method === 'POST' && url.endsWith('/shared-dirs/scratchpad/uploads')) {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                id: 'upl_cancel1',
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
          resolvePutStarted();
          return new Promise<Response>((_resolve, reject) => {
            init?.signal?.addEventListener('abort', () => {
              reject(new DOMException('Aborted', 'AbortError'));
            });
          });
        }
        if (method === 'DELETE') {
          deleteCalled = true;
          return Promise.resolve(new Response(null, { status: 204 }));
        }
        return Promise.resolve(new Response('{}', { status: 200 }));
      })
    );

    const { pane, sent } = mountPane();
    await pane.updateComplete;

    const big = makeFakeLargeFile('cancel.bin', 9 * 1024 * 1024);
    const dropPromise = pane._handleFileDrop(makeFileList([big]));
    await putStarted;
    await pane.updateComplete;

    const cancelBtn = pane.shadowRoot?.querySelector('.upload-cancel-btn') as HTMLElement | null;
    expect(cancelBtn).toBeTruthy();
    cancelBtn!.click();

    await dropPromise;
    await pane.updateComplete;

    expect(deleteCalled).toBe(true);
    expect(pane.isUploading).toBe(false);
    expect(pane.uploadStatus).toBe('');
    expect(sent).toEqual([]);
  });

  it('shows "Large uploads were disabled." when server returns experiment_disabled', async () => {
    setFeatureFlag('hub.large_uploads', true);
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              error: {
                code: 'experiment_disabled',
                message: 'experiment_disabled: hub.large_uploads is off',
              },
            }),
            { status: 404, headers: { 'Content-Type': 'application/json' } }
          )
        )
      )
    );

    const { pane, sent } = mountPane();
    await pane.updateComplete;

    const big = makeFakeLargeFile('disabled.bin', 9 * 1024 * 1024);
    await pane._handleFileDrop(makeFileList([big]));

    expect(pane.isUploading).toBe(false);
    expect(pane.uploadStatus).toBe('Large uploads were disabled.');
    expect(sent).toEqual([]);
  });
});
