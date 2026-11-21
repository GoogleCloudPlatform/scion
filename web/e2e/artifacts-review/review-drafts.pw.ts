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
 * Durable artifact review drafts journey (ptone/scion#7116, ptone/scion#8471),
 * in each engine:
 * - Context A (reviewer) opens Review mode, types marks, saves the draft
 *   (PUT /review-draft), verifies "Draft saved HH:MM", and closes the context.
 *   No version is published, no owner notification is emitted, and no draft
 *   data is stored in browser storage (C8).
 * - Context B (fresh browser context, simulating another device) enters Review
 *   mode, receives the draft byte-exact, submits the review (Submit Review),
 *   verifies the draft is deleted, the review version is published, and the
 *   artifact owner is notified exactly once.
 */

import { expect, test } from '@playwright/test';

import { createSession } from '../harness/auth.js';
import {
  admin,
  artifactPage,
  createProject,
  currentText,
  currentVersion,
  editorText,
  hub,
  publish,
  setExperiments,
  startGraphicalReview,
} from './fixtures.js';

const REVIEW_DRAFTS_FLAG = 'hub.artifacts_review_drafts';
const REVIEWER_NAME = 'Dana Drafter';

const DOC = [
  '# Draft Review Journey',
  '',
  'The first paragraph has words to revise.',
  '',
  'The second paragraph stays untouched.',
  '',
].join('\n');

/** Switches hub.artifacts_review_drafts on (true) or resets it (null). */
async function setReviewDrafts(value: boolean | null): Promise<void> {
  const read = await admin('GET', '/api/v1/admin/experiments');
  expect(read.status).toBe(200);
  const revision = (read.body as { revision: number }).revision;
  const res = await admin('PUT', '/api/v1/admin/experiments', {
    overrides: { [REVIEW_DRAFTS_FLAG]: value },
    expected_revision: revision,
  });
  expect(res.status).toBe(200);
}

interface NotificationItem {
  id: string;
  kind?: string;
  resourceId?: string;
  message?: string;
}

/** Counts review summary notifications received by the artifact owner (admin) for artifactId. */
async function ownerReviewNotifications(artifactId: string): Promise<number> {
  const res = await admin('GET', '/api/v1/notifications');
  expect(res.status).toBe(200);
  const body = res.body as
    | { notifications?: NotificationItem[]; items?: NotificationItem[] }
    | NotificationItem[];
  const list = Array.isArray(body) ? body : (body.notifications ?? body.items ?? []);
  return list.filter(
    (n) =>
      (n.resourceId === artifactId || (n.message ?? '').includes(REVIEWER_NAME)) &&
      (n.kind === 'artifact_review_summary' || (n.message ?? '').includes('Review'))
  ).length;
}

let project = '';
let reviewerState = '';
let reviewerId = '';

test.describe.configure({ mode: 'serial' });

test.beforeAll(async () => {
  await setExperiments(true);
  await setReviewDrafts(true);
  project = await createProject(`e2e-review-drafts-${Date.now().toString(36)}`);
  const email = `review-drafts-${Date.now().toString(36)}@e2e.test`;
  const reviewer = await createSession(hub().baseURL, {
    email,
    role: 'member',
    displayName: REVIEWER_NAME,
  });
  reviewerState = reviewer.storageStatePath;
  reviewerId = reviewer.user.id;
  const roles = await admin('GET', `/api/v1/projects/${project}/members/assignable-roles`);
  const role = ((roles.body as { items?: Array<{ id: string; name: string }> }).items ?? []).find(
    (r) => r.name === 'project-member'
  );
  expect(role, 'the project-member role').toBeTruthy();
  const added = await admin('POST', `/api/v1/projects/${project}/members`, {
    roleDefinitionId: role!.id,
    principalType: 'user',
    principalId: email,
  });
  expect([200, 201]).toContain(added.status);
});

test.afterAll(async () => {
  await setReviewDrafts(null);
  await setExperiments(null);
});

test('save draft in context A, close, resume byte-exact and submit in context B', async ({
  browser,
}) => {
  const a = await publish({
    project,
    title: 'Draft journey',
    entry: 'plan.md',
    files: { 'plan.md': Buffer.from(DOC) },
  });
  const grant = await admin('POST', `/api/v1/artifacts/${a.id}/grants`, {
    subjectKind: 'principal',
    subjectRef: `user:${reviewerId}`,
    permission: 'review',
  });
  expect([200, 201]).toContain(grant.status);

  const notificationsBefore = await ownerReviewNotifications(a.id);
  let savedDraftBody: string;

  // Context A: reviewer enters Review mode, types marks, saves draft, closes.
  const ctxA = await browser.newContext({ baseURL: hub().baseURL, storageState: reviewerState });
  try {
    const pageA = await ctxA.newPage();
    await pageA.goto(artifactPage(project, a.id), { waitUntil: 'domcontentloaded' });
    await startGraphicalReview(pageA);

    // Status line is empty before any save.
    const statusA = pageA.locator('scion-page-artifact-detail .g-toolbar .draft-status');
    await expect(statusA).toHaveText('');

    // Type a suggestion at the end of the first paragraph.
    const editorA = pageA.locator('scion-review-editor .ProseMirror');
    await editorA.locator('p', { hasText: 'first paragraph' }).first().click();
    await pageA.keyboard.press('End');
    await pageA.keyboard.type(' Carefully.');

    // Save Draft immediately flushes and PUTs /review-draft.
    const saveDraftBtn = pageA.locator('scion-page-artifact-detail .edit-footer sl-button', {
      hasText: 'Save Draft',
    });
    await expect(saveDraftBtn).toBeEnabled({ timeout: 15_000 });
    const putDone = pageA.waitForResponse(
      (r) => r.request().method() === 'PUT' && r.url().includes('/review-draft'),
      { timeout: 30_000 }
    );
    await saveDraftBtn.click();
    expect((await putDone).status()).toBe(200);
    await expect(statusA).toContainText(/Draft saved \d{2}:\d{2}/, { timeout: 15_000 });

    savedDraftBody = await editorText(pageA);
    expect(savedDraftBody).toContain('Carefully.');
    expect(savedDraftBody).not.toBe(DOC);

    // C8: no draft text in localStorage, sessionStorage, IndexedDB, or Cache API.
    const leaked = await pageA.evaluate(async (needle) => {
      for (let i = 0; i < localStorage.length; i++) {
        const k = localStorage.key(i)!;
        if ((localStorage.getItem(k) ?? '').includes(needle)) return `localStorage:${k}`;
      }
      for (let i = 0; i < sessionStorage.length; i++) {
        const k = sessionStorage.key(i)!;
        if ((sessionStorage.getItem(k) ?? '').includes(needle)) return `sessionStorage:${k}`;
      }
      if (typeof indexedDB !== 'undefined' && typeof indexedDB.databases === 'function') {
        const dbs = await indexedDB.databases();
        if (dbs.length > 0) return `indexedDB:${dbs.map((d) => d.name ?? '').join(',')}`;
      }
      if (typeof caches !== 'undefined' && typeof caches.keys === 'function') {
        const keys = await caches.keys();
        if (keys.length > 0) return `caches:${keys.join(',')}`;
      }
      return null;
    }, 'Carefully.');
    expect(leaked).toBeNull();
  } finally {
    await ctxA.close();
  }

  // After closing context A: no version published, owner notified 0 times,
  // and admin cannot see the reviewer's private draft.
  expect((await currentVersion(a.id)).seq).toBe(1);
  expect(await ownerReviewNotifications(a.id)).toBe(notificationsBefore);
  const adminDraft = await admin('GET', `/api/v1/artifacts/${a.id}/review-draft`);
  expect(adminDraft.status).toBe(200);
  expect((adminDraft.body as { draft: unknown }).draft).toBeNull();

  // Context B: fresh context (another device) enters Review mode, loads the
  // draft byte-exact, and submits the review.
  const ctxB = await browser.newContext({ baseURL: hub().baseURL, storageState: reviewerState });
  try {
    const pageB = await ctxB.newPage();
    await pageB.goto(artifactPage(project, a.id), { waitUntil: 'domcontentloaded' });
    const getDraft = pageB.waitForResponse(
      (r) => r.request().method() === 'GET' && r.url().includes('/review-draft'),
      { timeout: 30_000 }
    );
    await startGraphicalReview(pageB);
    expect((await getDraft).status()).toBe(200);

    // Loaded body is byte-exact with what context A saved.
    expect(await editorText(pageB)).toBe(savedDraftBody);
    await expect(
      pageB.locator('scion-page-artifact-detail .g-toolbar .draft-status')
    ).toContainText(/Draft saved \d{2}:\d{2}/);

    // Submit Review publishes v2 and conditionally deletes the draft.
    const submitBtn = pageB.locator('scion-page-artifact-detail .edit-footer sl-button', {
      hasText: 'Submit Review',
    });
    await expect(submitBtn).toBeEnabled({ timeout: 15_000 });
    const delDone = pageB.waitForResponse(
      (r) => r.request().method() === 'DELETE' && r.url().includes('/review-draft'),
      { timeout: 30_000 }
    );
    await submitBtn.click();
    expect((await delDone).status()).toBe(204);

    await expect.poll(async () => (await currentVersion(a.id)).seq, { timeout: 30_000 }).toBe(2);
    expect(await currentText(a.id, 'plan.md')).toBe(savedDraftBody);

    // Draft is gone on the hub for the reviewer too.
    const afterDraft = await pageB.evaluate(async (id) => {
      const r = await fetch(`/api/v1/artifacts/${id}/review-draft`, { credentials: 'include' });
      return r.json() as Promise<{ draft: unknown }>;
    }, a.id);
    expect(afterDraft.draft).toBeNull();
  } finally {
    await ctxB.close();
  }

  // Owner was notified exactly once for the submitted review.
  await expect
    .poll(() => ownerReviewNotifications(a.id), { timeout: 15_000 })
    .toBe(notificationsBefore + 1);
});
