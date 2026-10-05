/**
 * Copyright 2026 Google LLC
 * Licensed under the Apache License, Version 2.0 (the "License");
 */

import { afterEach, describe, expect, it, vi } from 'vitest';

const fetchSpy = vi.fn(() => Promise.reject(new Error('unexpected fetch')));
vi.stubGlobal('fetch', fetchSpy);

type NavigatingPage = HTMLElement & { navigate(path: string): void };

afterEach(() => {
  document.body.replaceChildren();
});

describe('admin access boundary pages', () => {
  it('make no network requests when imported', async () => {
    await import('./admin-access-boundaries.js');
    await import('./admin-access-boundary-detail.js');

    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it.each(['scion-page-admin-access-boundaries', 'scion-page-admin-access-boundary-detail'])(
    '%s navigates through a composed nav-click event',
    async (tag) => {
      await import('./admin-access-boundaries.js');
      await import('./admin-access-boundary-detail.js');
      const element = document.createElement(tag) as NavigatingPage;
      const paths: string[] = [];
      element.addEventListener('nav-click', (e) => {
        const event = e as CustomEvent<{ path: string }>;
        expect(event.bubbles).toBe(true);
        expect(event.composed).toBe(true);
        paths.push(event.detail.path);
      });

      element.navigate('/admin/access-boundaries');

      expect(paths).toEqual(['/admin/access-boundaries']);
    }
  );
});
