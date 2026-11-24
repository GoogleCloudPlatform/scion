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
 * `<scion-chat-secret-request>`: reusable secret-request card component
 * (ptone/scion#8747, design §4.6).
 *
 * Rendered on the standalone page (`/secret-requests/:messageId`, P5) and
 * designed to drop directly into native chat (`chat-message.ts`, P6).
 *
 * Security invariants:
 * - Secret values live only in an in-memory Map for the active form, never in
 *   drafts, localStorage, sessionStorage, or serialized state.
 * - Values are cleared immediately after submit, when rows leave `pending`,
 *   when `messageId` changes, and on disconnect/navigation.
 * - Values are never logged.
 * - Hidden (`nothing`) when `hub.agent_secrets` is off (presentation only;
 *   the Hub enforces the experiment server-side).
 */

import { LitElement, css, html, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import { FEATURE_FLAGS_CHANGED_EVENT, agentSecretsEnabled } from '../../../utils/feature-flags.js';
import {
  MAX_SECRET_REQUEST_VALUE_BYTES,
  dismissSecretRequest,
  fetchSecretRequestCard,
  fulfillSecretRequest,
  generateSecretRequestIdempotencyKey,
  isPendingSecretRequest,
  isSubmittingSecretRequest,
  mapSecretRequestConflictMessage,
  newerSecretRequestView,
  utf8ByteLength,
  type SecretRequestCardResponse,
  type SecretRequestConflictDetails,
  type SecretRequestSurface,
  type SecretRequestUpdatedEvent,
  type SecretRequestView,
} from '../../../client/secret-requests.js';
import {
  MAX_ACTION_DISMISS_REASON_RUNES,
  MessageActionStateController,
  dismissDialogStyles,
  messageActionBannerStyles,
  messageActionRowStatusClass,
} from './message-action-state.js';

export interface SecretRequestFulfilledDetail {
  messageId: string;
  rows: SecretRequestView[];
  responseMessageId: string;
  replayed: boolean;
}

export interface SecretRequestDismissedDetail {
  messageId: string;
  rows: SecretRequestView[];
  responseMessageId: string;
  replayed: boolean;
}

export interface SecretRequestUpdatedDetail {
  messageId: string;
  rows: SecretRequestView[];
}

export interface SecretRequestRefreshedDetail {
  messageId: string;
  card: SecretRequestCardResponse;
}

type LineEndingStyle = 'lf' | 'crlf' | 'mixed';

function normalizeToLf(value: string): string {
  return value.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
}

function isUndoRedoShortcut(e: KeyboardEvent): boolean {
  if (!(e.ctrlKey || e.metaKey) || e.altKey) return false;
  const key = e.key.toLowerCase();
  return key === 'z' || key === 'y';
}

function detectLineEndingStyle(value: string): LineEndingStyle {
  if (!value.includes('\r')) {
    return 'lf';
  }
  const withoutCrlf = value.replace(/\r\n/g, '');
  if (withoutCrlf.includes('\r') || withoutCrlf.includes('\n')) {
    return 'mixed';
  }
  return 'crlf';
}

/**
 * Maps a character offset in an LF-normalized textarea display string back to
 * the corresponding offset in the raw stored value (where `\r\n` takes 2 code
 * units in raw and 1 in display).
 */
function displayOffsetToRawOffset(raw: string, displayOffset: number): number {
  let rawIdx = 0;
  let dispIdx = 0;
  while (rawIdx < raw.length && dispIdx < displayOffset) {
    if (raw[rawIdx] === '\r' && raw[rawIdx + 1] === '\n') {
      rawIdx += 2;
    } else {
      rawIdx += 1;
    }
    dispIdx += 1;
  }
  return rawIdx;
}

@customElement('scion-chat-secret-request')
export class ScionChatSecretRequest extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  private readonly actionState = new MessageActionStateController<SecretRequestUpdatedEvent>(this, {
    kind: 'secret_request',
    dismissMinAgeMs: 0,
    getMessageId: (): string => this.messageId,
    getThreadId: (): string => this.threadId,
    hasDismissableRows: (): boolean => this.canRespondNow && this.pendingEditableRows.length > 0,
    getDismissableRowIds: (): string[] => this.pendingEditableRows.map((r) => r.rowId),
    onUpdated: (payload): void => this.handleLiveUpdate(payload),
    jumpEventName: 'secret-request-jump',
  });

  /** The secret-request prompt message id. */
  @property({ type: String })
  messageId = '';

  /** Requested secret rows on this card. */
  @property({ type: Array })
  rows: SecretRequestView[] = [];

  /** Name of the requesting agent. */
  @property({ type: String })
  agentName = '';

  /** Name of the project the agent belongs to. */
  @property({ type: String })
  projectName = '';

  /** Optional thread id for live update filtering. */
  @property({ type: String })
  threadId = '';

  /** Signed-in user's id (renders "you" when they fulfilled/dismissed). */
  @property({ type: String })
  currentUserId = '';

  /** Whether the viewer may fulfill or dismiss pending rows on this card. */
  @property({ type: Boolean })
  canRespond = false;

  /** Explanation shown when `canRespond` is false. */
  @property({ type: String })
  reason = '';

  /** Responder surface recorded on fulfill/dismiss POSTs. */
  @property({ type: String })
  surface: SecretRequestSurface = 'native';

  /**
   * Local row overrides received from own POSTs, 409 conflicts, or live
   * `chat.secret_request.updated` SSE events.
   */
  @state()
  private localRows = new Map<string, SecretRequestView>();

  /** Whether our own fulfill POST is in flight. */
  @state()
  private submitting = false;

  /** Inline error message from the last fulfill/dismiss attempt. */
  @state()
  private errorMessage = '';

  /** 403 denial reason learned from a POST refusal. */
  @state()
  private deniedReason = '';

  /** Per-row visibility toggle for masked inputs (keyed by rowId). */
  @state()
  private revealedRows = new Set<string>();

  /** Line-ending style snapshotted when a row is revealed, to avoid HTML textarea LF normalization. */
  private readonly revealedLineEndings = new Map<string, LineEndingStyle>();

  /**
   * Rows whose current value was captured via verbatim paste or file load
   * (or contains CR/LF) so the masked password input stays empty while the
   * full verbatim value is held in `formValues`.
   */
  private readonly capturedRows = new Set<string>();

  /** Monotonic counter bumped on `clearFormValues()` to cancel stale async file reads. */
  private clearGeneration = 0;

  /** Row ID whose revealed textarea should receive focus after clicking Show. */
  private pendingFocusRowId = '';

  /** Reactive counter bumped when in-memory form values change, without storing values in @state. */
  @state()
  private formRevision = 0;

  /**
   * Ephemeral in-memory form values keyed by rowId.
   * Never persisted to drafts, localStorage, sessionStorage, or logs.
   */
  private readonly formValues = new Map<string, string>();

  /** Per-attempt idempotency key for fulfill (cleared on edit or completion). */
  private pendingFulfillIdempotencyKey = '';

  /** Per-attempt idempotency key for dismiss (reused on retry of the same rows + reason). */
  private pendingDismissIdempotencyKey = '';

  /** Signature of the dismiss attempt (`rowIds` + `reason`) bound to `pendingDismissIdempotencyKey`. */
  private pendingDismissAttemptSignature = '';

  /** AbortController for stale-card refetch on 400 values mismatch. */
  private refetchAbort: AbortController | null = null;

  private readonly onFlagsChanged = (): void => {
    this.requestUpdate();
  };

  /** Clears ephemeral values on pagehide so bfcache navigation cannot restore secrets. */
  private readonly onPageHide = (): void => {
    this.clearFormValues();
  };

  static override styles = css`
    ${dismissDialogStyles}
    ${messageActionBannerStyles}

    :host {
      display: block;
      font-size: var(--chat-fs-base, 0.9375rem);
      line-height: 1.5;
    }

    .card {
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
      overflow: hidden;
    }

    .card-head {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.65rem 0.85rem;
      border-bottom: 1px solid var(--scion-border);
      background: var(--scion-bg);
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary);
    }

    .card-head sl-icon {
      flex-shrink: 0;
      font-size: 1rem;
      color: var(--scion-primary);
    }

    .card-title {
      font-weight: 600;
      color: var(--scion-text);
    }

    .row {
      padding: 0.75rem 0.85rem;
    }

    .row + .row {
      border-top: 1px solid var(--scion-border);
    }

    .row.replaced .row-meta,
    .row.withdrawn .row-meta,
    .row.dismissed .row-meta,
    .row.expired .row-meta,
    .row.closed .row-meta {
      opacity: 0.65;
    }

    .row-top {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.4rem;
    }

    .key-label {
      font: 600 0.8125rem/1.2 var(--scion-font-mono);
      background: var(--scion-badge-neutral-bg);
      color: var(--scion-badge-neutral-text);
      padding: 0.2rem 0.45rem;
      border-radius: var(--scion-radius-sm);
      overflow-wrap: anywhere;
    }

    .version {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
      white-space: nowrap;
    }

    .type-badge {
      font-size: 0.75rem;
      color: var(--scion-text-secondary);
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-sm);
      padding: 0.1rem 0.4rem;
      white-space: nowrap;
    }

    .target-path {
      font: 400 0.75rem/1.2 var(--scion-font-mono);
      color: var(--scion-text-muted);
      overflow-wrap: anywhere;
    }

    .description {
      margin: 0.4rem 0 0;
      color: var(--scion-text-secondary);
      font-size: var(--chat-fs-sm, 0.875rem);
      white-space: pre-wrap;
      overflow-wrap: anywhere;
    }

    .input-wrap {
      display: flex;
      align-items: flex-start;
      gap: 0.4rem;
      margin-top: 0.55rem;
    }

    .secret-input {
      flex: 1 1 auto;
      min-width: 0;
      height: 2.25rem;
      box-sizing: border-box;
      padding: 0.4rem 0.6rem;
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      background: var(--scion-surface);
      color: var(--scion-text);
      font-family: var(--scion-font-mono);
      font-size: 0.875rem;
      line-height: 1.4;
    }

    .secret-input::-ms-reveal,
    .secret-input::-ms-clear {
      display: none;
    }

    .secret-input:focus {
      outline: none;
      border-color: var(--scion-border-active);
    }

    .secret-input:focus-visible {
      outline: 2px solid var(--scion-primary);
      outline-offset: 1px;
    }

    .secret-input:disabled {
      opacity: 0.6;
      cursor: not-allowed;
      background: var(--scion-bg-subtle);
    }

    .secret-textarea {
      flex: 1 1 auto;
      min-width: 0;
      min-height: calc(var(--secret-rows, 2) * 1.4em + 0.9rem + 2px);
      max-height: 12rem;
      resize: vertical;
      box-sizing: border-box;
      padding: 0.45rem 0.6rem;
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      background: var(--scion-surface);
      color: var(--scion-text);
      font-family: var(--scion-font-mono);
      font-size: 0.875rem;
      line-height: 1.4;
    }

    .secret-textarea:focus {
      outline: none;
      border-color: var(--scion-border-active);
    }

    .secret-textarea:focus-visible {
      outline: 2px solid var(--scion-primary);
      outline-offset: 1px;
    }

    .secret-textarea:disabled {
      opacity: 0.6;
      cursor: not-allowed;
      background: var(--scion-bg-subtle);
    }

    .secret-textarea[readonly] {
      background: var(--scion-bg-subtle);
    }

    .mixed-line-endings-note {
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-muted);
    }

    .file-controls {
      display: flex;
      align-items: center;
      flex-wrap: wrap;
      gap: 0.5rem;
      min-height: 1.875rem;
      margin-top: 0.4rem;
    }

    .file-picker-input {
      position: absolute;
      width: 1px;
      height: 1px;
      padding: 0;
      margin: -1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
      border: 0;
    }

    .file-value-status {
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-muted);
    }

    .file-value-status sl-icon {
      flex-shrink: 0;
      font-size: 0.875rem;
    }

    @media (pointer: coarse) {
      .secret-input,
      .secret-textarea {
        font-size: 1rem;
      }
    }

    .toggle-visibility {
      flex: 0 0 auto;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 2.25rem;
      height: 2.25rem;
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      background: var(--scion-bg);
      color: var(--scion-text-secondary);
      cursor: pointer;
    }

    .toggle-visibility:hover:not(:disabled) {
      border-color: var(--scion-border-hover);
      color: var(--scion-text);
    }

    .toggle-visibility:focus-visible {
      outline: 2px solid var(--scion-primary);
      outline-offset: 1px;
    }

    .toggle-visibility:disabled {
      opacity: 0.5;
      cursor: not-allowed;
    }

    .row-foot {
      margin-top: 0.45rem;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-muted);
      display: flex;
      gap: 0.35rem;
      align-items: center;
      flex-wrap: wrap;
    }

    .row-foot sl-icon {
      font-size: 0.95rem;
    }

    .row-foot .ok {
      color: var(--scion-status-success);
    }

    .row-foot b {
      color: var(--scion-text-secondary);
    }

    .row-foot a.jump {
      color: var(--scion-primary);
      text-decoration: none;
      font-weight: 500;
    }

    .row-foot a.jump:hover {
      text-decoration: underline;
    }

    .storage-notice {
      display: flex;
      align-items: center;
      gap: 0.45rem;
      padding: 0.55rem 0.85rem;
      border-top: 1px solid var(--scion-border);
      background: var(--scion-bg);
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary);
    }

    .storage-notice sl-icon {
      flex-shrink: 0;
      color: var(--scion-text-muted);
    }

    .storage-notice strong {
      color: var(--scion-text);
      font-weight: 600;
    }

    .status-alert {
      display: flex;
      align-items: flex-start;
      gap: 0.45rem;
      padding: 0.55rem 0.85rem;
      border-top: 1px solid var(--scion-border);
      font-size: var(--chat-fs-sm, 0.875rem);
    }

    .status-alert.blocked {
      background: var(--scion-warning-bg);
      color: var(--scion-warning-text);
    }

    .status-alert.error {
      background: var(--scion-error-bg);
      color: var(--scion-error-text);
    }

    .status-alert sl-icon {
      flex-shrink: 0;
      margin-top: 0.15rem;
    }

    .card-foot {
      display: flex;
      align-items: center;
      justify-content: flex-end;
      gap: 0.5rem;
      flex-wrap: wrap;
      padding: 0.65rem 0.85rem;
      border-top: 1px solid var(--scion-border);
      background: var(--scion-bg);
    }

    .dismiss-dialog-title {
      display: inline-flex;
      align-items: center;
      gap: 0.45rem;
    }

    .dismiss-dialog-title sl-icon {
      color: var(--scion-status-danger);
      font-size: 1.05rem;
      flex-shrink: 0;
    }

    .dismiss-row-list {
      display: flex;
      flex-direction: column;
      gap: 0.45rem;
    }

    .dismiss-single-row {
      display: flex;
      align-items: center;
      gap: 0.45rem;
    }

    .dismiss-pick {
      display: flex;
      align-items: flex-start;
    }

    .dismiss-pick sl-checkbox::part(base) {
      align-items: flex-start;
    }

    .dismiss-pick sl-checkbox::part(label) {
      color: var(--scion-text);
      font-size: var(--chat-fs-base, 0.9375rem);
      line-height: 1.4;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
    window.addEventListener('pagehide', this.onPageHide);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
    window.removeEventListener('pagehide', this.onPageHide);
    this.refetchAbort?.abort();
    this.refetchAbort = null;
    this.clearFormValues();
  }

  protected override willUpdate(changed: PropertyValues): void {
    if (changed.has('messageId')) {
      this.refetchAbort?.abort();
      this.refetchAbort = null;
      this.clearFormValues();
      this.localRows = new Map();
      this.errorMessage = '';
      this.deniedReason = '';
      this.submitting = false;
      this.pendingFulfillIdempotencyKey = '';
      this.pendingDismissIdempotencyKey = '';
      this.pendingDismissAttemptSignature = '';
      this.actionState.closeDismissDialogState();
    }
    if (changed.has('rows')) {
      this.pruneNonPendingFormValues();
      this.actionState.reconcileOpenDismissDialog();
    }
  }

  /** Effective sorted rows combining `this.rows` with newer local/live updates. */
  private get views(): SecretRequestView[] {
    const merged = this.rows.map((row) => {
      const local = this.localRows.get(row.rowId);
      return local ? newerSecretRequestView(row, local) : row;
    });
    return merged.sort((a, b) => a.position - b.position);
  }

  /** Rows that are currently pending and not in-flight `submitting`. */
  private get pendingEditableRows(): SecretRequestView[] {
    return this.views.filter((r) => isPendingSecretRequest(r));
  }

  /** Effective permission state (prop + any 403 refusal learned on POST). */
  private get canRespondNow(): boolean {
    return this.canRespond && !this.deniedReason;
  }

  /** Reason text explaining why the form is disabled when `canRespondNow` is false. */
  private get disabledReasonText(): string {
    return (
      this.deniedReason || this.reason || "You don't have permission to respond to this request."
    );
  }

  private get resolvedAgentName(): string {
    return this.agentName || this.views[0]?.agentName || 'this agent';
  }

  /** Clears all ephemeral secret values and visibility toggles from memory. */
  clearFormValues(): void {
    this.clearGeneration++;
    this.pendingFocusRowId = '';
    this.formValues.clear();
    this.capturedRows.clear();
    this.revealedLineEndings.clear();
    this.revealedRows = new Set();
    this.pendingFulfillIdempotencyKey = '';
    this.formRevision++;
  }

  protected override updated(): void {
    if (this.pendingFocusRowId) {
      const targetId = this.pendingFocusRowId;
      this.pendingFocusRowId = '';
      const escaped =
        typeof CSS !== 'undefined' && typeof CSS.escape === 'function'
          ? CSS.escape(targetId)
          : targetId;
      const ta = this.shadowRoot?.querySelector<HTMLTextAreaElement>(
        `textarea[data-row-id="${escaped}"]`
      );
      ta?.focus();
    }
  }

  private pruneNonPendingFormValues(): void {
    let changed = false;
    const pendingIds = new Set(this.pendingEditableRows.map((r) => r.rowId));
    for (const rowId of [...this.formValues.keys()]) {
      if (!pendingIds.has(rowId)) {
        this.formValues.delete(rowId);
        this.capturedRows.delete(rowId);
        this.revealedLineEndings.delete(rowId);
        changed = true;
      }
    }
    if (this.revealedRows.size > 0) {
      const nextRevealed = new Set<string>();
      for (const rowId of this.revealedRows) {
        if (pendingIds.has(rowId)) nextRevealed.add(rowId);
        else {
          this.revealedLineEndings.delete(rowId);
          changed = true;
        }
      }
      if (nextRevealed.size !== this.revealedRows.size) {
        this.revealedRows = nextRevealed;
      }
    }
    if (changed) {
      this.formRevision++;
    }
  }

  private applyRowUpdates(incoming: readonly SecretRequestView[]): boolean {
    if (incoming.length === 0) return false;
    const next = new Map(this.localRows);
    let changed = false;
    for (const upd of incoming) {
      const base = next.get(upd.rowId) ?? this.rows.find((r) => r.rowId === upd.rowId);
      const chosen = base ? newerSecretRequestView(base, upd) : upd;
      if (chosen !== next.get(upd.rowId)) {
        next.set(upd.rowId, chosen);
        changed = true;
      }
    }
    if (changed) {
      this.localRows = next;
      this.pruneNonPendingFormValues();
      this.actionState.reconcileOpenDismissDialog();
    }
    return changed;
  }

  private handleLiveUpdate(payload: SecretRequestUpdatedEvent): void {
    const view = payload.secret_request;
    if (!view || !view.rowId) return;
    if (this.messageId && view.messageId && view.messageId !== this.messageId) return;
    const prev = this.views.find((r) => r.rowId === view.rowId);
    const becamePending = Boolean(
      prev && !isPendingSecretRequest(prev) && isPendingSecretRequest(view)
    );
    if (this.applyRowUpdates([view])) {
      if (becamePending && !this.canRespond && !this.deniedReason) {
        void this.refetchCard();
      }
      this.dispatchEvent(
        new CustomEvent<SecretRequestUpdatedDetail>('secret-request-updated', {
          detail: {
            messageId: this.messageId,
            rows: this.views,
          },
          bubbles: true,
          composed: true,
        })
      );
    }
  }

  private onValueInput(rowId: string, e: Event): void {
    const target = e.target as HTMLInputElement | HTMLTextAreaElement | null;
    let value = target?.value ?? '';
    if (this.revealedRows.has(rowId)) {
      const style = this.revealedLineEndings.get(rowId) ?? 'lf';
      if (style === 'mixed') {
        return;
      }
      if (style === 'crlf') {
        value = value.replace(/\r\n/g, '\n').replace(/\r/g, '\n').replace(/\n/g, '\r\n');
      }
    }
    this.capturedRows.delete(rowId);
    if (value.length === 0) {
      this.formValues.delete(rowId);
    } else {
      this.formValues.set(rowId, value);
    }
    this.pendingFulfillIdempotencyKey = '';
    if (this.errorMessage) {
      this.errorMessage = '';
    }
    this.formRevision++;
  }

  private isRowCaptured(rowId: string): boolean {
    const value = this.formValues.get(rowId) ?? '';
    return (
      value.length > 0 &&
      (this.capturedRows.has(rowId) || value.includes('\n') || value.includes('\r'))
    );
  }

  private applyMaskedInsert(
    rowId: string,
    target: HTMLInputElement | null,
    incoming: string,
    e: Event
  ): void {
    const hasNewline = incoming.includes('\n') || incoming.includes('\r');
    const isCaptured = this.isRowCaptured(rowId);

    // Non-captured row receiving single-line text: let the browser handle it
    // natively so password bullets, selection, and caret position are preserved.
    if (!isCaptured && !hasNewline) {
      return;
    }

    e.preventDefault();
    if (!incoming) return;

    let nextValue: string;
    if (isCaptured) {
      nextValue = incoming;
    } else {
      const current = target?.value ?? this.formValues.get(rowId) ?? '';
      const selStart = target?.selectionStart ?? current.length;
      const selEnd = target?.selectionEnd ?? selStart;
      nextValue = current.slice(0, selStart) + incoming + current.slice(selEnd);
    }

    this.formValues.set(rowId, nextValue);
    if (nextValue.includes('\n') || nextValue.includes('\r')) {
      this.capturedRows.add(rowId);
      if (target) target.value = '';
    } else {
      this.capturedRows.delete(rowId);
      if (target) {
        target.value = nextValue;
        target.setSelectionRange?.(nextValue.length, nextValue.length);
      }
    }
    this.pendingFulfillIdempotencyKey = '';
    if (this.errorMessage) {
      this.errorMessage = '';
    }
    this.formRevision++;
  }

  private clearCapturedRow(rowId: string, target: HTMLInputElement | null, e: Event): void {
    e.preventDefault();
    this.formValues.delete(rowId);
    this.capturedRows.delete(rowId);
    if (target) {
      target.value = '';
    }
    this.pendingFulfillIdempotencyKey = '';
    if (this.errorMessage) {
      this.errorMessage = '';
    }
    this.formRevision++;
  }

  private onMaskedKeydown(rowId: string, e: KeyboardEvent): void {
    if (isUndoRedoShortcut(e)) {
      e.preventDefault();
      return;
    }
    if ((e.key === 'Backspace' || e.key === 'Delete') && this.isRowCaptured(rowId)) {
      this.clearCapturedRow(rowId, e.target as HTMLInputElement | null, e);
    }
  }

  private extractBeforeInputText(e: InputEvent, includeLineBreaks: boolean): string | null {
    switch (e.inputType) {
      case 'insertFromPaste':
      case 'insertFromDrop':
      case 'insertReplacementText':
      case 'insertText':
        return e.dataTransfer?.getData('text/plain') ?? e.data ?? '';
      case 'insertLineBreak':
      case 'insertParagraph':
        return includeLineBreaks ? '\n' : null;
      default:
        return null;
    }
  }

  private onMaskedBeforeInput(rowId: string, e: InputEvent): void {
    if (e.inputType === 'historyUndo' || e.inputType === 'historyRedo') {
      e.preventDefault();
      return;
    }
    const target = e.target as HTMLInputElement | null;
    if (e.inputType.startsWith('delete') && this.isRowCaptured(rowId)) {
      this.clearCapturedRow(rowId, target, e);
      return;
    }
    const incoming = this.extractBeforeInputText(e, true);
    if (incoming === null) return;
    this.applyMaskedInsert(rowId, target, incoming, e);
  }

  private onMaskedPaste(rowId: string, e: ClipboardEvent): void {
    const pasted = e.clipboardData?.getData('text/plain') ?? '';
    if (!pasted) return;
    this.applyMaskedInsert(rowId, e.target as HTMLInputElement | null, pasted, e);
  }

  private onMaskedDrop(rowId: string, e: DragEvent): void {
    const dropped = e.dataTransfer?.getData('text/plain') ?? '';
    if (!dropped) return;
    this.applyMaskedInsert(rowId, e.target as HTMLInputElement | null, dropped, e);
  }

  private onRevealedKeydown(e: KeyboardEvent): void {
    if (isUndoRedoShortcut(e)) {
      e.preventDefault();
    }
  }

  private applyRevealedInsert(
    rowId: string,
    target: HTMLTextAreaElement | null,
    incoming: string,
    e: Event
  ): void {
    if (this.revealedLineEndings.get(rowId) === 'mixed') {
      e.preventDefault();
      return;
    }
    if (!incoming.includes('\r') && !incoming.includes('\n')) {
      return;
    }

    e.preventDefault();
    const rawCurrent = this.formValues.get(rowId) ?? '';
    const displayCurrent = normalizeToLf(rawCurrent);
    const dispStart = target?.selectionStart ?? displayCurrent.length;
    const dispEnd = target?.selectionEnd ?? dispStart;
    const rawStart = displayOffsetToRawOffset(rawCurrent, dispStart);
    const rawEnd = displayOffsetToRawOffset(rawCurrent, dispEnd);
    const rawBefore = rawCurrent.slice(0, rawStart);
    const rawAfter = rawCurrent.slice(rawEnd);
    const nextValue = rawBefore + incoming + rawAfter;

    this.formValues.set(rowId, nextValue);
    this.capturedRows.delete(rowId);
    this.revealedLineEndings.set(rowId, detectLineEndingStyle(nextValue));

    if (target) {
      const nextDisplay = normalizeToLf(nextValue);
      const nextCaret = normalizeToLf(rawBefore + incoming).length;
      target.value = nextDisplay;
      target.setSelectionRange?.(nextCaret, nextCaret);
    }

    this.pendingFulfillIdempotencyKey = '';
    if (this.errorMessage) {
      this.errorMessage = '';
    }
    this.formRevision++;
  }

  private onRevealedBeforeInput(rowId: string, e: InputEvent): void {
    if (e.inputType === 'historyUndo' || e.inputType === 'historyRedo') {
      e.preventDefault();
      return;
    }
    // Typing Enter (insertLineBreak/insertParagraph) is handled natively so
    // onValueInput preserves the row's current line-ending style (e.g. CRLF).
    const incoming = this.extractBeforeInputText(e, false);
    if (incoming === null) return;
    this.applyRevealedInsert(rowId, e.target as HTMLTextAreaElement | null, incoming, e);
  }

  private onRevealedPaste(rowId: string, e: ClipboardEvent): void {
    const pasted = e.clipboardData?.getData('text/plain') ?? '';
    if (!pasted) return;
    this.applyRevealedInsert(rowId, e.target as HTMLTextAreaElement | null, pasted, e);
  }

  private onRevealedDrop(rowId: string, e: DragEvent): void {
    const dropped = e.dataTransfer?.getData('text/plain') ?? '';
    if (!dropped) return;
    this.applyRevealedInsert(rowId, e.target as HTMLTextAreaElement | null, dropped, e);
  }

  private triggerFilePicker(rowId: string): void {
    const escaped =
      typeof CSS !== 'undefined' && typeof CSS.escape === 'function' ? CSS.escape(rowId) : rowId;
    const input = this.shadowRoot?.querySelector<HTMLInputElement>(
      `input[type="file"][data-row-id="${escaped}"]`
    );
    input?.click();
  }

  private async onFileSelected(row: SecretRequestView, e: Event): Promise<void> {
    const input = e.target as HTMLInputElement | null;
    const file = input?.files?.[0];
    if (!file) return;

    const gen = this.clearGeneration;
    const targetMsgId = this.messageId;

    try {
      if (file.size > MAX_SECRET_REQUEST_VALUE_BYTES) {
        this.errorMessage = `File for ${row.key} exceeds the 64 KiB limit.`;
        return;
      }

      const buf = await file.arrayBuffer();
      if (!this.isConnected || this.messageId !== targetMsgId || this.clearGeneration !== gen) {
        return;
      }
      if (buf.byteLength > MAX_SECRET_REQUEST_VALUE_BYTES) {
        this.errorMessage = `File for ${row.key} exceeds the 64 KiB limit.`;
        return;
      }

      let decoded: string;
      try {
        // Preserve a leading UTF-8 BOM verbatim (secret values are never altered).
        decoded = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(buf);
      } catch {
        this.errorMessage = `File for ${row.key} is not valid UTF-8 text. Binary files are not supported.`;
        return;
      }

      if (decoded.includes('\u0000')) {
        this.errorMessage = `File for ${row.key} appears to be binary (contains NUL bytes). Binary files are not supported.`;
        return;
      }

      if (decoded.length === 0) {
        this.errorMessage = `Selected file for ${row.key} is empty.`;
        return;
      }

      this.formValues.set(row.rowId, decoded);
      this.capturedRows.add(row.rowId);
      if (this.revealedRows.has(row.rowId)) {
        const next = new Set(this.revealedRows);
        next.delete(row.rowId);
        this.revealedLineEndings.delete(row.rowId);
        this.revealedRows = next;
      }
      this.pendingFulfillIdempotencyKey = '';
      this.errorMessage = '';
      this.formRevision++;
    } finally {
      if (input) {
        input.value = '';
      }
    }
  }

  private toggleReveal(rowId: string, currentlyVisible: boolean): void {
    const nextRevealed = new Set(this.revealedRows);
    if (currentlyVisible) {
      nextRevealed.delete(rowId);
      this.revealedLineEndings.delete(rowId);
    } else {
      const current = this.formValues.get(rowId) ?? '';
      this.revealedLineEndings.set(rowId, detectLineEndingStyle(current));
      nextRevealed.add(rowId);
      this.pendingFocusRowId = rowId;
    }
    this.revealedRows = nextRevealed;
  }

  private async refetchCard(): Promise<void> {
    if (!this.messageId) return;
    this.refetchAbort?.abort();
    const controller = new AbortController();
    this.refetchAbort = controller;
    const res = await fetchSecretRequestCard(this.messageId, { signal: controller.signal });
    if (controller.signal.aborted || !res.ok) return;
    this.applyRowUpdates(res.card.rows);
    this.canRespond = res.card.canRespond;
    this.reason = res.card.reason ?? '';
    if (!res.card.canRespond && res.card.reason) {
      this.deniedReason = res.card.reason;
      this.clearFormValues();
    } else if (res.card.canRespond) {
      this.deniedReason = '';
    }
    this.dispatchEvent(
      new CustomEvent<SecretRequestRefreshedDetail>('secret-request-refreshed', {
        detail: {
          messageId: this.messageId,
          card: res.card,
        },
        bubbles: true,
        composed: true,
      })
    );
  }

  private get allPendingValuesFilled(): boolean {
    const pending = this.pendingEditableRows;
    if (pending.length === 0) return false;
    return pending.every((r) => (this.formValues.get(r.rowId) ?? '').length > 0);
  }

  private applyConflictDetails(code: string, details?: SecretRequestConflictDetails): void {
    if (details?.secretRequest) {
      this.applyRowUpdates([details.secretRequest]);
      return;
    }
    if (!details?.rowId) return;
    const existing = this.views.find((r) => r.rowId === details.rowId);
    if (!existing) return;

    const nowIso = new Date().toISOString();
    switch (code) {
      case 'replaced': {
        const replacedBy = details.replacedBy ?? existing.replacedBy;
        this.applyRowUpdates([
          {
            ...existing,
            status: 'replaced',
            submitting: false,
            ...(replacedBy ? { replacedBy } : {}),
          },
        ]);
        break;
      }
      case 'withdrawn':
        this.applyRowUpdates([
          {
            ...existing,
            status: 'withdrawn',
            submitting: false,
            withdrawn: existing.withdrawn ?? {
              at: nowIso,
              by: 'agent',
              byName: existing.agentName,
            },
          },
        ]);
        break;
      case 'dismissed':
        this.applyRowUpdates([
          {
            ...existing,
            status: 'dismissed',
            submitting: false,
          },
        ]);
        break;
      case 'expired':
        this.applyRowUpdates([
          {
            ...existing,
            status: 'expired',
            submitting: false,
            expired: existing.expired ?? { at: nowIso },
          },
        ]);
        break;
      case 'already_fulfilled':
        this.applyRowUpdates([
          {
            ...existing,
            status: 'fulfilled',
            submitting: false,
          },
        ]);
        break;
      case 'submitting':
        this.applyRowUpdates([
          {
            ...existing,
            status: 'pending',
            submitting: true,
          },
        ]);
        break;
      default:
        break;
    }
  }

  private async handleSubmit(e?: Event): Promise<void> {
    e?.preventDefault();
    if (!this.canRespondNow || this.submitting || this.actionState.dismissSubmitting) return;

    const pending = this.pendingEditableRows;
    if (pending.length === 0) return;

    const values: Record<string, string> = {};
    for (const row of pending) {
      const val = this.formValues.get(row.rowId) ?? '';
      if (val.length === 0) {
        this.errorMessage = `Please enter a value for ${row.key}.`;
        return;
      }
      if (utf8ByteLength(val) > MAX_SECRET_REQUEST_VALUE_BYTES) {
        this.errorMessage = `Secret value for ${row.key} exceeds the 64 KiB limit.`;
        return;
      }
      values[row.key] = val;
    }

    if (!this.pendingFulfillIdempotencyKey) {
      this.pendingFulfillIdempotencyKey = generateSecretRequestIdempotencyKey();
    }

    this.submitting = true;
    this.errorMessage = '';

    const result = await fulfillSecretRequest(this.messageId, {
      values,
      surface: this.surface,
      idempotencyKey: this.pendingFulfillIdempotencyKey,
    });

    this.submitting = false;

    if (result.ok) {
      this.clearFormValues();
      this.applyRowUpdates(result.rows);
      this.dispatchEvent(
        new CustomEvent<SecretRequestFulfilledDetail>('secret-request-fulfilled', {
          detail: {
            messageId: this.messageId,
            rows: this.views,
            responseMessageId: result.responseMessageId,
            replayed: result.replayed,
          },
          bubbles: true,
          composed: true,
        })
      );
      return;
    }

    if (result.status === 403) {
      this.deniedReason = result.message;
      this.clearFormValues();
      return;
    }

    if (result.status === 400 && result.details?.field === 'values') {
      this.pendingFulfillIdempotencyKey = '';
      this.errorMessage = result.message;
      void this.refetchCard();
      return;
    }

    if (result.status === 409) {
      this.pendingFulfillIdempotencyKey = '';
      this.applyConflictDetails(result.code, result.details);
      this.errorMessage = mapSecretRequestConflictMessage(
        result.code,
        result.details,
        result.message
      );
      return;
    }

    this.errorMessage = result.message;
  }

  private openDismissDialog(): void {
    if (!this.canRespondNow || this.submitting || this.actionState.dismissSubmitting) return;
    this.errorMessage = '';
    this.actionState.openDismissDialog();
  }

  private async handleConfirmDismiss(): Promise<void> {
    if (!this.canRespondNow || this.actionState.dismissSubmitting) return;
    const pending = this.pendingEditableRows;
    const checked = pending.filter((r) => this.actionState.dismissCheckedIds.has(r.rowId));
    if (checked.length === 0) return;

    const rowIds = checked.map((r) => r.rowId);
    const reason = this.actionState.dismissReasonDraft.trim();
    const attemptSignature = JSON.stringify({ rowIds: [...rowIds].sort(), reason });

    if (
      !this.pendingDismissIdempotencyKey ||
      this.pendingDismissAttemptSignature !== attemptSignature
    ) {
      this.pendingDismissIdempotencyKey = generateSecretRequestIdempotencyKey();
      this.pendingDismissAttemptSignature = attemptSignature;
    }

    this.actionState.setDismissSubmitting(true);
    this.errorMessage = '';

    const result = await dismissSecretRequest(this.messageId, {
      rowIds,
      ...(reason ? { reason } : {}),
      surface: this.surface,
      idempotencyKey: this.pendingDismissIdempotencyKey,
    });

    this.actionState.setDismissSubmitting(false);

    if (result.ok) {
      this.pendingDismissIdempotencyKey = '';
      this.pendingDismissAttemptSignature = '';
      this.actionState.closeDismissDialogState();
      this.applyRowUpdates([...result.dismissed, ...result.alreadyDismissed]);
      this.dispatchEvent(
        new CustomEvent<SecretRequestDismissedDetail>('secret-request-dismissed', {
          detail: {
            messageId: this.messageId,
            rows: this.views,
            responseMessageId: result.responseMessageId,
            replayed: result.replayed,
          },
          bubbles: true,
          composed: true,
        })
      );
      return;
    }

    if (result.status === 403) {
      this.pendingDismissIdempotencyKey = '';
      this.pendingDismissAttemptSignature = '';
      this.actionState.closeDismissDialogState();
      this.deniedReason = result.message;
      this.clearFormValues();
      return;
    }

    if (result.status === 409) {
      this.pendingDismissIdempotencyKey = '';
      this.pendingDismissAttemptSignature = '';
      this.actionState.closeDismissDialogState();
      this.applyConflictDetails(result.code, result.details);
      this.errorMessage = mapSecretRequestConflictMessage(
        result.code,
        result.details,
        result.message
      );
      return;
    }

    this.actionState.closeDismissDialogState();
    this.errorMessage = result.message;
  }

  private actorLabel(
    byUserId: string | undefined,
    byName: string | undefined,
    byEmail: string | undefined
  ): string {
    if (this.currentUserId && byUserId && byUserId === this.currentUserId) {
      return 'you';
    }
    return byName || byEmail || 'a user';
  }

  private withdrawnByLabel(row: SecretRequestView): string {
    if (row.withdrawn?.by === 'hub') {
      return 'the hub';
    }
    return row.withdrawn?.byName || row.agentName || 'the agent';
  }

  private renderRowBody(row: SecretRequestView): TemplateResult | typeof nothing {
    if (isSubmittingSecretRequest(row) || (this.submitting && isPendingSecretRequest(row))) {
      return this.actionState.renderSubmittingBanner('Submitting…');
    }

    if (isPendingSecretRequest(row)) {
      const value = this.formValues.get(row.rowId) ?? '';
      const revealed = this.revealedRows.has(row.rowId);
      const lineEndingStyle = revealed ? (this.revealedLineEndings.get(row.rowId) ?? 'lf') : 'lf';
      const isReadonlyRevealed = revealed && lineEndingStyle === 'mixed';
      const textareaValue = normalizeToLf(value);
      const disabled = !this.canRespondNow || this.submitting || this.actionState.dismissSubmitting;
      const isFile = row.secretType === 'file';
      const textareaRows = isFile ? 4 : 2;
      const hasCapturedValue = !revealed && this.isRowCaptured(row.rowId);
      const placeholder = hasCapturedValue
        ? isFile
          ? 'Value entered — type, paste or load to replace'
          : 'Value entered — type or paste to replace'
        : isFile
          ? 'Paste secret file contents…'
          : `Enter value for ${row.key}…`;

      return html`
        <div class="input-wrap" data-form-rev=${this.formRevision}>
          ${revealed
            ? html`
                <textarea
                  class="secret-textarea"
                  style="--secret-rows: ${textareaRows}"
                  data-key=${row.key}
                  data-row-id=${row.rowId}
                  aria-label=${`Secret value for ${row.key}`}
                  placeholder=${isFile
                    ? 'Paste secret file contents…'
                    : `Enter value for ${row.key}…`}
                  autocomplete="off"
                  autocorrect="off"
                  autocapitalize="off"
                  spellcheck="false"
                  data-lpignore="true"
                  data-1p-ignore="true"
                  rows=${textareaRows}
                  ?disabled=${disabled}
                  ?readonly=${isReadonlyRevealed}
                  .value=${textareaValue}
                  @keydown=${(e: KeyboardEvent): void => this.onRevealedKeydown(e)}
                  @beforeinput=${(e: InputEvent): void => this.onRevealedBeforeInput(row.rowId, e)}
                  @paste=${(e: ClipboardEvent): void => this.onRevealedPaste(row.rowId, e)}
                  @drop=${(e: DragEvent): void => this.onRevealedDrop(row.rowId, e)}
                  @input=${(e: Event): void => this.onValueInput(row.rowId, e)}
                ></textarea>
              `
            : html`
                <input
                  class="secret-input"
                  type="password"
                  data-key=${row.key}
                  data-row-id=${row.rowId}
                  aria-label=${hasCapturedValue
                    ? `Secret value for ${row.key} entered, hidden`
                    : `Secret value for ${row.key}`}
                  placeholder=${placeholder}
                  autocomplete="off"
                  autocorrect="off"
                  autocapitalize="off"
                  spellcheck="false"
                  data-lpignore="true"
                  data-1p-ignore="true"
                  ?disabled=${disabled}
                  .value=${hasCapturedValue ? '' : value}
                  @keydown=${(e: KeyboardEvent): void => this.onMaskedKeydown(row.rowId, e)}
                  @beforeinput=${(e: InputEvent): void => this.onMaskedBeforeInput(row.rowId, e)}
                  @paste=${(e: ClipboardEvent): void => this.onMaskedPaste(row.rowId, e)}
                  @drop=${(e: DragEvent): void => this.onMaskedDrop(row.rowId, e)}
                  @input=${(e: Event): void => this.onValueInput(row.rowId, e)}
                />
              `}
          <button
            type="button"
            class="toggle-visibility"
            data-toggle-visibility=${row.key}
            aria-pressed=${revealed ? 'true' : 'false'}
            aria-label=${revealed ? `Hide value for ${row.key}` : `Show value for ${row.key}`}
            title=${revealed ? 'Hide value' : 'Show value'}
            ?disabled=${disabled}
            @click=${(): void => this.toggleReveal(row.rowId, revealed)}
          >
            <sl-icon name=${revealed ? 'eye-slash' : 'eye'} aria-hidden="true"></sl-icon>
          </button>
        </div>
        <div class="file-controls">
          ${isFile
            ? html`
                <input
                  type="file"
                  tabindex="-1"
                  class="file-picker-input"
                  data-file-input=${row.key}
                  data-row-id=${row.rowId}
                  aria-label=${`Load secret file for ${row.key}`}
                  ?disabled=${disabled}
                  @change=${(e: Event): void => void this.onFileSelected(row, e)}
                />
                <sl-button
                  type="button"
                  size="small"
                  variant="default"
                  class="load-file-btn"
                  data-load-file=${row.key}
                  ?disabled=${disabled}
                  @click=${(): void => this.triggerFilePicker(row.rowId)}
                >
                  <sl-icon slot="prefix" name="upload" aria-hidden="true"></sl-icon>
                  Load from file
                </sl-button>
              `
            : nothing}
          ${hasCapturedValue
            ? html`
                <span
                  class="file-value-status"
                  data-testid="file-value-masked"
                  role="status"
                  aria-label=${`Secret value for ${row.key} entered, hidden`}
                >
                  <sl-icon name="lock" aria-hidden="true"></sl-icon>
                  <span aria-hidden="true">&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;</span>
                  <span>Value entered (hidden)</span>
                </span>
              `
            : nothing}
          ${isReadonlyRevealed
            ? html`
                <span
                  class="mixed-line-endings-note"
                  data-testid="mixed-line-endings-note"
                  role="status"
                >
                  This value has mixed line endings; paste or load to replace it
                </span>
              `
            : nothing}
        </div>
      `;
    }

    if (row.status === 'fulfilled') {
      const who = this.actorLabel(
        row.fulfilled?.byUserId,
        row.fulfilled?.byName,
        row.fulfilled?.byEmail
      );
      const at = row.fulfilled?.at ?? row.resolvedAt ?? undefined;
      return html`<div class="row-foot" role="status">
        <sl-icon class="ok" name="check-circle-fill" aria-hidden="true"></sl-icon>
        <span>Fulfilled by <b>${who}</b></span>
        ${this.actionState.renderAt(at)}
      </div>`;
    }

    if (row.status === 'replaced') {
      return this.actionState.renderReplacedBanner(row.replacedBy);
    }

    if (row.status === 'withdrawn') {
      return this.actionState.renderWithdrawnBanner({
        by: this.withdrawnByLabel(row),
        at: row.withdrawn?.at ?? row.resolvedAt ?? undefined,
        reason: row.withdrawn?.reason,
        replacedBy: row.replacedBy,
      });
    }

    if (row.status === 'dismissed') {
      const who = this.actorLabel(
        row.dismissed?.byUserId,
        row.dismissed?.byName,
        row.dismissed?.byEmail
      );
      return this.actionState.renderDismissedBanner({
        by: who,
        at: row.dismissed?.at ?? row.resolvedAt ?? undefined,
        reason: row.dismissed?.reason,
        replacedBy: row.replacedBy,
      });
    }

    if (row.status === 'expired') {
      return this.actionState.renderExpiredBanner({
        at: row.expired?.at ?? row.expiresAt,
        replacedBy: row.replacedBy,
      });
    }

    return this.actionState.renderClosedBanner();
  }

  private renderRow(row: SecretRequestView): TemplateResult {
    const statusCls = messageActionRowStatusClass(row.status);
    return html`
      <div class="row${statusCls}" data-row-id=${row.rowId} data-status=${row.status}>
        <div class="row-meta">
          <div class="row-top">
            <span class="key-label">${row.key}</span>
            ${row.version > 0 ? html`<span class="version">v${row.version}</span>` : nothing}
            ${row.secretType && row.secretType !== 'environment'
              ? html`<span class="type-badge">${row.secretType}</span>`
              : nothing}
            ${row.target ? html`<span class="target-path">${row.target}</span>` : nothing}
          </div>
          ${row.description ? html`<p class="description">${row.description}</p>` : nothing}
        </div>
        ${this.renderRowBody(row)}
      </div>
    `;
  }

  private renderStorageNotice(): TemplateResult {
    const agent = this.resolvedAgentName;
    return html`
      <div class="storage-notice">
        <sl-icon name="lock" aria-hidden="true"></sl-icon>
        <span>
          Stored as a secret readable only by agent <strong>${agent}</strong>${this.projectName
            ? html` in project <strong>${this.projectName}</strong>`
            : nothing}.
        </span>
      </div>
    `;
  }

  private renderDismissDialog(): TemplateResult {
    const pending = this.pendingEditableRows;
    const checkedCount = pending.filter((r) =>
      this.actionState.dismissCheckedIds.has(r.rowId)
    ).length;
    const runeCount = [...this.actionState.dismissReasonDraft].length;
    const confirmDisabled = checkedCount === 0 || this.actionState.dismissSubmitting;

    return html`
      <sl-dialog
        class="dismiss-dialog"
        ?open=${this.actionState.dismissDialogOpen}
        @sl-request-close=${(e: Event): void => {
          if (this.actionState.dismissSubmitting) {
            e.preventDefault();
          } else {
            this.actionState.closeDismissDialog();
          }
        }}
      >
        <span slot="label" class="dismiss-dialog-title">
          <sl-icon name="x-circle" aria-hidden="true"></sl-icon>
          <span>Dismiss secret request</span>
        </span>
        <div class="dismiss-dialog-body">
          <div class="dismiss-dialog-note">
            ${pending.length === 1
              ? 'This key will be dismissed and the agent will be notified.'
              : 'Select which pending keys to dismiss. The agent will be notified.'}
          </div>
          <div class="dismiss-row-list">
            ${pending.length === 1
              ? html`<div class="dismiss-single-row">
                  <span class="key-label">${pending[0].key}</span>
                  ${pending[0].version > 0
                    ? html`<span class="version">v${pending[0].version}</span>`
                    : nothing}
                </div>`
              : pending.map(
                  (row) => html`
                    <div class="dismiss-pick">
                      <sl-checkbox
                        data-dismiss-row=${row.rowId}
                        ?checked=${this.actionState.dismissCheckedIds.has(row.rowId)}
                        ?disabled=${this.actionState.dismissSubmitting}
                        @sl-change=${(e: Event): void =>
                          this.actionState.onDismissRowToggle(row.rowId, e)}
                      >
                        <span class="key-label">${row.key}</span>
                        ${row.version > 0
                          ? html`<span class="version"> v${row.version}</span>`
                          : nothing}
                      </sl-checkbox>
                    </div>
                  `
                )}
          </div>
          <sl-textarea
            class="dismiss-reason-input"
            label="Reason (optional)"
            placeholder="Let the agent know why this request is being dismissed…"
            rows="3"
            ?disabled=${this.actionState.dismissSubmitting}
            .value=${this.actionState.dismissReasonDraft}
            @sl-input=${this.actionState.onDismissReasonInput}
          >
            <div slot="help-text" class="dismiss-reason-help">
              <span>Sent to the requesting agent.</span>
              <span class="dismiss-reason-count"
                >${runeCount} / ${MAX_ACTION_DISMISS_REASON_RUNES}</span
              >
            </div>
          </sl-textarea>
        </div>
        <div slot="footer" class="dismiss-dialog-footer">
          <sl-button
            size="small"
            ?disabled=${this.actionState.dismissSubmitting}
            @click=${this.actionState.closeDismissDialog}
          >
            Cancel
          </sl-button>
          <sl-button
            class="dismiss-confirm-btn"
            size="small"
            variant="danger"
            ?disabled=${confirmDisabled}
            ?loading=${this.actionState.dismissSubmitting}
            @click=${(): void => void this.handleConfirmDismiss()}
          >
            Dismiss${pending.length > 1 && checkedCount > 0 && checkedCount < pending.length
              ? ` (${checkedCount})`
              : ''}
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  override render(): TemplateResult | typeof nothing {
    if (!agentSecretsEnabled()) {
      return nothing;
    }

    const views = this.views;
    const hasPending = this.pendingEditableRows.length > 0;
    const submitDisabled =
      !this.canRespondNow ||
      !hasPending ||
      !this.allPendingValuesFilled ||
      this.submitting ||
      this.actionState.dismissSubmitting;
    const dismissDisabled =
      !this.canRespondNow || !hasPending || this.submitting || this.actionState.dismissSubmitting;

    return html`
      <form class="card" @submit=${(e: Event): void => void this.handleSubmit(e)}>
        <div class="card-head">
          <sl-icon name="shield-lock" aria-hidden="true"></sl-icon>
          <span class="card-title">Secret Request</span>
        </div>

        ${views.map((row) => this.renderRow(row))} ${this.renderStorageNotice()}
        ${hasPending && !this.canRespondNow
          ? html`<div class="status-alert blocked" role="status">
              <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
              <span>${this.disabledReasonText}</span>
            </div>`
          : nothing}
        ${this.errorMessage
          ? html`<div class="status-alert error" role="alert">
              <sl-icon name="exclamation-circle" aria-hidden="true"></sl-icon>
              <span>${this.errorMessage}</span>
            </div>`
          : nothing}
        ${hasPending
          ? html`
              <div class="card-foot">
                <sl-button
                  class="dismiss-btn"
                  type="button"
                  size="small"
                  variant="default"
                  ?disabled=${dismissDisabled}
                  @click=${(): void => this.openDismissDialog()}
                >
                  Dismiss
                </sl-button>
                <sl-button
                  class="submit-btn"
                  type="button"
                  size="small"
                  variant="primary"
                  ?disabled=${submitDisabled}
                  ?loading=${this.submitting}
                  @click=${(): void => void this.handleSubmit()}
                >
                  Submit
                </sl-button>
              </div>
            `
          : nothing}
      </form>
      ${this.renderDismissDialog()}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-secret-request': ScionChatSecretRequest;
  }
}
