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
 * Core Agent State Display Definitions
 *
 * Minimal phase/activity visual lookup used by `scion-status-badge` in the
 * entry chunk. Keeping this separate from the tooltip copy and badge helper
 * functions in `agent-state-display.ts` lets Vite keep the heavier helpers
 * out of `assets/main.js` until a page that renders agent badges loads.
 */

import type { AgentPhase, AgentActivity } from './types.js';

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
