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
 * Shared presentation helpers for the `lifecycle.never_suspend` experiment
 * (ptone/scion#9178, ptone/scion#9184).
 *
 * Presentation only: the hub enforces the experiment and permissions on every
 * create, PATCH, suspend, and auto-suspend sweep.
 */

import { html, nothing, type TemplateResult } from 'lit';

import { neverSuspendEnabled } from '../utils/feature-flags.js';
import { can, type Agent, type Capabilities, type User } from './types.js';
import type { ConfirmOptions } from '../components/shared/confirm-dialog.js';

/**
 * Reports whether an agent's never-suspend policy is active for presentation
 * (experiment enabled in this session and `suspendPolicy === 'never'`).
 */
export function isNeverSuspendActive(
  agent: Pick<Agent, 'suspendPolicy'> | null | undefined
): boolean {
  return neverSuspendEnabled() && agent?.suspendPolicy === 'never';
}

/**
 * Reports whether the current browser user may force-suspend a flagged agent.
 *
 * Force-suspend requires `agent.lifecycle` and a human user/dev principal;
 * agent principals and federated users cannot force-suspend. Interactive
 * browser sessions are always human user/dev principals unless marked
 * federated.
 */
export function canForceSuspend(
  capabilities: Capabilities | undefined,
  user?: (User & { federated?: boolean; principalKind?: string }) | null
): boolean {
  if (!can(capabilities, 'lifecycle')) return false;
  if (user?.federated) return false;
  if (user?.principalKind && user.principalKind !== 'user' && user.principalKind !== 'dev') {
    return false;
  }
  return true;
}

/** Confirmation copy for force-suspending a never-suspend agent. */
export function forceSuspendConfirmMessage(agentName: string): string {
  return `"${agentName}" has the Never suspend policy enabled. Force-suspending it will be audited, and the Never suspend policy will remain set when the agent is resumed.`;
}

/** Confirmation copy for converting a hub-suspended never-suspend agent to a user suspend. */
export function forceKeepSuspendedConfirmMessage(agentName: string): string {
  return `"${agentName}" has the Never suspend policy enabled. Keeping it suspended requires forcing the suspend, which will be audited. New work will no longer wake this agent until it is resumed.`;
}

/** Shared options for the force-suspend confirmation dialog. */
export const FORCE_SUSPEND_CONFIRM_OPTIONS: ConfirmOptions = {
  title: 'Force suspend agent',
  confirmText: 'Force suspend',
  variant: 'danger',
};

/**
 * Renders the "Never suspend" badge when the experiment is enabled and the
 * agent is flagged. Renders `nothing` otherwise (zero DOM nodes for unflagged
 * agents so list/project DOM budgets are unaffected).
 */
export function neverSuspendBadge(
  agent: Pick<Agent, 'suspendPolicy'> | null | undefined,
  options?: { compact?: boolean }
): TemplateResult | typeof nothing {
  if (!isNeverSuspendActive(agent)) return nothing;
  const cls = options?.compact
    ? 'never-suspend-badge never-suspend-badge--compact'
    : 'never-suspend-badge';
  return html`
    <sl-badge
      variant="primary"
      pill
      class=${cls}
      data-testid="never-suspend-badge"
      title="Never suspend: skipped by automatic idle/stalled and bulk suspends"
    >
      <sl-icon name="shield-check"></sl-icon>
      Never suspend
    </sl-badge>
  `;
}
