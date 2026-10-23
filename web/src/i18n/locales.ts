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

import { en, type MessageCatalog } from './locales/en.js';

export interface LocaleDefinition {
  code: string;
  name: string;
  dir: 'ltr' | 'rtl';
  /** Explicit matches only: do not silently map Traditional to Simplified Chinese. */
  aliases: readonly string[];
  loadMessages: () => Promise<MessageCatalog>;
}

/** The only registration point for a new language. English is the source/fallback. */
export const locales: readonly LocaleDefinition[] = [
  { code: 'en', name: 'English', dir: 'ltr', aliases: [], loadMessages: () => Promise.resolve(en) },
  {
    code: 'zh-CN',
    name: '简体中文',
    dir: 'ltr',
    aliases: ['zh', 'zh-Hans', 'zh-SG'],
    loadMessages: async () => {
      const [{ zhCN }] = await Promise.all([
        import('./locales/zh-CN.js'),
        import('@shoelace-style/shoelace/dist/translations/zh-cn.js'),
      ]);
      return zhCN;
    },
  },
];

export const DEFAULT_LOCALE = 'en';

/** Resolve a preference list in order, ignoring malformed/unsupported tags. */
export function resolveLocale(
  preferences: readonly string[],
  registry: readonly LocaleDefinition[] = locales
): LocaleDefinition {
  for (const preference of preferences) {
    try {
      const tag = new Intl.Locale(preference);
      const candidates = [tag.baseName, `${tag.language}-${tag.script}`, tag.language];
      // A bare language alias must not override an explicitly different script/region.
      for (const candidate of candidates) {
        const found = registry.find(
          (locale) =>
            locale.code.toLowerCase() === candidate.toLowerCase() ||
            locale.aliases.some((alias) => alias.toLowerCase() === candidate.toLowerCase())
        );
        if (
          found &&
          (candidate !== tag.language ||
            (!tag.region && !tag.script) ||
            found.code === tag.language)
        ) {
          return found;
        }
      }
    } catch {
      // Persisted values and browser language lists are untrusted input.
    }
  }
  return registry.find((locale) => locale.code === DEFAULT_LOCALE)!;
}
