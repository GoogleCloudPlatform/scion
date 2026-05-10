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
 * Dynamic page title management for the SPA.
 *
 * Provides a central function for setting the browser document title with
 * hierarchical context segments (e.g. "my-project — Projects — Scion").
 * Page components dispatch the custom event to refine the title with entity
 * names once data has loaded.
 */

const APP_NAME = 'Scion';

/**
 * Custom event name dispatched by page components to refine the document title
 * with entity-specific context (project name, agent name, etc.).
 */
export const PAGE_TITLE_EVENT = 'scion:page-title';

export interface PageTitleDetail {
  /** Title segments from most-specific to least-specific, e.g. ['my-agent', 'my-project'] */
  segments: string[];
}

/**
 * Set the browser document title from context segments.
 *
 * Segments are ordered most-specific first and joined with " — ".
 * The app name is always appended as the last segment.
 *
 * Examples:
 *   setDocumentTitle('Dashboard')              → "Dashboard — Scion"
 *   setDocumentTitle('my-project', 'Projects')     → "my-project — Projects — Scion"
 *   setDocumentTitle('agent-1', 'my-project')    → "agent-1 — my-project — Scion"
 */
export function setDocumentTitle(...segments: string[]): void {
  if (segments.length === 0) {
    document.title = APP_NAME;
    return;
  }
  document.title = [...segments, APP_NAME].join(' — ');
}

/**
 * Dispatch a page-title event from a page component so the shell can update
 * both the header and the document title with entity-specific context.
 *
 * Call this after entity data has loaded (e.g. project name, agent name).
 */
export function dispatchPageTitle(element: HTMLElement, ...segments: string[]): void {
  element.dispatchEvent(
    new CustomEvent<PageTitleDetail>(PAGE_TITLE_EVENT, {
      detail: { segments },
      bubbles: true,
      composed: true,
    }),
  );
}
