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
import { isHttpUrl } from './http-url.js';

describe('isHttpUrl', () => {
  it.each([
    'https://console.cloud.google.com/monitoring/dashboards/builder/x?project=p',
    'http://grafana.internal:3000/d/hub#panel',
    'HTTPS://dash.example.com',
  ])('accepts %s', (v) => {
    expect(isHttpUrl(v)).toBe(true);
  });

  it.each([
    '',
    'javascript:alert(1)',
    'JavaScript:alert(1)',
    'data:text/html,hi',
    'ftp://dash.example.com',
    '/relative',
    '//dash.example.com/x',
    'http:dash.example.com',
    'http:/dash.example.com',
    'https://user:pw@dash.example.com/',
    ' https://dash.example.com',
    'https://dash.example.com/a b',
  ])('rejects %j', (v) => {
    expect(isHttpUrl(v)).toBe(false);
  });

  it('rejects non-strings', () => {
    expect(isHttpUrl(undefined)).toBe(false);
    expect(isHttpUrl(null)).toBe(false);
  });
});
