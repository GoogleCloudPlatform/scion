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

import { LitElement, html, css, nothing, type PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { Chart, registerables } from 'chart.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { current, projectTrail, type Named } from '../../client/crumbs.js';
import { projectLabelNow, resolveProjectLabel } from '../../client/project-label.js';
import type {
  DashboardMeasurement,
  DashboardTokenTotals,
  NullableFloatPoint,
} from '../../shared/types.js';
import '../shared/breadcrumb.js';
import '../shared/measurement-status-badge.js';
import { FEATURE_FLAGS_CHANGED_EVENT, usageLedgerEnabled } from '../../utils/feature-flags.js';
import { formatNumber } from '../../utils/format-number.js';
import {
  DISPLAY_TIMEZONE_CHANGED_EVENT,
  dayBucketAxisTitle,
  dayBucketZoneLabel,
  effectiveTimeZone,
} from '../../utils/time.js';
import {
  formatCacheHitRateValue,
  formatCompactUsageToken,
  formatDashboardSourceLabel,
  hasPartialDashboardTotals,
  isMeasurementPartial,
} from '../../utils/usage-format.js';

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
  /** The IANA zone the hub bucketed by (absent from older hubs: UTC). */
  timeZone?: string;
  /** Backing data source ("cloud_monitoring" or "hub_rollups"). */
  source?: string;
  /** Rollup granularity used by HubRollupSource ("hour" or "day"). */
  granularity?: string;
  /** Session count, or null when the backing session query failed. */
  totalSessions: number | null;
  totalApiCalls: number;
  totalTokens: number;
  uniqueAgents: number;
  /** Nullable canonical Track 1 token buckets (HubRollupSource). */
  tokens?: DashboardTokenTotals;
  /** Cache hit rate: cache_read / (input + cache_read + cache_write), or null. */
  cacheHitRate?: number | null;
  /** Track 1 measurement coverage status and unmeasured/stale agent counts. */
  measurement?: DashboardMeasurement;
  /** True when one or more backing sub-queries failed. */
  partialFailure?: boolean;
  /** Descriptions of failed backing sub-queries when partialFailure is true. */
  partialErrors?: string[];
  /** Actionable setup hint when cloud telemetry cannot export under current settings. */
  telemetryHint?: string;
}

interface SessionsData {
  periodDays: number;
  /** The IANA zone the hub bucketed by (absent from older hubs: UTC). */
  timeZone?: string;
  source?: string;
  granularity?: string;
  dailyCounts: TimeSeriesPoint[];
  activeAgents: TimeSeriesPoint[];
  truncated?: boolean;
  partialFailure?: boolean;
  partialErrors?: string[];
  measurement?: DashboardMeasurement;
}

interface ModelCallsData {
  periodDays: number;
  /** The IANA zone the hub bucketed by (absent from older hubs: UTC). */
  timeZone?: string;
  source?: string;
  granularity?: string;
  groupBy?: string;
  byModel: LabeledTimeSeries[];
  byHarness: LabeledTimeSeries[];
  byUser?: LabeledTimeSeries[];
  truncated?: boolean;
  partialFailure?: boolean;
  partialErrors?: string[];
  measurement?: DashboardMeasurement;
}

interface TokensData {
  periodDays: number;
  /** The IANA zone the hub bucketed by (absent from older hubs: UTC). */
  timeZone?: string;
  source?: string;
  granularity?: string;
  groupBy?: string;
  input: LabeledTimeSeries[];
  output: LabeledTimeSeries[];
  cacheRead?: LabeledTimeSeries[];
  cacheWrite?: LabeledTimeSeries[];
  totals?: DashboardTokenTotals;
  dailyCacheHitRate?: NullableFloatPoint[];
  truncated?: boolean;
  partialFailure?: boolean;
  partialErrors?: string[];
  measurement?: DashboardMeasurement;
}

const CHART_COLORS = [
  '#3b82f6',
  '#10b981',
  '#f59e0b',
  '#ef4444',
  '#8b5cf6',
  '#ec4899',
  '#06b6d4',
  '#84cc16',
  '#f97316',
  '#6366f1',
];

/**
 * Metrics dashboard page. It asks the hub to bucket every daily series by
 * calendar day in the viewer's effective display zone (`effectiveTimeZone()`:
 * the user's zone preference, else the browser's zone), sent as the `tz`
 * query parameter (ptone/scion#3370). The hub falls back to UTC for a zone it
 * cannot resolve and reports the zone it used as `timeZone` in each view's
 * response. Each tab's headings and x-axis title name the zone reported with
 * that tab's own data, never the requested zone or another view's zone.
 */
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
  @state() private projectNamed: Named | null = null;
  @state() private usageLedgerOn = usageLedgerEnabled();

  private resolvedProjectId = '';
  private charts: Map<string, Chart> = new Map();

  /** Increments for every metrics request the page issues. */
  private requestSeq = 0;
  /** The sequence number of the newest request per view. */
  private readonly latestRequestByView = new Map<string, number>();

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
      color: var(--scion-primary-text, #ffffff);
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

    .telemetry-hint {
      margin-bottom: 1rem;
    }

    .source-indicator {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      padding: 0.2rem 0.5rem;
      white-space: nowrap;
    }

    .measurement-bar {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-wrap: wrap;
      margin-bottom: 1rem;
    }

    .partial-hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }

    .chart-empty {
      display: flex;
      align-items: center;
      justify-content: center;
      height: 100%;
      color: var(--scion-text-muted);
      font-size: 0.875rem;
    }

    .stat-value.unavailable {
      font-size: 1.125rem;
      color: var(--scion-text-muted);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    // Parse projectId from URL if not set as property (same pattern as project-detail.ts)
    if (!this.projectId && typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/projects\/([^/]+)\/metrics/);
      if (match) this.projectId = decodeURIComponent(match[1]);
    }
    this.syncProjectLabel();
    this.usageLedgerOn = usageLedgerEnabled();
    window.addEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, this.handleDisplayZoneChanged);
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.handleFeatureFlagsChanged);
    void this.loadView('summary');
  }

  protected override willUpdate(changed: PropertyValues<this>): void {
    super.willUpdate(changed);
    if (changed.has('projectId')) {
      this.syncProjectLabel();
    }
  }

  private syncProjectLabel(): void {
    const id = (this.projectId ?? '').trim();
    if (!id) {
      this.projectNamed = null;
      this.resolvedProjectId = '';
      return;
    }
    if (id === this.resolvedProjectId) return;
    this.resolvedProjectId = id;
    this.projectNamed = projectLabelNow(id);
    void resolveProjectLabel(id).then((resolved) => {
      if (this.isConnected && (this.projectId ?? '').trim() === id) {
        this.projectNamed = resolved;
      }
    });
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, this.handleDisplayZoneChanged);
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.handleFeatureFlagsChanged);
    this.destroyAllCharts();
  }

  private readonly handleFeatureFlagsChanged = (): void => {
    const next = usageLedgerEnabled();
    if (next !== this.usageLedgerOn) {
      this.usageLedgerOn = next;
      this.destroyAllCharts();
    }
  };

  /**
   * The day buckets are computed by the hub for the zone the page sends, so a
   * change of the display-zone preference needs a refetch, not just a
   * re-render.
   */
  private readonly handleDisplayZoneChanged = (): void => {
    this.destroyAllCharts();
    void this.loadView(this.activeTab);
  };

  private destroyAllCharts(): void {
    for (const chart of this.charts.values()) {
      chart.destroy();
    }
    this.charts.clear();
  }

  private async loadView(view: string): Promise<void> {
    // A response older than the newest request for the same view (for
    // example one sent before a display-zone change) is dropped, so it can
    // never replace newer data for that view.
    const seq = ++this.requestSeq;
    this.latestRequestByView.set(view, seq);
    this.loading = true;
    // this.error is page-global: only a request for the active tab may clear
    // it or set it, so a background request cannot hide or replace the error
    // the active tab is showing.
    if (view === this.activeTab) {
      this.error = null;
    }

    try {
      const basePath = this.projectId
        ? `/api/v1/projects/${this.projectId}/metrics`
        : `/api/v1/metrics/`;
      // suppressAccessDeniedToast: a failure is surfaced in this.error and
      // rendered by the page. Each view is a separate request and the dashboard
      // reloads on a timer, so leaving the toast on produces a stream of
      // duplicates rather than one actionable message.
      const tz = encodeURIComponent(effectiveTimeZone());
      const response = await apiFetch(
        `${basePath}?view=${view}&period=${this.periodDays}&tz=${tz}`,
        {
          suppressAccessDeniedToast: true,
        }
      );

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      const data: unknown = await response.json();
      if (this.latestRequestByView.get(view) !== seq) return;

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
      if (this.latestRequestByView.get(view) === seq && view === this.activeTab) {
        this.error = err instanceof Error ? err.message : String(err);
      }
    } finally {
      // Only the newest request overall ends the loading state.
      if (seq === this.requestSeq) {
        this.loading = false;
      }
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

  private formatCompactNumber(n: number): string {
    if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
    if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
    return formatNumber(n);
  }

  private static readonly CHART_PROPERTIES = new Set([
    'sessions',
    'modelCalls',
    'tokens',
    'activeTab',
    'loading',
    'usageLedgerOn',
  ]);

  override updated(changedProperties: Map<string, unknown>): void {
    super.updated(changedProperties);
    for (const key of changedProperties.keys()) {
      if (ScionPageMetrics.CHART_PROPERTIES.has(key)) {
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
            this.renderChart(
              this.sessions.timeZone,
              'chart-sessions',
              'bar',
              sorted.map((p) => p.timestamp),
              [
                {
                  label: 'Sessions',
                  data: sorted.map((p) => p.value),
                  backgroundColor: CHART_COLORS[0],
                },
              ]
            );
          }
          if (this.sessions.activeAgents?.length) {
            const sorted = this.sortPointsByDate(this.sessions.activeAgents);
            this.renderChart(
              this.sessions.timeZone,
              'chart-agents',
              'line',
              sorted.map((p) => p.timestamp),
              [
                {
                  label: 'Active Agents',
                  data: sorted.map((p) => p.value),
                  borderColor: CHART_COLORS[1],
                  backgroundColor: CHART_COLORS[1] + '33',
                },
              ]
            );
          }
        }
        break;
      case 'model-calls':
        if (this.modelCalls) {
          if (this.modelCalls.byModel?.length) {
            const allDates = this.collectDates(this.modelCalls.byModel);
            this.renderChart(
              this.modelCalls.timeZone,
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
              this.modelCalls.timeZone,
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
          const omitUnknownAsNull = this.tokens.source === 'hub_rollups';
          if (this.tokens.input?.length) {
            const allDates = this.collectDates(this.tokens.input);
            this.renderChart(
              this.tokens.timeZone,
              'chart-tokens-input',
              'bar',
              allDates,
              this.tokens.input.map((series, i) => ({
                label: series.label,
                data: this.alignToDateAxis(series.points, allDates, omitUnknownAsNull),
                backgroundColor: CHART_COLORS[i % CHART_COLORS.length],
              }))
            );
          }
          if (this.tokens.output?.length) {
            const allDates = this.collectDates(this.tokens.output);
            this.renderChart(
              this.tokens.timeZone,
              'chart-tokens-output',
              'bar',
              allDates,
              this.tokens.output.map((series, i) => ({
                label: series.label,
                data: this.alignToDateAxis(series.points, allDates, omitUnknownAsNull),
                backgroundColor: CHART_COLORS[(i + 3) % CHART_COLORS.length],
              }))
            );
          }
          if (this.usageLedgerOn) {
            if (this.tokens.cacheRead?.length) {
              const allDates = this.collectDates(this.tokens.cacheRead);
              this.renderChart(
                this.tokens.timeZone,
                'chart-tokens-cache-read',
                'bar',
                allDates,
                this.tokens.cacheRead.map((series, i) => ({
                  label: series.label,
                  data: this.alignToDateAxis(series.points, allDates, omitUnknownAsNull),
                  backgroundColor: CHART_COLORS[(i + 1) % CHART_COLORS.length],
                }))
              );
            }
            if (this.tokens.cacheWrite?.length) {
              const allDates = this.collectDates(this.tokens.cacheWrite);
              this.renderChart(
                this.tokens.timeZone,
                'chart-tokens-cache-write',
                'bar',
                allDates,
                this.tokens.cacheWrite.map((series, i) => ({
                  label: series.label,
                  data: this.alignToDateAxis(series.points, allDates, omitUnknownAsNull),
                  backgroundColor: CHART_COLORS[(i + 4) % CHART_COLORS.length],
                }))
              );
            }
            if (this.tokens.dailyCacheHitRate?.length) {
              const sorted = [...this.tokens.dailyCacheHitRate].sort((a, b) =>
                a.timestamp.localeCompare(b.timestamp)
              );
              this.renderChart(
                this.tokens.timeZone,
                'chart-tokens-cache-hit-rate',
                'line',
                sorted.map((p) => p.timestamp),
                [
                  {
                    label: 'Cache Hit Rate (%)',
                    data: sorted.map((p) =>
                      p.value === null ? null : Number((p.value * 100).toFixed(1))
                    ),
                    borderColor: CHART_COLORS[6],
                    backgroundColor: CHART_COLORS[6] + '33',
                    spanGaps: false,
                  },
                ],
                { yPercent: true }
              );
            }
          }
        }
        break;
    }
  }

  /**
   * `reportedZone` is the `timeZone` the hub sent with this chart's own data;
   * it names the x-axis.
   */
  private renderChart(
    reportedZone: string | undefined,
    canvasId: string,
    type: 'bar' | 'line',
    labels: string[],
    datasets: {
      label: string;
      data: (number | null)[];
      backgroundColor?: string;
      borderColor?: string;
      spanGaps?: boolean;
    }[],
    opts?: { yPercent?: boolean }
  ): void {
    requestAnimationFrame(() => {
      const canvas = this.shadowRoot?.getElementById(canvasId) as HTMLCanvasElement | null;
      if (!canvas) return;

      const existing = this.charts.get(canvasId);
      if (existing) {
        if ((existing.config as { type?: string }).type === type) {
          existing.data.labels = labels;
          existing.data.datasets = datasets;
          // The data may now come from a different zone, so the axis title
          // is refreshed along with it.
          const xTitle = (existing.options.scales?.x as { title?: { text?: string } } | undefined)
            ?.title;
          if (xTitle) xTitle.text = dayBucketAxisTitle(reportedZone);
          existing.update();
          return;
        }
        existing.destroy();
      }

      const gridColor =
        typeof getComputedStyle === 'function'
          ? getComputedStyle(this).getPropertyValue('--scion-border-subtle').trim()
          : '';
      const yScale: Record<string, unknown> = {
        beginAtZero: true,
        grid: gridColor ? { color: gridColor } : {},
        ticks: opts?.yPercent
          ? {
              font: { size: 11 },
              callback: (v: string | number) => `${v}%`,
            }
          : { font: { size: 11 } },
      };
      if (opts?.yPercent) {
        yScale.max = 100;
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
              title: {
                display: true,
                text: dayBucketAxisTitle(reportedZone),
                font: { size: 11 },
              },
            },
            y: yScale,
          },
        },
      });
      this.charts.set(canvasId, chart);
    });
  }

  private sortPointsByDate(points: TimeSeriesPoint[]): TimeSeriesPoint[] {
    return [...points].sort((a, b) => a.timestamp.localeCompare(b.timestamp));
  }

  private projectCrumbs() {
    const id = this.projectId.trim();
    if (!id) return [];
    const project = this.projectNamed?.id === id ? this.projectNamed : projectLabelNow(id);
    const hasData = Boolean(this.summary || this.sessions || this.modelCalls || this.tokens);
    if ((this.loading && !hasData) || this.error) {
      return projectTrail(project);
    }
    return [...projectTrail(project), current('Metrics', false)];
  }

  private activeViewMeta(): {
    source?: string;
    granularity?: string;
    timeZone?: string;
  } | null {
    switch (this.activeTab) {
      case 'summary':
        return this.summary;
      case 'sessions':
        return this.sessions;
      case 'model-calls':
        return this.modelCalls;
      case 'tokens':
        return this.tokens;
      default:
        return this.summary;
    }
  }

  private renderMeasurementBar(measurement: DashboardMeasurement | undefined, testId: string) {
    if (!this.usageLedgerOn || !measurement) return nothing;
    const partial = isMeasurementPartial(measurement);
    return html`
      <div class="measurement-bar" data-testid=${testId}>
        <scion-measurement-status-badge
          .status=${measurement.status}
          .unmeasuredAgents=${measurement.unmeasuredAgents}
          .staleAgents=${measurement.staleAgents}
        ></scion-measurement-status-badge>
        ${partial
          ? html`<span class="partial-hint" data-testid="${testId}-partial">
              Values marked with “≥” or “≈” reflect partial coverage.
            </span>`
          : nothing}
      </div>
    `;
  }

  override render() {
    const meta = this.activeViewMeta();
    const sourceLabel = this.usageLedgerOn
      ? formatDashboardSourceLabel(meta?.source, meta?.granularity, meta?.timeZone)
      : null;

    return html`
      ${this.projectId
        ? html`<scion-breadcrumb .crumbs=${this.projectCrumbs()}></scion-breadcrumb>`
        : nothing}
      <div class="header">
        <div class="header-left">
          <sl-icon name="graph-up"></sl-icon>
          <h1>${this.projectId ? 'Project Metrics' : 'Metrics Dashboard'}</h1>
          ${sourceLabel
            ? html`<span class="source-indicator" data-testid="metrics-source-indicator"
                >${sourceLabel}</span
              >`
            : nothing}
        </div>
        <div class="period-selector">
          ${[7, 14, 30].map(
            (d) => html`
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
      ${this.summary?.telemetryHint
        ? html`
            <sl-alert variant="warning" open class="telemetry-hint">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.summary.telemetryHint}
            </sl-alert>
          `
        : nothing}
      <sl-tab-group @sl-tab-show=${this.handleTabChange}>
        <sl-tab slot="nav" panel="summary" ?active=${this.activeTab === 'summary'}>Summary</sl-tab>
        <sl-tab slot="nav" panel="sessions" ?active=${this.activeTab === 'sessions'}
          >Sessions & Users</sl-tab
        >
        <sl-tab slot="nav" panel="model-calls" ?active=${this.activeTab === 'model-calls'}
          >Model Calls</sl-tab
        >
        <sl-tab slot="nav" panel="tokens" ?active=${this.activeTab === 'tokens'}
          >Token Usage</sl-tab
        >

        <sl-tab-panel name="summary">${this.renderSummaryTab()}</sl-tab-panel>
        <sl-tab-panel name="sessions">${this.renderSessionsTab()}</sl-tab-panel>
        <sl-tab-panel name="model-calls">${this.renderModelCallsTab()}</sl-tab-panel>
        <sl-tab-panel name="tokens">${this.renderTokensTab()}</sl-tab-panel>
      </sl-tab-group>
    `;
  }

  private renderViewNotices(
    view: {
      truncated?: boolean;
      granularity?: string;
      partialFailure?: boolean;
      partialErrors?: string[];
    },
    tabKey: string
  ) {
    const showTruncated = this.usageLedgerOn && Boolean(view.truncated);
    const showPartialFailure = Boolean(view.partialFailure);
    if (!showTruncated && !showPartialFailure) return nothing;
    const partialDetail = view.partialErrors?.length
      ? `Some metrics could not be loaded (${view.partialErrors.join('; ')}). Affected counts are shown as unavailable.`
      : 'Some metrics could not be loaded. Affected counts are shown as unavailable.';
    const truncatedDetail =
      view.granularity === 'day'
        ? 'Series data reached the query limit (daily granularity); the earliest boundary day was omitted from charts while window totals remain exact.'
        : 'Series data reached the query limit; the earliest boundary day was omitted from charts while window totals remain exact.';
    return html`
      ${showPartialFailure
        ? html`
            <sl-alert
              variant="warning"
              open
              class="telemetry-hint"
              data-testid="metrics-${tabKey}-partial-failure"
            >
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${partialDetail}
            </sl-alert>
          `
        : nothing}
      ${showTruncated
        ? html`
            <sl-alert
              variant="warning"
              open
              class="telemetry-hint"
              data-testid="metrics-${tabKey}-truncated"
            >
              <sl-icon slot="icon" name="info-circle"></sl-icon>
              ${truncatedDetail}
            </sl-alert>
          `
        : nothing}
    `;
  }

  private renderSummaryTab() {
    if (this.loading) return html`<sl-spinner></sl-spinner>`;
    if (!this.summary) return this.renderEmptyState();

    const s = this.summary;
    const measurementPartial = this.usageLedgerOn && isMeasurementPartial(s.measurement);
    const summablePartial =
      measurementPartial || (this.usageLedgerOn && hasPartialDashboardTotals(s.tokens));
    const totalTokensDisplay =
      this.usageLedgerOn && s.tokens
        ? formatCompactUsageToken(
            s.tokens.summableTokens,
            summablePartial,
            Boolean(s.tokens.saturated)
          )
        : this.formatCompactNumber(s.totalTokens);
    const showCacheHitRate =
      this.usageLedgerOn && (s.tokens !== undefined || s.cacheHitRate !== undefined);
    const hitRateValue = s.cacheHitRate !== undefined ? s.cacheHitRate : s.tokens?.cacheHitRate;
    const sessionsUnavailable = s.totalSessions === null || s.totalSessions === undefined;

    return html`
      ${this.renderViewNotices(s, 'summary')}
      ${this.renderMeasurementBar(s.measurement, 'metrics-summary-measurement')}
      <div class="stats-row">
        <div class="stat-card">
          <span class="stat-label">Total Sessions</span>
          <span
            class="stat-value ${sessionsUnavailable ? 'unavailable' : ''}"
            data-testid="metrics-summary-sessions"
            >${sessionsUnavailable
              ? 'Unavailable'
              : this.formatCompactNumber(s.totalSessions!)}</span
          >
        </div>
        <div class="stat-card">
          <span class="stat-label">API Calls</span>
          <span class="stat-value" data-testid="metrics-summary-calls"
            >${this.formatCompactNumber(s.totalApiCalls)}</span
          >
        </div>
        <div class="stat-card">
          <span class="stat-label">Total Tokens</span>
          <span class="stat-value" data-testid="metrics-summary-tokens">${totalTokensDisplay}</span>
        </div>
        ${showCacheHitRate
          ? html`
              <div class="stat-card">
                <span class="stat-label">Cache Hit Rate</span>
                <span class="stat-value" data-testid="metrics-summary-cache-hit-rate"
                  >${formatCacheHitRateValue(hitRateValue, measurementPartial)}</span
                >
              </div>
            `
          : nothing}
        <div class="stat-card">
          <span class="stat-label">Unique Agents</span>
          <span class="stat-value" data-testid="metrics-summary-agents"
            >${this.formatCompactNumber(s.uniqueAgents)}</span
          >
        </div>
      </div>
    `;
  }

  private renderSessionsTab() {
    if (this.loading) return html`<sl-spinner></sl-spinner>`;
    if (!this.sessions) return this.renderEmptyState();
    const zone = dayBucketZoneLabel(this.sessions.timeZone);
    const sessionsUnavailable =
      Boolean(this.sessions.partialFailure) && !this.sessions.dailyCounts?.length;

    return html`
      ${this.renderViewNotices(this.sessions, 'sessions')}
      ${this.renderMeasurementBar(this.sessions.measurement, 'metrics-sessions-measurement')}
      <div class="chart-row">
        <div class="section">
          <h3 class="chart-section-title">Daily Sessions (${zone})</h3>
          <div class="chart-container">
            ${sessionsUnavailable
              ? html`<div class="chart-empty" data-testid="metrics-sessions-unavailable">
                  Session data is currently unavailable.
                </div>`
              : html`<canvas id="chart-sessions"></canvas>`}
          </div>
        </div>
        <div class="section">
          <h3 class="chart-section-title">Active Agents per Day (${zone})</h3>
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
    const zone = dayBucketZoneLabel(this.modelCalls.timeZone);

    return html`
      ${this.renderViewNotices(this.modelCalls, 'model-calls')}
      ${this.renderMeasurementBar(this.modelCalls.measurement, 'metrics-model-calls-measurement')}
      <div class="chart-row">
        <div class="section">
          <h3 class="chart-section-title">Daily API Calls by Model (${zone})</h3>
          <div class="chart-container">
            <canvas id="chart-model-calls"></canvas>
          </div>
        </div>
        <div class="section">
          <h3 class="chart-section-title">Daily API Calls by Harness (${zone})</h3>
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
    const t = this.tokens;
    const zone = dayBucketZoneLabel(t.timeZone);
    const measurementPartial = this.usageLedgerOn && isMeasurementPartial(t.measurement);
    const summablePartial =
      measurementPartial || (this.usageLedgerOn && hasPartialDashboardTotals(t.totals));
    const showCacheCharts =
      this.usageLedgerOn &&
      (Boolean(t.cacheRead?.length) || Boolean(t.cacheWrite?.length) || t.source === 'hub_rollups');
    const showHitRateChart = this.usageLedgerOn && Boolean(t.dailyCacheHitRate?.length);
    const cacheReadEmptyText =
      t.totals && t.totals.cacheRead === null
        ? 'Cache read tokens not reported in this period.'
        : 'No cache read tokens in this period.';
    const cacheWriteEmptyText =
      t.totals && t.totals.cacheWrite === null
        ? 'Cache write tokens not reported in this period.'
        : 'No cache write tokens in this period.';

    return html`
      ${this.renderViewNotices(t, 'tokens')}
      ${this.renderMeasurementBar(t.measurement, 'metrics-tokens-measurement')}
      ${this.usageLedgerOn && t.totals
        ? html`
            <div class="stats-row" data-testid="metrics-tokens-totals">
              <div class="stat-card">
                <span class="stat-label">Summable Total</span>
                <span class="stat-value" data-testid="metrics-tokens-summable"
                  >${formatCompactUsageToken(
                    t.totals.summableTokens,
                    summablePartial,
                    Boolean(t.totals.saturated)
                  )}</span
                >
              </div>
              <div class="stat-card">
                <span class="stat-label">Input</span>
                <span class="stat-value" data-testid="metrics-tokens-input-total"
                  >${formatCompactUsageToken(t.totals.input, measurementPartial)}</span
                >
              </div>
              <div class="stat-card">
                <span class="stat-label">Output</span>
                <span class="stat-value" data-testid="metrics-tokens-output-total"
                  >${formatCompactUsageToken(t.totals.output, measurementPartial)}</span
                >
              </div>
              <div class="stat-card">
                <span class="stat-label">Cache Read</span>
                <span class="stat-value" data-testid="metrics-tokens-cache-read-total"
                  >${formatCompactUsageToken(t.totals.cacheRead, measurementPartial)}</span
                >
              </div>
              <div class="stat-card">
                <span class="stat-label">Cache Write</span>
                <span class="stat-value" data-testid="metrics-tokens-cache-write-total"
                  >${formatCompactUsageToken(t.totals.cacheWrite, measurementPartial)}</span
                >
              </div>
              <div class="stat-card">
                <span class="stat-label">Cache Hit Rate</span>
                <span class="stat-value" data-testid="metrics-tokens-cache-hit-rate-total"
                  >${formatCacheHitRateValue(t.totals.cacheHitRate, measurementPartial)}</span
                >
              </div>
            </div>
          `
        : nothing}
      <div class="chart-row">
        <div class="section">
          <h3 class="chart-section-title">Daily Input Tokens by Model (${zone})</h3>
          <div class="chart-container">
            <canvas id="chart-tokens-input"></canvas>
          </div>
        </div>
        <div class="section">
          <h3 class="chart-section-title">Daily Output Tokens by Model (${zone})</h3>
          <div class="chart-container">
            <canvas id="chart-tokens-output"></canvas>
          </div>
        </div>
      </div>
      ${showCacheCharts
        ? html`
            <div class="chart-row" data-testid="metrics-tokens-cache-charts">
              <div class="section">
                <h3 class="chart-section-title">Daily Cache Read Tokens by Model (${zone})</h3>
                <div class="chart-container">
                  ${t.cacheRead?.length
                    ? html`<canvas id="chart-tokens-cache-read"></canvas>`
                    : html`<div class="chart-empty" data-testid="metrics-cache-read-empty">
                        ${cacheReadEmptyText}
                      </div>`}
                </div>
              </div>
              <div class="section">
                <h3 class="chart-section-title">Daily Cache Write Tokens by Model (${zone})</h3>
                <div class="chart-container">
                  ${t.cacheWrite?.length
                    ? html`<canvas id="chart-tokens-cache-write"></canvas>`
                    : html`<div class="chart-empty" data-testid="metrics-cache-write-empty">
                        ${cacheWriteEmptyText}
                      </div>`}
                </div>
              </div>
            </div>
          `
        : nothing}
      ${showHitRateChart
        ? html`
            <div class="section" data-testid="metrics-tokens-hit-rate-section">
              <h3 class="chart-section-title">Daily Cache Hit Rate (${zone})</h3>
              <div class="chart-container">
                <canvas id="chart-tokens-cache-hit-rate"></canvas>
              </div>
            </div>
          `
        : nothing}
    `;
  }

  private renderEmptyState() {
    return html`
      <div class="empty-state">
        <sl-icon name="graph-up"></sl-icon>
        <p>No metrics data available for this period.</p>
        <p>
          ${this.usageLedgerOn
            ? 'Metrics are collected from Hub usage rollups or Google Cloud Monitoring.'
            : 'Metrics are collected from agent telemetry via Google Cloud Monitoring.'}
        </p>
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

  private alignToDateAxis(
    points: TimeSeriesPoint[],
    dates: string[],
    missingAsNull = false
  ): (number | null)[] {
    const map = new Map<string, number>();
    for (const p of points) {
      map.set(p.timestamp, p.value);
    }
    return dates.map((d) => {
      const v = map.get(d);
      if (v === undefined) return missingAsNull ? null : 0;
      return v;
    });
  }
}

/**
 * Project-scoped metrics page, registered under a separate tag so the
 * client-side router can distinguish project-level from hub-level metrics.
 * Behaviour is identical — the base class already reads projectId from the URL.
 */
@customElement('scion-page-project-metrics')
export class ScionPageProjectMetrics extends ScionPageMetrics {}
