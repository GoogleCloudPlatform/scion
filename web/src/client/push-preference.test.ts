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
 * Tests for the split alert preferences: chat messages and agent events.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import {
  canShowPushNotification,
  enablePushWithPermission,
  isPushOptedIn,
  LEGACY_PUSH_STORAGE_KEY,
  migrateLegacyPushPreference,
  PUSH_PREFERENCE_EVENT,
  PUSH_STORAGE_KEYS,
  setPushOptIn,
} from './push-preference.js';

class FakeNotification {
  static permission: NotificationPermission = 'granted';
  static requestPermission = vi.fn(async () => FakeNotification.permission);
}

beforeEach(() => {
  localStorage.clear();
  FakeNotification.permission = 'granted';
  FakeNotification.requestPermission.mockClear();
  (window as unknown as { Notification: unknown }).Notification = FakeNotification;
});

afterEach(() => {
  localStorage.clear();
});

describe('legacy migration', () => {
  it('copies an old opt-in into both categories and drops the old key', () => {
    localStorage.setItem(LEGACY_PUSH_STORAGE_KEY, 'true');
    expect(isPushOptedIn('chat')).toBe(true);
    expect(isPushOptedIn('agent')).toBe(true);
    expect(localStorage.getItem(LEGACY_PUSH_STORAGE_KEY)).toBeNull();
  });

  it('copies an old opt-out too', () => {
    localStorage.setItem(LEGACY_PUSH_STORAGE_KEY, 'false');
    migrateLegacyPushPreference();
    expect(localStorage.getItem(PUSH_STORAGE_KEYS.chat)).toBe('false');
    expect(localStorage.getItem(PUSH_STORAGE_KEYS.agent)).toBe('false');
  });

  it('never overwrites a value a category already has', () => {
    localStorage.setItem(PUSH_STORAGE_KEYS.chat, 'false');
    localStorage.setItem(LEGACY_PUSH_STORAGE_KEY, 'true');
    migrateLegacyPushPreference();
    expect(isPushOptedIn('chat')).toBe(false);
    expect(isPushOptedIn('agent')).toBe(true);
  });

  it('is a no-op without the old key', () => {
    migrateLegacyPushPreference();
    expect(localStorage.length).toBe(0);
    expect(isPushOptedIn('chat')).toBe(false);
  });
});

describe('per-category opt-in', () => {
  it('stores each category on its own', () => {
    setPushOptIn('chat', true);
    expect(isPushOptedIn('chat')).toBe(true);
    expect(isPushOptedIn('agent')).toBe(false);
    setPushOptIn('agent', true);
    setPushOptIn('chat', false);
    expect(isPushOptedIn('chat')).toBe(false);
    expect(isPushOptedIn('agent')).toBe(true);
  });

  it('announces the category that changed', () => {
    const seen: unknown[] = [];
    const listener = (e: Event): void => {
      seen.push((e as CustomEvent).detail);
    };
    window.addEventListener(PUSH_PREFERENCE_EVENT, listener);
    try {
      setPushOptIn('agent', true);
    } finally {
      window.removeEventListener(PUSH_PREFERENCE_EVENT, listener);
    }
    expect(seen).toEqual([{ category: 'agent', enabled: true }]);
  });

  it('needs browser permission as well as the opt-in', () => {
    setPushOptIn('chat', true);
    expect(canShowPushNotification('chat')).toBe(true);
    FakeNotification.permission = 'denied';
    expect(canShowPushNotification('chat')).toBe(false);
  });

  it('enables only the requested category after permission is granted', async () => {
    FakeNotification.permission = 'default';
    FakeNotification.requestPermission.mockImplementation(async () => {
      FakeNotification.permission = 'granted';
      return 'granted';
    });
    expect(await enablePushWithPermission('chat')).toBe('granted');
    expect(isPushOptedIn('chat')).toBe(true);
    expect(isPushOptedIn('agent')).toBe(false);
  });

  it('stores an opt-out when permission is refused', async () => {
    FakeNotification.permission = 'denied';
    expect(await enablePushWithPermission('agent')).toBe('denied');
    expect(isPushOptedIn('agent')).toBe(false);
  });
});
