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
 * "View as member" persistent banner (ptone/scion#8648, design §4.8).
 *
 * Modelled on `test-hub-banner.ts`: a non-closable `<sl-alert variant="primary" open>`
 * rendered in all four authenticated shells directly below the test-hub banner
 * and above `<scion-header>` whenever `user?.viewAsMember` is true — regardless
 * of whether the `hub.view_as_member` experiment flag is currently enabled.
 * Never rendered on standalone auth pages (`/login`, `/invite`, `/onboarding`).
 */

import { css, html, nothing, type TemplateResult } from 'lit';

import type { User } from '../../shared/types.js';
import { toggleViewAsMember, type ToggleViewAsMemberOptions } from '../../client/view-as-member.js';
import { showToast } from '../../utils/toast.js';

export const VIEW_AS_MEMBER_BANNER_HEADING = 'Viewing as member';
export const VIEW_AS_MEMBER_BANNER_DETAIL = 'hub admin access is off in this browser session.';

async function handleExitClick(e: Event, options?: ToggleViewAsMemberOptions): Promise<void> {
  const btn = e.currentTarget as (HTMLElement & { loading?: boolean; disabled?: boolean }) | null;
  if (btn?.disabled) return;
  if (btn) {
    btn.loading = true;
    btn.disabled = true;
  }
  try {
    await toggleViewAsMember(false, options);
  } catch (err) {
    console.error('Failed to exit View as member mode:', err);
    if (btn) {
      btn.loading = false;
      btn.disabled = false;
    }
    showToast(
      err instanceof Error ? err.message : 'Failed to exit "View as member" mode',
      'danger'
    );
  }
}

/**
 * Renders the non-closable "Viewing as member" banner when `user?.viewAsMember`
 * is true, or `nothing` (zero DOM nodes) otherwise.
 */
export function renderViewAsMemberBanner(
  user: User | null | undefined,
  options?: ToggleViewAsMemberOptions
): TemplateResult | typeof nothing {
  if (!user?.viewAsMember) return nothing;
  return html`
    <sl-alert
      class="view-as-member-banner"
      variant="primary"
      open
      data-testid="view-as-member-banner"
    >
      <sl-icon slot="icon" name="eye"></sl-icon>
      <div class="view-as-member-banner-body">
        <span>
          <strong>${VIEW_AS_MEMBER_BANNER_HEADING}</strong> &mdash; ${VIEW_AS_MEMBER_BANNER_DETAIL}
        </span>
        <sl-button
          class="view-as-member-exit-btn"
          size="small"
          variant="default"
          data-testid="view-as-member-exit-btn"
          @click=${(e: Event): void => {
            void handleExitClick(e, options);
          }}
        >
          Exit
        </sl-button>
      </div>
    </sl-alert>
  `;
}

/** Styles for a host's banner: full width, square corners, never shrunk away. */
export const viewAsMemberBannerStyles = css`
  sl-alert.view-as-member-banner {
    display: block;
    flex-shrink: 0;
    width: 100%;
  }
  sl-alert.view-as-member-banner::part(base) {
    border-radius: 0;
  }
  .view-as-member-banner-body {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
    flex-wrap: wrap;
  }
  .view-as-member-exit-btn {
    flex-shrink: 0;
  }
`;
