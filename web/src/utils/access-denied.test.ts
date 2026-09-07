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

import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import type { AccessDeniedDetail } from '../client/api.js';
import { formatAccessDenied, showAccessDeniedToast, _resetDedupState } from './access-denied.js';

/**
 * Stub sl-alert.toast() on any sl-alert elements created during a test.
 * Returns a restore function.
 */
function stubAlertToast() {
  const origCreate = document.createElement.bind(document);
  vi.spyOn(document, 'createElement').mockImplementation(
    (tag: string, options?: ElementCreationOptions) => {
      const el = origCreate(tag, options);
      if (tag === 'sl-alert') {
        (el as unknown as Record<string, unknown>).toast = vi.fn();
      }
      return el;
    }
  );
}

// ---------------------------------------------------------------------------
// formatAccessDenied — two-line rendering
// ---------------------------------------------------------------------------

describe('formatAccessDenied', () => {
  it('returns friendly primary and structured secondary for full detail', () => {
    const detail: AccessDeniedDetail = {
      action: 'delete',
      resource: 'agent',
      reason: 'Insufficient permissions',
    };
    const result = formatAccessDenied(detail);
    expect(result.primary).toBe("You don't have permission to perform this action.");
    expect(result.secondary).toBe('Permission needed: delete on agent');
  });

  it('uses custom reason as primary when not generic', () => {
    const detail: AccessDeniedDetail = {
      action: 'create',
      resource: 'project',
      reason: 'You cannot create agents in this project',
    };
    const result = formatAccessDenied(detail);
    expect(result.primary).toBe('You cannot create agents in this project');
    expect(result.secondary).toBe('Permission needed: create on project');
  });

  it('maps known action codes to friendly labels', () => {
    const detail: AccessDeniedDetail = { action: 'attach', resource: 'agent' };
    const result = formatAccessDenied(detail);
    expect(result.secondary).toBe('Permission needed: manage on agent');
  });

  it('passes through unknown action codes as-is', () => {
    const detail: AccessDeniedDetail = { action: 'custom_action', resource: 'widget' };
    const result = formatAccessDenied(detail);
    expect(result.secondary).toBe('Permission needed: custom_action on widget');
  });

  it('renders secondary with only action when resource is absent', () => {
    const detail: AccessDeniedDetail = { action: 'delete' };
    const result = formatAccessDenied(detail);
    expect(result.secondary).toBe('Permission needed: delete');
  });

  it('renders secondary with only resource when action is absent', () => {
    const detail: AccessDeniedDetail = { resource: 'agent' };
    const result = formatAccessDenied(detail);
    expect(result.secondary).toBe('Resource: agent');
  });

  it('degrades gracefully for legacy 403 with no detail', () => {
    const result = formatAccessDenied({});
    expect(result.primary).toBe("You don't have permission to perform this action.");
    expect(result.secondary).toBeUndefined();
  });

  it('degrades gracefully when only reason is present', () => {
    const detail: AccessDeniedDetail = { reason: 'Access denied' };
    const result = formatAccessDenied(detail);
    expect(result.primary).toBe('Access denied');
    expect(result.secondary).toBeUndefined();
  });

  it('omits secondary when action is just "forbidden" with no resource', () => {
    const detail: AccessDeniedDetail = { action: 'forbidden', reason: 'Insufficient permissions' };
    const result = formatAccessDenied(detail);
    expect(result.primary).toBe("You don't have permission to perform this action.");
    expect(result.secondary).toBeUndefined();
  });

  it('shows secondary when action is "forbidden" but resource is present', () => {
    const detail: AccessDeniedDetail = { action: 'forbidden', resource: 'agent' };
    const result = formatAccessDenied(detail);
    expect(result.secondary).toBe('Permission needed: forbidden on agent');
  });

  it('does not leak resource ID (only resource type is used)', () => {
    const detail: AccessDeniedDetail = {
      action: 'delete',
      resource: 'agent',
      reason: 'Insufficient permissions',
    };
    const result = formatAccessDenied(detail);
    expect(result.primary).not.toContain('secret');
    expect(result.secondary).not.toContain('secret');
  });

  // ----- R2: hostile / surprising input sanitization -----

  it('treats <script> payload in action as literal text', () => {
    const detail: AccessDeniedDetail = {
      action: '<script>alert("xss")</script>',
      resource: 'agent',
    };
    const result = formatAccessDenied(detail);
    expect(result.secondary).toContain('<script>');
    // The string itself is literal text, not stripped or interpreted.
    expect(result.secondary).toBe(
      'Permission needed: <script>alert("xss")</script> on agent'
    );
  });

  it('treats <img onerror> payload in resource as literal text', () => {
    const detail: AccessDeniedDetail = {
      action: 'delete',
      resource: '<img src=x onerror=alert(1)>',
    };
    const result = formatAccessDenied(detail);
    expect(result.secondary).toBe(
      'Permission needed: delete on <img src=x onerror=alert(1)>'
    );
  });

  it('treats hostile strings in reason as literal text', () => {
    const hostile = '"><svg onload=alert(document.cookie)>';
    const result = formatAccessDenied({ reason: hostile });
    expect(result.primary).toBe(hostile);
  });

  it('handles extremely long action/resource without error', () => {
    const long = 'x'.repeat(10000);
    const result = formatAccessDenied({ action: long, resource: long });
    expect(result.secondary).toContain(long);
  });

  // ----- User Administration denial payloads -----

  it('renders promote denial on user resource', () => {
    const detail: AccessDeniedDetail = {
      action: 'promote',
      resource: 'user',
      reason: 'requires user.promote permission',
    };
    const result = formatAccessDenied(detail);
    expect(result.primary).toBe('requires user.promote permission');
    expect(result.secondary).toBe('Permission needed: promote on user');
  });

  it('renders suspend denial on user resource', () => {
    const detail: AccessDeniedDetail = {
      action: 'suspend',
      resource: 'user',
      reason: 'requires user.suspend permission',
    };
    const result = formatAccessDenied(detail);
    expect(result.primary).toBe('requires user.suspend permission');
    expect(result.secondary).toBe('Permission needed: suspend on user');
  });

  it('renders update denial on user resource', () => {
    const detail: AccessDeniedDetail = {
      action: 'update',
      resource: 'user',
      reason: 'requires user.update permission to modify another user\'s profile',
    };
    const result = formatAccessDenied(detail);
    expect(result.primary).toBe(
      'requires user.update permission to modify another user\'s profile'
    );
    expect(result.secondary).toBe('Permission needed: update on user');
  });

  it('renders delete denial on user resource', () => {
    const detail: AccessDeniedDetail = {
      action: 'delete',
      resource: 'user',
      reason: 'requires user.delete permission',
    };
    const result = formatAccessDenied(detail);
    expect(result.primary).toBe('requires user.delete permission');
    expect(result.secondary).toBe('Permission needed: delete on user');
  });

  it('renders create denial on role_binding resource', () => {
    const detail: AccessDeniedDetail = {
      action: 'create',
      resource: 'role_binding',
      reason: 'Insufficient permissions',
    };
    const result = formatAccessDenied(detail);
    expect(result.primary).toBe(
      "You don't have permission to perform this action."
    );
    expect(result.secondary).toBe('Permission needed: create on role_binding');
  });

  it('renders delete denial on role_binding resource', () => {
    const detail: AccessDeniedDetail = {
      action: 'delete',
      resource: 'role_binding',
      reason: 'Insufficient permissions',
    };
    const result = formatAccessDenied(detail);
    expect(result.primary).toBe(
      "You don't have permission to perform this action."
    );
    expect(result.secondary).toBe('Permission needed: delete on role_binding');
  });

  it('does not expose user IDs in user admin denials', () => {
    const detail: AccessDeniedDetail = {
      action: 'promote',
      resource: 'user',
      reason: 'requires user.promote permission',
    };
    const result = formatAccessDenied(detail);
    // The formatted output should not contain any UUID-shaped strings.
    const uuidPattern =
      /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i;
    expect(result.primary).not.toMatch(uuidPattern);
    expect(result.secondary).not.toMatch(uuidPattern);
  });
});

// ---------------------------------------------------------------------------
// showAccessDeniedToast — DOM integration
// ---------------------------------------------------------------------------

describe('showAccessDeniedToast', () => {
  beforeEach(() => {
    _resetDedupState();
  });

  afterEach(() => {
    document.querySelectorAll('sl-alert').forEach((el) => el.remove());
    vi.restoreAllMocks();
    _resetDedupState();
  });

  it('creates an sl-alert with two-line content for structured detail', () => {
    stubAlertToast();

    showAccessDeniedToast({
      action: 'delete',
      resource: 'agent',
      reason: 'Insufficient permissions',
    });

    const alert = document.querySelector('sl-alert');
    expect(alert).not.toBeNull();
    const spans = alert!.querySelectorAll('span');
    expect(spans.length).toBe(2);
    expect(spans[0].textContent).toBe("You don't have permission to perform this action.");
    expect(spans[1].textContent).toBe('Permission needed: delete on agent');
  });

  it('creates a single-line toast for legacy 403 with no detail', () => {
    stubAlertToast();
    // Should not throw.
    showAccessDeniedToast({});
    // showToast creates an sl-alert too; verify one was appended.
    const alert = document.querySelector('sl-alert');
    expect(alert).not.toBeNull();
  });

  // R2: hostile payloads are textContent, never innerHTML
  it('renders hostile action/resource as textContent, not markup', () => {
    stubAlertToast();

    showAccessDeniedToast({
      action: '<script>alert(1)</script>',
      resource: '<img src=x onerror=alert(1)>',
    });

    const alert = document.querySelector('sl-alert');
    expect(alert).not.toBeNull();
    const secondarySpan = alert!.querySelectorAll('span')[1];
    // Must be textContent (literal), not parsed HTML. If it were innerHTML,
    // the DOM would contain a <script> or <img> element.
    expect(alert!.querySelector('script')).toBeNull();
    expect(alert!.querySelector('img')).toBeNull();
    expect(secondarySpan.textContent).toContain('<script>');
    expect(secondarySpan.textContent).toContain('<img');
  });

  it('renders hostile reason as textContent in single-line toast', () => {
    stubAlertToast();

    showAccessDeniedToast({
      reason: '<img src=x onerror=alert(document.cookie)>',
    });

    const alert = document.querySelector('sl-alert');
    expect(alert).not.toBeNull();
    expect(alert!.querySelector('img')).toBeNull();
    const span = alert!.querySelector('span');
    expect(span!.textContent).toContain('<img');
  });
});

// ---------------------------------------------------------------------------
// showAccessDeniedToast — dedup coalescing with mocked Date.now
// ---------------------------------------------------------------------------

describe('showAccessDeniedToast dedup', () => {
  let nowMs: number;

  beforeEach(() => {
    _resetDedupState();
    stubAlertToast();
    nowMs = 1000;
    vi.spyOn(Date, 'now').mockImplementation(() => nowMs);
  });

  afterEach(() => {
    vi.restoreAllMocks();
    document.querySelectorAll('sl-alert').forEach((el) => el.remove());
    _resetDedupState();
  });

  it('suppresses duplicate same-key toast within window', () => {
    showAccessDeniedToast({ action: 'read', resource: 'hub', reason: 'Insufficient permissions' });
    nowMs += 100; // +100ms, still within 500ms
    showAccessDeniedToast({ action: 'read', resource: 'hub', reason: 'Insufficient permissions' });

    expect(document.querySelectorAll('sl-alert').length).toBe(1);
  });

  it('suppresses at 499ms but allows at 501ms', () => {
    showAccessDeniedToast({ action: 'read', resource: 'hub' });

    nowMs += 499;
    showAccessDeniedToast({ action: 'read', resource: 'hub' });
    expect(document.querySelectorAll('sl-alert').length).toBe(1);

    nowMs += 2; // total 501ms from last recorded time
    showAccessDeniedToast({ action: 'read', resource: 'hub' });
    expect(document.querySelectorAll('sl-alert').length).toBe(2);
  });

  it('allows distinct keys within window', () => {
    showAccessDeniedToast({ action: 'read', resource: 'hub' });
    nowMs += 50;
    showAccessDeniedToast({ action: 'update', resource: 'hub' });

    expect(document.querySelectorAll('sl-alert').length).toBe(2);
  });

  it('suppresses A→B→A interleaved within window', () => {
    showAccessDeniedToast({ action: 'read', resource: 'hub' });   // A fires
    nowMs += 100;
    showAccessDeniedToast({ action: 'update', resource: 'hub' }); // B fires (distinct)
    nowMs += 100;
    showAccessDeniedToast({ action: 'read', resource: 'hub' });   // A again at +200ms — suppressed

    expect(document.querySelectorAll('sl-alert').length).toBe(2);
  });

  it('allows same key after window expires', () => {
    showAccessDeniedToast({ action: 'read', resource: 'hub' });
    nowMs += 501;
    showAccessDeniedToast({ action: 'read', resource: 'hub' });

    expect(document.querySelectorAll('sl-alert').length).toBe(2);
  });
});
