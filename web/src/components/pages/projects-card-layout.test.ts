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

/** Leaf style rules from Lit cssText. */
function styleRules(cssText: string): Map<string, string> {
  const rules = new Map<string, string>();
  const stack: string[] = [];
  let buf = '';
  for (const ch of cssText.replace(/\/\*[\s\S]*?\*\//g, '')) {
    if (ch === '{') {
      stack.push(buf.trim());
      buf = '';
    } else if (ch === '}') {
      const selector = stack.pop() ?? '';
      if (!selector.startsWith('@')) {
        for (const part of selector.split(',')) rules.set(part.trim(), buf);
      }
      buf = '';
    } else {
      buf += ch;
    }
  }
  return rules;
}

type ProjectsPage = HTMLElement & {
  renderProjectCard(project: Record<string, unknown>): TemplateResult;
};

describe('project card layout', () => {
  let rules: Map<string, string>;
  let page: ProjectsPage;

  beforeAll(async () => {
    await import('./projects.js');
    const ctor = customElements.get('scion-page-projects') as unknown as {
      styles: Array<{ cssText: string }>;
    };
    rules = styleRules(ctor.styles.map((s) => s.cssText).join('\n'));
    page = document.createElement('scion-page-projects') as ProjectsPage;
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

  it('lets a long unbroken name and git remote wrap instead of spilling past the card', () => {
    expect(rules.get('.project-header > div') ?? '').toMatch(/min-width:\s*0/);
    expect(rules.get('.resource-name > span') ?? '').toMatch(/overflow-wrap:\s*anywhere/);
    expect(rules.get('.resource-name') ?? '').toMatch(/min-width:\s*0/);
    expect(rules.get('.project-path') ?? '').toMatch(/word-break:\s*break-all/);
  });
});
