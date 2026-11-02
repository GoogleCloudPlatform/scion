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
 * The Brokers area "Project storage" card: the reported layout rows, the
 * hub's layout comparison when it is known, redaction, and no controls.
 */

import { describe, it, expect, beforeAll, afterEach } from 'vitest';

import type { BrokerWorkspaceStorage } from '../../shared/types.js';
import { LAYOUT_MISMATCH, nfsStorage } from '../pages/__fixtures__/broker-area.js';

type Card = HTMLElement & {
  storage: BrokerWorkspaceStorage | null | undefined;
  mismatch: string[] | null;
  updateComplete: Promise<boolean>;
};

const mounted: HTMLElement[] = [];

async function mount(
  storage: BrokerWorkspaceStorage | undefined,
  mismatch: string[] | null
): Promise<ShadowRoot> {
  const el = document.createElement('scion-broker-storage-card') as Card;
  el.storage = storage;
  el.mismatch = mismatch;
  document.body.appendChild(el);
  mounted.push(el);
  await el.updateComplete;
  return el.shadowRoot!;
}

function rows(root: ShadowRoot): Record<string, string> {
  const dts = Array.from(root.querySelectorAll('dl.storage-rows dt'));
  const dds = Array.from(root.querySelectorAll('dl.storage-rows dd'));
  return Object.fromEntries(
    dts.map((dt, i) => [dt.textContent!.trim(), dds[i].textContent!.trim()])
  );
}

describe('scion-broker-storage-card', () => {
  beforeAll(async () => {
    await import('./broker-storage-card.js');
  }, 30_000);

  afterEach(() => {
    while (mounted.length) mounted.pop()!.remove();
  });

  it('shows the reported layout and where storage is set, read-only', async () => {
    const root = await mount(nfsStorage(), []);
    expect(root.querySelector('h2')?.textContent).toBe('Project storage');
    expect(rows(root)).toEqual({
      Backend: 'nfs',
      'NFS server': 'filer.example.internal',
      'NFS export': '/vol/scion',
      'Subpath root': 'projects',
      'Share health': 'healthy',
      Homes: 'nfs',
    });
    expect(root.textContent).not.toContain('export-marker');
    expect(root.querySelector('.storage-source-note')?.textContent).toContain('settings.yaml');
    expect(root.querySelector('.storage-source-note')?.textContent).toContain('restart');
    expect(root.querySelector('sl-input, sl-button, sl-select, input, button, form')).toBeNull();
  });

  it('shows the layout differences when the hub reports them', async () => {
    const root = await mount(nfsStorage(), LAYOUT_MISMATCH);
    const items = Array.from(root.querySelectorAll('.storage-mismatch-alert li')).map((li) =>
      li.textContent!.trim()
    );
    expect(items).toEqual(LAYOUT_MISMATCH);
    expect(root.querySelector('.storage-mismatch-alert')?.textContent).toContain(
      'Storage layout differs from the hub'
    );
  });

  it('shows no alert when the layouts match', async () => {
    const root = await mount(nfsStorage(), []);
    expect(root.querySelector('.storage-mismatch-alert')).toBeNull();
    expect(root.querySelector('.storage-compare-note')).toBeNull();
  });

  it('says the comparison is not available when it is not known', async () => {
    const root = await mount(nfsStorage(), null);
    expect(root.querySelector('.storage-mismatch-alert')).toBeNull();
    expect(root.querySelector('.storage-compare-note')?.textContent?.trim()).toBe(
      "The comparison with the hub's layout is not available."
    );
  });

  it('says no layout was reported for an older broker', async () => {
    const root = await mount(undefined, []);
    expect(root.querySelector('dl.storage-rows')).toBeNull();
    expect(root.querySelector('.storage-none')).not.toBeNull();
  });

  it('redacts userinfo in reported values and in difference lines', async () => {
    const storage = nfsStorage({
      nfs: { server: 'user:pw123@filer', export: '/e', subPathRoot: 'r', healthy: true },
    });
    const root = await mount(storage, ['nfs server: hub "filer", broker "user:pw123@filer"']);
    expect(rows(root)['NFS server']).toBe('***@filer');
    expect(root.querySelector('.storage-mismatch-alert li')?.textContent?.trim()).toBe(
      'nfs server: hub "filer", broker "***@filer"'
    );
    expect(root.textContent).not.toContain('pw123');
  });

  it('redacts a difference line whose value contains an escaped quote', async () => {
    const root = await mount(nfsStorage(), ['nfs server: hub "filer", broker "user:p\\"w@filer"']);
    expect(root.querySelector('.storage-mismatch-alert li')?.textContent?.trim()).toBe(
      'nfs server: hub "filer", broker "***@filer"'
    );
    expect(root.textContent).not.toContain('user:p');
    expect(root.textContent).not.toContain('\\"w@');
  });

  it('redacts the rest of a difference line after an unpaired quote', async () => {
    const root = await mount(nfsStorage(), ['nfs server: hub "filer", broker "user:pw@filer']);
    expect(root.querySelector('.storage-mismatch-alert li')?.textContent?.trim()).toBe(
      'nfs server: hub "***'
    );
    expect(root.textContent).not.toContain('user:pw');
  });
});
