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
 * Files tab banner for the project storage migration (ptone/scion#5819):
 * shown when the workspace listing carries `migrationNotice`, absent
 * otherwise, and cleared when a later listing no longer carries it.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

import type { FileListResult, FileBrowserDataSource } from './file-browser.js';
import type { WorkspaceMigrationNotice } from './workspace-migration-notice.js';

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

const NOTICE: WorkspaceMigrationNotice = {
  message: "Some files of this project are still in the hub's previous storage location.",
  operationKey: 'project-storage-migrate',
};

function sourceReturning(results: FileListResult[]): FileBrowserDataSource {
  let i = 0;
  return {
    listFiles: vi.fn(() => Promise.resolve(results[Math.min(i++, results.length - 1)])),
    deleteFile: vi.fn(),
    uploadFiles: vi.fn(),
    getDownloadUrl: () => '/d',
    getPreviewUrl: () => '/p',
  };
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function mount(source: FileBrowserDataSource): Promise<any> {
  const el = new FileBrowserCtor();
  el.dataSource = source;
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
  return el;
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function bannerText(el: any): Promise<string | null> {
  const banner = el.shadowRoot.querySelector('scion-workspace-migration-notice');
  if (!banner) return null;
  await banner.updateComplete;
  const alert = banner.shadowRoot.querySelector('sl-alert');
  return alert ? alert.textContent.replace(/\s+/g, ' ').trim() : null;
}

describe('scion-workspace-migration-notice in the file browser', () => {
  it('shows the server notice and the operation key', async () => {
    const el = await mount(
      sourceReturning([{ files: [], totalSize: 0, totalCount: 0, migrationNotice: NOTICE }])
    );
    const text = await bannerText(el);
    expect(text).toContain('previous storage location');
    expect(text).toContain('project-storage-migrate');
  });

  it('shows nothing when the listing has no notice', async () => {
    const el = await mount(sourceReturning([{ files: [], totalSize: 0, totalCount: 0 }]));
    expect(await bannerText(el)).toBeNull();
  });

  it('clears the banner when a reload no longer carries the notice', async () => {
    const el = await mount(
      sourceReturning([
        { files: [], totalSize: 0, totalCount: 0, migrationNotice: NOTICE },
        { files: [], totalSize: 0, totalCount: 0 },
      ])
    );
    expect(await bannerText(el)).not.toBeNull();
    await el.loadFiles();
    await el.updateComplete;
    expect(await bannerText(el)).toBeNull();
  });
});
