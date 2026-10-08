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
import { describeSourceUrl, isTemplateSourceRefreshable } from './source-url.js';

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

describe('describeSourceUrl', () => {
  it('links http(s) sources without credentials or the git+ prefix', () => {
    expect(describeSourceUrl('git+https://user:secret@github.com/acme/repo')).toEqual({
      text: 'https://github.com/acme/repo',
      href: 'https://github.com/acme/repo',
    });
  });

  it('shows built-in sources as text', () => {
    expect(describeSourceUrl('builtin://scion/1.0/template/default')).toEqual({
      text: 'builtin://scion/1.0/template/default',
      href: null,
    });
  });

  it('returns null for an empty source', () => {
    expect(describeSourceUrl('')).toBeNull();
    expect(describeSourceUrl(undefined)).toBeNull();
  });

  // The display never shows credentials embedded in a source string.
  it.each([
    ':s3,access_key_id=AKIA,secret_access_key=secret:bucket/path',
    ':gcs,service_account_credentials=secret:bucket',
    's3://key:secret@bucket/path',
    'builtin://user:secret@scion/1.0/template/default',
    'mailto:secret@example.com',
    'javascript:alert("secret")',
    'not a url secret',
  ])('never shows credentials embedded in %s', (raw) => {
    const shown = describeSourceUrl(raw);
    expect(shown).not.toBeNull();
    expect(shown!.text).not.toContain('secret');
    expect(shown!.href ?? '').not.toContain('secret');
    if (shown!.href) expect(shown!.href).toMatch(/^https?:/);
  });
});
