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
import {
  ASSET_SLOTS,
  BRANDING_KOANF_PREFIX,
  MAX_BRAND_ASSET_BYTES,
  MAX_BRAND_TOTAL_BYTES,
  brandingFromConfig,
  buildBrandingDiff,
  buildFileModeResetPayload,
  formatKiB,
  hasAdvancedPaletteOverrides,
  isBrandingDirty,
  isCuratedOrBuiltinFont,
  parseDataUriInfo,
  totalDataUriBytes,
  validateAccentColorInput,
  validateAssetFile,
  validateAssetUrlInput,
  validateFontSansInput,
  type BrandingFormState,
} from './admin-branding-state.js';

function makeDataUri(mediaType: string, byteLength: number): string {
  const bytes = new Uint8Array(byteLength);
  let binary = '';
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
  return `data:${mediaType};base64,${btoa(binary)}`;
}

describe('admin-branding-state', () => {
  describe('brandingFromConfig', () => {
    it('normalizes empty/undefined config to stock defaults', () => {
      const state = brandingFromConfig(undefined);
      expect(state).toEqual({
        name: '',
        short_name: '',
        subtitle: '',
        attribution: 'shown',
        corners: 'default',
        font_sans: 'default',
        accent_color: '',
        logo: { light: '', dark: '' },
        icons: {
          favicon: '',
          apple_touch_icon: '',
          icon_192: '',
          icon_512: '',
          icon_maskable_512: '',
        },
      });
    });

    it('preserves configured values from raw server config', () => {
      const state = brandingFromConfig({
        name: 'Acme Cloud',
        short_name: 'Acme',
        subtitle: 'Engineering Platform',
        attribution: 'hidden',
        corners: 'square',
        font_sans: 'ibm-plex-sans',
        accent_color: '#0d9488',
        logo: { light: 'https://cdn.example.com/light.svg' },
        icons: { favicon: '/favicon-custom.svg', icon_192: 'https://cdn.example.com/192.png' },
      });
      expect(state.name).toBe('Acme Cloud');
      expect(state.short_name).toBe('Acme');
      expect(state.subtitle).toBe('Engineering Platform');
      expect(state.attribution).toBe('hidden');
      expect(state.corners).toBe('square');
      expect(state.font_sans).toBe('ibm-plex-sans');
      expect(state.accent_color).toBe('#0d9488');
      expect(state.logo).toEqual({ light: 'https://cdn.example.com/light.svg', dark: '' });
      expect(state.icons.favicon).toBe('/favicon-custom.svg');
      expect(state.icons.icon_192).toBe('https://cdn.example.com/192.png');
      expect(state.icons.icon_512).toBe('');
    });
  });

  describe('hasAdvancedPaletteOverrides', () => {
    it('returns false when palette is absent or empty', () => {
      expect(hasAdvancedPaletteOverrides(undefined)).toBe(false);
      expect(hasAdvancedPaletteOverrides({})).toBe(false);
      expect(hasAdvancedPaletteOverrides({ palette: {} })).toBe(false);
      expect(hasAdvancedPaletteOverrides({ palette: { light: {}, primary: {} } })).toBe(false);
    });

    it('returns true when semantic or scale overrides exist', () => {
      expect(hasAdvancedPaletteOverrides({ palette: { light: { primary: '#1d4ed8' } } })).toBe(
        true
      );
      expect(hasAdvancedPaletteOverrides({ palette: { neutral: { '50': '#fafaf9' } } })).toBe(true);
    });
  });

  describe('buildBrandingDiff', () => {
    it('returns empty object when nothing changed', () => {
      const base = brandingFromConfig({ name: 'Acme', accent_color: '#0d9488' });
      expect(buildBrandingDiff(base, base, new Set())).toEqual({});
      expect(isBrandingDirty(base, base, new Set())).toBe(false);
    });

    it('sends only changed keys, omitting untouched fields and never including palette', () => {
      const base = brandingFromConfig({
        name: 'Acme',
        subtitle: 'Old',
        logo: {
          light: makeDataUri('image/png', 64),
          dark: 'https://cdn.example.com/dark.svg',
        },
        palette: { light: { primary: '#1d4ed8' } },
      });
      const draft: BrandingFormState = {
        ...base,
        logo: { ...base.logo },
        icons: { ...base.icons },
        name: 'Acme Platform',
      };
      const diff = buildBrandingDiff(draft, base, new Set());
      expect(diff).toEqual({ name: 'Acme Platform' });
      expect('palette' in diff).toBe(false);
      expect('logo' in diff).toBe(false);
    });

    it('emits empty string when resetting a string or enum field back to default', () => {
      const base = brandingFromConfig({
        name: 'Acme',
        attribution: 'hidden',
        corners: 'square',
        font_sans: 'dm-sans',
        accent_color: '#0d9488',
        logo: { light: 'https://cdn.example.com/light.svg' },
      });
      const draft: BrandingFormState = {
        ...base,
        name: '',
        attribution: 'shown',
        corners: 'default',
        font_sans: 'default',
        accent_color: '',
        logo: { light: '', dark: '' },
        icons: { ...base.icons },
      };
      const diff = buildBrandingDiff(draft, base, new Set());
      expect(diff).toEqual({
        name: '',
        attribution: '',
        corners: '',
        font_sans: '',
        accent_color: '',
        logo: { light: '' },
      });
    });

    it('omits env-locked keys even if draft differs from baseline', () => {
      const base = brandingFromConfig({ name: 'Old' });
      const draft: BrandingFormState = {
        ...base,
        name: 'New',
        subtitle: 'Updated',
        logo: { light: 'https://cdn.example.com/a.svg', dark: '' },
        icons: { ...base.icons },
      };
      const locked = new Set([
        `${BRANDING_KOANF_PREFIX}.name`,
        `${BRANDING_KOANF_PREFIX}.logo.light`,
      ]);
      const diff = buildBrandingDiff(draft, base, locked);
      expect(diff).toEqual({ subtitle: 'Updated' });
    });
  });

  describe('buildFileModeResetPayload', () => {
    it('clears all non-env-locked branding keys and includes palette: null when palette is set', () => {
      const locked = new Set([`${BRANDING_KOANF_PREFIX}.name`]);
      const payload = buildFileModeResetPayload(locked, true);
      expect('name' in payload).toBe(false);
      expect(payload.short_name).toBe('');
      expect(payload.subtitle).toBe('');
      expect(payload.attribution).toBe('');
      expect(payload.corners).toBe('');
      expect(payload.font_sans).toBe('');
      expect(payload.accent_color).toBe('');
      expect(payload.logo).toEqual({ light: '', dark: '' });
      expect(payload.icons).toEqual({
        favicon: '',
        apple_touch_icon: '',
        icon_192: '',
        icon_512: '',
        icon_maskable_512: '',
      });
      expect(payload.palette).toBeNull();
    });

    it('omits palette when no palette overrides exist', () => {
      const payload = buildFileModeResetPayload(new Set(), false);
      expect('palette' in payload).toBe(false);
    });
  });

  describe('data: URI byte accounting & file validation', () => {
    it('parses data: URI media type and exact decoded byte length', () => {
      const uri = makeDataUri('image/png', 1024);
      const info = parseDataUriInfo(uri);
      expect(info).toEqual({ mediaType: 'image/png', decodedBytes: 1024 });
      expect(parseDataUriInfo('https://cdn.example.com/logo.png')).toBeNull();
    });

    it('sums decoded data: bytes across logo and icon slots only', () => {
      const state = brandingFromConfig({
        logo: {
          light: makeDataUri('image/svg+xml', 500),
          dark: 'https://cdn.example.com/dark.svg',
        },
        icons: {
          favicon: makeDataUri('image/png', 1500),
          icon_192: '/custom-192.png',
        },
      });
      expect(totalDataUriBytes(state)).toBe(2000);
      expect(formatKiB(1536)).toBe('1.5 KiB');
    });

    it('enforces PNG-only on PWA/Apple icon slots', () => {
      const appleSlot = ASSET_SLOTS.find((s) => s.key === 'icons.apple_touch_icon')!;
      expect(
        validateAssetFile({ name: 'icon.svg', type: 'image/svg+xml', size: 1024 }, appleSlot, 0)
      ).toMatch(/PNG/i);
      expect(
        validateAssetFile({ name: 'icon.png', type: 'image/png', size: 1024 }, appleSlot, 0)
      ).toBeNull();
    });

    it('rejects files over 128 KiB per asset and over 512 KiB total budget', () => {
      const logoSlot = ASSET_SLOTS.find((s) => s.key === 'logo.light')!;
      expect(
        validateAssetFile(
          { name: 'big.png', type: 'image/png', size: MAX_BRAND_ASSET_BYTES + 1 },
          logoSlot,
          0
        )
      ).toMatch(/128 KiB/);
      expect(
        validateAssetFile(
          { name: 'ok.png', type: 'image/png', size: 64 * 1024 },
          logoSlot,
          MAX_BRAND_TOTAL_BYTES - 32 * 1024
        )
      ).toMatch(/512 KiB/);
    });
  });

  describe('URL, font, and accent color validators', () => {
    it('flags plain http:// URLs and protocol-relative paths', () => {
      expect(validateAssetUrlInput('')).toBeNull();
      expect(validateAssetUrlInput('https://cdn.example.com/logo.svg')).toBeNull();
      expect(validateAssetUrlInput('/assets/logo.png')).toBeNull();
      expect(validateAssetUrlInput('http://example.com/logo.png')).toMatch(/http:\/\//i);
      expect(validateAssetUrlInput('//cdn.example.com/logo.png')).toMatch(/\/\//);
      expect(validateAssetUrlInput('ftp://example.com/logo.png')).toMatch(/https:\/\//);
    });

    it('validates font_sans curated presets and Google Fonts family regex', () => {
      expect(isCuratedOrBuiltinFont('default')).toBe(true);
      expect(isCuratedOrBuiltinFont('system')).toBe(true);
      expect(isCuratedOrBuiltinFont('ibm-plex-sans')).toBe(true);
      expect(isCuratedOrBuiltinFont('Lato')).toBe(false);

      expect(validateFontSansInput('default')).toBeNull();
      expect(validateFontSansInput('Lato')).toBeNull();
      expect(validateFontSansInput('Open Sans')).toBeNull();
      expect(validateFontSansInput('Bad;Font')).toMatch(/Google Fonts/i);
    });

    it('validates #RRGGBB accent color', () => {
      expect(validateAccentColorInput('')).toBeNull();
      expect(validateAccentColorInput('#0d9488')).toBeNull();
      expect(validateAccentColorInput('#FFF')).toMatch(/#RRGGBB/);
      expect(validateAccentColorInput('teal')).toMatch(/#RRGGBB/);
    });
  });
});
