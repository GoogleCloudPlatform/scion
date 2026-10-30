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

/** The shared popup menu contract: roles, keys and focus (context-menu.ts). */

import { describe, it, expect, afterEach, vi } from 'vitest';
import { render } from 'lit';

import { MenuFocus, renderMenu, type MenuAction } from './context-menu.js';

function actions(run = vi.fn()): MenuAction[] {
  return [
    { id: 'reply', label: 'Reply', icon: 'reply', run: () => run('reply') },
    { id: 'move', label: 'Move up', disabled: true, run: () => run('move') },
    { id: 'star', label: 'Star message', checked: false, run: () => run('star') },
  ];
}

interface Fixture {
  opener: HTMLButtonElement;
  menu: HTMLElement;
  focus: MenuFocus;
  close: ReturnType<typeof vi.fn>;
  run: ReturnType<typeof vi.fn>;
  items: () => HTMLElement[];
}

function open(): Fixture {
  const opener = document.createElement('button');
  opener.textContent = 'More actions';
  const after = document.createElement('button');
  after.textContent = 'after';
  const host = document.createElement('div');
  document.body.append(opener, host, after);
  opener.focus();
  const focus = new MenuFocus();
  const close = vi.fn(() => render(null, host));
  const run = vi.fn();
  focus.opened();
  render(renderMenu(actions(run), { label: 'Message actions', focus, close }), host);
  const menu = host.querySelector<HTMLElement>('[role="menu"]')!;
  focus.afterRender(menu);
  return {
    opener,
    menu,
    focus,
    close,
    run,
    items: () => [...menu.querySelectorAll<HTMLElement>('.context-menu-item')],
  };
}

function key(target: HTMLElement, k: string, shiftKey = false): KeyboardEvent {
  const e = new KeyboardEvent('keydown', { key: k, shiftKey, bubbles: true, cancelable: true });
  target.dispatchEvent(e);
  return e;
}

afterEach(() => {
  document.body.innerHTML = '';
});

describe('shared popup menu', () => {
  it('is a named menu of menuitems, a checkbox item for a toggle, disabled rows still focusable', () => {
    const f = open();
    expect(f.menu.getAttribute('aria-label')).toBe('Message actions');
    expect(f.items().map((i) => i.getAttribute('role'))).toEqual([
      'menuitem',
      'menuitem',
      'menuitemcheckbox',
    ]);
    expect(f.items()[2].getAttribute('aria-checked')).toBe('false');
    expect(f.items()[1].getAttribute('aria-disabled')).toBe('true');
    expect(f.items().every((i) => i.getAttribute('tabindex') === '-1')).toBe(true);
    // Icons are decoration: the label names the item.
    expect(f.items()[0].querySelector('sl-icon')?.getAttribute('aria-hidden')).toBe('true');
  });

  it('moves focus into the first item on open', () => {
    const f = open();
    expect(document.activeElement).toBe(f.items()[0]);
  });

  it('Up/Down wrap and Home/End reach the ends, including the disabled row', () => {
    const f = open();
    key(f.menu, 'ArrowDown');
    expect(document.activeElement).toBe(f.items()[1]);
    key(f.menu, 'ArrowDown');
    key(f.menu, 'ArrowDown');
    expect(document.activeElement).toBe(f.items()[0]);
    key(f.menu, 'ArrowUp');
    expect(document.activeElement).toBe(f.items()[2]);
    key(f.menu, 'Home');
    expect(document.activeElement).toBe(f.items()[0]);
    const e = key(f.menu, 'End');
    expect(document.activeElement).toBe(f.items()[2]);
    expect(e.defaultPrevented).toBe(true);
  });

  it('Enter and Space activate once; a disabled item never runs', () => {
    const f = open();
    key(f.menu, 'Enter');
    expect(f.run).toHaveBeenCalledTimes(1);
    expect(f.run).toHaveBeenLastCalledWith('reply');
    f.items()[1].focus();
    key(f.menu, ' ');
    key(f.menu, 'Enter');
    f.items()[1].click();
    expect(f.run).toHaveBeenCalledTimes(1);
    f.items()[2].focus();
    key(f.menu, ' ');
    expect(f.run).toHaveBeenLastCalledWith('star');
  });

  it('Escape closes and returns focus to the opener', () => {
    const f = open();
    key(f.menu, 'Escape');
    expect(f.close).toHaveBeenCalledTimes(1);
    expect(document.activeElement).toBe(f.opener);
  });

  it('Tab closes without being trapped: focus sits on the opener for the browser Tab step', () => {
    const f = open();
    const e = key(f.menu, 'Tab');
    expect(f.close).toHaveBeenCalledTimes(1);
    expect(e.defaultPrevented).toBe(false);
    expect(document.activeElement).toBe(f.opener);
    const back = open();
    expect(key(back.menu, 'Tab', true).defaultPrevented).toBe(false);
  });

  it('after an action, focus returns to the opener only if the action left none', async () => {
    const f = open();
    // The row goes away with the menu: nothing is focused.
    f.items()[0].click();
    f.close();
    await new Promise((r) => setTimeout(r, 5));
    expect(document.activeElement).toBe(f.opener);

    const g = open();
    const composer = document.createElement('textarea');
    document.body.append(composer);
    g.items()[0].click();
    g.close();
    composer.focus();
    await new Promise((r) => setTimeout(r, 5));
    expect(document.activeElement).toBe(composer);
  });

  it('an outside close forgets the opener: a later action does not pull focus back', async () => {
    const f = open();
    const other = document.createElement('button');
    document.body.append(other);
    f.focus.closed();
    f.close();
    other.focus();
    f.focus.restoreIfLost();
    await new Promise((r) => setTimeout(r, 5));
    expect(document.activeElement).toBe(other);
  });

  it('renders no popup for an empty action list', () => {
    const host = document.createElement('div');
    render(renderMenu([], { label: 'x', focus: new MenuFocus(), close: () => {} }), host);
    expect(host.querySelector('[role="menu"]')).toBeNull();
  });
});
