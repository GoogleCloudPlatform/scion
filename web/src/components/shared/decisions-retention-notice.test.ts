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
 * Unit tests for the Decision retention Made notice (ptone/scion#8984, PR-3 #9351):
 * exact mocks copy, gating by hub.decision_retention (plus web.decisions),
 * project active vs held/off, and profile multi-project vs filtered-to-one-project.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  DECISIONS_FLAG,
  DECISION_RETENTION_FLAG,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import { madeRetentionNoticeText } from './decisions-views.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());

vi.mock('../../client/api.js', async () => {
  const actual = await vi.importActual<typeof import('../../client/api.js')>('../../client/api.js');
  return { ...actual, apiFetch };
});

vi.mock('../../client/state.js', () => ({
  stateManager: {
    setScope: vi.fn(),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    getCurrentUserId: vi.fn(() => 'user-admin'),
    getProject: vi.fn(() => undefined),
    getProjects: vi.fn(() => []),
  },
}));

await import('../pages/project-decisions.js');
await import('../pages/profile-decisions.js');

const PROJ_A = '11111111-1111-1111-1111-111111111111';
const PROJ_B = '22222222-2222-2222-2222-222222222222';

function json(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

async function settle(): Promise<void> {
  for (let i = 0; i < 12; i++) await new Promise((resolve) => setTimeout(resolve, 0));
}

describe('madeRetentionNoticeText', () => {
  it('returns null when hub.decision_retention flag is off', () => {
    expect(
      madeRetentionNoticeText({
        scope: 'project',
        flagEnabled: false,
        projectRetention: { active: true, days: 180 },
      })
    ).toBeNull();
    expect(
      madeRetentionNoticeText({
        scope: 'profile',
        flagEnabled: false,
        profileRetention: { [PROJ_A]: { active: true, days: 180 } },
      })
    ).toBeNull();
  });

  it('uses the single-project mock copy on project scope when active', () => {
    expect(
      madeRetentionNoticeText({
        scope: 'project',
        flagEnabled: true,
        projectRetention: { active: true, days: 180 },
      })
    ).toBe('Settled decisions in this project are deleted 180 days after their last activity.');
  });

  it('hides the project notice when retention is absent, inactive, or zero days', () => {
    expect(
      madeRetentionNoticeText({ scope: 'project', flagEnabled: true, projectRetention: null })
    ).toBeNull();
    expect(
      madeRetentionNoticeText({
        scope: 'project',
        flagEnabled: true,
        projectRetention: { active: false, days: 180 },
      })
    ).toBeNull();
    expect(
      madeRetentionNoticeText({
        scope: 'project',
        flagEnabled: true,
        projectRetention: { active: true, days: 0 },
      })
    ).toBeNull();
  });

  it('uses the general mock copy on profile scope when any project in view is active', () => {
    expect(
      madeRetentionNoticeText({
        scope: 'profile',
        flagEnabled: true,
        profileRetention: { [PROJ_A]: { active: true, days: 180 } },
      })
    ).toBe(
      'Some projects delete settled decisions after a retention period, so older decisions may be missing.'
    );
  });

  it('switches to single-project wording on profile scope when filtered to one active project', () => {
    const projects = [
      { id: PROJ_A, name: 'Payments API', slug: 'payments-api' },
      { id: PROJ_B, name: 'Web Console', slug: 'web-console' },
    ];
    const profileRetention = { [PROJ_A]: { active: true, days: 90 } };

    // Filtered by project ID
    expect(
      madeRetentionNoticeText({
        scope: 'profile',
        flagEnabled: true,
        profileRetention,
        filteredProject: PROJ_A,
        projects,
      })
    ).toBe('Settled decisions in this project are deleted 90 days after their last activity.');

    // Filtered by project slug
    expect(
      madeRetentionNoticeText({
        scope: 'profile',
        flagEnabled: true,
        profileRetention,
        filteredProject: 'payments-api',
        projects,
      })
    ).toBe('Settled decisions in this project are deleted 90 days after their last activity.');

    // Filtered to a held project (absent from active retention map) -> hidden
    expect(
      madeRetentionNoticeText({
        scope: 'profile',
        flagEnabled: true,
        profileRetention,
        filteredProject: PROJ_B,
        projects,
      })
    ).toBeNull();
  });
});

describe('Made retention notice on project and profile Decisions pages', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    setFeatureFlag(DECISIONS_FLAG, true);
    setFeatureFlag(DECISION_RETENTION_FLAG, true);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    window.history.replaceState({}, '', '/');
  });

  it('renders the retention notice on Project Made when active and hides it when flag is off', async () => {
    window.history.replaceState({}, '', `/projects/${PROJ_A}/decisions?tab=made`);
    apiFetch.mockImplementation((path) => {
      if (path === `/api/v1/projects/${PROJ_A}`) {
        return Promise.resolve(json({ id: PROJ_A, name: 'payments-api', slug: 'payments-api' }));
      }
      if (path.includes('tab=made')) {
        return Promise.resolve(
          json({
            counts: { waitingOnYou: 0, waitingOnOthers: 0 },
            entries: [],
            agents: [],
            answerers: [],
            conversations: [],
            retention: { active: true, days: 180 },
          })
        );
      }
      return Promise.resolve(
        json({
          counts: { waitingOnYou: 0, waitingOnOthers: 0 },
          prompts: [],
          agents: [],
          answerers: [],
          conversations: [],
          retention: { active: true, days: 180 },
        })
      );
    });

    const el = document.createElement('scion-page-project-decisions');
    el.projectId = PROJ_A;
    document.body.appendChild(el);
    await settle();
    await el.updateComplete;

    const notice = el.shadowRoot?.querySelector('[data-retention-notice]');
    expect(notice?.textContent?.trim()).toBe(
      'Settled decisions in this project are deleted 180 days after their last activity.'
    );

    setFeatureFlag(DECISION_RETENTION_FLAG, false);
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-retention-notice]')).toBeNull();
  });

  it('renders general notice on Profile Made and single-project notice when filtered to one project', async () => {
    window.history.replaceState({}, '', '/profile/decisions?tab=made&sort=recent');
    apiFetch.mockImplementation((path) => {
      if (path.includes('/users/me/decisions')) {
        return Promise.resolve(
          json({
            sort: 'recent',
            counts: { waitingOnYou: 0, waitingOnOthers: 0 },
            entries: [],
            prompts: [],
            agents: [],
            answerers: [],
            conversations: [],
            projects: [
              { id: PROJ_A, name: 'payments-api', slug: 'payments-api' },
              { id: PROJ_B, name: 'web-console', slug: 'web-console' },
            ],
            truncatedProjects: false,
            retention: {
              [PROJ_A]: { active: true, days: 180 },
            },
          })
        );
      }
      return Promise.resolve(json({}));
    });

    const el = document.createElement('scion-page-profile-decisions');
    document.body.appendChild(el);
    await settle();
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-retention-notice]')?.textContent?.trim()).toBe(
      'Some projects delete settled decisions after a retention period, so older decisions may be missing.'
    );
    el.remove();

    // Now filtered to PROJ_A -> single-project wording
    window.history.replaceState(
      {},
      '',
      `/profile/decisions?tab=made&sort=recent&project=${PROJ_A}`
    );
    const el2 = document.createElement('scion-page-profile-decisions');
    document.body.appendChild(el2);
    await settle();
    await el2.updateComplete;

    expect(el2.shadowRoot?.querySelector('[data-retention-notice]')?.textContent?.trim()).toBe(
      'Settled decisions in this project are deleted 180 days after their last activity.'
    );
  });
});
