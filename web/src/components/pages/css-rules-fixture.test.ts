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

import { describe, expect, it } from 'vitest';
import { styleRules } from './__fixtures__/css-rules.js';

describe('styleRules fixture', () => {
  it('keys @media rules separately from top-level rules with the same selector', () => {
    const rules = styleRules(`
      .card { display: grid; }
      @media (max-width:  600px) {
        .card { display: block; }
      }
    `);
    expect(rules.get('.card')).toContain('display: grid');
    expect(rules.get('@media (max-width: 600px) .card')).toContain('display: block');
  });

  it('keys rules by the full at-rule chain', () => {
    const rules = styleRules(`
      @supports (display: grid) { @media (min-width: 1px) { .a { color: red; } } }
      @media (min-width: 1px) { .a { color: blue; } }
    `);
    expect(rules.get('@supports (display: grid) @media (min-width: 1px) .a')).toContain('red');
    expect(rules.get('@media (min-width: 1px) .a')).toContain('blue');
    expect(rules.has('.a')).toBe(false);
  });

  it('splits selector lists into one entry per selector', () => {
    const rules = styleRules('.a,\n  .b { gap: 4px; }');
    expect(rules.get('.a')).toContain('gap: 4px');
    expect(rules.get('.b')).toContain('gap: 4px');
  });

  it('merges a repeated top-level selector in source order', () => {
    const rule = styleRules('.a { color: red; } .b, .a { color: blue; }').get('.a') ?? '';
    expect(rule).toContain('red');
    expect(rule).toContain('blue');
    expect(rule.indexOf('red')).toBeLessThan(rule.indexOf('blue'));
  });

  it('merges a repeated selector inside the same at-rule in source order', () => {
    const rules = styleRules(
      '.a { color: green; } @media (x) { .a { color: red; } } @media (x) { .a { color: blue; } }'
    );
    const rule = rules.get('@media (x) .a') ?? '';
    expect(rule.indexOf('red')).toBeGreaterThanOrEqual(0);
    expect(rule.indexOf('red')).toBeLessThan(rule.indexOf('blue'));
    expect(rule).not.toContain('green');
    expect(rules.get('.a')).not.toContain('red');
  });

  it('separates merged bodies when the earlier one has no trailing semicolon', () => {
    const rule = styleRules('.a, .b { color: red } .a { min-width: 0 }').get('.a') ?? '';
    expect(rule).toMatch(/(^|;)\s*color:\s*red\s*;/);
    expect(rule).toMatch(/(^|;)\s*min-width:\s*0/);
  });

  it('ignores comments', () => {
    const rules = styleRules('/* .a { color: red; } */ .b { color: blue; }');
    expect(rules.has('.a')).toBe(false);
    expect(rules.get('.b')).toContain('blue');
  });
});
