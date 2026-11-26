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

export interface IntegrationTabContext {
  canReadGitHubApp: boolean;
}

export interface IntegrationTab {
  id: string;
  label: string;
  icon: string;
  tag: string;
  load: () => Promise<unknown>;
  visible: (ctx: IntegrationTabContext) => boolean;
}

/**
 * Client-side registry of Admin → Integrations tabs.
 * Tab ids are reserved path segments under /admin/integrations/<id>.
 * Adding an integration tab is one entry here plus one lazily loaded component.
 */
export const INTEGRATION_TABS: readonly IntegrationTab[] = [
  {
    id: 'chat',
    label: 'Chat',
    icon: 'chat-dots',
    tag: 'scion-admin-integrations-chat',
    load: () => import('./admin-integrations-chat.js'),
    visible: () => true,
  },
  {
    id: 'github',
    label: 'GitHub',
    icon: 'github',
    tag: 'scion-admin-integrations-github',
    load: () => import('./admin-integrations-github.js'),
    visible: (ctx) => ctx.canReadGitHubApp,
  },
];

export const INTEGRATION_TAB_IDS = new Set(INTEGRATION_TABS.map((t) => t.id));

export function isIntegrationTabId(segment: string): boolean {
  return INTEGRATION_TAB_IDS.has(segment);
}

function safeDecodeURIComponent(value: string): string | null {
  try {
    return decodeURIComponent(value);
  } catch {
    return null;
  }
}

/**
 * Legacy redirect helper: an old /admin/integrations/{name} URL where {name}
 * is not a registered tab id redirects to /admin/integrations/chat/{name}.
 */
export function legacyIntegrationRedirect(pathname: string): string | null {
  const match = pathname.match(/^\/admin\/integrations\/([^/]+)$/);
  if (!match) return null;
  const segment = safeDecodeURIComponent(match[1]);
  if (segment === null || isIntegrationTabId(segment)) return null;
  return `/admin/integrations/chat/${match[1]}`;
}

/**
 * Parses an /admin/integrations route into its tab id and optional sub-path
 * (e.g. /admin/integrations/chat/telegram -> { tabId: 'chat', subPath: 'telegram' }).
 * Only the chat tab supports detail sub-paths.
 */
export function parseIntegrationsRoute(pathname: string): {
  tabId: string | null;
  subPath: string | null;
} {
  const subMatch = pathname.match(/^\/admin\/integrations\/([^/]+)\/([^/]+)$/);
  if (subMatch) {
    const tabId = safeDecodeURIComponent(subMatch[1]);
    const subPath = safeDecodeURIComponent(subMatch[2]);
    if (tabId === null || subPath === null) {
      return { tabId: null, subPath: null };
    }
    return {
      tabId,
      subPath: tabId === 'chat' ? subPath : null,
    };
  }
  const tabMatch = pathname.match(/^\/admin\/integrations\/([^/]+)$/);
  if (tabMatch) {
    const seg = safeDecodeURIComponent(tabMatch[1]);
    if (seg === null) {
      return { tabId: null, subPath: null };
    }
    if (isIntegrationTabId(seg)) {
      return { tabId: seg, subPath: null };
    }
    return { tabId: 'chat', subPath: seg };
  }
  return { tabId: null, subPath: null };
}
