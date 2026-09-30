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
 * scion-file-browser — initial-load deduplication (ptone/scion#2380).
 *
 * Each shared directory used to be fetched twice: once from
 * connectedCallback() and again from the first updated() pass triggered by
 * the initial `dataSource` property assignment. These tests pin down the
 * fixed behavior: exactly one initial listing request per mounted browser,
 * correct behavior on data-source change / reconnect / explicit refresh, and
 * that a superseded in-flight load cannot clobber newer results.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

import type { FileEntry, FileListResult, FileBrowserDataSource } from './file-browser.js';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let FileBrowserCtor: any;

beforeAll(async () => {
  const mod = await import('./file-browser.js');
  FileBrowserCtor = mod.ScionFileBrowser;
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.restoreAllMocks();
});

function makeEntry(path: string): FileEntry {
  return { path, size: 10, modTime: '2026-01-01T00:00:00Z', mode: '-rw-r--r--' };
}

/** A data source whose listFiles() resolves via an externally-controlled promise. */
function makeControlledSource(label: string) {
  const calls: number[] = [];
  let resolveFn: ((r: FileListResult) => void) | null = null;
  const listFiles = vi.fn(() => {
    calls.push(calls.length);
    return new Promise<FileListResult>((resolve) => {
      resolveFn = resolve;
    });
  });
  const source: FileBrowserDataSource = {
    listFiles,
    deleteFile: vi.fn(),
    uploadFiles: vi.fn(),
    getDownloadUrl: () => `/download/${label}`,
    getPreviewUrl: () => `/preview/${label}`,
  };
  return {
    source,
    listFiles,
    resolve: (files: string[] = []) => resolveFn?.({ files: files.map(makeEntry), totalSize: 0, totalCount: files.length }),
  };
}

/** A data source whose listFiles() resolves immediately with the given files. */
function makeImmediateSource(label: string, files: string[] = []) {
  const listFiles = vi.fn(() =>
    Promise.resolve<FileListResult>({
      files: files.map(makeEntry),
      totalSize: 0,
      totalCount: files.length,
    })
  );
  const source: FileBrowserDataSource = {
    listFiles,
    deleteFile: vi.fn(),
    uploadFiles: vi.fn(),
    getDownloadUrl: () => `/download/${label}`,
    getPreviewUrl: () => `/preview/${label}`,
  };
  return { source, listFiles };
}

describe('scion-file-browser — one initial listing per data source', () => {
  it('issues exactly one request across connectedCallback + the initial dataSource update', async () => {
    const { source, listFiles } = makeImmediateSource('a', ['foo.txt']);

    const el = new FileBrowserCtor();
    el.dataSource = source; // set before connecting, as Lit template bindings do
    document.body.appendChild(el);
    await el.updateComplete;
    // Drain any microtask-queued second update pass.
    await el.updateComplete;

    expect(listFiles).toHaveBeenCalledTimes(1);
  });

  it('makes no request when no data source is assigned', async () => {
    const el = new FileBrowserCtor();
    document.body.appendChild(el);
    await el.updateComplete;

    expect((el as { loading: boolean }).loading).toBe(false);
  });

  it('loads correctly when the data source changes to a new source', async () => {
    const first = makeImmediateSource('a', ['foo.txt']);
    const second = makeImmediateSource('b', ['bar.txt']);

    const el = new FileBrowserCtor();
    el.dataSource = first.source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(first.listFiles).toHaveBeenCalledTimes(1);

    el.dataSource = second.source;
    await el.updateComplete;
    await el.updateComplete;

    expect(second.listFiles).toHaveBeenCalledTimes(1);
    expect(first.listFiles).toHaveBeenCalledTimes(1); // unchanged
    expect((el as { files: FileEntry[] }).files.map((f) => f.path)).toEqual(['bar.txt']);
  });

  it('reloads exactly once on reconnect (does not treat the cached source as already loaded forever)', async () => {
    const { source, listFiles } = makeImmediateSource('a', ['foo.txt']);

    const el = new FileBrowserCtor();
    el.dataSource = source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(listFiles).toHaveBeenCalledTimes(1);

    document.body.removeChild(el);
    document.body.appendChild(el); // reconnect same element/instance with the same dataSource
    await el.updateComplete;
    await el.updateComplete;

    expect(listFiles).toHaveBeenCalledTimes(2);
  });

  it('explicit refresh always issues a new request regardless of dedup state', async () => {
    const { source, listFiles } = makeImmediateSource('a', ['foo.txt']);

    const el = new FileBrowserCtor();
    el.dataSource = source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(listFiles).toHaveBeenCalledTimes(1);

    await el.loadFiles();
    expect(listFiles).toHaveBeenCalledTimes(2);
  });

  it('discards stale in-flight results from a superseded data source', async () => {
    const slow = makeControlledSource('slow');
    const fast = makeImmediateSource('fast', ['fast.txt']);

    const el = new FileBrowserCtor();
    el.dataSource = slow.source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(slow.listFiles).toHaveBeenCalledTimes(1);

    // Switch to a new source before the slow request resolves.
    el.dataSource = fast.source;
    await el.updateComplete;
    await el.updateComplete;
    expect((el as { files: FileEntry[] }).files.map((f) => f.path)).toEqual(['fast.txt']);

    // Now the superseded slow request resolves — it must not clobber the
    // already-current, newer result.
    slow.resolve(['stale.txt']);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    expect((el as { files: FileEntry[] }).files.map((f) => f.path)).toEqual(['fast.txt']);
  });

  it('rejects a stale in-flight error from a superseded data source', async () => {
    const listFiles1 = vi.fn();
    let rejectFn: ((err: Error) => void) | null = null;
    listFiles1.mockImplementation(
      () =>
        new Promise((_resolve, reject) => {
          rejectFn = reject;
        })
    );
    const source1: FileBrowserDataSource = {
      listFiles: listFiles1,
      deleteFile: vi.fn(),
      uploadFiles: vi.fn(),
      getDownloadUrl: () => '',
      getPreviewUrl: () => '',
    };
    const { source: source2 } = makeImmediateSource('b', ['ok.txt']);

    const el = new FileBrowserCtor();
    el.dataSource = source1;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;

    el.dataSource = source2;
    await el.updateComplete;
    await el.updateComplete;
    expect((el as { files: FileEntry[] }).files.map((f) => f.path)).toEqual(['ok.txt']);
    expect((el as { error: string | null }).error).toBeNull();

    rejectFn?.(new Error('boom'));
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    // The stale rejection must not surface as an error over the newer, good state.
    expect((el as { error: string | null }).error).toBeNull();
    expect((el as { files: FileEntry[] }).files.map((f) => f.path)).toEqual(['ok.txt']);
  });
});
