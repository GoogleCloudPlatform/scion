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
  OKLCH_SHADE_ANCHORS,
  SCALE_STEPS,
  compositeAlphaContrast,
  contrastRatio,
  defaultPrimaryScale,
  formatContrastRatio,
  generateOKLCHScale,
  isValidHexColor,
  relativeLuminance,
  resolveBrandingPalette,
} from './admin-branding-contrast.js';

describe('admin-branding-contrast', () => {
  describe('WCAG 2.1 luminance and contrast vectors', () => {
    it('computes exact known WCAG 2.1 contrast ratios', () => {
      expect(relativeLuminance('#000000')).toBeCloseTo(0, 6);
      expect(relativeLuminance('#ffffff')).toBeCloseTo(1, 6);
      expect(contrastRatio('#ffffff', '#000000')).toBeCloseTo(21, 5);
      expect(contrastRatio('#000000', '#ffffff')).toBeCloseTo(21, 5);
      expect(contrastRatio('#ffffff', '#ffffff')).toBeCloseTo(1, 5);

      // Stock Tailwind blue-500 (#3b82f6) vs white is 3.68:1 (fails AA normal text)
      expect(contrastRatio('#3b82f6', '#ffffff')!).toBeCloseTo(3.68, 2);
      // Tailwind blue-600 (#2563eb) vs white is 5.17:1 (passes AA normal text)
      expect(contrastRatio('#2563eb', '#ffffff')!).toBeCloseTo(5.17, 2);
      // Mid-gray #777777 vs white is 4.48:1 (just below 4.50:1)
      expect(contrastRatio('#777777', '#ffffff')!).toBeCloseTo(4.48, 2);
      // #767676 vs white is 4.54:1 (classic WCAG AA boundary pass)
      expect(contrastRatio('#767676', '#ffffff')!).toBeCloseTo(4.54, 2);

      expect(formatContrastRatio(5.172)).toBe('5.17:1');
      expect(contrastRatio('not-a-hex', '#ffffff')).toBeNull();
    });

    it('computes alpha-composited contrast for dark badges', () => {
      // 0% alpha over surface equals surface; 100% alpha equals tint
      expect(compositeAlphaContrast('#f1f5f9', '#3b82f6', 0, '#1e293b')!).toBeCloseTo(
        contrastRatio('#f1f5f9', '#1e293b')!,
        5
      );
      expect(compositeAlphaContrast('#f1f5f9', '#3b82f6', 1, '#1e293b')!).toBeCloseTo(
        contrastRatio('#f1f5f9', '#3b82f6')!,
        5
      );
    });
  });

  describe('generateOKLCHScale (Go parity with TestGenerateOKLCHScale_LuminanceAnchorsAcrossHues)', () => {
    const accents = [
      '#0d9488', // teal
      '#d97706', // amber
      '#eab308', // yellow
      '#3b82f6', // blue
      '#1e3a8a', // dark navy
      '#7c3aed', // violet
      '#dc2626', // red
      '#64748b', // slate
      '#555555', // neutral gray
    ];

    it.each(accents)(
      'produces monotonic 50..950 ramp within ±0.02 of luminance anchors for %s',
      (accent) => {
        const scale = generateOKLCHScale(accent);
        expect(scale).not.toBeNull();
        let prevY = 1.1;
        for (const anchor of OKLCH_SHADE_ANCHORS) {
          const hexVal = scale![anchor.step];
          expect(isValidHexColor(hexVal)).toBe(true);
          const y = relativeLuminance(hexVal)!;
          expect(y).toBeLessThan(prevY);
          prevY = y;
          expect(Math.abs(y - anchor.targetY)).toBeLessThanOrEqual(0.02);
        }
        expect(Object.keys(scale!)).toEqual([...SCALE_STEPS]);
      }
    );

    it('returns null for invalid hex colors', () => {
      expect(generateOKLCHScale('#fff')).toBeNull();
      expect(generateOKLCHScale('teal')).toBeNull();
    });
  });

  describe('resolveBrandingPalette (Go parity)', () => {
    it('returns stock defaults with no contrast failures when uncustomized', () => {
      const res = resolveBrandingPalette({});
      expect(res.hasCustomPalette).toBe(false);
      expect(res.checks).toHaveLength(0);
      expect(res.summary).toEqual({ total: 0, passed: 0, failed: 0, allPass: true });
      expect(res.light.primary).toBe('#3b82f6');
      expect(res.dark.primary).toBe('#60a5fa');
    });

    it('auto-selects dark foreground (#0f172a) for mid-tone accents in dark mode and passes all WCAG AA checks', () => {
      for (const accent of ['#0d9488', '#d97706', '#eab308', '#1e3a8a']) {
        const res = resolveBrandingPalette({ accentColor: accent });
        expect(res.hasCustomPalette).toBe(true);
        expect(res.syntaxErrors).toHaveLength(0);
        expect(res.summary.allPass).toBe(true);
        expect(res.summary.total).toBeGreaterThan(0);
        expect(res.dark.primary_text).toBe('#0f172a');
        expect(contrastRatio(res.light.primary_text, res.light.primary)!).toBeGreaterThanOrEqual(
          4.5
        );
        expect(contrastRatio(res.dark.primary_text, res.dark.primary)!).toBeGreaterThanOrEqual(4.5);
      }
    });

    it('matches all 9 WCAG AA rejection cases from Go TestValidateBranding_WCAGContrastRejections', () => {
      const cases: Array<{
        name: string;
        input: Parameters<typeof resolveBrandingPalette>[0];
        wantPathSub: string;
        wantMsgSub: string;
      }> = [
        {
          name: 'light primary low contrast on white surface',
          input: { palette: { light: { primary: '#eab308' } } },
          wantPathSub: 'server.hub.branding.palette.light.primary',
          wantMsgSub: 'minimum WCAG AA is 4.50:1',
        },
        {
          name: 'explicit white primary_text on bright dark-mode primary',
          input: { accentColor: '#d97706', palette: { dark: { primary_text: '#ffffff' } } },
          wantPathSub: 'server.hub.branding.palette.dark.primary_text',
          wantMsgSub: 'minimum WCAG AA is 4.50:1',
        },
        {
          name: 'light text_muted too faint against surface',
          input: { palette: { light: { text_muted: '#94a3b8' } } },
          wantPathSub: 'server.hub.branding.palette.light.text_muted',
          wantMsgSub: 'minimum WCAG AA is 4.50:1',
        },
        {
          name: 'dead-zone primary-600 fails Shoelace button contrast',
          input: { palette: { primary: { '600': '#777777' } } },
          wantPathSub: 'server.hub.branding.palette.primary.600',
          wantMsgSub: 'minimum WCAG AA is 4.50:1',
        },
        {
          name: 'bright primary-600 fails light Shoelace white-foreground contrast',
          input: { palette: { primary: { '600': '#eab308' } } },
          wantPathSub: 'server.hub.branding.palette.primary.600',
          wantMsgSub: 'minimum WCAG AA is 4.50:1',
        },
        {
          name: 'bright success-600 fails light Shoelace white-foreground contrast',
          input: { palette: { success: { '600': '#eab308' } } },
          wantPathSub: 'server.hub.branding.palette.success.600',
          wantMsgSub: 'minimum WCAG AA is 4.50:1',
        },
        {
          name: 'dark primary-300 fails dark Shoelace neutral-800 foreground contrast',
          input: { palette: { primary: { '300': '#1e3a8a' } } },
          wantPathSub: 'server.hub.branding.palette.primary.300',
          wantMsgSub: 'dark --sl-color-primary-600 (step 300)',
        },
        {
          name: 'dark primary-400 fails dark Shoelace hover neutral-800 foreground contrast',
          input: { palette: { primary: { '400': '#1e3a8a' } } },
          wantPathSub: 'server.hub.branding.palette.primary.400',
          wantMsgSub: 'minimum WCAG AA is 4.50:1',
        },
        {
          name: 'neutral-600 fails 3.0 UI border contrast on light surface',
          input: { palette: { neutral: { '600': '#cbd5e1' } } },
          wantPathSub: 'server.hub.branding.palette.neutral.600',
          wantMsgSub: '3.00:1',
        },
      ];

      for (const tc of cases) {
        const res = resolveBrandingPalette(tc.input);
        const failed = res.checks.filter((c) => !c.pass);
        expect(failed.length, tc.name).toBeGreaterThan(0);
        expect(res.summary.allPass, tc.name).toBe(false);
        const matched = failed.some(
          (c) => c.path.includes(tc.wantPathSub) && c.message.includes(tc.wantMsgSub)
        );
        expect(matched, `${tc.name}: got ${JSON.stringify(failed)}`).toBe(true);
      }
    });

    it('matches Go TestValidateBranding_DarkBadgeCompositeContrast (both branches)', () => {
      // Branch 1: custom success.300 fails both dark Shoelace button and dark badge composite contrast
      const res1 = resolveBrandingPalette({ palette: { success: { '300': '#334155' } } });
      const failed1 = res1.checks.filter((c) => !c.pass);
      expect(failed1).toHaveLength(2);
      expect(failed1.every((c) => c.path === 'server.hub.branding.palette.success.300')).toBe(true);
      expect(
        failed1.some((c) => c.message.includes('dark --sl-color-success-600 (step 300)'))
      ).toBe(true);
      expect(failed1.some((c) => c.message.includes('dark badge contrast'))).toBe(true);

      // Branch 2: custom lightish dark.surface flags dark badge contrast on .300
      const res2 = resolveBrandingPalette({
        palette: {
          dark: {
            bg: '#0f172a',
            surface: '#cbd5e1',
            text: '#0f172a',
            text_secondary: '#0f172a',
            text_muted: '#1e293b',
            primary: '#1d4ed8',
            primary_hover: '#1e40af',
            primary_active: '#1e3a8a',
            primary_text: '#ffffff',
          },
          neutral: { '400': '#334155' },
        },
      });
      const failed2 = res2.checks.filter((c) => !c.pass);
      expect(
        failed2.some((c) => c.path.endsWith('.300') && c.message.includes('dark badge contrast'))
      ).toBe(true);
    });

    it('promotes light.primary from grandfathered #3b82f6 to AA-passing primary-600 when primary_text alone is set', () => {
      const res = resolveBrandingPalette({ palette: { light: { primary_text: '#ffffff' } } });
      expect(res.summary.allPass).toBe(true);
      expect(res.light.primary).toBe(defaultPrimaryScale()['600']);
    });

    it('records syntaxErrors without throwing when accentColor is incomplete or invalid', () => {
      const res = resolveBrandingPalette({ accentColor: '#0d9' });
      expect(res.syntaxErrors).toHaveLength(1);
      expect(res.syntaxErrors[0].path).toBe('server.hub.branding.accent_color');
      expect(res.keyPairs.light).toHaveLength(3);
      expect(res.keyPairs.dark).toHaveLength(3);
    });
  });
});
