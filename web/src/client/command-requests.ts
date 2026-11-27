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

/** Responder surface recorded on approve, dismiss, and markdown-block requests. */
export type CommandRequestSurface = 'native' | 'standalone_page' | 'cli' | 'markdown_block';

/** Canonical hash constants mirroring `pkg/messages/command_requests.go` (§4.4). */
export const COMMAND_REQUEST_HASH_PREFIX = 'scion.command_request.v1\n';
export const COMMAND_REQUEST_INTERPRETER_BASH_LOGIN = 'bash-login';
export const MAX_COMMAND_REQUEST_SCRIPT_BYTES = 4096;
export const DEFAULT_COMMAND_REQUEST_TIMEOUT_S = 600;

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
  canWake?: boolean;
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

/** Structured conflict or validation details returned on HTTP 400/409/422. */
export interface CommandRequestConflictDetails {
  status?: string;
  field?: string;
  key?: string;
  version?: number;
  rowId?: string;
  messageId?: string;
  request?: CommandRequestView;
  run?: CommandRequestRunView;
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

/** Formatted single-line hashed field (e.g. workdir) with M1.2/O1 per-code-point escaping. */
export interface FormattedCommandInline {
  tokens: CommandScriptDisplayToken[];
  nonAsciiCount: number;
}

/**
 * Formats a single-line hashed field (such as `workdir`) with the same
 * per-code-point escape rule as `formatCommandScriptForDisplay` (O1):
 * printable ASCII (`0x20..0x7E`) is kept verbatim; every non-ASCII (`> 0x7E`)
 * or control (`< 0x20`, `0x7F`) code point becomes a visible `\u{XXXX}` token.
 */
export function formatCommandInlineForDisplay(value: string): FormattedCommandInline {
  const tokens: CommandScriptDisplayToken[] = [];
  let asciiBuf = '';
  let nonAsciiCount = 0;

  const flushAscii = (): void => {
    if (asciiBuf.length > 0) {
      tokens.push({ kind: 'ascii', text: asciiBuf });
      asciiBuf = '';
    }
  };

  for (const ch of value) {
    const cp = ch.codePointAt(0) ?? 0;
    if (cp >= 0x20 && cp <= 0x7e) {
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

  return { tokens, nonAsciiCount };
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
    case 'markdown_block_not_found':
      return 'Ask the agent to send this as a command request.';
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

/**
 * Computes the canonical SHA-256 hex digest of a command request payload
 * (§4.4), matching Go's `messages.CommandRequestCanonicalHash` byte-for-byte.
 *
 * Verified against Go-generated fixtures in
 * `pkg/messages/testdata/command_request_hash_vectors.json`.
 */
export async function computeCommandRequestCanonicalHash(
  script: string,
  workdir: string,
  timeoutS: number
): Promise<string> {
  // Property insertion order matches Go's `canonicalCommandPayload` struct
  // field order: interpreter, script, timeout_s, workdir. JSON.stringify does
  // not HTML-escape `<`, `>`, `&` (matching Go's `SetEscapeHTML(false)`).
  // Escape U+2028/U+2029 as `\u2028`/`\u2029` to match Go's encoding/json.
  const json = JSON.stringify({
    interpreter: COMMAND_REQUEST_INTERPRETER_BASH_LOGIN,
    script,
    timeout_s: timeoutS,
    workdir,
  })
    .replace(/\u2028/g, '\\u2028')
    .replace(/\u2029/g, '\\u2029');
  const bytes = new TextEncoder().encode(COMMAND_REQUEST_HASH_PREFIX + json);
  const digest = await crypto.subtle.digest('SHA-256', bytes);
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, '0')).join('');
}

function isBidiControlCodePoint(cp: number): boolean {
  return (
    (cp >= 0x202a && cp <= 0x202e) ||
    (cp >= 0x2066 && cp <= 0x2069) ||
    cp === 0x200e ||
    cp === 0x200f ||
    cp === 0x061c
  );
}

const INVISIBLE_OR_NON_ASCII_WS_RE = /[\p{Cf}\p{Zl}\p{Zp}\p{Zs}]/u;

function isInvisibleOrNonASCIIWhitespaceCodePoint(ch: string, cp: number): boolean {
  if (cp <= 0x7e) return false;
  if (
    cp === 0x115f ||
    cp === 0x1160 ||
    cp === 0x3164 ||
    cp === 0xffa0 ||
    (cp >= 0xfe00 && cp <= 0xfe0f) ||
    (cp >= 0xe0100 && cp <= 0xe01ef) ||
    (cp >= 0x180b && cp <= 0x180f) ||
    cp === 0x2800
  ) {
    return true;
  }
  return INVISIBLE_OR_NON_ASCII_WS_RE.test(ch);
}

/**
 * Validates a command script against the §4.5 content rules for presentation
 * (e.g. disabling the Markdown Execute button with a tooltip reason). The Hub
 * enforces the authoritative check server-side.
 *
 * Returns `null` when valid, or a short user-facing reason when invalid.
 */
export function validateCommandRequestScript(script: string): string | null {
  if (script.length === 0) {
    return 'Script is empty.';
  }
  const byteLength = new TextEncoder().encode(script).length;
  if (byteLength > MAX_COMMAND_REQUEST_SCRIPT_BYTES) {
    return `Script is ${byteLength} bytes; the limit is ${MAX_COMMAND_REQUEST_SCRIPT_BYTES} bytes.`;
  }
  for (const ch of script) {
    const cp = ch.codePointAt(0) ?? 0;
    if (cp === 0) {
      return 'Script contains a NUL byte.';
    }
    if (cp < 0x20 && cp !== 0x0a && cp !== 0x09) {
      const hex = cp.toString(16).toUpperCase().padStart(4, '0');
      return `Script contains control character U+${hex}.`;
    }
    if (cp === 0x7f || (cp >= 0x80 && cp <= 0x9f)) {
      const hex = cp.toString(16).toUpperCase().padStart(4, '0');
      return `Script contains control character U+${hex}.`;
    }
    if (isBidiControlCodePoint(cp)) {
      const hex = cp.toString(16).toUpperCase().padStart(4, '0');
      return `Script contains bidirectional control character U+${hex}.`;
    }
    if (isInvisibleOrNonASCIIWhitespaceCodePoint(ch, cp)) {
      const hex = cp.toString(16).toUpperCase().padStart(4, '0');
      return `Script contains invisible or non-ASCII whitespace character U+${hex}.`;
    }
  }
  return null;
}

/**
 * Creates and approves a v1 command request from a verbatim fenced shell block
 * in an agent message (`POST /api/v1/command-requests/from-message`, §4.14).
 */
export async function createCommandRequestFromMessage(body: {
  message_id: string;
  script: string;
  command_sha256: string;
  share_output: boolean;
  idempotency_key: string;
}): Promise<ApproveCommandRequestResult> {
  try {
    const res = await apiFetch('/api/v1/command-requests/from-message', {
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
    const err = await parseErrorResponse(res, `Failed to execute command block (${res.status})`);
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

interface RawUserExecContainerResponse {
  configured?: boolean;
  state?: string;
  reason?: string;
  agent?: {
    id?: string;
    slug?: string;
    name?: string;
    projectId?: string;
    projectName?: string;
    homePersistent?: boolean | null;
  };
}

/**
 * Fetches the signed-in user's execution container state from
 * `GET /api/v1/users/me/exec-container` and normalizes it into a
 * `CommandRequestExecContainerView` for Markdown Execute eligibility and
 * confirmation dialogs. Returns `null` when unavailable (e.g. experiment off
 * or network failure).
 */
export async function fetchCurrentUserExecContainer(opts?: {
  signal?: AbortSignal;
}): Promise<CommandRequestExecContainerView | null> {
  try {
    const res = await apiFetch('/api/v1/users/me/exec-container', {
      method: 'GET',
      suppressAccessDeniedToast: true,
      ...(opts?.signal ? { signal: opts.signal } : {}),
    });
    if (!res.ok) return null;
    const data = (await res.json()) as RawUserExecContainerResponse;
    if (!data.configured) {
      return { state: 'unconfigured' };
    }
    const state = data.state || 'unavailable';
    const agent = data.agent;
    return {
      state,
      ...(agent?.id ? { agentId: agent.id } : {}),
      ...(agent?.name || agent?.slug ? { agentName: agent.name || agent.slug } : {}),
      ...(agent?.projectId ? { projectId: agent.projectId } : {}),
      ...(agent?.projectName ? { projectName: agent.projectName } : {}),
      ...(agent?.homePersistent !== undefined ? { homePersistent: agent.homePersistent } : {}),
      // Suspended containers default to wakeable in presentation when the
      // exec-container summary does not carry a per-agent canWake boolean;
      // the Hub enforces lifecycle permission on POST from-message.
      ...(state === 'suspended' ? { canWake: true } : {}),
      ...(data.reason ? { reason: data.reason } : {}),
    };
  } catch {
    return null;
  }
}
