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
  currentSegments = segments;
  applyDocumentTitle();
}

/** The most recent title segments, replayed when the unread count changes. */
let currentSegments: string[] = [];

/** Unread conversations, rendered as a "(N) " prefix on the tab title. */
let unreadCount = 0;

/**
 * Writes `document.title` from the current segments and unread count.
 *
 * The prefix is applied here, on every write, rather than at the call sites:
 * routes change the title constantly (chat-shell on navigation, page
 * components again once their data loads) and any one of those writes would
 * otherwise silently drop the badge.
 */
function applyDocumentTitle(): void {
  const base = currentSegments.length === 0 ? APP_NAME : [...currentSegments, APP_NAME].join(' — ');
  document.title = unreadCount > 0 ? `(${unreadCount}) ${base}` : base;
}

/**
 * Sets the unread conversation count shown in the tab title.
 *
 * This is unread state, not notification state: it is deliberately
 * independent of the browser permission and of the push master toggle, since
 * a user who declined desktop popups has not asked to stop seeing their own
 * unread count.
 */
export function setUnreadBadge(count: number): void {
  const next = Number.isFinite(count) ? Math.max(0, Math.floor(count)) : 0;
  if (next === unreadCount) return;
  unreadCount = next;
  applyDocumentTitle();
}

/** The count currently shown in the tab title. */
export function getUnreadBadge(): number {
  return unreadCount;
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
    })
  );
}
