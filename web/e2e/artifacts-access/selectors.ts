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
 * Selectors of the artifact access journey: the only place they live.
 *
 * The artifact pages carry no data-testid today, so these are the
 * role/text/structure selectors of main (file:line in the comments, against
 * the spec's base). When the proposed data-testids land, only this file
 * changes. Playwright's CSS engine pierces open shadow roots.
 */

import type { Locator, Page } from '@playwright/test';

/** Escapes text for use inside a RegExp. */
export function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** An exact, whitespace-tolerant text match for a button label. */
function exact(label: string): RegExp {
  return new RegExp(`^\\s*${escapeRegExp(label)}\\s*$`);
}

export const SELECTORS = {
  /** Sidebar Artifacts item (components/shared/nav.ts:65-68). */
  navArtifacts: 'scion-nav a[href="/artifacts"]',
  /** Sidebar Projects item: a positive anchor that the nav rendered. */
  navProjects: 'scion-nav a[href="/projects"]',
  /** The 404 page (components/pages/not-found.ts:107-117). */
  notFound: 'scion-page-404',
  artifactPage: 'scion-page-artifact-detail',
  /** Header of the artifact page (artifact-detail.ts:1116). */
  header: 'scion-page-artifact-detail scion-detail-header',
  /** Header action buttons (artifact-detail.ts:1143). */
  headerActions: 'scion-page-artifact-detail scion-detail-header .header-actions',
  /** Owner meta (artifact-detail.ts:1127). */
  headerMeta: 'scion-page-artifact-detail scion-detail-header [slot="meta"] span',
  /** Version menu trigger (artifact-detail.ts:1083). */
  versionMenu:
    'scion-page-artifact-detail scion-detail-header .header-actions sl-dropdown sl-button[slot="trigger"]',
  /** Markdown preview, rendered once the entry text loaded (artifact-detail.ts:1262). */
  markdownFrame:
    'scion-page-artifact-detail sl-tab-panel[name="preview"] scion-artifact-markdown-frame',
  historyPanel: 'scion-page-artifact-detail sl-tab-panel[name="history"]',
  /** Edit view (artifact-detail.ts:1819-1866). */
  editorContent: 'scion-page-artifact-detail scion-code-editor .cm-content',
  editNoteInput: 'scion-page-artifact-detail .edit-footer sl-input input',
  editFooterButton: 'scion-page-artifact-detail .edit-footer sl-button',
  /** Project page (project-detail.ts:3084-3151). */
  projectPage: 'scion-page-project-detail',
  filesPlaceholder: 'scion-page-project-detail .files-section-placeholder',
  /**
   * The revealed Files area's tab group (project-detail.ts:3155): rendered
   * when artifacts are off (or another segment is shown) and the project has
   * file tabs, as a hub-managed project does (its workspace tab).
   */
  filesTabGroup: 'scion-page-project-detail .files-tab-group',
  filesSegmentArtifacts:
    'scion-page-project-detail sl-radio-group.files-segments sl-radio-button[value="artifacts"]',
  artifactList: 'scion-page-project-detail scion-artifact-list',
  /** New artifact (components/shared/artifact-list.ts:305). */
  newArtifactButton: 'scion-page-project-detail scion-artifact-list sl-button',
  /** Publish dialog (artifact-publish-dialog.ts:302-410). */
  publishDialog: 'scion-artifact-publish-dialog sl-dialog[open]',
  publishFileInput: 'scion-artifact-publish-dialog #file-input',
  publishTitleInput: 'scion-artifact-publish-dialog sl-input[label="Title"] input',
  publishFooterButton: 'scion-artifact-publish-dialog sl-button[slot="footer"]',
  publishError: 'scion-artifact-publish-dialog sl-alert[variant="danger"]',
  /** Share dialog (artifact-share-dialog.ts:1002). */
  shareDialog: 'scion-artifact-share-dialog sl-dialog[open]',
  sharePeopleSection: 'scion-artifact-share-dialog section[aria-labelledby="people-heading"]',
  /** People input of the Share mode (artifact-share-dialog.ts:762-768). */
  sharePeopleInput:
    'scion-artifact-share-dialog section[aria-labelledby="people-heading"] sl-input input',
  shareSuggestion: 'scion-artifact-share-dialog ul.suggestions [role="option"] button',
  /** The add row's Permission select (artifact-share-dialog.ts:787, 865). */
  shareAddPermission:
    'scion-artifact-share-dialog section[aria-labelledby="people-heading"] > .row sl-select[aria-label="Permission"]',
  shareAddRowButton:
    'scion-artifact-share-dialog section[aria-labelledby="people-heading"] > .row sl-button',
  /** Grant rows (artifact-share-dialog.ts:889-915). */
  shareGrantRow: 'scion-artifact-share-dialog section[aria-labelledby="people-heading"] .grant',
  /** Footer buttons: a div[slot="footer"] holds them (artifact-share-dialog.ts:1010-1036). */
  shareFooterButton: 'scion-artifact-share-dialog sl-dialog[open] [slot="footer"] sl-button',
  /** Artifacts list page (components/pages/artifacts.ts:490). */
  artifactsPage: 'scion-page-artifacts',
  artifactsRow: 'scion-page-artifacts tbody tr.clickable',
  artifactsEmpty: 'scion-page-artifacts .empty-state',
} as const;

/** Header actions graded per role (spec section 2). */
export const HEADER_CONTROLS = [
  'Edit',
  'Review',
  'Share',
  'Grant review access',
  'Download',
] as const;
export type HeaderControl = (typeof HEADER_CONTROLS)[number];

/** A header action button by its exact label (artifact-detail.ts:1146-1187). */
export function headerButton(page: Page, label: HeaderControl): Locator {
  return page.locator(`${SELECTORS.headerActions} > sl-button`).filter({ hasText: exact(label) });
}

/** The artifact title in the header (scion-detail-header's h1). */
export function headerTitle(page: Page): Locator {
  return page.locator(`${SELECTORS.header} h1`);
}

/** The "Owner: …" meta span. */
export function ownerMeta(page: Page): Locator {
  return page.locator(SELECTORS.headerMeta).filter({ hasText: /^\s*Owner:/ });
}

/** The 404 page's heading. */
export function notFoundHeading(page: Page): Locator {
  return page.locator(SELECTORS.notFound).getByRole('heading', { name: 'Page Not Found' });
}

/** An artifact page tab (artifact-detail.ts:1202-1204). */
export function tab(page: Page, panel: 'preview' | 'files' | 'history'): Locator {
  return page.locator(`${SELECTORS.artifactPage} sl-tab[panel="${panel}"]`);
}

/** Upload new version on the History tab (artifact-detail.ts:1586, 1607). */
export function uploadVersionButton(page: Page): Locator {
  return page
    .locator(`${SELECTORS.historyPanel} sl-button`)
    .filter({ hasText: exact('Upload new version') });
}

/** The History row of version seq (artifact-detail.ts:1504). */
export function historyRow(page: Page, seq: number): Locator {
  return page
    .locator(`${SELECTORS.historyPanel} tbody tr`)
    .filter({ has: page.locator('strong', { hasText: exact(`v${seq}`) }) });
}

/** The *Published by* cell of a History row (artifact-detail.ts:1514). */
export function historyPublishedBy(row: Locator): Locator {
  return row.locator('td').nth(2);
}

/** The *current* badge of a History row (artifact-detail.ts:1508). */
export function historyCurrentBadge(row: Locator): Locator {
  return row.locator('sl-badge', { hasText: exact('current') });
}

/** New artifact on the project page's Artifacts view. */
export function newArtifactButton(page: Page): Locator {
  return page
    .locator(SELECTORS.newArtifactButton)
    .filter({ hasText: exact('New artifact') })
    .first();
}

/** The publish dialog's Publish button. */
export function publishSubmit(page: Page): Locator {
  return page.locator(SELECTORS.publishFooterButton).filter({ hasText: exact('Publish') });
}

/** Publish new version in the edit view (artifact-detail.ts:1856). */
export function editPublishButton(page: Page): Locator {
  return page.locator(SELECTORS.editFooterButton).filter({ hasText: exact('Publish new version') });
}

/** Add in the Share dialog's people row (artifact-share-dialog.ts:790). */
export function shareAddButton(page: Page): Locator {
  return page.locator(SELECTORS.shareAddRowButton).filter({ hasText: exact('Add') });
}

/** A people-search suggestion naming text (an email). */
export function shareSuggestion(page: Page, text: string): Locator {
  return page.locator(SELECTORS.shareSuggestion).filter({ hasText: text });
}

/** The home project grant row ("(home project)", no controls). */
export function homeGrantRow(page: Page): Locator {
  return page.locator(SELECTORS.shareGrantRow).filter({ hasText: '(home project)' });
}

/** A grant row naming any of the labels (display name or email). */
export function grantRow(page: Page, labels: string[]): Locator {
  const alternatives = labels
    .filter((l) => l !== '')
    .map(escapeRegExp)
    .join('|');
  return page
    .locator(SELECTORS.shareGrantRow)
    .filter({ hasText: new RegExp(alternatives) })
    .filter({ hasNotText: '(home project)' });
}

/** The Share dialog's footer close button (Done or Close). */
export function shareCloseButton(page: Page): Locator {
  return page.locator(SELECTORS.shareFooterButton).filter({ hasText: /^\s*(Done|Close)\s*$/ });
}

/** The open Share dialog's label (`Share “<title>”` or "Grant review access"). */
export function shareDialog(page: Page): Locator {
  return page.locator(SELECTORS.shareDialog);
}

/** A grant row's Permission select (artifact-share-dialog.ts:905). */
export function grantRowPermission(row: Locator): Locator {
  return row.locator('sl-select[aria-label="Permission"]');
}

/** A grant row's Remove button (artifact-share-dialog.ts:908). */
export function grantRowRemove(row: Locator): Locator {
  return row.locator('sl-button').filter({ hasText: exact('Remove') });
}

/** Any select or button inside a grant row (the home row has none). */
export function grantRowControls(row: Locator): { selects: Locator; buttons: Locator } {
  return { selects: row.locator('sl-select'), buttons: row.locator('sl-button') };
}

/** An option of a Shoelace select by its value. */
export function permissionOption(select: Locator, value: string): Locator {
  return select.locator(`sl-option[value="${value}"]`);
}

/** The artifact page's own heading or the 404 page's: the page has rendered. */
export function anyPageHeading(page: Page): Locator {
  return page.locator(`${SELECTORS.header} h1, ${SELECTORS.notFound} h1`).first();
}

/** Edit on the History tab's single-version block (artifact-detail.ts:1597). */
export function historyEditButton(page: Page): Locator {
  return page.locator(`${SELECTORS.historyPanel} sl-button`).filter({ hasText: exact('Edit') });
}

/** A row of the /artifacts list by its title link. */
export function artifactsRow(page: Page, title: string): Locator {
  return page
    .locator(SELECTORS.artifactsRow)
    .filter({ has: page.locator('a', { hasText: exact(title) }) });
}

/** The access badge cell of an /artifacts row (artifacts.ts:498, 511-517). */
export function artifactsRowAccess(row: Locator): Locator {
  return row.locator('td').nth(1);
}
