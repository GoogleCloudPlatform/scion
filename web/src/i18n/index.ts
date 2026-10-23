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
import type { MessageKey } from './locales/en.js';
import type { LocalizationRuntime } from './runtime.js';
import { LOCALE_CHANGED_EVENT } from './events.js';

export type { MessageKey } from './locales/en.js';
export { locales } from './locales.js';
export { LOCALE_CHANGED_EVENT, LOCALE_STORAGE_KEY } from './events.js';

let runtime: LocalizationRuntime | undefined;
let generation = 0;

export function isLocalizationEnabled(): boolean {
  return runtime?.enabled ?? false;
}

export function getLocale(): string {
  return runtime?.getLocale() ?? 'en';
}

export function getLocalePreference(): string {
  return runtime?.getPreference() ?? '';
}

/** Source-only rendering before opt-in; target-language rules live in i18next. */
export function t(key: MessageKey, values: Record<string, string | number> = {}): string {
  if (runtime) return runtime.translate(key, values);
  const pluralKey = `${key}_${values.count === 1 ? 'one' : 'other'}`;
  const source = en[key as keyof typeof en] ?? en[pluralKey as keyof typeof en];
  if (source === undefined) return key;
  return source.replace(/\{\{(\w+)\}\}/g, (_match, name: string) => String(values[name] ?? ''));
}

function disable(): void {
  if (runtime) runtime.disable();
  else {
    document.documentElement.lang = 'en';
    document.documentElement.dir = 'ltr';
    window.dispatchEvent(new Event(LOCALE_CHANGED_EVENT));
  }
}

/** Resolve the experiment before calling; await opt-in resources before rendering. */
export async function initializeLocalization(enabled: boolean): Promise<() => void> {
  runtime?.dispose();
  const current = ++generation;
  if (!enabled) {
    disable();
    return () => {};
  }
  try {
    const [{ createRuntime }] = await Promise.all([
      import('./runtime.js'),
      import('../components/shared/language-select.js'),
    ]);
    const loaded = await createRuntime();
    if (current !== generation) return () => {};
    runtime = loaded;
    runtime.initialize();
    return runtime.dispose;
  } catch (error) {
    if (current === generation) {
      console.warn('[i18n] Unable to load localization; using English.', error);
      disable();
    }
    return () => {};
  }
}

export function setLocalePreference(value: string): void {
  runtime?.setPreference(value);
}
