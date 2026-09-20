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
 * functions so that every agent-terminal action reaches the retained workspace
 * via the coordinator when the terminal-workspace feature flag is enabled,
 * and falls back to the legacy route when it is not.
 *
 * Navigation is dispatched through the `nav-click` custom event that the
 * client-side router already listens for.  This avoids a circular import
 * between entry-point components and main.ts.
 */

import { isFeatureEnabled } from '../utils/feature-flags.js';

/**
 * Return the terminal URL path for an agent.
 *
 * When the terminal workspace is enabled, returns `/terminals/{agentId}` so
 * that the coordinator handles ownership and session reuse.  Otherwise returns
 * the legacy `/agents/{agentId}/terminal` standalone route.
 *
 * Use this as the `href` value on links and buttons to preserve real hrefs,
 * accessible names, keyboard activation, modified-click targets, and download
 * semantics.
 */
export function terminalHref(agentId: string): string {
  return isFeatureEnabled('web.terminal_workspace')
    ? `/terminals/${agentId}`
    : `/agents/${agentId}/terminal`;
}

/**
 * Programmatically open a terminal for the given agent.
 *
 * Routes through the workspace coordinator when the terminal workspace is
 * enabled, or navigates to the legacy standalone terminal page otherwise.
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
      composed: true,
    })
  );
}
