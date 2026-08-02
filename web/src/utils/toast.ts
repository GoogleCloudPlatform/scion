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
 * Shared toast notification utility.
 *
 * Wraps Shoelace's `sl-alert.toast()` to provide a consistent, non-blocking
 * notification surface across the entire frontend. Replaces native `alert()`.
 */

export type ToastVariant =
  | 'primary'
  | 'success'
  | 'neutral'
  | 'warning'
  | 'danger';

export interface ToastOptions {
  /** Shoelace icon name. Defaults to a variant-appropriate icon. */
  icon?: string;
  /** Auto-dismiss duration in ms. Defaults vary by variant. */
  duration?: number;
  /** Show a close button. Default true. */
  closable?: boolean;
}

/** Maps variant to a sensible default icon (Bootstrap Icons via Shoelace). */
const DEFAULT_ICONS: Record<ToastVariant, string> = {
  danger: 'exclamation-octagon',
  warning: 'exclamation-triangle',
  success: 'check-circle',
  neutral: 'info-circle',
  primary: 'info-circle',
};

/** Default auto-dismiss durations in ms, per variant. */
const DEFAULT_DURATIONS: Record<ToastVariant, number> = {
  danger: 8000,
  warning: 6000,
  success: 3000,
  neutral: 5000,
  primary: 5000,
};

/**
 * Show a toast notification using Shoelace's built-in toast stack.
 *
 * @param message  The text to display.
 * @param variant  Visual style — defaults to `'danger'` (most calls are errors).
 * @param options  Optional overrides for icon, duration, and closable state.
 */
export function showToast(
  message: string,
  variant: ToastVariant = 'danger',
  options?: ToastOptions,
): void {
  const icon = options?.icon ?? DEFAULT_ICONS[variant];
  const duration = options?.duration ?? DEFAULT_DURATIONS[variant];
  const closable = options?.closable ?? true;

  const alert = Object.assign(document.createElement('sl-alert'), {
    variant,
    closable,
    duration,
  });

  // Build icon element via DOM API to avoid interpolating into innerHTML.
  const iconEl = document.createElement('sl-icon');
  iconEl.setAttribute('name', icon);
  iconEl.setAttribute('slot', 'icon');
  alert.appendChild(iconEl);

  // Append message text.
  const span = document.createElement('span');
  span.textContent = message;
  alert.appendChild(span);

  // Remove the alert element from the DOM after it hides to prevent
  // accumulation — same pattern used in confirm-dialog.ts.
  alert.addEventListener('sl-after-hide', () => {
    alert.remove();
  }, { once: true });

  document.body.appendChild(alert);
  void (alert as HTMLElement & { toast(): Promise<void> }).toast();
}
