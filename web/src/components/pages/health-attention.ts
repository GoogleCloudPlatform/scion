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
 * "Needs attention" panel for the health dashboard (ptone/scion#3595).
 *
 * Renders the summary's attention list exactly as the server sends it: in
 * server order, with the server's sentence. An item links to its subject
 * only when the subject carries what the link needs (a broker, agent or
 * integration ID, or a hub instance ID). A hub item about one instance
 * links to that instance's row in the Hub instances table, on this page.
 * Fleet-wide hub items, dispatch and aggregate agent items are not linked.
 * Integration items are linked only when the summary says it carries
 * integration identity (integrations_detail true) and the item has an ID;
 * the server already omits the ID otherwise, this is a second check.
 *
 * A failing schedule item (ptone/scion#5404) links to its project's
 * schedules page and, when the summary's schedules section has a row for
 * it, shows a detail line: target, failed runs, last failure time and the
 * stored last error. The error is stored text: it is rendered only through
 * Lit text and attribute bindings (never as HTML) and shortened with
 * truncateText.
 *
 * A schedule the hub paused itself shows a detail line with its reason as
 * fixed text per known reason code (pausedReasonLabel); any other code,
 * including one a newer hub adds, gets a generic label. The code is never
 * rendered itself.
 */

import { LitElement, html, css, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import { INTEGRATIONS_PAGE } from './health-integrations.js';
import { followHubInstanceLink, hubInstanceAnchor } from './health-hub-card.js';
import { formatInstantWithZone } from '../../utils/time.js';

/** One entry of the summary's ranked "Needs attention" list (server-composed). */
export interface HealthAttentionItem {
  severity: 'critical' | 'warning';
  /** hub_check | hub_instance | broker_offline | broker_degraded | broker_nfs | broker_host_identity | broker_version_mismatch | integration | dispatch | agents | schedule */
  kind: string;
  /** What the item is about; fields that do not apply are omitted. */
  subject: { type: string; id?: string; name?: string; project_id?: string };
  /** Fixed, server-composed sentence. */
  message: string;
}

/** One active schedule whose last run failed (summary schedules.failing). */
export interface HealthFailingSchedule {
  id: string;
  name: string;
  project_id: string;
  /** Empty when the hub could not resolve the project. */
  project_slug: string;
  event_type: string;
  /** The agent the schedule addresses; empty when the payload names none. */
  target: string;
  run_count: number;
  error_count: number;
  /** RFC 3339; null when not recorded. */
  last_failure_at: string | null;
  /** Stored error text of the last run, capped by the hub. */
  last_error: string;
}

/** One schedule the hub paused itself (summary schedules.paused). */
export interface HealthPausedSchedule {
  id: string;
  name: string;
  project_id: string;
  /** Empty when the hub could not resolve the project. */
  project_slug: string;
  event_type: string;
  /** The agent the schedule addresses; empty when the payload names none. */
  target: string;
  /** creator_deleted | cron_zone_unsupported | other; a newer hub may add codes. */
  reason: string;
  /** RFC 3339 time of the schedule's last update; null when not recorded. */
  updated_at: string | null;
}

/** The schedules section of the summary; null when it could not be read. */
export interface HealthSummarySchedules {
  failing: HealthFailingSchedule[];
  /** Failing schedules visible to the caller; a lower bound when scan_limited. */
  total: number;
  truncated: boolean;
  /** True when the hub read only part of the failing schedules. */
  scan_limited: boolean;
  /** Schedules the hub paused; absent from an older hub, null when not reported. */
  paused?: HealthPausedSchedule[] | null;
  /** Hub-paused schedules visible to the caller; a lower bound when paused_scan_limited. */
  paused_total?: number;
  paused_truncated?: boolean;
  /** True when the hub read only part of the hub-paused schedules. */
  paused_scan_limited?: boolean;
  /** True when the hub could not list the hub-paused schedules (paused is then null). */
  paused_unavailable?: boolean;
}

/** Fixed text per known pause reason code. */
const PAUSED_REASON_LABELS: Readonly<Record<string, string>> = {
  creator_deleted: 'the agent or user it runs under was deleted',
  cron_zone_unsupported: 'its cron expression names a time zone, which is not supported',
};

/** Shown for a pause reason code this page does not know. */
export const PAUSED_REASON_GENERIC = 'reason not recognized';

/**
 * Fixed, human-readable text for a pause reason code; the generic label
 * for any other value. The code itself is never returned.
 */
export function pausedReasonLabel(reason: string | null | undefined): string {
  if (
    typeof reason === 'string' &&
    Object.prototype.hasOwnProperty.call(PAUSED_REASON_LABELS, reason)
  ) {
    return PAUSED_REASON_LABELS[reason];
  }
  return PAUSED_REASON_GENERIC;
}

/** Longest last error, in characters, a schedule detail line shows. */
export const SCHEDULE_ERROR_MAX_CHARS = 160;

/**
 * Cuts text to at most max characters (code points, so a character is never
 * split), appending an ellipsis when it was longer.
 */
export function truncateText(text: string, max: number): string {
  const chars = Array.from(text);
  if (chars.length <= max) return text;
  return chars.slice(0, max).join('') + '\u2026';
}

/**
 * The page an item's subject links to, or null when the subject is not a
 * single linkable resource or does not carry its ID. Integration subjects
 * link only when integrationsDetail is true.
 */
export function attentionHref(
  item: HealthAttentionItem,
  integrationsDetail: boolean
): string | null {
  const id = item.subject?.id;
  if (!id) return null;
  switch (item.subject.type) {
    case 'runtime_broker':
      return `/brokers/${encodeURIComponent(id)}`;
    case 'agent':
      return `/agents/${encodeURIComponent(id)}`;
    case 'hub':
      // An item about one hub instance carries its ID and label. Hub items
      // with an ID alone (the serving instance's own conditions) and
      // fleet-wide items are not linked.
      if (!item.subject.name) return null;
      return `#${hubInstanceAnchor(id)}`;
    case 'integration':
      if (!integrationsDetail) return null;
      return `${INTEGRATIONS_PAGE}/${encodeURIComponent(id)}`;
    case 'schedule':
      // Schedules have no page of their own: link to the project's list.
      if (!item.subject.project_id) return null;
      return `/projects/${encodeURIComponent(item.subject.project_id)}/schedules`;
    default:
      return null;
  }
}

/** Icon and accessible label per severity; both icons are in USED_ICONS. */
function severityIcon(severity: string): { name: string; label: string } {
  return severity === 'critical'
    ? { name: 'exclamation-octagon', label: 'Critical' }
    : { name: 'exclamation-triangle', label: 'Warning' };
}

@customElement('scion-health-attention')
export class ScionHealthAttention extends LitElement {
  /** The summary's attention list; null when the response had none. */
  @property({ attribute: false })
  items: HealthAttentionItem[] | null = null;

  /** The summary's integrations_detail; integration items link only when true. */
  @property({ attribute: false })
  integrationsDetail = false;

  /** The summary's schedules.failing; detail lines for schedule items. */
  @property({ attribute: false })
  failingSchedules: HealthFailingSchedule[] | null = null;

  /** The summary's schedules.paused; detail lines for hub-paused schedule items. */
  @property({ attribute: false })
  pausedSchedules: HealthPausedSchedule[] | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .card {
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
      padding: 1.25rem;
    }

    .card-title {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text-muted);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin: 0 0 0.75rem 0;
    }

    ul {
      margin: 0;
      padding: 0;
      list-style: none;
    }

    li {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      padding: 0.375rem 0;
      font-size: 0.875rem;
      color: var(--scion-text);
      border-bottom: 1px solid var(--scion-border);
      overflow-wrap: anywhere;
    }

    li:last-child {
      border-bottom: none;
    }

    sl-icon {
      flex: none;
      font-size: 1rem;
      margin-top: 0.0625rem;
    }

    .sev-critical sl-icon {
      color: var(--scion-badge-danger-text);
    }

    .sev-warning sl-icon {
      color: var(--scion-badge-warning-text);
    }

    a {
      color: var(--scion-text);
      text-decoration: underline;
      text-decoration-color: var(--scion-border-hover);
      text-underline-offset: 2px;
    }

    a:hover {
      text-decoration-color: currentColor;
    }

    .body {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
      min-width: 0;
    }

    .detail {
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
    }

    .last-error {
      font-family: var(--scion-font-mono, monospace);
    }

    .empty {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.875rem;
      color: var(--scion-text-muted);
    }

    .empty sl-icon {
      color: var(--scion-badge-success-text);
      margin-top: 0;
    }
  `;

  override render(): TemplateResult {
    return html`
      <section class="card" aria-labelledby="attention-title">
        <div class="card-title" id="attention-title">Needs attention</div>
        ${this.renderBody()}
      </section>
    `;
  }

  private renderBody(): TemplateResult {
    const items = this.items;
    if (!items) {
      return html`<div class="empty">Attention data not available</div>`;
    }
    if (items.length === 0) {
      return html`<div class="empty">
        <sl-icon name="check-circle" aria-hidden="true"></sl-icon>Nothing needs attention.
      </div>`;
    }
    return html`<ul>
      ${items.map((it) => this.renderItem(it))}
    </ul>`;
  }

  private renderItem(it: HealthAttentionItem): TemplateResult {
    const icon = severityIcon(it.severity);
    const href = attentionHref(it, this.integrationsDetail);
    return html`<li
      class="sev-${it.severity === 'critical' ? 'critical' : 'warning'}"
      data-kind=${it.kind}
    >
      <sl-icon name=${icon.name} label=${icon.label}></sl-icon>
      <span class="body">
        <span class="message">${href ? this.renderLink(it, href) : it.message}</span>
        ${this.renderScheduleDetail(it)}
      </span>
    </li>`;
  }

  /**
   * The detail line of a failing schedule item, or nothing when the item is
   * not about one schedule or the section has no row for it. Every value is
   * bound as text or as an attribute value.
   */
  private renderScheduleDetail(it: HealthAttentionItem): TemplateResult | null {
    if (it.subject?.type !== 'schedule' || !it.subject.id) return null;
    const row = this.failingSchedules?.find((s) => s.id === it.subject.id);
    if (!row) return this.renderPausedDetail(it.subject.id);
    const parts: string[] = [];
    if (row.target) parts.push(`Target: ${row.target}`);
    parts.push(`${row.error_count} of ${row.run_count} runs failed`);
    const when = row.last_failure_at ? formatInstantWithZone(row.last_failure_at) : '';
    if (when) parts.push(`last failure ${when}`);
    const err = row.last_error ?? '';
    return html`<span class="detail"
      >${parts.join(' \u00b7 ')}${err
        ? html`<br /><span class="last-error" title=${err}
              >Last error: ${truncateText(err, SCHEDULE_ERROR_MAX_CHARS)}</span
            >`
        : null}</span
    >`;
  }

  /**
   * The detail line of a hub-paused schedule item, or nothing when the
   * section has no row for it. The reason is fixed text (pausedReasonLabel);
   * every value is bound as text.
   */
  private renderPausedDetail(id: string): TemplateResult | null {
    const row = this.pausedSchedules?.find((s) => s.id === id);
    if (!row) return null;
    const parts: string[] = [`Paused by the hub: ${pausedReasonLabel(row.reason)}`];
    if (row.target) parts.push(`Target: ${row.target}`);
    const when = row.updated_at ? formatInstantWithZone(row.updated_at) : '';
    if (when) parts.push(`updated ${when}`);
    return html`<span class="detail paused-detail">${parts.join(' \u00b7 ')}</span>`;
  }

  /** A hub instance link moves within the page (followHubInstanceLink). */
  private renderLink(it: HealthAttentionItem, href: string): TemplateResult {
    const id = it.subject.id;
    if (it.subject.type === 'hub' && id) {
      return html`<a href=${href} @click=${(e: MouseEvent) => followHubInstanceLink(e, id)}
        >${it.message}</a
      >`;
    }
    return html`<a href=${href}>${it.message}</a>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-health-attention': ScionHealthAttention;
  }
}
