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
 * Health dashboard runtime broker table (ptone/scion#3582).
 */

import { describe, it, expect, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import {
  ScionHealthBrokerTable,
  agentsCell,
  sortBrokers,
  storageCell,
  type HealthSummaryBroker,
  type HealthSummaryBrokerList,
} from './health-broker-table.js';
import { elementStyleRules } from './__fixtures__/card-layout.js';

function broker(over: Partial<HealthSummaryBroker> = {}): HealthSummaryBroker {
  return {
    id: 'b1',
    name: 'alpha',
    version: '1.2.3',
    status: 'online',
    last_heartbeat: null,
    runtime: { type: 'docker', profile: 'docker' },
    workspace_storage: { backend: 'local' },
    agents: { running: 2, attention: 0 },
    ...over,
  };
}

function list(items: HealthSummaryBroker[], over: Partial<HealthSummaryBrokerList> = {}) {
  return { items, total: items.length, truncated: false, ...over };
}

const mounted: HTMLElement[] = [];

async function mount(brokers: HealthSummaryBrokerList | null): Promise<ShadowRoot> {
  const el = document.createElement('scion-health-broker-table') as ScionHealthBrokerTable;
  el.brokers = brokers;
  document.body.appendChild(el);
  mounted.push(el);
  await el.updateComplete;
  return el.shadowRoot as ShadowRoot;
}

function rows(root: ShadowRoot): HTMLTableRowElement[] {
  return [...root.querySelectorAll<HTMLTableRowElement>('tbody tr')];
}

function cell(row: Element, cls: string): string {
  return (row.querySelector(`td.${cls}`)?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

afterEach(() => {
  while (mounted.length) mounted.pop()?.remove();
});

describe('scion-health-broker-table cells', () => {
  it('links the name to the broker detail page', async () => {
    const root = await mount(list([broker({ id: 'b/1', name: 'alpha' })]));
    const a = rows(root)[0]?.querySelector('td.name a');
    expect(a?.textContent?.trim()).toBe('alpha');
    expect(a?.getAttribute('href')).toBe('/brokers/b%2F1');
  });

  it('shows a dash when the broker reported no runtime', async () => {
    const root = await mount(list([broker({ runtime: null })]));
    expect(cell(rows(root)[0]!, 'runtime')).toBe('—');
    expect(root.textContent).not.toMatch(/unknown/);
    expect(root.textContent).not.toMatch(/✗/);
  });

  it('shows the runtime type of the default profile', async () => {
    const root = await mount(list([broker({ runtime: { type: 'kubernetes', profile: 'gke' } })]));
    expect(cell(rows(root)[0]!, 'runtime')).toBe('kubernetes');
  });

  it('shows never for a broker that never sent a heartbeat', async () => {
    const root = await mount(list([broker({ last_heartbeat: null })]));
    expect(cell(rows(root)[0]!, 'heartbeat')).toBe('never');
  });

  it('shows a relative age for a reported heartbeat', async () => {
    const root = await mount(
      list([broker({ last_heartbeat: new Date(Date.now() - 30_000).toISOString() })])
    );
    expect(cell(rows(root)[0]!, 'heartbeat')).toMatch(/ago$/);
  });

  it('shows NFS only on NFS-backed brokers', async () => {
    const root = await mount(
      list([
        broker({ id: 'l', name: 'a-local', workspace_storage: { backend: 'local' } }),
        broker({
          id: 'n',
          name: 'b-nfs',
          workspace_storage: { backend: 'nfs', nfs_healthy: true },
        }),
        broker({ id: 'u', name: 'c-unreported', workspace_storage: null }),
      ])
    );
    const byId = new Map(rows(root).map((r) => [r.dataset.brokerId, cell(r, 'storage')]));
    expect(byId.get('l')).toBe('local');
    expect(byId.get('n')).toBe('NFS ✓');
    expect(byId.get('u')).toBe('');
    const nfsRows = rows(root).filter((r) => /NFS/.test(r.textContent ?? ''));
    expect(nfsRows.map((r) => r.dataset.brokerId)).toEqual(['n']);
  });

  it('marks an unhealthy NFS share', () => {
    expect(
      storageCell(broker({ workspace_storage: { backend: 'nfs', nfs_healthy: false } }))
    ).toEqual({ text: 'NFS ✗', tone: 'bad' });
    expect(storageCell(broker({ workspace_storage: { backend: 'nfs' } }))).toEqual({
      text: 'NFS',
      tone: 'plain',
    });
  });

  it('shows running / needing attention, version and status', async () => {
    const root = await mount(list([broker({ agents: { running: 4, attention: 1 }, version: '' })]));
    const r = rows(root)[0]!;
    expect(cell(r, 'agents .counts')).toBe('4 / 1');
    const td = r.querySelector('td.agents')!;
    // Screen readers get the counts spelled out; the visual "4 / 1" is hidden from them.
    expect(td.querySelector('.counts')?.getAttribute('aria-hidden')).toBe('true');
    expect(td.querySelector('.visually-hidden')?.textContent?.trim()).toBe(
      '4 running, 1 needing attention'
    );
    expect(td.getAttribute('title')).toBe('4 running, 1 needing attention');
    expect(td.querySelector('.attention')?.classList.contains('tone-warn')).toBe(true);
    expect(cell(r, 'version')).toBe('—');
    expect(cell(r, 'status')).toBe('online');
  });
});

describe('scion-health-broker-table neutral values', () => {
  it('shows a dash in the agents cell when agents are not reported', async () => {
    const root = await mount(list([broker({ agents: undefined })]));
    expect(cell(rows(root)[0]!, 'agents')).toBe('—');
  });

  it('keeps the dash when agents is an empty object', async () => {
    const root = await mount(list([broker({ agents: {} })]));
    expect(cell(rows(root)[0]!, 'agents')).toBe('—');
  });

  it('shows zero attention without a warning tone', async () => {
    const root = await mount(list([broker({ agents: { running: 0, attention: 0 } })]));
    const r = rows(root)[0]!;
    expect(cell(r, 'agents .counts')).toBe('0 / 0');
    expect(r.querySelector('td.agents .attention')?.classList.contains('tone-warn')).toBe(false);
  });

  it('agentsCell needs both counts', () => {
    expect(agentsCell(broker({ agents: { running: 3 } }))).toBeNull();
    expect(agentsCell(broker({ agents: { running: 3, attention: 2 } }))).toEqual({
      running: 3,
      attention: 2,
      title: '3 running, 2 needing attention',
    });
  });

  it('shows a degraded broker as a problem with a warning status', async () => {
    const root = await mount(
      list([broker({ id: 'a', name: 'a' }), broker({ id: 'd', name: 'z', status: 'degraded' })])
    );
    expect(rows(root)[0]?.dataset.brokerId).toBe('d');
    expect(rows(root)[0]?.querySelector('td.status .tone-warn')?.textContent?.trim()).toBe(
      'degraded'
    );
  });

  it('renders no empty title attributes', async () => {
    const root = await mount(list([broker({ runtime: null, last_heartbeat: null })]));
    const r = rows(root)[0]!;
    expect(r.querySelector('td.runtime')?.hasAttribute('title')).toBe(false);
    expect(r.querySelector('td.heartbeat')?.hasAttribute('title')).toBe(false);
  });

  it('titles the runtime with its profile and the heartbeat with its instant', async () => {
    const at = '2026-10-06T11:59:30Z';
    const root = await mount(
      list([broker({ runtime: { type: 'docker', profile: 'fast' }, last_heartbeat: at })])
    );
    const r = rows(root)[0]!;
    expect(r.querySelector('td.runtime')?.getAttribute('title')).toBe('profile fast');
    expect(r.querySelector('td.heartbeat')?.getAttribute('title')).toBe(at);
  });
});

describe('scion-health-broker-table ordering and truncation', () => {
  it('sorts problems first, then by name', () => {
    const sorted = sortBrokers([
      broker({ id: '1', name: 'delta' }),
      broker({ id: '2', name: 'charlie', status: 'offline' }),
      broker({ id: '3', name: 'alpha' }),
      broker({ id: '4', name: 'bravo', workspace_storage: { backend: 'nfs', nfs_healthy: false } }),
    ]);
    expect(sorted.map((b) => b.name)).toEqual(['bravo', 'charlie', 'alpha', 'delta']);
  });

  it('shows the truncation note only when truncated', async () => {
    const items = [broker({ id: '1' }), broker({ id: '2', name: 'beta' })];
    let root = await mount(list(items, { total: 140, truncated: true }));
    expect(root.querySelector('.note')?.textContent?.trim()).toBe('Showing 2 of 140');
    root = await mount(list(items));
    expect(root.querySelector('.note')).toBeNull();
  });

  it('shows an empty state with no brokers', async () => {
    for (const value of [null, list([])]) {
      const root = await mount(value);
      expect(root.querySelector('table')).toBeNull();
      expect(root.textContent).toMatch(/No runtime brokers registered/);
    }
  });
});

describe('scion-health-broker-table layout', () => {
  for (const n of [1, 2, 13]) {
    it(`renders ${n} broker(s) as rows of one table`, async () => {
      const items = Array.from({ length: n }, (_, i) =>
        broker({ id: `b${i}`, name: `broker-${String(i).padStart(2, '0')}` })
      );
      const root = await mount(list(items));
      expect(root.querySelectorAll('table')).toHaveLength(1);
      expect(rows(root)).toHaveLength(n);
      for (const r of rows(root)) expect(r.querySelectorAll('td')).toHaveLength(7);
      // No per-broker card that could grow to fill the row on its own.
      expect(root.querySelector('.broker-card, .broker-grid')).toBeNull();
    });
  }

  it('has no growing flex items and lets long names wrap', () => {
    const rules = elementStyleRules('scion-health-broker-table');
    for (const body of rules.values()) expect(body).not.toMatch(/flex(-grow)?:\s*1/);
    expect(rules.get('td.name') ?? '').toMatch(/overflow-wrap:\s*anywhere/);
  });

  it('uses theme tokens only, with no hex fallbacks', () => {
    const rules = elementStyleRules('scion-health-broker-table');
    for (const body of rules.values()) {
      expect(body).not.toMatch(/#[0-9a-f]{3,8}\b/i);
      for (const m of body.matchAll(/var\((--[\w-]+)/g)) expect(m[1]).toMatch(/^--scion-/);
    }
  });
});

// ---------------------------------------------------------------------------
// Contrast: resolve the colour tokens the table uses from theme.css, in the
// light theme and the dark theme, and check WCAG AA (4.5:1) against the card
// surface. Translucent badge backgrounds are composited onto the surface.
// ---------------------------------------------------------------------------

type RGBA = [number, number, number, number];

function block(cssText: string, selectorStart: string): string {
  const at = cssText.indexOf(selectorStart);
  if (at < 0) throw new Error(`no block ${selectorStart}`);
  const open = cssText.indexOf('{', at);
  let depth = 0;
  for (let i = open; i < cssText.length; i++) {
    if (cssText[i] === '{') depth++;
    if (cssText[i] === '}' && --depth === 0) return cssText.slice(open + 1, i);
  }
  throw new Error('unbalanced');
}

function decls(body: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const m of body.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) {
    out.set(m[1]!, m[2]!.trim());
  }
  return out;
}

function parseColor(v: string): RGBA {
  const hex = /^#([0-9a-f]{6})$/i.exec(v);
  if (hex) {
    const n = parseInt(hex[1]!, 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255, 1];
  }
  const rgba = /^rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)\s*(?:,\s*([\d.]+)\s*)?\)$/.exec(
    v
  );
  if (rgba) return [+rgba[1]!, +rgba[2]!, +rgba[3]!, rgba[4] === undefined ? 1 : +rgba[4]];
  throw new Error(`unparsed colour ${v}`);
}

function resolver(vars: Map<string, string>) {
  const get = (name: string, depth = 0): RGBA => {
    const v = vars.get(name);
    if (v === undefined || depth > 10) throw new Error(`unresolved ${name}`);
    const ref = /^var\((--[\w-]+)\)$/.exec(v);
    return ref ? get(ref[1]!, depth + 1) : parseColor(v);
  };
  return get;
}

function over(fg: RGBA, bg: RGBA): RGBA {
  const a = fg[3];
  return [0, 1, 2].map((i) => fg[i]! * a + bg[i]! * (1 - a)).concat(1) as RGBA;
}

function luminance([r, g, b]: RGBA): number {
  const lin = (c: number) => {
    const s = c / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

function contrast(a: RGBA, b: RGBA): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi! + 0.05) / (lo! + 0.05);
}

describe('scion-health-broker-table contrast', () => {
  const themeCss = readFileSync(resolve(__dirname, '../../styles/theme.css'), 'utf8');
  const light = decls(block(themeCss, '.sl-theme-light {'));
  const dark = new Map([...light, ...decls(block(themeCss, '.sl-theme-dark,'))]);
  const pairs: Array<[string, string | null]> = [
    ['--scion-text', null],
    ['--scion-text-muted', null],
    ['--scion-badge-success-text', '--scion-badge-success-bg'],
    ['--scion-badge-danger-text', '--scion-badge-danger-bg'],
    ['--scion-badge-warning-text', '--scion-badge-warning-bg'],
    ['--scion-badge-neutral-text', '--scion-badge-neutral-bg'],
  ];

  for (const [theme, vars] of [
    ['light', light],
    ['dark', dark],
  ] as const) {
    it(`passes AA in the ${theme} theme`, () => {
      const get = resolver(vars);
      const surface = get('--scion-surface');
      for (const [fg, bg] of pairs) {
        const back = bg ? over(get(bg), surface) : surface;
        const ratio = contrast(over(get(fg), back), back);
        expect(ratio, `${fg} on ${bg ?? '--scion-surface'}`).toBeGreaterThanOrEqual(4.5);
      }
    });
  }
});
