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
 * The SPA shell loads no Shoelace CDN autoloader, so every sl-* element the
 * app renders must be registered by an import in main.ts. A tag used in
 * src/ without that import renders as an unknown, unstyled element in a
 * production build.
 */

import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';

/** `web/src`, relative to this file (`web/src/client/`). */
const srcDir = resolve(__dirname, '..');
const mainPath = resolve(__dirname, 'main.ts');

function sourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) {
      out.push(...sourceFiles(path));
    } else if (/\.ts$/.test(name) && !/\.test\.ts$/.test(name)) {
      out.push(path);
    }
  }
  return out;
}

/** sl-* tags rendered by app code: template tags and createElement calls. */
function usedTags(): Map<string, string> {
  const used = new Map<string, string>();
  for (const file of sourceFiles(srcDir)) {
    const text = readFileSync(file, 'utf8');
    for (const m of text.matchAll(/<(sl-[a-z][a-z-]*)[\s>/]/g)) {
      if (!used.has(m[1])) used.set(m[1], relative(srcDir, file));
    }
    for (const m of text.matchAll(/createElement\(\s*['"](sl-[a-z][a-z-]*)['"]/g)) {
      if (!used.has(m[1])) used.set(m[1], relative(srcDir, file));
    }
  }
  return used;
}

/** Components main.ts registers by import. */
function registeredTags(): Set<string> {
  const text = readFileSync(mainPath, 'utf8');
  const tags = new Set<string>();
  for (const m of text.matchAll(
    /import '@shoelace-style\/shoelace\/dist\/components\/([a-z-]+)\/\1\.js';/g
  )) {
    tags.add(`sl-${m[1]}`);
  }
  return tags;
}

describe('Shoelace component registration', () => {
  it('finds the tags and registrations it checks (sanity)', () => {
    expect(usedTags().size).toBeGreaterThan(20);
    expect(registeredTags().has('sl-button')).toBe(true);
  });

  it('registers every sl-* tag used in src/ in main.ts', () => {
    const registered = registeredTags();
    const missing = [...usedTags()]
      .filter(([tag]) => !registered.has(tag))
      .map(([tag, file]) => `${tag} (first used in ${file})`);
    expect(missing).toEqual([]);
  });
});
