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
 * Shared reactive controller for message-action cards (ptone/scion#8839).
 *
 * Kind-agnostic lifecycle state and UI primitives shared across message-action
 * cards (decisions today; secret requests and future action-execution cards):
 * - terminal-state banners: replaced by vN with a jump link, withdrawn with
 *   reason, dismissed by whom with reason, expired, submitting, and closed;
 * - live subscription to `chat.<kind>.updated` SSE events;
 * - dismiss-with-reason affordance: dialog open/close, row selection and live
 *   reconciliation, 500-rune reason clamping, optional minimum-age countdown
 *   timer, and optional gone-asker probe cancellation.
 */

import { css, html, nothing } from 'lit';
import type { ReactiveController, ReactiveControllerHost, TemplateResult } from 'lit';
import { stateManager } from '../../../client/state.js';
import { formatInstant, formatInstantWithZone } from '../../../utils/time.js';

/** Maximum Unicode code points allowed in a dismissal reason (mirrors `pkg/msgaction`). */
export const MAX_ACTION_DISMISS_REASON_RUNES = 500;

/** Clamps a dismissal reason to `MAX_ACTION_DISMISS_REASON_RUNES` Unicode runes. */
export function clampActionDismissReason(raw: string): string {
  const runes = [...raw];
  return runes.length > MAX_ACTION_DISMISS_REASON_RUNES
    ? runes.slice(0, MAX_ACTION_DISMISS_REASON_RUNES).join('')
    : raw;
}

/** Shared styles for the dismiss confirmation dialog. */
export const dismissDialogStyles = css`
  sl-dialog.dismiss-dialog {
    --sl-panel-background-color: var(--scion-surface, #fff);
    --sl-panel-border-color: var(--scion-border, #e2e8f0);
    --sl-color-neutral-700: var(--scion-text, #1e293b);
  }

  sl-dialog.dismiss-dialog::part(panel) {
    width: min(32.5rem, calc(100vw - 1.5rem));
  }

  .dismiss-dialog-body {
    display: flex;
    flex-direction: column;
    gap: 0.65rem;
    font-size: var(--chat-fs-base, 0.9375rem);
    color: var(--scion-text, #1e293b);
  }

  .dismiss-dialog-note {
    font-size: var(--chat-fs-sm, 0.875rem);
    color: var(--scion-text-muted, #64748b);
  }

  .dismiss-reason-help {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 0.5rem;
  }

  .dismiss-reason-count {
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
    color: var(--scion-text-muted, #64748b);
  }

  .dismiss-tell {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    font-size: var(--chat-fs-sm, 0.875rem);
    color: var(--scion-text-secondary, #475569);
  }

  .dismiss-tell sl-icon {
    flex-shrink: 0;
  }

  .dismiss-dialog-footer {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
    width: 100%;
  }
`;

/** Shared styles for terminal-state banners (`.wd`, `.wd-reason`, jump links). */
export const messageActionBannerStyles = css`
  .wd {
    margin-top: 0.45rem;
    font-size: var(--chat-fs-sm, 0.875rem);
    color: var(--scion-text-muted, #64748b);
    display: flex;
    gap: 0.35rem;
    align-items: center;
    flex-wrap: wrap;
  }

  .wd.card-wd {
    margin-top: 0;
  }

  .wd-lead {
    display: inline-flex;
    align-items: center;
    gap: 0.35rem;
  }

  .wd sl-icon {
    font-size: 0.95rem;
  }

  .wd sl-icon.dc {
    color: var(--scion-status-danger, #dc2626);
  }

  .wd b {
    color: var(--scion-text-secondary, #475569);
  }

  .wd b.state {
    color: var(--scion-text-secondary, #475569);
    font-weight: 600;
  }

  .wd a.jump {
    color: var(--scion-primary, #3b82f6);
    text-decoration: none;
    font-weight: 500;
  }

  .wd a.jump:hover {
    text-decoration: underline;
  }

  .wd a.jump:focus-visible {
    outline: 2px solid var(--scion-primary, #3b82f6);
    outline-offset: 1px;
    border-radius: calc(var(--scion-radius-sm) * 0.8);
  }

  .wd-reason {
    margin-top: 0.35rem;
    font-size: var(--chat-fs-sm, 0.875rem);
    color: var(--scion-text-secondary, #475569);
    border-left: 3px solid var(--scion-border, #e2e8f0);
    padding: 0.1rem 0.55rem;
    overflow-wrap: anywhere;
  }
`;

/** Replacement pointer rendered on replaced, withdrawn, dismissed, or expired banners. */
export interface MessageActionReplacedBy {
  messageId: string;
  version?: number | undefined;
  at?: string | undefined;
}

/** Data rendered in a withdrawn banner. */
export interface MessageActionWithdrawnBanner {
  by: string;
  at?: string | undefined;
  reason?: string | undefined;
  replacedBy?: MessageActionReplacedBy | undefined;
}

/** Data rendered in a dismissed banner. */
export interface MessageActionDismissedBanner {
  by: string;
  at?: string | undefined;
  reason?: string | undefined;
  replacedBy?: MessageActionReplacedBy | undefined;
}

/** Data rendered in an expired banner. */
export interface MessageActionExpiredBanner {
  at?: string | undefined;
  replacedBy?: MessageActionReplacedBy | undefined;
}

/** Common shape of a `chat.<kind>.updated` payload routed via `StateManager`. */
export interface MessageActionUpdatedPayload {
  kind?: string | undefined;
  messageId?: string | undefined;
  threadId?: string | undefined;
  projectId?: string | undefined;
  [key: string]: unknown;
}

/** Options for `subscribeMessageActionUpdated`. */
export interface SubscribeMessageActionOptions {
  /** EventTarget to subscribe on (defaults to `stateManager`). */
  eventSource?: EventTarget | undefined;
  /** Optional messageId filter (string or getter). */
  messageId?: string | (() => string) | undefined;
  /** Optional threadId filter (string or getter). */
  threadId?: string | (() => string) | undefined;
}

/**
 * Subscribes to live `chat.<kind>.updated` events from `stateManager` (or an
 * injected `eventSource`).
 *
 * `kind === 'decision'` listens on the existing `'decision-updated'` state
 * event; all other kinds listen on `'message-action-updated'` and filter by
 * `payload.kind === kind`. Returns an unsubscribe callback.
 */
export function subscribeMessageActionUpdated<TPayload = MessageActionUpdatedPayload>(
  kind: string,
  onUpdate: (payload: TPayload) => void,
  opts: SubscribeMessageActionOptions = {}
): () => void {
  const source = opts.eventSource ?? stateManager;
  const eventName = kind === 'decision' ? 'decision-updated' : 'message-action-updated';
  const listener = (e: Event): void => {
    const raw = (e as CustomEvent).detail as
      | ({ data?: MessageActionUpdatedPayload } & MessageActionUpdatedPayload)
      | undefined;
    // stateManager wraps SSE payloads as { state, data }; tolerate flat details too.
    const data = raw?.data ?? raw;
    if (!data || typeof data !== 'object') return;
    if (kind !== 'decision' && data.kind !== kind) return;

    const expectedMsgId =
      typeof opts.messageId === 'function' ? opts.messageId() : (opts.messageId ?? '');
    if (expectedMsgId && data.messageId !== expectedMsgId) return;

    const expectedThreadId =
      typeof opts.threadId === 'function' ? opts.threadId() : (opts.threadId ?? '');
    if (expectedThreadId && data.threadId && data.threadId !== expectedThreadId) return;

    onUpdate(data as unknown as TPayload);
  };
  source.addEventListener(eventName, listener);
  return () => {
    source.removeEventListener(eventName, listener);
  };
}

/** CSS status modifier class for a message-action row. */
export function messageActionRowStatusClass(status: string, closedUnanswered = false): string {
  if (status === 'replaced') return ' replaced';
  if (status === 'withdrawn') return ' withdrawn';
  if (status === 'dismissed') return ' dismissed';
  if (status === 'expired') return ' expired';
  if (closedUnanswered) return ' closed';
  return '';
}

/** Minimal row shape for checking whether every row in a set was withdrawn together. */
export interface WithdrawnRowLike {
  status: string;
  withdrawn?: { at?: string; reason?: string };
  replacedBy?: { messageId?: string; version?: number };
}

/** Whether every row in `rows` shares the same withdrawal event. */
export function isWholeSetWithdrawn<T extends WithdrawnRowLike>(
  rows: readonly T[],
  getBy: (row: T) => string
): boolean {
  const first = rows[0];
  if (!first || !rows.every((r) => r.status === 'withdrawn')) return false;
  const at = first.withdrawn?.at ?? '';
  const reason = first.withdrawn?.reason ?? '';
  const by = getBy(first);
  const jumpMsg = first.replacedBy?.messageId ?? '';
  const jumpVer = first.replacedBy?.version ?? 0;
  return rows.every(
    (r) =>
      (r.withdrawn?.at ?? '') === at &&
      (r.withdrawn?.reason ?? '') === reason &&
      getBy(r) === by &&
      (r.replacedBy?.messageId ?? '') === jumpMsg &&
      (r.replacedBy?.version ?? 0) === jumpVer
  );
}

/** Minimal row shape for checking whether every row in a set was dismissed together. */
export interface DismissedRowLike {
  status: string;
  dismissed?: { responseMessageId?: string; at?: string; reason?: string };
  replacedBy?: { messageId?: string; version?: number };
}

/** Whether every row in `rows` shares the same dismissal event. */
export function isWholeSetDismissed<T extends DismissedRowLike>(
  rows: readonly T[],
  getBy: (row: T) => string
): boolean {
  const first = rows[0];
  if (!first || !rows.every((r) => r.status === 'dismissed' && Boolean(r.dismissed))) {
    return false;
  }
  const respId = first.dismissed?.responseMessageId ?? '';
  const at = first.dismissed?.at ?? '';
  const reason = first.dismissed?.reason ?? '';
  const by = getBy(first);
  const jumpMsg = first.replacedBy?.messageId ?? '';
  const jumpVer = first.replacedBy?.version ?? 0;
  return rows.every(
    (r) =>
      (r.dismissed?.responseMessageId ?? '') === respId &&
      (r.dismissed?.at ?? '') === at &&
      (r.dismissed?.reason ?? '') === reason &&
      getBy(r) === by &&
      (r.replacedBy?.messageId ?? '') === jumpMsg &&
      (r.replacedBy?.version ?? 0) === jumpVer
  );
}

/** Host interface required by `MessageActionStateController` (satisfied by `LitElement`). */
export type MessageActionHost = ReactiveControllerHost & EventTarget;

/** Configuration for `MessageActionStateController`. */
export interface MessageActionStateOptions<TPayload = MessageActionUpdatedPayload> {
  /** Action kind (`'decision'`, `'secret_request'`, etc.) for `chat.<kind>.updated`. */
  kind: string;
  /** Optional live-update handler; when provided, subscribes while the host is connected. */
  onUpdated?: (payload: TPayload) => void;
  /** Getter for the card's messageId (filters live updates when non-empty). */
  getMessageId?: () => string;
  /** Getter for the card's threadId (filters live updates when non-empty). */
  getThreadId?: () => string;
  /** EventTarget for live updates (defaults to `stateManager`). */
  eventSource?: EventTarget;
  /** Minimum card age in ms before dismiss is allowed (0 = immediate; decisions = 5 min). */
  dismissMinAgeMs?: number;
  /** Getter for the card's creation ISO timestamp (used when `dismissMinAgeMs > 0`). */
  getCreatedAt?: () => string;
  /**
   * Optional override for computing the base dismiss-unlock epoch ms directly
   * (e.g. from server-provided per-row `dismissAvailableAt` timestamps).
   */
  getDismissAvailableAtMs?: () => number;
  /** Injectable clock in epoch ms (defaults to `() => Date.now()`). */
  clockNow?: () => number;
  /** Returns whether at least one row is currently dismissable by the viewer. */
  hasDismissableRows?: () => boolean;
  /** Returns the currently dismissable row ids (for dialog open & reconciliation). */
  getDismissableRowIds?: () => string[];
  /** CustomEvent name emitted by jump links (default `'message-action-jump'`). */
  jumpEventName?: string;
}

/**
 * Reactive controller owning a message-action card's shared lifecycle state:
 * live `chat.<kind>.updated` subscription, dismiss-with-reason dialog state &
 * minimum-age timer, and terminal-state banner renderers.
 */
export class MessageActionStateController<
  TPayload = MessageActionUpdatedPayload,
> implements ReactiveController {
  private readonly host: MessageActionHost;
  private readonly opts: MessageActionStateOptions<TPayload>;

  private connected = false;
  private unsubscribeLive: (() => void) | null = null;
  private dismissUnlockTimer: ReturnType<typeof setTimeout> | null = null;
  private dismissProbeGen = 0;

  /** Whether the dismiss-with-reason dialog is currently open. */
  dismissDialogOpen = false;

  /** Row ids checked for dismissal inside a multi-row dismiss dialog. */
  dismissCheckedIds = new Set<string>();

  /** Optional plain-text dismissal reason (<= 500 runes). */
  dismissReasonDraft = '';

  /** Whether the dismiss POST is in flight. */
  dismissSubmitting = false;

  /** Whether an on-open asker probe reported the asking agent is gone. */
  dismissProbedGone = false;

  /** Epoch ms override returned by a 409 `too_recent` / `dismiss_changed` (`availableAt`). */
  dismissAvailableAtOverrideMs = 0;

  constructor(host: MessageActionHost, opts: MessageActionStateOptions<TPayload>) {
    this.host = host;
    this.opts = opts;
    host.addController(this);
  }

  get isConnected(): boolean {
    return this.connected;
  }

  hostConnected(): void {
    this.connected = true;
    if (this.opts.onUpdated) {
      this.unsubscribeLive = subscribeMessageActionUpdated<TPayload>(
        this.opts.kind,
        this.opts.onUpdated,
        {
          eventSource: this.opts.eventSource,
          messageId: this.opts.getMessageId,
          threadId: this.opts.getThreadId,
        }
      );
    }
    this.scheduleDismissUnlockTimer();
  }

  hostDisconnected(): void {
    this.connected = false;
    if (this.unsubscribeLive) {
      this.unsubscribeLive();
      this.unsubscribeLive = null;
    }
    this.clearDismissUnlockTimer();
    ++this.dismissProbeGen;
  }

  // ---- Dismiss minimum-age countdown ----

  private nowMs(): number {
    return this.opts.clockNow ? this.opts.clockNow() : Date.now();
  }

  private get baseDismissAvailableAtMs(): number {
    if (this.opts.getDismissAvailableAtMs) {
      return this.opts.getDismissAvailableAtMs();
    }
    const minAge = this.opts.dismissMinAgeMs ?? 0;
    if (minAge <= 0) return 0;
    const createdAt = this.opts.getCreatedAt?.() ?? '';
    if (!createdAt) return 0;
    const parsed = Date.parse(createdAt);
    return Number.isFinite(parsed) ? parsed + minAge : 0;
  }

  /** Effective epoch ms when dismiss becomes available (0 if immediately available). */
  get dismissAvailableAtMsEffective(): number {
    return Math.max(this.baseDismissAvailableAtMs, this.dismissAvailableAtOverrideMs);
  }

  /** Remaining whole minutes (rounded up, min 1) until dismiss unlocks, or 0 if available. */
  get dismissRemainingMin(): number {
    const availMs = this.dismissAvailableAtMsEffective;
    const now = this.nowMs();
    if (availMs <= 0 || now >= availMs) return 0;
    return Math.max(1, Math.ceil((availMs - now) / 60000));
  }

  /** Sets a server-provided `availableAt` override (ISO string or epoch ms) and reschedules timer. */
  setDismissAvailableAtOverride(availableAt: string | number): void {
    const parsed = typeof availableAt === 'number' ? availableAt : Date.parse(availableAt);
    if (!Number.isFinite(parsed)) return;
    this.dismissAvailableAtOverrideMs = parsed;
    this.host.requestUpdate();
    this.scheduleDismissUnlockTimer();
  }

  private clearDismissUnlockTimer(): void {
    if (this.dismissUnlockTimer !== null) {
      clearTimeout(this.dismissUnlockTimer);
      this.dismissUnlockTimer = null;
    }
  }

  /** Schedules a timeout for the next minute boundary so the countdown ticks and unlocks live. */
  scheduleDismissUnlockTimer(): void {
    this.clearDismissUnlockTimer();
    if (!this.connected) return;
    if (this.opts.hasDismissableRows && !this.opts.hasDismissableRows()) return;
    const availMs = this.dismissAvailableAtMsEffective;
    const remainingMs = availMs - this.nowMs();
    if (availMs <= 0 || remainingMs <= 0) return;
    const remMin = Math.max(1, Math.ceil(remainingMs / 60000));
    const nextBoundaryMs = remainingMs - (remMin - 1) * 60000;
    const delayMs = Math.min(2_147_483_647, Math.max(1, nextBoundaryMs + 25));
    this.dismissUnlockTimer = setTimeout(() => {
      this.dismissUnlockTimer = null;
      if (!this.connected) return;
      this.host.requestUpdate();
      this.scheduleDismissUnlockTimer();
    }, delayMs);
  }

  // ---- Dismiss-with-reason dialog state ----

  /**
   * Opens the dismiss dialog with `rowIds` (or `getDismissableRowIds()`)
   * initially checked. Returns `false` if dismiss is locked by minimum age or
   * no rows are dismissable.
   */
  openDismissDialog(rowIds?: readonly string[]): boolean {
    if (this.dismissRemainingMin > 0) return false;
    const ids = rowIds ?? this.opts.getDismissableRowIds?.() ?? [];
    if (ids.length === 0) return false;
    this.dismissCheckedIds = new Set(ids);
    this.dismissReasonDraft = '';
    this.dismissSubmitting = false;
    this.dismissProbedGone = false;
    this.dismissDialogOpen = true;
    this.host.requestUpdate();
    return true;
  }

  /** Resets and closes the dismiss dialog unconditionally (e.g. after submit or reconciliation). */
  closeDismissDialogState(): void {
    ++this.dismissProbeGen;
    this.dismissDialogOpen = false;
    this.dismissCheckedIds = new Set();
    this.dismissReasonDraft = '';
    this.dismissProbedGone = false;
    this.host.requestUpdate();
  }

  /** User-initiated close; ignored while a dismiss POST is in flight. */
  readonly closeDismissDialog = (): void => {
    if (this.dismissSubmitting) return;
    this.closeDismissDialogState();
  };

  /** Updates the submitting flag and notifies the host. */
  setDismissSubmitting(submitting: boolean): void {
    if (this.dismissSubmitting === submitting) return;
    this.dismissSubmitting = submitting;
    this.host.requestUpdate();
  }

  /**
   * Runs an async gone-asker probe while the dismiss dialog is open, ignoring
   * stale results if the dialog closed or the host disconnected.
   */
  probeAskerGone(probe: () => Promise<boolean>): void {
    const gen = ++this.dismissProbeGen;
    void probe().then((gone) => {
      if (gen !== this.dismissProbeGen || !this.connected || !this.dismissDialogOpen) return;
      if (gone) {
        this.dismissProbedGone = true;
        this.host.requestUpdate();
      }
    });
  }

  /**
   * Reconciles an open dismiss dialog against the currently dismissable rows:
   * closes the dialog when none remain, otherwise drops stale checked ids.
   */
  reconcileOpenDismissDialog(validRowIds?: readonly string[]): void {
    if (!this.dismissDialogOpen || this.dismissSubmitting) return;
    const ids = validRowIds ?? this.opts.getDismissableRowIds?.() ?? [];
    if (ids.length === 0) {
      this.closeDismissDialogState();
      return;
    }
    const validIds = new Set(ids);
    let changed = false;
    const nextChecked = new Set<string>();
    for (const id of this.dismissCheckedIds) {
      if (validIds.has(id)) nextChecked.add(id);
      else changed = true;
    }
    if (changed) {
      this.dismissCheckedIds = nextChecked;
      this.host.requestUpdate();
    }
  }

  /** Toggles a row's selection in the dismiss dialog. */
  toggleDismissRow(rowId: string, checked: boolean): void {
    const next = new Set(this.dismissCheckedIds);
    if (checked) next.add(rowId);
    else next.delete(rowId);
    this.dismissCheckedIds = next;
    this.host.requestUpdate();
  }

  /** Event handler for `<sl-checkbox @sl-change>` in the dismiss dialog. */
  onDismissRowToggle(rowId: string, e: Event): void {
    const checked = Boolean((e.currentTarget as HTMLElement & { checked?: boolean })?.checked);
    this.toggleDismissRow(rowId, checked);
  }

  /** Event handler for the dismiss reason `<sl-textarea>` (clamps to 500 runes). */
  readonly onDismissReasonInput = (e: Event): void => {
    const raw = (e.target as HTMLElement & { value?: string })?.value ?? '';
    const clamped = clampActionDismissReason(raw);
    if (clamped !== raw && e.target && 'value' in e.target) {
      (e.target as { value: string }).value = clamped;
    }
    this.dismissReasonDraft = clamped;
    this.host.requestUpdate();
  };

  // ---- Terminal-state banners & jump links ----

  /** Formats " · time" for an ISO instant, or nothing. */
  renderAt(at: string | undefined): TemplateResult | typeof nothing {
    if (!at) return nothing;
    return html`<span aria-hidden="true">·</span>
      <time datetime=${at} title=${formatInstantWithZone(at)}>${formatInstant(at, 'time')}</time>`;
  }

  /** Prevents default navigation and dispatches the jump event on the host. */
  jump(e: Event, messageId: string): void {
    e.preventDefault();
    this.host.dispatchEvent(
      new CustomEvent<{ messageId: string }>(this.opts.jumpEventName ?? 'message-action-jump', {
        detail: { messageId },
        bubbles: true,
        composed: true,
      })
    );
  }

  private renderJumpLink(
    replacedBy: MessageActionReplacedBy | undefined
  ): TemplateResult | typeof nothing {
    if (!replacedBy?.messageId) return nothing;
    const label = replacedBy.version ? `v${replacedBy.version}` : 'the new version';
    return html`<span aria-hidden="true">·</span>
      <a
        class="jump"
        href="#msg-${replacedBy.messageId}"
        @click=${(e: Event): void => this.jump(e, replacedBy.messageId)}
        >Jump to ${label}</a
      >`;
  }

  /** Banner rendered while a submission is in flight. */
  renderSubmittingBanner(label = 'Sending…'): TemplateResult {
    return html`<div class="row-foot" role="status">${label}</div>`;
  }

  /** Banner rendered when a row was replaced by a newer version. */
  renderReplacedBanner(replacedBy?: MessageActionReplacedBy): TemplateResult {
    return html`<div class="row-foot">
      <sl-icon name="arrow-repeat"></sl-icon>
      <span>${replacedBy?.version ? `Replaced by v${replacedBy.version}` : 'Replaced'}</span>
      ${this.renderAt(replacedBy?.at)} ${this.renderJumpLink(replacedBy)}
    </div>`;
  }

  /** Banner rendered when a row (or whole card) was withdrawn by the asking agent. */
  renderWithdrawnBanner(
    info: MessageActionWithdrawnBanner,
    opts?: { inCardFoot?: boolean }
  ): TemplateResult {
    const inCardFoot = Boolean(opts?.inCardFoot);
    return html`<div class="wd${inCardFoot ? ' card-wd' : ''}" role="status">
        <span class="wd-lead"
          ><sl-icon name="slash-circle" aria-hidden="true"></sl-icon>
          <b class="state">No longer needed</b></span
        >
        <span aria-hidden="true">·</span>
        <span>withdrawn by</span>
        <b>${info.by}</b>
        ${this.renderAt(info.at)} ${this.renderJumpLink(info.replacedBy)}
      </div>
      ${info.reason ? html`<div class="wd-reason">${info.reason}</div>` : nothing}`;
  }

  /** Banner rendered when a row (or whole card) was dismissed by a user. */
  renderDismissedBanner(
    info: MessageActionDismissedBanner,
    opts?: { inCardFoot?: boolean }
  ): TemplateResult {
    const inCardFoot = Boolean(opts?.inCardFoot);
    return html`<div class="wd${inCardFoot ? ' card-wd' : ''}" role="status">
        <sl-icon class="dc" name="x-circle" aria-hidden="true"></sl-icon>
        <span><b class="state">Dismissed</b> by <b>${info.by}</b></span>
        ${this.renderAt(info.at)} ${this.renderJumpLink(info.replacedBy)}
      </div>
      ${info.reason ? html`<div class="wd-reason">${info.reason}</div>` : nothing}`;
  }

  /** Banner rendered when a row expired without being resolved. */
  renderExpiredBanner(
    info?: MessageActionExpiredBanner,
    opts?: { inCardFoot?: boolean }
  ): TemplateResult {
    const inCardFoot = Boolean(opts?.inCardFoot);
    return html`<div class="wd${inCardFoot ? ' card-wd' : ''}" role="status">
      <sl-icon name="clock" aria-hidden="true"></sl-icon>
      <b class="state">Expired</b>
      ${this.renderAt(info?.at)} ${this.renderJumpLink(info?.replacedBy)}
    </div>`;
  }

  /** Fallback banner for an unknown closed-unanswered status. */
  renderClosedBanner(): TemplateResult {
    return html`<div class="row-foot" role="status">
      <sl-icon name="slash-circle" aria-hidden="true"></sl-icon>
      <span>Closed</span>
    </div>`;
  }
}
