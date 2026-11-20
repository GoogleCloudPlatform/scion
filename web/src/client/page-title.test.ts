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

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import {
  DEFAULT_APP_NAME,
  getAppName,
  setAppName,
  setDocumentTitle,
  setUnreadBadge,
} from './page-title.js';
import { readInitialPageData } from './ssr-page-data.js';

describe('page-title branding support', () => {
  beforeEach(() => {
    setAppName(undefined);
    setUnreadBadge(0);
    setDocumentTitle();
  });

  afterEach(() => {
    setAppName(undefined);
    setUnreadBadge(0);
    setDocumentTitle();
    document.getElementById('__SCION_DATA__')?.remove();
  });

  it('defaults to Scion and formats hierarchical segments', () => {
    expect(getAppName()).toBe(DEFAULT_APP_NAME);
    expect(document.title).toBe('Scion');

    setDocumentTitle('my-project', 'Projects');
    expect(document.title).toBe('my-project — Projects — Scion');
  });

  it('updates document.title live when setAppName changes and preserves unread badge', () => {
    setDocumentTitle('Login');
    expect(document.title).toBe('Login — Scion');

    setAppName('Acme Cloud');
    expect(getAppName()).toBe('Acme Cloud');
    expect(document.title).toBe('Login — Acme Cloud');

    setUnreadBadge(4);
    expect(document.title).toBe('(4) Login — Acme Cloud');

    // Resetting branding restores stock Scion while preserving segments & unread count.
    setAppName(undefined);
    expect(getAppName()).toBe('Scion');
    expect(document.title).toBe('(4) Login — Scion');
  });

  it('hydrates branding from anonymous __SCION_DATA__ payload', () => {
    const script = document.createElement('script');
    script.id = '__SCION_DATA__';
    script.type = 'application/json';
    script.textContent = JSON.stringify({
      path: '/login',
      title: 'Acme Portal',
      branding: {
        name: 'Acme Portal',
        shortName: 'Acme',
        attribution: 'shown',
        themeColor: '#1e293b',
      },
    });
    document.body.appendChild(script);

    const data = readInitialPageData(document);
    expect(data?.branding?.name).toBe('Acme Portal');
    setAppName(data?.branding?.name);
    setDocumentTitle('Login');
    expect(document.title).toBe('Login — Acme Portal');
  });
});
