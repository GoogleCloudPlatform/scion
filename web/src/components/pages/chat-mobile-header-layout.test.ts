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
 * Conversation header layout contract (ptone/scion#9657):
 * - Far left (title): `# <conversation-name>` for group conversations,
 *   `<agent-name>` for agent DMs, in the same place and style.
 * - Right side, immediately left of the terminal icon / actions: agent status
 *   badge followed by the linked project / agent breadcrumb using `folder` and
 *   `robot` `<sl-icon>`s (omitted for user DMs and group conversations without
 *   a default agent; no standalone robot emoji).
 * - At narrow/mobile widths, the breadcrumb shrinks/truncates before the title
 *   while preserving header padding, icon hit areas, and keyboard-independent
 *   geometry.
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
  el.v2AgentMembers = [
    {
      id: 'agent-1',
      kind: 'agent',
      displayName: peerName,
      slug: 'agent-1',
      projectId: 'proj-1',
      phase: 'running',
      activity: 'working',
    },
  ];
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

function pageWithGroupConversation(defaultAgent: string): any {
  const el = document.createElement('scion-page-chat') as any;
  el.pageData = { user: { id: 'user-me' } };
  el.isMobileLayout = false;
  el.v2AgentMembers = [
    {
      id: 'agent-1',
      kind: 'agent',
      displayName: 'preston-agent',
      slug: 'preston-agent',
      projectId: 'proj-1',
      phase: 'running',
      activity: 'working',
    },
  ];
  el._projectIdToSlug.set('proj-1', 'scion-fork');
  el.v2Conversation = {
    conversationKey: 'topic:proj-1:general',
    projectId: 'proj-1',
    projectSlug: 'scion-fork',
    threadName: 'general',
    defaultAgent,
    isDM: false,
    peerName: '',
    peerId: '',
    peerKind: 'user',
  };
  return el;
}

describe('conversation header — consistent layout markup', () => {
  it('renders agent DM with agent name on the left and status badge + linked project/agent breadcrumb right before actions', () => {
    const name = '会話のとても長いエージェント名 with-a-very-long-unbroken-identifier-0123456789';
    const page = phonePageWithAgentDM(name);
    const header = renderToFragment(page.renderV2Conversation()).querySelector('.v2-thread-header');

    expect(header?.classList.contains('compact')).toBe(true);

    // Far left: agent name only (no folder crumb or robot emoji inside .conv-title).
    const title = header?.querySelector('.conv-title');
    expect(title?.querySelector('.conv-name .conv-text')?.textContent?.trim()).toBe(name);
    expect(title?.querySelector('.conv-crumb')).toBeNull();
    expect(header?.textContent).not.toContain('🤖');
    expect(header?.querySelector('.conv-agent-emoji')).toBeNull();

    // Right side, immediately left of .header-actions (which starts with terminal):
    const context = header?.querySelector('.conv-agent-context');
    expect(context).not.toBeNull();
    expect(context?.nextElementSibling?.classList.contains('header-actions')).toBe(true);
    const statusBadge = context?.querySelector('scion-status-badge.conv-agent-status');
    const breadcrumb = context?.querySelector('.conv-agent-breadcrumb');
    expect(statusBadge?.getAttribute('status')).toBe('working');
    expect(statusBadge?.getAttribute('size')).toBe('small');
    // Pin exact cluster child order: status badge -> breadcrumb.
    expect(Array.from(context?.children ?? [])).toEqual([statusBadge, breadcrumb]);

    const projectLink = breadcrumb?.querySelector('a.conv-crumb');
    const sep = breadcrumb?.querySelector('.conv-crumb-sep');
    const agentLink = breadcrumb?.querySelector('a.conv-agent-crumb');
    expect(Array.from(breadcrumb?.children ?? [])).toEqual([projectLink, sep, agentLink]);
    expect(projectLink?.getAttribute('href')).toBe('/projects/proj-1');
    expect(projectLink?.querySelector('sl-icon')?.getAttribute('name')).toBe('folder');
    expect(projectLink?.querySelector('.conv-text')?.textContent?.trim()).toBe(
      'a-project-with-a-long-slug'
    );
    expect(agentLink?.getAttribute('href')).toBe('/agents/agent-1');
    expect(agentLink?.querySelector('sl-icon')?.getAttribute('name')).toBe('robot');
    expect(agentLink?.querySelector('.conv-text')?.textContent?.trim()).toBe(name);

    // First action button in .header-actions is the terminal icon.
    expect(header?.querySelector('.header-actions sl-icon-button')?.getAttribute('name')).toBe(
      'terminal'
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

  it('renders group conversation with default agent using the same right-hand status badge + linked breadcrumb', () => {
    const page = pageWithGroupConversation('preston-agent');
    const header = renderToFragment(page.renderV2Conversation()).querySelector('.v2-thread-header');

    // Far left: # <conversation-name>.
    expect(header?.querySelector('.conv-title .hash')?.textContent?.trim()).toBe('#');
    expect(header?.querySelector('.conv-title .conv-name .conv-text')?.textContent?.trim()).toBe(
      'general'
    );
    expect(header?.textContent).not.toContain('🤖');
    expect(header?.querySelector('.conv-agent-emoji')).toBeNull();

    // Right side, immediately left of .header-actions (which starts with terminal):
    const context = header?.querySelector('.conv-agent-context');
    expect(context).not.toBeNull();
    expect(context?.nextElementSibling?.classList.contains('header-actions')).toBe(true);
    const statusBadge = context?.querySelector('scion-status-badge.conv-agent-status');
    const breadcrumb = context?.querySelector('.conv-agent-breadcrumb');
    expect(statusBadge?.getAttribute('status')).toBe('working');
    expect(statusBadge?.getAttribute('size')).toBe('small');
    expect(Array.from(context?.children ?? [])).toEqual([statusBadge, breadcrumb]);

    const projectLink = breadcrumb?.querySelector('a.conv-crumb');
    const sep = breadcrumb?.querySelector('.conv-crumb-sep');
    const agentLink = breadcrumb?.querySelector('a.conv-agent-crumb');
    expect(Array.from(breadcrumb?.children ?? [])).toEqual([projectLink, sep, agentLink]);
    expect(projectLink?.getAttribute('href')).toBe('/projects/proj-1');
    expect(projectLink?.querySelector('sl-icon')?.getAttribute('name')).toBe('folder');
    expect(projectLink?.querySelector('.conv-text')?.textContent?.trim()).toBe('scion-fork');
    expect(agentLink?.getAttribute('href')).toBe('/agents/agent-1');
    expect(agentLink?.querySelector('sl-icon')?.getAttribute('name')).toBe('robot');
    expect(agentLink?.querySelector('.conv-text')?.textContent?.trim()).toBe('preston-agent');
    expect(header?.querySelector('.header-actions sl-icon-button')?.getAttribute('name')).toBe(
      'terminal'
    );
  });

  it('omits status badge and breadcrumb for a group conversation without a default agent', () => {
    const page = pageWithGroupConversation('');
    const header = renderToFragment(page.renderV2Conversation()).querySelector('.v2-thread-header');

    expect(header?.querySelector('.conv-title .conv-name .conv-text')?.textContent?.trim()).toBe(
      'general'
    );
    expect(header?.querySelector('.conv-agent-context')).toBeNull();
    expect(header?.textContent).not.toContain('🤖');
  });

  it('omits status badge and breadcrumb for a user-to-user DM', () => {
    const el = document.createElement('scion-page-chat') as any;
    el.pageData = { user: { id: 'user-me' } };
    el.isMobileLayout = false;
    el.v2Conversation = {
      conversationKey: 'dm:user:user-2:user:user-me',
      projectId: '',
      projectSlug: '',
      threadName: '',
      defaultAgent: '',
      isDM: true,
      peerName: 'Ada Lovelace',
      peerId: 'user-2',
      peerKind: 'user',
    };
    const header = renderToFragment(el.renderV2Conversation()).querySelector('.v2-thread-header');

    expect(header?.querySelector('.conv-title .conv-name .conv-text')?.textContent?.trim()).toBe(
      'Ada Lovelace'
    );
    expect(header?.querySelector('.conv-title sl-icon[name="person"]')).not.toBeNull();
    expect(header?.querySelector('.conv-agent-context')).toBeNull();
    expect(header?.textContent).not.toContain('🤖');
  });
});

describe('phone conversation header — style contract', () => {
  it('truncates the agent breadcrumb before the conversation title and aligns it immediately left of actions', () => {
    const titleFlex = decl(rules.get('.conv-title'), 'flex') ?? '';
    const contextFlex = decl(rules.get('.conv-agent-context'), 'flex') ?? '';
    const titleShrink = Number(titleFlex.split(/\s+/)[1]);
    const contextShrink = Number(contextFlex.split(/\s+/)[1]);
    expect(contextShrink).toBeGreaterThan(titleShrink);
    expect(decl(rules.get('.conv-agent-context'), 'margin-left')).toBe('auto');
    expect(decl(rules.get('.conv-agent-context + .header-actions'), 'margin-left')).toBe('0');
  });

  it('keeps each title and breadcrumb segment to one line, cut with an ellipsis', () => {
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

  it('keeps the compact row rules minimal so header geometry stays stable', () => {
    const allowed: Record<string, string[]> = {
      '.v2-thread-header.compact': ['flex-wrap', 'row-gap'],
      '.v2-thread-header.compact .header-secondary': ['display'],
      '.v2-thread-header.compact .header-more': ['display'],
      '.v2-thread-header.compact .conv-title': ['flex'],
      '.v2-thread-header.compact .conv-agent-context': ['order', 'flex', 'margin-left'],
      '.v2-thread-header.compact .conv-agent-breadcrumb': ['font-size'],
      '.v2-thread-header.compact .conv-name': ['line-height'],
    };
    const isCompact = (key: string): boolean => key.includes('.v2-thread-header.compact');
    const compactKeys = [...rules.keys()].filter(isCompact);
    expect(compactKeys.sort()).toEqual(Object.keys(allowed).sort());
    for (const key of compactKeys) {
      expect(props(rules.get(key)!).sort(), key).toEqual([...allowed[key]!].sort());
    }
    expect(decl(rules.get('.v2-thread-header.compact .conv-agent-breadcrumb'), 'font-size')).toBe(
      'var(--chat-fs-xs)'
    );
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
