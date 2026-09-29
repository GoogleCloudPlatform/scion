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
import { readFileSync } from 'fs';
import { join } from 'path';

const pageSource = readFileSync(join(__dirname, 'agent-detail.ts'), 'utf-8');

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

describe('agent detail layout', () => {
  let rules: Map<string, string>;

  beforeAll(async () => {
    await import('./agent-detail.js');
    const ctor = customElements.get('scion-page-agent-detail') as unknown as {
      styles: { cssText: string };
    };
    rules = styleRules(ctor.styles.cssText);
  });

  it('wraps a long agent name with its badges instead of floating them beside it', () => {
    const text = rules.get('.header-title-text') ?? '';
    expect(text).toMatch(/display:\s*flex/);
    expect(text).toMatch(/flex-wrap:\s*wrap/);
    expect(text).toMatch(/min-width:\s*0/);
    expect(rules.get('.header h1') ?? '').toMatch(/overflow-wrap:\s*anywhere/);
    expect(pageSource).toMatch(/<div class="header-title-text">\s*<h1>/);
  });

  it('keeps the message-mode select inside its column', () => {
    expect(rules.get('.messaging-grid') ?? '').toMatch(/flex-wrap:\s*wrap/);
    expect(rules.get('.messaging-grid .messaging-mode') ?? '').toMatch(/max-width:\s*360px/);
    expect(rules.get('.messaging-mode sl-select') ?? '').toMatch(/width:\s*100%/);
    // The select used to force itself wider than its grid column.
    expect(pageSource).not.toMatch(/min-width:\s*280px;\s*max-width:\s*360px/);
  });
});
