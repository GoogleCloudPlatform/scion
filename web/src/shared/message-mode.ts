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
 * Message Mode Display Definitions
 *
 * Centralized mapping of agent message modes to their visual
 * representation: icon, color variant, label, and description.
 * Edit this file to change how any message mode appears across
 * the web UI.
 */

import type { MessageMode } from './types.js';

/**
 * Visual configuration for a single message mode
 */
export interface MessageModeDisplay {
  /** Bootstrap icon name (used by sl-icon) */
  icon: string;
  /** Shoelace color variant */
  color: string;
  /** Human-readable short label */
  label: string;
  /** Longer description for tooltips */
  description: string;
}

// ---------------------------------------------------------------------------
// Message Mode display definitions
// ---------------------------------------------------------------------------

export const MESSAGE_MODE_DISPLAY: Record<MessageMode, MessageModeDisplay> = {
  project: {
    icon: 'globe2',
    color: 'success',
    label: 'Project',
    description: 'Messages all agents and users in the project',
  },
  branch: {
    icon: 'diagram-3',
    color: 'primary',
    label: 'Branch',
    description: 'Messages parent/children agents and lineage users',
  },
  lineage: {
    icon: 'person-lines-fill',
    color: 'warning',
    label: 'Lineage',
    description: 'Messages lineage users only; no agent-to-agent',
  },
  none: {
    icon: 'shield-lock',
    color: 'danger',
    label: 'Sealed',
    description: 'Cannot send or receive messages',
  },
};

// ---------------------------------------------------------------------------
// Sort order (most permissive first)
// ---------------------------------------------------------------------------

export const MODE_SORT_ORDER: Record<MessageMode, number> = {
  project: 0,
  branch: 1,
  lineage: 2,
  none: 3,
};

// ---------------------------------------------------------------------------
// Lookup helper
// ---------------------------------------------------------------------------

/**
 * Get the display config for a message mode.
 * Falls back to 'project' for undefined/unknown values (migration edge case)
 * or if the API returns a mode string not yet in the frontend vocabulary.
 */
export function getMessageModeDisplay(mode?: string): MessageModeDisplay {
  if (mode && mode in MESSAGE_MODE_DISPLAY) {
    return MESSAGE_MODE_DISPLAY[mode as MessageMode];
  }
  return MESSAGE_MODE_DISPLAY.project;
}
