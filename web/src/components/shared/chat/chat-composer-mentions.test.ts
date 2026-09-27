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
 * Tests for atomic backspace/delete over an accepted @mention (#1912).
 *
 * User report: accepting the wrong agent from autocomplete and then pressing
 * Backspace deleted the resolved `@agent-slug` one character at a time
 * instead of removing the whole token, the dropdown stayed open after
 * accept, and the textarea's displayed text lagged behind what had actually
 * been deleted.
 *
 * These tests drive the *real* `<sl-textarea>` and `<scion-mention-autocomplete>`
 * children through real DOM events (input/keydown/click), matching how a
 * user would actually interact with the composer, rather than poking private
 * state directly — the bug lived in exactly that native-event plumbing.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeAll, afterEach } from 'vitest';
import '@shoelace-style/shoelace/dist/components/textarea/textarea.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

beforeAll(async () => {
  await import('./chat-composer.js');
});

afterEach(() => {
  document.body.innerHTML = '';
});

const AGENTS = [
  { slug: 'bob', name: 'Bob' },
  { slug: 'alice', name: 'Alice' },
];

interface Harness {
  el: any;
  sl: any;
  ta: HTMLTextAreaElement;
  auto: any;
}

async function mount(): Promise<Harness> {
  const el = document.createElement('scion-chat-composer') as any;
  el.agents = AGENTS;
  document.body.appendChild(el);
  await el.updateComplete;
  const sl = el.shadowRoot.querySelector('sl-textarea');
  await sl.updateComplete;
  const ta = sl.shadowRoot.querySelector('textarea') as HTMLTextAreaElement;
  const auto = el.shadowRoot.querySelector('scion-mention-autocomplete');
  return { el, sl, ta, auto };
}

/** Simulate the browser applying a text change and firing a real input event. */
async function typeText(h: Harness, value: string, caret = value.length): Promise<void> {
  h.ta.value = value;
  h.ta.selectionStart = h.ta.selectionEnd = caret;
  h.ta.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
  await h.el.updateComplete;
  await h.sl.updateComplete;
}

/** Accept the candidate at `index` via a real mousedown+click on the dropdown item. */
async function acceptCandidate(h: Harness, index = 0): Promise<void> {
  const item = h.auto.shadowRoot.querySelectorAll('.dropdown-item')[index] as HTMLElement;
  expect(item).toBeTruthy();
  item.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, composed: true }));
  item.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
  await h.el.updateComplete;
  await h.sl.updateComplete;
}

/** Dispatch a real keydown at the given collapsed caret position. */
function pressKey(h: Harness, key: string, caret: number): KeyboardEvent {
  h.ta.selectionStart = h.ta.selectionEnd = caret;
  const event = new KeyboardEvent('keydown', {
    key,
    bubbles: true,
    composed: true,
    cancelable: true,
  });
  h.ta.dispatchEvent(event);
  return event;
}

describe('composer — atomic mention delete', () => {
  it('accept then backspace removes the whole mention, and it no longer routes', async () => {
    const h = await mount();
    await typeText(h, '@bo', 3);
    expect(h.auto.active).toBe(true);

    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('@bob ');

    const event = pressKey(h, 'Backspace', h.el.text.length);
    expect(event.defaultPrevented).toBe(true);
    await h.el.updateComplete;

    expect(h.el.text).toBe('');
    expect(h.ta.value).toBe('');

    // Type something unrelated and send — the deleted mention must not route.
    await typeText(h, 'hello team', 10);
    let detail: any;
    h.el.addEventListener('chat-send', (e: any) => (detail = e.detail));
    h.el.handleSend();

    expect(detail.text).toBe('hello team');
    expect(detail.mentions).toEqual([]);
  });

  it('accept then Backspace removes the whole mention and the dropdown does not reopen (problem 2)', async () => {
    // Regression test for the user's actual report: on base, the native
    // Backspace only eats the trailing space (`"@bob "` -> `"@bob"`), which
    // still reads a `@bob` trigger with query "bob" — so `handleInput`
    // reopens the dropdown even though the resolved mention is still fully
    // intact on screen. This must fail on base.
    const h = await mount();
    await typeText(h, '@bo', 3);
    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('@bob ');
    expect(h.auto.active).toBe(false);

    const event = pressKey(h, 'Backspace', h.el.text.length);
    expect(event.defaultPrevented).toBe(true);
    await h.el.updateComplete;

    expect(h.el.text).toBe('');
    expect(h.auto.active).toBe(false);
  });

  it('closes the autocomplete dropdown immediately after accepting (guard — also true on base)', async () => {
    const h = await mount();
    await typeText(h, '@al', 3);
    expect(h.auto.active).toBe(true);

    await acceptCandidate(h, 0);

    expect(h.auto.active).toBe(false);
    expect(h.auto.shadowRoot.querySelector('.dropdown')).toBeNull();
  });

  it('keeps the displayed value in sync with the model on every keystroke after accept (guard — also true on base)', async () => {
    const h = await mount();
    await typeText(h, '@bo', 3);
    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('@bob ');
    expect(h.ta.value).toBe('@bob ');

    // Continue typing normally after the accept — each keystroke must be
    // reflected immediately, not just on the next autocomplete pick (#1912).
    for (const value of ['@bob t', '@bob to', '@bob to ']) {
      await typeText(h, value, value.length);
      expect(h.ta.value).toBe(value);
      expect(h.el.text).toBe(value);
    }
  });

  it('leaves a plain, still-being-typed @partial to delete one character at a time', async () => {
    const h = await mount();
    await typeText(h, '@abc', 4);
    // No candidate matches "abc", so nothing was ever accepted.
    expect(h.el.text).toBe('@abc');

    const event = pressKey(h, 'Backspace', 4);
    expect(event.defaultPrevented).toBe(false);
  });

  it('Delete immediately before an accepted mention removes it atomically', async () => {
    const h = await mount();
    await typeText(h, '@bo', 3);
    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('@bob ');

    const event = pressKey(h, 'Delete', 0);
    expect(event.defaultPrevented).toBe(true);
    await h.el.updateComplete;

    expect(h.el.text).toBe('');
    expect(h.ta.value).toBe('');
  });

  it('editing text elsewhere does not break a later atomic mention delete', async () => {
    const h = await mount();
    await typeText(h, 'hi ', 3);
    await typeText(h, 'hi @bo', 6);
    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('hi @bob ');

    // Edit at the very start of the message — unrelated to the mention.
    await typeText(h, 'Xhi @bob ', 1);
    expect(h.el.text).toBe('Xhi @bob ');

    const event = pressKey(h, 'Backspace', h.el.text.length);
    expect(event.defaultPrevented).toBe(true);
    await h.el.updateComplete;

    expect(h.el.text).toBe('Xhi ');
    expect(h.ta.value).toBe('Xhi ');
  });

  it('hand-editing inside an accepted mention degrades it back to plain text', async () => {
    const h = await mount();
    await typeText(h, '@bo', 3);
    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('@bob ');

    // Select-and-replace the "b" in "bob" — an edit that lands inside the
    // tracked range, not at its edge.
    await typeText(h, '@boX ', 4);
    expect(h.el.text).toBe('@boX ');

    // The mention is no longer a clean accepted token: Backspace at the end
    // now deletes one character, not the whole thing.
    const event = pressKey(h, 'Backspace', h.el.text.length);
    expect(event.defaultPrevented).toBe(false);

    // And it must not route either, since it no longer reads "@bob".
    await typeText(h, '@boX', 4);
    let detail: any;
    h.el.addEventListener('chat-send', (e: any) => (detail = e.detail));
    h.el.handleSend();
    expect(detail.mentions).toEqual([]);
  });

  it('a genuinely new @ trigger after an atomic delete opens the dropdown (not a stale reopen)', async () => {
    const h = await mount();
    await typeText(h, '@bo', 3);
    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('@bob ');

    const event = pressKey(h, 'Backspace', h.el.text.length);
    expect(event.defaultPrevented).toBe(true);
    await h.el.updateComplete;
    expect(h.el.text).toBe('');
    expect(h.auto.active).toBe(false);

    await typeText(h, '@al', 3);
    expect(h.auto.active).toBe(true);
  });

  it('atomic delete in mid-text leaves the caret where the mention was', async () => {
    const h = await mount();
    await typeText(h, 'hi @bo', 6);
    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('hi @bob ');

    await typeText(h, 'hi @bob there', 13);
    expect(h.el.text).toBe('hi @bob there');

    // Caret right at the mention's end, immediately before "there".
    const event = pressKey(h, 'Backspace', 8);
    expect(event.defaultPrevented).toBe(true);
    await h.el.updateComplete;

    expect(h.el.text).toBe('hi there');
    expect(h.ta.selectionStart).toBe(3);
  });

  it('notifies the slash autocomplete (not just the mention autocomplete) after an atomic delete', async () => {
    const h = await mount();
    await typeText(h, '@bo', 3);
    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('@bob ');

    const slash = h.el.shadowRoot.querySelector('scion-slash-autocomplete');
    let calledWith: [string, number] | null = null;
    const original = slash.handleInput.bind(slash);
    slash.handleInput = (text: string, cursorPos: number) => {
      calledWith = [text, cursorPos];
      return original(text, cursorPos);
    };

    pressKey(h, 'Backspace', h.el.text.length);
    await h.el.updateComplete;

    expect(calledWith).not.toBeNull();
    expect((calledWith as unknown as [string, number])[0]).toBe('');
  });

  it('prepending a new mention before an already-accepted one keeps both routed (R1)', async () => {
    // Regression test for R1: a prefix/suffix diff that greedily matches the
    // "@" typed right before an accepted mention used to misplace the edit
    // inside that mention's range, silently dropping it from
    // `acceptedMentions` even though the message still visibly shows it.
    const h = await mount();
    await typeText(h, '@bo', 3);
    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('@bob ');

    await typeText(h, '@bob hi', 7);
    expect(h.el.text).toBe('@bob hi');

    // Caret to the very start, then type "@" one character at a time — this
    // is exactly the edit that used to be misdiffed as landing inside the
    // "@bob " range.
    await typeText(h, '@@bob hi', 1);
    await typeText(h, '@al@bob hi', 3);
    expect(h.auto.active).toBe(true);

    await acceptCandidate(h, 0); // only "alice" matches the "al" query
    expect(h.el.text).toBe('@alice @bob hi');

    // R2: the caret anchor that keeps the diff above from creeping into the
    // "@bob " range must come from the real textarea, not the <sl-textarea>
    // host (which has no `selectionStart`). If it silently fell back to
    // `newText.length` throughout, the anchor would never bind and the
    // reconcile above could still have dropped the "bob" range without any
    // of the assertions so far catching it. Backspace right at the edge of
    // "@bob " proves the range survived: it must be atomic, not a
    // char-by-char delete of the trailing space.
    const atomicEvent = pressKey(h, 'Backspace', 12);
    expect(atomicEvent.defaultPrevented).toBe(true);
    await h.el.updateComplete;
    expect(h.el.text).toBe('@alice hi');

    let detail: any;
    h.el.addEventListener('chat-send', (e: any) => (detail = e.detail));
    h.el.handleSend();

    expect(detail.text).toBe('@alice hi');
    expect(new Set(detail.mentions)).toEqual(new Set(['alice']));
  });

  it('atomically deleting one of two mentions of the same agent keeps the agent routed if the text still reads its slug (nit)', async () => {
    // Regression test for the round-2 review nit: `deleteMentionRange` used
    // to drop a slug from `acceptedMentions` whenever no *tracked range* for
    // it survived, even if the literal text still read `@slug` elsewhere.
    // That could de-route an agent the send-time `trimmed.includes('@slug')`
    // filter would still have routed.
    const h = await mount();
    await typeText(h, '@bo', 3);
    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('@bob ');

    await typeText(h, '@bob @bo', 8);
    await acceptCandidate(h, 0);
    expect(h.el.text).toBe('@bob @bob ');

    // Hand-edit the first token's trailing space to a comma. This lands
    // inside the first "@bob " range, degrading it back to plain text — but
    // the text still literally reads "@bob" via that remnant.
    await typeText(h, '@bob,@bob ', 5);
    expect(h.el.text).toBe('@bob,@bob ');

    // Atomically delete the second, still-tracked "@bob " token.
    const event = pressKey(h, 'Backspace', h.el.text.length);
    expect(event.defaultPrevented).toBe(true);
    await h.el.updateComplete;
    expect(h.el.text).toBe('@bob,');

    // The agent must still route: the text still reads "@bob".
    let detail: any;
    h.el.addEventListener('chat-send', (e: any) => (detail = e.detail));
    h.el.handleSend();

    expect(detail.text).toBe('@bob,');
    expect(detail.mentions).toEqual(['bob']);
  });
});
