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

import { afterEach, describe, expect, it } from 'vitest';
import { render } from 'lit';
import { applyBranding } from '../../client/branding.js';
import { syncBrandingDOM } from '../../client/branding-runtime.js';
import './nav.js';
import './header.js';
import '../pages/unauthorized.js';
import { renderUntrustedViewFrame } from './untrusted-view-frame.js';
import type { ScionNav } from './nav.js';
import type { ScionHeader } from './header.js';
import type { ScionPageUnauthorized } from '../pages/unauthorized.js';

describe('SPA branding surfaces (logo, collapsed rail, attribution, copy)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    applyBranding(null);
    syncBrandingDOM(null);
  });

  it('renders stock Scion mark without attribution on <scion-nav> when unbranded', async () => {
    const nav = document.createElement('scion-nav') as ScionNav;
    document.body.appendChild(nav);
    await nav.updateComplete;

    expect(nav.shadowRoot?.querySelector('.logo-text h1')?.textContent).toBe('Scion');
    expect(nav.shadowRoot?.querySelector('.logo-text span')?.textContent).toBe(
      'Agent Orchestration'
    );
    expect(nav.shadowRoot?.querySelector('.sidebar-attribution')).toBeNull();
  });

  it('renders light/dark logos when expanded, compact monogram/favicon when collapsed, and attribution when shown', async () => {
    applyBranding({
      name: 'Acme Cloud',
      shortName: 'Acme',
      subtitle: 'Engineering Platform',
      attribution: 'shown',
      logoLight: 'https://cdn.example.com/acme-light.svg',
      logoDark: 'https://cdn.example.com/acme-dark.svg',
      themeColor: '#0c0a09',
    });

    const nav = document.createElement('scion-nav') as ScionNav;
    document.body.appendChild(nav);
    await nav.updateComplete;

    const lightImg = nav.shadowRoot?.querySelector<HTMLImageElement>('img.brand-logo-light');
    const darkImg = nav.shadowRoot?.querySelector<HTMLImageElement>('img.brand-logo-dark');
    expect(lightImg?.getAttribute('src')).toBe('https://cdn.example.com/acme-light.svg');
    expect(darkImg?.getAttribute('src')).toBe('https://cdn.example.com/acme-dark.svg');
    expect(nav.shadowRoot?.querySelector('.brand-subtitle')?.textContent).toBe(
      'Engineering Platform'
    );
    expect(nav.shadowRoot?.querySelector('.sidebar-attribution')?.textContent).toBe(
      'Powered by Scion'
    );

    // Collapsed 64px rail renders monogram instead of wide wordmark.
    nav.collapsed = true;
    await nav.updateComplete;
    expect(nav.shadowRoot?.querySelector('img.brand-logo')).toBeNull();
    expect(nav.shadowRoot?.querySelector('.brand-monogram')?.textContent).toBe('A');

    // Hiding attribution removes the footer live without reload.
    applyBranding({
      name: 'Acme Cloud',
      shortName: 'Acme',
      attribution: 'hidden',
      faviconUrl: '/brand/favicon?v=12345678',
      themeColor: '#0c0a09',
    });
    await nav.updateComplete;
    expect(nav.shadowRoot?.querySelector('.sidebar-attribution')).toBeNull();
    expect(
      nav.shadowRoot?.querySelector<HTMLImageElement>('.brand-favicon')?.getAttribute('src')
    ).toBe('/brand/favicon?v=12345678');
  });

  it('renders custom logo + Chat badge in <scion-header> chat view with no attribution', async () => {
    applyBranding({
      name: 'Acme Cloud',
      shortName: 'Acme',
      attribution: 'shown',
      logoLight: 'https://cdn.example.com/acme-light.svg',
      themeColor: '#0c0a09',
    });

    const header = document.createElement('scion-header') as ScionHeader;
    header.currentPath = '/chat';
    document.body.appendChild(header);
    await header.updateComplete;

    expect(
      header.shadowRoot
        ?.querySelector<HTMLImageElement>('img.header-brand-logo')
        ?.getAttribute('src')
    ).toBe('https://cdn.example.com/acme-light.svg');
    expect(header.shadowRoot?.querySelector('.chat-brand-badge')?.textContent).toBe('Chat');
    expect(header.shadowRoot?.textContent).not.toContain('Powered by Scion');
  });

  it('uses custom brand name in unauthorized page and sandboxed artifact banner', async () => {
    applyBranding({
      name: 'Acme Cloud',
      shortName: 'Acme',
      attribution: 'shown',
      themeColor: '#0c0a09',
    });

    const unauth = document.createElement('scion-page-unauthorized') as ScionPageUnauthorized;
    document.body.appendChild(unauth);
    await unauth.updateComplete;
    expect(unauth.shadowRoot?.textContent).toContain('access this Acme Cloud instance');

    const host = document.createElement('div');
    render(
      renderUntrustedViewFrame({
        owner: 'alice',
        title: 'Report',
        url: 'https://view.example.com/1',
      }),
      host
    );
    expect(host.textContent).toContain('isolated from Acme Cloud');
  });
});
