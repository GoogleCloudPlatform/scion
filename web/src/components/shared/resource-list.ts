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
 * Shared resource list component
 *
 * Lists file-based resources (templates or harness-configs) for a given scope
 * and links each one to its detail/editor page. Used by both the project
 * settings Resources section and the Hub Resources page so the two render
 * identically.
 *
 * It does not handle import/creation — those affordances (where they exist,
 * e.g. template import) are rendered by the host page around this list.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch } from '../../client/api.js';

export type ResourceKind = 'template' | 'harness-config';

interface ResourceItem {
  id: string;
  name: string;
  displayName?: string;
  description?: string;
  harness?: string;
}

@customElement('scion-resource-list')
export class ScionResourceList extends LitElement {
  /** Which resource type to list. */
  @property({ type: String })
  kind: ResourceKind = 'template';

  /** Resource scope: 'project' or 'global'. */
  @property({ type: String })
  scope: 'project' | 'global' = 'project';

  /** Scope id (project id) — required for project scope, omitted for global. */
  @property({ type: String })
  scopeId = '';

  /**
   * Base path for the detail link. The resource segment + id are appended,
   * e.g. `${detailBasePath}/harness-configs/{id}`.
   * Project pages pass `/projects/{id}`; the Hub Resources page passes `/settings`.
   */
  @property({ type: String })
  detailBasePath = '';

  @state() private items: ResourceItem[] = [];
  @state() private loading = true;
  @state() private error: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .resource-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .resource-item {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem 1rem;
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      text-decoration: none;
      color: inherit;
      cursor: pointer;
    }

    .resource-item:hover {
      border-color: var(--scion-primary, #3b82f6);
    }

    .resource-item > sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.125rem;
      flex-shrink: 0;
    }

    .resource-info {
      flex: 1;
      min-width: 0;
    }

    .resource-name {
      font-weight: 600;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .resource-meta {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.125rem;
    }

    .resource-badge {
      font-size: 0.6875rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      border: 1px solid var(--scion-border, #e2e8f0);
      white-space: nowrap;
    }

    .empty {
      text-align: center;
      padding: 2rem 1rem;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    .empty sl-icon {
      font-size: 2rem;
      margin-bottom: 0.5rem;
      display: block;
    }

    .error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border-radius: var(--scion-radius, 0.5rem);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.load();
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('kind') || changed.has('scope') || changed.has('scopeId')) {
      void this.load();
    }
  }

  private get apiResource(): string {
    return this.kind === 'template' ? 'templates' : 'harness-configs';
  }

  private get detailSegment(): string {
    return this.kind === 'template' ? 'templates' : 'harness-configs';
  }

  private get icon(): string {
    return this.kind === 'template' ? 'file-earmark-code' : 'sliders';
  }

  /** Public method to refresh the list. */
  async load(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const params = new URLSearchParams({ status: 'active', limit: '100' });
      if (this.scope) params.set('scope', this.scope);
      // scopeId narrows to a single project (both template and harness-config
      // list handlers filter on scope_id); without it scope=project would match
      // every project's resources.
      if (this.scope === 'project' && this.scopeId) params.set('scopeId', this.scopeId);

      const response = await apiFetch(`/api/v1/${this.apiResource}?${params.toString()}`);
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      const data = (await response.json()) as Record<string, ResourceItem[]>;
      const list = this.kind === 'template' ? data.templates : data.harnessConfigs;
      this.items = Array.isArray(list) ? list : [];
    } catch (err) {
      console.error(`Failed to load ${this.apiResource}:`, err);
      this.error = err instanceof Error ? err.message : `Failed to load ${this.apiResource}`;
    } finally {
      this.loading = false;
    }
  }

  override render() {
    if (this.loading) {
      return html`<div class="empty"><sl-spinner></sl-spinner></div>`;
    }
    if (this.error) {
      return html`<div class="error">${this.error}</div>`;
    }
    if (this.items.length === 0) {
      return this.renderEmpty();
    }

    return html`
      <div class="resource-list">${this.items.map((item) => this.renderItem(item))}</div>
    `;
  }

  private renderItem(item: ResourceItem) {
    return html`
      <a href="${this.detailBasePath}/${this.detailSegment}/${item.id}" class="resource-item">
        <sl-icon name=${this.icon}></sl-icon>
        <div class="resource-info">
          <div class="resource-name">${item.displayName || item.name}</div>
          ${item.description ? html`<div class="resource-meta">${item.description}</div>` : ''}
        </div>
        ${item.harness ? html`<span class="resource-badge">${item.harness}</span>` : ''}
        <sl-icon
          name="chevron-right"
          style="color: var(--sl-color-neutral-400); font-size: 0.875rem;"
        ></sl-icon>
      </a>
    `;
  }

  private renderEmpty() {
    const label = this.kind === 'template' ? 'templates' : 'harness configs';
    return html`
      <div class="empty">
        <sl-icon name="file-earmark"></sl-icon>
        <p>No ${this.scope === 'global' ? 'global' : 'project'} ${label} yet.</p>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-resource-list': ScionResourceList;
  }
}
