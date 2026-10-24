// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Selectors this journey adds to the access journey's
 * (../artifacts-access/selectors.ts, used unchanged). The artifact pages
 * have no data-testids: role, text and structure selectors, each with its
 * source. The agent page parts use the async create journey's selectors.
 */

import type { Locator, Page } from '@playwright/test';

import { SELECTORS as AA, escapeRegExp } from '../artifacts-access/selectors.js';

function exact(label: string): RegExp {
  return new RegExp(`^\\s*${escapeRegExp(label)}\\s*$`);
}

/**
 * Every header control this journey grades (artifact-detail.ts renderHeader):
 * the access journey's five plus Retention…, Move… and Delete….
 */
export const AO_HEADER_CONTROLS = [
  'Edit',
  'Review',
  'Share',
  'Grant review access',
  'Retention…',
  'Move…',
  'Delete…',
  'Download',
] as const;
export type AoHeaderControl = (typeof AO_HEADER_CONTROLS)[number];

export const AO_SELECTORS = {
  /** Owner deleted badge beside the title (shared/artifact-owner-deleted.ts). */
  ownerDeletedBadge: 'scion-page-artifact-detail scion-detail-header sl-badge.owner-deleted',
  /** The struck owner name in the meta (artifact-detail.ts goneOwner). */
  ownerGone: `${AA.headerMeta} .owner-gone`,
  /** Review pending badge in the header (artifact-detail.ts renderHeader). */
  reviewPending: 'scion-page-artifact-detail scion-detail-header sl-badge[variant="warning"]',
  /** The Grant review access dialog: the share dialog in mode "review" (artifact-share-dialog.ts renderReview). */
  reviewSection: 'scion-artifact-share-dialog section[aria-labelledby="review-heading"]',
  reviewUserInput:
    'scion-artifact-share-dialog section[aria-labelledby="review-heading"] sl-input[aria-label="User"] input',
  reviewAddButton:
    'scion-artifact-share-dialog section[aria-labelledby="review-heading"] .row sl-button',
  reviewGranted: 'scion-artifact-share-dialog sl-alert.granted',
  dialogError: 'scion-artifact-share-dialog sl-alert[variant="danger"]',
  /** Review editor (artifact-detail.ts renderReview). */
  reviewEditor: 'scion-page-artifact-detail scion-code-editor.review-editor .cm-content',
  reviewCounts: 'scion-page-artifact-detail .review-toolbar .counts',
  reviewFooterButton: 'scion-page-artifact-detail .edit-footer sl-button',
  /** Delete confirmation (shared/artifact-delete-dialog.ts). */
  deleteDialog: 'scion-artifact-delete-dialog sl-dialog[open]',
  deleteDialogButton: 'scion-artifact-delete-dialog sl-dialog[open] sl-button',
  /** Agent page: the header Delete (an icon-only danger button, agent-detail.ts renderHeader). */
  agentPage: 'scion-page-agent-detail',
  agentHeaderDelete:
    'scion-page-agent-detail scion-detail-header sl-button[variant="danger"]:has(sl-icon[name="trash"])',
  /** Project page and its agent list, rendered: an agent card or table row, or the empty state (project-detail.ts). */
  projectPage: 'scion-page-project-detail',
  projectAgentListRendered:
    'scion-page-project-detail .agent-card, scion-page-project-detail .agent-table-container tbody tr, scion-page-project-detail .empty-state',
  /** Signed-out pages (components/pages/login.ts scion-login-page, unauthorized.ts). */
  loginPage: 'scion-login-page',
  unauthorizedPage: 'scion-page-unauthorized',
  /** A success toast (utils/toast.ts showToast: an sl-alert moved into the toast stack). */
  successToast: 'sl-alert[variant="success"]',
  /** The test-hub banner (shared test-hub banner, see e2e/test-hub-banner). */
  testHubBanner: 'sl-alert.test-hub-banner',
} as const;

/**
 * The share dialog element whose dialog is open. The artifact page can hold
 * more than one share dialog element at once (a steward also gets the closed
 * Retention dialog, whose body renders while closed), so section checks are
 * scoped to the open one.
 */
export function openShareDialog(page: Page): Locator {
  return page
    .locator('scion-artifact-share-dialog')
    .filter({ has: page.locator('sl-dialog[open]') });
}

/** A section of the open share dialog, by its heading id. */
export function openDialogSection(
  page: Page,
  heading: 'links-heading' | 'people-heading' | 'retention-heading' | 'review-heading'
): Locator {
  return openShareDialog(page).locator(`section[aria-labelledby="${heading}"]`);
}

/** A header action button by its exact label. */
export function headerControl(page: Page, label: AoHeaderControl): Locator {
  return page.locator(`${AA.headerActions} > sl-button`).filter({ hasText: exact(label) });
}

/** The Owner deleted badge of an /artifacts row's Status cell (artifacts.ts). */
export function rowOwnerDeletedBadge(row: Locator): Locator {
  return row.locator('sl-badge.owner-deleted');
}

/** A suggestion of the review dialog's people search. */
export function reviewSuggestion(page: Page, text: string): Locator {
  return page
    .locator(`${AO_SELECTORS.reviewSection} ul.suggestions [role="option"] button`)
    .filter({
      hasText: text,
    });
}

/** The review dialog's Add button. */
export function reviewAdd(page: Page): Locator {
  return page.locator(AO_SELECTORS.reviewAddButton).filter({ hasText: exact('Add') });
}

/** Save review in the review footer. */
export function saveReviewButton(page: Page): Locator {
  return page.locator(AO_SELECTORS.reviewFooterButton).filter({ hasText: exact('Save review') });
}

/** The delete dialog's confirming button (Delete). */
export function deleteConfirmButton(page: Page): Locator {
  return page.locator(AO_SELECTORS.deleteDialogButton).filter({ hasText: /^\s*Delete\b/ });
}
