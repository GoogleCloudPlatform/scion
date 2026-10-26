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

// A test-only third locale exercises the same registry contract used by shipped
// languages. No conditional language handling is added to components/runtime.
vi.mock('./locales.js', async (importOriginal) => {
  const original = await importOriginal<typeof import('./locales.js')>();
  const registry: import('./locales.js').LocaleDefinition[] = [
    ...original.locales,
    {
      code: 'ar',
      name: 'العربية',
      dir: 'rtl',
      aliases: [],
      loadMessages: () =>
        Promise.resolve({
          'header.unread_zero': 'صفر {{count}}',
          'header.unread_one': 'واحد {{count}}',
          'header.unread_two': 'اثنان {{count}}',
          'header.unread_few': 'قليل {{count}}',
          'header.unread_many': 'كثير {{count}}',
          'header.unread_other': 'آخر {{count}}',
          'header.help': '',
        }),
    },
  ];
  return {
    ...original,
    locales: registry,
    resolveLocale: (preferences: readonly string[]) =>
      original.resolveLocale(preferences, registry),
  };
});

import { initializeLocalization, setLocalePreference, t } from './index.js';

afterEach(async () => {
  await initializeLocalization(false);
  localStorage.clear();
});

it('supports a third language, RTL metadata, six plural forms and English fallback', async () => {
  await initializeLocalization(true);
  await setLocalePreference('ar');
  expect(document.documentElement.lang).toBe('ar');
  expect(document.documentElement.dir).toBe('rtl');
  for (const [count, word] of [
    [0, 'صفر'],
    [1, 'واحد'],
    [2, 'اثنان'],
    [3, 'قليل'],
    [11, 'كثير'],
    [100, 'آخر'],
  ] as const) {
    expect(t('header.unread', { count })).toBe(`${word} ${count}`);
  }
  expect(t('page.dashboard')).toBe('Dashboard');
  expect(t('header.help')).toBe('Help');
  await setLocalePreference('en');
  expect(document.documentElement.dir).toBe('ltr');
});
