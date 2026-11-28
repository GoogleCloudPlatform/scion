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
 * Secret request wire types, API client, conflict mapping, and live-update
 * merge helpers (ptone/scion#8747).
 *
 * Mirrors `pkg/messages/secret_requests.go` and `pkg/msgaction/secret_request.go`.
 * Secret values are never logged or retained beyond the active form submission.
 */

import { apiFetch, apiErrorMessageFromBody } from './api.js';

/** Wire status values for a secret request row (`fulfilling` is reported as `pending` + `submitting: true`). */
export type SecretRequestStatus =
  | 'pending'
  | 'fulfilled'
  | 'replaced'
  | 'withdrawn'
  | 'dismissed'
  | 'expired';

/** Supported secret types for a requested secret. */
export type SecretRequestSecretType = 'environment' | 'file' | 'on_demand';

/** Responder surface recorded on fulfill and dismiss requests. */
export type SecretRequestSurface = 'native' | 'standalone_page';

/** Newer version that replaced a secret request row. */
export interface SecretRequestReplacedBy {
  rowId: string;
  messageId: string;
  version: number;
  at: string;
}

/** Metadata recorded when a secret request row is fulfilled (never includes a value). */
export interface SecretRequestFulfilled {
  at: string;
  byUserId?: string;
  byName: string;
  byEmail?: string;
  surface: SecretRequestSurface;
  responseMessageId?: string;
  secretVersion?: number;
}

/** Metadata recorded when a secret request row is withdrawn by the agent or hub. */
export interface SecretRequestWithdrawn {
  at: string;
  reason?: string;
  by: 'agent' | 'hub';
  byName?: string;
}

/** Metadata recorded when a secret request row is dismissed by a user. */
export interface SecretRequestDismissed {
  at: string;
  byUserId?: string;
  byName: string;
  byEmail?: string;
  reason?: string;
  surface: SecretRequestSurface;
  responseMessageId?: string;
}

/** Metadata recorded when a secret request row expires. */
export interface SecretRequestExpired {
  at: string;
}

/** One version of a secret request as returned by the Hub API (never carries values). */
export interface SecretRequestView {
  rowId: string;
  messageId: string;
  projectId: string;
  agentId: string;
  agentName: string;
  key: string;
  version: number;
  description?: string;
  secretType: SecretRequestSecretType;
  target?: string;
  position: number;
  status: SecretRequestStatus;
  submitting?: boolean;
  conversationId?: string;
  createdAt: string;
  expiresAt: string;
  resolvedAt?: string | null;
  secretVersion?: number;
  replacedBy?: SecretRequestReplacedBy;
  fulfilled?: SecretRequestFulfilled;
  withdrawn?: SecretRequestWithdrawn;
  dismissed?: SecretRequestDismissed;
  expired?: SecretRequestExpired;
}

/** 200 response of `GET /api/v1/secret-requests/{messageId}`. */
export interface SecretRequestCardResponse {
  messageId: string;
  projectId: string;
  projectName?: string;
  agentId: string;
  agentName: string;
  conversationId?: string;
  threadId?: string;
  intro?: string;
  rows: SecretRequestView[];
  canRespond: boolean;
  promptAvailable?: boolean;
  reason?: string;
}

/** Live SSE payload for `chat.secret_request.updated` routed through `stateManager`. */
export interface SecretRequestUpdatedEvent {
  kind?: string;
  secret_request?: SecretRequestView;
  messageId?: string;
  threadId?: string;
  projectId?: string;
}

/** Structured conflict or validation details returned on HTTP 400/409 from fulfill or dismiss. */
export interface SecretRequestConflictDetails {
  rowId?: string;
  key?: string;
  version?: number;
  status?: string;
  field?: string;
  secretRequest?: SecretRequestView;
  replacedBy?: SecretRequestReplacedBy;
}

/** Maximum UTF-8 byte length for a single secret value (mirrors hub `maxSecretRequestValueBytes`). */
export const MAX_SECRET_REQUEST_VALUE_BYTES = 64 * 1024;

/** Returns the UTF-8 byte length of a string. */
export function utf8ByteLength(value: string): number {
  return new TextEncoder().encode(value).length;
}

/** Outcome of `GET /api/v1/secret-requests/{messageId}`. */
export type FetchSecretRequestCardResult =
  | { ok: true; card: SecretRequestCardResponse }
  | { ok: false; status: number; code: string; message: string };

/** Outcome of `POST /api/v1/secret-requests/{messageId}/fulfill`. */
export type FulfillSecretRequestResult =
  | {
      ok: true;
      rows: SecretRequestView[];
      responseMessageId: string;
      replayed: boolean;
    }
  | {
      ok: false;
      status: number;
      code: string;
      message: string;
      details?: SecretRequestConflictDetails;
    };

/** Outcome of `POST /api/v1/secret-requests/{messageId}/dismiss`. */
export type DismissSecretRequestResult =
  | {
      ok: true;
      dismissed: SecretRequestView[];
      alreadyDismissed: SecretRequestView[];
      responseMessageId: string;
      replayed: boolean;
    }
  | {
      ok: false;
      status: number;
      code: string;
      message: string;
      details?: SecretRequestConflictDetails;
    };

/** Whether a row is currently pending and editable (not in-flight `submitting`). */
export function isPendingSecretRequest(row: SecretRequestView): boolean {
  return row.status === 'pending' && !row.submitting;
}

/** Whether a row is currently claimed by an in-flight fulfill (`status=pending, submitting=true`). */
export function isSubmittingSecretRequest(row: SecretRequestView): boolean {
  return row.status === 'pending' && Boolean(row.submitting);
}

/** Whether a row has reached a terminal lifecycle status. */
export function isTerminalSecretRequest(row: SecretRequestView): boolean {
  return (
    row.status === 'fulfilled' ||
    row.status === 'replaced' ||
    row.status === 'withdrawn' ||
    row.status === 'dismissed' ||
    row.status === 'expired'
  );
}

function secretRequestStatusRank(row: SecretRequestView): number {
  return isTerminalSecretRequest(row) ? 1 : 0;
}

/**
 * Out-of-order guard for merging live `SecretRequestView` updates:
 * a terminal row never regresses back to `pending`; within `pending`,
 * transitions between `submitting: true` and `submitting: false` (claim,
 * rollback on 503, or stale-claim recovery) are always accepted.
 */
export function newerSecretRequestView(
  held: SecretRequestView,
  incoming: SecretRequestView
): SecretRequestView {
  if (secretRequestStatusRank(incoming) < secretRequestStatusRank(held)) return held;
  return incoming;
}

/**
 * Returns `rows` with each matching `rowId` in `incoming` merged through
 * `newerSecretRequestView`. Returns the original array reference when unchanged.
 */
export function patchSecretRequests(
  rows: readonly SecretRequestView[],
  incoming: readonly SecretRequestView[]
): SecretRequestView[] {
  if (incoming.length === 0) return rows as SecretRequestView[];
  const byId = new Map(incoming.map((r) => [r.rowId, r]));
  let changed = false;
  const next = rows.map((held) => {
    const update = byId.get(held.rowId);
    if (!update) return held;
    const chosen = newerSecretRequestView(held, update);
    if (chosen !== held) changed = true;
    return chosen;
  });
  return changed ? next : (rows as SecretRequestView[]);
}

/** Generates a per-attempt idempotency key (<= 128 bytes). */
export function generateSecretRequestIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `sr-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
}

/** Maps a 409 conflict code from fulfill or dismiss to a clear user-facing message. */
export function mapSecretRequestConflictMessage(
  code: string,
  details?: SecretRequestConflictDetails,
  fallbackMessage?: string
): string {
  const keyLabel = details?.key ? `Key ${details.key}` : 'This secret request';
  switch (code) {
    case 'replaced': {
      const nextVer = details?.replacedBy?.version ?? details?.secretRequest?.replacedBy?.version;
      return nextVer
        ? `${keyLabel} was replaced by v${nextVer}. Open the latest version to respond.`
        : `${keyLabel} was replaced by a newer version.`;
    }
    case 'withdrawn':
      return `${keyLabel} was withdrawn by the agent and is no longer needed.`;
    case 'dismissed':
      return `${keyLabel} was already dismissed.`;
    case 'expired':
      return `${keyLabel} has expired and can no longer be fulfilled.`;
    case 'already_fulfilled':
      return `${keyLabel} has already been fulfilled.`;
    case 'submitting':
      return 'Another submission is currently in progress. Please wait a moment and try again.';
    case 'dismiss_changed':
      return 'No targeted keys are still pending.';
    default:
      return fallbackMessage || 'This secret request changed before your action could complete.';
  }
}

interface RawApiError {
  error?: {
    code?: string;
    message?: string;
    details?: SecretRequestConflictDetails;
  };
  code?: string;
  message?: string;
  details?: SecretRequestConflictDetails;
}

async function parseErrorResponse(
  res: Response,
  fallback: string
): Promise<{ code: string; message: string; details?: SecretRequestConflictDetails }> {
  let body: RawApiError | null;
  try {
    body = (await res.json()) as RawApiError;
  } catch {
    body = null;
  }
  const code = body?.error?.code ?? body?.code ?? '';
  const details = body?.error?.details ?? body?.details;
  const message = apiErrorMessageFromBody(body) || fallback;
  return details !== undefined ? { code, message, details } : { code, message };
}

/** Fetches the secret-request card metadata and rows for `messageId`. */
export async function fetchSecretRequestCard(
  messageId: string,
  opts?: { signal?: AbortSignal }
): Promise<FetchSecretRequestCardResult> {
  try {
    const res = await apiFetch(`/api/v1/secret-requests/${encodeURIComponent(messageId)}`, {
      method: 'GET',
      suppressAccessDeniedToast: true,
      ...(opts?.signal ? { signal: opts.signal } : {}),
    });
    if (res.ok) {
      const card = (await res.json()) as SecretRequestCardResponse;
      return { ok: true, card };
    }
    const err = await parseErrorResponse(res, `Failed to load secret request (${res.status})`);
    return { ok: false, status: res.status, code: err.code, message: err.message };
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') {
      return { ok: false, status: 0, code: 'aborted', message: 'Request cancelled.' };
    }
    return {
      ok: false,
      status: 0,
      code: 'network_error',
      message: 'Could not reach the server. Check your connection and try again.',
    };
  }
}

/** Submits all pending secret values for `messageId`. Never logs `values`. */
export async function fulfillSecretRequest(
  messageId: string,
  body: {
    values: Record<string, string>;
    surface: SecretRequestSurface;
    idempotencyKey: string;
  }
): Promise<FulfillSecretRequestResult> {
  try {
    const res = await apiFetch(`/api/v1/secret-requests/${encodeURIComponent(messageId)}/fulfill`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      suppressAccessDeniedToast: true,
    });
    if (res.ok) {
      const data = (await res.json()) as {
        rows?: SecretRequestView[];
        responseMessageId?: string;
        replayed?: boolean;
      };
      return {
        ok: true,
        rows: Array.isArray(data.rows) ? data.rows : [],
        responseMessageId: data.responseMessageId ?? '',
        replayed: Boolean(data.replayed),
      };
    }
    const err = await parseErrorResponse(res, `Failed to submit secret request (${res.status})`);
    return {
      ok: false,
      status: res.status,
      code: err.code,
      message: err.message,
      ...(err.details !== undefined ? { details: err.details } : {}),
    };
  } catch {
    return {
      ok: false,
      status: 0,
      code: 'network_error',
      message: 'Could not reach the server. Please try again.',
    };
  }
}

/** Dismisses the whole card (omitted `rowIds`) or a subset of pending rows (`rowIds`). */
export async function dismissSecretRequest(
  messageId: string,
  body: {
    rowIds?: string[];
    reason?: string;
    surface: SecretRequestSurface;
    idempotencyKey: string;
  }
): Promise<DismissSecretRequestResult> {
  try {
    const payload: Record<string, unknown> = {
      surface: body.surface,
      idempotencyKey: body.idempotencyKey,
    };
    if (body.rowIds && body.rowIds.length > 0) {
      payload.rowIds = body.rowIds;
    }
    if (body.reason) {
      payload.reason = body.reason;
    }
    const res = await apiFetch(`/api/v1/secret-requests/${encodeURIComponent(messageId)}/dismiss`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
      suppressAccessDeniedToast: true,
    });
    if (res.ok) {
      const data = (await res.json()) as {
        dismissed?: SecretRequestView[];
        alreadyDismissed?: SecretRequestView[];
        responseMessageId?: string;
        replayed?: boolean;
      };
      return {
        ok: true,
        dismissed: Array.isArray(data.dismissed) ? data.dismissed : [],
        alreadyDismissed: Array.isArray(data.alreadyDismissed) ? data.alreadyDismissed : [],
        responseMessageId: data.responseMessageId ?? '',
        replayed: Boolean(data.replayed),
      };
    }
    const err = await parseErrorResponse(res, `Failed to dismiss secret request (${res.status})`);
    return {
      ok: false,
      status: res.status,
      code: err.code,
      message: err.message,
      ...(err.details !== undefined ? { details: err.details } : {}),
    };
  } catch {
    return {
      ok: false,
      status: 0,
      code: 'network_error',
      message: 'Could not reach the server. Please try again.',
    };
  }
}
