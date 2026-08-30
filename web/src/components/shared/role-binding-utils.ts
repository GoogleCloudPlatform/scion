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
 * Shared Role Binding Utilities
 *
 * Constants and helper functions shared across role-binding-related
 * components (admin-role-bindings, effective-role-provenance,
 * project-members-editor).
 */

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

/**
 * System-scope roles that may only be assigned to individual users (not groups).
 * Used in the admin role-bindings page.
 */
export const SYSTEM_DIRECT_USER_ONLY_ROLES = ['super-admin', 'project-owner'];

/**
 * Project-scope roles that may only be assigned to individual users (not groups).
 * Used in the project membership editor.
 */
export const PROJECT_DIRECT_USER_ONLY_ROLES = ['project-owner'];

/**
 * Role names that represent project ownership. A project must retain at least
 * one direct user with one of these roles.
 */
export const PROJECT_OWNER_ROLE_NAMES = ['project-owner', 'owner'];

// ---------------------------------------------------------------------------
// Lifecycle helpers
// ---------------------------------------------------------------------------

/** Lifecycle status of a role binding. */
export type LifecycleStatus = 'active' | 'expired' | 'pending';

/**
 * Determine the lifecycle status of a binding based on its notBefore/expiresAt
 * fields. Expired takes precedence over pending.
 */
export function getLifecycleStatus(binding: {
  notBefore?: string;
  expiresAt?: string;
}): LifecycleStatus {
  const now = Date.now();

  if (binding.expiresAt) {
    const expires = new Date(binding.expiresAt).getTime();
    if (!isNaN(expires) && expires < now) return 'expired';
  }

  if (binding.notBefore) {
    const notBefore = new Date(binding.notBefore).getTime();
    if (!isNaN(notBefore) && notBefore > now) return 'pending';
  }

  return 'active';
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

/**
 * Format a date string to a short human-readable form.
 * Example: "Aug 30, 2026, 5:42 PM"
 */
export function formatDateTime(dateString: string): string {
  try {
    const date = new Date(dateString);
    if (isNaN(date.getTime())) return dateString;
    return date.toLocaleString('en-US', {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
      hour: 'numeric',
      minute: '2-digit',
    });
  } catch {
    return dateString;
  }
}

// ---------------------------------------------------------------------------
// Icon helpers
// ---------------------------------------------------------------------------

/**
 * Return the Bootstrap icon name for a given principal type.
 */
export function getPrincipalIcon(principalType: string): string {
  switch (principalType) {
    case 'user':
      return 'person';
    case 'group':
      return 'diagram-3';
    case 'agent':
      return 'cpu';
    default:
      return 'question-circle';
  }
}
