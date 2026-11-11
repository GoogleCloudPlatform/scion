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

import { afterEach, describe, expect, it } from 'vitest';
import { hasOpenModalDescendant, isOpenModalElement } from './open-modal.js';

type Openable = HTMLElement & { open?: boolean };

function shoelace(tag: 'sl-dialog' | 'sl-drawer', open: boolean): Openable {
  const el = document.createElement(tag) as Openable;
  el.open = open;
  return el;
}

function nativeDialog(open: boolean): HTMLDialogElement {
  const el = document.createElement('dialog');
  if (open) el.setAttribute('open', '');
  return el;
}

/** A host element with an open shadow root containing `children`. */
function shadowHost(...children: Element[]): HTMLElement {
  const host = document.createElement('div');
  host.attachShadow({ mode: 'open' }).append(...children);
  return host;
}

afterEach(() => {
  document.body.replaceChildren();
});

describe('isOpenModalElement', () => {
  it('counts an open sl-dialog, an open non-contained sl-drawer and an open native dialog', () => {
    expect(isOpenModalElement(shoelace('sl-dialog', true))).toBe(true);
    expect(isOpenModalElement(shoelace('sl-drawer', true))).toBe(true);
    expect(isOpenModalElement(nativeDialog(true))).toBe(true);
  });

  it('does not count closed ones', () => {
    expect(isOpenModalElement(shoelace('sl-dialog', false))).toBe(false);
    expect(isOpenModalElement(shoelace('sl-drawer', false))).toBe(false);
    expect(isOpenModalElement(nativeDialog(false))).toBe(false);
  });

  it('does not count an open contained sl-drawer', () => {
    const drawer = shoelace('sl-drawer', true);
    drawer.setAttribute('contained', '');
    expect(isOpenModalElement(drawer)).toBe(false);
  });

  it('does not count other elements, even with open set', () => {
    const details = document.createElement('sl-details') as Openable;
    details.open = true;
    expect(isOpenModalElement(details)).toBe(false);
    expect(isOpenModalElement(document.createElement('div'))).toBe(false);
  });
});

describe('hasOpenModalDescendant', () => {
  it('is false for a document with no open modal', () => {
    document.body.append(shoelace('sl-dialog', false), nativeDialog(false));
    expect(hasOpenModalDescendant(document, null)).toBe(false);
  });

  it('finds an open modal in the light DOM', () => {
    document.body.append(nativeDialog(true));
    expect(hasOpenModalDescendant(document, null)).toBe(true);
  });

  it('finds an open modal inside a shadow root', () => {
    document.body.append(shadowHost(shoelace('sl-dialog', true)));
    expect(hasOpenModalDescendant(document, null)).toBe(true);
  });

  it('finds an open modal inside a nested shadow root', () => {
    document.body.append(shadowHost(shadowHost(shoelace('sl-drawer', true))));
    expect(hasOpenModalDescendant(document, null)).toBe(true);
  });

  it("skips an open modal inside a hidden host's shadow root", () => {
    const host = shadowHost(shoelace('sl-dialog', true));
    host.hidden = true;
    document.body.append(host);
    expect(hasOpenModalDescendant(document, null)).toBe(false);

    host.hidden = false;
    expect(hasOpenModalDescendant(document, null)).toBe(true);
  });

  it('skips an open modal under a hidden light-DOM ancestor', () => {
    const wrapper = document.createElement('div');
    wrapper.hidden = true;
    wrapper.append(document.createElement('div'));
    wrapper.firstElementChild!.append(nativeDialog(true), shadowHost(shoelace('sl-drawer', true)));
    document.body.append(wrapper);
    expect(hasOpenModalDescendant(document, null)).toBe(false);

    wrapper.hidden = false;
    expect(hasOpenModalDescendant(document, null)).toBe(true);
  });

  it('still finds a visible open modal beside a hidden one', () => {
    const hiddenHost = shadowHost(shoelace('sl-dialog', true));
    hiddenHost.hidden = true;
    document.body.append(hiddenHost, shadowHost(shoelace('sl-dialog', true)));
    expect(hasOpenModalDescendant(document, null)).toBe(true);
  });

  it('skips the excluded host and everything in its shadow tree', () => {
    const own = shadowHost(shoelace('sl-dialog', true));
    document.body.append(own);
    expect(hasOpenModalDescendant(document, own)).toBe(false);

    document.body.append(shadowHost(nativeDialog(true)));
    expect(hasOpenModalDescendant(document, own)).toBe(true);
  });
});
