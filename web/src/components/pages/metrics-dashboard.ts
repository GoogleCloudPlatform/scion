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

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { Chart, registerables } from 'chart.js';

import { apiFetch, extractApiError } from '../../client/api.js';

Chart.register(...registerables);

interface TimeSeriesPoint {
  timestamp: string;
  value: number;
}

interface LabeledTimeSeries {
  label: string;
  points: TimeSeriesPoint[];
}

interface SummaryData {
  periodDays: number;
  totalSessions: number;
  totalApiCalls: number;
  totalTokens: number;
  uniqueAgents: number;
}

interface SessionsData {
  periodDays: number;
  dailyCounts: TimeSeriesPoint[];
  activeAgents: TimeSeriesPoint[];
}

interface ModelCallsData {
  periodDays: number;
  byModel: LabeledTimeSeries[];
  byHarness: LabeledTimeSeries[];
}

interface TokensData {
  periodDays: number;
  input: LabeledTimeSeries[];
  output: LabeledTimeSeries[];
}

const CHART_COLORS = [
  '#3b82f6', '#10b981', '#f59e0b', '#ef4444', '#8b5cf6',
  '#ec4899', '#06b6d4', '#84cc16', '#f97316', '#6366f1',
];

@customElement('scion-page-metrics')
export class ScionPageMetrics extends LitElement {
  @property({ type: String })
  projectId = '';

  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private activeTab = 'summary';
  @state() private periodDays = 7;

  @state() private summary: SummaryData | null = null;
  @state() private sessions: SessionsData | null = null;
  @state() private modelCalls: ModelCallsData | null = null;
  @state() private tokens: TokensData | null = null;

  private charts: Map<string, Chart> = new Map();

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1.5rem;
    }

    .header-left {
      display: flex;
      align-items: center;
      gap: 0.75rem;
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

    .period-selector {
      display: flex;
      gap: 0.5rem;
    }

    .period-btn {
      padding: 0.375rem 0.75rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
      font-weight: 500;
      cursor: pointer;
      transition: all 0.15s;
    }

    .period-btn:hover {
      border-color: var(--scion-primary, #3b82f6);
      color: var(--scion-primary, #3b82f6);
    }

    .period-btn.active {
      background: var(--scion-primary, #3b82f6);
      border-color: var(--scion-primary, #3b82f6);
      color: #ffffff;
    }

    sl-tab-group {
      --indicator-color: var(--scion-primary, #3b82f6);
    }

    sl-tab-group::part(base) {
      background: transparent;
    }

    sl-tab::part(base) {
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .stats-row {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
      gap: 1rem;
      margin-bottom: 1.5rem;
    }

    .stat-card {
      display: flex;
      flex-direction: column;
      padding: 1.25rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .stat-label {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.375rem;
    }

    .stat-value {
      font-size: 1.75rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
    }

    .chart-container {
      position: relative;
      width: 100%;
      height: 320px;
    }

    .chart-row {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1.5rem;
    }

    @media (max-width: 900px) {
      .chart-row {
        grid-template-columns: 1fr;
      }
    }

    .chart-section-title {
      font-size: 0.9375rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 1rem 0;
    }

    .empty-state {
      text-align: center;
      padding: 3rem 1rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty-state sl-icon {
      font-size: 2.5rem;
      margin-bottom: 1rem;
      display: block;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    // Parse projectId from URL if not set as property (same pattern as project-detail.ts)
    if (!this.projectId && typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/projects\/([^/]+)\/metrics/);
      if (match) this.projectId = match[1];
    }
    void this.loadView('summary');
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.destroyAllCharts();
  }

  private destroyAllCharts(): void {
    for (const chart of this.charts.values()) {
      chart.destroy();
    }
    this.charts.clear();
  }

  private async loadView(view: string): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const basePath = this.projectId
        ? `/api/v1/projects/${this.projectId}/metrics`
        : `/api/v1/metrics/`;
      const response = await apiFetch(
        `${basePath}?view=${view}&period=${this.periodDays}`
      );

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      const data = await response.json();

      switch (view) {
        case 'summary':
          this.summary = data as SummaryData;
          break;
        case 'sessions':
          this.sessions = data as SessionsData;
          break;
        case 'model-calls':
          this.modelCalls = data as ModelCallsData;
          break;
        case 'tokens':
          this.tokens = data as TokensData;
          break;
      }
    } catch (err) {
      this.error = err instanceof Error ? err.message : String(err);
    } finally {
      this.loading = false;
    }
  }

  private handleTabChange(e: CustomEvent): void {
    const name = (e.detail as { name: string }).name;
    this.activeTab = name;
    this.destroyAllCharts();

    const viewMap: Record<string, string> = {
      summary: 'summary',
      sessions: 'sessions',
      'model-calls': 'model-calls',
      tokens: 'tokens',
    };

    void this.loadView(viewMap[name] ?? 'summary');
  }

  private handlePeriodChange(days: number): void {
    this.periodDays = days;
    this.destroyAllCharts();
    void this.loadView(this.activeTab === 'model-calls' ? 'model-calls' : this.activeTab);
  }

  private formatNumber(n: number): string {
    if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
    if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
    return n.toLocaleString();
  }

  private static readonly CHART_PROPERTIES = new Set([
    'sessions', 'modelCalls', 'tokens', 'activeTab', 'loading',
  ]);

  override updated(changedProperties: Map<string, unknown>): void {
    super.updated(changedProperties);
    for (const key of changedProperties.keys()) {
      if (ScionPageMetrics.CHART_PROPERTIES.has(key as string)) {
        this.updateCharts();
        break;
      }
    }
  }

  private updateCharts(): void {
    switch (this.activeTab) {
      case 'sessions':
        if (this.sessions) {
          if (this.sessions.dailyCounts?.length) {
            const sorted = this.sortPointsByDate(this.sessions.dailyCounts);
            this.renderChart('chart-sessions', 'bar', sorted.map(p => p.timestamp), [
              {
                label: 'Sessions',
                data: sorted.map(p => p.value),
                backgroundColor: CHART_COLORS[0],
              },
            ]);
          }
          if (this.sessions.activeAgents?.length) {
            const sorted = this.sortPointsByDate(this.sessions.activeAgents);
            this.renderChart('chart-agents', 'line', sorted.map(p => p.timestamp), [
              {
                label: 'Active Agents',
                data: sorted.map(p => p.value),
                borderColor: CHART_COLORS[1],
                backgroundColor: CHART_COLORS[1] + '33',
              },
            ]);
          }
        }
        break;
      case 'model-calls':
        if (this.modelCalls) {
          if (this.modelCalls.byModel?.length) {
            const allDates = this.collectDates(this.modelCalls.byModel);
            this.renderChart(
              'chart-model-calls',
              'bar',
              allDates,
              this.modelCalls.byModel.map((series, i) => ({
                label: series.label,
                data: this.alignToDateAxis(series.points, allDates),
                backgroundColor: CHART_COLORS[i % CHART_COLORS.length],
              }))
            );
          }
          if (this.modelCalls.byHarness?.length) {
            const allDates = this.collectDates(this.modelCalls.byHarness);
            this.renderChart(
              'chart-harness-calls',
              'bar',
              allDates,
              this.modelCalls.byHarness.map((series, i) => ({
                label: series.label,
                data: this.alignToDateAxis(series.points, allDates),
                backgroundColor: CHART_COLORS[(i + 3) % CHART_COLORS.length],
              }))
            );
          }
        }
        break;
      case 'tokens':
        if (this.tokens) {
          if (this.tokens.input?.length) {
            const allDates = this.collectDates(this.tokens.input);
            this.renderChart(
              'chart-tokens-input',
              'bar',
              allDates,
              this.tokens.input.map((series, i) => ({
                label: series.label,
                data: this.alignToDateAxis(series.points, allDates),
                backgroundColor: CHART_COLORS[i % CHART_COLORS.length],
              }))
            );
          }
          if (this.tokens.output?.length) {
            const allDates = this.collectDates(this.tokens.output);
            this.renderChart(
              'chart-tokens-output',
              'bar',
              allDates,
              this.tokens.output.map((series, i) => ({
                label: series.label,
                data: this.alignToDateAxis(series.points, allDates),
                backgroundColor: CHART_COLORS[(i + 3) % CHART_COLORS.length],
              }))
            );
          }
        }
        break;
    }
  }

  private renderChart(
    canvasId: string,
    type: 'bar' | 'line',
    labels: string[],
    datasets: { label: string; data: number[]; backgroundColor?: string; borderColor?: string }[]
  ): void {
    requestAnimationFrame(() => {
      const canvas = this.shadowRoot?.getElementById(canvasId) as HTMLCanvasElement | null;
      if (!canvas) return;

      const existing = this.charts.get(canvasId);
      if (existing) {
        if ((existing.config as { type?: string }).type === type) {
          existing.data.labels = labels;
          existing.data.datasets = datasets;
          existing.update();
          return;
        }
        existing.destroy();
      }

      const chart = new Chart(canvas, {
        type,
        data: { labels, datasets },
        options: {
          responsive: true,
          maintainAspectRatio: false,
          plugins: {
            legend: {
              position: 'bottom',
              labels: { boxWidth: 12, padding: 16, font: { size: 12 } },
            },
          },
          scales: {
            x: {
              grid: { display: false },
              ticks: { font: { size: 11 } },
            },
            y: {
              beginAtZero: true,
              grid: { color: '#f1f5f9' },
              ticks: { font: { size: 11 } },
            },
          },
        },
      });
      this.charts.set(canvasId, chart);
    });
  }

  private sortPointsByDate(points: TimeSeriesPoint[]): TimeSeriesPoint[] {
    return [...points].sort((a, b) => a.timestamp.localeCompare(b.timestamp));
  }

  override render() {
    return html`
      <div class="header">
        <div class="header-left">
          <sl-icon name="graph-up"></sl-icon>
          <h1>${this.projectId ? 'Project Metrics' : 'Metrics Dashboard'}</h1>
        </div>
        <div class="period-selector">
          ${[7, 14, 30].map(
            d => html`
              <button
                class="period-btn ${this.periodDays === d ? 'active' : ''}"
                @click=${() => this.handlePeriodChange(d)}
              >
                ${d}d
              </button>
            `
          )}
        </div>
      </div>

      ${this.error
        ? html`
            <sl-alert variant="danger" open>
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.error}
            </sl-alert>
          `
        : nothing}

      <sl-tab-group @sl-tab-show=${this.handleTabChange}>
        <sl-tab slot="nav" panel="summary" ?active=${this.activeTab === 'summary'}>Summary</sl-tab>
        <sl-tab slot="nav" panel="sessions" ?active=${this.activeTab === 'sessions'}>Sessions & Users</sl-tab>
        <sl-tab slot="nav" panel="model-calls" ?active=${this.activeTab === 'model-calls'}>Model Calls</sl-tab>
        <sl-tab slot="nav" panel="tokens" ?active=${this.activeTab === 'tokens'}>Token Usage</sl-tab>

        <sl-tab-panel name="summary">${this.renderSummaryTab()}</sl-tab-panel>
        <sl-tab-panel name="sessions">${this.renderSessionsTab()}</sl-tab-panel>
        <sl-tab-panel name="model-calls">${this.renderModelCallsTab()}</sl-tab-panel>
        <sl-tab-panel name="tokens">${this.renderTokensTab()}</sl-tab-panel>
      </sl-tab-group>
    `;
  }

  private renderSummaryTab() {
    if (this.loading) return html`<sl-spinner></sl-spinner>`;
    if (!this.summary) return this.renderEmptyState();

    const s = this.summary;
    return html`
      <div class="stats-row">
        <div class="stat-card">
          <span class="stat-label">Total Sessions</span>
          <span class="stat-value">${this.formatNumber(s.totalSessions)}</span>
        </div>
        <div class="stat-card">
          <span class="stat-label">API Calls</span>
          <span class="stat-value">${this.formatNumber(s.totalApiCalls)}</span>
        </div>
        <div class="stat-card">
          <span class="stat-label">Total Tokens</span>
          <span class="stat-value">${this.formatNumber(s.totalTokens)}</span>
        </div>
        <div class="stat-card">
          <span class="stat-label">Unique Agents</span>
          <span class="stat-value">${this.formatNumber(s.uniqueAgents)}</span>
        </div>
      </div>
    `;
  }

  private renderSessionsTab() {
    if (this.loading) return html`<sl-spinner></sl-spinner>`;
    if (!this.sessions) return this.renderEmptyState();

    return html`
      <div class="chart-row">
        <div class="section">
          <h3 class="chart-section-title">Daily Sessions</h3>
          <div class="chart-container">
            <canvas id="chart-sessions"></canvas>
          </div>
        </div>
        <div class="section">
          <h3 class="chart-section-title">Active Agents per Day</h3>
          <div class="chart-container">
            <canvas id="chart-agents"></canvas>
          </div>
        </div>
      </div>
    `;
  }

  private renderModelCallsTab() {
    if (this.loading) return html`<sl-spinner></sl-spinner>`;
    if (!this.modelCalls) return this.renderEmptyState();

    return html`
      <div class="chart-row">
        <div class="section">
          <h3 class="chart-section-title">API Calls by Model</h3>
          <div class="chart-container">
            <canvas id="chart-model-calls"></canvas>
          </div>
        </div>
        <div class="section">
          <h3 class="chart-section-title">API Calls by Harness</h3>
          <div class="chart-container">
            <canvas id="chart-harness-calls"></canvas>
          </div>
        </div>
      </div>
    `;
  }

  private renderTokensTab() {
    if (this.loading) return html`<sl-spinner></sl-spinner>`;
    if (!this.tokens) return this.renderEmptyState();

    return html`
      <div class="chart-row">
        <div class="section">
          <h3 class="chart-section-title">Input Tokens by Model</h3>
          <div class="chart-container">
            <canvas id="chart-tokens-input"></canvas>
          </div>
        </div>
        <div class="section">
          <h3 class="chart-section-title">Output Tokens by Model</h3>
          <div class="chart-container">
            <canvas id="chart-tokens-output"></canvas>
          </div>
        </div>
      </div>
    `;
  }

  private renderEmptyState() {
    return html`
      <div class="empty-state">
        <sl-icon name="graph-up"></sl-icon>
        <p>No metrics data available for this period.</p>
        <p>Metrics are collected from agent telemetry via Google Cloud Monitoring.</p>
      </div>
    `;
  }

  private collectDates(series: LabeledTimeSeries[]): string[] {
    const dateSet = new Set<string>();
    for (const s of series) {
      for (const p of s.points) {
        dateSet.add(p.timestamp);
      }
    }
    return [...dateSet].sort();
  }

  private alignToDateAxis(points: TimeSeriesPoint[], dates: string[]): number[] {
    const map = new Map<string, number>();
    for (const p of points) {
      map.set(p.timestamp, p.value);
    }
    return dates.map(d => map.get(d) ?? 0);
  }
}
