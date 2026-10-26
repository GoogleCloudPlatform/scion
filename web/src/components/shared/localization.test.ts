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

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { type LitElement } from 'lit';
import {
  getLocale,
  initializeLocalization,
  LOCALE_STORAGE_KEY,
  setLocalePreference,
} from '../../i18n/index.js';
import { PAGE_TITLE_EVENT } from '../../client/page-title.js';
import type { User } from '../../shared/types.js';

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return { ...actual, apiFetch: vi.fn(() => Promise.resolve(new Response('{}'))) };
});

type Shell = LitElement & { currentPath: string; user: User | null };

beforeAll(async () => {
  await import('../app-shell.js');
}, 30_000);

beforeEach(async () => {
  localStorage.clear();
  vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['en-US']);
  await initializeLocalization(true);
});

afterEach(async () => {
  document.body.replaceChildren();
  await initializeLocalization(false);
  vi.restoreAllMocks();
});

async function mountedShell(): Promise<Shell> {
  const shell = document.createElement('scion-app') as Shell;
  shell.currentPath = '/projects';
  shell.user = { id: 'u', role: 'admin', name: 'User' } as User;
  document.body.append(shell);
  await shell.updateComplete;
  await settle(shell);
  return shell;
}

async function settle(shell: Shell): Promise<void> {
  await shell.updateComplete;
  const components = [...shell.shadowRoot!.querySelectorAll<LitElement>('scion-nav, scion-header')];
  await Promise.all(components.map((component) => component.updateComplete));
  for (const header of shell.shadowRoot!.querySelectorAll<LitElement>('scion-header')) {
    const selector = header.shadowRoot?.querySelector<LitElement>('scion-language-select');
    await selector?.updateComplete;
  }
}

describe('localized application shell', () => {
  it('switches mounted navigation, titles, menus and accessibility labels without changing routes', async () => {
    const shell = await mountedShell();
    const nav = shell.shadowRoot!.querySelector<LitElement>('scion-nav')!;
    const header = shell.shadowRoot!.querySelector<LitElement>('scion-header')!;
    const links = (): string[] =>
      [...nav.shadowRoot!.querySelectorAll('a')].map((link) => link.getAttribute('href')!);
    const beforeLinks = links();
    expect(document.title).toBe('Projects — Scion');
    await setLocalePreference('zh-CN');
    await settle(shell);
    expect(nav.shadowRoot!.textContent).toContain('项目');
    expect(nav.shadowRoot!.textContent).toContain('服务器配置');
    expect(nav.shadowRoot!.querySelector('.collapse-toggle')!.getAttribute('aria-label')).toBe(
      '收起侧边栏'
    );
    expect(header.shadowRoot!.querySelector('.page-title')!.textContent).toBe('项目');
    expect(header.shadowRoot!.querySelector('.user-trigger')!.getAttribute('aria-label')).toBe(
      '账户菜单'
    );
    expect(header.shadowRoot!.querySelector('.mode-switch')!.getAttribute('aria-label')).toBe(
      '切换视图'
    );
    expect(document.title).toBe('项目 — Scion');
    expect(links()).toEqual(beforeLinks);
    expect(shell.currentPath).toBe('/projects');

    const selector = header.shadowRoot!.querySelector<LitElement>('scion-language-select')!;
    const select = selector.shadowRoot!.querySelector('select')!;
    expect(select.value).toBe('zh-CN');
    select.value = 'en';
    select.dispatchEvent(new Event('change'));
    await vi.waitFor(() => expect(getLocale()).toBe('en'));
    await settle(shell);
    expect(getLocale()).toBe('en');
    expect(document.title).toBe('Projects — Scion');
    expect(nav.shadowRoot!.textContent).toContain('Projects');
  });

  it('updates a previously interacted native selector after another tab changes the preference', async () => {
    const shell = await mountedShell();
    const header = shell.shadowRoot!.querySelector<LitElement>('scion-header')!;
    const selector = header.shadowRoot!.querySelector<LitElement>('scion-language-select')!;
    const select = selector.shadowRoot!.querySelector('select')!;
    select.value = 'en';
    select.dispatchEvent(new Event('change'));
    await vi.waitFor(() => expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe('en'));
    await settle(shell);
    localStorage.setItem(LOCALE_STORAGE_KEY, 'zh-CN');
    window.dispatchEvent(new StorageEvent('storage', { key: LOCALE_STORAGE_KEY }));
    await vi.waitFor(() => expect(getLocale()).toBe('zh-CN'));
    await settle(shell);
    expect(select.value).toBe('zh-CN');
    localStorage.removeItem(LOCALE_STORAGE_KEY);
    window.dispatchEvent(new StorageEvent('storage', { key: LOCALE_STORAGE_KEY }));
    await vi.waitFor(() => expect(getLocale()).toBe('en'));
    await settle(shell);
    expect(select.value).toBe('');
  });

  it('translates static admin breadcrumb segments while preserving entity labels', async () => {
    const breadcrumb = document.createElement('scion-breadcrumb');
    breadcrumb.path = '/admin/users/12345';
    breadcrumb.currentLabel = 'user-name';
    document.body.append(breadcrumb);
    await setLocalePreference('zh-CN');
    await breadcrumb.updateComplete;
    const labels = [...breadcrumb.shadowRoot!.querySelectorAll('sl-breadcrumb-item')].map((item) =>
      item.textContent!.trim()
    );
    expect(labels).toEqual(['首页', '系统管理', '用户', 'user-name']);
  });

  it('preserves entity title segments and stops updating a detached shell', async () => {
    const shell = await mountedShell();
    shell.dispatchEvent(
      new CustomEvent(PAGE_TITLE_EVENT, { detail: { segments: ['my-agent', 'my-project'] } })
    );
    await setLocalePreference('zh-CN');
    await settle(shell);
    expect(document.title).toBe('my-agent — my-project — Scion');
    const update = vi.spyOn(shell, 'requestUpdate');
    shell.remove();
    await setLocalePreference('en');
    expect(update).not.toHaveBeenCalled();
  });

  it('hides language controls and restores English when the experiment is disabled', async () => {
    await setLocalePreference('zh-CN');
    await initializeLocalization(false);
    const shell = await mountedShell();
    const header = shell.shadowRoot!.querySelector<LitElement>('scion-header')!;
    expect(header.shadowRoot!.querySelector('scion-language-select')).toBeNull();
    expect(document.title).toBe('Projects — Scion');
  });
});
