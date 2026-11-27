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

import { css, html, nothing, type TemplateResult } from 'lit';

import { neverSuspendEnabled } from '../utils/feature-flags.js';
import { can, type Agent, type Capabilities } from './types.js';
import { showConfirm, type ConfirmOptions } from '../components/shared/confirm-dialog.js';

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
 * Force-suspend requires `agent.lifecycle` and an allowlisted human (`user` or
 * `dev`) principal. Interactive browser sessions are always human `user`/`dev`
 * principals (agent and federated principals cannot hold web sessions), so the
 * `lifecycle` capability is sufficient for UI gating; the Hub enforces the
 * principal-kind check server-side.
 */
export function canForceSuspend(capabilities: Capabilities | undefined): boolean {
  return can(capabilities, 'lifecycle');
}

/**
 * Reports whether the Suspend action button should be rendered for `agent`.
 * Checks that the agent's harness does not declare `resume.support === 'no'`
 * and that the caller holds `lifecycle` (which also gates force-suspend on
 * flagged agents in browser sessions).
 */
export function showSuspendAction(
  agent: Pick<Agent, 'suspendPolicy' | 'harnessCapabilities' | '_capabilities'>,
  capabilities: Capabilities | undefined = agent._capabilities
): boolean {
  if (agent.harnessCapabilities?.resume?.support === 'no') return false;
  return canForceSuspend(capabilities);
}

/**
 * Resolves a Suspend (or Keep suspended) click into the lifecycle action to
 * send: `'suspend'` for unflagged agents (or when the experiment is off),
 * `'force-suspend'` after the user confirms the dialog on a flagged agent, or
 * `null` if the user cancelled the confirmation dialog.
 */
export async function resolveSuspendAction(
  agent: Pick<Agent, 'name' | 'suspendPolicy'>,
  options?: { keepSuspended?: boolean }
): Promise<'suspend' | 'force-suspend' | null> {
  if (!isNeverSuspendActive(agent)) return 'suspend';
  const message = options?.keepSuspended
    ? forceKeepSuspendedConfirmMessage(agent.name)
    : forceSuspendConfirmMessage(agent.name);
  const confirmed = await showConfirm(message, FORCE_SUSPEND_CONFIRM_OPTIONS);
  return confirmed ? 'force-suspend' : null;
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

const NEVER_SUSPEND_BADGE_TOOLTIP =
  'Never suspend: skipped by automatic idle/stalled and bulk suspends';

/**
 * Shared styles for the quiet "Never suspend" pill rendered by
 * `neverSuspendBadge`. Uses the `--scion-badge-primary-*` theme tokens so the
 * pill stays legible in both light and dark modes.
 */
export const neverSuspendBadgeStyles = css`
  .never-suspend-badge {
    display: inline-flex;
    align-items: center;
    align-self: center;
    flex-shrink: 0;
    padding: 0.125rem 0.5rem;
    border-radius: 9999px;
    font-size: 0.75rem;
    font-weight: 500;
    line-height: 1.25;
    white-space: nowrap;
    background: var(--scion-badge-primary-bg);
    color: var(--scion-badge-primary-text);
  }

  .never-suspend-badge--compact {
    font-size: 0.6875rem;
    padding: 0.0625rem 0.4375rem;
  }
`;

/**
 * Renders the "Never suspend" pill when the experiment is enabled and the
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
    <span
      class=${cls}
      data-testid="never-suspend-badge"
      title=${NEVER_SUSPEND_BADGE_TOOLTIP}
      aria-label=${NEVER_SUSPEND_BADGE_TOOLTIP}
      >Never suspend</span
    >
  `;
}
