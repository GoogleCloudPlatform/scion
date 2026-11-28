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

  it('emits filter-input live without committing on debounce, and commits on Enter', async () => {
    vi.useFakeTimers();
    const el = await mount();
    const inputs: FilterInputDetail[] = [];
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-input', (e) => inputs.push((e as CustomEvent).detail));
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.focus();
    input.value = 'phase:running';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    expect(inputs).toHaveLength(1);
    expect(inputs[0].valid).toBe(true);
    expect(inputs[0].query.mode).toBe('structured');
    expect(commits).toHaveLength(0);

    // Waiting past the old 300ms debounce must NOT commit while editing.
    vi.advanceTimersByTime(1000);
    expect(commits).toHaveLength(0);

    // Pressing Enter commits immediately and keeps focus.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await el.updateComplete;
    expect(commits).toHaveLength(1);
    expect(commits[0].serverQ).toBe('phase:running');
    expect(el.shadowRoot?.activeElement).toBe(input);
  });

  it('commits dirty valid query on blur and skips blur commit when unchanged or invalid', async () => {
    const el = await mount('phase:running');
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    // Focus and blur without editing -> no commit.
    input.dispatchEvent(new FocusEvent('focus'));
    input.dispatchEvent(new FocusEvent('blur', { relatedTarget: document.body }));
    await el.updateComplete;
    expect(commits).toHaveLength(0);

    // Edit to a new valid value and blur -> commits once.
    input.value = 'phase:stopped';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    expect(commits).toHaveLength(0);

    input.dispatchEvent(new FocusEvent('blur', { relatedTarget: document.body }));
    await el.updateComplete;
    expect(commits).toHaveLength(1);
    expect(commits[0].value).toBe('phase:stopped');
    expect(commits[0].serverQ).toBe('phase:stopped');

    // Blurring again without further edits does not re-commit.
    input.dispatchEvent(new FocusEvent('blur', { relatedTarget: document.body }));
    await el.updateComplete;
    expect(commits).toHaveLength(1);

    // Edit to an invalid value and blur -> does NOT commit, keeps inline error.
    input.value = 'phase:';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    input.dispatchEvent(new FocusEvent('blur', { relatedTarget: document.body }));
    await el.updateComplete;
    expect(commits).toHaveLength(1);
    expect(el.shadowRoot?.querySelector('.feedback.error')?.textContent).toContain(
      'expected value'
    );
  });

  it('does not commit on blur when focus moves to the help button inside the filter box', async () => {
    const el = await mount();
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    const helpBtn = el.shadowRoot?.querySelector('.help-btn') as HTMLElement;
    input.value = 'phase:running';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    input.dispatchEvent(new FocusEvent('blur', { relatedTarget: helpBtn }));
    await el.updateComplete;
    expect(commits).toHaveLength(0);
  });

  it('commits immediately on explicit clear button click and keeps focus', async () => {
    const el = await mount('phase:running');
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'phase:stopped';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    const clearBtn = el.shadowRoot?.querySelector('.clear-btn') as HTMLButtonElement;
    const mousedown = new MouseEvent('mousedown', { bubbles: true, cancelable: true });
    clearBtn.dispatchEvent(mousedown);
    expect(mousedown.defaultPrevented).toBe(true);
    clearBtn.click();
    await el.updateComplete;

    expect(el.value).toBe('');
    expect(commits).toHaveLength(1);
    expect(commits[0].value).toBe('');
    expect(commits[0].serverQ).toBe('');
    expect(el.shadowRoot?.activeElement).toBe(input);
  });

  it('accepting a value autocomplete suggestion keeps editing without premature commit', async () => {
    const el = await mount();
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.focus();
    input.value = 'phase:run';
    input.setSelectionRange(9, 9);
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    const opt = el.shadowRoot?.querySelector('[role="option"]') as HTMLElement | null;
    expect(opt).not.toBeNull();
    opt!.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));
    await el.updateComplete;

    expect(el.value).toBe('phase:running');
    expect(commits).toHaveLength(0);
    expect(el.shadowRoot?.activeElement).toBe(input);

    // Explicit Enter afterwards commits the composed query.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await el.updateComplete;
    expect(commits).toHaveLength(1);
    expect(commits[0].serverQ).toBe('phase:running');
  });

  it('preserves cursor position when editing in the middle of the query', async () => {
    const el = await mount('phase:running');
    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.focus();
    input.value = 'phxase:running';
    input.setSelectionRange(3, 3);
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    expect(el.shadowRoot?.activeElement).toBe(input);
    expect(input.selectionStart).toBe(3);
    expect(input.selectionEnd).toBe(3);
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

    expect(commits).toHaveLength(0);
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await el.updateComplete;
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

  it('does not commit when a subsequent edit makes the query invalid', async () => {
    const el = await mount();
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'phase:running';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    input.value = 'phase:running label:';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    input.dispatchEvent(new FocusEvent('blur', { relatedTarget: document.body }));
    await el.updateComplete;
    expect(commits).toHaveLength(0);
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

  it('resets dirty state when value is updated externally so blur does not re-commit', async () => {
    const el = await mount();
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'foo';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    // Simulate a chip click updating `.value` externally.
    el.value = 'phase:running foo';
    await el.updateComplete;

    input.dispatchEvent(new FocusEvent('blur', { relatedTarget: document.body }));
    await el.updateComplete;
    expect(commits).toHaveLength(0);
  });

  it('renders {value, help} object suggestions and preserves duplicate labels with distinct hints', async () => {
    const el = await mount();
    el.view = 'decisions';
    el.valueSuggestions = (key) => {
      if (key === 'agent') {
        return [
          { value: 'deploy-frontend' },
          { value: 'deploy-frontend', help: 'deleted · 3f9c' },
          'db-ops',
        ];
      }
      if (key === 'conversation') {
        return [{ value: 'DM with deploy', help: 'Direct message' }];
      }
      return [];
    };
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'agent:dep';
    input.setSelectionRange(9, 9);
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    const options = Array.from(el.shadowRoot?.querySelectorAll('[role="option"]') ?? []);
    expect(options).toHaveLength(2);
    expect(options[0].querySelector('.option-label')?.textContent).toBe('deploy-frontend');
    expect(options[0].querySelector('.option-help')).toBeNull();
    expect(options[1].querySelector('.option-label')?.textContent).toBe('deploy-frontend');
    expect(options[1].querySelector('.option-help')?.textContent).toBe('deleted · 3f9c');

    // Values with spaces are quoted when inserted.
    input.value = 'conversation:dm';
    input.setSelectionRange(15, 15);
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    const convOpt = el.shadowRoot?.querySelector('[role="option"]') as HTMLElement | null;
    expect(convOpt?.querySelector('.option-help')?.textContent).toBe('Direct message');
    convOpt?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));
    await el.updateComplete;
    expect(el.value).toBe('conversation:"DM with deploy"');
  });

  it('displays externalWarning as a non-blocking warning when no error or schema warning exists', async () => {
    const el = await mount('by:me');
    el.view = 'decisions';
    el.externalWarning = 'by: applies to the Made tab only';
    await el.updateComplete;

    const feedback = el.shadowRoot?.querySelector('.feedback');
    expect(feedback?.classList.contains('warning')).toBe(true);
    expect(feedback?.textContent).toContain('by: applies to the Made tab only');
  });

  it('debounces remoteValueSuggestions by 250ms, dims local results with Searching… and aria-busy, then replaces with remote results and footer', async () => {
    vi.useFakeTimers();
    const el = await mount();
    el.view = 'decisions';
    el.valueSuggestions = () => [{ value: 'deploy-local' }];

    let resolveRemote:
      | ((val: { values: Array<{ value: string; help?: string }>; total: number }) => void)
      | null = null;
    const provider = vi.fn(
      () =>
        new Promise<{ values: Array<{ value: string; help?: string }>; total: number }>(
          (resolve) => {
            resolveRemote = resolve;
          }
        )
    );
    el.remoteValueSuggestions = provider;
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'agent:dep';
    input.setSelectionRange(9, 9);
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    // Local results appear immediately; remote not called at 249ms, called at 250ms.
    expect(provider).not.toHaveBeenCalled();
    expect(el.shadowRoot?.querySelectorAll('[role="option"]')).toHaveLength(1);
    expect(el.shadowRoot?.querySelector('[data-remote-status="loading"]')).toBeNull();

    vi.advanceTimersByTime(249);
    await el.updateComplete;
    expect(provider).not.toHaveBeenCalled();

    vi.advanceTimersByTime(1);
    await el.updateComplete;

    expect(provider).toHaveBeenCalledTimes(1);
    expect(provider).toHaveBeenCalledWith('agent', 'dep', expect.any(AbortSignal));
    const listbox = el.shadowRoot?.querySelector('[role="listbox"]');
    expect(listbox?.getAttribute('aria-busy')).toBe('true');
    expect(el.shadowRoot?.querySelector('[data-remote-status="loading"]')?.textContent).toContain(
      'Searching…'
    );
    expect(el.shadowRoot?.querySelector('[role="option"]')?.classList.contains('dim')).toBe(true);

    // Resolve with remote results + total > shown.
    resolveRemote!({
      values: [{ value: 'deploy-frontend' }, { value: 'deploy-frontend', help: 'deleted · 3f9c' }],
      total: 143,
    });
    await Promise.resolve();
    await el.updateComplete;

    expect(listbox?.getAttribute('aria-busy')).toBeNull();
    const opts = Array.from(el.shadowRoot?.querySelectorAll('[role="option"]') ?? []);
    expect(opts).toHaveLength(2);
    expect(opts[0].classList.contains('dim')).toBe(false);
    expect(opts[0].querySelector('.option-label')?.textContent).toBe('deploy-frontend');
    expect(opts[1].querySelector('.option-help')?.textContent).toBe('deleted · 3f9c');
    const footer = el.shadowRoot?.querySelector('[data-remote-footer]');
    expect(footer?.textContent).toContain('Showing 2 of 143 · keep typing');
    expect(listbox?.contains(footer ?? null)).toBe(false);
    expect(el.shadowRoot?.querySelector('[role="listbox"] [data-remote-footer]')).toBeNull();
    expect(el.shadowRoot?.querySelector('[role="status"]')?.textContent?.trim()).toBe(
      '143 suggestions'
    );
  });

  it('aborts in-flight remoteValueSuggestions request when user types again', async () => {
    vi.useFakeTimers();
    const el = await mount();
    el.view = 'decisions';

    const signals: AbortSignal[] = [];
    const resolvers: Array<(v: Array<{ value: string }>) => void> = [];
    el.remoteValueSuggestions = (_key, _prefix, signal) => {
      signals.push(signal);
      return new Promise((resolve) => {
        resolvers.push(resolve);
      });
    };
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'agent:de';
    input.setSelectionRange(8, 8);
    input.dispatchEvent(new Event('input'));
    vi.advanceTimersByTime(250);
    await el.updateComplete;
    expect(signals).toHaveLength(1);
    expect(signals[0].aborted).toBe(false);

    // Typing again aborts the first request.
    input.value = 'agent:dep';
    input.setSelectionRange(9, 9);
    input.dispatchEvent(new Event('input'));
    expect(signals[0].aborted).toBe(true);

    vi.advanceTimersByTime(250);
    await el.updateComplete;
    expect(signals).toHaveLength(2);
    expect(signals[1].aborted).toBe(false);

    // Fresh request resolves first, then late stale request resolves and must not overwrite.
    resolvers[1]([{ value: 'fresh-agent' }]);
    await Promise.resolve();
    await el.updateComplete;
    resolvers[0]([{ value: 'stale-agent' }]);
    await Promise.resolve();
    await el.updateComplete;

    const labels = Array.from(el.shadowRoot?.querySelectorAll('.option-label') ?? []).map(
      (n) => n.textContent
    );
    expect(labels).toEqual(['fresh-agent']);
  });

  it('cancels remote suggestions and keeps listbox closed when host changes value externally', async () => {
    vi.useFakeTimers();
    const el = await mount();
    el.view = 'decisions';

    let resolveRemote: ((v: Array<{ value: string }>) => void) | null = null;
    let capturedSignal: AbortSignal | null = null;
    el.remoteValueSuggestions = (_key, _prefix, signal) => {
      capturedSignal = signal;
      return new Promise((resolve) => {
        resolveRemote = resolve;
      });
    };
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'agent:de';
    input.setSelectionRange(8, 8);
    input.dispatchEvent(new Event('input'));
    vi.advanceTimersByTime(250);
    await el.updateComplete;
    expect(capturedSignal).not.toBeNull();
    expect(capturedSignal!.aborted).toBe(false);

    // Host updates value externally (e.g. Clear filters or URL navigation).
    el.value = 'by:me';
    await el.updateComplete;
    expect(capturedSignal!.aborted).toBe(true);
    expect(el.shadowRoot?.querySelector('[role="listbox"]')).toBeNull();

    // Resolving the cancelled promise must not reopen the listbox.
    resolveRemote!([{ value: 'deploy-frontend' }]);
    await Promise.resolve();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[role="listbox"]')).toBeNull();
    expect(el.shadowRoot?.querySelector('[role="status"]')?.textContent?.trim() ?? '').toBe('');
  });

  it('clears liveAnnouncement between remote queries so consecutive same-count results re-announce', async () => {
    vi.useFakeTimers();
    const el = await mount();
    el.view = 'decisions';
    el.remoteValueSuggestions = (_key, prefix) =>
      Promise.resolve({ values: [{ value: `${prefix}-agent` }], total: 1 });
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'agent:alpha';
    input.setSelectionRange(11, 11);
    input.dispatchEvent(new Event('input'));
    vi.advanceTimersByTime(250);
    await Promise.resolve();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[role="status"]')?.textContent?.trim()).toBe(
      '1 suggestion'
    );

    // Starting the next remote query clears the live region before the next same-count result arrives.
    input.value = 'agent:beta';
    input.setSelectionRange(10, 10);
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[role="status"]')?.textContent?.trim()).toBe('');

    vi.advanceTimersByTime(250);
    await Promise.resolve();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[role="status"]')?.textContent?.trim()).toBe(
      '1 suggestion'
    );
  });

  it('skips remote loading when remoteValueSuggestions returns null', async () => {
    vi.useFakeTimers();
    const el = await mount();
    el.view = 'decisions';
    el.valueSuggestions = () => [{ value: 'deploy-local' }];
    const provider = vi.fn(() => null);
    el.remoteValueSuggestions = provider;
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'agent:dep';
    input.setSelectionRange(9, 9);
    input.dispatchEvent(new Event('input'));
    vi.advanceTimersByTime(250);
    await el.updateComplete;

    expect(provider).toHaveBeenCalledTimes(1);
    expect(el.shadowRoot?.querySelector('[data-remote-status]')).toBeNull();
    expect(el.shadowRoot?.querySelectorAll('[role="option"]')).toHaveLength(1);
  });

  it('shows no-match state and error fallback without blocking typed commit', async () => {
    vi.useFakeTimers();
    const el = await mount();
    el.view = 'decisions';
    const commits: FilterCommitDetail[] = [];
    el.addEventListener('filter-commit', (e) => commits.push((e as CustomEvent).detail));

    // Empty match state (provider supplies plural noun via emptyLabel).
    el.remoteValueSuggestions = () =>
      Promise.resolve({ values: [], total: 0, emptyLabel: 'agents' });
    await el.updateComplete;
    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'agent:zzq';
    input.setSelectionRange(9, 9);
    input.dispatchEvent(new Event('input'));
    vi.advanceTimersByTime(250);
    await Promise.resolve();
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-remote-status="empty"]')?.textContent).toContain(
      'No agents match ‘zzq’'
    );

    // Error fallback state.
    el.remoteValueSuggestions = () => Promise.reject(new Error('network down'));
    input.value = 'agent:offline-bot';
    input.setSelectionRange(17, 17);
    input.dispatchEvent(new Event('input'));
    vi.advanceTimersByTime(250);
    await Promise.resolve();
    await Promise.resolve();
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('[data-remote-status="error"]')?.textContent).toContain(
      "Couldn't load suggestions. You can still type a value."
    );

    // Pressing Enter still commits the typed query.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await el.updateComplete;
    expect(commits.some((c) => c.serverQ === 'agent:offline-bot')).toBe(true);
  });

  it('cancels pending remote suggestions on plain Enter before debounce and while in flight', async () => {
    vi.useFakeTimers();
    const el = await mount();
    el.view = 'decisions';

    // Scenario (a): press Enter before the 250ms debounce fires -> no provider call, listbox stays closed.
    const providerA = vi.fn(() =>
      Promise.resolve({ values: [{ value: 'deploy-frontend' }], total: 1 })
    );
    el.remoteValueSuggestions = providerA;
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'agent:dep';
    input.setSelectionRange(9, 9);
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[role="listbox"]')).toBeNull();

    vi.advanceTimersByTime(300);
    await Promise.resolve();
    await el.updateComplete;
    expect(providerA).not.toHaveBeenCalled();
    expect(el.shadowRoot?.querySelector('[role="listbox"]')).toBeNull();

    // Scenario (b): press Enter while remote promise is pending -> aborted, listbox stays closed,
    // no aria-busy, and live region is not updated when the promise resolves.
    let resolveB: ((v: { values: Array<{ value: string }>; total: number }) => void) | null = null;
    let signalB: AbortSignal | null = null;
    el.remoteValueSuggestions = (_key, _prefix, signal) => {
      signalB = signal;
      return new Promise((resolve) => {
        resolveB = resolve;
      });
    };
    await el.updateComplete;

    input.value = 'agent:deploy';
    input.setSelectionRange(12, 12);
    input.dispatchEvent(new Event('input'));
    vi.advanceTimersByTime(250);
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[role="listbox"]')?.getAttribute('aria-busy')).toBe(
      'true'
    );

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await el.updateComplete;
    expect(signalB!.aborted).toBe(true);
    expect(el.shadowRoot?.querySelector('[role="listbox"]')).toBeNull();

    resolveB!({ values: [{ value: 'deploy-frontend' }], total: 1 });
    await Promise.resolve();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[role="listbox"]')).toBeNull();
    expect(el.shadowRoot?.querySelector('[role="status"]')?.textContent?.trim()).toBe('');
  });

  it('preserves active highlight across remote replacement when same value+help remains', async () => {
    vi.useFakeTimers();
    const el = await mount();
    el.view = 'decisions';
    el.valueSuggestions = () => [
      { value: 'deploy-alpha' },
      { value: 'deploy-frontend', help: 'deleted · 3f9c' },
    ];
    let resolveRemote:
      | ((v: { values: Array<{ value: string; help?: string }>; total: number }) => void)
      | null = null;
    el.remoteValueSuggestions = () =>
      new Promise((resolve) => {
        resolveRemote = resolve;
      });
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'agent:dep';
    input.setSelectionRange(9, 9);
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    // Arrow down twice to highlight "deploy-frontend" (deleted · 3f9c).
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;

    vi.advanceTimersByTime(250);
    await el.updateComplete;

    // Remote results place the highlighted item at index 0 -> highlight follows it.
    resolveRemote!({
      values: [{ value: 'deploy-frontend', help: 'deleted · 3f9c' }, { value: 'deploy-zeta' }],
      total: 2,
    });
    await Promise.resolve();
    await el.updateComplete;

    const opts = Array.from(el.shadowRoot?.querySelectorAll('[role="option"]') ?? []);
    expect(opts[0].classList.contains('active')).toBe(true);
    expect(opts[1].classList.contains('active')).toBe(false);
  });

  it('strips a leading unmatched double quote from prefix for both local and remote suggestions', async () => {
    vi.useFakeTimers();
    const el = await mount();
    el.view = 'decisions';
    el.valueSuggestions = (key) => (key === 'by' ? [{ value: 'Sam Taylor' }] : []);
    const remoteProvider = vi.fn(() =>
      Promise.resolve({ values: [{ value: 'Sam Taylor', help: 'sam@example.com' }], total: 1 })
    );
    el.remoteValueSuggestions = remoteProvider;
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'by:"Sam';
    input.setSelectionRange(7, 7);
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    // Local suggestion matches despite the leading unmatched quote.
    expect(el.shadowRoot?.querySelector('.option-label')?.textContent).toBe('Sam Taylor');

    // Remote provider receives unquoted prefix "Sam".
    vi.advanceTimersByTime(250);
    await Promise.resolve();
    await el.updateComplete;
    expect(remoteProvider).toHaveBeenCalledWith('by', 'Sam', expect.any(AbortSignal));

    // Accepting replaces `"Sam` with `"Sam Taylor"` cleanly.
    const opt = el.shadowRoot?.querySelector('[role="option"]') as HTMLElement;
    opt.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));
    await el.updateComplete;
    expect(el.value).toBe('by:"Sam Taylor"');
  });
});
