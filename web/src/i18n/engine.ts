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

import { createInstance } from 'i18next';
import { DEFAULT_LOCALE, locales } from './locales.js';
import { en, type MessageKey } from './locales/en.js';

export interface MessageEngine {
  translate: (key: MessageKey, values: Record<string, string | number>) => string;
  setLocale: (locale: string) => void;
  loadLocale: (locale: string) => Promise<void>;
}

/** Single-flight load, reachable only after opting into the registered experiment. */
let loading: Promise<MessageEngine> | undefined;

export function loadEngine(): Promise<MessageEngine> {
  if (loading) return loading;
  loading = (async () => {
    const messages = createInstance();
    await messages.init({
      initAsync: false,
      showSupportNotice: false,
      lng: DEFAULT_LOCALE,
      fallbackLng: DEFAULT_LOCALE,
      resources: { [DEFAULT_LOCALE]: { translation: en } },
      keySeparator: false,
      nsSeparator: false,
      returnNull: false,
      returnEmptyString: false,
      interpolation: { escapeValue: false }, // Lit escapes text/attribute bindings.
    });
    const catalogs = new Map<string, Promise<void>>();
    return {
      translate: (key, values) => messages.t(key, values),
      // Call only after loadLocale succeeds; resources are already in memory.
      setLocale: (locale) => {
        void messages.changeLanguage(locale);
      },
      loadLocale: (code) => {
        if (code === DEFAULT_LOCALE) return Promise.resolve();
        const existing = catalogs.get(code);
        if (existing) return existing;
        const locale = locales.find((locale) => locale.code === code);
        if (!locale) return Promise.reject(new Error(`Unknown locale: ${code}`));
        const catalog = locale.loadMessages().then((translation) => {
          messages.addResourceBundle(code, 'translation', translation);
        });
        catalogs.set(code, catalog);
        void catalog.catch(() => catalogs.delete(code));
        return catalog;
      },
    };
  })();
  void loading.catch(() => {
    loading = undefined;
  });
  return loading;
}
