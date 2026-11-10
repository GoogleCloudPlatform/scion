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
 * Review summary rows (ptone/scion#6853): the excerpt of the reviewer's
 * summary note is shown as plain text under the message that attributes it,
 * the "View artifact" link is built from the resource fields alone, and the
 * browser notification body is plain text.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render } from 'lit';

import { PUSH_STORAGE_KEYS } from '../../client/push-preference.js';
import type { Notification } from '../../shared/types.js';
import {
  ARTIFACT_REVIEW_SUBMITTED_STATUS,
  ARTIFACT_REVIEW_SUMMARY_KIND,
  notificationFromEvent,
  reviewSummaryExcerpt,
  reviewSummaryLink,
} from './notification-tray.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('[]', { status: 200 }))),
}));

let popups: Array<{ title: string; options: NotificationOptions }> = [];

class FakeNotification {
  static permission: NotificationPermission = 'granted';
  constructor(title: string, options: NotificationOptions = {}) {
    popups.push({ title, options });
  }
}

const ARTIFACT_ID = '6a2b3c4d-0000-4000-8000-000000000001';
const PROJECT_ID = '6a2b3c4d-0000-4000-8000-0000000000aa';
const OTHER_ARTIFACT_ID = 'deadbeef-0000-4000-8000-000000000666';
const OTHER_PROJECT_ID = 'deadbeef-0000-4000-8000-000000000777';
const NIL_UUID = '00000000-0000-0000-0000-000000000000';

/**
 * The excerpt the hub stores for its hostile test note
 * (hostileReviewNote in pkg/hub/artifacts_review_summary_test.go): controls,
 * bidi and zero-width characters and line breaks already removed, capped
 * with an ellipsis. Markup, mentions, URLs and resource-like strings remain
 * as literal text.
 */
const HOSTILE_EXCERPT =
  '[31mRED[0m ]8;;https://evil.example/click]8;; 2Jgnp.exe iso zerowidth nulbel line2 line3 ' +
  'line4 line5 **bold** [link](https://evil.example/a) <img src=x onerror=alert(1)> ' +
  '<script>x</script> @channel <!channel> <@U012345> @here https://evil.…';

function summary(overrides: Partial<Notification> = {}): Notification {
  return {
    id: 'notif-review',
    subscriptionId: NIL_UUID,
    agentId: NIL_UUID,
    projectId: PROJECT_ID,
    subscriberType: 'user',
    subscriberId: 'me',
    status: ARTIFACT_REVIEW_SUBMITTED_STATUS,
    message: 'Review summary from Alice (v3 of your artifact):',
    dispatched: false,
    acknowledged: false,
    createdAt: new Date().toISOString(),
    kind: ARTIFACT_REVIEW_SUMMARY_KIND,
    resourceType: 'artifact',
    resourceId: ARTIFACT_ID,
    excerpt: HOSTILE_EXCERPT,
    ...overrides,
  };
}

function renderRow(n: Notification): HTMLElement {
  const host = document.createElement('div');
  const tray: any = document.createElement('scion-notification-tray');
  render(tray.renderItem(n), host);
  return host;
}

describe('notification tray: review summary', () => {
  beforeEach(() => {
    popups = [];
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    localStorage.setItem(PUSH_STORAGE_KEYS.agent, 'true');
  });

  afterEach(() => {
    localStorage.clear();
  });

  it('matches the hub kind and status', () => {
    expect(ARTIFACT_REVIEW_SUMMARY_KIND).toBe('artifact_review_summary');
    expect(ARTIFACT_REVIEW_SUBMITTED_STATUS).toBe('ARTIFACT_REVIEW_SUBMITTED');
  });

  it('renders a hostile excerpt as text under the attributing message', () => {
    const host = renderRow(summary());

    const message = host.querySelector('.notif-message');
    expect(message?.textContent).toBe('Review summary from Alice (v3 of your artifact):');
    const excerpt = host.querySelector('.notif-excerpt');
    expect(excerpt).not.toBeNull();
    expect(excerpt?.textContent).toBe(`“${HOSTILE_EXCERPT}”`);
    expect(excerpt?.children).toHaveLength(0);
    // The excerpt follows the message, in its own element.
    expect(message?.compareDocumentPosition(excerpt!)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    // No markup from the excerpt became elements anywhere in the row.
    expect(host.querySelector('img, script, b, strong, em')).toBeNull();
    expect(host.innerHTML).not.toContain('<img');
    expect(host.innerHTML).toContain('&lt;img src=x onerror=alert(1)&gt;');
  });

  it('links only to the artifact named by the resource fields', () => {
    const host = renderRow(summary());
    const links = [...host.querySelectorAll('a')];
    expect(links.map((a) => a.getAttribute('href'))).toEqual([
      `/projects/${PROJECT_ID}/artifacts/${ARTIFACT_ID}`,
    ]);
    expect(links[0].textContent?.trim()).toBe('View artifact');
    expect(host.innerHTML).not.toContain('href="https://evil.example');
  });

  it('builds no link from the excerpt when the resource fields name none', () => {
    const pathInExcerpt = `/projects/${OTHER_PROJECT_ID}/artifacts/${OTHER_ARTIFACT_ID}`;
    for (const overrides of [
      { resourceId: ARTIFACT_ID.toUpperCase() },
      { resourceId: `../${ARTIFACT_ID}` },
      { resourceId: undefined },
      { resourceType: 'agent' },
      { projectId: NIL_UUID },
      { projectId: '' },
    ] as Partial<Notification>[]) {
      const n = summary({ ...overrides, excerpt: pathInExcerpt });
      expect(reviewSummaryLink(n)).toBeNull();
      const host = renderRow(n);
      expect(host.querySelectorAll('a')).toHaveLength(0);
      expect(host.querySelector('.notif-excerpt')?.textContent).toBe(`“${pathInExcerpt}”`);
    }
  });

  it('keeps the link the same whatever the excerpt says', () => {
    const plain = reviewSummaryLink(summary({ excerpt: 'Looks good.' }));
    for (const excerpt of [
      HOSTILE_EXCERPT,
      `scion://artifact/${OTHER_ARTIFACT_ID}`,
      `/projects/${OTHER_PROJECT_ID}/artifacts/${OTHER_ARTIFACT_ID}`,
      `{"resourceId":"${OTHER_ARTIFACT_ID}","projectId":"${OTHER_PROJECT_ID}"}`,
    ]) {
      expect(reviewSummaryLink(summary({ excerpt }))).toBe(plain);
    }
  });

  it('shows no excerpt for any other kind', () => {
    const n = summary({ kind: 'artifact_grant', status: 'ARTIFACT_SHARED' });
    expect(reviewSummaryExcerpt(n)).toBe('');
    expect(reviewSummaryLink(n)).toBeNull();
    const host = renderRow(n);
    expect(host.querySelector('.notif-excerpt')).toBeNull();
    expect(host.textContent).not.toContain('@channel');
  });

  it('shows no excerpt block for a review without a note', () => {
    const host = renderRow(
      summary({ excerpt: undefined, message: 'Review (v3) of your artifact submitted by Alice.' })
    );
    expect(host.querySelector('.notif-excerpt')).toBeNull();
    expect(host.querySelectorAll('a')).toHaveLength(1);
  });

  it('pops a plain-text browser notification with the attributed excerpt', () => {
    const tray: any = document.createElement('scion-notification-tray');
    const n = summary();
    tray.dispatchBrowserNotification(n);
    expect(popups).toHaveLength(1);
    expect(popups[0].title).toBe('Review Submitted');
    expect(popups[0].options.body).toBe(`${n.message}\n“${HOSTILE_EXCERPT}”`);

    popups = [];
    tray.dispatchBrowserNotification(summary({ excerpt: undefined }));
    expect(popups[0].options.body).toBe(n.message);
    popups = [];
    tray.dispatchBrowserNotification(summary({ kind: 'artifact_grant' }));
    expect(popups[0].options.body).toBe(n.message);
  });

  it('strips controls and breaks a raw excerpt still carries, in the tray and the push body', () => {
    const raw = 'Ship\u202e it\x1b[2J\nnow\u2028<!channel> @here <b>x</b>\u200b';
    const shown = 'Ship it[2J now <!channel> @here <b>x</b>';
    const n = summary({ excerpt: raw });
    expect(reviewSummaryExcerpt(n)).toBe(shown);
    const host = renderRow(n);
    expect(host.querySelector('.notif-excerpt')?.textContent).toBe(`“${shown}”`);
    expect(host.querySelector('.notif-excerpt')?.children).toHaveLength(0);
    const tray: any = document.createElement('scion-notification-tray');
    tray.dispatchBrowserNotification(n);
    expect(popups[0].options.body).toBe(`${n.message}\n“${shown}”`);
    for (const bad of ['\u202e', '\x1b', '\u2028', '\u200b']) {
      expect(host.textContent).not.toContain(bad);
      expect(popups[0].options.body).not.toContain(bad);
    }
  });

  it('collapses the spaces around a replaced break in a raw excerpt', () => {
    expect(reviewSummaryExcerpt(summary({ excerpt: 'a \n b' }))).toBe('a b');
    expect(reviewSummaryExcerpt(summary({ excerpt: ' a\r\n\u2028 \u200b b \n' }))).toBe('a b');
  });

  it('keeps the kind, resource fields and excerpt of a user event', () => {
    const payload = {
      id: 'notif-review',
      agentId: NIL_UUID,
      projectId: PROJECT_ID,
      status: ARTIFACT_REVIEW_SUBMITTED_STATUS,
      message: 'Review summary from Alice (v3 of your artifact):',
      createdAt: '2026-11-04T00:00:00.000Z',
      subscriberId: 'me',
      kind: ARTIFACT_REVIEW_SUMMARY_KIND,
      resourceType: 'artifact',
      resourceId: ARTIFACT_ID,
      excerpt: HOSTILE_EXCERPT,
    };
    const n = notificationFromEvent(payload, 'me');
    expect(n).toMatchObject({
      kind: ARTIFACT_REVIEW_SUMMARY_KIND,
      resourceType: 'artifact',
      resourceId: ARTIFACT_ID,
      excerpt: HOSTILE_EXCERPT,
      projectId: PROJECT_ID,
    });
    const typed = notificationFromEvent({ ...payload, excerpt: 7, resourceId: {} }, 'me');
    expect(typed?.excerpt).toBeUndefined();
    expect(typed?.resourceId).toBeUndefined();
  });
});
