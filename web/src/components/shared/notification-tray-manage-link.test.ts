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
 * The tray footer's Manage subscriptions link leads to the notification
 * settings in the user's profile (ptone/scion#4500).
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { render } from 'lit';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('[]', { status: 200 }))),
}));

describe('notification tray: Manage subscriptions link', () => {
  beforeAll(async () => {
    await import('./notification-tray.js');
  });

  afterEach(() => {
    vi.restoreAllMocks();
    // The removeEventListener spy below must not outlive its test.
    expect(vi.isMockFunction(document.removeEventListener)).toBe(false);
  });

  function renderFooterLink(tray: any): HTMLAnchorElement {
    const host = document.createElement('div');
    render(tray.renderPanel(), host);
    const link = host.querySelector<HTMLAnchorElement>('a.manage-link');
    expect(link).not.toBeNull();
    return link as HTMLAnchorElement;
  }

  it('points at the profile notification settings', () => {
    const tray: any = document.createElement('scion-notification-tray');
    const link = renderFooterLink(tray);
    expect(link.textContent?.trim()).toBe('Manage subscriptions');
    expect(link.getAttribute('href')).toBe('/profile/settings');
  });

  it('navigates to the settings page and closes the tray on click', () => {
    const tray: any = document.createElement('scion-notification-tray');
    tray.open = true;
    const link = renderFooterLink(tray);

    const paths: string[] = [];
    const onNav = (e: Event): void => {
      paths.push((e as CustomEvent<{ path: string }>).detail.path);
    };
    document.addEventListener('nav-click', onNav);
    const removeListener = vi.spyOn(document, 'removeEventListener');
    try {
      const click = new MouseEvent('click', { bubbles: true, cancelable: true });
      link.dispatchEvent(click);
      expect(click.defaultPrevented).toBe(true);
    } finally {
      document.removeEventListener('nav-click', onNav);
    }

    expect(paths).toEqual(['/profile/settings']);
    expect(tray.open).toBe(false);
    expect(removeListener).toHaveBeenCalledWith('click', tray.boundOnClickOutside, true);
  });
});
