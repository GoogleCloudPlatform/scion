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
 * Shared Inbox sub-navigation tab strip (`Overview | Decisions`) rendered at
 * the top of `/profile/inbox` and `/profile/decisions` when `web.inbox_dashboard`
 * is enabled (design §6.1).
 */

import { css, html, nothing, type TemplateResult } from 'lit';
import { getLastKnownProfileDecisionAvailability } from '../../client/profile-decisions.js';
import { decisionsEnabled, isInboxDashboardEnabled } from '../../utils/feature-flags.js';

export type InboxTabId = 'overview' | 'decisions';

export interface InboxTabStripOptions {
  /**
   * Whether the Decisions tab should be shown, mirroring `decisionsNavVisibility === 'visible'`
   * in `<scion-profile-nav>`. When omitted, defaults to checking
   * `getLastKnownProfileDecisionAvailability() === true`.
   */
  decisionsVisible?: boolean;
}

export const inboxTabStripStyles = css`
  .inbox-tab-strip {
    display: flex;
    align-items: center;
    gap: 0.25rem;
    border-bottom: 1px solid var(--scion-border, #e2e8f0);
    margin-bottom: 1.25rem;
  }

  .inbox-tab-link {
    display: inline-flex;
    align-items: center;
    gap: 0.375rem;
    padding: 0.5rem 0.875rem;
    font-size: 0.875rem;
    font-weight: 500;
    color: var(--scion-text-muted, #64748b);
    text-decoration: none;
    border-bottom: 2px solid transparent;
    margin-bottom: -1px;
    transition:
      color var(--scion-transition-fast, 150ms ease),
      border-color var(--scion-transition-fast, 150ms ease);
  }

  .inbox-tab-link:hover {
    color: var(--scion-text, #1e293b);
  }

  .inbox-tab-link.active {
    color: var(--scion-primary, #3b82f6);
    border-bottom-color: var(--scion-primary, #3b82f6);
    font-weight: 600;
  }

  .inbox-tab-link sl-icon {
    font-size: 0.95rem;
  }
`;

export function renderInboxTabStrip(
  active: InboxTabId,
  options?: InboxTabStripOptions
): TemplateResult | typeof nothing {
  if (!isInboxDashboardEnabled()) return nothing;
  const showDecisions =
    options?.decisionsVisible !== undefined
      ? decisionsEnabled() && options.decisionsVisible
      : decisionsEnabled() && getLastKnownProfileDecisionAvailability() === true;
  return html`
    <nav class="inbox-tab-strip" aria-label="Inbox views" data-inbox-tab-strip>
      <a
        href="/profile/inbox"
        class="inbox-tab-link ${active === 'overview' ? 'active' : ''}"
        aria-current=${active === 'overview' ? 'page' : 'false'}
        data-inbox-tab="overview"
      >
        <sl-icon name="inbox"></sl-icon>
        <span>Overview</span>
      </a>
      ${showDecisions
        ? html`
            <a
              href="/profile/decisions"
              class="inbox-tab-link ${active === 'decisions' ? 'active' : ''}"
              aria-current=${active === 'decisions' ? 'page' : 'false'}
              data-inbox-tab="decisions"
            >
              <sl-icon name="signpost-split"></sl-icon>
              <span>Decisions</span>
            </a>
          `
        : nothing}
    </nav>
  `;
}
