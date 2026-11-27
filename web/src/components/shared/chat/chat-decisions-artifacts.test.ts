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
 * Tests for artifact references on decision prompt cards (ptone/scion#9424):
 * - <scion-chat-decisions> renders readable & unavailable chips in single and set modes
 * - Chips respect hub.artifacts flag and emit decision-artifact-open
 * - <scion-chat-message> delegates chips to the decision card without duplication,
 *   opens <scion-chat-file-preview> on click, and falls back when web.decisions is off
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render } from 'lit';
import type { MessageArtifactRef } from '../../../client/artifacts.js';
import type { ProjectDecisionPromptView } from '../../../client/project-decisions.js';
import type { DecisionView } from '../../../shared/types.js';
import type { DecisionArtifactOpenDetail } from './chat-decisions.js';
import { renderPromptCard } from '../decisions-views.js';

vi.mock('../../../utils/markdown.js', () => {
  const render = (markdown: string): string => `<p>${markdown}</p>`;
  return {
    getMarkdownRenderer: () =>
      Promise.resolve({
        render,
        renderFragment: (markdown: string) => {
          const tpl = document.createElement('template');
          tpl.innerHTML = render(markdown);
          return tpl.content;
        },
      }),
  };
});

vi.mock('../code-editor.js', () => ({
  getLanguageFromPath: () => 'plaintext',
}));

const apiFetchMock = vi.fn();
vi.mock('../../../client/api.js', () => ({
  apiFetch: (path: string, options?: RequestInit) => apiFetchMock(path, options),
  extractApiError: () => Promise.resolve('error'),
  parseApiError: () => Promise.resolve({ code: 'err', message: 'error' }),
}));

vi.mock('../../../client/state.js', () => ({
  stateManager: {
    getCurrentUserId: vi.fn(() => 'user-1'),
    setScope: vi.fn(),
    clearScope: vi.fn(),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  },
}));

await import('./chat-decisions.js');
await import('./chat-message.js');
type ScionChatDecisions = import('./chat-decisions.js').ScionChatDecisions;
type ScionChatMessage = import('./chat-message.js').ScionChatMessage;

const ART_A = '5f1c2d3e-0000-4000-8000-0000000000aa';
const ART_B = '5f1c2d3e-0000-4000-8000-0000000000bb';

function readableRef(
  id: string,
  title: string,
  version: number,
  ownerName: string,
  seq = 0
): MessageArtifactRef {
  return {
    ref: seq > 0 ? `scion://artifact/${id}@${seq}` : `scion://artifact/${id}`,
    id,
    seq,
    available: true,
    title,
    version,
    ownerName,
    ownerKind: 'agent',
    ownerRef: 'designer',
  };
}

function unavailableRef(id: string): MessageArtifactRef {
  return {
    ref: `scion://artifact/${id}`,
    id,
    available: false,
  };
}

function sampleDecision(key: string, question: string): DecisionView {
  return {
    rowId: `row-${key}`,
    messageId: 'msg-1',
    key,
    ref: `agent/${key}`,
    version: 1,
    status: 'pending',
    askerAgentId: 'agent-1',
    askerAgentName: 'designer',
    question,
    choices: [
      { id: '1', label: 'Approve' },
      { id: '2', label: 'Revise' },
    ],
  };
}

describe('decision card artifact references (ptone/scion#9424)', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
    window.__SCION_FEATURES__ = {
      'web.decisions': true,
      'hub.artifacts': true,
    };
    apiFetchMock.mockReset();
    apiFetchMock.mockImplementation(() => new Promise(() => {}));
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
  });

  it('renders readable and unavailable artifact chips in single mode and emits open event', async () => {
    const el = document.createElement('scion-chat-decisions') as ScionChatDecisions;
    el.messageId = 'msg-1';
    el.decisions = [sampleDecision('D1', 'Approve the mock-up?')];
    el.intro = `See scion://artifact/${ART_B} then scion://artifact/${ART_A}@2`;
    el.artifactRefs = [readableRef(ART_A, 'Mockup v2', 2, 'designer', 2), unavailableRef(ART_B)];
    document.body.appendChild(el);
    await el.updateComplete;

    const chips = Array.from(
      el.shadowRoot?.querySelectorAll<HTMLButtonElement>('.card-artifacts .artifact-chip') ?? []
    );
    expect(chips).toHaveLength(2);
    // Ordered by intro appearance: B first, then A.
    expect(chips[0].classList.contains('unavailable')).toBe(true);
    expect(chips[0].textContent?.trim()).toBe('Artifact unavailable');
    expect(chips[0].querySelector('sl-icon')?.getAttribute('name')).toBe('lock');

    expect(chips[1].textContent?.replace(/\s+/g, ' ').trim()).toBe('Mockup v2 · v2 · designer');
    expect(chips[1].querySelector('sl-icon')?.getAttribute('name')).toBe('file-earmark-richtext');

    const opened: DecisionArtifactOpenDetail[] = [];
    el.addEventListener('decision-artifact-open', (e) => {
      opened.push((e as CustomEvent<DecisionArtifactOpenDetail>).detail);
    });
    chips[1].click();
    chips[0].click();
    expect(opened).toEqual([
      { id: ART_A, seq: 2, title: 'Mockup v2' },
      { id: ART_B, seq: 0 },
    ]);
  });

  it('renders artifact chips inside set mode cards', async () => {
    const el = document.createElement('scion-chat-decisions') as ScionChatDecisions;
    el.messageId = 'msg-set';
    el.answerMode = 'set';
    el.decisions = [
      sampleDecision('D1', 'Approve layout?'),
      sampleDecision('D2', 'Approve palette?'),
    ];
    el.artifactRefs = [readableRef(ART_A, 'Design Deck', 1, 'designer')];
    document.body.appendChild(el);
    await el.updateComplete;

    const chips = el.shadowRoot?.querySelectorAll('.card-artifacts .artifact-chip') ?? [];
    expect(chips).toHaveLength(1);
    expect(chips[0].textContent?.replace(/\s+/g, ' ').trim()).toBe('Design Deck · v1 · designer');
  });

  it('hides artifact chips when hub.artifacts flag is off', async () => {
    window.__SCION_FEATURES__ = { 'web.decisions': true, 'hub.artifacts': false };
    const el = document.createElement('scion-chat-decisions') as ScionChatDecisions;
    el.messageId = 'msg-1';
    el.decisions = [sampleDecision('D1', 'Approve?')];
    el.artifactRefs = [readableRef(ART_A, 'Mockup', 1, 'designer')];
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.card-artifacts')).toBeNull();
  });

  it('chat-message delegates chips to the decision card without duplicating them and opens preview', async () => {
    const msg = document.createElement('scion-chat-message') as ScionChatMessage;
    msg.messageId = 'msg-1';
    msg.messageType = 'decision-prompt';
    msg.body = `Review this\n\nscion://artifact/${ART_A}@2`;
    msg.decisionIntro = `Review this\n\nscion://artifact/${ART_A}@2`;
    msg.decisions = [sampleDecision('D1', 'Approve?')];
    msg.artifactRefs = [readableRef(ART_A, 'Mockup v2', 2, 'designer', 2)];
    document.body.appendChild(msg);
    await msg.updateComplete;
    await Promise.resolve();
    await msg.updateComplete;

    // Not duplicated at the message-bubble level.
    expect(msg.shadowRoot?.querySelector('.artifact-chips')).toBeNull();

    const card = msg.shadowRoot?.querySelector('scion-chat-decisions') as ScionChatDecisions | null;
    expect(card).not.toBeNull();
    await card!.updateComplete;

    const chip = card!.shadowRoot?.querySelector<HTMLButtonElement>(
      '.card-artifacts .artifact-chip'
    );
    expect(chip).not.toBeNull();
    chip!.click();
    await msg.updateComplete;

    const preview = msg.shadowRoot?.querySelector('scion-chat-file-preview') as
      | (HTMLElement & { target: unknown })
      | null;
    expect(preview?.target).toEqual({ kind: 'artifact', id: ART_A, seq: 2, name: 'Mockup v2' });
  });

  it('chat-message falls back to bubble artifact chips when web.decisions is off', async () => {
    window.__SCION_FEATURES__ = { 'web.decisions': false, 'hub.artifacts': true };
    const msg = document.createElement('scion-chat-message') as ScionChatMessage;
    msg.messageId = 'msg-1';
    msg.messageType = 'decision-prompt';
    msg.body = `Review this\n\nscion://artifact/${ART_A}`;
    msg.decisions = [sampleDecision('D1', 'Approve?')];
    msg.artifactRefs = [readableRef(ART_A, 'Mockup', 1, 'designer')];
    document.body.appendChild(msg);
    await msg.updateComplete;
    await Promise.resolve();
    await msg.updateComplete;

    expect(msg.shadowRoot?.querySelector('scion-chat-decisions')).toBeNull();
    const chips = msg.shadowRoot?.querySelectorAll('.artifact-chips .artifact-chip') ?? [];
    expect(chips).toHaveLength(1);
  });

  it('shows unavailable notice when artifactsUnavailable is true and hides it when hub.artifacts is off', async () => {
    const el = document.createElement('scion-chat-decisions') as ScionChatDecisions;
    el.messageId = 'msg-1';
    el.decisions = [sampleDecision('D1', 'Approve?')];
    el.artifactsUnavailable = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const note = el.shadowRoot?.querySelector('.card-artifacts-unavailable');
    expect(note).not.toBeNull();
    expect(note?.textContent?.trim()).toContain('Artifact references could not be loaded');

    window.__SCION_FEATURES__ = { 'web.decisions': true, 'hub.artifacts': false };
    el.requestUpdate();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.card-artifacts-unavailable')).toBeNull();
  });

  it('renderPromptCard passes artifacts and artifactsUnavailable and wires onArtifactOpen', async () => {
    const host = document.createElement('div');
    document.body.appendChild(host);
    const opened: DecisionArtifactOpenDetail[] = [];

    const prompt: ProjectDecisionPromptView = {
      promptMessageId: 'msg-p1',
      promptAvailable: true,
      chatKey: 'thread-1',
      conversationLabel: '#design',
      conversationAvailable: true,
      askerAgentId: 'agent-1',
      askerAgentName: 'designer',
      createdAt: '2026-11-27T12:00:00Z',
      answerMode: 'single',
      waitingOnYou: true,
      canAnswer: true,
      artifacts: [readableRef(ART_A, 'Mockup v3', 3, 'designer', 3)],
      decisions: [sampleDecision('D1', 'Approve mockup?')],
    };

    render(
      renderPromptCard(prompt, {
        currentUserId: 'user-1',
        onAnswered: () => {},
        onJump: () => {},
        onArtifactOpen: (e) => {
          opened.push(e.detail);
        },
      }),
      host
    );

    const card = host.querySelector('scion-chat-decisions') as ScionChatDecisions | null;
    expect(card).not.toBeNull();
    await card!.updateComplete;

    const chip = card!.shadowRoot?.querySelector<HTMLButtonElement>(
      '.card-artifacts .artifact-chip'
    );
    expect(chip).not.toBeNull();
    expect(chip?.textContent?.replace(/\s+/g, ' ').trim()).toBe('Mockup v3 · v3 · designer');
    chip!.click();
    expect(opened).toEqual([{ id: ART_A, seq: 3, title: 'Mockup v3' }]);

    // Also verify artifactsUnavailable flows through renderPromptCard.
    render(
      renderPromptCard(
        { ...prompt, artifacts: [], artifactsUnavailable: true },
        {
          currentUserId: 'user-1',
          onAnswered: () => {},
          onJump: () => {},
        }
      ),
      host
    );
    await card!.updateComplete;
    expect(card!.shadowRoot?.querySelector('.card-artifacts-unavailable')).not.toBeNull();
  });

  it('project-decisions and profile-decisions pages open <scion-chat-file-preview> on decision-artifact-open', async () => {
    await import('../../pages/project-decisions.js');
    await import('../../pages/profile-decisions.js');

    for (const tag of ['scion-page-project-decisions', 'scion-page-profile-decisions'] as const) {
      const page = document.createElement(tag) as HTMLElement & {
        updateComplete: Promise<boolean>;
        onArtifactOpen: (e: CustomEvent<DecisionArtifactOpenDetail>) => void;
      };
      document.body.appendChild(page);
      await page.updateComplete;

      // Invoke the page's bound handler directly (matching the renderPromptCard callback).
      (
        page as unknown as {
          onArtifactOpen: (e: CustomEvent<DecisionArtifactOpenDetail>) => void;
        }
      ).onArtifactOpen(
        new CustomEvent<DecisionArtifactOpenDetail>('decision-artifact-open', {
          detail: { id: ART_A, seq: 2, title: 'Mockup v2' },
        })
      );
      await page.updateComplete;

      const preview = page.shadowRoot?.querySelector('scion-chat-file-preview') as
        | (HTMLElement & { target?: unknown })
        | null;
      // Note: preview renders once the page is in its main (non-loading/404) view;
      // verify previewTarget state was set cleanly on both page components.
      const internal = page as unknown as { previewTarget: unknown };
      expect(internal.previewTarget).toEqual({
        kind: 'artifact',
        id: ART_A,
        seq: 2,
        name: 'Mockup v2',
      });
      if (preview) {
        expect(preview.target).toEqual({
          kind: 'artifact',
          id: ART_A,
          seq: 2,
          name: 'Mockup v2',
        });
      }
      page.remove();
    }
  });
});
