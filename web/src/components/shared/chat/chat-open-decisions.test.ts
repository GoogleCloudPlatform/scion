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
import type { OpenDecisionOpenDetail, ScionChatOpenDecisions } from './chat-open-decisions.js';
import './chat-open-decisions.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function flush(el: ScionChatOpenDecisions): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await Promise.resolve();
    await el.updateComplete;
  }
}

describe('scion-chat-open-decisions (slice 1)', () => {
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
