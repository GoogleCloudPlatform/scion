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
 * Breadcrumb coverage for Phase 2b pages (ptone/scion#9023):
 * - hub/user scopes of template-detail and hub scope of harness-config-detail
 *   (including loading and error states);
 * - broker-area-detail, skill-create, admin-access-boundary-editor (new & edit),
 *   and admin-integrations (detail vs list);
 * - loading, error, not-found, and permission-denied states across P2b pages;
 * - CSS guard confirming migrated non-detail-header pages keep no local back-link CSS.
 */

import { describe, it, expect, beforeAll, vi } from 'vitest';
import { render, type CSSResult, type TemplateResult } from 'lit';

import { crumbTuples } from '../../test-utils/crumbs.js';
import { styleRules } from './__fixtures__/css-rules.js';

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

const BACK_LINK_SELECTORS = [
  '.back-link',
  '.back-link:hover',
  '.back-links',
  '.header-top',
  '.breadcrumb',
  '.breadcrumb a',
  '.crumbs',
  '.crumbs a',
  '.editor-back-link',
  '.back',
];

function renderPage(tag: string, state: Record<string, unknown>): HTMLElement {
  const el = document.createElement(tag);
  Object.assign(el, state);
  const host = document.createElement('div');
  render((el as unknown as { render: () => TemplateResult }).render(), host);
  return host;
}

beforeAll(async () => {
  await Promise.all([
    import('./template-detail.js'),
    import('./harness-config-detail.js'),
    import('./broker-detail.js'),
    import('./broker-area-detail.js'),
    import('./skill-detail.js'),
    import('./skill-create.js'),
    import('./admin-group-detail.js'),
    import('./admin-role-detail.js'),
    import('./admin-access-boundary-detail.js'),
    import('./admin-access-boundary-editor.js'),
    import('./admin-skill-registry-detail.js'),
    import('./admin-integrations.js'),
    import('./admin-integrations-chat.js'),
  ]);
}, 60_000);

describe('Phase 2b breadcrumb trails', () => {
  describe('template-detail (hub and user scopes)', () => {
    it('renders user-scope trail when loaded', () => {
      const host = renderPage('scion-page-template-detail', {
        loading: false,
        scope: 'user',
        templateId: 't-user',
        template: { id: 't-user', name: 'my-tpl', scope: 'user', status: 'active' },
      });
      expect(crumbTuples(host)).toEqual([
        ['Profile', '/profile'],
        ['Templates', '/profile/templates'],
        ['my-tpl', undefined],
      ]);
      expect(host.querySelector('scion-back-link')).toBeNull();
    });

    it('renders ancestor-only trail in hub loading and error states', () => {
      for (const state of [
        { loading: true, scope: 'hub' },
        { loading: false, error: 'boom', scope: 'hub' },
      ]) {
        const host = renderPage('scion-page-template-detail', state);
        expect(crumbTuples(host)).toEqual([
          ['Hub Resources', '/settings'],
          ['Templates', '/settings?tab=templates'],
        ]);
      }
    });

    it('renders ancestor-only trail in user loading and error states', () => {
      for (const state of [
        { loading: true, scope: 'user' },
        { loading: false, error: 'boom', scope: 'user' },
      ]) {
        const host = renderPage('scion-page-template-detail', state);
        expect(crumbTuples(host)).toEqual([
          ['Profile', '/profile'],
          ['Templates', '/profile/templates'],
        ]);
      }
    });
  });

  describe('harness-config-detail (hub scope)', () => {
    it('renders ancestor-only trail in hub loading and error states', () => {
      for (const state of [{ loading: true }, { loading: false, error: 'boom' }]) {
        const host = renderPage('scion-page-harness-config-detail', state);
        expect(crumbTuples(host)).toEqual([
          ['Hub Resources', '/settings'],
          ['Harness Configs', '/settings?tab=harness-configs'],
        ]);
      }
    });
  });

  describe('broker-area-detail', () => {
    it('renders Brokers / {broker} when loaded', () => {
      const host = renderPage('scion-page-broker-area-detail', {
        loading: false,
        brokerId: 'b-1',
        broker: { id: 'b-1', name: 'prod-broker', status: 'online' },
      });
      expect(crumbTuples(host)).toEqual([
        ['Brokers', '/brokers'],
        ['prod-broker', undefined],
      ]);
      expect(host.querySelector('scion-back-link')).toBeNull();
    });

    it('renders ancestor-only trail in loading and error states', () => {
      for (const state of [{ loading: true }, { loading: false, error: 'boom' }]) {
        const host = renderPage('scion-page-broker-area-detail', state);
        expect(crumbTuples(host)).toEqual([['Brokers', '/brokers']]);
        expect(host.querySelector('scion-back-link')).toBeNull();
      }
    });
  });

  describe('skill-create', () => {
    it('renders Skills / New skill when ready', () => {
      const host = renderPage('scion-page-skill-create', {
        loading: false,
        canCreate: true,
        flowState: 'form',
      });
      expect(crumbTuples(host)).toEqual([
        ['Skills', '/skills'],
        ['New skill', undefined],
      ]);
      expect(host.querySelector('a.back-link')).toBeNull();
    });

    it('renders ancestor-only trail in loading and access-denied states', () => {
      for (const state of [
        { loading: true, canCreate: false },
        { loading: false, canCreate: false },
      ]) {
        const host = renderPage('scion-page-skill-create', state);
        expect(crumbTuples(host)).toEqual([['Skills', '/skills']]);
        expect(host.querySelector('a.back-link')).toBeNull();
      }
    });
  });

  describe('admin-access-boundary-editor', () => {
    it('renders Access Constraints / New in create mode', () => {
      const host = renderPage('scion-page-admin-access-boundary-editor', {
        loadingBoundary: false,
        loadError: '',
        isEditMode: false,
      });
      expect(crumbTuples(host)).toEqual([
        ['Access Constraints', '/admin/access-boundaries'],
        ['New', undefined],
      ]);
      expect(host.querySelector('a.editor-back-link')).toBeNull();
    });

    it('renders Access Constraints / {boundary} / Edit in edit mode with encoded href', () => {
      const host = renderPage('scion-page-admin-access-boundary-editor', {
        loadingBoundary: false,
        loadError: '',
        isEditMode: true,
        boundaryId: 'ab/1?x',
        loadedBoundaryName: 'prod-lock',
        draftName: '',
      });
      expect(crumbTuples(host)).toEqual([
        ['Access Constraints', '/admin/access-boundaries'],
        ['prod-lock', '/admin/access-boundaries/ab%2F1%3Fx'],
        ['Edit', undefined],
      ]);
      expect(host.querySelector('a.editor-back-link')).toBeNull();
    });

    it('renders ancestor-only trail in loading and error states', () => {
      for (const state of [
        { loadingBoundary: true, loadError: '' },
        { loadingBoundary: false, loadError: 'boom' },
      ]) {
        const host = renderPage('scion-page-admin-access-boundary-editor', state);
        expect(crumbTuples(host)).toEqual([['Access Constraints', '/admin/access-boundaries']]);
      }
    });
  });

  describe('admin-access-boundary-detail non-ready states', () => {
    it.each(['loading', 'error', 'not_found', 'permission_denied'])(
      'renders ancestor-only trail in %s phase',
      (phase) => {
        const host = renderPage('scion-page-admin-access-boundary-detail', {
          phase,
          boundaryId: 'ab-1',
          errorMessage: 'boom',
        });
        expect(crumbTuples(host)).toEqual([['Access Constraints', '/admin/access-boundaries']]);
        expect(host.querySelector('scion-back-link')).toBeNull();
      }
    );
  });

  describe('admin-integrations', () => {
    it('renders no breadcrumb on the list view', () => {
      window.history.replaceState({}, '', '/admin/integrations/chat');
      try {
        const host = renderPage('scion-admin-integrations-chat', {
          loading: false,
          integrations: [],
          availableIntegrations: [],
        });
        expect(host.querySelector('scion-breadcrumb')).toBeNull();
      } finally {
        window.history.replaceState({}, '', '/');
      }
    });

    it('renders Integrations / {integration} on the detail view', () => {
      window.history.replaceState({}, '', '/admin/integrations/chat/telegram');
      try {
        const host = renderPage('scion-admin-integrations-chat', {
          loading: false,
          detail: {
            name: 'telegram',
            platform: 'telegram',
            self_managed: false,
            settings: {},
            has_secrets: {},
          },
        });
        expect(crumbTuples(host)).toEqual([
          ['Integrations', '/admin/integrations/chat'],
          ['telegram', undefined],
        ]);
        expect(host.querySelector('a.back-link')).toBeNull();
      } finally {
        window.history.replaceState({}, '', '/');
      }
    });

    it('renders ancestor-only trail in detail loading and not-found states', () => {
      window.history.replaceState({}, '', '/admin/integrations/chat/telegram');
      try {
        for (const state of [
          { loading: true, detail: null },
          { loading: false, detail: null },
        ]) {
          const host = renderPage('scion-admin-integrations-chat', state);
          expect(crumbTuples(host)).toEqual([['Integrations', '/admin/integrations/chat']]);
          expect(host.querySelector('a.back-link')).toBeNull();
        }
      } finally {
        window.history.replaceState({}, '', '/');
      }
    });
  });

  describe('loading states on detail-header P2b pages', () => {
    it.each([
      ['scion-page-broker-detail', 'Brokers', '/brokers'],
      ['scion-page-skill-detail', 'Skills', '/skills'],
      ['scion-page-admin-group-detail', 'Groups', '/admin/groups'],
      ['scion-page-admin-role-detail', 'Roles', '/admin/roles'],
      ['scion-page-admin-skill-registry-detail', 'Skill Registries', '/admin/skill-registries'],
    ])('%s renders ancestor-only trail while loading', (tag, label, href) => {
      const host = renderPage(tag, { loading: true });
      expect(crumbTuples(host)).toEqual([[label, href]]);
      expect(host.querySelector('scion-back-link')).toBeNull();
    });
  });

  describe('non-detail-header P2b pages keep no local back-link CSS', () => {
    it.each([
      'scion-page-broker-area-detail',
      'scion-page-skill-create',
      'scion-page-admin-access-boundary-editor',
      'scion-page-admin-integrations',
    ])('%s defines no back-link selectors', (tag) => {
      const ctor = customElements.get(tag) as unknown as { elementStyles: CSSResult[] };
      const rules = styleRules(ctor.elementStyles.map((s) => s.cssText).join('\n'));
      for (const sel of BACK_LINK_SELECTORS) {
        expect(rules.has(sel), `${tag} ${sel}`).toBe(false);
      }
    });
  });
});
