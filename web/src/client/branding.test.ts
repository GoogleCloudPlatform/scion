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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PublicBranding } from '../shared/types.js';
import {
  applyBranding,
  BRANDING_STYLE_ID,
  BRANDING_UPDATED_EVENT,
  DEFAULT_BRAND_SUBTITLE,
  DEFAULT_NOTIFICATION_ICON,
  getBranding,
  getBrandMonogram,
  getBrandName,
  getBrandSubtitle,
  getNotificationIconUrl,
  isCustomBranding,
  shouldShowAttribution,
} from './branding.js';
import { BRANDING_FONT_SANS_ID, buildBrandingCSS, syncBrandingDOM } from './branding-runtime.js';
import { getAppName } from './page-title.js';

describe('branding state & runtime DOM sync', () => {
  beforeEach(() => {
    document.head.innerHTML = `
      <link rel="icon" href="/favicon.ico" sizes="any" />
      <link rel="icon" href="/favicon.svg" type="image/svg+xml" />
      <link rel="apple-touch-icon" href="/apple-touch-icon.png" />
      <meta name="theme-color" content="#1e293b" />
      <style id="theme-css-stub">html:root { --scion-primary: #3b82f6; }</style>
    `;
    applyBranding(null);
    syncBrandingDOM(null);
  });

  afterEach(() => {
    applyBranding(null);
    syncBrandingDOM(null);
  });

  it('defaults to stock Scion identity with attribution hidden', () => {
    expect(getBranding()).toBeNull();
    expect(getBrandName()).toBe('Scion');
    expect(getAppName()).toBe('Scion');
    expect(isCustomBranding()).toBe(false);
    expect(shouldShowAttribution()).toBe(false);
    expect(getBrandSubtitle()).toBe(DEFAULT_BRAND_SUBTITLE);
    expect(getBrandSubtitle('Agent Orchestration')).toBe('Agent Orchestration');
    expect(getBrandMonogram()).toBe('S');
    expect(getNotificationIconUrl()).toBe(DEFAULT_NOTIFICATION_ICON);
  });

  it('hides stock subtitle when custom brand name omits subtitle, and shows attribution by default', () => {
    const custom: PublicBranding = {
      name: 'Acme Cloud',
      shortName: 'Acme',
      attribution: 'shown',
      themeColor: '#0c0a09',
    };
    const listener = vi.fn();
    window.addEventListener(BRANDING_UPDATED_EVENT, listener);

    applyBranding(custom);

    expect(getBrandName()).toBe('Acme Cloud');
    expect(getAppName()).toBe('Acme Cloud');
    expect(isCustomBranding()).toBe(true);
    expect(shouldShowAttribution()).toBe(true);
    expect(getBrandSubtitle()).toBe('');
    expect(getBrandSubtitle('Agent Orchestration')).toBe('');
    expect(getBrandMonogram()).toBe('A');
    expect(listener).toHaveBeenCalledTimes(1);

    // Re-applying identical payload is a no-op (no duplicate event).
    applyBranding({ ...custom });
    expect(listener).toHaveBeenCalledTimes(1);

    window.removeEventListener(BRANDING_UPDATED_EVENT, listener);
  });

  it('respects explicit subtitle, hidden attribution, custom logo on stock name, and notification icon', () => {
    applyBranding({
      name: 'Scion',
      shortName: 'Scion',
      subtitle: 'Custom Platform',
      attribution: 'shown',
      logoLight: 'https://cdn.example.com/logo.svg',
      notificationIconUrl: '/brand/icon-192?v=12345678',
      themeColor: '#1e293b',
    });

    expect(isCustomBranding()).toBe(true);
    expect(shouldShowAttribution()).toBe(true);
    expect(getBrandSubtitle()).toBe('Custom Platform');
    expect(getNotificationIconUrl()).toBe('/brand/icon-192?v=12345678');

    applyBranding({
      name: 'Acme',
      shortName: 'Acme',
      attribution: 'hidden',
      themeColor: '#1e293b',
    });
    expect(isCustomBranding()).toBe(true);
    expect(shouldShowAttribution()).toBe(false);
  });

  it('syncs #scion-branding-tokens at the end of document.head, updates icons/theme-color, and restores stock on reset', () => {
    const custom: PublicBranding = {
      name: 'Acme',
      shortName: 'Acme',
      attribution: 'shown',
      faviconUrl: '/brand/favicon?v=abcd1234',
      appleTouchIconUrl: '/brand/apple-touch-icon?v=abcd1234',
      themeColor: '#0c0a09',
      cssText: 'html:root, :host, .sl-theme-light {\n  --scion-primary: #0f766e;\n}\n',
    };

    syncBrandingDOM(custom);

    const styleEl = document.getElementById(BRANDING_STYLE_ID);
    expect(styleEl).not.toBeNull();
    expect(styleEl?.textContent).toContain('--scion-primary: #0f766e;');
    // Style element must be positioned after theme.css.
    const themeStub = document.getElementById('theme-css-stub');
    expect(
      Boolean(
        themeStub &&
        styleEl &&
        themeStub.compareDocumentPosition(styleEl) & Node.DOCUMENT_POSITION_FOLLOWING
      )
    ).toBe(true);

    // Custom favicon replaces stock /favicon.ico and /favicon.svg so browsers do not prefer stock SVG.
    const icons = document.head.querySelectorAll<HTMLLinkElement>('link[rel="icon"]');
    expect(icons).toHaveLength(1);
    expect(icons[0].getAttribute('href')).toBe('/brand/favicon?v=abcd1234');

    const apple = document.head.querySelector<HTMLLinkElement>('link[rel="apple-touch-icon"]');
    expect(apple?.getAttribute('href')).toBe('/brand/apple-touch-icon?v=abcd1234');

    const themeMeta = document.head.querySelector<HTMLMetaElement>('meta[name="theme-color"]');
    expect(themeMeta?.content).toBe('#0c0a09');

    // Reset back to stock Scion.
    syncBrandingDOM(null);

    expect(document.getElementById(BRANDING_STYLE_ID)).toBeNull();
    const restoredHrefs = Array.from(
      document.head.querySelectorAll<HTMLLinkElement>('link[rel="icon"]')
    ).map((el) => el.getAttribute('href'));
    expect(restoredHrefs).toEqual(['/favicon.ico', '/favicon.svg']);
    expect(
      document.head
        .querySelector<HTMLLinkElement>('link[rel="apple-touch-icon"]')
        ?.getAttribute('href')
    ).toBe('/apple-touch-icon.png');
    expect(document.head.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.content).toBe(
      '#1e293b'
    );
  });

  it('builds declarative light/dark/system-dark CSS from scales & semantic modes and ignores non-hex values', () => {
    const css = buildBrandingCSS({
      name: 'Acme',
      shortName: 'Acme',
      attribution: 'shown',
      themeColor: '#0c0a09',
      scales: {
        primary: {
          '500': '#0d9488',
          '600': '#0f766e',
          '700': 'red; } body { display: none',
        },
      },
      light: {
        primary: '#0f766e',
        primary_text: '#ffffff',
      },
      dark: {
        primary: '#2dd4bf',
        primary_text: '#0c0a09',
      },
    });

    expect(css).toContain('html:root, :host, .sl-theme-light');
    expect(css).toContain('--scion-primary-500: #0d9488;');
    expect(css).toContain('--sl-color-primary-600: #0f766e;');
    expect(css).toContain("html:root.sl-theme-dark, html:root[data-theme='dark']");
    expect(css).toContain('@media (prefers-color-scheme: dark)');
    expect(css).toContain('--scion-primary: #2dd4bf;');
    expect(css).not.toContain('display: none');
  });

  it('swaps #scion-brand-font-sans live for curated/custom Google Fonts, omits Inter on system, and restores Inter on reset', () => {
    document.head.innerHTML = `
      <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&display=swap" rel="stylesheet">
      <link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
      <style id="theme-css-stub">html:root { --scion-font-sans: 'Inter', sans-serif; }</style>
    `;

    // 1. Curated preset swaps the Inter link in place and leaves JetBrains Mono untouched.
    syncBrandingDOM({
      name: 'Scion',
      shortName: 'Scion',
      attribution: 'shown',
      fontSans: 'ibm-plex-sans',
      themeColor: '#1e293b',
      cssText:
        "html:root, :host, .sl-theme-light {\n  --scion-font-sans: 'IBM Plex Sans', sans-serif;\n}\n",
    });

    const curatedLink = document.getElementById(BRANDING_FONT_SANS_ID) as HTMLLinkElement | null;
    expect(curatedLink?.getAttribute('href')).toBe(
      'https://fonts.googleapis.com/css2?family=IBM+Plex+Sans:wght@400;500;600;700&display=swap'
    );
    expect(document.head.querySelector('link[href*="JetBrains+Mono"]')).not.toBeNull();

    // 2. Arbitrary validated Google Font family updates href with URL encoding.
    syncBrandingDOM({
      name: 'Scion',
      shortName: 'Scion',
      attribution: 'shown',
      fontSans: 'Work Sans',
      themeColor: '#1e293b',
      cssText:
        "html:root, :host, .sl-theme-light {\n  --scion-font-sans: 'Work Sans', sans-serif;\n}\n",
    });
    expect(
      (document.getElementById(BRANDING_FONT_SANS_ID) as HTMLLinkElement | null)?.getAttribute(
        'href'
      )
    ).toBe('https://fonts.googleapis.com/css2?family=Work+Sans:wght@400;500;600;700&display=swap');

    // 3. "system" removes the sans Google Font link while preserving JetBrains Mono.
    syncBrandingDOM({
      name: 'Scion',
      shortName: 'Scion',
      attribution: 'shown',
      fontSans: 'system',
      themeColor: '#1e293b',
      cssText:
        'html:root, :host, .sl-theme-light {\n  --scion-font-sans: ui-sans-serif, system-ui, sans-serif;\n}\n',
    });
    expect(document.getElementById(BRANDING_FONT_SANS_ID)).toBeNull();
    expect(document.head.querySelector('link[href*="family=Inter"]')).toBeNull();
    expect(document.head.querySelector('link[href*="JetBrains+Mono"]')).not.toBeNull();

    // 4. Resetting back to stock restores the Inter link before JetBrains Mono.
    syncBrandingDOM(null);
    const restored = document.getElementById(BRANDING_FONT_SANS_ID) as HTMLLinkElement | null;
    expect(restored?.getAttribute('href')).toBe(
      'https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&display=swap'
    );
    const mono = document.head.querySelector('link[href*="JetBrains+Mono"]');
    expect(
      Boolean(
        restored &&
        mono &&
        restored.compareDocumentPosition(mono) & Node.DOCUMENT_POSITION_FOLLOWING
      )
    ).toBe(true);
  });

  it('rejects injection attempts in fontSans during live DOM sync', () => {
    document.head.innerHTML = `
      <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&display=swap" rel="stylesheet">
    `;

    syncBrandingDOM({
      name: 'Scion',
      shortName: 'Scion',
      attribution: 'shown',
      fontSans: "Inter'; } body { display: none",
      themeColor: '#1e293b',
    });

    const links = document.head.querySelectorAll<HTMLLinkElement>('link[rel="stylesheet"]');
    expect(links).toHaveLength(1);
    expect(links[0].getAttribute('href')).toBe(
      'https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&display=swap'
    );
  });
});
