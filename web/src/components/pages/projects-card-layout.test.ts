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

import { describe, it, expect, beforeAll } from 'vitest';
import { render, type TemplateResult } from 'lit';
import { elementStyleRules } from './__fixtures__/css-rules.js';

type ProjectsPage = HTMLElement & {
  renderProjectCard(project: Record<string, unknown>): TemplateResult;
};

describe('project card layout', () => {
  let rules: Map<string, string>;
  let page: ProjectsPage;

  beforeAll(async () => {
    await import('./projects.js');
    rules = elementStyleRules('scion-page-projects');
    page = document.createElement('scion-page-projects') as unknown as ProjectsPage;
  });

  it('renders the project name in the span the shared wrapping rules apply to', () => {
    const name = 'a_very_long_project_name_with_no_spaces_or_hyphens_to_break_on';
    const container = document.createElement('div');
    render(
      page.renderProjectCard({
        id: 'p1',
        name,
        slug: name,
        agentCount: 1,
        ownerName: 'Owner',
        gitRemote: `https://git.example.com/org/${name}.git`,
      }),
      container
    );
    const span = container.querySelector('.resource-name > span');
    expect(span?.textContent?.trim()).toBe(name);
  });

  it('keeps the linked badge inside the wrapping span', () => {
    const container = document.createElement('div');
    render(
      page.renderProjectCard({
        id: 'p2',
        name: 'linked_project',
        slug: 'linked_project',
        agentCount: 0,
        ownerName: 'Owner',
        projectType: 'linked',
      }),
      container
    );
    expect(container.querySelector('.resource-name > span sl-tooltip')).not.toBeNull();
  });

  it('lets a long unbroken name and git remote wrap instead of spilling past the card', () => {
    expect(rules.get('.project-header > div') ?? '').toMatch(/min-width:\s*0/);
    expect(rules.get('.resource-name > span') ?? '').toMatch(/overflow-wrap:\s*anywhere/);
    expect(rules.get('.resource-name') ?? '').toMatch(/min-width:\s*0/);
    expect(rules.get('.project-path') ?? '').toMatch(/word-break:\s*break-all/);
  });
});
