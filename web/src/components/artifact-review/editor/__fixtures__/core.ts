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

/** Test helpers: a real editor core in happy-dom, and the shared corpus. */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { createCore } from '../create.js';
import type { Core, CoreOptions } from '../create.js';
import type { RenderContext } from '../safe-render.js';

export interface CorpusCase {
  name: string;
  in: string;
  clean: string;
  accept: string;
}

export const corpus: CorpusCase[] = (
  JSON.parse(
    readFileSync(
      join(__dirname, '../../../../../../pkg/artifacts/critic/testdata/corpus.json'),
      'utf-8'
    )
  ) as { cases: CorpusCase[] }
).cases;

export const renderContext: RenderContext = {
  id: 'art1',
  seq: 3,
  entryPath: 'docs/index.md',
  files: [
    { path: 'docs/index.md', size: 10, sha256: 'a', mediaType: 'text/markdown' },
    { path: 'docs/img/dot.png', size: 10, sha256: 'b', mediaType: 'image/png' },
    { path: 'docs/notes.md', size: 10, sha256: 'c', mediaType: 'text/markdown' },
    {
      path: '_remote/1/pic.png',
      size: 10,
      sha256: 'd',
      mediaType: 'image/png',
      origin: 'remote',
      sourceUrl: 'https://img.example/pic.png',
      fetchStatus: 'ok',
    },
  ],
};

const open: Core[] = [];

/** A core mounted in the document; destroyed by destroyCores. */
export async function makeCore(extra: Partial<CoreOptions> = {}): Promise<Core> {
  const root = document.createElement('div');
  document.body.append(root);
  const core = await createCore({ root, render: () => renderContext, ...extra });
  open.push(core);
  return core;
}

export async function destroyCores(): Promise<void> {
  for (const c of open.splice(0)) await c.destroy();
  document.body.replaceChildren();
}

/** A document of the edit sweep's own fixture set. */
export interface SweepDoc {
  name: string;
  in: string;
}

/** Lists, tables and normalized syntax the shared corpus lacks (sweep-docs.json). */
export const sweepDocs: SweepDoc[] = (
  JSON.parse(readFileSync(join(__dirname, 'sweep-docs.json'), 'utf-8')) as { cases: SweepDoc[] }
).cases;
