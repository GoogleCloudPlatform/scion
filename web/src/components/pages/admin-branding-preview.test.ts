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

import { describe, it, expect, beforeAll, afterEach } from 'vitest';
import type { ScionBrandingPreview } from './admin-branding-preview.js';
import { resolvePreviewFontStack, resolvePreviewRadii } from './admin-branding-preview.js';
import { defaultBrandingFormState, type BrandingFormState } from './admin-branding-state.js';
import { resolveBrandingPalette } from './admin-branding-contrast.js';

function query<T extends Element = Element>(el: HTMLElement, selector: string): T | null {
  return (el.shadowRoot?.querySelector(selector) as T | null) ?? null;
}

describe('scion-branding-preview', () => {
  let element: ScionBrandingPreview | null = null;

  beforeAll(async () => {
    await import('./admin-branding-preview.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
  });

  async function mountPreview(
    draftOverrides: Partial<BrandingFormState> = {},
    palette?: Parameters<typeof resolveBrandingPalette>[0]['palette']
  ): Promise<ScionBrandingPreview> {
    const base = defaultBrandingFormState();
    const draft: BrandingFormState = {
      ...base,
      ...draftOverrides,
      logo: { ...base.logo, ...(draftOverrides.logo ?? {}) },
      icons: { ...base.icons, ...(draftOverrides.icons ?? {}) },
    };
    const el = document.createElement('scion-branding-preview') as ScionBrandingPreview;
    el.draft = draft;
    el.resolved = resolveBrandingPalette({ accentColor: draft.accent_color, palette });
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  it('renders scoped Light and Dark preview panes without mutating document.documentElement', async () => {
    const beforeStyle = document.documentElement.getAttribute('style');
    element = await mountPreview({
      name: 'Acme Cloud',
      subtitle: 'Engineering Hub',
      corners: 'square',
      font_sans: 'dm-sans',
      accent_color: '#0d9488',
    });

    const lightPane = query<HTMLElement>(element, '[data-preview-pane="light"]')!;
    const darkPane = query<HTMLElement>(element, '[data-preview-pane="dark"]')!;
    expect(lightPane).not.toBeNull();
    expect(darkPane).not.toBeNull();

    // Scoped inline CSS custom properties reflect resolved palette, corners, and font
    const lightStyle = lightPane.getAttribute('style') ?? '';
    const darkStyle = darkPane.getAttribute('style') ?? '';
    expect(lightStyle).toContain('--preview-radius: 0');
    expect(lightStyle).toContain("--preview-font-sans: 'DM Sans'");
    expect(lightStyle).toContain(`--preview-primary: ${element.resolved.light.primary}`);
    expect(darkStyle).toContain(`--preview-primary: ${element.resolved.dark.primary}`);
    expect(darkStyle).toContain(`--preview-bg: ${element.resolved.dark.bg}`);

    // Brand name, subtitle, and attribution render inside panes
    expect(lightPane.querySelector('[data-preview-brand-name]')?.textContent).toBe('Acme Cloud');
    expect(lightPane.querySelector('[data-preview-subtitle]')?.textContent).toBe('Engineering Hub');
    expect(lightPane.querySelector('[data-preview-attribution]')?.textContent).toContain(
      'Powered by Scion'
    );

    // Never mutates document.documentElement
    expect(document.documentElement.getAttribute('style')).toBe(beforeStyle);
  });

  it('honors attribution=hidden, custom-name subtitle hiding, and logo light/dark fallback', async () => {
    element = await mountPreview({
      name: 'Acme Cloud',
      subtitle: '',
      attribution: 'hidden',
      logo: { light: 'https://cdn.example.com/light.svg', dark: '' },
    });

    const lightPane = query<HTMLElement>(element, '[data-preview-pane="light"]')!;
    const darkPane = query<HTMLElement>(element, '[data-preview-pane="dark"]')!;

    // Stock subtitle hidden when custom name set and subtitle empty
    expect(lightPane.querySelector('[data-preview-subtitle]')).toBeNull();
    // Attribution hidden when attribution === 'hidden'
    expect(lightPane.querySelector('[data-preview-attribution]')).toBeNull();
    expect(darkPane.querySelector('[data-preview-attribution]')).toBeNull();

    // Dark logo falls back to light logo with referrerpolicy="no-referrer"
    const darkImg = darkPane.querySelector<HTMLImageElement>('img.mock-logo');
    expect(darkImg?.getAttribute('src')).toBe('https://cdn.example.com/light.svg');
    expect(darkImg?.getAttribute('referrerpolicy')).toBe('no-referrer');
  });

  it('ignores invalid http:// logo URLs in preview and falls back to monogram', async () => {
    element = await mountPreview({
      name: 'Acme',
      logo: { light: 'http://insecure.example.com/logo.png', dark: '' },
    });

    const lightPane = query<HTMLElement>(element, '[data-preview-pane="light"]')!;
    expect(lightPane.querySelector('img.mock-logo')).toBeNull();
    expect(lightPane.querySelector('.mock-monogram')?.textContent).toBe('A');
  });

  it('shows explicit text contrast summary badges (stock, pass, fail, invalid) and has no noisy aria-live regions', async () => {
    // Stock defaults
    element = await mountPreview();
    expect(
      query(element, '[data-preview-contrast-summary="stock"]')?.textContent?.trim()
    ).toContain('Stock palette defaults');
    expect(element.shadowRoot?.querySelector('[aria-live]')).toBeNull();

    // Passing custom accent
    element.draft = { ...element.draft, accent_color: '#0d9488' };
    element.resolved = resolveBrandingPalette({ accentColor: '#0d9488' });
    await element.updateComplete;
    expect(query(element, '[data-preview-contrast-summary="pass"]')?.textContent?.trim()).toContain(
      'WCAG AA Pass'
    );

    // Failing palette override
    element.resolved = resolveBrandingPalette({
      palette: { light: { primary: '#eab308' } },
    });
    await element.updateComplete;
    expect(query(element, '[data-preview-contrast-summary="fail"]')?.textContent?.trim()).toContain(
      'WCAG AA Fail'
    );

    // Invalid hex format
    element.resolved = resolveBrandingPalette({ accentColor: '#0d9' });
    await element.updateComplete;
    expect(
      query(element, '[data-preview-contrast-summary="invalid"]')?.textContent?.trim()
    ).toContain('Invalid color format');

    // Still zero aria-live regions so live color drags are never announced noisily
    expect(element.shadowRoot?.querySelector('[aria-live]')).toBeNull();
  });

  it('resolves font stacks and corner radii matching Go branding defaults', () => {
    expect(resolvePreviewFontStack('default')).toContain("'Inter'");
    expect(resolvePreviewFontStack('system')).toContain('ui-sans-serif');
    expect(resolvePreviewFontStack('ibm-plex-sans')).toContain("'IBM Plex Sans'");
    expect(resolvePreviewFontStack('Work Sans')).toContain("'Work Sans'");
    expect(resolvePreviewFontStack('Bad;Font')).toContain("'Inter'");

    expect(resolvePreviewRadii('default')).toEqual({
      sm: '0.25rem',
      base: '0.5rem',
      lg: '0.75rem',
    });
    expect(resolvePreviewRadii('square')).toEqual({ sm: '0', base: '0', lg: '0' });
    expect(resolvePreviewRadii('rounded')).toEqual({ sm: '0.375rem', base: '0.75rem', lg: '1rem' });
  });
});
