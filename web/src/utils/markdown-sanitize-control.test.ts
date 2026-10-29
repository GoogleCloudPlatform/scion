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

/**
 * Control for markdown-sanitize.test.ts: the same path WITHOUT the nodeName
 * fixture. In the test DOM, DOMPurify then unwraps the first element it
 * meets and stops walking, so disallowed markup is left in place. This shows
 * the clean results in markdown-sanitize.test.ts depend on the fixture
 * making the sanitizer run, and are not an accident of the test DOM.
 *
 * If this starts failing after a test DOM upgrade, the upgrade fixed the
 * getter: check the fixture is still needed and drop both if not.
 */

import { describe, it, expect, vi } from 'vitest';
import { disallowedLeft } from './__fixtures__/disallowed-markup.js';

vi.mock('marked', () => ({
  marked: {
    use: (): void => undefined,
    parse: (source: string): string => source,
  },
}));

import { getMarkdownRenderer } from './markdown.js';

describe('renderFragment without the nodeName fixture', () => {
  it('leaves disallowed markup in place, so the fixture is what makes the sanitizer run', async () => {
    const renderer = await getMarkdownRenderer();
    const fragment = renderer.renderFragment(
      '<p>first</p><img src="/x.png" onerror="alert(1)"><script>alert(1)</script>'
    );
    expect(disallowedLeft(fragment)).toEqual(expect.arrayContaining(['img[onerror]', '<script>']));
  });
});
