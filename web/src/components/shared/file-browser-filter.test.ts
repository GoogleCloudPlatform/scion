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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetServerFlagStateForTests, setFeatureFlag } from '../../utils/feature-flags.js';
import {
  FILTERS_QUERY_DIALECT_FLAG,
  getViewSchema,
  knownKeysFor,
  parse,
  type Query,
  type ViewSchema,
} from '../../shared/filter-query/index.js';
import {
  collectFileExtensions,
  extractFileExtension,
  extractFileQueryText,
  filterFilesByQuery,
  isFileQueryEmpty,
  postFilterFilesByQuery,
} from './file-browser-filter.js';
import './file-browser.js';
import type {
  FileBrowserDataSource,
  FileEntry,
  FileListResult,
  ScionFileBrowser,
} from './file-browser.js';
import type { ScionFilterBox } from './filter-box/filter-box.js';

function makeEntry(path: string, size = 10): FileEntry {
  return { path, size, modTime: '2026-01-01T00:00:00Z', mode: '-rw-r--r--' };
}

function parseFilesQuery(input: string): Query {
  const schema = getViewSchema('files') as ViewSchema;
  const res = parse(input, knownKeysFor(schema));
  expect(res.error).toBeUndefined();
  return res.query as Query;
}

describe('file-browser-filter pure helpers', () => {
  const sampleFiles: FileEntry[] = [
    makeEntry('src/app.ts'),
    makeEntry('src/button.tsx'),
    makeEntry('src/button.test.ts'),
    makeEntry('pkg/hub/server.go'),
    makeEntry('README.md'),
    makeEntry('Makefile'),
    makeEntry('dir.v1/LICENSE'),
    makeEntry('.gitignore'),
    makeEntry('.eslintrc.json'),
  ];

  it('extracts lowercase extensions without leading dot and ignores extensionless / dotfile names', () => {
    expect(extractFileExtension('src/app.ts')).toBe('ts');
    expect(extractFileExtension('src/button.test.TSX')).toBe('tsx');
    expect(extractFileExtension('Makefile')).toBe('');
    expect(extractFileExtension('dir.v1/LICENSE')).toBe('');
    expect(extractFileExtension('.gitignore')).toBe('');
    expect(extractFileExtension('.eslintrc.json')).toBe('json');
    expect(extractFileExtension('trailing.')).toBe('');
  });

  it('collects sorted unique extensions for value suggestions', () => {
    expect(collectFileExtensions(sampleFiles)).toEqual(['go', 'json', 'md', 'ts', 'tsx']);
  });

  it('preserves regex and fuzzy fallback verbatim in pure-text mode', () => {
    // Regex match
    const regexQ = parseFilesQuery(String.raw`src/.*\.tsx?$`);
    expect(regexQ.mode).toBe('text');
    expect(filterFilesByQuery(sampleFiles, regexQ).map((f) => f.path)).toEqual([
      'src/app.ts',
      'src/button.tsx',
      'src/button.test.ts',
    ]);

    // Unclosed '(' triggers fuzzy fallback: '(', 'h', 'u', 'b' in 'a(hub).go'
    const fuzzyWithUnclosed = parseFilesQuery('(hub');
    expect(fuzzyWithUnclosed.mode).toBe('text');
    expect(
      filterFilesByQuery(
        [makeEntry('a(hub).go'), makeEntry('pkg/hub/server.go')],
        fuzzyWithUnclosed
      ).map((f) => f.path)
    ).toEqual(['a(hub).go']);
  });

  it('filters by ext: (comma OR, case-insensitive, leading dot tolerated, negation)', () => {
    const orQ = parseFilesQuery('ext:ts,tsx');
    expect(filterFilesByQuery(sampleFiles, orQ).map((f) => f.path)).toEqual([
      'src/app.ts',
      'src/button.tsx',
      'src/button.test.ts',
    ]);

    const dotQ = parseFilesQuery('ext:.GO');
    expect(filterFilesByQuery(sampleFiles, dotQ).map((f) => f.path)).toEqual(['pkg/hub/server.go']);

    const negQ = parseFilesQuery('-ext:ts,tsx,go,md');
    expect(filterFilesByQuery(sampleFiles, negQ).map((f) => f.path)).toEqual([
      'Makefile',
      'dir.v1/LICENSE',
      '.gitignore',
      '.eslintrc.json',
    ]);
  });

  it('combines ext:, positive text (regex -> fuzzy), and excluded text', () => {
    const q = parseFilesQuery('ext:ts,tsx button -test');
    expect(extractFileQueryText(q)).toBe('button');
    expect(isFileQueryEmpty(q)).toBe(false);
    expect(filterFilesByQuery(sampleFiles, q).map((f) => f.path)).toEqual(['src/button.tsx']);
  });

  it('post-filters backend results by key terms and excluded text without touching pure-text results', () => {
    const backend: FileEntry[] = [
      makeEntry('deep/button.ts'),
      makeEntry('deep/button.test.ts'),
      makeEntry('deep/button.go'),
    ];
    const structuredQ = parseFilesQuery('ext:ts button -test');
    expect(postFilterFilesByQuery(backend, structuredQ).map((f) => f.path)).toEqual([
      'deep/button.ts',
    ]);

    const pureTextQ = parseFilesQuery('button');
    expect(postFilterFilesByQuery(backend, pureTextQ).map((f) => f.path)).toEqual([
      'deep/button.ts',
      'deep/button.test.ts',
      'deep/button.go',
    ]);
  });
});

describe('<scion-file-browser> unified filter dialect (P5)', () => {
  beforeEach(() => {
    resetServerFlagStateForTests();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    resetServerFlagStateForTests();
    vi.restoreAllMocks();
  });

  function makeSource(
    initialFiles: string[],
    searchFn?: (q: string, limit?: number, signal?: AbortSignal) => Promise<FileListResult>
  ): FileBrowserDataSource {
    const source: FileBrowserDataSource = {
      listFiles: vi.fn(() =>
        Promise.resolve({
          files: initialFiles.map((p) => makeEntry(p)),
          totalSize: initialFiles.length * 10,
          totalCount: initialFiles.length,
        })
      ),
      deleteFile: vi.fn(),
      uploadFiles: vi.fn(),
      getDownloadUrl: (p) => `/download/${p}`,
      getPreviewUrl: (p) => `/preview/${p}`,
    };
    if (searchFn) {
      source.searchFiles = searchFn;
    }
    return source;
  }

  async function mountBrowser(source: FileBrowserDataSource): Promise<ScionFileBrowser> {
    const el = document.createElement('scion-file-browser') as ScionFileBrowser;
    el.dataSource = source;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
    return el;
  }

  function renderedPaths(el: ScionFileBrowser): string[] {
    return Array.from(el.shadowRoot?.querySelectorAll('.file-name') ?? []).map(
      (n) => n.textContent?.trim() ?? ''
    );
  }

  it('renders legacy <sl-input> and keeps behavior unchanged when filters.query_dialect is off', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, false);
    const source = makeSource(['src/a.ts', 'src/b.go', '.env']);
    const el = await mountBrowser(source);

    expect(el.shadowRoot?.querySelector('scion-filter-box')).toBeNull();
    expect(el.shadowRoot?.querySelector('sl-input.filter-input')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('sl-checkbox.dot-files-toggle')).not.toBeNull();
    expect(renderedPaths(el)).toEqual(['src/a.ts', 'src/b.go']);
  });

  it('renders <scion-filter-box view="files"> and keeps the dotfiles toggle when experiment is on', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    const source = makeSource(['src/a.ts', 'src/b.go', '.env']);
    const el = await mountBrowser(source);

    const box = el.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox | null;
    expect(box).not.toBeNull();
    expect(box?.view).toBe('files');
    expect(el.shadowRoot?.querySelector('sl-input.filter-input')).toBeNull();
    // Dotfiles toggle stays (Q3)
    expect(el.shadowRoot?.querySelector('sl-checkbox.dot-files-toggle')).not.toBeNull();
    expect(renderedPaths(el)).toEqual(['src/a.ts', 'src/b.go']);
  });

  it('sends only the text part to ?q= and post-filters merged backend results with ext:', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    const searchFiles = vi.fn((_q: string) =>
      Promise.resolve({
        files: [
          makeEntry('deep/button.ts'),
          makeEntry('deep/button.go'),
          makeEntry('.hidden/button.ts'),
        ],
        totalSize: 30,
        totalCount: 3,
      })
    );
    const source = makeSource(['src/button.tsx', 'src/readme.md'], searchFiles);
    const el = await mountBrowser(source);

    const box = el.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;

    // Type structured query and press Enter to commit immediately
    input.value = 'ext:ts,tsx button';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));

    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    // Backend receives ONLY the text part ("button"), never "ext:ts,tsx"
    expect(searchFiles).toHaveBeenCalledTimes(1);
    expect(searchFiles.mock.calls[0]?.[0]).toBe('button');

    // Merged results include local .tsx + backend .ts, exclude backend .go and dotfile
    expect(renderedPaths(el)).toEqual(['deep/button.ts', 'src/button.tsx']);
  });

  it('does not call backend search when query has only ext: terms (no text)', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    const searchFiles = vi.fn(() => Promise.resolve({ files: [], totalSize: 0, totalCount: 0 }));
    const source = makeSource(['src/a.ts', 'src/b.go'], searchFiles);
    const el = await mountBrowser(source);

    const box = el.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;

    input.value = 'ext:go';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));

    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    expect(searchFiles).not.toHaveBeenCalled();
    expect(renderedPaths(el)).toEqual(['src/b.go']);
  });

  it('keeps last valid filter applied when the query has a syntax error', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    const source = makeSource(['src/a.ts', 'src/b.go']);
    const el = await mountBrowser(source);

    const box = el.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;

    // Apply a valid filter first
    input.value = 'ext:ts';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await el.updateComplete;
    expect(renderedPaths(el)).toEqual(['src/a.ts']);

    // Now type an invalid query ("ext:ts OR ext:go") — last valid filter ("ext:ts") stays applied
    input.value = 'ext:ts OR ext:go';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await el.updateComplete;
    expect(renderedPaths(el)).toEqual(['src/a.ts']);
  });

  it('works client-only for template/harness-config data sources without searchFiles', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    const source = makeSource(['config.yaml', 'prompt.md', 'scripts/run.sh']);
    const el = await mountBrowser(source);

    const box = el.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;

    input.value = 'ext:yaml,sh';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await el.updateComplete;

    expect(renderedPaths(el)).toEqual(['config.yaml', 'scripts/run.sh']);
  });

  it('supplies loaded file extensions as dynamic value suggestions for ext:', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    const source = makeSource(['src/a.ts', 'pkg/b.go', 'README.md']);
    const el = await mountBrowser(source);

    const box = el.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    expect(box.valueSuggestions?.('ext', '')).toEqual(['go', 'md', 'ts']);
    expect(box.valueSuggestions?.('other', '')).toEqual([]);
  });

  it('keeps truncation visible in the count label for key-only queries when initial listing hasMore', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    const searchFiles = vi.fn(() => Promise.resolve({ files: [], totalSize: 0, totalCount: 0 }));
    const files = [
      makeEntry('pkg/server.go'),
      makeEntry('pkg/server_test.go'),
      makeEntry('dist/app.map'),
    ];
    const source: FileBrowserDataSource = {
      listFiles: vi.fn(() =>
        Promise.resolve({
          files,
          totalSize: 30,
          totalCount: 5000,
          hasMore: true,
        })
      ),
      searchFiles,
      deleteFile: vi.fn(),
      uploadFiles: vi.fn(),
      getDownloadUrl: (p) => `/download/${p}`,
      getPreviewUrl: (p) => `/preview/${p}`,
    };
    const el = await mountBrowser(source);

    const box = el.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;

    const cases: Array<{ query: string; expectedPaths: string[]; expectedPrefix: string }> = [
      {
        query: 'ext:go',
        expectedPaths: ['pkg/server_test.go', 'pkg/server.go'],
        expectedPrefix: '2 matches in the 3 most recent files',
      },
      {
        query: '-ext:map',
        expectedPaths: ['pkg/server_test.go', 'pkg/server.go'],
        expectedPrefix: '2 matches in the 3 most recent files',
      },
      {
        query: 'ext:go -test',
        expectedPaths: ['pkg/server.go'],
        expectedPrefix: '1 match in the 3 most recent files',
      },
    ];

    for (const tc of cases) {
      input.value = tc.query;
      input.dispatchEvent(new Event('input', { bubbles: true }));
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;

      expect(renderedPaths(el)).toEqual(tc.expectedPaths);
      const label = el.shadowRoot?.querySelector('.toolbar-meta')?.textContent?.trim() ?? '';
      expect(label).toContain(tc.expectedPrefix);
      expect(label).toContain('add text to search the whole workspace');
    }

    expect(searchFiles).not.toHaveBeenCalled();
  });

  it('keeps stale backend rows hidden while a second search is held pending', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    let resolveSecond: ((r: FileListResult) => void) | null = null;
    const searchFiles = vi.fn((q: string) => {
      if (q === 'foo') {
        return Promise.resolve({
          files: [makeEntry('deep/foo.ts')],
          totalSize: 10,
          totalCount: 1,
        });
      }
      return new Promise<FileListResult>((resolve) => {
        resolveSecond = resolve;
      });
    });
    const source = makeSource(['src/other.ts'], searchFiles);
    const el = await mountBrowser(source);

    const box = el.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;

    // First search resolves with deep/foo.ts
    input.value = 'foo';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;
    expect(renderedPaths(el)).toEqual(['deep/foo.ts']);

    // Second search ("bar") is committed and held pending: stale deep/foo.ts must stay hidden
    input.value = 'bar';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await el.updateComplete;
    expect(searchFiles).toHaveBeenCalledTimes(2);
    expect(renderedPaths(el)).toEqual([]);

    // Resolve the second search and confirm its result appears
    resolveSecond?.({
      files: [makeEntry('deep/bar.ts')],
      totalSize: 10,
      totalCount: 1,
    });
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;
    expect(renderedPaths(el)).toEqual(['deep/bar.ts']);
  });

  it('preserves a server-only fuzzy hit through the post-filter branch while enforcing ext:', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    // Simulate a server-only hit ("deep/srv_hit.ts") whose path does not match the client
    // regex for "button" — it must survive postFilterFilesByQuery once settled, while
    // "deep/srv_hit.go" is excluded by ext:ts.
    const searchFiles = vi.fn(() =>
      Promise.resolve({
        files: [makeEntry('deep/srv_hit.ts'), makeEntry('deep/srv_hit.go')],
        totalSize: 20,
        totalCount: 2,
      })
    );
    const source = makeSource(['src/local.md'], searchFiles);
    const el = await mountBrowser(source);

    const box = el.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;

    input.value = 'ext:ts button';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    expect(searchFiles).toHaveBeenCalledTimes(1);
    expect(searchFiles.mock.calls[0]?.[0]).toBe('button');
    expect(renderedPaths(el)).toEqual(['deep/srv_hit.ts']);
  });
});
