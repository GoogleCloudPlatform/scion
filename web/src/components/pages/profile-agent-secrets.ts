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
 * Profile Agent Secrets page (`/profile/agent-secrets`, ptone/scion#9299, design §5.5).
 *
 * Built on the reusable `<scion-action-inbox-page>` component and
 * `ActionInboxKindConfig` pattern. Lists secret requests across all of the
 * user's projects and DMs via `GET /api/v1/users/me/secret-requests`, and
 * renders each card with `<scion-chat-secret-request surface="standalone_page">`
 * using its `.card` property (no per-card fetch).
 */

import { LitElement, css, html, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { navigateTo } from '../../client/navigation.js';
import {
  isPendingSecretRequest,
  newerSecretRequestView,
  type SecretRequestCardResponse,
  type SecretRequestView,
} from '../../client/secret-requests.js';
import { agentSecretsEnabled } from '../../utils/feature-flags.js';
import type { ActionInboxKindConfig } from '../shared/action-inbox-controller.js';
import '../shared/action-inbox-page.js';
import '../shared/chat/chat-secret-request.js';

export const AGENT_SECRETS_API_PATH = '/api/v1/users/me/secret-requests';

/**
 * Merges two card fragments for the same `messageId` when a multi-row card
 * spans adjacent pages on the Resolved tab.
 */
export function mergeSecretRequestCardFragments(
  existing: SecretRequestCardResponse,
  incoming: SecretRequestCardResponse
): SecretRequestCardResponse {
  const byRowId = new Map<string, SecretRequestView>();
  for (const row of existing.rows) {
    byRowId.set(row.rowId, row);
  }
  for (const row of incoming.rows) {
    const prev = byRowId.get(row.rowId);
    byRowId.set(row.rowId, prev ? newerSecretRequestView(prev, row) : row);
  }
  const mergedRows = [...byRowId.values()].sort((a, b) => a.position - b.position);
  const projectName = incoming.projectName || existing.projectName;
  const intro = incoming.intro || existing.intro;
  return {
    ...existing,
    ...incoming,
    ...(projectName !== undefined ? { projectName } : {}),
    ...(intro !== undefined ? { intro } : {}),
    canRespond: existing.canRespond || incoming.canRespond,
    rows: mergedRows,
  };
}

@customElement('scion-page-profile-agent-secrets')
export class ScionPageProfileAgentSecrets extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  private readonly kindConfig: ActionInboxKindConfig<SecretRequestCardResponse> = {
    kind: 'secret_request',
    tabId: 'agent-secrets',
    apiPath: AGENT_SECRETS_API_PATH,
    title: 'Agent Secrets',
    descriptor: 'Secret requests from agents across your projects and DMs',
    searchPlaceholder: 'Filter by key, description, or agent…',
    isEnabled: (): boolean => agentSecretsEnabled(),
    getCardId: (card): string => card.messageId,
    mergeCard: mergeSecretRequestCardFragments,
    renderCard: (card, ctx): TemplateResult => this.renderSecretRequestCard(card, ctx.onResolved),
  };

  static override styles = css`
    :host {
      display: block;
    }

    .card-wrapper {
      display: flex;
      flex-direction: column;
      gap: 0.45rem;
    }

    .card-context {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.4rem 0.9rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
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

    .waiting-others-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.3rem;
      padding: 0.1rem 0.5rem;
      border-radius: var(--scion-radius-full);
      font-size: 0.75rem;
      font-weight: 500;
      background: var(--scion-warning-bg);
      color: var(--scion-warning-text);
    }

    .card-intro {
      margin: 0;
      padding: 0.6rem 0.8rem;
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      color: var(--scion-text);
      font-size: 0.875rem;
      white-space: pre-wrap;
      overflow-wrap: anywhere;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    dispatchPageTitle(this, 'Agent Secrets', 'Profile');
  }

  private handleJump(e: CustomEvent<{ messageId: string }>): void {
    const targetId = e.detail?.messageId;
    if (!targetId) return;
    navigateTo(`/secret-requests/${encodeURIComponent(targetId)}`);
  }

  private renderSecretRequestCard(
    card: SecretRequestCardResponse,
    onResolved: () => void
  ): TemplateResult {
    const hasPending = card.rows.some((r) => isPendingSecretRequest(r));
    const waitingOnOthers = hasPending && !card.canRespond;

    return html`
      <article class="card-wrapper" data-secret-request-card=${card.messageId}>
        <div class="card-context" data-card-context>
          ${card.projectName || card.projectId
            ? html`
                <span class="context-item">
                  <sl-icon name="folder" aria-hidden="true"></sl-icon>
                  <strong>${card.projectName || card.projectId}</strong>
                </span>
              `
            : nothing}
          ${card.agentName
            ? html`
                <span class="context-item">
                  <sl-icon name="cpu" aria-hidden="true"></sl-icon>
                  <strong>${card.agentName}</strong>
                </span>
              `
            : nothing}
          ${waitingOnOthers
            ? html`
                <span class="waiting-others-badge" data-waiting-on-others>
                  <sl-icon name="clock" aria-hidden="true"></sl-icon>
                  Waiting on someone else
                </span>
              `
            : nothing}
        </div>
        ${card.intro ? html`<p class="card-intro">${card.intro}</p>` : nothing}
        <scion-chat-secret-request
          .card=${card}
          .currentUserId=${this.pageData?.user?.id ?? ''}
          surface="standalone_page"
          @secret-request-fulfilled=${(): void => onResolved()}
          @secret-request-dismissed=${(): void => onResolved()}
          @secret-request-jump=${(e: CustomEvent<{ messageId: string }>): void =>
            this.handleJump(e)}
        ></scion-chat-secret-request>
      </article>
    `;
  }

  override render(): TemplateResult {
    return html`
      <scion-action-inbox-page
        .config=${this.kindConfig}
        .pageData=${this.pageData}
      ></scion-action-inbox-page>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-agent-secrets': ScionPageProfileAgentSecrets;
  }
}
