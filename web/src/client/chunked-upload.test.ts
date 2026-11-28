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

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import {
  _setUploadTestConfig,
  DEFAULT_CHUNK_BYTES,
  fitsSingleMultipartRequest,
  formatUploadSize,
  MIN_CHUNK_BYTES,
  UploadCancelledError,
  uploadFileChunked,
  UploadHttpError,
  uploadWorkspaceFiles,
} from './chunked-upload.js';
import { requestUrl } from './__fixtures__/request-url.js';

function json(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

interface RecordedCall {
  method: string;
  path: string;
  offset: number | null;
  size: number;
  contentType: string | null;
}

interface FakeHubOptions {
  chunkBytes: number;
  maxBytes?: number;
  /** Initial expiresAt offset from clock now (ms). Default 1 h. */
  expiresInMs?: number;
  /** Optional proxy limit: any single or chunk request > proxyLimitBytes returns 413. */
  proxyLimitBytes?: number;
  /** Fail specific chunk call number (1-indexed). */
  failChunkCalls?: Record<
    number,
    | { kind: 'network' }
    | {
        kind: 'status';
        status: number;
        code?: string;
        message?: string;
        details?: Record<string, unknown>;
      }
  >;
  /** Return 202 finalizing on the first N complete calls, then 200. */
  finalizingPolls?: number;
  /** Fail the first N complete calls with 504. */
  failCompleteCalls?: number;
  /** Return 409 upload_busy on the first N complete calls. */
  busyCompleteCalls?: number;
  /** Return 404 on the first N chunk calls (rolling update simulation). */
  rollout404ChunkCalls?: number;
  /** Answer every chunk with received = offset (no progress). */
  noProgress?: boolean;
  /** Return 409 upload_busy on the first N DELETE calls before returning 204. */
  busyDeleteCalls?: number;
  /** Hook called before each chunk response (e.g. to advance a fake clock). */
  onChunk?: (callNumber: number, chunkSize: number) => void;
  /** Hook called before each complete response (e.g. to trigger cancel). */
  onComplete?: (callNumber: number) => void;
  /** Clock provider. */
  now?: () => number;
}

function createFakeHub(opts: FakeHubOptions) {
  const nowFn = opts.now ?? (() => Date.now());
  const calls: RecordedCall[] = [];
  let received = new Uint8Array(0);
  let chunkCallNum = 0;
  let completeCallNum = 0;
  let statusCallNum = 0;
  let deleteCallNum = 0;
  let state: 'uploading' | 'finalizing' | 'completed' | 'aborted' = 'uploading';
  const startBodies: Array<Record<string, unknown>> = [];

  const fetchMock = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const href = requestUrl(input);
    const url = new URL(href, 'http://hub.test');
    const method = (init?.method ?? 'GET').toUpperCase();
    const headers = new Headers(init?.headers);
    const contentType = headers.get('content-type');
    const body = init?.body;
    const size = body instanceof Blob ? body.size : typeof body === 'string' ? body.length : 0;
    const offset = url.searchParams.has('offset') ? Number(url.searchParams.get('offset')) : null;

    calls.push({ method, path: url.pathname, offset, size, contentType });

    // Single-request multipart endpoint (/files)
    if (url.pathname.endsWith('/files') && method === 'POST') {
      if (opts.proxyLimitBytes !== undefined && body instanceof FormData) {
        let total = 0;
        for (const v of body.values()) {
          if (v instanceof Blob) total += v.size;
        }
        if (total > opts.proxyLimitBytes) {
          return new Response('Payload Too Large', { status: 413 });
        }
      }
      return json({ files: [] });
    }

    // Start: POST .../uploads
    if (url.pathname.endsWith('/uploads') && method === 'POST') {
      const parsed = JSON.parse(typeof body === 'string' ? body : '{}') as Record<string, unknown>;
      startBodies.push(parsed);
      received = new Uint8Array(0);
      state = 'uploading';
      const expiresAt = new Date(nowFn() + (opts.expiresInMs ?? 3600_000)).toISOString();
      return json(
        {
          id: 'upl_test123',
          transport: 'hub_chunked',
          chunkBytes: opts.chunkBytes,
          maxBytes: opts.maxBytes ?? 1 << 30,
          received: 0,
          expiresAt,
        },
        201
      );
    }

    // Complete: POST .../uploads/{sid}/complete
    if (url.pathname.endsWith('/complete') && method === 'POST') {
      completeCallNum++;
      opts.onComplete?.(completeCallNum);
      if (init?.signal?.aborted) {
        throw new DOMException('Aborted', 'AbortError');
      }
      if (completeCallNum <= (opts.failCompleteCalls ?? 0)) {
        return json({ error: { code: 'internal_error', message: 'gateway timeout' } }, 504);
      }
      if (completeCallNum <= (opts.busyCompleteCalls ?? 0)) {
        return json({ error: { code: 'upload_busy', message: 'busy' } }, 409);
      }
      if (opts.finalizingPolls && state !== 'completed') {
        state = 'finalizing';
        return json({ state: 'finalizing' }, 202);
      }
      state = 'completed';
      return json({ path: 'file.bin', size: received.length });
    }

    // Abort: DELETE .../uploads/{sid}
    if (method === 'DELETE' && url.pathname.includes('/uploads/')) {
      deleteCallNum++;
      if (deleteCallNum <= (opts.busyDeleteCalls ?? 0)) {
        return json(
          { error: { code: 'upload_busy', message: 'Upload session has an active chunk write' } },
          409
        );
      }
      state = 'aborted';
      return new Response(null, { status: 204 });
    }

    // Status: GET .../uploads/{sid}
    if (method === 'GET' && url.pathname.includes('/uploads/')) {
      statusCallNum++;
      if (state === 'finalizing' && opts.finalizingPolls && statusCallNum >= opts.finalizingPolls) {
        state = 'completed';
      }
      return json({
        id: 'upl_test123',
        state,
        received: received.length,
        declaredSize: received.length,
        expiresAt: new Date(nowFn() + (opts.expiresInMs ?? 3600_000)).toISOString(),
      });
    }

    // Chunk: PUT .../uploads/{sid}?offset=N
    if (method === 'PUT' && url.pathname.includes('/uploads/')) {
      chunkCallNum++;
      opts.onChunk?.(chunkCallNum, size);

      if (opts.rollout404ChunkCalls && chunkCallNum <= opts.rollout404ChunkCalls) {
        return new Response('404 page not found', { status: 404 });
      }

      const injected = opts.failChunkCalls?.[chunkCallNum];
      if (injected) {
        if (injected.kind === 'network') throw new TypeError('Failed to fetch');
        return json(
          {
            error: {
              code: injected.code ?? '',
              message: injected.message ?? `HTTP ${injected.status}`,
              ...(injected.details ? { details: injected.details } : {}),
            },
          },
          injected.status
        );
      }

      if (opts.proxyLimitBytes !== undefined && size > opts.proxyLimitBytes) {
        return new Response('Payload Too Large', { status: 413 });
      }

      if (offset !== received.length) {
        return json(
          {
            error: {
              code: 'offset_mismatch',
              message: `Expected offset ${received.length}, got ${offset}`,
              details: { received: received.length },
            },
          },
          409
        );
      }

      if (opts.noProgress) {
        return json({ received: offset, expiresAt: new Date(nowFn() + 3600_000).toISOString() });
      }

      const bytes = new Uint8Array(await (body as Blob).arrayBuffer());
      const next = new Uint8Array(received.length + bytes.length);
      next.set(received);
      next.set(bytes, received.length);
      received = next;

      return json({
        received: received.length,
        expiresAt: new Date(nowFn() + (opts.expiresInMs ?? 3600_000)).toISOString(),
      });
    }

    return new Response('Not Found', { status: 404 });
  });

  vi.stubGlobal('fetch', fetchMock);
  return {
    calls,
    startBodies,
    received: () => received,
    setReceived: (data: Uint8Array) => {
      received = data;
    },
    getState: () => state,
  };
}

function makeFile(size: number, name = 'test.bin', type = 'application/octet-stream'): File {
  const data = new Uint8Array(size);
  for (let i = 0; i < size; i++) data[i] = (i * 31 + 7) % 251;
  return new File([data], name, { type });
}

describe('chunked-upload', () => {
  beforeEach(() => {
    _setUploadTestConfig({
      retryDelayMs: 0,
      maxBackoffMs: 0,
      abortRetryDelayMs: 0,
      abortMaxBackoffMs: 0,
      minChunkBytes: 4,
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    _setUploadTestConfig(null);
  });

  it('formats sizes for messages', () => {
    expect(formatUploadSize(1 << 30)).toBe('1 GiB');
    expect(formatUploadSize(8 * 1024 * 1024)).toBe('8 MiB');
    expect(formatUploadSize(1536)).toBe('1.5 KiB');
    expect(formatUploadSize(900)).toBe('900 B');
  });

  it('checks single-request threshold against chunkBytes (§3.2)', () => {
    const fileOf = (size: number) => ({ size }) as Blob;
    expect(
      fitsSingleMultipartRequest([{ path: 'a.txt', file: fileOf(100) }], DEFAULT_CHUNK_BYTES)
    ).toBe(true);
    // Whole body > chunkBytes does not fit.
    expect(
      fitsSingleMultipartRequest(
        [{ path: 'a.bin', file: fileOf(DEFAULT_CHUNK_BYTES) }],
        DEFAULT_CHUNK_BYTES
      )
    ).toBe(false);
    // Two files summing > chunkBytes do not fit.
    expect(
      fitsSingleMultipartRequest(
        [
          { path: 'a.bin', file: fileOf(5 * 1024 * 1024) },
          { path: 'b.bin', file: fileOf(4 * 1024 * 1024) },
        ],
        DEFAULT_CHUNK_BYTES
      )
    ).toBe(false);
  });

  it('uploads a file in chunks (start → PUT chunks → complete) and sends exact bytes', async () => {
    const hub = createFakeHub({ chunkBytes: 10 });
    const file = makeFile(35, 'model.bin');
    const progress: Array<[number, number]> = [];

    const res = await uploadFileChunked('/api/v1/projects/p1/workspace/uploads', file, {
      path: 'dir/model.bin',
      overwrite: true,
      onProgress: (sent, total) => progress.push([sent, total]),
    });

    expect(res.ok).toBe(true);
    expect(hub.startBodies[0]).toEqual({
      name: 'model.bin',
      size: 35,
      mediaType: 'application/octet-stream',
      path: 'dir/model.bin',
      overwrite: true,
    });
    expect(hub.calls.map((c) => [c.method, c.path, c.offset, c.size])).toEqual([
      ['POST', '/api/v1/projects/p1/workspace/uploads', null, expect.any(Number)],
      ['PUT', '/api/v1/projects/p1/workspace/uploads/upl_test123', 0, 10],
      ['PUT', '/api/v1/projects/p1/workspace/uploads/upl_test123', 10, 10],
      ['PUT', '/api/v1/projects/p1/workspace/uploads/upl_test123', 20, 10],
      ['PUT', '/api/v1/projects/p1/workspace/uploads/upl_test123', 30, 5],
      ['POST', '/api/v1/projects/p1/workspace/uploads/upl_test123/complete', null, 2],
    ]);
    expect(
      hub.calls
        .filter((c) => c.method === 'PUT')
        .every((c) => c.contentType === 'application/octet-stream')
    ).toBe(true);
    expect(progress).toEqual([
      [0, 35],
      [10, 35],
      [20, 35],
      [30, 35],
      [35, 35],
    ]);
    expect(Array.from(hub.received())).toEqual(
      Array.from(new Uint8Array(await file.arrayBuffer()))
    );
  });

  it('halves chunk size on 413 and resumes from committed offset (§3.2)', async () => {
    // Server allows 16-byte chunks, proxy caps at 6 bytes → 16 → 8 (413) → 4 (ok).
    const hub = createFakeHub({ chunkBytes: 16, proxyLimitBytes: 6 });
    const file = makeFile(14);

    const res = await uploadFileChunked('/api/v1/projects/p1/workspace/uploads', file);
    expect(res.ok).toBe(true);

    const putSizes = hub.calls.filter((c) => c.method === 'PUT').map((c) => [c.offset, c.size]);
    expect(putSizes).toEqual([
      [0, 16 - 2], // 14 > 6 → 413, halve 16 → 8
      [0, 8], // 8 > 6 → 413, halve 8 → 4
      [0, 4], // ok
      [4, 4], // ok
      [8, 4], // ok
      [12, 2], // ok
    ]);
    expect(Array.from(hub.received())).toEqual(
      Array.from(new Uint8Array(await file.arrayBuffer()))
    );
  });

  it('stops halving at the 1 MiB floor and fails if 413 persists', async () => {
    _setUploadTestConfig({ retryDelayMs: 0, maxBackoffMs: 0, minChunkBytes: MIN_CHUNK_BYTES });
    const hub = createFakeHub({
      chunkBytes: 2 * MIN_CHUNK_BYTES,
      proxyLimitBytes: 512 * 1024, // below 1 MiB floor
    });
    // Use a lightweight blob-like object so we don't allocate 2 MiB in the unit test.
    const fakeBlob = {
      size: 2 * MIN_CHUNK_BYTES,
      type: 'application/octet-stream',
      name: 'big.bin',
      slice: (start: number, end: number) => new Blob([new Uint8Array(end - start)]),
    } as unknown as File;

    const err = await uploadFileChunked('/api/v1/projects/p1/workspace/uploads', fakeBlob).catch(
      (e: unknown) => e
    );
    expect(err).toBeInstanceOf(UploadHttpError);
    expect((err as UploadHttpError).status).toBe(413);
    const putSizes = hub.calls.filter((c) => c.method === 'PUT').map((c) => c.size);
    // Tried 2 MiB, halved to 1 MiB (floor), failed at 1 MiB.
    expect(putSizes).toEqual([2 * MIN_CHUNK_BYTES, MIN_CHUNK_BYTES]);
    expect(hub.calls.some((c) => c.method === 'DELETE')).toBe(true);
  });

  it('resumes from error.details.received on 409 offset_mismatch', async () => {
    const file = makeFile(30);
    const fileBytes = new Uint8Array(await file.arrayBuffer());
    const hub = createFakeHub({
      chunkBytes: 10,
      onChunk: (callNum) => {
        // Simulate a lost response for chunk 0: server already holds bytes [0, 10).
        if (callNum === 1) hub.setReceived(fileBytes.slice(0, 10));
      },
      failChunkCalls: {
        1: {
          kind: 'status',
          status: 409,
          code: 'offset_mismatch',
          message: 'Expected offset 10, got 0',
          details: { received: 10 },
        },
      },
    });

    const res = await uploadFileChunked('/api/v1/projects/p1/workspace/uploads', file);
    expect(res.ok).toBe(true);
    expect(hub.calls.filter((c) => c.method === 'PUT').map((c) => c.offset)).toEqual([0, 10, 20]);
    expect(Array.from(hub.received())).toEqual(Array.from(fileBytes));
  });

  it('backs off and retries on 409 upload_busy', async () => {
    const hub = createFakeHub({
      chunkBytes: 10,
      failChunkCalls: {
        1: { kind: 'status', status: 409, code: 'upload_busy', message: 'busy' },
        2: { kind: 'status', status: 409, code: 'upload_busy', message: 'busy' },
      },
    });
    const res = await uploadFileChunked('/api/v1/projects/p1/workspace/uploads', makeFile(15));
    expect(res.ok).toBe(true);
    expect(hub.calls.filter((c) => c.method === 'PUT').map((c) => c.offset)).toEqual([0, 0, 0, 10]);
  });

  it('polls GET on 202 finalizing and replays complete once completed', async () => {
    const hub = createFakeHub({ chunkBytes: 10, finalizingPolls: 3 });
    const res = await uploadFileChunked('/api/v1/projects/p1/workspace/uploads', makeFile(10));
    expect(res.ok).toBe(true);
    const gets = hub.calls.filter((c) => c.method === 'GET');
    const completes = hub.calls.filter((c) => c.path.endsWith('/complete'));
    expect(gets.length).toBe(3);
    expect(completes.length).toBe(2);
  });

  it('retries network errors with exponential backoff up to expiresAt', async () => {
    let clock = 1_000_000;
    _setUploadTestConfig({
      retryDelayMs: 100,
      maxBackoffMs: 1000,
      minChunkBytes: 4,
      now: () => clock,
    });
    // Fail twice with network error, then succeed.
    const hub = createFakeHub({
      chunkBytes: 10,
      expiresInMs: 60_000,
      now: () => clock,
      failChunkCalls: {
        1: { kind: 'network' },
        2: { kind: 'network' },
      },
    });

    // Make setTimeout advance our fake clock immediately.
    vi.spyOn(globalThis, 'setTimeout').mockImplementation(((cb: () => void, ms?: number) => {
      clock += ms ?? 0;
      cb();
      return 0 as unknown as ReturnType<typeof setTimeout>;
    }) as typeof setTimeout);

    const res = await uploadFileChunked('/api/v1/projects/p1/workspace/uploads', makeFile(10));
    expect(res.ok).toBe(true);
    expect(hub.calls.filter((c) => c.method === 'PUT').length).toBe(3);
    // Exponential backoff: 100ms + 200ms = 300ms elapsed.
    expect(clock).toBe(1_000_300);
  });

  it('stops retrying network errors once expiresAt is reached', async () => {
    let clock = 1_000_000;
    _setUploadTestConfig({
      retryDelayMs: 500,
      maxBackoffMs: 2000,
      minChunkBytes: 4,
      now: () => clock,
    });
    createFakeHub({
      chunkBytes: 10,
      expiresInMs: 1000,
      now: () => clock,
      failChunkCalls: {
        1: { kind: 'network' },
        2: { kind: 'network' },
        3: { kind: 'network' },
      },
    });
    vi.spyOn(globalThis, 'setTimeout').mockImplementation(((cb: () => void, ms?: number) => {
      clock += ms ?? 0;
      cb();
      return 0 as unknown as ReturnType<typeof setTimeout>;
    }) as typeof setTimeout);

    await expect(
      uploadFileChunked('/api/v1/projects/p1/workspace/uploads', makeFile(10))
    ).rejects.toThrow('Failed to fetch');
  });

  it('retries plain 404 during active session for up to 60 s (rolling updates, §5)', async () => {
    let clock = 1_000_000;
    _setUploadTestConfig({
      retryDelayMs: 1000,
      maxBackoffMs: 10_000,
      rollout404WindowMs: 60_000,
      minChunkBytes: 4,
      now: () => clock,
    });
    vi.spyOn(globalThis, 'setTimeout').mockImplementation(((cb: () => void, ms?: number) => {
      clock += ms ?? 0;
      cb();
      return 0 as unknown as ReturnType<typeof setTimeout>;
    }) as typeof setTimeout);

    const hub = createFakeHub({
      chunkBytes: 10,
      now: () => clock,
      rollout404ChunkCalls: 3, // 3 404s (1s + 2s + 4s = 7s < 60s), then 200
    });
    const res = await uploadFileChunked('/api/v1/projects/p1/workspace/uploads', makeFile(10));
    expect(res.ok).toBe(true);
    expect(hub.calls.filter((c) => c.method === 'PUT').length).toBe(4);
  });

  it('fails immediately on 404 experiment_disabled without retrying', async () => {
    const hub = createFakeHub({
      chunkBytes: 10,
      failChunkCalls: {
        1: {
          kind: 'status',
          status: 404,
          code: 'experiment_disabled',
          message: 'experiment_disabled: hub.large_uploads is off',
        },
      },
    });

    const err = await uploadFileChunked(
      '/api/v1/projects/p1/workspace/uploads',
      makeFile(10)
    ).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(UploadHttpError);
    expect((err as UploadHttpError).code).toBe('experiment_disabled');
    expect((err as Error).message).toBe('Large uploads were disabled.');
    expect(hub.calls.filter((c) => c.method === 'PUT').length).toBe(1);
  });

  it('sends DELETE on user cancel via AbortSignal and throws UploadCancelledError', async () => {
    const controller = new AbortController();
    const hub = createFakeHub({
      chunkBytes: 10,
      onChunk: (callNum) => {
        if (callNum === 2) controller.abort();
      },
    });

    await expect(
      uploadFileChunked('/api/v1/projects/p1/workspace/uploads', makeFile(30), {
        signal: controller.signal,
      })
    ).rejects.toBeInstanceOf(UploadCancelledError);

    expect(hub.calls.some((c) => c.method === 'DELETE')).toBe(true);
    expect(hub.getState()).toBe('aborted');
  });

  it('retries DELETE on 409 upload_busy until 204 when cancelled mid-chunk', async () => {
    const controller = new AbortController();
    const hub = createFakeHub({
      chunkBytes: 10,
      busyDeleteCalls: 2,
      onChunk: (callNum) => {
        if (callNum === 1) controller.abort();
      },
    });

    await expect(
      uploadFileChunked('/api/v1/projects/p1/workspace/uploads', makeFile(20), {
        signal: controller.signal,
      })
    ).rejects.toBeInstanceOf(UploadCancelledError);

    expect(hub.calls.filter((c) => c.method === 'DELETE').length).toBe(3);
    expect(hub.getState()).toBe('aborted');
  });

  it('bounds DELETE retries on persistent 409 upload_busy to abortWindowMs', async () => {
    let clock = 1_000_000;
    _setUploadTestConfig({
      retryDelayMs: 0,
      maxBackoffMs: 0,
      abortRetryDelayMs: 200,
      abortMaxBackoffMs: 1000,
      abortWindowMs: 1500,
      minChunkBytes: 4,
      now: () => clock,
    });
    vi.spyOn(globalThis, 'setTimeout').mockImplementation(((cb: () => void, ms?: number) => {
      clock += ms ?? 0;
      cb();
      return 0 as unknown as ReturnType<typeof setTimeout>;
    }) as typeof setTimeout);

    const controller = new AbortController();
    const hub = createFakeHub({
      chunkBytes: 10,
      busyDeleteCalls: 99,
      now: () => clock,
      onChunk: (callNum) => {
        if (callNum === 1) controller.abort();
      },
    });

    await expect(
      uploadFileChunked('/api/v1/projects/p1/workspace/uploads', makeFile(20), {
        signal: controller.signal,
      })
    ).rejects.toBeInstanceOf(UploadCancelledError);

    // 200 + 400 + 800 = 1400ms (4 attempts total); next 1000ms would exceed 1500ms window.
    expect(hub.calls.filter((c) => c.method === 'DELETE').length).toBe(4);
    expect(clock).toBe(1_001_400);
  });

  it('sends DELETE (and retries upload_busy) when cancelled during complete', async () => {
    const controller = new AbortController();
    const hub = createFakeHub({
      chunkBytes: 10,
      busyDeleteCalls: 1,
      onComplete: () => {
        controller.abort();
      },
    });

    await expect(
      uploadFileChunked('/api/v1/projects/p1/workspace/uploads', makeFile(10), {
        signal: controller.signal,
      })
    ).rejects.toBeInstanceOf(UploadCancelledError);

    expect(hub.calls.filter((c) => c.method === 'DELETE').length).toBe(2);
    expect(hub.getState()).toBe('aborted');
  });

  it('adapts chunk size down on slow links to target ~30 s per chunk (§3.2)', async () => {
    let clock = 0;
    _setUploadTestConfig({
      retryDelayMs: 0,
      maxBackoffMs: 0,
      minChunkBytes: 4,
      slowLinkTargetMs: 30_000,
      now: () => clock,
    });
    // First chunk of 20 bytes takes 60 s (2× slower than target 30 s) → shrinks to 10 bytes.
    const hub = createFakeHub({
      chunkBytes: 20,
      now: () => clock,
      onChunk: (callNum) => {
        clock += callNum === 1 ? 60_000 : 15_000;
      },
    });
    const file = makeFile(40);
    const res = await uploadFileChunked('/api/v1/projects/p1/workspace/uploads', file);
    expect(res.ok).toBe(true);

    const putSizes = hub.calls.filter((c) => c.method === 'PUT').map((c) => c.size);
    expect(putSizes).toEqual([20, 10, 10]);
  });

  it('uploadWorkspaceFiles uses single multipart request when ≤ chunkBytes, chunked when larger, and falls back on proxy 413', async () => {
    // Small file ≤ chunkBytes (2048) → single POST to /files
    const hub1 = createFakeHub({ chunkBytes: 2048 });
    await uploadWorkspaceFiles(
      '/api/v1/projects/p1/workspace/files',
      '/api/v1/projects/p1/workspace/uploads',
      [{ path: 'small.txt', file: makeFile(100, 'small.txt') }],
      { chunkBytes: 2048 }
    );
    expect(hub1.calls.map((c) => [c.method, c.path])).toEqual([
      ['POST', '/api/v1/projects/p1/workspace/files'],
    ]);

    // Large file > chunkBytes → chunked session against /uploads
    const hub2 = createFakeHub({ chunkBytes: 10 });
    await uploadWorkspaceFiles(
      '/api/v1/projects/p1/workspace/files',
      '/api/v1/projects/p1/workspace/uploads',
      [{ path: 'big.bin', file: makeFile(25, 'big.bin') }],
      { chunkBytes: 10 }
    );
    expect(hub2.calls[0].path).toBe('/api/v1/projects/p1/workspace/uploads');
    expect(hub2.calls.at(-1)?.path).toBe(
      '/api/v1/projects/p1/workspace/uploads/upl_test123/complete'
    );

    // Single request hits proxy 413 → halves chunk size and falls back to chunked
    const hub3 = createFakeHub({ chunkBytes: 2048, proxyLimitBytes: 1024 });
    await uploadWorkspaceFiles(
      '/api/v1/projects/p1/workspace/files',
      '/api/v1/projects/p1/workspace/uploads',
      [{ path: 'med.bin', file: makeFile(1100, 'med.bin') }],
      { chunkBytes: 4096 }
    );
    expect(hub3.calls[0]).toMatchObject({
      method: 'POST',
      path: '/api/v1/projects/p1/workspace/files',
    });
    expect(hub3.calls[1]).toMatchObject({
      method: 'POST',
      path: '/api/v1/projects/p1/workspace/uploads',
    });
    expect(hub3.calls.at(-1)?.path).toBe(
      '/api/v1/projects/p1/workspace/uploads/upl_test123/complete'
    );
  });
});
