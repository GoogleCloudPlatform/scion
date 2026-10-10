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
 * Resource detail pages render their header through the shared
 * scion-detail-header (ptone/scion#3856), which owns the wrapping layout
 * (detail-header.test.ts): a long name breaks inside the h1, the badges
 * follow it onto the next line, the icon keeps its size and the actions
 * drop below the title on a narrow screen. These tests check each page
 * hands its parts to the right slots and keeps no copy of the header CSS.
 * The agent page is covered in agent-detail-layout.test.ts.
 */

import { describe, it, expect, beforeAll, vi } from 'vitest';
import { render, type CSSResult, type TemplateResult } from 'lit';

import { styleRules } from './__fixtures__/css-rules.js';

// Some page module graphs reach the app entry point, which bootstraps the
// SPA on load; stub it as the agent page tests do.
vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

const LONG_NAME = 'a-very-long-resource-name-that-will-not-fit-on-one-line-beside-its-badges';

interface PageCase {
  /** Page module, relative to this file. */
  module: string;
  tag: string;
  /** Private state that lets the page render its header. */
  state: Record<string, unknown>;
  /** Page method that returns the header template. */
  method: 'render' | 'renderHeader' | 'renderDetail' | 'renderPageHeader';
  /** Arguments for `method`, if it takes any. */
  args?: unknown[];
  /** The slotted icon, as tag.class[name]; null for a page with no icon. */
  icon: string | null;
  /** Tags of the badges (unslotted children), in order. */
  badges: string[];
  /** Classes of the meta slot's elements, in order. */
  meta: string[];
  /** State overrides under which no header action renders. */
  noActions?: Record<string, unknown>;
}

const CAPS = { _capabilities: { actions: ['read', 'update', 'delete'] } };

const ROLE = {
  id: 'role-1',
  name: LONG_NAME,
  description: 'Edits things',
  scopeType: 'hub',
  permissions: [],
  system: false,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

// Only the fields the access constraint header reads.
const BOUNDARY = {
  id: 'ab-1',
  name: LONG_NAME,
  status: 'active',
  risk: ['tightening'],
  scope: { type: 'system' },
  subject: { kind: 'all_principals' },
  updatedAt: '2026-01-01T00:00:00Z',
  updatedBy: { id: 'u-1', type: 'user', displayName: 'Ada' },
  _capabilities: { actions: ['read', 'previewTighten', 'delete'] },
};

const cases: Array<[string, PageCase]> = [
  [
    'broker',
    {
      module: './broker-detail.js',
      tag: 'scion-page-broker-detail',
      state: {
        loading: false,
        pageData: { path: '/brokers/b-1', user: { id: 'u', role: 'admin' } },
        broker: {
          id: 'b-1',
          name: LONG_NAME,
          status: 'online',
          version: '1.2.3',
          labels: { 'scion.io/broker-type': 'hosted' },
          ...CAPS,
        },
      },
      method: 'render',
      icon: 'sl-icon.[hdd-rack]',
      badges: ['span', 'scion-status-badge'],
      meta: ['header-subtitle'],
      noActions: { pageData: { path: '/brokers/b-1', user: { id: 'u', role: 'member' } } },
    },
  ],
  [
    'skill',
    {
      module: './skill-detail.js',
      tag: 'scion-page-skill-detail',
      state: {
        loading: false,
        skill: { id: 's-1', name: LONG_NAME, status: 'active', scope: 'global', ...CAPS },
      },
      method: 'renderHeader',
      icon: 'sl-icon.[lightning-charge]',
      badges: ['scion-status-badge'],
      meta: ['header-meta'],
      noActions: {
        skill: {
          id: 's-1',
          name: LONG_NAME,
          status: 'active',
          scope: 'global',
          _capabilities: { actions: ['read'] },
        },
      },
    },
  ],
  [
    'group',
    {
      module: './admin-group-detail.js',
      tag: 'scion-page-admin-group-detail',
      state: {
        loading: false,
        group: {
          id: 'g-1',
          name: LONG_NAME,
          slug: 'g',
          groupType: 'explicit',
          created: '2026-01-01T00:00:00Z',
          updated: '2026-01-01T00:00:00Z',
          ...CAPS,
        },
      },
      method: 'render',
      icon: 'div.group-icon explicit[]',
      badges: ['span'],
      meta: ['header-slug'],
      noActions: {
        group: {
          id: 'g-1',
          name: LONG_NAME,
          slug: 'g',
          groupType: 'explicit',
          created: '2026-01-01T00:00:00Z',
          updated: '2026-01-01T00:00:00Z',
          _capabilities: { actions: ['read'] },
        },
      },
    },
  ],
  [
    'project',
    {
      module: './project-detail.js',
      tag: 'scion-page-project-detail',
      state: {
        loading: false,
        project: { id: 'p-1', name: LONG_NAME, slug: 'p', projectType: 'linked', ...CAPS },
      },
      method: 'render',
      icon: 'sl-icon.[folder-fill]',
      badges: ['sl-tooltip'],
      meta: ['header-path'],
    },
  ],
  [
    'template',
    {
      module: './template-detail.js',
      tag: 'scion-page-template-detail',
      state: {
        loading: false,
        template: {
          id: 't-1',
          name: LONG_NAME,
          description: 'd',
          harness: 'claude',
          scope: 'global',
          sourceUrl: 'https://github.com/example/templates',
          ...CAPS,
        },
      },
      method: 'renderHeader',
      icon: 'sl-icon.[file-earmark-code]',
      badges: ['span'],
      meta: ['template-description', 'template-meta-row'],
      // Refresh from Source shows only for a GitHub source.
      noActions: {
        template: {
          id: 't-1',
          name: LONG_NAME,
          harness: 'claude',
          scope: 'global',
          ...CAPS,
        },
      },
    },
  ],
  [
    'harness config',
    {
      module: './harness-config-detail.js',
      tag: 'scion-page-harness-config-detail',
      state: {
        loading: false,
        harnessConfig: {
          id: 'h-1',
          name: LONG_NAME,
          description: 'd',
          harness: 'claude',
          scope: 'global',
          sourceUrl: 'https://example.com/hc',
          _capabilities: { actions: ['read', 'delete'] },
        },
      },
      method: 'renderHeader',
      icon: 'sl-icon.[sliders]',
      badges: ['span'],
      // The description sits in the title column, beside the actions.
      meta: ['resource-description', 'resource-meta-row'],
      noActions: {
        harnessConfig: {
          id: 'h-1',
          name: LONG_NAME,
          harness: 'claude',
          scope: 'global',
          _capabilities: { actions: ['read'] },
        },
      },
    },
  ],
  [
    'skill registry',
    {
      module: './admin-skill-registry-detail.js',
      tag: 'scion-page-admin-skill-registry-detail',
      state: {
        loading: false,
        registry: { id: 'r-1', name: LONG_NAME, status: 'active', trustLevel: 'open' },
      },
      method: 'renderHeader',
      icon: 'sl-icon.[cloud-arrow-down]',
      badges: [],
      meta: [],
    },
  ],
  [
    'artifact',
    {
      module: './artifact-detail.js',
      tag: 'scion-page-artifact-detail',
      state: {
        data: {
          artifact: {
            id: 'a-1',
            ref: 'scion://artifact/a-1',
            scopeKind: 'project',
            scopeRef: 'p-1',
            ownerKind: 'user',
            ownerRef: 'u-1',
            title: LONG_NAME,
            currentSeq: 1,
            createdAt: '2026-01-01T00:00:00Z',
            updatedAt: '2026-01-01T00:00:00Z',
          },
          version: {
            seq: 1,
            ref: 'scion://artifact/a-1@1',
            kind: 'publish',
            entryPath: 'a.md',
            totalBytes: 5,
            fileCount: 1,
            createdAt: '2026-01-01T00:00:00Z',
            state: 'ready',
            files: [{ path: 'a.md', size: 5, sha256: 'ab', mediaType: 'text/markdown' }],
          },
        },
      },
      method: 'renderHeader',
      icon: 'sl-icon.[file-earmark-richtext]',
      badges: [],
      meta: ['meta'],
      // The actions hide while the artifact is being edited.
      noActions: { editing: true },
    },
  ],
  [
    'gcp service account',
    {
      module: './gcp-service-account-detail.js',
      tag: 'scion-page-gcp-service-account-detail',
      state: {
        loading: false,
        account: {
          id: 'sa-1',
          email: `${LONG_NAME}@example-project.iam.gserviceaccount.com`,
          displayName: 'Builder',
          scope: 'hub',
          _capabilities: { actions: ['read', 'verify', 'delete'] },
        },
      },
      method: 'render',
      icon: null,
      badges: [],
      meta: ['display-name'],
      noActions: {
        account: {
          id: 'sa-1',
          email: `${LONG_NAME}@example-project.iam.gserviceaccount.com`,
          scope: 'hub',
          _capabilities: { actions: ['read'] },
        },
      },
    },
  ],
  [
    'role',
    {
      module: './admin-role-detail.js',
      tag: 'scion-page-admin-role-detail',
      state: { loading: false, roleData: ROLE },
      method: 'renderDetail',
      icon: null,
      badges: ['span', 'span'],
      meta: ['header-description', 'metadata-row'],
    },
  ],
  [
    'access constraint',
    {
      module: './admin-access-boundary-detail.js',
      tag: 'scion-page-admin-access-boundary-detail',
      state: { phase: 'ready', boundary: BOUNDARY },
      method: 'renderPageHeader',
      args: [BOUNDARY],
      icon: null,
      badges: ['scion-access-boundary-status'],
      meta: ['header-meta'],
      noActions: { boundary: { ...BOUNDARY, _capabilities: { actions: ['read'] } } },
    },
  ],
];

/** Header layout selectors the pages used to define for themselves. */
const LAYOUT_SELECTORS = [
  '.header',
  '.header-info',
  '.header-title',
  '.header-title > sl-icon',
  '.header-title-text',
  '.header h1',
  '.header-actions',
  '.template-header',
  '.template-title',
  '.template-title h1',
  '.resource-header',
  '.resource-title',
  '.resource-title-main',
  '.resource-title h1',
  '.title',
  '.title h1',
  '.title sl-icon',
  '.actions',
  '.badges',
  '.page-header',
  '.header-main',
  '.header-badges',
  '.boundary-name',
];

const loaded = new Map<string, Map<string, string>>();
const rulesOf = (c: { tag: string }): Map<string, string> => loaded.get(c.tag)!;

beforeAll(async () => {
  for (const [, c] of cases) {
    await import(/* @vite-ignore */ c.module);
    const ctor = customElements.get(c.tag) as unknown as { elementStyles: CSSResult[] };
    loaded.set(c.tag, styleRules(ctor.elementStyles.map((s) => s.cssText).join('\n')));
  }
}, 60_000);

/** Render the page header for case `c` and return its scion-detail-header. */
function renderHeader(c: PageCase): HTMLElement & { heading: string } {
  const el = document.createElement(c.tag);
  Object.assign(el, c.state);
  const tpl = (el as unknown as Record<string, (...a: unknown[]) => TemplateResult>)[c.method](
    ...(c.args ?? [])
  );
  const host = document.createElement('div');
  render(tpl, host);
  const headers = host.querySelectorAll('scion-detail-header');
  expect(headers).toHaveLength(1);
  return headers[0] as HTMLElement & { heading: string };
}

const describeEl = (n: Element): string =>
  `${n.tagName.toLowerCase()}.${n.className}[${n.getAttribute('name') ?? ''}]`;

describe.each(cases)('%s detail header', (_label, c) => {
  it('renders the name as the shared header heading', () => {
    const header = renderHeader(c);
    expect(header.heading).toContain(LONG_NAME);
    // The h1 is the shared header's; the page adds none of its own.
    expect(header.querySelector('h1')).toBeNull();
  });

  it('hands the icon, badges, meta and actions to their slots', () => {
    const header = renderHeader(c);
    const children = Array.from(header.children);
    const inSlot = (name: string) => children.filter((n) => n.getAttribute('slot') === name);
    expect(inSlot('icon').map(describeEl)).toEqual(c.icon ? [c.icon] : []);
    expect(
      children.filter((n) => !n.hasAttribute('slot')).map((n) => n.tagName.toLowerCase())
    ).toEqual(c.badges);
    expect(inSlot('meta').map((n) => n.className)).toEqual(c.meta);
    const actions = inSlot('actions');
    expect(actions.map((n) => n.className)).toEqual(['header-actions']);
    expect(actions[0].querySelector('sl-button')).not.toBeNull();
    expect(
      children.every((n) => ['icon', 'meta', 'actions', null].includes(n.getAttribute('slot')))
    ).toBe(true);
  });

  it('keeps no copy of the header layout CSS', () => {
    const rules = rulesOf(c);
    for (const sel of LAYOUT_SELECTORS) expect(rules.has(sel), sel).toBe(false);
  });

  it('sets no inline width or icon style in the header', () => {
    const header = renderHeader(c);
    for (const node of [header, ...Array.from(header.querySelectorAll('[style]'))]) {
      expect(node.getAttribute('style') ?? '').not.toMatch(/(min-|max-)?width/);
    }
    // The icon takes its size from the shared header's stylesheet.
    expect(header.querySelector(':scope > [slot="icon"]')?.hasAttribute('style') ?? false).toBe(
      false
    );
  });
});

// Pages whose actions all depend on state render no actions wrapper when
// none applies: an empty wrapper would still be a flex item taking the row
// gap.
describe.each(cases.filter(([, c]) => c.noActions))(
  '%s detail header without actions',
  (_label, c) => {
    it('renders no actions wrapper', () => {
      const header = renderHeader({ ...c, state: { ...c.state, ...c.noActions } });
      expect(header.querySelector(':scope > [slot="actions"]')).toBeNull();
    });
  }
);

describe('project detail linked badge', () => {
  const c = cases.find(([label]) => label === 'project')![1];

  it('shows the linked marker as a badge in the primary colour', () => {
    expect(rulesOf(c).get('.linked-badge') ?? '').toMatch(/(^|;)\s*color:\s*var\(--scion-primary/);
    const icon = renderHeader(c).querySelector(':scope > sl-tooltip > sl-icon.linked-badge');
    expect(icon?.getAttribute('name')).toBe('link-45deg');
    expect(icon?.hasAttribute('style')).toBe(false);
  });

  it('shows no marker for a project that is not linked', () => {
    const header = renderHeader({
      ...c,
      state: { ...c.state, project: { id: 'p-2', name: LONG_NAME, slug: 'p', ...CAPS } },
    });
    expect(header.querySelector('sl-tooltip')).toBeNull();
  });
});
