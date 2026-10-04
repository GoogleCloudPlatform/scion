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
 * Failure banner with Retry and Force for a failed agent delete
 * (ptone/scion#2483 phase 2, design R2/R3).
 *
 * It renders only for an effective `failed` view (pass
 * `DeletionLeaseController.view(agent)`, so a client-flipped abandoned view
 * shows too) and nothing otherwise; in particular Force is never offered on
 * a live `deleting` view (live pre-emption is phase 2b). It owns no delete
 * logic: Retry and Force dispatch `deletion-retry` and `deletion-force`
 * events, and the page runs them through the shared delete helper
 * (`client/agent-delete.ts`).
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import type { DeletionInfo } from '../../shared/types.js';
import {
  DELETION_START_BLOCKED_HINT,
  deletionBannerText,
  deletionBlocksStart,
} from '../../shared/agent-deletion.js';

@customElement('scion-deletion-banner')
export class ScionDeletionBanner extends LitElement {
  /** The effective deletion view; the banner shows only for `failed`. */
  @property({ attribute: false })
  deletion: DeletionInfo | null = null;

  /** Show Retry and Force (the viewer has the `delete` capability). */
  @property({ type: Boolean, attribute: 'can-delete' })
  canDelete = false;

  /**
   * The agent's name, for the buttons' accessible labels ("Retry delete of
   * <name>", "Force delete <name>"), so a list of failed rows reads
   * distinctly to assistive tech.
   */
  @property({ type: String, attribute: 'agent-name' })
  agentName = '';

  /** A delete request for this agent is in flight; disable the buttons. */
  @property({ type: Boolean })
  busy = false;

  /**
   * Dense form for table rows: one line of text plus small buttons, with
   * the full explanation in `title`. Cards and the detail header use the
   * full form.
   */
  @property({ type: Boolean })
  compact = false;

  /**
   * Announce the banner (`role="alert"`). Set it only on a page's single
   * primary banner (agent-detail's header), so a list of failed rows does
   * not flood assistive tech.
   */
  @property({ type: Boolean })
  live = false;

  static override styles = css`
    :host {
      display: block;
      min-width: 0;
    }

    :host([hidden]) {
      display: none;
    }

    .banner {
      display: flex;
      gap: 0.625rem;
      align-items: flex-start;
      padding: 0.625rem 0.75rem;
      border: 1px solid var(--scion-badge-danger-border, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-badge-danger-bg, #fee2e2);
      color: var(--scion-badge-danger-text, #991b1b);
      font-size: 0.875rem;
    }

    .banner > sl-icon {
      flex: none;
      margin-top: 0.125rem;
      font-size: 1rem;
    }

    .body {
      flex: 1;
      min-width: 0;
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }

    .title {
      font-weight: 600;
      overflow-wrap: anywhere;
    }

    .detail {
      overflow-wrap: anywhere;
    }

    .actions {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
      margin-top: 0.25rem;
    }

    .compact {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
      color: var(--scion-badge-danger-text, #991b1b);
      font-size: 0.8125rem;
      max-width: 18rem;
    }

    .compact .line {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      min-width: 0;
    }

    .compact .title {
      font-weight: 500;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    /* The full explanation for keyboard and screen-reader users on dense
       rows, where it is otherwise only in the hover title. */
    .visually-hidden {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip: rect(0 0 0 0);
      clip-path: inset(50%);
      white-space: nowrap;
    }

    .compact .hint {
      font-size: 0.75rem;
    }

    .compact .actions {
      margin-top: 0;
    }
  `;

  protected override willUpdate(): void {
    this.toggleAttribute('hidden', this.deletion?.state !== 'failed');
  }

  private emit(name: 'deletion-retry' | 'deletion-force', e: Event): void {
    // Cards and rows may react to clicks themselves (navigation); the
    // banner's buttons act only through these events.
    e.stopPropagation();
    this.dispatchEvent(new CustomEvent(name));
  }

  private renderActions(): TemplateResult | typeof nothing {
    if (!this.canDelete) return nothing;
    return html`
      <div class="actions">
        <sl-button
          class="retry"
          size="small"
          aria-label=${this.agentName ? `Retry delete of ${this.agentName}` : 'Retry delete'}
          ?disabled=${this.busy}
          ?loading=${this.busy}
          @click=${(e: Event): void => this.emit('deletion-retry', e)}
        >
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
        <sl-button
          class="force"
          size="small"
          aria-label=${this.agentName ? `Force delete ${this.agentName}` : 'Force delete'}
          variant="danger"
          outline
          ?disabled=${this.busy}
          @click=${(e: Event): void => this.emit('deletion-force', e)}
        >
          <sl-icon slot="prefix" name="trash"></sl-icon>
          Force delete
        </sl-button>
      </div>
    `;
  }

  override render(): TemplateResult | typeof nothing {
    const d = this.deletion;
    if (!d || d.state !== 'failed') return nothing;
    const { title, detail } = deletionBannerText(d);
    const blocked = deletionBlocksStart(d);
    if (this.compact) {
      return html`
        <div class="compact" data-code=${d.code ?? ''} title=${`${title}. ${detail}`}>
          <span class="line">
            <sl-icon name="exclamation-triangle"></sl-icon>
            <span class="title">${title}</span>
          </span>
          ${blocked ? html`<span class="hint">${DELETION_START_BLOCKED_HINT}</span>` : nothing}
          <span class="detail visually-hidden">${detail}</span>
          ${this.renderActions()}
        </div>
      `;
    }
    return html`
      <div class="banner" role=${this.live ? 'alert' : nothing} data-code=${d.code ?? ''}>
        <sl-icon name="exclamation-triangle"></sl-icon>
        <div class="body">
          <span class="title">${title}</span>
          <span class="detail">${detail}</span>
          ${this.renderActions()}
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-deletion-banner': ScionDeletionBanner;
  }
}
