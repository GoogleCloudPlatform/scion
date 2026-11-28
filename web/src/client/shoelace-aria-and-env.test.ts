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
 * Static guards for two browser/a11y invariants across `web/src`:
 *
 * 1. `shoelace-dead-host-aria`: Shoelace `sl-*` shadow hosts do not forward
 *    ARIA state attributes (`aria-pressed`, `aria-expanded`, `aria-checked`,
 *    `aria-selected`) to their inner interactive elements, so placing them on
 *    an `<sl-*>` host reaches no assistive technology (see `web/AGENTS.md`).
 *    Use a native element (e.g. `<button>`) for toggle state instead.
 *
 * 2. `browser-no-unguarded-process-env`: Client modules execute in the browser
 *    where `process` is undefined at runtime unless guarded with
 *    `typeof process !== 'undefined'`.
 *
 * Justified exceptions may use a reason-required inline suppression comment on
 * the same line or the immediately preceding line:
 *   `// scion-a11y-ignore: <reason>`
 * A bare `// scion-a11y-ignore` without a non-empty reason fails the check.
 */

import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative, resolve, sep } from 'node:path';

/** `web/src`, relative to this file (`web/src/client/`). */
const srcDir = resolve(__dirname, '..');

const DEAD_HOST_ARIA_RE = /\b(aria-pressed|aria-expanded|aria-checked|aria-selected)\s*=/g;
const PROCESS_ENV_RE = /\bprocess\.env\b/g;
const TYPEOF_PROCESS_GUARD_RE = /\btypeof\s+process\s*!==?\s*['"]undefined['"]/;
const IGNORE_DIRECTIVE_RE = /\/\/\s*scion-a11y-ignore\b(.*)/;

export interface Finding {
  rule: 'shoelace-dead-host-aria' | 'browser-no-unguarded-process-env' | 'bare-ignore-directive';
  file: string;
  line: number;
  message: string;
}

function sourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    if (statSync(full).isDirectory()) {
      out.push(...sourceFiles(full));
    } else if (/\.ts$/.test(name) && !/\.test\.ts$/.test(name)) {
      out.push(full);
    }
  }
  return out.sort();
}

function normalizeRelPath(filePath: string): string {
  return relative(srcDir, filePath).split(sep).join('/');
}

function lineAt(src: string, idx: number): number {
  let line = 1;
  for (let i = 0; i < idx; i++) {
    if (src.charCodeAt(i) === 10) line++;
  }
  return line;
}

interface MaskedSource {
  /**
   * Template literal text (HTML markup) preserved; JS code, comments, and
   * non-template string literals replaced with spaces (preserving `\n`).
   * Used for scanning `<sl-*>` host tags so non-template strings and comments
   * never produce false positives.
   */
  templateOnly: string;
  /**
   * Executable JS/TS code preserved; comments, string literals, and template
   * text outside `${...}` replaced with spaces (preserving `\n`).
   * Used for scanning `process.env` references.
   */
  codeOnly: string;
}

/**
 * Single-pass lexer that separates JS/TS code from template literal markup
 * while masking comments and string literals, preserving every `\n` so
 * character indices map 1:1 to original 1-based line numbers.
 */
function splitCodeAndTemplates(src: string): MaskedSource {
  const n = src.length;
  const tplChars = new Array<string>(n);
  const codeChars = new Array<string>(n);

  const blank = (i: number): void => {
    const ch = src[i] === '\n' ? '\n' : ' ';
    tplChars[i] = ch;
    codeChars[i] = ch;
  };
  const keepCode = (i: number): void => {
    tplChars[i] = src[i] === '\n' ? '\n' : ' ';
    codeChars[i] = src[i]!;
  };
  const keepTpl = (i: number): void => {
    tplChars[i] = src[i]!;
    codeChars[i] = src[i] === '\n' ? '\n' : ' ';
  };

  // Stack of active template literal `${...}` brace depths.
  // When empty, we are in top-level JS code; when non-empty and top === 0,
  // we are inside template markup; when top > 0, we are inside `${...}` code.
  const tplBraceStack: number[] = [];
  const inTemplateMarkup = (): boolean =>
    tplBraceStack.length > 0 && tplBraceStack[tplBraceStack.length - 1] === 0;

  let i = 0;
  while (i < n) {
    const ch = src[i]!;
    const next = i + 1 < n ? src[i + 1] : '';

    if (inTemplateMarkup()) {
      if (ch === '\\' && i + 1 < n) {
        keepTpl(i);
        keepTpl(i + 1);
        i += 2;
        continue;
      }
      if (ch === '`') {
        tplBraceStack.pop();
        blank(i);
        i++;
        continue;
      }
      if (ch === '$' && next === '{') {
        tplBraceStack[tplBraceStack.length - 1] = 1;
        // Keep `${` in templateOnly so extractOpeningTags tracks interpolations.
        keepTpl(i);
        keepTpl(i + 1);
        i += 2;
        continue;
      }
      keepTpl(i);
      i++;
      continue;
    }

    // In JS/TS code (either top-level or inside `${...}`).
    // Line comment
    if (ch === '/' && next === '/') {
      while (i < n && src[i] !== '\n') {
        blank(i);
        i++;
      }
      continue;
    }
    // Block comment
    if (ch === '/' && next === '*') {
      blank(i);
      blank(i + 1);
      i += 2;
      while (i < n) {
        if (src[i] === '*' && src[i + 1] === '/') {
          blank(i);
          blank(i + 1);
          i += 2;
          break;
        }
        blank(i);
        i++;
      }
      continue;
    }
    // Single- or double-quoted string literal
    if (ch === "'" || ch === '"') {
      const quote = ch;
      blank(i);
      i++;
      while (i < n) {
        if (src[i] === '\\' && i + 1 < n) {
          blank(i);
          blank(i + 1);
          i += 2;
          continue;
        }
        const cur = src[i]!;
        blank(i);
        i++;
        if (cur === quote || cur === '\n') break;
      }
      continue;
    }
    // Template literal start
    if (ch === '`') {
      tplBraceStack.push(0);
      blank(i);
      i++;
      continue;
    }
    // Track braces when inside `${...}`
    if (tplBraceStack.length > 0) {
      if (ch === '{') {
        tplBraceStack[tplBraceStack.length - 1]!++;
        keepCode(i);
        i++;
        continue;
      }
      if (ch === '}') {
        tplBraceStack[tplBraceStack.length - 1]!--;
        if (tplBraceStack[tplBraceStack.length - 1] === 0) {
          // Keep closing `}` in templateOnly to balance `${` for tag extraction.
          keepTpl(i);
        } else {
          keepCode(i);
        }
        i++;
        continue;
      }
    }

    keepCode(i);
    i++;
  }

  return {
    templateOnly: tplChars.join(''),
    codeOnly: codeChars.join(''),
  };
}

/**
 * Extract `<sl-*>` opening tags from template markup, skipping `${...}`
 * interpolations and quoted attribute values so `>` inside expressions does
 * not terminate the tag early.
 */
function extractShoelaceOpeningTags(
  templateOnly: string
): Array<{ tagName: string; start: number; line: number; rawTag: string }> {
  const tags: Array<{ tagName: string; start: number; line: number; rawTag: string }> = [];
  const re = /<(sl-[a-z][a-z0-9-]*)(?=[\s>/])/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(templateOnly)) !== null) {
    const tagName = m[1]!;
    const start = m.index;
    let i = start + m[0].length;
    let braceDepth = 0;
    let quote: string | null = null;
    while (i < templateOnly.length) {
      const ch = templateOnly[i]!;
      if (quote) {
        if (ch === '\\') {
          i += 2;
          continue;
        }
        if (ch === quote) quote = null;
      } else {
        if (ch === '"' || ch === "'") {
          quote = ch;
        } else if (ch === '$' && templateOnly[i + 1] === '{') {
          braceDepth++;
          i += 2;
          continue;
        } else if (ch === '{' && braceDepth > 0) {
          braceDepth++;
        } else if (ch === '}' && braceDepth > 0) {
          braceDepth--;
        } else if (ch === '>' && braceDepth === 0) {
          i++;
          break;
        }
      }
      i++;
    }
    const rawTag = templateOnly.slice(start, i);
    tags.push({ tagName, start, line: lineAt(templateOnly, start), rawTag });
  }
  return tags;
}

/**
 * Parse `// scion-a11y-ignore` directives from the raw source lines.
 * Returns:
 * - `suppressedLines`: 1-based line numbers covered by a valid suppression
 *   (the directive's own line and the immediately following line)
 * - `bareViolations`: findings for any bare `// scion-a11y-ignore` lacking a
 *   `: <reason>` explanation.
 */
function parseSuppressions(
  relPath: string,
  rawLines: string[]
): { suppressedLines: Set<number>; bareViolations: Finding[] } {
  const suppressedLines = new Set<number>();
  const bareViolations: Finding[] = [];

  for (let i = 0; i < rawLines.length; i++) {
    const lineNum = i + 1;
    const match = IGNORE_DIRECTIVE_RE.exec(rawLines[i]!);
    if (!match) continue;
    const rest = match[1] ?? '';
    const reasonMatch = /^\s*:\s*(\S.*)$/.exec(rest);
    if (!reasonMatch) {
      bareViolations.push({
        rule: 'bare-ignore-directive',
        file: relPath,
        line: lineNum,
        message: 'scion-a11y-ignore requires a non-empty reason (`// scion-a11y-ignore: <reason>`)',
      });
      continue;
    }
    suppressedLines.add(lineNum);
    suppressedLines.add(lineNum + 1);
    // Also cover an attribute on the immediately preceding line when the
    // directive sits inside a multi-line Lit interpolation (`aria-expanded=${\n // scion-a11y-ignore: ...`).
    if (lineNum > 1 && /\$\{\s*$/.test(rawLines[i - 1]!)) {
      suppressedLines.add(lineNum - 1);
    }
  }

  return { suppressedLines, bareViolations };
}

/**
 * True if `process.env` on 1-based `lineNum` is guarded by a
 * `typeof process !== 'undefined'` check on the same line or in an enclosing
 * `if` / ternary / logical-AND guard in the preceding lines.
 */
function hasProcessGuard(rawLines: string[], lineNum: number): boolean {
  const idx = lineNum - 1;
  if (TYPEOF_PROCESS_GUARD_RE.test(rawLines[idx]!)) return true;

  // Walk backwards up to 12 lines tracking brace balance to see if an
  // enclosing `if (... typeof process !== 'undefined' ...)` or chained `&&`
  // / `?` expression guards this line.
  let braceBalance = 0;
  for (let j = idx - 1; j >= Math.max(0, idx - 12); j--) {
    const l = rawLines[j]!;
    for (let k = l.length - 1; k >= 0; k--) {
      if (l[k] === '}') braceBalance++;
      else if (l[k] === '{') braceBalance--;
    }
    if (braceBalance <= 0 && TYPEOF_PROCESS_GUARD_RE.test(l)) {
      return true;
    }
  }
  return false;
}

export function scanSource(relPath: string, rawSrc: string): Finding[] {
  const hasSlAriaCandidate =
    rawSrc.includes('<sl-') &&
    (rawSrc.includes('aria-pressed') ||
      rawSrc.includes('aria-expanded') ||
      rawSrc.includes('aria-checked') ||
      rawSrc.includes('aria-selected'));
  const hasProcessEnv = rawSrc.includes('process.env');
  const hasIgnore = rawSrc.includes('scion-a11y-ignore');
  if (!hasSlAriaCandidate && !hasProcessEnv && !hasIgnore) return [];

  const rawLines = rawSrc.split('\n');
  const { suppressedLines, bareViolations } = hasIgnore
    ? parseSuppressions(relPath, rawLines)
    : { suppressedLines: new Set<number>(), bareViolations: [] };
  const { templateOnly, codeOnly } =
    hasSlAriaCandidate || hasProcessEnv
      ? splitCodeAndTemplates(rawSrc)
      : { templateOnly: '', codeOnly: '' };
  const findings: Finding[] = [...bareViolations];

  // 1. shoelace-dead-host-aria
  if (hasSlAriaCandidate) {
    for (const tag of extractShoelaceOpeningTags(templateOnly)) {
      DEAD_HOST_ARIA_RE.lastIndex = 0;
      let m: RegExpExecArray | null;
      while ((m = DEAD_HOST_ARIA_RE.exec(tag.rawTag)) !== null) {
        const attrLine = tag.line + lineAt(tag.rawTag, m.index) - 1;
        if (suppressedLines.has(tag.line) || suppressedLines.has(attrLine)) continue;
        findings.push({
          rule: 'shoelace-dead-host-aria',
          file: relPath,
          line: attrLine,
          message: `<${tag.tagName}> does not forward ${m[1]} to its inner control; use a native element for toggle state`,
        });
      }
    }
  }

  // 2. browser-no-unguarded-process-env
  if (hasProcessEnv) {
    const codeLines = codeOnly.split('\n');
    for (let i = 0; i < codeLines.length; i++) {
      const lineNum = i + 1;
      PROCESS_ENV_RE.lastIndex = 0;
      if (!PROCESS_ENV_RE.test(codeLines[i]!)) continue;
      if (suppressedLines.has(lineNum)) continue;
      if (hasProcessGuard(rawLines, lineNum)) continue;
      findings.push({
        rule: 'browser-no-unguarded-process-env',
        file: relPath,
        line: lineNum,
        message: `unguarded process.env in browser module; guard with typeof process !== 'undefined'`,
      });
    }
  }

  return findings;
}

describe('Shoelace dead host ARIA & browser process.env guard', () => {
  describe('shoelace-dead-host-aria fixtures', () => {
    it('flags aria-pressed on <sl-button>', () => {
      const src = `
        const tpl = html\`
          <sl-button variant="primary" aria-pressed=\${this.selected}>Option A</sl-button>
        \`;
      `;
      const findings = scanSource('fixture.ts', src);
      expect(findings).toHaveLength(1);
      expect(findings[0]).toMatchObject({
        rule: 'shoelace-dead-host-aria',
        line: 3,
      });
      expect(findings[0]!.message).toContain('aria-pressed');
    });

    it('flags aria-expanded on <sl-icon-button>', () => {
      const src = `
        const tpl = html\`
          <sl-icon-button
            name="arrows-angle-expand"
            label="Expand agents"
            aria-expanded=\${this.expanded ? 'true' : 'false'}
          ></sl-icon-button>
        \`;
      `;
      const findings = scanSource('fixture.ts', src);
      expect(findings).toHaveLength(1);
      expect(findings[0]).toMatchObject({
        rule: 'shoelace-dead-host-aria',
        line: 6,
      });
      expect(findings[0]!.message).toContain('aria-expanded');
    });

    it('flags aria-checked and aria-selected on any <sl-*> host', () => {
      const src = `
        const tpl = html\`
          <sl-checkbox aria-checked="true">Accept</sl-checkbox>
          <sl-tab aria-selected=\${this.active}>Tab 1</sl-tab>
        \`;
      `;
      const findings = scanSource('fixture.ts', src);
      expect(findings).toHaveLength(2);
      expect(findings.map((f) => f.rule)).toEqual([
        'shoelace-dead-host-aria',
        'shoelace-dead-host-aria',
      ]);
      expect(findings[0]!.message).toContain('aria-checked');
      expect(findings[1]!.message).toContain('aria-selected');
    });

    it('passes native <button aria-pressed> and <button aria-expanded>', () => {
      const src = `
        const tpl = html\`
          <button type="button" aria-pressed=\${this.selected}>Option A</button>
          <button type="button" aria-expanded=\${this.open}>Toggle</button>
        \`;
      `;
      expect(scanSource('fixture.ts', src)).toEqual([]);
    });

    it('passes aria-disabled and aria-label on <sl-button>', () => {
      const src = `
        const tpl = html\`
          <sl-button aria-disabled="true" aria-label="Save">Save</sl-button>
        \`;
      `;
      expect(scanSource('fixture.ts', src)).toEqual([]);
    });

    it('ignores attribute text inside comments and non-template string literals', () => {
      const src = `
        // <sl-button aria-pressed="true">commented out</sl-button>
        /* <sl-icon-button aria-expanded="false"></sl-icon-button> */
        const single = '<sl-button aria-pressed="true">string</sl-button>';
        const double = "<sl-checkbox aria-checked='true'></sl-checkbox>";
      `;
      expect(scanSource('fixture.ts', src)).toEqual([]);
    });
  });

  describe('browser-no-unguarded-process-env fixtures', () => {
    it('flags unguarded process.env in client code and template interpolations', () => {
      const src = `
        const mode = process.env.NODE_ENV;
        const tpl = html\`<div>\${process.env.API_URL}</div>\`;
      `;
      const findings = scanSource('fixture.ts', src);
      expect(findings).toHaveLength(2);
      expect(findings[0]).toMatchObject({ rule: 'browser-no-unguarded-process-env', line: 2 });
      expect(findings[1]).toMatchObject({ rule: 'browser-no-unguarded-process-env', line: 3 });
    });

    it('passes typeof process !== "undefined" guards (inline and block)', () => {
      const src = `
        const a = typeof process !== 'undefined' ? process.env.NODE_ENV : 'production';
        const b = typeof process !== "undefined" && process.env.DEBUG;
        let c = '';
        if (typeof process !== 'undefined') {
          c = process.env.SCION_FOO ?? '';
        }
        // process.env.COMMENTED
        const text = 'process.env.LITERAL';
      `;
      expect(scanSource('fixture.ts', src)).toEqual([]);
    });
  });

  describe('inline suppression directives', () => {
    it('allows scion-a11y-ignore with a non-empty reason', () => {
      const src = `
        // scion-a11y-ignore: custom Shoelace wrapper forwards aria-expanded internally
        const tpl = html\`<sl-button aria-expanded="true">Custom</sl-button>\`;
        const tpl2 = html\`
          <sl-input
            aria-expanded=\${
              // scion-a11y-ignore: pre-existing combobox tracked for follow-up
              this.open ? 'true' : 'false'
            }
          ></sl-input>
        \`;
        // scion-a11y-ignore: build-time Vite define replaces this token
        const envVal = process.env.VITE_TOKEN;
      `;
      expect(scanSource('fixture.ts', src)).toEqual([]);
    });

    it('fails bare scion-a11y-ignore without a reason', () => {
      const src = `
        // scion-a11y-ignore
        const tpl = html\`<sl-button aria-pressed="true">Bad</sl-button>\`;
        // scion-a11y-ignore:
        const envVal = process.env.FOO;
      `;
      const findings = scanSource('fixture.ts', src);
      expect(findings.filter((f) => f.rule === 'bare-ignore-directive')).toHaveLength(2);
      expect(findings.filter((f) => f.rule === 'shoelace-dead-host-aria')).toHaveLength(1);
      expect(findings.filter((f) => f.rule === 'browser-no-unguarded-process-env')).toHaveLength(1);
    });
  });

  describe('repository scan', () => {
    it('scans web/src and reports zero violations', () => {
      const files = sourceFiles(srcDir);
      expect(files.length).toBeGreaterThan(100);

      const allFindings: string[] = [];
      for (const file of files) {
        const rel = normalizeRelPath(file);
        const findings = scanSource(rel, readFileSync(file, 'utf8'));
        for (const f of findings) {
          allFindings.push(`${f.file}:${f.line} [${f.rule}] ${f.message}`);
        }
      }
      expect(allFindings).toEqual([]);
    });
  });
});
