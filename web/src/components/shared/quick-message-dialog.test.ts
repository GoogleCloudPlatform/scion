/**
 * Tests for <scion-quick-message-dialog> cross-project label.
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
