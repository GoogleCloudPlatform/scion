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
 * `<scion-chat-command-request>`: reusable human-approved command-request card
 * component (ptone/scion#9283, design §4.7, §4.11, D7).
 *
 * Rendered inline in native chat (`chat-message.ts`) and on the standalone
 * `/command-requests/:id` page (`pages/command-request.ts`).
 *
 * Security invariants:
 * - Every approval surface (card + confirm dialog) renders EVERY non-ASCII
 *   code point and non-printable ASCII control as a visible `\u{XXXX}` escape
 *   badge (audit M1.2), matching CLI `formatCommandScriptForDisplay`.
 * - Description is always rendered as plain text (never Markdown).
 * - Hidden (`nothing`) when `hub.command_requests` is off (presentation only;
 *   the Hub enforces the experiment server-side).
 */

import { LitElement, css, html, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import {
  FEATURE_FLAGS_CHANGED_EVENT,
  commandRequestsEnabled,
} from '../../../utils/feature-flags.js';
import {
  approveCommandRequest,
  dismissCommandRequest,
  fetchCommandRequestOutput,
  formatCommandDurationMs,
  formatCommandScriptForDisplay,
  formatCommandTimeout,
  generateCommandRequestIdempotencyKey,
  isTerminalCommandRun,
  mapCommandRequestConflictMessage,
  newerCommandRequestView,
  resolveCommandWorkdirKind,
  type CommandRequestDetailResponse,
  type CommandRequestExecContainerView,
  type CommandRequestPreviousUnknown,
  type CommandRequestRunView,
  type CommandRequestSurface,
  type CommandRequestUpdatedEvent,
  type CommandRequestView,
  type FormattedCommandScript,
} from '../../../client/command-requests.js';
import {
  MAX_ACTION_DISMISS_REASON_RUNES,
  MessageActionStateController,
  dismissDialogStyles,
  messageActionBannerStyles,
  messageActionRowStatusClass,
} from './message-action-state.js';

export interface CommandRequestApprovedDetail {
  rowId: string;
  request: CommandRequestView;
  run: CommandRequestRunView;
  replayed: boolean;
}

export interface CommandRequestDismissedDetail {
  rowId: string;
  request: CommandRequestView;
}

export interface CommandRequestUpdatedDetail {
  rowId: string;
  request: CommandRequestView;
}

function runStatusLabel(status: string): string {
  switch (status) {
    case 'queued':
      return 'Queued';
    case 'waking':
      return 'Waking container…';
    case 'dispatching':
      return 'Starting…';
    case 'running':
      return 'Running';
    case 'succeeded':
      return 'Succeeded';
    case 'failed':
      return 'Failed';
    case 'unknown':
      return 'Unknown — may have run';
    case 'not_started':
      return 'Not started';
    default:
      return status;
  }
}

function runStatusIcon(status: string): string {
  switch (status) {
    case 'succeeded':
      return 'check-circle';
    case 'failed':
      return 'x-circle';
    case 'unknown':
      return 'exclamation-triangle';
    case 'not_started':
      return 'slash-circle';
    case 'queued':
    case 'waking':
    case 'dispatching':
    case 'running':
      return 'hourglass-split';
    default:
      return 'info-circle';
  }
}

function runStatusTone(status: string): string {
  switch (status) {
    case 'succeeded':
      return 'success';
    case 'failed':
      return 'danger';
    case 'unknown':
      return 'warning';
    case 'not_started':
      return 'neutral';
    default:
      return 'info';
  }
}

@customElement('scion-chat-command-request')
export class ScionChatCommandRequest extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  private readonly actionState = new MessageActionStateController<CommandRequestUpdatedEvent>(
    this,
    {
      kind: 'command_request',
      dismissMinAgeMs: 0,
      getMessageId: (): string => this.effectiveRequest?.messageId ?? '',
      getThreadId: (): string => this.threadId,
      hasDismissableRows: (): boolean => this.canDismissNow,
      getDismissableRowIds: (): string[] =>
        this.canDismissNow && this.effectiveRequest ? [this.effectiveRequest.rowId] : [],
      onUpdated: (payload): void => this.handleLiveUpdate(payload),
      jumpEventName: 'command-request-jump',
    }
  );

  /** Full detail payload from `GET /api/v1/command-requests/{id}`. */
  @property({ type: Object })
  detail: CommandRequestDetailResponse | null = null;

  /** Signed-in user's id (renders "you" when they are the approver). */
  @property({ type: String })
  currentUserId = '';

  /** Responder surface recorded on approve/dismiss POSTs. */
  @property({ type: String })
  surface: CommandRequestSurface = 'native';

  /** Local updated request view from own POSTs or live SSE updates. */
  @state()
  private localRequest: CommandRequestView | null = null;

  /** Whether the confirm dialog (§4.7) is open. */
  @state()
  private confirmDialogOpen = false;

  /** Approver's current share-output selection (defaults to `request.shareOutputRequested`). */
  @state()
  private shareOutputChecked = false;

  /** Whether the approve POST is in flight. */
  @state()
  private approving = false;

  /** Inline error message from the last approve/dismiss attempt. */
  @state()
  private errorMessage = '';

  /** 403 denial reason learned from a POST refusal. */
  @state()
  private deniedReason = '';

  /** Lazy-loaded output state (feature-detected; active once P4 lands). */
  @state()
  private outputOpen = false;

  @state()
  private outputLoading = false;

  @state()
  private outputText = '';

  @state()
  private outputError = '';

  private pendingApproveIdempotencyKey = '';
  private pendingDismissIdempotencyKey = '';
  private initializedShareOutputRowId = '';
  private outputAbort: AbortController | null = null;

  private readonly onFlagsChanged = (): void => {
    this.requestUpdate();
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
      flex-wrap: wrap;
      gap: 0.45rem;
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

    .key-label {
      font: 600 0.8125rem/1.2 var(--scion-font-mono);
      background: var(--scion-badge-neutral-bg);
      color: var(--scion-badge-neutral-text);
      padding: 0.15rem 0.45rem;
      border-radius: var(--scion-radius-sm);
      overflow-wrap: anywhere;
    }

    .version {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
      white-space: nowrap;
    }

    .asker {
      margin-left: auto;
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }

    .body {
      padding: 0.75rem 0.85rem;
      display: flex;
      flex-direction: column;
      gap: 0.65rem;
    }

    .body.replaced,
    .body.withdrawn,
    .body.dismissed,
    .body.expired {
      opacity: 0.72;
    }

    .banner {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      padding: 0.55rem 0.75rem;
      border-radius: var(--scion-radius);
      font-size: var(--chat-fs-sm, 0.875rem);
      line-height: 1.4;
    }

    .banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.15rem;
      font-size: 1rem;
    }

    .banner.warning {
      background: var(--scion-warning-bg);
      border: 1px solid var(--scion-warning-border);
      color: var(--scion-warning-text);
    }

    .banner.error {
      background: var(--scion-error-bg);
      border: 1px solid var(--scion-border);
      color: var(--scion-error-text);
    }

    .banner.info {
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      color: var(--scion-text-secondary);
    }

    .description {
      margin: 0;
      color: var(--scion-text);
      font-size: var(--chat-fs-base, 0.9375rem);
      white-space: pre-wrap;
      overflow-wrap: anywhere;
    }

    .script-wrap {
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      background: var(--scion-bg);
      overflow: hidden;
    }

    .script-nonascii-note {
      display: flex;
      align-items: center;
      gap: 0.45rem;
      padding: 0.4rem 0.65rem;
      background: var(--scion-warning-bg);
      border-bottom: 1px solid var(--scion-warning-border);
      color: var(--scion-warning-text);
      font-size: 0.8125rem;
    }

    .script-nonascii-note sl-icon {
      flex-shrink: 0;
    }

    .script-scroll {
      overflow-x: auto;
      padding: 0.5rem 0;
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
      line-height: 1.5;
      color: var(--scion-text);
    }

    .script-line {
      display: flex;
      align-items: baseline;
      padding: 0 0.65rem 0 0;
      min-width: max-content;
    }

    .script-lineno {
      user-select: none;
      min-width: 2.5rem;
      padding: 0 0.6rem 0 0.5rem;
      text-align: right;
      color: var(--scion-text-muted);
      font-variant-numeric: tabular-nums;
      flex-shrink: 0;
    }

    .script-code {
      white-space: pre;
    }

    .ws-token {
      display: inline-block;
      padding: 0 0.2rem;
      margin: 0 0.05rem;
      border-radius: var(--scion-radius-sm);
      background: var(--scion-badge-neutral-bg);
      color: var(--scion-text-muted);
      font-size: 0.75rem;
    }

    .escape-token {
      display: inline-block;
      padding: 0 0.25rem;
      margin: 0 0.05rem;
      border-radius: var(--scion-radius-sm);
      background: var(--scion-warning-bg);
      border: 1px solid var(--scion-warning-border);
      color: var(--scion-warning-text);
      font-weight: 600;
      font-size: 0.75rem;
    }

    .script-foot {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      padding: 0.3rem 0.65rem;
      border-top: 1px solid var(--scion-border);
      background: var(--scion-bg-subtle);
      color: var(--scion-text-muted);
      font-size: 0.75rem;
      font-variant-numeric: tabular-nums;
    }

    .meta-grid {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.45rem 0.9rem;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary);
    }

    .meta-item {
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      min-width: 0;
    }

    .meta-item sl-icon {
      flex-shrink: 0;
      font-size: 0.9rem;
      color: var(--scion-text-muted);
    }

    .workdir-badge {
      font: 500 0.8125rem/1.2 var(--scion-font-mono);
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      color: var(--scion-text);
      padding: 0.1rem 0.4rem;
      border-radius: var(--scion-radius-sm);
      overflow-wrap: anywhere;
    }

    .workdir-badge.warn {
      background: var(--scion-warning-bg);
      border-color: var(--scion-warning-border);
      color: var(--scion-warning-text);
    }

    .exec-container-line {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.4rem 0.6rem;
      padding: 0.5rem 0.7rem;
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      background: var(--scion-bg);
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary);
    }

    .exec-container-line sl-icon {
      flex-shrink: 0;
      color: var(--scion-text-muted);
    }

    .exec-container-name {
      font-weight: 600;
      color: var(--scion-text);
    }

    .exec-state-chip {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.1rem 0.45rem;
      border-radius: var(--scion-radius-sm);
      font-size: 0.75rem;
      font-weight: 600;
      background: var(--scion-badge-neutral-bg);
      color: var(--scion-badge-neutral-text);
    }

    .exec-state-chip.ready {
      color: var(--scion-status-success);
    }

    .exec-state-chip.stopped,
    .exec-state-chip.unavailable {
      background: var(--scion-warning-bg);
      color: var(--scion-warning-text);
    }

    .exec-container-note {
      color: var(--scion-text-muted);
      font-size: 0.8125rem;
    }

    .exec-container-link {
      color: var(--scion-primary);
      font-weight: 600;
      text-decoration: none;
    }

    .exec-container-link:hover {
      text-decoration: underline;
    }

    .share-output-row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary);
    }

    .run-section {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
      padding-top: 0.45rem;
      border-top: 1px solid var(--scion-border);
    }

    .run-status-row {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.45rem 0.65rem;
      font-size: var(--chat-fs-sm, 0.875rem);
    }

    .run-chip {
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      padding: 0.2rem 0.55rem;
      border-radius: var(--scion-radius-sm);
      font-weight: 600;
      font-size: 0.8125rem;
      background: var(--scion-badge-neutral-bg);
      color: var(--scion-badge-neutral-text);
    }

    .run-chip.success {
      color: var(--scion-status-success);
    }

    .run-chip.danger {
      background: var(--scion-error-bg);
      color: var(--scion-error-text);
    }

    .run-chip.warning {
      background: var(--scion-warning-bg);
      border: 1px solid var(--scion-warning-border);
      color: var(--scion-warning-text);
    }

    .run-chip.info {
      color: var(--scion-primary);
    }

    .run-metric {
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
      color: var(--scion-text-secondary);
      font-variant-numeric: tabular-nums;
    }

    .run-reason {
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-muted);
    }

    .output-panel {
      margin: 0;
      padding: 0.6rem 0.75rem;
      max-height: 18rem;
      overflow: auto;
      background: var(--scion-bg);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
      line-height: 1.45;
      color: var(--scion-text);
      white-space: pre-wrap;
      overflow-wrap: anywhere;
    }

    .row-foot {
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

    sl-dialog.confirm-dialog {
      --sl-panel-background-color: var(--scion-surface);
      --sl-panel-border-color: var(--scion-border);
      --sl-color-neutral-700: var(--scion-text);
    }

    sl-dialog.confirm-dialog::part(panel) {
      width: min(42rem, calc(100vw - 1.5rem));
    }

    .confirm-dialog-body {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
      font-size: var(--chat-fs-base, 0.9375rem);
      color: var(--scion-text);
    }

    .confirm-sha {
      font-family: var(--scion-font-mono);
      font-size: 0.75rem;
      color: var(--scion-text-muted);
      overflow-wrap: anywhere;
    }

    .confirm-dialog-footer {
      display: flex;
      justify-content: flex-end;
      gap: 0.5rem;
      width: 100%;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
    this.outputAbort?.abort();
    this.outputAbort = null;
  }

  protected override willUpdate(changed: PropertyValues): void {
    if (changed.has('detail')) {
      const nextRowId = this.detail?.request?.rowId ?? '';
      if (nextRowId && nextRowId !== this.initializedShareOutputRowId) {
        this.initializedShareOutputRowId = nextRowId;
        this.shareOutputChecked = Boolean(this.detail?.request?.shareOutputRequested);
        this.localRequest = null;
        this.errorMessage = '';
        this.deniedReason = '';
        this.confirmDialogOpen = false;
        this.approving = false;
        this.pendingApproveIdempotencyKey = '';
        this.pendingDismissIdempotencyKey = '';
        this.outputOpen = false;
        this.outputLoading = false;
        this.outputText = '';
        this.outputError = '';
        this.actionState.closeDismissDialogState();
      }
    }
  }

  private get threadId(): string {
    return this.detail?.threadId ?? '';
  }

  private get effectiveRequest(): CommandRequestView | null {
    const base = this.detail?.request ?? null;
    if (!base) return this.localRequest;
    const baseWithRun = !base.run && this.detail?.run ? { ...base, run: this.detail.run } : base;
    return this.localRequest
      ? newerCommandRequestView(baseWithRun, this.localRequest)
      : baseWithRun;
  }

  private get effectiveRun(): CommandRequestRunView | null {
    return this.effectiveRequest?.run ?? this.detail?.run ?? null;
  }

  private get execContainer(): CommandRequestExecContainerView | null {
    return this.detail?.exec_container ?? null;
  }

  private get previousVersionUnknown(): CommandRequestPreviousUnknown | null {
    return this.detail?.previousVersionUnknown ?? null;
  }

  private get isApprover(): boolean {
    const req = this.effectiveRequest;
    if (!req) return false;
    if (this.currentUserId) return this.currentUserId === req.approverUserId;
    return Boolean(this.detail?.canApprove || this.detail?.exec_container);
  }

  private get canApproveNow(): boolean {
    const req = this.effectiveRequest;
    if (!req || req.status !== 'pending') return false;
    return Boolean(this.detail?.canApprove) && !this.deniedReason;
  }

  private get canDismissNow(): boolean {
    const req = this.effectiveRequest;
    if (!req || req.status !== 'pending') return false;
    return Boolean(this.detail?.canDismiss) && this.canApproveNow;
  }

  private get execContainerAllowsApprove(): boolean {
    const ec = this.execContainer;
    if (!ec) return true;
    return ec.state === 'ready' || ec.state === 'starting' || ec.state === 'suspended';
  }

  private get approveDisabledHint(): string {
    const ec = this.execContainer;
    if (!ec) return '';
    if (ec.state === 'stopped') return 'Start your execution container to approve';
    if (ec.state === 'unconfigured') return 'Set up an execution container in Profile Settings';
    if (ec.state === 'unavailable') return 'Your execution container is unavailable';
    return '';
  }

  private applyRequestUpdate(incoming: CommandRequestView): boolean {
    const current = this.effectiveRequest;
    const chosen = current ? newerCommandRequestView(current, incoming) : incoming;
    if (chosen === current) return false;
    this.localRequest = chosen;
    if (chosen.status !== 'pending') {
      this.confirmDialogOpen = false;
      this.actionState.reconcileOpenDismissDialog();
    }
    return true;
  }

  private handleLiveUpdate(payload: CommandRequestUpdatedEvent): void {
    const view = payload.command_request;
    if (!view || !view.rowId) return;
    const current = this.effectiveRequest;
    if (current && view.rowId !== current.rowId) return;
    if (this.applyRequestUpdate(view)) {
      this.dispatchEvent(
        new CustomEvent<CommandRequestUpdatedDetail>('command-request-updated', {
          detail: {
            rowId: view.rowId,
            request: this.effectiveRequest ?? view,
          },
          bubbles: true,
          composed: true,
        })
      );
    }
  }

  private openConfirmDialog(): void {
    if (!this.canApproveNow || !this.execContainerAllowsApprove || this.approving) return;
    this.errorMessage = '';
    this.confirmDialogOpen = true;
  }

  private closeConfirmDialog(): void {
    if (this.approving) return;
    this.confirmDialogOpen = false;
  }

  private onShareOutputChange(e: Event): void {
    this.shareOutputChecked = Boolean(
      (e.currentTarget as HTMLElement & { checked?: boolean })?.checked
    );
    this.pendingApproveIdempotencyKey = '';
  }

  private async handleConfirmApprove(): Promise<void> {
    const req = this.effectiveRequest;
    if (!req || !this.canApproveNow || !this.execContainerAllowsApprove || this.approving) return;

    if (!this.pendingApproveIdempotencyKey) {
      this.pendingApproveIdempotencyKey = generateCommandRequestIdempotencyKey();
    }

    this.approving = true;
    this.errorMessage = '';

    const result = await approveCommandRequest(req.rowId, {
      row_id: req.rowId,
      command_sha256: req.commandSha256,
      share_output: this.shareOutputChecked,
      surface: this.surface,
      idempotency_key: this.pendingApproveIdempotencyKey,
    });

    this.approving = false;

    if (result.ok) {
      this.pendingApproveIdempotencyKey = '';
      this.confirmDialogOpen = false;
      const merged: CommandRequestView = { ...result.request, run: result.run };
      this.applyRequestUpdate(merged);
      this.dispatchEvent(
        new CustomEvent<CommandRequestApprovedDetail>('command-request-approved', {
          detail: {
            rowId: req.rowId,
            request: merged,
            run: result.run,
            replayed: result.replayed,
          },
          bubbles: true,
          composed: true,
        })
      );
      return;
    }

    if (result.status === 403) {
      this.deniedReason = mapCommandRequestConflictMessage(
        result.code,
        result.details,
        result.message
      );
      this.confirmDialogOpen = false;
      return;
    }

    if (result.status === 409) {
      this.pendingApproveIdempotencyKey = '';
      this.errorMessage = mapCommandRequestConflictMessage(
        result.code,
        result.details,
        result.message
      );
      if (result.details?.status && result.details.status !== 'pending') {
        this.applyRequestUpdate({
          ...req,
          status: result.details.status as CommandRequestView['status'],
        });
      }
      return;
    }

    this.errorMessage = result.message;
  }

  private openDismissDialog(): void {
    if (!this.canDismissNow || this.approving || this.actionState.dismissSubmitting) return;
    this.errorMessage = '';
    this.actionState.openDismissDialog();
  }

  private async handleConfirmDismiss(): Promise<void> {
    const req = this.effectiveRequest;
    if (!req || !this.canDismissNow || this.actionState.dismissSubmitting) return;

    if (!this.pendingDismissIdempotencyKey) {
      this.pendingDismissIdempotencyKey = generateCommandRequestIdempotencyKey();
    }

    this.actionState.setDismissSubmitting(true);
    this.errorMessage = '';

    const reason = this.actionState.dismissReasonDraft.trim();
    const result = await dismissCommandRequest(req.rowId, {
      ...(reason ? { reason } : {}),
      surface: this.surface,
      idempotency_key: this.pendingDismissIdempotencyKey,
    });

    this.actionState.setDismissSubmitting(false);

    if (result.ok) {
      this.pendingDismissIdempotencyKey = '';
      this.actionState.closeDismissDialogState();
      const updated: CommandRequestView = result.request ?? {
        ...req,
        status: 'dismissed',
        ...(reason ? { dismissReason: reason } : {}),
      };
      this.applyRequestUpdate(updated);
      this.dispatchEvent(
        new CustomEvent<CommandRequestDismissedDetail>('command-request-dismissed', {
          detail: { rowId: req.rowId, request: updated },
          bubbles: true,
          composed: true,
        })
      );
      return;
    }

    this.pendingDismissIdempotencyKey = '';
    this.actionState.closeDismissDialogState();
    this.errorMessage = mapCommandRequestConflictMessage(
      result.code,
      result.details,
      result.message
    );
  }

  private async toggleOutput(): Promise<void> {
    if (this.outputOpen) {
      this.outputOpen = false;
      return;
    }
    const req = this.effectiveRequest;
    if (!req) return;
    this.outputOpen = true;
    if (this.outputText || this.outputLoading) return;

    this.outputAbort?.abort();
    const controller = new AbortController();
    this.outputAbort = controller;
    this.outputLoading = true;
    this.outputError = '';

    const res = await fetchCommandRequestOutput(req.rowId, { signal: controller.signal });
    if (controller.signal.aborted) return;
    this.outputLoading = false;
    if (!res.ok) {
      this.outputError = res.message;
      return;
    }
    this.outputText = res.output;
  }

  private renderScriptBlock(formatted: FormattedCommandScript): TemplateResult {
    return html`
      <div class="script-wrap" data-testid="command-script-block">
        ${formatted.nonAsciiCount > 0
          ? html`
              <div class="script-nonascii-note" role="status" data-testid="non-ascii-warning">
                <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
                <span>
                  Script contains ${formatted.nonAsciiCount} non-ASCII
                  ${formatted.nonAsciiCount === 1 ? 'character' : 'characters'}, shown as
                  <code>\\u{XXXX}</code>.
                </span>
              </div>
            `
          : nothing}
        <div class="script-scroll">
          ${formatted.lines.map(
            (line) => html`
              <div class="script-line">
                <span class="script-lineno" aria-hidden="true">${line.lineNumber}</span>
                <span class="script-code"
                  >${line.tokens.map((tok) => {
                    if (tok.kind === 'ascii') return tok.text;
                    if (tok.kind === 'tab') {
                      return html`<span class="ws-token tab" title="Tab (U+0009)"
                        >${tok.text}</span
                      >`;
                    }
                    if (tok.kind === 'trailing-space') {
                      return html`<span
                        class="ws-token trailing-space"
                        title="Trailing space (U+0020)"
                        >${tok.text}</span
                      >`;
                    }
                    return html`<span
                      class="escape-token"
                      data-testid="escape-token"
                      title=${`U+${(tok.codePoint ?? 0).toString(16).toUpperCase().padStart(4, '0')}`}
                      >${tok.text}</span
                    >`;
                  })}</span
                >
              </div>
            `
          )}
        </div>
        <div class="script-foot">
          <span
            >${formatted.lineCount} ${formatted.lineCount === 1 ? 'line' : 'lines'} ·
            ${formatted.byteLength} ${formatted.byteLength === 1 ? 'byte' : 'bytes'}</span
          >
          <span>/bin/bash --login</span>
        </div>
      </div>
    `;
  }

  private renderWorkdirWarning(
    request: CommandRequestView,
    formatted: FormattedCommandScript
  ): TemplateResult | typeof nothing {
    const kind = resolveCommandWorkdirKind(request);
    if (kind === 'workspace') {
      return html`
        <div class="banner warning" role="status" data-testid="workdir-warning">
          <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
          <span>Working directory is under <code>/workspace</code> (agent-writable).</span>
        </div>
      `;
    }
    if (kind === 'shared_dir') {
      return html`
        <div class="banner warning" role="status" data-testid="workdir-warning">
          <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
          <span>Working directory is a shared directory (agent-writable).</span>
        </div>
      `;
    }
    if (formatted.mentionsAgentWritablePaths) {
      return html`
        <div class="banner warning" role="status" data-testid="script-path-warning">
          <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
          <span
            >Script references <code>/workspace</code> or a shared directory (agent-writable).</span
          >
        </div>
      `;
    }
    return nothing;
  }

  private renderExecContainerLine(ec: CommandRequestExecContainerView): TemplateResult {
    if (ec.state === 'unconfigured') {
      return html`
        <div class="exec-container-line" data-testid="exec-container-line">
          <sl-icon name="terminal" aria-hidden="true"></sl-icon>
          <span>No execution container configured.</span>
          <a class="exec-container-link" href="/profile/settings" data-testid="exec-setup-link">
            Set up an execution container
          </a>
        </div>
      `;
    }

    const name = ec.agentName || ec.agentId || 'Execution container';
    const projectSuffix = ec.projectName ? ` (${ec.projectName})` : '';
    const stateClass =
      ec.state === 'ready'
        ? 'ready'
        : ec.state === 'stopped'
          ? 'stopped'
          : ec.state === 'unavailable'
            ? 'unavailable'
            : '';

    return html`
      <div class="exec-container-line" data-testid="exec-container-line">
        <sl-icon name="terminal" aria-hidden="true"></sl-icon>
        <span class="exec-container-name">${name}${projectSuffix}</span>
        <span class="exec-state-chip ${stateClass}" data-testid="exec-container-state"
          >${ec.state}</span
        >
        ${ec.state === 'suspended'
          ? html`<span class="exec-container-note">Will be resumed when you approve.</span>`
          : nothing}
        ${ec.state === 'stopped'
          ? html`<span class="exec-container-note" data-testid="exec-stopped-note">
              Start your execution container to approve.
            </span>`
          : nothing}
        ${ec.state === 'unavailable'
          ? html`<span class="exec-container-note">
              Execution container is currently unavailable.
            </span>`
          : nothing}
        ${ec.homePersistent === false
          ? html`<span class="exec-container-note" data-testid="exec-home-nonpersistent">
              Home directory is not persistent.
            </span>`
          : nothing}
      </div>
    `;
  }

  private renderRunStatus(req: CommandRequestView, run: CommandRequestRunView): TemplateResult {
    const tone = runStatusTone(run.status);
    const showViewOutput =
      Boolean(this.detail?.outputAvailable) && this.isApprover && isTerminalCommandRun(run.status);

    return html`
      <div class="run-section" data-testid="command-run-status">
        ${req.approved
          ? html`
              <div class="row-foot">
                <sl-icon class="ok" name="check-circle" aria-hidden="true"></sl-icon>
                <span>
                  Approved by
                  <b>
                    ${this.currentUserId && req.approved.byUserId === this.currentUserId
                      ? 'you'
                      : req.approved.byName || req.approved.byEmail || 'approver'}
                  </b>
                </span>
                ${this.actionState.renderAt(req.approved.at)}
              </div>
            `
          : nothing}

        <div class="run-status-row">
          <span class="run-chip ${tone}" role="status" data-testid="run-status-chip">
            <sl-icon name=${runStatusIcon(run.status)} aria-hidden="true"></sl-icon>
            <span>${runStatusLabel(run.status)}</span>
          </span>

          ${run.exitCode !== undefined && run.exitCode !== null
            ? html`<span class="run-metric" data-testid="run-exit-code">
                exit ${run.exitCode}
              </span>`
            : nothing}
          ${run.durationMs !== undefined && run.durationMs !== null
            ? html`<span class="run-metric" data-testid="run-duration">
                ${formatCommandDurationMs(run.durationMs)}
              </span>`
            : nothing}
          ${showViewOutput
            ? html`
                <sl-button
                  size="small"
                  variant="default"
                  data-testid="view-output-btn"
                  @click=${(): void => void this.toggleOutput()}
                >
                  <sl-icon slot="prefix" name="eye"></sl-icon>
                  ${this.outputOpen ? 'Hide output' : 'View output'}
                </sl-button>
              `
            : nothing}
        </div>

        ${run.status === 'not_started' && run.reason === 'exec_unsupported'
          ? html`
              <div class="banner warning" role="status">
                <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
                <span>Restart your execution container to enable approved commands.</span>
              </div>
            `
          : run.reason
            ? html`<div class="run-reason">${run.reason}</div>`
            : nothing}
        ${this.outputOpen
          ? this.outputLoading
            ? html`<div class="row-foot" role="status">
                <sl-spinner></sl-spinner>
                <span>Loading output…</span>
              </div>`
            : this.outputError
              ? html`<div class="banner error" role="alert">
                  <sl-icon name="exclamation-circle" aria-hidden="true"></sl-icon>
                  <span>${this.outputError}</span>
                </div>`
              : html`<pre class="output-panel" data-testid="command-output-panel">
${this.outputText || '(no output)'}</pre
                >`
          : nothing}
      </div>
    `;
  }

  private renderTerminalBanner(req: CommandRequestView): TemplateResult | typeof nothing {
    switch (req.status) {
      case 'replaced':
        return this.actionState.renderReplacedBanner(
          req.replacedBy
            ? {
                messageId: req.replacedBy.messageId || req.replacedBy.rowId,
                version: req.replacedBy.version,
                at: req.replacedBy.at,
              }
            : undefined
        );
      case 'withdrawn':
        return this.actionState.renderWithdrawnBanner({
          by: req.withdrawnBy || req.agentName || 'agent',
          at: req.resolvedAt ?? undefined,
          reason: req.withdrawnReason,
        });
      case 'dismissed':
        return this.actionState.renderDismissedBanner({
          by: 'approver',
          at: req.resolvedAt ?? undefined,
          reason: req.dismissReason,
        });
      case 'expired':
        return this.actionState.renderExpiredBanner({
          at: req.resolvedAt ?? req.expiresAt,
        });
      default:
        return nothing;
    }
  }

  private renderConfirmDialog(
    req: CommandRequestView,
    formatted: FormattedCommandScript
  ): TemplateResult {
    const ec = this.execContainer;
    const workdirLabel = req.workdir || '$HOME';
    const workdirKind = resolveCommandWorkdirKind(req);

    return html`
      <sl-dialog
        class="confirm-dialog"
        label="Approve & run command"
        ?open=${this.confirmDialogOpen}
        @sl-request-close=${(e: CustomEvent): void => {
          if (e.target !== e.currentTarget) return;
          if (this.approving) {
            e.preventDefault();
            return;
          }
          this.closeConfirmDialog();
        }}
        @sl-after-hide=${(e: Event): void => {
          if (e.target !== e.currentTarget) return;
          this.confirmDialogOpen = false;
        }}
      >
        <div class="confirm-dialog-body" data-testid="confirm-dialog-body">
          ${this.renderScriptBlock(formatted)}

          <div class="meta-grid">
            <span class="meta-item">
              <sl-icon name="folder" aria-hidden="true"></sl-icon>
              <span>Workdir:</span>
              <span
                class="workdir-badge${workdirKind === 'workspace' || workdirKind === 'shared_dir'
                  ? ' warn'
                  : ''}"
                >${workdirLabel}</span
              >
            </span>
            <span class="meta-item">
              <sl-icon name="clock" aria-hidden="true"></sl-icon>
              <span>Timeout: ${formatCommandTimeout(req.timeoutS)}</span>
            </span>
          </div>

          ${this.renderWorkdirWarning(req, formatted)}
          ${ec ? this.renderExecContainerLine(ec) : nothing}

          <div class="share-output-row">
            <sl-checkbox
              ?checked=${this.shareOutputChecked}
              ?disabled=${this.approving}
              @sl-change=${(e: Event): void => this.onShareOutputChange(e)}
            >
              Share redacted output tail with agent (up to 4 KiB)
            </sl-checkbox>
          </div>

          <div class="confirm-sha">SHA-256: ${req.commandSha256}</div>

          ${this.errorMessage
            ? html`
                <div class="banner error" role="alert">
                  <sl-icon name="exclamation-circle" aria-hidden="true"></sl-icon>
                  <span>${this.errorMessage}</span>
                </div>
              `
            : nothing}
        </div>

        <div slot="footer" class="confirm-dialog-footer">
          <sl-button
            variant="default"
            ?disabled=${this.approving}
            @click=${(): void => this.closeConfirmDialog()}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="primary"
            class="confirm-approve-btn"
            data-testid="confirm-approve-btn"
            ?loading=${this.approving}
            ?disabled=${this.approving || !this.execContainerAllowsApprove}
            @click=${(): void => void this.handleConfirmApprove()}
          >
            Approve &amp; run
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private renderDismissDialog(req: CommandRequestView): TemplateResult | typeof nothing {
    if (!this.detail?.canDismiss) return nothing;
    const reasonLen = [...this.actionState.dismissReasonDraft].length;
    return html`
      <sl-dialog
        class="dismiss-dialog"
        label="Dismiss command request"
        ?open=${this.actionState.dismissDialogOpen}
        @sl-request-close=${(e: CustomEvent): void => {
          if (e.target !== e.currentTarget) return;
          if (this.actionState.dismissSubmitting) {
            e.preventDefault();
            return;
          }
          this.actionState.closeDismissDialog();
        }}
        @sl-after-hide=${(e: Event): void => {
          if (e.target !== e.currentTarget) return;
          this.actionState.closeDismissDialog();
        }}
      >
        <div class="dismiss-dialog-body">
          <div class="dismiss-dialog-note">
            Dismiss <code>${req.key}</code> (v${req.version}) without running it.
          </div>
          <sl-textarea
            label="Reason (optional)"
            rows="3"
            .value=${this.actionState.dismissReasonDraft}
            ?disabled=${this.actionState.dismissSubmitting}
            @sl-input=${this.actionState.onDismissReasonInput}
          ></sl-textarea>
          <div class="dismiss-reason-help">
            <span class="dismiss-dialog-note">Sent back to ${req.agentName}</span>
            <span class="dismiss-reason-count">
              ${reasonLen} / ${MAX_ACTION_DISMISS_REASON_RUNES}
            </span>
          </div>
        </div>
        <div slot="footer" class="dismiss-dialog-footer">
          <sl-button
            variant="default"
            ?disabled=${this.actionState.dismissSubmitting}
            @click=${this.actionState.closeDismissDialog}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="danger"
            data-testid="confirm-dismiss-btn"
            ?loading=${this.actionState.dismissSubmitting}
            ?disabled=${this.actionState.dismissSubmitting}
            @click=${(): void => void this.handleConfirmDismiss()}
          >
            Dismiss
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  override render(): TemplateResult | typeof nothing {
    if (!commandRequestsEnabled()) return nothing;
    const req = this.effectiveRequest;
    if (!req) return nothing;

    const formatted = formatCommandScriptForDisplay(req.script);
    const workdirLabel = req.workdir || '$HOME';
    const workdirKind = resolveCommandWorkdirKind(req);
    const approverLabel =
      this.currentUserId && this.currentUserId === req.approverUserId
        ? 'you'
        : this.detail?.approverName || this.detail?.approverEmail || req.approverUserId;
    const run = this.effectiveRun;
    const ec = this.execContainer;
    const prevUnknown = this.previousVersionUnknown;
    const statusClass = messageActionRowStatusClass(req.status);

    return html`
      <div class="card" data-testid="command-request-card">
        <div class="card-head">
          <sl-icon name="terminal" aria-hidden="true"></sl-icon>
          <span class="card-title">Command request</span>
          <span class="key-label">${req.key}</span>
          <span class="version">v${req.version}</span>
          ${req.agentName ? html`<span class="asker">from ${req.agentName}</span>` : nothing}
        </div>

        <div class="body${statusClass}">
          ${prevUnknown
            ? html`
                <div class="banner warning" role="status" data-testid="d7-previous-unknown-banner">
                  <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
                  <span>
                    Previous version v${prevUnknown.version} result <strong>unknown</strong>: it may
                    have run.
                  </span>
                </div>
              `
            : nothing}
          ${req.description
            ? html`<p class="description" data-testid="command-description">${req.description}</p>`
            : nothing}
          ${this.renderScriptBlock(formatted)}

          <div class="meta-grid">
            <span class="meta-item">
              <sl-icon name="folder" aria-hidden="true"></sl-icon>
              <span>Workdir:</span>
              <span
                class="workdir-badge${workdirKind === 'workspace' || workdirKind === 'shared_dir'
                  ? ' warn'
                  : ''}"
                data-testid="command-workdir"
                >${workdirLabel}</span
              >
            </span>
            <span class="meta-item" data-testid="command-timeout">
              <sl-icon name="clock" aria-hidden="true"></sl-icon>
              <span>Timeout: ${formatCommandTimeout(req.timeoutS)}</span>
            </span>
            <span class="meta-item" data-testid="command-approver">
              <sl-icon name="person" aria-hidden="true"></sl-icon>
              <span>Approver: ${approverLabel}</span>
            </span>
          </div>

          ${this.renderWorkdirWarning(req, formatted)}
          ${ec ? this.renderExecContainerLine(ec) : nothing}
          ${req.status === 'pending' && this.canApproveNow
            ? html`
                <div class="share-output-row">
                  <sl-checkbox
                    data-testid="share-output-checkbox"
                    ?checked=${this.shareOutputChecked}
                    ?disabled=${this.approving}
                    @sl-change=${(e: Event): void => this.onShareOutputChange(e)}
                  >
                    Share redacted output tail with agent (up to 4 KiB)
                  </sl-checkbox>
                </div>
              `
            : nothing}
          ${req.status === 'pending' && !this.canApproveNow
            ? html`
                <div class="banner info" role="status" data-testid="cannot-approve-notice">
                  <sl-icon name="info-circle" aria-hidden="true"></sl-icon>
                  <span>
                    ${this.deniedReason ||
                    (this.detail?.reason === 'not_approver'
                      ? 'Only the named approver may approve this command request.'
                      : this.detail?.reason || 'You cannot approve this command request.')}
                  </span>
                </div>
              `
            : nothing}
          ${this.errorMessage && !this.confirmDialogOpen
            ? html`
                <div class="banner error" role="alert" data-testid="command-error-banner">
                  <sl-icon name="exclamation-circle" aria-hidden="true"></sl-icon>
                  <span>${this.errorMessage}</span>
                </div>
              `
            : nothing}
          ${run ? this.renderRunStatus(req, run) : nothing}
          ${req.status !== 'pending' && req.status !== 'approved'
            ? this.renderTerminalBanner(req)
            : nothing}
        </div>

        ${req.status === 'pending' && this.canApproveNow
          ? html`
              <div class="card-foot">
                ${this.canDismissNow
                  ? html`
                      <sl-button
                        size="small"
                        variant="default"
                        data-testid="dismiss-btn"
                        ?disabled=${this.approving || this.actionState.dismissSubmitting}
                        @click=${(): void => this.openDismissDialog()}
                      >
                        Dismiss
                      </sl-button>
                    `
                  : nothing}
                <sl-button
                  size="small"
                  variant="primary"
                  class="approve-btn"
                  data-testid="approve-btn"
                  title=${this.approveDisabledHint}
                  ?disabled=${!this.execContainerAllowsApprove ||
                  this.approving ||
                  this.actionState.dismissSubmitting}
                  @click=${(): void => this.openConfirmDialog()}
                >
                  <sl-icon slot="prefix" name="play-circle"></sl-icon>
                  Approve &amp; run…
                </sl-button>
              </div>
            `
          : nothing}
      </div>

      ${this.renderConfirmDialog(req, formatted)} ${this.renderDismissDialog(req)}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-command-request': ScionChatCommandRequest;
  }
}
