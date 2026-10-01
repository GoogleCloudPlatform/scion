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

/**
 * A data source whose listFiles() resolves via an externally-controlled
 * promise. Each call gets its own independent resolver (resolveCall(n, ...))
 * so tests can control which of several overlapping in-flight calls
 * resolves, and in what order — e.g. an earlier, now-stale call resolving
 * after a later one has already started.
 */
function makeControlledSource(label: string) {
  const resolvers: Array<(r: FileListResult) => void> = [];
  const listFiles = vi.fn(() => {
    return new Promise<FileListResult>((resolve) => {
      resolvers.push(resolve);
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
    /** Resolve the Nth (0-indexed) call to listFiles(). */
    resolveCall: (n: number, files: string[] = []) =>
      resolvers[n]?.({ files: files.map(makeEntry), totalSize: 0, totalCount: files.length }),
    /** Resolve the most recent call to listFiles(). */
    resolve: (files: string[] = []) =>
      resolvers[resolvers.length - 1]?.({
        files: files.map(makeEntry),
        totalSize: 0,
        totalCount: files.length,
      }),
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

/** A data source whose listFiles() immediately rejects with the given message. */
function makeFailingSource(label: string, message = 'boom') {
  const listFiles = vi.fn(() => Promise.reject(new Error(message)));
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
    await el.updateComplete;

    // loadFiles() always increments _loadToken before doing anything else,
    // so a token still at its initial value of 0 is direct proof no load
    // was ever started (loading === false alone would also be true after
    // an unrelated load already finished, so it doesn't prove this).
    expect((el as unknown as { _loadToken: number })._loadToken).toBe(0);
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

  it('reloads when the data source is cleared to null and reassigned to the same instance', async () => {
    const { source, listFiles } = makeImmediateSource('a', ['foo.txt']);

    const el = new FileBrowserCtor();
    el.dataSource = source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(listFiles).toHaveBeenCalledTimes(1);

    // Clear the data source, then reassign the exact same instance. Without
    // resetting on null, _requestedSource would still equal `source` and
    // this reassignment would be silently treated as "already requested".
    el.dataSource = null;
    await el.updateComplete;
    expect((el as { files: FileEntry[] }).files).toEqual([]);

    el.dataSource = source;
    await el.updateComplete;
    await el.updateComplete;

    expect(listFiles).toHaveBeenCalledTimes(2);
    expect((el as { files: FileEntry[] }).files.map((f) => f.path)).toEqual(['foo.txt']);
  });

  it('invalidates an in-flight request when the data source is cleared before it resolves', async () => {
    const slow = makeControlledSource('slow');

    const el = new FileBrowserCtor();
    el.dataSource = slow.source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(slow.listFiles).toHaveBeenCalledTimes(1);

    el.dataSource = null;
    await el.updateComplete;

    // The in-flight response for the cleared source must not repopulate
    // state after the fact.
    slow.resolve(['stale.txt']);
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    expect((el as { files: FileEntry[] }).files).toEqual([]);
    expect((el as { loading: boolean }).loading).toBe(false);
  });

  it('reconnects to a different data source assigned while detached (not the pre-disconnect one)', async () => {
    const first = makeImmediateSource('a', ['foo.txt']);
    const second = makeImmediateSource('b', ['bar.txt']);

    const el = new FileBrowserCtor();
    el.dataSource = first.source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(first.listFiles).toHaveBeenCalledTimes(1);

    document.body.removeChild(el);
    el.dataSource = second.source; // changed while detached
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;

    expect(second.listFiles).toHaveBeenCalledTimes(1);
    expect(first.listFiles).toHaveBeenCalledTimes(1); // not reloaded
    expect((el as { files: FileEntry[] }).files.map((f) => f.path)).toEqual(['bar.txt']);
  });

  it('coalesces a disconnect/reconnect that happens before the initial request resolves', async () => {
    const slow = makeControlledSource('slow');

    const el = new FileBrowserCtor();
    el.dataSource = slow.source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(slow.listFiles).toHaveBeenCalledTimes(1);

    // Disconnect and immediately reconnect with the same (still-loading)
    // data source, before the in-flight request resolves.
    document.body.removeChild(el);
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;

    // The reconnect must not have fired a second request for the same
    // still-in-flight source.
    expect(slow.listFiles).toHaveBeenCalledTimes(1);

    slow.resolve(['foo.txt']);
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    expect((el as { files: FileEntry[] }).files.map((f) => f.path)).toEqual(['foo.txt']);
  });

  // ── Round-2 review: the null-reset (NB1) and in-flight coalescing (NB2)
  // fixes interacted badly — coalescing keyed on source identity alone let
  // an A -> null -> A reassignment coalesce into A's now-stale, already
  // in-flight request, which the null-reset had invalidated via the load
  // token. Nothing ever reloaded and the browser was stuck empty with no
  // error and no spinner. Fixed by keying "in flight" on (source, token)
  // together instead of source alone. ──

  it('reloads when the data source is cleared to null and reassigned while the original request is still in flight', async () => {
    const slow = makeControlledSource('a');

    const el = new FileBrowserCtor();
    el.dataSource = slow.source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(slow.listFiles).toHaveBeenCalledTimes(1);

    // Clear to null (invalidates the in-flight call via the load token,
    // per the earlier "invalidates an in-flight request..." test) and
    // immediately reassign the exact same source instance, all before the
    // first call resolves.
    el.dataSource = null;
    await el.updateComplete;
    el.dataSource = slow.source;
    await el.updateComplete;
    await el.updateComplete;

    // Coalescing must recognize the first call is stale (superseded by the
    // null-reset) and start a genuinely new request rather than silently
    // treating the dead first call as "already in flight for this source".
    expect(slow.listFiles).toHaveBeenCalledTimes(2);

    // Resolve the first (stale) call — it must not resurrect the browser
    // out of its "no request landed" state incorrectly, and specifically
    // must not clear the bookkeeping for the second, still-current call.
    slow.resolveCall(0, ['stale.txt']);
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    // Now resolve the second (current) call — this is the one that must
    // actually populate the browser.
    slow.resolveCall(1, ['fresh.txt']);
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    expect((el as { files: FileEntry[] }).files.map((f) => f.path)).toEqual(['fresh.txt']);
    expect((el as { loading: boolean }).loading).toBe(false);
  });

  it('invalidates an in-flight request cleared to null while the component is disconnected', async () => {
    const slow = makeControlledSource('a');

    const el = new FileBrowserCtor();
    el.dataSource = slow.source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(slow.listFiles).toHaveBeenCalledTimes(1);

    // Disconnect (this nulls _requestedSource internally) and then clear
    // dataSource to null while still detached — a null-reset gated only on
    // _requestedSource would see it already null and skip resetting,
    // leaving the in-flight request's token still "current".
    document.body.removeChild(el);
    el.dataSource = null;
    await el.updateComplete;

    slow.resolve(['stale.txt']);
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    expect((el as { files: FileEntry[] }).files).toEqual([]);
    expect((el as { loading: boolean }).loading).toBe(false);
  });

  it('does not let a stale request clear the bookkeeping for a newer, still-in-flight request to the same source', async () => {
    const slow = makeControlledSource('a');

    const el = new FileBrowserCtor();
    el.dataSource = slow.source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(slow.listFiles).toHaveBeenCalledTimes(1);

    // Explicit refresh starts a second, overlapping request for the same
    // source while the first is still in flight.
    void el.loadFiles();
    expect(slow.listFiles).toHaveBeenCalledTimes(2);

    // The first (now stale) call resolves first.
    slow.resolveCall(0, ['stale.txt']);
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    // Disconnect and reconnect — if the stale call's `finally` incorrectly
    // cleared the in-flight bookkeeping for the still-outstanding second
    // call, this would coalesce-fail and fire a third, unnecessary request.
    document.body.removeChild(el);
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(slow.listFiles).toHaveBeenCalledTimes(2);

    slow.resolveCall(1, ['fresh.txt']);
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    expect((el as { files: FileEntry[] }).files.map((f) => f.path)).toEqual(['fresh.txt']);
  });

  it('clears stale settled files when disconnected then cleared to null, even with nothing requested or in flight', async () => {
    // Upstream review (GoogleCloudPlatform/scion#2175): after a load has
    // already completed (so _requestedSource and _inFlightSource are both
    // effectively idle once disconnectedCallback() nulls _requestedSource),
    // clearing dataSource to null must still clear the stale `files` — not
    // just skip the reset because nothing was "requested" or "in flight".
    const { source, listFiles } = makeImmediateSource('a', ['foo.txt']);

    const el = new FileBrowserCtor();
    el.dataSource = source;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;
    expect(listFiles).toHaveBeenCalledTimes(1);
    expect((el as { files: FileEntry[] }).files.map((f) => f.path)).toEqual(['foo.txt']);

    // Disconnect (the load already settled, so nothing is in flight; this
    // nulls _requestedSource) ...
    document.body.removeChild(el);
    // ... then clear the data source to null while detached.
    el.dataSource = null;
    await el.updateComplete;

    expect((el as { files: FileEntry[] }).files).toEqual([]);
  });

  it('clears a stale error when disconnected then cleared to null, even with nothing requested or in flight', async () => {
    // Round-5 review nit: the widened reset condition's `error !== null`
    // clause had no test. Same shape as the files-clearing test above, but
    // for a load that failed rather than one that succeeded.
    const { source } = makeFailingSource('a');

    const el = new FileBrowserCtor();
    el.dataSource = source;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
    expect((el as { error: string | null }).error).not.toBeNull();

    // Disconnect (the load already settled with an error, so nothing is in
    // flight; this nulls _requestedSource) ...
    document.body.removeChild(el);
    // ... then clear the data source to null while detached.
    el.dataSource = null;
    await el.updateComplete;

    expect((el as { error: string | null }).error).toBeNull();
  });
});
