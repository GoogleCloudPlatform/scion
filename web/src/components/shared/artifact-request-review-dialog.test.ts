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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import './artifact-request-review-dialog.js';
import type { ScionArtifactRequestReviewDialog } from './artifact-request-review-dialog.js';
import type { Artifact, CreateReviewRequestsResponse } from '../../client/artifacts.js';

const ARTIFACT: Artifact = {
  id: '22222222-2222-2222-2222-222222222222',
  ref: 'scion://artifact/22222222-2222-2222-2222-222222222222',
  scopeKind: 'project',
  scopeRef: '11111111-1111-1111-1111-111111111111',
  ownerKind: 'user',
  ownerRef: 'u-alice',
  title: 'Launch Plan',
  currentSeq: 2,
  createdAt: '2026-11-25T00:00:00Z',
  updatedAt: '2026-11-25T00:00:00Z',
};

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function settle(el: ScionArtifactRequestReviewDialog): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

describe('scion-artifact-request-review-dialog', () => {
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;

  beforeEach(() => {
    fetchMock = vi.fn<typeof fetch>();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = '';
  });

  async function mount(): Promise<ScionArtifactRequestReviewDialog> {
    const el = document.createElement(
      'scion-artifact-request-review-dialog'
    ) as ScionArtifactRequestReviewDialog;
    el.artifact = ARTIFACT;
    el.currentUserId = 'u-alice';
    el.open = true;
    document.body.appendChild(el);
    await settle(el);
    return el;
  }

  it('searches users (excluding self), picks reviewers, validates 1000-rune note, and shows per-reviewer results', async () => {
    const responseBody: CreateReviewRequestsResponse = {
      requests: [
        {
          reviewer: 'user:u-bob',
          outcome: 'created',
          access: 'existing',
          notified: true,
          request: {
            id: 'rr-1',
            reviewer: { ref: 'u-bob', displayName: 'Bob' },
            requester: { kind: 'user', ref: 'u-alice', displayName: 'Alice' },
            requestedSeq: 2,
            note: 'Please check section 2',
            access: 'existing',
            state: 'open',
            createdAt: '2026-11-25T10:00:00Z',
          },
        },
        {
          reviewer: 'user:u-carol',
          outcome: 'reminded',
          access: 'existing',
          notified: false,
          notifySkipped: 'rate_limited',
        },
        {
          reviewer: 'user:u-dave',
          outcome: 'unchanged',
          access: 'existing',
          notified: false,
        },
        {
          reviewer: 'user:u-eve',
          outcome: 'refused',
          code: 'reviewer_cannot_review',
          notified: false,
        },
      ],
    };

    fetchMock.mockImplementation((input: string | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/users?search=')) {
        return Promise.resolve(
          jsonResponse({
            users: [
              { id: 'u-alice', email: 'alice@example.com', displayName: 'Alice' },
              { id: 'u-bob', email: 'bob@example.com', displayName: 'Bob' },
              { id: 'u-carol', email: 'carol@example.com', displayName: 'Carol' },
              { id: 'u-dave', email: 'dave@example.com', displayName: 'Dave' },
              { id: 'u-eve', email: 'eve@example.com', displayName: 'Eve' },
            ],
          })
        );
      }
      if (url === `/api/v1/artifacts/${ARTIFACT.id}/review-requests`) {
        return Promise.resolve(jsonResponse(responseBody));
      }
      return Promise.resolve(jsonResponse({ error: 'unexpected' }, 500));
    });

    const el = await mount();
    const events: CreateReviewRequestsResponse[] = [];
    el.addEventListener('artifact-review-requested', (e) => {
      events.push((e as CustomEvent<CreateReviewRequestsResponse>).detail);
    });

    // Search for users; Alice (self) must be excluded.
    const searchInput = el.shadowRoot!.querySelector('.reviewer-search') as HTMLElement & {
      value: string;
    };
    searchInput.value = 'example';
    searchInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await new Promise((r) => setTimeout(r, 300));
    await settle(el);

    const suggestionButtons = [
      ...el.shadowRoot!.querySelectorAll('.suggestions button'),
    ] as HTMLButtonElement[];
    expect(suggestionButtons.map((b) => b.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      'Bob bob@example.com',
      'Carol carol@example.com',
      'Dave dave@example.com',
      'Eve eve@example.com',
    ]);

    // Pick Bob, then search again to pick Carol, Dave, Eve.
    for (let i = 0; i < 4; i++) {
      const first = el.shadowRoot!.querySelector('.suggestions button') as HTMLButtonElement;
      first.click();
      await settle(el);
      if (i < 3) {
        searchInput.value = 'example';
        searchInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
        await new Promise((r) => setTimeout(r, 300));
        await settle(el);
      }
    }

    const tags = [...el.shadowRoot!.querySelectorAll('.selected-list sl-tag')];
    expect(tags).toHaveLength(4);

    // Note > 1000 runes disables Submit (emoji counts as 1 rune each).
    const textarea = el.shadowRoot!.querySelector('.request-note') as HTMLElement & {
      value: string;
    };
    const submitBtn = el.shadowRoot!.querySelector('.submit-request-btn') as HTMLButtonElement;
    textarea.value = '😀'.repeat(1001);
    textarea.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await settle(el);
    expect(el.shadowRoot!.textContent).toContain('1001 / 1000');
    expect(submitBtn.hasAttribute('disabled')).toBe(true);

    // Valid note re-enables Submit.
    textarea.value = 'Please check section 2';
    textarea.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await settle(el);
    expect(el.shadowRoot!.textContent).toContain('22 / 1000');
    expect(submitBtn.hasAttribute('disabled')).toBe(false);

    submitBtn.click();
    await settle(el);

    // Verify POST payload.
    const postCall = fetchMock.mock.calls.find((c) => {
      const u = typeof c[0] === 'string' ? c[0] : c[0] instanceof URL ? c[0].toString() : c[0].url;
      return u === `/api/v1/artifacts/${ARTIFACT.id}/review-requests`;
    });
    expect(postCall).toBeDefined();
    expect(JSON.parse((postCall![1] as RequestInit).body as string)).toEqual({
      reviewers: ['user:u-bob', 'user:u-carol', 'user:u-dave', 'user:u-eve'],
      note: 'Please check section 2',
    });

    // Verify per-reviewer outcomes rendered.
    const rows = [...el.shadowRoot!.querySelectorAll('.result-row')];
    expect(rows).toHaveLength(4);
    expect(rows[0].textContent).toContain('Bob');
    expect(rows[0].textContent).toContain('Requested');
    expect(rows[0].textContent).toContain('Notified');
    expect(rows[1].textContent).toContain('Carol');
    expect(rows[1].textContent).toContain('Reminded');
    expect(rows[1].textContent).toContain('rate limited');
    expect(rows[2].textContent).toContain('Dave');
    expect(rows[2].textContent).toContain('Already requested');
    expect(rows[2].textContent).toContain('24-hour cooldown');
    expect(rows[3].textContent).toContain('Eve');
    expect(rows[3].textContent).toContain('Refused');
    expect(rows[3].textContent).toContain('reviewer_cannot_review');

    expect(events).toHaveLength(1);
    expect(events[0].requests).toHaveLength(4);
  });

  it('shows Reminder sent for reminded+notified, Gave Can review for raised access, and friendly unavailable text', async () => {
    const responseBody: CreateReviewRequestsResponse = {
      requests: [
        {
          reviewer: 'user:u-bob',
          outcome: 'created',
          access: 'raised',
          notified: true,
        },
        {
          reviewer: 'user:u-carol',
          outcome: 'reminded',
          access: 'existing',
          notified: true,
        },
        {
          reviewer: 'user:u-dave',
          outcome: 'refused',
          code: 'unavailable',
          notified: false,
        },
      ],
    };
    fetchMock.mockImplementation((input: string | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/users?search=')) {
        return Promise.resolve(
          jsonResponse({
            users: [
              { id: 'u-bob', email: 'bob@example.com', displayName: 'Bob' },
              { id: 'u-carol', email: 'carol@example.com', displayName: 'Carol' },
              { id: 'u-dave', email: 'dave@example.com', displayName: 'Dave' },
            ],
          })
        );
      }
      if (url === `/api/v1/artifacts/${ARTIFACT.id}/review-requests`) {
        return Promise.resolve(jsonResponse(responseBody));
      }
      return Promise.resolve(jsonResponse({ error: 'unexpected' }, 500));
    });

    const el = await mount();
    const searchInput = el.shadowRoot!.querySelector('.reviewer-search') as HTMLElement & {
      value: string;
    };
    for (let i = 0; i < 3; i++) {
      searchInput.value = 'example';
      searchInput.dispatchEvent(new Event('sl-input', { bubbles: true }));
      await new Promise((r) => setTimeout(r, 300));
      await settle(el);
      (el.shadowRoot!.querySelector('.suggestions button') as HTMLButtonElement).click();
      await settle(el);
    }

    (el.shadowRoot!.querySelector('.submit-request-btn') as HTMLButtonElement).click();
    await settle(el);

    const rows = [...el.shadowRoot!.querySelectorAll('.result-row')];
    expect(rows).toHaveLength(3);
    expect(rows[0].textContent).toContain('Requested');
    expect(rows[0].textContent).toContain('Notified');
    expect(rows[0].textContent).toContain('Gave Can review');
    expect(rows[1].textContent).toContain('Reminded');
    expect(rows[1].textContent).toContain('Reminder sent');
    expect(rows[2].textContent).toContain('Refused');
    expect(rows[2].textContent).toContain('Temporarily unavailable — try again');
    expect(rows[2].textContent).toContain('unavailable');
  });

  it('ignores bubbled sl-after-hide from nested elements', async () => {
    const el = await mount();
    const closed = vi.fn();
    el.addEventListener('artifact-request-review-closed', closed);

    const inner = el.shadowRoot!.querySelector('.reviewer-search')!;
    inner.dispatchEvent(new CustomEvent('sl-after-hide', { bubbles: true, composed: true }));
    await settle(el);
    expect(closed).not.toHaveBeenCalled();

    const dialog = el.shadowRoot!.querySelector('sl-dialog')!;
    dialog.dispatchEvent(new CustomEvent('sl-after-hide', { bubbles: true, composed: true }));
    await settle(el);
    expect(closed).toHaveBeenCalledTimes(1);
  });
});
