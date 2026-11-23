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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import {
  evaluateScaleStepBadge,
  evaluateSemanticTokenBadge,
  type PaletteOpenChangeDetail,
  type PaletteScaleChangeDetail,
  type PaletteScaleClearDetail,
  type PaletteSemanticChangeDetail,
  type ScionBrandingPaletteEditor,
} from './admin-branding-palette.js';
import {
  BRANDING_KOANF_PREFIX,
  defaultPaletteFormState,
  paletteFromConfig,
  paletteFormToRawConfig,
} from './admin-branding-state.js';
import {
  SCALE_NAMES,
  SCALE_STEPS,
  SEMANTIC_KEYS,
  resolveBrandingPalette,
} from './admin-branding-contrast.js';

function query<T extends Element = Element>(el: HTMLElement, selector: string): T | null {
  return (el.shadowRoot?.querySelector(selector) as T | null) ?? null;
}

describe('admin-branding-palette', () => {
  let element: ScionBrandingPaletteEditor | null = null;

  beforeAll(async () => {
    await import('./admin-branding-palette.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
  });

  describe('evaluateSemanticTokenBadge & evaluateScaleStepBadge', () => {
    it('reports unenforced stock ratios when palette is uncustomized', () => {
      const resolved = resolveBrandingPalette({});
      const lightPrimary = evaluateSemanticTokenBadge('light', 'primary', resolved);
      expect(lightPrimary.enforced).toBe(false);
      expect(lightPrimary.ratio).toBeCloseTo(3.68, 2);

      const darkText = evaluateSemanticTokenBadge('dark', 'text', resolved);
      expect(darkText.enforced).toBe(false);
      expect(darkText.pass).toBe(true);

      expect(evaluateScaleStepBadge('primary', '600', resolved)).toBeNull();
    });

    it('reports enforced AA Fail and AA Pass consistent with server checks', () => {
      const failing = resolveBrandingPalette({ palette: { light: { primary: '#eab308' } } });
      const failBadge = evaluateSemanticTokenBadge('light', 'primary', failing);
      expect(failBadge.enforced).toBe(true);
      expect(failBadge.pass).toBe(false);
      expect(failBadge.ratio).toBeLessThan(4.5);

      const passing = resolveBrandingPalette({ accentColor: '#0d9488' });
      const passBadge = evaluateSemanticTokenBadge('light', 'primary', passing);
      expect(passBadge.enforced).toBe(true);
      expect(passBadge.pass).toBe(true);
      expect(passBadge.ratio).toBeGreaterThanOrEqual(4.5);

      const scaleFail = resolveBrandingPalette({ palette: { primary: { '600': '#eab308' } } });
      const stepBadge = evaluateScaleStepBadge('primary', '600', scaleFail);
      expect(stepBadge).not.toBeNull();
      expect(stepBadge!.enforced).toBe(true);
      expect(stepBadge!.pass).toBe(false);
      expect(evaluateScaleStepBadge('primary', '50', scaleFail)).toBeNull();
    });
  });

  describe('<scion-branding-palette-editor>', () => {
    async function mountEditor(
      props: Partial<ScionBrandingPaletteEditor> = {}
    ): Promise<ScionBrandingPaletteEditor> {
      const el = document.createElement(
        'scion-branding-palette-editor'
      ) as ScionBrandingPaletteEditor;
      Object.assign(el, props);
      document.body.appendChild(el);
      await el.updateComplete;
      return el;
    }

    it('renders all 24 semantic tokens and 55 shade scale steps with distinct accessible labels', async () => {
      const palette = defaultPaletteFormState();
      const resolved = resolveBrandingPalette({});
      element = await mountEditor({ palette, resolved, open: true });

      const details = query(element, '[data-advanced-palette-details]')!;
      expect(details.hasAttribute('open')).toBe(true);

      const semanticInputs = element.shadowRoot!.querySelectorAll('[data-palette-field]');
      expect(semanticInputs.length).toBe(SEMANTIC_KEYS.length * 2);

      const scaleInputs = element.shadowRoot!.querySelectorAll('[data-palette-scale]');
      expect(scaleInputs.length).toBe(SCALE_NAMES.length * SCALE_STEPS.length);

      // Every sl-input has a unique non-empty label attribute
      const labels = Array.from(element.shadowRoot!.querySelectorAll('sl-input[label]')).map((el) =>
        el.getAttribute('label')
      );
      expect(labels.length).toBe(
        SEMANTIC_KEYS.length * 2 + SCALE_NAMES.length * SCALE_STEPS.length
      );
      expect(new Set(labels).size).toBe(labels.length);
    });

    it('emits semantic change and per-key reset events', async () => {
      const palette = paletteFromConfig({ light: { primary: '#0f766e' } });
      const resolved = resolveBrandingPalette({ palette: paletteFormToRawConfig(palette) });
      element = await mountEditor({ palette, resolved, open: true });

      const changes: PaletteSemanticChangeDetail[] = [];
      element.addEventListener('palette-semantic-change', (e: Event) => {
        changes.push((e as CustomEvent<PaletteSemanticChangeDetail>).detail);
      });

      expect(query(element, '[data-palette-override-count]')?.textContent).toContain('1 override');

      const input = query<HTMLInputElement>(element, '[data-palette-field="light.primary"]')!;
      input.value = '#115e59';
      input.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));
      expect(changes[0]).toEqual({ mode: 'light', key: 'primary', value: '#115e59' });

      const resetBtn = query<HTMLElement>(element, '[data-reset-token="light.primary"]')!;
      resetBtn.click();
      expect(changes[1]).toEqual({ mode: 'light', key: 'primary', value: '' });
    });

    it('emits scale step change, per-step reset, and per-ramp clear events', async () => {
      const palette = paletteFromConfig({ primary: { '600': '#007268' } });
      const resolved = resolveBrandingPalette({ palette: paletteFormToRawConfig(palette) });
      element = await mountEditor({ palette, resolved, open: true });

      const stepChanges: PaletteScaleChangeDetail[] = [];
      const scaleClears: PaletteScaleClearDetail[] = [];
      element.addEventListener('palette-scale-change', (e: Event) => {
        stepChanges.push((e as CustomEvent<PaletteScaleChangeDetail>).detail);
      });
      element.addEventListener('palette-scale-clear', (e: Event) => {
        scaleClears.push((e as CustomEvent<PaletteScaleClearDetail>).detail);
      });

      const stepInput = query<HTMLInputElement>(element, '[data-palette-scale="primary.600"]')!;
      stepInput.value = '#0f766e';
      stepInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));
      expect(stepChanges[0]).toEqual({ scale: 'primary', step: '600', value: '#0f766e' });

      const resetStepBtn = query<HTMLElement>(element, '[data-reset-step="primary.600"]')!;
      resetStepBtn.click();
      expect(stepChanges[1]).toEqual({ scale: 'primary', step: '600', value: '' });

      const clearBtn = query<HTMLElement>(element, '[data-clear-scale="primary"]')!;
      expect(clearBtn.hasAttribute('disabled')).toBe(false);
      clearBtn.click();
      expect(scaleClears).toEqual([{ scale: 'primary' }]);
    });

    it('auto-opens nested scale details when errorScales includes that scale and renders inline errors', async () => {
      const palette = paletteFromConfig({ primary: { '600': '#eab308' } });
      const resolved = resolveBrandingPalette({ palette: paletteFormToRawConfig(palette) });
      element = await mountEditor({
        palette,
        resolved,
        open: true,
        errorScales: new Set(['primary']),
        serverFieldErrors: {
          'palette.primary.600': ['contrast ratio below 4.50:1'],
        },
      });

      expect(query(element, '[data-scale-details="primary"]')?.hasAttribute('open')).toBe(true);
      expect(query(element, '[data-scale-details="neutral"]')?.hasAttribute('open')).toBe(false);
      expect(query(element, '[data-palette-error-count]')?.textContent).toContain('1 error');
      expect(query(element, '[data-field-error="palette.primary.600"]')?.textContent).toContain(
        '4.50:1'
      );
      expect(query(element, '[data-token-contrast="primary.600"]')?.textContent).toContain(
        'AA Fail'
      );
    });

    it('renders env-locked palette keys read-only without editable inputs', async () => {
      const palette = paletteFromConfig({ light: { primary: '#1d4ed8' } });
      const resolved = resolveBrandingPalette({ palette: paletteFormToRawConfig(palette) });
      const lockKey = `${BRANDING_KOANF_PREFIX}.palette.light.primary`;
      element = await mountEditor({
        palette,
        resolved,
        open: true,
        envLockedKeys: new Set([lockKey]),
        envOverrides: [lockKey],
        envValues: {
          [lockKey]: { value: '#0f766e', in_effect: true, saved_differs: true },
        },
        settingsTier: 'db',
      });

      expect(query(element, '[data-palette-field="light.primary"]')).toBeNull();
      expect(query(element, '[data-reset-token="light.primary"]')).toBeNull();
      const card = query(element, '[data-palette-token="light.primary"]')!;
      expect(card.textContent).toContain('#0f766e');
      expect(card.textContent).toContain('Set via environment variable');
    });

    it('does not bubble nested scale sl-show/sl-hide events as outer palette-open-change', async () => {
      element = await mountEditor({ open: true });
      const openEvents: PaletteOpenChangeDetail[] = [];
      const listener = vi.fn((e: Event) => {
        openEvents.push((e as CustomEvent<PaletteOpenChangeDetail>).detail);
      });
      element.addEventListener('palette-open-change', listener);

      const nested = query(element, '[data-scale-details="primary"]')!;
      nested.dispatchEvent(new CustomEvent('sl-hide', { bubbles: true }));
      expect(listener).not.toHaveBeenCalled();

      const outer = query(element, '[data-advanced-palette-details]')!;
      outer.dispatchEvent(new CustomEvent('sl-hide', { bubbles: true }));
      expect(openEvents).toEqual([{ open: false }]);
    });
  });
});
