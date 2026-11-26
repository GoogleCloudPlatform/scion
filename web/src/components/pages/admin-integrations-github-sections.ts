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

export interface GitHubAdminSection {
  id: string;
  tag: string;
  load: () => Promise<unknown>;
}

/**
 * Ordered section registry for Admin → Integrations → GitHub.
 * Later milestones add their own section line and component file here.
 */
export const GITHUB_ADMIN_SECTIONS: readonly GitHubAdminSection[] = [
  {
    id: 'app',
    tag: 'scion-admin-github-app-config',
    load: () => import('../admin/admin-github-app-config.js'),
  },
  {
    id: 'installations',
    tag: 'scion-admin-github-installations',
    load: () => import('../admin/admin-github-installations.js'),
  },
  {
    id: 'associations',
    tag: 'scion-admin-github-associations',
    load: () => import('../admin/admin-github-associations.js'),
  },
  {
    id: 'routing',
    tag: 'scion-admin-github-routing',
    load: () => import('../admin/admin-github-routing.js'),
  },
];
