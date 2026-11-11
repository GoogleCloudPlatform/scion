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
 * Pending secret rows: an agent saved a user secret that waits for the
 * user's approval. The row links to Profile > Secrets.
 */

import { describe, it, expect, vi } from 'vitest';
import { render } from 'lit';

import type { Notification } from '../../shared/types.js';
import {
  SECRET_PENDING_KIND,
  SECRET_PENDING_STATUS,
  pendingSecretLink,
} from './notification-tray.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('[]', { status: 200 }))),
}));

const AGENT_ID = '6a2b3c4d-0000-4000-8000-000000000001';
const PROJECT_ID = '6a2b3c4d-0000-4000-8000-0000000000aa';

function row(overrides: Partial<Notification> = {}): Notification {
  return {
    id: 'notif-secret',
    subscriptionId: '',
    agentId: AGENT_ID,
    projectId: PROJECT_ID,
    subscriberType: 'user',
    subscriberId: 'me',
    status: SECRET_PENDING_STATUS,
    message:
      'Agent helper in project alpha saved secret TOKEN (environment) for your account at ' +
      '2026-11-10T07:00:00Z. Approve or reject in Profile > Secrets.',
    dispatched: false,
    acknowledged: false,
    createdAt: new Date().toISOString(),
    kind: SECRET_PENDING_KIND,
    resourceType: 'pending_secret',
    resourceId: 'p-1',
    ...overrides,
  };
}

function renderRow(n: Notification): HTMLElement {
  const host = document.createElement('div');
  const tray: any = document.createElement('scion-notification-tray');
  render(tray.renderItem(n), host);
  return host;
}

describe('notification tray: pending secret', () => {
  it('matches the hub kind and status', () => {
    expect(SECRET_PENDING_KIND).toBe('secret_pending');
    expect(SECRET_PENDING_STATUS).toBe('SECRET_PENDING');
  });

  it('links pending secret rows to Profile > Secrets and no other row', () => {
    expect(pendingSecretLink(row())).toBe('/profile/secrets');
    expect(pendingSecretLink(row({ kind: undefined }))).toBe('/profile/secrets');
    expect(pendingSecretLink(row({ kind: 'artifact_review_summary', status: 'COMPLETED' }))).toBe(
      null
    );
  });

  it('renders the message, a Review secrets link and the agent link', () => {
    const host = renderRow(row());
    expect(host.querySelector('.notif-message')?.textContent).toContain('saved secret TOKEN');
    expect(host.querySelector('.notif-icon sl-icon')?.getAttribute('name')).toBe('shield-lock');
    const links = [...host.querySelectorAll('a')].map((a) => [
      a.getAttribute('href'),
      a.textContent?.trim(),
    ]);
    expect(links).toEqual([
      ['/profile/secrets', 'Review secrets'],
      [`/agents/${AGENT_ID}`, 'View agent'],
    ]);
  });
});
