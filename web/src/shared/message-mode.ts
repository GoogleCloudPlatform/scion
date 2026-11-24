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
  hub: {
    icon: 'globe',
    color: 'success',
    label: 'Hub',
    description: 'Project messaging plus permitted agents in other projects on this Hub',
  },
  project: {
    icon: 'globe2',
    color: 'success',
    label: 'Project',
    description: 'Messages all agents and users in the project',
  },
  tree: {
    icon: 'tree',
    color: 'primary',
    label: 'Tree plus owner',
    description: 'Messages agents in its creation tree (also in Tree mode) and its owner',
  },
  direct: {
    icon: 'diagram-3',
    color: 'primary',
    label: 'Direct plus owner',
    description: 'Messages its direct parent and children (also in Direct mode) and its owner',
  },
  owner: {
    icon: 'person-lines-fill',
    color: 'warning',
    label: 'Owner',
    description: 'Messages its owner only; no agent-to-agent',
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
  hub: 0,
  project: 1,
  tree: 2,
  direct: 3,
  owner: 4,
  none: 5,
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

// ---------------------------------------------------------------------------
// Denial reason codes and user-facing copy
// ---------------------------------------------------------------------------

export type MessageDenialReason =
  | 'mode_none'
  | 'mode_none_sender'
  | 'mode_owner_only'
  | 'mode_direct_no_edge'
  | 'mode_tree_outside'
  | 'mode_incompatible'
  | 'missing_permission'
  | 'cross_project_disabled'
  | 'cross_project_sender_mode'
  | 'cross_project_target_mode'
  | 'cross_project_inbound_none'
  | 'cross_project_origin_not_member'
  | 'cross_project_untrusted_origin'
  | 'cross_project_surface_unsupported'
  | 'denied';

/**
 * Maps denial reason codes to user-facing copy.
 * The {recipient} and {sender} placeholders should be replaced by the caller.
 */
export const DENIAL_REASON_COPY: Record<MessageDenialReason, string> = {
  mode_none: '{recipient} is sealed (mode: none) and cannot receive messages.',
  mode_none_sender: 'You cannot send messages: this agent is sealed (mode: none).',
  mode_owner_only: '{recipient} only accepts messages from its owner.',
  mode_direct_no_edge:
    '{recipient} is in Direct mode and only exchanges messages with its direct parent and children that are also in Direct mode.',
  mode_tree_outside:
    '{recipient} is in Tree mode and only exchanges messages with Tree-mode agents in its own creation tree.',
  mode_incompatible: '{sender} and {recipient} are in message modes that cannot exchange messages.',
  missing_permission: 'You do not have permission to message this agent.',
  cross_project_disabled: 'Cross-project messaging is disabled by the Hub administrator.',
  cross_project_sender_mode: '{sender} must be in Hub mode to send messages across projects.',
  cross_project_target_mode:
    '{recipient} must be in Project or Hub mode to receive cross-project messages.',
  cross_project_inbound_none:
    "{recipient}'s project does not accept messages from external agents.",
  cross_project_origin_not_member:
    "Your originating user is not a member of {recipient}'s project.",
  cross_project_untrusted_origin:
    "The sender's identity origin could not be verified for cross-project messaging.",
  cross_project_surface_unsupported:
    'Cross-project messaging is not supported for this conversation type.',
  denied: 'Message delivery denied.',
};

/**
 * Get user-facing denial message from reason code.
 * Substitutes agent names into the template.
 */
export function getDenialMessage(
  reason: string | undefined,
  recipientName?: string,
  senderName?: string
): string {
  const template = DENIAL_REASON_COPY[reason as MessageDenialReason];
  if (!template) {
    return 'Message delivery denied.';
  }
  // Function replacements: a name is inserted as is, with no `$&`-style
  // patterns expanded.
  return template
    .replace('{recipient}', () => recipientName || 'This agent')
    .replace('{sender}', () => senderName || 'This agent');
}
