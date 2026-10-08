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

import { describe, it, expect } from 'vitest';
import { displaySourceUrl, isTemplateSourceRefreshable } from './source-url.js';

describe('isTemplateSourceRefreshable', () => {
  it.each([
    ['https://github.com/acme/repo/tree/main/.scion/templates/t', true],
    ['https://GitHub.com/acme/repo', true],
    ['', false],
    [undefined, false],
    ['builtin://scion/1.0/template/default', false],
    ['http://github.com/acme/repo', false],
    ['https://example.com/t.tgz', false],
    ['https://other.github.com/acme/repo', false],
    ['https://user:secret@github.com/acme/repo', false],
    [':gcs:bucket/path', false],
    ['not a url', false],
  ])('%s -> %s', (url, want) => {
    expect(isTemplateSourceRefreshable(url)).toBe(want);
  });
});

describe('displaySourceUrl', () => {
  it('drops credentials and the git+ prefix', () => {
    expect(displaySourceUrl('git+https://user:secret@github.com/acme/repo')).toBe(
      'https://github.com/acme/repo'
    );
  });
  it('returns null for non-http sources', () => {
    expect(displaySourceUrl('builtin://scion/1.0/template/default')).toBeNull();
    expect(displaySourceUrl('javascript:alert(1)')).toBeNull();
    expect(displaySourceUrl('')).toBeNull();
  });
});
