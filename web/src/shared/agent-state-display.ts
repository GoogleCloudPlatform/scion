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

import { getAgentDisplayStatus, isKnownSuspendReason, isRetainedSuspendActivity } from './types.js';
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
  // Display only: the activity is still 'initializing' in the API. The
  // container is up but the harness has not shown it reads input yet;
  // messages sent now are held until it is ready (ptone/scion#4482).
  initializing: {
    emoji: '🚀',
    icon: 'arrow-repeat',
    variant: 'warning',
    pulse: true,
    label: 'starting',
  },
  working: { emoji: '🔄', icon: 'circle-fill', variant: 'success', pulse: false },
  thinking: { emoji: '💭', icon: 'lightning-charge', variant: 'primary', pulse: true },
  executing: { emoji: '⚙️', icon: 'gear', variant: 'primary', pulse: true },
  // Display only: the activity is still 'waiting_for_input' in the API and
  // CLI. The input may come from a user or a parent agent, so one label
  // covers both. Pairs with 'blocked' below (ptone/scion#3301).
  waiting_for_input: {
    emoji: '💬',
    icon: 'chat-dots',
    variant: 'warning',
    pulse: false,
    label: 'waiting on parent',
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
  // The agent reported it has nothing to do until new work arrives (between
  // assignments, or after answering a message). Healthy, not a warning: a
  // message for it ends idleness (ptone/scion#7826).
  idle: { emoji: '💤', icon: 'moon', variant: 'neutral', pulse: false },
  completed: { emoji: '✅', icon: 'check-circle', variant: 'success', pulse: false },
  limits_exceeded: {
    emoji: '🚫',
    icon: 'exclamation-octagon',
    variant: 'danger',
    pulse: false,
    label: 'limits exceeded',
  },
  // The harness is alive at its prompt but its last turn failed on a
  // model/API error; detail.message has the reason. A new message retries
  // (ptone/scion#7858).
  harness_error: {
    emoji: '⚠️',
    icon: 'exclamation-triangle',
    variant: 'danger',
    pulse: false,
    label: 'harness error',
  },
  stalled: { emoji: '⏳', icon: 'hourglass-bottom', variant: 'warning', pulse: false },
  offline: { emoji: '📡', icon: 'wifi-off', variant: 'neutral', pulse: false },
};

/**
 * One-line system explanations for key agent activity states shown in
 * status badge tooltips (ptone/scion#8723).
 */
export const ACTIVITY_TOOLTIPS: Partial<Record<AgentActivity, string>> = {
  completed: "Finished its most recent task. It isn't done forever and can take new work.",
  idle: 'Done with any known work. Similar to completed, but not tied to a specific task.',
  blocked: 'Active and expecting to hear back from another agent, a person, or a system.',
  stalled: "The system hasn't heard from the agent recently.",
};

/**
 * One-line explanation for an agent activity state, or `undefined` when no
 * explanation is defined for that state. Matched case-insensitively.
 */
export function activityTooltip(status?: string | null): string | undefined {
  if (!status) {
    return undefined;
  }
  const key = status.toLowerCase();
  if (key in ACTIVITY_TOOLTIPS) {
    return ACTIVITY_TOOLTIPS[key as AgentActivity];
  }
  return undefined;
}

/**
 * The agent's own reported status message formatted for a tooltip
 * (`"Agent says: ..."`), for agent-signaled states (`completed`, `idle`,
 * `blocked`), or `undefined` when unavailable or not applicable.
 *
 * For `completed`, `sciontool status task_completed` writes `taskSummary`
 * rather than `message`, so `taskSummary` takes precedence over `message`.
 */
export function agentReportedStatusHint(
  agent: Pick<Agent, 'detail' | 'message' | 'taskSummary'> | undefined | null,
  status?: string | null
): string | undefined {
  if (!agent || !status) {
    return undefined;
  }
  const key = status.toLowerCase();
  let raw: string | undefined;
  if (key === 'completed') {
    raw = agent.detail?.taskSummary || agent.taskSummary || agent.detail?.message || agent.message;
  } else if (key === 'idle' || key === 'blocked') {
    raw = agent.detail?.message || agent.message;
  } else {
    return undefined;
  }
  const trimmed = raw?.trim();
  return trimmed ? `Agent says: ${trimmed}` : undefined;
}

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

/**
 * Hint set by the Hub stall detector when an agent never reported any harness
 * hook event after start (ptone/scion#4556).
 * Keep in sync with state.StalledNoHookActivityHint in pkg/agent/state/state.go.
 */
export const STALLED_NO_HOOK_ACTIVITY_HINT =
  'no harness activity since start; check harness credentials/task';

/**
 * The badge tooltip for a harness_error status (including when retained on a
 * suspended agent): the reason the harness reported (detail.message), or
 * undefined for any other status.
 */
export function harnessErrorHint(
  agent: Pick<Agent, 'activity' | 'detail' | 'message'>,
  status: string
): string | undefined {
  const isHarnessError =
    status === 'harness_error' ||
    (status === 'suspended' && agent.activity?.toLowerCase() === 'harness_error');
  if (!isHarnessError) {
    return undefined;
  }
  return agent.detail?.message || agent.message || undefined;
}

/**
 * The badge tooltip for a stalled status when the Hub recorded the
 * no-hook-activity-since-start hint, or undefined for any other status or
 * stale pre-stall message.
 */
export function stalledHint(
  agent: Pick<Agent, 'detail' | 'message'>,
  status: string
): string | undefined {
  if (status !== 'stalled') {
    return undefined;
  }
  const msg = agent.detail?.message || agent.message;
  return msg === STALLED_NO_HOOK_ACTIVITY_HINT ? msg : undefined;
}

interface AgentStatusBadgeOptions {
  /** Badge status; defaults to getAgentDisplayStatus(agent). */
  status?: string;
  /** Badge label; defaults to stateLabel(status). */
  label?: string;
  size?: 'small' | 'medium' | 'large';
  /** A data-testid for the badge, for UI automation. */
  testId?: string;
}

/** The agent fields the suspended display reads. */
export interface SuspendedDisplayFields {
  phase?: string | undefined;
  activity?: string | undefined;
  suspendReason?: string | undefined;
}

/**
 * Status label for a suspended agent, matching `scion list`: the retained
 * pre-suspend activity after a middle dot, then the recorded suspend reason
 * in parentheses, e.g. `"suspended · completed (idle)"`, `"suspended · idle"`
 * or `"suspended (user)"`. Returns `undefined` when the agent is not
 * suspended, or is suspended with neither a retained activity nor a known
 * reason (plain `"suspended"`).
 */
export function suspendedStatusLabel(agent: SuspendedDisplayFields): string | undefined {
  if (agent.phase !== 'suspended') return undefined;
  const retained = isRetainedSuspendActivity(agent.activity);
  const reason = isKnownSuspendReason(agent.suspendReason) ? agent.suspendReason : undefined;
  if (!retained && !reason) return undefined;
  let label = stateLabel('suspended');
  if (retained) label += ` · ${stateLabel(agent.activity!.toLowerCase())}`;
  if (reason) label += ` (${reason})`;
  return label;
}

/**
 * A one-line explanation of a suspended agent's suspend reason, for a
 * tooltip, or `undefined` when the agent is not suspended or has no known
 * reason. Auto-wake of an agent the hub suspended needs the hub's
 * lifecycle.idle_suspend experiment; the idle policy only runs with it, so
 * an idle suspension always wakes, and a stalled one wakes where it is on.
 */
export function suspendReasonHint(agent: SuspendedDisplayFields): string | undefined {
  if (agent.phase !== 'suspended') return undefined;
  switch (agent.suspendReason) {
    case 'user':
      return 'Suspended by a user. Only a resume brings it back.';
    case 'idle':
      return 'Suspended by the hub because it was idle. New work for it wakes it automatically.';
    case 'stalled':
      return 'Suspended by the hub because it stalled. Where the hub has auto-wake on, new work for it wakes it.';
    default:
      return undefined;
  }
}

/** The non-empty hints, one per line, or undefined when there are none. */
function joinHints(...hints: (string | undefined)[]): string | undefined {
  const lines = hints.filter((h): h is string => !!h);
  return lines.length ? lines.join('\n') : undefined;
}

/**
 * Build the accessible tooltip text for an agent status badge, combining
 * any one-line state explanation, the agent's own reported status message
 * when available, and any diagnostic or suspend-reason hint.
 */
export function agentStatusTooltip(
  agent: Pick<
    Agent,
    | 'name'
    | 'phase'
    | 'activity'
    | 'suspendReason'
    | 'provisionedOnly'
    | 'detail'
    | 'message'
    | 'taskSummary'
  >,
  status?: string
): string | undefined {
  if (isProvisionedOnly(agent)) {
    return `Not started yet. Use Start, or run: scion start ${agent.name}`;
  }
  const displayStatus = getAgentDisplayStatus(agent as Agent);
  const effectiveStatus = status ?? displayStatus;
  const isDisplayStatus = effectiveStatus === displayStatus;
  return joinHints(
    activityTooltip(effectiveStatus),
    agentReportedStatusHint(agent, effectiveStatus),
    harnessErrorHint(agent, effectiveStatus) ?? stalledHint(agent, effectiveStatus),
    isDisplayStatus ? suspendReasonHint(agent) : undefined
  );
}

/**
 * An agent status badge. Use it for every agent status badge: it shows a
 * provision-only agent as "created (not started)" with a start hint, a
 * suspended agent as "suspended · <activity> (<reason>)" (see
 * suspendedStatusLabel) with the reason explained in its tooltip, and
 * accessible tooltips for key states (completed, idle, waiting on others,
 * stalled) including the agent's own reported status where available.
 */
export function agentStatusBadge(
  agent: Agent,
  { status = getAgentDisplayStatus(agent), label, size, testId }: AgentStatusBadgeOptions = {}
): TemplateResult {
  const po = isProvisionedOnly(agent);
  const isDisplayStatus = status === getAgentDisplayStatus(agent);
  const hint = agentStatusTooltip(agent, status);
  const defaultLabel = isDisplayStatus
    ? (suspendedStatusLabel(agent) ?? stateLabel(status))
    : stateLabel(status);
  return html`<scion-status-badge
    status=${status}
    label=${po ? PROVISIONED_ONLY_LABEL : (label ?? defaultLabel)}
    tooltip=${ifDefined(hint)}
    size=${ifDefined(size)}
    data-testid=${ifDefined(testId)}
  ></scion-status-badge>`;
}
