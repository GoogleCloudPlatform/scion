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
 * Selectors of the artifact render journey, added to the access journey's
 * selectors.ts (which this file reuses and does not change). The artifact
 * pages have no data-testid yet, so these are role, text and structure
 * selectors; when the testids land, only the selector files change.
 */

import type { Locator, Page } from '@playwright/test';

import { SELECTORS, escapeRegExp } from './selectors.js';

function exact(label: string): RegExp {
  return new RegExp(`^\\s*${escapeRegExp(label)}\\s*$`);
}

const PAGE = 'scion-page-artifact-detail';
const PREVIEW = `${PAGE} sl-tab-panel[name="preview"]`;
const FILES = `${PAGE} sl-tab-panel[name="files"]`;
const DIALOG = 'scion-artifact-publish-dialog';

export const RENDER_SELECTORS = {
  /** Publish dialog: mode Folder, folder input, entry, note (artifact-publish-dialog.ts:286-381). */
  publishModeFolder: `${DIALOG} sl-radio-button[value="folder"]`,
  publishFolderInput: `${DIALOG} #folder-input`,
  publishEntrySelect: `${DIALOG} sl-select[label="Entry file"]`,
  publishNoteInput: `${DIALOG} sl-input[label="Note (optional)"] input`,
  /** HTML view (artifact-detail.ts:1296-1358). */
  htmlBar: `${PREVIEW} .entry-bar`,
  untrustedBar: `${PREVIEW} .untrusted-bar`,
  htmlFrame: `${PREVIEW} .untrusted iframe`,
  remoteImagesNotice: `${PREVIEW} sl-alert.notice`,
  /** Markdown frame (artifact-markdown-frame.ts:173-178). */
  markdownIframe: `${PREVIEW} scion-artifact-markdown-frame iframe[title="Markdown preview"]`,
  entryBarButton: `${PREVIEW} .entry-bar sl-button`,
  /** Files tab (artifact-detail.ts:1361-1425). */
  filesSummary: `${FILES} .summary`,
  filesRow: `${FILES} tbody tr`,
  /** Version menu items (artifact-detail.ts:1084-1098). */
  versionMenuItem: `${SELECTORS.headerActions} sl-dropdown sl-menu-item`,
  /** History rows of ready versions (artifact-detail.ts:1503-1535); discarded groups are tr.discarded-group. */
  historyVersionRow: `${SELECTORS.historyPanel} tbody tr:has(strong)`,
  historyDiscardedRow: `${SELECTORS.historyPanel} tbody tr.discarded-group`,
  /** Chat: a linkified scion://artifact/ reference (utils/chat-file-links.ts:1176-1182). */
  chatArtifactLink: 'a.artifact-link',
  /** Chat: an artifact chip, shown only for references the hub recorded (chat-message.ts:2672-2704). */
  chatArtifactChip: 'button.artifact-chip',
  /** Chat preview dialog and its markdown frame (chat-file-preview.ts:917-926, 1017-1054). */
  chatPreviewDialog: 'scion-chat-file-preview sl-dialog.file-preview-dialog[open]',
  chatPreviewLabel: 'scion-chat-file-preview sl-dialog.file-preview-dialog[open] .artifact-label',
  chatMarkdownIframe:
    'scion-chat-file-preview scion-artifact-markdown-frame iframe[title="Markdown preview"]',
} as const;

/** A preview entry-bar button by exact label (Full screen, Open in new tab, Open raw, Download). */
export function previewBarButton(page: Page, label: string): Locator {
  return page.locator(RENDER_SELECTORS.entryBarButton).filter({ hasText: exact(label) });
}

/** The HTML bundle frame's content. */
export function htmlFrame(page: Page): ReturnType<Page['frameLocator']> {
  return page.frameLocator(RENDER_SELECTORS.htmlFrame);
}

/** The markdown preview frame's content. */
export function markdownFrame(page: Page): ReturnType<Page['frameLocator']> {
  return page.frameLocator(RENDER_SELECTORS.markdownIframe);
}

/** The remote-images notice of the HTML view. */
export function remoteImagesNotice(page: Page): Locator {
  return page
    .locator(RENDER_SELECTORS.remoteImagesNotice)
    .filter({ hasText: 'Remote images are not loaded' });
}

/** A Files tab row by its path (artifact-detail.ts:1392-1420). */
export function fileRow(page: Page, filePath: string): Locator {
  return page
    .locator(RENDER_SELECTORS.filesRow)
    .filter({ has: page.locator('.file-path', { hasText: new RegExp(escapeRegExp(filePath)) }) });
}

/** A Files tab icon button by exact label (Open <path>, Download <path>, Not shown in the browser). */
export function fileIconButton(row: Locator, label: string): Locator {
  return row.locator(`sl-icon-button[label="${label}"]`);
}

/** A version menu item by seq. */
export function versionMenuItem(page: Page, seq: number): Locator {
  return page.locator(`${RENDER_SELECTORS.versionMenuItem}[value="${seq}"]`);
}

/** History's View vN button (artifact-detail.ts:1520-1526). */
export function historyViewButton(page: Page, seq: number): Locator {
  return page.locator(`${SELECTORS.historyPanel} sl-icon-button[label="View v${seq}"]`);
}

/** The publish dialog's Folder mode button. */
export function publishFolderMode(page: Page): Locator {
  return page.locator(RENDER_SELECTORS.publishModeFolder);
}
