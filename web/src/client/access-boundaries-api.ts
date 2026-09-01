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
 * Access Boundary — API client (WP C1)
 *
 * Narrow API client for the access boundary domain. All frontend components
 * import from this module — no page file may define its own fetch helpers.
 *
 * Features:
 *   - Parses/validates discriminated unions from API responses
 *   - Normalizes structured errors (B7 error contract)
 *   - AbortController for request cancellation
 *   - Request sequence protection (stale response prevention)
 *   - Idempotency keys for mutations
 *   - Unknown-outcome refetch helpers
 */

import { apiFetch } from './api.js';
import type {
  AccessBoundaryCommitRequest,
  AccessBoundaryCommitResponse,
  AccessBoundaryDetail,
  AccessBoundaryListFilters,
  AccessBoundaryListResponse,
  AccessBoundaryPreview,
  AccessBoundaryPreviewJob,
  AccessBoundaryPreviewRequest,
  AccessBoundaryAuditPage,
  AffectedPrincipalsPage,
  BoundaryRevision,
  PageToken,
  StructuredAPIError,
} from '../shared/access-boundaries.js';
import {
  isConstraintSubject,
  isConstraintScope,
  isStructuredAPIErrorResponse,
} from '../shared/access-boundaries.js';

/* -------------------------------------------------------------------------- */
/* Constants                                                                  */
/* -------------------------------------------------------------------------- */

const BASE_PATH = '/api/v1/admin/access-constraints';
const PREVIEW_PATH = '/api/v1/admin/access-constraint-previews';
const PREVIEW_JOB_PATH = '/api/v1/admin/access-constraint-preview-jobs';

/* -------------------------------------------------------------------------- */
/* Error handling                                                             */
/* -------------------------------------------------------------------------- */

/**
 * Typed error for access boundary API failures. Wraps the B7 structured error
 * contract so callers can inspect `code` for context-aware recovery.
 */
export class AccessBoundaryAPIError extends Error {
  /**
   * Error code from the B7 structured error contract.
   *
   * Known codes are defined by `AccessBoundaryErrorCode` in the shared
   * contract module; the type is widened to `string` because the server may
   * introduce new codes before the client is updated. Use
   * {@link isRevisionConflict}, {@link isLockoutError}, or direct comparison
   * against the exported `ACCESS_BOUNDARY_ERROR_CODES` array for type-safe
   * matching.
   */
  readonly code: string;
  readonly httpStatus: number;
  readonly retryable: boolean;
  readonly correlationId: string;
  readonly requestId: string | undefined;
  readonly details: Record<string, unknown> | undefined;

  constructor(httpStatus: number, error: StructuredAPIError) {
    super(error.message);
    this.name = 'AccessBoundaryAPIError';
    this.code = error.code;
    this.httpStatus = httpStatus;
    this.retryable = error.retryable;
    this.correlationId = error.correlationId;
    this.requestId = error.requestId;
    this.details = error.details;
  }
}

/**
 * Shape of a legacy (pre-B7) error response body. Used to safely access
 * fields in the fallback path of {@link parseErrorResponse} without
 * triggering `@typescript-eslint/no-unsafe-*` rules on every property access.
 */
interface LegacyErrorBody {
  error?:
    | {
        code?: string;
        message?: string;
        details?: Record<string, unknown>;
      }
    | string;
  message?: string;
}

/**
 * Parse an error response into an AccessBoundaryAPIError. Falls back to a
 * generic error if the response doesn't match the B7 structured error contract.
 */
async function parseErrorResponse(res: Response): Promise<AccessBoundaryAPIError> {
  try {
    // The response body is untyped JSON — it may be a B7 structured error,
    // a legacy error shape, or something entirely unexpected. The guard
    // `isStructuredAPIErrorResponse` handles the happy path; the fallback
    // path intentionally accesses loosely-typed fields, consistent with how
    // api.ts:extractApiError handles the same pattern.
    // eslint-disable-next-line @typescript-eslint/no-unsafe-assignment
    const body: LegacyErrorBody = await res.json();
    if (isStructuredAPIErrorResponse(body)) {
      return new AccessBoundaryAPIError(res.status, body.error);
    }
    // Fall back to legacy error shape
    const errorObj = typeof body.error === 'object' ? body.error : null;
    const msg =
      errorObj?.message ||
      body.message ||
      (typeof body.error === 'string' ? body.error : null) ||
      res.statusText;
    const fallback: StructuredAPIError = {
      code: errorObj?.code || '',
      message: msg,
      retryable: false,
      correlationId: '',
    };
    if (errorObj?.details) {
      fallback.details = errorObj.details;
    }
    return new AccessBoundaryAPIError(res.status, fallback);
  } catch {
    return new AccessBoundaryAPIError(res.status, {
      code: '',
      message: res.statusText || `HTTP ${res.status}`,
      retryable: false,
      correlationId: '',
    });
  }
}

/* -------------------------------------------------------------------------- */
/* Request sequence protection                                                */
/* -------------------------------------------------------------------------- */

/**
 * Tracks the latest request sequence number per endpoint key, so stale
 * responses (from slower earlier requests) are discarded when a newer
 * request has already completed.
 *
 * SSR safety: this module lives under `web/src/client/` and imports from
 * `./api.js` (the browser fetch layer). It is never imported server-side.
 * Module-level mutable state is safe here.
 */
const sequenceCounters = new Map<string, number>();

function nextSequence(key: string): number {
  const next = (sequenceCounters.get(key) ?? 0) + 1;
  sequenceCounters.set(key, next);
  return next;
}

function isStale(key: string, seq: number): boolean {
  return (sequenceCounters.get(key) ?? 0) > seq;
}

/**
 * Error thrown when a response arrives after a newer request for the same
 * logical endpoint has already completed.
 */
export class StaleResponseError extends Error {
  constructor(key: string) {
    super(`Response for "${key}" is stale — a newer request has superseded it.`);
    this.name = 'StaleResponseError';
  }
}

/* -------------------------------------------------------------------------- */
/* Idempotency                                                                */
/* -------------------------------------------------------------------------- */

/** Generate an idempotency key for mutations. */
function generateIdempotencyKey(): string {
  return `idk-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

/* -------------------------------------------------------------------------- */
/* Helpers                                                                    */
/* -------------------------------------------------------------------------- */

/** Build a query string from filters, omitting undefined/null/empty values. */
function buildQueryString(filters: object): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(filters)) {
    if (value !== undefined && value !== null && value !== '') {
      params.set(key, String(value));
    }
  }
  const qs = params.toString();
  return qs ? `?${qs}` : '';
}

/**
 * Validate that summary items contain well-formed discriminated unions.
 * Logs warnings for malformed items rather than throwing, so partial
 * pages are still usable.
 */
function validateListItems<T extends { subject?: unknown; scope?: unknown }>(items: T[]): T[] {
  for (const item of items) {
    if (item.subject !== undefined && !isConstraintSubject(item.subject)) {
      console.warn('[access-boundaries-api] Malformed subject in response item:', item.subject);
    }
    if (item.scope !== undefined && !isConstraintScope(item.scope)) {
      console.warn('[access-boundaries-api] Malformed scope in response item:', item.scope);
    }
  }
  return items;
}

interface RequestOptions {
  signal?: AbortSignal | null;
}

/** Normalize signal for RequestInit: convert undefined to null. */
function signalInit(signal: AbortSignal | null | undefined): AbortSignal | null {
  return signal ?? null;
}

/* -------------------------------------------------------------------------- */
/* API methods                                                                */
/* -------------------------------------------------------------------------- */

/**
 * List access boundaries with optional filtering.
 *
 * Supports AbortController for cancellation and request sequence protection
 * so that stale list responses are discarded.
 */
export async function list(
  filters: AccessBoundaryListFilters = {},
  options: RequestOptions = {}
): Promise<AccessBoundaryListResponse> {
  const seqKey = 'list';
  const seq = nextSequence(seqKey);

  const qs = buildQueryString(filters);
  const res = await apiFetch(`${BASE_PATH}${qs}`, {
    signal: signalInit(options.signal),
  });

  if (isStale(seqKey, seq)) {
    throw new StaleResponseError(seqKey);
  }

  if (!res.ok) {
    throw await parseErrorResponse(res);
  }

  const data = (await res.json()) as AccessBoundaryListResponse;
  validateListItems(data.items);
  return data;
}

/**
 * Get a single access boundary by ID.
 */
export async function get(
  constraintId: string,
  options: RequestOptions = {}
): Promise<AccessBoundaryDetail> {
  const seqKey = `get:${constraintId}`;
  const seq = nextSequence(seqKey);

  const res = await apiFetch(`${BASE_PATH}/${encodeURIComponent(constraintId)}`, {
    signal: signalInit(options.signal),
  });

  if (isStale(seqKey, seq)) {
    throw new StaleResponseError(seqKey);
  }

  if (!res.ok) {
    throw await parseErrorResponse(res);
  }

  const data = (await res.json()) as AccessBoundaryDetail;
  if (!isConstraintSubject(data.subject)) {
    console.warn('[access-boundaries-api] Malformed subject in detail response:', data.subject);
  }
  if (!isConstraintScope(data.scope)) {
    console.warn('[access-boundaries-api] Malformed scope in detail response:', data.scope);
  }
  return data;
}

/**
 * Request an impact preview for a boundary mutation.
 *
 * Returns either a synchronous preview (201) or an async job handle (202).
 */
export async function preview(
  request: AccessBoundaryPreviewRequest,
  options: RequestOptions = {}
): Promise<
  | { kind: 'preview'; preview: AccessBoundaryPreview }
  | { kind: 'job'; job: AccessBoundaryPreviewJob }
> {
  const idempotencyKey = generateIdempotencyKey();

  const res = await apiFetch(PREVIEW_PATH, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Idempotency-Key': idempotencyKey,
    },
    body: JSON.stringify(request),
    signal: signalInit(options.signal),
  });

  if (!res.ok) {
    throw await parseErrorResponse(res);
  }

  if (res.status === 202) {
    const job = (await res.json()) as AccessBoundaryPreviewJob;
    return { kind: 'job', job };
  }

  const previewData = (await res.json()) as AccessBoundaryPreview;
  return { kind: 'preview', preview: previewData };
}

/**
 * Poll an async preview job by job ID.
 */
export async function pollPreviewJob(
  jobId: string,
  options: RequestOptions = {}
): Promise<AccessBoundaryPreviewJob> {
  const res = await apiFetch(`${PREVIEW_JOB_PATH}/${encodeURIComponent(jobId)}`, {
    signal: signalInit(options.signal),
  });

  if (!res.ok) {
    throw await parseErrorResponse(res);
  }

  return (await res.json()) as AccessBoundaryPreviewJob;
}

/**
 * Poll a preview job until it completes, with exponential backoff.
 *
 * Returns the completed job. Throws on failure or cancellation.
 * The caller SHOULD pass an AbortSignal for user-initiated cancellation.
 */
export async function pollPreviewJobUntilDone(
  jobId: string,
  options: RequestOptions & {
    /** Initial poll interval in ms. Defaults to 2000. */
    initialIntervalMs?: number;
    /** Maximum poll interval in ms. Defaults to 10000. */
    maxIntervalMs?: number;
    /** Called on each poll with the current job state. */
    onProgress?: (job: AccessBoundaryPreviewJob) => void;
  } = {}
): Promise<AccessBoundaryPreviewJob> {
  const { initialIntervalMs = 2000, maxIntervalMs = 10000, onProgress, signal } = options;
  let interval = initialIntervalMs;

  // eslint-disable-next-line no-constant-condition
  while (true) {
    const job = await pollPreviewJob(jobId, { signal: signalInit(signal) });

    if (onProgress) {
      onProgress(job);
    }

    if (job.status === 'succeeded' || job.status === 'failed' || job.status === 'cancelled') {
      return job;
    }

    // Use server-suggested retry interval when available
    if (job.retryAfterSeconds !== null && job.retryAfterSeconds > 0) {
      interval = job.retryAfterSeconds * 1000;
    }

    // Check for abort before entering the delay — if the signal was aborted
    // between poll resolution and here, the addEventListener below would
    // register on an already-dispatched event and never fire.
    if (signal?.aborted) {
      throw signal.reason ?? new DOMException('Aborted', 'AbortError');
    }

    await new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => {
        // Remove the abort listener when the timeout completes normally
        // to prevent memory leak of event listeners on the signal.
        if (signal && onAbort) {
          signal.removeEventListener('abort', onAbort);
        }
        resolve();
      }, interval);
      let onAbort: (() => void) | undefined;
      if (signal) {
        onAbort = () => {
          clearTimeout(timer);
          reject(signal.reason ?? new DOMException('Aborted', 'AbortError'));
        };
        signal.addEventListener('abort', onAbort, { once: true });
      }
    });

    // Exponential backoff, capped
    interval = Math.min(interval * 1.5, maxIntervalMs);
  }
}

/**
 * Commit a create operation (POST with If-Match: "new").
 */
export async function commitCreate(
  request: AccessBoundaryCommitRequest,
  options: RequestOptions = {}
): Promise<AccessBoundaryCommitResponse> {
  const idempotencyKey = generateIdempotencyKey();

  const res = await apiFetch(BASE_PATH, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'If-Match': '"new"',
      'Idempotency-Key': idempotencyKey,
    },
    body: JSON.stringify(request),
    signal: signalInit(options.signal),
  });

  if (!res.ok) {
    throw await parseErrorResponse(res);
  }

  return (await res.json()) as AccessBoundaryCommitResponse;
}

/**
 * Commit an update operation (PUT with If-Match for optimistic concurrency).
 */
export async function commitUpdate(
  constraintId: string,
  revision: BoundaryRevision,
  request: AccessBoundaryCommitRequest,
  options: RequestOptions = {}
): Promise<AccessBoundaryCommitResponse> {
  const idempotencyKey = generateIdempotencyKey();

  const res = await apiFetch(`${BASE_PATH}/${encodeURIComponent(constraintId)}`, {
    method: 'PUT',
    headers: {
      'Content-Type': 'application/json',
      'If-Match': `"${revision}"`,
      'Idempotency-Key': idempotencyKey,
    },
    body: JSON.stringify(request),
    signal: signalInit(options.signal),
  });

  if (!res.ok) {
    throw await parseErrorResponse(res);
  }

  return (await res.json()) as AccessBoundaryCommitResponse;
}

/**
 * Commit a delete operation (POST …/:delete with If-Match for optimistic concurrency).
 */
export async function commitDelete(
  constraintId: string,
  revision: BoundaryRevision,
  request: Pick<AccessBoundaryCommitRequest, 'previewToken' | 'acknowledgements'>,
  options: RequestOptions = {}
): Promise<AccessBoundaryCommitResponse> {
  const idempotencyKey = generateIdempotencyKey();

  const res = await apiFetch(`${BASE_PATH}/${encodeURIComponent(constraintId)}:delete`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'If-Match': `"${revision}"`,
      'Idempotency-Key': idempotencyKey,
    },
    body: JSON.stringify(request),
    signal: signalInit(options.signal),
  });

  if (!res.ok) {
    throw await parseErrorResponse(res);
  }

  return (await res.json()) as AccessBoundaryCommitResponse;
}

/**
 * List affected principals for a boundary, with pagination.
 */
export async function listAffected(
  constraintId: string,
  params: { pageToken?: PageToken; pageSize?: number } = {},
  options: RequestOptions = {}
): Promise<AffectedPrincipalsPage> {
  const seqKey = `affected:${constraintId}`;
  const seq = nextSequence(seqKey);

  const qs = buildQueryString(params);
  const res = await apiFetch(
    `${BASE_PATH}/${encodeURIComponent(constraintId)}/affected-principals${qs}`,
    { signal: signalInit(options.signal) }
  );

  if (isStale(seqKey, seq)) {
    throw new StaleResponseError(seqKey);
  }

  if (!res.ok) {
    throw await parseErrorResponse(res);
  }

  return (await res.json()) as AffectedPrincipalsPage;
}

/**
 * List audit events for a boundary, with pagination.
 */
export async function listAudit(
  constraintId: string,
  params: { pageToken?: PageToken; pageSize?: number } = {},
  options: RequestOptions = {}
): Promise<AccessBoundaryAuditPage> {
  const seqKey = `audit:${constraintId}`;
  const seq = nextSequence(seqKey);

  const qs = buildQueryString(params);
  const res = await apiFetch(`${BASE_PATH}/${encodeURIComponent(constraintId)}/audit${qs}`, {
    signal: signalInit(options.signal),
  });

  if (isStale(seqKey, seq)) {
    throw new StaleResponseError(seqKey);
  }

  if (!res.ok) {
    throw await parseErrorResponse(res);
  }

  return (await res.json()) as AccessBoundaryAuditPage;
}

/* -------------------------------------------------------------------------- */
/* Unknown-outcome helpers                                                    */
/* -------------------------------------------------------------------------- */

/**
 * Re-fetch a boundary after an unknown-outcome mutation (e.g., network timeout
 * during commit). Returns the current state so the caller can compare revisions
 * and determine whether the mutation applied.
 *
 * This is the ONLY safe recovery path — never retry a commit blindly.
 *
 * @returns `revisionChanged` — `true` when the boundary's revision differs
 *   from `expectedRevision`. Note: this does NOT guarantee that *this*
 *   mutation was applied — a concurrent mutation by a different user would
 *   also change the revision. Callers should inspect `current` for
 *   definitive reconciliation.
 */
export async function refetchAfterUnknownOutcome(
  constraintId: string,
  expectedRevision: BoundaryRevision,
  options: RequestOptions = {}
): Promise<{
  revisionChanged: boolean;
  current: AccessBoundaryDetail;
}> {
  const detail = await get(constraintId, options);
  return {
    revisionChanged: detail.revision !== expectedRevision,
    current: detail,
  };
}

/**
 * Determine whether a mutation error is retryable after re-previewing.
 *
 * `retryable: true` means "the same intent may succeed after re-previewing" —
 * it never means "resend this exact request".
 */
export function isRetryableAfterRepreview(error: unknown): boolean {
  if (error instanceof AccessBoundaryAPIError) {
    return error.retryable;
  }
  return false;
}

/**
 * Determine whether an error represents a revision conflict.
 * The caller should offer "reload and reapply" rather than silent overwrite.
 */
export function isRevisionConflict(error: unknown): boolean {
  return error instanceof AccessBoundaryAPIError && error.code === 'REVISION_CONFLICT';
}

/**
 * Determine whether an error represents a lockout prevention block.
 */
export function isLockoutError(error: unknown): boolean {
  return error instanceof AccessBoundaryAPIError && error.code === 'CONSTRAINT_ADMIN_LOCKOUT';
}

/**
 * Reset the request sequence counter for a key. Useful when the UI
 * knows it wants the next response regardless of prior in-flight requests
 * (e.g., after a mutation changes the underlying data).
 */
export function resetSequence(key: string): void {
  sequenceCounters.delete(key);
}

/**
 * Reset all sequence counters. Useful on page navigation.
 */
export function resetAllSequences(): void {
  sequenceCounters.clear();
}
