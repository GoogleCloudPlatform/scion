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
 * Pure breadcrumb trail builders (`ptone/scion#8281`).
 *
 * Imported only by lazy page chunks (never by the entry bundle). Owns
 * hierarchy wording, URL encoding, and truncation flags in one place.
 */

import type { Crumb } from '../components/shared/breadcrumb.js';

export interface Named {
  id: string;
  label: string;
  pending?: boolean;
}

const enc = encodeURIComponent;

function entity(label: string, href?: string, pending?: boolean): Crumb {
  const crumb: Crumb = { label, truncate: true };
  if (href !== undefined) crumb.href = href;
  if (pending) crumb.pending = true;
  return crumb;
}

/** Root crumb for the Projects list. */
export const projectsRoot = (): Crumb => ({ label: 'Projects', href: '/projects' });

/** `Projects / {project}` (both linked). */
export function projectTrail(p: Named): Crumb[] {
  return [projectsRoot(), entity(p.label, `/projects/${enc(p.id)}`, p.pending)];
}

/** `Projects / {project} / Settings` (all linked). */
export function projectSettingsTrail(p: Named): Crumb[] {
  return [...projectTrail(p), { label: 'Settings', href: `/projects/${enc(p.id)}/settings` }];
}

export type ResourceScope =
  | { kind: 'project'; project: Named }
  | { kind: 'user' }
  | { kind: 'hub' };

export interface ScopedResourceSpec {
  plural: string;
  projectTab: string;
  hubTab: string;
  profilePath?: string;
}

/** Parent chain for template / harness-config / service-account detail pages. */
export function scopedResourceTrail(scope: ResourceScope, res: ScopedResourceSpec): Crumb[] {
  switch (scope.kind) {
    case 'project':
      return [
        ...projectSettingsTrail(scope.project),
        {
          label: res.plural,
          href: `/projects/${enc(scope.project.id)}/settings?tab=${res.projectTab}`,
        },
      ];
    case 'hub':
      return [
        { label: 'Hub Resources', href: '/settings' },
        { label: res.plural, href: `/settings?tab=${res.hubTab}` },
      ];
    case 'user':
      return [
        { label: 'Profile', href: '/profile' },
        { label: res.plural, href: res.profilePath! },
      ];
  }
}

/** Agent parent trail: `Projects / {project}` when known, else `Agents`. */
export function agentParentTrail(project: Named | null): Crumb[] {
  return project ? projectTrail(project) : [{ label: 'Agents', href: '/agents' }];
}

/** Current-page crumb (unlinked; must be last in the trail). */
export const current = (label: string, truncate = true): Crumb => ({ label, truncate });
