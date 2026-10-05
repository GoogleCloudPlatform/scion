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
 * Agent State Display Definitions
 *
 * Consolidated mapping of agent phases and activities to their visual
 * representation: emoji, Bootstrap icon, color variant, label, and
 * animation. Edit this file to change how any agent state appears
 * across the web UI.
 */

import { html } from 'lit';
import type { TemplateResult } from 'lit';
import { ifDefined } from 'lit/directives/if-defined.js';

import { getAgentDisplayStatus } from './types.js';
import type { Agent, AgentPhase, AgentActivity } from './types.js';

/**
 * Color variant for status badge rendering
 */
export type StatusVariant = 'success' | 'warning' | 'danger' | 'primary' | 'neutral';

/**
 * Visual configuration for a single agent state
 */
export interface StateDisplay {
  /** Emoji shown before the label */
  emoji: string;
  /** Bootstrap icon name (used by sl-icon) */
  icon: string;
  /** Color variant for the badge */
  variant: StatusVariant;
  /** Whether to show a pulsing animation */
  pulse: boolean;
  /** Human-readable label (defaults to the key if not provided) */
  label?: string;
}

// ---------------------------------------------------------------------------
// Agent Phase display definitions
// ---------------------------------------------------------------------------

export const PHASE_DISPLAY: Record<AgentPhase, StateDisplay> = {
  created: { emoji: '🆕', icon: 'circle', variant: 'neutral', pulse: false },
  provisioning: { emoji: '📦', icon: 'hourglass-split', variant: 'warning', pulse: true },
  cloning: { emoji: '📥', icon: 'arrow-down-circle', variant: 'warning', pulse: true },
  starting: { emoji: '🚀', icon: 'arrow-repeat', variant: 'warning', pulse: true },
  running: { emoji: '▶️', icon: 'play-circle', variant: 'success', pulse: false },
  stopping: { emoji: '⏹️', icon: 'arrow-repeat', variant: 'warning', pulse: true },
  stopped: { emoji: '⏹️', icon: 'stop-circle', variant: 'neutral', pulse: false },
  suspended: { emoji: '⏸️', icon: 'pause-circle', variant: 'warning', pulse: false },
  error: { emoji: '❌', icon: 'exclamation-triangle', variant: 'danger', pulse: false },
};

// ---------------------------------------------------------------------------
// Agent Activity display definitions
// ---------------------------------------------------------------------------

export const ACTIVITY_DISPLAY: Record<AgentActivity, StateDisplay> = {
  working: { emoji: '🔄', icon: 'circle-fill', variant: 'success', pulse: false },
  thinking: { emoji: '💭', icon: 'lightning-charge', variant: 'primary', pulse: true },
  executing: { emoji: '⚙️', icon: 'gear', variant: 'primary', pulse: true },
  // Display only: the activity is still 'waiting_for_input' in the API and
  // CLI. Pairs with 'blocked' below: one waits on a person, the other on
  // agents or events (ptone/scion#3301).
  waiting_for_input: {
    emoji: '💬',
    icon: 'chat-dots',
    variant: 'warning',
    pulse: false,
    label: 'waiting for user',
  },
  // Display only: the activity is still 'blocked' in the API and CLI. Users
  // read 'blocked' as broken; it means waiting on an external dependency
  // (another agent, a user reply, a scheduled event) (ptone/scion#1571).
  blocked: {
    emoji: '🕓',
    icon: 'clock-history',
    variant: 'neutral',
    pulse: false,
    label: 'waiting on others',
  },
  completed: { emoji: '✅', icon: 'check-circle', variant: 'success', pulse: false },
  limits_exceeded: {
    emoji: '🚫',
    icon: 'exclamation-octagon',
    variant: 'danger',
    pulse: false,
    label: 'limits exceeded',
  },
  stalled: { emoji: '⏳', icon: 'hourglass-bottom', variant: 'warning', pulse: false },
  offline: { emoji: '📡', icon: 'wifi-off', variant: 'neutral', pulse: false },
};

// ---------------------------------------------------------------------------
// Lookup helpers
// ---------------------------------------------------------------------------

/**
 * Get the display config for a status string (phase, activity, or other).
 * Falls back to a neutral default for unknown values.
 */
export function getStateDisplay(status: string): StateDisplay {
  if (status in PHASE_DISPLAY) {
    return PHASE_DISPLAY[status as AgentPhase];
  }
  if (status in ACTIVITY_DISPLAY) {
    return ACTIVITY_DISPLAY[status as AgentActivity];
  }
  return { emoji: '', icon: '', variant: 'neutral', pulse: false };
}

/**
 * Human-readable label for an agent phase or activity: the display label
 * when one is defined (e.g. 'blocked' → 'waiting on others'), otherwise the
 * raw value. Use this wherever a state name is shown as text, so no call
 * site renders a raw state that has a display label.
 */
export function stateLabel(status: string): string {
  return getStateDisplay(status).label ?? status;
}

// ---------------------------------------------------------------------------
// Provisioned, not started (ptone/scion#2929)
// ---------------------------------------------------------------------------

/** Status label for a provision-only agent; the same wording as the CLI. */
export const PROVISIONED_ONLY_LABEL = 'created (not started)';

/**
 * Whether to show `agent` as provisioned but not started. The hub computes
 * `provisionedOnly`; the phase check hides a stale flag as soon as an SSE
 * delta moves the agent out of `created` (a start is under way).
 */
export function isProvisionedOnly(agent: Pick<Agent, 'phase' | 'provisionedOnly'>): boolean {
  return agent.provisionedOnly === true && agent.phase === 'created';
}

interface AgentStatusBadgeOptions {
  /** Badge status; defaults to getAgentDisplayStatus(agent). */
  status?: string;
  /** Badge label; defaults to stateLabel(status). */
  label?: string;
  size?: 'small' | 'medium' | 'large';
}

/**
 * An agent status badge. Use it for every agent status badge: it shows a
 * provision-only agent as "created (not started)" with a start hint.
 */
export function agentStatusBadge(
  agent: Agent,
  { status = getAgentDisplayStatus(agent), label, size }: AgentStatusBadgeOptions = {}
): TemplateResult {
  const po = isProvisionedOnly(agent);
  const hint = po ? `Not started yet. Use Start, or run: scion start ${agent.name}` : undefined;
  return html`<scion-status-badge
    status=${status}
    label=${po ? PROVISIONED_ONLY_LABEL : (label ?? stateLabel(status))}
    title=${ifDefined(hint)}
    size=${ifDefined(size)}
  ></scion-status-badge>`;
}
