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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import type { ConversationOpenDecisionsResponse } from '../../../client/conversation-open-decisions.js';
import { stateManager } from '../../../client/state.js';
import type { OpenDecisionOpenDetail, ScionChatOpenDecisions } from './chat-open-decisions.js';
import './chat-open-decisions.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function flush(el: ScionChatOpenDecisions): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await Promise.resolve();
    await el.updateComplete;
  }
}

describe('scion-chat-open-decisions', () => {
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;
  let el: ScionChatOpenDecisions;

  beforeEach(() => {
    fetchMock = vi.fn<typeof fetch>();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    el?.remove();
    vi.unstubAllGlobals();
  });

  it('fetches on mount, opens desktop panel, and dispatches open-decision-open on click', async () => {
    const payload: ConversationOpenDecisionsResponse = {
      counts: { waitingOnYou: 2, waitingOnOthers: 0 },
      prompts: [
        {
          promptMessageId: 'msg-1',
          promptAvailable: true,
          createdAt: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
          askerAgentId: 'a1',
          askerAgentName: 'deploy-lead',
          answerMode: 'set',
          openCount: 2,
          totalCount: 3,
          question: 'Which region should the canary go to first?',
          waitingOnYou: true,
          awaitingConfirmation: false,
        },
        {
          promptMessageId: 'msg-2',
          promptAvailable: true,
          createdAt: new Date(Date.now() - 30 * 60 * 1000).toISOString(),
          askerAgentId: 'a2',
          askerAgentName: 'docs-writer',
          answerMode: 'single',
          openCount: 1,
          totalCount: 1,
          question: 'Keep the old screenshots?',
          waitingOnYou: true,
          awaitingConfirmation: true,
        },
      ],
      truncated: false,
    };
    fetchMock.mockImplementation(() => Promise.resolve(jsonResponse(payload)));

    el = document.createElement('scion-chat-open-decisions') as ScionChatOpenDecisions;
    el.conversationKey = 'topic-1';
    el.projectSlug = 'demo';
    document.body.appendChild(el);
    await flush(el);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/chat/conversations/topic-1/decisions');

    const trigger = el.shadowRoot!.querySelector<HTMLButtonElement>('.trigger')!;
    expect(trigger).toBeTruthy();
    expect(trigger.classList.contains('waiting-on-you')).toBe(true);
    expect(trigger.getAttribute('aria-label')).toBe(
      'Open decisions in this conversation: 2, 2 waiting on you'
    );

    trigger.click();
    await flush(el);

    const panel = el.shadowRoot!.querySelector('.panel');
    expect(panel).toBeTruthy();
    expect(panel!.querySelector('.panel-title')!.textContent?.replace(/\s+/g, ' ').trim()).toBe(
      'Open decisions2'
    );
    expect(panel!.querySelector('.panel-count')!.textContent).toBe('2');

    const entries = el.shadowRoot!.querySelectorAll<HTMLAnchorElement>('a.entry');
    expect(entries).toHaveLength(2);
    expect(entries[0].textContent).toContain('2 of 3 open');
    expect(entries[1].textContent).toContain('Confirm');

    let jumped: OpenDecisionOpenDetail | null = null;
    el.addEventListener('open-decision-open', (e) => {
      jumped = (e as CustomEvent<OpenDecisionOpenDetail>).detail;
    });

    entries[0].click();
    await flush(el);

    expect(jumped).toEqual({ messageId: 'msg-1' });
    expect(el.isOpen).toBe(false);
  });

  it('shows neutral icon and empty state at zero', async () => {
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        jsonResponse({
          counts: { waitingOnYou: 0, waitingOnOthers: 0 },
          prompts: [],
          truncated: false,
        } satisfies ConversationOpenDecisionsResponse)
      )
    );

    el = document.createElement('scion-chat-open-decisions') as ScionChatOpenDecisions;
    el.conversationKey = 'topic-empty';
    document.body.appendChild(el);
    await flush(el);

    const trigger = el.shadowRoot!.querySelector<HTMLButtonElement>('.trigger')!;
    expect(trigger.classList.contains('waiting-on-you')).toBe(false);
    expect(trigger.getAttribute('aria-label')).toBe('Open decisions in this conversation: none');

    trigger.click();
    await flush(el);
    expect(el.shadowRoot!.querySelector('.status')!.textContent).toContain(
      'No open decisions in this conversation.'
    );
  });

  it('renders Waiting on others expanded with reasons and purged rows with Decisions link', async () => {
    const payload: ConversationOpenDecisionsResponse = {
      counts: { waitingOnYou: 0, waitingOnOthers: 2 },
      prompts: [
        {
          promptMessageId: 'msg-gone',
          promptAvailable: false,
          createdAt: new Date(Date.now() - 86_400_000).toISOString(),
          askerAgentId: 'a1',
          askerAgentName: 'db-migrator',
          answerMode: 'single',
          openCount: 1,
          totalCount: 1,
          question: 'Run index rebuild?',
          waitingOnYou: false,
          awaitingConfirmation: false,
          disabledReason: 'Read-only in this conversation',
        },
        {
          promptMessageId: 'msg-ro',
          promptAvailable: true,
          createdAt: new Date(Date.now() - 3_600_000).toISOString(),
          askerAgentId: 'a2',
          askerAgentName: 'pager-agent',
          answerMode: 'single',
          openCount: 1,
          totalCount: 1,
          question: 'Escalate alert?',
          waitingOnYou: false,
          awaitingConfirmation: false,
          disabledReason: 'Read-only in this conversation',
        },
      ],
      truncated: false,
    };
    fetchMock.mockImplementation(() => Promise.resolve(jsonResponse(payload)));

    el = document.createElement('scion-chat-open-decisions') as ScionChatOpenDecisions;
    el.conversationKey = 'topic-ro';
    el.projectSlug = 'infra';
    document.body.appendChild(el);
    await flush(el);

    const trigger = el.shadowRoot!.querySelector<HTMLButtonElement>('.trigger')!;
    expect(trigger.classList.contains('waiting-on-you')).toBe(false);
    trigger.click();
    await flush(el);

    // Purged row is a non-link div.entry.gone with Open in Decisions link.
    const gone = el.shadowRoot!.querySelector<HTMLDivElement>('div.entry.gone')!;
    expect(gone).toBeTruthy();
    const goneLink = gone.querySelector<HTMLAnchorElement>('a')!;
    expect(goneLink.getAttribute('href')).toBe('/projects/infra/decisions?conversation=topic-ro');

    // Live read-only row carries its disabledReason.
    const live = el.shadowRoot!.querySelector<HTMLAnchorElement>('a.entry.others')!;
    expect(live).toBeTruthy();
    expect(live.textContent).toContain('Read-only in this conversation');
  });

  it('renders truncation footer when truncated is true and closes on Escape', async () => {
    const payload: ConversationOpenDecisionsResponse = {
      counts: { waitingOnYou: 132, waitingOnOthers: 0 },
      prompts: [
        {
          promptMessageId: 'msg-1',
          promptAvailable: true,
          createdAt: new Date().toISOString(),
          askerAgentId: 'a1',
          askerAgentName: 'bulk-triage',
          answerMode: 'single',
          openCount: 1,
          totalCount: 1,
          question: 'Close duplicate?',
          waitingOnYou: true,
          awaitingConfirmation: false,
        },
      ],
      truncated: true,
    };
    fetchMock.mockImplementation(() => Promise.resolve(jsonResponse(payload)));

    el = document.createElement('scion-chat-open-decisions') as ScionChatOpenDecisions;
    el.conversationKey = 'topic-trunc';
    el.projectSlug = 'demo';
    document.body.appendChild(el);
    await flush(el);

    const trigger = el.shadowRoot!.querySelector<HTMLButtonElement>('.trigger')!;
    trigger.click();
    await flush(el);

    const footer = el.shadowRoot!.querySelector('.panel-footer')!;
    expect(footer).toBeTruthy();
    expect(footer.textContent).toContain('Showing the oldest 1 of 132');
    expect(footer.querySelector('a')!.getAttribute('href')).toBe(
      '/projects/demo/decisions?conversation=topic-trunc'
    );

    // Escape closes panel and restores focus to trigger.
    const panel = el.shadowRoot!.querySelector<HTMLElement>('.panel')!;
    panel.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await flush(el);
    expect(el.isOpen).toBe(false);
    expect(el.shadowRoot!.activeElement).toBe(trigger);
  });

  it('announces new decisions politely only when panel is closed and count increases', async () => {
    let count = 0;
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        jsonResponse({
          counts: { waitingOnYou: count, waitingOnOthers: 0 },
          prompts: [],
          truncated: false,
        } satisfies ConversationOpenDecisionsResponse)
      )
    );

    el = document.createElement('scion-chat-open-decisions') as ScionChatOpenDecisions;
    el.conversationKey = 'topic-live';
    document.body.appendChild(el);
    await flush(el);

    const liveRegion = (): string =>
      el.shadowRoot!.querySelector('[aria-live="polite"]')?.textContent?.trim() ?? '';
    expect(liveRegion()).toBe('');

    // Count increases while closed via reconnect refetch -> announces.
    count = 1;
    stateManager.dispatchEvent(new Event('connected'));
    await flush(el);
    expect(liveRegion()).toBe('New decision waiting');

    // Opening the panel clears the announcement.
    el.shadowRoot!.querySelector<HTMLButtonElement>('.trigger')!.click();
    await flush(el);
    expect(liveRegion()).toBe('');
  });

  it('hides cleanly when the hub returns 404', async () => {
    fetchMock.mockImplementation(() => Promise.resolve(jsonResponse({ error: 'not found' }, 404)));

    el = document.createElement('scion-chat-open-decisions') as ScionChatOpenDecisions;
    el.conversationKey = 'topic-off';
    document.body.appendChild(el);
    await flush(el);

    expect(el.hasAttribute('data-hidden')).toBe(true);
    expect(el.shadowRoot!.querySelector('.trigger')).toBeNull();
  });
});
