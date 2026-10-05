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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { apiFetch } from '../../client/api.js';
import {
  MEMBERSHIP_CHANGED_EVENT,
  type MembershipChangedDetail,
} from '../../utils/membership-events.js';

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return { ...actual, apiFetch: vi.fn() };
});

/** The workspace step's state and handlers, reached past their private modifiers. */
interface OnboardingPage extends HTMLElement {
  currentStep: number;
  error: string | null;
  wsProjectName: string;
  wsEmbeddedBrokerID: string;
  wsPathValidation: { resolved: string; exists: boolean; isDir: boolean } | null;
  handleWsHubCreate(): Promise<void>;
  handleWsLinkedCreate(): Promise<void>;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function respond(...responses: Response[]): void {
  const queue = [...responses];
  vi.mocked(apiFetch).mockImplementation(() => Promise.resolve(queue.shift()!));
}

let heard: MembershipChangedDetail[] = [];
const onMembership = (e: Event): void => {
  heard.push((e as CustomEvent<MembershipChangedDetail>).detail);
};

beforeAll(async () => {
  await import('./onboarding.js');
}, 60_000);

afterEach(() => {
  window.removeEventListener(MEMBERSHIP_CHANGED_EVENT, onMembership);
  vi.mocked(apiFetch).mockReset();
});

function createPage(): OnboardingPage {
  heard = [];
  window.addEventListener(MEMBERSHIP_CHANGED_EVENT, onMembership);
  const el = document.createElement('scion-page-onboarding') as unknown as OnboardingPage;
  el.wsProjectName = 'Notes';
  el.wsEmbeddedBrokerID = 'broker-1';
  el.wsPathValidation = { resolved: '/home/u/notes', exists: true, isDir: true };
  return el;
}

describe('onboarding workspace step: membership-changed after project creation', () => {
  it('creating a hub project announces a membership change for it', async () => {
    const el = createPage();
    respond(jsonResponse({ project: { id: 'p-new' } }, 201));

    await el.handleWsHubCreate();

    expect(heard).toEqual([{ kind: 'project', id: 'p-new' }]);
    expect(el.currentStep).toBe(6);
  });

  it('a hub project create that finds an existing project announces nothing', async () => {
    const el = createPage();
    respond(jsonResponse({ project: { id: 'p-old' } }, 200));

    await el.handleWsHubCreate();

    expect(heard).toEqual([]);
    expect(el.currentStep).toBe(6);
  });

  it('a failed hub project create announces nothing', async () => {
    const el = createPage();
    respond(jsonResponse({ error: { message: 'nope' } }, 500));

    await el.handleWsHubCreate();

    expect(heard).toEqual([]);
    expect(el.currentStep).toBe(0);
  });

  it('creating a linked project announces a membership change, even when linking fails', async () => {
    const el = createPage();
    respond(
      jsonResponse({ project: { id: 'p-linked' } }, 201),
      jsonResponse({ error: { message: 'link failed' } }, 500)
    );

    await el.handleWsLinkedCreate();

    expect(heard).toEqual([{ kind: 'project', id: 'p-linked' }]);
    expect(el.error).toBeTruthy();
  });

  it('a linked project create that finds an existing project announces nothing', async () => {
    const el = createPage();
    respond(
      jsonResponse({ project: { id: 'p-old' } }, 200),
      jsonResponse({ error: { message: 'link failed' } }, 500)
    );

    await el.handleWsLinkedCreate();

    expect(heard).toEqual([]);
  });

  it('a failed linked project create announces nothing', async () => {
    const el = createPage();
    respond(jsonResponse({ error: { message: 'nope' } }, 500));

    await el.handleWsLinkedCreate();

    expect(heard).toEqual([]);
  });
});
