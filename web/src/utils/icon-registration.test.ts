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
 * Every Shoelace icon the source names literally must be registered in the
 * copy script, which ships only the listed icons: one it misses renders in
 * development (served from node_modules) but is blank in a production build.
 */

import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, resolve } from 'node:path';

const WEB_ROOT = resolve(__dirname, '../..');

function registeredIcons(): Set<string> {
  const script = readFileSync(join(WEB_ROOT, 'scripts/copy-shoelace-icons.mjs'), 'utf8');
  const start = script.indexOf('const USED_ICONS');
  const list = script.slice(start, script.indexOf('];', start));
  return new Set([...list.matchAll(/'([a-z0-9-]+)'/g)].map((m) => m[1]!));
}

function sourceFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return sourceFiles(path);
    return name.endsWith('.ts') && !name.endsWith('.test.ts') ? [path] : [];
  });
}

/**
 * Quoted strings that look like icon names but are not used as icons: CSS
 * values and identifiers that happen to match a Bootstrap icon.
 */
const NOT_ICONS = new Set(['border-width', 'card-list', 'qr-code']);

/** The quoted names in an expression, leaving out the right of a comparison. */
function quotedNames(expr: string): string[] {
  const values = expr.replace(/[!=]==?\s*(['"`])[^'"`]*\1/g, '');
  return [...values.matchAll(/(['"`])([a-z0-9]+(?:-[a-z0-9]+)*)\1/g)].map((q) => q[2]!);
}

/**
 * Icon names named in a source text:
 * - `<sl-icon name="x">`, and the quoted names in a bound `name=${a ? 'x' : 'y'}`,
 *   bare or inside quotes;
 * - the quoted names assigned to anything called `icon`, `dirIcon`, `iconName`
 *   and so on, such as `icon: muted ? 'bell-slash' : 'bell'`;
 * - any quoted hyphenated string, which catches a name that reaches the
 *   template some other way. Single words are too often ordinary strings.
 */
function iconNamesIn(text: string): string[] {
  const names: string[] = [];
  // The binding may be bare, name=${...}, or quoted, name="${...}".
  for (const m of text.matchAll(
    /<sl-icon(?:-button)?\b[^>]*?\bname=(?:"([a-z0-9-]+)"|"?\$\{([^}]*)\}"?)/gs
  )) {
    if (m[1]) names.push(m[1]);
    else names.push(...quotedNames(m[2]!));
  }
  for (const m of text.matchAll(/\b\w*icon\w*\s*(?:[:=]|\?\?=?)\s*([^;,\n]+)/gi)) {
    names.push(...quotedNames(m[1]!));
  }
  for (const m of text.matchAll(/(['"`])([a-z0-9]+(?:-[a-z0-9]+)+)\1/g)) names.push(m[2]!);
  return names.filter((name) => !NOT_ICONS.has(name));
}

/** Every icon name in the source, with the first file that names it. */
function usedIcons(): Map<string, string> {
  const used = new Map<string, string>();
  for (const file of sourceFiles(join(WEB_ROOT, 'src'))) {
    for (const name of iconNamesIn(readFileSync(file, 'utf8'))) {
      if (!used.has(name)) used.set(name, file.slice(WEB_ROOT.length + 1));
    }
  }
  return used;
}

describe('Shoelace icon registration', () => {
  it('finds icon names in each form the source uses', () => {
    const found = (text: string) => new Set(iconNamesIn(text));
    expect(found('<sl-icon name="gear"></sl-icon>')).toContain('gear');
    expect(found("<sl-icon name=${on ? 'bell' : 'clock'}></sl-icon>")).toEqual(
      new Set(['bell', 'clock'])
    );
    expect(found(`<sl-icon name="\${g.type === 'agents' ? 'cpu' : 'people'}"></sl-icon>`)).toEqual(
      new Set(['cpu', 'people'])
    );
    expect(
      found(`<sl-icon-button
        label="Mute"
        name=\${muted
          ? 'volume-mute'
          : 'volume-up'}
      ></sl-icon-button>`)
    ).toEqual(new Set(['volume-mute', 'volume-up']));
    expect(found("icon: muted ? 'bell-slash' : 'bell',")).toEqual(new Set(['bell-slash', 'bell']));
    expect(found("const dirIcon = inbound ? 'inbox' : 'send';")).toEqual(
      new Set(['inbox', 'send'])
    );
    expect(found("const x = { width: 'border-width', kind: 'card-list' };")).toEqual(new Set());
  });

  it('registers every icon the source names', () => {
    const registered = registeredIcons();
    const available = new Set(
      readdirSync(join(WEB_ROOT, 'node_modules/@shoelace-style/shoelace/dist/assets/icons')).map(
        (f) => f.replace(/\.svg$/, '')
      )
    );
    // Only Bootstrap icon names count: other `icon:` strings name emoji or
    // app-specific glyphs that never reach <sl-icon>.
    const missing = [...usedIcons()]
      .filter(([name]) => available.has(name) && !registered.has(name))
      .map(([name, file]) => `${name} (${file})`);
    expect(missing).toEqual([]);
  });
});
