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
 * Standalone command request page (`/command-requests/:id`, ptone/scion#9283).
 *
 * Gated on `hub.command_requests`: renders `<scion-page-404>` and makes zero
 * API requests when the experiment is off.
 *
 * Loads the command request from `GET /api/v1/command-requests/{id}`,
 * subscribes to live updates via `stateManager`, renders the agent-authored
 * intro strictly as plain text (never Markdown, audit L4), displays a
 * replacement banner when opened via an older version link, and renders
 * `<scion-chat-command-request surface="standalone_page">`.
 */

import { LitElement, css, html, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import type { PageData } from '../../shared/types.js';
import { current, projectTrail } from '../../client/crumbs.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { navigateTo } from '../../client/navigation.js';
import { stateManager } from '../../client/state.js';
import { FEATURE_FLAGS_CHANGED_EVENT, commandRequestsEnabled } from '../../utils/feature-flags.js';
import {
  fetchCommandRequestDetail,
  type CommandRequestDetailResponse,
} from '../../client/command-requests.js';
import type { Crumb } from '../shared/breadcrumb.js';
import '../shared/breadcrumb.js';
import type {
  CommandRequestApprovedDetail,
  CommandRequestDismissedDetail,
  CommandRequestUpdatedDetail,
} from '../shared/chat/chat-command-request.js';
import '../shared/chat/chat-command-request.js';
import './not-found.js';

@customElement('scion-page-command-request')
export class ScionPageCommandRequest extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  /** Optional direct override (otherwise extracted from the URL). */
  @property({ type: String })
  requestId = '';

  @state()
  private resolvedId = '';

  @state()
  private loading = true;

  @state()
  private errorStatus = 0;

  @state()
  private errorMessage = '';

  @state()
  private detail: CommandRequestDetailResponse | null = null;

  private abortController: AbortController | null = null;

  private readonly onFlagsChanged = (): void => {
    this.requestUpdate();
    if (commandRequestsEnabled() && !this.detail && !this.loading && this.resolvedId) {
      void this.loadDetail();
    }
  };

  static override styles = css`
    :host {
      display: block;
    }

    .page {
      max-width: 48rem;
      margin: 0 auto;
      display: flex;
      flex-direction: column;
      gap: 1rem;
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
    this.resolvedId = this.extractRequestId();
    if (commandRequestsEnabled()) {
      void this.loadDetail();
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
    if (changed.has('pageData') || changed.has('requestId')) {
      const nextId = this.extractRequestId();
      if (nextId !== this.resolvedId) {
        this.resolvedId = nextId;
        if (commandRequestsEnabled()) {
          void this.loadDetail();
        }
      }
    }
  }

  private extractRequestId(): string {
    if (this.requestId) return this.requestId;
    const rawPath = this.pageData?.path ?? window.location.pathname;
    const pathname = rawPath.split(/[?#]/, 1)[0];
    const match = pathname.match(/^\/command-requests\/([^/]+)$/);
    if (!match?.[1]) return '';
    try {
      return decodeURIComponent(match[1]);
    } catch {
      return '';
    }
  }

  private async loadDetail(): Promise<void> {
    this.abortController?.abort();
    if (!commandRequestsEnabled()) {
      this.loading = false;
      return;
    }
    if (!this.resolvedId) {
      this.loading = false;
      this.errorStatus = 400;
      this.errorMessage = 'Invalid command request link.';
      this.detail = null;
      return;
    }

    const controller = new AbortController();
    this.abortController = controller;
    this.loading = true;
    this.errorStatus = 0;
    this.errorMessage = '';
    this.detail = null;

    const result = await fetchCommandRequestDetail(this.resolvedId, {
      signal: controller.signal,
    });
    if (controller.signal.aborted) return;

    this.loading = false;
    if (!result.ok) {
      this.errorStatus = result.status;
      this.errorMessage = result.message;
      return;
    }

    this.detail = result.detail;
    if (result.detail.request.projectId) {
      stateManager.setScope({ type: 'project', projectId: result.detail.request.projectId });
    }
    dispatchPageTitle(
      this,
      result.detail.request.key
        ? `Command Request · ${result.detail.request.key}`
        : 'Command Request'
    );
  }

  private handleRequestChanged(
    e: CustomEvent<
      CommandRequestApprovedDetail | CommandRequestDismissedDetail | CommandRequestUpdatedDetail
    >
  ): void {
    if (!this.detail || !e.detail?.request) return;
    this.detail = {
      ...this.detail,
      request: e.detail.request,
      ...(e.detail.request.run ? { run: e.detail.request.run } : {}),
    };
  }

  private handleJump(e: CustomEvent<{ messageId: string }>): void {
    const targetId = e.detail?.messageId;
    if (!targetId) return;
    const replacedBy = this.detail?.request?.replacedBy;
    const routeTarget = replacedBy?.rowId || targetId;
    navigateTo(`/command-requests/${encodeURIComponent(routeTarget)}`);
  }

  private pageCrumbs(includeCurrent: boolean): Crumb[] {
    const projectId = this.detail?.request?.projectId;
    const ancestors: Crumb[] = projectId
      ? projectTrail({
          id: projectId,
          label: this.detail?.projectName || projectId,
        })
      : [{ label: 'Dashboard', href: '/' }];
    return includeCurrent ? [...ancestors, current('Command Request', false)] : ancestors;
  }

  private renderReplacementBanner(): TemplateResult | typeof nothing {
    const req = this.detail?.request;
    if (!req || req.status !== 'replaced' || !req.replacedBy?.rowId) return nothing;
    return html`
      <div class="replacement-banner" role="status">
        <sl-icon name="arrow-repeat" aria-hidden="true"></sl-icon>
        <div>
          <div>This command request has been replaced by a newer version.</div>
          <a
            class="replacement-link"
            href=${`/command-requests/${encodeURIComponent(req.replacedBy.rowId)}`}
          >
            Open current version (v${req.replacedBy.version})
          </a>
        </div>
      </div>
    `;
  }

  override render(): TemplateResult {
    if (!commandRequestsEnabled()) {
      return html`<scion-page-404></scion-page-404>`;
    }

    if (this.loading) {
      return html`
        <div class="page">
          <scion-breadcrumb .crumbs=${this.pageCrumbs(false)}></scion-breadcrumb>
          <div class="state-card" role="status">
            <sl-spinner></sl-spinner>
            <span>Loading command request…</span>
          </div>
        </div>
      `;
    }

    if (this.errorMessage || !this.detail) {
      const heading =
        this.errorStatus === 404
          ? 'Command Request Not Found'
          : this.errorStatus === 403
            ? 'Access Denied'
            : 'Unable to Load Command Request';
      return html`
        <div class="page">
          <scion-breadcrumb .crumbs=${this.pageCrumbs(false)}></scion-breadcrumb>
          <div class="state-card error" role="alert">
            <sl-icon name="exclamation-circle" aria-hidden="true"></sl-icon>
            <h2>${heading}</h2>
            <p>${this.errorMessage || 'This command request could not be found.'}</p>
            ${this.errorStatus !== 404 && this.errorStatus !== 403
              ? html`<sl-button size="small" @click=${(): void => void this.loadDetail()}>
                  Retry
                </sl-button>`
              : nothing}
          </div>
        </div>
      `;
    }

    const req = this.detail.request;
    return html`
      <div class="page">
        <scion-breadcrumb .crumbs=${this.pageCrumbs(true)}></scion-breadcrumb>

        <div class="context-bar">
          <span class="context-item">
            <sl-icon name="cpu" aria-hidden="true"></sl-icon>
            Agent: <strong>${req.agentName}</strong>
          </span>
          ${this.detail.projectName
            ? html`<span class="context-item">
                <sl-icon name="folder" aria-hidden="true"></sl-icon>
                Project: <strong>${this.detail.projectName}</strong>
              </span>`
            : nothing}
        </div>

        ${this.renderReplacementBanner()}
        ${this.detail.intro
          ? html`<p class="intro" data-testid="command-request-intro">${this.detail.intro}</p>`
          : nothing}

        <scion-chat-command-request
          .detail=${this.detail}
          .currentUserId=${this.pageData?.user?.id ?? ''}
          surface="standalone_page"
          @command-request-approved=${(e: CustomEvent<CommandRequestApprovedDetail>): void =>
            this.handleRequestChanged(e)}
          @command-request-dismissed=${(e: CustomEvent<CommandRequestDismissedDetail>): void =>
            this.handleRequestChanged(e)}
          @command-request-updated=${(e: CustomEvent<CommandRequestUpdatedDetail>): void =>
            this.handleRequestChanged(e)}
          @command-request-jump=${(e: CustomEvent<{ messageId: string }>): void =>
            this.handleJump(e)}
        ></scion-chat-command-request>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-command-request': ScionPageCommandRequest;
  }
}
