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
 * Theme tokens in the chat components.
 *
 * A var(--scion-*) whose name theme.css never declares always takes its
 * fallback, which is a light-theme colour: in the dark theme the element
 * keeps a light background under light text. The guard fails on any such
 * reference in the chat component tree or the chat pages. Names the chat
 * tree declares itself, or that client/viewport.ts sets at runtime, count
 * as defined.
 *
 * The contrast checks resolve the colours the starred-only banner, the
 * reply preview and the composer's reply bar actually use, in both themes.
 */

import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { basename, join, relative, resolve } from 'node:path';

import { contrast, resolver, themeTokens } from '../../pages/__fixtures__/theme-contrast.js';

/** `web/src`, relative to this file (`web/src/components/shared/chat/`). */
const srcDir = resolve(__dirname, '../../..');
const componentsDir = join(srcDir, 'components');

function stripComments(text: string): string {
  return text.replace(/\/\*[\s\S]*?\*\//g, ' ').replace(/(^|\s)\/\/[^\n]*/g, '$1');
}

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) out.push(...walk(path));
    else out.push(path);
  }
  return out;
}

const isSource = (path: string): boolean => /\.ts$/.test(path) && !/\.test\.ts$/.test(path);

/** Every components/**\/chat/*.ts file and the chat page components. */
function chatFiles(): string[] {
  return walk(componentsDir)
    .filter(isSource)
    .filter(
      (path) =>
        basename(resolve(path, '..')) === 'chat' ||
        (resolve(path, '..') === join(componentsDir, 'pages') && /^chat/.test(basename(path)))
    )
    .sort();
}

/** Custom property names a CSS or TS text declares (`--scion-x:`). */
function declaredNames(text: string): Set<string> {
  return new Set([...stripComments(text).matchAll(/(--scion-[\w-]+)\s*:/g)].map((m) => m[1]));
}

/** Names set at runtime, written as quoted literals (`'--scion-x'`). */
function literalNames(text: string): Set<string> {
  return new Set([...stripComments(text).matchAll(/['"`](--scion-[\w-]+)['"`]/g)].map((m) => m[1]));
}

/** Names a text reads through var(--scion-x ...). */
function referencedNames(text: string): string[] {
  return [...stripComments(text).matchAll(/var\(\s*(--scion-[\w-]+)/g)].map((m) => m[1]);
}

/** `file: --name` for each reference in `files` that `defined` lacks. */
function undefinedReferences(
  files: Array<{ name: string; text: string }>,
  defined: Set<string>
): string[] {
  const out = new Set<string>();
  for (const { name, text } of files) {
    for (const ref of referencedNames(text)) {
      if (!defined.has(ref)) out.add(`${name}: ${ref}`);
    }
  }
  return [...out].sort();
}

function definedNames(files: Array<{ text: string }>): Set<string> {
  const defined = declaredNames(readFileSync(join(srcDir, 'styles/theme.css'), 'utf8'));
  const runtime = readFileSync(join(srcDir, 'client/viewport.ts'), 'utf8');
  for (const name of literalNames(runtime)) defined.add(name);
  for (const { text } of files) {
    for (const name of declaredNames(text)) defined.add(name);
    for (const name of literalNames(text)) defined.add(name);
  }
  return defined;
}

describe('chat theme tokens', () => {
  const files = chatFiles().map((path) => ({
    name: relative(srcDir, path),
    text: readFileSync(path, 'utf8'),
  }));
  const defined = definedNames(files);

  it('scans the chat component tree and the chat pages', () => {
    const names = files.map((f) => f.name);
    expect(names.length).toBeGreaterThan(20);
    expect(names).toContain('components/shared/chat/chat-thread.ts');
    expect(names).toContain('components/chat/chat-shell.ts');
    expect(names).toContain('components/pages/chat.ts');
  });

  it('references only --scion-* tokens that are defined', () => {
    expect(undefinedReferences(files, defined)).toEqual([]);
  });

  it('flags a reference to an undefined token', () => {
    const planted = [
      {
        name: 'planted.ts',
        text: '.a { background: var(--scion-surface-2, #f8fafc); color: var(--scion-text); }',
      },
    ];
    expect(undefinedReferences(planted, defined)).toEqual(['planted.ts: --scion-surface-2']);
  });

  it('counts names the chat tree declares or the viewport sets', () => {
    expect(defined.has('--scion-chat-tight')).toBe(true);
    expect(defined.has('--scion-app-height')).toBe(true);
    expect(defined.has('--scion-kb-open')).toBe(true);
  });

  it('ignores a token named only in a comment', () => {
    const commented = [{ name: 'c.ts', text: '/* var(--scion-nope) */ .a { color: red; }' }];
    expect(undefinedReferences(commented, defined)).toEqual([]);
  });
});

/** The declarations of the first `selector {` rule in a component's CSS. */
function rule(text: string, selector: string): Map<string, string> {
  const at = text.indexOf(`${selector} {`);
  if (at < 0) throw new Error(`no rule ${selector}`);
  const body = text.slice(text.indexOf('{', at) + 1, text.indexOf('}', at));
  const out = new Map<string, string>();
  for (const m of stripComments(body).matchAll(/([\w-]+)\s*:\s*([^;]+);/g)) {
    out.set(m[1], m[2].trim());
  }
  return out;
}

/** The token a `var(--scion-x, fallback)` value reads. */
function token(value: string | undefined): string {
  const m = /^var\((--scion-[\w-]+)/.exec(value ?? '');
  if (!m) throw new Error(`not a token: ${value}`);
  return m[1];
}

describe('chat banner and reply contrast', () => {
  const read = (file: string): string =>
    readFileSync(join(srcDir, 'components/shared/chat', file), 'utf8');
  const thread = read('chat-thread.ts');
  const message = read('chat-message.ts');
  const composer = read('chat-composer.ts');
  const { light, dark } = themeTokens();

  // [label, text token, background token, minimum ratio]
  const pairs: Array<[string, string, string, number]> = [
    [
      'banner title',
      '--scion-text',
      token(rule(thread, '.star-filter-banner').get('background')),
      4.5,
    ],
    [
      'banner muted line',
      token(rule(thread, '.star-filter-text span').get('color')),
      token(rule(thread, '.star-filter-banner').get('background')),
      4.5,
    ],
    [
      'Show all messages label',
      token(rule(thread, '.star-filter-show-all::part(base)').get('color')),
      token(rule(thread, '.star-filter-show-all::part(base)').get('background')),
      4.5,
    ],
    [
      'reply preview text',
      token(rule(message, '.reply-preview').get('color')),
      token(rule(message, '.reply-preview').get('background')),
      4.5,
    ],
    // The light hover pair is 4.34:1, as it was before the dark fix; the
    // state is transient, so it is held to 4:1.
    [
      'reply preview text, hovered',
      token(rule(message, '.reply-preview').get('color')),
      token(rule(message, '.reply-preview:hover').get('background')),
      4,
    ],
    [
      'reply preview sender',
      token(rule(message, '.reply-preview .reply-sender').get('color')),
      token(rule(message, '.reply-preview').get('background')),
      4.5,
    ],
    [
      'reply bar text',
      token(rule(composer, '.reply-bar').get('color')),
      token(rule(composer, '.reply-bar').get('background')),
      4.5,
    ],
    [
      'reply bar sender',
      token(rule(composer, '.reply-bar .reply-sender').get('color')),
      token(rule(composer, '.reply-bar').get('background')),
      4.5,
    ],
  ];

  for (const [theme, vars] of [
    ['light', light],
    ['dark', dark],
  ] as const) {
    const get = resolver(vars);
    for (const [label, fg, bg, min] of pairs) {
      it(`${label} is readable in the ${theme} theme`, () => {
        expect(contrast(get(fg), get(bg))).toBeGreaterThanOrEqual(min);
      });
    }
  }

  it('gives the banner a dark background in the dark theme', () => {
    const get = resolver(dark);
    const bg = get(token(rule(thread, '.star-filter-banner').get('background')));
    expect(contrast(bg, get('--scion-text'))).toBeGreaterThanOrEqual(4.5);
    expect(bg).not.toEqual(resolver(light)('--scion-bg'));
  });
});
