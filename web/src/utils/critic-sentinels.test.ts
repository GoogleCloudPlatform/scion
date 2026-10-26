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

import {
  SENTINEL_CANDIDATES,
  chooseAlphabet,
  chooseOpeningAlphabet,
  criticToSentinelText,
  readTag,
  sentinelSet,
  sentinelTextToCritic,
} from './critic-sentinels.js';

describe('sentinel alphabet', () => {
  it('draws only from Unicode Po punctuation, without repeats', () => {
    for (const c of SENTINEL_CANDIDATES) expect(/^\p{Po}$/u.test(c), c).toBe(true);
    expect(new Set(SENTINEL_CANDIDATES).size).toBe(SENTINEL_CANDIDATES.length);
  });

  it('skips every candidate the document contains', () => {
    const used = SENTINEL_CANDIDATES.slice(0, 12).join('');
    const a = chooseAlphabet(`text ${used} more`)!;
    for (const c of sentinelSet(a)) expect(used.includes(c)).toBe(false);
    expect(sentinelSet(a).size).toBe(12);
  });

  it('gives no alphabet when fewer than twelve candidates are free', () => {
    const n = SENTINEL_CANDIDATES.length;
    expect(chooseAlphabet(SENTINEL_CANDIDATES.slice(0, n - 11).join(''))).toBeNull();
    // Positive control: one more free candidate is enough.
    expect(chooseAlphabet(SENTINEL_CANDIDATES.slice(0, n - 12).join(''))).not.toBeNull();
  });
});

describe('opening alphabet (probe)', () => {
  const C = SENTINEL_CANDIDATES;

  it('excludes what the parser yields from the probe form, when the source has no candidate', () => {
    // A stand-in parser that decodes one reference, as Markdown would in
    // text a note hid in the source.
    const shown = (t: string): string => t.replace('&#x2E00;', C[0]);
    const md = '{>><b title="<<} &#x2E00; ">';
    expect(md.split('').some((c) => C.includes(c))).toBe(false);
    const a = chooseOpeningAlphabet(md, shown)!;
    expect(sentinelSet(a).has(C[0])).toBe(false);
    expect(a.insOpen).toBe(C[1]);
    // Positive control: without the decoded reference, the first candidate is used.
    expect(chooseOpeningAlphabet(md, (t) => t)!.insOpen).toBe(C[0]);
  });

  it('builds the probe form from the last twelve free candidates, which a decoded probe character cannot reach', () => {
    const probe = C.slice(-12);
    let probeText = '';
    const shown = (t: string): string => {
      probeText = t;
      return t.replace('&#x2CFF;', C.at(-1)!);
    };
    const md = 'a {>>n<<} &#x2CFF; {++b++}';
    const a = chooseOpeningAlphabet(md, shown)!;
    // The probe form uses only probe characters for the marks.
    expect(probeText).toContain(probe[0]);
    expect(probeText).toContain(probe[9]);
    // The probe form is tagged: the note's ordinal 0 follows its sentinel.
    expect(probeText).toContain(probe[9] + probe[10]);
    for (const c of sentinelSet(a)) expect(probe.includes(c)).toBe(false);
    expect(a.insOpen).toBe(C[0]);
  });

  it('needs twenty-four free candidates', () => {
    expect(chooseOpeningAlphabet(C.slice(0, C.length - 24).join(''), (t) => t)).not.toBeNull();
    expect(chooseOpeningAlphabet(C.slice(0, C.length - 23).join(''), (t) => t)).toBeNull();
  });
});

describe('tagged sentinel text', () => {
  it('follows each note sentinel with its ordinal in binary, standing for no source text', () => {
    const md = 'a {>>one<<} b {>>two<<} c {>>three<<}';
    const a = chooseAlphabet(md)!;
    const s = criticToSentinelText(md, a, true);
    const tags = (t: string): number[] => {
      const out: number[] = [];
      for (let i = 0; i < t.length; i++) if (t[i] === a.note) out.push(readTag(t, i, a)!.ordinal);
      return out;
    };
    expect(tags(s.text)).toEqual([0, 1, 2]);
    expect(s.text).toContain(a.note + a.tag1 + a.tag0);
    for (let i = 0; i < s.text.length; i++) {
      if (s.text[i] === a.tag0 || s.text[i] === a.tag1) expect(s.starts[i]).toBe(s.ends[i]);
    }
    // Untagged by default.
    expect(criticToSentinelText(md, a).text.includes(a.tag0)).toBe(false);
    expect(readTag('x', 0, a)).toBeNull();
  });
});

describe('sentinel text', () => {
  const md = 'a {++in++} {--out--} {~~old~>new~~} {==hl==}{>>a *note*<<} `{++code++}`';

  it('round-trips, keeping notes aside and code literal', () => {
    const a = chooseAlphabet(md)!;
    const s = criticToSentinelText(md, a);
    expect(s.notes).toEqual(['a *note*']);
    expect(s.text).not.toContain('{++in');
    expect(s.text).toContain('`{++code++}`');
    expect(s.text.includes('*note*')).toBe(false);
    expect(sentinelTextToCritic(s.text, s.notes, a)).toBe(md);
  });

  it('maps every sentinel-text character to the source span it stands for', () => {
    const a = chooseAlphabet(md)!;
    const s = criticToSentinelText(md, a);
    expect(s.starts.length).toBe(s.text.length);
    for (let i = 0; i < s.text.length; i++) {
      const c = s.text[i];
      const span = md.slice(s.starts[i], s.ends[i]);
      if (!sentinelSet(a).has(c)) expect(span).toBe(c);
    }
    const note = s.text.indexOf(a.note);
    expect(md.slice(s.starts[note], s.ends[note])).toBe('{>>a *note*<<}');
  });
});
