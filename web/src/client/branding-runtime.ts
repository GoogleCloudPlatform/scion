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
 * Lazy-loaded DOM runtime for Hub branding.
 *
 * Manages <style id="scion-branding-tokens"> (positioned at the end of
 * document.head so custom palette rules win over theme.css regardless of load
 * order), dynamic <link rel="icon"> / <link rel="apple-touch-icon">, and
 * <meta name="theme-color">.
 */

import type { BrandingSemanticMode, PublicBranding } from '../shared/types.js';
import { BRANDING_STYLE_ID } from './branding.js';

const DEFAULT_THEME_COLOR = '#1e293b';
const SCALE_RAMPS = ['primary', 'neutral', 'success', 'warning', 'danger'] as const;
const SCALE_STEPS = [
  '50',
  '100',
  '200',
  '300',
  '400',
  '500',
  '600',
  '700',
  '800',
  '900',
  '950',
] as const;
const HEX_COLOR_RE = /^#[0-9a-f]{6}$/i;

function isHexColor(v: string | undefined): v is string {
  return typeof v === 'string' && HEX_COLOR_RE.test(v.trim());
}

function formatSemanticMode(mode: BrandingSemanticMode | undefined, indent: string): string {
  if (!mode) return '';
  const pairs: Array<[string, string | undefined]> = [
    ['--scion-bg', mode.bg],
    ['--scion-bg-subtle', mode.bg_subtle],
    ['--scion-surface', mode.surface],
    ['--scion-surface-raised', mode.surface_raised],
    ['--scion-text', mode.text],
    ['--scion-text-secondary', mode.text_secondary],
    ['--scion-text-muted', mode.text_muted],
    ['--scion-border', mode.border],
    ['--scion-primary', mode.primary],
    ['--scion-primary-hover', mode.primary_hover],
    ['--scion-primary-active', mode.primary_active],
    ['--scion-primary-text', mode.primary_text],
  ];
  let out = '';
  for (const [prop, val] of pairs) {
    if (isHexColor(val)) {
      out += `${indent}${prop}: ${val.trim().toLowerCase()};\n`;
    }
  }
  return out;
}

/**
 * Builds the declarative light / explicit-dark / system-dark CSS sheet from a
 * PublicBranding payload when pre-rendered `cssText` is not already supplied.
 */
export function buildBrandingCSS(branding: PublicBranding): string {
  if (branding.cssText && branding.cssText.trim().length > 0) {
    return branding.cssText;
  }

  let lightDecls = '';
  if (branding.scales) {
    for (const ramp of SCALE_RAMPS) {
      const scale = branding.scales[ramp];
      if (!scale) continue;
      for (const step of SCALE_STEPS) {
        const val = scale[step];
        if (isHexColor(val)) {
          const hex = val.trim().toLowerCase();
          lightDecls += `  --scion-${ramp}-${step}: ${hex};\n`;
          lightDecls += `  --sl-color-${ramp}-${step}: ${hex};\n`;
        }
      }
    }
  }
  lightDecls += formatSemanticMode(branding.light, '  ');
  const darkDecls = formatSemanticMode(branding.dark, '  ');
  const systemDarkDecls = formatSemanticMode(branding.dark, '    ');

  if (!lightDecls && !darkDecls) return '';

  let css = '';
  if (lightDecls) {
    css += `html:root, :host, .sl-theme-light {\n${lightDecls}}\n`;
  }
  if (darkDecls) {
    css += `html:root.sl-theme-dark, html:root[data-theme='dark'], :root.dark {\n${darkDecls}}\n`;
    css += `@media (prefers-color-scheme: dark) {\n  html:root:not([data-theme='light']) {\n${systemDarkDecls}  }\n}\n`;
  }
  return css;
}

function syncStyleElement(branding: PublicBranding | null): void {
  const existing = document.getElementById(BRANDING_STYLE_ID) as HTMLStyleElement | null;
  const css = branding ? buildBrandingCSS(branding) : '';

  if (!css) {
    existing?.remove();
    return;
  }

  const styleEl = existing ?? document.createElement('style');
  styleEl.id = BRANDING_STYLE_ID;
  if (styleEl.textContent !== css) {
    styleEl.textContent = css;
  }
  // Always keep #scion-branding-tokens at the end of document.head so it comes
  // after theme.css (which main.ts appends at module load).
  if (document.head.lastElementChild !== styleEl) {
    document.head.appendChild(styleEl);
  }
}

function syncFavicon(faviconUrl: string | undefined): void {
  const iconLinks = Array.from(
    document.head.querySelectorAll<HTMLLinkElement>('link[rel="icon"], link[rel="shortcut icon"]')
  );

  if (faviconUrl) {
    const existingCustom = iconLinks.find((l) => l.dataset.scionBrandingIcon === 'true');
    for (const link of iconLinks) {
      if (link !== existingCustom) {
        link.remove();
      }
    }
    const target = existingCustom ?? document.createElement('link');
    target.rel = 'icon';
    target.dataset.scionBrandingIcon = 'true';
    if (target.getAttribute('href') !== faviconUrl) {
      target.href = faviconUrl;
    }
    if (!target.parentNode) {
      document.head.appendChild(target);
    }
    return;
  }

  // Restoring stock icons if a custom branding icon was previously injected.
  const customLinks = iconLinks.filter((l) => l.dataset.scionBrandingIcon === 'true');
  if (customLinks.length === 0) return;
  for (const link of customLinks) {
    link.remove();
  }
  const hasStockIco = document.head.querySelector('link[rel="icon"][href="/favicon.ico"]');
  if (!hasStockIco) {
    const ico = document.createElement('link');
    ico.rel = 'icon';
    ico.href = '/favicon.ico';
    ico.setAttribute('sizes', 'any');
    document.head.appendChild(ico);
  }
  const hasStockSvg = document.head.querySelector('link[rel="icon"][href="/favicon.svg"]');
  if (!hasStockSvg) {
    const svg = document.createElement('link');
    svg.rel = 'icon';
    svg.href = '/favicon.svg';
    svg.type = 'image/svg+xml';
    document.head.appendChild(svg);
  }
}

function syncAppleTouchIcon(appleTouchIconUrl: string | undefined): void {
  const link = document.head.querySelector<HTMLLinkElement>('link[rel="apple-touch-icon"]');
  if (appleTouchIconUrl) {
    const target = link ?? document.createElement('link');
    target.rel = 'apple-touch-icon';
    target.dataset.scionBrandingAppleIcon = 'true';
    if (target.getAttribute('href') !== appleTouchIconUrl) {
      target.href = appleTouchIconUrl;
    }
    if (!target.parentNode) {
      document.head.appendChild(target);
    }
    return;
  }

  if (link?.dataset.scionBrandingAppleIcon === 'true') {
    link.href = '/apple-touch-icon.png';
    delete link.dataset.scionBrandingAppleIcon;
  }
}

function syncThemeColor(themeColor: string | undefined): void {
  const nextColor = isHexColor(themeColor) ? themeColor.trim().toLowerCase() : DEFAULT_THEME_COLOR;
  const meta = document.head.querySelector<HTMLMetaElement>('meta[name="theme-color"]');
  if (meta) {
    if (meta.content !== nextColor) {
      meta.content = nextColor;
    }
  } else if (themeColor) {
    const created = document.createElement('meta');
    created.name = 'theme-color';
    created.content = nextColor;
    document.head.appendChild(created);
  }
}

/**
 * Synchronizes document.head branding tags (<style id="scion-branding-tokens">,
 * favicons, and <meta name="theme-color">) with the active PublicBranding.
 */
export function syncBrandingDOM(branding: PublicBranding | null): void {
  if (typeof document === 'undefined' || !document.head) return;
  syncStyleElement(branding);
  syncFavicon(branding?.faviconUrl);
  syncAppleTouchIcon(branding?.appleTouchIconUrl);
  syncThemeColor(branding?.themeColor);
}
