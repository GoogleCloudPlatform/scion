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
 * Debug Panel Component
 *
 * Full-height right-side panel for real-time subscription system observability.
 * Shows connection status, scope, subscriptions, state summary, event log,
 * and auth debug info.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state, query } from 'lit/decorators.js';
import { debugLog } from '../../client/debug-log.js';
import type { DebugEntry, DebugCategory } from '../../client/debug-log.js';
import { stateManager } from '../../client/state.js';

interface DebugData {
  debug: boolean;
  timestamp: string;
  auth: {
    stateUser: { id: string; email: string; name: string } | null;
    sessionUser: { id: string; email: string; name: string } | null;
    devToken: string;
    devAuthEnabled: boolean;
  };
  session: {
    exists: boolean;
    isNew: boolean;
    keys: string[];
    hasUser: boolean;
    hasReturnTo: boolean;
    hasOauthState: boolean;
  };
  cookies: {
    header: string;
    count: number;
    names: string[];
    hasSessionCookie: boolean;
  };
  config: {
    production: boolean;
    debug: boolean;
    baseUrl: string;
    hubApiUrl: string;
    hasGoogleOAuth: boolean;
    hasGitHubOAuth: boolean;
    authorizedDomains: string[];
  };
}

@customElement('scion-debug-panel')
export class ScionDebugPanel extends LitElement {
  @property({ type: Boolean })
  expanded = false;

  @state()
  private debugData: DebugData | null = null;

  @state()
  private loading = false;

  @state()
  private error: string | null = null;

  @state()
  private debugAvailable = true;

  @state()
  private logEntries: readonly DebugEntry[] = [];

  @state()
  private expandedEntryId: number | null = null;

  @state()
  private authExpanded = false;

  @state()
  private stateIdsExpanded = false;

  @state()
  private autoScroll = true;

  @query('.event-log-list')
  private logListEl!: HTMLElement;

  private logUpdateHandler = () => {
    this.logEntries = [...debugLog.log];
    if (this.autoScroll) {
      this.updateComplete.then(() => this.scrollLogToBottom());
    }
  };

  private stateUpdateHandler = () => {
    this.requestUpdate();
  };

  static override styles = css`
    :host {
      display: block;
      font-family: var(--scion-font-mono, 'SF Mono', 'Fira Code', monospace);
      font-size: 0.75rem;
      z-index: 10000;
    }

    /* Toggle button - fixed at bottom-right */
    .toggle-button {
      position: fixed;
      bottom: 1rem;
      right: 1rem;
      z-index: 10001;
      background: #1e293b;
      color: #f1f5f9;
      border: 1px solid #334155;
      padding: 0.5rem 1rem;
      border-radius: 0.375rem;
      cursor: pointer;
      font-family: inherit;
      font-size: inherit;
      display: flex;
      align-items: center;
      gap: 0.5rem;
      box-shadow: 0 4px 6px -1px rgba(0, 0, 0, 0.3);
      transition: background 0.15s;
    }

    .toggle-button:hover {
      background: #334155;
    }

    .toggle-button .dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      display: inline-block;
    }

    .dot.green { background: #22c55e; }
    .dot.yellow { background: #f59e0b; }
    .dot.red { background: #ef4444; }
    .dot.gray { background: #64748b; }

    /* Panel - full height right side */
    .panel {
      position: fixed;
      top: 0;
      right: 0;
      height: 100vh;
      width: 420px;
      background: #1e293b;
      color: #f1f5f9;
      z-index: 10000;
      display: flex;
      flex-direction: column;
      box-shadow: -4px 0 20px rgba(0, 0, 0, 0.3);
      transform: translateX(100%);
      transition: transform 0.2s ease-out;
      overflow: hidden;
    }

    .panel.open {
      transform: translateX(0);
    }

    /* Panel header */
    .panel-header {
      background: #0f172a;
      padding: 0.75rem 1rem;
      display: flex;
      align-items: center;
      justify-content: space-between;
      border-bottom: 1px solid #334155;
      flex-shrink: 0;
    }

    .panel-header h3 {
      margin: 0;
      font-size: 0.875rem;
      font-weight: 600;
    }

    .panel-header-actions {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .panel-header button {
      background: transparent;
      border: none;
      color: #94a3b8;
      cursor: pointer;
      padding: 0.25rem 0.5rem;
      font-family: inherit;
      font-size: 0.875rem;
    }

    .panel-header button:hover {
      color: #f1f5f9;
    }

    /* Scrollable content */
    .panel-content {
      flex: 1;
      overflow-y: auto;
      overflow-x: hidden;
    }

    /* Sections */
    .section {
      border-bottom: 1px solid #334155;
      padding: 0.75rem 1rem;
    }

    .section:last-child {
      border-bottom: none;
    }

    .section-title {
      font-weight: 600;
      color: #3b82f6;
      margin-bottom: 0.5rem;
      text-transform: uppercase;
      font-size: 0.625rem;
      letter-spacing: 0.05em;
      display: flex;
      align-items: center;
      justify-content: space-between;
    }

    .section-title button {
      background: transparent;
      border: 1px solid #475569;
      color: #94a3b8;
      padding: 0.125rem 0.375rem;
      border-radius: 0.25rem;
      cursor: pointer;
      font-family: inherit;
      font-size: 0.625rem;
    }

    .section-title button:hover {
      color: #f1f5f9;
      border-color: #64748b;
    }

    .collapsible-title {
      cursor: pointer;
      user-select: none;
    }

    .collapsible-title:hover {
      color: #60a5fa;
    }

    /* Info rows */
    .info-row {
      display: flex;
      justify-content: space-between;
      padding: 0.2rem 0;
    }

    .info-label {
      color: #94a3b8;
    }

    .info-value {
      color: #f1f5f9;
      text-align: right;
      max-width: 250px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .info-value.success { color: #22c55e; }
    .info-value.warning { color: #f59e0b; }
    .info-value.error { color: #ef4444; }

    /* Status indicator */
    .status-row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.2rem 0;
    }

    .status-dot {
      width: 10px;
      height: 10px;
      border-radius: 50%;
      flex-shrink: 0;
    }

    .status-dot.connected { background: #22c55e; }
    .status-dot.reconnecting { background: #f59e0b; animation: pulse 1s infinite; }
    .status-dot.disconnected { background: #ef4444; }
    .status-dot.idle { background: #64748b; }

    @keyframes pulse {
      0%, 100% { opacity: 1; }
      50% { opacity: 0.4; }
    }

    /* Subject list */
    .subject-list {
      list-style: none;
      margin: 0;
      padding: 0;
    }

    .subject-item {
      padding: 0.2rem 0;
      color: #e2e8f0;
      font-size: 0.7rem;
    }

    .subject-item::before {
      content: '> ';
      color: #64748b;
    }

    /* ID list (expandable) */
    .id-list {
      margin: 0.25rem 0 0 0;
      padding: 0 0 0 0.75rem;
      list-style: none;
      font-size: 0.65rem;
      color: #94a3b8;
    }

    .id-list li {
      padding: 0.1rem 0;
      font-family: inherit;
    }

    /* Event log */
    .event-log-list {
      max-height: 300px;
      overflow-y: auto;
      margin: 0;
      padding: 0;
      list-style: none;
    }

    .log-entry {
      padding: 0.35rem 0;
      border-bottom: 1px solid #1e293b;
      cursor: pointer;
    }

    .log-entry:hover {
      background: rgba(255, 255, 255, 0.03);
    }

    .log-entry-header {
      display: flex;
      align-items: center;
      gap: 0.4rem;
      font-size: 0.7rem;
    }

    .log-time {
      color: #64748b;
      flex-shrink: 0;
      font-size: 0.65rem;
    }

    .log-badge {
      padding: 0.05rem 0.3rem;
      border-radius: 0.2rem;
      font-size: 0.6rem;
      font-weight: 600;
      text-transform: uppercase;
      flex-shrink: 0;
    }

    .log-badge.sse { background: #1e3a5f; color: #60a5fa; }
    .log-badge.state { background: #14532d; color: #4ade80; }
    .log-badge.connection { background: #422006; color: #fbbf24; }

    .log-label {
      color: #e2e8f0;
      flex-shrink: 0;
    }

    .log-subject {
      color: #94a3b8;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      flex: 1;
      min-width: 0;
    }

    .log-detail {
      background: #0f172a;
      padding: 0.5rem;
      margin-top: 0.25rem;
      border-radius: 0.25rem;
      font-size: 0.65rem;
      color: #cbd5e1;
      white-space: pre-wrap;
      word-break: break-all;
      max-height: 200px;
      overflow-y: auto;
    }

    .empty-state {
      color: #64748b;
      font-style: italic;
      padding: 0.25rem 0;
    }

    /* Refresh/clear buttons */
    .action-button {
      background: #334155;
      color: #e2e8f0;
      border: none;
      padding: 0.35rem 0.75rem;
      border-radius: 0.25rem;
      cursor: pointer;
      font-family: inherit;
      font-size: 0.7rem;
    }

    .action-button:hover {
      background: #475569;
    }

    .action-button:disabled {
      opacity: 0.5;
      cursor: not-allowed;
    }

    .error-message {
      color: #ef4444;
      padding: 0.5rem;
      background: rgba(239, 68, 68, 0.1);
      border-radius: 0.25rem;
      margin-bottom: 0.5rem;
    }

    .hidden {
      display: none !important;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    debugLog.addEventListener('log-updated', this.logUpdateHandler);
    stateManager.addEventListener('connected', this.stateUpdateHandler);
    stateManager.addEventListener('disconnected', this.stateUpdateHandler);
    stateManager.addEventListener('scope-changed', this.stateUpdateHandler);
    stateManager.addEventListener('agents-updated', this.stateUpdateHandler);
    stateManager.addEventListener('projects-updated', this.stateUpdateHandler);
    stateManager.addEventListener('brokers-updated', this.stateUpdateHandler);
    this.logEntries = [...debugLog.log];
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    debugLog.removeEventListener('log-updated', this.logUpdateHandler);
    stateManager.removeEventListener('connected', this.stateUpdateHandler);
    stateManager.removeEventListener('disconnected', this.stateUpdateHandler);
    stateManager.removeEventListener('scope-changed', this.stateUpdateHandler);
    stateManager.removeEventListener('agents-updated', this.stateUpdateHandler);
    stateManager.removeEventListener('projects-updated', this.stateUpdateHandler);
    stateManager.removeEventListener('brokers-updated', this.stateUpdateHandler);
  }

  private scrollLogToBottom(): void {
    if (this.logListEl) {
      this.logListEl.scrollTop = this.logListEl.scrollHeight;
    }
  }

  private handleLogScroll(): void {
    if (!this.logListEl) return;
    const { scrollTop, scrollHeight, clientHeight } = this.logListEl;
    // If user scrolled up more than 50px from bottom, pause auto-scroll
    this.autoScroll = scrollHeight - scrollTop - clientHeight < 50;
  }

  private togglePanel(): void {
    this.expanded = !this.expanded;
    if (this.expanded && !this.debugData) {
      void this.loadDebugData();
    }
  }

  private toggleEntry(id: number): void {
    this.expandedEntryId = this.expandedEntryId === id ? null : id;
  }

  private async loadDebugData(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const response = await fetch('/auth/debug', { credentials: 'include' });
      if (response.status === 404) {
        this.debugAvailable = false;
        return;
      }
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}: ${response.statusText}`);
      }
      this.debugData = (await response.json()) as DebugData;
      this.debugAvailable = true;
    } catch (err) {
      console.error('Failed to load debug data:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load debug data';
    } finally {
      this.loading = false;
    }
  }

  private getConnectionStatus(): 'connected' | 'reconnecting' | 'disconnected' | 'idle' {
    if (stateManager.isConnected) return 'connected';
    const sse = stateManager.sseClientInstance;
    if (sse.reconnectAttemptCount > 0) return 'reconnecting';
    // No subjects means no connection was attempted — distinct from a failed connection
    if (sse.currentSubjects.length === 0) return 'idle';
    return 'disconnected';
  }

  private getConnectionStatusLabel(): string {
    const status = this.getConnectionStatus();
    if (status === 'connected') return 'Connected';
    if (status === 'reconnecting') {
      return `Reconnecting (attempt ${stateManager.sseClientInstance.reconnectAttemptCount})`;
    }
    if (status === 'idle') return 'Idle (no scope)';
    return 'Disconnected';
  }

  private exportDebugData(): void {
    const scope = stateManager.currentScope;
    const subjects = stateManager.currentSubjects;
    const snap = stateManager.getStateSnapshot();

    const payload = {
      exportedAt: new Date().toISOString(),
      pageUrl: window.location.href,
      connection: {
        status: this.getConnectionStatus(),
        connectionId: debugLog.connectionId,
        reconnectAttempts: stateManager.sseClientInstance.reconnectAttemptCount,
      },
      scope: scope ?? null,
      subscriptions: subjects,
      state: snap,
      eventLog: [...debugLog.log],
    };

    const blob = new Blob([JSON.stringify(payload, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `scion-debug-${new Date().toISOString().replace(/[:.]/g, '-')}.json`;
    a.click();
    URL.revokeObjectURL(url);
  }

  private formatTime(ts: number): string {
    const d = new Date(ts);
    return d.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })
      + '.' + String(d.getMilliseconds()).padStart(3, '0');
  }

  private formatJson(data: unknown): string {
    try {
      return JSON.stringify(data, null, 2);
    } catch {
      return String(data);
    }
  }

  private badgeClass(category: DebugCategory): string {
    return `log-badge ${category}`;
  }

  override render() {
    if (!this.debugAvailable) {
      return html``;
    }

    const connStatus = this.getConnectionStatus();
    const dotClass = connStatus === 'connected' ? 'green' : connStatus === 'reconnecting' ? 'yellow' : connStatus === 'idle' ? 'gray' : 'red';

    return html`
      <button class="toggle-button" @click=${() => this.togglePanel()}>
        <span class="dot ${dotClass}"></span>
        <span>${this.expanded ? 'Hide' : 'Show'} Debug</span>
      </button>

      <div class="panel ${this.expanded ? 'open' : ''}">
        <div class="panel-header">
          <h3>Debug Panel</h3>
          <div class="panel-header-actions">
            <button class="action-button" @click=${() => this.exportDebugData()}>Export</button>
            <button @click=${() => this.togglePanel()}>X</button>
          </div>
        </div>
        <div class="panel-content">
          ${this.renderConnectionStatus()}
          ${this.renderCurrentScope()}
          ${this.renderActiveSubscriptions()}
          ${this.renderStateSummary()}
          ${this.renderEventLog()}
          ${this.renderAuthDebug()}
        </div>
      </div>
    `;
  }

  private renderConnectionStatus() {
    const status = this.getConnectionStatus();
    const sse = stateManager.sseClientInstance;
    const subjects = sse.currentSubjects;

    return html`
      <div class="section">
        <div class="section-title">SSE Connection</div>
        <div class="status-row">
          <div class="status-dot ${status}"></div>
          <span>${this.getConnectionStatusLabel()}</span>
        </div>
        <div class="info-row">
          <span class="info-label">Endpoint</span>
          <span class="info-value">/events</span>
        </div>
        ${debugLog.connectionId ? html`
          <div class="info-row">
            <span class="info-label">Connection ID</span>
            <span class="info-value">${debugLog.connectionId}</span>
          </div>
        ` : nothing}
        ${status !== 'idle' ? html`
          <div class="info-row">
            <span class="info-label">Reconnect Attempts</span>
            <span class="info-value">${sse.reconnectAttemptCount}</span>
          </div>
        ` : nothing}
        ${subjects.length > 0 ? html`
          <div class="info-row">
            <span class="info-label">Subjects</span>
            <span class="info-value">${subjects.length}</span>
          </div>
        ` : nothing}
      </div>
    `;
  }

  private renderCurrentScope() {
    const scope = stateManager.currentScope;
    if (!scope) {
      return html`
        <div class="section">
          <div class="section-title">Current Scope</div>
          <div class="empty-state">No scope set</div>
        </div>
      `;
    }

    let detail = scope.type;
    if ('projectId' in scope) detail += ` | projectId: ${scope.projectId}`;
    if ('agentId' in scope) detail += ` | agentId: ${scope.agentId}`;
    if ('brokerId' in scope) detail += ` | brokerId: ${scope.brokerId}`;

    return html`
      <div class="section">
        <div class="section-title">Current Scope</div>
        <div class="info-row">
          <span class="info-value">${detail}</span>
        </div>
      </div>
    `;
  }

  private renderActiveSubscriptions() {
    const subjects = stateManager.currentSubjects;
    return html`
      <div class="section">
        <div class="section-title">Active Subscriptions</div>
        ${subjects.length === 0
          ? html`<div class="empty-state">No active subscriptions</div>`
          : html`
            <ul class="subject-list">
              ${subjects.map(s => html`<li class="subject-item">${s}</li>`)}
            </ul>
          `}
      </div>
    `;
  }

  private renderStateSummary() {
    const snap = stateManager.getStateSnapshot();

    return html`
      <div class="section">
        <div class="section-title">
          <span class="collapsible-title" @click=${() => { this.stateIdsExpanded = !this.stateIdsExpanded; }}>
            State Summary ${this.stateIdsExpanded ? '[-]' : '[+]'}
          </span>
        </div>
        <div class="info-row">
          <span class="info-label">Agents</span>
          <span class="info-value">${snap.agentCount}</span>
        </div>
        <div class="info-row">
          <span class="info-label">Projects</span>
          <span class="info-value">${snap.projectCount}</span>
        </div>
        <div class="info-row">
          <span class="info-label">Brokers</span>
          <span class="info-value">${snap.brokerCount}</span>
        </div>
        ${snap.deletedProjectIds.length > 0 ? html`
          <div class="info-row">
            <span class="info-label">Deleted Projects</span>
            <span class="info-value warning">${snap.deletedProjectIds.length}</span>
          </div>
        ` : nothing}
        ${this.stateIdsExpanded ? html`
          ${snap.agentIds.length > 0 ? html`
            <div class="info-label" style="margin-top: 0.4rem;">Agent IDs:</div>
            <ul class="id-list">${snap.agentIds.map(id => html`<li>${id}</li>`)}</ul>
          ` : nothing}
          ${snap.projectIds.length > 0 ? html`
            <div class="info-label" style="margin-top: 0.4rem;">Project IDs:</div>
            <ul class="id-list">${snap.projectIds.map(id => html`<li>${id}</li>`)}</ul>
          ` : nothing}
          ${snap.brokerIds.length > 0 ? html`
            <div class="info-label" style="margin-top: 0.4rem;">Broker IDs:</div>
            <ul class="id-list">${snap.brokerIds.map(id => html`<li>${id}</li>`)}</ul>
          ` : nothing}
          ${snap.deletedProjectIds.length > 0 ? html`
            <div class="info-label" style="margin-top: 0.4rem;">Deleted Project IDs:</div>
            <ul class="id-list">${snap.deletedProjectIds.map(id => html`<li>${id}</li>`)}</ul>
          ` : nothing}
        ` : nothing}
      </div>
    `;
  }

  private renderEventLog() {
    return html`
      <div class="section">
        <div class="section-title">
          <span>Event Log (${this.logEntries.length})</span>
          <button @click=${() => debugLog.clear()}>Clear</button>
        </div>
        ${this.logEntries.length === 0
          ? html`<div class="empty-state">No events captured</div>`
          : html`
            <ul class="event-log-list" @scroll=${() => this.handleLogScroll()}>
              ${this.logEntries.map(entry => this.renderLogEntry(entry))}
            </ul>
          `}
      </div>
    `;
  }

  private renderLogEntry(entry: DebugEntry) {
    const isExpanded = this.expandedEntryId === entry.id;

    return html`
      <li class="log-entry" @click=${() => this.toggleEntry(entry.id)}>
        <div class="log-entry-header">
          <span class="log-time">${this.formatTime(entry.timestamp)}</span>
          <span class="${this.badgeClass(entry.category)}">${entry.category}</span>
          <span class="log-label">${entry.label}</span>
          ${entry.subject ? html`<span class="log-subject">${entry.subject}</span>` : nothing}
        </div>
        ${isExpanded && entry.data != null ? html`
          <div class="log-detail">${this.formatJson(entry.data)}</div>
        ` : nothing}
      </li>
    `;
  }

  private renderAuthDebug() {
    return html`
      <div class="section">
        <div class="section-title">
          <span class="collapsible-title" @click=${() => { this.authExpanded = !this.authExpanded; }}>
            Auth Debug ${this.authExpanded ? '[-]' : '[+]'}
          </span>
          ${this.authExpanded ? html`
            <button class="action-button" ?disabled=${this.loading} @click=${() => this.loadDebugData()}>
              ${this.loading ? 'Loading...' : 'Refresh'}
            </button>
          ` : nothing}
        </div>
        ${this.authExpanded ? html`
          ${this.error ? html`<div class="error-message">${this.error}</div>` : nothing}
          ${this.loading ? html`<div class="empty-state">Loading...</div>` : this.renderAuthData()}
        ` : nothing}
      </div>
    `;
  }

  private renderAuthData() {
    if (!this.debugData) {
      return html`<div class="empty-state">No auth data loaded</div>`;
    }
    const data = this.debugData;

    return html`
      <div class="info-row">
        <span class="info-label">State User</span>
        <span class="info-value ${data.auth.stateUser ? 'success' : 'error'}">
          ${data.auth.stateUser?.email || 'None'}
        </span>
      </div>
      <div class="info-row">
        <span class="info-label">Session User</span>
        <span class="info-value ${data.auth.sessionUser ? 'success' : 'error'}">
          ${data.auth.sessionUser?.email || 'None'}
        </span>
      </div>
      <div class="info-row">
        <span class="info-label">Dev Auth</span>
        <span class="info-value">${data.auth.devAuthEnabled ? 'Yes' : 'No'}</span>
      </div>
      <div class="info-row">
        <span class="info-label">Session Exists</span>
        <span class="info-value ${data.session.exists ? 'success' : 'error'}">
          ${data.session.exists ? 'Yes' : 'No'}
        </span>
      </div>
      <div class="info-row">
        <span class="info-label">Session Cookie</span>
        <span class="info-value ${data.cookies.hasSessionCookie ? 'success' : 'error'}">
          ${data.cookies.hasSessionCookie ? 'Present' : 'Missing'}
        </span>
      </div>
      <div class="info-row">
        <span class="info-label">Production</span>
        <span class="info-value">${data.config.production ? 'Yes' : 'No'}</span>
      </div>
      <div class="info-row">
        <span class="info-label">Base URL</span>
        <span class="info-value">${data.config.baseUrl}</span>
      </div>
      <div class="info-row">
        <span class="info-label">Hub API URL</span>
        <span class="info-value">${data.config.hubApiUrl}</span>
      </div>
      <div class="info-row">
        <span class="info-label">Server Time</span>
        <span class="info-value">${data.timestamp}</span>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-debug-panel': ScionDebugPanel;
  }
}
