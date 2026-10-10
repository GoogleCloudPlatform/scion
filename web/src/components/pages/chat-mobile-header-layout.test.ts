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
 * The phone conversation header (the compact row in the mobile layout):
 * the conversation name leads with the project beneath it, a long name
 * wraps to a bounded number of lines instead of being cut at one, every
 * action keeps a separate 44px touch target, the fold between the full
 * and compact rows measures the same inline box in both states, and
 * nothing in the header depends on the keyboard, so opening it never
 * changes the header's height under the thread's composer sizing
 * (composer-room.ts). These are static markup and style contracts; they
 * are not geometry, rendering or device evidence.
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

function mobileRule(selector: string): string | undefined {
  return rules.get(`${MOBILE} ${selector}`);
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
  it('keeps the full name and project in the DOM text (visual is clamped) and labels every row action', () => {
    const name = '会話のとても長いエージェント名 with-a-very-long-unbroken-identifier-0123456789';
    const page = phonePageWithAgentDM(name);
    const header = renderToFragment(page.renderV2Conversation()).querySelector(
      '.v2-thread-header'
    );

    expect(header?.classList.contains('compact')).toBe(true);
    // The complete name and project strings are present in the DOM text,
    // not only in a title attribute that a touch screen cannot hover. This
    // checks text presence only: the visible name is clamped to two lines
    // and the crumb to one, which a static test cannot measure.
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
    const nameOrder = Number(decl(mobileRule('.v2-thread-header.compact .conv-name'), 'order'));
    const crumbOrder = Number(
      decl(mobileRule('.v2-thread-header.compact .conv-crumb'), 'order') ?? '0'
    );
    expect(nameOrder).toBeLessThan(crumbOrder);
    // The column itself still comes from the shared compact rule.
    expect(decl(rules.get('.v2-thread-header.compact .conv-title'), 'flex-direction')).toBe(
      'column'
    );
  });

  it('lets a long or CJK name wrap to a bounded number of lines instead of one cut line', () => {
    const body = mobileRule('.v2-thread-header.compact .conv-name .conv-text');
    expect(decl(body, 'white-space')).toBe('normal');
    // The clamp only takes effect on a vertical -webkit-box that clips its
    // overflow; without any of these the name would wrap without limit.
    expect(decl(body, 'display')).toBe('-webkit-box');
    expect(decl(body, '-webkit-box-orient')).toBe('vertical');
    const overflow = decl(body, 'overflow') ?? decl(rules.get('.conv-text'), 'overflow');
    expect(overflow).toBe('hidden');
    // Unspaced identifiers and CJK runs can break anywhere rather than overflow.
    expect(decl(body, 'overflow-wrap')).toBe('anywhere');
    const lines = Number(decl(body, '-webkit-line-clamp'));
    expect(lines).toBeGreaterThan(1);
    expect(lines).toBeLessThanOrEqual(2);
  });

  it('gives each compact-row button a 44px box whose hit area stays inside it', () => {
    const base = mobileRule('.v2-thread-header.compact sl-icon-button::part(base)');
    expect(parseFloat(decl(base, 'width') ?? '0')).toBeGreaterThanOrEqual(44);
    expect(parseFloat(decl(base, 'height') ?? '0')).toBeGreaterThanOrEqual(44);
    // The wider-row hit-area extension would overlap neighbours at 44px.
    expect(
      decl(mobileRule('.v2-thread-header.compact sl-icon-button::part(base)::before'), 'inset')
    ).toBe('0');
  });

  it('measures the same inline box for the fold in the full and compact states', () => {
    // observeConversationHeader folds from the header's content-box width,
    // which excludes padding and borders. Any inline padding, border or
    // box-sizing that applies to one state only would make that width jump
    // on each fold and could flip the row back and forth. Block padding
    // does not affect the width and is allowed per state.
    const inlineBox = new Set([
      'padding',
      'padding-left',
      'padding-right',
      'padding-inline',
      'padding-inline-start',
      'padding-inline-end',
      'border',
      'border-left',
      'border-right',
      'border-inline',
      'border-inline-start',
      'border-inline-end',
      'border-width',
      'border-style',
      'border-left-width',
      'border-right-width',
      'box-sizing',
    ]);
    const perState = /\.v2-thread-header(\.compact|:not\(\.compact\))$/;
    for (const [key, body] of rules) {
      if (!perState.test(key)) continue;
      for (const part of body.split(';')) {
        const prop = part.slice(0, part.indexOf(':')).trim();
        expect(inlineBox.has(prop), `${key} sets ${prop}`).toBe(false);
      }
    }
    // The phone padding is set once for the header, so both states share it
    // and it replaces the base rule's inline padding in each.
    expect(decl(mobileRule('.v2-thread-header'), 'padding-inline')).toBeTruthy();
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
