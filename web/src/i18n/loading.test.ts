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

import { afterEach, expect, it, vi } from 'vitest';
import type { MessageEngine } from './engine.js';

const loadEngine = vi.hoisted(() => vi.fn<() => Promise<MessageEngine>>());
vi.mock('./engine.js', () => ({ loadEngine }));

import {
  getLocale,
  initializeLocalization,
  isLocalizationEnabled,
  setLocalePreference,
  getLocalePreference,
  t,
} from './index.js';

afterEach(async () => {
  vi.useRealTimers();
  await initializeLocalization(false);
  loadEngine.mockReset();
  vi.restoreAllMocks();
});

it('returns an unknown message key before the localization runtime loads', () => {
  expect(t('missing.message' as Parameters<typeof t>[0])).toBe('missing.message');
});

it('keeps the English shell usable when an opt-in chunk fails to load', async () => {
  const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
  loadEngine.mockRejectedValueOnce(new Error('chunk unavailable'));
  await initializeLocalization(true);
  expect(isLocalizationEnabled()).toBe(false);
  expect(getLocale()).toBe('en');
  expect(t('page.dashboard')).toBe('Dashboard');
  expect(t('header.unread', { count: 1 })).toBe('1 unread conversation');
  expect(t('header.unread', { count: 5 })).toBe('5 unread conversations');
  expect(warn.mock.calls.filter(([message]) => String(message).startsWith('[i18n]'))).toHaveLength(
    1
  );
});

it('does not apply a stale load after a newer disable', async () => {
  let finish!: (engine: MessageEngine) => void;
  loadEngine.mockReturnValueOnce(
    new Promise((resolve) => {
      finish = resolve;
    })
  );
  const pending = initializeLocalization(true);
  await vi.waitFor(() => expect(loadEngine).toHaveBeenCalledOnce());
  await initializeLocalization(false);
  const setLocale = vi.fn();
  finish({ setLocale, loadLocale: () => Promise.resolve(), translate: () => 'stale' });
  await pending;
  expect(isLocalizationEnabled()).toBe(false);
  expect(t('page.dashboard')).toBe('Dashboard');
  expect(setLocale).not.toHaveBeenCalled();
});

it('bounds a stalled opt-in load and ignores its eventual completion', async () => {
  vi.useFakeTimers();
  let finish!: (engine: MessageEngine) => void;
  loadEngine.mockReturnValueOnce(
    new Promise((resolve) => {
      finish = resolve;
    })
  );
  const pending = initializeLocalization(true);
  await vi.waitFor(() => expect(loadEngine).toHaveBeenCalledOnce());
  await vi.advanceTimersByTimeAsync(1500);
  await pending;
  expect(isLocalizationEnabled()).toBe(false);
  expect(document.documentElement.lang).toBe('en');
  expect(t('page.dashboard')).toBe('Dashboard');
  const setLocale = vi.fn();
  finish({ setLocale, loadLocale: () => Promise.resolve(), translate: () => 'late' });
  await vi.advanceTimersByTimeAsync(0);
  expect(setLocale).not.toHaveBeenCalled();
  expect(isLocalizationEnabled()).toBe(false);
});

it('keeps a newer choice when an older language switch finishes late', async () => {
  let finish!: () => void;
  const setLocale = vi.fn();
  const loadLocale = vi.fn((code: string) =>
    code === 'zh-CN'
      ? new Promise<void>((resolve) => {
          finish = resolve;
        })
      : Promise.resolve()
  );
  loadEngine.mockResolvedValueOnce({ setLocale, loadLocale, translate: () => 'English' });
  await initializeLocalization(true);
  const pending = setLocalePreference('zh-CN');
  await setLocalePreference('en');
  finish();
  await pending;
  expect(getLocale()).toBe('en');
  expect(getLocalePreference()).toBe('en');
  expect(setLocale).not.toHaveBeenCalledWith('zh-CN');
});
it('keeps the previous preference when a language switch fails', async () => {
  vi.spyOn(console, 'warn').mockImplementation(() => {});
  loadEngine.mockResolvedValueOnce({
    setLocale: vi.fn(),
    loadLocale: (code) =>
      code === 'zh-CN' ? Promise.reject(new Error('offline')) : Promise.resolve(),
    translate: () => 'English',
  });
  await initializeLocalization(true);
  const previous = getLocalePreference();
  await setLocalePreference('zh-CN');
  expect(getLocale()).toBe('en');
  expect(getLocalePreference()).toBe(previous);
});
it('invalidates a pending initial catalog when the boot budget expires', async () => {
  vi.useFakeTimers();
  vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['zh-CN']);
  let finish!: () => void;
  const loadLocale = vi.fn(
    () =>
      new Promise<void>((resolve) => {
        finish = resolve;
      })
  );
  const setLocale = vi.fn();
  loadEngine.mockResolvedValueOnce({ setLocale, loadLocale, translate: () => 'English' });
  const pending = initializeLocalization(true);
  await vi.waitFor(() => expect(loadLocale).toHaveBeenCalledOnce());
  await vi.advanceTimersByTimeAsync(1500);
  await pending;
  expect(isLocalizationEnabled()).toBe(false);
  expect(document.documentElement.lang).toBe('en');
  finish();
  await vi.advanceTimersByTimeAsync(0);
  expect(setLocale).not.toHaveBeenCalledWith('zh-CN');
  expect(document.documentElement.lang).toBe('en');
});
