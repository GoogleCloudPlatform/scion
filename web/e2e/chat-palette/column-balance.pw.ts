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
 * Chromium: the two result columns stay the same width whatever the result
 * text. A document whose container path has no break opportunities wraps
 * inside its own column instead of widening that column, squeezing the
 * other one, or overflowing the panel. The single-column narrow layout
 * keeps every row inside the panel too.
 */

import { test, expect, type Page } from '@playwright/test';
import {
  setupApiMocks,
  SPACE_ALPHA,
  THREAD_ALPHA,
  USER_WITH_DM,
  DOC_TEXT_FILE,
} from './mock-api.js';

/** A single unbreakable token, far wider than either column. */
const LONG_FILE_NAME = `${'investigation'.repeat(4)}.md`;
const LONG_CONTAINER_PATH = `/scion-volumes/scratchpad/projects/${'roadmap'.repeat(12)}/${LONG_FILE_NAME}`;

/** Sub-pixel rounding allowance for column-width comparisons. */
const TOLERANCE_PX = 2;

async function openPaletteWithLongDocument(page: Page): Promise<void> {
  await setupApiMocks(page, {
    spaces: [SPACE_ALPHA],
    threadsByProjectId: { [SPACE_ALPHA.projectId]: [THREAD_ALPHA] },
    users: [USER_WITH_DM],
  });
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  await page.evaluate(
    (f) => window.chatPaletteFixture.seedRecentFiles(f),
    [
      {
        name: LONG_FILE_NAME,
        sentAt: DOC_TEXT_FILE.sentAt,
        projectId: DOC_TEXT_FILE.projectId,
        projectName: DOC_TEXT_FILE.projectName,
        target: {
          kind: 'path' as const,
          projectId: DOC_TEXT_FILE.projectId,
          containerPath: LONG_CONTAINER_PATH,
          location: { kind: 'workspace' as const, filePath: LONG_FILE_NAME },
        },
      },
    ]
  );

  await page.keyboard.press('Control+k');
  await expect(page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]')).toBeVisible();
  for (const group of ['agents', 'documents']) {
    await expect(
      page
        .locator(`scion-quick-palette [aria-labelledby="palette-heading-${group}"] .palette-option`)
        .first()
    ).toBeVisible();
  }
  // Measure the settled layout, not a frame of the dialog's open animation.
  await expect
    .poll(() =>
      page
        .locator('scion-quick-palette sl-dialog')
        .evaluate(
          (dialog) => dialog.shadowRoot!.querySelector('[part~="panel"]')!.getAnimations().length
        )
    )
    .toBe(0);
}

interface Layout {
  results: { left: number; right: number; scrollWidth: number; clientWidth: number };
  cells: Record<string, { left: number; width: number }>;
  overflowingRows: string[];
}

async function measure(page: Page): Promise<Layout> {
  return page.evaluate(() => {
    const findPalette = (scope: Document | ShadowRoot): Element | null => {
      const direct = scope.querySelector('scion-quick-palette');
      if (direct) return direct;
      for (const el of scope.querySelectorAll('*')) {
        const found = el.shadowRoot ? findPalette(el.shadowRoot) : null;
        if (found) return found;
      }
      return null;
    };
    const root = findPalette(document)!.shadowRoot!;
    const results = root.querySelector<HTMLElement>('.palette-results')!;
    const r = results.getBoundingClientRect();
    const cells: Record<string, { left: number; width: number }> = {};
    for (const cell of root.querySelectorAll<HTMLElement>('.palette-group-cell')) {
      const b = cell.getBoundingClientRect();
      cells[cell.dataset.paletteGroup!] = { left: b.left, width: b.width };
    }
    const overflowingRows: string[] = [];
    for (const row of root.querySelectorAll<HTMLElement>('.palette-option')) {
      const b = row.getBoundingClientRect();
      if (row.scrollWidth > row.clientWidth + 1 || b.right > r.right + 1 || b.left < r.left - 1) {
        overflowingRows.push(row.textContent!.trim().slice(0, 60));
      }
    }
    return {
      results: {
        left: r.left,
        right: r.right,
        scrollWidth: results.scrollWidth,
        clientWidth: results.clientWidth,
      },
      cells,
      overflowingRows,
    };
  });
}

test('a long unbreakable document path keeps both columns the same width', async ({ page }) => {
  await openPaletteWithLongDocument(page);
  const layout = await measure(page);

  const { agents, threads, people, documents } = layout.cells;
  expect(Math.abs(agents.width - threads.width)).toBeLessThanOrEqual(TOLERANCE_PX);
  expect(Math.abs(people.width - documents.width)).toBeLessThanOrEqual(TOLERANCE_PX);
  expect(Math.abs(agents.width - documents.width)).toBeLessThanOrEqual(TOLERANCE_PX);
  // Two columns side by side, not stacked.
  expect(documents.left).toBeGreaterThan(agents.left + agents.width - TOLERANCE_PX);

  expect(layout.overflowingRows).toEqual([]);
  expect(layout.results.scrollWidth).toBeLessThanOrEqual(layout.results.clientWidth + 1);
});

test('the full path stays visible, wrapped within its row', async ({ page }) => {
  await openPaletteWithLongDocument(page);
  const docRow = page
    .locator('scion-quick-palette [aria-labelledby="palette-heading-documents"] .palette-option')
    .first();
  await expect(docRow.locator('.palette-secondary')).toContainText(LONG_CONTAINER_PATH);
});

test('the single-column narrow layout keeps every row inside the panel', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openPaletteWithLongDocument(page);
  const layout = await measure(page);

  const { agents, documents } = layout.cells;
  // Stacked: every group starts at the same left edge.
  expect(Math.abs(agents.left - documents.left)).toBeLessThanOrEqual(TOLERANCE_PX);
  expect(Math.abs(agents.width - documents.width)).toBeLessThanOrEqual(TOLERANCE_PX);

  expect(layout.overflowingRows).toEqual([]);
  expect(layout.results.scrollWidth).toBeLessThanOrEqual(layout.results.clientWidth + 1);
});
