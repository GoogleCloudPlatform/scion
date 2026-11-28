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
 * Profile Inbox page (`/profile/inbox`, ptone/scion#9289, ptone/scion#9290, design §6.1–§6.3).
 *
 * Gated on `web.inbox_dashboard`: when off (or when the hub answers 404),
 * renders `<scion-page-404>`.
 *
 * Renders the responsive summary-of-state card grid from the data-driven
 * `INBOX_CARDS` registry. Disabled categories are hidden; unavailable
 * categories show their descriptor and a sign-in note; empty cards show
 * "Nothing Waiting" with no action links.
 */

import { LitElement, css, html, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import {
  INBOX_SUMMARY_CHANGED_EVENT,
  inboxSummaryStore,
  type InboxCategory,
  type InboxSummaryResponse,
} from '../../client/inbox-summary.js';
import {
  PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
  fetchProfileDecisionProbe,
  getInitialProfileDecisionVisibility,
  resolveProfileDecisionVisibility,
  type ProfileDecisionVisibility,
} from '../../client/profile-decisions.js';
import {
  ARTIFACTS_REVIEW_DRAFTS_FLAG,
  ARTIFACTS_REVIEW_REQUESTS_FLAG,
  FEATURE_FLAGS_CHANGED_EVENT,
  decisionsEnabled,
  isFeatureEnabled,
  isInboxDashboardEnabled,
} from '../../utils/feature-flags.js';
import {
  INBOX_NOTHING_WAITING,
  formatInboxProseSegments,
  type InboxLinkKey,
} from '../../utils/inbox-prose.js';
import { inboxTabStripStyles, renderInboxTabStrip } from '../shared/inbox-tabs.js';
import './not-found.js';

export type InboxCardKey = 'decisions' | 'secrets' | 'executions' | 'waiting' | 'reviews' | 'chat';

export interface InboxCardConfig {
  key: InboxCardKey;
  icon: string;
  title: string;
  descriptor: string;
  defaultHref: string;
}

/**
 * Data-driven Inbox card registry in canonical display order
 * (design §6.3, amended 2026-11-26 for the single merged Secrets card).
 */
export const INBOX_CARDS: readonly InboxCardConfig[] = [
  {
    key: 'decisions',
    icon: 'signpost-split',
    title: 'Decisions',
    descriptor: 'Open decisions answerable by you',
    defaultHref: '/profile/decisions?tab=pending',
  },
  {
    key: 'secrets',
    icon: 'shield-lock',
    title: 'Secrets',
    descriptor: 'Secret requests from agents and secrets awaiting your approval',
    defaultHref: '/profile/secrets',
  },
  {
    key: 'executions',
    icon: 'terminal',
    title: 'Agent Executions',
    descriptor: 'Command execution requests from agents, awaiting your approval',
    defaultHref: '/profile/agent-executions',
  },
  {
    key: 'waiting',
    icon: 'hourglass-split',
    title: 'Agents Waiting',
    descriptor: 'Agents waiting for input in your projects',
    defaultHref: '/agents?q=activity%3Awaiting_for_input',
  },
  {
    key: 'reviews',
    icon: 'clipboard-check',
    title: 'Artifact Reviews',
    descriptor: 'Reviews on your artifacts, review requests for you, and your unfinished drafts',
    defaultHref: '/artifacts?review_on_latest=1&owner=me',
  },
  {
    key: 'chat',
    icon: 'chat-dots',
    title: 'Chat',
    descriptor: 'Unread conversations and starred messages',
    defaultHref: '/chat',
  },
];

/**
 * Web-owned hrefs for non-primary inline prose link keys (design §14.3).
 * `reviews-to-address` resolves to the hub-owned primary href (`cat.href`).
 */
export const INBOX_LINK_HREFS: Partial<
  Record<Exclude<InboxLinkKey, 'primary' | 'secrets-requests' | 'secrets-pending'>, string>
> = {
  'chat-unread': '/chat?filter=unread',
  'chat-mentions': '/chat?filter=mentions',
  'chat-starred': '/chat?filter=starred',
  'reviews-requested': '/artifacts?requested=1',
  'reviews-drafts': '/artifacts?drafts=1',
};

function resolveMergedSecretsCategory(
  categories: InboxSummaryResponse['categories']
): InboxCategory | null {
  const asCat = categories.agent_secrets;
  const psCat = categories.pending_secrets;
  if (!asCat && !psCat) return null;

  const asState = asCat?.state ?? 'disabled';
  const psState = psCat?.state ?? 'disabled';

  if (asState === 'disabled' && psState === 'disabled') {
    return {
      state: 'disabled',
      count: 0,
      capped: false,
      countsTowardTotal: false,
      href: '/profile/secrets',
      previews: [],
    };
  }

  if (asState === 'error' || psState === 'error') {
    const errorCode = (asState === 'error' ? asCat?.errorCode : psCat?.errorCode) ?? 'internal';
    return {
      state: 'error',
      count: 0,
      capped: false,
      countsTowardTotal: false,
      href: '/profile/secrets',
      errorCode,
      previews: [],
    };
  }

  const asOK = asState === 'ok';
  const psOK = psState === 'ok';

  if (!asOK && !psOK) {
    return {
      state: 'unavailable',
      count: 0,
      capped: false,
      countsTowardTotal: false,
      href: psCat?.href || asCat?.href || '/profile/secrets',
      previews: [],
    };
  }

  const secretRequests = asOK && asCat ? asCat.count : 0;
  const pendingSecrets = psOK && psCat ? psCat.count : 0;
  const requestsCapped = Boolean(asOK && asCat?.capped);
  const pendingCapped = Boolean(psOK && psCat?.capped);
  const count = secretRequests + pendingSecrets;
  const capped = requestsCapped || pendingCapped;

  let projects = 0;
  if (asOK && asCat) {
    if (typeof asCat.breakdown?.secretsProjects === 'number') {
      projects = asCat.breakdown.secretsProjects;
    } else {
      projects = asCat.breakdown?.projects ?? 0;
      if (psOK && psCat && projects === 0) {
        projects = psCat.breakdown?.projects ?? 0;
      }
    }
  } else if (psOK && psCat) {
    projects = psCat.breakdown?.projects ?? 0;
  }

  return {
    state: 'ok',
    count,
    capped,
    countsTowardTotal: true,
    href: psCat?.href || asCat?.href || '/profile/secrets',
    previews: [],
    breakdown: {
      secretRequests,
      secretRequestsCapped: requestsCapped ? 1 : 0,
      agents: asOK && asCat ? (asCat.breakdown?.agents ?? 0) : 0,
      pendingSecrets,
      pendingSecretsCapped: pendingCapped ? 1 : 0,
      projects,
    },
  };
}

function resolveCardCategory(
  cardKey: InboxCardKey,
  categories: InboxSummaryResponse['categories']
): InboxCategory | null {
  if (cardKey === 'secrets') {
    return resolveMergedSecretsCategory(categories);
  }
  if (cardKey === 'executions') {
    return categories.agent_executions ?? null;
  }
  return categories[cardKey] ?? null;
}

function resolveInboxLinkHref(
  link: InboxLinkKey,
  cat: InboxCategory,
  card: InboxCardConfig,
  categories?: InboxSummaryResponse['categories']
): string | undefined {
  if (link === 'primary' || link === 'reviews-to-address') {
    return cat.href || card.defaultHref;
  }
  if (link === 'secrets-requests') {
    return categories?.agent_secrets?.href || '/profile/agent-secrets';
  }
  if (link === 'secrets-pending') {
    return categories?.pending_secrets?.href || '/profile/secrets';
  }
  if (link === 'reviews-requested' && !isFeatureEnabled(ARTIFACTS_REVIEW_REQUESTS_FLAG)) {
    return undefined;
  }
  if (link === 'reviews-drafts' && !isFeatureEnabled(ARTIFACTS_REVIEW_DRAFTS_FLAG)) {
    return undefined;
  }
  return INBOX_LINK_HREFS[link];
}

@customElement('scion-page-profile-inbox')
export class ScionPageProfileInbox extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @state()
  private enabled = false;

  @state()
  private summary: InboxSummaryResponse | null = null;

  @state()
  private loading = false;

  @state()
  private error: string | null = null;

  @state()
  private unavailable404 = false;

  /** Mirrors `<scion-profile-nav>`'s 404 probe state for the Decisions sub-tab. */
  @state()
  private decisionsNavVisibility: ProfileDecisionVisibility = 'unknown';

  private unmountPage: (() => void) | null = null;

  private boundOnSummaryChanged = (): void => {
    this.syncFromStore();
  };

  private boundOnFeatureFlagsChanged = (): void => {
    const next = isInboxDashboardEnabled();
    if (next !== this.enabled) {
      this.enabled = next;
      if (next) {
        this.startStore();
      } else {
        this.stopStore();
      }
    }
    if (this.enabled && decisionsEnabled()) {
      this.syncOrProbeDecisionsAvailability();
    } else {
      this.decisionsNavVisibility = 'unknown';
    }
    this.requestUpdate();
  };

  private boundOnProfileDecisionsCountChanged = (e: Event): void => {
    const detail = (e as CustomEvent<{ available?: boolean; waitingOnYou?: number }>).detail;
    if (detail?.available === false) {
      this.decisionsNavVisibility = 'hidden';
      return;
    }
    if (typeof detail?.waitingOnYou === 'number') {
      this.decisionsNavVisibility = 'visible';
    }
  };

  static override styles = [
    inboxTabStripStyles,
    css`
      :host {
        display: block;
        max-width: 1100px;
        margin: 0 auto;
        padding: 1.5rem 1.25rem;
      }

      .page-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 1rem;
        margin-bottom: 0.75rem;
      }

      .page-title {
        margin: 0;
        font-size: 1.5rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .caught-up-banner {
        display: flex;
        align-items: center;
        gap: 0.625rem;
        padding: 0.875rem 1rem;
        margin-bottom: 1rem;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        background: var(--scion-surface, #ffffff);
        color: var(--scion-text-muted, #64748b);
        font-size: 0.9rem;
      }

      .caught-up-banner sl-icon {
        color: var(--sl-color-success-600, #16a34a);
        font-size: 1.15rem;
      }

      .card-grid {
        display: grid;
        grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
        gap: 1rem;
        align-items: stretch;
      }

      .inbox-card {
        display: flex;
        flex-direction: column;
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        overflow: hidden;
      }

      .card-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.75rem;
        padding: 0.875rem 1rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }

      .card-title-group {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        min-width: 0;
      }

      .card-title-group sl-icon {
        font-size: 1.1rem;
        color: var(--scion-primary, #3b82f6);
        flex-shrink: 0;
      }

      .card-title {
        margin: 0;
        font-size: 0.95rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .card-body {
        display: flex;
        flex-direction: column;
        flex: 1;
        gap: 0.5rem;
        padding: 0.875rem 1rem;
      }

      .card-descriptor {
        margin: 0;
        font-size: 0.8125rem;
        font-style: italic;
        color: var(--scion-text-muted, #64748b);
      }

      .card-prose {
        margin: 0;
        font-size: 0.9375rem;
        color: var(--scion-text, #1e293b);
      }

      .card-prose-link {
        color: var(--scion-primary, #3b82f6);
        font-weight: 500;
        text-decoration: underline;
        text-underline-offset: 0.15em;
      }

      .card-prose-link:hover {
        color: var(--scion-primary-hover, #2563eb);
      }

      .card-prose-link:focus-visible {
        outline: 2px solid var(--scion-primary, #3b82f6);
        outline-offset: 2px;
        border-radius: var(--scion-radius-sm, 0.25rem);
      }

      .card-empty,
      .card-unavailable {
        margin: 0;
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
      }

      .card-error {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.75rem;
        font-size: 0.875rem;
        color: var(--sl-color-danger-600, #dc2626);
      }

      .loading-state {
        display: flex;
        align-items: center;
        justify-content: center;
        gap: 0.75rem;
        padding: 3rem 1rem;
        color: var(--scion-text-muted, #64748b);
      }

      @media (max-width: 480px) {
        :host {
          padding: 1rem 0.75rem;
        }
        .card-grid {
          grid-template-columns: 1fr;
        }
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    dispatchPageTitle(this, 'Inbox');
    this.enabled = isInboxDashboardEnabled();
    if (typeof window !== 'undefined') {
      window.addEventListener(INBOX_SUMMARY_CHANGED_EVENT, this.boundOnSummaryChanged);
      window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
      window.addEventListener(
        PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
        this.boundOnProfileDecisionsCountChanged
      );
    }
    if (this.enabled) {
      this.startStore();
      if (decisionsEnabled()) {
        this.syncOrProbeDecisionsAvailability();
      }
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopStore();
    if (typeof window !== 'undefined') {
      window.removeEventListener(INBOX_SUMMARY_CHANGED_EVENT, this.boundOnSummaryChanged);
      window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
      window.removeEventListener(
        PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
        this.boundOnProfileDecisionsCountChanged
      );
    }
  }

  private startStore(): void {
    if (this.unmountPage) return;
    this.unmountPage = inboxSummaryStore.mountPage(this.pageData?.user?.id);
    this.syncFromStore();
  }

  private stopStore(): void {
    if (this.unmountPage) {
      this.unmountPage();
      this.unmountPage = null;
    }
  }

  private syncFromStore(): void {
    this.summary = inboxSummaryStore.summary;
    this.loading = inboxSummaryStore.loading;
    this.error = inboxSummaryStore.error;
    this.unavailable404 = inboxSummaryStore.unavailable404;
  }

  private syncOrProbeDecisionsAvailability(): void {
    const initial = getInitialProfileDecisionVisibility();
    this.decisionsNavVisibility = initial.visibility;
    if (!initial.needsProbe) {
      return;
    }
    void this.probeDecisionsAvailability();
  }

  private async probeDecisionsAvailability(): Promise<void> {
    if (!decisionsEnabled()) return;
    const probe = await fetchProfileDecisionProbe();
    if (!this.isConnected || !decisionsEnabled()) return;
    this.decisionsNavVisibility = resolveProfileDecisionVisibility(
      this.decisionsNavVisibility,
      probe
    ).visibility;
  }

  private handleRefreshClick(): void {
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
  }

  private renderCard(
    card: InboxCardConfig,
    cat: InboxCategory,
    categories: InboxSummaryResponse['categories']
  ): TemplateResult | typeof nothing {
    if (cat.state === 'disabled') return nothing;

    const countText = cat.capped ? `${cat.count}+` : String(cat.count);
    const proseCategory = card.key === 'executions' ? 'agent_executions' : card.key;
    const segments =
      cat.state === 'ok'
        ? formatInboxProseSegments(proseCategory, cat.count, cat.capped, cat.breakdown)
        : 'nothing';

    return html`
      <section class="inbox-card" data-inbox-card=${card.key}>
        <header class="card-header">
          <div class="card-title-group">
            <sl-icon name=${card.icon}></sl-icon>
            <h2 class="card-title">${card.title}</h2>
          </div>
          ${cat.state === 'ok' && cat.count > 0
            ? html`<sl-badge variant="primary" pill data-card-count>${countText}</sl-badge>`
            : nothing}
        </header>
        <div class="card-body">
          <p class="card-descriptor" data-card-descriptor>${card.descriptor}</p>
          ${cat.state === 'unavailable'
            ? html`<p class="card-unavailable" data-card-unavailable>
                Not available with this sign-in
              </p>`
            : cat.state === 'error'
              ? html`
                  <div class="card-error" data-card-error>
                    <span>Couldn't load</span>
                    <sl-button
                      size="small"
                      variant="default"
                      @click=${(): void => this.handleRefreshClick()}
                    >
                      Retry
                    </sl-button>
                  </div>
                `
              : segments === 'nothing'
                ? html`<p class="card-empty" data-card-empty>${INBOX_NOTHING_WAITING}</p>`
                : html`<p class="card-prose" data-card-prose>
                    ${segments.map((seg) => {
                      const segHref = seg.link
                        ? resolveInboxLinkHref(seg.link, cat, card, categories)
                        : undefined;
                      return segHref
                        ? html`<a
                            class="card-prose-link"
                            href=${segHref}
                            data-card-prose-link=${seg.link!}
                            >${seg.text}</a
                          >`
                        : seg.text;
                    })}
                  </p>`}
        </div>
      </section>
    `;
  }

  override render(): TemplateResult {
    if (!this.enabled || this.unavailable404) {
      return html`<scion-page-404></scion-page-404>`;
    }

    const categories = this.summary?.categories;
    const enabledCategories = categories
      ? INBOX_CARDS.map((card) => resolveCardCategory(card.key, categories)).filter(
          (c): c is InboxCategory =>
            Boolean(c && c.state !== 'disabled' && c.state !== 'unavailable')
        )
      : [];
    const allCaughtUp =
      this.summary !== null &&
      enabledCategories.length > 0 &&
      enabledCategories.every((c) => c.state === 'ok' && c.count === 0);

    return html`
      <div class="page-header">
        <h1 class="page-title">Inbox</h1>
        <sl-button
          size="small"
          variant="default"
          ?loading=${this.loading}
          @click=${(): void => this.handleRefreshClick()}
        >
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Refresh
        </sl-button>
      </div>

      ${renderInboxTabStrip('overview', {
        decisionsVisible: this.decisionsNavVisibility === 'visible',
      })}
      ${this.loading && !this.summary
        ? html`
            <div class="loading-state" data-inbox-loading>
              <sl-spinner></sl-spinner>
              <span>Loading inbox…</span>
            </div>
          `
        : this.error && !this.summary
          ? html`
              <sl-alert variant="danger" open data-inbox-error>
                <sl-icon slot="icon" name="exclamation-octagon"></sl-icon>
                ${this.error}
                <sl-button
                  size="small"
                  variant="default"
                  style="margin-left: 0.75rem;"
                  @click=${(): void => this.handleRefreshClick()}
                >
                  Retry
                </sl-button>
              </sl-alert>
            `
          : html`
              ${allCaughtUp
                ? html`
                    <div class="caught-up-banner" data-inbox-caught-up>
                      <sl-icon name="check-circle-fill"></sl-icon>
                      <span>You're all caught up.</span>
                    </div>
                  `
                : nothing}
              <div class="card-grid" data-inbox-grid>
                ${categories
                  ? INBOX_CARDS.map((card) => {
                      const cat = resolveCardCategory(card.key, categories);
                      return cat ? this.renderCard(card, cat, categories) : nothing;
                    })
                  : nothing}
              </div>
            `}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-inbox': ScionPageProfileInbox;
  }
}
