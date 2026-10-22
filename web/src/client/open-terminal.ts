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
 * Central terminal-opening helpers.
 *
 * All list, detail, project, graph, tree and chat entry points use these
 * functions so that every agent-terminal action reaches the retained terminal
 * workspace via the coordinator.
 *
 * Navigation is dispatched through the `nav-click` custom event that the
 * client-side router already listens for.  This avoids a circular import
 * between entry-point components and main.ts.
 */

/**
 * Return the terminal URL path for an agent: `/terminals/{agentId}`, so that
 * the terminal workspace coordinator handles ownership and session reuse.
 *
 * Use this as the `href` value on links and buttons to preserve real hrefs,
 * accessible names, keyboard activation, modified-click targets, and download
 * semantics.
 */
export function terminalHref(agentId: string): string {
  return `/terminals/${agentId}`;
}

/**
 * Programmatically open a terminal for the given agent.
 *
 * Routes through the terminal workspace coordinator.
 *
 * This dispatches a `nav-click` event that the router in `main.ts` handles,
 * avoiding a direct import of `navigateTo` and the circular dependency that
 * would create.
 */
export function openTerminal(agentId: string): void {
  document.dispatchEvent(
    new CustomEvent('nav-click', {
      detail: { path: terminalHref(agentId) },
      bubbles: true,
    })
  );
}

/**
 * Return the graph-view URL for an agent, focused on that agent within its
 * project.
 *
 * The chat toolbar, members sidebar and message context menu each render an
 * "Open in graph" control for a (possibly different) agent; this is the
 * single place that builds the URL so the three never drift out of sync.
 */
export function agentGraphHref(projectId: string, agentId: string): string {
  return `/agents/graph?project=${encodeURIComponent(projectId)}&focus=${encodeURIComponent(agentId)}`;
}
