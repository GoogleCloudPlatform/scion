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
 * The phone conversation header (the compact row in the mobile layout)
 * shows the conversation name first with the project crumb beneath it,
 * and otherwise keeps the baseline geometry: padding, gap, borders, button
 * boxes, hit areas, one-line names and their type sizes. The fold between
 * the full and compact rows, and the header's height under the thread's
 * composer sizing (composer-room.ts), depend on that geometry. These are
 * static markup and style contracts, not geometry, rendering or device
 * evidence.
 */

import { describe, it, expect, vi, beforeAll, beforeEach } from 'vitest';
import { render, type TemplateResult } from 'lit';
import { elementStyleRules } from './__fixtures__/css-rules.js';
import { FakeEventSource } from '../../client/__fixtures__/agent-store-harness.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
  };
});

const MOBILE = '@media (max-width: 768px)';

let rules: Map<string, string>;

beforeAll(async () => {
  await import('./chat.js');
  rules = elementStyleRules('scion-page-chat');
});

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
});

/** The last value a rule body gives `prop`, as the cascade would apply it. */
function decl(body: string | undefined, prop: string): string | undefined {
  if (!body) return undefined;
  let value: string | undefined;
  for (const part of body.split(';')) {
    const i = part.indexOf(':');
    if (i < 0) continue;
    if (part.slice(0, i).trim() === prop) value = part.slice(i + 1).trim();
  }
  return value;
}

/** The property names a rule body declares. */
function props(body: string): string[] {
  return body
    .split(';')
    .map((part) => part.slice(0, part.indexOf(':')).trim())
    .filter((p) => p.length > 0);
}

function renderToFragment(tpl: TemplateResult): HTMLElement {
  const host = document.createElement('div');
  render(tpl, host);
  return host;
}

/** A phone-layout page with an agent DM open; the header is compact before it is measured. */
function phonePageWithAgentDM(peerName: string): any {
  const el = document.createElement('scion-page-chat') as any;
  el.pageData = { user: { id: 'user-me' } };
  el.isMobileLayout = true;
  el.mobilePanel = 'center';
  el.v2Conversation = {
    conversationKey: 'dm:agent:agent-1:user:user-me',
    projectId: 'proj-1',
    projectSlug: 'proj-one',
    threadName: '',
    defaultAgent: '',
    isDM: true,
    peerName,
    peerId: 'agent-1',
    peerKind: 'agent',
    muted: false,
  };
  vi.spyOn(el, 'getAgentProjectSlug').mockReturnValue('a-project-with-a-long-slug');
  return el;
}

describe('phone conversation header — markup', () => {
  it('keeps the full name and project in the DOM text and labels every row action', () => {
    const name = '会話のとても長いエージェント名 with-a-very-long-unbroken-identifier-0123456789';
    const page = phonePageWithAgentDM(name);
    const header = renderToFragment(page.renderV2Conversation()).querySelector('.v2-thread-header');

    expect(header?.classList.contains('compact')).toBe(true);
    // The complete name and project strings are present in the DOM text.
    // This checks text presence only: on screen each is one line, cut with
    // an ellipsis, which a static test cannot measure.
    expect(header?.querySelector('.conv-name .conv-text')?.textContent?.trim()).toBe(name);
    expect(header?.querySelector('.conv-crumb .conv-text')?.textContent?.trim()).toBe(
      'a-project-with-a-long-slug'
    );

    // The compact row's controls, each with an accessible name.
    for (const sel of ['.mobile-back', '.mobile-members', '.header-more']) {
      expect(header?.querySelector(sel)?.getAttribute('label'), sel).toBeTruthy();
    }
    expect(header?.querySelector('sl-icon-button[name="search"]')?.getAttribute('label')).toBe(
      'Search messages'
    );
    // The folded actions are still mounted (and offered by More), not dropped.
    expect(header?.querySelector('.header-secondary')).not.toBeNull();
  });
});

describe('phone conversation header — style contract', () => {
  it('puts the name before the project crumb in the compact column', () => {
    const name = rules.get(`${MOBILE} .v2-thread-header.compact .conv-name`);
    const crumb = rules.get(`${MOBILE} .v2-thread-header.compact .conv-crumb`);
    const nameOrder = Number(decl(name, 'order'));
    const crumbOrder = Number(decl(crumb, 'order') ?? '0');
    expect(nameOrder).toBeLessThan(crumbOrder);
    expect(decl(rules.get('.v2-thread-header.compact .conv-title'), 'flex-direction')).toBe(
      'column'
    );
  });

  it('keeps each title line to one line, cut with an ellipsis', () => {
    const base = rules.get('.conv-text');
    expect(decl(base, 'white-space')).toBe('nowrap');
    expect(decl(base, 'text-overflow')).toBe('ellipsis');
    expect(decl(base, 'overflow')).toBe('hidden');
    for (const [key, body] of rules) {
      if (!key.includes('.conv-text') || key === '.conv-text') continue;
      for (const prop of ['white-space', 'display', '-webkit-line-clamp', 'line-clamp']) {
        expect(decl(body, prop), `${key} overrides ${prop}`).toBeUndefined();
      }
    }
  });

  it('adds nothing to the compact row but the name-first order', () => {
    // The compact rules that predate the hierarchy change, and what each
    // declares. Anything more would change the header's geometry per state.
    const allowed: Record<string, string[]> = {
      '.v2-thread-header.compact .header-secondary': ['display'],
      '.v2-thread-header.compact .header-more': ['display'],
      '.v2-thread-header.compact .conv-title': ['flex-direction', 'align-items', 'gap'],
      '.v2-thread-header.compact .conv-crumb': ['font-size', 'line-height'],
      '.v2-thread-header.compact .conv-name': ['line-height'],
      [`${MOBILE} .v2-thread-header.compact .conv-name`]: ['order'],
    };
    const isCompact = (key: string): boolean => key.includes('.v2-thread-header.compact');
    const compactKeys = [...rules.keys()].filter(isCompact);
    expect(compactKeys.sort()).toEqual(Object.keys(allowed).sort());
    for (const key of compactKeys) {
      expect(props(rules.get(key)!).sort(), key).toEqual([...allowed[key]!].sort());
    }
    // Baseline title type in the compact column.
    expect(decl(rules.get('.v2-thread-header.compact .conv-crumb'), 'font-size')).toBe(
      'var(--chat-fs-xs)'
    );
    expect(decl(rules.get('.v2-thread-header.compact .conv-crumb'), 'line-height')).toBe('1.3');
    expect(decl(rules.get('.v2-thread-header.compact .conv-name'), 'line-height')).toBe('1.3');
  });

  it('keeps the shared header padding and the baseline phone hit areas', () => {
    // One padding for the header in both states: the fold measures the
    // content box, so per-state inline padding would shift it.
    expect(decl(rules.get('.v2-thread-header'), 'padding')).toBe('0.5rem 1rem');
    const mobileHeaderKeys = [...rules.keys()].filter((key) =>
      key.startsWith(`${MOBILE} .v2-thread-header`)
    );
    expect(mobileHeaderKeys.sort()).toEqual(
      [
        `${MOBILE} .v2-thread-header sl-icon-button::part(base)`,
        `${MOBILE} .v2-thread-header sl-icon-button::part(base)::before`,
        `${MOBILE} .v2-thread-header.compact .conv-name`,
      ].sort()
    );
    // The native-size button with a hit area grown by an invisible overlay.
    const overlay = rules.get(`${MOBILE} .v2-thread-header sl-icon-button::part(base)::before`);
    expect(decl(overlay, 'inset')).toBe('-6px -2px');
  });

  it('makes no header rule depend on the keyboard state', () => {
    const keyboardTokens = /--scion-(kb|chat-kb|chat-tight)/;
    const headerRules = [...rules].filter(([key]) => key.includes('v2-thread-header'));
    expect(headerRules.length).toBeGreaterThan(0);
    for (const [key, body] of headerRules) {
      expect(body, key).not.toMatch(keyboardTokens);
    }
  });
});
