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

type SkillPage = HTMLElement & {
  renderSkillCard(item: Record<string, unknown>): TemplateResult;
};

describe('skill card layout', () => {
  let rules: Map<string, string>;
  let page: SkillPage;

  beforeAll(async () => {
    await import('./skills.js');
    rules = elementStyleRules('scion-page-skills');
    page = document.createElement('scion-page-skills') as unknown as SkillPage;
  });

  it('renders the skill name in the span the shared wrapping rules apply to', () => {
    const name = 'a_very_long_skill_name_with_no_spaces_or_hyphens_to_break_on';
    const container = document.createElement('div');
    render(
      page.renderSkillCard({
        id: 's1',
        name,
        scope: 'project',
        updated: new Date().toISOString(),
      }),
      container
    );
    const span = container.querySelector('.resource-name > span');
    expect(span?.textContent?.trim()).toBe(name);
  });

  it('lets a long unbroken name wrap instead of spilling past the card', () => {
    expect(rules.get('.skill-header > div') ?? '').toMatch(/min-width:\s*0/);
    expect(rules.get('.resource-name > span') ?? '').toMatch(/overflow-wrap:\s*anywhere/);
    expect(rules.get('.resource-name') ?? '').toMatch(/min-width:\s*0/);
  });
});
