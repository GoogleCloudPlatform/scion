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
 * Warn publishers when a review is open (ptone/scion#6849), in each
 * engine: in context A a project member with a review grant opens Review
 * mode, and its tab reports the open review to the hub; in context B the
 * admin edits the artifact and publishes. The hub refuses the publish, the
 * dialog names A by display name, Wait publishes nothing and keeps the
 * edit, and Publish anyway publishes.
 */

import { expect, test } from '@playwright/test';

import { createSession } from '../harness/auth.js';
import {
  admin,
  artifactPage,
  createProject,
  currentVersion,
  hub,
  openArtifact,
  publish,
  setExperiments,
} from './fixtures.js';

const OPEN_REVIEW_CHECK = 'hub.artifacts_open_review_check';
const REVIEWER_NAME = 'Ona Reviewer';

/** Switches the open-review check on (true) or resets it (null). */
async function setOpenReviewCheck(value: boolean | null): Promise<void> {
  const read = await admin('GET', '/api/v1/admin/experiments');
  expect(read.status).toBe(200);
  const revision = (read.body as { revision: number }).revision;
  const res = await admin('PUT', '/api/v1/admin/experiments', {
    overrides: { [OPEN_REVIEW_CHECK]: value },
    expected_revision: revision,
  });
  expect(res.status).toBe(200);
}

let project = '';
let reviewerState = '';
let reviewerId = '';

test.describe.configure({ mode: 'serial' });

test.beforeAll(async () => {
  await setExperiments(true);
  await setOpenReviewCheck(true);
  project = await createProject(`e2e-open-review-${Date.now().toString(36)}`);
  const email = `open-review-${Date.now().toString(36)}@e2e.test`;
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
  await setOpenReviewCheck(null);
  await setExperiments(null);
});

test('a publisher is told about an open review and may wait or publish anyway', async ({
  page,
  browser,
}) => {
  const a = await publish({
    project,
    title: 'Open review',
    entry: 'plan.md',
    files: { 'plan.md': Buffer.from('# Plan\n\nWe ship in Q3.\n') },
  });
  const grant = await admin('POST', `/api/v1/artifacts/${a.id}/grants`, {
    subjectKind: 'principal',
    subjectRef: `user:${reviewerId}`,
    permission: 'review',
  });
  expect([200, 201]).toContain(grant.status);

  // Context A: the reviewer opens Review mode; its first beat is answered.
  const ctxA = await browser.newContext({ baseURL: hub().baseURL, storageState: reviewerState });
  try {
    const pageA = await ctxA.newPage();
    await pageA.goto(artifactPage(project, a.id), { waitUntil: 'domcontentloaded' });
    const review = pageA.locator('scion-page-artifact-detail .header-actions sl-button', {
      hasText: 'Review',
    });
    await expect(review).toBeVisible({ timeout: 30_000 });
    const beat = pageA.waitForResponse(
      (r) => r.request().method() === 'PUT' && r.url().includes('/review-sessions/'),
      { timeout: 30_000 }
    );
    await review.click();
    expect((await beat).status()).toBe(200);

    // Context B: the admin edits and publishes.
    await openArtifact(page, project, a.id);
    await page
      .locator('scion-page-artifact-detail .header-actions sl-button', { hasText: 'Edit' })
      .click();
    const codeEditor = page.locator('scion-page-artifact-detail scion-code-editor');
    await expect(codeEditor).toBeVisible({ timeout: 30_000 });
    await codeEditor.evaluate((el) =>
      el.dispatchEvent(
        new CustomEvent('content-changed', { detail: { content: '# Plan\n\nWe ship in Q4.\n' } })
      )
    );
    const publishButton = page.locator('scion-page-artifact-detail .edit-footer sl-button', {
      hasText: 'Publish new version',
    });
    await publishButton.click();

    const dialog = page.locator('scion-page-artifact-detail sl-dialog.review-open-dialog');
    await expect(dialog).toBeVisible({ timeout: 30_000 });
    await expect(dialog).toContainText(`${REVIEWER_NAME} is reviewing this version.`);
    await expect(dialog).not.toContainText(reviewerId);

    // Wait: nothing is published, and the edit stays open.
    await dialog.locator('sl-button.review-open-wait').click();
    await expect(dialog).toBeHidden();
    await expect(codeEditor).toBeVisible();
    expect((await currentVersion(a.id)).seq).toBe(1);

    // Publish anyway: the new version is published.
    await publishButton.click();
    await expect(dialog).toBeVisible({ timeout: 30_000 });
    await dialog.locator('sl-button.review-open-force').click();
    await expect.poll(async () => (await currentVersion(a.id)).seq, { timeout: 30_000 }).toBe(2);
  } finally {
    await ctxA.close();
  }
});
