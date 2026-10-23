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

import { getLocale, initializeLocalization, isLocalizationEnabled, t } from './index.js';

afterEach(async () => {
  await initializeLocalization(false);
  loadEngine.mockReset();
  vi.restoreAllMocks();
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
  expect(warn).toHaveBeenCalledOnce();
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
  finish({ setLocale, translate: () => 'stale' });
  await pending;
  expect(isLocalizationEnabled()).toBe(false);
  expect(t('page.dashboard')).toBe('Dashboard');
  expect(setLocale).not.toHaveBeenCalled();
});
