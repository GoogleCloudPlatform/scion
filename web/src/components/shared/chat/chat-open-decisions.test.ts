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

  it('announces every consecutive new prompt while closed and does not announce restored optimistic drops', async () => {
    let prompts: ConversationOpenDecisionsResponse['prompts'] = [];
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        jsonResponse({
          counts: { waitingOnYou: prompts.length, waitingOnOthers: 0 },
          prompts,
          truncated: false,
        } satisfies ConversationOpenDecisionsResponse)
      )
    );

    el = document.createElement('scion-chat-open-decisions') as ScionChatOpenDecisions;
    el.conversationKey = 'topic-live';
    document.body.appendChild(el);
    await flush(el);

    const liveEl = el.shadowRoot!.querySelector<HTMLElement>('[aria-live="polite"]')!;
    expect(liveEl.textContent?.trim() ?? '').toBe('');

    const observedTexts: string[] = [];
    const observer = new MutationObserver(() => {
      observedTexts.push(liveEl.textContent?.trim() ?? '');
    });
    observer.observe(liveEl, { childList: true, characterData: true, subtree: true });

    try {
      // 1st new prompt while closed -> announces.
      prompts = [
        {
          promptMessageId: 'msg-a',
          promptAvailable: true,
          createdAt: new Date().toISOString(),
          askerAgentId: 'a1',
          askerAgentName: 'bot',
          answerMode: 'single',
          openCount: 1,
          totalCount: 1,
          question: 'First?',
          waitingOnYou: true,
          awaitingConfirmation: false,
        },
      ];
      stateManager.dispatchEvent(new Event('connected'));
      await flush(el);
      expect(liveEl.textContent?.trim()).toBe('New decision waiting');
      const countAfterFirst = observedTexts.filter((t) => t === 'New decision waiting').length;
      expect(countAfterFirst).toBe(1);

      // 2nd consecutive new prompt while STILL closed -> clears and re-announces (DOM mutation).
      prompts = [
        ...prompts,
        {
          promptMessageId: 'msg-b',
          promptAvailable: true,
          createdAt: new Date().toISOString(),
          askerAgentId: 'a1',
          askerAgentName: 'bot',
          answerMode: 'single',
          openCount: 1,
          totalCount: 1,
          question: 'Second?',
          waitingOnYou: true,
          awaitingConfirmation: false,
        },
      ];
      stateManager.dispatchEvent(new Event('connected'));
      await flush(el);
      expect(liveEl.textContent?.trim()).toBe('New decision waiting');
      const countAfterSecond = observedTexts.filter((t) => t === 'New decision waiting').length;
      expect(countAfterSecond).toBe(2);

      // Optimistic drop of msg-a followed by server refetch restoring msg-a (mixed-section set):
      // msg-a was already seen, so must NOT announce a 3rd time.
      stateManager.dispatchEvent(
        new CustomEvent('decision-updated', {
          detail: {
            data: {
              messageId: 'msg-a',
              threadId: 'topic-live',
              decision: {
                rowId: 'r-other',
                messageId: 'msg-a',
                askerAgentId: 'a1',
                askerAgentName: 'bot',
                key: 'k1',
                ref: 'D1',
                version: 1,
                status: 'decided',
                question: 'First?',
                choices: [{ id: '1', label: 'Yes' }],
                decision: {
                  choiceId: '1',
                  label: 'Yes',
                  matchedDefault: false,
                  answeredByUserId: 'u2',
                  answeredByName: 'Bob',
                  provenance: 'direct',
                  surface: 'native',
                  responseMessageId: 'resp-1',
                  at: new Date().toISOString(),
                },
              },
            },
          },
        })
      );
      stateManager.dispatchEvent(new Event('connected'));
      await flush(el);
      const countAfterRestore = observedTexts.filter((t) => t === 'New decision waiting').length;
      expect(countAfterRestore).toBe(2);

      // Opening the panel clears the announcement.
      el.shadowRoot!.querySelector<HTMLButtonElement>('.trigger')!.click();
      await flush(el);
      expect(liveEl.textContent?.trim() ?? '').toBe('');
    } finally {
      observer.disconnect();
    }
  });

  it('keeps the Waiting on others header when truncation hides its rows past the cap', async () => {
    const payload: ConversationOpenDecisionsResponse = {
      counts: { waitingOnYou: 100, waitingOnOthers: 5 },
      prompts: [
        {
          promptMessageId: 'msg-you-1',
          promptAvailable: true,
          createdAt: new Date().toISOString(),
          askerAgentId: 'a1',
          askerAgentName: 'bulk-triage',
          answerMode: 'single',
          openCount: 1,
          totalCount: 1,
          question: 'Waiting on you item?',
          waitingOnYou: true,
          awaitingConfirmation: false,
        },
      ],
      truncated: true,
    };
    fetchMock.mockImplementation(() => Promise.resolve(jsonResponse(payload)));

    el = document.createElement('scion-chat-open-decisions') as ScionChatOpenDecisions;
    el.conversationKey = 'topic-trunc-others';
    el.projectSlug = 'demo';
    document.body.appendChild(el);
    await flush(el);

    el.shadowRoot!.querySelector<HTMLButtonElement>('.trigger')!.click();
    await flush(el);

    const headers = [...el.shadowRoot!.querySelectorAll('.section-header')].map((h) =>
      h.textContent?.replace(/\s+/g, ' ').trim()
    );
    expect(headers).toContain('Waiting on you 100');
    expect(headers).toContain('Waiting on others 5');
  });

  it('opens a modal bottom sheet dialog at the narrow-screen breakpoint', async () => {
    const origMatchMedia = window.matchMedia;
    vi.stubGlobal('matchMedia', (query: string) => ({
      matches: query.includes('max-width'),
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }));
    const showModalSpy = vi.fn();
    HTMLDialogElement.prototype.showModal = showModalSpy;

    try {
      fetchMock.mockImplementation(() =>
        Promise.resolve(
          jsonResponse({
            counts: { waitingOnYou: 1, waitingOnOthers: 0 },
            prompts: [
              {
                promptMessageId: 'msg-m1',
                promptAvailable: true,
                createdAt: new Date().toISOString(),
                askerAgentId: 'a1',
                askerAgentName: 'deploy-lead',
                answerMode: 'single',
                openCount: 1,
                totalCount: 1,
                question: 'Deploy on mobile?',
                waitingOnYou: true,
                awaitingConfirmation: false,
              },
            ],
            truncated: false,
          } satisfies ConversationOpenDecisionsResponse)
        )
      );

      el = document.createElement('scion-chat-open-decisions') as ScionChatOpenDecisions;
      el.conversationKey = 'topic-mobile';
      document.body.appendChild(el);
      await flush(el);

      el.shadowRoot!.querySelector<HTMLButtonElement>('.trigger')!.click();
      await flush(el);

      const sheet = el.shadowRoot!.querySelector<HTMLDialogElement>('dialog.sheet');
      expect(sheet).toBeTruthy();
      expect(showModalSpy).toHaveBeenCalledTimes(1);
      expect(sheet!.querySelector('button.close')).toBeTruthy();
      expect(sheet!.querySelector('.grab')).toBeTruthy();
    } finally {
      window.matchMedia = origMatchMedia;
    }
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

  it('stays silent on new waiting-on-others prompts and truncated count drops, but announces --update replacements', async () => {
    let payload: ConversationOpenDecisionsResponse = {
      counts: { waitingOnYou: 1, waitingOnOthers: 0 },
      prompts: [
        {
          promptMessageId: 'msg-you-1',
          promptAvailable: true,
          createdAt: new Date().toISOString(),
          askerAgentId: 'a1',
          askerAgentName: 'deploy-lead',
          answerMode: 'single',
          openCount: 1,
          totalCount: 1,
          question: 'Initial?',
          waitingOnYou: true,
          awaitingConfirmation: false,
        },
      ],
      truncated: false,
    };
    fetchMock.mockImplementation(() => Promise.resolve(jsonResponse(payload)));

    el = document.createElement('scion-chat-open-decisions') as ScionChatOpenDecisions;
    el.conversationKey = 'topic-announce-rules';
    document.body.appendChild(el);
    await flush(el);

    const liveEl = el.shadowRoot!.querySelector<HTMLElement>('[aria-live="polite"]')!;
    const observed: string[] = [];
    const observer = new MutationObserver(() => {
      const text = liveEl.textContent?.trim() ?? '';
      if (text) observed.push(text);
    });
    observer.observe(liveEl, { childList: true, subtree: true, characterData: true });

    try {
      // 1. A new prompt arrives in Waiting on others only -> silent.
      payload = {
        counts: { waitingOnYou: 1, waitingOnOthers: 1 },
        prompts: [
          ...payload.prompts,
          {
            promptMessageId: 'msg-other-1',
            promptAvailable: true,
            createdAt: new Date().toISOString(),
            askerAgentId: 'a1',
            askerAgentName: 'deploy-lead',
            answerMode: 'single',
            openCount: 1,
            totalCount: 1,
            question: 'Waiting on Bob?',
            waitingOnYou: false,
            awaitingConfirmation: true,
            disabledReason: 'Waiting for Bob to confirm',
          },
        ],
        truncated: false,
      };
      stateManager.dispatchEvent(new Event('connected'));
      await flush(el);
      expect(observed).toEqual([]);

      // 2. An --update replacement replaces msg-you-1 with msg-you-2 (count 1 -> 1) -> announces.
      payload = {
        counts: { waitingOnYou: 1, waitingOnOthers: 1 },
        prompts: [
          {
            promptMessageId: 'msg-you-2',
            promptAvailable: true,
            createdAt: new Date().toISOString(),
            askerAgentId: 'a1',
            askerAgentName: 'deploy-lead',
            answerMode: 'single',
            openCount: 1,
            totalCount: 1,
            question: 'Updated question?',
            waitingOnYou: true,
            awaitingConfirmation: false,
          },
          payload.prompts[1],
        ],
        truncated: false,
      };
      stateManager.dispatchEvent(new Event('connected'));
      await flush(el);
      expect(observed).toEqual(['New decision waiting']);

      // 3. Truncated list where count increases (1 -> 105) -> announces once,
      // then count falls (105 -> 104) as prompt #101 slides into the window -> silent.
      payload = {
        counts: { waitingOnYou: 105, waitingOnOthers: 0 },
        prompts: [payload.prompts[0]],
        truncated: true,
      };
      stateManager.dispatchEvent(new Event('connected'));
      await flush(el);
      expect(observed).toEqual(['New decision waiting', 'New decision waiting']);

      payload = {
        counts: { waitingOnYou: 104, waitingOnOthers: 0 },
        prompts: [
          {
            promptMessageId: 'msg-slid-in-101',
            promptAvailable: true,
            createdAt: new Date().toISOString(),
            askerAgentId: 'a1',
            askerAgentName: 'deploy-lead',
            answerMode: 'single',
            openCount: 1,
            totalCount: 1,
            question: 'Prompt #101 slid in after older resolved',
            waitingOnYou: true,
            awaitingConfirmation: false,
          },
        ],
        truncated: true,
      };
      stateManager.dispatchEvent(new Event('connected'));
      await flush(el);
      expect(observed).toEqual(['New decision waiting', 'New decision waiting']);
    } finally {
      observer.disconnect();
    }
  });

  it('uses the hub-resolved projectSlug for Open in Decisions links on agent DMs opened from another project', async () => {
    const dmKey = 'dm:agent:a-proj-b:user:u1';
    const payload: ConversationOpenDecisionsResponse = {
      projectId: 'proj-b-id',
      projectSlug: 'proj-b',
      counts: { waitingOnYou: 1, waitingOnOthers: 0 },
      prompts: [
        {
          promptMessageId: 'msg-purged-dm',
          promptAvailable: false,
          createdAt: new Date().toISOString(),
          askerAgentId: 'a-proj-b',
          askerAgentName: 'agent-b',
          answerMode: 'single',
          openCount: 1,
          totalCount: 1,
          question: 'Purged DM prompt?',
          waitingOnYou: true,
          awaitingConfirmation: false,
        },
      ],
      truncated: false,
    };
    fetchMock.mockImplementation(() => Promise.resolve(jsonResponse(payload)));

    el = document.createElement('scion-chat-open-decisions') as ScionChatOpenDecisions;
    el.conversationKey = dmKey;
    // Inherited from project A's view when the user opened the DM.
    el.projectSlug = 'proj-a';
    document.body.appendChild(el);
    await flush(el);

    el.shadowRoot!.querySelector<HTMLButtonElement>('.trigger')!.click();
    await flush(el);

    const link = el.shadowRoot!.querySelector<HTMLAnchorElement>('.entry.gone a');
    expect(link).toBeTruthy();
    expect(link!.getAttribute('href')).toBe(
      `/projects/proj-b/decisions?conversation=${encodeURIComponent(dmKey)}`
    );
  });
});
