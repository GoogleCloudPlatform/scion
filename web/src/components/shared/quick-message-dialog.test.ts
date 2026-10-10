/**
 * Tests for <scion-quick-message-dialog>: cross-project label and the
 * "Open agent DM" footer button.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
  extractApiError: vi.fn(() => Promise.resolve('Error')),
}));

vi.mock('../../shared/message-mode.js', () => ({
  getDenialMessage: vi.fn(() => 'denied'),
}));

const currentUser = { id: '' };
vi.mock('../../client/state.js', () => ({
  stateManager: { getCurrentUserId: () => currentUser.id },
}));

const flags = { nativeChat: true };
vi.mock('../../utils/feature-flags.js', () => ({
  isFeatureEnabled: (name: string) => name === 'web.native_chat' && flags.nativeChat,
}));

await import('./quick-message-dialog.js');
type ScionQuickMessageDialog = import('./quick-message-dialog.js').ScionQuickMessageDialog;

describe('scion-quick-message-dialog', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('shows agent name in dialog label', async () => {
    const el = document.createElement('scion-quick-message-dialog') as ScionQuickMessageDialog;
    el.agentName = 'my-agent';
    el.open = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const dialog = el.shadowRoot?.querySelector('sl-dialog');
    expect(dialog?.getAttribute('label')).toBe('Message my-agent');
  });

  it('shows project / agent format for cross-project', async () => {
    const el = document.createElement('scion-quick-message-dialog') as ScionQuickMessageDialog;
    el.agentName = 'remote-bot';
    el.projectName = 'other-project';
    el.open = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const dialog = el.shadowRoot?.querySelector('sl-dialog');
    expect(dialog?.getAttribute('label')).toBe('Message other-project / remote-bot');
  });

  it('shows fallback when neither name is set', async () => {
    const el = document.createElement('scion-quick-message-dialog') as ScionQuickMessageDialog;
    el.open = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const dialog = el.shadowRoot?.querySelector('sl-dialog');
    expect(dialog?.getAttribute('label')).toBe('Send Message');
  });

  it('shows just agent name when projectName is set but agentName is not', async () => {
    const el = document.createElement('scion-quick-message-dialog') as ScionQuickMessageDialog;
    el.projectName = 'other-project';
    el.open = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const dialog = el.shadowRoot?.querySelector('sl-dialog');
    // projectName only shows when agentName is also set
    expect(dialog?.getAttribute('label')).toBe('Send Message');
  });
});

describe('scion-quick-message-dialog — Open agent DM', () => {
  const AGENT = '11111111-1111-1111-1111-111111111111';
  const USER = '22222222-2222-2222-2222-222222222222';
  const DM_KEY = `dm:agent:${AGENT}:user:${USER}`;
  const DM_PATH = `/chat/dm/${encodeURIComponent(DM_KEY)}`;
  const DRAFT_KEY = `scion-chat-draft-${DM_KEY}`;

  beforeEach(() => {
    document.body.innerHTML = '';
    localStorage.clear();
    currentUser.id = USER;
    flags.nativeChat = true;
  });

  afterEach(() => {
    document.body.innerHTML = '';
    localStorage.clear();
  });

  async function mount(): Promise<ScionQuickMessageDialog> {
    const el = document.createElement('scion-quick-message-dialog') as ScionQuickMessageDialog;
    el.agentId = AGENT;
    el.agentName = 'my-agent';
    el.open = true;
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  function dmButton(el: ScionQuickMessageDialog): HTMLElement | null {
    return el.shadowRoot?.querySelector<HTMLElement>('sl-button.open-dm') ?? null;
  }

  it('renders the button in the footer, before Cancel and Send', async () => {
    const el = await mount();
    const btn = dmButton(el);
    expect(btn?.textContent?.trim()).toBe('Open agent DM');
    const footer = el.shadowRoot?.querySelector('[slot="footer"]');
    expect(footer?.firstElementChild).toBe(btn);
    const actions = footer?.querySelector('.dialog-footer-actions');
    expect(Array.from(actions?.children ?? []).map((b) => b.textContent?.trim())).toEqual([
      'Cancel',
      'Send',
    ]);
  });

  it('closes the dialog and navigates to the agent DM', async () => {
    const el = await mount();
    const nav = vi.fn();
    const closed = vi.fn();
    document.addEventListener('nav-click', nav);
    el.addEventListener('sl-request-close', closed);
    try {
      dmButton(el)!.click();
      expect(el.open).toBe(false);
      expect(closed).toHaveBeenCalledTimes(1);
      expect(nav).toHaveBeenCalledTimes(1);
      expect((nav.mock.calls[0][0] as CustomEvent).detail).toEqual({ path: DM_PATH });
    } finally {
      document.removeEventListener('nav-click', nav);
    }
  });

  it('carries unsent text into the DM draft', async () => {
    const el = await mount();
    (el as unknown as { messageText: string }).messageText = 'half-written';
    dmButton(el)!.click();
    expect(localStorage.getItem(DRAFT_KEY)).toBe('half-written');
  });

  it('appends unsent text to an existing DM draft', async () => {
    localStorage.setItem(DRAFT_KEY, 'earlier draft');
    const el = await mount();
    (el as unknown as { messageText: string }).messageText = 'new text';
    dmButton(el)!.click();
    expect(localStorage.getItem(DRAFT_KEY)).toBe('earlier draft\n\nnew text');
  });

  it('leaves an existing DM draft alone when nothing was typed', async () => {
    localStorage.setItem(DRAFT_KEY, 'earlier draft');
    const el = await mount();
    dmButton(el)!.click();
    expect(localStorage.getItem(DRAFT_KEY)).toBe('earlier draft');
  });

  it('is hidden when native chat is disabled', async () => {
    flags.nativeChat = false;
    const el = await mount();
    expect(dmButton(el)).toBeNull();
  });

  it('is hidden when the signed-in user is unknown', async () => {
    currentUser.id = '';
    const el = await mount();
    expect(dmButton(el)).toBeNull();
  });

  it('uses the userId property and appears once the user is passed in', async () => {
    currentUser.id = '';
    const el = await mount();
    expect(dmButton(el)).toBeNull();
    el.userId = USER;
    await el.updateComplete;
    const nav = vi.fn();
    document.addEventListener('nav-click', nav);
    try {
      dmButton(el)!.click();
      expect((nav.mock.calls[0][0] as CustomEvent).detail).toEqual({ path: DM_PATH });
    } finally {
      document.removeEventListener('nav-click', nav);
    }
  });

  it('prefers the userId property over the app-wide current user', async () => {
    currentUser.id = '33333333-3333-3333-3333-333333333333';
    const el = await mount();
    el.userId = USER;
    await el.updateComplete;
    const nav = vi.fn();
    document.addEventListener('nav-click', nav);
    try {
      dmButton(el)!.click();
      expect((nav.mock.calls[0][0] as CustomEvent).detail).toEqual({ path: DM_PATH });
    } finally {
      document.removeEventListener('nav-click', nav);
    }
  });
});
