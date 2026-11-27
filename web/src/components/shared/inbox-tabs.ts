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
 * Data-driven Inbox sub-navigation tab strip (`Overview | Decisions | ...`)
 * rendered at the top of Inbox views when `web.inbox_dashboard` is enabled and
 * at least two tabs are visible (design §6.4).
 *
 * `INBOX_CHILD_ROUTES` is derived from `INBOX_TABS` so `<scion-profile-nav>`
 * active-state highlighting and the tab strip can never disagree.
 */

import { css, html, nothing, type TemplateResult } from 'lit';
import { getLastKnownProfileDecisionAvailability } from '../../client/profile-decisions.js';
import {
  agentSecretsEnabled,
  decisionsEnabled,
  isInboxDashboardEnabled,
} from '../../utils/feature-flags.js';

export type InboxTabId = 'overview' | 'decisions' | 'agent-secrets';

export interface InboxTabStripOptions {
  /**
   * Whether the Decisions tab should be shown, mirroring `decisionsNavVisibility === 'visible'`
   * in `<scion-profile-nav>`. When omitted, defaults to checking
   * `getLastKnownProfileDecisionAvailability() === true`.
   */
  decisionsVisible?: boolean;
}

export interface InboxTabConfig {
  id: InboxTabId;
  label: string;
  icon: string;
  href: string;
  isVisible: (options?: InboxTabStripOptions) => boolean;
}

/**
 * Canonical Inbox tab registry (design §6.4). Order determines tab strip order.
 * Future message-action kinds append an entry here without changing
 * `<scion-profile-nav>` or `renderInboxTabStrip`.
 */
export const INBOX_TABS: readonly InboxTabConfig[] = [
  {
    id: 'overview',
    label: 'Overview',
    icon: 'inbox',
    href: '/profile/inbox',
    isVisible: (): boolean => true,
  },
  {
    id: 'decisions',
    label: 'Decisions',
    icon: 'signpost-split',
    href: '/profile/decisions',
    isVisible: (options?: InboxTabStripOptions): boolean =>
      options?.decisionsVisible !== undefined
        ? decisionsEnabled() && options.decisionsVisible
        : decisionsEnabled() && getLastKnownProfileDecisionAvailability() === true,
  },
  {
    id: 'agent-secrets',
    label: 'Agent Secrets',
    icon: 'shield-lock',
    href: '/profile/agent-secrets',
    isVisible: (): boolean => agentSecretsEnabled(),
  },
];

/**
 * Route prefixes belonging to the Inbox view family, derived from `INBOX_TABS`
 * so the sidebar `Inbox` item stays active on every Inbox tab route.
 */
export const INBOX_CHILD_ROUTES: readonly string[] = INBOX_TABS.map((tab) => tab.href);

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
  const visibleTabs = INBOX_TABS.filter((tab) => tab.isVisible(options));
  if (visibleTabs.length < 2) return nothing;
  return html`
    <nav class="inbox-tab-strip" aria-label="Inbox views" data-inbox-tab-strip>
      ${visibleTabs.map(
        (tab) => html`
          <a
            href=${tab.href}
            class="inbox-tab-link ${active === tab.id ? 'active' : ''}"
            aria-current=${active === tab.id ? 'page' : 'false'}
            data-inbox-tab=${tab.id}
          >
            <sl-icon name=${tab.icon}></sl-icon>
            <span>${tab.label}</span>
          </a>
        `
      )}
    </nav>
  `;
}
