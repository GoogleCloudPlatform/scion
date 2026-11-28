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
 *   (PUT /review-draft), verifies "Draft saved <date+time>", and closes the context.
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
  collectPageErrors,
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
const OTHER_REVIEWER_NAME = 'Sam Second';

const DOC = [
  '# Draft Review Journey',
  '',
  'The first paragraph has words to revise.',
  '',
  'The second paragraph stays untouched.',
  '',
].join('\n');

const DOC_V2 = [
  '# Draft Review Journey (v2)',
  '',
  'The first paragraph was rewritten in version two.',
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
let otherReviewerState = '';
let otherReviewerId = '';
const errors: string[] = [];

test.describe.configure({ mode: 'serial' });

test.afterEach(() => {
  const found = errors.splice(0);
  expect(found, 'no page errors or console errors').toEqual([]);
});

test.beforeAll(async () => {
  await setExperiments(true);
  await setReviewDrafts(true);
  project = await createProject(`e2e-review-drafts-${Date.now().toString(36)}`);
  const stamp = Date.now().toString(36);
  const email = `review-drafts-${stamp}@e2e.test`;
  const reviewer = await createSession(hub().baseURL, {
    email,
    role: 'member',
    displayName: REVIEWER_NAME,
  });
  reviewerState = reviewer.storageStatePath;
  reviewerId = reviewer.user.id;

  const otherEmail = `review-drafts-other-${stamp}@e2e.test`;
  const other = await createSession(hub().baseURL, {
    email: otherEmail,
    role: 'member',
    displayName: OTHER_REVIEWER_NAME,
  });
  otherReviewerState = other.storageStatePath;
  otherReviewerId = other.user.id;

  const roles = await admin('GET', `/api/v1/projects/${project}/members/assignable-roles`);
  const role = ((roles.body as { items?: Array<{ id: string; name: string }> }).items ?? []).find(
    (r) => r.name === 'project-member'
  );
  expect(role, 'the project-member role').toBeTruthy();
  for (const principalId of [email, otherEmail]) {
    const added = await admin('POST', `/api/v1/projects/${project}/members`, {
      roleDefinitionId: role!.id,
      principalType: 'user',
      principalId,
    });
    expect([200, 201]).toContain(added.status);
  }
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
    collectPageErrors(pageA, errors);
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
    await expect(saveDraftBtn.locator('button')).toBeEnabled({ timeout: 15_000 });
    const putDone = pageA.waitForResponse(
      (r) => r.request().method() === 'PUT' && r.url().includes('/review-draft'),
      { timeout: 30_000 }
    );
    await saveDraftBtn.click();
    expect((await putDone).status()).toBe(200);
    await expect(statusA).toContainText(/Draft saved [A-Z][a-z]{2} \d{1,2}, \d{2}:\d{2}/, {
      timeout: 15_000,
    });

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

  // Context B: fresh context (another device) opens the artifact page, sees
  // the Resume draft button and Draft badge without auto-entering Review mode,
  // enters Review mode, loads the draft byte-exact, and submits the review.
  const ctxB = await browser.newContext({ baseURL: hub().baseURL, storageState: reviewerState });
  try {
    const pageB = await ctxB.newPage();
    collectPageErrors(pageB, errors);
    await pageB.goto(artifactPage(project, a.id), { waitUntil: 'domcontentloaded' });

    const resumeBtn = pageB.locator('scion-page-artifact-detail .header-actions sl-button', {
      hasText: 'Resume draft',
    });
    await expect(resumeBtn).toBeVisible({ timeout: 30_000 });
    await expect(
      pageB.locator('scion-page-artifact-detail .header-actions .review-draft-badge')
    ).toHaveText('Draft');
    // O1: loading the page does NOT auto-enter Review mode.
    await expect(pageB.locator('scion-review-editor')).toHaveCount(0);

    const getDraft = pageB.waitForResponse(
      (r) => r.request().method() === 'GET' && r.url().includes('/review-draft'),
      { timeout: 30_000 }
    );
    await resumeBtn.click();
    expect((await getDraft).status()).toBe(200);
    await expect(pageB.locator('scion-review-editor .ProseMirror')).toBeVisible({
      timeout: 30_000,
    });

    // Loaded body is byte-exact with what context A saved.
    expect(await editorText(pageB)).toBe(savedDraftBody);
    await expect(
      pageB.locator('scion-page-artifact-detail .g-toolbar .draft-status')
    ).toContainText(/Draft saved [A-Z][a-z]{2} \d{1,2}, \d{2}:\d{2}/);

    // Submit Review publishes v2 and conditionally deletes the draft.
    const submitBtn = pageB.locator('scion-page-artifact-detail .edit-footer sl-button', {
      hasText: 'Submit Review',
    });
    await expect(submitBtn.locator('button')).toBeEnabled({ timeout: 15_000 });
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

test('two-context: Draft badge is private to the author and Discard from History chip deletes it', async ({
  browser,
}) => {
  const a = await publish({
    project,
    title: 'Private draft badge',
    entry: 'plan.md',
    files: { 'plan.md': Buffer.from(DOC) },
  });
  for (const uid of [reviewerId, otherReviewerId]) {
    const grant = await admin('POST', `/api/v1/artifacts/${a.id}/grants`, {
      subjectKind: 'principal',
      subjectRef: `user:${uid}`,
      permission: 'review',
    });
    expect([200, 201]).toContain(grant.status);
  }

  // Author saves a draft on v1 in context A and closes.
  const ctxAuthorSave = await browser.newContext({
    baseURL: hub().baseURL,
    storageState: reviewerState,
  });
  try {
    const page = await ctxAuthorSave.newPage();
    collectPageErrors(page, errors);
    await page.goto(artifactPage(project, a.id), { waitUntil: 'domcontentloaded' });
    await startGraphicalReview(page);

    const editor = page.locator('scion-review-editor .ProseMirror');
    await editor.locator('p', { hasText: 'first paragraph' }).first().click();
    await page.keyboard.press('End');
    await page.keyboard.type(' Private note.');

    const saveDraftBtn = page.locator('scion-page-artifact-detail .edit-footer sl-button', {
      hasText: 'Save Draft',
    });
    await expect(saveDraftBtn.locator('button')).toBeEnabled({ timeout: 15_000 });
    const putDone = page.waitForResponse(
      (r) => r.request().method() === 'PUT' && r.url().includes('/review-draft'),
      { timeout: 30_000 }
    );
    await saveDraftBtn.click();
    expect((await putDone).status()).toBe(200);
  } finally {
    await ctxAuthorSave.close();
  }

  // Context 1 (other reviewer): sees NO Draft badge anywhere and button reads "Review".
  const ctxOther = await browser.newContext({
    baseURL: hub().baseURL,
    storageState: otherReviewerState,
  });
  try {
    const otherPage = await ctxOther.newPage();
    collectPageErrors(otherPage, errors);
    const otherDraftGet = otherPage.waitForResponse(
      (r) => r.request().method() === 'GET' && r.url().includes('/review-draft'),
      { timeout: 30_000 }
    );
    await otherPage.goto(artifactPage(project, a.id), { waitUntil: 'domcontentloaded' });
    expect((await otherDraftGet).status()).toBe(200);

    await expect(
      otherPage.locator('scion-page-artifact-detail .header-actions sl-button', {
        hasText: 'Review',
      })
    ).toBeVisible({ timeout: 30_000 });
    await expect(
      otherPage.locator('scion-page-artifact-detail .header-actions sl-button', {
        hasText: 'Resume draft',
      })
    ).toHaveCount(0);
    await expect(otherPage.locator('scion-page-artifact-detail .review-draft-badge')).toHaveCount(
      0
    );

    await otherPage.locator('scion-page-artifact-detail sl-tab[panel="history"]').click();
    await expect(
      otherPage.locator('scion-page-artifact-detail sl-tab-panel[name="history"] tbody tr')
    ).toHaveCount(1, { timeout: 15_000 });
    await expect(otherPage.locator('scion-page-artifact-detail .draft-chip-badge')).toHaveCount(0);
  } finally {
    await ctxOther.close();
  }

  // Context 2 (author): sees Draft badge next to Resume draft and on v1 in History,
  // then discards the draft from the History chip with confirmation.
  const ctxAuthor = await browser.newContext({
    baseURL: hub().baseURL,
    storageState: reviewerState,
  });
  try {
    const page = await ctxAuthor.newPage();
    collectPageErrors(page, errors);
    await page.goto(artifactPage(project, a.id), { waitUntil: 'domcontentloaded' });

    await expect(
      page.locator('scion-page-artifact-detail .header-actions sl-button', {
        hasText: 'Resume draft',
      })
    ).toBeVisible({ timeout: 30_000 });
    await expect(
      page.locator('scion-page-artifact-detail .header-actions .review-draft-badge')
    ).toHaveText('Draft');

    await page.locator('scion-page-artifact-detail sl-tab[panel="history"]').click();
    const chipTrigger = page.locator(
      'scion-page-artifact-detail sl-tab-panel[name="history"] .draft-chip-trigger'
    );
    await expect(chipTrigger).toBeVisible({ timeout: 15_000 });
    await expect(chipTrigger.locator('.draft-chip-badge')).toHaveText('Draft');

    // Open chip menu -> Discard draft -> confirm in dialog.
    await chipTrigger.click();
    const discardItem = page.locator('scion-page-artifact-detail .draft-chip-discard');
    await expect(discardItem).toBeVisible({ timeout: 10_000 });
    await discardItem.click();

    const confirmBtn = page.locator(
      'scion-page-artifact-detail sl-dialog.draft-discard-dialog sl-button.draft-discard-confirm'
    );
    await expect(confirmBtn).toBeVisible({ timeout: 10_000 });
    const delDone = page.waitForResponse(
      (r) => r.request().method() === 'DELETE' && r.url().includes('/review-draft?rev=1'),
      { timeout: 30_000 }
    );
    await confirmBtn.click();
    expect((await delDone).status()).toBe(204);

    // Badge clears and header button returns to "Review".
    await expect(page.locator('scion-page-artifact-detail .draft-chip-badge')).toHaveCount(0);
    await expect(page.locator('scion-page-artifact-detail .review-draft-badge')).toHaveCount(0);
    await expect(
      page.locator('scion-page-artifact-detail .header-actions sl-button', { hasText: 'Review' })
    ).toBeVisible({ timeout: 15_000 });
  } finally {
    await ctxAuthor.close();
  }
});

test('stale-version move flow: newer publish triggers move with read-only carried text across resume', async ({
  browser,
}) => {
  const a = await publish({
    project,
    title: 'Stale draft move',
    entry: 'plan.md',
    files: { 'plan.md': Buffer.from(DOC) },
  });
  const grant = await admin('POST', `/api/v1/artifacts/${a.id}/grants`, {
    subjectKind: 'principal',
    subjectRef: `user:${reviewerId}`,
    permission: 'review',
  });
  expect([200, 201]).toContain(grant.status);

  let v1DraftBody: string;

  // Context A: reviewer saves a draft on v1 and closes.
  const ctxA = await browser.newContext({ baseURL: hub().baseURL, storageState: reviewerState });
  try {
    const pageA = await ctxA.newPage();
    collectPageErrors(pageA, errors);
    await pageA.goto(artifactPage(project, a.id), { waitUntil: 'domcontentloaded' });
    await startGraphicalReview(pageA);

    const editorA = pageA.locator('scion-review-editor .ProseMirror');
    await editorA.locator('p', { hasText: 'first paragraph' }).first().click();
    await pageA.keyboard.press('End');
    await pageA.keyboard.type(' Keep this for v2.');

    const saveDraftBtn = pageA.locator('scion-page-artifact-detail .edit-footer sl-button', {
      hasText: 'Save Draft',
    });
    await expect(saveDraftBtn.locator('button')).toBeEnabled({ timeout: 15_000 });
    const putDone = pageA.waitForResponse(
      (r) => r.request().method() === 'PUT' && r.url().includes('/review-draft'),
      { timeout: 30_000 }
    );
    await saveDraftBtn.click();
    expect((await putDone).status()).toBe(200);
    v1DraftBody = await editorText(pageA);
    expect(v1DraftBody).toContain('Keep this for v2.');
  } finally {
    await ctxA.close();
  }

  // Publisher publishes v2 while the reviewer is away.
  const v2 = await publish({
    artifactId: a.id,
    entry: 'plan.md',
    files: { 'plan.md': Buffer.from(DOC_V2) },
  });
  expect(v2.seq).toBe(2);

  let v2DraftBody: string;

  // Context B: reviewer opens the artifact, sees the stale-base dialog, moves
  // to v2 with v1 kept read-only, edits v2, and saves the moved draft.
  const ctxB = await browser.newContext({ baseURL: hub().baseURL, storageState: reviewerState });
  try {
    const pageB = await ctxB.newPage();
    collectPageErrors(pageB, errors);
    await pageB.goto(artifactPage(project, a.id), { waitUntil: 'domcontentloaded' });

    const resumeBtn = pageB.locator('scion-page-artifact-detail .header-actions sl-button', {
      hasText: 'Resume draft',
    });
    await expect(resumeBtn).toBeVisible({ timeout: 30_000 });
    await resumeBtn.click();

    const staleDialog = pageB.locator('scion-page-artifact-detail sl-dialog.stale-draft-dialog');
    await expect(staleDialog).toBeVisible({ timeout: 15_000 });
    await expect(staleDialog).toContainText('version 1');
    await expect(staleDialog).toContainText('version 2');

    await staleDialog.locator('sl-button.stale-draft-move').click();
    await expect(pageB.locator('scion-review-editor .ProseMirror')).toBeVisible({
      timeout: 30_000,
    });

    // Carried earlier v1 review is visible read-only.
    const carried = pageB.locator('scion-page-artifact-detail sl-details.carried-review');
    await expect(carried).toBeVisible({ timeout: 15_000 });
    await expect(carried.locator('pre')).toHaveText(v1DraftBody);

    // Edit v2 and save draft -> rewrites draft to base 2 with carried v1 text.
    const editorB = pageB.locator('scion-review-editor .ProseMirror');
    await editorB.locator('p', { hasText: 'version two' }).first().click();
    await pageB.keyboard.press('End');
    await pageB.keyboard.type(' Updated for v2.');

    const saveDraftBtn = pageB.locator('scion-page-artifact-detail .edit-footer sl-button', {
      hasText: 'Save Draft',
    });
    await expect(saveDraftBtn.locator('button')).toBeEnabled({ timeout: 15_000 });
    const putMoved = pageB.waitForResponse(
      (r) => r.request().method() === 'PUT' && r.url().includes('/review-draft'),
      { timeout: 30_000 }
    );
    await saveDraftBtn.click();
    expect((await putMoved).status()).toBe(200);
    v2DraftBody = await editorText(pageB);
    expect(v2DraftBody).toContain('Updated for v2.');
  } finally {
    await ctxB.close();
  }

  // Context C: fresh context resumes the moved draft; carried text persists
  // read-only until dismissed, then Submit Review publishes v3 and deletes draft.
  const ctxC = await browser.newContext({ baseURL: hub().baseURL, storageState: reviewerState });
  try {
    const pageC = await ctxC.newPage();
    collectPageErrors(pageC, errors);
    await pageC.goto(artifactPage(project, a.id), { waitUntil: 'domcontentloaded' });

    const resumeBtn = pageC.locator('scion-page-artifact-detail .header-actions sl-button', {
      hasText: 'Resume draft',
    });
    await expect(resumeBtn).toBeVisible({ timeout: 30_000 });
    await resumeBtn.click();
    await expect(pageC.locator('scion-review-editor .ProseMirror')).toBeVisible({
      timeout: 30_000,
    });

    expect(await editorText(pageC)).toBe(v2DraftBody);
    const carriedC = pageC.locator('scion-page-artifact-detail sl-details.carried-review');
    await expect(carriedC).toBeVisible({ timeout: 15_000 });
    await expect(carriedC.locator('pre')).toHaveText(v1DraftBody);

    // Dismiss carried text, save, and submit review.
    await carriedC.locator('sl-button.carried-dismiss-btn').click();
    await expect(carriedC).toHaveCount(0);

    const submitBtn = pageC.locator('scion-page-artifact-detail .edit-footer sl-button', {
      hasText: 'Submit Review',
    });
    await expect(submitBtn.locator('button')).toBeEnabled({ timeout: 15_000 });
    const delDone = pageC.waitForResponse(
      (r) => r.request().method() === 'DELETE' && r.url().includes('/review-draft'),
      { timeout: 30_000 }
    );
    await submitBtn.click();
    expect((await delDone).status()).toBe(204);

    await expect.poll(async () => (await currentVersion(a.id)).seq, { timeout: 30_000 }).toBe(3);
    expect(await currentText(a.id, 'plan.md')).toBe(v2DraftBody);
  } finally {
    await ctxC.close();
  }
});

test('abandon review from sticky overflow menu deletes draft with ?rev= and exits Review mode', async ({
  browser,
}) => {
  const a = await publish({
    project,
    title: 'Abandon from overflow menu',
    entry: 'plan.md',
    files: { 'plan.md': Buffer.from(DOC) },
  });
  const grant = await admin('POST', `/api/v1/artifacts/${a.id}/grants`, {
    subjectKind: 'principal',
    subjectRef: `user:${reviewerId}`,
    permission: 'review',
  });
  expect([200, 201]).toContain(grant.status);

  const ctx = await browser.newContext({ baseURL: hub().baseURL, storageState: reviewerState });
  try {
    const page = await ctx.newPage();
    collectPageErrors(page, errors);
    await page.goto(artifactPage(project, a.id), { waitUntil: 'domcontentloaded' });
    await startGraphicalReview(page);

    const editor = page.locator('scion-review-editor .ProseMirror');
    await editor.locator('p', { hasText: 'first paragraph' }).first().click();
    await page.keyboard.press('End');
    await page.keyboard.type(' Will abandon.');

    const saveDraftBtn = page.locator('scion-page-artifact-detail .edit-footer sl-button', {
      hasText: 'Save Draft',
    });
    await expect(saveDraftBtn.locator('button')).toBeEnabled({ timeout: 15_000 });
    const putDone = page.waitForResponse(
      (r) => r.request().method() === 'PUT' && r.url().includes('/review-draft'),
      { timeout: 30_000 }
    );
    await saveDraftBtn.click();
    expect((await putDone).status()).toBe(200);

    // Open real Shoelace overflow menu in sticky header and choose Abandon review.
    const overflowBtn = page.locator(
      'scion-page-artifact-detail .g-sticky .g-review-actions .review-actions-overflow-btn'
    );
    await expect(overflowBtn).toBeVisible({ timeout: 15_000 });
    await overflowBtn.click();

    const abandonItem = page.locator('scion-page-artifact-detail .abandon-review-menu-item');
    await expect(abandonItem).toBeVisible({ timeout: 10_000 });
    await abandonItem.click();

    const confirmBtn = page.locator(
      'scion-page-artifact-detail sl-dialog.draft-abandon-dialog sl-button.draft-abandon-confirm'
    );
    await expect(confirmBtn).toBeVisible({ timeout: 10_000 });
    const delDone = page.waitForResponse(
      (r) => r.request().method() === 'DELETE' && r.url().includes('/review-draft?rev=1'),
      { timeout: 30_000 }
    );
    await confirmBtn.click();
    expect((await delDone).status()).toBe(204);

    // Exited Review mode, Draft badge cleared, and header returns to "Review".
    await expect(page.locator('scion-review-editor')).toHaveCount(0);
    await expect(page.locator('scion-page-artifact-detail .review-draft-badge')).toHaveCount(0);
    await expect(
      page.locator('scion-page-artifact-detail .header-actions sl-button', { hasText: 'Review' })
    ).toBeVisible({ timeout: 15_000 });
  } finally {
    await ctx.close();
  }
});
