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
 * Lists, list pages and detail pages migrated to `utils/time.ts`
 * (tz-refactor task 19) render absolute times in the display zone, on a
 * 24-hour clock, with a zone label.
 *
 * Vitest pins the browser zone to UTC, so every case sets a preference that
 * differs from it (Asia/Tokyo) and uses an instant that is midnight there:
 * 15:00Z is 00:00 the next day in Tokyo.
 */

// @vitest-environment happy-dom

import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { render } from 'lit';
import { setPreferredTimeZone } from '../../utils/time.js';

await import('./token-list.js');
await import('./project-template-list.js');
await import('./pre-start-hook-list.js');
await import('../pages/broker-detail.js');
await import('../pages/project-detail.js');
await import('../pages/project-settings.js');
await import('../pages/home.js');

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

const MIDNIGHT_TOKYO = '2026-10-01T15:00:00Z';

function element(tag: string): AnyEl {
  return document.createElement(tag);
}

function renderText(result: unknown): string {
  const host = document.createElement('div');
  render(result, host);
  return (host.textContent ?? '').replace(/\s+/g, ' ').trim();
}

describe('list and detail times in the display zone (tz-refactor task 19)', () => {
  beforeEach(() => {
    // Rendered rows contain Shoelace icons, which fetch their SVGs.
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('', { status: 404 })))
    );
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    setPreferredTimeZone('');
    document.body.innerHTML = '';
  });

  it('detail pages show created/updated as a labelled 24-hour time', () => {
    setPreferredTimeZone('Asia/Tokyo');
    for (const tag of ['scion-page-broker-detail', 'scion-page-project-detail']) {
      expect(element(tag).formatDate(MIDNIGHT_TOKYO), tag).toBe('Oct 2, 2026, 00:00 (Asia/Tokyo)');
    }
  });

  it('follows the browser zone when the preference is Auto', () => {
    expect(element('scion-page-broker-detail').formatDate(MIDNIGHT_TOKYO)).toBe(
      'Oct 1, 2026, 15:00 (UTC)'
    );
  });

  it('token list shows the expiry instant in the preferred zone, labelled', () => {
    setPreferredTimeZone('Asia/Tokyo');
    const text = renderText(
      element('scion-token-list').renderRow({
        id: 't1',
        name: 'ci-token',
        prefix: 'scion_abc',
        projectId: 'p1',
        scopes: ['agent:read'],
        revoked: false,
        expiresAt: MIDNIGHT_TOKYO,
        lastUsed: null,
        created: '2026-09-01T00:00:00Z',
      })
    );
    expect(text).toContain('Oct 2, 2026, 00:00 (Asia/Tokyo)');
  });

  it('project template list shows the created date in the preferred zone, labelled', () => {
    setPreferredTimeZone('Asia/Tokyo');
    const text = renderText(
      element('scion-project-template-list').renderTemplate({
        id: 'tpl1',
        name: 'Base',
        slug: 'base',
        created: MIDNIGHT_TOKYO,
      })
    );
    expect(text).toContain('Oct 2, 2026 (Asia/Tokyo)');
  });

  it('project settings shows the last token mint in the preferred zone, labelled', () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = element('scion-page-project-settings');
    el.project = { id: 'p1', name: 'p', gitRemote: 'https://github.com/acme/repo' };
    el.githubAppConfigured = true;
    el.githubAppInstallationId = 42;
    el.githubAppStatus = {
      state: 'ok',
      last_token_mint: MIDNIGHT_TOKYO,
      last_checked: MIDNIGHT_TOKYO,
    };
    expect(renderText(el.renderGitHubAppSection())).toContain(
      'Last Token Mint Oct 2, 2026, 00:00 (Asia/Tokyo)'
    );
  });

  it('home shows items older than 30 days as a labelled date in the preferred zone', () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    try {
      vi.setSystemTime(new Date('2026-11-15T00:00:00Z'));
      setPreferredTimeZone('Asia/Tokyo');
      const el = element('scion-page-home');
      expect(el.formatRelativeTime(MIDNIGHT_TOKYO)).toBe('Oct 2, 2026 (Asia/Tokyo)');
      // Within 30 days: compact relative time; future (clock skew): "just now".
      expect(el.formatRelativeTime('2026-11-14T21:00:00Z')).toBe('3h ago');
      expect(el.formatRelativeTime('2026-11-12T00:00:00Z')).toBe('3d ago');
      expect(el.formatRelativeTime('2026-11-15T00:05:00Z')).toBe('just now');
      expect(el.formatRelativeTime('')).toBe('');
    } finally {
      vi.useRealTimers();
    }
  });

  it('pre-start hook list shows the created date in the preferred zone, labelled', () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = element('scion-pre-start-hook-list');
    expect(el.formatDate(MIDNIGHT_TOKYO)).toBe('Oct 2, 2026 (Asia/Tokyo)');
    expect(el.formatDate(undefined)).toBe('—');
  });
});
