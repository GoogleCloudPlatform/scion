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
 * 160-174ms of a large listing's render time to that. formatDate() now goes
 * through `time.ts`'s `formatInstant`, which builds one formatter per style
 * and display zone and reuses it. These tests pin the fix (after the first
 * render, no new construction however many rows/renders), the display-zone
 * formatting (tz-refactor task 21) and the invalid-date fallback.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { setPreferredTimeZone } from '../../utils/time.js';

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
  setPreferredTimeZone('');
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

/**
 * Renders one row so `formatInstant` has built its formatter for the
 * current zone (the first render); later renders must reuse it.
 */
async function warmFormatterCache(): Promise<void> {
  const el = await mountWithFiles([makeEntry('warm.txt', '2026-01-01T00:00:00Z')]);
  el.remove();
}

function dateCellsText(el: { shadowRoot: ShadowRoot }): string[] {
  return Array.from(el.shadowRoot.querySelectorAll<HTMLElement>('.file-date')).map(
    (n) => n.textContent ?? ''
  );
}

describe('scion-file-browser — shared date formatter', () => {
  it('does not construct a new Intl.DateTimeFormat per row or per render', async () => {
    // formatInstant builds one formatter per style and zone. A first render
    // warms it; after that, rendering any number of rows must build none —
    // the exact regression this fix prevents. A display preference is set,
    // so the zone itself needs no lookup (see the auto-zone test below).
    setPreferredTimeZone('Asia/Tokyo');
    await warmFormatterCache();
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

  it('with no preference (Auto), resolves the browser zone once per render, not per row', async () => {
    await warmFormatterCache();
    const ctorSpy = vi.spyOn(Intl, 'DateTimeFormat');
    const entries = Array.from({ length: 200 }, (_, i) =>
      makeEntry(`file-${i}.txt`, '2026-03-14T09:41:00Z')
    );
    const el = await mountWithFiles(entries);
    expect(dateCellsText(el).length).toBe(200);
    const afterMount = ctorSpy.mock.calls.length;
    // mountWithFiles may take a couple of render passes; bounded by those,
    // never by the 200 rows.
    expect(afterMount).toBeLessThanOrEqual(3);

    ctorSpy.mockClear();
    el.requestUpdate();
    await el.updateComplete;
    expect(ctorSpy.mock.calls.length).toBeLessThanOrEqual(1);
  });

  it('formats in the display zone, 24-hour, with midnight as 00:00', async () => {
    // vitest pins the browser zone to UTC; the preference differs from it.
    setPreferredTimeZone('Asia/Tokyo');
    // 15:00Z is midnight the next day in Tokyo (+09:00).
    const el = await mountWithFiles([makeEntry('a.txt', '2026-03-14T15:00:00Z')]);
    const [text] = dateCellsText(el);
    expect(text).toBe('Mar 15, 2026, 00:00');
    const header = el.shadowRoot.querySelector('th .zone-label');
    expect(header?.textContent).toBe('(Asia/Tokyo)');
  });

  it('re-renders the column when the display zone changes', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = await mountWithFiles([makeEntry('a.txt', '2026-03-14T15:00:00Z')]);
    expect(dateCellsText(el)).toEqual(['Mar 15, 2026, 00:00']);

    setPreferredTimeZone('America/New_York');
    await el.updateComplete;
    expect(dateCellsText(el)).toEqual(['Mar 14, 2026, 11:00']);
    expect(el.shadowRoot.querySelector('th .zone-label')?.textContent).toBe('(America/New_York)');
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
    'renders a representative 1000-row listing without a formatter per row',
    async () => {
      setPreferredTimeZone('Asia/Tokyo');
      await warmFormatterCache();
      const ctorSpy = vi.spyOn(Intl, 'DateTimeFormat');
      const entries = Array.from({ length: 1000 }, (_, i) =>
        makeEntry(`dir/file-${i}.txt`, '2026-06-01T12:00:00Z')
      );
      const el = await mountWithFiles(entries);

      // The table caps rendered rows at 1000, so all of them get a
      // formatted date cell, and none of them builds a formatter.
      expect(dateCellsText(el).length).toBe(1000);
      expect(ctorSpy).not.toHaveBeenCalled();
    },
    REPRESENTATIVE_ROW_COUNT_TIMEOUT_MS
  );
});
