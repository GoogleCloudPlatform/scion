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
 * Test identity chip (ptone/scion#4240, phase WS1).
 *
 * Shown in the app shell while the browser holds a test identity web
 * session: "Test identity <email> · expires <time>". It cannot be
 * dismissed. Its only input is GET /auth/me, which carries
 * `testIdentity.expiresAt` for such a session and nothing for any other.
 */

import { css, html, nothing, type TemplateResult } from 'lit';

import type { User } from '../../shared/types.js';
import { formatInstantWithZone } from '../../utils/time.js';

/** The chip text for a user, or null when the user is not a test identity session. */
export function testIdentityChipText(user: User | null | undefined): string | null {
  const expiresAt = user?.testIdentity?.expiresAt;
  if (!user || !expiresAt) return null;
  const when = formatInstantWithZone(expiresAt, 'datetime');
  return when ? `Test identity ${user.email} · expires ${when}` : `Test identity ${user.email}`;
}

/** The chip, or `nothing` (no element) for any session that is not a test identity's. */
export function renderTestIdentityChip(
  user: User | null | undefined
): TemplateResult | typeof nothing {
  const text = testIdentityChipText(user);
  if (!text) return nothing;
  return html`
    <div class="test-identity-chip" role="status" data-testid="test-identity-chip">
      <sl-icon name="person-circle"></sl-icon>
      <span>${text}</span>
    </div>
  `;
}

/** Styles for the chip: a full-width strip under the test-hub banner. */
export const testIdentityChipStyles = css`
  .test-identity-chip {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-shrink: 0;
    padding: 0.375rem 1rem;
    background: var(--sl-color-warning-100, #fef3c7);
    color: var(--sl-color-warning-900, #78350f);
    border-bottom: 1px solid var(--sl-color-warning-300, #fcd34d);
    font-size: 0.875rem;
    font-weight: 600;
    overflow-wrap: anywhere;
  }
`;
