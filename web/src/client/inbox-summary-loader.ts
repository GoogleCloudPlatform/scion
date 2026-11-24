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
 * Lightweight lazy-loader and cached badge reader for `InboxSummaryStore`.
 *
 * Always-mounted shell components (`nav.ts`, `notification-tray.ts`) import
 * this module instead of `inbox-summary.ts` directly so the store stays out
 * of the initial `main.js` entry bundle while `web.inbox_dashboard` is off
 * and loads on demand when the flag is on.
 */

export const INBOX_SUMMARY_CHANGED_EVENT = 'scion:inbox-summary-changed';

interface InboxSummaryChangedDetail {
  total?: number;
  totalCapped?: boolean;
}

type InboxSummaryImporter = () => Promise<typeof import('./inbox-summary.js')>;
const defaultImportInboxSummary: InboxSummaryImporter = () => import('./inbox-summary.js');

let cachedTotal = 0;
let cachedTotalCapped = false;
let loadPromise: Promise<typeof import('./inbox-summary.js')> | null = null;
let importInboxSummary: InboxSummaryImporter = defaultImportInboxSummary;

if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
  window.addEventListener(INBOX_SUMMARY_CHANGED_EVENT, (e: Event) => {
    const detail = (e as CustomEvent<InboxSummaryChangedDetail | undefined>).detail;
    if (typeof detail?.total === 'number') {
      cachedTotal = detail.total;
      cachedTotalCapped = detail.totalCapped === true;
    }
  });
}

export function getInboxSummaryTotal(): number {
  return cachedTotal;
}

export function formatInboxSummaryBadge(): string | null {
  if (cachedTotal <= 0) return null;
  return cachedTotalCapped ? `${cachedTotal}+` : String(cachedTotal);
}

/** @internal test-only hook to simulate chunk import failures. */
export function setInboxSummaryImporterForTests(importer?: InboxSummaryImporter): void {
  importInboxSummary = importer ?? defaultImportInboxSummary;
  loadPromise = null;
}

/** @internal called by `inboxSummaryStore.resetForTests()`. */
export function resetInboxSummaryBadgeCacheForTests(): void {
  cachedTotal = 0;
  cachedTotalCapped = false;
  loadPromise = null;
  importInboxSummary = defaultImportInboxSummary;
}

/**
 * Dynamically loads `inbox-summary.ts` and starts `inboxSummaryStore` for the
 * given user id. Safe to call repeatedly; a failed chunk import is cleared so
 * a later call retries.
 */
export function ensureInboxSummaryStarted(userId?: string): void {
  if (!loadPromise) {
    const load = importInboxSummary();
    loadPromise = load;
    load.catch(() => {
      if (loadPromise === load) loadPromise = null;
    });
  }
  void loadPromise
    .then((mod) => {
      mod.inboxSummaryStore.ensureStarted(userId);
      if (
        cachedTotal !== mod.inboxSummaryStore.total ||
        cachedTotalCapped !== mod.inboxSummaryStore.totalCapped
      ) {
        cachedTotal = mod.inboxSummaryStore.total;
        cachedTotalCapped = mod.inboxSummaryStore.totalCapped;
        if (typeof window !== 'undefined' && typeof window.dispatchEvent === 'function') {
          window.dispatchEvent(
            new CustomEvent(INBOX_SUMMARY_CHANGED_EVENT, {
              detail: {
                summary: mod.inboxSummaryStore.summary,
                total: cachedTotal,
                totalCapped: cachedTotalCapped,
              },
            })
          );
        }
      }
    })
    .catch((err: unknown) => {
      console.warn('[inbox-summary-loader] failed to load inbox-summary chunk:', err);
    });
}
