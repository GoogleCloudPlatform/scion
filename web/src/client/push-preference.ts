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
 * Browser push preferences: one opt-in per alert category.
 *
 * Two categories, each with its own toggle:
 *
 * - `chat`: a popup for every new message in a conversation the user
 *   takes part in (DMs and member threads), muted conversations excepted.
 *   Read by the chat notification dispatcher.
 * - `agent`: a popup for agent events (completed, needs input, and so on)
 *   from the notification tray.
 *
 * The profile settings page and the notification tray write these, and the
 * dispatchers read them, through this module only, so a third key with a
 * slightly different name cannot appear by accident.
 *
 * "Enabled" always means both halves: the user opted in *and* the browser
 * granted permission. Permission can be revoked in site settings without the
 * page knowing, so the stored flag alone is never sufficient. Permission is
 * per site, so both categories share it.
 */

/** An alert category with its own opt-in. */
export type PushCategory = 'chat' | 'agent';

/** localStorage keys, one per category. */
export const PUSH_STORAGE_KEYS: Readonly<Record<PushCategory, string>> = {
  chat: 'scion-push-chat-messages',
  agent: 'scion-push-agent-events',
};

/**
 * The single key used before the split. Its value seeds both categories the
 * first time either is read, so an existing opt-in survives the upgrade.
 */
export const LEGACY_PUSH_STORAGE_KEY = 'scion-push-notifications';

/** Fired on `window` whenever a preference changes, so open surfaces sync. */
export const PUSH_PREFERENCE_EVENT = 'scion-push-preference-changed';

export interface PushPreferenceDetail {
  category: PushCategory;
  enabled: boolean;
}

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

/**
 * Copies the legacy single opt-in into any category that has no value of
 * its own yet, then removes the legacy key. Idempotent; safe to call on
 * every read.
 */
export function migrateLegacyPushPreference(): void {
  try {
    const legacy = localStorage.getItem(LEGACY_PUSH_STORAGE_KEY);
    if (legacy === null) return;
    for (const key of Object.values(PUSH_STORAGE_KEYS)) {
      if (localStorage.getItem(key) === null) localStorage.setItem(key, legacy);
    }
    localStorage.removeItem(LEGACY_PUSH_STORAGE_KEY);
  } catch {
    // Storage can throw in private-browsing modes; nothing to migrate.
  }
}

/** The stored opt-in for a category, independent of browser permission. */
export function isPushOptedIn(category: PushCategory): boolean {
  migrateLegacyPushPreference();
  try {
    return localStorage.getItem(PUSH_STORAGE_KEYS[category]) === 'true';
  } catch {
    // Storage can throw in private-browsing modes; treat as opted out.
    return false;
  }
}

/**
 * True only when a browser notification of this category may actually be
 * shown: supported, permission granted, and the user opted in.
 */
export function canShowPushNotification(category: PushCategory): boolean {
  return (
    isPushSupported() && window.Notification.permission === 'granted' && isPushOptedIn(category)
  );
}

/** Writes a category's opt-in and notifies other surfaces. */
export function setPushOptIn(category: PushCategory, enabled: boolean): void {
  migrateLegacyPushPreference();
  try {
    localStorage.setItem(PUSH_STORAGE_KEYS[category], enabled ? 'true' : 'false');
  } catch {
    // Non-fatal: the toggle simply won't persist across reloads.
  }
  if (typeof window !== 'undefined') {
    window.dispatchEvent(
      new CustomEvent<PushPreferenceDetail>(PUSH_PREFERENCE_EVENT, {
        detail: { category, enabled },
      })
    );
  }
}

/**
 * Turns a category on, requesting browser permission if it has not been
 * decided.
 *
 * MUST be called from a user gesture — browsers ignore (or permanently deny)
 * `requestPermission()` outside one, and a permission prompt on page load is
 * the behaviour this feature is explicitly not allowed to have.
 *
 * Returns the resulting permission state; the opt-in is only stored as `true`
 * when that state is 'granted'.
 */
export async function enablePushWithPermission(
  category: PushCategory
): Promise<PushPermissionState> {
  if (!isPushSupported()) return 'unsupported';

  const permission =
    window.Notification.permission === 'default'
      ? await window.Notification.requestPermission()
      : window.Notification.permission;

  setPushOptIn(category, permission === 'granted');
  return permission;
}
