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
 * Standalone secret request page (`/secret-requests/:messageId`, ptone/scion#8747).
 *
 * Gated on `hub.agent_secrets`: renders `<scion-page-404>` and makes zero API
 * requests when the experiment is off.
 *
 * Loads the card from `GET /api/v1/secret-requests/{messageId}`, subscribes to
 * live updates via `stateManager`, displays a replacement banner with a link to
 * the current version when opened via an old link, and renders
 * `<scion-chat-secret-request surface="standalone_page">`.
 */

import { LitElement, css, html, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import type { PageData } from '../../shared/types.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { navigateTo } from '../../client/navigation.js';
import { stateManager } from '../../client/state.js';
import { FEATURE_FLAGS_CHANGED_EVENT, agentSecretsEnabled } from '../../utils/feature-flags.js';
import {
  fetchSecretRequestCard,
  type SecretRequestCardResponse,
  type SecretRequestReplacedBy,
} from '../../client/secret-requests.js';
import type {
  SecretRequestDismissedDetail,
  SecretRequestFulfilledDetail,
  SecretRequestRefreshedDetail,
  SecretRequestUpdatedDetail,
} from '../shared/chat/chat-secret-request.js';
import '../shared/chat/chat-secret-request.js';
import './not-found.js';

@customElement('scion-page-secret-request')
export class ScionPageSecretRequest extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  /** Optional direct override (otherwise extracted from the URL). */
  @property({ type: String })
  messageId = '';

  @state()
  private resolvedMessageId = '';

  @state()
  private loading = true;

  @state()
  private errorStatus = 0;

  @state()
  private errorMessage = '';

  @state()
  private card: SecretRequestCardResponse | null = null;

  private abortController: AbortController | null = null;

  private readonly onFlagsChanged = (): void => {
    this.requestUpdate();
    if (agentSecretsEnabled() && !this.card && !this.loading && this.resolvedMessageId) {
      void this.loadCard();
    }
  };

  static override styles = css`
    :host {
      display: block;
    }

    .page {
      max-width: 44rem;
      margin: 0 auto;
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      color: var(--scion-text-secondary);
      text-decoration: none;
      font-size: 0.875rem;
      width: fit-content;
    }

    .back-link:hover {
      color: var(--scion-primary);
    }

    .context-bar {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text-secondary);
    }

    .context-item {
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
    }

    .context-item strong {
      color: var(--scion-text);
      font-weight: 600;
    }

    .intro {
      margin: 0;
      padding: 0.75rem 0.9rem;
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      color: var(--scion-text);
      white-space: pre-wrap;
      overflow-wrap: anywhere;
    }

    .replacement-banner {
      display: flex;
      align-items: flex-start;
      gap: 0.6rem;
      padding: 0.75rem 0.9rem;
      background: var(--scion-warning-bg);
      border: 1px solid var(--scion-warning-border);
      border-radius: var(--scion-radius);
      color: var(--scion-warning-text);
      font-size: 0.875rem;
    }

    .replacement-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.15rem;
      font-size: 1rem;
    }

    .replacement-links {
      display: flex;
      flex-wrap: wrap;
      gap: 0.35rem 0.75rem;
      margin-top: 0.25rem;
    }

    .replacement-link {
      color: var(--scion-primary);
      font-weight: 600;
      text-decoration: none;
    }

    .replacement-link:hover {
      text-decoration: underline;
    }

    .state-card {
      padding: 2rem 1.5rem;
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
      display: flex;
      flex-direction: column;
      align-items: center;
      text-align: center;
      gap: 0.75rem;
      color: var(--scion-text-secondary);
    }

    .state-card sl-icon {
      font-size: 2rem;
      color: var(--scion-text-muted);
    }

    .state-card.error sl-icon {
      color: var(--scion-status-danger);
    }

    .state-card h2 {
      margin: 0;
      font-size: 1.125rem;
      color: var(--scion-text);
    }

    .state-card p {
      margin: 0;
      max-width: 32rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
    this.resolvedMessageId = this.extractMessageId();
    if (agentSecretsEnabled()) {
      void this.loadCard();
    } else {
      this.loading = false;
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
    this.abortController?.abort();
    this.abortController = null;
  }

  protected override updated(changed: PropertyValues): void {
    if (changed.has('pageData') || changed.has('messageId')) {
      const nextId = this.extractMessageId();
      if (nextId !== this.resolvedMessageId) {
        this.resolvedMessageId = nextId;
        if (agentSecretsEnabled()) {
          void this.loadCard();
        }
      }
    }
  }

  private extractMessageId(): string {
    if (this.messageId) return this.messageId;
    const rawPath = this.pageData?.path ?? window.location.pathname;
    const pathname = rawPath.split(/[?#]/, 1)[0];
    const match = pathname.match(/^\/secret-requests\/([^/]+)$/);
    if (!match?.[1]) return '';
    try {
      return decodeURIComponent(match[1]);
    } catch {
      return '';
    }
  }

  private async loadCard(): Promise<void> {
    this.abortController?.abort();
    if (!agentSecretsEnabled()) {
      this.loading = false;
      return;
    }
    if (!this.resolvedMessageId) {
      this.loading = false;
      this.errorStatus = 400;
      this.errorMessage = 'Invalid secret request link.';
      this.card = null;
      return;
    }

    const controller = new AbortController();
    this.abortController = controller;
    this.loading = true;
    this.errorStatus = 0;
    this.errorMessage = '';
    this.card = null;

    const result = await fetchSecretRequestCard(this.resolvedMessageId, {
      signal: controller.signal,
    });
    if (controller.signal.aborted) return;

    this.loading = false;
    if (!result.ok) {
      this.errorStatus = result.status;
      this.errorMessage = result.message;
      return;
    }

    this.card = result.card;
    if (result.card.projectId) {
      stateManager.setScope({ type: 'project', projectId: result.card.projectId });
    }
    dispatchPageTitle(
      this,
      result.card.agentName ? `Secret Request · ${result.card.agentName}` : 'Secret Request'
    );
  }

  private handleCardRowsChanged(
    e: CustomEvent<
      SecretRequestFulfilledDetail | SecretRequestDismissedDetail | SecretRequestUpdatedDetail
    >
  ): void {
    if (!this.card || !e.detail?.rows) return;
    this.card = { ...this.card, rows: e.detail.rows };
  }

  private handleCardRefreshed(e: CustomEvent<SecretRequestRefreshedDetail>): void {
    if (!e.detail?.card) return;
    this.card = e.detail.card;
  }

  private handleJump(e: CustomEvent<{ messageId: string }>): void {
    const targetId = e.detail?.messageId;
    if (!targetId) return;
    navigateTo(`/secret-requests/${encodeURIComponent(targetId)}`);
  }

  /** Collects distinct replacement targets from replaced rows on this card. */
  private get replacementTargets(): SecretRequestReplacedBy[] {
    if (!this.card) return [];
    const seen = new Map<string, SecretRequestReplacedBy>();
    for (const row of this.card.rows) {
      if (row.status === 'replaced' && row.replacedBy?.messageId) {
        if (!seen.has(row.replacedBy.messageId)) {
          seen.set(row.replacedBy.messageId, row.replacedBy);
        }
      }
    }
    return [...seen.values()];
  }

  private renderReplacementBanner(): TemplateResult | typeof nothing {
    const targets = this.replacementTargets;
    if (targets.length === 0) return nothing;

    const allReplaced = Boolean(
      this.card && this.card.rows.length > 0 && this.card.rows.every((r) => r.status === 'replaced')
    );

    return html`
      <div class="replacement-banner" role="status">
        <sl-icon name="arrow-repeat" aria-hidden="true"></sl-icon>
        <div>
          <div>
            ${allReplaced
              ? 'This secret request has been replaced by a newer version.'
              : 'One or more keys on this secret request have been replaced by a newer version.'}
          </div>
          <div class="replacement-links">
            ${targets.map(
              (target) => html`
                <a
                  class="replacement-link"
                  href=${`/secret-requests/${encodeURIComponent(target.messageId)}`}
                >
                  Open current version (v${target.version})
                </a>
              `
            )}
          </div>
        </div>
      </div>
    `;
  }

  override render(): TemplateResult {
    if (!agentSecretsEnabled()) {
      return html`<scion-page-404></scion-page-404>`;
    }

    if (this.loading) {
      return html`
        <div class="page">
          <div class="state-card" role="status">
            <sl-spinner></sl-spinner>
            <span>Loading secret request…</span>
          </div>
        </div>
      `;
    }

    if (this.errorMessage || !this.card) {
      const heading =
        this.errorStatus === 404
          ? 'Secret Request Not Found'
          : this.errorStatus === 403
            ? 'Access Denied'
            : 'Unable to Load Secret Request';
      return html`
        <div class="page">
          <a class="back-link" href="/">
            <sl-icon name="arrow-left" aria-hidden="true"></sl-icon>
            Back to Dashboard
          </a>
          <div class="state-card error" role="alert">
            <sl-icon name="exclamation-circle" aria-hidden="true"></sl-icon>
            <h2>${heading}</h2>
            <p>${this.errorMessage || 'This secret request could not be found.'}</p>
            ${this.errorStatus !== 404 && this.errorStatus !== 403
              ? html`<sl-button size="small" @click=${(): void => void this.loadCard()}>
                  Retry
                </sl-button>`
              : nothing}
          </div>
        </div>
      `;
    }

    return html`
      <div class="page">
        <a class="back-link" href="/">
          <sl-icon name="arrow-left" aria-hidden="true"></sl-icon>
          Back to Dashboard
        </a>

        <div class="context-bar">
          <span class="context-item">
            <sl-icon name="cpu" aria-hidden="true"></sl-icon>
            Agent: <strong>${this.card.agentName}</strong>
          </span>
          ${this.card.projectName
            ? html`<span class="context-item">
                <sl-icon name="folder" aria-hidden="true"></sl-icon>
                Project: <strong>${this.card.projectName}</strong>
              </span>`
            : nothing}
        </div>

        ${this.renderReplacementBanner()}
        ${this.card.intro ? html`<p class="intro">${this.card.intro}</p>` : nothing}

        <scion-chat-secret-request
          .messageId=${this.card.messageId}
          .rows=${this.card.rows}
          .agentName=${this.card.agentName}
          .projectName=${this.card.projectName ?? ''}
          .threadId=${this.card.threadId ?? ''}
          .currentUserId=${this.pageData?.user?.id ?? ''}
          .canRespond=${this.card.canRespond}
          .reason=${this.card.reason ?? ''}
          surface="standalone_page"
          @secret-request-fulfilled=${(e: CustomEvent<SecretRequestFulfilledDetail>): void =>
            this.handleCardRowsChanged(e)}
          @secret-request-dismissed=${(e: CustomEvent<SecretRequestDismissedDetail>): void =>
            this.handleCardRowsChanged(e)}
          @secret-request-updated=${(e: CustomEvent<SecretRequestUpdatedDetail>): void =>
            this.handleCardRowsChanged(e)}
          @secret-request-refreshed=${(e: CustomEvent<SecretRequestRefreshedDetail>): void =>
            this.handleCardRefreshed(e)}
          @secret-request-jump=${(e: CustomEvent<{ messageId: string }>): void =>
            this.handleJump(e)}
        ></scion-chat-secret-request>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-secret-request': ScionPageSecretRequest;
  }
}
