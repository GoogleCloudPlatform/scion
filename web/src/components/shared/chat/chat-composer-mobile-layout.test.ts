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
 * The phone composer's input row: attach and send are both full 44px
 * targets at either end of the field, and the row's horizontal spacing
 * does not change with the keyboard (a change would rewrap the draft as
 * the frame crosses the tight threshold, moving the field). These are
 * static style contracts, not geometry, rendering or device evidence.
 */

import { describe, it, expect, vi, beforeAll } from 'vitest';
import { elementStyleRules } from '../../pages/__fixtures__/css-rules.js';

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const MOBILE = '@media (max-width: 768px)';

let rules: Map<string, string>;

beforeAll(async () => {
  await import('./chat-composer.js');
  rules = elementStyleRules('scion-chat-composer');
});

function decl(body: string | undefined, prop: string): string | undefined {
  if (!body) return undefined;
  let value: string | undefined;
  for (const part of body.split(';')) {
    const i = part.indexOf(':');
    if (i < 0) continue;
    if (part.slice(0, i).trim() === prop) value = part.slice(i + 1).trim();
  }
  return value;
}

/** Split a value on top-level whitespace, keeping var()/calc() groups whole. */
function splitTopLevel(value: string): string[] {
  const parts: string[] = [];
  let depth = 0;
  let cur = '';
  for (const ch of value.trim()) {
    if (ch === '(') depth++;
    else if (ch === ')') depth--;
    if (/\s/.test(ch) && depth === 0) {
      if (cur) parts.push(cur);
      cur = '';
    } else {
      cur += ch;
    }
  }
  if (cur) parts.push(cur);
  return parts;
}

/** The inline (left/right) values of a padding shorthand. */
function paddingInline(value: string): string[] {
  const v = splitTopLevel(value);
  if (v.length === 1) return [v[0]!];
  if (v.length <= 3) return [v[1]!];
  return [v[1]!, v[3]!];
}

describe('phone composer — input row targets', () => {
  it('sizes attach as a 44px square, matching send', () => {
    const attach = rules.get(`${MOBILE} .attach-btn::part(base)`);
    const send = rules.get(`${MOBILE} .send-btn::part(base)`);
    for (const prop of ['width', 'height']) {
      expect(parseFloat(decl(attach, prop) ?? '0'), `attach ${prop}`).toBeGreaterThanOrEqual(44);
      expect(decl(attach, prop), prop).toBe(decl(send, prop));
    }
  });

  it('keeps the composer gutters and input-row spacing independent of the keyboard', () => {
    const keyboardTokens = /--scion-(kb|chat-kb|chat-tight)/;
    const inline = ['padding-left', 'padding-right', 'padding-inline', 'gap'];
    let shorthands = 0;
    for (const [key, body] of rules) {
      if (!/\.(composer|input-row)$/.test(key)) continue;
      for (const prop of inline) {
        expect(decl(body, prop) ?? '', `${key} ${prop}`).not.toMatch(keyboardTokens);
      }
      // The padding shorthand's block half may follow the tight frame
      // (--composer-pad-block); its inline half must not.
      const padding = decl(body, 'padding');
      if (padding) {
        shorthands++;
        for (const value of paddingInline(padding)) {
          expect(value, `${key} padding inline`).not.toMatch(keyboardTokens);
        }
      }
    }
    // The base .composer rule uses the shorthand, so this part is exercised.
    expect(shorthands).toBeGreaterThan(0);
  });

  it('reads the inline half of a padding shorthand', () => {
    expect(paddingInline('1rem')).toEqual(['1rem']);
    expect(paddingInline('var(--composer-pad-block) 1rem')).toEqual(['1rem']);
    expect(paddingInline('0 calc(1rem - 0.25rem * var(--scion-chat-tight, 0)) 2px')).toEqual([
      'calc(1rem - 0.25rem * var(--scion-chat-tight, 0))',
    ]);
    expect(paddingInline('1px 2px 3px 4px')).toEqual(['2px', '4px']);
  });
});
