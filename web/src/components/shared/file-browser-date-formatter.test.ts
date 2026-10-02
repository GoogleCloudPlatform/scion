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
 * scion-file-browser — shared date formatter (ptone/scion#2382).
 *
 * Each row's "Modified" column used to construct a fresh
 * Intl.DateTimeFormat on every render. CPU samples attributed roughly
 * 160-174ms of a large listing's render time to that. formatDate() now
 * reuses a single formatter built once at module load. These tests pin the
 * fix (no new construction, however many rows/renders) and confirm the
 * displayed formatting and invalid-date fallback are unchanged.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

import type { FileEntry, FileListResult, FileBrowserDataSource } from './file-browser.js';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let FileBrowserCtor: any;

beforeAll(async () => {
  const mod = await import('./file-browser.js');
  FileBrowserCtor = mod.ScionFileBrowser;
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.restoreAllMocks();
});

function makeEntry(path: string, modTime: string): FileEntry {
  return { path, size: 10, modTime, mode: '-rw-r--r--' };
}

function makeSource(entries: FileEntry[]) {
  const source: FileBrowserDataSource = {
    listFiles: vi.fn(() =>
      Promise.resolve<FileListResult>({
        files: entries,
        totalSize: entries.reduce((s, f) => s + f.size, 0),
        totalCount: entries.length,
      })
    ),
    deleteFile: vi.fn(),
    uploadFiles: vi.fn(),
    getDownloadUrl: () => '',
    getPreviewUrl: () => '',
  };
  return source;
}

async function mountWithFiles(entries: FileEntry[]) {
  const el = new FileBrowserCtor();
  el.dataSource = makeSource(entries);
  document.body.appendChild(el);
  await el.updateComplete;
  await el.updateComplete;
  return el as InstanceType<typeof FileBrowserCtor> & { shadowRoot: ShadowRoot };
}

function dateCellsText(el: { shadowRoot: ShadowRoot }): string[] {
  return Array.from(el.shadowRoot.querySelectorAll<HTMLElement>('.file-date')).map(
    (n) => n.textContent ?? ''
  );
}

describe('scion-file-browser — shared date formatter', () => {
  it('does not construct a new Intl.DateTimeFormat while rendering rows', async () => {
    // FILE_DATE_FORMATTER is built once at module import time (already
    // happened before this spy attaches), so any additional construction
    // observed here would come from formatDate() — the exact regression
    // this fix prevents.
    const ctorSpy = vi.spyOn(Intl, 'DateTimeFormat');

    const entries = Array.from({ length: 200 }, (_, i) =>
      makeEntry(`file-${i}.txt`, '2026-03-14T09:41:00Z')
    );
    const el = await mountWithFiles(entries);

    expect(dateCellsText(el).length).toBe(200);
    expect(ctorSpy).not.toHaveBeenCalled();

    // Re-render several times (sort toggles, filter changes, etc. all
    // re-invoke formatDate() per row) — still zero new constructions.
    for (let i = 0; i < 5; i++) {
      el.requestUpdate();
      await el.updateComplete;
    }
    expect(ctorSpy).not.toHaveBeenCalled();
  });

  it('formats a valid date exactly as the pre-fix per-row formatter did', async () => {
    const modTime = '2026-03-14T09:41:00Z';
    // `expected` is a second, independent Intl.DateTimeFormat instance built
    // with the exact same locale/options formatDate() uses — not a spy or a
    // mock of anything, just a plain reference value to compare against.
    // Asserting exact equality against it (rather than a loose substring
    // match) means a change to the options — dropping hour/minute, changing
    // the locale, etc. — fails this test even if the output happened to
    // still contain "Mar 14, 2026".
    const expected = new Intl.DateTimeFormat('en', {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
    }).format(new Date(modTime));

    const el = await mountWithFiles([makeEntry('a.txt', modTime)]);
    const [text] = dateCellsText(el);
    expect(text).toBe(expected);
  });

  it('falls back to the raw string for an invalid date', async () => {
    const el = await mountWithFiles([makeEntry('bad.txt', 'not-a-real-date')]);
    const [text] = dateCellsText(el);
    expect(text).toBe('not-a-real-date');
  });

  it('never calls the formatter for an invalid date — no throw/catch on the hot path', async () => {
    // GoogleCloudPlatform/scion#2176 review: Intl.DateTimeFormat#format()
    // throws a RangeError for an invalid Date, and throwing/catching is
    // comparatively expensive per row on a large listing. formatDate() now
    // checks getTime() up front and returns before ever calling format(),
    // rather than relying on catching that exception.
    // `format` is a getter (returns a bound formatting function), not a
    // plain method — spy on the accessor itself, since even *accessing*
    // FILE_DATE_FORMATTER.format only happens on the path that goes on to
    // call it.
    const formatSpy = vi.spyOn(Intl.DateTimeFormat.prototype, 'format', 'get');
    const el = await mountWithFiles([makeEntry('bad.txt', 'not-a-real-date')]);
    const [text] = dateCellsText(el);
    // Output is unchanged...
    expect(text).toBe('not-a-real-date');
    // ...but it's no longer produced by attempting-and-catching a throw.
    expect(formatSpy).not.toHaveBeenCalled();
  });

  // Mounting 1000 real rows (each with several Shoelace icon-buttons) in
  // happy-dom is inherently slower than the default 5s test timeout — that
  // is DOM/custom-element upgrade cost in the test environment, not the
  // formatter behavior under test, hence the longer explicit timeout below.
  const REPRESENTATIVE_ROW_COUNT_TIMEOUT_MS = 20_000;

  it(
    'renders a representative 1000-row listing without constructing new formatters',
    async () => {
      const ctorSpy = vi.spyOn(Intl, 'DateTimeFormat');
      const entries = Array.from({ length: 1000 }, (_, i) =>
        makeEntry(`dir/file-${i}.txt`, '2026-06-01T12:00:00Z')
      );
      const el = await mountWithFiles(entries);

      // The table caps rendered rows at 1000, so all of them get a
      // formatted date cell.
      expect(dateCellsText(el).length).toBe(1000);
      expect(ctorSpy).not.toHaveBeenCalled();
    },
    REPRESENTATIVE_ROW_COUNT_TIMEOUT_MS
  );
});
