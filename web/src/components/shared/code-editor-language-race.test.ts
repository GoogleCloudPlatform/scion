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
 * the import fails. This file covers a slow import racing a language change
 * and a failing import; code-editor-language.test.ts covers the rest.
 *
 * Every @codemirror/lang-* module is mocked. Each factory runs when its
 * module is first imported, so `imports` records which language packages
 * were loaded. The mocked support adds a data-lang attribute to the editor
 * content, which shows which language the editor is configured with.
 */

import { it, expect, vi, beforeEach, afterEach } from 'vitest';

const h = vi.hoisted(() => {
  let releaseRust: () => void = () => {};
  const rustGate = new Promise<void>((resolve) => {
    releaseRust = resolve;
  });
  return { imports: [] as string[], rustGate, releaseRust: (): void => releaseRust() };
});

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
// Held until the race test releases it: a slow import.
vi.mock('@codemirror/lang-rust', async () => {
  await h.rustGate;
  return fakeLanguage('rust', 'rust');
});
// A failing import (for example a chunk that cannot be fetched).
vi.mock('@codemirror/lang-yaml', () => {
  h.imports.push('yaml');
  throw new Error('chunk load failed');
});

import { getLanguageFromPath, languageFor, type ScionCodeEditor } from './code-editor.js';

const editors: ScionCodeEditor[] = [];

beforeEach(() => {
  h.imports.length = 0;
});

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

it('a failed language import keeps plain text and does not throw', async () => {
  const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
  await expect(languageFor('yaml')).resolves.toEqual([]);
  expect(warn).toHaveBeenCalledTimes(1);
  const el = await openEditor('deploy.yaml');
  // The editor's own request has failed (and been logged) as well.
  await vi.waitFor(() => {
    expect(warn).toHaveBeenCalledTimes(2);
  });
  expect(h.imports).toContain('yaml');
  expect(el.shadowRoot?.querySelector('.error-state')).toBeNull();
  expect(content(el)?.textContent).toBe('contents of deploy.yaml');
  expect(content(el)?.hasAttribute('data-lang')).toBe(false);
});

it('a language change applies the new language and a slower earlier import does not win', async () => {
  const el = await openEditor('src/lib.rs');
  // The rust import is still pending: plain text, no language yet.
  expect(content(el)?.hasAttribute('data-lang')).toBe(false);
  el.language = getLanguageFromPath('src/lib.py');
  await vi.waitFor(() => {
    expect(content(el)?.getAttribute('data-lang')).toBe('python');
  });
  h.releaseRust();
  // Settles once the rust module has loaded, the same import the editor's
  // stale request is waiting on. From there the editor's remaining steps
  // are promise continuations only, which a zero-delay task runs after.
  await expect(languageFor('rust')).resolves.not.toEqual([]);
  expect(h.imports).toContain('rust');
  await new Promise((r) => setTimeout(r, 0));
  expect(content(el)?.getAttribute('data-lang')).toBe('python');
});
