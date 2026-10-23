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

import { en } from './locales/en.js';
import type { MessageEngine } from './engine.js';
import { DEFAULT_LOCALE, locales, resolveLocale } from './locales.js';
import type { MessageKey } from './locales/en.js';

export type { MessageKey } from './locales/en.js';
export { locales } from './locales.js';
export const LOCALE_CHANGED_EVENT = 'scion-locale-changed';
export const LOCALE_STORAGE_KEY = 'scion-language';

// The English source renderer keeps the default-off shell independent of the
// translation engine. Source messages use only named placeholders/cardinal plurals.
let engine: MessageEngine | undefined;
let activeLocale = DEFAULT_LOCALE;
let generation = 0;

let enabled = false;
let preference = '';
let dispose: (() => void) | undefined;

export function isLocalizationEnabled(): boolean {
  return enabled;
}

export function getLocale(): string {
  return activeLocale;
}

/** Empty means follow the browser; an explicit unsupported preference falls back to English. */
export function getLocalePreference(): string {
  return preference;
}

export function t(key: MessageKey, values: Record<string, string | number> = {}): string {
  if (engine) return engine.translate(key, values);
  const pluralKey = `${key}_${values.count === 1 ? 'one' : 'other'}`;
  const source = en[key as keyof typeof en] ?? en[pluralKey as keyof typeof en];
  return source.replace(/\{\{(\w+)\}\}/g, (_match, name: string) => String(values[name] ?? ''));
}

function readPreference(): string {
  try {
    const stored = localStorage.getItem(LOCALE_STORAGE_KEY);
    return stored ? resolveLocale([stored]).code : '';
  } catch {
    return '';
  }
}

function applyLocale(): void {
  const locale = resolveLocale(
    enabled ? (preference ? [preference] : navigator.languages) : [DEFAULT_LOCALE]
  );
  activeLocale = locale.code;
  engine?.setLocale(locale.code);
  document.documentElement.lang = locale.code;
  document.documentElement.dir = locale.dir;
  window.dispatchEvent(new Event(LOCALE_CHANGED_EVENT));
}

/** Called after experiment resolution and before the first app render. */
export async function initializeLocalization(allowLocalization: boolean): Promise<() => void> {
  dispose?.();
  const current = ++generation;
  enabled = allowLocalization;
  // No engine or target catalogs enter the default-off entry closure.
  if (enabled && !engine) {
    try {
      const { loadEngine } = await import('./engine.js');
      const loaded = await loadEngine();
      if (current !== generation) return () => {};
      engine = loaded;
    } catch (error) {
      if (current !== generation) return () => {};
      console.warn('[i18n] Unable to load localization; using English.', error);
      enabled = false;
    }
  }
  preference = enabled ? readPreference() : '';
  applyLocale();

  const handleStorage = (event: StorageEvent): void => {
    if (event.key !== LOCALE_STORAGE_KEY && event.key !== null) return;
    if (event.storageArea && event.storageArea !== localStorage) return;
    preference = readPreference();
    applyLocale();
  };
  const handleLanguage = (): void => {
    if (!preference) applyLocale();
  };
  if (enabled) {
    window.addEventListener('storage', handleStorage);
    window.addEventListener('languagechange', handleLanguage);
  }
  dispose = (): void => {
    window.removeEventListener('storage', handleStorage);
    window.removeEventListener('languagechange', handleLanguage);
  };
  return dispose;
}

export function setLocalePreference(value: string): void {
  if (!enabled) return;
  // UI options are registry codes (or Auto); reject unregistered values.
  if (value && !locales.some((locale) => locale.code === value)) return;
  preference = value;
  try {
    if (value) localStorage.setItem(LOCALE_STORAGE_KEY, value);
    else localStorage.removeItem(LOCALE_STORAGE_KEY);
  } catch {
    // Still usable for this tab when browser storage is disabled or full.
  }
  applyLocale();
}
