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
 * The Owner deleted badge of an artifact (ptone/scion#4535, approved mock
 * states S1 and S3): shown wherever the hub reports ownerDeleted, on list
 * rows and in the artifact page header, with the owner's name struck
 * through.
 */

import { html, css } from 'lit';
import type { TemplateResult } from 'lit';

/** The tooltip of the badge (mock S3). */
export const OWNER_DELETED_TOOLTIP =
  'The agent that owned this artifact was deleted. It stays readable until a custodian deletes it or it expires.';

/** The tooltip for an artifact whose owner was a user. */
export const OWNER_DELETED_USER_TOOLTIP =
  'The user who owned this artifact was deleted. It stays readable until a custodian deletes it or it expires.';

/** The Owner deleted badge, with the tooltip that fits the owner's kind. */
export function ownerDeletedBadge(ownerKind: string): TemplateResult {
  const tip = ownerKind === 'user' ? OWNER_DELETED_USER_TOOLTIP : OWNER_DELETED_TOOLTIP;
  // The badge is focusable and carries the explanation as its accessible
  // name, so keyboard and screen-reader users reach what the tooltip shows
  // on hover.
  return html`<sl-tooltip content=${tip}>
    <sl-badge
      class="owner-deleted"
      variant="neutral"
      pill
      tabindex="0"
      role="note"
      aria-label=${`Owner deleted. ${tip}`}
    >
      <sl-icon name="person-x" aria-hidden="true"></sl-icon>&nbsp;Owner deleted
    </sl-badge>
  </sl-tooltip>`;
}

/** Styles for the badge and the struck-through owner name. */
export const ownerDeletedStyles = css`
  .owner-gone {
    text-decoration: line-through;
    color: var(--scion-text-muted, #64748b);
  }
  sl-badge.owner-deleted::part(base) {
    gap: 0.15rem;
  }
`;
