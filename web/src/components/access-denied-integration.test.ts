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
 * Integration tests verifying the full event→handler→DOM pipeline used by
 * both app-shell and chat-shell for scion:access-denied events.
 *
 * The shells are Lit custom elements with heavy DOM trees; instantiating them
 * in happy-dom is brittle. Instead, these tests replicate the exact handler
 * logic from both shells (the _handled guard + showAccessDeniedToast call)
 * and verify the resulting DOM. This is faithful because the shells' handlers
 * were simplified to two-line delegations to the shared renderer.
 */

import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import type { AccessDeniedDetail } from '../client/api.js';
import { showAccessDeniedToast, _resetDedupState } from '../utils/access-denied.js';

// Stub sl-alert.toast() so Shoelace doesn't throw in test env.
const origCreateElement = document.createElement.bind(document);
vi.spyOn(document, 'createElement').mockImplementation(
  (tag: string, options?: ElementCreationOptions) => {
    const el = origCreateElement(tag, options);
    if (tag === 'sl-alert') {
      (el as unknown as Record<string, unknown>).toast = vi.fn();
    }
    return el;
  }
);

/**
 * Replicate app-shell's handleAccessDenied — the _handled flag + delegation.
 * This is the exact logic in app-shell.ts lines 243-247.
 */
function appShellHandler(event: CustomEvent<AccessDeniedDetail>): void {
  const detail = event.detail || {};
  (detail as Record<string, unknown>)._handled = true;
  showAccessDeniedToast(detail);
}

/**
 * Replicate chat-shell's handleAccessDenied — the _handled guard + delegation.
 * This is the exact logic in chat-shell.ts lines 209-215.
 */
function chatShellHandler(event: CustomEvent<AccessDeniedDetail>): void {
  const detail = event.detail || {};
  if ((detail as Record<string, unknown>)._handled) return;
  (detail as Record<string, unknown>)._handled = true;
  showAccessDeniedToast(detail);
}

/** Collect all sl-alert elements from the document. */
function getAlerts(): Element[] {
  return Array.from(document.querySelectorAll('sl-alert'));
}

/** Fire a scion:access-denied event on window and return the detail. */
function fireAccessDenied(detail: AccessDeniedDetail): AccessDeniedDetail {
  window.dispatchEvent(
    new CustomEvent('scion:access-denied', { detail })
  );
  return detail;
}

// ---------------------------------------------------------------------------
// app-shell handler integration
// ---------------------------------------------------------------------------

describe('app-shell access-denied handler integration', () => {
  beforeEach(() => {
    _resetDedupState();
    window.addEventListener(
      'scion:access-denied',
      appShellHandler as EventListener
    );
  });

  afterEach(() => {
    window.removeEventListener(
      'scion:access-denied',
      appShellHandler as EventListener
    );
    document.querySelectorAll('sl-alert').forEach((el) => el.remove());
    _resetDedupState();
  });

  it('renders two-line toast on structured access-denied event', () => {
    fireAccessDenied({
      action: 'delete',
      resource: 'agent',
      reason: 'Insufficient permissions',
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    const spans = alerts[0].querySelectorAll('span');
    expect(spans.length).toBe(2);
    expect(spans[0].textContent).toBe(
      "You don't have permission to perform this action."
    );
    expect(spans[1].textContent).toBe('Permission needed: delete on agent');
  });

  it('renders single-line toast on legacy access-denied event', () => {
    fireAccessDenied({
      action: 'forbidden',
      reason: 'Insufficient permissions',
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    const spans = alerts[0].querySelectorAll('span');
    expect(spans.length).toBe(1);
    expect(spans[0].textContent).toBe(
      "You don't have permission to perform this action."
    );
  });

  it('renders custom backend reason on first line', () => {
    fireAccessDenied({
      action: 'create',
      resource: 'agent',
      reason: 'Agents can only create sub-agents within their own project',
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    const spans = alerts[0].querySelectorAll('span');
    expect(spans[0].textContent).toBe(
      'Agents can only create sub-agents within their own project'
    );
    expect(spans[1].textContent).toBe('Permission needed: create on agent');
  });

  it('renders hostile payloads as text, never markup', () => {
    fireAccessDenied({
      action: '<script>alert(1)</script>',
      resource: '<img src=x onerror=alert(1)>',
      reason: '"><svg onload=alert(document.cookie)>',
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    // No injected elements.
    expect(alerts[0].querySelector('script')).toBeNull();
    expect(alerts[0].querySelector('img')).toBeNull();
    expect(alerts[0].querySelector('svg')).toBeNull();
    // Content is literal text.
    const spans = alerts[0].querySelectorAll('span');
    expect(spans[0].textContent).toContain('<svg');
    expect(spans[1].textContent).toContain('<script>');
    expect(spans[1].textContent).toContain('<img');
  });

  it('sets _handled flag to prevent chat-shell duplication', () => {
    const detail = fireAccessDenied({ action: 'delete', resource: 'agent' });
    expect((detail as Record<string, unknown>)._handled).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// chat-shell handler integration
// ---------------------------------------------------------------------------

describe('chat-shell access-denied handler integration', () => {
  beforeEach(() => {
    _resetDedupState();
    window.addEventListener(
      'scion:access-denied',
      chatShellHandler as EventListener
    );
  });

  afterEach(() => {
    window.removeEventListener(
      'scion:access-denied',
      chatShellHandler as EventListener
    );
    document.querySelectorAll('sl-alert').forEach((el) => el.remove());
    _resetDedupState();
  });

  it('renders two-line toast on structured access-denied event', () => {
    fireAccessDenied({
      action: 'manage',
      resource: 'project',
      reason: 'Insufficient permissions',
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    const spans = alerts[0].querySelectorAll('span');
    expect(spans.length).toBe(2);
    expect(spans[0].textContent).toBe(
      "You don't have permission to perform this action."
    );
    expect(spans[1].textContent).toBe('Permission needed: manage on project');
  });

  it('renders single-line toast on legacy access-denied event', () => {
    fireAccessDenied({ reason: 'Custom backend denial' });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    const spans = alerts[0].querySelectorAll('span');
    expect(spans.length).toBe(1);
    expect(spans[0].textContent).toBe('Custom backend denial');
  });

  it('skips toast when _handled flag is already set', () => {
    const alertsBefore = getAlerts().length;

    const detail: AccessDeniedDetail & { _handled?: boolean } = {
      action: 'delete',
      resource: 'agent',
    };
    detail._handled = true;
    window.dispatchEvent(
      new CustomEvent('scion:access-denied', { detail })
    );

    // No new alerts created.
    expect(getAlerts().length).toBe(alertsBefore);
  });

  it('renders hostile payloads as text, never markup', () => {
    fireAccessDenied({
      action: '<script>alert("xss")</script>',
      resource: '<img onerror=steal()>',
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    expect(alerts[0].querySelector('script')).toBeNull();
    expect(alerts[0].querySelector('img')).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Deduplication: both shells mounted
// ---------------------------------------------------------------------------

describe('app-shell + chat-shell deduplication', () => {
  beforeEach(() => {
    _resetDedupState();
    // app-shell handler runs first (registered first, like parent element).
    window.addEventListener(
      'scion:access-denied',
      appShellHandler as EventListener
    );
    window.addEventListener(
      'scion:access-denied',
      chatShellHandler as EventListener
    );
  });

  afterEach(() => {
    window.removeEventListener(
      'scion:access-denied',
      appShellHandler as EventListener
    );
    window.removeEventListener(
      'scion:access-denied',
      chatShellHandler as EventListener
    );
    document.querySelectorAll('sl-alert').forEach((el) => el.remove());
    _resetDedupState();
  });

  it('produces exactly one toast when both shells are mounted', () => {
    fireAccessDenied({
      action: 'delete',
      resource: 'agent',
      reason: 'Insufficient permissions',
    });

    // app-shell sets _handled, so chat-shell skips → exactly one toast.
    expect(getAlerts().length).toBe(1);
  });

  it('deduplicates hostile payloads too', () => {
    fireAccessDenied({
      action: '<script>alert(1)</script>',
      resource: '<img onerror=x>',
    });

    expect(getAlerts().length).toBe(1);
    expect(getAlerts()[0].querySelector('script')).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// User Administration denial payloads — two-line toast
// ---------------------------------------------------------------------------

describe('user admin denial payloads', () => {
  beforeEach(() => {
    _resetDedupState();
    window.addEventListener(
      'scion:access-denied',
      appShellHandler as EventListener
    );
  });

  afterEach(() => {
    window.removeEventListener(
      'scion:access-denied',
      appShellHandler as EventListener
    );
    document.querySelectorAll('sl-alert').forEach((el) => el.remove());
    _resetDedupState();
  });

  it('renders two-line toast for promote denial on user', () => {
    fireAccessDenied({
      action: 'promote',
      resource: 'user',
      reason: 'requires user.promote permission',
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    const spans = alerts[0].querySelectorAll('span');
    expect(spans.length).toBe(2);
    expect(spans[0].textContent).toBe('requires user.promote permission');
    expect(spans[1].textContent).toBe('Permission needed: promote on user');
  });

  it('renders two-line toast for suspend denial on user', () => {
    fireAccessDenied({
      action: 'suspend',
      resource: 'user',
      reason: 'requires user.suspend permission',
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    const spans = alerts[0].querySelectorAll('span');
    expect(spans.length).toBe(2);
    expect(spans[0].textContent).toBe('requires user.suspend permission');
    expect(spans[1].textContent).toBe('Permission needed: suspend on user');
  });

  it('renders two-line toast for update denial on user', () => {
    fireAccessDenied({
      action: 'update',
      resource: 'user',
      reason: "requires user.update permission to modify another user's profile",
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    const spans = alerts[0].querySelectorAll('span');
    expect(spans.length).toBe(2);
    expect(spans[0].textContent).toBe(
      "requires user.update permission to modify another user's profile"
    );
    expect(spans[1].textContent).toBe('Permission needed: update on user');
  });

  it('renders two-line toast for delete denial on user', () => {
    fireAccessDenied({
      action: 'delete',
      resource: 'user',
      reason: 'requires user.delete permission',
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    const spans = alerts[0].querySelectorAll('span');
    expect(spans.length).toBe(2);
    expect(spans[0].textContent).toBe('requires user.delete permission');
    expect(spans[1].textContent).toBe('Permission needed: delete on user');
  });

  it('renders two-line toast for role_binding create denial', () => {
    fireAccessDenied({
      action: 'create',
      resource: 'role_binding',
      reason: 'Insufficient permissions',
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    const spans = alerts[0].querySelectorAll('span');
    expect(spans.length).toBe(2);
    expect(spans[0].textContent).toBe(
      "You don't have permission to perform this action."
    );
    expect(spans[1].textContent).toBe(
      'Permission needed: create on role_binding'
    );
  });

  it('renders all user admin payloads as safe literal text', () => {
    // Ensure textContent is used, not innerHTML, even for user admin denials.
    fireAccessDenied({
      action: 'promote',
      resource: 'user',
      reason: '<script>alert("xss")</script>',
    });

    const alerts = getAlerts();
    expect(alerts.length).toBe(1);
    expect(alerts[0].querySelector('script')).toBeNull();
    const spans = alerts[0].querySelectorAll('span');
    expect(spans[0].textContent).toContain('<script>');
  });
});

// ---------------------------------------------------------------------------
// Duplicate-toast suppression (cross-event dedup)
// ---------------------------------------------------------------------------

describe('duplicate-toast suppression', () => {
  beforeEach(() => {
    _resetDedupState();
    window.addEventListener(
      'scion:access-denied',
      appShellHandler as EventListener
    );
  });

  afterEach(() => {
    window.removeEventListener(
      'scion:access-denied',
      appShellHandler as EventListener
    );
    document.querySelectorAll('sl-alert').forEach((el) => el.remove());
    _resetDedupState();
  });

  it('coalesces two same-key events within the 500ms window into one toast', () => {
    fireAccessDenied({ action: 'read', resource: 'hub' });
    fireAccessDenied({ action: 'read', resource: 'hub' });

    expect(getAlerts().length).toBe(1);
  });

  it('allows same-key event after window expires', () => {
    // First event
    fireAccessDenied({ action: 'read', resource: 'hub' });
    expect(getAlerts().length).toBe(1);

    // Simulate window expiry by resetting dedup state
    _resetDedupState();

    // Second event after window
    fireAccessDenied({ action: 'read', resource: 'hub' });
    expect(getAlerts().length).toBe(2);
  });

  it('allows distinct action+resource keys within the window', () => {
    fireAccessDenied({ action: 'read', resource: 'hub' });
    fireAccessDenied({ action: 'update', resource: 'hub' });

    expect(getAlerts().length).toBe(2);
  });

  it('allows distinct resource with same action within the window', () => {
    fireAccessDenied({ action: 'read', resource: 'agent' });
    fireAccessDenied({ action: 'read', resource: 'project' });

    expect(getAlerts().length).toBe(2);
  });

  it('coalesces three identical rapid events', () => {
    fireAccessDenied({ action: 'read', resource: 'hub' });
    fireAccessDenied({ action: 'read', resource: 'hub' });
    fireAccessDenied({ action: 'read', resource: 'hub' });

    expect(getAlerts().length).toBe(1);
  });

  it('coalesces legacy events with no action or resource', () => {
    // Both have empty action+resource -> same key ":"
    fireAccessDenied({ reason: 'Insufficient permissions' });
    fireAccessDenied({ reason: 'Insufficient permissions' });

    expect(getAlerts().length).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// Dedup with both shells mounted
// ---------------------------------------------------------------------------

describe('dedup with both app-shell and chat-shell mounted', () => {
  beforeEach(() => {
    _resetDedupState();
    window.addEventListener(
      'scion:access-denied',
      appShellHandler as EventListener
    );
    window.addEventListener(
      'scion:access-denied',
      chatShellHandler as EventListener
    );
  });

  afterEach(() => {
    window.removeEventListener(
      'scion:access-denied',
      appShellHandler as EventListener
    );
    window.removeEventListener(
      'scion:access-denied',
      chatShellHandler as EventListener
    );
    document.querySelectorAll('sl-alert').forEach((el) => el.remove());
    _resetDedupState();
  });

  it('produces one toast per event with both shells mounted', () => {
    // Single event: app-shell handles, chat-shell skips (_handled flag)
    fireAccessDenied({ action: 'read', resource: 'hub' });
    expect(getAlerts().length).toBe(1);
  });

  it('coalesces duplicate events with both shells mounted', () => {
    // Two identical events: first handled by app-shell, second suppressed by dedup
    fireAccessDenied({ action: 'read', resource: 'hub' });
    fireAccessDenied({ action: 'read', resource: 'hub' });
    expect(getAlerts().length).toBe(1);
  });

  it('distinct events each produce one toast with both shells', () => {
    fireAccessDenied({ action: 'read', resource: 'agent' });
    fireAccessDenied({ action: 'update', resource: 'project' });
    expect(getAlerts().length).toBe(2);
  });
});
