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

import { loadEngine } from './engine.js';
import { DEFAULT_LOCALE, locales, resolveLocale } from './locales.js';
import { LOCALE_CHANGED_EVENT, LOCALE_STORAGE_KEY } from './events.js';
import type { MessageKey } from './locales/en.js';
import '../components/shared/language-select.js';

export interface LocalizationRuntime {
  readonly enabled: boolean;
  getLocale: () => string;
  getPreference: () => string;
  translate: (key: MessageKey, values: Record<string, string | number>) => string;
  initialize: () => Promise<void>;
  disable: () => void;
  dispose: () => void;
  setPreference: (value: string) => Promise<void>;
}

function readPreference(): string {
  try {
    const stored = localStorage.getItem(LOCALE_STORAGE_KEY);
    return stored ? resolveLocale([stored]).code : '';
  } catch {
    return '';
  }
}

/** No document/storage effects until initialize(), after the caller's generation check. */
export async function createRuntime(): Promise<LocalizationRuntime> {
  const engine = await loadEngine();
  let enabled = false;
  let preference = '';
  let activeLocale = DEFAULT_LOCALE;
  let generation = 0;

  const publishLocale = (code: string): void => {
    const locale = resolveLocale([code]);
    activeLocale = locale.code;
    engine.setLocale(locale.code);
    document.documentElement.lang = locale.code;
    document.documentElement.dir = locale.dir;
    window.dispatchEvent(new Event(LOCALE_CHANGED_EVENT));
  };
  const applyLocale = async (value: string, persist = false): Promise<void> => {
    const current = ++generation;
    const locale = resolveLocale(value ? [value] : navigator.languages);
    try {
      await engine.loadLocale(locale.code);
    } catch (error) {
      if (!enabled || current !== generation) return;
      throw error;
    }
    if (!enabled || current !== generation) return;
    preference = value;
    if (persist) {
      try {
        if (value) localStorage.setItem(LOCALE_STORAGE_KEY, value);
        else localStorage.removeItem(LOCALE_STORAGE_KEY);
      } catch {
        // Browser storage failures must not prevent using the current tab.
      }
    }
    publishLocale(locale.code);
  };
  const changeLocale = async (value: string, persist = false): Promise<void> => {
    try {
      await applyLocale(value, persist);
    } catch (error) {
      if (!enabled) return;
      console.warn('[i18n] Unable to switch language; keeping the current language.', error);
      window.dispatchEvent(new Event(LOCALE_CHANGED_EVENT));
    }
  };
  const handleStorage = (event: StorageEvent): void => {
    if (event.key !== LOCALE_STORAGE_KEY && event.key !== null) return;
    try {
      if (event.storageArea && event.storageArea !== localStorage) return;
    } catch {
      // Storage access may be revoked after initialization; keep the tab's choice.
      return;
    }
    void changeLocale(readPreference());
  };
  const handleLanguage = (): void => {
    if (!preference) void changeLocale('');
  };
  const dispose = (): void => {
    enabled = false;
    generation++;
    window.removeEventListener('storage', handleStorage);
    window.removeEventListener('languagechange', handleLanguage);
  };

  return {
    get enabled() {
      return enabled;
    },
    getLocale: () => activeLocale,
    getPreference: () => preference,
    translate: engine.translate,
    initialize: async () => {
      enabled = true;
      window.addEventListener('storage', handleStorage);
      window.addEventListener('languagechange', handleLanguage);
      await applyLocale(readPreference());
    },
    disable: () => {
      dispose();
      preference = '';
      publishLocale(DEFAULT_LOCALE);
    },
    dispose,
    setPreference: async (value) => {
      if (!enabled) return;
      if (value && !locales.some((locale) => locale.code === value)) return;
      await changeLocale(value, true);
    },
  };
}
