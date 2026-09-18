/**
 * Tests for <scion-chat-interagent-marker> cross-project display.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('../../../utils/markdown.js', () => ({
  getMarkdownRenderer: () =>
    Promise.resolve({
      render: (markdown: string) => `<p>${markdown}</p>`,
    }),
}));

await import('./chat-interagent-marker.js');
type ScionChatInteragentMarker =
  import('./chat-interagent-marker.js').ScionChatInteragentMarker;
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
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders collapsed pill with message count', async () => {
    const el = document.createElement(
      'scion-chat-interagent-marker'
    ) as ScionChatInteragentMarker;
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
    const el = document.createElement(
      'scion-chat-interagent-marker'
    ) as ScionChatInteragentMarker;
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
    const el = document.createElement(
      'scion-chat-interagent-marker'
    ) as ScionChatInteragentMarker;
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
    const el = document.createElement(
      'scion-chat-interagent-marker'
    ) as ScionChatInteragentMarker;
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
    const el = document.createElement(
      'scion-chat-interagent-marker'
    ) as ScionChatInteragentMarker;
    el.currentProjectId = 'proj-a';
    el.messageCount = 1;
    el.messages = [
      makeMessage({ senderProjectId: 'proj-b' }),
    ];
    el.expanded = true;
    document.body.appendChild(el);
    await el.updateComplete;

    // Cross-project badge should be present
    const badge = el.shadowRoot?.querySelector('.ia-cross-project');
    expect(badge).toBeTruthy();
  });
});
