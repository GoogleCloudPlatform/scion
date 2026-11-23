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

import { describe, it, expect } from 'vitest';

import {
  agentParentTrail,
  current,
  projectSettingsTrail,
  projectTrail,
  projectsRoot,
  scopedResourceTrail,
} from './crumbs.js';

describe('crumbs trail builders', () => {
  it('builds the Projects root crumb', () => {
    expect(projectsRoot()).toEqual({ label: 'Projects', href: '/projects' });
  });

  it('builds projectTrail with truncation, pending flag, and URL encoding', () => {
    expect(projectTrail({ id: 'p-1', label: 'Payments' })).toEqual([
      { label: 'Projects', href: '/projects' },
      { label: 'Payments', href: '/projects/p-1', truncate: true },
    ]);

    expect(projectTrail({ id: 'org/p?a=1', label: 'org/p?a=1', pending: true })).toEqual([
      { label: 'Projects', href: '/projects' },
      { label: 'org/p?a=1', href: '/projects/org%2Fp%3Fa%3D1', truncate: true, pending: true },
    ]);
  });

  it('builds projectSettingsTrail with URL encoding', () => {
    expect(projectSettingsTrail({ id: 'org/p?a=1', label: 'Payments', pending: true })).toEqual([
      { label: 'Projects', href: '/projects' },
      { label: 'Payments', href: '/projects/org%2Fp%3Fa%3D1', truncate: true, pending: true },
      { label: 'Settings', href: '/projects/org%2Fp%3Fa%3D1/settings' },
    ]);
  });

  it('builds scopedResourceTrail for project, hub, and user scopes', () => {
    const templateSpec = {
      plural: 'Templates',
      projectTab: 'templates',
      hubTab: 'templates',
      profilePath: '/profile/templates',
    };

    expect(
      scopedResourceTrail(
        { kind: 'project', project: { id: 'org/p?a=1', label: 'Payments' } },
        templateSpec
      )
    ).toEqual([
      { label: 'Projects', href: '/projects' },
      { label: 'Payments', href: '/projects/org%2Fp%3Fa%3D1', truncate: true },
      { label: 'Settings', href: '/projects/org%2Fp%3Fa%3D1/settings' },
      { label: 'Templates', href: '/projects/org%2Fp%3Fa%3D1/settings?tab=templates' },
    ]);

    expect(scopedResourceTrail({ kind: 'hub' }, templateSpec)).toEqual([
      { label: 'Hub Resources', href: '/settings' },
      { label: 'Templates', href: '/settings?tab=templates' },
    ]);

    expect(scopedResourceTrail({ kind: 'user' }, templateSpec)).toEqual([
      { label: 'Profile', href: '/profile' },
      { label: 'Templates', href: '/profile/templates' },
    ]);
  });

  it('builds agentParentTrail under project when known and under Agents otherwise', () => {
    expect(agentParentTrail({ id: 'p-1', label: 'Payments' })).toEqual([
      { label: 'Projects', href: '/projects' },
      { label: 'Payments', href: '/projects/p-1', truncate: true },
    ]);
    expect(agentParentTrail(null)).toEqual([{ label: 'Agents', href: '/agents' }]);
  });

  it('builds current crumb with default truncate=true and optional truncate=false', () => {
    expect(current('worker-1')).toEqual({ label: 'worker-1', truncate: true });
    expect(current('Settings', false)).toEqual({ label: 'Settings', truncate: false });
  });
});
