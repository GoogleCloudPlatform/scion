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
 * Profile Agent Executions page (`/profile/agent-executions`, ptone/scion#9698).
 *
 * Built on the reusable `<scion-action-inbox-page>` component and
 * `ActionInboxKindConfig` pattern. Lists command execution requests awaiting
 * the viewer's approval across all projects and DMs via
 * `GET /api/v1/users/me/command-requests`, and renders each card with
 * `<scion-chat-command-request surface="standalone_page">` using its `.detail`
 * property (no per-card fetch).
 */

import { LitElement, css, html, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { navigateTo } from '../../client/navigation.js';
import type { CommandRequestDetailResponse } from '../../client/command-requests.js';
import { commandRequestsEnabled } from '../../utils/feature-flags.js';
import type { ActionInboxKindConfig } from '../shared/action-inbox-controller.js';
import {
  actionCardChatHref,
  actionCardContextStyles,
  renderOpenInChat,
} from '../shared/action-inbox-page.js';
import '../shared/chat/chat-command-request.js';

export const AGENT_EXECUTIONS_API_PATH = '/api/v1/users/me/command-requests';

@customElement('scion-page-profile-agent-executions')
export class ScionPageProfileAgentExecutions extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  private readonly kindConfig: ActionInboxKindConfig<CommandRequestDetailResponse> = {
    kind: 'command_request',
    category: 'agent_executions',
    tabId: 'agent-executions',
    apiPath: AGENT_EXECUTIONS_API_PATH,
    title: 'Agent Executions',
    descriptor: 'Command execution requests from agents, awaiting your approval',
    searchPlaceholder: 'Filter by key, description, or agent…',
    isEnabled: (): boolean => commandRequestsEnabled(),
    getCardId: (card): string => card.request.rowId,
    renderCard: (card, ctx): TemplateResult => this.renderCommandRequestCard(card, ctx.onResolved),
  };

  static override styles = [
    actionCardContextStyles,
    css`
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
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    dispatchPageTitle(this, 'Agent Executions', 'Profile');
  }

  private handleJump(
    card: CommandRequestDetailResponse,
    e: CustomEvent<{ messageId: string }>
  ): void {
    const targetId = e.detail?.messageId;
    if (!targetId) return;
    const chatHref = actionCardChatHref(
      card.threadId,
      card.request.projectId,
      targetId,
      card.promptAvailable
    );
    if (chatHref) {
      navigateTo(chatHref);
      return;
    }
    const routeTarget = card.request.replacedBy?.rowId || targetId;
    navigateTo(`/command-requests/${encodeURIComponent(routeTarget)}`);
  }

  private renderCommandRequestCard(
    card: CommandRequestDetailResponse,
    onResolved: () => void
  ): TemplateResult {
    const chatHref = actionCardChatHref(
      card.threadId,
      card.request.projectId,
      card.request.messageId,
      card.promptAvailable
    );

    return html`
      <article class="card-wrapper" data-command-request-card=${card.request.rowId}>
        <div class="card-context" data-card-context>
          ${card.projectName || card.request.projectId
            ? html`
                <span class="context-item">
                  <sl-icon name="folder" aria-hidden="true"></sl-icon>
                  <strong>${card.projectName || card.request.projectId}</strong>
                </span>
              `
            : nothing}
          ${card.request.agentName
            ? html`
                <span class="context-item">
                  <sl-icon name="cpu" aria-hidden="true"></sl-icon>
                  <strong>${card.request.agentName}</strong>
                </span>
              `
            : nothing}
          ${renderOpenInChat(chatHref, card.promptAvailable)}
        </div>
        ${card.intro ? html`<p class="card-intro">${card.intro}</p>` : nothing}
        <scion-chat-command-request
          .detail=${card}
          .currentUserId=${this.pageData?.user?.id ?? ''}
          surface="standalone_page"
          @command-request-approved=${(): void => onResolved()}
          @command-request-dismissed=${(): void => onResolved()}
          @command-request-jump=${(e: CustomEvent<{ messageId: string }>): void =>
            this.handleJump(card, e)}
        ></scion-chat-command-request>
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
    'scion-page-profile-agent-executions': ScionPageProfileAgentExecutions;
  }
}
