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
 * Command request wire types, API client, conflict mapping, SSE merge guard,
 * and M1.2-compliant script display formatter (ptone/scion#9283).
 *
 * Mirrors `pkg/messages/command_requests.go` and `cmd/command_request.go`.
 */

import { apiFetch, apiErrorMessageFromBody } from './api.js';

/** Wire status values for a command request row. */
export type CommandRequestStatus =
  | 'pending'
  | 'approved'
  | 'replaced'
  | 'withdrawn'
  | 'dismissed'
  | 'expired';

/** Wire status values for a command run row. */
export type CommandRunStatus =
  | 'queued'
  | 'waking'
  | 'dispatching'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'unknown'
  | 'not_started';

/** Working directory classification mirroring `messages.CommandWorkdirKind`. */
export type CommandWorkdirKind = 'home' | 'workspace' | 'shared_dir' | 'explicit';

/** Responder surface recorded on approve and dismiss requests. */
export type CommandRequestSurface = 'native' | 'standalone_page' | 'cli';

/** Execution container states returned in `exec_container`. */
export type ExecContainerState =
  | 'ready'
  | 'starting'
  | 'suspended'
  | 'stopped'
  | 'unconfigured'
  | 'unavailable';

/** Newer version that replaced a command request row. */
export interface CommandRequestReplacedBy {
  rowId: string;
  messageId?: string;
  version: number;
  at: string;
}

/** Metadata recorded when a command request row is approved. */
export interface CommandRequestApproved {
  at: string;
  byUserId?: string;
  byName: string;
  byEmail?: string;
  surface: CommandRequestSurface;
}

/** Client-visible view of a command run (never includes output bytes). */
export interface CommandRequestRunView {
  runId: string;
  requestId: string;
  status: CommandRunStatus;
  reason?: string;
  wake?: string;
  execAgentId?: string;
  execProjectId?: string;
  shareOutput: boolean;
  timeoutS: number;
  workdir?: string;
  exitCode?: number | null;
  durationMs?: number | null;
  outputBytes?: number | null;
  outputStatus?: string;
  createdAt: string;
  dispatchedAt?: string | null;
  finishedAt?: string | null;
}

/** Approver-only execution container resolution view. */
export interface CommandRequestExecContainerView {
  state: string;
  agentId?: string;
  agentName?: string;
  projectId?: string;
  projectName?: string;
  homePersistent?: boolean | null;
  reason?: string;
}

/** Prior version of the same key whose run ended with status=unknown (D7). */
export interface CommandRequestPreviousUnknown {
  version: number;
  rowId: string;
  runId?: string;
}

/** One version of a command request as returned by the Hub API. */
export interface CommandRequestView {
  rowId: string;
  messageId?: string;
  projectId: string;
  agentId: string;
  agentName: string;
  key: string;
  version: number;
  description?: string;
  script: string;
  workdir?: string;
  workdirKind?: CommandWorkdirKind;
  timeoutS: number;
  shareOutputRequested: boolean;
  commandSha256: string;
  approverUserId: string;
  conversationId?: string;
  status: CommandRequestStatus;
  createdAt: string;
  expiresAt: string;
  resolvedAt?: string | null;
  replacedBy?: CommandRequestReplacedBy;
  approved?: CommandRequestApproved;
  run?: CommandRequestRunView;
  withdrawnReason?: string;
  withdrawnBy?: string;
  dismissReason?: string;
}

/** 200 response of `GET /api/v1/command-requests/{id}`. */
export interface CommandRequestDetailResponse {
  request: CommandRequestView;
  run?: CommandRequestRunView;
  exec_container?: CommandRequestExecContainerView;
  projectName?: string;
  threadId?: string;
  intro?: string;
  approverName?: string;
  approverEmail?: string;
  previousVersionUnknown?: CommandRequestPreviousUnknown;
  canApprove: boolean;
  canDismiss?: boolean;
  outputAvailable?: boolean;
  reason?: string;
}

/** Live SSE payload for `chat.command_request.updated` routed through `stateManager`. */
export interface CommandRequestUpdatedEvent {
  kind?: string;
  command_request?: CommandRequestView;
  messageId?: string;
  threadId?: string;
  projectId?: string;
}

/** Structured conflict or validation details returned on HTTP 400/409. */
export interface CommandRequestConflictDetails {
  status?: string;
  field?: string;
  key?: string;
  version?: number;
  rowId?: string;
  messageId?: string;
}

/** Outcome of `GET /api/v1/command-requests/{id}`. */
export type FetchCommandRequestDetailResult =
  | { ok: true; detail: CommandRequestDetailResponse }
  | { ok: false; status: number; code: string; message: string };

/** Outcome of `POST /api/v1/command-requests/{id}/approve`. */
export type ApproveCommandRequestResult =
  | {
      ok: true;
      request: CommandRequestView;
      run: CommandRequestRunView;
      replayed: boolean;
    }
  | {
      ok: false;
      status: number;
      code: string;
      message: string;
      details?: CommandRequestConflictDetails;
    };

/** Outcome of `POST /api/v1/command-requests/{id}/dismiss` (P5). */
export type DismissCommandRequestResult =
  | {
      ok: true;
      request?: CommandRequestView;
      replayed?: boolean;
    }
  | {
      ok: false;
      status: number;
      code: string;
      message: string;
      details?: CommandRequestConflictDetails;
    };

/** Outcome of `GET /api/v1/command-requests/{id}/output` (P4). */
export type FetchCommandRequestOutputResult =
  | {
      ok: true;
      output: string;
      outputBytes?: number;
      outputStatus?: string;
      truncated?: boolean;
    }
  | {
      ok: false;
      status: number;
      code: string;
      message: string;
    };

/** Classifies a command working directory identically to `messages.CommandWorkdirKind`. */
export function classifyCommandWorkdirKind(workdir: string | undefined): CommandWorkdirKind {
  if (!workdir || workdir === '~') return 'home';
  if (workdir === '/workspace' || workdir.startsWith('/workspace/')) return 'workspace';
  if (
    workdir === '/scion-volumes' ||
    workdir.startsWith('/scion-volumes/') ||
    workdir === '/.scion-volumes' ||
    workdir.startsWith('/.scion-volumes/')
  ) {
    return 'shared_dir';
  }
  return 'explicit';
}

/** Resolves the effective workdir kind, preferring the hub-supplied `workdirKind`. */
export function resolveCommandWorkdirKind(
  request: Pick<CommandRequestView, 'workdir' | 'workdirKind'>
): CommandWorkdirKind {
  if (
    request.workdirKind === 'home' ||
    request.workdirKind === 'workspace' ||
    request.workdirKind === 'shared_dir' ||
    request.workdirKind === 'explicit'
  ) {
    return request.workdirKind;
  }
  return classifyCommandWorkdirKind(request.workdir);
}

/** One rendered token inside a script line for M1.2-compliant display. */
export interface CommandScriptDisplayToken {
  kind: 'ascii' | 'tab' | 'trailing-space' | 'escape';
  text: string;
  codePoint?: number;
  nonAscii?: boolean;
}

/** One numbered line of a formatted script. */
export interface CommandScriptDisplayLine {
  lineNumber: number;
  tokens: CommandScriptDisplayToken[];
}

/** Formatted script representation for approval surfaces (M1.2). */
export interface FormattedCommandScript {
  lines: CommandScriptDisplayLine[];
  nonAsciiCount: number;
  lineCount: number;
  byteLength: number;
  mentionsAgentWritablePaths: boolean;
}

const AGENT_WRITABLE_PATH_RE =
  /(?:^|[\s"'`=:(/])(?:\/workspace(?:\/|$|[\s"'`):;])|\/\.?scion-volumes(?:\/|$|[\s"'`):;]))/;

/**
 * Formats an agent-authored script for approval surfaces following the exact
 * same rule as CLI `formatCommandScriptForDisplay` (audit M1.2):
 * - preserves `\n` line breaks with 1-based line numbers;
 * - renders `\t` as a visible `\t` token;
 * - renders trailing ASCII spaces as visible `\u{0020}` tokens;
 * - renders EVERY non-ASCII code point (> 0x7E) and every non-printable ASCII
 *   code point (< 0x20 or 0x7F) as a visible `\u{XXXX}` escape badge.
 *
 * Iterates Unicode code points so astral characters (e.g. U+E0100) are
 * formatted as a single escape rather than split UTF-16 surrogates.
 */
export function formatCommandScriptForDisplay(script: string): FormattedCommandScript {
  const byteLength = new TextEncoder().encode(script).length;
  const trimmed = script.endsWith('\n') && script.length > 1 ? script.slice(0, -1) : script;
  const rawLines = trimmed.split('\n');

  let nonAsciiCount = 0;
  const lines: CommandScriptDisplayLine[] = [];

  for (let li = 0; li < rawLines.length; li++) {
    const codePoints = [...rawLines[li]];
    let trailingStart = codePoints.length;
    while (trailingStart > 0 && codePoints[trailingStart - 1] === ' ') {
      trailingStart--;
    }

    const tokens: CommandScriptDisplayToken[] = [];
    let asciiBuf = '';
    const flushAscii = (): void => {
      if (asciiBuf.length > 0) {
        tokens.push({ kind: 'ascii', text: asciiBuf });
        asciiBuf = '';
      }
    };

    for (let i = 0; i < codePoints.length; i++) {
      const ch = codePoints[i];
      const cp = ch.codePointAt(0) ?? 0;
      if (cp === 0x09) {
        flushAscii();
        tokens.push({ kind: 'tab', text: '\\t', codePoint: cp });
      } else if (i >= trailingStart && cp === 0x20) {
        flushAscii();
        tokens.push({ kind: 'trailing-space', text: '\\u{0020}', codePoint: cp });
      } else if (cp >= 0x20 && cp <= 0x7e) {
        asciiBuf += ch;
      } else {
        flushAscii();
        const nonAscii = cp > 0x7e;
        if (nonAscii) {
          nonAsciiCount++;
        }
        const hex = cp.toString(16).toUpperCase().padStart(4, '0');
        tokens.push({
          kind: 'escape',
          text: `\\u{${hex}}`,
          codePoint: cp,
          nonAscii,
        });
      }
    }
    flushAscii();

    lines.push({
      lineNumber: li + 1,
      tokens,
    });
  }

  return {
    lines,
    nonAsciiCount,
    lineCount: lines.length,
    byteLength,
    mentionsAgentWritablePaths: AGENT_WRITABLE_PATH_RE.test(script),
  };
}

/** Formats a timeout in seconds into a concise human-readable label. */
export function formatCommandTimeout(timeoutS: number): string {
  if (!Number.isFinite(timeoutS) || timeoutS <= 0) return '10m';
  if (timeoutS < 60) return `${timeoutS}s`;
  const mins = Math.floor(timeoutS / 60);
  const secs = timeoutS % 60;
  return secs === 0 ? `${mins}m` : `${mins}m ${secs}s`;
}

/** Formats a run duration in milliseconds into a concise human-readable label. */
export function formatCommandDurationMs(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '0ms';
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const totalSec = ms / 1000;
  if (totalSec < 60) return `${totalSec.toFixed(totalSec < 10 ? 1 : 0)}s`;
  const mins = Math.floor(totalSec / 60);
  const secs = Math.round(totalSec % 60);
  return secs === 0 ? `${mins}m` : `${mins}m ${secs}s`;
}

/** Whether a command request status is terminal. */
export function isTerminalCommandRequest(status: string): boolean {
  return (
    status === 'approved' ||
    status === 'replaced' ||
    status === 'withdrawn' ||
    status === 'dismissed' ||
    status === 'expired'
  );
}

/** Whether a command run status is terminal. */
export function isTerminalCommandRun(status: string | undefined): boolean {
  return (
    status === 'succeeded' ||
    status === 'failed' ||
    status === 'unknown' ||
    status === 'not_started'
  );
}

function commandRunStatusRank(status: string | undefined): number {
  switch (status) {
    case 'queued':
      return 1;
    case 'waking':
      return 2;
    case 'dispatching':
      return 3;
    case 'running':
      return 4;
    case 'succeeded':
    case 'failed':
    case 'unknown':
    case 'not_started':
      return 5;
    default:
      return 0;
  }
}

/**
 * Out-of-order guard for merging live `CommandRequestView` updates:
 * - a terminal request status never regresses back to `pending`;
 * - a run status never regresses to an earlier lifecycle stage;
 * - preserves `execAgentId` / `execProjectId` from `held.run` when SSE omits them.
 */
export function newerCommandRequestView(
  held: CommandRequestView,
  incoming: CommandRequestView
): CommandRequestView {
  const heldTerminal = isTerminalCommandRequest(held.status);
  const incomingTerminal = isTerminalCommandRequest(incoming.status);
  if (heldTerminal && !incomingTerminal) {
    return held;
  }

  let mergedRun = incoming.run ?? held.run;
  if (held.run && incoming.run) {
    const heldRank = commandRunStatusRank(held.run.status);
    const incRank = commandRunStatusRank(incoming.run.status);
    if (incRank < heldRank) {
      mergedRun = held.run;
    } else {
      mergedRun = {
        ...incoming.run,
        ...(incoming.run.execAgentId
          ? {}
          : held.run.execAgentId
            ? { execAgentId: held.run.execAgentId }
            : {}),
        ...(incoming.run.execProjectId
          ? {}
          : held.run.execProjectId
            ? { execProjectId: held.run.execProjectId }
            : {}),
      };
    }
  }

  return {
    ...held,
    ...incoming,
    ...(mergedRun ? { run: mergedRun } : {}),
  };
}

/** Generates a per-attempt idempotency key (<= 128 bytes). */
export function generateCommandRequestIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `cr-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
}

/** Maps a 403/409 conflict code from approve or dismiss to a clear user-facing message. */
export function mapCommandRequestConflictMessage(
  code: string,
  details?: CommandRequestConflictDetails,
  fallbackMessage?: string
): string {
  switch (code) {
    case 'exec_container_stopped':
      return 'Start your execution container, then approve.';
    case 'exec_container_unconfigured':
      return 'No execution container is configured. Set up an execution container in Profile Settings first.';
    case 'exec_container_unavailable':
      return 'Your execution container is currently unavailable.';
    case 'command_hash_mismatch':
      return 'The command changed since this view loaded. Reload and review before approving.';
    case 'not_approver':
      return 'Only the named approver may approve this command request.';
    case 'replaced':
      return 'This command request was replaced by a newer version.';
    case 'withdrawn':
      return 'This command request was withdrawn by the agent and is no longer needed.';
    case 'dismissed':
      return 'This command request has already been dismissed.';
    case 'expired':
      return 'This command request has expired and can no longer be approved.';
    case 'already_resolved':
      return 'This command request has already been approved.';
    default: {
      if (details?.status === 'replaced') {
        return 'This command request was replaced by a newer version.';
      }
      return fallbackMessage || 'This command request changed before your action could complete.';
    }
  }
}

interface RawApiError {
  error?: {
    code?: string;
    message?: string;
    details?: CommandRequestConflictDetails;
  };
  code?: string;
  message?: string;
  details?: CommandRequestConflictDetails;
}

async function parseErrorResponse(
  res: Response,
  fallback: string
): Promise<{ code: string; message: string; details?: CommandRequestConflictDetails }> {
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

/** Fetches the command-request detail view for `id` (rowId or messageId). */
export async function fetchCommandRequestDetail(
  id: string,
  opts?: { signal?: AbortSignal }
): Promise<FetchCommandRequestDetailResult> {
  try {
    const res = await apiFetch(`/api/v1/command-requests/${encodeURIComponent(id)}`, {
      method: 'GET',
      suppressAccessDeniedToast: true,
      ...(opts?.signal ? { signal: opts.signal } : {}),
    });
    if (res.ok) {
      const detail = (await res.json()) as CommandRequestDetailResponse;
      return { ok: true, detail };
    }
    const err = await parseErrorResponse(res, `Failed to load command request (${res.status})`);
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

/** Approves a pending command request by row ID. */
export async function approveCommandRequest(
  rowId: string,
  body: {
    row_id: string;
    command_sha256: string;
    share_output: boolean;
    surface: CommandRequestSurface;
    idempotency_key: string;
  }
): Promise<ApproveCommandRequestResult> {
  try {
    const res = await apiFetch(`/api/v1/command-requests/${encodeURIComponent(rowId)}/approve`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      suppressAccessDeniedToast: true,
    });
    if (res.ok) {
      const data = (await res.json()) as {
        request: CommandRequestView;
        run: CommandRequestRunView;
        replayed?: boolean;
      };
      return {
        ok: true,
        request: data.request,
        run: data.run,
        replayed: Boolean(data.replayed),
      };
    }
    const err = await parseErrorResponse(res, `Failed to approve command request (${res.status})`);
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

/** Dismisses a pending command request by row ID (feature-detected; API arrives in P5). */
export async function dismissCommandRequest(
  rowId: string,
  body: {
    reason?: string;
    surface: CommandRequestSurface;
    idempotency_key: string;
  }
): Promise<DismissCommandRequestResult> {
  try {
    const payload: Record<string, unknown> = {
      surface: body.surface,
      idempotency_key: body.idempotency_key,
    };
    if (body.reason) {
      payload.reason = body.reason;
    }
    const res = await apiFetch(`/api/v1/command-requests/${encodeURIComponent(rowId)}/dismiss`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
      suppressAccessDeniedToast: true,
    });
    if (res.ok) {
      const data = (await res.json()) as {
        request?: CommandRequestView;
        replayed?: boolean;
      };
      return {
        ok: true,
        ...(data.request ? { request: data.request } : {}),
        replayed: Boolean(data.replayed),
      };
    }
    const err = await parseErrorResponse(res, `Failed to dismiss command request (${res.status})`);
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

/** Fetches the approver-only run output tail (feature-detected; route arrives in P4). */
export async function fetchCommandRequestOutput(
  rowId: string,
  opts?: { signal?: AbortSignal }
): Promise<FetchCommandRequestOutputResult> {
  try {
    const res = await apiFetch(`/api/v1/command-requests/${encodeURIComponent(rowId)}/output`, {
      method: 'GET',
      suppressAccessDeniedToast: true,
      ...(opts?.signal ? { signal: opts.signal } : {}),
    });
    if (res.ok) {
      const contentType = res.headers.get('content-type') ?? '';
      if (contentType.includes('application/json')) {
        const data = (await res.json()) as {
          output?: string;
          outputBytes?: number;
          outputStatus?: string;
          truncated?: boolean;
        };
        return {
          ok: true,
          output: data.output ?? '',
          ...(data.outputBytes !== undefined ? { outputBytes: data.outputBytes } : {}),
          ...(data.outputStatus !== undefined ? { outputStatus: data.outputStatus } : {}),
          ...(data.truncated !== undefined ? { truncated: data.truncated } : {}),
        };
      }
      const text = await res.text();
      return { ok: true, output: text };
    }
    const err = await parseErrorResponse(res, `Failed to load command output (${res.status})`);
    return { ok: false, status: res.status, code: err.code, message: err.message };
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') {
      return { ok: false, status: 0, code: 'aborted', message: 'Request cancelled.' };
    }
    return {
      ok: false,
      status: 0,
      code: 'network_error',
      message: 'Could not reach the server. Please try again.',
    };
  }
}
