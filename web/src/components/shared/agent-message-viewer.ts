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
 * Agent message viewer component.
 *
 * Displays structured messages from the dedicated "scion-messages" Cloud
 * Logging log. Shows message direction (sent/received), sender/recipient,
 * and provides a compose box for sending new messages with optional interrupt.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import type { Message } from '../../shared/types.js';
import './json-browser.js';

interface MessageLogEntry {
  timestamp: string;
  severity: string;
  message: string;
  labels?: Record<string, string>;
  resource?: Record<string, unknown>;
  jsonPayload?: Record<string, unknown>;
  insertId: string;
  sourceLocation?: { file?: string; line?: string; function?: string };
}

interface MessageLogsResponse {
  entries: MessageLogEntry[];
  nextPageToken?: string;
  hasMore?: boolean;
}

/** Parsed message info for rendering (from Hub store or Cloud Logging). */
interface ParsedMessage {
  sender: string;
  recipient: string;
  direction: 'sent' | 'received';
  msgType: string;
  body: string;
  urgent: boolean;
  broadcasted: boolean;
  timestamp: string;
  insertId: string;
  raw: MessageLogEntry | null;
}

const MAX_BUFFER = 500;

@customElement('scion-agent-message-viewer')
export class ScionAgentMessageViewer extends LitElement {
  @property()
  agentId = '';

  @property()
  agentName = '';

  /** Whether the user has message capability. */
  @property({ type: Boolean })
  canSend = false;

  /**
   * Whether Cloud Logging is available for this agent. When false, the
   * viewer skips the Cloud-Logging-only fallback fetch from
   * /message-logs. The hub message store path and the hub-store-backed
   * SSE stream (/messages/stream) work regardless of this setting.
   */
  @property({ type: Boolean })
  cloudLogging = false;

  /**
   * Custom API URL for fetching message logs.
   * When set, overrides the default agent-scoped URL.
   * Query params (tail, since) are appended automatically.
   */
  @property()
  logsUrl = '';

  /**
   * Custom API URL for the SSE message log stream.
   * When set, overrides the default agent-scoped URL.
   */
  @property()
  streamUrl = '';

  /**
   * Label shown for the "self" side of messages when no agentId is set
   * (e.g. project-level view). When agentId is present, agentName is used.
   */
  @property()
  contextLabel = '';

  /**
   * URL for broadcasting a message to all running agents in a project.
   * When set, the compose box uses the broadcast API instead of the
   * agent-scoped message endpoint.
   */
  @property()
  broadcastUrl = '';

  @state() private messages: ParsedMessage[] = [];
  @state() private entryMap = new Map<string, ParsedMessage>();
  @state() private loading = false;
  @state() private error: string | null = null;
  @state() private streaming = false;
  @state() private loaded = false;
  @state() private expandedIds = new Set<string>();

  // Compose state
  @state() private composeText = '';
  @state() private composeInterrupt = false;
  @state() private composePlain = true;
  @state() private sending = false;
  @state() private sendError: string | null = null;

  private eventSource: EventSource | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    /* Compose box */
    .compose-box {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
      padding: 1rem;
      margin-bottom: 1rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }
    .compose-label {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
    }
    .compose-row {
      display: flex;
      align-items: flex-start;
      gap: 0.75rem;
    }
    .compose-input {
      flex: 1;
    }
    .compose-input sl-input::part(base) {
      font-size: 0.875rem;
    }
    .compose-actions {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      flex-shrink: 0;
      padding-top: 0.125rem;
    }
    .compose-actions label {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      white-space: nowrap;
    }
    .send-error {
      font-size: 0.75rem;
      color: var(--scion-danger-600, #dc2626);
      margin-top: 0.375rem;
    }

    /* Toolbar */
    .toolbar {
      display: flex;
      align-items: center;
      justify-content: flex-end;
      gap: 0.75rem;
      margin-bottom: 1rem;
    }
    .toolbar-label {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin-right: 0.25rem;
    }

    .stream-indicator {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.75rem;
      color: var(--scion-success-600, #16a34a);
    }
    .stream-dot {
      width: 6px;
      height: 6px;
      border-radius: 50%;
      background: var(--scion-success-500, #22c55e);
      animation: pulse 1.5s ease-in-out infinite;
    }
    @keyframes pulse {
      0%, 100% { opacity: 1; }
      50% { opacity: 0.3; }
    }

    /* Message list */
    .message-list {
      display: flex;
      flex-direction: column;
      gap: 0;
    }

    .message-row {
      display: flex;
      align-items: flex-start;
      gap: 0.75rem;
      padding: 0.625rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      cursor: pointer;
      transition: background 0.1s ease;
    }
    .message-row:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .msg-direction {
      display: flex;
      align-items: center;
      justify-content: center;
      width: 1.75rem;
      height: 1.75rem;
      border-radius: 50%;
      flex-shrink: 0;
      margin-top: 0.125rem;
    }
    .msg-direction.received {
      background: var(--scion-primary-50, #eff6ff);
      color: var(--scion-primary-600, #2563eb);
    }
    .msg-direction.sent {
      background: var(--scion-success-50, #f0fdf4);
      color: var(--scion-success-600, #16a34a);
    }
    .msg-direction sl-icon {
      font-size: 0.875rem;
    }

    .msg-content {
      flex: 1;
      min-width: 0;
    }

    .msg-header {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-bottom: 0.25rem;
      flex-wrap: wrap;
    }
    .msg-actor {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #0f172a);
    }
    .msg-arrow {
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #0f172a);
    }
    .msg-target {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #0f172a);
    }
    .msg-time {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      margin-left: auto;
      white-space: nowrap;
    }

    .msg-badges {
      display: flex;
      gap: 0.375rem;
      align-items: center;
    }
    .msg-badge {
      display: inline-block;
      padding: 0.0625rem 0.375rem;
      border-radius: 0.25rem;
      font-size: 0.625rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.03em;
    }
    .badge-type {
      background: var(--scion-neutral-100, #f1f5f9);
      color: var(--scion-neutral-600, #475569);
    }
    .badge-urgent {
      background: var(--scion-danger-50, #fef2f2);
      color: var(--scion-danger-700, #b91c1c);
    }
    .badge-broadcast {
      background: var(--scion-warning-50, #fffbeb);
      color: var(--scion-warning-700, #b45309);
    }

    .msg-body {
      font-size: 0.8125rem;
      color: var(--scion-text-secondary, #475569);
      line-height: 1.5;
      word-break: break-word;
      white-space: pre-wrap;
    }

    /* Expanded detail */
    .msg-detail {
      margin-top: 0.5rem;
      padding: 0.5rem 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
    }

    /* Date divider */
    .date-divider {
      padding: 0.5rem 0.75rem 0.25rem;
      font-size: 0.6875rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
    }

    /* Empty / Loading / Error */
    .state-msg {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 3rem 2rem;
      color: var(--scion-text-muted, #64748b);
      gap: 0.75rem;
    }
    .state-msg sl-spinner {
      font-size: 1.5rem;
    }
    .state-msg sl-icon {
      font-size: 2rem;
      opacity: 0.4;
    }
  `;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopStream();
  }

  /** Called by the parent when the messages tab is first shown. */
  loadMessages(): void {
    if (this.loaded) return;
    this.loaded = true;
    void this.fetchMessages();
  }

  // ---------------------------------------------------------------------------
  // Data fetching
  // ---------------------------------------------------------------------------

  private get resolvedLogsUrl(): string {
    if (this.logsUrl) return this.logsUrl;
    if (this.agentId) return `/api/v1/agents/${this.agentId}/message-logs`;
    return '';
  }

  private get resolvedStreamUrl(): string {
    if (this.streamUrl) return this.streamUrl;
    if (!this.agentId) return '';
    // Prefer the hub-store-backed per-agent stream, which works on any
    // deployment regardless of Cloud Logging. The older
    // /message-logs/stream endpoint is Cloud Logging only and is kept
    // as a fallback for deployments that explicitly enable that.
    return `/api/v1/agents/${this.agentId}/messages/stream`;
  }

  private async fetchMessages(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      // Primary: Hub message store API
      if (this.agentId) {
        const hubRes = await apiFetch(`/api/v1/agents/${this.agentId}/messages?limit=200`);
        if (hubRes.ok) {
          const data = (await hubRes.json()) as { items?: Message[] } | null;
          const items = data?.items ?? [];
          if (items.length > 0) {
            this.mergeHubMessages(items);
            return;
          }
        }
      }

      // Fallback: Cloud Logging proxy (for pre-migration records or when Hub is unavailable).
      // Skipped when Cloud Logging is unavailable — the /message-logs endpoint returns 501
      // in that case, which would turn an empty hub-store result into a user-facing error
      // instead of the intended "No messages found" empty state.
      if (!this.cloudLogging && !this.logsUrl) return;
      const baseUrl = this.resolvedLogsUrl;
      if (!baseUrl) return;

      const params = new URLSearchParams({ tail: '200' });
      if (this.messages.length > 0) {
        params.set('since', this.messages[0].timestamp);
      }
      const res = await apiFetch(`${baseUrl}?${params.toString()}`);
      if (!res.ok) {
        const errData = await res.json().catch(() => ({})) as { error?: { message?: string }; message?: string };
        throw new Error(
          (errData.error as { message?: string })?.message || errData.message || `HTTP ${res.status}`
        );
      }
      const logData = (await res.json()) as MessageLogsResponse;
      this.mergeEntries(logData.entries || []);
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to fetch messages';
    } finally {
      this.loading = false;
    }
  }

  /** Parse a Hub store Message into a ParsedMessage for rendering. */
  private parseHubMessage(msg: Message): ParsedMessage {
    // Hub store: senderId === agentId means the agent sent the message (outbound).
    // Otherwise the agent is the recipient (inbound from human).
    const direction: 'sent' | 'received' = !this.agentId || msg.senderId === this.agentId
      ? 'sent'
      : 'received';

    return {
      sender: msg.sender,
      recipient: msg.recipient,
      direction,
      msgType: msg.type,
      body: msg.msg,
      urgent: msg.urgent ?? false,
      broadcasted: msg.broadcasted ?? false,
      timestamp: msg.createdAt,
      insertId: `hub:${msg.id}`,
      raw: null,
    };
  }

  private mergeHubMessages(items: Message[]): void {
    for (const item of items) {
      const parsed = this.parseHubMessage(item);
      if (!this.entryMap.has(parsed.insertId)) {
        this.entryMap.set(parsed.insertId, parsed);
      }
    }

    const sorted = Array.from(this.entryMap.values()).sort(
      (a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime()
    );

    if (sorted.length > MAX_BUFFER) {
      const evicted = sorted.splice(MAX_BUFFER);
      for (const e of evicted) {
        this.entryMap.delete(e.insertId);
      }
    }

    this.messages = sorted;
  }

  private parseEntry(entry: MessageLogEntry): ParsedMessage {
    const labels = entry.labels || {};
    const payload = entry.jsonPayload || {};
    const sender = labels['sender'] || (payload['sender'] as string) || '';
    const recipient = labels['recipient'] || (payload['recipient'] as string) || '';
    const msgType = labels['msg_type'] || (payload['msg_type'] as string) || '';
    const urgent = (payload['urgent'] === true) || (labels['urgent'] === 'true');
    const broadcasted = (payload['broadcasted'] === true) || (labels['broadcasted'] === 'true');

    // Determine direction relative to this agent using unique IDs.
    // Check sender_id and recipient_id labels first (UUID-based, unambiguous).
    // Fall back to agent_id label for older log entries.
    // When no agentId is set (project-level view), always show as 'sent'
    // (sender → recipient) since there's no "self" agent.
    let direction: 'sent' | 'received';
    if (!this.agentId) {
      direction = 'sent';
    } else {
      const senderIdLabel = labels['sender_id'] || '';
      const recipientIdLabel = labels['recipient_id'] || '';
      if (senderIdLabel === this.agentId) {
        direction = 'sent';
      } else if (recipientIdLabel === this.agentId) {
        direction = 'received';
      } else {
        // Fallback for entries logged before sender_id/recipient_id were added
        const entryAgentId = labels['agent_id'] || '';
        direction = entryAgentId === this.agentId ? 'received' : 'sent';
      }
    }

    // Extract message body from the payload.
    // payload['message'] and entry.message are the Cloud Logging message
    // (e.g. "message dispatched"), NOT the scion message content.
    // The actual message body is in payload['message_content'].
    const body = (payload['message_content'] as string)
      || '';

    return {
      sender,
      recipient,
      direction,
      msgType,
      body,
      urgent,
      broadcasted,
      timestamp: entry.timestamp,
      insertId: entry.insertId,
      raw: entry,
    };
  }

  private mergeEntries(newEntries: MessageLogEntry[]): void {
    for (const entry of newEntries) {
      if (!this.entryMap.has(entry.insertId)) {
        this.entryMap.set(entry.insertId, this.parseEntry(entry));
      }
    }

    const sorted = Array.from(this.entryMap.values()).sort(
      (a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime()
    );

    if (sorted.length > MAX_BUFFER) {
      const evicted = sorted.splice(MAX_BUFFER);
      for (const e of evicted) {
        this.entryMap.delete(e.insertId);
      }
    }

    this.messages = sorted;
  }

  // ---------------------------------------------------------------------------
  // Streaming
  // ---------------------------------------------------------------------------

  private startStream(): void {
    if (this.eventSource) return;
    const url = this.resolvedStreamUrl;
    if (!url) return;
    this.streaming = true;

    this.eventSource = new EventSource(url);

    // Hub-store-backed stream: emits "message" events with a full
    // UserMessageEvent payload (hub store record) per message.
    this.eventSource.addEventListener('message', (event: Event) => {
      try {
        const msg = JSON.parse((event as MessageEvent).data) as Message;
        this.mergeHubMessages([msg]);
      } catch {
        // Skip unparseable entries
      }
    });

    // Cloud-Logging-backed stream (fallback path via /message-logs/stream
    // when explicitly opted in): emits "log" events with raw log entries.
    this.eventSource.addEventListener('log', (event: Event) => {
      try {
        const entry = JSON.parse((event as MessageEvent).data) as MessageLogEntry;
        this.mergeEntries([entry]);
      } catch {
        // Skip unparseable entries
      }
    });

    this.eventSource.addEventListener('timeout', () => {
      this.stopStream();
      void this.fetchMessages(); // backfill anything dropped during the prior window
      this.startStream();
    });

    this.eventSource.onerror = () => {
      // EventSource will auto-reconnect for transient errors
    };
  }

  /** Stop the SSE stream. Can be called by parent components (e.g. on collapse). */
  stopStream(): void {
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }
    this.streaming = false;
  }

  /** Reset loaded state so the next loadMessages() call will refetch. */
  resetLoaded(): void {
    this.loaded = false;
  }

  // ---------------------------------------------------------------------------
  // Send message
  // ---------------------------------------------------------------------------

  private async handleSend(): Promise<void> {
    const text = this.composeText.trim();
    if (!text || this.sending) return;

    this.sending = true;
    this.sendError = null;

    try {
      let url: string;
      let body: Record<string, unknown>;

      if (this.broadcastUrl) {
        // Broadcast mode: always sends structured_message
        url = this.broadcastUrl;
        body = {
          structured_message: { msg: text, plain: this.composePlain },
          interrupt: this.composeInterrupt,
        };
      } else {
        // Agent-scoped mode
        url = `/api/v1/agents/${this.agentId}/message`;
        body = {
          structured_message: { msg: text, plain: this.composePlain },
          interrupt: this.composeInterrupt,
        };
      }

      const res = await apiFetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        this.sendError = await extractApiError(res, 'Failed to send message');
        return;
      }
      this.composeText = '';
      // Refresh to pick up the newly sent message
      void this.fetchMessages();
    } catch (err) {
      this.sendError = err instanceof Error ? err.message : 'Failed to send message';
    } finally {
      this.sending = false;
    }
  }

  private handleComposeKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      void this.handleSend();
    }
  }

  // ---------------------------------------------------------------------------
  // UI handlers
  // ---------------------------------------------------------------------------

  private toggleExpand(insertId: string): void {
    if (this.expandedIds.has(insertId)) {
      this.expandedIds.delete(insertId);
    } else {
      this.expandedIds.add(insertId);
    }
    this.requestUpdate();
  }

  private handleStreamToggle(e: Event): void {
    const checked = (e.target as HTMLInputElement).checked;
    if (checked) {
      this.startStream();
    } else {
      this.stopStream();
    }
  }

  private handleRefresh(): void {
    void this.fetchMessages();
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      ${this.canSend ? this.renderCompose() : nothing}
      ${this.renderToolbar()}
      ${this.renderContent()}
    `;
  }

  private renderCompose() {
    const isBroadcast = !!this.broadcastUrl;
    const placeholder = isBroadcast
      ? 'Broadcast message to all running agents in project...'
      : 'Send a message to this agent...';
    const buttonLabel = isBroadcast ? 'Broadcast' : 'Send';

    return html`
      <div class="compose-box">
        ${isBroadcast ? html`
          <div class="compose-label">
            <sl-icon name="broadcast-pin" style="font-size: 0.875rem;"></sl-icon>
            Broadcast to all running agents in this project
          </div>
        ` : nothing}
        <div class="compose-row">
          <div class="compose-input">
            <sl-input
              placeholder=${placeholder}
              size="small"
              .value=${this.composeText}
              @sl-input=${(e: Event) => { this.composeText = (e.target as HTMLInputElement).value; }}
              @keydown=${this.handleComposeKeydown}
              ?disabled=${this.sending}
            ></sl-input>
            ${this.sendError ? html`<div class="send-error">${this.sendError}</div>` : nothing}
          </div>
          <div class="compose-actions">
            <label>
              <sl-checkbox
                size="small"
                ?checked=${this.composePlain}
                @sl-change=${(e: Event) => { this.composePlain = (e.target as HTMLInputElement).checked; }}
              ></sl-checkbox>
              Plain
            </label>
            <label>
              <sl-checkbox
                size="small"
                ?checked=${this.composeInterrupt}
                @sl-change=${(e: Event) => { this.composeInterrupt = (e.target as HTMLInputElement).checked; }}
              ></sl-checkbox>
              Interrupt
            </label>
            <sl-button
              size="small"
              variant=${isBroadcast ? 'warning' : 'primary'}
              ?loading=${this.sending}
              ?disabled=${!this.composeText.trim() || this.sending}
              @click=${this.handleSend}
            >
              <sl-icon slot="prefix" name=${isBroadcast ? 'broadcast-pin' : 'send'}></sl-icon>
              ${buttonLabel}
            </sl-button>
          </div>
        </div>
      </div>
    `;
  }

  private renderToolbar() {
    return html`
      <div class="toolbar">
        ${this.streaming
          ? html`<span class="stream-indicator"><span class="stream-dot"></span>Streaming</span>`
          : nothing}
        <sl-button
          size="small"
          variant="default"
          ?loading=${this.loading}
          ?disabled=${this.loading || this.streaming}
          @click=${this.handleRefresh}
        >
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Refresh
        </sl-button>
        ${this.resolvedStreamUrl
          ? html`
              <span class="toolbar-label">Stream</span>
              <sl-switch
                size="small"
                ?checked=${this.streaming}
                @sl-change=${this.handleStreamToggle}
              ></sl-switch>
            `
          : nothing}
      </div>
    `;
  }

  private renderContent() {
    if (this.loading && this.messages.length === 0) {
      return html`
        <div class="state-msg">
          <sl-spinner></sl-spinner>
          <span>Loading messages...</span>
        </div>
      `;
    }

    if (this.error && this.messages.length === 0) {
      return html`
        <div class="state-msg">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <span>${this.error}</span>
          <sl-button size="small" @click=${this.handleRefresh}>Retry</sl-button>
        </div>
      `;
    }

    if (this.messages.length === 0) {
      return html`
        <div class="state-msg">
          <sl-icon name="chat-dots"></sl-icon>
          <span>No messages found</span>
        </div>
      `;
    }

    return html`<div class="message-list">${this.renderMessages()}</div>`;
  }

  private renderMessages() {
    const rows: unknown[] = [];
    let lastDate = '';

    for (const msg of this.messages) {
      const d = new Date(msg.timestamp);
      const dateStr = d.toLocaleDateString('en', { year: 'numeric', month: 'short', day: 'numeric' });

      if (dateStr !== lastDate) {
        lastDate = dateStr;
        rows.push(html`<div class="date-divider">${dateStr}</div>`);
      }

      const timeStr = d.toLocaleTimeString('en', {
        hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit',
      });
      const isExpanded = this.expandedIds.has(msg.insertId);

      const arrowIcon = msg.direction === 'sent' ? 'arrow-right' : 'arrow-left';
      const dirIcon = msg.direction === 'sent' ? 'box-arrow-up-right' : 'box-arrow-in-down-left';

      // In agent-scoped view, show the current agent first with direction arrow.
      // In project-scoped view (no agentId), always show sender → recipient.
      const fromLabel = this.agentId
        ? (this.agentName || this.agentId)
        : (msg.sender || 'unknown');
      const toLabel = this.agentId
        ? (msg.direction === 'sent' ? (msg.recipient || 'unknown') : (msg.sender || 'unknown'))
        : (msg.recipient || 'unknown');

      rows.push(html`
        <div class="message-row" @click=${() => this.toggleExpand(msg.insertId)}>
          <div class="msg-direction ${msg.direction}">
            <sl-icon name=${dirIcon}></sl-icon>
          </div>
          <div class="msg-content">
            <div class="msg-header">
              <span class="msg-actor">${fromLabel}</span>
              <sl-icon name=${arrowIcon} class="msg-arrow" style="font-size:0.6875rem"></sl-icon>
              <span class="msg-target">${toLabel}</span>
              <div class="msg-badges">
                ${msg.msgType ? html`<span class="msg-badge badge-type">${msg.msgType}</span>` : nothing}
                ${msg.urgent ? html`<span class="msg-badge badge-urgent">urgent</span>` : nothing}
                ${msg.broadcasted ? html`<span class="msg-badge badge-broadcast">broadcast</span>` : nothing}
              </div>
              <span class="msg-time">${timeStr}</span>
            </div>
            <div class="msg-body">${this.truncateBody(msg.body)}</div>
            ${isExpanded ? this.renderDetail(msg) : nothing}
          </div>
        </div>
      `);
    }

    return rows;
  }

  private truncateBody(body: string): string {
    if (body.length > 300) {
      return body.substring(0, 300) + '...';
    }
    return body;
  }

  private renderDetail(msg: ParsedMessage) {
    const detail: Record<string, unknown> = {
      timestamp: msg.timestamp,
      sender: msg.sender,
      recipient: msg.recipient,
      type: msg.msgType,
      urgent: msg.urgent,
      broadcasted: msg.broadcasted,
      message: msg.body,
    };
    if (msg.raw) {
      if (msg.raw.labels && Object.keys(msg.raw.labels).length > 0) {
        detail['labels'] = msg.raw.labels;
      }
      if (msg.raw.jsonPayload && Object.keys(msg.raw.jsonPayload).length > 0) {
        detail['payload'] = msg.raw.jsonPayload;
      }
    }
    return html`
      <div class="msg-detail" @click=${(e: Event) => e.stopPropagation()}>
        <scion-json-browser .data=${detail} expand-first></scion-json-browser>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-agent-message-viewer': ScionAgentMessageViewer;
  }
}
