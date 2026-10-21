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
 * CodeMirror per-language chunks (webperf C8, ptone/scion#5351): the code
 * editor imports a language package only when an editor needs that
 * language, shows plain text until it arrives, applies only the latest
 * requested language, and stays in plain text when there is no support or
 * the import fails.
 *
 * Slow and failing imports are covered in code-editor-language-race.test.ts,
 * so every mock here loads normally (a static import of all languages would
 * load them all and fail the first test, not stop the file).
 *
 * Every @codemirror/lang-* module is mocked. Each factory runs when its
 * module is first imported, so `imports` records which language packages
 * were loaded. The mocked support adds a data-lang attribute to the editor
 * content, which shows which language the editor is configured with.
 */

import { it, expect, vi, afterEach } from 'vitest';

const h = vi.hoisted(() => ({ imports: [] as string[] }));

/** A mocked language module: records its import, exports `fn` as the support function. */
async function fakeLanguage(name: string, fn: string): Promise<Record<string, unknown>> {
  h.imports.push(name);
  const { EditorView } = await import('@codemirror/view');
  return { [fn]: () => EditorView.contentAttributes.of({ 'data-lang': name }) };
}

vi.mock('@codemirror/lang-go', () => fakeLanguage('go', 'go'));
vi.mock('@codemirror/lang-python', () => fakeLanguage('python', 'python'));
vi.mock('@codemirror/lang-javascript', () => fakeLanguage('javascript', 'javascript'));
vi.mock('@codemirror/lang-json', () => fakeLanguage('json', 'json'));
vi.mock('@codemirror/lang-markdown', () => fakeLanguage('markdown', 'markdown'));
vi.mock('@codemirror/lang-html', () => fakeLanguage('html', 'html'));
vi.mock('@codemirror/lang-css', () => fakeLanguage('css', 'css'));
vi.mock('@codemirror/lang-rust', () => fakeLanguage('rust', 'rust'));
vi.mock('@codemirror/lang-yaml', () => fakeLanguage('yaml', 'yaml'));
import { getLanguageFromPath, languageFor, type ScionCodeEditor } from './code-editor.js';

const editors: ScionCodeEditor[] = [];

afterEach(() => {
  for (const el of editors.splice(0)) el.remove();
  vi.restoreAllMocks();
});

async function openEditor(path: string): Promise<ScionCodeEditor> {
  const el = document.createElement('scion-code-editor');
  el.language = getLanguageFromPath(path);
  el.content = `contents of ${path}`;
  editors.push(el);
  document.body.appendChild(el);
  await vi.waitFor(() => {
    expect(content(el)).not.toBeNull();
  });
  return el;
}

function content(el: ScionCodeEditor): HTMLElement | null {
  return el.shadowRoot?.querySelector<HTMLElement>('.cm-content') ?? null;
}

it('editor loads only the requested language', async () => {
  const el = await openEditor('cmd/main.go');
  await vi.waitFor(() => {
    expect(content(el)?.getAttribute('data-lang')).toBe('go');
  });
  // Cumulative since the file loaded: a static import of the languages
  // would have run every factory before this test.
  expect(h.imports).toEqual(['go']);
});

it('language ids match case-insensitively', async () => {
  await expect(languageFor('JSON')).resolves.not.toEqual([]);
  expect(h.imports).toContain('json');
  const el = document.createElement('scion-code-editor');
  el.language = 'JSON';
  el.content = '{}';
  editors.push(el);
  document.body.appendChild(el);
  await vi.waitFor(() => {
    expect(content(el)?.getAttribute('data-lang')).toBe('json');
  });
});

it('a language without support imports nothing and stays plain text', async () => {
  const before = [...h.imports];
  // languageFor settles only after any import it starts, so an unexpected
  // import would already be recorded here.
  await expect(languageFor('toml')).resolves.toEqual([]);
  await expect(languageFor('constructor')).resolves.toEqual([]);
  expect(h.imports).toEqual(before);
  // The editor resolves its language through the same languageFor.
  const el = await openEditor('config.toml');
  await el.updateComplete;
  expect(h.imports).toEqual(before);
  expect(content(el)?.textContent).toBe('contents of config.toml');
  expect(content(el)?.hasAttribute('data-lang')).toBe(false);
});
