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
 * Main Application Shell Component
 *
 * Provides the overall layout structure with sidebar navigation and content area.
 * Uses Shoelace components for UI and integrates with shared Scion components.
 */

import type { MessageKey } from './locales/en.js';

/**
 * Page title configuration
 */
export const PAGE_TITLES: Record<string, MessageKey> = {
  '/': 'page.dashboard',
  '/projects': 'page.projects',
  '/agents': 'page.agents',
  '/terminals': 'page.terminals',
  '/brokers': 'page.brokers',
  '/settings': 'page.settings',
  '/admin/scheduler': 'page.scheduler',
  '/admin/users': 'page.users',
  '/admin/groups': 'page.groups',
  '/admin/server-config': 'page.serverConfig',
  '/admin/federation': 'page.federation',
  '/admin/integrations': 'page.integrations',
  '/admin/roles': 'page.roles',
  '/admin/role-bindings': 'page.roleBindings',
  '/admin/quotas': 'page.quotas',
  '/metrics': 'page.metrics',
  '/admin/skill-registries': 'page.skillRegistries',
  '/skills': 'page.skills',
  '/github-app/installed': 'page.githubSetup',
  '/onboarding': 'page.setup',
};
