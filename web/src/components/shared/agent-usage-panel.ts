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
 * Agent Usage Panel Component
 *
 * Displays Track 1 operational token usage from GET /api/v1/agents/{id}/usage
 * (gated by tokenomics.usage_ledger): canonical nullable token buckets,
 * cache hit rate, summable total, call counts, per-model breakdown,
 * measurement_status badge, and last updated receipt timestamp.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type {
  AgentUsageResponse,
  AgentUsageTokens,
  UsageMeasurementStatus,
} from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { formatNumber } from '../../utils/format-number.js';

export type UsageWindowPreset = '7d' | '30d' | '90d';

/**
 * Format a nullable token count. Unknown (null/undefined) always renders as
 * "—", never 0 (§3.1). When unknownEvents > 0, prefixes with "≥ " to indicate
 * a lower bound from partially unknown events.
 */
export function formatUsageToken(value: number | null | undefined, unknownEvents = 0): string {
  if (value === null || value === undefined) return '—';
  const formatted = formatNumber(value);
  return unknownEvents > 0 ? `≥ ${formatted}` : formatted;
}

/**
 * Return the maximum unknown-event count affecting summable_tokens
 * (input, output, cache_read, cache_write, or all-null events).
 */
export function summableUnknownEvents(tokens: AgentUsageTokens, nullTokenEvents = 0): number {
  const u = tokens.unknown_events;
  return Math.max(
    nullTokenEvents,
    u?.input ?? 0,
    u?.output ?? 0,
    u?.cache_read ?? 0,
    u?.cache_write ?? 0
  );
}

/**
 * Return true if any token bucket has mixed known+unknown events.
 */
export function hasPartialTokenBuckets(tokens: AgentUsageTokens): boolean {
  const u = tokens.unknown_events;
  if (!u) return false;
  return (
    (u.input ?? 0) > 0 ||
    (u.output ?? 0) > 0 ||
    (u.cache_read ?? 0) > 0 ||
    (u.cache_write ?? 0) > 0 ||
    (u.cache_write_1h ?? 0) > 0 ||
    (u.reasoning ?? 0) > 0
  );
}

/**
 * Compute cache hit rate: cache_read / (input + cache_read + cache_write).
 * Returns null when any required bucket is unknown (null) or the denominator
 * is 0.
 */
export function computeCacheHitRate(tokens: AgentUsageTokens): number | null {
  if (tokens.input === null || tokens.cache_read === null || tokens.cache_write === null) {
    return null;
  }
  const denom = tokens.input + tokens.cache_read + tokens.cache_write;
  if (denom <= 0) return null;
  return tokens.cache_read / denom;
}

/**
 * Format a cache hit rate ratio [0, 1] as a percentage string (e.g. "23.1%"),
 * prefixed with "≈ " when any of input, cache_read, or cache_write has
 * partially unknown events, or "—" when unknown.
 */
export function formatCacheHitRate(tokens: AgentUsageTokens): string {
  const rate = computeCacheHitRate(tokens);
  if (rate === null) return '—';
  const u = tokens.unknown_events;
  const approx = (u?.input ?? 0) > 0 || (u?.cache_read ?? 0) > 0 || (u?.cache_write ?? 0) > 0;
  const pct = `${(rate * 100).toFixed(1)}%`;
  return approx ? `≈ ${pct}` : pct;
}

/**
 * Map measurement_status to a Shoelace badge variant and human-readable label.
 */
export function measurementStatusDisplay(status: UsageMeasurementStatus): {
  variant: 'success' | 'warning' | 'neutral';
  label: string;
} {
  switch (status) {
    case 'measured':
      return { variant: 'success', label: 'Measured' };
    case 'stale':
      return { variant: 'warning', label: 'Stale' };
    case 'partial':
      return { variant: 'warning', label: 'Partial' };
    case 'calls_only':
      return { variant: 'warning', label: 'Calls only' };
    case 'legacy_client':
      return { variant: 'neutral', label: 'Legacy client' };
    case 'unmeasured':
    default:
      return { variant: 'neutral', label: 'Unmeasured' };
  }
}

@customElement('scion-agent-usage-panel')
export class ScionAgentUsagePanel extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  @property({ type: String })
  agentId = '';

  @state()
  private windowPreset: UsageWindowPreset = '30d';

  @state()
  private loading = false;

  @state()
  private error: string | null = null;

  @state()
  private usage: AgentUsageResponse | null = null;

  private loadGeneration = 0;
  private loadedForAgentId = '';

  static override styles = css`
    :host {
      display: block;
    }

    .toolbar {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      justify-content: space-between;
      gap: 0.75rem;
      margin-bottom: 1rem;
    }

    .toolbar-left {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.75rem;
    }

    .toolbar-right {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .last-updated {
      font-size: 0.875rem;
      color: var(--scion-text-muted);
    }

    .window-select {
      min-width: 9rem;
    }

    .window-select::part(form-control-label) {
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

    .card {
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
      padding: 1.25rem;
      margin-bottom: 1rem;
    }

    .card-title {
      margin: 0 0 1rem 0;
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text);
    }

    .info-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(160px, 1fr));
      gap: 1rem;
    }

    .info-item {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }

    .info-label {
      font-size: 0.75rem;
      font-weight: 600;
      letter-spacing: 0.04em;
      text-transform: uppercase;
      color: var(--scion-text-muted);
    }

    .info-value {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text);
      font-variant-numeric: tabular-nums;
    }

    .error-count {
      color: var(--scion-danger-500);
      font-size: 0.875rem;
      font-weight: 500;
      margin-left: 0.25rem;
    }

    .notice {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.75rem 1rem;
      margin-bottom: 1rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      font-size: 0.875rem;
      color: var(--scion-text-muted);
    }

    .table-wrap {
      overflow-x: auto;
    }

    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.875rem;
    }

    th,
    td {
      text-align: left;
      padding: 0.625rem 0.75rem;
      border-bottom: 1px solid var(--scion-border);
    }

    th {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      color: var(--scion-text-muted);
    }

    td.num,
    th.num {
      text-align: right;
      font-variant-numeric: tabular-nums;
    }

    tbody tr:last-child td {
      border-bottom: none;
    }

    .model-name {
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
      color: var(--scion-text);
    }

    .empty-text {
      color: var(--scion-text-muted);
      font-size: 0.875rem;
      margin: 0;
    }

    .loading-state {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 1.5rem 0;
      color: var(--scion-text-muted);
      font-size: 0.875rem;
    }

    .error-state {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 1rem;
      padding: 0.875rem 1rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      color: var(--scion-danger-500);
      font-size: 0.875rem;
    }
  `;

  override updated(changed: PropertyValues): void {
    // Only auto-reload on agentId change if the panel has already been shown
    // and loaded once; initial loading is triggered lazily via loadIfNeeded()
    // when the Usage tab is first selected.
    if (
      changed.has('agentId') &&
      this.agentId &&
      this.loadedForAgentId !== '' &&
      this.loadedForAgentId !== this.agentId
    ) {
      void this.loadUsage();
    }
  }

  /**
   * Load usage on first tab activation (or when agentId changed since the last
   * load). Subsequent tab switches reuse the loaded snapshot until Refresh or
   * a window change is requested.
   */
  async loadIfNeeded(): Promise<void> {
    if (!this.agentId) return;
    if (this.loadedForAgentId === this.agentId) return;
    await this.loadUsage();
  }

  async loadUsage(): Promise<void> {
    if (!this.agentId) return;
    const gen = ++this.loadGeneration;
    this.loadedForAgentId = this.agentId;
    this.loading = true;
    this.error = null;
    try {
      const url = `/api/v1/agents/${encodeURIComponent(this.agentId)}/usage?since=${encodeURIComponent(this.windowPreset)}`;
      const res = await apiFetch(url);
      if (gen !== this.loadGeneration) return;
      if (!res.ok) {
        this.error = await extractApiError(res, `Failed to load usage (${res.status})`);
        this.usage = null;
        return;
      }
      this.usage = (await res.json()) as AgentUsageResponse;
    } catch (err) {
      if (gen !== this.loadGeneration) return;
      this.error = err instanceof Error ? err.message : 'Failed to load usage';
      this.usage = null;
    } finally {
      if (gen === this.loadGeneration) {
        this.loading = false;
      }
    }
  }

  private handleWindowChange(e: Event): void {
    const target = e.target as HTMLElement & { value?: string };
    const next = target.value as UsageWindowPreset | undefined;
    if (next && next !== this.windowPreset) {
      this.windowPreset = next;
      void this.loadUsage();
    }
  }

  private renderLastUpdated(lastReceiptAt: string | null): string {
    if (!lastReceiptAt) return '—';
    const rel = formatRelative(lastReceiptAt);
    const abs = formatInstantWithZone(lastReceiptAt);
    return rel ? `${rel} (${abs})` : abs || lastReceiptAt;
  }

  override render(): TemplateResult {
    if (this.loading && !this.usage) {
      return html`
        <div class="loading-state" data-testid="usage-loading">
          <sl-spinner></sl-spinner>
          <span>Loading usage…</span>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state" data-testid="usage-error">
          <span>${this.error}</span>
          <sl-button
            size="small"
            @click=${(): void => {
              void this.loadUsage();
            }}
            >Retry</sl-button
          >
        </div>
      `;
    }

    const u = this.usage;
    if (!u) {
      return html`
        <div class="card">
          <p class="empty-text">No usage data available yet.</p>
        </div>
      `;
    }

    const statusInfo = measurementStatusDisplay(u.measurement_status);
    const t = u.tokens;
    const unk = t.unknown_events;
    const showPartialNotice = u.null_token_events > 0 || hasPartialTokenBuckets(t);

    return html`
      <div class="toolbar">
        <div class="toolbar-left">
          <sl-badge
            variant=${statusInfo.variant}
            data-testid="usage-measurement-status"
            data-status=${u.measurement_status}
          >
            ${statusInfo.label}
          </sl-badge>
          <span class="last-updated" data-testid="usage-last-updated">
            Last updated: ${this.renderLastUpdated(u.last_receipt_at)}
          </span>
        </div>
        <div class="toolbar-right">
          <sl-select
            size="small"
            class="window-select"
            label="Usage time window"
            data-testid="usage-window-select"
            .value=${this.windowPreset}
            @sl-change=${this.handleWindowChange}
          >
            <sl-option value="7d">Last 7 days</sl-option>
            <sl-option value="30d">Last 30 days</sl-option>
            <sl-option value="90d">Last 90 days</sl-option>
          </sl-select>
          <sl-button
            size="small"
            variant="default"
            ?loading=${this.loading}
            @click=${(): void => {
              void this.loadUsage();
            }}
            data-testid="usage-refresh-btn"
          >
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Refresh
          </sl-button>
        </div>
      </div>

      ${showPartialNotice
        ? html`
            <div class="notice" data-testid="usage-null-events-notice">
              <sl-icon name="exclamation-triangle"></sl-icon>
              <span>
                ${u.null_token_events > 0
                  ? `${formatNumber(u.null_token_events)} of ${formatNumber(u.events)} usage event${u.events === 1 ? '' : 's'} reported unknown token counts.`
                  : 'Some events in this window reported unknown values for one or more token buckets.'}
                Values marked with “≥” are lower bounds.
              </span>
            </div>
          `
        : nothing}

      <div class="card">
        <h3 class="card-title">Token Usage</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Summable Total</span>
            <span class="info-value" data-testid="usage-summable-total">
              ${formatUsageToken(t.summable_tokens, summableUnknownEvents(t, u.null_token_events))}
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Input</span>
            <span class="info-value" data-testid="usage-input">
              ${formatUsageToken(t.input, unk?.input ?? 0)}
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Output</span>
            <span class="info-value" data-testid="usage-output">
              ${formatUsageToken(t.output, unk?.output ?? 0)}
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Cache Read</span>
            <span class="info-value" data-testid="usage-cache-read">
              ${formatUsageToken(t.cache_read, unk?.cache_read ?? 0)}
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Cache Write</span>
            <span class="info-value" data-testid="usage-cache-write">
              ${formatUsageToken(t.cache_write, unk?.cache_write ?? 0)}
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Cache Hit Rate</span>
            <span class="info-value" data-testid="usage-cache-hit-rate">
              ${formatCacheHitRate(t)}
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Calls</span>
            <span class="info-value" data-testid="usage-calls">
              ${formatNumber(u.calls)}${u.error_calls > 0
                ? html`<span class="error-count">(${formatNumber(u.error_calls)} errors)</span>`
                : nothing}
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Reasoning</span>
            <span class="info-value" data-testid="usage-reasoning">
              ${formatUsageToken(t.reasoning, unk?.reasoning ?? 0)}
            </span>
          </div>
        </div>
      </div>

      <div class="card">
        <h3 class="card-title">By Model</h3>
        ${u.by_model.length === 0
          ? html`<p class="empty-text" data-testid="usage-by-model-empty">
              No model usage recorded in this window.
            </p>`
          : html`
              <div class="table-wrap">
                <table data-testid="usage-by-model-table">
                  <thead>
                    <tr>
                      <th>Model</th>
                      <th class="num">Calls</th>
                      <th class="num">Input</th>
                      <th class="num">Output</th>
                      <th class="num">Cache Read</th>
                      <th class="num">Cache Write</th>
                      <th class="num">Cache Hit Rate</th>
                      <th class="num">Summable</th>
                    </tr>
                  </thead>
                  <tbody>
                    ${u.by_model.map(
                      (row) => html`
                        <tr data-testid="usage-model-row">
                          <td class="model-name">${row.model}</td>
                          <td class="num">
                            ${formatNumber(row.calls)}${row.error_calls > 0
                              ? html`<span class="error-count"
                                  >(${formatNumber(row.error_calls)} err)</span
                                >`
                              : nothing}
                          </td>
                          <td class="num">
                            ${formatUsageToken(
                              row.tokens.input,
                              row.tokens.unknown_events?.input ?? 0
                            )}
                          </td>
                          <td class="num">
                            ${formatUsageToken(
                              row.tokens.output,
                              row.tokens.unknown_events?.output ?? 0
                            )}
                          </td>
                          <td class="num">
                            ${formatUsageToken(
                              row.tokens.cache_read,
                              row.tokens.unknown_events?.cache_read ?? 0
                            )}
                          </td>
                          <td class="num">
                            ${formatUsageToken(
                              row.tokens.cache_write,
                              row.tokens.unknown_events?.cache_write ?? 0
                            )}
                          </td>
                          <td class="num">${formatCacheHitRate(row.tokens)}</td>
                          <td class="num">
                            ${formatUsageToken(
                              row.tokens.summable_tokens,
                              summableUnknownEvents(row.tokens, row.null_token_events)
                            )}
                          </td>
                        </tr>
                      `
                    )}
                  </tbody>
                </table>
              </div>
            `}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-agent-usage-panel': ScionAgentUsagePanel;
  }
}
