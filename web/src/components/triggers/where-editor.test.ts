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

import { describe, it, expect, afterEach } from 'vitest';
import './where-editor.js';
import type { ScionTriggerWhereEditor } from './where-editor.js';
import { parseTriggerRuleRowErrors, type TriggerMatchRuleSpec } from './types.js';

describe('scion-trigger-where-editor', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('shows Types-only note when empty and adds/reorders/removes rows', async () => {
    const el = document.createElement('scion-trigger-where-editor') as ScionTriggerWhereEditor;
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-where-empty]')).toBeTruthy();

    let latest: TriggerMatchRuleSpec[] = [];
    el.addEventListener('rules-changed', (e: Event) => {
      latest = (e as CustomEvent<{ rules: TriggerMatchRuleSpec[] }>).detail.rules;
    });

    const addBtn = el.shadowRoot?.querySelector('[data-add-where-rule]') as HTMLElement;
    addBtn.click();
    await el.updateComplete;
    expect(latest).toEqual([{ dialect: 'cel', expr: '' }]);

    // Set expression on row 0 and add row 1
    const expr0 = el.shadowRoot?.querySelector('[data-where-expr="0"]') as HTMLTextAreaElement;
    expr0.value = 'first == true';
    expr0.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;

    addBtn.click();
    await el.updateComplete;
    const expr1 = el.shadowRoot?.querySelector('[data-where-expr="1"]') as HTMLTextAreaElement;
    expr1.value = 'second == true';
    expr1.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;

    expect(latest.map((r) => r.expr)).toEqual(['first == true', 'second == true']);

    // Move row 1 up
    const moveUp1 = el.shadowRoot?.querySelector('[data-where-up="1"]') as HTMLElement;
    moveUp1.click();
    await el.updateComplete;
    expect(latest.map((r) => r.expr)).toEqual(['second == true', 'first == true']);

    // Remove row 0
    const remove0 = el.shadowRoot?.querySelector('[data-where-remove="0"]') as HTMLElement;
    remove0.click();
    await el.updateComplete;
    expect(latest.map((r) => r.expr)).toEqual(['first == true']);
  });

  it('shows only admitted dialects in per-row select', async () => {
    const el = document.createElement('scion-trigger-where-editor') as ScionTriggerWhereEditor;
    el.rules = [{ dialect: 'cel', expr: 'true' }];
    document.body.appendChild(el);
    await el.updateComplete;

    const options = Array.from(
      el.shadowRoot?.querySelectorAll('[data-where-dialect="0"] sl-option') ?? []
    ).map((o) => o.getAttribute('value'));
    expect(options).toEqual(['cel']);
  });

  it('parses 400 invalid_rule per-element errors (1-based) and renders them safely as text', async () => {
    const parsed = parseTriggerRuleRowErrors({
      error: {
        code: 'invalid_rule',
        message: 'syntax error',
        details: {
          field: 'match',
          elements: [
            {
              element: 2,
              category: 'syntax',
              message: '<script>alert(1)</script> unexpected token',
              line: 1,
              column: 14,
            },
          ],
        },
      },
    });

    expect(parsed[1]).toEqual({
      category: 'syntax',
      message: '<script>alert(1)</script> unexpected token',
      line: 1,
      column: 14,
      dialect: undefined,
      reason: undefined,
    });

    const el = document.createElement('scion-trigger-where-editor') as ScionTriggerWhereEditor;
    el.rules = [
      { dialect: 'cel', expr: 'true' },
      { dialect: 'cel', expr: 'bad ===' },
    ];
    el.rowErrors = parsed;
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-where-error="0"]')).toBeNull();
    const errEl = el.shadowRoot?.querySelector('[data-where-error="1"]');
    expect(errEl).toBeTruthy();
    expect(errEl?.textContent).toContain('1:14');
    expect(errEl?.textContent).toContain('[syntax]');
    expect(errEl?.textContent).toContain('<script>alert(1)</script>');
    expect(errEl?.querySelector('script')).toBeNull();
  });

  it('hides mutation controls in readonly mode', async () => {
    const el = document.createElement('scion-trigger-where-editor') as ScionTriggerWhereEditor;
    el.readonly = true;
    el.rules = [{ dialect: 'cel', expr: 'true' }];
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-add-where-rule]')).toBeNull();
    expect(el.shadowRoot?.querySelector('[data-where-remove="0"]')).toBeNull();
  });
});
