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
 */

import { LitElement, html, css, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import { INTEGRATIONS_PAGE } from './health-integrations.js';
import { hubInstanceAnchor } from './health-hub-card.js';

/** One entry of the summary's ranked "Needs attention" list (server-composed). */
export interface HealthAttentionItem {
  severity: 'critical' | 'warning';
  /** hub_check | hub_instance | broker_offline | broker_degraded | broker_nfs | integration | dispatch | agents */
  kind: string;
  /** What the item is about; fields that do not apply are omitted. */
  subject: { type: string; id?: string; name?: string; project_id?: string };
  /** Fixed, server-composed sentence. */
  message: string;
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
      <span class="message">${href ? html`<a href=${href}>${it.message}</a>` : it.message}</span>
    </li>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-health-attention': ScionHealthAttention;
  }
}
