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
 * Display text for an agent's authority state (ptone/scion#2143): the
 * badge label and the tooltip naming the cause and the remediation.
 */

import type { AgentAuthority, AgentAuthorityPrincipal } from './types.js';

/** Whether the state is one an operator should act on. */
export function isAuthorityFlagged(a: AgentAuthority | null | undefined): a is AgentAuthority {
  return !!a && (a.state === 'orphaned' || a.state === 'revoked');
}

/** Badge label: "Orphaned" or "Revoked", or "" for any other state. */
export function authorityBadgeLabel(a: AgentAuthority | null | undefined): string {
  if (!isAuthorityFlagged(a)) return '';
  return a.state === 'orphaned' ? 'Orphaned' : 'Revoked';
}

function principalText(p: AgentAuthorityPrincipal | undefined, fallback: string): string {
  if (!p || (p.kind !== 'user' && p.kind !== 'agent')) return fallback;
  if (p.id) return `${p.kind} ${p.id}`;
  return p.kind === 'agent' ? 'an agent' : 'a user';
}

/** One sentence naming the cause. */
export function authorityCauseText(a: AgentAuthority): string {
  const who = principalText(a.principal, 'a principal');
  const relativePronoun = a.principal?.kind === 'user' ? 'who' : 'which';
  switch (a.cause) {
    case 'delegator_deleted':
      return `Its delegation comes from ${who}, ${relativePronoun} was deleted.`;
    case 'delegator_inactive':
      return `Its delegation comes from ${who}, ${relativePronoun} is not active.`;
    case 'delegator_revoked':
      return `Its delegation comes from ${who}, whose own delegation was revoked.`;
    case 'chain_invalid':
      return 'Its recorded delegation chain cannot be resolved.';
    case 'user_delete_cascade':
      return `Its delegation was revoked when ${who} was deleted.`;
    default:
      return a.cause
        ? `Its delegation was deactivated (${a.cause}).`
        : 'Its delegation was deactivated.';
  }
}

/** Remediation sentence shown with every flagged state. */
export const AUTHORITY_REMEDIATION =
  'Requests that need its delegation are denied. Stop or delete this agent; anyone who may delete it can.';

/** Full tooltip text for a flagged state, or "". */
export function authorityTooltip(a: AgentAuthority | null | undefined): string {
  if (!isAuthorityFlagged(a)) return '';
  return `${authorityBadgeLabel(a)}. ${authorityCauseText(a)} ${AUTHORITY_REMEDIATION}`;
}
