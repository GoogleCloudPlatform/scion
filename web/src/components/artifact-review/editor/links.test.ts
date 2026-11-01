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
 * Following links in the graphical review editor (ptone/scion#6848): a
 * Mod-click (Cmd on macOS, Ctrl elsewhere) or a middle click opens a
 * followable link in a new tab, by the same rule as the preview
 * (artifactLinkTarget); a plain click only places the caret; no click
 * follows a link natively; a link the rule refuses stays text and opens
 * nothing.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const platform = vi.hoisted(() => ({ mac: false }));
vi.mock('../../../utils/platform.js', () => ({ isMacPlatform: (): boolean => platform.mac }));

import { ReviewController, followsLink, holdsLinkPress, isModPress } from './controller.js';
import { linkOpenHint } from './safe-render.js';
import { renderContext } from './__fixtures__/core.js';

const made: ReviewController[] = [];
let opened: unknown[][] = [];

beforeEach(() => {
  platform.mac = false;
  opened = [];
  vi.stubGlobal(
    'open',
    vi.fn((...args: unknown[]) => {
      opened.push(args);
      return null;
    })
  );
});

afterEach(async () => {
  for (const c of made.splice(0)) await c.destroy();
  document.body.replaceChildren();
  vi.unstubAllGlobals();
});

async function open(md: string): Promise<ReviewController> {
  const root = document.createElement('div');
  document.body.append(root);
  const c = await ReviewController.create({
    root,
    render: () => renderContext,
    askNote: () => Promise.resolve(null),
    onHint: () => {},
    onChange: () => {},
  });
  made.push(c);
  expect(c.load(md)).toBe(true);
  return c;
}

const DOC =
  'See [the site](https://example.com/page) and [notes](notes.md) and ' +
  '[bad](javascript:alert(1)) here.\n';

/** The link element whose text is text. */
function link(c: ReviewController, text: string): HTMLAnchorElement {
  const a = Array.from(c.view.dom.querySelectorAll('a')).find((x) => x.textContent === text);
  if (!a) throw new Error(`no link ${text}`);
  return a;
}

function mouse(type: 'click' | 'auxclick' | 'mousedown', init: MouseEventInit = {}): MouseEvent {
  return new MouseEvent(type, { bubbles: true, cancelable: true, button: 0, ...init });
}

/** The text node inside a link: events dispatched there bubble to the link. */
function textIn(a: HTMLAnchorElement): Node {
  const t = a.firstChild;
  if (!t || t.nodeType !== 3) throw new Error('link has no text node');
  return t;
}

function markdown(c: ReviewController): string {
  return c.flush()!;
}

describe('links in the review editor (ptone/scion#6848)', () => {
  it('renders followable links with the target the preview gives, and a refused one as text', async () => {
    const c = await open(DOC);
    const site = link(c, 'the site');
    expect(site.getAttribute('href')).toBe('https://example.com/page');
    expect(site.getAttribute('target')).toBe('_blank');
    expect(site.getAttribute('rel')).toBe('noopener noreferrer');
    expect(site.getAttribute('title')).toBe(linkOpenHint(false));
    expect(link(c, 'notes').getAttribute('href')).toBe(
      '/api/v1/artifacts/art1/versions/3/files/docs/notes.md'
    );
    const bad = link(c, 'bad');
    expect(bad.hasAttribute('href')).toBe(false);
    expect(bad.classList.contains('inert-link')).toBe(true);
  });

  it('Ctrl-click opens an allowed link once in a new tab and leaves the document alone; a refused one opens nothing', async () => {
    const c = await open(DOC);
    const before = markdown(c);
    const sel = c.view.state.selection.toJSON();

    // Positive control: the allowed link in the same document opens.
    const ok = mouse('click', { ctrlKey: true });
    textIn(link(c, 'the site')).dispatchEvent(ok);
    expect(ok.defaultPrevented).toBe(true);
    expect(opened).toEqual([['https://example.com/page', '_blank', 'noopener,noreferrer']]);

    // The refused scheme: default prevented, nothing opened.
    const refused = mouse('click', { ctrlKey: true });
    textIn(link(c, 'bad')).dispatchEvent(refused);
    expect(refused.defaultPrevented).toBe(true);
    expect(opened).toHaveLength(1);

    expect(markdown(c)).toBe(before);
    expect(c.view.state.selection.toJSON()).toEqual(sel);
  });

  it('opens a link to a file of the version at its file route', async () => {
    const c = await open(DOC);
    textIn(link(c, 'notes')).dispatchEvent(mouse('click', { ctrlKey: true }));
    expect(opened).toEqual([
      ['/api/v1/artifacts/art1/versions/3/files/docs/notes.md', '_blank', 'noopener,noreferrer'],
    ]);
  });

  it('a plain click on a link opens nothing and does not follow it natively', async () => {
    const c = await open(DOC);
    const before = markdown(c);
    const e = mouse('click');
    textIn(link(c, 'the site')).dispatchEvent(e);
    expect(e.defaultPrevented).toBe(true);
    expect(opened).toEqual([]);
    expect(markdown(c)).toBe(before);
  });

  it('a middle click opens an allowed link, and a refused one stays closed', async () => {
    const c = await open(DOC);
    textIn(link(c, 'bad')).dispatchEvent(mouse('auxclick', { button: 1 }));
    expect(opened).toEqual([]);
    textIn(link(c, 'the site')).dispatchEvent(mouse('auxclick', { button: 1 }));
    expect(opened).toEqual([['https://example.com/page', '_blank', 'noopener,noreferrer']]);
  });

  it('on macOS the gesture is Cmd-click, and Ctrl-click (the context menu there) opens nothing', async () => {
    platform.mac = true;
    const c = await open(DOC);
    expect(link(c, 'the site').getAttribute('title')).toBe(linkOpenHint(true));
    textIn(link(c, 'the site')).dispatchEvent(mouse('click', { ctrlKey: true }));
    expect(opened).toEqual([]);
    textIn(link(c, 'the site')).dispatchEvent(mouse('click', { metaKey: true }));
    expect(opened).toEqual([['https://example.com/page', '_blank', 'noopener,noreferrer']]);
  });

  it('a Mod press on a link is held back, so the caret does not move', async () => {
    const c = await open(DOC);
    const sel = c.view.state.selection.toJSON();
    const modDown = mouse('mousedown', { ctrlKey: true });
    textIn(link(c, 'the site')).dispatchEvent(modDown);
    expect(modDown.defaultPrevented).toBe(true);
    expect(c.view.state.selection.toJSON()).toEqual(sel);
  });

  it('a middle press on a link is held back, so it pastes nothing (PRIMARY on Linux)', async () => {
    const c = await open(DOC);
    const before = markdown(c);
    const down = mouse('mousedown', { button: 1 });
    textIn(link(c, 'the site')).dispatchEvent(down);
    expect(down.defaultPrevented).toBe(true);
    expect(markdown(c)).toBe(before);
    // The middle click that follows opens the link once.
    textIn(link(c, 'the site')).dispatchEvent(mouse('auxclick', { button: 1 }));
    expect(opened).toEqual([['https://example.com/page', '_blank', 'noopener,noreferrer']]);
  });

  it('a middle press outside a link is not held back, so plain text keeps its native paste', async () => {
    const c = await open(DOC);
    const plain = Array.from(c.view.dom.querySelectorAll('p'))[0].firstChild!;
    expect(plain.nodeType).toBe(3);
    expect(plain.parentElement!.closest('a')).toBeNull();
    const down = mouse('mousedown', { button: 1 });
    plain.dispatchEvent(down);
    expect(down.defaultPrevented).toBe(false);
    // Control: the same press on a link is held back.
    const onLink = mouse('mousedown', { button: 1 });
    textIn(link(c, 'the site')).dispatchEvent(onLink);
    expect(onLink.defaultPrevented).toBe(true);
  });

  it('a Mod-click on a #fragment link moves to its heading and opens nothing', async () => {
    const c = await open('[go](#second-part)\n\n# First\n\n## Second part\n');
    const go = link(c, 'go');
    expect(go.hasAttribute('href')).toBe(false);
    const e = mouse('click', { ctrlKey: true });
    textIn(go).dispatchEvent(e);
    expect(e.defaultPrevented).toBe(true);
    expect(opened).toEqual([]);
    expect(c.view.state.selection.$from.parent.textContent).toBe('Second part');
  });

  it('decides the gestures by platform: Mod is Cmd on macOS and Ctrl elsewhere', () => {
    const e = (type: string, init: Partial<MouseEvent>): MouseEvent =>
      ({ type, button: 0, metaKey: false, ctrlKey: false, ...init }) as MouseEvent;
    // Elsewhere: Ctrl follows, Cmd does not.
    expect(followsLink(e('click', { ctrlKey: true }), false)).toBe(true);
    expect(followsLink(e('click', { metaKey: true }), false)).toBe(false);
    // macOS: Cmd follows, Ctrl (the context menu) does not.
    expect(followsLink(e('click', { metaKey: true }), true)).toBe(true);
    expect(followsLink(e('click', { ctrlKey: true }), true)).toBe(false);
    // A plain click never follows; a middle click always does; a right click never.
    expect(followsLink(e('click', {}), false)).toBe(false);
    expect(followsLink(e('auxclick', { button: 1 }), true)).toBe(true);
    expect(followsLink(e('auxclick', { button: 2 }), false)).toBe(false);
    // The press held back is the same Mod press of the main button only.
    expect(isModPress(e('mousedown', { ctrlKey: true }), false)).toBe(true);
    expect(isModPress(e('mousedown', {}), false)).toBe(false);
    expect(isModPress(e('mousedown', { ctrlKey: true, button: 2 }), false)).toBe(false);
    expect(isModPress(e('mousedown', { ctrlKey: true }), true)).toBe(false);
    // Held back on a link: a Mod press or a middle press; a plain or right press is not.
    expect(holdsLinkPress(e('mousedown', { button: 1 }), false)).toBe(true);
    expect(holdsLinkPress(e('mousedown', { button: 1 }), true)).toBe(true);
    expect(holdsLinkPress(e('mousedown', { ctrlKey: true }), false)).toBe(true);
    expect(holdsLinkPress(e('mousedown', {}), false)).toBe(false);
    expect(holdsLinkPress(e('mousedown', { button: 2 }), false)).toBe(false);
  });

  it('the open hint is a fixed text per platform', () => {
    expect(linkOpenHint(true)).toBe('Cmd-click opens in a new tab');
    expect(linkOpenHint(false)).toBe('Ctrl-click opens in a new tab');
  });
});
