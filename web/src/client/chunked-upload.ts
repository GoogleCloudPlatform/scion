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
 * HA-safe resumable chunked web upload client (ptone/scion#8654 §3.2, §3.3, §3.14).
 *
 * Protocol (under `.../uploads`, e.g. `/api/v1/projects/{pid}/workspace/uploads`):
 * - `POST .../uploads`                  → 201 `{ id, transport, chunkBytes, maxBytes, received, expiresAt }`
 * - `PUT  .../uploads/{sid}?offset=N`   → 200 `{ received, expiresAt }` (`application/octet-stream`)
 * - `GET  .../uploads/{sid}`            → 200 `{ id, state, received, declaredSize, expiresAt }`
 * - `POST .../uploads/{sid}/complete`   → 200 target result | 202 `{ state: "finalizing" }`
 * - `DELETE .../uploads/{sid}`          → 204
 *
 * Errors use the Hub envelope `{ error: { code, message, details } }`.
 */

import { apiFetch, parseApiError } from './api.js';

/** Default server chunk size (8 MiB, matches opsettings `uploads.chunk_bytes`). */
export const DEFAULT_CHUNK_BYTES = 8 * 1024 * 1024;

/** Floor for 413 halving and slow-link adaptation (1 MiB, §3.2). */
export const MIN_CHUNK_BYTES = 1 * 1024 * 1024;

/** Target duration per chunk for slow-link chunk-size adaptation (~30 s, §3.2). */
const SLOW_LINK_TARGET_MS = 30_000;

/** Rolling-update retry window for 404 on an active session (60 s, §5). */
const DEFAULT_ROLLOUT_404_WINDOW_MS = 60_000;

/** Cap on exponential backoff between retries (10 s). */
const DEFAULT_MAX_BACKOFF_MS = 10_000;

/** Initial backoff before retrying DELETE on `upload_busy` (200 ms). */
const DEFAULT_ABORT_RETRY_DELAY_MS = 200;

/** Cap on backoff between DELETE retries (1 s). */
const DEFAULT_ABORT_MAX_BACKOFF_MS = 1_000;

/** Bounded total window for retrying DELETE on `upload_busy` (5 s). */
const DEFAULT_ABORT_WINDOW_MS = 5_000;

/** Fallback session expiry window when a response omits `expiresAt` (1 h). */
const FALLBACK_EXPIRY_MS = 60 * 60 * 1000;

/** Room reserved per file for multipart/form-data framing headers. */
const MULTIPART_OVERHEAD_PER_FILE = 1024;

const utf8 = new TextEncoder();

/** Tunable timing/sizing knobs overridden only in unit tests. */
interface UploadClientConfig {
  retryDelayMs: number;
  maxBackoffMs: number;
  abortRetryDelayMs: number;
  abortMaxBackoffMs: number;
  abortWindowMs: number;
  rollout404WindowMs: number;
  minChunkBytes: number;
  slowLinkTargetMs: number;
  now: () => number;
}

const DEFAULT_CONFIG: Readonly<UploadClientConfig> = Object.freeze({
  retryDelayMs: 1000,
  maxBackoffMs: DEFAULT_MAX_BACKOFF_MS,
  abortRetryDelayMs: DEFAULT_ABORT_RETRY_DELAY_MS,
  abortMaxBackoffMs: DEFAULT_ABORT_MAX_BACKOFF_MS,
  abortWindowMs: DEFAULT_ABORT_WINDOW_MS,
  rollout404WindowMs: DEFAULT_ROLLOUT_404_WINDOW_MS,
  minChunkBytes: MIN_CHUNK_BYTES,
  slowLinkTargetMs: SLOW_LINK_TARGET_MS,
  now: () => Date.now(),
});

let config: UploadClientConfig = { ...DEFAULT_CONFIG };

/** @internal test-only: override retry/timing/sizing parameters; pass null to reset. */
export function _setUploadTestConfig(overrides: Partial<UploadClientConfig> | null): void {
  config = overrides ? { ...DEFAULT_CONFIG, ...overrides } : { ...DEFAULT_CONFIG };
}

/** An upload request refused by the hub or a proxy. */
export class UploadHttpError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code = '',
    readonly details?: Record<string, unknown>
  ) {
    super(message);
    this.name = 'UploadHttpError';
  }
}

/** Thrown when the caller cancels an upload via `AbortSignal`. */
export class UploadCancelledError extends Error {
  constructor(message = 'Upload cancelled') {
    super(message);
    this.name = 'UploadCancelledError';
  }
}

/** Format a byte count for human-readable upload messages ("1 GiB", "8 MiB", "900 B"). */
export function formatUploadSize(bytes: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let value = bytes;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i++;
  }
  const text = Number.isInteger(value) ? String(value) : value.toFixed(1);
  return `${text} ${units[i]}`;
}

/**
 * Estimate whether a set of workspace/shared-dir entries fits in a single
 * multipart request whose total body is at most `chunkBytes` (§3.2).
 *
 * Each multipart part carries the path in both the field name and filename
 * headers (up to 3x expansion for escaped characters), plus framing overhead.
 */
export function fitsSingleMultipartRequest(
  entries: ReadonlyArray<{ path: string; file: Blob }>,
  chunkBytes: number = DEFAULT_CHUNK_BYTES
): boolean {
  let total = 0;
  for (const e of entries) {
    total += e.file.size + MULTIPART_OVERHEAD_PER_FILE + 6 * utf8.encode(e.path).length;
  }
  return total <= chunkBytes;
}

function positiveInt(value: unknown, fallback: number): number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0
    ? Math.floor(value)
    : fallback;
}

function parseExpiresAtMs(raw: unknown): number {
  if (typeof raw === 'string' && raw) {
    const ms = Date.parse(raw);
    if (Number.isFinite(ms)) return ms;
  }
  return config.now() + FALLBACK_EXPIRY_MS;
}

function parseRetryAfterMs(res: Response): number | null {
  const header = res.headers?.get?.('retry-after');
  if (!header) return null;
  const seconds = Number(header);
  if (Number.isFinite(seconds) && seconds >= 0) {
    return Math.ceil(seconds * 1000);
  }
  const dateMs = Date.parse(header);
  if (Number.isFinite(dateMs)) {
    return Math.max(0, dateMs - config.now());
  }
  return null;
}

function backoffDelayMs(attempt: number): number {
  const exp = config.retryDelayMs * 2 ** Math.min(attempt, 10);
  return Math.min(exp, config.maxBackoffMs);
}

function isAbortError(err: unknown, signal?: AbortSignal): boolean {
  if (signal?.aborted) return true;
  return (
    err instanceof UploadCancelledError ||
    (err instanceof Error && err.name === 'AbortError') ||
    (typeof DOMException !== 'undefined' &&
      err instanceof DOMException &&
      err.name === 'AbortError')
  );
}

function sleepAbortable(ms: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted) return Promise.reject(new UploadCancelledError());
  if (ms <= 0) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const onAbort = () => {
      clearTimeout(timer);
      reject(new UploadCancelledError());
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

async function toUploadError(res: Response): Promise<UploadHttpError> {
  const info = await parseApiError(res, '');
  if (info.code === 'experiment_disabled') {
    return new UploadHttpError('Large uploads were disabled.', res.status, info.code, info.details);
  }
  if (info.code === 'artifact_attachments_required') {
    return new UploadHttpError(
      info.message ||
        'Large chat attachments require artifact-backed attachments (hub.attachment_artifacts) to be enabled.',
      res.status,
      info.code,
      info.details
    );
  }
  if (info.code === 'disallowed_media_type') {
    return new UploadHttpError(
      info.message || 'This file type is not allowed for chat attachments.',
      res.status,
      info.code,
      info.details
    );
  }
  if (info.code === 'large_uploads_unsupported_storage') {
    return new UploadHttpError(
      info.message || 'Configured blob storage does not support large chat attachments.',
      res.status,
      info.code,
      info.details
    );
  }
  if (info.code === 'chunk_too_small') {
    return new UploadHttpError(
      info.message || 'Upload chunk is below the server minimum chunk size.',
      res.status,
      info.code,
      info.details
    );
  }
  return new UploadHttpError(
    info.message || `Upload failed: HTTP ${res.status}`,
    res.status,
    info.code,
    info.details
  );
}

function sleepUninterruptible(ms: number): Promise<void> {
  if (ms <= 0) return Promise.resolve();
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

/**
 * Send `DELETE .../{sid}` to release the session and its quota.
 *
 * When a user cancels mid-chunk, the server may still hold the chunk lease
 * briefly until it observes the aborted request body, answering `409 upload_busy`.
 * Retry with a short exponential backoff within a bounded window (`abortWindowMs`)
 * so the session transitions to `aborted` instead of pinning quota until `idle_ttl`.
 */
async function abortSessionBestEffort(uploadsUrl: string, id: string): Promise<void> {
  const url = `${uploadsUrl}/${encodeURIComponent(id)}`;
  const deadline = config.now() + config.abortWindowMs;

  for (let attempt = 0; ; attempt++) {
    let res: Response;
    try {
      res = await apiFetch(url, { method: 'DELETE' });
    } catch {
      const delay = Math.min(
        config.abortRetryDelayMs * 2 ** Math.min(attempt, 6),
        config.abortMaxBackoffMs
      );
      if (config.now() + delay > deadline) return;
      await sleepUninterruptible(delay);
      continue;
    }

    if (res.ok || res.status === 204 || res.status === 404) {
      return;
    }

    if (res.status === 409) {
      const info = await parseApiError(res, '');
      if (info.code !== 'upload_busy') return;
    } else if (res.status < 500) {
      return;
    }

    const delay = Math.min(
      config.abortRetryDelayMs * 2 ** Math.min(attempt, 6),
      config.abortMaxBackoffMs
    );
    if (config.now() + delay > deadline) return;
    await sleepUninterruptible(delay);
  }
}

export interface ChunkedUploadOptions {
  /** File name (defaults to `file.name` or `'file'`). */
  name?: string;
  /** Destination relative path (workspace and shared-dir targets). */
  path?: string;
  /** Optional project scope for chat attachment uploads. */
  projectId?: string;
  /** MIME media type (defaults to `file.type` or `'application/octet-stream'`). */
  mediaType?: string;
  /** Whether to overwrite an existing destination file (workspace/shared-dir). */
  overwrite?: boolean;
  /**
   * Optional initial chunk-size ceiling (e.g. after a single-request 413).
   * Always clamped to at most the server's `chunkBytes`.
   */
  initialChunkBytes?: number;
  /** Called with the committed byte offset and total file size. */
  onProgress?: (sent: number, total: number) => void;
  /** AbortSignal for user cancellation (sends best-effort `DELETE`). */
  signal?: AbortSignal;
}

interface StartResponse {
  id?: string;
  transport?: string;
  chunkBytes?: number;
  maxBytes?: number;
  received?: number;
  expiresAt?: string;
}

interface ChunkResponse {
  received?: number;
  expiresAt?: string;
}

interface StatusResponse {
  id?: string;
  state?: string;
  received?: number;
  declaredSize?: number;
  expiresAt?: string;
}

/**
 * Upload a single file via the Hub resumable chunked upload protocol.
 *
 * Resolves to the `200 OK` completion `Response`.
 * Rejects with `UploadCancelledError` on user cancel (after best-effort `DELETE`),
 * `UploadHttpError` on terminal HTTP refusals, or the underlying network `Error`
 * if retries exhaust `expiresAt`.
 */
export async function uploadFileChunked(
  uploadsUrl: string,
  file: Blob & { name?: string },
  options: ChunkedUploadOptions = {}
): Promise<Response> {
  const { signal } = options;
  if (signal?.aborted) throw new UploadCancelledError();

  const name = options.name ?? file.name ?? 'file';
  const mediaType = options.mediaType || file.type || 'application/octet-stream';
  const startPayload: Record<string, unknown> = {
    name,
    size: file.size,
    mediaType,
  };
  if (options.path !== undefined) startPayload.path = options.path;
  if (options.projectId) startPayload.projectId = options.projectId;
  if (options.overwrite !== undefined) startPayload.overwrite = options.overwrite;

  let startRes: Response;
  try {
    startRes = await apiFetch(uploadsUrl, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(startPayload),
      ...(signal ? { signal } : {}),
    });
  } catch (err) {
    if (isAbortError(err, signal)) throw new UploadCancelledError();
    throw err;
  }

  if (!startRes.ok) {
    throw await toUploadError(startRes);
  }

  const start = (await startRes.json()) as StartResponse;
  const id = start.id ?? '';
  if (!id) {
    throw new UploadHttpError('Upload start response missing session id', startRes.status);
  }

  if (signal?.aborted) {
    await abortSessionBestEffort(uploadsUrl, id);
    throw new UploadCancelledError();
  }

  const serverChunkBytes = positiveInt(start.chunkBytes, DEFAULT_CHUNK_BYTES);
  const effectiveMinChunkBytes = Math.min(config.minChunkBytes, serverChunkBytes);
  let maxAllowedChunkBytes =
    options.initialChunkBytes !== undefined && options.initialChunkBytes > 0
      ? Math.max(effectiveMinChunkBytes, Math.min(options.initialChunkBytes, serverChunkBytes))
      : serverChunkBytes;
  let currentChunkBytes = maxAllowedChunkBytes;

  let offset = typeof start.received === 'number' && start.received >= 0 ? start.received : 0;
  let expiresAtMs = parseExpiresAtMs(start.expiresAt);
  const sessionUrl = `${uploadsUrl}/${encodeURIComponent(id)}`;

  options.onProgress?.(offset, file.size);

  // Upload chunks until `offset === file.size`.
  let networkAttempt = 0;
  let busyAttempt = 0;
  let notFoundAttempt = 0;
  let first404At: number | null = null;

  try {
    while (offset < file.size) {
      if (signal?.aborted) throw new UploadCancelledError();

      const chunkLen = Math.min(currentChunkBytes, file.size - offset);
      const chunkStartMs = config.now();
      let res: Response;
      try {
        res = await apiFetch(`${sessionUrl}?offset=${offset}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/octet-stream' },
          body: file.slice(offset, offset + chunkLen),
          ...(signal ? { signal } : {}),
        });
      } catch (err) {
        if (isAbortError(err, signal)) throw new UploadCancelledError();
        const delay = backoffDelayMs(networkAttempt++);
        if (config.now() + delay >= expiresAtMs) {
          throw err instanceof Error ? err : new Error('Upload failed: network error');
        }
        await sleepAbortable(delay, signal);
        continue;
      }

      if (res.ok) {
        networkAttempt = 0;
        busyAttempt = 0;
        notFoundAttempt = 0;
        first404At = null;

        const body = (await res.json().catch(() => ({}))) as ChunkResponse;
        if (body.expiresAt) expiresAtMs = parseExpiresAtMs(body.expiresAt);
        const received = typeof body.received === 'number' ? body.received : offset + chunkLen;
        if (received <= offset) {
          throw new UploadHttpError('Upload failed: the hub received no data', res.status);
        }

        // Slow-link chunk-size adaptation (§3.2): keep each chunk under ~30 s.
        const elapsedMs = config.now() - chunkStartMs;
        if (elapsedMs > config.slowLinkTargetMs && chunkLen > 0) {
          const targetBytes = Math.floor((chunkLen / elapsedMs) * config.slowLinkTargetMs);
          currentChunkBytes = Math.max(
            effectiveMinChunkBytes,
            Math.min(maxAllowedChunkBytes, targetBytes)
          );
        } else if (
          elapsedMs > 0 &&
          elapsedMs < config.slowLinkTargetMs / 3 &&
          chunkLen === currentChunkBytes &&
          currentChunkBytes < maxAllowedChunkBytes
        ) {
          currentChunkBytes = Math.min(maxAllowedChunkBytes, currentChunkBytes * 2);
        }

        offset = received;
        options.onProgress?.(offset, file.size);
        continue;
      }

      const err = await toUploadError(res);

      // 404: experiment_disabled is terminal; plain 404 may be a rolling update
      // hitting an old replica (§5), retried for up to 60 s.
      if (res.status === 404) {
        if (err.code === 'experiment_disabled') throw err;
        const now = config.now();
        if (first404At === null) first404At = now;
        const delay = backoffDelayMs(notFoundAttempt++);
        if (now - first404At + delay < config.rollout404WindowMs && now + delay < expiresAtMs) {
          await sleepAbortable(delay, signal);
          continue;
        }
        throw err;
      }

      // Any non-404 response clears the rollout-404 timer.
      first404At = null;
      notFoundAttempt = 0;

      // 413: halve chunk size (floor 1 MiB) and retry from committed offset (§3.2).
      if (res.status === 413) {
        if (err.code === 'declared_size_exceeded') throw err;
        const serverLimit =
          err.code === 'chunk_too_large' && typeof err.details?.chunkBytes === 'number'
            ? Math.floor(err.details.chunkBytes)
            : null;
        if (serverLimit !== null && serverLimit > 0 && serverLimit < currentChunkBytes) {
          currentChunkBytes = Math.max(effectiveMinChunkBytes, serverLimit);
          maxAllowedChunkBytes = Math.min(maxAllowedChunkBytes, currentChunkBytes);
          continue;
        }
        if (currentChunkBytes > effectiveMinChunkBytes) {
          currentChunkBytes = Math.max(effectiveMinChunkBytes, Math.floor(currentChunkBytes / 2));
          maxAllowedChunkBytes = Math.min(maxAllowedChunkBytes, currentChunkBytes);
          continue;
        }
        throw err;
      }

      // 409: offset_mismatch resumes from server's committed offset; upload_busy backs off.
      if (res.status === 409) {
        if (err.code === 'offset_mismatch') {
          const serverReceived = err.details?.received;
          if (
            typeof serverReceived === 'number' &&
            Number.isFinite(serverReceived) &&
            serverReceived >= 0 &&
            serverReceived <= file.size &&
            serverReceived !== offset
          ) {
            offset = serverReceived;
            networkAttempt = 0;
            busyAttempt = 0;
            options.onProgress?.(offset, file.size);
            continue;
          }
          throw err;
        }
        if (err.code === 'upload_busy') {
          const delay = backoffDelayMs(busyAttempt++);
          if (config.now() + delay >= expiresAtMs) throw err;
          await sleepAbortable(delay, signal);
          continue;
        }
        throw err;
      }

      // 5xx or 429: exponential backoff up to expiresAt.
      if (res.status >= 500 || res.status === 429) {
        const retryAfter = parseRetryAfterMs(res);
        const delay = retryAfter !== null ? retryAfter : backoffDelayMs(networkAttempt++);
        if (config.now() + delay >= expiresAtMs) throw err;
        await sleepAbortable(delay, signal);
        continue;
      }

      // Other 4xx (400, 401, 403, etc.): terminal refusal.
      throw err;
    }
  } catch (err) {
    // Best-effort abort on cancel or terminal failure (skip if session is already
    // gone or experiment disabled, or auth failed).
    const skipAbort =
      err instanceof UploadHttpError &&
      (err.status === 404 || err.status === 401 || err.status === 403);
    if (!skipAbort) {
      await abortSessionBestEffort(uploadsUrl, id);
    }
    throw err;
  }

  try {
    return await completeUpload(sessionUrl, expiresAtMs, signal);
  } catch (err) {
    if (err instanceof UploadCancelledError) {
      await abortSessionBestEffort(uploadsUrl, id);
    }
    throw err;
  }
}

/**
 * Call `POST .../complete`, handling `202 finalizing` by polling `GET .../{sid}`,
 * `upload_busy` backoff, rolling-update 404 retries, and network/5xx backoff up
 * to `expiresAtMs`.
 */
async function completeUpload(
  sessionUrl: string,
  initialExpiresAtMs: number,
  signal?: AbortSignal
): Promise<Response> {
  let expiresAtMs = initialExpiresAtMs;
  let attempt = 0;
  let notFoundAttempt = 0;
  let first404At: number | null = null;

  for (;;) {
    if (signal?.aborted) throw new UploadCancelledError();

    let res: Response;
    try {
      res = await apiFetch(`${sessionUrl}/complete`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
        ...(signal ? { signal } : {}),
      });
    } catch (err) {
      if (isAbortError(err, signal)) throw new UploadCancelledError();
      const delay = backoffDelayMs(attempt++);
      if (config.now() + delay >= expiresAtMs) {
        throw err instanceof Error ? err : new Error('Upload finalize failed: network error');
      }
      await sleepAbortable(delay, signal);
      continue;
    }

    if (res.status === 200) return res;

    if (res.status === 202) {
      attempt = 0;
      first404At = null;
      notFoundAttempt = 0;
      expiresAtMs = await pollUntilFinalized(sessionUrl, expiresAtMs, signal);
      // Session is now completed (or back to uploading); replay POST /complete
      // to retrieve the stored 200 result.
      continue;
    }

    const err = await toUploadError(res);

    if (res.status === 404) {
      if (err.code === 'experiment_disabled') throw err;
      const now = config.now();
      if (first404At === null) first404At = now;
      const delay = backoffDelayMs(notFoundAttempt++);
      if (now - first404At + delay < config.rollout404WindowMs && now + delay < expiresAtMs) {
        await sleepAbortable(delay, signal);
        continue;
      }
      throw err;
    }

    first404At = null;
    notFoundAttempt = 0;

    if (res.status === 409 && err.code === 'upload_busy') {
      const delay = backoffDelayMs(attempt++);
      if (config.now() + delay >= expiresAtMs) throw err;
      await sleepAbortable(delay, signal);
      continue;
    }

    if (res.status >= 500 || res.status === 429) {
      const retryAfter = parseRetryAfterMs(res);
      const delay = retryAfter !== null ? retryAfter : backoffDelayMs(attempt++);
      if (config.now() + delay >= expiresAtMs) throw err;
      await sleepAbortable(delay, signal);
      continue;
    }

    throw err;
  }
}

/**
 * Poll `GET .../{sid}` while the server is finalizing (`202`), returning the
 * updated `expiresAtMs` once the session leaves `finalizing` (e.g. `completed`).
 */
async function pollUntilFinalized(
  sessionUrl: string,
  initialExpiresAtMs: number,
  signal?: AbortSignal
): Promise<number> {
  let expiresAtMs = initialExpiresAtMs;
  let pollAttempt = 0;
  let notFoundAttempt = 0;
  let first404At: number | null = null;

  for (;;) {
    const delay = backoffDelayMs(pollAttempt++);
    if (config.now() + delay >= expiresAtMs) {
      throw new UploadHttpError('Upload finalization timed out', 504, 'finalize_timeout');
    }
    await sleepAbortable(delay, signal);

    let res: Response;
    try {
      res = await apiFetch(sessionUrl, {
        method: 'GET',
        ...(signal ? { signal } : {}),
      });
    } catch (err) {
      if (isAbortError(err, signal)) throw new UploadCancelledError();
      // Continue polling with backoff until expiresAtMs.
      continue;
    }

    if (res.ok) {
      first404At = null;
      notFoundAttempt = 0;
      const body = (await res.json().catch(() => ({}))) as StatusResponse;
      if (body.expiresAt) expiresAtMs = parseExpiresAtMs(body.expiresAt);
      if (body.state === 'finalizing') {
        continue;
      }
      if (body.state === 'completed' || body.state === 'uploading') {
        return expiresAtMs;
      }
      throw new UploadHttpError(
        `Upload ${body.state || 'failed'}`,
        409,
        'upload_invalid_state',
        body.state ? { state: body.state } : undefined
      );
    }

    const err = await toUploadError(res);

    if (res.status === 404) {
      if (err.code === 'experiment_disabled') throw err;
      const now = config.now();
      if (first404At === null) first404At = now;
      const wait = backoffDelayMs(notFoundAttempt++);
      if (now - first404At + wait < config.rollout404WindowMs && now + wait < expiresAtMs) {
        continue;
      }
      throw err;
    }

    first404At = null;
    notFoundAttempt = 0;

    if (res.status >= 500 || res.status === 429) {
      continue;
    }

    throw err;
  }
}

/** One file of a workspace upload and its relative path in the workspace. */
export interface WorkspaceUploadEntry {
  path: string;
  file: File;
}

export interface WorkspaceUploadOptions {
  /** Threshold for single-request multipart vs chunked upload (defaults to 8 MiB, §3.2). */
  chunkBytes?: number;
  /** Called with aggregate bytes sent across all files and total bytes. */
  onProgress?: (sent: number, total: number) => void;
  /** AbortSignal for user cancellation. */
  signal?: AbortSignal;
}

/**
 * Upload files to a workspace (when `hub.large_uploads` is enabled).
 *
 * Sends the existing single multipart request to `filesUrl` only if the whole
 * estimated body is ≤ `chunkBytes` (§3.2). If a proxy answers `413` to that
 * single request, halves the chunk size (floor 1 MiB) and falls back to one
 * chunked session per file against `uploadsUrl`.
 */
export async function uploadWorkspaceFiles(
  filesUrl: string,
  uploadsUrl: string,
  entries: WorkspaceUploadEntry[],
  options: WorkspaceUploadOptions = {}
): Promise<void> {
  const { signal, onProgress } = options;
  if (signal?.aborted) throw new UploadCancelledError();

  const threshold = positiveInt(options.chunkBytes, DEFAULT_CHUNK_BYTES);
  const total = entries.reduce((sum, e) => sum + e.file.size, 0);
  let initialChunkBytes: number | undefined;

  if (fitsSingleMultipartRequest(entries, threshold)) {
    const formData = new FormData();
    for (const e of entries) formData.append(e.path, e.file);
    let res: Response;
    try {
      res = await apiFetch(filesUrl, {
        method: 'POST',
        body: formData,
        ...(signal ? { signal } : {}),
      });
    } catch (err) {
      if (isAbortError(err, signal)) throw new UploadCancelledError();
      throw err;
    }
    if (res.ok) {
      onProgress?.(total, total);
      return;
    }
    const err = await toUploadError(res);
    // Proxy 413 fallback (§3.2): halve chunk size (floor 1 MiB) and switch to chunked.
    if (res.status === 413 && err.code !== 'declared_size_exceeded') {
      initialChunkBytes = Math.max(config.minChunkBytes, Math.floor(threshold / 2));
    } else {
      throw err;
    }
  }

  let done = 0;
  for (const e of entries) {
    if (signal?.aborted) throw new UploadCancelledError();
    const baseName = e.path.split('/').pop() || e.file.name || 'file';
    await uploadFileChunked(uploadsUrl, e.file, {
      name: baseName,
      path: e.path,
      overwrite: true,
      ...(initialChunkBytes !== undefined ? { initialChunkBytes } : {}),
      onProgress: (sent) => onProgress?.(done + sent, total),
      ...(signal ? { signal } : {}),
    });
    done += e.file.size;
  }
}

/** Chat attachment object returned by `POST /api/v1/chat/attachments` or chunked `complete`. */
export interface UploadedChatAttachment {
  id: string;
  name: string;
  mime: string;
  size: number;
  url: string;
}

/** Per-file refusal in a chat attachment upload batch. */
export interface ChatAttachmentFailure {
  name: string;
  error: string;
}

/** Result of a chat attachment upload batch. */
export interface ChatAttachmentUploadResult {
  attachments: UploadedChatAttachment[];
  failures: ChatAttachmentFailure[];
}

export interface ChatAttachmentUploadOptions {
  /**
   * Project scope for topic uploads; omit (undefined) for DMs.
   * Matches the legacy `POST /api/v1/chat/attachments` form field semantics.
   */
  projectId?: string;
  /** Threshold for single-request multipart vs chunked upload (defaults to 8 MiB, §3.2). */
  chunkBytes?: number;
  /** Maximum concurrent chunked sessions for multi-file sends (defaults to 2, §3.2). */
  concurrency?: number;
  /**
   * Legacy per-file limit (bytes) used to decide whether a batch that receives
   * `409 artifact_attachments_required` can fall back to the legacy multipart
   * endpoint (`POST /api/v1/chat/attachments`). Defaults to 10 MiB, capped at
   * 100 MiB (the legacy multipart ceiling).
   */
  legacyMaxFileSize?: number;
  /** Called with aggregate bytes sent across all files and total bytes. */
  onProgress?: (sent: number, total: number) => void;
  /** AbortSignal for user cancellation (sends best-effort `DELETE` per active session). */
  signal?: AbortSignal;
}

const CHAT_ATTACHMENTS_URL = '/api/v1/chat/attachments';
const CHAT_ATTACHMENT_UPLOADS_URL = '/api/v1/chat/attachments/uploads';

/** Default per-file limit for legacy multipart chat fallback (10 MiB). */
const DEFAULT_LEGACY_CHAT_MAX_FILE_SIZE = 10 * 1024 * 1024;
/** Hard ceiling for legacy multipart chat fallback (100 MiB). */
const MAX_LEGACY_CHAT_FALLBACK_FILE_SIZE = 100 * 1024 * 1024;

/** Error codes that abort the whole chat batch rather than a single file. */
const FATAL_CHAT_UPLOAD_CODES = new Set([
  'experiment_disabled',
  'artifact_attachments_required',
  'large_uploads_unsupported_storage',
]);

async function postLegacyChatAttachments(
  files: File[],
  projectId: string | undefined,
  total: number,
  onProgress?: (sent: number, total: number) => void,
  signal?: AbortSignal
): Promise<{ status: number; response: Response; result?: ChatAttachmentUploadResult }> {
  const formData = new FormData();
  if (projectId !== undefined) {
    formData.append('project_id', projectId);
  }
  for (const file of files) {
    formData.append('files', file);
  }

  let res: Response;
  try {
    res = await apiFetch(CHAT_ATTACHMENTS_URL, {
      method: 'POST',
      body: formData,
      ...(signal ? { signal } : {}),
    });
  } catch (err) {
    if (isAbortError(err, signal)) throw new UploadCancelledError();
    throw err;
  }

  if (res.status === 413) {
    return { status: 413, response: res };
  }

  const data = (await res.json().catch(() => ({}))) as {
    attachments?: UploadedChatAttachment[];
    failures?: ChatAttachmentFailure[];
    error?: { code?: string; message?: string };
    message?: string;
  };
  const attachments = data.attachments ?? [];
  const failures = data.failures ?? [];
  if (res.ok) {
    onProgress?.(total, total);
    return { status: res.status, response: res, result: { attachments, failures } };
  }
  if (failures.length > 0) {
    return { status: res.status, response: res, result: { attachments, failures } };
  }
  throw new UploadHttpError(
    data.error?.message || data.message || `Upload failed: HTTP ${res.status}`,
    res.status,
    data.error?.code ?? ''
  );
}

/**
 * Upload chat attachments when `hub.large_uploads` is enabled (§3.2, §3.14, §7 P2).
 *
 * Uses a single `POST /api/v1/chat/attachments` multipart request only when the
 * estimated total request body is ≤ `chunkBytes`. Otherwise runs one chunked
 * session per file against `/api/v1/chat/attachments/uploads` with concurrency 2.
 * A proxy `413` on the single request halves the chunk size (floor 1 MiB) and
 * falls back to chunked sessions.
 *
 * If chunked upload start returns `409 artifact_attachments_required` (i.e.
 * `hub.large_uploads` is on while `hub.attachment_artifacts` is off) and every
 * file fits within the legacy per-file limit, falls back to the legacy
 * multipart request so multi-file batches over `chunkBytes` (e.g. 3 × 5 MiB)
 * continue to succeed.
 */
export async function uploadChatAttachments(
  files: File[],
  options: ChatAttachmentUploadOptions = {}
): Promise<ChatAttachmentUploadResult> {
  const { signal, onProgress } = options;
  if (signal?.aborted) throw new UploadCancelledError();
  if (files.length === 0) return { attachments: [], failures: [] };

  const threshold = positiveInt(options.chunkBytes, DEFAULT_CHUNK_BYTES);
  const concurrency = positiveInt(options.concurrency, 2);
  const total = files.reduce((sum, f) => sum + f.size, 0);
  let initialChunkBytes: number | undefined;
  let singleRequestRejected413 = false;

  const projectFieldOverhead =
    options.projectId !== undefined ? 256 + utf8.encode(options.projectId).length : 0;
  const singleRequestEntries = files.map((file) => ({ path: file.name, file }));

  if (
    threshold > projectFieldOverhead &&
    fitsSingleMultipartRequest(singleRequestEntries, threshold - projectFieldOverhead)
  ) {
    const single = await postLegacyChatAttachments(
      files,
      options.projectId,
      total,
      onProgress,
      signal
    );
    if (single.result) {
      return single.result;
    }
    const err = await toUploadError(single.response);
    if (err.code !== 'declared_size_exceeded') {
      singleRequestRejected413 = true;
      initialChunkBytes = Math.max(config.minChunkBytes, Math.floor(threshold / 2));
    } else {
      throw err;
    }
  }

  onProgress?.(0, total);

  const sentByIndex = new Array<number>(files.length).fill(0);
  const attachmentsByIndex = new Array<UploadedChatAttachment | undefined>(files.length);
  const failuresByIndex = new Array<ChatAttachmentFailure | undefined>(files.length);

  const reportAggregateProgress = (): void => {
    let sum = 0;
    for (let i = 0; i < sentByIndex.length; i++) sum += sentByIndex[i];
    onProgress?.(sum, total);
  };

  const batchAbort = new AbortController();
  const onParentAbort = (): void => batchAbort.abort();
  signal?.addEventListener('abort', onParentAbort, { once: true });

  let nextIndex = 0;
  const batchState: { fatalError: Error | null } = { fatalError: null };

  const worker = async (): Promise<void> => {
    for (;;) {
      if (batchAbort.signal.aborted || signal?.aborted) return;
      const idx = nextIndex++;
      if (idx >= files.length) return;
      const file = files[idx];

      try {
        const res = await uploadFileChunked(CHAT_ATTACHMENT_UPLOADS_URL, file, {
          name: file.name,
          ...(options.projectId ? { projectId: options.projectId } : {}),
          ...(initialChunkBytes !== undefined ? { initialChunkBytes } : {}),
          onProgress: (sent) => {
            sentByIndex[idx] = sent;
            reportAggregateProgress();
          },
          signal: batchAbort.signal,
        });
        const attachment = (await res.json()) as UploadedChatAttachment;
        attachmentsByIndex[idx] = attachment;
        sentByIndex[idx] = file.size;
        reportAggregateProgress();
      } catch (err) {
        if (err instanceof UploadCancelledError || signal?.aborted) {
          return;
        }
        if (err instanceof UploadHttpError && FATAL_CHAT_UPLOAD_CODES.has(err.code)) {
          if (batchState.fatalError === null) {
            batchState.fatalError = err;
            batchAbort.abort();
          }
          return;
        }
        failuresByIndex[idx] = {
          name: file.name,
          error: err instanceof Error ? err.message : 'Upload failed',
        };
      }
    }
  };

  try {
    const workerCount = Math.min(concurrency, files.length);
    await Promise.all(Array.from({ length: workerCount }, () => worker()));
  } finally {
    signal?.removeEventListener('abort', onParentAbort);
  }

  if (signal?.aborted) {
    throw new UploadCancelledError();
  }
  if (batchState.fatalError !== null) {
    if (
      !singleRequestRejected413 &&
      batchState.fatalError instanceof UploadHttpError &&
      batchState.fatalError.code === 'artifact_attachments_required'
    ) {
      const legacyMax = Math.min(
        positiveInt(options.legacyMaxFileSize, DEFAULT_LEGACY_CHAT_MAX_FILE_SIZE),
        MAX_LEGACY_CHAT_FALLBACK_FILE_SIZE
      );
      if (files.every((f) => f.size <= legacyMax)) {
        const fallback = await postLegacyChatAttachments(
          files,
          options.projectId,
          total,
          onProgress,
          signal
        );
        if (fallback.result) {
          return fallback.result;
        }
        throw await toUploadError(fallback.response);
      }
    }
    throw batchState.fatalError;
  }

  return {
    attachments: attachmentsByIndex.filter((a): a is UploadedChatAttachment => a !== undefined),
    failures: failuresByIndex.filter((f): f is ChatAttachmentFailure => f !== undefined),
  };
}
