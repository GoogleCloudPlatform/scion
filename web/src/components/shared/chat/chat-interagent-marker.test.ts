/**
 * Tests for <scion-chat-interagent-marker> cross-project display.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { setPreferredTimeZone } from '../../../utils/time.js';

const apiFetchMock = vi.fn();

vi.mock('../../../client/api.js', () => ({
  apiFetch: (...args: unknown[]) => apiFetchMock(...args),
}));

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

await import('./chat-interagent-marker.js');
type ScionChatInteragentMarker = import('./chat-interagent-marker.js').ScionChatInteragentMarker;
type Message = import('../../../shared/types.js').Message;

function makeMessage(overrides: Partial<Message> = {}): Message {
  return {
    id: 'msg-1',
    projectId: 'proj-a',
    sender: 'agent:alpha',
    senderId: 'agent-alpha-id',
    recipient: 'agent:beta',
    recipientId: 'agent-beta-id',
    msg: 'Hello',
    type: 'agent-message',
    agentId: 'agent-alpha-id',
    createdAt: new Date().toISOString(),
    ...overrides,
  };
}

describe('scion-chat-interagent-marker', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
    apiFetchMock.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders collapsed pill with message count', async () => {
    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.messageCount = 3;
    el.messages = [makeMessage(), makeMessage({ id: 'msg-2' }), makeMessage({ id: 'msg-3' })];
    document.body.appendChild(el);
    await el.updateComplete;

    const pill = el.shadowRoot?.querySelector('.marker-pill');
    expect(pill).toBeTruthy();
    expect(pill?.textContent).toContain('3');
    expect(pill?.textContent).toContain('messages');
  });

  it('shows cross-project count in collapsed pill', async () => {
    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.currentProjectId = 'proj-a';
    el.messageCount = 2;
    el.messages = [
      makeMessage({ senderProjectId: 'proj-b' }),
      makeMessage({ id: 'msg-2', senderProjectId: 'proj-a' }),
    ];
    document.body.appendChild(el);
    await el.updateComplete;

    const text = el.shadowRoot?.textContent || '';
    expect(text).toContain('cross-project');
  });

  it('formats participant with project prefix for cross-project messages when expanded', async () => {
    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.currentProjectId = 'proj-a';
    el.messageCount = 1;
    el.messages = [
      makeMessage({
        sender: 'agent:remote-bot',
        senderProjectId: 'proj-bbbb-cccc-dddd',
        recipient: 'agent:local-bot',
        recipientProjectId: 'proj-a',
      }),
    ];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const sender = el.shadowRoot?.querySelector('.ia-sender');
    expect(sender?.textContent).toContain('proj-bbb'); // truncated project ID prefix
    expect(sender?.textContent).toContain('remote-bot');
  });

  it('does not add prefix for same-project participants', async () => {
    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.currentProjectId = 'proj-a';
    el.messageCount = 1;
    el.messages = [
      makeMessage({
        sender: 'agent:local-bot',
        senderProjectId: 'proj-a',
        recipient: 'agent:other-bot',
        recipientProjectId: 'proj-a',
      }),
    ];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const sender = el.shadowRoot?.querySelector('.ia-sender');
    expect(sender?.textContent).toBe('local-bot');
    const recipient = el.shadowRoot?.querySelector('.ia-recipient');
    expect(recipient?.textContent).toBe('other-bot');
  });

  it('isCrossProject detects cross-project messages', async () => {
    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.currentProjectId = 'proj-a';
    el.messageCount = 1;
    el.messages = [makeMessage({ senderProjectId: 'proj-b' })];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    // Cross-project badge should be present
    const badge = el.shadowRoot?.querySelector('.ia-cross-project');
    expect(badge).toBeTruthy();
  });

  it('renders the shared date divider for each subsequent day when messages span multiple days', async () => {
    // Defensive case: the main timeline splits runs by day before handing
    // messages to a marker, but the marker itself must still fall back to
    // the shared separator (not a bespoke style) if it ever spans days.
    // The first day's divider is the main timeline's own — rendered directly
    // above the marker — so the marker itself must not repeat it; only the
    // day change to Jan 16 gets an internal divider.
    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.messageCount = 2;
    el.messages = [
      makeMessage({ id: 'msg-1', createdAt: new Date(2026, 0, 15, 9, 0).toISOString() }),
      makeMessage({ id: 'msg-2', createdAt: new Date(2026, 0, 16, 10, 0).toISOString() }),
    ];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelectorAll('.ia-date-divider').length).toBe(0);
    const dividers = el.shadowRoot?.querySelectorAll('.date-divider');
    expect(dividers?.length).toBe(1);
    expect(dividers?.[0].textContent).toContain('Jan 16');
  });

  it('renders no internal date divider when all messages fall on the same day', async () => {
    // The main timeline already renders the shared separator above the
    // marker for this day, so a single-day marker must add nothing of its
    // own — otherwise every marker would show the date twice.
    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.messageCount = 2;
    el.messages = [
      makeMessage({ id: 'msg-1', createdAt: new Date(2026, 0, 15, 9, 0).toISOString() }),
      makeMessage({ id: 'msg-2', createdAt: new Date(2026, 0, 15, 10, 0).toISOString() }),
    ];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const dividers = el.shadowRoot?.querySelectorAll('.date-divider');
    expect(dividers?.length).toBe(0);
  });

  it('renders a time label in the header for each message', async () => {
    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.messageCount = 1;
    el.messages = [makeMessage({ createdAt: new Date(2026, 0, 15, 14, 30).toISOString() })];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const time = el.shadowRoot?.querySelector('.ia-time');
    expect(time).toBeTruthy();
    expect(time?.textContent).toMatch(/^\d{2}:\d{2}$/);
    expect(time?.textContent).toContain('14:30');
  });

  it('renders an empty time label for an invalid createdAt', async () => {
    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.messageCount = 1;
    el.messages = [makeMessage({ createdAt: 'not-a-date' })];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const time = el.shadowRoot?.querySelector('.ia-time');
    expect(time).toBeTruthy();
    expect(time?.textContent).toBe('');
  });

  // AC4, review R2-3: the expanded row's time carries a title with the
  // full instant and zone label.
  it('titles the time label with the full instant and zone (AC4, review R2-3)', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    try {
      const el = document.createElement(
        'scion-chat-interagent-marker'
      ) as ScionChatInteragentMarker;
      el.messageCount = 1;
      el.messages = [makeMessage({ createdAt: '2026-09-23T15:00:00Z' })]; // -> 2026-09-24T00:00 JST
      el.expanded = true;
      document.body.appendChild(el);
      await el.updateComplete;

      const time = el.shadowRoot?.querySelector('.ia-time');
      expect(time?.textContent).toBe('00:00');
      expect(time?.getAttribute('title')).toBe('Sep 24, 2026, 00:00 (Asia/Tokyo)');
    } finally {
      setPreferredTimeZone('');
    }
  });

  // Review R2-1/R3-3: the marker's DisplayZoneController re-renders the
  // expanded time when the preference changes after mount.
  it('re-renders in the new zone after a mounted marker outlives a preference change', async () => {
    try {
      const el = document.createElement(
        'scion-chat-interagent-marker'
      ) as ScionChatInteragentMarker;
      el.messageCount = 1;
      el.messages = [makeMessage({ createdAt: '2026-09-23T15:00:00Z' })];
      el.expanded = true;
      document.body.appendChild(el);
      await el.updateComplete;

      expect(el.shadowRoot?.querySelector('.ia-time')?.textContent).toBe('15:00'); // UTC (Auto)

      setPreferredTimeZone('Asia/Tokyo');
      await el.updateComplete;

      const time = el.shadowRoot?.querySelector('.ia-time');
      expect(time?.textContent).toBe('00:00');
      expect(time?.getAttribute('title')).toBe('Sep 24, 2026, 00:00 (Asia/Tokyo)');
    } finally {
      setPreferredTimeZone('');
    }
  });

  it('shows expand icon for msgTruncated without DOM overflow', async () => {
    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.messageCount = 2;
    el.messages = [
      makeMessage({ id: 'msg-trunc', msg: 'Preview text...', msgTruncated: true }),
      makeMessage({ id: 'msg-short', msg: 'Short text' }),
    ];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const rows = el.shadowRoot?.querySelectorAll('.ia-msg');
    expect(rows?.length).toBe(2);
    expect(rows?.[0].querySelector('.ia-expand sl-icon-button')).toBeTruthy();
    expect(rows?.[1].querySelector('.ia-expand sl-icon-button')).toBeNull();
    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('fetches full message on expand click only, showing preview + spinner first, then caches on reopen', async () => {
    let resolveFetch!: (res: Response) => void;
    apiFetchMock.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveFetch = resolve;
        })
    );

    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.setAttribute('conversation-key', 'dm:agent:agent-alpha-id:user:user-1');
    el.messageCount = 1;
    el.messages = [
      makeMessage({
        id: 'msg-trunc-1',
        msg: 'Truncated preview...',
        msgTruncated: true,
      }),
    ];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    // Expanding the pill must not fetch full bodies.
    expect(apiFetchMock).not.toHaveBeenCalled();

    const expandBtn = el.shadowRoot?.querySelector(
      '.ia-expand sl-icon-button'
    ) as HTMLElement | null;
    expect(expandBtn).toBeTruthy();
    expandBtn!.click();
    await el.updateComplete;

    // Dialog opens immediately with preview + spinner while fetch is in flight.
    expect(apiFetchMock).toHaveBeenCalledTimes(1);
    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/chat/conversations/dm%3Aagent%3Aagent-alpha-id%3Auser%3Auser-1/interagent?messageId=msg-trunc-1',
      { suppressAccessDeniedToast: true }
    );
    const dialog = el.shadowRoot?.querySelector('sl-dialog.ia-full-preview');
    expect(dialog).toBeTruthy();
    expect(dialog?.textContent).toContain('Truncated preview...');
    expect(dialog?.querySelector('sl-spinner')).toBeTruthy();

    // Resolve full body response.
    resolveFetch(
      new Response(
        JSON.stringify({
          messages: [
            makeMessage({
              id: 'msg-trunc-1',
              msg: 'Full markdown body content',
              msgTruncated: false,
            }),
          ],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await vi.waitFor(async () => {
      await el.updateComplete;
      expect(el.shadowRoot?.querySelector('.ia-full-body')?.textContent).toBe(
        'Full markdown body content'
      );
    });
    expect(el.shadowRoot?.querySelector('sl-dialog sl-spinner')).toBeNull();

    // Close the dialog and reopen the same message -> served from cache without refetching.
    const slDialog = el.shadowRoot?.querySelector('sl-dialog.ia-full-preview') as HTMLElement;
    slDialog.dispatchEvent(new CustomEvent('sl-after-hide', { bubbles: false }));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('sl-dialog.ia-full-preview')).toBeNull();

    expandBtn!.click();
    await vi.waitFor(async () => {
      await el.updateComplete;
      expect(el.shadowRoot?.querySelector('.ia-full-body')?.textContent).toBe(
        'Full markdown body content'
      );
    });
    expect(apiFetchMock).toHaveBeenCalledTimes(1);
  });

  it('shows preview with fallback note on 403/404 failure without refetching toast', async () => {
    apiFetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ error: { code: 'NOT_FOUND', message: 'not found' } }), {
        status: 404,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.setAttribute('conversation-key', 'dm:agent:agent-alpha-id:user:user-1');
    el.messageCount = 1;
    el.messages = [
      makeMessage({
        id: 'msg-missing',
        msg: 'Saved preview text...',
        msgTruncated: true,
      }),
    ];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const expandBtn = el.shadowRoot?.querySelector('.ia-expand sl-icon-button') as HTMLElement;
    expandBtn.click();

    await vi.waitFor(async () => {
      await el.updateComplete;
      const dialogText =
        el.shadowRoot?.querySelector('sl-dialog.ia-full-preview')?.textContent ?? '';
      expect(dialogText).toContain("Couldn't load full message");
      expect(dialogText).toContain('Saved preview text...');
    });
    expect(el.shadowRoot?.querySelector('sl-dialog sl-spinner')).toBeNull();
    expect(apiFetchMock).toHaveBeenCalledWith(
      '/api/v1/chat/conversations/dm%3Aagent%3Aagent-alpha-id%3Auser%3Auser-1/interagent?messageId=msg-missing',
      { suppressAccessDeniedToast: true }
    );
  });

  it('drops stale full-message response if dialog closed or switched to another message', async () => {
    let resolveFirst!: (res: Response) => void;
    let resolveSecond!: (res: Response) => void;
    apiFetchMock
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirst = resolve;
          })
      )
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveSecond = resolve;
          })
      );

    const el = document.createElement('scion-chat-interagent-marker') as ScionChatInteragentMarker;
    el.setAttribute('conversation-key', 'dm:agent:agent-alpha-id:user:user-1');
    el.messageCount = 2;
    el.messages = [
      makeMessage({ id: 'msg-1', msg: 'Preview 1...', msgTruncated: true }),
      makeMessage({ id: 'msg-2', msg: 'Preview 2...', msgTruncated: true }),
    ];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const buttons = el.shadowRoot?.querySelectorAll('.ia-expand sl-icon-button');
    expect(buttons?.length).toBe(2);

    // Open msg-1, then switch to msg-2 before msg-1 resolves.
    (buttons![0] as HTMLElement).click();
    await el.updateComplete;
    (buttons![1] as HTMLElement).click();
    await el.updateComplete;

    resolveSecond(
      new Response(
        JSON.stringify({
          messages: [makeMessage({ id: 'msg-2', msg: 'Full body 2', msgTruncated: false })],
        }),
        { status: 200 }
      )
    );
    await vi.waitFor(async () => {
      await el.updateComplete;
      expect(el.shadowRoot?.querySelector('.ia-full-body')?.textContent).toBe('Full body 2');
    });

    // Late resolution of msg-1 must not overwrite msg-2 in the dialog.
    resolveFirst(
      new Response(
        JSON.stringify({
          messages: [makeMessage({ id: 'msg-1', msg: 'Stale full body 1', msgTruncated: false })],
        }),
        { status: 200 }
      )
    );
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.ia-full-body')?.textContent).toBe('Full body 2');
  });
});
