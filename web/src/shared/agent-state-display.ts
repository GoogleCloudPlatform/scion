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
  error: { emoji: '❌', icon: 'exclamation-triangle', variant: 'danger', pulse: false },
};

// ---------------------------------------------------------------------------
// Agent Activity display definitions
// ---------------------------------------------------------------------------

export const ACTIVITY_DISPLAY: Record<AgentActivity, StateDisplay> = {
  idle: { emoji: '💤', icon: 'circle-fill', variant: 'success', pulse: false },
  thinking: { emoji: '💭', icon: 'lightning-charge', variant: 'primary', pulse: true },
  executing: { emoji: '⚙️', icon: 'gear', variant: 'primary', pulse: true },
  waiting_for_input: {
    emoji: '💬',
    icon: 'chat-dots',
    variant: 'warning',
    pulse: false,
    label: 'waiting for input',
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
