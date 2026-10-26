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

import { beforeEach, expect, it, vi } from 'vitest';
const loaders = vi.hoisted(() => ({ zh: vi.fn(), ar: vi.fn() }));
vi.mock('./locales.js', async (importOriginal) => {
  const original = await importOriginal<typeof import('./locales.js')>();
  return {
    ...original,
    locales: [
      ...original.locales.map((locale) =>
        locale.code === 'zh-CN' ? { ...locale, loadMessages: loaders.zh } : locale
      ),
      { code: 'ar', name: 'العربية', dir: 'rtl', aliases: [], loadMessages: loaders.ar },
    ],
  };
});
beforeEach(() => {
  vi.resetModules();
  loaders.zh.mockReset().mockResolvedValue({ 'page.dashboard': '仪表盘' });
  loaders.ar.mockReset().mockResolvedValue({});
});
it('loads English alone, then single-flights only the requested catalog', async () => {
  const { loadEngine } = await import('./engine.js');
  const engine = await loadEngine();
  expect(engine.translate('page.dashboard', {})).toBe('Dashboard');
  expect(loaders.zh).not.toHaveBeenCalled();
  expect(loaders.ar).not.toHaveBeenCalled();
  await Promise.all([engine.loadLocale('zh-CN'), engine.loadLocale('zh-CN')]);
  engine.setLocale('zh-CN');
  expect(engine.translate('page.dashboard', {})).toBe('仪表盘');
  await engine.loadLocale('zh-CN');
  expect(loaders.zh).toHaveBeenCalledOnce();
  expect(loaders.ar).not.toHaveBeenCalled();
});
it('allows retry after a catalog failure without changing the active language', async () => {
  const { loadEngine } = await import('./engine.js');
  const engine = await loadEngine();
  loaders.zh.mockRejectedValueOnce(new Error('offline'));
  await expect(engine.loadLocale('zh-CN')).rejects.toThrow('offline');
  expect(engine.translate('page.dashboard', {})).toBe('Dashboard');
  await engine.loadLocale('zh-CN');
  engine.setLocale('zh-CN');
  expect(engine.translate('page.dashboard', {})).toBe('仪表盘');
  expect(loaders.zh).toHaveBeenCalledTimes(2);
});

it('falls back to English for both missing and empty target messages', async () => {
  const { loadEngine } = await import('./engine.js');
  const engine = await loadEngine();
  loaders.zh.mockResolvedValueOnce({ 'page.dashboard': '' });
  await engine.loadLocale('zh-CN');
  engine.setLocale('zh-CN');
  expect(engine.translate('page.dashboard', {})).toBe('Dashboard');
  expect(engine.translate('header.help', {})).toBe('Help');
});
