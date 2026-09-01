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
 * Effective Access Boundary Notice
 *
 * A neutral notice indicating that effective access may be reduced by access
 * boundaries. Used on:
 * - User detail pages (link to explain with user principal context)
 * - Agent detail pages (link to explain with agent principal context)
 * - Project detail/settings (link to boundaries affecting this project)
 * - Role bindings page (summary outside binding rows)
 *
 * Fetches the count of applicable boundaries from the explain API and
 * displays a notice with a link to the explain view. Handles redacted
 * boundaries by showing "Access boundary (details unavailable)" with reason.
 *
 * TERMINOLOGY: layers are descriptive. Never "priority", "override", "winner".
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch } from '../../client/api.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface BoundaryCountResponse {
  count?: number;
  boundaries?: Array<{
    id: string;
    name?: string | null;
    status?: string;
    redacted?: { message?: string; reason?: string };
  }>;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-effective-access-boundary-notice')
export class ScionEffectiveAccessBoundaryNotice extends LitElement {
  /** Context type: 'user', 'agent', or 'project'. */
  @property() contextType: 'user' | 'agent' | 'project' = 'user';

  /** ID of the principal or project. */
  @property() contextId = '';

  /** Whether to show as inline text (for embedding in role binding rows). */
  @property({ type: Boolean }) inline = false;

  @state() private boundaryCount = 0;
  @state() private loading = true;
  @state() private loaded = false;

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .notice {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.5rem 0.75rem;
        background: var(--sl-color-neutral-50, #f8fafc);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
      }

      .notice.inline {
        background: transparent;
        border: none;
        padding: 0.25rem 0;
        font-size: 0.75rem;
      }

      .notice sl-icon {
        font-size: 0.875rem;
        color: var(--sl-color-warning-500, #f59e0b);
        flex-shrink: 0;
      }

      .notice-text {
        flex: 1;
      }

      .notice-link {
        color: var(--sl-color-primary-600, #2563eb);
        text-decoration: none;
        font-weight: 500;
        white-space: nowrap;
      }

      .notice-link:hover {
        text-decoration: underline;
      }

      /* Zoom / touch targets */
      .notice-link {
        min-height: 44px;
        display: inline-flex;
        align-items: center;
      }

      .notice-text {
        overflow-wrap: anywhere;
      }

      /* Utility: screen-reader-only */

      /* Responsive: mobile full-width */
      @media (max-width: 768px) {
        .notice {
          border-radius: 0;
          margin-left: -0.75rem;
          margin-right: -0.75rem;
          padding: 0.625rem 0.75rem;
        }
      }

      /* High contrast mode */
      @media (forced-colors: active) {
        .notice {
          border: 1px solid ButtonText;
        }

        .notice-link {
          color: LinkText;
        }

        .notice sl-icon {
          color: ButtonText;
        }
      }
    `,
  ];

  private _initialLoadDone = false;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.contextId) {
      this._initialLoadDone = true;
      void this.loadBoundaryCount();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    if ((changed.has('contextId') || changed.has('contextType')) && this.contextId) {
      if (!this._initialLoadDone) {
        void this.loadBoundaryCount();
      }
      this._initialLoadDone = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private async loadBoundaryCount(): Promise<void> {
    if (!this.contextId) return;

    this.loading = true;
    try {
      let url: string;
      if (this.contextType === 'project') {
        url = `/api/v1/admin/access-constraints?scopeType=project&scopeId=${encodeURIComponent(this.contextId)}&pageSize=0`;
      } else {
        url = `/api/v1/admin/access-explain?principalType=${encodeURIComponent(this.contextType)}&principalId=${encodeURIComponent(this.contextId)}&summary=true`;
      }

      const res = await apiFetch(url);
      if (res.ok) {
        const data = (await res.json()) as BoundaryCountResponse;
        this.boundaryCount = data.count ?? data.boundaries?.length ?? 0;
        this.loaded = true;
      }
    } catch {
      // Silently fail — this is a non-critical enhancement
    } finally {
      this.loading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    if (this.loading || !this.loaded || this.boundaryCount === 0) {
      return nothing;
    }

    const explainHref = this.getExplainHref();

    return html`
      <div class="notice ${this.inline ? 'inline' : ''}" role="status">
        <sl-icon name="shield-exclamation"></sl-icon>
        <span class="notice-text">
          Effective access may be reduced by ${this.boundaryCount} access
          ${this.boundaryCount === 1 ? 'boundary' : 'boundaries'}
        </span>
        ${explainHref ? html`<a class="notice-link" href=${explainHref}>View details</a>` : nothing}
      </div>
    `;
  }

  private getExplainHref(): string | null {
    if (this.contextType === 'project') {
      return `/admin/access-boundaries?scopeType=project&scopeId=${encodeURIComponent(this.contextId)}`;
    }
    return `/admin/access-explain?principalType=${encodeURIComponent(this.contextType)}&principalId=${encodeURIComponent(this.contextId)}`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-effective-access-boundary-notice': ScionEffectiveAccessBoundaryNotice;
  }
}
