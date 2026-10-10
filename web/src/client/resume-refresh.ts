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
 * Calls back when a page comes back to the foreground after long enough in
 * the background that its live data may have gone stale.
 *
 * A mobile browser (an installed PWA most of all) suspends a hidden page and
 * drops its connections. The SSE client reconnects when the page is shown,
 * but the server keeps no backlog, so whatever was published while the page
 * was away never arrives: lists kept current by events stay as they were
 * when the page was hidden. A page that reloads its lists from here catches
 * up.
 *
 * Fires on `visibilitychange` to visible after at least `minHiddenMs` hidden,
 * and on `pageshow` for a page restored from the back/forward cache (which
 * was frozen however briefly it was away).
 */

/** How long a page must be hidden before showing it again counts as a resume. */
export const RESUME_REFRESH_AFTER_MS = 30_000;

export interface ResumeRefreshOptions {
  minHiddenMs?: number;
  /** Wall clock. Not `performance.now()`, which can stop while a device sleeps. */
  now?: () => number;
  doc?: Document;
  win?: Window;
}

export class ResumeRefresh {
  private readonly minHiddenMs: number;
  private readonly now: () => number;
  private readonly doc: Document;
  private readonly win: Window;
  private hiddenAt: number | null = null;
  private started = false;

  constructor(
    private readonly onResume: () => void,
    options: ResumeRefreshOptions = {}
  ) {
    this.minHiddenMs = options.minHiddenMs ?? RESUME_REFRESH_AFTER_MS;
    this.now = options.now ?? Date.now;
    this.doc = options.doc ?? document;
    this.win = options.win ?? window;
  }

  start(): void {
    if (this.started) return;
    this.started = true;
    this.hiddenAt = this.doc.visibilityState === 'hidden' ? this.now() : null;
    this.doc.addEventListener('visibilitychange', this.onVisibilityChange);
    this.win.addEventListener('pageshow', this.onPageShow);
  }

  stop(): void {
    if (!this.started) return;
    this.started = false;
    this.hiddenAt = null;
    this.doc.removeEventListener('visibilitychange', this.onVisibilityChange);
    this.win.removeEventListener('pageshow', this.onPageShow);
  }

  private readonly onVisibilityChange = (): void => {
    if (this.doc.visibilityState === 'hidden') {
      // Keep the first instant of a run of hidden events.
      this.hiddenAt ??= this.now();
      return;
    }
    if (this.doc.visibilityState !== 'visible') return;
    const hiddenAt = this.hiddenAt;
    this.hiddenAt = null;
    if (hiddenAt !== null && this.now() - hiddenAt >= this.minHiddenMs) this.onResume();
  };

  private readonly onPageShow = (e: Event): void => {
    if (!(e as PageTransitionEvent).persisted) return;
    this.hiddenAt = null;
    this.onResume();
  };
}
