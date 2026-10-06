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

/**
 * Detail page header rows wrap on narrow screens (ptone/scion#3386): the
 * icon keeps its size, a long name breaks inside its own line, and badges
 * follow on the next line instead of overflowing the page. Same pattern
 * as the agent page (agent-detail-layout.test.ts). jsdom does no layout,
 * so these check the compiled rules and the header markup.
 */

import { describe, it, expect, beforeAll, vi } from 'vitest';
import { render, type CSSResult, type TemplateResult } from 'lit';

// Some page module graphs reach the app entry point, which bootstraps the
// SPA on load; stub it as the agent page tests do.
vi.mock('../../client/main.js', () => ({
  navigateTo: vi.fn(),
  stateManager: new EventTarget(),
}));

const LONG_NAME = 'a-very-long-resource-name-that-will-not-fit-on-one-line-beside-its-badges';

/** Leaf style rules from Lit cssText (media blocks are flattened). */
function styleRules(cssText: string): Map<string, string> {
  const rules = new Map<string, string>();
  const stack: string[] = [];
  let buf = '';
  for (const ch of cssText.replace(/\/\*[\s\S]*?\*\//g, '')) {
    if (ch === '{') {
      stack.push(buf.trim());
      buf = '';
    } else if (ch === '}') {
      const selector = stack.pop() ?? '';
      if (!selector.startsWith('@')) {
        for (const part of selector.split(',')) rules.set(part.trim(), buf);
      }
      buf = '';
    } else {
      buf += ch;
    }
  }
  return rules;
}

interface PageCase {
  /** Page module, relative to this file. */
  module: string;
  tag: string;
  /** Selector of the row that holds the icon and the name. */
  row: string;
  /** Rule that sizes the icon. */
  icon: string;
  /** Rule for the page title. */
  h1: string;
  /** Tag (and class) of the icon element in the row. */
  iconTag: string;
  /** Private state that lets the page render its header. */
  state: Record<string, unknown>;
  /** Tags expected in the wrapping text row, in order. */
  text: string[];
  /** Page method that returns the header template. */
  method: 'render' | 'renderHeader';
}

const CAPS = { _capabilities: { actions: ['read'] } };

const cases: Array<[string, PageCase]> = [
  [
    'broker',
    {
      module: './broker-detail.js',
      tag: 'scion-page-broker-detail',
      row: '.header-title',
      icon: '.header-title > sl-icon',
      h1: '.header h1',
      iconTag: 'sl-icon.',
      state: {
        loading: false,
        broker: {
          id: 'b-1',
          name: LONG_NAME,
          status: 'online',
          labels: { 'scion.io/broker-type': 'hosted' },
          ...CAPS,
        },
      },
      text: ['h1', 'span', 'scion-status-badge'],
      method: 'render',
    },
  ],
  [
    'skill',
    {
      module: './skill-detail.js',
      tag: 'scion-page-skill-detail',
      row: '.header-title',
      icon: '.header-title > sl-icon',
      h1: '.header h1',
      iconTag: 'sl-icon.',
      state: {
        loading: false,
        skill: { id: 's-1', name: LONG_NAME, status: 'active', scope: 'global', ...CAPS },
      },
      text: ['h1', 'scion-status-badge'],
      method: 'renderHeader',
    },
  ],
  [
    'group',
    {
      module: './admin-group-detail.js',
      tag: 'scion-page-admin-group-detail',
      row: '.header-title',
      icon: '.group-icon',
      h1: '.header h1',
      iconTag: 'div.group-icon explicit',
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
      text: ['h1', 'span'],
      method: 'render',
    },
  ],
  [
    'project',
    {
      module: './project-detail.js',
      tag: 'scion-page-project-detail',
      row: '.header-title',
      icon: '.header-title > sl-icon',
      h1: '.header h1',
      iconTag: 'sl-icon.',
      state: {
        loading: false,
        project: { id: 'p-1', name: LONG_NAME, slug: 'p', projectType: 'linked', ...CAPS },
      },
      text: ['h1'],
      method: 'render',
    },
  ],
  [
    'template',
    {
      module: './template-detail.js',
      tag: 'scion-page-template-detail',
      row: '.template-title',
      icon: '.template-title > sl-icon',
      h1: '.template-title h1',
      iconTag: 'sl-icon.',
      state: {
        loading: false,
        template: { id: 't-1', name: LONG_NAME, harness: 'claude', scope: 'global', ...CAPS },
      },
      text: ['h1', 'span'],
      method: 'renderHeader',
    },
  ],
  [
    'harness config',
    {
      module: './harness-config-detail.js',
      tag: 'scion-page-harness-config-detail',
      row: '.resource-title-main',
      icon: '.resource-title-main > sl-icon',
      h1: '.resource-title h1',
      iconTag: 'sl-icon.',
      state: {
        loading: false,
        harnessConfig: {
          id: 'h-1',
          name: LONG_NAME,
          harness: 'claude',
          scope: 'global',
          sourceUrl: 'https://example.com/hc',
          _capabilities: { actions: ['read', 'delete'] },
        },
      },
      text: ['h1', 'span'],
      method: 'renderHeader',
    },
  ],
];

const loaded = new Map<string, Map<string, string>>();

beforeAll(async () => {
  for (const [, c] of cases) {
    await import(/* @vite-ignore */ c.module);
    const ctor = customElements.get(c.tag) as unknown as { elementStyles: CSSResult[] };
    loaded.set(c.tag, styleRules(ctor.elementStyles.map((s) => s.cssText).join('\n')));
  }
}, 60_000);

/** Render the page header for case `c` into a detached host. */
function renderHeader(c: PageCase): HTMLElement {
  const el = document.createElement(c.tag);
  Object.assign(el, c.state);
  const tpl = (el as unknown as Record<string, () => TemplateResult>)[c.method]();
  const host = document.createElement('div');
  render(tpl, host);
  return host;
}

describe.each(cases)('%s detail header', (_label, c) => {
  it('wraps a long name instead of overflowing the row', () => {
    const rules = loaded.get(c.tag)!;
    expect(rules.get(c.row) ?? '').toMatch(/display:\s*flex/);
    const text = rules.get('.header-title-text') ?? '';
    expect(text).toMatch(/display:\s*flex/);
    expect(text).toMatch(/flex-wrap:\s*wrap/);
    expect(text).toMatch(/(^|;)\s*min-width:\s*0/);
    const h1 = rules.get(c.h1) ?? '';
    expect(h1).toMatch(/(^|;)\s*min-width:\s*0/);
    expect(h1).toMatch(/overflow-wrap:\s*anywhere/);
    expect(rules.get(c.icon) ?? '').toMatch(/flex-shrink:\s*0/);
  });

  it('puts the name and badges in the wrapping row beside the icon', () => {
    const host = renderHeader(c);
    const row = host.querySelector(c.row);
    expect(row).not.toBeNull();
    expect(
      Array.from(row!.children).map((n) => n.tagName.toLowerCase() + '.' + n.className)
    ).toEqual([c.iconTag, 'div.header-title-text']);
    const text = row!.querySelector(':scope > .header-title-text')!;
    expect(Array.from(text.children).map((n) => n.tagName.toLowerCase())).toEqual(c.text);
    expect(text.querySelector(':scope > h1')!.textContent).toContain(LONG_NAME);
  });

  it('sets no inline width or icon style in the header row', () => {
    const row = renderHeader(c).querySelector(c.row)!;
    for (const node of [row, ...Array.from(row.querySelectorAll('[style]'))]) {
      expect(node.getAttribute('style') ?? '').not.toMatch(/(min-|max-)?width/);
    }
    // The icon takes its size from the stylesheet, where flex-shrink is set.
    expect(row.firstElementChild!.hasAttribute('style')).toBe(false);
  });
});

describe('harness config detail header actions', () => {
  const c = cases.find(([label]) => label === 'harness config')![1];

  it('drops the actions to the next line when the name does not fit', () => {
    const rules = loaded.get(c.tag)!;
    expect(rules.get('.resource-title') ?? '').toMatch(/flex-wrap:\s*wrap/);
    expect(rules.get('.resource-title-main') ?? '').toMatch(/(^|;)\s*min-width:\s*0/);
    const title = renderHeader(c).querySelector('.resource-title')!;
    expect(Array.from(title.children).map((n) => n.className)).toEqual([
      'resource-title-main',
      'header-actions',
    ]);
  });
});
