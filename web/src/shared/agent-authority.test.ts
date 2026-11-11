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

import { describe, it, expect } from 'vitest';
import {
  AUTHORITY_REMEDIATION,
  authorityBadgeLabel,
  authorityCauseText,
  authorityTooltip,
  isAuthorityFlagged,
} from './agent-authority.js';

describe('agent authority display', () => {
  it('flags only orphaned and revoked', () => {
    expect(isAuthorityFlagged({ state: 'orphaned' })).toBe(true);
    expect(isAuthorityFlagged({ state: 'revoked' })).toBe(true);
    expect(isAuthorityFlagged({ state: 'live' })).toBe(false);
    expect(isAuthorityFlagged({ state: 'unrecorded' })).toBe(false);
    expect(isAuthorityFlagged(null)).toBe(false);
    expect(isAuthorityFlagged(undefined)).toBe(false);
    expect(authorityBadgeLabel({ state: 'live' })).toBe('');
  });

  it('names the cause and the principal when the hub shows its ID', () => {
    expect(
      authorityCauseText({
        state: 'orphaned',
        cause: 'delegator_deleted',
        principal: { kind: 'agent', id: 'a-1', deleted: true },
      })
    ).toBe('Its delegation comes from agent a-1, which was deleted.');
    expect(
      authorityCauseText({
        state: 'orphaned',
        cause: 'delegator_inactive',
        principal: { kind: 'user', deleted: false },
      })
    ).toBe('Its delegation comes from a user, who is not active.');
    expect(
      authorityCauseText({
        state: 'orphaned',
        cause: 'delegator_inactive',
        principal: { kind: 'agent', deleted: false },
      })
    ).toBe('Its delegation comes from an agent, which is not active.');
    expect(
      authorityCauseText({
        state: 'revoked',
        cause: 'user_delete_cascade',
        principal: { kind: 'user', deleted: true },
      })
    ).toBe('Its delegation was revoked when a user was deleted.');
    expect(authorityCauseText({ state: 'orphaned', cause: 'chain_invalid' })).toBe(
      'Its recorded delegation chain cannot be resolved.'
    );
    expect(authorityCauseText({ state: 'revoked', cause: 'agent_soft_delete' })).toBe(
      'Its delegation was deactivated (agent_soft_delete).'
    );
  });

  it('builds the tooltip with the remediation', () => {
    const tip = authorityTooltip({
      state: 'orphaned',
      cause: 'delegator_revoked',
      principal: { kind: 'agent', deleted: false },
    });
    expect(tip).toBe(
      `Orphaned. Its delegation comes from an agent, whose own delegation was revoked. ${AUTHORITY_REMEDIATION}`
    );
    expect(authorityTooltip({ state: 'live' })).toBe('');
  });
});
