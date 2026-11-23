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

import { describe, it, expect, beforeAll } from 'vitest';
import { render, type TemplateResult } from 'lit';
import type { Template } from '../../shared/types.js';
import type { ScionHashDisplay } from '../shared/hash-display.js';

const TAG = 'scion-page-template-detail';
const FULL_SHA = '0123456789abcdef0123456789abcdef01234567';

beforeAll(async () => {
  await import('./template-detail.js');
});

function renderTemplateHeader(template: Partial<Template>): HTMLElement {
  const el = document.createElement(TAG);
  const full: Template = {
    id: 'tpl-1',
    name: 'my-tpl',
    slug: 'my-tpl',
    harness: 'claude',
    status: 'active',
    scope: 'project',
    ...template,
  };
  Object.assign(el, { loading: false, template: full });
  const tpl = (el as unknown as { renderHeader: () => TemplateResult }).renderHeader();
  const host = document.createElement('div');
  render(tpl, host);
  return host;
}

describe('scion-page-template-detail sourceCommit', () => {
  it('renders short SHA with GitHub tree link and full SHA on hash-display', () => {
    const host = renderTemplateHeader({
      sourceUrl: 'https://github.com/acme/repo/tree/main/.scion/templates/my-tpl',
      sourceCommit: FULL_SHA,
    });
    const meta = host.querySelector('.source-commit-meta');
    expect(meta).not.toBeNull();
    const display = meta!.querySelector('scion-hash-display') as ScionHashDisplay | null;
    expect(display).not.toBeNull();
    expect(display!.hash).toBe(FULL_SHA);
    expect(display!.displayText).toBe('0123456');
    expect(display!.href).toBe(
      `https://github.com/acme/repo/tree/${FULL_SHA}/.scion/templates/my-tpl`
    );
  });

  it('renders muted hint when GitHub sourceUrl is set but sourceCommit is empty', () => {
    const host = renderTemplateHeader({
      sourceUrl: 'https://github.com/acme/repo/tree/main/.scion/templates/my-tpl',
    });
    expect(host.querySelector('.source-commit-meta')).toBeNull();
    const empty = host.querySelector('.source-commit-empty');
    expect(empty).not.toBeNull();
    expect(empty!.textContent).toContain('not recorded — refresh from source to record');
  });

  it('hides sourceCommit row when neither sourceCommit nor refreshable sourceUrl is set', () => {
    const host = renderTemplateHeader({});
    expect(host.querySelector('.source-commit-meta')).toBeNull();
    expect(host.querySelector('.source-commit-empty')).toBeNull();
  });

  it('hides sourceCommit hint for builtin:// sources', () => {
    const host = renderTemplateHeader({
      sourceUrl: 'builtin://scion/1.0/template/default',
    });
    expect(host.querySelector('.source-commit-meta')).toBeNull();
    expect(host.querySelector('.source-commit-empty')).toBeNull();
  });
});
