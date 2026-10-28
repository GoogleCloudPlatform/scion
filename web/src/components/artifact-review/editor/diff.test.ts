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

import { apply, diff, rebase } from './diff.js';

/** A small deterministic generator, so failures reproduce. */
function rng(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0;
    return s / 2 ** 32;
  };
}

function randomText(r: () => number, n: number, alphabet = 'ab c\n-*'): string {
  let out = '';
  for (let i = 0; i < n; i++) out += alphabet[Math.floor(r() * alphabet.length)];
  return out;
}

describe('diff', () => {
  it('turns a into b, for random pairs', () => {
    const r = rng(7);
    for (let i = 0; i < 300; i++) {
      const a = randomText(r, Math.floor(r() * 40));
      const b = randomText(r, Math.floor(r() * 40));
      const edits = diff(a, b)!;
      expect(apply(a, edits)).toBe(b);
      for (let k = 1; k < edits.length; k++)
        expect(edits[k].from).toBeGreaterThan(edits[k - 1].to - 1);
    }
  });

  it('finds the edit alone in a long text', () => {
    const a = 'x'.repeat(5000) + 'abc' + 'y'.repeat(5000);
    const b = 'x'.repeat(5000) + 'a{++Z++}bc' + 'y'.repeat(5000);
    expect(diff(a, b)).toEqual([{ from: 5001, to: 5001, insert: '{++Z++}' }]);
  });

  it('gives up at once when the lengths alone differ by more than the cap', () => {
    expect(diff(`${'x'.repeat(3000)}y`, 'zz', 1000)).toBeNull();
    expect(diff(`${'x'.repeat(30)}y`, 'zz', 1000)).not.toBeNull();
  });

  it('takes a diff of the source computed earlier', () => {
    const original = '* one\n* two';
    const base = '- one\n- two';
    const toOriginal = diff(original, base);
    expect(rebase(original, base, '- one\n- t{++X++}wo', toOriginal)).toBe('* one\n* t{++X++}wo');
    expect(rebase(original, base, '- one\n- t{++X++}wo', null)).toBeNull();
  });

  it('gives up past the cap', () => {
    const r = rng(3);
    const a = randomText(r, 400, 'abcdefgh');
    const b = randomText(r, 400, 'ijklmnop');
    expect(diff(a, b, 50)).toBeNull();
    expect(apply(a, diff(a, b)!)).toBe(b);
  });
});

describe('rebase (three-way patch)', () => {
  it('lands an edit in the source when the serializer writes another bullet', () => {
    const original = '* one\n* two';
    const base = '- one\n- two';
    const now = '- one\n- t{++X++}wo';
    expect(rebase(original, base, now)).toBe('* one\n* t{++X++}wo');
  });

  it('keeps the source numbering of an ordered list', () => {
    expect(rebase('3. alpha', '1. alpha', '1. al{--ph--}a')).toBe('3. al{--ph--}a');
  });

  it('keeps a backslash the serializer adds or drops', () => {
    // The serializer puts a backslash before a tilde the source left bare.
    expect(rebase('a ~ b c', 'a \\~ b c', 'a \\~ b {++X++}c')).toBe('a ~ b {++X++}c');
    // An insertion right after the backslashed character.
    expect(rebase('a ~ b', 'a \\~ b', 'a \\~{++X++} b')).toBe('a ~{++X++} b');
  });

  it('keeps table padding the serializer does not write', () => {
    const original = '| a    | bb |';
    const base = '| a | bb |';
    const now = '| a{++Z++} | bb |';
    expect(rebase(original, base, now)).toBe('| a{++Z++}    | bb |');
  });

  it('returns the source unchanged when there is no edit, and now when nothing is normalized', () => {
    expect(rebase('* a', '- a', '- a')).toBe('* a');
    expect(rebase('- a', '- a', '- {++b++}a')).toBe('- {++b++}a');
  });

  it('does not apply an edit that starts inside text written differently from the source', () => {
    // The serializer wrote "***" where the source has "___": an insertion
    // between the stars has no exact place in the source.
    expect(rebase('___', '***', '*{++X++}**')).toBeNull();
  });

  it('does not apply when a diff is too large', () => {
    const r = rng(11);
    const original = randomText(r, 6000, 'abcdefgh');
    const base = randomText(r, 6000, 'ijklmnop');
    expect(rebase(original, base, `${base}x`)).toBeNull();
  });
});
