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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { html, render } from 'lit';
import {
  getLocale,
  getLocalePreference,
  initializeLocalization,
  isLocalizationEnabled,
  LOCALE_STORAGE_KEY,
  setLocalePreference,
  t,
} from './index.js';
import { locales, resolveLocale } from './locales.js';

beforeEach(async () => {
  localStorage.clear();
  vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['en-US']);
  await initializeLocalization(false);
});

afterEach(async () => {
  vi.restoreAllMocks();
  await initializeLocalization(false);
  localStorage.clear();
});

describe('locale resolution and preferences', () => {
  it.each([
    [['en-AU'], 'en'],
    [['zh-CN'], 'zh-CN'],
    [['zh-cn-u-nu-latn'], 'zh-CN'],
    [['zh-Hans-CN'], 'zh-CN'],
    [['zh-SG'], 'zh-CN'],
    [['zh'], 'zh-CN'],
    [['zh-TW'], 'en'],
    [['zh-Hant'], 'en'],
    [['fr', 'zh-Hans'], 'zh-CN'],
    [['not_a_locale', 'zh-CN'], 'zh-CN'],
    [[], 'en'],
  ])('resolves %j to %s', (preferences, expected) => {
    expect(resolveLocale(preferences).code).toBe(expected);
  });

  it('keeps the experiment-off UI English and preserves the saved preference', async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, 'zh-CN');
    vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['zh-CN']);
    await initializeLocalization(false);
    setLocalePreference('zh-CN');
    expect(isLocalizationEnabled()).toBe(false);
    expect(t('page.dashboard')).toBe('Dashboard');
    expect(document.documentElement.lang).toBe('en');
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe('zh-CN');
  });

  it('uses browser order in Auto, while an explicit preference survives reinitialization', async () => {
    vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['fr', 'zh-CN', 'en']);
    await initializeLocalization(true);
    expect(getLocale()).toBe('zh-CN');
    expect(getLocalePreference()).toBe('');
    expect(document.documentElement.lang).toBe('zh-CN');
    expect(document.documentElement.dir).toBe('ltr');
    setLocalePreference('en');
    await initializeLocalization(true);
    expect(getLocale()).toBe('en');
    expect(getLocalePreference()).toBe('en');
    setLocalePreference('');
    expect(getLocale()).toBe('zh-CN');
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBeNull();
  });

  it('falls back to English for an unsupported persisted preference', async () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, 'unsupported');
    vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['zh-CN']);
    await initializeLocalization(true);
    expect(getLocale()).toBe('en');
    expect(getLocalePreference()).toBe('en');
    setLocalePreference('not-registered');
    expect(getLocale()).toBe('en');
  });

  it('remains usable when storage access fails', async () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('full');
    });
    await initializeLocalization(true);
    expect(() => setLocalePreference('zh-CN')).not.toThrow();
    expect(getLocale()).toBe('zh-CN');
  });

  it('follows other tabs and browser changes, then removes its listeners on dispose', async () => {
    const stop = await initializeLocalization(true);
    localStorage.setItem(LOCALE_STORAGE_KEY, 'zh-CN');
    window.dispatchEvent(new StorageEvent('storage', { key: 'unrelated' }));
    expect(getLocale()).toBe('en');
    window.dispatchEvent(new StorageEvent('storage', { key: LOCALE_STORAGE_KEY }));
    expect(getLocale()).toBe('zh-CN');
    localStorage.clear();
    window.dispatchEvent(new StorageEvent('storage', { key: null }));
    expect(getLocale()).toBe('en');
    vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['zh-CN']);
    window.dispatchEvent(new Event('languagechange'));
    expect(getLocale()).toBe('zh-CN');
    setLocalePreference('en');
    window.dispatchEvent(new Event('languagechange'));
    expect(getLocale()).toBe('en');
    stop();
    localStorage.setItem(LOCALE_STORAGE_KEY, 'zh-CN');
    window.dispatchEvent(new StorageEvent('storage', { key: LOCALE_STORAGE_KEY }));
    expect(getLocale()).toBe('en');
  });

  it('ignores storage events when the localStorage getter is blocked', async () => {
    const storage = localStorage;
    const getter = vi.spyOn(globalThis, 'localStorage', 'get').mockImplementation(() => {
      throw new DOMException('Storage blocked', 'SecurityError');
    });
    const onError = vi.fn((event: ErrorEvent) => event.preventDefault());
    window.addEventListener('error', onError);
    try {
      await initializeLocalization(true);
      setLocalePreference('zh-CN');
      expect(getLocale()).toBe('zh-CN');
      getter.mockClear();
      window.dispatchEvent(
        new StorageEvent('storage', { key: LOCALE_STORAGE_KEY, storageArea: storage })
      );
      expect(getter).toHaveBeenCalledOnce();
      expect(onError).not.toHaveBeenCalled();
      expect(getLocale()).toBe('zh-CN');
    } finally {
      window.removeEventListener('error', onError);
    }
  });
});

describe('message contracts', () => {
  it('uses the active language plural rules and preserves interpolation as safe text', async () => {
    await initializeLocalization(true);
    expect(t('header.unread', { count: 1 })).toBe('1 unread conversation');
    expect(t('header.unread', { count: 2 })).toBe('2 unread conversations');
    setLocalePreference('zh-CN');
    expect(t('header.unread', { count: 1 })).toBe('1 个未读会话');
    expect(t('header.unread', { count: 2 })).toBe('2 个未读会话');
    const container = document.createElement('div');
    render(
      html`${t('header.jumpToAgent', { shortcut: '<img src=x onerror=alert(1)>' })}`,
      container
    );
    expect(container.querySelector('img')).toBeNull();
    expect(container.textContent).toContain('<img src=x onerror=alert(1)>');
  });

  it('falls back to English for a missing or empty translation', async () => {
    const catalog = await locales.find((locale) => locale.code === 'zh-CN')!.loadMessages();
    const saved = catalog['header.help'];
    try {
      await initializeLocalization(true);
      setLocalePreference('zh-CN');
      delete catalog['header.help'];
      expect(t('header.help')).toBe('Help');
      catalog['header.help'] = '';
      expect(t('header.help')).toBe('Help');
    } finally {
      if (saved !== undefined) catalog['header.help'] = saved;
      else delete catalog['header.help'];
    }
  });
});
