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

import { describe, expect, it } from 'vitest';
import { locales, DEFAULT_LOCALE } from './locales.js';
import { en, type MessageKey } from './locales/en.js';

const pluralSuffix = /_(zero|one|two|few|many|other)$/;
const base = (key: string): string => key.replace(pluralSuffix, '');
const parameters = (text: string): string[] =>
  [...text.matchAll(/\{\{(\w+)\}\}/g)].map((match) => match[1]).sort();

describe('shipped locale catalogs', () => {
  it('has unique canonical codes, aliases and a registered English fallback', () => {
    const names = locales.flatMap((locale) => [locale.code, ...locale.aliases]);
    expect(new Set(names.map((name) => name.toLowerCase())).size).toBe(names.length);
    expect(locales.some((locale) => locale.code === DEFAULT_LOCALE)).toBe(true);
    for (const locale of locales) {
      expect(new Intl.Locale(locale.code).baseName).toBe(locale.code);
      expect(locale.name.trim()).not.toBe('');
    }
  });

  for (const locale of locales) {
    it(`${locale.code} covers the migrated messages and preserves their placeholders`, async () => {
      const catalog = await locale.loadMessages();
      const source = Object.entries(en);
      const sourceBases = new Set(source.map(([key]) => base(key)));
      const target = Object.entries(catalog);
      for (const [key, value] of target) {
        expect(sourceBases.has(base(key)), `unknown key ${key}`).toBe(true);
        expect(value.trim(), `empty message ${key}`).not.toBe('');
        const sourceText = source.find(([sourceKey]) => base(sourceKey) === base(key))![1];
        expect(parameters(value), `parameters in ${key}`).toEqual(parameters(sourceText));
      }
      for (const key of sourceBases) {
        const isPlural = source.some(
          ([sourceKey]) => base(sourceKey) === key && pluralSuffix.test(sourceKey)
        );
        if (isPlural) {
          for (const category of new Intl.PluralRules(locale.code).resolvedOptions()
            .pluralCategories) {
            expect(
              catalog[`${key}_${category}` as keyof typeof catalog],
              `${key}_${category}`
            ).toBeTruthy();
          }
        } else {
          expect(catalog[key as MessageKey], key).toBeTruthy();
        }
      }
    });
  }
});
