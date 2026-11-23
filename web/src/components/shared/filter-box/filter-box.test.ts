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

import { afterEach, describe, expect, it, vi } from 'vitest';
import './filter-box.js';
import type { FilterCommitDetail, FilterInputDetail, ScionFilterBox } from './filter-box.js';

describe('<scion-filter-box>', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.useRealTimers();
  });

  async function mount(value = ''): Promise<ScionFilterBox> {
    const el = document.createElement('scion-filter-box') as ScionFilterBox;
    el.view = 'agents';
    el.value = value;
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  it('emits filter-input immediately and filter-commit after debounce', async () => {
    vi.useFakeTimers();
    const el = await mount();
    const inputs: FilterInputDetail[] = [];
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-input', (e) => inputs.push((e as CustomEvent).detail));
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'phase:running';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    expect(inputs).toHaveLength(1);
    expect(inputs[0].valid).toBe(true);
    expect(inputs[0].query.mode).toBe('structured');
    expect(commits).toHaveLength(0);

    vi.advanceTimersByTime(300);
    expect(commits).toHaveLength(1);
    expect(commits[0].serverQ).toBe('phase:running');
  });

  it('keeps the last valid query applied when an invalid query is typed', async () => {
    vi.useFakeTimers();
    const el = await mount('phase:running');
    const inputs: FilterInputDetail[] = [];
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-input', (e) => inputs.push((e as CustomEvent).detail));
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'phase:';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    expect(inputs).toHaveLength(1);
    expect(inputs[0].valid).toBe(false);
    // Last valid query ("phase:running") stays applied.
    expect(inputs[0].query.mode).toBe('structured');
    if (inputs[0].query.mode === 'structured') {
      expect(inputs[0].query.terms[0].values).toEqual(['running']);
    }

    // No server commit emitted for invalid query.
    vi.advanceTimersByTime(500);
    expect(commits).toHaveLength(0);

    const feedback = el.shadowRoot?.querySelector('.feedback');
    expect(feedback?.classList.contains('error')).toBe(true);
    expect(feedback?.textContent).toContain('expected value');
  });

  it('shows a non-blocking warning for unknown keys while still emitting valid', async () => {
    vi.useFakeTimers();
    const el = await mount();
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'foo:bar';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    const feedback = el.shadowRoot?.querySelector('.feedback');
    expect(feedback?.classList.contains('warning')).toBe(true);
    expect(feedback?.textContent).toContain("Unknown key 'foo'");

    vi.advanceTimersByTime(300);
    expect(commits).toHaveLength(1);
    expect(commits[0].serverQ).toBe('foo:bar');
  });

  it('shows key suggestions on focus and accepts on Tab', async () => {
    const el = await mount();
    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.dispatchEvent(new Event('focus'));
    await el.updateComplete;

    const options = el.shadowRoot?.querySelectorAll('[role="option"]');
    expect(options?.length).toBeGreaterThanOrEqual(2);

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', bubbles: true }));
    await el.updateComplete;
    expect(el.value).toMatch(/^(label|phase):$/);
  });
});
