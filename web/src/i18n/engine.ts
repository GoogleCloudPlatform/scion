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
import type { MessageKey } from './locales/en.js';

export interface MessageEngine {
  translate: (key: MessageKey, values: Record<string, string | number>) => string;
  setLocale: (locale: string) => void;
}

/** Single-flight load, reachable only after opting into the registered experiment. */
let loading: Promise<MessageEngine> | undefined;

export function loadEngine(): Promise<MessageEngine> {
  if (loading) return loading;
  loading = (async () => {
    const resources = await Promise.all(
      locales.map(
        async (locale) => [locale.code, { translation: await locale.loadMessages() }] as const
      )
    );
    const messages = createInstance();
    await messages.init({
      initAsync: false,
      showSupportNotice: false,
      lng: DEFAULT_LOCALE,
      fallbackLng: DEFAULT_LOCALE,
      resources: Object.fromEntries(resources),
      keySeparator: false,
      nsSeparator: false,
      returnNull: false,
      returnEmptyString: false,
      interpolation: { escapeValue: false }, // Lit escapes text/attribute bindings.
    });
    return {
      translate: (key, values) => messages.t(key, values),
      // All catalogs are loaded before the engine is published: synchronous.
      setLocale: (locale) => {
        void messages.changeLanguage(locale);
      },
    };
  })();
  void loading.catch(() => {
    loading = undefined;
  });
  return loading;
}
