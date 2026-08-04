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
 * Unified log viewer component for the diagnostics dashboard.
 *
 * Streams and displays log entries from all Scion system components
 * (hub, broker, agent, messages) via a single SSE connection. Features
 * source color-coding, severity filtering (server-side), source and
 * text filtering (client-side), auto-scroll with pause-on-scroll-up,
 * entry detail expansion, and reconnection with gap-fill.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch } from '../../client/api.js';
import './json-browser.js';

interface DiagnosticLogEntry {
  timestamp: string;
  severity: string;
  message: string;
  labels?: Record<string, string>;
  resource?: Record<string, unknown>;
  jsonPayload?: Record<string, unknown>;
  insertId: string;
  sourceLocation?: { file?: string; line?: string; function?: string };
  logName?: string;
  source: string; // "hub", "broker", "agent", "messages", "server"
  _ts?: number; // cached numeric timestamp for sort performance
}

interface DiagnosticsLogResponse {
  entries: DiagnosticLogEntry[];
  hasMore: boolean;
  gcpProjectId?: string;
}

const MAX_BUFFER = 2000;

const SEVERITY_LEVELS = ['DEFAULT', 'DEBUG', 'INFO', 'WARNING', 'ERROR', 'CRITICAL'] as const;

const SOURCE_CONFIG: Record<string, { badge: string; color: string; bg: string }> = {
  hub: { badge: 'HUB', color: '#2563eb', bg: '#eff6ff' },
  broker: { badge: 'BRK', color: '#059669', bg: '#ecfdf5' },
  agent: { badge: 'AGT', color: '#d97706', bg: '#fffbeb' },
  messages: { badge: 'MSG', color: '#7c3aed', bg: '#f5f3ff' },
  server: { badge: 'SRV', color: '#4b5563', bg: '#f9fafb' },
};

const ALL_SOURCES = ['hub', 'broker', 'agent', 'messages', 'server'];

const SEARCH_DEBOUNCE_MS = 300;
const AUTO_SCROLL_THRESHOLD = 50; // px from bottom

@customElement('scion-unified-log-viewer')
export class ScionUnifiedLogViewer extends LitElement {
  @property({ type: String })
  gcpProjectId = '';

  @property({ type: String })
  initialSeverity = 'INFO';

  @state() private entries: DiagnosticLogEntry[] = [];
  private entryMap = new Map<string, DiagnosticLogEntry>();
  @state() private streaming = false;
  @state() private loading = false;
  @state() private error: string | null = null;
  @state() private severity = '';
  @state() private enabledSources = new Set<string>(ALL_SOURCES);
  @state() private searchText = '';
  @state() private autoScroll = true;
  @state() private newEntriesBelowCount = 0;
  @state() private expandedIds = new Set<string>();
  @state() private reconnecting = false;
  @state() private reconnectAttempt = 0;

  private eventSource: EventSource | null = null;
  private searchDebounceTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private logViewerRef: Element | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    /* Filter bar */
    .filter-bar {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem 1rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--sl-border-radius-medium, 0.5rem);
      margin-bottom: 0.5rem;
      flex-wrap: wrap;
    }

    .filter-group {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .filter-label {
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }

    .source-toggle {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.25rem 0.5rem;
      border-radius: 0.25rem;
      font-size: 0.75rem;
      font-weight: 600;
      font-family: var(--scion-font-mono, monospace);
      cursor: pointer;
      border: 1px solid transparent;
      transition: opacity 0.15s, border-color 0.15s;
      user-select: none;
    }

    .source-toggle.disabled {
      opacity: 0.3;
      border-color: var(--scion-border, #e2e8f0);
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

    .reconnect-indicator {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.75rem;
      color: var(--scion-warning-600, #d97706);
    }

    .filter-spacer {
      flex: 1;
    }

    /* Log viewer */
    .log-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--sl-border-radius-medium, 0.5rem);
      overflow: hidden;
      position: relative;
    }

    .log-scroller {
      max-height: calc(100vh - 22rem);
      min-height: 400px;
      overflow-y: auto;
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.8125rem;
    }

    /* Log entry rows */
    .log-row {
      display: grid;
      grid-template-columns: 7rem 3rem 4rem 10rem 1fr;
      gap: 0.5rem;
      padding: 0.375rem 0.75rem;
      cursor: pointer;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      transition: background 0.1s ease;
      align-items: start;
      border-left: 3px solid transparent;
    }

    .log-row:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .log-row .ts {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.75rem;
      white-space: nowrap;
    }

    .source-badge {
      display: inline-block;
      padding: 0.0625rem 0.375rem;
      border-radius: 0.25rem;
      font-size: 0.6875rem;
      font-weight: 600;
      letter-spacing: 0.03em;
      text-align: center;
      line-height: 1.4;
      font-family: var(--scion-font-mono, monospace);
    }

    .sev {
      display: inline-block;
      padding: 0.0625rem 0.375rem;
      border-radius: 0.25rem;
      font-size: 0.6875rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.03em;
      line-height: 1.4;
    }

    .sev-DEFAULT { background: var(--scion-neutral-100, #f1f5f9); color: var(--scion-neutral-500, #64748b); }
    .sev-DEBUG { background: var(--scion-neutral-100, #f1f5f9); color: var(--scion-neutral-500, #64748b); opacity: 0.7; }
    .sev-INFO { background: var(--scion-primary-50, #eff6ff); color: var(--scion-primary-700, #1d4ed8); }
    .sev-WARNING { background: var(--scion-warning-50, #fffbeb); color: var(--scion-warning-700, #b45309); }
    .sev-ERROR { background: var(--scion-danger-50, #fef2f2); color: var(--scion-danger-700, #b91c1c); }
    .sev-CRITICAL { background: var(--scion-danger-100, #fee2e2); color: var(--scion-danger-800, #991b1b); font-weight: 700; }

    .sub {
      color: var(--scion-text-secondary, #475569);
      font-size: 0.75rem;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .msg {
      color: var(--scion-text, #1e293b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    /* Detail panel */
    .detail-row {
      padding: 0.75rem 0.75rem 0.75rem 2.5rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .detail-meta {
      display: flex;
      flex-wrap: wrap;
      gap: 1rem;
      margin-bottom: 0.75rem;
      font-size: 0.75rem;
      color: var(--scion-text-secondary, #475569);
    }

    .detail-meta-item {
      display: flex;
      gap: 0.25rem;
    }

    .detail-meta-label {
      font-weight: 600;
    }

    .detail-cloud-link {
      font-size: 0.75rem;
      color: var(--scion-primary-600, #2563eb);
      text-decoration: none;
    }
    .detail-cloud-link:hover {
      text-decoration: underline;
    }

    /* New entries banner */
    .new-entries-banner {
      position: sticky;
      bottom: 0;
      display: flex;
      justify-content: center;
      padding: 0.5rem;
      background: var(--scion-primary-50, #eff6ff);
      border-top: 1px solid var(--scion-primary-200, #bfdbfe);
      cursor: pointer;
      font-size: 0.8125rem;
      color: var(--scion-primary-700, #1d4ed8);
      font-weight: 500;
    }

    .new-entries-banner:hover {
      background: var(--scion-primary-100, #dbeafe);
    }

    /* Status bar */
    .status-bar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.5rem 1rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-top: 0;
      border-radius: 0 0 var(--sl-border-radius-medium, 0.5rem) var(--sl-border-radius-medium, 0.5rem);
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Empty / Loading / Error states */
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

  override connectedCallback(): void {
    super.connectedCallback();
    this.severity = this.initialSeverity || 'INFO';
    this.loadInitialData();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopStream();
    if (this.searchDebounceTimer) {
      clearTimeout(this.searchDebounceTimer);
    }
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  private async loadInitialData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const params = new URLSearchParams({ tail: '200' });
      if (this.severity) params.set('severity', this.severity);
      const res = await apiFetch(`/api/v1/admin/diagnostics/logs?${params}`);
      if (!res.ok) {
        const errData = (await res.json().catch(() => ({}))) as {
          error?: { message?: string };
          message?: string;
        };
        throw new Error(
          (errData.error as { message?: string })?.message ||
            errData.message ||
            `HTTP ${res.status}`
        );
      }
      const data = (await res.json()) as DiagnosticsLogResponse;
      this.mergeEntries(data.entries);
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to fetch logs';
    } finally {
      this.loading = false;
    }

    // Start streaming after initial data
    this.startStream();
  }

  // ---------------------------------------------------------------------------
  // SSE Stream Management
  // ---------------------------------------------------------------------------

  private startStream(): void {
    if (this.eventSource) return;
    this.streaming = true;
    this.reconnecting = false;
    this.reconnectAttempt = 0;

    const params = new URLSearchParams();
    if (this.severity) params.set('severity', this.severity);
    const qs = params.toString();
    const url = `/api/v1/admin/diagnostics/logs/stream${qs ? '?' + qs : ''}`;
    this.eventSource = new EventSource(url);

    this.eventSource.addEventListener('log', (event: Event) => {
      try {
        const entry = JSON.parse(
          (event as MessageEvent).data
        ) as DiagnosticLogEntry;
        this.mergeEntries([entry]);

        // Only count entries that pass current filters for the "new entries" banner
        if (!this.autoScroll && this.entryPassesFilters(entry)) {
          this.newEntriesBelowCount++;
        }
      } catch {
        // skip unparseable
      }
    });

    this.eventSource.addEventListener('timeout', () => {
      this.stopStream();
      this.startStream();
    });

    this.eventSource.onerror = () => {
      if (this.eventSource?.readyState === EventSource.CLOSED) {
        this.streaming = false;
        this.reconnecting = true;
        this.eventSource = null;
        this.reconnectWithBackoff();
      }
    };
  }

  private stopStream(): void {
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }
    this.streaming = false;
    this.reconnecting = false;
    this.reconnectAttempt = 0;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  private reconnectWithBackoff(): void {
    if (this.reconnectAttempt >= 10) {
      this.reconnecting = false;
      this.error = 'Connection lost after 10 reconnection attempts';
      return;
    }

    const delay = Math.min(1000 * Math.pow(2, this.reconnectAttempt), 30000);
    this.reconnectAttempt++;

    this.reconnectTimer = setTimeout(async () => {
      this.reconnectTimer = null;
      // Gap-fill: fetch entries since our last entry
      if (this.entries.length > 0) {
        try {
          const params = new URLSearchParams({ tail: '200' });
          if (this.severity) params.set('severity', this.severity);
          // Entries are sorted oldest-first (ascending), so last element is the latest
          params.set('since', this.entries[this.entries.length - 1].timestamp);
          const res = await apiFetch(
            `/api/v1/admin/diagnostics/logs?${params}`
          );
          if (res.ok) {
            const data = (await res.json()) as DiagnosticsLogResponse;
            this.mergeEntries(data.entries);
          }
        } catch {
          // Gap-fill failed, but we'll still try to reconnect the stream
        }
      }
      this.startStream();
    }, delay);
  }

  // ---------------------------------------------------------------------------
  // Buffer Management
  // ---------------------------------------------------------------------------

  /** Cache numeric timestamp on an entry for sort performance. */
  private ensureTimestamp(entry: DiagnosticLogEntry): number {
    if (entry._ts === undefined) {
      entry._ts = new Date(entry.timestamp).getTime();
    }
    return entry._ts;
  }

  private mergeEntries(newEntries: DiagnosticLogEntry[]): void {
    // Collect genuinely new entries (not already in the dedup map)
    const toInsert: DiagnosticLogEntry[] = [];
    for (const entry of newEntries) {
      if (!this.entryMap.has(entry.insertId)) {
        this.ensureTimestamp(entry);
        this.entryMap.set(entry.insertId, entry);
        toInsert.push(entry);
      }
    }

    if (toInsert.length === 0) return;

    if (this.entries.length === 0) {
      // First load: sort the full batch once
      toInsert.sort((a, b) => a._ts! - b._ts!);
      this.entries = toInsert;
    } else if (
      toInsert.length === 1 &&
      toInsert[0]._ts! >= this.entries[this.entries.length - 1]._ts!
    ) {
      // Common case: single stream entry newer than latest — append directly
      this.entries = [...this.entries, toInsert[0]];
    } else {
      // Multiple entries or out-of-order: merge and sort
      const merged = [...this.entries, ...toInsert];
      merged.sort((a, b) => a._ts! - b._ts!);
      this.entries = merged;
    }

    // Cap buffer — evict oldest entries
    if (this.entries.length > MAX_BUFFER) {
      const overflow = this.entries.length - MAX_BUFFER;
      const evicted = this.entries.slice(0, overflow);
      for (const e of evicted) {
        this.entryMap.delete(e.insertId);
      }
      this.entries = this.entries.slice(overflow);
    }

    // Auto-scroll after render
    if (this.autoScroll) {
      this.updateComplete.then(() => this.scrollToBottom());
    }
  }

  /** Check if a single entry passes current client-side filters. */
  private entryPassesFilters(entry: DiagnosticLogEntry): boolean {
    if (!this.enabledSources.has(entry.source)) return false;
    if (this.searchText) {
      const text = this.searchText.toLowerCase();
      if (!entry.message?.toLowerCase().includes(text)) return false;
    }
    return true;
  }

  private getFilteredEntries(): DiagnosticLogEntry[] {
    return this.entries.filter((entry) => this.entryPassesFilters(entry));
  }

  // ---------------------------------------------------------------------------
  // Filter Handlers
  // ---------------------------------------------------------------------------

  private handleSeverityChange(e: Event): void {
    this.severity = (e.target as unknown as { value: string }).value;
    // Notify parent of severity change (used by popout link)
    this.dispatchEvent(
      new CustomEvent('severity-change', {
        detail: { severity: this.severity },
        bubbles: true,
        composed: true,
      })
    );
    this.stopStream();
    this.entryMap.clear();
    this.entries = [];
    this.expandedIds.clear();
    this.newEntriesBelowCount = 0;
    this.loadInitialData();
  }

  private handleSourceToggle(source: string): void {
    const newSet = new Set(this.enabledSources);
    if (newSet.has(source)) {
      newSet.delete(source);
    } else {
      newSet.add(source);
    }
    this.enabledSources = newSet;
  }

  private handleSearchInput(e: Event): void {
    const value = (e.target as unknown as { value: string }).value;
    if (this.searchDebounceTimer) {
      clearTimeout(this.searchDebounceTimer);
    }
    this.searchDebounceTimer = setTimeout(() => {
      this.searchText = value;
    }, SEARCH_DEBOUNCE_MS);
  }

  private handleClear(): void {
    this.entryMap.clear();
    this.entries = [];
    this.expandedIds.clear();
    this.newEntriesBelowCount = 0;
  }

  // ---------------------------------------------------------------------------
  // Scroll Management
  // ---------------------------------------------------------------------------

  private handleScroll(e: Event): void {
    const el = e.target as HTMLElement;
    const atBottom =
      el.scrollHeight - el.scrollTop - el.clientHeight < AUTO_SCROLL_THRESHOLD;

    if (atBottom && !this.autoScroll) {
      this.autoScroll = true;
      this.newEntriesBelowCount = 0;
    } else if (!atBottom && this.autoScroll) {
      this.autoScroll = false;
    }
  }

  private scrollToBottom(): void {
    const scroller = this.logViewerRef || this.shadowRoot?.querySelector('.log-scroller');
    if (scroller) {
      this.logViewerRef = scroller;
      scroller.scrollTop = scroller.scrollHeight;
    }
  }

  private handleNewEntriesClick(): void {
    this.autoScroll = true;
    this.newEntriesBelowCount = 0;
    this.scrollToBottom();
  }

  // ---------------------------------------------------------------------------
  // Entry Expansion
  // ---------------------------------------------------------------------------

  private toggleExpand(insertId: string): void {
    if (this.expandedIds.has(insertId)) {
      this.expandedIds.delete(insertId);
    } else {
      this.expandedIds.add(insertId);
    }
    this.requestUpdate();
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    // Compute filtered entries once and pass to both sub-renders to avoid double-filtering.
    const filteredEntries = this.getFilteredEntries();
    return html`
      ${this.renderFilterBar()}
      ${this.renderLogContainer(filteredEntries)}
      ${this.renderStatusBar(filteredEntries)}
    `;
  }

  private renderFilterBar() {
    return html`
      <div class="filter-bar">
        <div class="filter-group">
          <span class="filter-label">Sources:</span>
          ${ALL_SOURCES.map((src) => {
            const config = SOURCE_CONFIG[src];
            const enabled = this.enabledSources.has(src);
            return html`
              <span
                class="source-toggle ${enabled ? '' : 'disabled'}"
                style="background: ${enabled ? config.bg : 'transparent'}; color: ${config.color}; border-color: ${enabled ? config.color + '40' : 'var(--scion-border, #e2e8f0)'};"
                @click=${() => this.handleSourceToggle(src)}
              >${config.badge}</span>
            `;
          })}
        </div>
        <div class="filter-group">
          <span class="filter-label">Level:</span>
          <sl-select
            size="small"
            value=${this.severity}
            @sl-change=${this.handleSeverityChange}
            style="min-width: 8rem"
          >
            ${SEVERITY_LEVELS.map(
              (level) => html`<sl-option value=${level}>${level} +</sl-option>`
            )}
          </sl-select>
        </div>
        <div class="filter-group">
          <span class="filter-label">Search:</span>
          <sl-input
            size="small"
            placeholder="Filter messages..."
            @sl-input=${this.handleSearchInput}
            clearable
            style="min-width: 12rem"
          >
            <sl-icon slot="prefix" name="search"></sl-icon>
          </sl-input>
        </div>
        <div class="filter-spacer"></div>
        ${this.streaming
          ? html`<span class="stream-indicator"><span class="stream-dot"></span>Live</span>`
          : this.reconnecting
            ? html`<span class="reconnect-indicator"><sl-spinner style="font-size: 0.75rem;"></sl-spinner> Reconnecting...</span>`
            : nothing}
        <sl-button size="small" variant="text" @click=${this.handleClear}>
          <sl-icon slot="prefix" name="trash"></sl-icon>
          Clear
        </sl-button>
      </div>
    `;
  }

  private renderLogContainer(filteredEntries: DiagnosticLogEntry[]) {
    return html`
      <div class="log-container">
        ${this.loading && this.entries.length === 0
          ? html`<div class="state-msg"><sl-spinner></sl-spinner><span>Loading logs...</span></div>`
          : this.error && this.entries.length === 0
            ? html`
                <div class="state-msg">
                  <sl-icon name="exclamation-triangle"></sl-icon>
                  <span>${this.error}</span>
                  <sl-button size="small" @click=${() => this.loadInitialData()}>Retry</sl-button>
                </div>
              `
            : html`
                <div class="log-scroller" @scroll=${this.handleScroll}>
                  ${this.renderEntries(filteredEntries)}
                  ${!this.autoScroll && this.newEntriesBelowCount > 0
                    ? html`
                        <div class="new-entries-banner" @click=${this.handleNewEntriesClick}>
                          ⬇ New entries below (${this.newEntriesBelowCount})
                        </div>
                      `
                    : nothing}
                </div>
              `}
      </div>
    `;
  }

  private renderEntries(filtered: DiagnosticLogEntry[]) {
    if (filtered.length === 0 && this.entries.length > 0) {
      return html`
        <div class="state-msg">
          <sl-icon name="funnel"></sl-icon>
          <span>No entries match current filters</span>
        </div>
      `;
    }

    if (filtered.length === 0) {
      return html`
        <div class="state-msg">
          <sl-icon name="file-text"></sl-icon>
          <span>No log entries yet</span>
        </div>
      `;
    }

    const rows: unknown[] = [];
    for (const entry of filtered) {
      const config = SOURCE_CONFIG[entry.source] || SOURCE_CONFIG.server;
      const d = new Date(entry.timestamp);
      const timeStr = d.toLocaleTimeString('en', {
        hour12: false,
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        fractionalSecondDigits: 3,
      } as Intl.DateTimeFormatOptions);
      const subsystem =
        (entry.jsonPayload?.['subsystem'] as string) ||
        entry.labels?.['component'] ||
        '';
      const isExpanded = this.expandedIds.has(entry.insertId);

      rows.push(html`
        <div
          class="log-row"
          style="border-left-color: ${config.color}"
          @click=${() => this.toggleExpand(entry.insertId)}
        >
          <span class="ts">${timeStr}</span>
          <span
            class="source-badge"
            style="background: ${config.bg}; color: ${config.color};"
          >${config.badge}</span>
          <span class="sev sev-${entry.severity}">${entry.severity}</span>
          <span class="sub" title=${subsystem}>${subsystem}</span>
          <span class="msg" title=${entry.message}>${entry.message}</span>
        </div>
      `);

      if (isExpanded) {
        rows.push(this.renderDetailPanel(entry));
      }
    }

    return rows;
  }

  private renderDetailPanel(entry: DiagnosticLogEntry) {
    const d = new Date(entry.timestamp);
    const fullTimestamp = d.toISOString();

    return html`
      <div class="detail-row">
        <div class="detail-meta">
          <span class="detail-meta-item">
            <span class="detail-meta-label">Timestamp:</span>
            ${fullTimestamp}
          </span>
          ${entry.logName
            ? html`<span class="detail-meta-item"><span class="detail-meta-label">Log:</span> ${entry.logName}</span>`
            : nothing}
          ${entry.labels?.['agent_id']
            ? html`<span class="detail-meta-item"><span class="detail-meta-label">Agent:</span> ${entry.labels['agent_id']}</span>`
            : nothing}
          ${entry.labels?.['project_id']
            ? html`<span class="detail-meta-item"><span class="detail-meta-label">Project:</span> ${entry.labels['project_id']}</span>`
            : nothing}
          ${entry.labels?.['broker_id']
            ? html`<span class="detail-meta-item"><span class="detail-meta-label">Broker:</span> ${entry.labels['broker_id']}</span>`
            : nothing}
          ${this.gcpProjectId && entry.insertId
            ? html`
                <a
                  class="detail-cloud-link"
                  href="https://console.cloud.google.com/logs/query;query=insertId%3D%22${encodeURIComponent(entry.insertId)}%22?project=${this.gcpProjectId}"
                  target="_blank"
                >View in Cloud Logging →</a>
              `
            : nothing}
        </div>
        <scion-json-browser
          .data=${this.buildDetailObject(entry)}
          expand-first
        ></scion-json-browser>
      </div>
    `;
  }

  private buildDetailObject(
    entry: DiagnosticLogEntry
  ): Record<string, unknown> {
    const obj: Record<string, unknown> = {
      timestamp: entry.timestamp,
      severity: entry.severity,
      source: entry.source,
      message: entry.message,
    };
    if (entry.labels && Object.keys(entry.labels).length > 0) {
      obj['labels'] = entry.labels;
    }
    if (entry.jsonPayload && Object.keys(entry.jsonPayload).length > 0) {
      obj['jsonPayload'] = entry.jsonPayload;
    }
    if (entry.resource && Object.keys(entry.resource).length > 0) {
      obj['resource'] = entry.resource;
    }
    if (entry.sourceLocation) {
      obj['sourceLocation'] = entry.sourceLocation;
    }
    if (entry.logName) {
      obj['logName'] = entry.logName;
    }
    obj['insertId'] = entry.insertId;
    return obj;
  }

  private renderStatusBar(filtered: DiagnosticLogEntry[]) {
    const streamState = this.streaming
      ? 'Streaming'
      : this.reconnecting
        ? 'Reconnecting'
        : 'Disconnected';
    const scrollState = this.autoScroll ? '▼ auto-scrolling' : '⏸ paused';

    return html`
      <div class="status-bar">
        <span>${streamState} · ${filtered.length} entries (${this.entries.length} total) · Buffer: ${this.entries.length}/${MAX_BUFFER}</span>
        <span>${scrollState}</span>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-unified-log-viewer': ScionUnifiedLogViewer;
  }
}
