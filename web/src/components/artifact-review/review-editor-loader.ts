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
 * Single-flight loader of the review editor chunk (review-editor.ts and
 * the editor stack). The artifact page imports only this module; the
 * editor is fetched when a review opens in the graphical view, or ahead
 * of time when the page is idle. A failed load is not kept, so the next
 * call tries again.
 */

/** What the artifact page calls on a <scion-review-editor>. */
export interface ReviewEditorElement extends HTMLElement {
  flush(): string | null;
  comment(): Promise<void>;
  /** Whether the reviewer has made a suggestion or comment in this view since it opened. */
  hasOwnMarks(): boolean;
  /** Whether an input method composition is still running (its text cannot be written yet). */
  isComposing(): boolean;
}

/*
 * import.meta.glob gives the same lazily loaded chunk as import(), and
 * keeps the editor (typed as its own TypeScript project, see tsconfig.json
 * in this folder) out of the page's type graph.
 */
const editorModule = import.meta.glob('./review-editor.ts');

let pending: Promise<unknown> | null = null;

/** Loads the editor chunk once; it registers <scion-review-editor>. */
export function loadReviewEditor(): Promise<unknown> {
  if (!pending) {
    const importer = editorModule['./review-editor.ts'];
    const load = importer ? importer() : Promise.reject(new Error('review editor missing'));
    pending = load;
    load.catch(() => {
      if (pending === load) pending = null;
    });
  }
  return pending;
}

/** Starts loading the editor chunk when the browser is idle. */
export function preloadReviewEditor(): void {
  const start = (): void => {
    loadReviewEditor().catch(() => {
      // Loaded again (and reported) when a review opens.
    });
  };
  if (typeof window.requestIdleCallback === 'function') {
    window.requestIdleCallback(start, { timeout: 5000 });
  } else {
    setTimeout(start, 1000);
  }
}
