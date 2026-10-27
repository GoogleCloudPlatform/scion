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
 * The bundle check's marker strings for the review editor
 * (bundle-budget.json requiredStrings) prove a module is in the built
 * chunk only if no other source file holds the same string: each must
 * occur in exactly one non-test source file, the module it stands for.
 */

import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { describe, expect, it } from 'vitest';

const WEB = join(__dirname, '../../..');
const SRC = join(WEB, 'src');

/** The module each marker stands for. */
const MARKER_MODULES: Record<string, string> = {
  hardened: 'src/components/artifact-review/editor/safe-render.ts',
  'image-placeholder failed': 'src/components/artifact-review/editor/safe-render.ts',
  'inert-link': 'src/components/artifact-review/editor/safe-render.ts',
  criticPaste: 'src/components/artifact-review/editor/paste.ts',
  criticPolicy: 'src/components/artifact-review/editor/policy.ts',
  criticTrack: 'src/components/artifact-review/editor/track.ts',
};

function sourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) {
      if (name !== '__fixtures__' && name !== 'node_modules') out.push(...sourceFiles(p));
    } else if (/\.(ts|js|mjs|css)$/.test(name) && !/\.test\.ts$/.test(name)) {
      out.push(p);
    }
  }
  return out;
}

describe('review editor bundle markers', () => {
  const budget = JSON.parse(readFileSync(join(WEB, 'bundle-budget.json'), 'utf-8')) as {
    requiredStrings: Record<string, string[]>;
  };
  const markers = budget.requiredStrings['assets/review-editor-'];
  const files = sourceFiles(SRC).map((f) => ({
    path: relative(WEB, f),
    text: readFileSync(f, 'utf-8'),
  }));

  it('lists a module for every marker, and a marker for every listed module', () => {
    expect([...markers].sort()).toEqual(Object.keys(MARKER_MODULES).sort());
  });

  it('finds each marker in exactly one source file, its module', () => {
    expect(files.length).toBeGreaterThan(100);
    for (const m of markers) {
      const holders = files.filter((f) => f.text.includes(m)).map((f) => f.path);
      expect({ marker: m, holders }).toEqual({ marker: m, holders: [MARKER_MODULES[m]] });
    }
  });
});
