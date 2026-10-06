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

import { describe, it, expect, beforeAll, vi } from 'vitest';
import { render, type TemplateResult } from 'lit';

import type { Agent } from '../../shared/types.js';
import { PROVISIONED_ONLY_LABEL } from '../../shared/agent-state-display.js';
import { styleRules } from './__fixtures__/card-layout.js';

// chat-thread (imported by agent-detail) pulls in the app entry point,
// which bootstraps the SPA on load; stub it as the header tests do.
vi.mock('../../client/main.js', () => ({
  navigateTo: vi.fn(),
  stateManager: new EventTarget(),
}));

describe('agent detail layout', () => {
  let rules: Map<string, string>;

  beforeAll(async () => {
    await import('./agent-detail.js');
    const ctor = customElements.get('scion-page-agent-detail') as unknown as {
      styles: { cssText: string };
    };
    rules = styleRules(ctor.styles.cssText);
  }, 30_000);

  it('wraps a long agent name with its badges instead of floating them beside it', () => {
    const text = rules.get('.header-title-text') ?? '';
    expect(text).toMatch(/display:\s*flex/);
    expect(text).toMatch(/flex-wrap:\s*wrap/);
    expect(text).toMatch(/min-width:\s*0/);
    expect(rules.get('.header h1') ?? '').toMatch(/overflow-wrap:\s*anywhere/);
  });

  it('wraps the template, project and broker links under the name', () => {
    const meta = rules.get('.header-meta') ?? '';
    expect(meta).toMatch(/display:\s*flex/);
    expect(meta).toMatch(/flex-wrap:\s*wrap/);
  });

  it('breaks one long template, project or broker name inside its item', () => {
    for (const sel of ['.template-badge', '.project-link', '.broker-link']) {
      const rule = rules.get(sel) ?? '';
      expect(rule, sel).toMatch(/(^|;)\s*min-width:\s*0/);
      expect(rule, sel).toMatch(/overflow-wrap:\s*anywhere/);
    }
  });

  it('keeps the message-mode select inside its column', () => {
    expect(rules.get('.messaging-grid') ?? '').toMatch(/flex-wrap:\s*wrap/);
    const column = rules.get('.messaging-grid .messaging-mode') ?? '';
    expect(column).toMatch(/flex:\s*1 1 280px/);
    expect(column).toMatch(/min-width:\s*0/);
    // The cap is on the select, not the column, so a read-only mode
    // description can use the full column width.
    expect(column).not.toMatch(/max-width/);
    const select = rules.get('.messaging-mode sl-select') ?? '';
    expect(select).toMatch(/(^|;)\s*width:\s*100%/);
    expect(select).toMatch(/max-width:\s*360px/);
    // The select used to force itself wider than its column with an inline
    // min-width; it now takes its width from the stylesheet only.
    const el = renderMessagingCard(
      makeAgent({ _capabilities: { actions: ['read', 'set_message_mode'] } })
    ).querySelector('sl-select');
    expect(el).not.toBeNull();
    expect(el!.hasAttribute('style')).toBe(false);
  });

  it('lets a read-only mode description fill the mode column', () => {
    const column = renderMessagingCard(
      makeAgent({ messageMode: 'project', _capabilities: { actions: ['read'] } })
    ).querySelector('.messaging-grid > .messaging-mode');
    expect(column).not.toBeNull();
    expect(column!.querySelector('sl-select')).toBeNull();
    expect(column!.querySelector('scion-message-mode-badge')).not.toBeNull();
    // Nothing inside the column caps its width inline.
    for (const node of Array.from(column!.querySelectorAll('[style]'))) {
      expect(node.getAttribute('style')).not.toMatch(/max-width/);
    }
  });

  /** Render one of the page's template methods for `agent`. */
  function renderPart(agent: Agent, method: 'renderHeader' | 'renderMessagingCard'): HTMLElement {
    const el = document.createElement('scion-page-agent-detail');
    (el as unknown as { agentId: string }).agentId = agent.id;
    (el as unknown as { agent: Agent }).agent = agent;
    const tpl = (el as unknown as Record<typeof method, () => TemplateResult>)[method]();
    const host = document.createElement('div');
    render(tpl, host);
    return host;
  }
  const renderHeader = (agent: Agent): HTMLElement => renderPart(agent, 'renderHeader');
  const renderMessagingCard = (agent: Agent): HTMLElement =>
    renderPart(agent, 'renderMessagingCard');

  function makeAgent(overrides: Partial<Agent>): Agent {
    return {
      id: 'a-1',
      name: 'a-very-long-agent-name-that-will-not-fit-on-one-line-beside-its-badges',
      projectId: 'p-1',
      template: 't',
      phase: 'running',
      created: '2026-01-01T00:00:00Z',
      updated: '2026-01-01T00:00:00Z',
      messageMode: 'project',
      _capabilities: { actions: ['read'] },
      ...overrides,
    } as Agent;
  }

  // Every header badge state, including the provisioned-not-started label
  // from ptone/scion#2929, must sit in the wrapping row after the name.
  const states: Array<[string, Partial<Agent>]> = [
    ['running', { phase: 'running', activity: 'thinking' }],
    ['provisioned, not started', { phase: 'created', provisionedOnly: true }],
    ['stopped', { phase: 'stopped' }],
    [
      'deleting',
      {
        phase: 'running',
        deletion: {
          state: 'deleting',
          soft: false,
          claim: 1,
          startedAt: '2026-01-01T00:00:00Z',
          leaseExpiresAt: '2999-01-01T00:00:00Z',
        },
      },
    ],
  ];

  it.each(states)('puts the name and every badge in the wrapping row (%s)', (_label, overrides) => {
    const host = renderHeader(makeAgent(overrides));
    const title = host.querySelector('.header-title');
    expect(title).not.toBeNull();
    // The title row holds only the icon and the wrapping text row, so no
    // badge can float beside a multi-line name.
    expect(
      Array.from(title!.children).map((c) => c.tagName.toLowerCase() + '.' + c.className)
    ).toEqual(['sl-icon.', 'div.header-title-text']);
    const row = title!.querySelector(':scope > .header-title-text')!;
    const tags = Array.from(row.children).map((c) => c.tagName.toLowerCase());
    expect(tags).toEqual([
      'h1',
      'scion-status-badge',
      'scion-deletion-badge',
      'scion-message-mode-badge',
    ]);
  });

  it('keeps the provisioned-not-started label in the wrapping row', () => {
    const host = renderHeader(makeAgent({ phase: 'created', provisionedOnly: true }));
    const badge = host.querySelector('.header-title-text > scion-status-badge');
    expect(badge?.getAttribute('label')).toBe(PROVISIONED_ONLY_LABEL);
  });
});
