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
 * Shared access-denied rendering helper.
 *
 * Both app-shell and chat-shell use this to produce the two-line
 * insufficient-permission toast, ensuring consistent wording and safe
 * redaction across every surface that handles the `scion:access-denied`
 * event.
 */

import type { AccessDeniedDetail } from '../client/api.js';
import { showToast } from './toast.js';

/** Human-friendly labels for machine-readable action codes. */
const ACTION_LABELS: Record<string, string> = {
  create: 'create',
  read: 'view',
  update: 'update',
  delete: 'delete',
  manage: 'manage',
  attach: 'manage',
  list: 'list',
  execute: 'execute',
  promote: 'promote',
  suspend: 'suspend',
};

/**
 * Build a human-readable label for a denied action.
 * Returns the friendly label when one exists, falls back to the raw
 * code, or undefined when no action is known.
 */
function actionLabel(action?: string): string | undefined {
  if (!action) return undefined;
  return ACTION_LABELS[action] ?? action;
}

/**
 * Format the two-line access-denied message.
 *
 * - Line 1 (primary): friendly explanation, never exposes internals.
 * - Line 2 (secondary): actionable context (denied action / resource type)
 *   when the backend provided structured details; omitted for legacy 403s.
 *
 * The resource ID and internal denial reason are never surfaced.
 */
export function formatAccessDenied(detail: AccessDeniedDetail): {
  primary: string;
  secondary: string | undefined;
} {
  // Primary: prefer the backend message when it is not the generic
  // "Insufficient permissions" (which adds no information). Custom
  // messages from authorizeMsg carry real user-facing guidance.
  const isGeneric =
    !detail.reason ||
    detail.reason === 'Insufficient permissions';

  const primary = isGeneric
    ? "You don't have permission to perform this action."
    : detail.reason!;

  // Secondary: build from structured detail (denied_action / resource_type).
  const label = actionLabel(detail.action);
  const resource = detail.resource;

  let secondary: string | undefined;
  if (label && resource) {
    secondary = `Permission needed: ${label} on ${resource}`;
  } else if (label && label !== 'forbidden') {
    secondary = `Permission needed: ${label}`;
  } else if (resource) {
    secondary = `Resource: ${resource}`;
  }
  // When neither is available (legacy 403), secondary stays undefined —
  // the toast degrades to a single line.

  return { primary, secondary };
}

// ---------------------------------------------------------------------------
// Cross-event dedup state — centralized so app-shell and chat-shell cannot
// each maintain divergent clocks. Two events with the same action+resource
// key within DEDUP_WINDOW_MS are coalesced into one toast.
//
// A per-key Map is used rather than a single last-key so that the pattern
// A→B→A within the window correctly suppresses the second A while still
// rendering B.
// ---------------------------------------------------------------------------

const DEDUP_WINDOW_MS = 500;
const _recentToasts = new Map<string, number>();

/** Exported for testing — resets the internal dedup state. */
export function _resetDedupState(): void {
  _recentToasts.clear();
}

/**
 * Returns true if this key was seen within the dedup window (i.e. the
 * caller should suppress the toast). Prunes expired entries on each call
 * to bound the Map size.
 */
function _isDuplicate(key: string, now: number): boolean {
  // Prune expired entries first to keep the Map bounded.
  for (const [k, ts] of _recentToasts) {
    if (now - ts >= DEDUP_WINDOW_MS) {
      _recentToasts.delete(k);
    }
  }

  const prev = _recentToasts.get(key);
  if (prev !== undefined && now - prev < DEDUP_WINDOW_MS) {
    // Duplicate within window — suppress without extending the window.
    return true;
  }
  _recentToasts.set(key, now);
  return false;
}

/**
 * Show a two-line access-denied toast.
 *
 * This is the single renderer both app-shell and chat-shell delegate to.
 * The first line is the friendly primary message; the second line (when
 * present) gives the denied action/resource context on a separate line
 * in a smaller font.
 *
 * Repeated events with the same action+resource key within 500ms are
 * coalesced: the first fires a toast, subsequent duplicates are suppressed.
 * Genuinely distinct denials (different action or resource) always fire.
 */
export function showAccessDeniedToast(detail: AccessDeniedDetail): void {
  // Coalesce rapid duplicate 403 toasts: suppress if same key within window.
  const key = `${detail.action ?? ''}:${detail.resource ?? ''}`;
  if (_isDuplicate(key, Date.now())) {
    return;
  }
  const { primary, secondary } = formatAccessDenied(detail);

  if (!secondary) {
    showToast(primary, 'warning');
    return;
  }

  // Build a two-line toast: reuse showToast's DOM pattern but with a
  // secondary detail line.
  const alert = Object.assign(document.createElement('sl-alert'), {
    variant: 'warning',
    closable: true,
    duration: 6000,
  });

  const iconEl = document.createElement('sl-icon');
  iconEl.setAttribute('name', 'exclamation-triangle');
  iconEl.setAttribute('slot', 'icon');
  alert.appendChild(iconEl);

  const primarySpan = document.createElement('span');
  primarySpan.textContent = primary;
  alert.appendChild(primarySpan);

  const br = document.createElement('br');
  alert.appendChild(br);

  const secondarySpan = document.createElement('span');
  secondarySpan.textContent = secondary;
  secondarySpan.style.fontSize = '0.85em';
  secondarySpan.style.opacity = '0.8';
  alert.appendChild(secondarySpan);

  alert.addEventListener(
    'sl-after-hide',
    () => {
      alert.remove();
    },
    { once: true }
  );

  document.body.appendChild(alert);
  void (alert as HTMLElement & { toast(): Promise<void> }).toast();
}
