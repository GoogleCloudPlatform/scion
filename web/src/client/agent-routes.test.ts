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
 * The retired Configure page's URL redirects to the Edit page
 * (ptone/scion#3975), and main.ts no longer routes to or imports the
 * Configure page.
 */

import { describe, expect, it } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

import { legacyAgentRedirect } from './agent-routes.js';

describe('legacyAgentRedirect', () => {
  it('sends /agents/{id}/configure to /agents/{id}/edit', () => {
    expect(legacyAgentRedirect('/agents/a-1/configure')).toBe('/agents/a-1/edit');
    expect(legacyAgentRedirect('/agents/a%201/configure')).toBe('/agents/a%201/edit');
  });

  it('leaves every other path alone', () => {
    for (const p of [
      '/agents/a-1',
      '/agents/a-1/edit',
      '/agents/configure',
      '/agents/a-1/configure/x',
      '/projects/p-1/configure',
    ]) {
      expect(legacyAgentRedirect(p)).toBeNull();
    }
  });

  it('main.ts redirects through it and has no Configure route', () => {
    const dir = path.dirname(fileURLToPath(import.meta.url));
    const main = readFileSync(path.join(dir, 'main.ts'), 'utf-8');
    expect(main).toContain('legacyAgentRedirect(pathname)');
    expect(main).not.toContain('agent-configure');
    expect(main).not.toContain('scion-page-agent-configure');
  });

  it('no source file imports the Configure page', () => {
    const src = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
    const offenders: string[] = [];
    const walk = (dir: string): void => {
      for (const entry of readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, entry.name);
        if (entry.isDirectory()) walk(full);
        else if (/\.ts$/.test(entry.name) && full !== fileURLToPath(import.meta.url)) {
          if (
            /from ['"][^'"]*agent-configure(\.js)?['"]|import\(['"][^'"]*agent-configure/.test(
              readFileSync(full, 'utf-8')
            )
          ) {
            offenders.push(path.relative(src, full));
          }
        }
      }
    };
    walk(src);
    expect(offenders).toEqual([]);
  });
});
