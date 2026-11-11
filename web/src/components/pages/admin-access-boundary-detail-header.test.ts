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
 * The access constraint detail page renders its header through the shared
 * scion-detail-header (ptone/scion#4066): the name is the heading, the
 * status badge follows it in the default slot, the scope/subject line is
 * the meta and the edit/delete controls are the actions.
 */

import { describe, it, expect, beforeAll, vi } from 'vitest';
import { render, type CSSResult, type TemplateResult } from 'lit';

import { styleRules } from './__fixtures__/css-rules.js';
import type { ScionDetailHeader } from '../shared/detail-header.js';

vi.mock('../../client/main.js', () => {
  throw new Error('access boundary pages must not import client/main');
});

const TAG = 'scion-page-admin-access-boundary-detail';

// Only the fields the header reads.
const BOUNDARY = {
  id: 'ab-1',
  name: 'contractors-read-only',
  status: 'scheduled',
  risk: ['tightening', 'lockout_sensitive'],
  scope: { type: 'project', projectId: 'p-1' },
  scopeDisplay: { label: 'Project: Payments' },
  subject: { kind: 'group_closure', groupId: 'g-1' },
  subjectDisplay: { label: 'Group: Contractors' },
  updatedAt: '2026-01-01T00:00:00Z',
  updatedBy: { id: 'u-1', type: 'user', displayName: 'Ada' },
  _capabilities: { actions: ['read', 'previewTighten', 'previewRelax', 'delete'] },
};

type Boundary = typeof BOUNDARY & Record<string, unknown>;

beforeAll(async () => {
  await import('./admin-access-boundary-detail.js');
});

/** Render the page header for `boundary` into a detached host. */
function renderPageHeader(boundary: Boundary = BOUNDARY): HTMLElement {
  const el = document.createElement(TAG);
  Object.assign(el, { phase: 'ready', boundary });
  const tpl = (el as unknown as Record<string, (b: Boundary) => TemplateResult>).renderPageHeader(
    boundary
  );
  const host = document.createElement('div');
  render(tpl, host);
  return host;
}

function header(host: HTMLElement): ScionDetailHeader {
  const headers = host.querySelectorAll('scion-detail-header');
  expect(headers).toHaveLength(1);
  return headers[0] as ScionDetailHeader;
}

const slotted = (h: Element, name: string) =>
  Array.from(h.children).filter((n) => n.getAttribute('slot') === name);

describe('access constraint detail header', () => {
  it('renders the name through scion-detail-header', () => {
    const host = renderPageHeader();
    const h = header(host);
    expect(h.heading).toBe('contractors-read-only');
    // The h1 is the shared header's; the page renders none of its own.
    expect(host.querySelector('h1')).toBeNull();
    for (const sel of ['.page-header', '.header-main', '.header-info', '.header-badges']) {
      expect(host.querySelector(sel), sel).toBeNull();
    }
  });

  it('renders the h1 inside the shared header', async () => {
    const host = renderPageHeader();
    document.body.appendChild(host);
    try {
      const h = header(host);
      await h.updateComplete;
      expect(h.shadowRoot?.querySelector('h1')?.textContent).toBe('contractors-read-only');
    } finally {
      host.remove();
    }
  });

  it("puts the back link in the shared header's back slot", () => {
    const host = renderPageHeader();
    const back = slotted(header(host), 'back');
    expect(back.map((n) => n.tagName.toLowerCase())).toEqual(['scion-back-link']);
    expect(back[0].getAttribute('href')).toBe('/admin/access-boundaries');
    expect(back[0].textContent?.trim()).toBe('Access Constraints');
    // No back link of the page's own sits above the header any more.
    expect(host.querySelector('.header-top')).toBeNull();
    expect(host.querySelector('a.back-link')).toBeNull();
  });

  it('shows the Access Constraints nav icon in the icon slot', () => {
    const icons = slotted(header(renderPageHeader()), 'icon');
    expect(icons.map((n) => `${n.tagName.toLowerCase()}[${n.getAttribute('name')}]`)).toEqual([
      'sl-icon[shield-check]',
    ]);
  });

  it('puts the status badge after the name in the default slot', () => {
    const h = header(renderPageHeader());
    const badges = Array.from(h.children).filter((n) => !n.hasAttribute('slot'));
    expect(badges.map((n) => n.tagName.toLowerCase())).toEqual(['scion-access-boundary-status']);
    const status = badges[0] as HTMLElement & { status: string; risk: string[] };
    expect(status.getAttribute('status')).toBe('scheduled');
    expect(status.risk).toEqual(['tightening', 'lockout_sensitive']);
  });

  it('puts the scope and subject line in the meta slot', () => {
    const h = header(renderPageHeader());
    const meta = slotted(h, 'meta');
    expect(meta.map((n) => n.className)).toEqual(['header-meta']);
    const parts = Array.from(meta[0].querySelectorAll('span')).map((s) => s.textContent?.trim());
    expect(parts[0]).toBe('Project: Payments');
    expect(parts[1]).toBe('Group: Contractors');
    expect(parts[2]).toMatch(/^Updated /);
    expect(parts[3]).toBe('by Ada');
  });

  it('puts Edit and Delete in a single actions-slot wrapper', () => {
    const h = header(renderPageHeader());
    const actions = slotted(h, 'actions');
    expect(actions.map((n) => n.className)).toEqual(['header-actions']);
    const labels = Array.from(actions[0].querySelectorAll('sl-button')).map((b) =>
      b.textContent?.trim()
    );
    expect(labels).toEqual(['Edit', 'Delete']);
  });

  it('shows only the actions the capabilities allow', () => {
    const h = header(renderPageHeader({ ...BOUNDARY, _capabilities: { actions: ['delete'] } }));
    const labels = Array.from(h.querySelectorAll('[slot="actions"] sl-button')).map((b) =>
      b.textContent?.trim()
    );
    expect(labels).toEqual(['Delete']);
  });

  it('renders no actions wrapper without edit or delete capabilities', () => {
    const h = header(renderPageHeader({ ...BOUNDARY, _capabilities: { actions: ['read'] } }));
    expect(slotted(h, 'actions')).toHaveLength(0);
  });

  it('renders no actions wrapper for a recovery-disabled constraint', () => {
    const h = header(renderPageHeader({ ...BOUNDARY, status: 'recovery_disabled' }));
    expect(slotted(h, 'actions')).toHaveLength(0);
    expect(h.querySelector('sl-button')).toBeNull();
  });

  it('keeps no copy of the header layout CSS, including mobile overrides', () => {
    const ctor = customElements.get(TAG) as unknown as { elementStyles: CSSResult[] };
    const rules = styleRules(ctor.elementStyles.map((s) => s.cssText).join('\n'));
    for (const sel of [
      '.page-header',
      '.header-main',
      '.header-info',
      '.boundary-name',
      '.header-badges',
      '.header-actions',
    ]) {
      // Also catch a copy left inside a media query ("@media ... .sel").
      const copies = [...rules.keys()].filter((k) => k === sel || k.endsWith(` ${sel}`));
      expect(copies, sel).toEqual([]);
    }
    // The page still styles its own meta line.
    expect(rules.has('.header-meta')).toBe(true);
  });

  it('breaks a long scope or subject label inside the meta line', () => {
    const ctor = customElements.get(TAG) as unknown as { elementStyles: CSSResult[] };
    const rules = styleRules(ctor.elementStyles.map((s) => s.cssText).join('\n'));
    expect(rules.get('.header-meta') ?? '').toMatch(/overflow-wrap:\s*anywhere/);
  });
});
