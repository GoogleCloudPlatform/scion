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

import { readFileSync } from 'fs';
import { join } from 'path';
import { describe, expect, it } from 'vitest';

import {
  countCritic,
  criticToolEdit,
  criticToSentinels,
  normalizeCritic,
  onlyMarksChanged,
  parseCritic,
  projectCritic,
  renderCriticSentinels,
} from './critic.js';

interface Case {
  name: string;
  in: string;
  clean: string;
  accept: string;
}

const corpus = (
  JSON.parse(
    readFileSync(join(__dirname, '../../../pkg/artifacts/critic/testdata/corpus.json'), 'utf-8')
  ) as { cases: Case[] }
).cases;

describe('critic corpus shared with the hub', () => {
  it('has the shared cases', () => {
    expect(corpus.length).toBeGreaterThanOrEqual(40);
  });
  for (const c of corpus) {
    it(c.name, () => {
      expect(projectCritic(c.in, 'clean')).toBe(c.clean);
      expect(projectCritic(c.in, 'accept')).toBe(c.accept);
      expect(projectCritic(c.in, 'raw')).toBe(c.in);
      // Segments tile the input.
      let pos = 0;
      for (const s of parseCritic(c.in)) {
        expect(s.start).toBe(pos);
        expect(s.end).toBeGreaterThan(s.start);
        pos = s.end;
      }
      expect(pos).toBe(c.in.length);
    });
  }
});

describe('parseCritic cost', () => {
  // A per-opener rescan of the tail examines O(n^2) characters and fails.
  const n = 1 << 14;
  const inputs: Record<string, string> = {
    'unterminated insertions': '{++'.repeat(n),
    'unterminated each kind': '{++{--{~~{>>{=='.repeat(n / 5),
    'substitutions without separator': '{~~a'.repeat(n) + '~~}',
    'separator far away': '{~~a~~}'.repeat(n) + '~>',
    'openers then one closer': '{++a'.repeat(n) + '++}',
    'malformed then valid': '{~~x~~}{++'.repeat(n / 2),
  };
  for (const [name, src] of Object.entries(inputs)) {
    it(name, () => {
      const steps = { n: 0 };
      parseCritic(src, steps);
      expect(steps.n).toBeLessThanOrEqual(16 * (src.length + 1));
    });
  }
});

describe('review helpers', () => {
  it('counts marks', () => {
    expect(countCritic('{++a++}{--b--}{~~c~>d~~}{>>e<<}{==f==}{>>g<<}')).toEqual({
      suggestions: 3,
      comments: 2,
      highlights: 1,
    });
  });
  it('normalises line endings and NFC', () => {
    expect(normalizeCritic('café\r\nx\ry')).toBe('café\nx\ny');
  });
  it('accepts marks only', () => {
    const base = 'We ship in Q3.\n';
    expect(onlyMarksChanged('We {~~ship~>launch~~} in Q3.{>>ok?<<}\n', base)).toBe(true);
    expect(onlyMarksChanged('We ship in Q4.\n', base)).toBe(false);
    expect(onlyMarksChanged('We ship in Q3.\r\n', base)).toBe(true);
  });
});

describe('mark rendering sentinels', () => {
  it('renders marks as elements and numbers comments', () => {
    const html = renderCriticSentinels(
      criticToSentinels('a {++b++} {--c--} {~~d~>e~~} {==f==}{>>g<<} {>>h<<}')
    );
    expect(html).toBe(
      'a <ins class=critic-ins>b</ins> <del class=critic-del>c</del> ' +
        '<del class=critic-del>d</del><ins class=critic-ins>e</ins> ' +
        '<mark class=critic-hl>f</mark><sup class=critic-ref>1</sup><span class=critic-note>' +
        '<span class=critic-note-n>1</span> g</span> <sup class=critic-ref>2</sup>' +
        '<span class=critic-note><span class=critic-note-n>2</span> h</span>'
    );
  });
  it('does not let text pose as a mark', () => {
    const out = criticToSentinels('x\uE000y\uE006z');
    expect(out).toBe('x\uFFFDy\uFFFDz');
    expect(renderCriticSentinels(out)).not.toContain('<ins');
  });
});

describe('criticToolEdit', () => {
  const apply = (src: string, e: ReturnType<typeof criticToolEdit>): [string, string] => {
    if (!e) return [src, ''];
    const out = src.slice(0, e.from) + e.insert + src.slice(e.to);
    return [out, out.slice(e.selectFrom, e.selectTo)];
  };
  const src = 'We ship in Q3.';
  const sel = { from: 3, to: 7, text: 'ship' };
  const cursor = { from: 3, to: 3, text: '' };
  it('comments on a selection and leaves the comment selected', () => {
    expect(apply(src, criticToolEdit('comment', sel))).toEqual([
      'We {==ship==}{>>comment<<} in Q3.',
      'comment',
    ]);
    expect(apply(src, criticToolEdit('comment', cursor))).toEqual([
      'We {>>comment<<}ship in Q3.',
      'comment',
    ]);
  });
  it('suggests a replacement with the new side selected', () => {
    expect(apply(src, criticToolEdit('suggest', sel))).toEqual([
      'We {~~ship~>ship~~} in Q3.',
      'ship',
    ]);
    expect(apply(src, criticToolEdit('suggest', cursor))[1]).toBe('replacement');
  });
  it('inserts after the selection', () => {
    expect(apply(src, criticToolEdit('insert', sel))).toEqual(['We ship{++text++} in Q3.', 'text']);
  });
  it('deletes a selection and needs one', () => {
    expect(apply(src, criticToolEdit('delete', sel))[0]).toBe('We {--ship--} in Q3.');
    expect(criticToolEdit('delete', cursor)).toBeNull();
  });
  it('keeps the baseline: every toolbar mark is rejected by clean', () => {
    for (const tool of ['comment', 'suggest', 'insert', 'delete'] as const) {
      const [out] = apply(src, criticToolEdit(tool, sel));
      expect(onlyMarksChanged(out, src)).toBe(true);
    }
  });
});
