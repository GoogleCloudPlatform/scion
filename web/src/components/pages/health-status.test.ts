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
 * Status tones, and the contrast of the pill and text token pairs the
 * health dashboard uses, in the light and dark themes.
 */

import { describe, it, expect } from 'vitest';

import { healthTone } from './health-status.js';
import { contrast, over, resolver, themeTokens } from './__fixtures__/theme-contrast.js';

describe('healthTone', () => {
  it('maps status words to tones', () => {
    expect(healthTone('healthy')).toBe('ok');
    expect(healthTone('online')).toBe('ok');
    expect(healthTone('degraded')).toBe('warn');
    expect(healthTone('unhealthy')).toBe('bad');
    expect(healthTone('offline')).toBe('bad');
    expect(healthTone('unknown')).toBe('neutral');
    expect(healthTone('')).toBe('neutral');
    expect(healthTone(undefined)).toBe('neutral');
  });

  it('reads only the word before a fixed reason', () => {
    expect(healthTone('unhealthy: registration pending')).toBe('bad');
    expect(healthTone('unavailable: could not compare filesystem device IDs')).toBe('bad');
  });
});

// The overall status pill sits on the page background; the section pills
// and text sit on the card surface. Check WCAG AA (4.5:1) on both, in both
// themes. Translucent badge backgrounds are composited first.
describe('health dashboard contrast', () => {
  const { light, dark } = themeTokens();
  const pairs: Array<[string, string | null]> = [
    ['--scion-text', null],
    ['--scion-text-muted', null],
    ['--scion-badge-success-text', '--scion-badge-success-bg'],
    ['--scion-badge-warning-text', '--scion-badge-warning-bg'],
    ['--scion-badge-danger-text', '--scion-badge-danger-bg'],
    ['--scion-badge-neutral-text', '--scion-badge-neutral-bg'],
  ];

  for (const [theme, vars] of [
    ['light', light],
    ['dark', dark],
  ] as const) {
    for (const base of ['--scion-bg', '--scion-surface']) {
      it(`passes AA on ${base} in the ${theme} theme`, () => {
        const get = resolver(vars);
        const ground = get(base);
        for (const [fg, bg] of pairs) {
          const back = bg ? over(get(bg), ground) : ground;
          const ratio = contrast(over(get(fg), back), back);
          expect(ratio, `${fg} on ${bg ?? base}`).toBeGreaterThanOrEqual(4.5);
        }
      });
    }
  }
});
