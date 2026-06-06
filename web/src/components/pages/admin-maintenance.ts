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
 * Admin Maintenance Operations page component
 *
 * Displays maintenance operations and migrations with execution support.
 * Phase 1: Read-only display of operations and migration checklist.
 * Phase 2: Migration execution with dry-run support and status polling.
 * Phase 3: Routine operation execution with run history.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

interface MaintenanceOperation {
  id: string;
  key: string;
  title: string;
  description: string;
  category: string;
  status: string;
  createdAt: string;
  startedAt?: string;
  completedAt?: string;
  startedBy?: string;
  result?: string;
}

interface MaintenanceRun {
  id: string;
  operationKey?: string;
  status: string;
  startedAt: string;
  completedAt?: string;
  startedBy?: string;
  result?: string;
  log?: string;
}

interface MaintenanceOperationWithRun extends MaintenanceOperation {
  lastRun?: MaintenanceRun;
}

interface MaintenanceResponse {
  migrations: MaintenanceOperation[] | null;
  operations: MaintenanceOperationWithRun[] | null;
}

interface UpdateCommitInfo {
  hash: string;
  subject: string;
}

interface UpdateCheckResult {
  update_available: boolean;
  current_commit: string;
  latest_commit: string;
  current_branch: string;
  tracking_ref: string;
  commits_behind: number;
  new_commits?: UpdateCommitInfo[];
}

@customElement('scion-page-admin-maintenance')
export class ScionPageAdminMaintenance extends LitElement {
  @state()
  private loading = true;

  @state()
  private error: string | null = null;

  @state()
  private migrations: MaintenanceOperation[] = [];

  @state()
  private operations: MaintenanceOperationWithRun[] = [];

  /** Key of migration being confirmed for execution via dialog. */
  @state()
  private runDialogKey: string | null = null;

  /** Category of the item being run (migration or operation). */
  @state()
  private runDialogCategory: string = 'migration';

  /** Dry-run checkbox state in the run dialog. */
  @state()
  private runDialogDryRun = false;

  /** Whether a run request is in-flight. */
  @state()
  private runInProgress = false;

  /** Polling timer for running operations/migrations. */
  private pollTimer: ReturnType<typeof setInterval> | null = null;

  /** Keys of operations whose run history is expanded. */
  @state()
  private expandedHistory: Set<string> = new Set();

  /** Loaded run history keyed by operation key. */
  @state()
  private runHistory: Map<string, MaintenanceRun[]> = new Map();

  /** Whether maintenance mode is enabled. */
  @state()
  private maintenanceEnabled = false;

  /** Run detail currently being viewed. */
  @state()
  private viewingRun: MaintenanceRun | null = null;

  /** Whether the bulk reset-auth request is in-flight. */
  @state()
  private resetAuthAllLoading = false;

  /** Result of the last bulk reset-auth request. */
  @state()
  private resetAuthAllResult: { succeeded: { id: string; name: string }[]; failed: { id: string; name: string; error: string }[]; total: number } | null = null;

  /** Update check state for rebuild-server. */
  @state()
  private updateCheckLoading = false;

  @state()
  private updateCheckError: string | null = null;

  @state()
  private updateCheckResult: UpdateCheckResult | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 2rem;
    }

    .header sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    /* -- Maintenance mode toggle ------------------------------------------ */

    .maintenance-mode-toggle {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .maintenance-mode-toggle .toggle-track {
      position: relative;
      width: 36px;
      height: 20px;
      background: var(--scion-border, #e2e8f0);
      border-radius: 10px;
      cursor: pointer;
      transition: background 0.2s ease;
      border: none;
      padding: 0;
      flex-shrink: 0;
    }

    .maintenance-mode-toggle .toggle-track:hover {
      background: var(--scion-text-muted, #94a3b8);
    }

    .maintenance-mode-toggle .toggle-track.active {
      background: var(--scion-warning, #f59e0b);
    }

    .maintenance-mode-toggle .toggle-track.active:hover {
      background: #d97706;
    }

    .maintenance-mode-toggle .toggle-knob {
      position: absolute;
      top: 2px;
      left: 2px;
      width: 16px;
      height: 16px;
      background: white;
      border-radius: 50%;
      transition: transform 0.2s ease;
      pointer-events: none;
    }

    .maintenance-mode-toggle .toggle-track.active .toggle-knob {
      transform: translateX(16px);
    }

    .maintenance-mode-toggle .toggle-label {
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    /* -- Sections --------------------------------------------------------- */

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section-title {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .section-description {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    /* -- Cards ------------------------------------------------------------ */

    .card-list {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .card {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 1.25rem;
    }

    .card-header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.5rem;
    }

    .card-header sl-icon {
      font-size: 1.25rem;
      flex-shrink: 0;
    }

    .card-header sl-icon.pending {
      color: var(--scion-text-muted, #64748b);
    }

    .card-header sl-icon.completed {
      color: var(--sl-color-success-600, #16a34a);
    }

    .card-header sl-icon.failed {
      color: var(--sl-color-danger-600, #dc2626);
    }

    .card-header sl-icon.running {
      color: var(--scion-primary, #3b82f6);
    }

    .card-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      flex: 1;
    }

    .card-description {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      line-height: 1.5;
      margin-bottom: 0.75rem;
    }

    .card-meta {
      display: flex;
      gap: 1.5rem;
      flex-wrap: wrap;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .card-meta span {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
    }

    .card-footer {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-top: 0.75rem;
    }

    /* -- Result log ------------------------------------------------------- */

    .result-log {
      margin-top: 0.75rem;
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.8125rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      white-space: pre-wrap;
      word-break: break-word;
      max-height: 300px;
      overflow-y: auto;
      color: var(--scion-text, #1e293b);
    }

    .result-error {
      color: var(--sl-color-danger-700, #b91c1c);
    }

    /* -- Status badges ---------------------------------------------------- */

    .status-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .status-badge.pending {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .status-badge.completed {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .status-badge.failed {
      background: var(--sl-color-danger-100, #fee2e2);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .status-badge.running {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    /* -- Update check banner --------------------------------------------- */

    .update-check-banner {
      margin-top: 0.5rem;
      margin-bottom: 0.5rem;
      padding: 0.625rem 0.75rem;
      border-radius: 0.375rem;
      border: 1px solid var(--sl-color-primary-200, #bfdbfe);
      background: var(--sl-color-primary-50, #eff6ff);
      font-size: 0.8125rem;
    }

    .update-check-banner.up-to-date {
      border-color: var(--sl-color-success-200, #bbf7d0);
      background: var(--sl-color-success-50, #f0fdf4);
    }

    .update-check-header {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      font-weight: 600;
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .update-check-banner.up-to-date .update-check-header {
      color: var(--sl-color-success-700, #15803d);
    }

    .update-check-header sl-icon {
      font-size: 0.875rem;
    }

    .update-check-commits {
      margin-top: 0.375rem;
      max-height: 8rem;
      overflow-y: auto;
      font-family: var(--sl-font-mono, monospace);
      line-height: 1.5;
      color: var(--scion-text, #1e293b);
    }

    .update-check-commits .commit-hash {
      color: var(--sl-color-primary-600, #2563eb);
      margin-right: 0.375rem;
    }

    .update-check-error {
      margin-top: 0.375rem;
      font-size: 0.8125rem;
      color: var(--sl-color-danger-700, #b91c1c);
    }

    /* -- Run history table ------------------------------------------------ */

    .history-toggle {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.8125rem;
      color: var(--scion-primary, #3b82f6);
      cursor: pointer;
      border: none;
      background: none;
      padding: 0.25rem 0;
      margin-top: 0.5rem;
    }

    .history-toggle:hover {
      text-decoration: underline;
    }

    .history-toggle sl-icon {
      font-size: 0.75rem;
      transition: transform 0.2s;
    }

    .history-toggle.expanded sl-icon {
      transform: rotate(90deg);
    }

    .run-history-table {
      width: 100%;
      border-collapse: collapse;
      margin-top: 0.75rem;
      font-size: 0.8125rem;
    }

    .run-history-table th {
      text-align: left;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      color: var(--scion-text-muted, #64748b);
      font-weight: 500;
    }

    .run-history-table td {
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border-subtle, #f1f5f9);
      color: var(--scion-text, #1e293b);
    }

    .run-history-table tr {
      cursor: pointer;
    }

    .run-history-table tr:hover td {
      background: var(--scion-bg-subtle, #f8fafc);
    }

    /* -- Dialog ----------------------------------------------------------- */

    .dialog-body {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .dialog-body p {
      margin: 0;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      line-height: 1.5;
    }

    /* -- Empty state ------------------------------------------------------ */

    .empty-inline {
      padding: 1.5rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    /* -- Loading / Error -------------------------------------------------- */

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-state sl-spinner {
      font-size: 2rem;
      margin-bottom: 1rem;
    }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .error-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-danger-500, #ef4444);
      margin-bottom: 1rem;
    }

    .error-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .error-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .error-details {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 1rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadData();
    void this.fetchMaintenanceState();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopPolling();
  }

  private async fetchMaintenanceState(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/admin/maintenance');
      if (res.ok) {
        const data = await res.json();
        this.maintenanceEnabled = data.enabled;
      }
    } catch {
      // Silently ignore — toggle will default to off.
    }
  }

  private async toggleMaintenance(): Promise<void> {
    const newValue = !this.maintenanceEnabled;
    try {
      const res = await apiFetch('/api/v1/admin/maintenance', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: newValue }),
      });
      if (res.ok) {
        const data = await res.json();
        this.maintenanceEnabled = data.enabled;
      }
    } catch {
      // Silently ignore — keep current state on failure.
    }
  }

  private async loadData(isPolling = false): Promise<void> {
    // Only show loading spinner on initial load, not during poll refreshes.
    if (!isPolling) {
      this.loading = true;
      this.error = null;
    }

    try {
      const response = await apiFetch('/api/v1/admin/maintenance/operations');
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as MaintenanceResponse;
      const newMigrations = data.migrations ?? [];
      const newOperations = data.operations ?? [];

      // Only update state (and trigger re-render) if data actually changed.
      if (JSON.stringify(this.migrations) !== JSON.stringify(newMigrations)) {
        this.migrations = newMigrations;
      }
      if (JSON.stringify(this.operations) !== JSON.stringify(newOperations)) {
        this.operations = newOperations;
      }

      // Start polling if any migration or operation run is active.
      const hasRunning =
        this.migrations.some((m) => m.status === 'running') ||
        this.operations.some((op) => op.lastRun?.status === 'running');
      if (hasRunning) {
        this.startPolling();
      } else {
        this.stopPolling();
      }
    } catch (err) {
      console.error('Failed to load maintenance operations:', err);
      if (!isPolling) {
        this.error = err instanceof Error ? err.message : 'Failed to load maintenance operations';
      }
    } finally {
      if (!isPolling) {
        this.loading = false;
      }
    }
  }

  private startPolling(): void {
    if (this.pollTimer) return;
    this.pollTimer = setInterval(() => void this.loadData(true), 3000);
  }

  private stopPolling(): void {
    if (this.pollTimer) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
  }

  private formatDate(dateString: string | undefined): string {
    if (!dateString) return '';
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return '';
      return date.toLocaleDateString('en-US', {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
      });
    } catch {
      return dateString;
    }
  }

  private formatDateTime(dateString: string | undefined): string {
    if (!dateString) return '';
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return '';
      return date.toLocaleDateString('en-US', {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
      });
    } catch {
      return dateString;
    }
  }

  private formatRelativeTime(dateString: string | undefined): string {
    if (!dateString) return '';
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return '';
      const diffMs = Date.now() - date.getTime();
      const diffSeconds = Math.round(diffMs / 1000);
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));
      const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffSeconds) < 60) {
        return rtf.format(-diffSeconds, 'second');
      } else if (Math.abs(diffMinutes) < 60) {
        return rtf.format(-diffMinutes, 'minute');
      } else if (Math.abs(diffHours) < 24) {
        return rtf.format(-diffHours, 'hour');
      } else {
        return rtf.format(-diffDays, 'day');
      }
    } catch {
      return dateString;
    }
  }

  private formatDuration(startStr: string, endStr?: string): string {
    if (!startStr || !endStr) return '--';
    try {
      const start = new Date(startStr).getTime();
      const end = new Date(endStr).getTime();
      const diffMs = end - start;
      if (diffMs < 0 || isNaN(diffMs)) return '--';
      if (diffMs < 1000) return '<1s';
      const seconds = Math.round(diffMs / 1000);
      if (seconds < 60) return `${seconds}s`;
      const minutes = Math.floor(seconds / 60);
      const remainSecs = seconds % 60;
      return `${minutes}m ${remainSecs}s`;
    } catch {
      return '--';
    }
  }

  private statusIcon(status: string): string {
    switch (status) {
      case 'completed':
        return 'check-circle-fill';
      case 'failed':
        return 'exclamation-circle-fill';
      case 'running':
        return 'hourglass-split';
      default:
        return 'circle';
    }
  }

  // -- Migration execution ------------------------------------------------

  private openRunDialog(key: string, category: string): void {
    this.runDialogKey = key;
    this.runDialogCategory = category;
    this.runDialogDryRun = false;
  }

  private closeRunDialog(): void {
    if (!this.runInProgress) {
      this.runDialogKey = null;
      this.runDialogDryRun = false;
    }
  }

  private async executeRunMigration(): Promise<void> {
    if (!this.runDialogKey) return;

    this.runInProgress = true;
    try {
      const response = await apiFetch(
        `/api/v1/admin/maintenance/migrations/${this.runDialogKey}/run`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            params: {
              dryRun: this.runDialogDryRun,
            },
          }),
        },
      );

      if (!response.ok) {
        const errMsg = await extractApiError(response, `HTTP ${response.status}`);
        throw new Error(errMsg);
      }

      this.runDialogKey = null;
      this.runDialogDryRun = false;

      // Reload and start polling for status.
      await this.loadData();
      this.startPolling();
    } catch (err) {
      console.error('Failed to start migration:', err);
      this.error = err instanceof Error ? err.message : 'Failed to start migration';
    } finally {
      this.runInProgress = false;
    }
  }

  // -- Operation execution ------------------------------------------------

  private async executeRunOperation(): Promise<void> {
    if (!this.runDialogKey) return;

    this.runInProgress = true;
    try {
      const response = await apiFetch(
        `/api/v1/admin/maintenance/operations/${this.runDialogKey}/run`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ params: {} }),
        },
      );

      if (!response.ok) {
        const errMsg = await extractApiError(response, `HTTP ${response.status}`);
        throw new Error(errMsg);
      }

      this.runDialogKey = null;

      // Reload and start polling for status.
      await this.loadData();
      this.startPolling();
    } catch (err) {
      console.error('Failed to start operation:', err);
      this.error = err instanceof Error ? err.message : 'Failed to start operation';
    } finally {
      this.runInProgress = false;
    }
  }

  // -- Run history --------------------------------------------------------

  private async toggleHistory(key: string): Promise<void> {
    const expanded = new Set(this.expandedHistory);
    if (expanded.has(key)) {
      expanded.delete(key);
      this.expandedHistory = expanded;
      return;
    }

    expanded.add(key);
    this.expandedHistory = expanded;

    // Load history if not cached.
    if (!this.runHistory.has(key)) {
      await this.loadRunHistory(key);
    }
  }

  private async loadRunHistory(key: string): Promise<void> {
    try {
      const response = await apiFetch(
        `/api/v1/admin/maintenance/operations/${key}/runs?limit=20`,
      );
      if (!response.ok) return;

      const data = (await response.json()) as { runs: MaintenanceRun[] | null };
      const updated = new Map(this.runHistory);
      updated.set(key, data.runs ?? []);
      this.runHistory = updated;
    } catch {
      // Silently ignore load errors for history.
    }
  }

  private async viewRunDetail(run: MaintenanceRun): Promise<void> {
    // If we already have log data, show it immediately.
    if (run.log) {
      this.viewingRun = run;
      return;
    }

    // Otherwise fetch the full run detail.
    try {
      const key = run.operationKey ?? '';
      const response = await apiFetch(
        `/api/v1/admin/maintenance/operations/${key}/runs/${run.id}`,
      );
      if (response.ok) {
        const detail = (await response.json()) as MaintenanceRun;
        this.viewingRun = detail;
      } else {
        this.viewingRun = run;
      }
    } catch {
      this.viewingRun = run;
    }
  }

  private closeRunDetail(): void {
    this.viewingRun = null;
  }

  private parseMigrationResult(resultStr: string | undefined): { log?: string; error?: string; dryRun?: boolean } | null {
    if (!resultStr) return null;
    try {
      return JSON.parse(resultStr) as { log?: string; error?: string; dryRun?: boolean };
    } catch {
      return null;
    }
  }

  // -- Rendering ----------------------------------------------------------

  override render() {
    return html`
      <div class="header">
        <sl-icon name="wrench-adjustable"></sl-icon>
        <h1>Maintenance</h1>
      </div>

      ${this.loading
        ? this.renderLoading()
        : this.error
          ? this.renderError()
          : this.renderContent()}

      ${this.renderRunDialog()}
      ${this.renderRunDetailDialog()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading maintenance operations...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderContent() {
    return html`
      ${this.renderMaintenanceMode()}
      ${this.renderQuickActions()}
      ${this.renderMigrations()}
      ${this.renderOperations()}
    `;
  }

  private renderQuickActions() {
    return html`
      <div class="section">
        <h2 class="section-title">Quick Actions</h2>
        <p class="section-description">
          One-off administrative actions across all agents.
        </p>
        <div class="card-list">
          <div class="card">
            <div class="card-header">
              <sl-icon name="key" class="pending"></sl-icon>
              <span class="card-title">Reset Auth &mdash; All Running Agents</span>
              ${this.resetAuthAllLoading
                ? html`<sl-button size="small" disabled loading>Running...</sl-button>`
                : html`
                    <sl-button
                      variant="warning"
                      size="small"
                      @click=${() => this.handleResetAuthAll()}
                    >
                      <sl-icon slot="prefix" name="key"></sl-icon>
                      Reset Auth
                    </sl-button>
                  `}
            </div>
            <div class="card-description">
              Inject a fresh auth token into every running agent without restarting them.
              The token refresh loop in each agent will pick up the new credentials automatically.
            </div>
            ${this.resetAuthAllResult
              ? html`
                  <div class="card-meta">
                    <span>
                      Total: ${this.resetAuthAllResult.total} &middot;
                      Succeeded: ${this.resetAuthAllResult.succeeded?.length ?? 0} &middot;
                      Failed: ${this.resetAuthAllResult.failed?.length ?? 0}
                    </span>
                  </div>
                  ${(this.resetAuthAllResult.failed?.length ?? 0) > 0
                    ? html`
                        <div class="result-log result-error">${this.resetAuthAllResult.failed
                            .map((f) => `${f.name || f.id}: ${f.error}`)
                            .join('\n')}</div>
                      `
                    : nothing}
                `
              : nothing}
          </div>
        </div>
      </div>
    `;
  }

  private async handleResetAuthAll() {
    this.resetAuthAllLoading = true;
    this.resetAuthAllResult = null;
    try {
      const response = await apiFetch('/api/v1/admin/agents/reset-auth-all', {
        method: 'POST',
      });
      if (!response.ok) {
        const errMsg = await extractApiError(response, `HTTP ${response.status}`);
        throw new Error(errMsg);
      }
      this.resetAuthAllResult = await response.json();
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to reset auth for all agents');
    } finally {
      this.resetAuthAllLoading = false;
    }
  }

  private renderMaintenanceMode() {
    return html`
      <div class="section">
        <h2 class="section-title">Maintenance Mode</h2>
        <p class="section-description">
          When maintenance mode is enabled, only administrators can log in.
          All other users will be temporarily blocked until maintenance mode is
          disabled.
        </p>
        <div class="maintenance-mode-toggle">
          <button
            class="toggle-track ${this.maintenanceEnabled ? 'active' : ''}"
            @click=${() => { this.toggleMaintenance(); }}
            aria-label="Toggle maintenance mode"
          >
            <span class="toggle-knob"></span>
          </button>
          <span class="toggle-label">
            ${this.maintenanceEnabled ? 'Enabled' : 'Disabled'}
          </span>
        </div>
      </div>
    `;
  }

  private renderMigrations() {
    return html`
      <div class="section">
        <h2 class="section-title">Migrations</h2>
        <p class="section-description">
          One-time data migrations that transition the system between states.
          Completed migrations cannot be re-run from the UI.
        </p>
        ${this.migrations.length === 0
          ? html`<div class="empty-inline">No migrations registered.</div>`
          : html`
              <div class="card-list">
                ${this.migrations.map((m) => this.renderMigrationCard(m))}
              </div>
            `}
      </div>
    `;
  }

  private renderMigrationCard(m: MaintenanceOperation) {
    const canRun = m.status === 'pending' || m.status === 'failed';
    const isRunning = m.status === 'running';
    const result = this.parseMigrationResult(m.result);

    return html`
      <div class="card">
        <div class="card-header">
          ${isRunning
            ? html`<sl-spinner style="font-size: 1.25rem;"></sl-spinner>`
            : html`<sl-icon
                name="${this.statusIcon(m.status)}"
                class="${m.status}"
              ></sl-icon>`}
          <span class="card-title">${m.title}</span>
          <span class="status-badge ${m.status}">${m.status}</span>
        </div>
        <div class="card-description">${m.description}</div>
        <div class="card-meta">
          <span>Created: ${this.formatDate(m.createdAt)}</span>
          ${m.completedAt
            ? html`<span>Completed: ${this.formatDate(m.completedAt)}</span>`
            : nothing}
          ${m.startedBy
            ? html`<span>By: ${m.startedBy}</span>`
            : nothing}
        </div>
        ${result?.log
          ? html`<div class="result-log ${result.error ? 'result-error' : ''}">${result.log}</div>`
          : nothing}
        ${result?.error && !result.log
          ? html`<div class="result-log result-error">${result.error}</div>`
          : nothing}
        ${canRun || isRunning
          ? html`
              <div class="card-footer">
                <div></div>
                ${canRun
                  ? html`
                      <sl-button
                        variant="primary"
                        size="small"
                        @click=${() => this.openRunDialog(m.key, 'migration')}
                      >
                        <sl-icon slot="prefix" name="play-circle"></sl-icon>
                        ${m.status === 'failed' ? 'Retry' : 'Run'}
                      </sl-button>
                    `
                  : html`
                      <sl-button size="small" disabled loading>
                        Running...
                      </sl-button>
                    `}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  private renderOperations() {
    return html`
      <div class="section">
        <h2 class="section-title">Routine Operations</h2>
        <p class="section-description">
          Repeatable infrastructure tasks that can be run on demand.
        </p>
        ${this.operations.length === 0
          ? html`<div class="empty-inline">No operations registered.</div>`
          : html`
              <div class="card-list">
                ${this.operations.map((op) => this.renderOperationCard(op))}
              </div>
            `}
      </div>
    `;
  }

  private renderOperationCard(op: MaintenanceOperationWithRun) {
    const isRunning = op.lastRun?.status === 'running';
    const historyExpanded = this.expandedHistory.has(op.key);
    const runs = this.runHistory.get(op.key) ?? [];
    const isRebuild = op.key === 'rebuild-server';

    return html`
      <div class="card">
        <div class="card-header">
          ${isRunning
            ? html`<sl-spinner style="font-size: 1.25rem;"></sl-spinner>`
            : html`<sl-icon name="play-circle" class="pending"></sl-icon>`}
          <span class="card-title">${op.title}</span>
          ${isRebuild
            ? html`
                <sl-button
                  size="small"
                  variant="default"
                  ?loading=${this.updateCheckLoading}
                  ?disabled=${isRunning}
                  @click=${() => this.checkForUpdates()}
                >
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Check for Updates
                </sl-button>
              `
            : nothing}
          ${isRunning
            ? html`<sl-button size="small" disabled loading>Running...</sl-button>`
            : html`
                <sl-button
                  variant="primary"
                  size="small"
                  @click=${() => this.openRunDialog(op.key, 'operation')}
                >
                  <sl-icon slot="prefix" name="play-circle"></sl-icon>
                  Run
                </sl-button>
              `}
        </div>
        <div class="card-description">${op.description}</div>
        ${isRebuild ? this.renderUpdateCheckBanner() : nothing}
        ${op.lastRun
          ? html`
              <div class="card-meta">
                <span>
                  Last run: ${this.formatRelativeTime(op.lastRun.startedAt)}
                  (<span class="status-badge ${op.lastRun.status}">${op.lastRun.status}</span>)
                </span>
                ${op.lastRun.startedBy
                  ? html`<span>by ${op.lastRun.startedBy}</span>`
                  : nothing}
              </div>
              ${op.lastRun.status === 'failed'
                ? this.renderLastRunError(op.lastRun)
                : nothing}
            `
          : html`
              <div class="card-meta">
                <span>Never run</span>
              </div>
            `}
        <button
          class="history-toggle ${historyExpanded ? 'expanded' : ''}"
          @click=${() => this.toggleHistory(op.key)}
        >
          <sl-icon name="chevron-right"></sl-icon>
          Run History
        </button>
        ${historyExpanded ? this.renderRunHistoryTable(op.key, runs) : nothing}
      </div>
    `;
  }

  private renderLastRunError(run: MaintenanceRun) {
    const result = this.parseMigrationResult(run.result);
    const errorMsg = result?.error;
    if (!errorMsg) return nothing;
    return html`<div class="result-log result-error">${errorMsg}</div>`;
  }

  private renderRunHistoryTable(key: string, runs: MaintenanceRun[]) {
    if (runs.length === 0) {
      return html`<div class="empty-inline">No runs recorded yet.</div>`;
    }

    return html`
      <table class="run-history-table">
        <thead>
          <tr>
            <th>Started</th>
            <th>Duration</th>
            <th>Status</th>
            <th>By</th>
          </tr>
        </thead>
        <tbody>
          ${runs.map(
            (run) => html`
              <tr @click=${() => this.viewRunDetail({ ...run, operationKey: key })}>
                <td>${this.formatDateTime(run.startedAt)}</td>
                <td>${this.formatDuration(run.startedAt, run.completedAt)}</td>
                <td><span class="status-badge ${run.status}">${run.status}</span></td>
                <td>${run.startedBy ?? '--'}</td>
              </tr>
            `,
          )}
        </tbody>
      </table>
    `;
  }

  private renderRunDialog() {
    if (!this.runDialogKey) return nothing;

    if (this.runDialogCategory === 'migration') {
      return this.renderMigrationRunDialog();
    }
    return this.renderOperationRunDialog();
  }

  private renderMigrationRunDialog() {
    const migration = this.migrations.find((m) => m.key === this.runDialogKey);
    if (!migration) return nothing;

    return html`
      <sl-dialog
        label="Run Migration"
        open
        @sl-request-close=${() => this.closeRunDialog()}
      >
        <div class="dialog-body">
          <p><strong>${migration.title}</strong></p>
          <p>${migration.description}</p>
          <sl-checkbox
            ?checked=${this.runDialogDryRun}
            @sl-change=${(e: Event) => {
              this.runDialogDryRun = (e.target as HTMLInputElement).checked;
            }}
          >
            Dry run (preview changes without applying)
          </sl-checkbox>
        </div>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => this.closeRunDialog()}
          ?disabled=${this.runInProgress}
        >Cancel</sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.runInProgress}
          @click=${() => this.executeRunMigration()}
        >
          <sl-icon slot="prefix" name="play-circle"></sl-icon>
          ${this.runDialogDryRun ? 'Dry Run' : 'Run Migration'}
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderOperationRunDialog() {
    const operation = this.operations.find((op) => op.key === this.runDialogKey);
    if (!operation) return nothing;

    const isDestructive = this.runDialogKey === 'rebuild-server';

    return html`
      <sl-dialog
        label="Run Operation"
        open
        @sl-request-close=${() => this.closeRunDialog()}
      >
        <div class="dialog-body">
          <p><strong>${operation.title}</strong></p>
          <p>${operation.description}</p>
          ${isDestructive
            ? html`<p style="color: var(--sl-color-warning-700, #a16207); font-weight: 500;">
                This operation will restart the server. You will temporarily lose connectivity.
              </p>`
            : nothing}
        </div>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => this.closeRunDialog()}
          ?disabled=${this.runInProgress}
        >Cancel</sl-button>
        <sl-button
          slot="footer"
          variant="${isDestructive ? 'warning' : 'primary'}"
          ?loading=${this.runInProgress}
          @click=${() => this.executeRunOperation()}
        >
          <sl-icon slot="prefix" name="play-circle"></sl-icon>
          Run
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderRunDetailDialog() {
    if (!this.viewingRun) return nothing;
    const run = this.viewingRun;
    const result = this.parseMigrationResult(run.result);

    return html`
      <sl-dialog
        label="Run Detail"
        open
        @sl-request-close=${() => this.closeRunDetail()}
        style="--width: 48rem;"
      >
        <div class="dialog-body">
          <div class="card-meta">
            <span>Status: <span class="status-badge ${run.status}">${run.status}</span></span>
            <span>Started: ${this.formatDateTime(run.startedAt)}</span>
            ${run.completedAt
              ? html`<span>Duration: ${this.formatDuration(run.startedAt, run.completedAt)}</span>`
              : nothing}
            ${run.startedBy ? html`<span>By: ${run.startedBy}</span>` : nothing}
          </div>
          ${result?.error
            ? html`<div class="result-log result-error">${result.error}</div>`
            : nothing}
          ${run.log
            ? html`<div class="result-log">${run.log}</div>`
            : html`<div class="empty-inline">No log output captured.</div>`}
        </div>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => this.closeRunDetail()}
        >Close</sl-button>
      </sl-dialog>
    `;
  }

  // ── Update check ──

  private async checkForUpdates(): Promise<void> {
    this.updateCheckLoading = true;
    this.updateCheckError = null;
    this.updateCheckResult = null;
    try {
      const res = await apiFetch('/api/v1/admin/maintenance/check-updates', {
        method: 'POST',
      });
      if (!res.ok) {
        this.updateCheckError = await extractApiError(res, 'Failed to check for updates');
        return;
      }
      this.updateCheckResult = (await res.json()) as UpdateCheckResult;
    } catch {
      this.updateCheckError = 'Failed to connect to server';
    } finally {
      this.updateCheckLoading = false;
    }
  }

  private renderUpdateCheckBanner() {
    if (this.updateCheckError) {
      return html`<div class="update-check-error">${this.updateCheckError}</div>`;
    }
    const r = this.updateCheckResult;
    if (!r) return nothing;

    const branchLabel = r.current_branch && r.current_branch !== 'main'
      ? html` on <code>${r.current_branch}</code>`
      : nothing;

    if (!r.update_available) {
      return html`
        <div class="update-check-banner up-to-date">
          <div class="update-check-header">
            <sl-icon name="check-circle"></sl-icon>
            Server is up to date${branchLabel}
          </div>
        </div>
      `;
    }

    return html`
      <div class="update-check-banner">
        <div class="update-check-header">
          <sl-icon name="info-circle"></sl-icon>
          Update available${branchLabel} &mdash; ${r.commits_behind} new
          commit${r.commits_behind === 1 ? '' : 's'}
        </div>
        ${r.new_commits && r.new_commits.length > 0
          ? html`
              <div class="update-check-commits">
                ${r.new_commits.map(
                  (c) => html`
                    <div>
                      <span class="commit-hash">${c.hash}</span>${c.subject}
                    </div>
                  `,
                )}
              </div>
            `
          : nothing}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-maintenance': ScionPageAdminMaintenance;
  }
}
