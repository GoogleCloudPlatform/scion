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
 * Agent detail page component
 *
 * Header + Tab layout (Status / Configuration / Logs) as specified in
 * .design/hosted/agent-detail-layout.md
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type {
  PageData,
  Agent,
  AgentAppliedConfig,
  AgentInlineConfig,
  TelemetryConfig,
  GCPIdentityConfig,
  Grove,
  Notification,
  Subscription,
} from '../../shared/types.js';
import {
  can,
  isTerminalAvailable,
  getAgentDisplayStatus,
  isAgentRunning,
} from '../../shared/types.js';

interface AgentNotificationsResponse {
  userNotifications: Notification[];
  agentNotifications: Notification[];
}
import type { StatusType } from '../shared/status-badge.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { stateManager } from '../../client/state.js';
import '../shared/status-badge.js';
import '../shared/agent-log-viewer.js';
import type { ScionAgentLogViewer } from '../shared/agent-log-viewer.js';
import '../shared/agent-message-viewer.js';
import type { ScionAgentMessageViewer } from '../shared/agent-message-viewer.js';
import '../shared/hash-display.js';

/**
 * Parse a Go-style duration string (e.g. "2h30m", "1h", "45m", "90s") into
 * total seconds. Returns 0 if the string cannot be parsed.
 */
function parseDuration(s: string): number {
  if (!s) return 0;
  let total = 0;
  const re = /(\d+(?:\.\d+)?)\s*(h|m|s)/g;
  let match: RegExpExecArray | null;
  while ((match = re.exec(s)) !== null) {
    const val = parseFloat(match[1]);
    switch (match[2]) {
      case 'h':
        total += val * 3600;
        break;
      case 'm':
        total += val * 60;
        break;
      case 's':
        total += val;
        break;
    }
  }
  return total;
}

/**
 * Format seconds as "Xh Ym Zs".
 */
function formatDurationHMS(totalSeconds: number): string {
  if (totalSeconds <= 0) return '0s';
  const h = Math.floor(totalSeconds / 3600);
  const m = Math.floor((totalSeconds % 3600) / 60);
  const s = Math.floor(totalSeconds % 60);
  const parts: string[] = [];
  if (h > 0) parts.push(`${h}h`);
  if (m > 0) parts.push(`${m}m`);
  if (s > 0 || parts.length === 0) parts.push(`${s}s`);
  return parts.join(' ');
}

@customElement('scion-page-agent-detail')
export class ScionPageAgentDetail extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  agentId = '';

  @state()
  private loading = true;

  @state()
  private agent: Agent | null = null;

  @state()
  private grove: Grove | null = null;

  @state()
  private error: string | null = null;

  @state()
  private actionLoading: Record<string, boolean> = {};

  @state()
  private userNotifications: Notification[] = [];

  @state()
  private agentNotifications: Notification[] = [];

  @state()
  private subscribed = false;

  @state()
  private subscriptionId: string | null = null;

  @state()
  private subscriptionLoading = false;

  static override styles = css`
    :host {
      display: block;
    }

    /* ---- Back link ---- */
    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }
    .back-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    /* ---- Header ---- */
    .header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1.5rem;
      gap: 1rem;
    }
    .header-info {
      flex: 1;
    }
    .header-title {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.5rem;
    }
    .header-title sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }
    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }
    .header-meta {
      display: flex;
      align-items: center;
      gap: 1rem;
      margin-top: 0.5rem;
    }
    .template-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.25rem 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }
    .grove-link,
    .broker-link {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
    }
    .grove-link:hover,
    .broker-link:hover {
      color: var(--scion-primary, #3b82f6);
    }
    .header-actions {
      display: flex;
      gap: 0.5rem;
      flex-shrink: 0;
    }

    /* ---- Error banner ---- */
    .agent-error-banner {
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1rem 1.25rem;
      margin-bottom: 1.5rem;
      display: flex;
      align-items: flex-start;
      gap: 0.75rem;
    }
    .agent-error-banner sl-icon {
      color: var(--sl-color-danger-500, #ef4444);
      font-size: 1.25rem;
      flex-shrink: 0;
      margin-top: 0.125rem;
    }
    .agent-error-banner .error-content {
      flex: 1;
      min-width: 0;
    }
    .agent-error-banner .error-title {
      font-weight: 600;
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 0.25rem;
    }
    .agent-error-banner .error-message {
      font-size: 0.875rem;
      color: var(--sl-color-danger-600, #dc2626);
      font-family: var(--scion-font-mono, monospace);
      white-space: pre-wrap;
      word-break: break-word;
    }

    /* ---- Tabs ---- */
    sl-tab-group {
      --track-color: var(--scion-border, #e2e8f0);
    }
    sl-tab-group::part(body) {
      padding-top: 1.5rem;
    }

    /* ---- Cards ---- */
    .card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }
    .card-title-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
      padding-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    .card-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    /* ---- Info grid ---- */
    .info-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
      gap: 1.5rem;
    }
    .info-item {
      display: flex;
      flex-direction: column;
    }
    .info-label {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin-bottom: 0.25rem;
    }
    .info-value {
      font-size: 1rem;
      color: var(--scion-text, #1e293b);
    }
    .info-value.mono {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
    }

    /* ---- Task summary ---- */
    .task-summary {
      font-size: 1rem;
      color: var(--scion-text, #1e293b);
      padding: 1rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      white-space: pre-wrap;
      line-height: 1.5;
    }

    /* ---- Telemetry tag list ---- */
    .tag-list {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
    }
    .tag-item {
      display: inline-block;
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.75rem;
      padding: 0.125rem 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--scion-text, #1e293b);
    }
    .telemetry-section {
      margin-bottom: 1.25rem;
    }
    .telemetry-section:last-child {
      margin-bottom: 0;
    }
    .telemetry-section-title {
      font-size: 0.8rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.5rem;
    }
    .telemetry-empty {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      font-style: italic;
    }

    /* ---- Progress bars ---- */
    .limits-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
      gap: 1.5rem;
    }
    .limit-item {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }
    .limit-label {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }
    .limit-value {
      font-size: 1rem;
      color: var(--scion-text, #1e293b);
      font-weight: 500;
    }
    .progress-bar-track {
      width: 100%;
      height: 6px;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: 3px;
      overflow: hidden;
    }
    .progress-bar-fill {
      height: 100%;
      border-radius: 3px;
      transition: width 0.3s ease;
    }
    .progress-bar-fill.normal {
      background: var(--scion-success-500, #22c55e);
    }
    .progress-bar-fill.warning {
      background: var(--scion-warning-500, #f59e0b);
    }
    .progress-bar-fill.danger {
      background: var(--scion-danger-500, #ef4444);
    }

    /* ---- Notification items ---- */
    .notif-section-title {
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin: 1rem 0 0.5rem 0;
    }
    .notif-section-title:first-of-type {
      margin-top: 0;
    }
    .notif-list-item {
      display: flex;
      gap: 0.625rem;
      padding: 0.625rem 0;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    .notif-list-item:last-child {
      border-bottom: none;
    }
    .notif-icon {
      flex-shrink: 0;
      display: flex;
      align-items: flex-start;
      padding-top: 2px;
    }
    .notif-icon sl-icon {
      font-size: 1rem;
    }
    .notif-icon.status-success sl-icon {
      color: var(--scion-success, #22c55e);
    }
    .notif-icon.status-warning sl-icon {
      color: var(--scion-warning, #f59e0b);
    }
    .notif-icon.status-danger sl-icon {
      color: var(--scion-danger, #ef4444);
    }
    .notif-icon.status-info sl-icon {
      color: var(--scion-text-muted, #64748b);
    }
    .notif-body {
      flex: 1;
      min-width: 0;
    }
    .notif-message {
      font-size: 0.8125rem;
      line-height: 1.4;
      color: var(--scion-text, #1e293b);
      word-break: break-word;
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
    }
    .notif-truncation-badge {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      padding: 0 0.375rem;
      margin-top: 0.125rem;
      font-size: 0.6875rem;
      font-weight: 700;
      line-height: 1.25rem;
      border-radius: 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      letter-spacing: 0.05em;
    }
    .notif-truncation-badge:hover {
      background: var(--scion-border, #e2e8f0);
    }
    .notif-meta {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-top: 0.25rem;
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }
    .notif-mark-read {
      border: none;
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.6875rem;
      cursor: pointer;
      padding: 0;
      transition: color 0.15s ease;
    }
    .notif-mark-read:hover {
      color: var(--scion-primary, #3b82f6);
    }
    .notif-empty {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 1.5rem 0;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
    }
    .notif-empty sl-icon {
      font-size: 1.25rem;
      opacity: 0.4;
    }

    /* ---- Loading / Error states ---- */
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

    /* ---- Visibility badge ---- */
    .visibility-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.8125rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

  `;

  private boundOnAgentsUpdated = this.onAgentsUpdated.bind(this);
  private boundOnGrovesUpdated = this.onGrovesUpdated.bind(this);
  private relativeTimeInterval: ReturnType<typeof setInterval> | null = null;

  override connectedCallback(): void {
    super.connectedCallback();
    if (!this.agentId && typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/agents\/([^/]+)/);
      if (match) {
        this.agentId = match[1];
      }
    }
    void this.loadData();

    stateManager.addEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
    stateManager.addEventListener('groves-updated', this.boundOnGrovesUpdated as EventListener);

    this.relativeTimeInterval = setInterval(() => this.requestUpdate(), 15000);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
    stateManager.removeEventListener('groves-updated', this.boundOnGrovesUpdated as EventListener);
    if (this.relativeTimeInterval) {
      clearInterval(this.relativeTimeInterval);
      this.relativeTimeInterval = null;
    }
  }

  private onAgentsUpdated(): void {
    const updatedAgent = stateManager.getAgent(this.agentId);
    if (updatedAgent && this.agent) {
      this.agent = { ...this.agent, ...updatedAgent };
    }
  }

  private onGrovesUpdated(): void {
    if (this.agent?.groveId) {
      const updatedGrove = stateManager.getGrove(this.agent.groveId);
      if (updatedGrove && this.grove) {
        this.grove = { ...this.grove, ...updatedGrove };
      }
    }
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      // Use SSR-prefetched agent data when available to avoid a redundant fetch.
      const ssrAgent = this.pageData?.data as Agent | undefined;
      if (ssrAgent && ssrAgent.id === this.agentId) {
        this.agent = ssrAgent;
      } else {
        const agentResponse = await apiFetch(`/api/v1/agents/${this.agentId}`);

        if (!agentResponse.ok) {
          throw new Error(await extractApiError(agentResponse, `HTTP ${agentResponse.status}: ${agentResponse.statusText}`));
        }

        this.agent = (await agentResponse.json()) as Agent;
      }

      if (this.agent.groveId) {
        stateManager.setScope({
          type: 'agent-detail',
          groveId: this.agent.groveId,
          agentId: this.agentId,
        });
      }

      // Fetch grove and notifications in parallel — they are independent.
      const parallel: Promise<void>[] = [];

      if (this.agent.groveId) {
        const groveId = this.agent.groveId;
        parallel.push(
          apiFetch(`/api/v1/groves/${groveId}`)
            .then(async (groveResponse) => {
              if (groveResponse.ok) {
                this.grove = (await groveResponse.json()) as Grove;
              }
            })
            .catch(() => {
              // Grove loading is optional
            })
        );
      }

      if (this.pageData?.user) {
        parallel.push(
          apiFetch(`/api/v1/notifications?agentId=${this.agentId}`)
            .then(async (notifRes) => {
              if (notifRes.ok) {
                const data = (await notifRes.json()) as AgentNotificationsResponse;
                this.userNotifications = data.userNotifications ?? [];
                this.agentNotifications = data.agentNotifications ?? [];
              }
            })
            .catch(() => {
              // Notification loading is optional
            })
        );

        // Check if user has a subscription for this agent
        parallel.push(
          apiFetch(`/api/v1/notifications/subscriptions?agentId=${this.agentId}`)
            .then(async (subRes) => {
              if (subRes.ok) {
                const data = (await subRes.json()) as Subscription[] | { subscriptions?: Subscription[] };
                const subs = Array.isArray(data) ? data : (data as { subscriptions?: Subscription[] }).subscriptions || [];
                const match = subs.find((s) => s.scope === 'agent' && s.agentId === this.agentId);
                if (match) {
                  this.subscribed = true;
                  this.subscriptionId = match.id;
                }
              }
            })
            .catch(() => {
              // Subscription check is optional
            })
        );
      }

      await Promise.all(parallel);

      stateManager.seedAgents([this.agent]);
      if (this.grove) {
        stateManager.seedGroves([this.grove]);
        dispatchPageTitle(this, this.agent.name, this.grove.name || this.agent.groveId);
      } else {
        dispatchPageTitle(this, this.agent.name, 'Agents');
      }
    } catch (err) {
      console.error('Failed to load agent:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load agent';
    } finally {
      this.loading = false;
    }
  }

  private formatDate(dateString: string): string {
    try {
      if (this.isZeroDate(dateString)) return '—';
      const date = new Date(dateString);
      return new Intl.DateTimeFormat('en', {
        month: 'short',
        day: 'numeric',
        year: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
      }).format(date);
    } catch {
      return dateString;
    }
  }

  private isZeroDate(dateString: string): boolean {
    if (!dateString) return true;
    const date = new Date(dateString);
    return isNaN(date.getTime()) || date.getFullYear() < 2000;
  }

  private formatRelativeTime(dateString: string): string {
    try {
      if (this.isZeroDate(dateString)) return '—';
      const date = new Date(dateString);
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

  private async handleAction(
    action: 'start' | 'stop' | 'suspend' | 'resume' | 'delete',
    event?: MouseEvent
  ): Promise<void> {
    if (!this.agent) return;

    if (action === 'delete') {
      if (!event?.altKey && !confirm('Are you sure you want to delete this agent?')) {
        return;
      }
      this.actionLoading = { ...this.actionLoading, delete: true };

      try {
        const response = await apiFetch(`/api/v1/agents/${this.agentId}`, {
          method: 'DELETE',
        });

        if (!response.ok) {
          throw new Error(await extractApiError(response, 'Failed to delete agent'));
        }

        window.location.href = this.grove ? `/groves/${this.grove.id}` : '/agents';
      } catch (err) {
        console.error('Failed to delete agent:', err);
        alert(err instanceof Error ? err.message : 'Failed to delete agent');
      } finally {
        this.actionLoading = { ...this.actionLoading, delete: false };
      }
      return;
    }

    const optimisticPhase: Record<string, string> = {
      start: 'starting',
      stop: 'stopping',
      suspend: 'stopping',
      resume: 'starting',
    };
    this.agent = {
      ...this.agent,
      phase: optimisticPhase[action] as Agent['phase'],
    };

    const actionUrls: Record<string, string> = {
      start: `/api/v1/agents/${this.agentId}/start`,
      stop: `/api/v1/agents/${this.agentId}/stop`,
      suspend: `/api/v1/agents/${this.agentId}/suspend`,
      resume: `/api/v1/agents/${this.agentId}/start`,
    };

    try {
      const response = await apiFetch(actionUrls[action], { method: 'POST' });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `Failed to ${action} agent`));
      }

      this.backgroundRefresh();
    } catch (err) {
      console.error(`Failed to ${action} agent:`, err);
      alert(err instanceof Error ? err.message : `Failed to ${action} agent`);
      this.backgroundRefresh();
    }
  }

  private async toggleSubscription(): Promise<void> {
    if (!this.agent) return;
    this.subscriptionLoading = true;

    try {
      if (this.subscribed && this.subscriptionId) {
        const res = await apiFetch(
          `/api/v1/notifications/subscriptions/${encodeURIComponent(this.subscriptionId)}`,
          { method: 'DELETE' }
        );
        if (res.ok || res.status === 204) {
          this.subscribed = false;
          this.subscriptionId = null;
        }
      } else {
        const res = await apiFetch('/api/v1/notifications/subscriptions', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            scope: 'agent',
            agentId: this.agentId,
            groveId: this.agent.groveId,
            triggerActivities: ['COMPLETED', 'WAITING_FOR_INPUT', 'LIMITS_EXCEEDED'],
          }),
        });
        if (res.ok) {
          const sub = (await res.json()) as Subscription;
          this.subscribed = true;
          this.subscriptionId = sub.id;
        }
      }
    } catch (err) {
      console.error('Failed to toggle subscription:', err);
    } finally {
      this.subscriptionLoading = false;
    }
  }

  private backgroundRefresh(): void {
    this.fetchAndMergeAgent().catch((err) => {
      console.warn('Background refresh failed:', err);
    });
  }

  private async fetchAndMergeAgent(): Promise<void> {
    const agentResponse = await apiFetch(`/api/v1/agents/${this.agentId}`);
    if (!agentResponse.ok) return;

    this.agent = (await agentResponse.json()) as Agent;
    stateManager.seedAgents([this.agent]);
  }

  private handleTabShow(e: CustomEvent<{ name: string }>): void {
    if (e.detail.name === 'logs') {
      const viewer = this.shadowRoot?.querySelector(
        'scion-agent-log-viewer'
      ) as ScionAgentLogViewer | null;
      viewer?.loadLogs();
    }
    if (e.detail.name === 'messages') {
      const viewer = this.shadowRoot?.querySelector(
        'scion-agent-message-viewer'
      ) as ScionAgentMessageViewer | null;
      viewer?.loadMessages();
    }
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    if (this.loading) {
      return this.renderLoading();
    }

    if (this.error || !this.agent) {
      return this.renderError();
    }

    return html`
      <a href="${this.grove ? `/groves/${this.grove.id}` : '/agents'}" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        ${this.grove ? `To ${this.grove.name}` : 'Back to Agents'}
      </a>

      ${this.renderHeader()}
      ${this.agent.phase === 'error' && (this.agent.detail?.message || this.agent.message)
        ? html`
            <div class="agent-error-banner">
              <sl-icon name="exclamation-octagon"></sl-icon>
              <div class="error-content">
                <div class="error-title">Agent Error</div>
                <div class="error-message">${this.agent.detail?.message || this.agent.message}</div>
              </div>
            </div>
          `
        : ''}

      <sl-tab-group @sl-tab-show=${this.handleTabShow}>
        <sl-tab slot="nav" panel="status">Status</sl-tab>
        ${this.agent.cloudLogging ? html`<sl-tab slot="nav" panel="logs">Logs</sl-tab>` : nothing}
        <sl-tab slot="nav" panel="messages">Messages</sl-tab>
        <sl-tab slot="nav" panel="configuration">Configuration</sl-tab>

        <sl-tab-panel name="status">${this.renderStatusTab()}</sl-tab-panel>
        ${this.agent.cloudLogging
          ? html`
              <sl-tab-panel name="logs">
                <scion-agent-log-viewer
                  agentId=${this.agentId}
                  .brokers=${this.agent.runtimeBrokerId
                    ? {
                        [this.agent.runtimeBrokerId]:
                          this.agent.runtimeBrokerName || this.agent.runtimeBrokerId,
                      }
                    : {}}
                ></scion-agent-log-viewer>
              </sl-tab-panel>
            `
          : nothing}
        <sl-tab-panel name="messages">
          <scion-agent-message-viewer
            agentId=${this.agentId}
            agentName=${this.agent.name || ''}
            ?canSend=${can(this.agent._capabilities, 'message')}
            ?cloudLogging=${this.agent.cloudLogging || false}
          ></scion-agent-message-viewer>
        </sl-tab-panel>
        <sl-tab-panel name="configuration">${this.renderConfigurationTab()}</sl-tab-panel>
      </sl-tab-group>
    `;
  }

  // ---------------------------------------------------------------------------
  // Header
  // ---------------------------------------------------------------------------

  private renderHeader() {
    const agent = this.agent!;
    return html`
      <div class="header">
        <div class="header-info">
          <div class="header-title">
            <sl-icon name="cpu"></sl-icon>
            <h1>${agent.name}</h1>
            <scion-status-badge
              status=${getAgentDisplayStatus(agent) as StatusType}
              label=${getAgentDisplayStatus(agent)}
            ></scion-status-badge>
          </div>
          <div class="header-meta">
            <span class="template-badge">
              <sl-icon name="code-square"></sl-icon>
              ${agent.template}
            </span>
            ${this.grove
              ? html`
                  <a href="/groves/${this.grove.id}" class="grove-link">
                    <sl-icon name="folder"></sl-icon>
                    ${this.grove.name}
                  </a>
                `
              : ''}
            ${agent.runtimeBrokerId
              ? html`
                  <a href="/brokers/${agent.runtimeBrokerId}" class="broker-link">
                    <sl-icon name="hdd-rack"></sl-icon>
                    ${agent.runtimeBrokerName || agent.runtimeBrokerId}
                  </a>
                `
              : ''}
          </div>
        </div>
        <div class="header-actions">
          ${can(agent._capabilities, 'attach')
            ? html`
                <a href="/agents/${this.agentId}/terminal" style="text-decoration: none;">
                  <sl-button
                    variant="primary"
                    size="small"
                    ?disabled=${!isTerminalAvailable(agent)}
                  >
                    <sl-icon slot="prefix" name="terminal"></sl-icon>
                    Terminal
                  </sl-button>
                </a>
              `
            : nothing}
          ${isAgentRunning(agent)
            ? html`
                ${can(agent._capabilities, 'stop')
                  ? html`
                      ${agent.harnessCapabilities?.resume?.support !== 'no'
                        ? html`
                            <sl-button
                              variant="warning"
                              size="small"
                              outline
                              ?loading=${this.actionLoading['suspend']}
                              ?disabled=${this.actionLoading['suspend']}
                              @click=${() => this.handleAction('suspend')}
                            >
                              <sl-icon slot="prefix" name="pause-circle"></sl-icon>
                              Suspend
                            </sl-button>
                          `
                        : nothing}
                      <sl-button
                        variant="danger"
                        size="small"
                        outline
                        ?loading=${this.actionLoading['stop']}
                        ?disabled=${this.actionLoading['stop']}
                        @click=${() => this.handleAction('stop')}
                      >
                        <sl-icon slot="prefix" name="stop-circle"></sl-icon>
                        Stop
                      </sl-button>
                    `
                  : nothing}
              `
            : agent.phase === 'suspended'
              ? can(agent._capabilities, 'start')
                ? html`
                    <sl-button
                      variant="success"
                      size="small"
                      ?loading=${this.actionLoading['resume']}
                      ?disabled=${this.actionLoading['resume']}
                      @click=${() => this.handleAction('resume')}
                    >
                      <sl-icon slot="prefix" name="play-circle"></sl-icon>
                      Resume
                    </sl-button>
                  `
                : nothing
              : can(agent._capabilities, 'start')
                ? html`
                    <sl-button
                      variant="success"
                      size="small"
                      ?loading=${this.actionLoading['start']}
                      ?disabled=${this.actionLoading['start']}
                      @click=${() => this.handleAction('start')}
                    >
                      <sl-icon slot="prefix" name="play-circle"></sl-icon>
                      Start
                    </sl-button>
                  `
                : nothing}
          ${agent.phase === 'created'
            ? html`
                <a href="/agents/${this.agentId}/configure" style="text-decoration: none;">
                  <sl-button variant="default" size="small">
                    <sl-icon slot="prefix" name="sliders"></sl-icon>
                    Configure
                  </sl-button>
                </a>
              `
            : nothing}
          ${can(agent._capabilities, 'delete')
            ? html`
                <sl-button
                  variant="danger"
                  size="small"
                  ?loading=${this.actionLoading['delete']}
                  ?disabled=${this.actionLoading['delete']}
                  @click=${(e: MouseEvent) => this.handleAction('delete', e)}
                >
                  <sl-icon slot="prefix" name="trash"></sl-icon>
                </sl-button>
              `
            : nothing}
        </div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Status Tab
  // ---------------------------------------------------------------------------

  private renderStatusTab() {
    const agent = this.agent!;
    return html`
      ${this.renderCurrentStateCard(agent)} ${this.renderCurrentTaskCard(agent)}
      ${this.renderLimitsUsageCard(agent)} ${this.renderConnectivityCard(agent)}
      ${this.renderNotificationsCard()}
    `;
  }

  private renderCurrentStateCard(agent: Agent) {
    return html`
      <div class="card">
        <div class="card-title-row">
          <h3 class="card-title">Current State</h3>
          ${this.pageData?.user
            ? html`
                <sl-tooltip content=${this.subscribed ? 'Unsubscribe from notifications' : 'Subscribe to notifications'}>
                  <sl-button
                    size="small"
                    variant=${this.subscribed ? 'primary' : 'default'}
                    outline
                    @click=${() => void this.toggleSubscription()}
                    ?loading=${this.subscriptionLoading}
                    aria-label=${this.subscribed ? 'Unsubscribe' : 'Subscribe'}
                  >
                    <sl-icon slot="prefix" name=${this.subscribed ? 'bell-fill' : 'bell'}></sl-icon>
                    ${this.subscribed ? 'Subscribed' : 'Subscribe'}
                  </sl-button>
                </sl-tooltip>
              `
            : nothing}
        </div>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Phase</span>
            <span class="info-value">
              <scion-status-badge
                status=${agent.phase as StatusType}
                label=${agent.phase}
                size="small"
              ></scion-status-badge>
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Activity</span>
            <span class="info-value">
              ${agent.activity
                ? html`<scion-status-badge
                    status=${agent.activity as StatusType}
                    label=${agent.activity}
                    size="small"
                  ></scion-status-badge>`
                : html`<span style="color: var(--scion-text-muted, #64748b);">—</span>`}
            </span>
          </div>
          ${agent.detail?.toolName
            ? html`
                <div class="info-item">
                  <span class="info-label">Tool</span>
                  <span class="info-value mono">${agent.detail.toolName}</span>
                </div>
              `
            : ''}
          ${agent.detail?.message && agent.phase !== 'error'
            ? html`
                <div class="info-item">
                  <span class="info-label">Detail</span>
                  <span class="info-value">${agent.detail.message}</span>
                </div>
              `
            : ''}
        </div>
      </div>
    `;
  }

  private renderCurrentTaskCard(agent: Agent) {
    const task = agent.detail?.taskSummary || agent.taskSummary;
    if (!task) return nothing;
    return html`
      <div class="card">
        <h3 class="card-title">Current Task</h3>
        <div class="task-summary">${task}</div>
      </div>
    `;
  }

  private renderLimitsUsageCard(agent: Agent) {
    const cfg = agent.appliedConfig?.inlineConfig;
    const maxTurns = cfg?.max_turns ?? 0;
    const maxModelCalls = cfg?.max_model_calls ?? 0;
    const maxDurationStr = cfg?.max_duration ?? '';
    const maxDurationSec = parseDuration(maxDurationStr);

    if (!maxTurns && !maxModelCalls && !maxDurationSec) return nothing;

    const currentTurns = agent.currentTurns ?? 0;
    const currentModelCalls = agent.currentModelCalls ?? 0;

    let timeRemainingSec = 0;
    let elapsedSec = 0;
    if (maxDurationSec > 0 && agent.startedAt) {
      elapsedSec = (Date.now() - new Date(agent.startedAt).getTime()) / 1000;
      timeRemainingSec = Math.max(0, maxDurationSec - elapsedSec);
    }

    return html`
      <div class="card">
        <h3 class="card-title">Limits & Usage</h3>
        <div class="limits-grid">
          ${maxTurns > 0 ? this.renderLimitItem('Turns', currentTurns, maxTurns) : nothing}
          ${maxModelCalls > 0
            ? this.renderLimitItem('Model Calls', currentModelCalls, maxModelCalls)
            : nothing}
          ${maxDurationSec > 0
            ? this.renderTimeLimitItem(timeRemainingSec, elapsedSec, maxDurationSec)
            : nothing}
        </div>
      </div>
    `;
  }

  private renderLimitItem(label: string, current: number, max: number) {
    const pct = Math.min(100, (current / max) * 100);
    const colorClass = pct > 90 ? 'danger' : pct > 75 ? 'warning' : 'normal';
    return html`
      <div class="limit-item">
        <span class="limit-label">${label}</span>
        <span class="limit-value">${current} / ${max}</span>
        <div class="progress-bar-track">
          <div class="progress-bar-fill ${colorClass}" style="width: ${pct}%"></div>
        </div>
      </div>
    `;
  }

  private renderTimeLimitItem(remainingSec: number, elapsedSec: number, totalSec: number) {
    const pct = Math.min(100, (elapsedSec / totalSec) * 100);
    const remainPct = 100 - pct;
    const colorClass = remainPct < 10 ? 'danger' : remainPct < 25 ? 'warning' : 'normal';
    return html`
      <div class="limit-item">
        <span class="limit-label">Time Remaining</span>
        <span class="limit-value">${formatDurationHMS(remainingSec)}</span>
        <div class="progress-bar-track">
          <div class="progress-bar-fill ${colorClass}" style="width: ${pct}%"></div>
        </div>
      </div>
    `;
  }

  private renderConnectivityCard(agent: Agent) {
    const candidates = [
      agent.lastSeen,
      agent.updated,
      agent.updatedAt,
      agent.created,
      agent.createdAt,
    ];
    const lastSeenStr = candidates.find((d) => d && !this.isZeroDate(d)) || '';
    return html`
      <div class="card">
        <h3 class="card-title">Connectivity</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Last Seen</span>
            <span class="info-value" title="${this.formatDate(lastSeenStr)}">
              ${this.formatRelativeTime(lastSeenStr)}
            </span>
          </div>
          ${agent.connectionState
            ? html`
                <div class="info-item">
                  <span class="info-label">Connection State</span>
                  <span class="info-value">
                    <scion-status-badge
                      status=${agent.connectionState === 'connected'
                        ? ('success' as StatusType)
                        : ('danger' as StatusType)}
                      label=${agent.connectionState}
                      size="small"
                    ></scion-status-badge>
                  </span>
                </div>
              `
            : ''}
        </div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Configuration Tab
  // ---------------------------------------------------------------------------

  private renderConfigurationTab() {
    const agent = this.agent!;
    const cfg = agent.appliedConfig;
    const inline = cfg?.inlineConfig;

    return html`
      ${this.renderIdentityCard(agent, cfg?.gcpIdentity)} ${this.renderHarnessModelCard(agent, cfg, inline)}
      ${this.renderRuntimeCard(agent, inline)} ${this.renderGCPIdentityCard(cfg?.gcpIdentity)}
      ${this.renderConfigLimitsCard(inline)} ${this.renderTelemetryCard(inline?.telemetry)}
      ${this.renderInitialTaskCard(cfg)}
    `;
  }

  private renderIdentityCard(agent: Agent, gcpIdentity?: GCPIdentityConfig) {
    return html`
      <div class="card">
        <h3 class="card-title">Identity</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Agent ID</span>
            <span class="info-value mono">${agent.id}</span>
          </div>
          ${agent.slug
            ? html`
                <div class="info-item">
                  <span class="info-label">Slug</span>
                  <span class="info-value mono">${agent.slug}</span>
                </div>
              `
            : ''}
          <div class="info-item">
            <span class="info-label">Created</span>
            <span class="info-value"
              >${this.formatDate(agent.created || agent.createdAt || '')}</span
            >
          </div>
          ${agent.appliedConfig?.creatorName
            ? html`
                <div class="info-item">
                  <span class="info-label">Created By</span>
                  <span class="info-value">${agent.appliedConfig.creatorName}</span>
                </div>
              `
            : ''}
          ${agent.harnessAuth || agent.appliedConfig?.harnessAuth
            ? html`
                <div class="info-item">
                  <span class="info-label">Auth Method</span>
                  <span class="info-value">${agent.harnessAuth || agent.appliedConfig?.harnessAuth}</span>
                </div>
              `
            : ''}
          ${agent.visibility
            ? html`
                <div class="info-item">
                  <span class="info-label">Visibility</span>
                  <span class="info-value">
                    <span class="visibility-badge">${agent.visibility}</span>
                  </span>
                </div>
              `
            : ''}
          ${gcpIdentity?.metadataMode === 'assign' && gcpIdentity.serviceAccountEmail
            ? html`
                <div class="info-item">
                  <span class="info-label">GCP Service Account</span>
                  <span class="info-value mono">${gcpIdentity.serviceAccountEmail}</span>
                </div>
              `
            : ''}
          ${gcpIdentity?.metadataMode === 'assign' && gcpIdentity.projectId
            ? html`
                <div class="info-item">
                  <span class="info-label">GCP Project</span>
                  <span class="info-value mono">${gcpIdentity.projectId}</span>
                </div>
              `
            : ''}
        </div>
      </div>
    `;
  }

  private renderHarnessModelCard(
    agent: Agent,
    cfg: AgentAppliedConfig | undefined,
    inline: AgentInlineConfig | undefined
  ) {
    const harness = agent.harnessConfig || cfg?.harnessConfig;
    const auth = agent.harnessAuth || cfg?.harnessAuth;
    const model = inline?.model || cfg?.model;

    return html`
      <div class="card">
        <h3 class="card-title">Harness & Model</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Template</span>
            <span class="info-value">${agent.template}</span>
          </div>
          ${cfg?.templateId
            ? html`
                <div class="info-item">
                  <span class="info-label">Template ID</span>
                  <span class="info-value mono">${cfg.templateId}</span>
                </div>
              `
            : ''}
          ${cfg?.templateHash
            ? html`
                <div class="info-item">
                  <span class="info-label">Template Hash</span>
                  <span class="info-value mono">
                    <scion-hash-display .hash=${cfg.templateHash} max-width="14ch"></scion-hash-display>
                  </span>
                </div>
              `
            : ''}
          ${harness
            ? html`
                <div class="info-item">
                  <span class="info-label">Harness Config</span>
                  <span class="info-value">${harness}</span>
                </div>
              `
            : ''}
          ${auth
            ? html`
                <div class="info-item">
                  <span class="info-label">Auth Method</span>
                  <span class="info-value">${auth}</span>
                </div>
              `
            : ''}
          ${model
            ? html`
                <div class="info-item">
                  <span class="info-label">Model</span>
                  <span class="info-value mono">${model}</span>
                </div>
              `
            : ''}
        </div>
      </div>
    `;
  }

  private renderRuntimeCard(agent: Agent, inline: AgentInlineConfig | undefined) {
    const image = agent.image || inline?.image || agent.appliedConfig?.image;
    const branch = inline?.branch;
    const profile = agent.appliedConfig?.profile;

    return html`
      <div class="card">
        <h3 class="card-title">Runtime Environment</h3>
        <div class="info-grid">
          ${agent.runtimeBrokerId
            ? html`
                <div class="info-item">
                  <span class="info-label">Runtime Broker</span>
                  <span class="info-value">
                    <a
                      href="/brokers/${agent.runtimeBrokerId}"
                      class="broker-link"
                      style="font-size: 1rem;"
                    >
                      ${agent.runtimeBrokerName || agent.runtimeBrokerId}
                    </a>
                  </span>
                </div>
              `
            : ''}
          ${agent.runtime
            ? html`
                <div class="info-item">
                  <span class="info-label">Runtime Type</span>
                  <span class="info-value">${agent.runtime}</span>
                </div>
              `
            : ''}
          ${profile
            ? html`
                <div class="info-item">
                  <span class="info-label">Profile</span>
                  <span class="info-value">${profile}</span>
                </div>
              `
            : ''}
          ${image
            ? html`
                <div class="info-item">
                  <span class="info-label">Image</span>
                  <span class="info-value mono">${image}</span>
                </div>
              `
            : ''}
          ${branch
            ? html`
                <div class="info-item">
                  <span class="info-label">Branch</span>
                  <span class="info-value mono">${branch}</span>
                </div>
              `
            : ''}
        </div>
      </div>
    `;
  }

  private renderGCPIdentityCard(gcpIdentity: GCPIdentityConfig | undefined) {
    if (!gcpIdentity) return nothing;

    const modeVariant =
      gcpIdentity.metadataMode === 'assign'
        ? 'primary'
        : gcpIdentity.metadataMode === 'passthrough'
          ? 'warning'
          : 'neutral';

    return html`
      <div class="card">
        <h3 class="card-title">GCP Identity</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Metadata Mode</span>
            <span class="info-value">
              <sl-badge variant=${modeVariant}>${gcpIdentity.metadataMode}</sl-badge>
            </span>
          </div>
        </div>
      </div>
    `;
  }

  private renderConfigLimitsCard(inline: AgentInlineConfig | undefined) {
    const maxTurns = inline?.max_turns ?? 0;
    const maxModelCalls = inline?.max_model_calls ?? 0;
    const maxDuration = inline?.max_duration ?? '';

    if (!maxTurns && !maxModelCalls && !maxDuration) return nothing;

    return html`
      <div class="card">
        <h3 class="card-title">Limits</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Max Turns</span>
            <span class="info-value">${maxTurns || 'None'}</span>
          </div>
          <div class="info-item">
            <span class="info-label">Max Model Calls</span>
            <span class="info-value">${maxModelCalls || 'None'}</span>
          </div>
          <div class="info-item">
            <span class="info-label">Max Duration</span>
            <span class="info-value">${maxDuration || 'None'}</span>
          </div>
        </div>
      </div>
    `;
  }

  private renderTelemetryCard(telemetry: TelemetryConfig | undefined) {
    if (!telemetry) return nothing;

    const enabledLabel = telemetry.enabled === false ? 'Disabled' : 'Enabled';
    const filter = telemetry.filter;
    const cloud = telemetry.cloud;
    const hub = telemetry.hub;
    const local = telemetry.local;

    const hasDestinations = cloud || hub || local;
    const hasFilter = filter?.events || filter?.attributes || filter?.sampling;

    return html`
      <div class="card">
        <h3 class="card-title">Telemetry</h3>
        <div
          class="info-grid"
          style="margin-bottom: ${hasDestinations || hasFilter ? '1.25rem' : '0'}"
        >
          <div class="info-item">
            <span class="info-label">Status</span>
            <span class="info-value">${enabledLabel}</span>
          </div>
        </div>

        ${hasDestinations
          ? html`
              <div class="telemetry-section">
                <div class="telemetry-section-title">Export Destinations</div>
                <div class="info-grid">
                  ${cloud
                    ? html`
                        <div class="info-item">
                          <span class="info-label">Cloud</span>
                          <span class="info-value"
                            >${cloud.enabled === false ? 'Disabled' : 'Enabled'}${cloud.provider
                              ? ` (${cloud.provider})`
                              : ''}${cloud.endpoint
                              ? html`<br /><span class="mono" style="font-size: 0.8rem"
                                    >${cloud.endpoint}</span
                                  >`
                              : ''}</span
                          >
                        </div>
                      `
                    : ''}
                  ${hub
                    ? html`
                        <div class="info-item">
                          <span class="info-label">Hub</span>
                          <span class="info-value"
                            >${hub.enabled === false ? 'Disabled' : 'Enabled'}${hub.report_interval
                              ? ` (${hub.report_interval})`
                              : ''}</span
                          >
                        </div>
                      `
                    : ''}
                  ${local
                    ? html`
                        <div class="info-item">
                          <span class="info-label">Local</span>
                          <span class="info-value"
                            >${local.enabled === false ? 'Disabled' : 'Enabled'}${local.file
                              ? html`<br /><span class="mono" style="font-size: 0.8rem"
                                    >${local.file}</span
                                  >`
                              : ''}</span
                          >
                        </div>
                      `
                    : ''}
                </div>
              </div>
            `
          : ''}
        ${hasFilter
          ? html`
              <div class="telemetry-section">
                <div class="telemetry-section-title">Filters</div>
                ${filter?.events?.exclude?.length
                  ? html`
                      <div class="info-item" style="margin-bottom: 0.75rem">
                        <span class="info-label">Excluded Events</span>
                        <div class="tag-list">
                          ${filter.events.exclude.map(
                            (e) => html`<span class="tag-item">${e}</span>`
                          )}
                        </div>
                      </div>
                    `
                  : ''}
                ${filter?.events?.include?.length
                  ? html`
                      <div class="info-item" style="margin-bottom: 0.75rem">
                        <span class="info-label">Included Events</span>
                        <div class="tag-list">
                          ${filter.events.include.map(
                            (e) => html`<span class="tag-item">${e}</span>`
                          )}
                        </div>
                      </div>
                    `
                  : ''}
                ${filter?.attributes?.redact?.length
                  ? html`
                      <div class="info-item" style="margin-bottom: 0.75rem">
                        <span class="info-label">Redacted Attributes</span>
                        <div class="tag-list">
                          ${filter.attributes.redact.map(
                            (a) => html`<span class="tag-item">${a}</span>`
                          )}
                        </div>
                      </div>
                    `
                  : ''}
                ${filter?.attributes?.hash?.length
                  ? html`
                      <div class="info-item" style="margin-bottom: 0.75rem">
                        <span class="info-label">Hashed Attributes</span>
                        <div class="tag-list">
                          ${filter.attributes.hash.map(
                            (a) => html`<span class="tag-item">${a}</span>`
                          )}
                        </div>
                      </div>
                    `
                  : ''}
                ${filter?.sampling
                  ? html`
                      <div class="info-item">
                        <span class="info-label">Sampling</span>
                        <span class="info-value"
                          >${filter.sampling.default != null
                            ? `Default: ${(filter.sampling.default * 100).toFixed(0)}%`
                            : ''}${filter.sampling.rates
                            ? Object.entries(filter.sampling.rates)
                                .map(([k, v]) => `${k}: ${(v * 100).toFixed(0)}%`)
                                .join(', ')
                            : ''}</span
                        >
                      </div>
                    `
                  : ''}
              </div>
            `
          : ''}
        ${!hasDestinations && !hasFilter
          ? html`<div class="telemetry-empty">No detailed configuration</div>`
          : ''}
      </div>
    `;
  }

  private renderInitialTaskCard(cfg: AgentAppliedConfig | undefined) {
    const task = cfg?.task;
    if (!task) return nothing;

    return html`
      <div class="card">
        <h3 class="card-title">Initial Task</h3>
        <div class="task-summary">${task}</div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Notifications card
  // ---------------------------------------------------------------------------

  private renderNotificationsCard() {
    const hasUser = this.userNotifications.length > 0;
    const hasAgent = this.agentNotifications.length > 0;

    return html`
      <div class="card">
        <h3 class="card-title">Notifications</h3>
        ${!hasUser && !hasAgent
          ? html`<div class="notif-empty">
              <sl-icon name="bell-slash"></sl-icon>
              <span>No notifications for this agent</span>
            </div>`
          : html`
              ${hasUser
                ? html`
                    ${hasAgent
                      ? html`<div class="notif-section-title">Your Notifications</div>`
                      : nothing}
                    ${this.userNotifications.map((n) => this.renderNotifItem(n, true))}
                  `
                : nothing}
              ${hasAgent
                ? html`
                    ${hasUser
                      ? html`<div class="notif-section-title">Agent Notifications</div>`
                      : nothing}
                    ${this.agentNotifications.map((n) => this.renderNotifItem(n, false))}
                  `
                : nothing}
            `}
      </div>
    `;
  }

  private renderNotifItem(n: Notification, canAck: boolean) {
    return html`
      <div class="notif-list-item">
        <div class="notif-icon ${this.notifStatusClass(n.status)}">
          <sl-icon name=${this.notifStatusIcon(n.status)}></sl-icon>
        </div>
        <div class="notif-body">
          <div class="notif-message">${n.message}</div>
          <sl-tooltip content=${n.message} hoist>
            <span class="notif-truncation-badge" style="display:none">...</span>
          </sl-tooltip>
          <div class="notif-meta">
            <span>${this.formatRelativeTime(n.createdAt)}</span>
            ${canAck && !n.acknowledged
              ? html`<button class="notif-mark-read" @click=${() => this.ackNotification(n.id)}>
                  Mark read
                </button>`
              : nothing}
          </div>
        </div>
      </div>
    `;
  }

  private notifStatusIcon(status: string): string {
    switch (status) {
      case 'COMPLETED':
        return 'check-circle-fill';
      case 'WAITING_FOR_INPUT':
        return 'exclamation-circle-fill';
      case 'LIMITS_EXCEEDED':
        return 'x-circle-fill';
      default:
        return 'info-circle-fill';
    }
  }

  private notifStatusClass(status: string): string {
    switch (status) {
      case 'COMPLETED':
        return 'status-success';
      case 'WAITING_FOR_INPUT':
        return 'status-warning';
      case 'LIMITS_EXCEEDED':
        return 'status-danger';
      default:
        return 'status-info';
    }
  }

  private async ackNotification(id: string): Promise<void> {
    try {
      await apiFetch(`/api/v1/notifications/${id}/ack`, { method: 'POST' });
      this.userNotifications = this.userNotifications.filter((n) => n.id !== id);
    } catch {
      // Ignore
    }
  }

  override updated(changed: Map<string, unknown>): void {
    super.updated(changed);
    this.detectNotifTruncation();
  }

  private detectNotifTruncation(): void {
    const messages = this.shadowRoot?.querySelectorAll('.notif-message');
    if (!messages) return;
    messages.forEach((el) => {
      const badge = el.parentElement?.querySelector(
        '.notif-truncation-badge'
      ) as HTMLElement | null;
      if (!badge) return;
      badge.style.display = el.scrollHeight > el.clientHeight ? 'inline-flex' : 'none';
    });
  }

  // ---------------------------------------------------------------------------
  // Loading / Error states
  // ---------------------------------------------------------------------------

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading agent...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <a href="${this.grove ? `/groves/${this.grove.id}` : '/agents'}" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        ${this.grove ? `To ${this.grove.name}` : 'Back to Agents'}
      </a>

      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Agent</h2>
        <p>There was a problem loading this agent.</p>
        <div class="error-details">${this.error || 'Agent not found'}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agent-detail': ScionPageAgentDetail;
  }
}
