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

  it('shows key suggestions on focus and accepts on Tab only after ArrowDown selection', async () => {
    const el = await mount();
    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.dispatchEvent(new Event('focus'));
    await el.updateComplete;

    const options = el.shadowRoot?.querySelectorAll('[role="option"]');
    expect(options?.length).toBeGreaterThanOrEqual(2);

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', bubbles: true }));
    await el.updateComplete;
    expect(el.value).toMatch(/^[a-z_]+:$/);

    // Accepting a key suggestion opens value suggestions and suppresses the
    // immediate "expected value" error while the listbox is open.
    expect(el.shadowRoot?.querySelector('.feedback.error')).toBeNull();
    expect(el.shadowRoot?.querySelector('.input-shell')?.classList.contains('has-error')).toBe(
      false
    );

    // Pressing Escape closes the value list and reveals the error if still empty.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.feedback.error')?.textContent).toContain(
      'expected value'
    );
  });

  it('highlights the offending span in the error feedback line', async () => {
    const el = await mount();
    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'phase:running OR phase:error';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    const mark = el.shadowRoot?.querySelector('.feedback.error mark.error-span');
    expect(mark).not.toBeNull();
    expect(mark?.textContent).toBe('OR');
  });

  it('does not cancel a pending debounced commit when a subsequent edit is invalid', async () => {
    vi.useFakeTimers();
    const el = await mount();
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'phase:running';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    vi.advanceTimersByTime(100);
    input.value = 'phase:running label:';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    vi.advanceTimersByTime(250);
    expect(commits).toHaveLength(1);
    expect(commits[0].serverQ).toBe('phase:running');
  });

  it('renders full schema-generated help popover including client-only badge', async () => {
    const el = await mount();
    const helpBtn = el.shadowRoot?.querySelector('.help-btn') as HTMLElement;
    helpBtn.click();
    await el.updateComplete;

    const popover = el.shadowRoot?.querySelector('.help-popover');
    expect(popover).not.toBeNull();
    expect(popover?.textContent).toContain('activity:');
    expect(popover?.textContent).toContain('project:');
    expect(popover?.textContent).toContain('Project slug or ID');
    expect(popover?.textContent).toContain('Runtime broker name or ID');
    expect(popover?.textContent).toContain("Owner ID, email, or 'me'");
    expect(popover?.textContent).not.toContain('name, slug, or ID');
    expect(popover?.textContent).toContain('mode:');
    expect(popover?.textContent).toContain('local');
  });

  it('does not hijack Tab on an empty focused box', async () => {
    const el = await mount();
    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.dispatchEvent(new Event('focus'));
    await el.updateComplete;

    const tabEvent = new KeyboardEvent('keydown', {
      key: 'Tab',
      bubbles: true,
      cancelable: true,
    });
    input.dispatchEvent(tabEvent);
    await el.updateComplete;

    expect(tabEvent.defaultPrevented).toBe(false);
    expect(el.value).toBe('');
  });

  it('commits plain text on Enter even when it fuzzy-matches a key name', async () => {
    const el = await mount();
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    for (const term of ['pe', 'lab']) {
      input.value = term;
      input.dispatchEvent(new Event('input'));
      await el.updateComplete;
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      await el.updateComplete;
    }

    expect(commits.map((c) => c.value)).toEqual(['pe', 'lab']);
    expect(commits.map((c) => c.serverQ)).toEqual(['pe', 'lab']);
  });

  it('cancels pending debounced commit when value is updated externally', async () => {
    vi.useFakeTimers();
    const el = await mount();
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'foo';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    // Simulate a chip click updating `.value` before the 300 ms debounce fires.
    el.value = 'phase:running foo';
    await el.updateComplete;

    vi.advanceTimersByTime(500);
    expect(commits).toHaveLength(0);
  });
});
