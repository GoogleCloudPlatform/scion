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
 * Browser push preference — the single master toggle.
 *
 * Three surfaces read this: the profile settings page, the notification tray,
 * and the chat notification dispatcher. They used to reach for the localStorage
 * key directly, which is how a second toggle with a slightly different key
 * gets written by accident. There is one key and one predicate here.
 *
 * "Enabled" always means both halves: the user opted in *and* the browser
 * granted permission. Permission can be revoked in site settings without the
 * page knowing, so the stored flag alone is never sufficient.
 */

/** localStorage key. Historical name, kept so existing opt-ins survive. */
export const PUSH_STORAGE_KEY = 'scion-push-notifications';

/** Fired on `window` whenever the preference changes, so open surfaces sync. */
export const PUSH_PREFERENCE_EVENT = 'scion-push-preference-changed';

export type PushPermissionState = NotificationPermission | 'unsupported';

/** Whether this browser has the Notification API at all. */
export function isPushSupported(): boolean {
  return typeof window !== 'undefined' && 'Notification' in window;
}

/** Current browser permission, or 'unsupported'. */
export function pushPermission(): PushPermissionState {
  if (!isPushSupported()) return 'unsupported';
  return window.Notification.permission;
}

/** The stored opt-in, independent of browser permission. */
export function isPushOptedIn(): boolean {
  try {
    return localStorage.getItem(PUSH_STORAGE_KEY) === 'true';
  } catch {
    // Storage can throw in private-browsing modes; treat as opted out.
    return false;
  }
}

/**
 * True only when a browser notification may actually be shown: supported,
 * permission granted, and the user opted in.
 */
export function canShowPushNotification(): boolean {
  return isPushSupported() && window.Notification.permission === 'granted' && isPushOptedIn();
}

/** Writes the opt-in and notifies other surfaces. */
export function setPushOptIn(enabled: boolean): void {
  try {
    localStorage.setItem(PUSH_STORAGE_KEY, enabled ? 'true' : 'false');
  } catch {
    // Non-fatal: the toggle simply won't persist across reloads.
  }
  if (typeof window !== 'undefined') {
    window.dispatchEvent(
      new CustomEvent<{ enabled: boolean }>(PUSH_PREFERENCE_EVENT, { detail: { enabled } })
    );
  }
}

/**
 * Turns push on, requesting browser permission if it has not been decided.
 *
 * MUST be called from a user gesture — browsers ignore (or permanently deny)
 * `requestPermission()` outside one, and a permission prompt on page load is
 * the behaviour this feature is explicitly not allowed to have.
 *
 * Returns the resulting permission state; the opt-in is only stored as `true`
 * when that state is 'granted'.
 */
export async function enablePushWithPermission(): Promise<PushPermissionState> {
  if (!isPushSupported()) return 'unsupported';

  const permission =
    window.Notification.permission === 'default'
      ? await window.Notification.requestPermission()
      : window.Notification.permission;

  setPushOptIn(permission === 'granted');
  return permission;
}
