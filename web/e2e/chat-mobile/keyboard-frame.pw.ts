// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * The chat column fits the frame the iOS keyboard leaves, with a long draft.
 *
 * Chromium's visual viewport never falls short of the layout viewport, so
 * these tests replace `window.visualViewport` before the app loads with one
 * whose height they control, and fire its `resize` as iOS would when the
 * keyboard opens. That drives the real `client/viewport.ts` (the frame
 * height, and the short and tight frame states), the chat shell's top-bar
 * hiding, and the thread's measured room for the composer's field.
 *
 * In every frame, with a 30-line draft: the composer and its field stay
 * inside the frame (so the caret never sits below it and iOS has nothing to
 * pan to), the conversation header stays visible above the composer, part
 * of the message list stays visible, and the draft scrolls inside the field.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import { GENERAL_THREAD_ID } from './mock-api.js';

/** A draft far taller than any frame. */
const LONG_DRAFT = Array.from({ length: 30 }, (_, i) => `line ${i + 1}`).join('\n');

type KeyboardWindow = Window & { __setTestKeyboard: (frameHeight: number | null) => void };

/**
 * Replace `window.visualViewport` with a controllable one, before any app
 * script runs. `__setTestKeyboard(h)` opens a keyboard that leaves `h` px;
 * `__setTestKeyboard(null)` closes it.
 */
async function installFakeKeyboard(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const vv = Object.assign(new EventTarget(), {
      scale: 1,
      offsetTop: 0,
      offsetLeft: 0,
      pageTop: 0,
      pageLeft: 0,
      width: window.innerWidth,
      height: window.innerHeight,
    });
    Object.defineProperty(window, 'visualViewport', { configurable: true, get: () => vv });
    (window as unknown as KeyboardWindow).__setTestKeyboard = (frameHeight): void => {
      vv.height = frameHeight ?? window.innerHeight;
      vv.dispatchEvent(new Event('resize'));
    };
  });
}

/** Make `CSS.supports('field-sizing', …)` false, so the composer uses Shoelace's JS autosize. */
async function withoutFieldSizing(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const supports = CSS.supports.bind(CSS);
    CSS.supports = ((...args: [string, string?]) =>
      String(args[0]).includes('field-sizing')
        ? false
        : supports(...(args as [string, string]))) as typeof CSS.supports;
  });
}

/**
 * Drop the composer's `@supports (height: 1lh)` rule, as an engine without
 * the lh unit would, leaving only the declarations before it.
 */
async function dropLhRule(page: Page): Promise<void> {
  const dropped = await page.evaluate(() => {
    const find = (root: ParentNode): Element | null => {
      const hit = root.querySelector('scion-chat-composer');
      if (hit) return hit;
      for (const el of root.querySelectorAll('*')) {
        const nested = el.shadowRoot ? find(el.shadowRoot) : null;
        if (nested) return nested;
      }
      return null;
    };
    const root = find(document)!.shadowRoot!;
    let count = 0;
    const walk = (rules: CSSRuleList): void => {
      for (let i = rules.length - 1; i >= 0; i--) {
        const rule = rules[i];
        if (rule instanceof CSSSupportsRule && rule.conditionText.includes('lh')) {
          (rule.parentRule as CSSGroupingRule | null)?.deleteRule(i);
          count++;
        } else if (rule instanceof CSSGroupingRule) {
          walk(rule.cssRules);
        }
      }
    };
    for (const sheet of root.adoptedStyleSheets) walk(sheet.cssRules);
    return count;
  });
  expect(dropped, 'the lh rule was found and dropped').toBe(1);
}

async function setKeyboard(page: Page, frameHeight: number | null): Promise<void> {
  await page.evaluate(
    (h) => (window as unknown as KeyboardWindow).__setTestKeyboard(h),
    frameHeight
  );
  const want = frameHeight === null ? '' : `${frameHeight}px`;
  await expect
    .poll(() =>
      page.evaluate(() => document.documentElement.style.getPropertyValue('--scion-app-height'))
    )
    .toBe(want);
  // Let the layout, the measured room and the field's autosize settle.
  await page.waitForTimeout(300);
}

/**
 * Focus the composer's field with a real focus change (from a blurred
 * state), as a tap would.
 */
async function focusComposer(page: Page): Promise<void> {
  await page.evaluate(() => {
    let el: Element | null = document.activeElement;
    while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
    (el as HTMLElement | null)?.blur();
  });
  await page.locator('scion-chat-composer textarea').click();
}

/** Boxes of the chat column's parts, in CSS px. */
interface Box {
  frameBottom: number;
  appHeaderHeight: number;
  threadHeaderBottom: number;
  messagesHeight: number;
  /** Top of the message list: below the headers and any row above it. */
  listTop: number;
  /**
   * The least the composer can be: its input row plus its own padding and
   * border. Below the list there must be at least this much for the field
   * and Send to fit at all.
   */
  composerMin: number;
  composerTop: number;
  composerBottom: number;
  fieldTop: number;
  fieldBottom: number;
  fieldHeight: number;
  fieldScrollHeight: number;
  fieldClientHeight: number;
  fieldOverflowY: string;
  /** The Send button's box: with the field, what must stay on screen. */
  sendTop: number;
  sendBottom: number;
  chipHeight: number;
  footerHeight: number;
  scrollY: number;
}

/**
 * Measure the chat column (piercing shadow roots). `hostSelector` picks the
 * chat thread's host when there is more than one.
 */
async function measure(page: Page, threadHost = 'scion-page-chat'): Promise<Box> {
  return page.evaluate((threadHost): Box => {
    const find = (root: ParentNode, selector: string): Element | null => {
      const hit = root.querySelector(selector);
      if (hit) return hit;
      for (const el of root.querySelectorAll('*')) {
        if (el.shadowRoot) {
          const nested = find(el.shadowRoot, selector);
          if (nested) return nested;
        }
      }
      return null;
    };
    const scope = document.querySelector(threadHost)!;
    const thread = find(scope.shadowRoot ?? scope, 'scion-chat-thread')!.shadowRoot!;
    const composerEl = thread.querySelector('scion-chat-composer')!;
    const composer = composerEl.shadowRoot!;
    const textarea = composer
      .querySelector('sl-textarea')!
      .shadowRoot!.querySelector('textarea') as HTMLTextAreaElement;
    // A part hidden by the tight frame (out of flow, visibility: hidden)
    // counts as 0, like one that is not rendered.
    const inComposer = (selector: string): DOMRect | null => {
      const el = composer.querySelector(selector);
      if (!el || getComputedStyle(el).visibility === 'hidden') return null;
      return el.getBoundingClientRect();
    };
    const field = textarea.getBoundingClientRect();
    const frame = document.documentElement.style.getPropertyValue('--scion-app-height');
    return {
      frameBottom: frame ? parseFloat(frame) : window.innerHeight,
      appHeaderHeight: find(document, 'scion-header')?.getBoundingClientRect().height ?? 0,
      threadHeaderBottom: find(document, '.v2-thread-header')!.getBoundingClientRect().bottom,
      messagesHeight: thread.querySelector('.messages-scroll, .state-msg')!.getBoundingClientRect()
        .height,
      listTop: thread.querySelector('.messages-scroll, .state-msg')!.getBoundingClientRect().top,
      composerMin: ((): number => {
        const column = composer.querySelector('.composer')!;
        const cs = getComputedStyle(column);
        return (
          composer.querySelector('.input-row')!.getBoundingClientRect().height +
          parseFloat(cs.paddingTop) +
          parseFloat(cs.paddingBottom) +
          parseFloat(cs.borderTopWidth)
        );
      })(),
      composerTop: composerEl.getBoundingClientRect().top,
      composerBottom: composerEl.getBoundingClientRect().bottom,
      fieldTop: field.top,
      fieldBottom: field.bottom,
      fieldHeight: field.height,
      fieldScrollHeight: textarea.scrollHeight,
      fieldClientHeight: textarea.clientHeight,
      fieldOverflowY: getComputedStyle(textarea).overflowY,
      chipHeight: inComposer('.destination-chip')?.height ?? 0,
      footerHeight: inComposer('.footer-row')?.height ?? 0,
      sendTop: composer.querySelector('.send-btn')!.getBoundingClientRect().top,
      sendBottom: composer.querySelector('.send-btn')!.getBoundingClientRect().bottom,
      scrollY: window.scrollY,
    };
  }, threadHost);
}

/**
 * The field (caret) and Send are inside the frame, wherever that is
 * physically possible: when the frame below the list's top holds at least
 * the composer's minimum. Returns whether it was possible.
 */
function expectFieldAndSendFit(box: Box, frame: number, label = ''): boolean {
  if (frame - box.listTop < box.composerMin) {
    // Impossible: the rows above the list leave no room. The field's top
    // must still be on screen.
    expect(box.fieldTop, `${label}field top on screen`).toBeLessThan(frame);
    return false;
  }
  expect(box.fieldBottom, `${label}field (caret) inside the frame`).toBeLessThanOrEqual(frame);
  expect(box.sendBottom, `${label}Send inside the frame`).toBeLessThanOrEqual(frame);
  expect(box.fieldTop, `${label}field below the list's top`).toBeGreaterThanOrEqual(
    box.listTop - 0.5
  );
  return true;
}

/** What must hold in any keyboard frame with a long draft. */
function expectColumnFits(box: Box, frame: number): void {
  expect(box.frameBottom, 'the frame follows the keyboard').toBeCloseTo(frame, 0);
  expect(box.composerBottom, 'composer inside the frame').toBeLessThanOrEqual(frame);
  expect(box.fieldBottom, 'text field inside the frame').toBeLessThanOrEqual(frame);
  expect(box.sendBottom, 'Send inside the frame').toBeLessThanOrEqual(frame);
  expect(box.composerTop, 'composer below the conversation header').toBeGreaterThanOrEqual(
    box.threadHeaderBottom - 0.5
  );
  expect(box.messagesHeight, 'part of the message list visible').toBeGreaterThan(0);
  expect(box.fieldScrollHeight, 'the draft scrolls inside the field').toBeGreaterThan(
    box.fieldClientHeight
  );
  expect(box.scrollY, 'the page itself never scrolls').toBe(0);
}

/**
 * The chrome that can show around the field, one name per combination:
 * 'none', or parts joined with '+':
 * - around the composer: 'reply', 'edit', 'error' (a send error),
 *   'attachment' or '3 attachments';
 * - above the list: 'toggle' (an agent DM's agent-agent toggle bar) or
 *   'row50' (a synthetic 50px row, standing for any bar or banner);
 * - 'typing': a typing indicator that arrives once the draft is typed.
 */
const WORST_CASE = 'reply+error+3 attachments';

const CHROME_COMBOS = [
  'none',
  'reply',
  'edit',
  'error',
  'attachment',
  '3 attachments',
  'reply+attachment',
  'error+attachment',
  'typing',
  'toggle',
  'row50',
  'toggle+reply+typing',
] as const;
type ChromeCombo = (typeof CHROME_COMBOS)[number] | typeof WORST_CASE | 'reply+error+attachment';

const LONG_SEND_ERROR =
  'Failed to send message: the server refused the request because the conversation is archived';

const AGENT_DM_KEY = 'dm:agent:agent-coder-one:user:user-ada';

/** Set a reply on the conversation's composer, as "Reply" in the message sheet does. */
async function startReply(page: Page): Promise<void> {
  await applyChrome(page, 'reply');
}

/**
 * Turn the open thread into an agent DM with agent-agent messages, so its
 * real toggle bar shows above the list.
 */
async function showAgentDmToggleBar(page: Page): Promise<void> {
  const base = Date.parse('2026-09-01T00:00:00Z');
  const message = (i: number, recipient: string): Record<string, unknown> => ({
    id: `dm-msg-${recipient}-${i}`,
    sender: i % 2 === 0 ? 'agent' : 'user',
    senderId: i % 2 === 0 ? 'agent-coder-one' : 'user-ada',
    recipient,
    recipientId: recipient === 'agent' ? 'agent-reviewer' : 'user-ada',
    msg: `A message in the agent DM, number ${i + 1}, long enough to wrap a little`,
    type: 'chat',
    agentId: 'agent-coder-one',
    createdAt: new Date(base + i * 60_000).toISOString(),
  });
  const messages = Array.from({ length: 30 }, (_, i) => message(i, 'user'));
  await page.route(/\/conversations\/dm%3Aagent[^/]*\/messages/, (route) =>
    route.fulfill({ json: { items: messages, messages } })
  );
  await page.route(/\/conversations\/dm%3Aagent[^/]*\/interagent/, (route) =>
    route.fulfill({ json: { messages: [message(5, 'agent')] } })
  );
  await page.evaluate((key) => {
    const find = (root: ParentNode): Element | null => {
      const hit = root.querySelector('scion-chat-thread');
      if (hit) return hit;
      for (const el of root.querySelectorAll('*')) {
        const nested = el.shadowRoot ? find(el.shadowRoot) : null;
        if (nested) return nested;
      }
      return null;
    };
    const thread = find(document) as HTMLElement & { isDM: boolean; conversationKey: string };
    thread.isDM = true;
    thread.conversationKey = key;
  }, AGENT_DM_KEY);
  await expect(page.locator('scion-chat-thread .interagent-toggle-bar')).toBeVisible();
  await expect(page.locator('scion-chat-thread .messages-scroll')).toBeVisible();
}

/** Show the chrome named by `combo`, apart from 'typing' (see addTyping). */
async function applyChrome(page: Page, combo: ChromeCombo): Promise<void> {
  if (combo === 'none') return;
  const parts = combo.split('+');
  if (parts.includes('toggle')) await showAgentDmToggleBar(page);
  await page.evaluate(
    async ({ parts, error }) => {
      const find = (root: ParentNode, selector: string): Element | null => {
        const hit = root.querySelector(selector);
        if (hit) return hit;
        for (const el of root.querySelectorAll('*')) {
          const nested = el.shadowRoot ? find(el.shadowRoot, selector) : null;
          if (nested) return nested;
        }
        return null;
      };
      type Updatable = HTMLElement & { updateComplete: Promise<unknown> } & Record<string, unknown>;
      const thread = find(document, 'scion-chat-thread') as Updatable;
      const composer = thread.shadowRoot!.querySelector(
        'scion-chat-composer'
      ) as unknown as Updatable;
      for (const part of parts) {
        if (part === 'reply') {
          composer['replyTo'] = {
            messageId: 'm-1',
            senderName: 'artifact-arch',
            content: 'A fairly long message being replied to, long enough to need an ellipsis',
          };
        } else if (part === 'edit') {
          composer['editMessage'] = { messageId: 'm-1', content: 'An earlier message' };
        } else if (part === 'error') {
          thread['sendError'] = error;
        } else if (part === 'attachment' || part === '3 attachments') {
          const count = part === '3 attachments' ? 3 : 1;
          composer['pendingFiles'] = Array.from({ length: count }, (_, i) => ({
            id: `f-${i}`,
            name: `design-notes-${i + 1}.txt`,
            mime: 'text/plain',
            size: 1200,
            url: `/files/f-${i}`,
          }));
        } else if (part === 'row50') {
          const list = thread.shadowRoot!.querySelector('.messages-scroll')!;
          const row = document.createElement('div');
          row.className = 'synthetic-row';
          row.style.cssText = 'height: 50px; flex: none; background: #fde68a;';
          list.before(row);
        }
      }
      await thread.updateComplete;
      await composer.updateComplete;
    },
    { parts, error: LONG_SEND_ERROR }
  );
}

/** If `combo` has 'typing', show a typing indicator now (mid-typing). */
async function addTyping(page: Page, combo: ChromeCombo): Promise<void> {
  if (!combo.split('+').includes('typing')) return;
  await page.evaluate(async () => {
    const find = (root: ParentNode): Element | null => {
      const hit = root.querySelector('scion-chat-thread');
      if (hit) return hit;
      for (const el of root.querySelectorAll('*')) {
        const nested = el.shadowRoot ? find(el.shadowRoot) : null;
        if (nested) return nested;
      }
      return null;
    };
    const thread = find(document) as HTMLElement & {
      typingUsers: Map<string, unknown>;
      updateComplete: Promise<unknown>;
    };
    thread.typingUsers = new Map([
      ['user-grace', { displayName: 'Grace Hopper', timer: setTimeout(() => {}, 600_000) }],
      ['user-ada', { displayName: 'Ada Lovelace', timer: setTimeout(() => {}, 600_000) }],
      ['user-x', { displayName: 'A third person', timer: setTimeout(() => {}, 600_000) }],
    ]);
    await thread.updateComplete;
  });
  await page.waitForTimeout(300);
}

/**
 * Open #general in a state with no message list: empty (no messages),
 * loading (the history request never answers) or error (it fails).
 */
async function openInState(page: Page, state: 'empty' | 'loading' | 'error'): Promise<void> {
  await openChatRail(page, async (p) => {
    await p.route(
      new RegExp(`/api/v1/chat/conversations/${GENERAL_THREAD_ID}/messages`),
      (route) => {
        if (state === 'empty') void route.fulfill({ json: { items: [], messages: [] } });
        else if (state === 'error') void route.fulfill({ status: 500, json: { error: 'down' } });
        // loading: never answer.
      }
    );
  });
  await openGeneralThread(page);
  const text = { empty: 'No messages yet', loading: 'Loading messages', error: 'Retry' }[state];
  await expect(page.locator('scion-chat-thread .state-msg', { hasText: text })).toBeVisible();
}

async function openThread(page: Page): Promise<void> {
  await openChatRail(page);
  await openGeneralThread(page);
}

/** Open the thread, focus the field, open the keyboard and type a long draft. */
async function typeLongDraft(page: Page, frame: number, combo: ChromeCombo = 'none'): Promise<Box> {
  await focusComposer(page);
  await setKeyboard(page, frame);
  await page.locator('scion-chat-composer textarea').fill(LONG_DRAFT);
  await page.waitForTimeout(300);
  await addTyping(page, combo);
  return measure(page);
}

/** The field's height with a one-line draft, in the current frame. */
async function oneLineFieldHeight(page: Page): Promise<number> {
  await page.locator('scion-chat-composer textarea').fill('one line');
  await page.waitForTimeout(300);
  return (await measure(page)).fieldHeight;
}

test.describe('the chat column fits the keyboard frame with a long draft', () => {
  test.beforeEach(async ({ page }, testInfo) => {
    test.skip(testInfo.project.name !== 'chromium-390', 'each case fixes its own viewport');
    await installFakeKeyboard(page);
  });

  test.describe('a 320px-wide phone in portrait', () => {
    test.use({ viewport: { width: 320, height: 568 }, isMobile: true, hasTouch: true });

    test('200px tight frame: chrome gives way to the list; field one line', async ({ page }) => {
      await openThread(page);
      await focusComposer(page);
      await setKeyboard(page, 200);
      const oneLine = await oneLineFieldHeight(page);
      const box = await typeLongDraft(page, 200);
      expectColumnFits(box, 200);
      expect(box.appHeaderHeight, 'app top bar hidden').toBe(0);
      expect(box.chipHeight, 'destination chip hidden').toBe(0);
      expect(box.footerHeight, 'footer row hidden').toBe(0);
      // What the hidden chrome frees is phased in: two lines of draft and a
      // message row from a 200px frame up.
      expect(box.fieldHeight, 'two lines of draft').toBeGreaterThanOrEqual(2 * oneLine - 18);
      expect(box.messagesHeight, 'at least one message row').toBeGreaterThanOrEqual(56);

      // Closing the keyboard brings everything back.
      await page.evaluate(() => {
        let el: Element | null = document.activeElement;
        while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
        (el as HTMLElement | null)?.blur();
      });
      await setKeyboard(page, null);
      const closed = await measure(page);
      expect(closed.appHeaderHeight).toBeGreaterThan(0);
      expect(closed.chipHeight).toBeGreaterThan(0);
      expect(closed.footerHeight).toBeGreaterThan(0);
    });

    test('200px tight frame with a reply bar: the column still fits', async ({ page }) => {
      await openThread(page);
      await startReply(page);
      const box = await typeLongDraft(page, 200);
      expectColumnFits(box, 200);
    });
  });

  test.describe('a phone in landscape', () => {
    test.use({ viewport: { width: 844, height: 390 }, isMobile: true, hasTouch: true });

    test('150px tight frame: header and composer fit; a short list row stays', async ({ page }) => {
      await openThread(page);
      await focusComposer(page);
      await setKeyboard(page, 150);
      const oneLine = await oneLineFieldHeight(page);
      const box = await typeLongDraft(page, 150);
      expectColumnFits(box, 150);
      expect(box.appHeaderHeight, 'app top bar hidden').toBe(0);
      expect(box.fieldHeight, 'one line or more').toBeGreaterThanOrEqual(oneLine - 0.5);
      expect(box.fieldHeight, 'two lines at most').toBeLessThanOrEqual(2 * oneLine - 17 + 0.5);
      expect(box.messagesHeight, 'a short message row').toBeGreaterThanOrEqual(39);
    });

    test('140px tight frame with a reply bar: the field floor is exactly one line', async ({
      page,
    }) => {
      await openThread(page);
      await focusComposer(page);
      await setKeyboard(page, 140);
      const oneLine = await oneLineFieldHeight(page);
      await startReply(page);
      const box = await typeLongDraft(page, 140);
      expect(expectFieldAndSendFit(box, 140)).toBe(true);
      expect(box.fieldHeight, 'the floor is exactly one line').toBeCloseTo(oneLine, 0);
    });

    test('150px tight frame with a reply bar and a one-line draft: it fits', async ({ page }) => {
      await openThread(page);
      await startReply(page);
      await focusComposer(page);
      await setKeyboard(page, 150);
      await oneLineFieldHeight(page);
      const box = await measure(page);
      expect(box.composerBottom, 'composer inside the frame').toBeLessThanOrEqual(150);
      expect(box.fieldBottom, 'text field inside the frame').toBeLessThanOrEqual(150);
      // The message list is the only part that gives way (it has no minimum
      // in a tight frame), so its height is the column's spare room.
      expect(box.messagesHeight, '4px or more to spare').toBeGreaterThanOrEqual(4);
      expect(box.composerTop, 'below the conversation header').toBeGreaterThanOrEqual(
        box.threadHeaderBottom - 0.5
      );
      expect(box.messagesHeight).toBeGreaterThan(0);
    });

    test('150px tight frame with a reply bar and a long draft: 4px to spare', async ({ page }) => {
      await openThread(page);
      await startReply(page);
      const box = await typeLongDraft(page, 150);
      expectColumnFits(box, 150);
      expect(box.messagesHeight, '4px or more to spare').toBeGreaterThanOrEqual(4);
    });

    test('140px tight frame with a reply bar and a long draft: field and Send fit', async ({
      page,
    }) => {
      await openThread(page);
      await startReply(page);
      const box = await typeLongDraft(page, 140);
      // The list may give up all its room here; the field and Send fit.
      expect(expectFieldAndSendFit(box, 140)).toBe(true);
    });

    test('150px tight frame with a reply bar at an 18px root font: it fits', async ({ page }) => {
      await openThread(page);
      await page.addStyleTag({ content: 'html { font-size: 18px !important; }' });
      await startReply(page);
      const box = await typeLongDraft(page, 150);
      expectColumnFits(box, 150);
    });

    test('without the lh unit the floor is still one line and the cap holds', async ({ page }) => {
      await openThread(page);
      await focusComposer(page);
      await setKeyboard(page, 140);
      const oneLine = await oneLineFieldHeight(page);
      await dropLhRule(page);
      await startReply(page);
      const box = await typeLongDraft(page, 140);
      expect(expectFieldAndSendFit(box, 140)).toBe(true);
      expect(box.fieldHeight, 'the 2.4em fallback is one line').toBeCloseTo(oneLine, 0);
    });
  });

  test.describe('a 375px phone in portrait', () => {
    test.use({ viewport: { width: 375, height: 667 }, isMobile: true, hasTouch: true });

    test('300px short frame: the top bar gives way to the list; chip and a row stay', async ({
      page,
    }) => {
      await openThread(page);
      const box = await typeLongDraft(page, 300);
      expectColumnFits(box, 300);
      expect(box.appHeaderHeight, 'app top bar hidden').toBe(0);
      expect(box.chipHeight, 'destination chip kept').toBeGreaterThan(0);
      expect(box.messagesHeight, 'at least one message row').toBeGreaterThanOrEqual(56);
      expect(box.fieldHeight, 'two lines of draft or more').toBeGreaterThanOrEqual(64);
    });

    test('300px short frame with a reply bar: a message row still shows', async ({ page }) => {
      await openThread(page);
      await startReply(page);
      const box = await typeLongDraft(page, 300);
      expectColumnFits(box, 300);
      expect(box.messagesHeight, 'at least one message row').toBeGreaterThanOrEqual(56);
    });

    test('the top bar hides only while the field has focus', async ({ page }) => {
      await openThread(page);
      await setKeyboard(page, 300);
      expect((await measure(page)).appHeaderHeight, 'kept with nothing focused').toBeGreaterThan(0);
      await focusComposer(page);
      await expect.poll(async () => (await measure(page)).appHeaderHeight).toBe(0);
      await page.evaluate(() => {
        let el: Element | null = document.activeElement;
        while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
        (el as HTMLElement | null)?.blur();
      });
      await expect.poll(async () => (await measure(page)).appHeaderHeight).toBeGreaterThan(0);
    });

    test('a quick switcher dismissed in a short frame returns focus to its button', async ({
      page,
    }) => {
      await openThread(page);
      const button = page.locator('scion-header .palette-button');
      await button.click();
      const input = page.locator('scion-quick-palette input');
      await expect(input).toBeFocused();
      // The palette's input brings up the keyboard: a short frame.
      await setKeyboard(page, 300);
      expect((await measure(page)).appHeaderHeight, 'top bar kept').toBeGreaterThan(0);
      await page.keyboard.press('Escape');
      await expect(input).toBeHidden();
      await expect(button).toBeFocused();
      await setKeyboard(page, null);
      await expect(button).toBeFocused();
    });

    test('Reply from the message sheet in a short frame: top bar hidden, list kept', async ({
      page,
    }) => {
      await openThread(page);
      await focusComposer(page);
      await setKeyboard(page, 300);
      await expect.poll(async () => (await measure(page)).appHeaderHeight).toBe(0);
      // A touch-only device: a tap on a message opens its action sheet,
      // which takes focus.
      const cdp = await page.context().newCDPSession(page);
      await cdp.send('Emulation.setEmulatedMedia', {
        features: [
          { name: 'hover', value: 'none' },
          { name: 'pointer', value: 'coarse' },
        ],
      });
      await page.locator('scion-chat-message').last().locator('.bubble-content').first().tap();
      const reply = page.locator('scion-action-sheet[open] .item', { hasText: 'Reply' });
      await expect(reply).toBeVisible();
      await reply.tap();
      await expect(page.locator('scion-action-sheet[open]')).toHaveCount(0);
      await expect(page.locator('scion-chat-composer textarea')).toBeFocused();
      await page.waitForTimeout(300);
      const box = await measure(page);
      expect(box.appHeaderHeight, 'top bar hidden while replying').toBe(0);
      expect(box.messagesHeight, 'at least one message row').toBeGreaterThanOrEqual(56);
      expect(box.composerBottom).toBeLessThanOrEqual(300);
    });

    test('the resize="auto" fallback is capped and scrolls the same way', async ({ page }) => {
      await withoutFieldSizing(page);
      await openThread(page);
      const box = await typeLongDraft(page, 300);
      expectColumnFits(box, 300);
      expect(box.fieldOverflowY, 'overrides Shoelace overflow-y: hidden').toBe('auto');
      expect(box.messagesHeight, 'at least one message row').toBeGreaterThanOrEqual(56);
    });

    for (const combo of CHROME_COMBOS) {
      test(`a taller frame never gets a shorter field (chrome: ${combo})`, async ({ page }) => {
        await openThread(page);
        await applyChrome(page, combo);
        await focusComposer(page);
        await setKeyboard(page, 420);
        await page.locator('scion-chat-composer textarea').fill(LONG_DRAFT);
        await addTyping(page, combo);
        const frames = [
          150, 180, 200, 230, 250, 270, 280, 289, 291, 300, 320, 340, 350, 359, 361, 380, 400, 420,
        ];
        let previous = 0;
        for (const frame of frames) {
          await setKeyboard(page, frame);
          const box = await measure(page);
          expectFieldAndSendFit(box, frame, `${frame}px: `);
          if (combo === 'none' && frame >= 200) {
            expect(box.fieldHeight, `two lines at ${frame}px`).toBeGreaterThanOrEqual(64);
            expect(box.messagesHeight, `a message row at ${frame}px`).toBeGreaterThanOrEqual(56);
          }
          // With a reply, edit or attachment row, the list keeps a short row
          // from 200px, and the draft has two lines once that row allows them
          // (from about 230px with a bar, 250px with an attachment row).
          if (['reply', 'edit', 'attachment'].includes(combo) && frame >= 200) {
            // Unless the field is at its one-line floor (no room for more),
            // the list keeps a short row.
            if (box.fieldHeight > 42) {
              expect(box.messagesHeight, `a short row at ${frame}px`).toBeGreaterThanOrEqual(39);
            }
            if (frame >= 250) {
              expect(box.fieldHeight, `two lines at ${frame}px`).toBeGreaterThanOrEqual(62);
            }
          }
          expect(box.fieldHeight, `field at ${frame}px`).toBeGreaterThanOrEqual(previous - 1);
          previous = box.fieldHeight;
        }
      });
    }

    test('a list pinned to the bottom stays pinned when the keyboard opens', async ({ page }) => {
      await openThread(page);
      const list = page.locator('scion-chat-thread .messages-scroll');
      const gap = (): Promise<number> =>
        list.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight);
      await expect.poll(gap).toBeLessThanOrEqual(2);
      await focusComposer(page);
      await setKeyboard(page, 300);
      expect(await gap(), 'the latest message is in view').toBeLessThanOrEqual(2);
    });

    test('a list the user scrolled up keeps its position when the keyboard opens', async ({
      page,
    }) => {
      await openThread(page);
      const list = page.locator('scion-chat-thread .messages-scroll');
      await list.evaluate((el) => {
        el.scrollTop = 200;
        el.dispatchEvent(new Event('scroll'));
      });
      await page.waitForTimeout(100);
      await focusComposer(page);
      await setKeyboard(page, 300);
      expect(await list.evaluate((el) => el.scrollTop)).toBeCloseTo(200, 0);
    });

    for (const bar of ['reply', 'edit'] as const) {
      for (const keyboard of ['down', 'up'] as const) {
        test(`the ${bar} bar's cancel button is a 44px target, keyboard ${keyboard}`, async ({
          page,
        }) => {
          await openThread(page);
          await applyChrome(page, bar);
          if (keyboard === 'up') {
            await focusComposer(page);
            await setKeyboard(page, 300);
          }
          const hits = await page.evaluate((bar) => {
            const find = (root: ParentNode): Element | null => {
              const hit = root.querySelector('scion-chat-composer');
              if (hit) return hit;
              for (const el of root.querySelectorAll('*')) {
                const nested = el.shadowRoot ? find(el.shadowRoot) : null;
                if (nested) return nested;
              }
              return null;
            };
            const root = find(document)!.shadowRoot!;
            const button = root.querySelector(`.${bar}-bar sl-icon-button`)!;
            const icon = button.shadowRoot!.querySelector('[part~="base"]')!;
            const r = button.getBoundingClientRect();
            const cx = r.left + r.width / 2;
            const cy = r.top + r.height / 2;
            const on = (x: number, y: number, target: Element): boolean => {
              const el = root.elementFromPoint(x, y);
              return !!el && (el === target || target.contains(el));
            };
            // Just inside each edge and corner of a 44px box on the centre.
            const edges = [-21, 0, 21].flatMap((dx) =>
              [-21, 0, 21].map((dy) => on(cx + dx, cy + dy, button))
            );
            const send = root.querySelector('.send-btn')!.getBoundingClientRect();
            const field = root.querySelector('sl-textarea')!.getBoundingClientRect();
            return {
              box: [r.width, r.height],
              icon: icon.getBoundingClientRect().width,
              edges,
              sendFree: on(
                send.left + send.width / 2,
                send.top + 2,
                root.querySelector('.send-btn')!
              ),
              fieldFree: on(field.left + 10, field.top + 2, root.querySelector('sl-textarea')!),
            };
          }, bar);
          expect(hits.box[0]).toBeGreaterThanOrEqual(44);
          expect(hits.box[1]).toBeGreaterThanOrEqual(44);
          expect(hits.icon, 'the icon itself keeps its size').toBeLessThan(30);
          expect(hits.edges, 'every point of the 44px box lands on it').toEqual(
            Array<boolean>(9).fill(true)
          );
          expect(hits.sendFree, 'Send is not covered').toBe(true);
          expect(hits.fieldFree, 'the field is not covered').toBe(true);
        });
      }
    }

    test('hidden chip and footer leave the tab order and the accessibility tree', async ({
      page,
    }) => {
      await openThread(page);
      await focusComposer(page);
      await setKeyboard(page, 200);
      await page.locator('scion-chat-composer textarea').fill('a draft');
      const state = await page.evaluate(() => {
        const find = (root: ParentNode): Element | null => {
          const hit = root.querySelector('scion-chat-composer');
          if (hit) return hit;
          for (const el of root.querySelectorAll('*')) {
            const nested = el.shadowRoot ? find(el.shadowRoot) : null;
            if (nested) return nested;
          }
          return null;
        };
        const root = find(document)!.shadowRoot!;
        const chip = root.querySelector<HTMLElement>('.destination-chip')!;
        const footer = root.querySelector<HTMLElement>('.footer-row')!;
        chip.setAttribute('tabindex', '0');
        chip.focus();
        return {
          chipVisibility: getComputedStyle(chip).visibility,
          footerVisibility: getComputedStyle(footer).visibility,
          chipFocusable: root.activeElement === chip,
        };
      });
      expect(state.chipVisibility).toBe('hidden');
      expect(state.footerVisibility).toBe('hidden');
      expect(state.chipFocusable, 'a hidden element cannot take focus').toBe(false);
      const snapshot = await page.locator('scion-chat-composer').ariaSnapshot();
      expect(snapshot).not.toContain('thread default');
      expect(snapshot).not.toMatch(/\d+ \/ \d+/);
    });

    for (const keyboard of ['down', 'up'] as const) {
      test(`the typing indicator shows at the end of the list, keyboard ${keyboard}`, async ({
        page,
      }) => {
        await openThread(page);
        if (keyboard === 'up') {
          await focusComposer(page);
          await setKeyboard(page, 300);
        }
        await addTyping(page, 'typing');
        const seen = await page.evaluate(() => {
          const find = (root: ParentNode, sel: string): Element | null => {
            const hit = root.querySelector(sel);
            if (hit) return hit;
            for (const el of root.querySelectorAll('*')) {
              const nested = el.shadowRoot ? find(el.shadowRoot, sel) : null;
              if (nested) return nested;
            }
            return null;
          };
          const root = find(document, 'scion-chat-thread')!.shadowRoot!;
          const list = root.querySelector('.messages-scroll')!.getBoundingClientRect();
          const indicator = root.querySelector('.typing-indicator');
          if (!indicator) return null;
          const box = indicator.getBoundingClientRect();
          const hit = root.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
          const messages = root.querySelectorAll('scion-chat-message');
          const last = messages[messages.length - 1].getBoundingClientRect();
          return {
            count: root.querySelectorAll('.typing-indicator').length,
            inMessagesList: !!indicator.parentElement?.classList.contains('messages-list'),
            inList: box.top >= list.top - 0.5 && box.bottom <= list.bottom + 0.5,
            height: box.height,
            hitsIt: !!hit && (hit === indicator || indicator.contains(hit)),
            lastBottom: last.bottom,
            indicatorTop: box.top,
            listTop: list.top,
          };
        });
        expect(seen, 'the indicator is rendered').not.toBeNull();
        expect(seen!.count, 'exactly one indicator').toBe(1);
        expect(seen!.inMessagesList, 'it is an item of the message list').toBe(true);
        expect(seen!.height, 'it has a height').toBeGreaterThan(10);
        expect(seen!.inList, 'its box is inside the list viewport').toBe(true);
        expect(seen!.hitsIt, 'a tap at its centre lands on it: nothing covers it').toBe(true);
        expect(seen!.lastBottom, 'the last message ends above it, uncovered').toBeLessThanOrEqual(
          seen!.indicatorTop + 0.5
        );
        expect(seen!.lastBottom, 'and is in view').toBeGreaterThan(seen!.listTop);
      });
    }

    for (const state of ['empty', 'loading', 'error'] as const)
      for (const keyboard of ['down', 'up'] as const) {
        test(`in the ${state} state the typing indicator shows, keyboard ${keyboard}`, async ({
          page,
        }) => {
          await openInState(page, state);
          if (keyboard === 'up') {
            await focusComposer(page);
            await setKeyboard(page, 300);
          }
          await addTyping(page, 'typing');
          const seen = await page.evaluate(() => {
            const find = (root: ParentNode, sel: string): Element | null => {
              const hit = root.querySelector(sel);
              if (hit) return hit;
              for (const el of root.querySelectorAll('*')) {
                const nested = el.shadowRoot ? find(el.shadowRoot, sel) : null;
                if (nested) return nested;
              }
              return null;
            };
            const root = find(document, 'scion-chat-thread')!.shadowRoot!;
            const indicator = root.querySelector('.typing-indicator');
            if (!indicator) return null;
            const box = indicator.getBoundingClientRect();
            const hit = root.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
            return {
              height: box.height,
              hitsIt: !!hit && (hit === indicator || indicator.contains(hit)),
            };
          });
          expect(seen, 'the indicator is rendered').not.toBeNull();
          expect(seen!.height, 'it has a height').toBeGreaterThan(10);
          expect(seen!.hitsIt, 'a tap at its centre lands on it').toBe(true);
        });
      }

    test('in an empty thread a typist arriving or leaving never moves the field', async ({
      page,
    }) => {
      await openInState(page, 'empty');
      await focusComposer(page);
      await setKeyboard(page, 300);
      await page.locator('scion-chat-composer textarea').fill(LONG_DRAFT);
      await page.waitForTimeout(300);
      const field = async (): Promise<{ height: number; caretGap: number }> =>
        page.locator('scion-chat-composer textarea').evaluate((el) => {
          const ta = el as HTMLTextAreaElement;
          return {
            height: ta.getBoundingClientRect().height,
            // How far the end of the draft (the caret's line) is below
            // the field's visible bottom.
            caretGap: ta.scrollHeight - ta.scrollTop - ta.clientHeight,
          };
        });
      const before = await field();
      expect(before.height, 'a capped, multi-line field').toBeGreaterThan(60);
      await addTyping(page, 'typing');
      await expect(page.locator('scion-chat-thread .typing-indicator')).toBeVisible();
      const typing = await field();
      expect(typing.height, 'field height kept').toBeCloseTo(before.height, 0);
      expect(typing.caretGap, 'the caret line still in view').toBeLessThanOrEqual(
        before.caretGap + 1
      );
      await page.evaluate(async () => {
        const find = (root: ParentNode): Element | null => {
          const hit = root.querySelector('scion-chat-thread');
          if (hit) return hit;
          for (const el of root.querySelectorAll('*')) {
            const nested = el.shadowRoot ? find(el.shadowRoot) : null;
            if (nested) return nested;
          }
          return null;
        };
        const thread = find(document) as HTMLElement & {
          typingUsers: Map<string, unknown>;
          updateComplete: Promise<unknown>;
        };
        thread.typingUsers = new Map();
        await thread.updateComplete;
      });
      await page.waitForTimeout(300);
      expect((await field()).height, 'field height kept when they leave').toBeCloseTo(
        before.height,
        0
      );
    });

    test('the send error re-checks its truncation on rotation', async ({ page }) => {
      const loopErrors: string[] = [];
      page.on('console', (m) => {
        if (/ResizeObserver loop/i.test(m.text())) loopErrors.push(m.text());
      });
      page.on('pageerror', (e) => {
        if (/ResizeObserver loop/i.test(e.message)) loopErrors.push(e.message);
      });
      await page.setViewportSize({ width: 667, height: 375 });
      await openThread(page);
      const error = page.locator('scion-chat-thread .send-error');
      const fitsLandscape = 'Failed to send: the conversation is archived now';
      await page.evaluate(async (text) => {
        const find = (root: ParentNode): Element | null => {
          const hit = root.querySelector('scion-chat-thread');
          if (hit) return hit;
          for (const el of root.querySelectorAll('*')) {
            const nested = el.shadowRoot ? find(el.shadowRoot) : null;
            if (nested) return nested;
          }
          return null;
        };
        const thread = find(document) as HTMLElement & {
          sendError: string;
          updateComplete: Promise<unknown>;
        };
        thread.sendError = text;
        await thread.updateComplete;
      }, fitsLandscape);
      await expect(error).toHaveJSProperty('tagName', 'DIV');
      const look = (): Promise<Record<string, string>> =>
        error.evaluate((el) => {
          const cs = getComputedStyle(el);
          return {
            appearance: cs.appearance,
            background: cs.backgroundColor,
            color: cs.color,
            padding: `${cs.paddingTop} ${cs.paddingRight} ${cs.paddingBottom} ${cs.paddingLeft}`,
            borderTop: `${cs.borderTopWidth} ${cs.borderTopStyle} ${cs.borderTopColor}`,
            borderBottom: cs.borderBottomWidth,
            radius: cs.borderTopLeftRadius,
            font: `${cs.fontSize} ${cs.fontWeight} ${cs.lineHeight} ${cs.fontFamily}`,
            align: cs.textAlign,
          };
        });
      const asDiv = await look();
      // Rotate to portrait: the line now cuts the text, so it is a button.
      await page.setViewportSize({ width: 320, height: 568 });
      await expect(error).toHaveJSProperty('tagName', 'BUTTON');
      await expect(error).toHaveAttribute('title', fitsLandscape);
      // The button looks like the plain row: no native button styling.
      expect(await look()).toEqual(asDiv);
      // And back: it fits again, so it is plain text.
      await page.setViewportSize({ width: 667, height: 375 });
      await expect(error).toHaveJSProperty('tagName', 'DIV');
      expect(loopErrors, 'no ResizeObserver loop errors').toEqual([]);
    });

    test('the measured room is written once, not on every resize', async ({ page }) => {
      await openThread(page);
      const box = await typeLongDraft(page, 300);
      expectColumnFits(box, 300);
      const writes = await page.evaluate(async () => {
        const find = (root: ParentNode): Element | null => {
          const hit = root.querySelector('scion-chat-composer');
          if (hit) return hit;
          for (const el of root.querySelectorAll('*')) {
            const nested = el.shadowRoot ? find(el.shadowRoot) : null;
            if (nested) return nested;
          }
          return null;
        };
        const composer = find(document)!;
        let count = 0;
        const observer = new MutationObserver((records) => {
          count += records.length;
        });
        observer.observe(composer, { attributes: true, attributeFilter: ['style'] });
        const textarea = composer
          .shadowRoot!.querySelector('sl-textarea')!
          .shadowRoot!.querySelector('textarea')!;
        for (let i = 0; i < 20; i++) {
          window.visualViewport!.dispatchEvent(new Event('resize'));
          window.dispatchEvent(new Event('resize'));
          textarea.value += 'x';
          textarea.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
          await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
        }
        await new Promise((r) => setTimeout(r, 300));
        observer.disconnect();
        return count;
      });
      expect(writes, 'no writes while the room is unchanged').toBe(0);
    });
  });

  test.describe('a 430px phone in portrait', () => {
    test.use({ viewport: { width: 430, height: 932 }, isMobile: true, hasTouch: true });

    test('380px frame (not short): all chrome stays; a row and 3+ lines fit', async ({ page }) => {
      await openThread(page);
      const box = await typeLongDraft(page, 380);
      expectColumnFits(box, 380);
      expect(box.appHeaderHeight, 'app top bar kept').toBeGreaterThan(0);
      expect(box.chipHeight, 'destination chip kept').toBeGreaterThan(0);
      expect(box.messagesHeight, 'at least one message row').toBeGreaterThanOrEqual(56);
      expect(box.fieldHeight, 'three lines of draft or more').toBeGreaterThanOrEqual(85);
    });
  });

  /** The field, and the composer, stay inside the frame with any chrome. */
  for (const { name, viewport, frames } of [
    { name: 'landscape', viewport: { width: 844, height: 390 }, frames: [140, 150] },
    { name: 'a 320px phone', viewport: { width: 320, height: 568 }, frames: [200] },
  ]) {
    test.describe(`fit matrix: ${name}`, () => {
      test.use({ viewport, isMobile: true, hasTouch: true });

      for (const frame of frames) {
        for (const combo of CHROME_COMBOS) {
          test(`${frame}px, chrome: ${combo}`, async ({ page }) => {
            await openThread(page);
            await applyChrome(page, combo);
            const box = await typeLongDraft(page, frame, combo);
            const possible = expectFieldAndSendFit(box, frame);
            // Only a 50px row above the list in a landscape keyboard frame
            // leaves no room for even the input row.
            expect(possible).toBe(!(combo === 'row50' && name === 'landscape'));
            expect(box.scrollY).toBe(0);
          });
        }
      }
    });
  }

  /** An empty conversation: the empty state stands where the list would. */
  for (const { name, viewport, frame } of [
    { name: 'landscape', viewport: { width: 844, height: 390 }, frame: 150 },
    { name: 'a 320px phone', viewport: { width: 320, height: 568 }, frame: 200 },
  ]) {
    test.describe(`an empty thread: ${name}`, () => {
      test.use({ viewport, isMobile: true, hasTouch: true });

      test(`${frame}px with a reply bar and typing: field and Send fit`, async ({ page }) => {
        await openChatRail(page, async (p) => {
          await p.route(
            new RegExp(`/api/v1/chat/conversations/${GENERAL_THREAD_ID}/messages`),
            (route) => route.fulfill({ json: { items: [], messages: [] } })
          );
        });
        await openGeneralThread(page);
        await expect(page.getByText('No messages yet')).toBeVisible();
        await applyChrome(page, 'reply');
        const box = await typeLongDraft(page, frame, 'typing');
        expect(expectFieldAndSendFit(box, frame)).toBe(true);
      });
    });
  }

  /**
   * Every interactive element in and just above the composer takes taps at
   * its own centre, and nothing from the context rows paints or takes taps
   * outside their wrapper: points across the send error hit the error.
   */
  for (const { name, viewport, frame } of [
    { name: 'a 320px phone, 200px tight', viewport: { width: 320, height: 568 }, frame: 200 },
    { name: 'landscape, 150px tight', viewport: { width: 844, height: 390 }, frame: 150 },
    { name: 'landscape, 140px tight', viewport: { width: 844, height: 390 }, frame: 140 },
    { name: 'a 375px phone, 300px short', viewport: { width: 375, height: 667 }, frame: 300 },
    // frame 0: the keyboard stays down (the whole 667px viewport).
    { name: 'a 375px phone, keyboard down', viewport: { width: 375, height: 667 }, frame: 0 },
  ]) {
    test.describe(`no overlapping targets: ${name}`, () => {
      test.use({ viewport, isMobile: true, hasTouch: true });

      test('with a reply bar, an error and attachments', async ({ page }) => {
        await openThread(page);
        await applyChrome(page, 'reply+error+attachment');
        if (frame > 0) {
          await typeLongDraft(page, frame);
        } else {
          await focusComposer(page);
          await page.locator('scion-chat-composer textarea').fill(LONG_DRAFT);
          await page.waitForTimeout(300);
        }
        const bottom = frame > 0 ? frame : viewport.height;
        const result = await page.evaluate((frame) => {
          const find = (root: ParentNode, sel: string): Element | null => {
            const hit = root.querySelector(sel);
            if (hit) return hit;
            for (const el of root.querySelectorAll('*')) {
              const nested = el.shadowRoot ? find(el.shadowRoot, sel) : null;
              if (nested) return nested;
            }
            return null;
          };
          const thread = find(document, 'scion-chat-thread')!.shadowRoot!;
          const composer = thread.querySelector('scion-chat-composer')!.shadowRoot!;
          const context = composer.querySelector('.composer-context')!.getBoundingClientRect();
          const report: string[] = [];
          let skipped = 0;
          // Each target takes taps at its centre, where its centre is on
          // screen and not clipped away with the rows that gave way.
          const targets = [
            ...composer.querySelectorAll(
              '.input-row sl-icon-button, .send-btn, sl-textarea, .reply-bar sl-icon-button, .edit-bar sl-icon-button, .remove-btn, .destination-chip'
            ),
          ];
          let hidden = 0;
          for (const el of targets) {
            // The destination chip is hidden (out of flow, visibility:
            // hidden) in a tight frame; it takes no taps then.
            if (getComputedStyle(el).visibility === 'hidden') {
              hidden++;
              continue;
            }
            const r = el.getBoundingClientRect();
            const x = r.left + r.width / 2;
            const y = r.top + r.height / 2;
            const inContext = !!el.closest('.composer-context');
            if ((inContext && (y < context.top || y > context.bottom)) || y > frame) {
              skipped++;
              continue;
            }
            const hit = composer.elementFromPoint(x, y);
            if (!hit || !(hit === el || el.contains(hit))) {
              report.push(
                `${el.localName}.${el.className} at ${Math.round(y)} hit ${hit?.localName}`
              );
            }
          }
          // Points across the send error hit the error.
          const error = thread.querySelector('.send-error')!;
          const e = error.getBoundingClientRect();
          for (const fx of [0.1, 0.5, 0.9]) {
            for (const fy of [0.2, 0.5, 0.8]) {
              const x = e.left + e.width * fx;
              const y = e.top + e.height * fy;
              if (e.height < 1) continue;
              const hit = thread.elementFromPoint(x, y);
              if (!hit || !(hit === error || error.contains(hit))) {
                report.push(`error at ${fx},${fy} hit ${hit?.localName}.${hit?.className}`);
              }
            }
          }
          return { report, checked: targets.length, skipped, hidden, error: e.height };
        }, bottom);
        expect(result.checked).toBeGreaterThanOrEqual(5);
        expect(result.report).toEqual([]);
        // Where the frame has room for every row (300px, and the keyboard
        // down; in the tight ones the rows above the field give way),
        // nothing is clipped: every target, the reply bar's cancel and the
        // attachment's remove button included, is visible and takes taps.
        if (frame === 0 || frame >= 300) {
          expect(result.skipped, 'targets clipped away').toBe(0);
          // And the destination chip shows there, so it was checked too.
          expect(result.hidden, 'the destination chip is shown').toBe(0);
        }
      });
    });
  }

  test.describe('a composer the frame clips', () => {
    test.use({ viewport: { width: 844, height: 390 }, isMobile: true, hasTouch: true });

    test('its clipped height counts the context rows its wrapper cuts off', async ({ page }) => {
      await openThread(page);
      await applyChrome(page, 'reply+attachment');
      const box = await typeLongDraft(page, 140);
      expect(expectFieldAndSendFit(box, 140)).toBe(true);
      const parts = await page.evaluate(() => {
        const find = (root: ParentNode): Element | null => {
          const hit = root.querySelector('scion-chat-composer');
          if (hit) return hit;
          for (const el of root.querySelectorAll('*')) {
            const nested = el.shadowRoot ? find(el.shadowRoot) : null;
            if (nested) return nested;
          }
          return null;
        };
        const host = find(document) as HTMLElement & { clippedHeight(): number };
        const root = host.shadowRoot!;
        const rows = root.querySelector('.composer-context-rows')!.getBoundingClientRect();
        const wrapper = root.querySelector('.composer-context')!.getBoundingClientRect();
        return {
          reported: host.clippedHeight(),
          rowsCut: rows.height - wrapper.height,
          hostOverflow: Math.max(0, host.scrollHeight - host.clientHeight),
        };
      });
      // In this frame the wrapper really does cut rows off.
      expect(parts.rowsCut, 'rows cut off by the wrapper').toBeGreaterThan(5);
      expect(parts.reported, 'reported clipped height').toBeCloseTo(
        parts.rowsCut + parts.hostOverflow,
        0
      );
    });
  });

  test.describe('the worst case', () => {
    test.use({ viewport: { width: 844, height: 390 }, isMobile: true, hasTouch: true });

    for (const root of [16, 18]) {
      test(`reply, error, 3 attachments and typing at 140px, ${root}px root: field and Send fit`, async ({
        page,
      }) => {
        await openThread(page);
        await page.addStyleTag({ content: `html { font-size: ${root}px !important; }` });
        await applyChrome(page, WORST_CASE);
        await typeLongDraft(page, 140, 'typing');
        const box = await measure(page);
        expect(box.fieldBottom, 'field (caret) inside the frame').toBeLessThanOrEqual(140);
        expect(box.sendBottom, 'Send inside the frame').toBeLessThanOrEqual(140);
        expect(box.fieldTop, 'field below the conversation header').toBeGreaterThanOrEqual(
          box.threadHeaderBottom
        );
      });
    }
  });

  test.describe('the send error', () => {
    test.use({ viewport: { width: 375, height: 667 }, isMobile: true, hasTouch: true });

    test('cut to one line, it expands as a button; the field and Send stay inside', async ({
      page,
    }) => {
      await openThread(page);
      await applyChrome(page, 'error+attachment');
      await typeLongDraft(page, 300);
      const error = page.locator('scion-chat-thread .send-error');
      await expect(error).toHaveJSProperty('tagName', 'BUTTON');
      await expect(error).toHaveAttribute('aria-expanded', 'false');
      await expect(error).toHaveAttribute('title', LONG_SEND_ERROR);
      const oneLine = (await error.boundingBox())!.height;
      await error.focus();
      await page.keyboard.press('Enter');
      await expect(error).toHaveAttribute('aria-expanded', 'true');
      await page.waitForTimeout(300);
      expect((await error.boundingBox())!.height, 'it really expands').toBeGreaterThan(oneLine);
      const box = await measure(page);
      expect(box.fieldBottom, 'field (caret) inside the frame').toBeLessThanOrEqual(300);
      expect(box.sendBottom, 'Send inside the frame').toBeLessThanOrEqual(300);
      expect(box.fieldTop).toBeGreaterThanOrEqual(box.threadHeaderBottom);
      await page.keyboard.press('Enter');
      await expect(error).toHaveAttribute('aria-expanded', 'false');
    });

    test('a short error that fits is plain text, not a button', async ({ page }) => {
      await openThread(page);
      await page.evaluate(async () => {
        const find = (root: ParentNode): Element | null => {
          const hit = root.querySelector('scion-chat-thread');
          if (hit) return hit;
          for (const el of root.querySelectorAll('*')) {
            const nested = el.shadowRoot ? find(el.shadowRoot) : null;
            if (nested) return nested;
          }
          return null;
        };
        const thread = find(document) as HTMLElement & {
          sendError: string;
          updateComplete: Promise<unknown>;
        };
        thread.sendError = 'Send failed';
        await thread.updateComplete;
      });
      const error = page.locator('scion-chat-thread .send-error');
      await expect(error).toHaveJSProperty('tagName', 'DIV');
      await expect(error).not.toHaveAttribute('aria-expanded', /.*/);
    });
  });

  test.describe('a thread outside the chat shell, under a visible top bar', () => {
    test.use({ viewport: { width: 844, height: 390 }, isMobile: true, hasTouch: true });

    test('in a tight frame it keeps its chrome and still fits', async ({ page }) => {
      await openThread(page);
      // Mount a second thread for the same conversation outside the chat
      // shell, under a 61px bar, the way the agent page hosts one.
      await page.evaluate(async () => {
        const find = (root: ParentNode, sel: string): Element | null => {
          const hit = root.querySelector(sel);
          if (hit) return hit;
          for (const el of root.querySelectorAll('*')) {
            const nested = el.shadowRoot ? find(el.shadowRoot, sel) : null;
            if (nested) return nested;
          }
          return null;
        };
        const source = find(document, 'scion-chat-thread') as HTMLElement & {
          conversationKey: string;
          projectId: string;
        };
        const host = document.createElement('div');
        host.id = 'outside-shell';
        host.style.cssText =
          'position: fixed; inset: 0 0 auto 0; height: var(--scion-app-height, 100dvh);' +
          'display: flex; flex-direction: column; z-index: 10000; background: #fff;';
        const bar = document.createElement('div');
        bar.style.cssText = 'height: 61px; flex: none;';
        const thread = document.createElement('scion-chat-thread') as HTMLElement & {
          conversationKey: string;
          projectId: string;
          updateComplete: Promise<unknown>;
        };
        thread.style.cssText = 'flex: 1; min-height: 0;';
        thread.projectId = source.projectId;
        thread.conversationKey = source.conversationKey;
        host.append(bar, thread);
        document.body.appendChild(host);
        await thread.updateComplete;
      });
      await page
        .locator('#outside-shell scion-chat-composer textarea')
        .waitFor({ state: 'visible' });
      await setKeyboard(page, 200);
      await page.locator('#outside-shell scion-chat-composer textarea').fill(LONG_DRAFT);
      await page.waitForTimeout(300);
      const box = await measure(page, '#outside-shell');
      expect(box.composerBottom, 'composer inside the frame').toBeLessThanOrEqual(200.5);
      expect(box.fieldBottom, 'text field inside the frame').toBeLessThanOrEqual(200.5);
      expect(box.composerTop, 'composer below the bar').toBeGreaterThanOrEqual(61);
      expect(box.footerHeight, 'no tight compaction outside the chat shell').toBeGreaterThan(0);
      expect(box.fieldScrollHeight).toBeGreaterThan(box.fieldClientHeight);
    });
  });
});

test('on desktop the reply cancel button keeps its own hit area', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop-1440', 'the desktop layout');
  await openChatRail(page);
  await openGeneralThread(page);
  await startReply(page);
  const hits = await page.evaluate(() => {
    const find = (root: ParentNode): Element | null => {
      const hit = root.querySelector('scion-chat-composer');
      if (hit) return hit;
      for (const el of root.querySelectorAll('*')) {
        const nested = el.shadowRoot ? find(el.shadowRoot) : null;
        if (nested) return nested;
      }
      return null;
    };
    const root = find(document)!.shadowRoot!;
    const button = root.querySelector('.reply-bar sl-icon-button')!;
    const r = button.getBoundingClientRect();
    const el = root.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2 - 21);
    return !!el && (el === button || button.contains(el));
  });
  expect(hits, 'no enlarged hit area on desktop').toBe(false);
});

test('on desktop the send error is plain full text, with no tab stop', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop-1440', 'the desktop layout');
  await openChatRail(page);
  await openGeneralThread(page);
  await applyChrome(page, 'error');
  const error = page.locator('scion-chat-thread .send-error');
  await expect(error).toHaveJSProperty('tagName', 'DIV');
  await expect(error).not.toHaveAttribute('aria-expanded', /.*/);
  await expect(error).toHaveText(LONG_SEND_ERROR);
  expect(await error.evaluate((el) => getComputedStyle(el).whiteSpace)).toBe('normal');
  expect(await error.evaluate((el) => (el as HTMLElement).tabIndex)).toBe(-1);
});
