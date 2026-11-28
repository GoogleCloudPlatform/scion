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
 * Shoelace's `<sl-menu-item>` logs a console error ("The checked attribute can
 * only be used on menu items with type=\"checkbox\"") and clears `checked`
 * whenever `checked` is set without `type="checkbox"`.
 *
 * This guard scans `web/src/` (excluding tests) so any `<sl-menu-item>` that
 * binds or sets `checked` must also declare `type="checkbox"`.
 */

import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative, resolve, sep } from 'node:path';

/** `web/src`, relative to this file (`web/src/client/`). */
const srcDir = resolve(__dirname, '..');

interface MenuItemTag {
  line: number;
  attrs: string;
}

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

/**
 * Strip block and line comments while preserving newlines so reported line
 * numbers match the original source file.
 */
function stripCommentsPreservingLines(text: string): string {
  return text
    .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, ' '))
    .replace(/(^|\s)\/\/[^\n]*/g, '$1');
}

/**
 * Extract every `<sl-menu-item ...>` opening tag from source text, handling
 * quoted attribute values and `${...}` Lit expressions (which may contain `>`
 * such as arrow functions or comparisons).
 */
function findMenuItemOpenTags(text: string): MenuItemTag[] {
  const stripped = stripCommentsPreservingLines(text);
  const tags: MenuItemTag[] = [];
  const startRe = /<sl-menu-item(?=[\s>])/g;
  let match: RegExpExecArray | null;

  while ((match = startRe.exec(stripped)) !== null) {
    const tagStart = match.index;
    let i = tagStart + '<sl-menu-item'.length;
    let quote: string | null = null;
    let exprDepth = 0;

    while (i < stripped.length) {
      const ch = stripped[i];
      if (quote) {
        if (ch === '\\') {
          i += 2;
          continue;
        }
        if (ch === quote) quote = null;
      } else if (exprDepth > 0) {
        if (ch === "'" || ch === '"' || ch === '`') {
          quote = ch;
        } else if (ch === '{') {
          exprDepth++;
        } else if (ch === '}') {
          exprDepth--;
        }
      } else {
        if (ch === "'" || ch === '"') {
          quote = ch;
        } else if (ch === '$' && stripped[i + 1] === '{') {
          exprDepth = 1;
          i += 2;
          continue;
        } else if (ch === '>') {
          break;
        }
      }
      i++;
    }

    const line = stripped.slice(0, tagStart).split('\n').length;
    const attrs = stripped.slice(tagStart + '<sl-menu-item'.length, i);
    tags.push({ line, attrs });
    startRe.lastIndex = i + 1;
  }

  return tags;
}

function hasCheckedBinding(attrs: string): boolean {
  return /(?:^|\s)[?.]?checked(?:\s*=|\s|\/|$)/.test(attrs);
}

function hasCheckboxType(attrs: string): boolean {
  return /(?:^|\s)type\s*=\s*(?:"checkbox"|'checkbox')/.test(attrs);
}

function findViolationsInSource(text: string, fileLabel: string): string[] {
  const violations: string[] = [];
  for (const tag of findMenuItemOpenTags(text)) {
    if (hasCheckedBinding(tag.attrs) && !hasCheckboxType(tag.attrs)) {
      violations.push(`${fileLabel}:${tag.line}`);
    }
  }
  return violations;
}

describe('Shoelace sl-menu-item checked guard', () => {
  it('flags checked without type="checkbox" and allows valid usages (unit)', () => {
    const sample = `
      html\`
        <sl-menu-item value="plain">Plain</sl-menu-item>
        <sl-menu-item type="checkbox" value="ok" ?checked=\${true}>OK</sl-menu-item>
        <sl-menu-item value="bad-bool" ?checked=\${a > b}>Bad bool</sl-menu-item>
        <sl-menu-item value="bad-bare" checked>Bad bare</sl-menu-item>
        <sl-menu-item value="bad-prop" .checked=\${fn(() => true)}>Bad prop</sl-menu-item>
        /* <sl-menu-item value="commented" ?checked=\${true}>Ignored</sl-menu-item> */
      \`;
    `;
    expect(findViolationsInSource(sample, 'sample.ts')).toEqual([
      'sample.ts:5',
      'sample.ts:6',
      'sample.ts:7',
    ]);
  });

  it('finds checked sl-menu-item usages in src/ (sanity)', () => {
    let checkedCount = 0;
    for (const file of sourceFiles(srcDir)) {
      const text = readFileSync(file, 'utf8');
      for (const tag of findMenuItemOpenTags(text)) {
        if (hasCheckedBinding(tag.attrs)) checkedCount++;
      }
    }
    expect(checkedCount).toBeGreaterThan(10);
  });

  it('requires type="checkbox" on every sl-menu-item using checked in src/', () => {
    const violations: string[] = [];
    for (const file of sourceFiles(srcDir)) {
      const rel = relative(srcDir, file).split(sep).join('/');
      violations.push(...findViolationsInSource(readFileSync(file, 'utf8'), rel));
    }
    expect(violations).toEqual([]);
  });
});
